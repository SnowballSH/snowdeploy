package reconcile

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/SnowballSH/snowdeploy/internal/manifest"
)

// manifestDefaultTimeout mirrors the manifest package's probe default so a
// hand-built manifest still gets a bounded probe.
const manifestDefaultTimeout = manifest.DefaultHealthTimeout

// commandTimeout bounds each systemctl or podman invocation.
const commandTimeout = 2 * time.Minute

// Systemd is the host side of a reconcile. The real implementation drives the
// caller's own `systemd --user` instance; it never touches system units.
type Systemd interface {
	WriteUnit(name, content string) error
	ReloadAndRestart(ctx context.Context, service string) error
	RunningImage(ctx context.Context, service string) (string, error)
}

type userSystemd struct {
	unitDir string
}

// NewUserSystemd drives `systemctl --user` against unitDir.
func NewUserSystemd(unitDir string) Systemd {
	return &userSystemd{unitDir: unitDir}
}

// WriteUnit writes the Quadlet file durably: a torn unit file after a crash
// would be a service that cannot start.
func (s *userSystemd) WriteUnit(name, content string) error {
	if strings.Contains(name, "/") || strings.Contains(name, "..") {
		return fmt.Errorf("unit name %q is not a plain file name", name)
	}
	if err := os.MkdirAll(s.unitDir, 0o750); err != nil {
		return fmt.Errorf("create unit directory: %w", err)
	}

	final := filepath.Join(s.unitDir, name)
	tmp, err := os.CreateTemp(s.unitDir, "."+name+".*")
	if err != nil {
		return fmt.Errorf("create temporary unit: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.WriteString(content); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write unit: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync unit: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close unit: %w", err)
	}
	// The unit is read by this user's own systemd generator and by podman
	// running as this user, so it never needs to be group- or world-readable.
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return fmt.Errorf("chmod unit: %w", err)
	}
	if err := os.Rename(tmpName, final); err != nil {
		return fmt.Errorf("install unit: %w", err)
	}
	return syncDir(s.unitDir)
}

func syncDir(dir string) error {
	d, err := os.Open(dir) // #nosec G304 -- operator-configured unit directory
	if err != nil {
		return fmt.Errorf("open unit directory: %w", err)
	}
	defer func() { _ = d.Close() }()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("sync unit directory: %w", err)
	}
	return nil
}

func (s *userSystemd) ReloadAndRestart(ctx context.Context, service string) error {
	if err := run(ctx, "systemctl", "--user", "daemon-reload"); err != nil {
		return err
	}
	return run(ctx, "systemctl", "--user", "restart", service+".service")
}

func (s *userSystemd) RunningImage(ctx context.Context, service string) (string, error) {
	out, err := output(ctx, "podman", "inspect",
		"--format", "{{.ImageDigest}}", "systemd-"+service)
	if err != nil {
		return "", err
	}
	digest := strings.TrimSpace(out)
	if digest == "" {
		return "", fmt.Errorf("podman reported no image digest for %s", service)
	}
	return digest, nil
}

func run(ctx context.Context, name string, args ...string) error {
	_, err := output(ctx, name, args...)
	return err
}

func output(ctx context.Context, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, name, args...) // #nosec G204 -- fixed binaries, validated names
	out, err := cmd.Output()
	if err == nil {
		return string(out), nil
	}

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return "", fmt.Errorf("%s %s: %w: %s",
			name, strings.Join(args, " "), err, strings.TrimSpace(string(exitErr.Stderr)))
	}
	return "", fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
}
