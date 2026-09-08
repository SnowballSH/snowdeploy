package api

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
)

// remoteUserHeader is set by the fronting proxy after it has authenticated the
// browser session. It is trustworthy only because the API listens on loopback
// and nothing but the proxy can reach it.
const remoteUserHeader = "Remote-User"

// cliActorFallback labels a bearer token whose hash line carries no label.
const cliActorFallback = "cli"

// scopePrefix marks the optional trailing field of a hash line that narrows
// what the token may do.
const scopePrefix = "scope="

// action is one mutating operation the API offers. The daemon authorizes the
// action, not merely the identity: a token may be issued for converge alone.
type action string

const (
	actionDeploy   action = "deploy"
	actionRollback action = "rollback"
	actionConverge action = "converge"
)

var knownActions = map[action]bool{
	actionDeploy:   true,
	actionRollback: true,
	actionConverge: true,
}

// scope is the set of actions an identity may take. A nil scope is unscoped
// and allows every action, which is what an unlabelled hash line, an operator
// token predating scopes, and the proxy-authenticated browser all carry.
type scope map[action]bool

func (s scope) allows(a action) bool { return s == nil || s[a] }

// authenticator resolves a request to the actor the journal will record, and
// to what that actor is allowed to do.
type authenticator struct {
	tokenHashFile string

	mu           sync.Mutex
	loggedReason string
}

// actor returns the authenticated identity and its scope, or false. A bearer
// token is checked against SHA-256 hashes on disk, so no token is ever stored;
// the proxy header is the browser path. Neither present means no identity.
func (a *authenticator) actor(r *http.Request) (string, scope, bool) {
	if token, ok := bearerToken(r); ok {
		return a.matchToken(token)
	}
	if user := strings.TrimSpace(r.Header.Get(remoteUserHeader)); user != "" {
		return user, nil, true
	}
	return "", nil, false
}

func bearerToken(r *http.Request) (string, bool) {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, prefix) {
		return "", false
	}
	token := strings.TrimSpace(strings.TrimPrefix(h, prefix))
	return token, token != ""
}

// matchToken compares the token's hash against every hash on file. A missing,
// unreadable or malformed file authenticates nobody: the file is re-read per
// request, so a scope field that stops parsing after the daemon started denies
// every bearer rather than widening one, while the proxy path — which never
// reaches here — keeps working until the operator repairs the file.
func (a *authenticator) matchToken(token string) (string, scope, bool) {
	if a.tokenHashFile == "" {
		return "", nil, false
	}
	data, err := os.ReadFile(a.tokenHashFile) // #nosec G304 -- operator-configured path
	if err != nil {
		return "", nil, false
	}
	lines, err := parseTokenHashFile(data)
	if err != nil {
		a.logRefusal(err)
		return "", nil, false
	}

	sum := sha256.Sum256([]byte(token))
	want := []byte(hex.EncodeToString(sum[:]))

	for _, line := range lines {
		if subtle.ConstantTimeCompare([]byte(line.hash), want) == 1 {
			return line.label, line.allowed, true
		}
	}
	return "", nil, false
}

// logRefusal reports a malformed file once per distinct reason: the file is
// read on every request, and one line per request would bury the log.
func (a *authenticator) logRefusal(err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.loggedReason == err.Error() {
		return
	}
	a.loggedReason = err.Error()
	slog.Error("the CLI token hash file is malformed; every bearer token is refused until it is repaired",
		"file", a.tokenHashFile, "error", err)
}

// ValidateCLITokenHashFile refuses a present but malformed token hash file at
// start-up, so a mistyped scope field is a configuration error the operator
// sees immediately rather than an authorization surprise later. An absent path
// is valid and means no CLI access; a path that cannot be read yet is left to
// the per-request read, which fails closed on its own.
func ValidateCLITokenHashFile(path string) error {
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path) // #nosec G304 -- operator-configured path
	if err != nil {
		return nil
	}
	if _, err := parseTokenHashFile(data); err != nil {
		return fmt.Errorf("cli_token_hash_file %s: %w", path, err)
	}
	return nil
}

// tokenLine is one accepted hash with the identity and the authorization it
// carries.
type tokenLine struct {
	hash    string
	label   string
	allowed scope
}

// parseTokenHashFile reads every hash line, or refuses the whole file. The
// grammar is a hash, an optional label that may contain spaces, and an
// optional trailing scope=<comma-separated actions> field; a field that looks
// like a scope field but is not exactly that — a different case, a colon,
// spaces around the equals sign, a field before the label — is an error rather
// than part of the label, because reading it as a label would silently leave
// the token unscoped.
func parseTokenHashFile(data []byte) ([]tokenLine, error) {
	var lines []tokenLine
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for number := 1; scanner.Scan(); number++ {
		text := strings.TrimSpace(scanner.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		line, err := parseTokenLine(text)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", number, err)
		}
		lines = append(lines, line)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return lines, nil
}

func parseTokenLine(text string) (tokenLine, error) {
	fields := strings.Fields(text)
	line := tokenLine{hash: strings.ToLower(fields[0]), label: cliActorFallback}
	rest := fields[1:]

	for i, field := range rest {
		if !strings.HasPrefix(strings.ToLower(field), "scope") {
			continue
		}
		if i != len(rest)-1 {
			return tokenLine{}, fmt.Errorf(
				"%q is not the last field; a scope field follows the label", field)
		}
		if !strings.HasPrefix(field, scopePrefix) {
			return tokenLine{}, fmt.Errorf(
				"%q is not a scope field; write scope=<action>[,<action>]", field)
		}
		allowed, err := parseScope(strings.TrimPrefix(field, scopePrefix))
		if err != nil {
			return tokenLine{}, err
		}
		line.allowed = allowed
		rest = rest[:i]
		break
	}

	if label := strings.Join(rest, " "); label != "" {
		line.label = label
	}
	return line, nil
}

// parseScope reads the comma-separated action names of a scope field. An
// unknown name is an error, not a narrower scope: a typo must be visible. A
// field naming nothing at all authorizes nothing.
func parseScope(field string) (scope, error) {
	allowed := scope{}
	if field == "" {
		return allowed, nil
	}
	for name := range strings.SplitSeq(field, ",") {
		if !knownActions[action(name)] {
			return nil, fmt.Errorf("%q is not an action this daemon offers", name)
		}
		allowed[action(name)] = true
	}
	return allowed, nil
}
