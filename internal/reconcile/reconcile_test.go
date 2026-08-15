package reconcile

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SnowballSH/snowdeploy/internal/manifest"
)

const safeTemplate = `[Unit]
Description={{.Name}}

[Container]
Image={{.Image.Repository}}@{{.Image.Digest}}
PublishPort=127.0.0.1:{{.Port}}:{{.Port}}
Network={{.Network}}
{{- range $k, $v := .Env}}
Environment={{$k}}={{$v}}
{{- end}}
{{- range .EnvFiles}}
EnvironmentFile={{.}}
{{- end}}
{{- range .Volumes}}
Volume={{.}}
{{- end}}

[Install]
WantedBy=default.target
`

// escapingTemplate renders a unit the manifest checks cannot see: the manifest
// is clean, the rendered unit is not.
const escapingTemplate = safeTemplate + "Privileged=true\n"

const testDigest = "sha256:0000000000000000000000000000000000000000000000000000000000000abc"

func testManifest() *manifest.Manifest {
	return &manifest.Manifest{
		Name:     "web",
		Image:    manifest.Image{Repository: "registry.example.com/acme/web", Digest: testDigest},
		Template: "web.container.tmpl",
		Port:     8080,
		Network:  "acme",
		Env:      map[string]string{"MODE": "production"},
		EnvFiles: []string{"/etc/acme/web.env"},
		Volumes:  []string{"/srv/acme/web:/data:Z"},
		Health:   manifest.Health{URL: "http://127.0.0.1:8080/healthz", Timeout: time.Second},
	}
}

// fakeSystemd records what the applier asked the host to do, in order.
type fakeSystemd struct {
	mu       sync.Mutex
	calls    []string
	units    map[string]string
	running  map[string]string
	writeErr error
	restErr  error
}

func newFakeSystemd() *fakeSystemd {
	return &fakeSystemd{units: map[string]string{}, running: map[string]string{}}
}

func (f *fakeSystemd) WriteUnit(name, content string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "WriteUnit:"+name)
	if f.writeErr != nil {
		return f.writeErr
	}
	f.units[name] = content
	return nil
}

func (f *fakeSystemd) ReloadAndRestart(_ context.Context, service string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "ReloadAndRestart:"+service)
	return f.restErr
}

func (f *fakeSystemd) RunningImage(_ context.Context, service string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "RunningImage:"+service)
	d, ok := f.running[service]
	if !ok {
		return "", errors.New("not running")
	}
	return d, nil
}

func (f *fakeSystemd) recorded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func TestRenderSubstitutesManifestFields(t *testing.T) {
	unit, err := Render(safeTemplate, testManifest())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	for _, want := range []string{
		"Image=registry.example.com/acme/web@" + testDigest,
		"PublishPort=127.0.0.1:8080:8080",
		"Network=acme",
		"Environment=MODE=production",
		"EnvironmentFile=/etc/acme/web.env",
		"Volume=/srv/acme/web:/data:Z",
		"Description=web",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("rendered unit missing %q:\n%s", want, unit)
		}
	}
}

func TestRenderRejectsMissingKey(t *testing.T) {
	if _, err := Render("Image={{.NoSuchField}}\n", testManifest()); err == nil {
		t.Fatal("template referencing an unknown field rendered")
	}
}

func TestRenderRejectsUnparseableTemplate(t *testing.T) {
	if _, err := Render("{{.Name", testManifest()); err == nil {
		t.Fatal("malformed template rendered")
	}
}

// This is the rendered-unit safety gate proving red: a template that escapes
// the manifest rules must be caught before anything reaches the host.
func TestApplyRefusesEscapingTemplateBeforeWriting(t *testing.T) {
	f := newFakeSystemd()
	a := &Applier{S: f}

	err := a.Apply(t.Context(), testManifest(), escapingTemplate, nil)
	if err == nil {
		t.Fatal("Apply accepted a template that renders Privileged=true")
	}
	if !strings.Contains(err.Error(), "Privileged") {
		t.Errorf("error does not name the offending key: %v", err)
	}
	if calls := f.recorded(); len(calls) != 0 {
		t.Fatalf("host was touched despite the gate: %v", calls)
	}
}

func TestApplyRefusesNonLoopbackPublishBeforeWriting(t *testing.T) {
	f := newFakeSystemd()
	a := &Applier{S: f}

	tmpl := "[Container]\nImage={{.Image.Repository}}@{{.Image.Digest}}\n" +
		"PublishPort=0.0.0.0:{{.Port}}:{{.Port}}\n"
	if err := a.Apply(t.Context(), testManifest(), tmpl, nil); err == nil {
		t.Fatal("Apply accepted a public publish")
	}
	if calls := f.recorded(); len(calls) != 0 {
		t.Fatalf("host was touched despite the gate: %v", calls)
	}
}

func TestApplyHappyPathCallOrder(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	m := testManifest()
	m.Health.URL = srv.URL + "/healthz"

	f := newFakeSystemd()
	a := &Applier{S: f}

	if err := a.Apply(t.Context(), m, safeTemplate, nil); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	want := []string{"WriteUnit:web.container", "ReloadAndRestart:web"}
	got := f.recorded()
	if len(got) != len(want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("call %d = %q, want %q", i, got[i], want[i])
		}
	}
	if !strings.Contains(f.units["web.container"], "Image=registry.example.com/acme/web@"+testDigest) {
		t.Errorf("written unit is not the rendered one:\n%s", f.units["web.container"])
	}
}

func TestApplyStopsWhenRestartFails(t *testing.T) {
	f := newFakeSystemd()
	f.restErr = errors.New("unit failed to start")
	a := &Applier{S: f}

	m := testManifest()
	m.Health.URL = "http://127.0.0.1:1/healthz"

	err := a.Apply(t.Context(), m, safeTemplate, nil)
	if err == nil {
		t.Fatal("Apply succeeded despite a failed restart")
	}
	if !strings.Contains(err.Error(), "unit failed to start") {
		t.Errorf("error = %v", err)
	}
}

func TestProbeSucceedsAfterTransientFailures(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if hits.Add(1) <= 2 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if err := Probe(t.Context(), srv.URL, 3*time.Second, 5*time.Millisecond); err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if hits.Load() < 3 {
		t.Errorf("probe gave up after %d attempts", hits.Load())
	}
}

func TestProbeFailsAtDeadline(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	err := Probe(t.Context(), srv.URL, 60*time.Millisecond, 5*time.Millisecond)
	if err == nil {
		t.Fatal("Probe succeeded against a permanently failing endpoint")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error does not carry the last status: %v", err)
	}
}

func TestProbeFailsWhenUnreachable(t *testing.T) {
	if err := Probe(t.Context(), "http://127.0.0.1:1/healthz",
		50*time.Millisecond, 5*time.Millisecond); err == nil {
		t.Fatal("Probe succeeded against a closed port")
	}
}

func TestContainerNamesTriesTheBareNameFirst(t *testing.T) {
	got := ContainerNames("portfolio")
	want := []string{"portfolio", "systemd-portfolio"}
	if len(got) != len(want) {
		t.Fatalf("ContainerNames = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ContainerNames[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	// A unit that sets ContainerName= uses the bare name, and every unit this
	// was written against does. Trying only the prefixed form reports every
	// service as not running, which reads as total drift.
	if got[0] != "portfolio" {
		t.Errorf("the bare ContainerName= form must be tried first, got %q", got[0])
	}
}

func TestUnitNameDerivesFromService(t *testing.T) {
	if got := UnitName("web"); got != "web.container" {
		t.Errorf("UnitName = %q, want web.container", got)
	}
}
