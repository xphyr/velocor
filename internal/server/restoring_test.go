package server

import (
	"net/http"
	"strings"
	"testing"
)

// Until velod's first pass has finished (after a reboot: until the pools, volumes and exports are
// back), status mustn't say ONLINE (docs/review-history.md P2-11).
func TestStatusBeforeTheFirstPass(t *testing.T) {
	h := newHarness(t)
	overall := func() (string, bool) {
		st := h.expect(h.do("GET", "/api/v1/system/status", h.admin, nil))(http.StatusOK)
		return st["overall_health"].(string), st["data_plane"].(map[string]any)["restoring"].(bool)
	}
	if o, r := overall(); o != "RECONCILING" || !r {
		t.Fatalf("before the first pass: %s restoring=%v, want RECONCILING", o, r)
	}
	if m := h.metrics(); !strings.Contains(m, "velocor_dataplane_restoring 1\n") {
		t.Fatal("metric velocor_dataplane_restoring isn't 1 while restoring")
	}
	h.reconcile()
	if o, r := overall(); o != "ONLINE" || r {
		t.Fatalf("after it: %s restoring=%v, want ONLINE", o, r)
	}
}
