package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func uiServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(UI())
	t.Cleanup(srv.Close)
	return srv
}

func get(t *testing.T, srv *httptest.Server, path string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func TestRootServesTheEmbeddedIndex(t *testing.T) {
	resp := get(t, uiServer(t), "/")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET / = %d, want 200", resp.StatusCode)
	}
	body := readAll(t, resp)
	if !strings.Contains(body, `<div id="app">`) {
		t.Errorf("root did not serve the built index.html:\n%s", body)
	}
	if !strings.Contains(body, "/assets/") {
		t.Errorf("index.html references no built assets:\n%s", body)
	}
}

func TestDeepLinkFallsBackToIndex(t *testing.T) {
	resp := get(t, uiServer(t), "/service/web")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /service/web = %d, want 200 so a reload keeps working", resp.StatusCode)
	}
	if !strings.Contains(readAll(t, resp), `<div id="app">`) {
		t.Error("deep link did not fall back to index.html")
	}
}

func TestMissingAssetIsNotFound(t *testing.T) {
	resp := get(t, uiServer(t), "/assets/does-not-exist.js")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("missing asset = %d, want 404 (never a silent HTML body)", resp.StatusCode)
	}
}

func TestServerHandlerMountsTheUI(t *testing.T) {
	h := newHarness(t)
	h.api.opts.UI = UI()

	srv := httptest.NewServer(h.api.Handler())
	defer srv.Close()

	resp := get(t, srv, "/")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET / through the API mux = %d", resp.StatusCode)
	}
	if !strings.Contains(readAll(t, resp), `<div id="app">`) {
		t.Error("the API mux did not serve the UI at /")
	}
}

func TestUIDoesNotShadowTheAPI(t *testing.T) {
	h := newHarness(t)
	h.api.opts.UI = UI()

	srv := httptest.NewServer(h.api.Handler())
	defer srv.Close()

	resp := get(t, srv, "/api/v1/services")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("API route = %d, want 401; the UI must not swallow /api/v1", resp.StatusCode)
	}
}
