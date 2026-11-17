package csi

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// blockingMounter hangs ConnectTarget for one volume, like a format stuck on a dead target.
type blockingMounter struct {
	*fakeMounter
	stuck   string
	release chan struct{}
}

func (b *blockingMounter) ConnectTarget(ctx context.Context, pc map[string]string, h, i string) (string, error) {
	if pc["export_id"] == b.stuck {
		<-b.release
	}
	return b.fakeMounter.ConnectTarget(ctx, pc, h, i)
}

func stageReq(t *testing.T, vol, export string) *csi.NodeStageVolumeRequest {
	return &csi.NodeStageVolumeRequest{
		VolumeId:          vol,
		StagingTargetPath: filepath.Join(t.TempDir(), "staging"),
		PublishContext:    map[string]string{"export_id": export},
		VolumeCapability: &csi.VolumeCapability{
			AccessType: &csi.VolumeCapability_Block{Block: &csi.VolumeCapability_BlockVolume{}},
			AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
		},
	}
}

// One hung volume must not block the node's other volumes (seen on NVW: a stuck iSCSI format
// made every NVMe volume on the node time out), and a second call for the busy volume is
// Aborted so the sidecar retries.
func TestNodeVolumeLocksAreIndependent(t *testing.T) {
	m := &blockingMounter{fakeMounter: newFakeMounter(), stuck: "exp-stuck", release: make(chan struct{})}
	s := NewNodeServer("node-1", m)
	done := make(chan error, 1)
	go func() { _, err := s.NodeStageVolume(context.Background(), stageReq(t, "vol-stuck", "exp-stuck")); done <- err }()
	time.Sleep(100 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := s.NodeStageVolume(ctx, stageReq(t, "vol-ok", "exp-ok")); err != nil {
		t.Fatalf("other volume blocked by a stuck one: %v", err)
	}
	if _, err := s.NodeStageVolume(ctx, stageReq(t, "vol-stuck", "exp-stuck")); status.Code(err) != codes.Aborted {
		t.Fatalf("busy volume: got %v, want Aborted", err)
	}
	close(m.release)
	if err := <-done; err != nil {
		t.Fatalf("stuck stage after release: %v", err)
	}
}
