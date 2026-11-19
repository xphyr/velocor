package csi

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const stageMetaFilename = ".velocor-stage.json"

type stageMetadata struct {
	DevicePath     string            `json:"device_path"`
	PublishContext map[string]string `json:"publish_context"`
	IsBlock        bool              `json:"is_block"`
}

// NodeServer implements the CSI Node service.
type NodeServer struct {
	csi.UnimplementedNodeServer
	nodeID           string
	mounter          Mounter
	publishedTargets map[string]map[string]bool
	stagingPaths     map[string]string // volume ID -> staging_target_path (for NodeExpandVolume)
	mu               sync.Mutex        // guards the maps only; never held across I/O
	busy             map[string]bool   // volume IDs with an operation in progress
}

// lockVolume serializes operations per volume. Calls for other volumes proceed, so one hung
// device (say, a format waiting on a stuck target) can't block every pod on the node. A second
// call for a busy volume gets Aborted, which the CSI sidecars retry.
func (s *NodeServer) lockVolume(id string) (func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.busy[id] {
		return nil, status.Errorf(codes.Aborted, "an operation on volume %s is already in progress", id)
	}
	s.busy[id] = true
	return func() {
		s.mu.Lock()
		delete(s.busy, id)
		s.mu.Unlock()
	}, nil
}

// NewNodeServer creates a CSI NodeServer.
func NewNodeServer(nodeID string, mounter Mounter) *NodeServer {
	return &NodeServer{
		nodeID:           nodeID,
		mounter:          mounter,
		publishedTargets: map[string]map[string]bool{},
		stagingPaths:     map[string]string{},
		busy:             map[string]bool{},
	}
}

func (s *NodeServer) NodeGetCapabilities(_ context.Context, _ *csi.NodeGetCapabilitiesRequest) (*csi.NodeGetCapabilitiesResponse, error) {
	caps := []csi.NodeServiceCapability_RPC_Type{
		csi.NodeServiceCapability_RPC_STAGE_UNSTAGE_VOLUME,
		csi.NodeServiceCapability_RPC_GET_VOLUME_STATS,
		csi.NodeServiceCapability_RPC_EXPAND_VOLUME,
		csi.NodeServiceCapability_RPC_SINGLE_NODE_MULTI_WRITER,
	}
	out := make([]*csi.NodeServiceCapability, 0, len(caps))
	for _, c := range caps {
		out = append(out, &csi.NodeServiceCapability{
			Type: &csi.NodeServiceCapability_Rpc{
				Rpc: &csi.NodeServiceCapability_RPC{Type: c},
			},
		})
	}
	return &csi.NodeGetCapabilitiesResponse{Capabilities: out}, nil
}

func (s *NodeServer) NodeGetInfo(_ context.Context, _ *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
	return &csi.NodeGetInfoResponse{
		NodeId: s.nodeID,
	}, nil
}

func (s *NodeServer) NodeStageVolume(ctx context.Context, req *csi.NodeStageVolumeRequest) (*csi.NodeStageVolumeResponse, error) {
	if req.GetVolumeId() == "" || req.GetStagingTargetPath() == "" || req.GetVolumeCapability() == nil {
		return nil, status.Error(codes.InvalidArgument, "volume_id, staging_target_path, and volume_capability are required")
	}
	if err := validateVolumeCapabilities([]*csi.VolumeCapability{req.GetVolumeCapability()}); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	unlock, err := s.lockVolume(req.GetVolumeId())
	if err != nil {
		return nil, err
	}
	defer unlock()

	devPath, err := s.mounter.ConnectTarget(ctx, req.GetPublishContext(), nodeHostNQN(s.nodeID), nodeInitiatorIQN(s.nodeID))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "connect target: %v", err)
	}

	isBlock := req.GetVolumeCapability().GetBlock() != nil
	if isBlock {
		if err := os.MkdirAll(req.GetStagingTargetPath(), 0750); err != nil {
			return nil, status.Errorf(codes.Internal, "mkdir staging: %v", err)
		}
	} else {
		mnt := req.GetVolumeCapability().GetMount()
		mountPath := filepath.Join(req.GetStagingTargetPath(), "mount")
		if err := s.mounter.FormatAndMount(ctx, devPath, mountPath, mnt.GetFsType(), mnt.GetMountFlags()); err != nil {
			return nil, status.Errorf(codes.Internal, "format and mount: %v", err)
		}
	}

	meta := stageMetadata{
		DevicePath:     devPath,
		PublishContext: req.GetPublishContext(),
		IsBlock:        isBlock,
	}
	b, err := json.Marshal(meta)
	if err == nil {
		err = os.WriteFile(filepath.Join(req.GetStagingTargetPath(), stageMetaFilename), b, 0600)
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "persist staging metadata: %v", err)
	}
	s.mu.Lock()
	s.stagingPaths[req.GetVolumeId()] = req.GetStagingTargetPath()
	s.mu.Unlock()
	return &csi.NodeStageVolumeResponse{}, nil
}

func (s *NodeServer) NodeUnstageVolume(ctx context.Context, req *csi.NodeUnstageVolumeRequest) (*csi.NodeUnstageVolumeResponse, error) {
	if req.GetVolumeId() == "" || req.GetStagingTargetPath() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume_id and staging_target_path are required")
	}

	unlock, err := s.lockVolume(req.GetVolumeId())
	if err != nil {
		return nil, err
	}
	defer unlock()

	mountPath := filepath.Join(req.GetStagingTargetPath(), "mount")
	if err := s.mounter.Unmount(ctx, mountPath); err != nil {
		return nil, status.Errorf(codes.Internal, "unmount staging: %v", err)
	}
	_ = os.Remove(mountPath)

	metaPath := filepath.Join(req.GetStagingTargetPath(), stageMetaFilename)
	if raw, err := os.ReadFile(metaPath); err == nil {
		var meta stageMetadata
		if json.Unmarshal(raw, &meta) == nil {
			if meta.IsBlock && meta.DevicePath != "" {
				if err := s.mounter.FlushDevice(ctx, meta.DevicePath); err != nil {
					return nil, status.Errorf(codes.Internal, "flush %s: %v", meta.DevicePath, err)
				}
			}
			// On failure keep the metadata, so the retried unstage disconnects again.
			if err := s.mounter.DisconnectTarget(ctx, meta.PublishContext); err != nil {
				return nil, status.Errorf(codes.Internal, "disconnect: %v", err)
			}
		}
		_ = os.Remove(metaPath)
	} else if errors.Is(err, os.ErrNotExist) {
		// No stage metadata (never staged here, or lost): still make sure nothing of the
		// volume stays connected, or the node keeps a session to a target that may be deleted
		// once Kubernetes detaches.
		if err := s.mounter.DisconnectVolume(ctx, req.GetVolumeId()); err != nil {
			return nil, status.Errorf(codes.Internal, "disconnect: %v", err)
		}
	}
	s.mu.Lock()
	delete(s.stagingPaths, req.GetVolumeId())
	s.mu.Unlock()
	return &csi.NodeUnstageVolumeResponse{}, nil
}

func (s *NodeServer) NodePublishVolume(ctx context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	if req.GetVolumeId() == "" || req.GetTargetPath() == "" || req.GetStagingTargetPath() == "" || req.GetVolumeCapability() == nil {
		return nil, status.Error(codes.InvalidArgument, "volume_id, staging_target_path, target_path, and volume_capability are required")
	}
	if err := validateVolumeCapabilities([]*csi.VolumeCapability{req.GetVolumeCapability()}); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	unlock, err := s.lockVolume(req.GetVolumeId())
	if err != nil {
		return nil, err
	}
	defer unlock()

	if req.GetVolumeCapability().GetAccessMode().GetMode() == csi.VolumeCapability_AccessMode_SINGLE_NODE_SINGLE_WRITER {
		s.mu.Lock()
		for tp := range s.publishedTargets[req.GetVolumeId()] {
			if tp != req.GetTargetPath() {
				s.mu.Unlock()
				return nil, status.Errorf(codes.FailedPrecondition, "volume %s already published at %s", req.GetVolumeId(), tp)
			}
		}
		s.mu.Unlock()
	}

	var meta stageMetadata
	if raw, err := os.ReadFile(filepath.Join(req.GetStagingTargetPath(), stageMetaFilename)); err == nil {
		_ = json.Unmarshal(raw, &meta)
	}

	var opts []string
	if req.GetReadonly() {
		opts = append(opts, "ro")
	}
	if mnt := req.GetVolumeCapability().GetMount(); mnt != nil {
		opts = append(opts, mnt.GetMountFlags()...)
	}

	isBlock := req.GetVolumeCapability().GetBlock() != nil
	source := filepath.Join(req.GetStagingTargetPath(), "mount")
	if isBlock {
		source = meta.DevicePath
		if source == "" {
			return nil, status.Error(codes.FailedPrecondition, "staged block device path not found")
		}
	}

	if err := s.mounter.BindMount(ctx, source, req.GetTargetPath(), opts, isBlock); err != nil {
		return nil, status.Errorf(codes.Internal, "bind mount: %v", err)
	}
	s.mu.Lock()
	if s.publishedTargets[req.GetVolumeId()] == nil {
		s.publishedTargets[req.GetVolumeId()] = map[string]bool{}
	}
	s.publishedTargets[req.GetVolumeId()][req.GetTargetPath()] = true
	s.mu.Unlock()
	return &csi.NodePublishVolumeResponse{}, nil
}

func (s *NodeServer) NodeUnpublishVolume(ctx context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	if req.GetVolumeId() == "" || req.GetTargetPath() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume_id and target_path are required")
	}

	unlock, err := s.lockVolume(req.GetVolumeId())
	if err != nil {
		return nil, err
	}
	defer unlock()

	if err := s.mounter.Unmount(ctx, req.GetTargetPath()); err != nil {
		return nil, status.Errorf(codes.Internal, "unmount target: %v", err)
	}
	_ = os.RemoveAll(req.GetTargetPath())
	s.mu.Lock()
	if targets := s.publishedTargets[req.GetVolumeId()]; targets != nil {
		delete(targets, req.GetTargetPath())
	}
	s.mu.Unlock()
	return &csi.NodeUnpublishVolumeResponse{}, nil
}

func (s *NodeServer) NodeGetVolumeStats(_ context.Context, req *csi.NodeGetVolumeStatsRequest) (*csi.NodeGetVolumeStatsResponse, error) {
	if req.GetVolumeId() == "" || req.GetVolumePath() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume_id and volume_path are required")
	}
	if _, err := os.Stat(req.GetVolumePath()); os.IsNotExist(err) {
		return nil, status.Errorf(codes.NotFound, "volume_path %q does not exist", req.GetVolumePath())
	}

	stats, err := s.mounter.GetVolumeStats(req.GetVolumePath())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "get volume stats: %v", err)
	}

	usage := []*csi.VolumeUsage{
		{
			Unit:      csi.VolumeUsage_BYTES,
			Available: stats.AvailableBytes,
			Total:     stats.TotalBytes,
			Used:      stats.UsedBytes,
		},
	}
	if !stats.IsBlock && stats.TotalInodes > 0 {
		usage = append(usage, &csi.VolumeUsage{
			Unit:      csi.VolumeUsage_INODES,
			Available: stats.AvailableInodes,
			Total:     stats.TotalInodes,
			Used:      stats.UsedInodes,
		})
	}
	return &csi.NodeGetVolumeStatsResponse{Usage: usage}, nil
}

func (s *NodeServer) NodeExpandVolume(ctx context.Context, req *csi.NodeExpandVolumeRequest) (*csi.NodeExpandVolumeResponse, error) {
	if req.GetVolumeId() == "" || req.GetVolumePath() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume_id and volume_path are required")
	}
	if _, err := os.Stat(req.GetVolumePath()); os.IsNotExist(err) {
		return nil, status.Errorf(codes.NotFound, "volume_path %q does not exist", req.GetVolumePath())
	}

	var meta stageMetadata
	stagingPath := req.GetStagingTargetPath() // optional in the CSI spec
	if stagingPath == "" {
		s.mu.Lock()
		stagingPath = s.stagingPaths[req.GetVolumeId()]
		s.mu.Unlock()
	}
	if stagingPath != "" {
		if raw, err := os.ReadFile(filepath.Join(stagingPath, stageMetaFilename)); err == nil {
			_ = json.Unmarshal(raw, &meta)
		}
	}
	if meta.DevicePath == "" {
		return nil, status.Error(codes.FailedPrecondition, "staged device for volume is unknown; staging_target_path is required")
	}
	// The host must observe the new LUN/namespace size before the filesystem can grow.
	if err := s.mounter.RescanTarget(ctx, meta.PublishContext); err != nil {
		return nil, status.Errorf(codes.Internal, "rescan target: %v", err)
	}
	if meta.IsBlock {
		return &csi.NodeExpandVolumeResponse{CapacityBytes: req.GetCapacityRange().GetRequiredBytes()}, nil
	}
	if err := s.mounter.ResizeFilesystem(ctx, meta.DevicePath, req.GetVolumePath()); err != nil {
		return nil, status.Errorf(codes.Internal, "resize filesystem: %v", err)
	}

	return &csi.NodeExpandVolumeResponse{
		CapacityBytes: req.GetCapacityRange().GetRequiredBytes(),
	}, nil
}
