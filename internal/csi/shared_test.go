package csi

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/velocor/velocor/internal/auth"
	"github.com/velocor/velocor/internal/dataplane/spdkplane"
	"github.com/velocor/velocor/internal/server"
	"github.com/velocor/velocor/internal/spdk/spdktest"
	"github.com/velocor/velocor/internal/store"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func sharedHarness(t *testing.T) (*ControllerServer, *spdktest.Fake) {
	t.Helper()
	st, err := store.OpenBoltStore(filepath.Join(t.TempDir(), "velocor.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	fake := seedBackend(t, st, "pool-nvme")
	if err := st.CreatePortal(&store.Portal{ID: "portal-nvme", Name: "nvme", Protocol: store.ExportProtocolNVMeoFTCP, IPAddress: "127.0.0.1", Port: 4420}); err != nil {
		t.Fatal(err)
	}
	authMgr, err := auth.NewManager(st, "test-pass-123")
	if err != nil {
		t.Fatal(err)
	}
	_, pat, err := authMgr.CreateApiToken("csi", store.RoleCSIProvisioner, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(server.New(st, spdkplane.New(fake.Client(), nil), nil, authMgr, nil, nil).Handler())
	t.Cleanup(ts.Close)
	return NewControllerServer(NewHTTPClient(ts.URL, pat, true)), fake
}

func blockCap(mode csi.VolumeCapability_AccessMode_Mode) *csi.VolumeCapability {
	return &csi.VolumeCapability{
		AccessType: &csi.VolumeCapability_Block{Block: &csi.VolumeCapability_BlockVolume{}},
		AccessMode: &csi.VolumeCapability_AccessMode{Mode: mode},
	}
}

// What a KubeVirt live migration does: a ReadWriteMany raw block volume is published to the
// target node while the source still uses it, then unpublished from the source.
func TestSharedBlockVolumeAcrossNodes(t *testing.T) {
	ctx := context.Background()
	cs, fake := sharedHarness(t)
	rwx := blockCap(csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER)

	// A ReadWriteMany filesystem is refused: two nodes would corrupt it.
	fsRWX := &csi.VolumeCapability{
		AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{FsType: "ext4"}},
		AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER},
	}
	if _, err := cs.CreateVolume(ctx, &csi.CreateVolumeRequest{Name: "pvc-fs", VolumeCapabilities: []*csi.VolumeCapability{fsRWX},
		CapacityRange: &csi.CapacityRange{RequiredBytes: 1 << 30}}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("RWX filesystem: %v, want InvalidArgument", err)
	}

	vol, err := cs.CreateVolume(ctx, &csi.CreateVolumeRequest{Name: "pvc-vm", VolumeCapabilities: []*csi.VolumeCapability{rwx},
		CapacityRange: &csi.CapacityRange{RequiredBytes: 1 << 30}, Parameters: map[string]string{"protocol": "NVMEOF_TCP"}})
	if err != nil {
		t.Fatal(err)
	}
	id, vctx := vol.GetVolume().GetVolumeId(), vol.GetVolume().GetVolumeContext()
	publish := func(node string) map[string]string {
		t.Helper()
		r, err := cs.ControllerPublishVolume(ctx, &csi.ControllerPublishVolumeRequest{VolumeId: id, NodeId: node, VolumeCapability: rwx, VolumeContext: vctx})
		if err != nil {
			t.Fatalf("publish %s: %v", node, err)
		}
		return r.GetPublishContext()
	}
	hosts := func(nqn string) []string {
		t.Helper()
		sub, ok := fake.Subsystem(nqn)
		if !ok {
			return nil
		}
		var out []string
		for _, h := range sub.Hosts {
			out = append(out, h.Nqn)
		}
		slices.Sort(out)
		return out
	}

	a := publish("node-a")
	nqn := a["target_nqn"]
	b := publish("node-b")
	if b["target_nqn"] != nqn {
		t.Fatalf("second node got another subsystem: %s vs %s", b["target_nqn"], nqn)
	}
	if got, want := hosts(nqn), []string{nodeHostNQN("node-a"), nodeHostNQN("node-b")}; !slices.Equal(got, slices.Sorted(slices.Values(want))) {
		t.Fatalf("hosts while both nodes use it: %v, want %v", got, want)
	}
	// Publishing again is idempotent.
	if publish("node-b")["target_nqn"] != nqn {
		t.Fatal("republish changed the subsystem")
	}
	// A single-node publish of a shared volume is refused.
	if _, err := cs.ControllerPublishVolume(ctx, &csi.ControllerPublishVolumeRequest{VolumeId: id, NodeId: "node-c",
		VolumeCapability: blockCap(csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER), VolumeContext: vctx}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("single-node publish of a shared volume: %v", err)
	}

	// The migration's end: the source leaves; the target keeps its access.
	if _, err := cs.ControllerUnpublishVolume(ctx, &csi.ControllerUnpublishVolumeRequest{VolumeId: id, NodeId: "node-a"}); err != nil {
		t.Fatal(err)
	}
	if got := hosts(nqn); !slices.Equal(got, []string{nodeHostNQN("node-b")}) {
		t.Fatalf("hosts after the source left: %v", got)
	}
	if _, err := cs.ControllerUnpublishVolume(ctx, &csi.ControllerUnpublishVolumeRequest{VolumeId: id, NodeId: "node-b"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := fake.Subsystem(nqn); ok {
		t.Fatal("shared subsystem left behind after the last node unpublished")
	}
	// Now it can be used by a single node again.
	if _, err := cs.ControllerPublishVolume(ctx, &csi.ControllerPublishVolumeRequest{VolumeId: id, NodeId: "node-c",
		VolumeCapability: blockCap(csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER), VolumeContext: vctx}); err != nil {
		t.Fatalf("single-node publish after the shared export went away: %v", err)
	}
}
