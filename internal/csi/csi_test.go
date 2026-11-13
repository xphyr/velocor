package csi

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/kubernetes-csi/csi-test/v5/pkg/sanity"
	"github.com/velocor/velocor/internal/auth"
	"github.com/velocor/velocor/internal/server"
	"github.com/velocor/velocor/internal/store"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type fakeMounter struct {
	mu      sync.Mutex
	mounted map[string]bool
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

	if err := st.UpsertDisk(&store.Disk{
		ID:           "disk-1",
		PCIAddress:   "0000:00:05.0",
		SizeBytes:    100 << 30,
		DriverState:  store.DiskDriverStateKernel,
		HealthStatus: store.HealthStatusOnline,
	}); err != nil {
		t.Fatalf("UpsertDisk: %v", err)
	}
	if _, err := st.ClaimDisk("disk-1", store.DiskDriverStateVfioPCI, "Nvme0n1"); err != nil {
		t.Fatalf("ClaimDisk: %v", err)
	}
	if err := st.CreatePool(&store.Pool{
		ID:               "pool-nvme",
		Name:             "nvme-pool",
		LVSName:          "nvme-pool",
		DiskIDs:          []string{"disk-1"},
		TotalBytes:       100 << 30,
		FreeBytes:        100 << 30,
		ClusterSizeBytes: 4 << 20,
		HealthStatus:     store.HealthStatusOnline,
	}); err != nil {
		t.Fatalf("CreatePool: %v", err)
	}
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

	srv := server.New(st, nil, nil, authMgr, nil, nil)
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

type mockCBTClient struct {
	VelocorClient
	alloc *BlockMetadataResult
	delta *BlockMetadataResult
}

func (m *mockCBTClient) GetSnapshotAllocatedBlocks(_ context.Context, _ string) (*BlockMetadataResult, error) {
	return m.alloc, nil
}

func (m *mockCBTClient) GetSnapshotChangedBlocks(_ context.Context, _, _ string) (*BlockMetadataResult, error) {
	return m.delta, nil
}

func TestSnapshotMetadataCBT(t *testing.T) {
	tmp := t.TempDir()
	sockPath := filepath.Join(tmp, "cbt.sock")
	endpoint := "unix://" + sockPath

	mock := &mockCBTClient{
		alloc: &BlockMetadataResult{
			SnapshotID:      "snap-2",
			VolumeSizeBytes: 16 << 20,
			Extents: []BlockExtent{
				{OffsetBytes: 0, LengthBytes: 4 << 20},
				{OffsetBytes: 8 << 20, LengthBytes: 4 << 20},
			},
		},
		delta: &BlockMetadataResult{
			SnapshotID:      "snap-2",
			VolumeSizeBytes: 16 << 20,
			Extents: []BlockExtent{
				{OffsetBytes: 8 << 20, LengthBytes: 4 << 20},
			},
		},
	}

	drv, err := NewDriver(DriverConfig{
		Endpoint: endpoint,
		Mode:     ModeController,
		Client:   mock,
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

	cbt := csi.NewSnapshotMetadataClient(conn)

	// Test GetMetadataAllocated with MaxResults=1 (verifies streaming chunking).
	stream, err := cbt.GetMetadataAllocated(context.Background(), &csi.GetMetadataAllocatedRequest{
		SnapshotId: "snap-2",
		MaxResults: 1,
	})
	if err != nil {
		t.Fatalf("GetMetadataAllocated: %v", err)
	}
	var gotAlloc []*csi.BlockMetadata
	for {
		resp, err := stream.Recv()
		if err != nil {
			break
		}
		gotAlloc = append(gotAlloc, resp.GetBlockMetadata()...)
	}
	if len(gotAlloc) != 2 || gotAlloc[0].GetByteOffset() != 0 || gotAlloc[1].GetByteOffset() != 8<<20 {
		t.Fatalf("unexpected allocated metadata: %+v", gotAlloc)
	}

	// Test GetMetadataDelta.
	dStream, err := cbt.GetMetadataDelta(context.Background(), &csi.GetMetadataDeltaRequest{
		BaseSnapshotId:   "snap-1",
		TargetSnapshotId: "snap-2",
	})
	if err != nil {
		t.Fatalf("GetMetadataDelta: %v", err)
	}
	resp, err := dStream.Recv()
	if err != nil {
		t.Fatalf("Recv delta: %v", err)
	}
	if len(resp.GetBlockMetadata()) != 1 || resp.GetBlockMetadata()[0].GetByteOffset() != 8<<20 {
		t.Fatalf("unexpected delta metadata: %+v", resp.GetBlockMetadata())
	}
}
