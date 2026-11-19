package server

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/velocor/velocor/internal/store"
)

// fakeNM behaves like NetworkManager's checkpoints: a checkpoint is pending until committed,
// rolled back, or expired (expire simulates NetworkManager acting at the deadline).
type fakeNM struct {
	mu          sync.Mutex
	next        int
	checkpoints map[string]string // checkpoint -> interface
	applied     []string          // "iface temporary|saved method"
	rolledBack  []string
	committed   []string
	timeouts    []time.Duration
	failApply   bool
}

func newFakeNM() *fakeNM { return &fakeNM{checkpoints: map[string]string{}} }

func (f *fakeNM) Checkpoint(_ context.Context, iface string, timeout time.Duration) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	cp := fmt.Sprintf("/org/freedesktop/NetworkManager/Checkpoint/%d", f.next)
	f.checkpoints[cp] = iface
	f.timeouts = append(f.timeouts, timeout)
	return cp, nil
}

func (f *fakeNM) CheckpointExists(_ context.Context, cp string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.checkpoints[cp]
	return ok, nil
}

func (f *fakeNM) Commit(_ context.Context, cp string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.checkpoints, cp)
	f.committed = append(f.committed, cp)
	return nil
}

func (f *fakeNM) Rollback(_ context.Context, cp string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.checkpoints, cp)
	f.rolledBack = append(f.rolledBack, cp)
	return nil
}

func (f *fakeNM) Apply(_ context.Context, iface string, req UpdateNetworkInterfaceRequest, _ []string, temporary bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failApply && temporary {
		return fmt.Errorf("nmcli says no")
	}
	mode := "saved"
	if temporary {
		mode = "temporary"
	}
	f.applied = append(f.applied, fmt.Sprintf("%s %s %s", iface, mode, req.Method))
	return nil
}

// expire drops every checkpoint, as NetworkManager does at the deadline.
func (f *fakeNM) expire() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checkpoints = map[string]string{}
}

func netHarness(t *testing.T) (*harness, *fakeNM, *Server) {
	t.Helper()
	h := newHarness(t)
	srv := h.srv
	nm := newFakeNM()
	srv.net.backend = nm
	srv.netQuery = func(context.Context) NetworkStatus {
		return NetworkStatus{Available: true, NMCLIAvailable: true, Interfaces: []NetworkInterface{
			{Name: "mgmt0", Method: "dhcp", Addresses: []string{"192.0.2.10/24"}},
			{Name: "san0", Method: "manual", Addresses: []string{"10.20.0.5/24"}},
		}}
	}
	return h, nm, srv
}

func TestNetworkChangeConfirm(t *testing.T) {
	h, nm, _ := netHarness(t)
	change := map[string]any{"method": "manual", "address": "192.0.2.20/24", "gateway": "192.0.2.1", "confirm_timeout_seconds": 90}
	resp := h.expect(h.do("PUT", "/api/v1/system/network/interfaces/mgmt0", h.admin, change))(http.StatusAccepted)
	p := resp["pending"].(map[string]any)
	id := p["id"].(string)
	if p["state"] != "PENDING" || p["new_address"] != "192.0.2.20" || p["interface"] != "mgmt0" {
		t.Fatalf("pending: %v", p)
	}
	if len(nm.timeouts) != 1 || nm.timeouts[0] != 90*time.Second || nm.applied[0] != "mgmt0 temporary manual" {
		t.Fatalf("checkpoint %v, applied %v: want a 90 s checkpoint and a temporary change", nm.timeouts, nm.applied)
	}
	got := h.expect(h.do("GET", "/api/v1/system/network", h.admin, nil))(http.StatusOK)
	if got["pending_change"].(map[string]any)["id"] != id {
		t.Fatalf("GET shows %v", got["pending_change"])
	}
	// One change at a time.
	h.expect(h.do("PUT", "/api/v1/system/network/interfaces/mgmt0", h.admin, map[string]any{"method": "dhcp"}))(http.StatusConflict)

	h.expect(h.do("POST", "/api/v1/system/network/confirm", h.admin, map[string]any{"id": "netchg-nope"}))(http.StatusNotFound)
	done := h.expect(h.do("POST", "/api/v1/system/network/confirm", h.admin, map[string]any{"id": id}))(http.StatusOK)["change"].(map[string]any)
	if done["state"] != "CONFIRMED" || len(nm.committed) != 1 || nm.applied[len(nm.applied)-1] != "mgmt0 saved manual" {
		t.Fatalf("confirm: %v, committed %v, applied %v: want the change saved, then committed", done, nm.committed, nm.applied)
	}
	got = h.expect(h.do("GET", "/api/v1/system/network", h.admin, nil))(http.StatusOK)
	if got["pending_change"] != nil || got["last_change"].(map[string]any)["state"] != "CONFIRMED" {
		t.Fatalf("after confirm: %v / %v", got["pending_change"], got["last_change"])
	}
}

func TestNetworkChangeRollbackAndExpiry(t *testing.T) {
	h, nm, _ := netHarness(t)
	id := h.expect(h.do("PUT", "/api/v1/system/network/interfaces/mgmt0", h.admin, map[string]any{"method": "disabled"}))(http.StatusAccepted)["pending"].(map[string]any)["id"].(string)
	done := h.expect(h.do("POST", "/api/v1/system/network/rollback", h.admin, map[string]any{"id": id}))(http.StatusOK)["change"].(map[string]any)
	if done["state"] != "ROLLED_BACK" || len(nm.rolledBack) != 1 {
		t.Fatalf("rollback: %v %v", done, nm.rolledBack)
	}

	// Not confirmed in time: NetworkManager restored it; a late confirmation is refused.
	id = h.expect(h.do("PUT", "/api/v1/system/network/interfaces/mgmt0", h.admin, map[string]any{"method": "dhcp", "mtu": 1400}))(http.StatusAccepted)["pending"].(map[string]any)["id"].(string)
	nm.expire()
	_, out := h.do("POST", "/api/v1/system/network/confirm", h.admin, map[string]any{"id": id})
	if !strings.Contains(fmt.Sprint(out["error"]), "too late") {
		t.Fatalf("late confirm: %v", out)
	}
	got := h.expect(h.do("GET", "/api/v1/system/network", h.admin, nil))(http.StatusOK)
	if got["last_change"].(map[string]any)["state"] != "EXPIRED" || got["pending_change"] != nil {
		t.Fatalf("after expiry: %v", got)
	}
	for _, a := range nm.applied {
		if strings.Contains(a, "saved") {
			t.Fatalf("an unconfirmed change was saved: %v", nm.applied)
		}
	}
}

func TestNetworkChangeGuards(t *testing.T) {
	h, nm, _ := netHarness(t)
	if err := h.st.CreatePortal(&store.Portal{ID: "portal-san", Name: "san-nvme", Protocol: store.ExportProtocolNVMeoFTCP, IPAddress: "10.20.0.5", Port: 4420}); err != nil {
		t.Fatal(err)
	}
	_, out := h.do("PUT", "/api/v1/system/network/interfaces/san0", h.admin, map[string]any{"method": "dhcp"})
	if !strings.Contains(fmt.Sprint(out["error"]), "san-nvme") || !strings.Contains(fmt.Sprint(out["error"]), `"force": true`) {
		t.Fatalf("an interface with portals: %v", out)
	}
	if len(nm.timeouts) != 0 {
		t.Fatal("refused change touched NetworkManager")
	}
	id := h.expect(h.do("PUT", "/api/v1/system/network/interfaces/san0", h.admin, map[string]any{"method": "dhcp", "force": true}))(http.StatusAccepted)["pending"].(map[string]any)["id"].(string)
	h.expect(h.do("POST", "/api/v1/system/network/rollback", h.admin, map[string]any{"id": id}))(http.StatusOK)

	h.expect(h.do("PUT", "/api/v1/system/network/interfaces/nope0", h.admin, map[string]any{"method": "dhcp"}))(http.StatusNotFound)
	h.expect(h.do("PUT", "/api/v1/system/network/interfaces/mgmt0", h.admin, map[string]any{"method": "dhcp", "confirm_timeout_seconds": 30}))(http.StatusBadRequest)
	h.expect(h.do("PUT", "/api/v1/system/network/interfaces/mgmt0", h.csiToken(), map[string]any{"method": "dhcp"}))(http.StatusForbidden)

	// A change NetworkManager refuses is rolled back at once and nothing stays pending.
	nm.failApply = true
	h.expect(h.do("PUT", "/api/v1/system/network/interfaces/mgmt0", h.admin, map[string]any{"method": "dhcp"}))(http.StatusInternalServerError)
	if got := h.expect(h.do("GET", "/api/v1/system/network", h.admin, nil))(http.StatusOK); got["pending_change"] != nil || len(nm.rolledBack) != 2 {
		t.Fatalf("after a failed apply: pending %v, rollbacks %v", got["pending_change"], nm.rolledBack)
	}
}
