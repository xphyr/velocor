package server

import (
	"net/http"
	"strings"
	"testing"

	"github.com/velocor/velocor/internal/dataplane"
)

// P2-12: a portal whose address isn't on the appliance (here TEST-NET-3, see TestMain) is
// never sent to the data plane. The export is served on its other portals and reported DEGRADED with
// the reason, unrelated changes still apply, changes that add such a portal are refused, and
// the portal is served once its address appears.
func TestPortalWithoutItsAddress(t *testing.T) {
	h := newHarness(t)
	h.addDisk("d1", "0000:01:00.0")
	poolID := h.pool("p", "RAID0", "d1")
	v1, v2 := h.volume(poolID, "a", 1<<30), h.volume(poolID, "b", 1<<30)
	portal := func(name, proto, ip string) string {
		return field(h.expect(h.do("POST", "/api/v1/portals", h.admin, map[string]any{"name": name, "protocol": proto, "ip_address": ip}))(http.StatusCreated), "portal", "id").(string)
	}
	here, gone := portal("here", "NVMEOF_TCP", "10.0.0.10"), portal("gone", "NVMEOF_TCP", "203.0.113.5")
	iGone := portal("igone", "ISCSI", "203.0.113.6")
	ig := field(h.expect(h.do("POST", "/api/v1/initiator-groups", h.admin, map[string]any{
		"name": "g", "host_nqns": []string{"nqn.2026-01.io.example:h"}, "initiator_iqns": []string{"iqn.2026-01.io.example:h"},
	}))(http.StatusCreated), "initiator_group", "id").(string)
	ig2 := field(h.expect(h.do("POST", "/api/v1/initiator-groups", h.admin, map[string]any{"name": "g2", "host_nqns": []string{"nqn.2026-01.io.example:h2"}}))(http.StatusCreated), "initiator_group", "id").(string)

	// Exporting on it is refused with the reason, and leaves nothing behind.
	code, out := h.do("POST", "/api/v1/exports", h.admin, map[string]any{"name": "x", "volume_id": v1["id"], "protocol": "NVMEOF_TCP", "portal_ids": []string{gone}, "initiator_group_ids": []string{ig}})
	if code != http.StatusBadRequest || !strings.Contains(out["error"].(string), "203.0.113.5, which isn't on this appliance") {
		t.Fatalf("export on a missing address: %d %v", code, out)
	}
	if left := h.fake.Targets(); len(left) != 0 {
		t.Fatalf("a refused export left targets: %v", left)
	}

	exp := h.expect(h.do("POST", "/api/v1/exports", h.admin, map[string]any{"name": "e", "volume_id": v1["id"], "protocol": "NVMEOF_TCP", "portal_ids": []string{here}, "initiator_group_ids": []string{ig}}))(http.StatusCreated)["export"].(map[string]any)
	path, nqn := "/api/v1/exports/"+exp["id"].(string), exp["subsystem_nqn"].(string)
	// Adding the portal is refused too, and changes nothing.
	h.expect(h.do("PATCH", path, h.admin, map[string]any{"portal_ids": []string{here, gone}}))(http.StatusBadRequest)
	if e, _ := h.st.GetExport(exp["id"].(string)); len(e.PortalIDs) != 1 {
		t.Fatalf("refused portal change persisted: %v", e.PortalIDs)
	}

	// The address was there when the export got the portal, and is gone after a reboot.
	restore := dataplane.SetAddressCheck(func(string) bool { return true })
	h.expect(h.do("PATCH", path, h.admin, map[string]any{"portal_ids": []string{here, gone}}))(http.StatusOK)
	iexp := h.expect(h.do("POST", "/api/v1/exports", h.admin, map[string]any{"name": "i", "volume_id": v2["id"], "protocol": "ISCSI", "portal_ids": []string{iGone}, "initiator_group_ids": []string{ig}}))(http.StatusCreated)["export"].(map[string]any)
	restore()
	h.fake.Reboot()
	h.reconcile()
	listens := func() []string {
		sub, _ := h.fake.Target(nqn)
		return sub.Listeners
	}
	calls := h.fake.CallCount("ApplyExport")
	for range 3 { // every pass: nothing changes for the missing address
		h.reconcile()
	}
	if got := listens(); len(got) != 1 || got[0] != "10.0.0.10:4420" || h.fake.CallCount("ApplyExport") != calls {
		t.Fatalf("listeners %v, changes %d -> %d", got, calls, h.fake.CallCount("ApplyExport"))
	}
	status := func(id string) (string, string) {
		e := h.expect(h.do("GET", "/api/v1/exports/"+id, h.admin, nil))(http.StatusOK)["export"].(map[string]any)
		msg, _ := e["status_message"].(string)
		return e["health_status"].(string), msg
	}
	if hs, msg := status(exp["id"].(string)); hs != "DEGRADED" || !strings.Contains(msg, "portal gone ("+gone+") has address 203.0.113.5, which isn't on this appliance") {
		t.Fatalf("NVMe export: %s %q", hs, msg)
	}
	if hs, msg := status(iexp["id"].(string)); hs != "DEGRADED" || !strings.Contains(msg, "203.0.113.6") {
		t.Fatalf("iSCSI export: %s %q", hs, msg)
	}
	if tgt, _ := h.fake.Target(iexp["target_iqn"].(string)); len(tgt.Listeners) != 0 {
		t.Fatalf("iSCSI target served on an address that isn't here: %v", tgt.Listeners)
	}

	// An unrelated change still applies; so does dropping the missing portal.
	h.expect(h.do("PATCH", path, h.admin, map[string]any{"initiator_group_ids": []string{ig, ig2}}))(http.StatusOK)
	if sub, _ := h.fake.Target(nqn); len(sub.Hosts) != 2 {
		t.Fatalf("hosts not updated: %+v", sub.Hosts)
	}
	// Disabled, then enabled again while the address is still missing: refused.
	h.expect(h.do("PATCH", path, h.admin, map[string]any{"enabled": false}))(http.StatusOK)
	h.expect(h.do("PATCH", path, h.admin, map[string]any{"enabled": true}))(http.StatusBadRequest)

	// The address appears: the next pass serves both.
	defer dataplane.SetAddressCheck(func(string) bool { return true })()
	h.expect(h.do("PATCH", path, h.admin, map[string]any{"enabled": true}))(http.StatusOK)
	h.reconcile()
	if got := listens(); len(got) != 2 {
		t.Fatalf("listeners after the address came back: %v", got)
	}
	for _, id := range []string{exp["id"].(string), iexp["id"].(string)} {
		if hs, msg := status(id); hs != "ONLINE" {
			t.Fatalf("export %s: %s %q", id, hs, msg)
		}
	}
	if n, ok := h.fake.Target(iexp["target_iqn"].(string)); !ok || len(n.Listeners) != 1 {
		t.Fatalf("iSCSI target after the address came back: %+v %v", n, ok)
	}
}
