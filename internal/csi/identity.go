package csi

import (
	"context"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/velocor/velocor/internal/version"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// IdentityServer implements the CSI Identity service.
type IdentityServer struct {
	csi.UnimplementedIdentityServer
	driverName string
	client     VelocorClient
	// snapshotMetadata advertises SNAPSHOT_METADATA_SERVICE (served by this process).
	snapshotMetadata bool
}

// NewIdentityServer creates a CSI IdentityServer.
func NewIdentityServer(driverName string, client VelocorClient) *IdentityServer {
	return &IdentityServer{
		driverName: driverName,
		client:     client,
	}
}

func (s *IdentityServer) GetPluginInfo(_ context.Context, _ *csi.GetPluginInfoRequest) (*csi.GetPluginInfoResponse, error) {
	if s.driverName == "" {
		return nil, status.Error(codes.Unavailable, "driver name not configured")
	}
	v := version.Version
	if v == "" {
		v = "v0.2.0"
	}
	return &csi.GetPluginInfoResponse{
		Name:          s.driverName,
		VendorVersion: v,
	}, nil
}

func (s *IdentityServer) GetPluginCapabilities(_ context.Context, _ *csi.GetPluginCapabilitiesRequest) (*csi.GetPluginCapabilitiesResponse, error) {
	resp := &csi.GetPluginCapabilitiesResponse{
		Capabilities: []*csi.PluginCapability{
			{
				Type: &csi.PluginCapability_Service_{
					Service: &csi.PluginCapability_Service{
						Type: csi.PluginCapability_Service_CONTROLLER_SERVICE,
					},
				},
			},
			{
				Type: &csi.PluginCapability_Service_{
					Service: &csi.PluginCapability_Service{
						Type: csi.PluginCapability_Service_GROUP_CONTROLLER_SERVICE,
					},
				},
			},
			{
				Type: &csi.PluginCapability_VolumeExpansion_{
					VolumeExpansion: &csi.PluginCapability_VolumeExpansion{
						Type: csi.PluginCapability_VolumeExpansion_ONLINE,
					},
				},
			},
			{
				Type: &csi.PluginCapability_VolumeExpansion_{
					VolumeExpansion: &csi.PluginCapability_VolumeExpansion{
						Type: csi.PluginCapability_VolumeExpansion_OFFLINE,
					},
				},
			},
		},
	}
	if s.snapshotMetadata {
		resp.Capabilities = append(resp.Capabilities, &csi.PluginCapability{
			Type: &csi.PluginCapability_Service_{
				Service: &csi.PluginCapability_Service{Type: csi.PluginCapability_Service_SNAPSHOT_METADATA_SERVICE},
			},
		})
	}
	return resp, nil
}

func (s *IdentityServer) Probe(ctx context.Context, _ *csi.ProbeRequest) (*csi.ProbeResponse, error) {
	if s.client != nil {
		if err := s.client.Ping(ctx); err != nil {
			return &csi.ProbeResponse{Ready: wrapperspb.Bool(false)}, nil
		}
	}
	return &csi.ProbeResponse{Ready: wrapperspb.Bool(true)}, nil
}
