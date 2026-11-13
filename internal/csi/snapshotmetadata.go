package csi

import (
	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const defaultMetadataChunkSize = 256

// SnapshotMetadataServer implements the CSI SnapshotMetadata service (CBT / KEP-3314).
type SnapshotMetadataServer struct {
	csi.UnimplementedSnapshotMetadataServer
	client VelocorClient
}

// NewSnapshotMetadataServer creates a CSI SnapshotMetadataServer.
func NewSnapshotMetadataServer(client VelocorClient) *SnapshotMetadataServer {
	return &SnapshotMetadataServer{client: client}
}

func (s *SnapshotMetadataServer) GetMetadataAllocated(req *csi.GetMetadataAllocatedRequest, stream csi.SnapshotMetadata_GetMetadataAllocatedServer) error {
	if req.GetSnapshotId() == "" {
		return status.Error(codes.InvalidArgument, "snapshot_id is required")
	}
	if req.GetStartingOffset() < 0 {
		return status.Error(codes.InvalidArgument, "starting_offset must be >= 0")
	}

	res, err := s.client.GetSnapshotAllocatedBlocks(stream.Context(), req.GetSnapshotId())
	if err != nil {
		return mapAPIError(err)
	}

	tuples := filterExtents(res.Extents, req.GetStartingOffset())
	chunkSize := int(req.GetMaxResults())
	if chunkSize <= 0 {
		chunkSize = defaultMetadataChunkSize
	}

	if len(tuples) == 0 {
		return stream.Send(&csi.GetMetadataAllocatedResponse{
			BlockMetadataType:   csi.BlockMetadataType_VARIABLE_LENGTH,
			VolumeCapacityBytes: int64(res.VolumeSizeBytes),
		})
	}

	for i := 0; i < len(tuples); i += chunkSize {
		end := i + chunkSize
		if end > len(tuples) {
			end = len(tuples)
		}
		if err := stream.Send(&csi.GetMetadataAllocatedResponse{
			BlockMetadataType:   csi.BlockMetadataType_VARIABLE_LENGTH,
			VolumeCapacityBytes: int64(res.VolumeSizeBytes),
			BlockMetadata:       tuples[i:end],
		}); err != nil {
			return err
		}
	}
	return nil
}

func (s *SnapshotMetadataServer) GetMetadataDelta(req *csi.GetMetadataDeltaRequest, stream csi.SnapshotMetadata_GetMetadataDeltaServer) error {
	if req.GetBaseSnapshotId() == "" || req.GetTargetSnapshotId() == "" {
		return status.Error(codes.InvalidArgument, "base_snapshot_id and target_snapshot_id are required")
	}
	if req.GetStartingOffset() < 0 {
		return status.Error(codes.InvalidArgument, "starting_offset must be >= 0")
	}

	res, err := s.client.GetSnapshotChangedBlocks(stream.Context(), req.GetTargetSnapshotId(), req.GetBaseSnapshotId())
	if err != nil {
		return mapAPIError(err)
	}

	tuples := filterExtents(res.Extents, req.GetStartingOffset())
	chunkSize := int(req.GetMaxResults())
	if chunkSize <= 0 {
		chunkSize = defaultMetadataChunkSize
	}

	if len(tuples) == 0 {
		return stream.Send(&csi.GetMetadataDeltaResponse{
			BlockMetadataType:   csi.BlockMetadataType_VARIABLE_LENGTH,
			VolumeCapacityBytes: int64(res.VolumeSizeBytes),
		})
	}

	for i := 0; i < len(tuples); i += chunkSize {
		end := i + chunkSize
		if end > len(tuples) {
			end = len(tuples)
		}
		if err := stream.Send(&csi.GetMetadataDeltaResponse{
			BlockMetadataType:   csi.BlockMetadataType_VARIABLE_LENGTH,
			VolumeCapacityBytes: int64(res.VolumeSizeBytes),
			BlockMetadata:       tuples[i:end],
		}); err != nil {
			return err
		}
	}
	return nil
}

func filterExtents(extents []BlockExtent, startingOffset int64) []*csi.BlockMetadata {
	var out []*csi.BlockMetadata
	for _, e := range extents {
		end := int64(e.OffsetBytes + e.LengthBytes)
		if end <= startingOffset {
			continue
		}
		off := int64(e.OffsetBytes)
		size := int64(e.LengthBytes)
		if off < startingOffset {
			delta := startingOffset - off
			off = startingOffset
			size -= delta
		}
		if size > 0 {
			out = append(out, &csi.BlockMetadata{
				ByteOffset: off,
				SizeBytes:  size,
			})
		}
	}
	return out
}
