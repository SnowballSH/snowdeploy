// Command snowdeployd is the host deploy daemon. It watches a configuration
// repository and a registry, and turns an operator's one-click deploy into a
// pull request, a merge, a rendered Quadlet unit, a restart, a health probe,
// and a receipt — rolling back automatically when the probe fails.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/SnowballSH/snowdeploy/internal/api"
	"github.com/SnowballSH/snowdeploy/internal/config"
	"github.com/SnowballSH/snowdeploy/internal/deploy"
	"github.com/SnowballSH/snowdeploy/internal/gitops"
	"github.com/SnowballSH/snowdeploy/internal/journal"
	"github.com/SnowballSH/snowdeploy/internal/reconcile"
	"github.com/SnowballSH/snowdeploy/internal/registry"
	"github.com/SnowballSH/snowdeploy/internal/revision"
	"golang.org/x/sync/errgroup"
)

var version = "dev"

// shutdownGrace bounds how long the listeners get to drain.
const shutdownGrace = 15 * time.Second

func main() {
	if err := run(); err != nil {
		slog.Error("snowdeployd exited", "error", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		configPath  = flag.String("config", "/etc/snowdeploy/config.yaml", "configuration file")
		showVersion = flag.Bool("version", false, "print the version and exit")
	)
	flag.Parse()

	if *showVersion {
		_, err := fmt.Fprintf(os.Stdout, "snowdeployd %s\n", version)
		return err
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	if err := api.ValidateCLITokenHashFile(cfg.CLITokenHashFile); err != nil {
		return err
	}
	proxy, err := proxyBoundary(cfg)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(),
		os.Interrupt, syscall.SIGTERM)
	defer stop()

	d, err := build(ctx, cfg, proxy)
	if err != nil {
		return err
	}
	defer d.close()

	slog.Info("snowdeployd starting",
		"version", version, "listen", cfg.Listen, "metrics", cfg.MetricsListen)
	return d.serve(ctx, cfg)
}

type daemon struct {
	jrnl    *journal.Journal
	repo    *gitops.Repo
	engine  *deploy.Engine
	server  *api.Server
	watcher *registry.Watcher
}

func (d *daemon) close() {
	d.engine.Wait()
	if err := d.jrnl.Close(); err != nil {
		slog.Error("closing the journal", "error", err)
	}
}

func build(ctx context.Context, cfg *config.Config, proxy *api.ProxyBoundary) (*daemon, error) {
	if err := ensureDir(filepath.Dir(cfg.JournalPath)); err != nil {
		return nil, err
	}
	jrnl, err := journal.Open(cfg.JournalPath)
	if err != nil {
		return nil, err
	}
	if err := failOrphanedDeploys(jrnl); err != nil {
		_ = jrnl.Close()
		return nil, err
	}

	pr, tokenFn, err := gitops.NewGitHubApp(gitops.GitHubAppConfig{
		AppID:          cfg.GitHubAppID,
		InstallationID: cfg.GitHubInstallID,
		KeyPath:        cfg.GitHubKeyFile,
		Owner:          cfg.GitHubOwner,
		Repo:           cfg.GitHubRepo,
		ManifestDir:    cfg.ManifestDir,
		BaseBranch:     cfg.RepoBranch,
	})
	if err != nil {
		_ = jrnl.Close()
		return nil, err
	}

	repo := gitops.NewRepo(gitops.RepoConfig{
		URL:         cfg.RepoURL,
		Branch:      cfg.RepoBranch,
		CacheDir:    cfg.CacheDir,
		ManifestDir: cfg.ManifestDir,
		TemplateDir: cfg.TemplateDir,
		TokenFn:     tokenFn,
	})

	systemd := reconcile.NewUserSystemd(cfg.UnitDir)
	applier := &reconcile.Applier{
		S:             systemd,
		Lim:           cfg.Limits(),
		ProbeInterval: cfg.ProbeInterval,
	}
	watcher := registry.NewWatcher(registry.NewOCI(), cfg.PollInterval)

	repoWebURL := "https://github.com/" + cfg.GitHubOwner + "/" + cfg.GitHubRepo

	var server *api.Server
	engine := deploy.New(ctx, deploy.Options{
		Repo:      repo,
		PR:        pr,
		Applier:   applier,
		Journal:   jrnl,
		Inspector: systemd,
		Notify: func(ev deploy.Event) {
			if server != nil {
				server.Publish(ev)
			}
		},
		CheckPoll:  cfg.CheckPollInterval,
		RepoWebURL: repoWebURL,
	})
	revisions := revision.NewOCI()
	server = api.New(api.Options{
		Engine:           engine,
		Repo:             repo,
		Inspector:        systemd,
		Watcher:          watcher,
		History:          jrnl,
		CLITokenHashFile: cfg.CLITokenHashFile,
		Proxy:            proxy,
		UI:               api.UI(),
		Revisions:        revisions,
		RepoWebURL:       repoWebURL,
	})

	// A registry that stops answering offers no deploy, which on its own is
	// indistinguishable from having nothing new to offer. The counter is what
	// an alert reads; the log line is what tells the operator why.
	watcher.SetObserver(func(repository string, err error) {
		server.RecordRegistryPoll(repository, err)
		if err != nil {
			slog.Warn("registry poll failed; no new digest can be offered for this repository",
				"repository", repository, "error", err)
			return
		}
		// Warm the revision cache as digests are discovered, so the commit
		// behind a digest is usually known before any browser asks. The
		// source deduplicates in-flight fills and caches success forever, so
		// re-polling an unchanged digest costs nothing; the answer here is
		// deliberately discarded — caching it was the point.
		if digest, ok := watcher.Latest(repository); ok {
			go revisions.Lookup(ctx, repository, digest)
		}
	})

	// The first sync is best-effort: a daemon that cannot reach GitHub must
	// still start and keep serving reads, per the sealed-store posture.
	if _, err := repo.Sync(ctx); err != nil {
		slog.Warn("initial configuration sync failed; deploys will fail until it succeeds",
			"error", err)
	}
	return &daemon{
		jrnl: jrnl, repo: repo, engine: engine, server: server, watcher: watcher,
	}, nil
}

// proxyBoundary opens the browser path only when the configuration asks for
// it, and refuses to start on a secret it cannot read rather than quietly
// leaving the path closed. The secret is read once: rotating it is a restart.
func proxyBoundary(cfg *config.Config) (*api.ProxyBoundary, error) {
	if cfg.ProxySecretFile == "" {
		slog.Warn("no proxy_secret_file is configured: the Remote-User browser path is closed " +
			"and only bearer tokens authenticate")
		return nil, nil
	}
	secret, err := api.LoadProxySecret(cfg.ProxySecretFile)
	if err != nil {
		return nil, fmt.Errorf("proxy_secret_file: %w", err)
	}
	boundary, err := api.NewProxyBoundary(secret, cfg.ProxyAllowedUsers)
	if err != nil {
		return nil, fmt.Errorf("proxy_allowed_users: %w", err)
	}
	return boundary, nil
}

// failOrphanedDeploys closes out journal rows a previous process left
// in flight. No goroutine in this process is driving them, but the UI seeds a
// service's live state from its newest row — so a row stuck in a non-terminal
// state would read as a deploy in progress forever and lock the service.
func failOrphanedDeploys(jrnl *journal.Journal) error {
	orphans, err := jrnl.Unfinished()
	if err != nil {
		return err
	}
	for _, e := range orphans {
		detail := fmt.Sprintf("the daemon restarted while this deploy was in %q", e.State)
		if e.PRNumber > 0 {
			detail += fmt.Sprintf("; PR #%d may still be open — check it and re-run", e.PRNumber)
		} else {
			detail += "; re-run"
		}
		if err := jrnl.Finish(e.ID, journal.StateFailed, detail); err != nil {
			return fmt.Errorf("fail orphaned deploy %d: %w", e.ID, err)
		}
		slog.Warn("closed out a deploy orphaned by an earlier process",
			"journalId", e.ID, "service", e.Service, "state", e.State, "pr", e.PRNumber)
	}
	return nil
}

func (d *daemon) serve(ctx context.Context, cfg *config.Config) error {
	apiServer := &http.Server{
		Addr:              cfg.Listen,
		Handler:           d.server.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	metricsServer := &http.Server{
		Addr:              cfg.MetricsListen,
		Handler:           d.server.MetricsHandler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	group, gctx := errgroup.WithContext(ctx)
	group.Go(func() error { return listen(apiServer, "api") })
	group.Go(func() error { return listen(metricsServer, "metrics") })
	group.Go(func() error {
		d.watcher.Run(gctx, d.repositories)
		return nil
	})
	group.Go(func() error {
		d.watchDrift(gctx, cfg.DriftInterval)
		return nil
	})
	group.Go(func() error {
		<-gctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()
		return errors.Join(
			apiServer.Shutdown(shutdownCtx),
			metricsServer.Shutdown(shutdownCtx),
		)
	})

	err := group.Wait()
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

func listen(s *http.Server, name string) error {
	if err := s.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("%s listener: %w", name, err)
	}
	return nil
}

// repositories are the image repositories the registry watcher polls, read
// from the merged manifests each tick so a manifest change needs no restart.
func (d *daemon) repositories() []string {
	services, err := d.repo.Services()
	if err != nil {
		return nil
	}
	var repos []string
	for _, service := range services {
		m, _, err := d.repo.Manifest(service)
		if err != nil {
			continue
		}
		repos = append(repos, m.Image.Repository)
	}
	return repos
}

// watchDrift keeps the drift gauges current. Per D036 nothing is pushed: the
// gauge is what an alert rule and the UI read.
func (d *daemon) watchDrift(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	check := func() {
		drift, err := d.engine.Drift(ctx)
		if err != nil {
			slog.Warn("drift check failed", "error", err)
			return
		}
		d.server.SetDrift(drift)
		for service, detail := range drift {
			slog.Warn("service drifted from its merged manifest",
				"service", service, "detail", detail)
		}
	}

	check()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			check()
		}
	}
}

func ensureDir(dir string) error {
	if dir == "" || dir == "." {
		return nil
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	return nil
}
