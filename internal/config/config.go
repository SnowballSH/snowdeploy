// Package config loads and validates the daemon's configuration. Everything
// site-specific lives here, so the binary itself carries no deployment's
// names, hosts, or paths.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/SnowballSH/snowdeploy/internal/manifest"
	"gopkg.in/yaml.v3"
)

// Defaults. The listen addresses are loopback because the daemon is always
// fronted by a proxy that does the authenticating.
const (
	DefaultListen            = "127.0.0.1:8092"
	DefaultMetricsListen     = "127.0.0.1:9105"
	DefaultBranch            = "main"
	DefaultManifestDir       = "deploy/manifests"
	DefaultTemplateDir       = "deploy/templates"
	DefaultPollInterval      = 5 * time.Minute
	DefaultDriftInterval     = 5 * time.Minute
	DefaultCheckPollInterval = 15 * time.Second
	DefaultProbeInterval     = 2 * time.Second
)

// loopbackPrefixes are the only address families the daemon may bind.
var loopbackPrefixes = []string{"127.0.0.1:", "[::1]:"}

// Config is the daemon's whole configuration.
type Config struct {
	Listen        string `yaml:"listen"`
	MetricsListen string `yaml:"metrics_listen"`

	RepoURL     string `yaml:"repo_url"`
	RepoBranch  string `yaml:"repo_branch"`
	ManifestDir string `yaml:"manifest_dir"`
	TemplateDir string `yaml:"template_dir"`
	CacheDir    string `yaml:"cache_dir"`

	UnitDir     string `yaml:"unit_dir"`
	JournalPath string `yaml:"journal_path"`

	GitHubAppID      int64  `yaml:"github_app_id"`
	GitHubInstallID  int64  `yaml:"github_install_id"`
	GitHubKeyFile    string `yaml:"github_key_file"`
	GitHubOwner      string `yaml:"github_owner"`
	GitHubRepo       string `yaml:"github_repo"`
	CLITokenHashFile string `yaml:"cli_token_hash_file"`

	PollInterval      time.Duration `yaml:"poll_interval"`
	DriftInterval     time.Duration `yaml:"drift_interval"`
	CheckPollInterval time.Duration `yaml:"check_poll_interval"`
	ProbeInterval     time.Duration `yaml:"probe_interval"`

	VolumePrefixes  []string `yaml:"volume_prefixes"`
	EnvFilePrefixes []string `yaml:"env_file_prefixes"`
}

// Limits are the host-path allowlists the manifest checks enforce.
func (c *Config) Limits() manifest.Limits {
	return manifest.Limits{
		VolumePrefixes:  c.VolumePrefixes,
		EnvFilePrefixes: c.EnvFilePrefixes,
	}
}

// Load reads, defaults, and validates a configuration file. An unknown key is
// an error: a typo must never silently disable a safety setting.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- operator-supplied path
	if err != nil {
		return nil, fmt.Errorf("read configuration: %w", err)
	}

	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)

	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse configuration %s: %w", path, err)
	}

	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("configuration %s: %w", path, err)
	}
	return &cfg, nil
}

func (c *Config) applyDefaults() {
	setString(&c.Listen, DefaultListen)
	setString(&c.MetricsListen, DefaultMetricsListen)
	setString(&c.RepoBranch, DefaultBranch)
	setString(&c.ManifestDir, DefaultManifestDir)
	setString(&c.TemplateDir, DefaultTemplateDir)
	setDuration(&c.PollInterval, DefaultPollInterval)
	setDuration(&c.DriftInterval, DefaultDriftInterval)
	setDuration(&c.CheckPollInterval, DefaultCheckPollInterval)
	setDuration(&c.ProbeInterval, DefaultProbeInterval)
}

func setString(field *string, fallback string) {
	if strings.TrimSpace(*field) == "" {
		*field = fallback
	}
}

func setDuration(field *time.Duration, fallback time.Duration) {
	if *field <= 0 {
		*field = fallback
	}
}

func (c *Config) validate() error {
	var errs []error
	fail := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf(format, args...))
	}

	if err := checkLoopback("listen", c.Listen); err != nil {
		errs = append(errs, err)
	}
	if err := checkLoopback("metrics_listen", c.MetricsListen); err != nil {
		errs = append(errs, err)
	}
	if c.Listen == c.MetricsListen {
		fail("listen and metrics_listen must differ (both %s)", c.Listen)
	}

	for field, value := range map[string]string{
		"repo_url":        c.RepoURL,
		"github_key_file": c.GitHubKeyFile,
		"github_owner":    c.GitHubOwner,
		"github_repo":     c.GitHubRepo,
	} {
		if strings.TrimSpace(value) == "" {
			fail("%s is required", field)
		}
	}
	if c.GitHubAppID == 0 {
		fail("github_app_id is required")
	}
	if c.GitHubInstallID == 0 {
		fail("github_install_id is required")
	}

	for _, p := range c.VolumePrefixes {
		if !strings.HasPrefix(p, "/") {
			fail("volume_prefixes entry %q is not an absolute path", p)
		}
	}
	for _, p := range c.EnvFilePrefixes {
		if !strings.HasPrefix(p, "/") {
			fail("env_file_prefixes entry %q is not an absolute path", p)
		}
	}

	return errors.Join(errs...)
}

// checkLoopback refuses any listener that could be reached from off-host. The
// authenticating proxy is the only intended client.
func checkLoopback(field, addr string) error {
	for _, prefix := range loopbackPrefixes {
		if strings.HasPrefix(addr, prefix) {
			return nil
		}
	}
	return fmt.Errorf(
		"%s %q is not loopback (it must start with %q or %q)",
		field, addr, loopbackPrefixes[0], loopbackPrefixes[1])
}
