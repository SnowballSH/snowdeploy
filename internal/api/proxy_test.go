package api

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testProxySecret = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func proxied(name string) map[string]string {
	return map[string]string{ProxySecretHeader: testProxySecret, "Remote-User": name}
}

// doRaw sends a request whose headers may repeat, which the map-based helper
// cannot express.
func (h *harness) doRaw(t *testing.T, method, path, body string, header http.Header) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, h.srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header = header
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func (h *harness) mustNotReachTheEngine(t *testing.T) {
	t.Helper()
	if calls := h.engine.recorded(); len(calls) != 0 {
		t.Fatalf("a refused request reached the engine: %+v", calls)
	}
}

const deployBody = `{"digest":"` + digestLatest + `"}`

func TestForgedRemoteUserWithoutTheProxySecretIsRejected(t *testing.T) {
	h := newHarness(t)
	for _, route := range []struct{ method, path, body string }{
		{http.MethodPost, "/api/v1/services/web/deploy", deployBody},
		{http.MethodPost, "/api/v1/services/web/rollback", ""},
		{http.MethodPost, "/api/v1/services/web/converge", ""},
		{http.MethodGet, "/api/v1/services/web/history", ""},
		{http.MethodGet, "/api/v1/services", ""},
		{http.MethodGet, "/api/v1/services/web/revisions?digests=" + digestLatest, ""},
		{http.MethodGet, "/api/v1/events", ""},
	} {
		resp := h.do(t, route.method, route.path, route.body, map[string]string{"Remote-User": "admin"})
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s with a bare Remote-User = %d, want 401",
				route.method, route.path, resp.StatusCode)
		}
	}
	h.mustNotReachTheEngine(t)
}

func TestProxyIdentityBecomesTheActor(t *testing.T) {
	h := newHarness(t)
	resp := h.do(t, http.MethodPost, "/api/v1/services/web/deploy", deployBody, proxied("admin"))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("deploy through the proxy = %d, want 202", resp.StatusCode)
	}
	if calls := h.engine.recorded(); len(calls) != 1 || calls[0].Actor != "admin" {
		t.Fatalf("actor not attributed: %+v", calls)
	}
}

func TestWrongProxySecretIsRejected(t *testing.T) {
	h := newHarness(t)
	headers := proxied("admin")
	headers[ProxySecretHeader] = strings.Repeat("f", len(testProxySecret))
	resp := h.do(t, http.MethodPost, "/api/v1/services/web/deploy", deployBody, headers)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong proxy secret = %d, want 401", resp.StatusCode)
	}
	h.mustNotReachTheEngine(t)
}

func TestRepeatedProxyHeadersAreRejected(t *testing.T) {
	for name, header := range map[string]http.Header{
		"secret twice": {
			ProxySecretHeader: {testProxySecret, testProxySecret},
			"Remote-User":     {"admin"},
		},
		"identity twice": {
			ProxySecretHeader: {testProxySecret},
			"Remote-User":     {"admin", "admin"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			resp := h.doRaw(t, http.MethodPost, "/api/v1/services/web/deploy", deployBody, header)
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("%s = %d, want 401", name, resp.StatusCode)
			}
			h.mustNotReachTheEngine(t)
		})
	}
}

func TestIdentityOutsideTheAllowlistIsRejected(t *testing.T) {
	h := newHarness(t)
	for _, name := range []string{"", "family", "Admin", "admin, family"} {
		resp := h.do(t, http.MethodPost, "/api/v1/services/web/deploy", deployBody, proxied(name))
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("Remote-User %q = %d, want 401", name, resp.StatusCode)
		}
	}
	h.mustNotReachTheEngine(t)
}

func TestProxySecretWithoutAnIdentityIsRejected(t *testing.T) {
	h := newHarness(t)
	resp := h.do(t, http.MethodPost, "/api/v1/services/web/deploy", deployBody,
		map[string]string{ProxySecretHeader: testProxySecret})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("secret without Remote-User = %d, want 401", resp.StatusCode)
	}
	h.mustNotReachTheEngine(t)
}

func TestWithoutAProxyBoundaryOnlyBearersAuthenticate(t *testing.T) {
	h := newHarness(t)
	h.api.auth.proxy = nil

	resp := h.do(t, http.MethodPost, "/api/v1/services/web/deploy", deployBody, proxied("admin"))
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("proxy headers with no boundary configured = %d, want 401", resp.StatusCode)
	}
	h.mustNotReachTheEngine(t)

	resp = h.do(t, http.MethodPost, "/api/v1/services/web/deploy", deployBody, bearer(h.token))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("bearer with no boundary configured = %d, want 202", resp.StatusCode)
	}
}

func TestABearerIsJudgedAloneEvenBesideValidProxyHeaders(t *testing.T) {
	h := newHarness(t)
	headers := proxied("admin")
	headers["Authorization"] = "Bearer not-the-token"
	resp := h.do(t, http.MethodPost, "/api/v1/services/web/deploy", deployBody, headers)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong bearer beside valid proxy headers = %d, want 401", resp.StatusCode)
	}
	h.mustNotReachTheEngine(t)
}

func TestNewProxyBoundaryRefusesAWeakConfiguration(t *testing.T) {
	for name, tc := range map[string]struct {
		secret  string
		allowed []string
	}{
		"short secret":       {strings.Repeat("a", MinProxySecretBytes-1), []string{"admin"}},
		"empty allowlist":    {testProxySecret, nil},
		"blank identity":     {testProxySecret, []string{"admin", ""}},
		"padded identity":    {testProxySecret, []string{" admin"}},
		"identity with list": {testProxySecret, []string{"admin,family"}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewProxyBoundary([]byte(tc.secret), tc.allowed); err == nil {
				t.Fatalf("NewProxyBoundary accepted %s", name)
			}
		})
	}
}

func TestLoadProxySecret(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o400); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		return path
	}

	secret, err := LoadProxySecret(write("newline", testProxySecret+"\n"))
	if err != nil {
		t.Fatalf("LoadProxySecret: %v", err)
	}
	if string(secret) != testProxySecret {
		t.Fatalf("the trailing newline was not trimmed")
	}

	if _, err := LoadProxySecret(write("short", "too-short\n")); err == nil {
		t.Fatal("a short secret was accepted")
	}
	if _, err := LoadProxySecret(filepath.Join(dir, "absent")); err == nil {
		t.Fatal("an absent file was accepted")
	}
}
