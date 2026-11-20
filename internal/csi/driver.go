package csi

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"
)

const DefaultDriverName = "csi.velocor.io"

// DriverMode controls which CSI gRPC services are registered.
type DriverMode string

const (
	ModeAll        DriverMode = "all"
	ModeController DriverMode = "controller"
	ModeNode       DriverMode = "node"
)

// DriverConfig configures the Velocor CSI gRPC server.
type DriverConfig struct {
	DriverName string
	Endpoint   string
	NodeID     string
	Mode       DriverMode
	Client     VelocorClient
	Mounter    Mounter
	// DefaultPortals applies to StorageClasses without a "portals" parameter.
	DefaultPortals string
	// SnapshotMetadata serves and advertises the CSI SnapshotMetadata service (changed-block
	// tracking, KEP-3314) in the controller. It needs the external-snapshot-metadata sidecar.
	SnapshotMetadata bool
}

// Driver hosts the CSI Identity, Controller, GroupController and Node gRPC services, and the
// SnapshotMetadata service (changed-block tracking) when configured to.
type Driver struct {
	cfg    DriverConfig
	server *grpc.Server
	lis    net.Listener
}

// NewDriver initializes the CSI gRPC server and registers services per Mode.
func NewDriver(cfg DriverConfig) (*Driver, error) {
	if cfg.DriverName == "" {
		cfg.DriverName = DefaultDriverName
	}
	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("endpoint is required")
	}
	if cfg.Mode == "" {
		cfg.Mode = ModeAll
	}
	if cfg.Mounter == nil {
		cfg.Mounter = NewLinuxMounter()
	}

	srv := grpc.NewServer(grpc.UnaryInterceptor(logCalls))
	metadata := cfg.SnapshotMetadata && cfg.Mode != ModeNode
	ids := NewIdentityServer(cfg.DriverName, cfg.Client)
	ids.snapshotMetadata = metadata
	csi.RegisterIdentityServer(srv, ids)
	if metadata {
		csi.RegisterSnapshotMetadataServer(srv, NewSnapshotMetadataServer(cfg.Client))
	}

	switch cfg.Mode {
	case ModeAll:
		cs := NewControllerServer(cfg.Client, cfg.NodeID)
		cs.SetDefaultPortals(cfg.DefaultPortals)
		csi.RegisterControllerServer(srv, cs)
		csi.RegisterGroupControllerServer(srv, NewGroupControllerServer(cfg.Client))
		csi.RegisterNodeServer(srv, NewNodeServer(cfg.NodeID, cfg.Mounter))
	case ModeController:
		cs := NewControllerServer(cfg.Client)
		cs.SetDefaultPortals(cfg.DefaultPortals)
		csi.RegisterControllerServer(srv, cs)
		csi.RegisterGroupControllerServer(srv, NewGroupControllerServer(cfg.Client))
	case ModeNode:
		csi.RegisterNodeServer(srv, NewNodeServer(cfg.NodeID, cfg.Mounter))
	default:
		return nil, fmt.Errorf("unsupported driver mode %q", cfg.Mode)
	}

	return &Driver{
		cfg:    cfg,
		server: srv,
	}, nil
}

// Run starts listening on cfg.Endpoint and serves gRPC requests until Stop is called.
func (d *Driver) Run() error {
	scheme, addr, err := parseEndpoint(d.cfg.Endpoint)
	if err != nil {
		return err
	}
	if scheme == "unix" {
		if err := os.MkdirAll(filepath.Dir(addr), 0750); err != nil {
			return err
		}
		_ = os.Remove(addr)
	}
	lis, err := net.Listen(scheme, addr)
	if err != nil {
		return fmt.Errorf("listen %s %s: %w", scheme, addr, err)
	}
	d.lis = lis
	return d.server.Serve(lis)
}

// Stop gracefully stops the gRPC server.
func (d *Driver) Stop() {
	if d.server != nil {
		d.server.GracefulStop()
	}
}

func parseEndpoint(ep string) (string, string, error) {
	if strings.HasPrefix(ep, "unix://") || strings.HasPrefix(ep, "tcp://") {
		u, err := url.Parse(ep)
		if err != nil {
			return "", "", err
		}
		if u.Scheme == "unix" {
			return "unix", u.Host + u.Path, nil
		}
		return "tcp", u.Host, nil
	}
	return "unix", ep, nil
}
