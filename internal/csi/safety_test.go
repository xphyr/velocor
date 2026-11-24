package csi

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/velocor/velocor/internal/auth"
	"github.com/velocor/velocor/internal/server"
	"github.com/velocor/velocor/internal/store"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestFindNVMeNamespaceUsesSubsystemNotControllerName(t *testing.T) {
	sys := t.TempDir()
	m := &LinuxMounter{SysRoot: sys}
	// Two subsystems. Our target is nvme-subsys1, reached via controller nvme0, so its head
	// device is nvme1n1. The naive "/dev/<ctrl>n<nsid>" guess (nvme0n1) is another volume.
	writeFile(t, filepath.Join(sys, "class", "nvme-subsystem", "nvme-subsys0", "subsysnqn"), "nqn.other\n")
	writeFile(t, filepath.Join(sys, "class", "nvme-subsystem", "nvme-subsys0", "nvme0n1", ".keep"), "")
	writeFile(t, filepath.Join(sys, "class", "block", "nvme0n1", "nsid"), "1\n")
	writeFile(t, filepath.Join(sys, "class", "block", "nvme0n1", "uuid"), "aaaaaaaa-0000-0000-0000-000000000000\n")

	writeFile(t, filepath.Join(sys, "class", "nvme-subsystem", "nvme-subsys1", "subsysnqn"), "nqn.target\n")
	writeFile(t, filepath.Join(sys, "class", "nvme-subsystem", "nvme-subsys1", "nvme1n1", ".keep"), "")
	writeFile(t, filepath.Join(sys, "class", "block", "nvme1n1", "nsid"), "1\n")
	writeFile(t, filepath.Join(sys, "class", "block", "nvme1n1", "uuid"), "BBBBBBBB-0000-0000-0000-000000000000\n")
	writeFile(t, filepath.Join(sys, "class", "nvme", "nvme0", "subsysnqn"), "nqn.target\n")

	dev, err := m.findNVMeNamespace("nqn.target", "1", "bbbbbbbb-0000-0000-0000-000000000000")
	if err != nil || dev != "/dev/nvme1n1" {
		t.Fatalf("got %q, %v; want /dev/nvme1n1", dev, err)
	}
	if _, err := m.findNVMeNamespace("nqn.target", "1", "cccccccc-0000-0000-0000-000000000000"); err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("UUID mismatch must be refused, got %v", err)
	}
}

func TestParseBlkid(t *testing.T) {
	for _, tc := range []struct {
		out     string
		code    int
		want    string
		wantErr bool
	}{
		{"", 2, "", false}, // positively blank
		{"DEVNAME=/dev/x\nTYPE=xfs\n", 0, "xfs", false},    // existing filesystem
		{"DEVNAME=/dev/x\nPTTYPE=gpt\n", 0, "", true},      // partitioned: never format
		{"error: /dev/x: Input/output error", 4, "", true}, // probe failure: never format
		{"blkid: cannot open /dev/x: Permission denied", 1, "", true},
	} {
		got, err := parseBlkid([]byte(tc.out), tc.code)
		if got != tc.want || (err != nil) != tc.wantErr {
			t.Errorf("parseBlkid(%q, %d) = %q, %v", tc.out, tc.code, got, err)
		}
	}
}

func TestPublishIsPerNode(t *testing.T) {
	tmp := t.TempDir()
	st, err := store.OpenBoltStore(filepath.Join(tmp, "v.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	fake := seedBackend(t, st, "pool-1")
	if err := st.CreatePortal(&store.Portal{ID: "pt", Name: "pt", Protocol: store.ExportProtocolNVMeoFTCP, IPAddress: "10.0.0.1", Port: 4420}); err != nil {
		t.Fatal(err)
	}
	authMgr, _ := auth.NewManager(st, "pass-123456")
	_, tok, _ := authMgr.CreateApiToken("csi", store.RoleCSIProvisioner, time.Hour)
	ts := httptest.NewServer(server.New(st, fake, nil, authMgr, nil, nil).Handler())
	defer ts.Close()
	ctrl := NewControllerServer(NewHTTPClient(ts.URL, tok, true))
	ctx := context.Background()

	capab := &csi.VolumeCapability{
		AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{}},
		AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
	}
	vol, err := ctrl.CreateVolume(ctx, &csi.CreateVolumeRequest{Name: "pvc-1", VolumeCapabilities: []*csi.VolumeCapability{capab}})
	if err != nil {
		t.Fatal(err)
	}
	volID := vol.GetVolume().GetVolumeId()

	pubA, err := ctrl.ControllerPublishVolume(ctx, &csi.ControllerPublishVolumeRequest{VolumeId: volID, NodeId: "node-a", VolumeCapability: capab})
	if err != nil {
		t.Fatal(err)
	}
	if pubA.GetPublishContext()["volume_uuid"] == "" {
		t.Fatalf("publish context must carry the volume UUID for node-side verification")
	}
	if _, err := ctrl.ControllerPublishVolume(ctx, &csi.ControllerPublishVolumeRequest{VolumeId: volID, NodeId: "node-b", VolumeCapability: capab}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("publishing a single-node volume to a second node must fail with FailedPrecondition, got %v", err)
	}

	got, err := ctrl.ControllerGetVolume(ctx, &csi.ControllerGetVolumeRequest{VolumeId: volID})
	if err != nil || !slices.Equal(got.GetStatus().GetPublishedNodeIds(), []string{"node-a"}) {
		t.Fatalf("PublishedNodeIds must be node IDs: %v, %v", got.GetStatus().GetPublishedNodeIds(), err)
	}

	// A stale unpublish from another node must not tear down node-a's access.
	if _, err := ctrl.ControllerUnpublishVolume(ctx, &csi.ControllerUnpublishVolumeRequest{VolumeId: volID, NodeId: "node-b"}); err != nil {
		t.Fatal(err)
	}
	if exps, _ := st.ListExports(volID, ""); len(exps) != 1 {
		t.Fatalf("unpublish for node-b removed node-a's export")
	}
	if _, err := ctrl.ControllerUnpublishVolume(ctx, &csi.ControllerUnpublishVolumeRequest{VolumeId: volID, NodeId: "node-a"}); err != nil {
		t.Fatal(err)
	}
	if exps, _ := st.ListExports(volID, ""); len(exps) != 0 {
		t.Fatalf("unpublish for node-a left %d exports", len(exps))
	}
	if _, err := ctrl.ControllerPublishVolume(ctx, &csi.ControllerPublishVolumeRequest{VolumeId: volID, NodeId: "node-b", VolumeCapability: capab}); err != nil {
		t.Fatalf("after unpublish, node-b must be able to attach: %v", err)
	}
}

// A clone whose growth failed is grown by the retry, and a failed clone leaves no intermediate
// snapshot; expanding a raw block volume tells the node.
func TestCloneRetryAndBlockExpand(t *testing.T) {
	tmp := t.TempDir()
	st, err := store.OpenBoltStore(filepath.Join(tmp, "v.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	fake := seedBackend(t, st, "pool-1")
	if err := st.CreatePortal(&store.Portal{ID: "pt", Name: "pt", Protocol: store.ExportProtocolNVMeoFTCP, IPAddress: "10.0.0.1", Port: 4420}); err != nil {
		t.Fatal(err)
	}
	authMgr, _ := auth.NewManager(st, "pass-123456")
	_, tok, _ := authMgr.CreateApiToken("csi", store.RoleCSIProvisioner, time.Hour)
	ts := httptest.NewServer(server.New(st, fake, nil, authMgr, nil, nil).Handler())
	defer ts.Close()
	ctrl := NewControllerServer(NewHTTPClient(ts.URL, tok, true))
	ctx := context.Background()
	block := &csi.VolumeCapability{
		AccessType: &csi.VolumeCapability_Block{Block: &csi.VolumeCapability_BlockVolume{}},
		AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
	}
	src, err := ctrl.CreateVolume(ctx, &csi.CreateVolumeRequest{Name: "src", VolumeCapabilities: []*csi.VolumeCapability{block}, CapacityRange: &csi.CapacityRange{RequiredBytes: 1 << 30}})
	if err != nil {
		t.Fatal(err)
	}
	srcID := src.GetVolume().GetVolumeId()
	clone := &csi.CreateVolumeRequest{Name: "copy", VolumeCapabilities: []*csi.VolumeCapability{block}, CapacityRange: &csi.CapacityRange{RequiredBytes: 2 << 30},
		VolumeContentSource: &csi.VolumeContentSource{Type: &csi.VolumeContentSource_Volume{Volume: &csi.VolumeContentSource_VolumeSource{VolumeId: srcID}}}}

	fake.Fail("Clone", 1)
	if _, err := ctrl.CreateVolume(ctx, clone); err == nil {
		t.Fatal("a failed clone succeeded")
	}
	if snaps, _ := st.ListSnapshots(srcID, ""); len(snaps) != 0 {
		t.Errorf("a failed clone left %d snapshots", len(snaps))
	}
	fake.Fail("ResizeVolume", 1)
	if _, err := ctrl.CreateVolume(ctx, clone); err == nil {
		t.Fatal("a failed growth succeeded")
	}
	got, err := ctrl.CreateVolume(ctx, clone)
	if err != nil || got.GetVolume().GetCapacityBytes() != 2<<30 {
		t.Fatalf("retry: %v %v", got, err)
	}
	// Not a clone of that source: the name is taken.
	other := &csi.CreateVolumeRequest{Name: "src", VolumeCapabilities: []*csi.VolumeCapability{block}, CapacityRange: &csi.CapacityRange{RequiredBytes: 2 << 30}}
	if _, err := ctrl.CreateVolume(ctx, other); status.Code(err) != codes.AlreadyExists {
		t.Fatalf("a bigger volume of the same name: %v", err)
	}

	exp, err := ctrl.ControllerExpandVolume(ctx, &csi.ControllerExpandVolumeRequest{VolumeId: srcID, VolumeCapability: block, CapacityRange: &csi.CapacityRange{RequiredBytes: 3 << 30}})
	if err != nil || !exp.GetNodeExpansionRequired() || exp.GetCapacityBytes() != 3<<30 {
		t.Fatalf("expand: %v %v", exp, err)
	}
}
