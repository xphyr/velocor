package csi

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/velocor/velocor/internal/auth"
	"github.com/velocor/velocor/internal/dataplane/spdkplane"
	"github.com/velocor/velocor/internal/server"
	"github.com/velocor/velocor/internal/store"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestResolvePortals(t *testing.T) {
	nvA := &store.Portal{ID: "portal-a", Name: "san-a", Protocol: store.ExportProtocolNVMeoFTCP, IPAddress: "10.0.1.1", Port: 4420}
	nvB := &store.Portal{ID: "portal-b", Name: "san-b", Protocol: store.ExportProtocolNVMeoFTCP, IPAddress: "10.0.2.1", Port: 4420}
	mgmt := &store.Portal{ID: "portal-m", Name: "mgmt", Protocol: store.ExportProtocolNVMeoFTCP, IPAddress: "192.0.2.1", Port: 4420}
	is := &store.Portal{ID: "portal-i", Name: "san-iscsi", Protocol: store.ExportProtocolISCSI, IPAddress: "10.0.1.1", Port: 3260}
	wild := &store.Portal{ID: "portal-w", Name: "wild", Protocol: store.ExportProtocolISCSI, IPAddress: "0.0.0.0", Port: 3260}
	all := []*store.Portal{mgmt, nvA, nvB, is}
	nvme, iscsi := store.ExportProtocolNVMeoFTCP, store.ExportProtocolISCSI

	ids := func(ps []*store.Portal) []string {
		var out []string
		for _, p := range ps {
			out = append(out, p.ID)
		}
		return out
	}
	for _, tc := range []struct {
		name, spec, def string
		proto           store.ExportProtocol
		portals         []*store.Portal
		want            []string
		errHas          string
	}{
		{name: "only portal of the protocol", proto: iscsi, portals: all, want: []string{"portal-i"}},
		{name: "several and no choice", proto: nvme, portals: all, errHas: `set the StorageClass parameter "portals"`},
		{name: "none", proto: iscsi, portals: []*store.Portal{nvA}, errHas: "no ISCSI portal"},
		{name: "names and IDs in order", spec: "san-b, portal-a", proto: nvme, portals: all, want: []string{"portal-b", "portal-a"}},
		{name: "duplicates once", spec: "san-a,portal-a", proto: nvme, portals: all, want: []string{"portal-a"}},
		{name: "unknown", spec: "san-c", proto: nvme, portals: all, errHas: `portal "san-c"`},
		{name: "wrong protocol in class", spec: "san-iscsi", proto: nvme, portals: all, errHas: "is ISCSI, but the StorageClass uses NVMEOF_TCP"},
		{name: "default covers both protocols", def: "san-a,san-iscsi", proto: iscsi, portals: all, want: []string{"portal-i"}},
		{name: "default for the other protocol only", def: "san-a", proto: iscsi, portals: all, want: []string{"portal-i"}},
		{name: "class beats default", spec: "san-b", def: "san-a", proto: nvme, portals: all, want: []string{"portal-b"}},
		{name: "default names a missing portal", def: "gone", proto: nvme, portals: all, errHas: `portal "gone"`},
		{name: "wildcard refused", proto: iscsi, portals: []*store.Portal{wild}, errHas: "listens on every address"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &ControllerServer{defaultPortals: tc.def}
			got, err := s.resolvePortals(tc.spec, tc.proto, tc.portals)
			if tc.errHas != "" {
				if err == nil || !strings.Contains(err.Error(), tc.errHas) {
					t.Fatalf("got %v, %v; want an error containing %q", ids(got), err, tc.errHas)
				}
				return
			}
			if err != nil || !slices.Equal(ids(got), tc.want) {
				t.Fatalf("got %v, %v; want %v", ids(got), err, tc.want)
			}
		})
	}

	t.Run("wildcard allowed with the override", func(t *testing.T) {
		t.Setenv("VELOCOR_PORTAL_OVERRIDE", "10.0.1.1:3260")
		if _, err := (&ControllerServer{}).resolvePortals("", iscsi, []*store.Portal{wild}); err != nil {
			t.Fatal(err)
		}
	})
}

func TestTargetAddrs(t *testing.T) {
	got, err := targetAddrs(map[string]string{"portals": "10.0.1.1:4420,[fd00::1]:4420", "portal_ip": "10.0.1.1", "portal_port": "4420"})
	if err != nil || !slices.Equal(got, []targetAddr{{"10.0.1.1", "4420"}, {"fd00::1", "4420"}}) {
		t.Fatalf("portals list: %v, %v", got, err)
	}
	got, err = targetAddrs(map[string]string{"portal_ip": "10.0.1.1", "portal_port": "3260"})
	if err != nil || !slices.Equal(got, []targetAddr{{"10.0.1.1", "3260"}}) {
		t.Fatalf("from an older controller: %v, %v", got, err)
	}
	if _, err := targetAddrs(map[string]string{}); err == nil {
		t.Fatal("no address must be an error")
	}
	t.Setenv("VELOCOR_PORTAL_OVERRIDE", "192.0.2.9:4420")
	got, _ = targetAddrs(map[string]string{"portals": "10.0.1.1:4420,10.0.2.1:4420"})
	if !slices.Equal(got, []targetAddr{{"192.0.2.9", "4420"}}) {
		t.Fatalf("override: %v", got)
	}
}

// With a management and two SAN portals, a class must say which to use; its volumes are then
// exported on exactly those, and nodes are told every address.
func TestPublishUsesTheClassPortals(t *testing.T) {
	st, err := store.OpenBoltStore(filepath.Join(t.TempDir(), "v.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	fake := seedBackend(t, st, "pool-1")
	for _, p := range []*store.Portal{
		{ID: "portal-m", Name: "mgmt", Protocol: store.ExportProtocolNVMeoFTCP, IPAddress: "192.0.2.1", Port: 4420},
		{ID: "portal-a", Name: "san-a", Protocol: store.ExportProtocolNVMeoFTCP, IPAddress: "10.0.1.1", Port: 4420},
		{ID: "portal-b", Name: "san-b", Protocol: store.ExportProtocolNVMeoFTCP, IPAddress: "10.0.2.1", Port: 4420},
	} {
		if err := st.CreatePortal(p); err != nil {
			t.Fatal(err)
		}
	}
	authMgr, _ := auth.NewManager(st, "pass-123456")
	_, tok, _ := authMgr.CreateApiToken("csi", store.RoleCSIProvisioner, time.Hour)
	ts := httptest.NewServer(server.New(st, spdkplane.New(fake.Client(), nil), nil, authMgr, nil, nil).Handler())
	t.Cleanup(ts.Close)
	ctrl := NewControllerServer(NewHTTPClient(ts.URL, tok, true))
	ctx := context.Background()
	capab := blockCap(csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER)
	create := func(name string, params map[string]string) (*csi.CreateVolumeResponse, error) {
		return ctrl.CreateVolume(ctx, &csi.CreateVolumeRequest{Name: name, Parameters: params, VolumeCapabilities: []*csi.VolumeCapability{capab}})
	}

	if _, err := create("pvc-guess", map[string]string{"protocol": "NVMEOF_TCP"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("three NVMe/TCP portals and no choice: %v, want InvalidArgument", err)
	}
	if _, err := create("pvc-typo", map[string]string{"portals": "san-c"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("unknown portal: %v, want InvalidArgument", err)
	}
	vol, err := create("pvc-1", map[string]string{"protocol": "NVMEOF_TCP", "portals": "san-a,san-b"})
	if err != nil {
		t.Fatal(err)
	}
	vc := vol.GetVolume().GetVolumeContext()
	if vc["portals"] != "san-a,san-b" {
		t.Fatalf("volume context %v must carry the class's portals", vc)
	}
	pub, err := ctrl.ControllerPublishVolume(ctx, &csi.ControllerPublishVolumeRequest{
		VolumeId: vol.GetVolume().GetVolumeId(), NodeId: "node-a", VolumeCapability: capab, VolumeContext: vc})
	if err != nil {
		t.Fatal(err)
	}
	pc := pub.GetPublishContext()
	if pc["portals"] != "10.0.1.1:4420,10.0.2.1:4420" || pc["portal_ip"] != "10.0.1.1" {
		t.Fatalf("publish context %v", pc)
	}
	exps, _ := st.ListExports(vol.GetVolume().GetVolumeId(), "")
	if len(exps) != 1 || !slices.Equal(exps[0].PortalIDs, []string{"portal-a", "portal-b"}) {
		t.Fatalf("export portals: %+v", exps)
	}
	// A repeated publish reports the same addresses.
	again, err := ctrl.ControllerPublishVolume(ctx, &csi.ControllerPublishVolumeRequest{
		VolumeId: vol.GetVolume().GetVolumeId(), NodeId: "node-a", VolumeCapability: capab, VolumeContext: vc})
	if err != nil || again.GetPublishContext()["portals"] != pc["portals"] {
		t.Fatalf("republish: %v, %v", again.GetPublishContext(), err)
	}

	// Volumes from before the parameter (no "portals" in their context) use the driver default.
	old, err := ctrl.client.CreateVolume(ctx, CreateVolumeRequest{Name: "old", PoolID: "pool-1", SizeBytes: 1 << 30, ThinProvision: true})
	if err != nil {
		t.Fatal(err)
	}
	oldVC := map[string]string{"protocol": "NVMEOF_TCP", "pool_id": "pool-1"}
	if _, err := ctrl.ControllerPublishVolume(ctx, &csi.ControllerPublishVolumeRequest{
		VolumeId: old.ID, NodeId: "node-a", VolumeCapability: capab, VolumeContext: oldVC}); status.Code(err) != codes.FailedPrecondition ||
		!strings.Contains(err.Error(), "defaultPortals") {
		t.Fatalf("old volume, no default: %v, want FailedPrecondition naming defaultPortals", err)
	}
	ctrl.SetDefaultPortals("san-b")
	pub, err = ctrl.ControllerPublishVolume(ctx, &csi.ControllerPublishVolumeRequest{
		VolumeId: old.ID, NodeId: "node-a", VolumeCapability: capab, VolumeContext: oldVC})
	if err != nil || pub.GetPublishContext()["portals"] != "10.0.2.1:4420" {
		t.Fatalf("old volume with the default: %v, %v", pub.GetPublishContext(), err)
	}
}
