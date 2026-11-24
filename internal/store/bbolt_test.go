package store

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *BoltStore {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "velocor-test.db")
	st, err := OpenBoltStore(dbPath)
	if err != nil {
		t.Fatalf("OpenBoltStore(%s) failed: %v", dbPath, err)
	}
	t.Cleanup(func() {
		_ = st.Close()
	})
	return st
}

func TestSystemAndAuthLifecycle(t *testing.T) {
	st := newTestStore(t)

	if _, err := st.GetSystemConfig(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for uninitialized system config, got %v", err)
	}

	cfg := &SystemConfig{
		NodeID:        "node-01",
		JWTSecret:     []byte("secret-jwt-key"),
		MasterKeySalt: []byte("master-salt"),
	}
	if err := st.SaveSystemConfig(cfg); err != nil {
		t.Fatalf("SaveSystemConfig failed: %v", err)
	}

	gotCfg, err := st.GetSystemConfig()
	if err != nil {
		t.Fatalf("GetSystemConfig failed: %v", err)
	}
	if gotCfg.NodeID != "node-01" || gotCfg.InitializedAt.IsZero() {
		t.Fatalf("unexpected SystemConfig: %+v", gotCfg)
	}

	admin := &AdminUser{
		Username:     "admin",
		PasswordHash: "$2a$10$testhash",
	}
	if err := st.SaveAdminUser(admin); err != nil {
		t.Fatalf("SaveAdminUser failed: %v", err)
	}
	gotAdmin, err := st.GetAdminUser("admin")
	if err != nil {
		t.Fatalf("GetAdminUser failed: %v", err)
	}
	if gotAdmin.PasswordHash != admin.PasswordHash {
		t.Fatalf("expected password hash %q, got %q", admin.PasswordHash, gotAdmin.PasswordHash)
	}

	tok := &ApiToken{
		ID:        "tok-1",
		Name:      "k8s-csi",
		Prefix:    "velo_pat_tok-1",
		TokenHash: "sha256-hash",
		Role:      RoleCSIProvisioner,
	}
	if err := st.CreateApiToken(tok); err != nil {
		t.Fatalf("CreateApiToken failed: %v", err)
	}
	if err := st.CreateApiToken(tok); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("expected ErrAlreadyExists on duplicate token, got %v", err)
	}

	usedAt := time.Now().UTC().Truncate(time.Second)
	if err := st.UpdateApiTokenLastUsed("tok-1", usedAt); err != nil {
		t.Fatalf("UpdateApiTokenLastUsed failed: %v", err)
	}

	gotTok, err := st.GetApiToken("tok-1")
	if err != nil {
		t.Fatalf("GetApiToken failed: %v", err)
	}
	if gotTok.LastUsedAt == nil || !gotTok.LastUsedAt.Equal(usedAt) {
		t.Fatalf("expected LastUsedAt=%v, got %v", usedAt, gotTok.LastUsedAt)
	}

	tokens, err := st.ListApiTokens()
	if err != nil || len(tokens) != 1 {
		t.Fatalf("ListApiTokens returned %d tokens, err=%v", len(tokens), err)
	}

	if err := st.DeleteApiToken("tok-1"); err != nil {
		t.Fatalf("DeleteApiToken failed: %v", err)
	}
	if _, err := st.GetApiToken("tok-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound after DeleteApiToken, got %v", err)
	}
}

func TestDiskBootGuardAndClaimLifecycle(t *testing.T) {
	st := newTestStore(t)

	bootDisk := &Disk{
		ID:           "disk-boot",
		Name:         "nvme0n1",
		Type:         DiskTypeNVMe,
		PCIAddress:   "0000:00:04.0",
		DevicePath:   "/dev/nvme0n1",
		SizeBytes:    100 * 1024 * 1024 * 1024,
		IsBootDevice: true,
		DriverState:  DiskDriverStateKernel,
	}
	if err := st.UpsertDisk(bootDisk); err != nil {
		t.Fatalf("UpsertDisk(bootDisk) failed: %v", err)
	}

	// Attempting to claim a boot disk must fail with ErrBootDeviceProtected.
	if _, err := st.ClaimDisk("disk-boot", DiskDriverStateKernel, "nvme0n1"); !errors.Is(err, ErrBootDeviceProtected) {
		t.Fatalf("expected ErrBootDeviceProtected when claiming boot disk, got %v", err)
	}

	// Upserting the boot disk with IsBootDevice=false must preserve IsBootDevice=true permanently.
	bootDisk.IsBootDevice = false
	if err := st.UpsertDisk(bootDisk); err != nil {
		t.Fatalf("UpsertDisk(bootDisk update) failed: %v", err)
	}
	persistedBoot, err := st.GetDisk("disk-boot")
	if err != nil {
		t.Fatalf("GetDisk(disk-boot) failed: %v", err)
	}
	if !persistedBoot.IsBootDevice {
		t.Fatalf("expected IsBootDevice to remain permanently true")
	}

	// Regular data NVMe drive can be claimed and released.
	dataDisk := &Disk{
		ID:          "disk-nvme1",
		Name:        "nvme1n1",
		Type:        DiskTypeNVMe,
		PCIAddress:  "0000:01:00.0",
		SizeBytes:   1024 * 1024 * 1024 * 1024,
		DriverState: DiskDriverStateKernel,
	}
	if err := st.UpsertDisk(dataDisk); err != nil {
		t.Fatalf("UpsertDisk(dataDisk) failed: %v", err)
	}

	claimed, err := st.ClaimDisk("disk-nvme1", DiskDriverStateKernel, "Nvme1n1")
	if err != nil {
		t.Fatalf("ClaimDisk failed: %v", err)
	}
	if !claimed.Claimed || claimed.DriverState != DiskDriverStateKernel || claimed.Handle != "Nvme1n1" {
		t.Fatalf("unexpected claimed disk state: %+v", claimed)
	}

	released, err := st.ReleaseDisk("disk-nvme1")
	if err != nil {
		t.Fatalf("ReleaseDisk failed: %v", err)
	}
	if released.Claimed || released.DriverState != DiskDriverStateKernel || released.Handle != "" {
		t.Fatalf("unexpected released disk state: %+v", released)
	}
}

func TestPoolVolumeSnapshotAndExportReferentialIntegrity(t *testing.T) {
	st := newTestStore(t)

	for _, id := range []string{"disk-1", "disk-2"} {
		if err := st.UpsertDisk(&Disk{
			ID:          id,
			Name:        id,
			Type:        DiskTypeNVMe,
			SizeBytes:   500 * 1024 * 1024 * 1024,
			DriverState: DiskDriverStateKernel,
		}); err != nil {
			t.Fatalf("UpsertDisk(%s) failed: %v", id, err)
		}
	}

	// Creating a pool with unclaimed disks must fail.
	pool := &Pool{
		ID:        "pool-1",
		Name:      "flash-tier0",
		RaidLevel: RaidLevel1,
		DiskIDs:   []string{"disk-1", "disk-2"},
	}
	if err := st.CreatePool(pool); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("expected ErrInvalidArgument when creating pool with unclaimed disks, got %v", err)
	}

	// Claim both disks.
	for _, id := range []string{"disk-1", "disk-2"} {
		if _, err := st.ClaimDisk(id, DiskDriverStateKernel, "bdev_"+id); err != nil {
			t.Fatalf("ClaimDisk(%s) failed: %v", id, err)
		}
	}

	if err := st.CreatePool(pool); err != nil {
		t.Fatalf("CreatePool failed: %v", err)
	}

	// Verify both disks now have PoolID="pool-1" and cannot be released while in the pool.
	d1, err := st.GetDisk("disk-1")
	if err != nil || d1.PoolID != "pool-1" {
		t.Fatalf("expected disk-1 PoolID=pool-1, got %+v (err=%v)", d1, err)
	}
	if _, err := st.ReleaseDisk("disk-1"); !errors.Is(err, ErrInUse) {
		t.Fatalf("expected ErrInUse when releasing pooled disk, got %v", err)
	}

	// Create a Volume inside the Pool.
	vol := &Volume{
		ID:            "vol-1",
		Name:          "pvc-database",
		PoolID:        "pool-1",
		SizeBytes:     50 * 1024 * 1024 * 1024,
		ThinProvision: true,
	}
	if err := st.CreateVolume(vol); err != nil {
		t.Fatalf("CreateVolume failed: %v", err)
	}
	if vol.AttachmentState != AttachmentStateUnattached {
		t.Fatalf("expected initial volume AttachmentState=UNATTACHED, got %s", vol.AttachmentState)
	}

	// Pool cannot be deleted while it contains a Volume.
	if err := st.DeletePool("pool-1"); !errors.Is(err, ErrInUse) {
		t.Fatalf("expected ErrInUse when deleting non-empty pool, got %v", err)
	}

	// Create 3 Snapshots and verify automatic CBT lineage (ParentSnapshotID & SequenceNumber).
	snap1 := &Snapshot{ID: "snap-1", Name: "s1", VolumeID: "vol-1"}
	snap2 := &Snapshot{ID: "snap-2", Name: "s2", VolumeID: "vol-1"}
	snap3 := &Snapshot{ID: "snap-3", Name: "s3", VolumeID: "vol-1"}
	for _, s := range []*Snapshot{snap1, snap2, snap3} {
		if err := st.CreateSnapshot(s); err != nil {
			t.Fatalf("CreateSnapshot(%s) failed: %v", s.ID, err)
		}
	}

	if snap1.SequenceNumber != 1 || snap1.ParentSnapshotID != "" {
		t.Fatalf("unexpected snap1 lineage: %+v", snap1)
	}
	if snap2.SequenceNumber != 2 || snap2.ParentSnapshotID != "snap-1" {
		t.Fatalf("unexpected snap2 lineage: %+v", snap2)
	}
	if snap3.SequenceNumber != 3 || snap3.ParentSnapshotID != "snap-2" {
		t.Fatalf("unexpected snap3 lineage: %+v", snap3)
	}

	// Deleting intermediate snap-2 must stitch snap-3's ParentSnapshotID to snap-1.
	if err := st.DeleteSnapshot("snap-2"); err != nil {
		t.Fatalf("DeleteSnapshot(snap-2) failed: %v", err)
	}
	gotSnap3, err := st.GetSnapshot("snap-3")
	if err != nil {
		t.Fatalf("GetSnapshot(snap-3) failed: %v", err)
	}
	if gotSnap3.ParentSnapshotID != "snap-1" {
		t.Fatalf("expected snap-3 ParentSnapshotID to stitch to snap-1, got %q", gotSnap3.ParentSnapshotID)
	}

	// Clone a new volume from snap-1; snap-1 cannot be deleted while the clone exists.
	cloneVol := &Volume{
		ID:               "vol-clone",
		Name:             "pvc-database-clone",
		PoolID:           "pool-1",
		SizeBytes:        vol.SizeBytes,
		ThinProvision:    true,
		ParentSnapshotID: "snap-1",
	}
	if err := st.CreateVolume(cloneVol); err != nil {
		t.Fatalf("CreateVolume(cloneVol) failed: %v", err)
	}
	if err := st.DeleteSnapshot("snap-1"); !errors.Is(err, ErrInUse) {
		t.Fatalf("expected ErrInUse when deleting snapshot backing a clone, got %v", err)
	}

	// Create Portal, InitiatorGroup, and Export for vol-1.
	portal := &Portal{
		ID:        "portal-1",
		Name:      "nvmeof-tcp-0",
		Protocol:  ExportProtocolNVMeoFTCP,
		IPAddress: "10.100.0.10",
		Port:      4420,
	}
	if err := st.CreatePortal(portal); err != nil {
		t.Fatalf("CreatePortal failed: %v", err)
	}

	ig := &InitiatorGroup{
		ID:       "ig-1",
		Name:     "k8s-workers",
		HostNQNs: []string{"nqn.2014-08.org.nvmexpress:uuid:worker-1"},
	}
	if err := st.CreateInitiatorGroup(ig); err != nil {
		t.Fatalf("CreateInitiatorGroup failed: %v", err)
	}

	exp := &Export{
		ID:                "exp-1",
		Name:              "exp-pvc-database",
		VolumeID:          "vol-1",
		Protocol:          ExportProtocolNVMeoFTCP,
		SubsystemNQN:      "nqn.2026-11.io.velocor:pvc-database",
		NamespaceID:       1,
		PortalIDs:         []string{"portal-1"},
		InitiatorGroupIDs: []string{"ig-1"},
	}
	if err := st.CreateExport(exp); err != nil {
		t.Fatalf("CreateExport failed: %v", err)
	}

	// Verify vol-1 transitioned to BLOCK_EXPORT and cannot be attached to FILESYSTEM or deleted.
	gotVol1, err := st.GetVolume("vol-1")
	if err != nil {
		t.Fatalf("GetVolume(vol-1) failed: %v", err)
	}
	if gotVol1.AttachmentState != AttachmentStateBlockExport {
		t.Fatalf("expected vol-1 AttachmentState=BLOCK_EXPORT, got %s", gotVol1.AttachmentState)
	}
	if _, err := st.SetVolumeAttachmentState("vol-1", AttachmentStateFilesystem); !errors.Is(err, ErrAttachmentConflict) {
		t.Fatalf("expected ErrAttachmentConflict when transitioning BLOCK_EXPORT volume to FILESYSTEM, got %v", err)
	}
	if err := st.DeleteVolume("vol-1"); !errors.Is(err, ErrInUse) {
		t.Fatalf("expected ErrInUse when deleting exported volume, got %v", err)
	}

	// Portal and InitiatorGroup cannot be deleted while referenced by exp-1.
	if err := st.DeletePortal("portal-1"); !errors.Is(err, ErrInUse) {
		t.Fatalf("expected ErrInUse when deleting portal in use, got %v", err)
	}
	if err := st.DeleteInitiatorGroup("ig-1"); !errors.Is(err, ErrInUse) {
		t.Fatalf("expected ErrInUse when deleting initiator group in use, got %v", err)
	}

	// Deleting exp-1 transitions vol-1 back to UNATTACHED.
	if err := st.DeleteExport("exp-1"); err != nil {
		t.Fatalf("DeleteExport failed: %v", err)
	}
	gotVol1, err = st.GetVolume("vol-1")
	if err != nil {
		t.Fatalf("GetVolume(vol-1) failed: %v", err)
	}
	if gotVol1.AttachmentState != AttachmentStateUnattached {
		t.Fatalf("expected vol-1 AttachmentState=UNATTACHED after export removal, got %s", gotVol1.AttachmentState)
	}

	// Clean up clone, snapshots, volume, and pool in order.
	if err := st.DeleteVolume("vol-clone"); err != nil {
		t.Fatalf("DeleteVolume(vol-clone) failed: %v", err)
	}
	if err := st.DeleteSnapshot("snap-1"); err != nil {
		t.Fatalf("DeleteSnapshot(snap-1) failed: %v", err)
	}
	if err := st.DeleteSnapshot("snap-3"); err != nil {
		t.Fatalf("DeleteSnapshot(snap-3) failed: %v", err)
	}
	if err := st.DeleteVolume("vol-1"); err != nil {
		t.Fatalf("DeleteVolume(vol-1) failed: %v", err)
	}
	if err := st.DeletePool("pool-1"); err != nil {
		t.Fatalf("DeletePool(pool-1) failed: %v", err)
	}

	// Verify member disks had PoolID cleared and can now be released.
	for _, id := range []string{"disk-1", "disk-2"} {
		d, err := st.GetDisk(id)
		if err != nil {
			t.Fatalf("GetDisk(%s) failed: %v", id, err)
		}
		if d.PoolID != "" {
			t.Fatalf("expected %s PoolID to be cleared after pool deletion, got %q", id, d.PoolID)
		}
		if _, err := st.ReleaseDisk(id); err != nil {
			t.Fatalf("ReleaseDisk(%s) after pool deletion failed: %v", id, err)
		}
	}
}
