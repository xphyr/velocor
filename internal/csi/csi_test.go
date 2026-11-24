package csi

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/kubernetes-csi/csi-test/v5/pkg/sanity"
	"github.com/velocor/velocor/internal/auth"
	"github.com/velocor/velocor/internal/dataplane/planetest"
	"github.com/velocor/velocor/internal/server"
	"github.com/velocor/velocor/internal/store"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

type fakeMounter struct {
	mu                  sync.Mutex
	mounted             map[string]bool
	disconnectedVolumes []string
	flushErr            error
}

func newFakeMounter() *fakeMounter {
	return &fakeMounter{mounted: map[string]bool{}}
}

func (f *fakeMounter) ConnectTarget(_ context.Context, publishCtx map[string]string, _, _ string) (string, error) {
	return "/dev/nvme0n1-" + publishCtx["export_id"], nil
}

func (f *fakeMounter) DisconnectTarget(_ context.Context, _ map[string]string) error {
	return nil
}

func (f *fakeMounter) DisconnectVolume(_ context.Context, volumeID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.disconnectedVolumes = append(f.disconnectedVolumes, volumeID)
	return nil
}

func (f *fakeMounter) FlushDevice(_ context.Context, _ string) error {
	return f.flushErr
}

func (f *fakeMounter) FormatAndMount(_ context.Context, _, target, _ string, _ []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := os.MkdirAll(target, 0750); err != nil {
		return err
	}
	f.mounted[target] = true
	return nil
}

func (f *fakeMounter) BindMount(_ context.Context, _, target string, _ []string, isBlock bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if isBlock {
		if err := os.MkdirAll(filepath.Dir(target), 0750); err != nil {
			return err
		}
		if err := os.WriteFile(target, nil, 0660); err != nil {
			return err
		}
	} else {
		if err := os.MkdirAll(target, 0750); err != nil {
			return err
		}
	}
	f.mounted[target] = true
	return nil
}

func (f *fakeMounter) Unmount(_ context.Context, target string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.mounted, target)
	return nil
}

func (f *fakeMounter) IsMountPoint(target string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.mounted[target], nil
}

func (f *fakeMounter) ResizeFilesystem(_ context.Context, _, _ string) error {
	return nil
}

func (f *fakeMounter) RescanTarget(_ context.Context, _ map[string]string) error {
	return nil
}

func (f *fakeMounter) GetVolumeStats(_ string) (VolumeStats, error) {
	return VolumeStats{
		AvailableBytes:  800 << 20,
		TotalBytes:      1024 << 20,
		UsedBytes:       224 << 20,
		AvailableInodes: 65000,
		TotalInodes:     65536,
		UsedInodes:      536,
	}, nil
}

func TestCSISanitySuite(t *testing.T) {
	tmp := t.TempDir()
	st, err := store.OpenBoltStore(filepath.Join(tmp, "velocor.db"))
	if err != nil {
		t.Fatalf("OpenBoltStore: %v", err)
	}
	defer func() { _ = st.Close() }()

	fake := seedBackend(t, st, "pool-nvme")
	if err := st.CreatePortal(&store.Portal{
		ID:        "portal-nvme",
		Name:      "nvme-tcp-default",
		Protocol:  store.ExportProtocolNVMeoFTCP,
		IPAddress: "127.0.0.1",
		Port:      4420,
	}); err != nil {
		t.Fatalf("CreatePortal: %v", err)
	}

	authMgr, err := auth.NewManager(st, "test-pass-123")
	if err != nil {
		t.Fatalf("auth.NewManager: %v", err)
	}
	_, patSecret, err := authMgr.CreateApiToken("csi-sanity", store.RoleCSIProvisioner, time.Hour)
	if err != nil {
		t.Fatalf("CreateApiToken: %v", err)
	}

	srv := server.New(st, fake, nil, authMgr, nil, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	sockPath := filepath.Join(tmp, "csi.sock")
	endpoint := "unix://" + sockPath
	client := NewHTTPClient(ts.URL, patSecret, true)

	drv, err := NewDriver(DriverConfig{
		DriverName: DefaultDriverName,
		Endpoint:   endpoint,
		NodeID:     "sanity-node-1",
		Mode:       ModeAll,
		Client:     client,
		Mounter:    newFakeMounter(),
	})
	if err != nil {
		t.Fatalf("NewDriver: %v", err)
	}

	go func() {
		_ = drv.Run()
	}()
	defer drv.Stop()

	// Wait briefly for unix socket to appear.
	for i := 0; i < 50; i++ {
		if _, err := os.Stat(sockPath); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	cfg := sanity.NewTestConfig()
	cfg.Address = endpoint
	cfg.TargetPath = filepath.Join(tmp, "target")
	cfg.StagingPath = filepath.Join(tmp, "staging")
	sanity.Test(t, cfg)
}

func startDriver(t *testing.T, cfg DriverConfig) *grpc.ClientConn {
	t.Helper()
	sockPath := filepath.Join(t.TempDir(), "csi.sock")
	cfg.Endpoint = "unix://" + sockPath
	if cfg.NodeID == "" {
		cfg.NodeID = "node-1"
	}
	drv, err := NewDriver(cfg)
	if err != nil {
		t.Fatalf("NewDriver: %v", err)
	}
	go func() { _ = drv.Run() }()
	t.Cleanup(drv.Stop)
	for i := 0; i < 50; i++ {
		if _, err := os.Stat(sockPath); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	conn, err := grpc.NewClient(cfg.Endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func advertisesSnapshotMetadata(t *testing.T, conn *grpc.ClientConn) bool {
	t.Helper()
	caps, err := csi.NewIdentityClient(conn).GetPluginCapabilities(context.Background(), &csi.GetPluginCapabilitiesRequest{})
	if err != nil {
		t.Fatalf("GetPluginCapabilities: %v", err)
	}
	for _, c := range caps.GetCapabilities() {
		if c.GetService().GetType() == csi.PluginCapability_Service_SNAPSHOT_METADATA_SERVICE {
			return true
		}
	}
	return false
}

// SnapshotMetadata (CBT) is off unless enabled (it needs the external-snapshot-metadata
// sidecar): neither advertised nor served. A node plugin never serves it.
func TestSnapshotMetadataOffByDefault(t *testing.T) {
	for _, cfg := range []DriverConfig{{Mode: ModeAll}, {Mode: ModeController}, {Mode: ModeNode, SnapshotMetadata: true}} {
		t.Run(string(cfg.Mode), func(t *testing.T) {
			conn := startDriver(t, cfg)
			if advertisesSnapshotMetadata(t, conn) {
				t.Fatal("driver advertises SNAPSHOT_METADATA_SERVICE")
			}
			stream, err := csi.NewSnapshotMetadataClient(conn).GetMetadataAllocated(context.Background(), &csi.GetMetadataAllocatedRequest{SnapshotId: "s"})
			if err == nil {
				_, err = stream.Recv()
			}
			if status.Code(err) != codes.Unimplemented {
				t.Fatalf("GetMetadataAllocated: %v, want Unimplemented", err)
			}
		})
	}
}

type mockCBTClient struct {
	VelocorClient
	alloc *BlockMetadataResult
	delta *BlockMetadataResult
	err   error
	base  string
}

func (m *mockCBTClient) GetSnapshotAllocatedBlocks(_ context.Context, _ string) (*BlockMetadataResult, error) {
	return m.alloc, m.err
}

func (m *mockCBTClient) GetSnapshotChangedBlocks(_ context.Context, _, base string) (*BlockMetadataResult, error) {
	m.base = base
	return m.delta, m.err
}

func TestSnapshotMetadataCBT(t *testing.T) {
	mock := &mockCBTClient{
		alloc: &BlockMetadataResult{SnapshotID: "snap-2", VolumeSizeBytes: 16 << 20, Extents: []BlockExtent{
			{OffsetBytes: 0, LengthBytes: 4 << 20}, {OffsetBytes: 8 << 20, LengthBytes: 4 << 20}}},
		delta: &BlockMetadataResult{SnapshotID: "snap-2", VolumeSizeBytes: 16 << 20, Extents: []BlockExtent{
			{OffsetBytes: 8 << 20, LengthBytes: 4 << 20}}},
	}
	conn := startDriver(t, DriverConfig{Mode: ModeController, Client: mock, SnapshotMetadata: true})
	if !advertisesSnapshotMetadata(t, conn) {
		t.Fatal("enabled driver doesn't advertise SNAPSHOT_METADATA_SERVICE")
	}
	cbt := csi.NewSnapshotMetadataClient(conn)

	// Allocated, one tuple per message (max_results 1).
	stream, err := cbt.GetMetadataAllocated(context.Background(), &csi.GetMetadataAllocatedRequest{SnapshotId: "snap-2", MaxResults: 1})
	if err != nil {
		t.Fatal(err)
	}
	var got []*csi.BlockMetadata
	for {
		resp, err := stream.Recv()
		if err != nil {
			break
		}
		if len(resp.GetBlockMetadata()) > 1 || resp.GetVolumeCapacityBytes() != 16<<20 {
			t.Fatalf("message: %+v", resp)
		}
		got = append(got, resp.GetBlockMetadata()...)
	}
	if len(got) != 2 || got[0].GetByteOffset() != 0 || got[1].GetByteOffset() != 8<<20 {
		t.Fatalf("allocated: %+v", got)
	}

	// starting_offset inside an extent: the first tuple is clipped to start there.
	stream, err = cbt.GetMetadataAllocated(context.Background(), &csi.GetMetadataAllocatedRequest{SnapshotId: "snap-2", StartingOffset: 2 << 20})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := stream.Recv()
	if err != nil || len(resp.GetBlockMetadata()) != 2 || resp.GetBlockMetadata()[0].GetByteOffset() != 2<<20 || resp.GetBlockMetadata()[0].GetSizeBytes() != 2<<20 {
		t.Fatalf("allocated from 2 MiB: %+v %v", resp, err)
	}

	// Delta.
	dStream, err := cbt.GetMetadataDelta(context.Background(), &csi.GetMetadataDeltaRequest{BaseSnapshotId: "snap-1", TargetSnapshotId: "snap-2"})
	if err != nil {
		t.Fatal(err)
	}
	dResp, err := dStream.Recv()
	if err != nil || len(dResp.GetBlockMetadata()) != 1 || dResp.GetBlockMetadata()[0].GetByteOffset() != 8<<20 || mock.base != "snap-1" {
		t.Fatalf("delta: %+v %v (base %q)", dResp, err, mock.base)
	}

	// A data plane that can't report allocation (velod answers 501): Unimplemented, never data.
	mock.err = &ErrAPI{StatusCode: 501, Message: "changed block tracking unsupported"}
	stream, err = cbt.GetMetadataAllocated(context.Background(), &csi.GetMetadataAllocatedRequest{SnapshotId: "snap-2"})
	if err == nil {
		_, err = stream.Recv()
	}
	if status.Code(err) != codes.Unimplemented {
		t.Fatalf("501 from velod: %v, want Unimplemented", err)
	}
	mock.err = &ErrAPI{StatusCode: 400, Message: "snapshot snap-9 is not an ancestor of snap-2"}
	dStream, err = cbt.GetMetadataDelta(context.Background(), &csi.GetMetadataDeltaRequest{BaseSnapshotId: "snap-9", TargetSnapshotId: "snap-2"})
	if err == nil {
		_, err = dStream.Recv()
	}
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("400 from velod: %v, want InvalidArgument", err)
	}
}

func TestVolumeGroupSnapshot(t *testing.T) {
	tmp := t.TempDir()
	db, err := store.OpenBoltStore(filepath.Join(tmp, "velocor.db"))
	if err != nil {
		t.Fatalf("OpenBoltStore: %v", err)
	}
	defer func() { _ = db.Close() }()

	fake := seedBackend(t, db, "pool-1")
	pool, _ := db.GetPool("pool-1")
	for _, vid := range []string{"vol-a", "vol-b"} {
		uuid, err := fake.CreateVolume(context.Background(), pool.ThinPoolName, vid, 1<<30, true)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.CreateVolume(&store.Volume{
			ID:        vid,
			Name:      vid,
			PoolID:    "pool-1",
			UUID:      uuid,
			Handle:    pool.ThinPoolName + "/" + vid,
			SizeBytes: 1 << 30,
		}); err != nil {
			t.Fatalf("CreateVolume %s: %v", vid, err)
		}
	}

	authMgr, err := auth.NewManager(db, "test-pass-123")
	if err != nil {
		t.Fatalf("auth.NewManager: %v", err)
	}
	_, rawToken, err := authMgr.CreateApiToken("vgs-test", store.RoleCSIProvisioner, time.Hour)
	if err != nil {
		t.Fatalf("CreateApiToken: %v", err)
	}

	srv := server.New(db, fake, nil, authMgr, nil, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	sockPath := filepath.Join(tmp, "vgs.sock")
	endpoint := "unix://" + sockPath
	drv, err := NewDriver(DriverConfig{
		Endpoint: endpoint,
		Mode:     ModeController,
		Client:   NewHTTPClient(ts.URL, rawToken, true),
	})
	if err != nil {
		t.Fatalf("NewDriver: %v", err)
	}
	go func() { _ = drv.Run() }()
	defer drv.Stop()

	for i := 0; i < 50; i++ {
		if _, err := os.Stat(sockPath); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	conn, err := grpc.NewClient(endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	defer func() { _ = conn.Close() }()

	ctx := context.Background()
	idClient := csi.NewIdentityClient(conn)
	caps, err := idClient.GetPluginCapabilities(ctx, &csi.GetPluginCapabilitiesRequest{})
	if err != nil {
		t.Fatalf("GetPluginCapabilities: %v", err)
	}
	hasGroupCap := false
	for _, c := range caps.GetCapabilities() {
		if svc := c.GetService(); svc != nil && svc.GetType() == csi.PluginCapability_Service_GROUP_CONTROLLER_SERVICE {
			hasGroupCap = true
		}
	}
	if !hasGroupCap {
		t.Fatalf("missing GROUP_CONTROLLER_SERVICE plugin capability")
	}

	gc := csi.NewGroupControllerClient(conn)
	gCaps, err := gc.GroupControllerGetCapabilities(ctx, &csi.GroupControllerGetCapabilitiesRequest{})
	if err != nil {
		t.Fatalf("GroupControllerGetCapabilities: %v", err)
	}
	if len(gCaps.GetCapabilities()) == 0 {
		t.Fatalf("expected non-empty group controller capabilities")
	}

	// Create VolumeGroupSnapshot across vol-a and vol-b.
	created, err := gc.CreateVolumeGroupSnapshot(ctx, &csi.CreateVolumeGroupSnapshotRequest{
		Name:            "vgs-1",
		SourceVolumeIds: []string{"vol-a", "vol-b"},
	})
	if err != nil {
		t.Fatalf("CreateVolumeGroupSnapshot: %v", err)
	}
	vgs := created.GetGroupSnapshot()
	if vgs.GetGroupSnapshotId() == "" || !vgs.GetReadyToUse() || len(vgs.GetSnapshots()) != 2 {
		t.Fatalf("unexpected group snapshot: %+v", vgs)
	}

	// Idempotent retry with same volume set succeeds.
	retry, err := gc.CreateVolumeGroupSnapshot(ctx, &csi.CreateVolumeGroupSnapshotRequest{
		Name:            "vgs-1",
		SourceVolumeIds: []string{"vol-b", "vol-a"},
	})
	if err != nil {
		t.Fatalf("idempotent CreateVolumeGroupSnapshot: %v", err)
	}
	if retry.GetGroupSnapshot().GetGroupSnapshotId() != vgs.GetGroupSnapshotId() {
		t.Fatalf("expected same group snapshot ID on retry")
	}

	// Same name with different volume set returns AlreadyExists.
	if _, err := gc.CreateVolumeGroupSnapshot(ctx, &csi.CreateVolumeGroupSnapshotRequest{
		Name:            "vgs-1",
		SourceVolumeIds: []string{"vol-a"},
	}); status.Code(err) != codes.AlreadyExists {
		t.Fatalf("expected AlreadyExists on mismatched volume set, got %v", err)
	}

	// GetVolumeGroupSnapshot succeeds and checks snapshot_ids consistency.
	snapIDs := []string{vgs.GetSnapshots()[0].GetSnapshotId(), vgs.GetSnapshots()[1].GetSnapshotId()}
	got, err := gc.GetVolumeGroupSnapshot(ctx, &csi.GetVolumeGroupSnapshotRequest{
		GroupSnapshotId: vgs.GetGroupSnapshotId(),
		SnapshotIds:     snapIDs,
	})
	if err != nil {
		t.Fatalf("GetVolumeGroupSnapshot: %v", err)
	}
	if len(got.GetGroupSnapshot().GetSnapshots()) != 2 {
		t.Fatalf("expected 2 snapshots in GetVolumeGroupSnapshot, got %d", len(got.GetGroupSnapshot().GetSnapshots()))
	}

	if _, err := gc.GetVolumeGroupSnapshot(ctx, &csi.GetVolumeGroupSnapshotRequest{
		GroupSnapshotId: vgs.GetGroupSnapshotId(),
		SnapshotIds:     []string{"snap-wrong"},
	}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument on snapshot_ids mismatch, got %v", err)
	}

	// DeleteVolumeGroupSnapshot removes group and member snapshots, and is idempotent.
	if _, err := gc.DeleteVolumeGroupSnapshot(ctx, &csi.DeleteVolumeGroupSnapshotRequest{
		GroupSnapshotId: vgs.GetGroupSnapshotId(),
		SnapshotIds:     snapIDs,
	}); err != nil {
		t.Fatalf("DeleteVolumeGroupSnapshot: %v", err)
	}
	if _, err := gc.DeleteVolumeGroupSnapshot(ctx, &csi.DeleteVolumeGroupSnapshotRequest{
		GroupSnapshotId: vgs.GetGroupSnapshotId(),
		SnapshotIds:     snapIDs,
	}); err != nil {
		t.Fatalf("idempotent DeleteVolumeGroupSnapshot: %v", err)
	}

	if _, err := gc.GetVolumeGroupSnapshot(ctx, &csi.GetVolumeGroupSnapshotRequest{
		GroupSnapshotId: vgs.GetGroupSnapshotId(),
	}); status.Code(err) != codes.NotFound {
		t.Fatalf("expected NotFound after delete, got %v", err)
	}

	snaps, err := db.ListSnapshots("", "")
	if err != nil {
		t.Fatalf("ListSnapshots: %v", err)
	}
	if len(snaps) != 0 {
		t.Fatalf("expected 0 remaining snapshots after group delete, got %d", len(snaps))
	}
}

// seedBackend creates a data plane holding one claimed NVMe disk, a RAID0 array and a thin pool,
// and records the matching disk + pool (ID poolID) in st.
func seedBackend(t *testing.T, st *store.BoltStore, poolID string) *planetest.Fake {
	t.Helper()
	fake := planetest.New(t)
	planetest.Seed(t, fake, st, &store.Pool{ID: poolID, Name: poolID, RaidLevel: store.RaidLevel0, TotalBytes: 100 << 30, FreeBytes: 100 << 30,
		HealthStatus: store.HealthStatusOnline},
		&store.Disk{ID: "disk-1", Type: store.DiskTypeNVMe, PCIAddress: "0000:00:05.0", SizeBytes: 100 << 30})
	return fake
}

// A protected volume's DeleteVolume fails with FailedPrecondition (the provisioner retries);
// an unprotected one is deleted, kept by velod until its retention ends (docs/review-history.md P1-4).
func TestDeleteVolumeProtectionAndRetention(t *testing.T) {
	tmp := t.TempDir()
	db, err := store.OpenBoltStore(filepath.Join(tmp, "velocor.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	fake := seedBackend(t, db, "pool-1")
	pool, _ := db.GetPool("pool-1")
	uuid, err := fake.CreateVolume(context.Background(), pool.ThinPoolName, "vol-p", 1<<30, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CreateVolume(&store.Volume{ID: "vol-p", Name: "vol-p", PoolID: "pool-1", UUID: uuid,
		Handle: pool.ThinPoolName + "/vol-p", SizeBytes: 1 << 30, DeletionProtection: true}); err != nil {
		t.Fatal(err)
	}
	authMgr, err := auth.NewManager(db, "test-pass-123")
	if err != nil {
		t.Fatal(err)
	}
	_, rawToken, err := authMgr.CreateApiToken("csi", store.RoleCSIProvisioner, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(server.New(db, fake, nil, authMgr, nil, nil).Handler())
	defer ts.Close()
	cs := &ControllerServer{client: NewHTTPClient(ts.URL, rawToken, true)}
	ctx := context.Background()

	_, err = cs.DeleteVolume(ctx, &csi.DeleteVolumeRequest{VolumeId: "vol-p"})
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "deletion protection") {
		t.Fatalf("protected volume: %v", err)
	}
	if _, err := db.MutateVolume("vol-p", func(v *store.Volume) error { v.DeletionProtection = false; return nil }); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ { // the second call is a retry
		if _, err := cs.DeleteVolume(ctx, &csi.DeleteVolumeRequest{VolumeId: "vol-p"}); err != nil {
			t.Fatalf("DeleteVolume #%d: %v", i+1, err)
		}
	}
	if v, err := db.GetVolume("vol-p"); err != nil || !v.Deleted() || !fake.VolumeExists(pool.ThinPoolName+"/vol-p") {
		t.Fatalf("volume should be kept as deleted: %+v %v", v, err)
	}
	if _, err := cs.ControllerGetVolume(ctx, &csi.ControllerGetVolumeRequest{VolumeId: "vol-p"}); status.Code(err) != codes.NotFound {
		t.Fatalf("ControllerGetVolume of a deleted volume: %v", err)
	}
}
