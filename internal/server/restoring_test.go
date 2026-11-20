package server

import (
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

// After spdk_tgt restarts, status mustn't say ONLINE on the strength of a pass that reconciled
// the previous process (docs/review-history.md P2-11): it says RECONCILING until a pass covers the new one.
func TestStatusAfterDataPlaneRestart(t *testing.T) {
	h := newHarness(t)
	var inst atomic.Value
	inst.Store("100:5000")
	h.rec.SetSPDKInstance(func() string { return inst.Load().(string) })
	h.reconcile()
	overall := func() (string, bool) {
		st := h.expect(h.do("GET", "/api/v1/system/status", h.admin, nil))(http.StatusOK)
		return st["overall_health"].(string), st["spdk"].(map[string]any)["restoring"].(bool)
	}
	if o, r := overall(); o != "ONLINE" || r {
		t.Fatalf("after a pass on this spdk_tgt: %s restoring=%v, want ONLINE", o, r)
	}

	inst.Store("200:9000") // spdk_tgt restarted
	if o, r := overall(); o != "RECONCILING" || !r {
		t.Fatalf("right after a restart: %s restoring=%v, want RECONCILING", o, r)
	}
	if m := h.metrics(); !strings.Contains(m, "velocor_dataplane_restoring 1\n") {
		t.Fatal("metric velocor_dataplane_restoring isn't 1 while restoring")
	}

	h.reconcile()
	if o, r := overall(); o != "ONLINE" || r {
		t.Fatalf("after a pass on the new spdk_tgt: %s restoring=%v, want ONLINE", o, r)
	}

	// Unknown process (e.g. another PID namespace): no restart detection, no false alarm.
	inst.Store("")
	if o, _ := overall(); o != "ONLINE" {
		t.Fatalf("without a visible spdk_tgt: %s", o)
	}
}
