package server

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// /healthz and /readyz answer without authentication and say nothing beyond ok / not ready
// (docs/review-history.md P2-6).
func TestHealthEndpoints(t *testing.T) {
	h := newHarness(t)
	get := func(path string) (int, string) {
		resp, err := http.Get(h.ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, strings.TrimSpace(string(b))
	}
	if code, body := get("/healthz"); code != 200 || body != "ok" {
		t.Fatalf("/healthz: %d %q", code, body)
	}
	if code, body := get("/readyz"); code != 200 || body != "ok" {
		t.Fatalf("/readyz with the data plane answering: %d %q", code, body)
	}
	h.fake.Fail("Version", 1)
	if code, body := get("/readyz"); code != 503 || body != "not ready" {
		t.Fatalf("/readyz with the data plane failing: %d %q, want 503 and no detail", code, body)
	}
	if code, _ := get("/healthz"); code != 200 {
		t.Fatal("/healthz must not depend on the data plane")
	}
}
