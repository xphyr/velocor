//go:build integration

package integration

import (
	"fmt"
	"strings"
	"testing"

	"github.com/velocor/velocor/internal/hardware"
	"github.com/velocor/velocor/internal/store"
)

// A velod that lost its database finds the pool on its disks (reading SPDK's real RAID
// superblocks through the files) and imports it from SPDK's view (the skeleton: rootless
// containers can't use ublk, so no metadata copy is written here). Data, snapshots and
// clone parentage come back.
func TestImportSkeletonAfterLosingTheDatabase(t *testing.T) {
	a, pool, vol, in := setup(t, "RAID1", 2)
	poolID, volID := pool["id"].(string), vol["id"].(string)
	cluster := int(pool["cluster_size_bytes"].(float64))
	first := randomData(t, cluster)
	if err := in.write(0, first); err != nil {
		t.Fatal(err)
	}
	s1 := a.must("POST", "/volumes/"+volID+"/snapshots", map[string]any{"name": "s1"}, "snapshot")
	clone := a.must("POST", "/snapshots/"+s1["id"].(string)+":clone", map[string]any{"clone_name": "c1"}, "volume")

	// The superblock as the kernel sees it: our parser and SPDK agree on layout and checksum.
	var raidUUID string
	for _, id := range []string{"diska", "diskb"} {
		d, _ := a.st.GetDisk(id)
		sb, err := hardware.ReadRaidSuperblock(d.DevicePath)
		if err != nil || sb == nil {
			t.Fatalf("superblock of %s: %+v %v", id, sb, err)
		}
		if sb.Name != "raid_"+poolID || sb.Level != 1 || sb.NumMembers != 2 || len(sb.Members) != 2 {
			t.Fatalf("superblock of %s: %+v", id, sb)
		}
		raidUUID = sb.UUID
	}
	if raid := a.tg.rawBdev("raid_" + poolID); raid["uuid"] != raidUUID {
		t.Fatalf("superblock UUID %s, SPDK says %v", raidUUID, raid["uuid"])
	}

	// Lose the database: spdk_tgt restarts empty and a fresh velod sees two unclaimed disks.
	disks := map[string]*store.Disk{}
	for _, id := range []string{"diska", "diskb"} {
		d, _ := a.st.GetDisk(id)
		disks[id] = d
	}
	a.tg.stop()
	a.tg.start()
	b := newAppliance(t, a.tg)
	b.port = a.port
	for _, d := range disks {
		fresh := &store.Disk{ID: d.ID, Name: d.Name, Type: d.Type, DevicePath: d.DevicePath, SizeBytes: d.SizeBytes, BlockSize: d.BlockSize, SerialNumber: d.SerialNumber}
		if err := b.st.UpsertDisk(fresh); err != nil {
			t.Fatal(err)
		}
	}

	cands := b.must("GET", "/pool-imports", nil, "")["candidates"].([]any)
	if len(cands) != 1 || cands[0].(map[string]any)["pool_id"] != poolID || cands[0].(map[string]any)["complete"] != true {
		t.Fatalf("candidates: %v", cands)
	}
	res := b.must("POST", "/pool-imports", map[string]any{"pool_id": poolID}, "")
	if res["source"] != "skeleton" || res["volumes"] != float64(2) || res["snapshots"] != float64(1) {
		t.Fatalf("import: %v", res)
	}
	b.reconcile()
	p, _ := b.st.GetPool(poolID)
	if p.HealthStatus != store.HealthStatusOnline || len(p.DiskIDs) != 2 {
		t.Fatalf("imported pool: %s %s %v", p.HealthStatus, p.StatusMessage, p.DiskIDs)
	}
	// SPDK can't tell the snapshot's volume from its clone: one owns it, the other is its
	// clone, and the result says so.
	s, err := b.st.GetSnapshot(s1["id"].(string))
	if err != nil {
		t.Fatal(err)
	}
	other := map[string]string{volID: clone["id"].(string), clone["id"].(string): volID}[s.VolumeID]
	c, err := b.st.GetVolume(other)
	if other == "" || err != nil || c.ParentSnapshotID != s1["id"] {
		t.Fatalf("snapshot owned by %s; the other volume %+v %v", s.VolumeID, c, err)
	}
	if !strings.Contains(fmt.Sprint(res["warnings"]), "recorded as taken of") {
		t.Fatalf("no warning about the ambiguous snapshot: %v", res["warnings"])
	}

	// The data is there: export the volume again and read it.
	nqn := b.exportTo(volID, "v2", hostA)
	b.initiator(nqn, hostA).mustRead(0, first)
}
