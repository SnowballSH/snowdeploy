// Command snowdeploy drives a snowdeployd daemon, and validates manifest
// directories. The validate subcommand is deliberately server-independent so a
// configuration repository's own CI can run it with no host access at all.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"
)

var version = "dev"

// Environment fallbacks for the connection flags.
const (
	envServer    = "SNOWDEPLOY_SERVER"
	envTokenFile = "SNOWDEPLOY_TOKEN_FILE"
)

// defaultFollowTimeout bounds how long the CLI waits for a terminal state.
const defaultFollowTimeout = 30 * time.Minute

// terminalStates end a followed deploy. Only healthy is a success.
var terminalStates = map[string]bool{
	"healthy": true, "rolled-back": true, "failed": true,
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return 2
	}
	if args[0] == "--version" || args[0] == "-version" || args[0] == "version" {
		fmt.Fprintf(stdout, "snowdeploy %s\n", version)
		return 0
	}

	ctx, stop := signal.NotifyContext(context.Background(),
		os.Interrupt, syscall.SIGTERM)
	defer stop()

	sub, rest := args[0], args[1:]
	var err error
	switch sub {
	case "validate":
		err = cmdValidate(rest, stdout)
	case "status":
		err = cmdStatus(ctx, rest, stdout)
	case "deploy":
		return followedCommand(ctx, "deploy", rest, stdout, stderr)
	case "rollback":
		return followedCommand(ctx, "rollback", rest, stdout, stderr)
	case "history":
		err = cmdHistory(ctx, rest, stdout)
	case "help", "-h", "--help":
		usage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "snowdeploy: unknown subcommand %q\n\n", sub)
		usage(stderr)
		return 2
	}

	if err != nil {
		fmt.Fprintf(stderr, "snowdeploy: %v\n", err)
		return 1
	}
	return 0
}

func usage(w io.Writer) {
	fmt.Fprint(w, `snowdeploy — deploy, roll back, and inspect managed services.

  snowdeploy status
  snowdeploy deploy   <service> [--digest sha256:...]
  snowdeploy rollback <service> [--to sha256:...]
  snowdeploy history  <service> [-n 20]
  snowdeploy validate <dir> [--volume-prefixes p1,p2] [--env-file-prefixes p1,p2]

Connection flags (status, deploy, rollback, history):
  --server      daemon base URL      (env `+envServer+`)
  --token-file  file holding the CLI bearer token (env `+envTokenFile+`)
`)
}

// connectionFlags registers the flags every server-backed subcommand shares.
func connectionFlags(fs *flag.FlagSet) (server, tokenFile *string) {
	server = fs.String("server", os.Getenv(envServer), "daemon base URL")
	tokenFile = fs.String("token-file", os.Getenv(envTokenFile),
		"file holding the CLI bearer token")
	return server, tokenFile
}

func newFlagSet(name string, out io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(out)
	return fs
}

// parseWithOperand accepts the operand on either side of the flags. Go's flag
// package stops at the first non-flag argument, so `deploy web --digest x`
// would otherwise silently ignore the flags.
func parseWithOperand(fs *flag.FlagSet, args []string) (string, error) {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		if err := fs.Parse(args[1:]); err != nil {
			return "", err
		}
		if fs.NArg() != 0 {
			return "", fmt.Errorf("unexpected extra arguments: %v", fs.Args())
		}
		return args[0], nil
	}
	if err := fs.Parse(args); err != nil {
		return "", err
	}
	if fs.NArg() != 1 {
		return "", errors.New("exactly one operand is required")
	}
	return fs.Arg(0), nil
}

func cmdStatus(ctx context.Context, args []string, stdout io.Writer) error {
	fs := newFlagSet("status", stdout)
	server, tokenFile := connectionFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}

	c, err := newClient(*server, *tokenFile)
	if err != nil {
		return err
	}
	services, err := c.services(ctx)
	if err != nil {
		return err
	}

	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SERVICE\tRUNNING\tMANIFEST\tLATEST\tSTATE")
	for _, s := range services {
		state := "in sync"
		if s.Drifted {
			state = "DRIFTED"
		} else if s.LatestAvailable != "" && s.LatestAvailable != s.ManifestDigest {
			state = "update available"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			s.Name, short(s.RunningDigest), short(s.ManifestDigest),
			short(s.LatestAvailable), state)
	}
	return tw.Flush()
}

// followedCommand runs a deploy or rollback and streams its progress. It
// returns the process exit code: nonzero unless the service ended healthy.
func followedCommand(
	ctx context.Context, action string, args []string, stdout, stderr io.Writer,
) int {
	fs := newFlagSet(action, stderr)
	server, tokenFile := connectionFlags(fs)

	digestFlag := "digest"
	if action == "rollback" {
		digestFlag = "to"
	}
	digest := fs.String(digestFlag, "", "digest to pin (sha256:...)")
	timeout := fs.Duration("timeout", defaultFollowTimeout,
		"give up waiting for a terminal state after this long")

	service, err := parseWithOperand(fs, args)
	if err != nil {
		fmt.Fprintf(stderr, "snowdeploy %s: %v\n", action, err)
		return 2
	}

	c, clientErr := newClient(*server, *tokenFile)
	if clientErr != nil {
		fmt.Fprintf(stderr, "snowdeploy: %v\n", clientErr)
		return 1
	}

	target := *digest
	if target == "" && action == "deploy" {
		resolved, err := latestFor(ctx, c, service)
		if err != nil {
			fmt.Fprintf(stderr, "snowdeploy: %v\n", err)
			return 1
		}
		target = resolved
	}

	// Bound the wait: a stalled stream must not hang the operator's terminal.
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	// Subscribe before starting, so no transition happens unobserved.
	scanner, closeStream, err := c.openEvents(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "snowdeploy: %v\n", err)
		return 1
	}
	defer closeStream()

	journalID, err := c.startDeploy(ctx, action, service, target)
	if err != nil {
		fmt.Fprintf(stderr, "snowdeploy: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "%s %s (journal entry %d)\n", action, service, journalID)

	return follow(scanner, journalID, stdout, stderr)
}

func follow(scanner *scannerType, journalID int64, stdout, stderr io.Writer) int {
	for {
		ev, ok := nextEvent(scanner, journalID)
		if !ok {
			fmt.Fprintln(stderr, "snowdeploy: the event stream ended before a terminal state")
			return 1
		}
		fmt.Fprintf(stdout, "  %-13s %s\n", ev.State, ev.Detail)
		if terminalStates[ev.State] {
			if ev.State == "healthy" {
				return 0
			}
			return 1
		}
	}
}

// latestFor resolves the digest a bare `deploy <service>` means.
func latestFor(ctx context.Context, c *client, service string) (string, error) {
	services, err := c.services(ctx)
	if err != nil {
		return "", err
	}
	for _, s := range services {
		if s.Name != service {
			continue
		}
		if s.LatestAvailable == "" {
			return "", fmt.Errorf(
				"no digest is known for %s yet; pass --digest", service)
		}
		return s.LatestAvailable, nil
	}
	return "", fmt.Errorf("no such service: %s", service)
}

func cmdHistory(ctx context.Context, args []string, stdout io.Writer) error {
	fs := newFlagSet("history", stdout)
	server, tokenFile := connectionFlags(fs)
	n := fs.Int("n", 20, "how many entries to show")
	service, err := parseWithOperand(fs, args)
	if err != nil {
		return err
	}

	c, err := newClient(*server, *tokenFile)
	if err != nil {
		return err
	}
	entries, err := c.history(ctx, service, *n)
	if err != nil {
		return err
	}

	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "WHEN\tACTION\tACTOR\tDIGEST\tSTATE\tDETAIL")
	for _, e := range entries {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			e.StartedAt.Local().Format(time.RFC3339), e.Action, e.Actor,
			short(e.NewDigest), e.State, truncate(e.Detail, 60))
	}
	return tw.Flush()
}

func short(digest string) string {
	if digest == "" {
		return "-"
	}
	trimmed := strings.TrimPrefix(digest, "sha256:")
	if len(trimmed) > 12 {
		return trimmed[:12]
	}
	return trimmed
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
