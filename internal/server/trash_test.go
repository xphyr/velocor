package server

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/velocor/velocor/internal/store"
)

// expirePurge moves a deleted volume's purge time into the past.
func (h *harness) expirePurge(volID string) {
	h.t.Helper()
	if _, err := h.st.MutateVolume(volID, func(v *store.Volume) error {
		past := time.Now().Add(-time.Minute)
		v.PurgeAt = &past
		return nil
	}); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) csiToken() string {
	h.t.Helper()
	tok := h.expect(h.do("POST", "/api/v1/auth/tokens", h.admin, map[string]any{"name": "csi", "role": "CSI_PROVISIONER"}))(http.StatusCreated)
	return tok["secret_token"].(string)
}

func TestDeletedVolumeIsKeptAndRestorable(t *testing.T) {
	h := newHarness(t)
	h.addDisk("d1", "0000:01:00.0")
	poolID := h.pool("p", "RAID0", "d1")
	vol := h.volume(poolID, "data", 1<<30)
	id, lvol := vol["id"].(string), vol["handle"].(string)
	h.fake.WriteChunk(lvol, 3)
	csi := h.csiToken()

	before := time.Now()
	del := h.expect(h.do("DELETE", "/api/v1/volumes/"+id, csi, nil))(http.StatusOK)["volume"].(map[string]any)
	purgeAt, err := time.Parse(time.RFC3339, del["purge_at"].(string))
	if err != nil || purgeAt.Before(before.Add(store.DefaultVolumeRetention-time.Minute)) {
		t.Fatalf("purge_at %v, want about 24 h from now", del["purge_at"])
	}
	if del["deleted_by"] != "token:csi" {
		t.Errorf("deleted_by = %v", del["deleted_by"])
	}
	// A retry (CSI) is harmless.
	h.expect(h.do("DELETE", "/api/v1/volumes/"+id, csi, nil))(http.StatusOK)

	// Gone for clients, kept on the pool.
	h.expect(h.do("GET", "/api/v1/volumes/"+id, h.admin, nil))(http.StatusNotFound)
	if vols := h.expect(h.do("GET", "/api/v1/volumes", h.admin, nil))(http.StatusOK)["volumes"].([]any); len(vols) != 0 {
		t.Fatalf("deleted volume listed: %v", vols)
	}
	trash := h.expect(h.do("GET", "/api/v1/deleted-volumes", h.admin, nil))(http.StatusOK)
	if vols := trash["volumes"].([]any); len(vols) != 1 || trash["retention_hours"] != float64(24) {
		t.Fatalf("deleted volumes: %v", trash)
	}
	if !h.fake.VolumeExists(lvol) {
		t.Fatal("deleting a volume removed its lvol")
	}
	h.reconcile()
	if !h.fake.VolumeExists(lvol) {
		t.Fatal("the reconciler purged a volume before its retention ended")
	}

	// Nothing can use it while deleted.
	pt := h.expect(h.do("POST", "/api/v1/portals", h.admin, map[string]any{"name": "n", "protocol": "NVMEOF_TCP", "ip_address": "10.0.0.10"}))(http.StatusCreated)
	h.expect(h.do("POST", "/api/v1/exports", h.admin, map[string]any{
		"name": "e", "volume_id": id, "protocol": "NVMEOF_TCP", "allow_any_host": true,
		"portal_ids": []string{field(pt, "portal", "id").(string)},
	}))(http.StatusNotFound)
	h.expect(h.do("POST", "/api/v1/volumes/"+id+"/snapshots", h.admin, map[string]any{"name": "s"}))(http.StatusNotFound)
	h.expect(h.do("POST", "/api/v1/volumes/"+id+":resize", h.admin, map[string]any{"new_size_bytes": 2 << 30}))(http.StatusNotFound)
	h.expect(h.do("PUT", "/api/v1/volumes/"+id+"/protection", h.admin, map[string]any{"deletion_protection": true}))(http.StatusNotFound)

	// Its name is free again; restoring under it then needs a new name.
	h.volume(poolID, "data", 1<<30)
	h.expect(h.do("POST", "/api/v1/deleted-volumes/"+id+"/restore", h.admin, nil))(http.StatusConflict)
	// CSI tokens can't restore or purge.
	h.expect(h.do("POST", "/api/v1/deleted-volumes/"+id+"/restore", csi, map[string]any{"name": "data-old"}))(http.StatusForbidden)
	h.expect(h.do("DELETE", "/api/v1/deleted-volumes/"+id, csi, nil))(http.StatusForbidden)

	got := h.expect(h.do("POST", "/api/v1/deleted-volumes/"+id+"/restore", h.admin, map[string]any{"name": "data-old"}))(http.StatusOK)["volume"].(map[string]any)
	if got["name"] != "data-old" || got["deleted_at"] != nil || got["purge_at"] != nil {
		t.Fatalf("restored volume: %v", got)
	}
	h.expect(h.do("GET", "/api/v1/volumes/"+id, h.admin, nil))(http.StatusOK)
	if !h.fake.VolumeExists(lvol) {
		t.Fatal("restored volume lost its lvol")
	}
	h.expect(h.do("POST", "/api/v1/volumes/"+id+"/snapshots", h.admin, map[string]any{"name": "s"}))(http.StatusCreated)
}

func TestDeletedVolumeIsPurgedWhenRetentionEnds(t *testing.T) {
	h := newHarness(t)
	h.addDisk("d1", "0000:01:00.0")
	poolID := h.pool("p", "RAID0", "d1")
	keep := h.volume(poolID, "keep", 1<<30)
	gone := h.volume(poolID, "gone", 1<<30)
	h.expect(h.do("DELETE", "/api/v1/volumes/"+keep["id"].(string), h.admin, nil))(http.StatusOK)
	h.expect(h.do("DELETE", "/api/v1/volumes/"+gone["id"].(string), h.admin, nil))(http.StatusOK)

	// The pool can't be deleted while it holds deleted volumes.
	resp := h.expect(h.do("DELETE", "/api/v1/pools/"+poolID, h.admin, nil))(http.StatusConflict)
	if !strings.Contains(resp["error"].(string), "deleted volume") {
		t.Fatalf("pool delete refusal: %v", resp)
	}

	h.expirePurge(gone["id"].(string))
	h.reconcile()
	if h.fake.VolumeExists(gone["handle"].(string)) {
		t.Fatal("expired volume's lvol not purged")
	}
	if _, err := h.st.GetVolume(gone["id"].(string)); err == nil {
		t.Fatal("expired volume's record not purged")
	}
	if !h.fake.VolumeExists(keep["handle"].(string)) {
		t.Fatal("volume purged before its retention ended")
	}

	// A purge that has started can't be undone by a restore.
	if _, err := store.StartPurge(h.st, keep["id"].(string), time.Time{}); err != nil {
		t.Fatal(err)
	}
	h.expect(h.do("POST", "/api/v1/deleted-volumes/"+keep["id"].(string)+"/restore", h.admin, nil))(http.StatusConflict)
	h.expect(h.do("DELETE", "/api/v1/deleted-volumes/"+keep["id"].(string), h.admin, nil))(http.StatusOK)
	if h.fake.VolumeExists(keep["handle"].(string)) {
		t.Fatal("purge left the lvol")
	}
	h.expect(h.do("DELETE", "/api/v1/pools/"+poolID, h.admin, nil))(http.StatusOK)
}

func TestDeletionProtection(t *testing.T) {
	h := newHarness(t)
	h.addDisk("d1", "0000:01:00.0")
	poolID := h.pool("p", "RAID0", "d1")
	csi := h.csiToken()
	vol := h.expect(h.do("POST", "/api/v1/volumes", csi, map[string]any{
		"name": "db", "pool_id": poolID, "size_bytes": 1 << 30, "deletion_protection": true,
	}))(http.StatusCreated)["volume"].(map[string]any)
	id := vol["id"].(string)

	resp := h.expect(h.do("DELETE", "/api/v1/volumes/"+id, csi, nil))(http.StatusConflict)
	if !strings.Contains(resp["error"].(string), "deletion protection") {
		t.Fatalf("refusal: %v", resp)
	}
	h.expect(h.do("PUT", "/api/v1/volumes/"+id+"/protection", csi, map[string]any{"deletion_protection": false}))(http.StatusForbidden)
	h.expect(h.do("PUT", "/api/v1/volumes/"+id+"/protection", h.admin, map[string]any{}))(http.StatusBadRequest)
	got := h.expect(h.do("PUT", "/api/v1/volumes/"+id+"/protection", h.admin, map[string]any{"deletion_protection": false}))(http.StatusOK)["volume"].(map[string]any)
	if got["deletion_protection"] != nil && got["deletion_protection"] != false {
		t.Fatalf("protection still on: %v", got)
	}
	h.expect(h.do("DELETE", "/api/v1/volumes/"+id, csi, nil))(http.StatusOK)
}

func TestVolumeRetentionSetting(t *testing.T) {
	h := newHarness(t)
	h.addDisk("d1", "0000:01:00.0")
	poolID := h.pool("p", "RAID0", "d1")
	if got := h.expect(h.do("GET", "/api/v1/system/volume-retention", h.admin, nil))(http.StatusOK); got["hours"] != float64(24) {
		t.Fatalf("default retention: %v", got)
	}
	h.expect(h.do("PUT", "/api/v1/system/volume-retention", h.admin, map[string]any{"hours": 721}))(http.StatusBadRequest)
	h.expect(h.do("PUT", "/api/v1/system/volume-retention", h.admin, map[string]any{}))(http.StatusBadRequest)
	h.expect(h.do("PUT", "/api/v1/system/volume-retention", h.csiToken(), map[string]any{"hours": 0}))(http.StatusForbidden)

	h.expect(h.do("PUT", "/api/v1/system/volume-retention", h.admin, map[string]any{"hours": 2}))(http.StatusOK)
	vol := h.volume(poolID, "a", 1<<30)
	del := h.expect(h.do("DELETE", "/api/v1/volumes/"+vol["id"].(string), h.admin, nil))(http.StatusOK)["volume"].(map[string]any)
	at, _ := time.Parse(time.RFC3339, del["purge_at"].(string))
	if d := time.Until(at); d < 110*time.Minute || d > 2*time.Hour {
		t.Fatalf("purge in %v, want 2 h", d)
	}

	// 0: deleted at once, as before P1-4.
	h.expect(h.do("PUT", "/api/v1/system/volume-retention", h.admin, map[string]any{"hours": 0}))(http.StatusOK)
	vol = h.volume(poolID, "b", 1<<30)
	h.expect(h.do("DELETE", "/api/v1/volumes/"+vol["id"].(string), h.admin, nil))(http.StatusOK)
	if h.fake.VolumeExists(vol["handle"].(string)) {
		t.Fatal("retention 0 kept the lvol")
	}
	if _, err := h.st.GetVolume(vol["id"].(string)); err == nil {
		t.Fatal("retention 0 kept the record")
	}
}

// A snapshot can be deleted while volumes cloned from it exist (Kubernetes allows deleting a
// VolumeSnapshot after restoring from it): it is hidden and purged once they are gone.
func TestSnapshotDeleteWaitsForItsClones(t *testing.T) {
	h := newHarness(t)
	h.addDisk("d1", "0000:01:00.0")
	poolID := h.pool("p", "RAID0", "d1")
	src := h.volume(poolID, "src", 1<<30)
	snap := h.expect(h.do("POST", "/api/v1/volumes/"+src["id"].(string)+"/snapshots", h.admin, map[string]any{"name": "s1"}))(http.StatusCreated)["snapshot"].(map[string]any)
	snapID := snap["id"].(string)
	clone := h.expect(h.do("POST", "/api/v1/snapshots/"+snapID+":clone", h.admin, map[string]any{"clone_name": "restored"}))(http.StatusCreated)["volume"].(map[string]any)
	cloneID := clone["id"].(string)
	snapLvol := snap["handle"].(string)

	// Live clone: the delete succeeds, the snapshot disappears, its lvol stays.
	h.expect(h.do("DELETE", "/api/v1/snapshots/"+snapID, h.admin, nil))(http.StatusOK)
	h.expect(h.do("GET", "/api/v1/snapshots/"+snapID, h.admin, nil))(http.StatusNotFound)
	if snaps := h.expect(h.do("GET", "/api/v1/snapshots", h.admin, nil))(http.StatusOK)["snapshots"].([]any); len(snaps) != 0 {
		t.Fatalf("deleted snapshot listed: %v", snaps)
	}
	h.expect(h.do("POST", "/api/v1/snapshots/"+snapID+":clone", h.admin, map[string]any{"clone_name": "again"}))(http.StatusNotFound)
	h.expect(h.do("DELETE", "/api/v1/snapshots/"+snapID, h.admin, nil))(http.StatusOK) // retry
	h.reconcile()
	if !h.fake.VolumeExists(snapLvol) {
		t.Fatal("snapshot purged while a clone depends on it")
	}
	// A new snapshot can take the name.
	h.expect(h.do("POST", "/api/v1/volumes/"+src["id"].(string)+"/snapshots", h.admin, map[string]any{"name": "s1"}))(http.StatusCreated)

	// Deleted clone: still waits, and the clone can be restored meanwhile.
	h.expect(h.do("DELETE", "/api/v1/volumes/"+cloneID, h.admin, nil))(http.StatusOK)
	h.reconcile()
	if !h.fake.VolumeExists(snapLvol) {
		t.Fatal("snapshot purged while a deleted clone that can be restored depends on it")
	}
	h.expect(h.do("POST", "/api/v1/deleted-volumes/"+cloneID+"/restore", h.admin, nil))(http.StatusOK)
	h.expect(h.do("DELETE", "/api/v1/volumes/"+cloneID, h.admin, nil))(http.StatusOK)

	// Once the clone is purged, the snapshot goes too.
	h.expirePurge(cloneID)
	h.reconcile()
	if h.fake.VolumeExists(snapLvol) {
		t.Fatal("snapshot not purged after its last clone")
	}
	if _, err := h.st.GetSnapshot(snapID); err == nil {
		t.Fatal("snapshot record not purged")
	}
}
