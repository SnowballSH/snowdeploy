package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const minimal = `
repo_url: https://example.com/acme/config
github_app_id: 123
github_install_id: 456
github_key_file: /run/snowdeploy/app.pem
github_owner: acme
github_repo: config
`

func write(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestLoadAppliesDefaults(t *testing.T) {
	cfg, err := Load(write(t, minimal))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Listen != "127.0.0.1:8092" {
		t.Errorf("Listen = %q", cfg.Listen)
	}
	if cfg.MetricsListen != "127.0.0.1:9105" {
		t.Errorf("MetricsListen = %q", cfg.MetricsListen)
	}
	if cfg.RepoBranch != "main" {
		t.Errorf("RepoBranch = %q", cfg.RepoBranch)
	}
	if cfg.ManifestDir != "deploy/manifests" {
		t.Errorf("ManifestDir = %q", cfg.ManifestDir)
	}
	if cfg.TemplateDir != "deploy/templates" {
		t.Errorf("TemplateDir = %q", cfg.TemplateDir)
	}
	if cfg.PollInterval <= 0 {
		t.Errorf("PollInterval = %v", cfg.PollInterval)
	}
	if cfg.DriftInterval <= 0 {
		t.Errorf("DriftInterval = %v", cfg.DriftInterval)
	}
	if cfg.CheckPollInterval <= 0 {
		t.Errorf("CheckPollInterval = %v", cfg.CheckPollInterval)
	}
}

func TestLoadParsesDurations(t *testing.T) {
	cfg, err := Load(write(t, minimal+"poll_interval: 90s\ndrift_interval: 10m\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.PollInterval != 90*time.Second {
		t.Errorf("PollInterval = %v, want 90s", cfg.PollInterval)
	}
	if cfg.DriftInterval != 10*time.Minute {
		t.Errorf("DriftInterval = %v, want 10m", cfg.DriftInterval)
	}
}

func TestLoadRejectsUnknownKey(t *testing.T) {
	_, err := Load(write(t, minimal+"allow_root: true\n"))
	if err == nil {
		t.Fatal("unknown key accepted")
	}
	if !strings.Contains(err.Error(), "allow_root") {
		t.Errorf("error should name the key: %v", err)
	}
}

func TestLoadRejectsMissingRequiredFields(t *testing.T) {
	cases := map[string]string{
		"repo_url":          strings.Replace(minimal, "repo_url: https://example.com/acme/config\n", "", 1),
		"github_key_file":   strings.Replace(minimal, "github_key_file: /run/snowdeploy/app.pem\n", "", 1),
		"github_app_id":     strings.Replace(minimal, "github_app_id: 123\n", "", 1),
		"github_install_id": strings.Replace(minimal, "github_install_id: 456\n", "", 1),
		"github_owner":      strings.Replace(minimal, "github_owner: acme\n", "", 1),
		"github_repo":       strings.Replace(minimal, "github_repo: config\n", "", 1),
	}
	for field, body := range cases {
		if _, err := Load(write(t, body)); err == nil {
			t.Errorf("missing %s accepted", field)
		}
	}
}

func TestListenAddressesMustBeLoopback(t *testing.T) {
	for _, addr := range []string{
		"0.0.0.0:8092", "8092", ":8092", "192.168.1.5:8092", "example.com:8092",
	} {
		if _, err := Load(write(t, minimal+"listen: "+addr+"\n")); err == nil {
			t.Errorf("listen %q accepted; the API must never leave loopback", addr)
		}
		if _, err := Load(write(t, minimal+"metrics_listen: "+addr+"\n")); err == nil {
			t.Errorf("metrics_listen %q accepted", addr)
		}
	}
	for _, addr := range []string{"127.0.0.1:8092", `"[::1]:8092"`} {
		if _, err := Load(write(t, minimal+"listen: "+addr+"\n")); err != nil {
			t.Errorf("loopback listen %q rejected: %v", addr, err)
		}
	}
}

func TestListenAddressesMustDiffer(t *testing.T) {
	body := minimal + "listen: 127.0.0.1:8092\nmetrics_listen: 127.0.0.1:8092\n"
	if _, err := Load(write(t, body)); err == nil {
		t.Fatal("the API and metrics listeners were allowed to share a port")
	}
}

func TestVolumePrefixesMustBeAbsolute(t *testing.T) {
	body := minimal + "volume_prefixes:\n  - relative/path\n"
	if _, err := Load(write(t, body)); err == nil {
		t.Fatal("relative volume prefix accepted")
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "absent.yaml")); err == nil {
		t.Fatal("missing config file accepted")
	}
}

func TestLimitsCarryBothPrefixSets(t *testing.T) {
	body := minimal +
		"volume_prefixes:\n  - /srv/acme/\n" +
		"env_file_prefixes:\n  - /etc/acme/\n"
	cfg, err := Load(write(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	lim := cfg.Limits()
	if len(lim.VolumePrefixes) != 1 || lim.VolumePrefixes[0] != "/srv/acme/" {
		t.Errorf("VolumePrefixes = %v", lim.VolumePrefixes)
	}
	if len(lim.EnvFilePrefixes) != 1 || lim.EnvFilePrefixes[0] != "/etc/acme/" {
		t.Errorf("EnvFilePrefixes = %v", lim.EnvFilePrefixes)
	}
}

func TestExampleConfigIsLoadable(t *testing.T) {
	cfg, err := Load("../../example.config.yaml")
	if err != nil {
		t.Fatalf("the shipped example configuration does not load: %v", err)
	}
	if cfg.Listen != "127.0.0.1:8092" || cfg.MetricsListen != "127.0.0.1:9105" {
		t.Errorf("example listens on %s / %s", cfg.Listen, cfg.MetricsListen)
	}
}
