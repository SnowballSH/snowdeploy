package api

import (
	"maps"
	"net/http"
	"net/url"
	"testing"
)

type mutatingRoute struct {
	path string
	body string
}

var mutatingRoutes = []mutatingRoute{
	{"/api/v1/services/web/deploy", `{"digest":"` + digestLatest + `"}`},
	{"/api/v1/services/web/rollback", ``},
	{"/api/v1/services/web/converge", ``},
}

func withHeaders(base map[string]string, extra map[string]string) map[string]string {
	out := maps.Clone(base)
	maps.Copy(out, extra)
	return out
}

func (h *harness) origin(t *testing.T) string {
	t.Helper()
	u, err := url.Parse(h.srv.URL)
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}
	return u.Scheme + "://" + u.Host
}

func TestBrowserCrossOriginMutationsAreRefused(t *testing.T) {
	for _, tc := range []struct {
		name    string
		headers map[string]string
	}{
		{"same-site", map[string]string{"Sec-Fetch-Site": "same-site"}},
		{"cross-site", map[string]string{"Sec-Fetch-Site": "cross-site"}},
		{"foreign origin without fetch metadata", map[string]string{"Origin": "https://chat.snowballsh.com"}},
		{"opaque origin without fetch metadata", map[string]string{"Origin": "null"}},
	} {
		for _, route := range mutatingRoutes {
			t.Run(tc.name+" "+route.path, func(t *testing.T) {
				h := newHarness(t)
				resp := h.do(t, http.MethodPost, route.path, route.body,
					withHeaders(proxied("admin"), tc.headers))
				if resp.StatusCode != http.StatusForbidden {
					t.Errorf("POST %s = %d, want 403", route.path, resp.StatusCode)
				}
				if calls := h.engine.recorded(); len(calls) != 0 {
					t.Fatalf("a cross-origin request reached the engine: %v", calls)
				}
			})
		}
	}
}

func TestCrossOriginRefusalHoldsForBearerRequestsToo(t *testing.T) {
	h := newHarness(t)
	resp := h.do(t, http.MethodPost, "/api/v1/services/web/converge", ``,
		withHeaders(bearer(h.token), map[string]string{"Sec-Fetch-Site": "cross-site"}))
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("cross-site bearer converge = %d, want 403", resp.StatusCode)
	}
}

func TestSameOriginAndNonBrowserMutationsAreAccepted(t *testing.T) {
	for _, tc := range []struct {
		name    string
		headers func(h *harness) map[string]string
	}{
		{"same-origin UI", func(*harness) map[string]string {
			return withHeaders(proxied("admin"), map[string]string{"Sec-Fetch-Site": "same-origin"})
		}},
		{"user-initiated navigation", func(*harness) map[string]string {
			return withHeaders(proxied("admin"), map[string]string{"Sec-Fetch-Site": "none"})
		}},
		{"matching origin without fetch metadata", func(h *harness) map[string]string {
			return withHeaders(proxied("admin"), map[string]string{"Origin": h.origin(t)})
		}},
		{"CLI bearer", func(h *harness) map[string]string { return bearer(h.token) }},
		{"proxied tooling", func(*harness) map[string]string { return proxied("admin") }},
	} {
		for _, route := range mutatingRoutes {
			t.Run(tc.name+" "+route.path, func(t *testing.T) {
				h := newHarness(t)
				resp := h.do(t, http.MethodPost, route.path, route.body, tc.headers(h))
				if resp.StatusCode != http.StatusAccepted {
					t.Errorf("POST %s = %d, want 202", route.path, resp.StatusCode)
				}
			})
		}
	}
}

func TestCrossSiteReadsAreUnaffected(t *testing.T) {
	h := newHarness(t)
	resp := h.do(t, http.MethodGet, "/api/v1/services", "",
		withHeaders(proxied("admin"), map[string]string{"Sec-Fetch-Site": "cross-site"}))
	if resp.StatusCode != http.StatusOK {
		t.Errorf("cross-site GET = %d, want 200", resp.StatusCode)
	}
}
