package server

import (
	"net/http"
	"testing"
)

// nvmeExport creates a pool, volume, NVMe/TCP portal and an export limited to a group with hostNQN.
func (h *harness) nvmeExport(hostNQN string) (exp, vol map[string]any, igID string) {
	h.t.Helper()
	h.addDisk("d1", "0000:01:00.0")
	poolID := h.pool("p", "RAID0", "d1")
	vol = h.volume(poolID, "v", 1<<30)
	pt := h.expect(h.do("POST", "/api/v1/portals", h.admin, map[string]any{"name": "n", "protocol": "NVMEOF_TCP", "ip_address": "10.0.0.10"}))(http.StatusCreated)
	ig := h.expect(h.do("POST", "/api/v1/initiator-groups", h.admin, map[string]any{"name": "g", "host_nqns": []string{hostNQN}}))(http.StatusCreated)
	igID = field(ig, "initiator_group", "id").(string)
	exp = h.expect(h.do("POST", "/api/v1/exports", h.admin, map[string]any{
		"name": "e", "volume_id": vol["id"], "protocol": "NVMEOF_TCP",
		"portal_ids": []string{field(pt, "portal", "id").(string)}, "initiator_group_ids": []string{igID},
	}))(http.StatusCreated)["export"].(map[string]any)
	return exp, vol, igID
}

func TestUpdateExportAccess(t *testing.T) {
	h := newHarness(t)
	exp, _, igID := h.nvmeExport("nqn.2026-01.io.example:host-a")
	path := "/api/v1/exports/" + exp["id"].(string)
	nqn := exp["subsystem_nqn"].(string)

	// Removing every initiator without allow_any_host is refused and changes nothing.
	if code, _ := h.do("PATCH", path, h.admin, map[string]any{"initiator_group_ids": []string{}}); code != http.StatusBadRequest {
		t.Fatalf("emptying the ACL returned %d", code)
	}
	if sub, _ := h.fake.Target(nqn); len(sub.Hosts) != 1 {
		t.Fatalf("refused change touched the target: %+v", sub.Hosts)
	}

	// Opening up and closing again is applied and persisted.
	h.expect(h.do("PATCH", path, h.admin, map[string]any{"allow_any_host": true, "initiator_group_ids": []string{}}))(http.StatusOK)
	if sub, _ := h.fake.Target(nqn); !sub.AnyHost || len(sub.Hosts) != 0 {
		t.Fatalf("allow_any_host not applied: %+v", sub)
	}
	h.expect(h.do("PATCH", path, h.admin, map[string]any{"allow_any_host": false, "initiator_group_ids": []string{igID}}))(http.StatusOK)
	sub, _ := h.fake.Target(nqn)
	if sub.AnyHost || len(sub.Hosts) != 1 || sub.Hosts[0] != "nqn.2026-01.io.example:host-a" {
		t.Fatalf("ACL not restored: %+v", sub)
	}
	if e, _ := h.st.GetExport(exp["id"].(string)); e.AllowAnyHost || len(e.InitiatorGroupIDs) != 1 {
		t.Fatalf("store not updated: %+v", e)
	}

	// A data plane failure leaves the store unchanged.
	h.fake.Fail("ApplyExport", 1)
	if code, _ := h.do("PATCH", path, h.admin, map[string]any{"allow_any_host": true}); code != http.StatusInternalServerError {
		t.Fatalf("a failing update returned %d", code)
	}
	if e, _ := h.st.GetExport(exp["id"].(string)); e.AllowAnyHost {
		t.Fatalf("failed update was persisted")
	}

	h.fake.Connect(nqn, "nqn.2026-01.io.example:host-a")
	conns := h.expect(h.do("GET", path+"/connections", h.admin, nil))(http.StatusOK)["connections"].([]any)
	if len(conns) != 1 || conns[0].(map[string]any)["host"] != "nqn.2026-01.io.example:host-a" {
		t.Fatalf("connections wrong: %v", conns)
	}
}

func TestUpdateInitiatorGroupReappliesExports(t *testing.T) {
	h := newHarness(t)
	exp, vol, igID := h.nvmeExport("nqn.2026-01.io.example:host-a")
	nqn := exp["subsystem_nqn"].(string)
	path := "/api/v1/initiator-groups/" + igID

	h.expect(h.do("PATCH", path, h.admin, map[string]any{"host_nqns": []string{"nqn.2026-01.io.example:host-b"}}))(http.StatusOK)
	sub, _ := h.fake.Target(nqn)
	if len(sub.Hosts) != 1 || sub.Hosts[0] != "nqn.2026-01.io.example:host-b" {
		t.Fatalf("group change not applied to export: %+v", sub.Hosts)
	}
	if code, _ := h.do("PATCH", path, h.admin, map[string]any{"host_nqns": []string{}}); code != http.StatusBadRequest {
		t.Fatalf("leaving an export with no initiators returned %d", code)
	}
	if g, _ := h.st.GetInitiatorGroup(igID); len(g.HostNQNs) != 1 {
		t.Fatalf("refused change was persisted: %+v", g)
	}

	usage := h.expect(h.do("GET", "/api/v1/volumes/"+vol["id"].(string)+"/usage", h.admin, nil))(http.StatusOK)
	if usage["size_bytes"].(float64) != 1<<30 || usage["allocated_bytes"].(float64) != 0 {
		t.Fatalf("usage wrong: %v", usage)
	}
	h.fake.WriteChunk(vol["uuid"].(string), 0)
	usage = h.expect(h.do("GET", "/api/v1/volumes/"+vol["id"].(string)+"/usage", h.admin, nil))(http.StatusOK)
	if usage["allocated_bytes"].(float64) == 0 {
		t.Fatalf("allocation not reported: %v", usage)
	}
}

// A group snapshot of volumes in use is taken (docs/review-history.md P1-15): their I/O is held
// together, so hosts needn't stop.
func TestGroupSnapshotOfExportedVolumes(t *testing.T) {
	h := newHarness(t)
	_, vol, _ := h.nvmeExport("nqn.2026-01.io.example:host-a")
	other := h.volume(vol["pool_id"].(string), "idle", 64<<20)
	resp := h.expect(h.do("POST", "/api/v1/volume-group-snapshots", h.admin, map[string]any{"name": "g", "volume_ids": []string{vol["id"].(string), other["id"].(string)}}))(http.StatusCreated)
	if snaps := resp["snapshots"].([]any); len(snaps) != 2 {
		t.Fatalf("group snapshot of an exported and an idle volume: %v", resp)
	}
}

// Names that would leave their configfs directory are refused wherever they are entered.
func TestQualifiedNamesAreValidated(t *testing.T) {
	h := newHarness(t)
	_, vol, ig := h.nvmeExport("nqn.2026-01.io.example:host-a")
	ipt := h.expect(h.do("POST", "/api/v1/portals", h.admin, map[string]any{"name": "i", "protocol": "ISCSI", "ip_address": "10.0.0.10"}))(http.StatusCreated)
	npt := h.expect(h.do("POST", "/api/v1/portals", h.admin, map[string]any{"name": "n2", "protocol": "NVMEOF_TCP", "ip_address": "10.0.0.11"}))(http.StatusCreated)
	badIQN, badNQN := "iqn.2024-01.x:../../core/x", "nqn.2024-01.x:../../subsystems/evil"
	for name, do := range map[string]func() (int, map[string]any){
		"subsystem": func() (int, map[string]any) {
			return h.do("POST", "/api/v1/exports", h.admin, map[string]any{"name": "e2", "volume_id": vol["id"], "protocol": "NVMEOF_TCP", "subsystem_nqn": badNQN,
				"portal_ids": []string{field(npt, "portal", "id").(string)}, "allow_any_host": true})
		},
		"target": func() (int, map[string]any) {
			return h.do("POST", "/api/v1/exports", h.admin, map[string]any{"name": "e3", "volume_id": vol["id"], "protocol": "ISCSI", "target_iqn": badIQN,
				"portal_ids": []string{field(ipt, "portal", "id").(string)}, "allow_any_host": true})
		},
		"host IQN": func() (int, map[string]any) {
			return h.do("POST", "/api/v1/hosts", h.admin, map[string]any{"name": "h", "initiator_iqn": badIQN})
		},
		"host NQN": func() (int, map[string]any) {
			return h.do("POST", "/api/v1/hosts", h.admin, map[string]any{"name": "h", "host_nqn": badNQN})
		},
		"group IQN": func() (int, map[string]any) {
			return h.do("POST", "/api/v1/initiator-groups", h.admin, map[string]any{"name": "g2", "initiator_iqns": []string{badIQN}})
		},
		"group update": func() (int, map[string]any) {
			return h.do("PATCH", "/api/v1/initiator-groups/"+ig, h.admin, map[string]any{"initiator_iqns": []string{badIQN}})
		},
	} {
		if code, body := do(); code != http.StatusBadRequest {
			t.Errorf("%s: %d %v", name, code, body)
		}
	}
}
