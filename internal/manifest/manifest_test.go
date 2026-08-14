package manifest

import (
	"strings"
	"testing"
	"time"
)

const goodYAML = `
name: web
image:
  repository: registry.example.com/acme/web
  digest: sha256:0000000000000000000000000000000000000000000000000000000000000abc
template: web.container.tmpl
port: 8080
network: acme
env:
  MODE: production
env_files:
  - /etc/acme/web.env
volumes:
  - /srv/acme/web:/data:Z
health:
  url: http://127.0.0.1:8080/healthz
  timeout: 45s
`

func lim() Limits {
	return Limits{
		VolumePrefixes:  []string{"/srv/acme/"},
		EnvFilePrefixes: []string{"/etc/acme/"},
	}
}

func valid(t *testing.T) *Manifest {
	t.Helper()
	m, err := Parse([]byte(goodYAML))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return m
}

func TestParseGoodManifest(t *testing.T) {
	m := valid(t)
	if m.Name != "web" {
		t.Errorf("Name = %q, want web", m.Name)
	}
	if m.Image.Repository != "registry.example.com/acme/web" {
		t.Errorf("Repository = %q", m.Image.Repository)
	}
	if m.Port != 8080 {
		t.Errorf("Port = %d, want 8080", m.Port)
	}
	if m.Network != "acme" {
		t.Errorf("Network = %q, want acme", m.Network)
	}
	if m.Env["MODE"] != "production" {
		t.Errorf("Env[MODE] = %q", m.Env["MODE"])
	}
	if len(m.EnvFiles) != 1 || m.EnvFiles[0] != "/etc/acme/web.env" {
		t.Errorf("EnvFiles = %v", m.EnvFiles)
	}
	if len(m.Volumes) != 1 || m.Volumes[0] != "/srv/acme/web:/data:Z" {
		t.Errorf("Volumes = %v", m.Volumes)
	}
	if m.Health.Timeout != 45*time.Second {
		t.Errorf("Health.Timeout = %v, want 45s", m.Health.Timeout)
	}
	if err := m.Validate(lim()); err != nil {
		t.Fatalf("Validate on good manifest: %v", err)
	}
}

func TestParseRejectsUnknownField(t *testing.T) {
	if _, err := Parse([]byte(goodYAML + "\nprivileged: true\n")); err == nil {
		t.Fatal("unknown field accepted")
	}
}

func TestParseAppliesDefaultTimeout(t *testing.T) {
	y := strings.Replace(goodYAML, "  timeout: 45s\n", "", 1)
	m, err := Parse([]byte(y))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if m.Health.Timeout != DefaultHealthTimeout {
		t.Errorf("Health.Timeout = %v, want default %v", m.Health.Timeout, DefaultHealthTimeout)
	}
}

func TestValidateRejectsTag(t *testing.T) {
	m := valid(t)
	m.Image.Repository = "registry.example.com/acme/web:latest"
	if err := m.Validate(lim()); err == nil {
		t.Fatal("tagged repository accepted")
	}
}

func TestValidateRejectsEmbeddedDigestInRepository(t *testing.T) {
	m := valid(t)
	m.Image.Repository = "registry.example.com/acme/web@sha256:" + strings.Repeat("a", 64)
	if err := m.Validate(lim()); err == nil {
		t.Fatal("repository with embedded digest accepted")
	}
}

func TestValidateAllowsRegistryPort(t *testing.T) {
	m := valid(t)
	m.Image.Repository = "registry.example.com:5000/acme/web"
	if err := m.Validate(lim()); err != nil {
		t.Fatalf("registry host port rejected: %v", err)
	}
}

func TestValidateRejectsBadDigest(t *testing.T) {
	for _, d := range []string{
		"",
		"latest",
		"sha256:short",
		"sha256:" + strings.Repeat("A", 64),
		"sha512:" + strings.Repeat("a", 64),
		strings.Repeat("a", 64),
	} {
		m := valid(t)
		m.Image.Digest = d
		if err := m.Validate(lim()); err == nil {
			t.Errorf("digest %q accepted", d)
		}
	}
}

func TestValidateRejectsEmptyRequiredFields(t *testing.T) {
	cases := map[string]func(*Manifest){
		"name":       func(m *Manifest) { m.Name = "" },
		"template":   func(m *Manifest) { m.Template = "" },
		"health url": func(m *Manifest) { m.Health.URL = "" },
		"repository": func(m *Manifest) { m.Image.Repository = "" },
	}
	for label, mutate := range cases {
		m := valid(t)
		mutate(m)
		if err := m.Validate(lim()); err == nil {
			t.Errorf("empty %s accepted", label)
		}
	}
}

func TestValidateRejectsBadPort(t *testing.T) {
	for _, p := range []int{0, -1, 70000} {
		m := valid(t)
		m.Port = p
		if err := m.Validate(lim()); err == nil {
			t.Errorf("port %d accepted", p)
		}
	}
}

func TestValidateAcceptsNamedQuadletVolume(t *testing.T) {
	m := valid(t)
	m.Volumes = []string{"acme-data.volume:/data"}
	if err := m.Validate(lim()); err != nil {
		t.Fatalf("named Quadlet volume rejected: %v", err)
	}
}

func TestParseKeepsDescription(t *testing.T) {
	m, err := Parse([]byte(goodYAML + "description: Acme web frontend\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if m.Description != "Acme web frontend" {
		t.Errorf("Description = %q", m.Description)
	}
	if err := m.Validate(lim()); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestDescriptionDefaultsToName(t *testing.T) {
	m := valid(t)
	if got := m.UnitDescription(); got != "web" {
		t.Errorf("UnitDescription = %q, want the name", got)
	}
	m.Description = "Acme web frontend"
	if got := m.UnitDescription(); got != "Acme web frontend" {
		t.Errorf("UnitDescription = %q", got)
	}
}

func TestValidateRejectsVolumeOutsidePrefix(t *testing.T) {
	for _, v := range []string{
		"/etc:/data:Z",
		"/srv/acme/../../etc:/data:Z",
		"named-volume:/data",
		"/srv/acmeevil/x:/data",
	} {
		m := valid(t)
		m.Volumes = []string{v}
		if err := m.Validate(lim()); err == nil {
			t.Errorf("volume %q accepted", v)
		}
	}
}

func TestValidateRejectsEnvFileOutsidePrefix(t *testing.T) {
	m := valid(t)
	m.EnvFiles = []string{"/etc/shadow"}
	if err := m.Validate(lim()); err == nil {
		t.Fatal("env file outside allowed prefix accepted")
	}
}

func TestValidateSkipsPrefixChecksWhenUnset(t *testing.T) {
	m := valid(t)
	m.Volumes = []string{"/anywhere/at/all:/data"}
	m.EnvFiles = []string{"/anywhere/at/all.env"}
	if err := m.Validate(Limits{}); err != nil {
		t.Fatalf("prefix check ran with no prefixes configured: %v", err)
	}
}

func TestValidateRejectsBadHealthURL(t *testing.T) {
	for _, u := range []string{"://nope", "ftp://127.0.0.1/x", "no-scheme/healthz"} {
		m := valid(t)
		m.Health.URL = u
		if err := m.Validate(lim()); err == nil {
			t.Errorf("health url %q accepted", u)
		}
	}
}

func TestValidateRejectsNegativeTimeout(t *testing.T) {
	m := valid(t)
	m.Health.Timeout = -time.Second
	if err := m.Validate(lim()); err == nil {
		t.Fatal("negative timeout accepted")
	}
}

const cleanUnit = `[Unit]
Description=web

[Container]
Image=registry.example.com/acme/web@sha256:abc
PublishPort=127.0.0.1:8080:8080
Environment=MODE=production

[Install]
WantedBy=default.target
`

func TestRenderedAcceptsCleanUnit(t *testing.T) {
	if err := ValidateRendered(cleanUnit); err != nil {
		t.Fatalf("clean unit rejected: %v", err)
	}
}

func TestRenderedAcceptsIPv6Loopback(t *testing.T) {
	if err := ValidateRendered("PublishPort=[::1]:8080:8080\n"); err != nil {
		t.Fatalf("ipv6 loopback publish rejected: %v", err)
	}
}

func TestRenderedRejectsPublicPublish(t *testing.T) {
	if err := ValidateRendered("PublishPort=0.0.0.0:8082:8082\n"); err == nil {
		t.Fatal("public publish accepted")
	}
}

func TestRenderedRejectsBarePublish(t *testing.T) {
	if err := ValidateRendered("PublishPort=8082:8082\n"); err == nil {
		t.Fatal("bare publish accepted")
	}
}

func TestRenderedRejectsAddCapability(t *testing.T) {
	if err := ValidateRendered("AddCapability=NET_ADMIN\n"); err == nil {
		t.Fatal("AddCapability accepted")
	}
}

func TestRenderedRejectsPrivileged(t *testing.T) {
	if err := ValidateRendered("Privileged=true\n"); err == nil {
		t.Fatal("Privileged accepted")
	}
}

func TestRenderedRejectsSpacedKeys(t *testing.T) {
	if err := ValidateRendered("  Privileged = true\n"); err == nil {
		t.Fatal("whitespace-padded Privileged accepted")
	}
}

func TestRenderedRejectsPodmanArgsEscapes(t *testing.T) {
	for _, line := range []string{
		"PodmanArgs=--privileged\n",
		"PodmanArgs=--cap-add=NET_ADMIN\n",
		"PodmanArgs=--publish 0.0.0.0:9000:9000\n",
		"PodmanArgs=--userns=host\n",
	} {
		if err := ValidateRendered(line); err == nil {
			t.Errorf("PodmanArgs escape accepted: %q", line)
		}
	}
}

func TestRenderedRejectsHostNetwork(t *testing.T) {
	if err := ValidateRendered("Network=host\n"); err == nil {
		t.Fatal("host network accepted: it defeats the loopback-only publish rule")
	}
	if err := ValidateRendered("Network=acme\n"); err != nil {
		t.Fatalf("named network rejected: %v", err)
	}
}

func TestRenderedRejectsUnpinnedImage(t *testing.T) {
	if err := ValidateRendered("Image=registry.example.com/acme/web:latest\n"); err == nil {
		t.Fatal("tag-pinned image in rendered unit accepted")
	}
}

func TestRenderedIgnoresComments(t *testing.T) {
	if err := ValidateRendered("# Privileged=true\n; AddCapability=NET_ADMIN\n"); err != nil {
		t.Fatalf("commented-out forbidden keys rejected: %v", err)
	}
}
