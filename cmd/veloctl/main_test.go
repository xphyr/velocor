package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/velocor/velocor/internal/auth"
	"github.com/velocor/velocor/internal/dataplane"
	"github.com/velocor/velocor/internal/server"
	"github.com/velocor/velocor/internal/spdk/spdktest"
	"github.com/velocor/velocor/internal/store"
)

type fixture struct {
	cfg  *cliConfig
	st   *store.BoltStore
	fake *spdktest.Fake
	exp  *store.Export
	ig   *store.InitiatorGroup
	vol  *store.Volume
}

// newFixture serves a real velod API (backed by a fake spdk_tgt) with one exported volume.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	t.Setenv("HOME", t.TempDir()) // keep saved tokens out of the real home directory
	st, err := store.OpenBoltStore(filepath.Join(t.TempDir(), "v.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	fake := spdktest.New(t)
	authMgr, err := auth.NewManager(st, "bootstrap-pass-1")
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(server.New(st, fake.Client(), nil, authMgr, nil, nil).Handler())
	t.Cleanup(ts.Close)

	ctx, c := context.Background(), fake.Client()
	fake.AddNVMe("0000:01:00.0")
	bdevs, _ := c.BdevNvmeAttachController(ctx, "Nvme0", "PCIe", "0000:01:00.0")
	_ = st.UpsertDisk(&store.Disk{ID: "d1", Type: store.DiskTypeNVMe, PCIAddress: "0000:01:00.0", SizeBytes: 1 << 40})
	_, _ = st.ClaimDisk("d1", store.DiskDriverStateVfioPCI, bdevs[0])
	pool := &store.Pool{ID: "p1", Name: "p1", DiskIDs: []string{"d1"}, RaidLevel: store.RaidLevel0, HealthStatus: store.HealthStatusOnline}
	_ = st.CreatePool(pool)
	_ = c.BdevRaidCreate(ctx, pool.BaseBdevName, "raid0", 64, bdevs, true)
	lvs, _ := c.BdevLvolCreateLvstore(ctx, pool.EffectiveBdevName, pool.LVSName, 0, "")
	_, _ = st.MutatePool("p1", func(p *store.Pool) error { p.LVSUUID = lvs; return nil })
	uuid, _ := c.BdevLvolCreate(ctx, pool.LVSName, "v1", 1<<30, true, "")
	vol := &store.Volume{ID: "v1", Name: "v1", PoolID: "p1", UUID: uuid, LvolBdevName: pool.LVSName + "/v1", SizeBytes: 1 << 30}
	_ = st.CreateVolume(vol)
	_ = st.CreatePortal(&store.Portal{ID: "pt", Name: "pt", Protocol: store.ExportProtocolNVMeoFTCP, IPAddress: "10.0.0.1", Port: 4420})
	ig := &store.InitiatorGroup{ID: "ig1", Name: "g", HostNQNs: []string{"nqn.2026-01.io.example:host-a"}}
	_ = st.CreateInitiatorGroup(ig)
	exp := &store.Export{ID: "e1", Name: "e1", VolumeID: "v1", Protocol: store.ExportProtocolNVMeoFTCP, SubsystemNQN: "nqn.test:e1",
		NamespaceID: 1, PortalIDs: []string{"pt"}, InitiatorGroupIDs: []string{"ig1"}}
	_ = st.CreateExport(exp)
	if err := dataplane.EnsureExport(ctx, c, st, exp, vol); err != nil {
		t.Fatal(err)
	}

	cfg := &cliConfig{endpoint: ts.URL, httpClient: &http.Client{Timeout: 30 * time.Second}}
	return &fixture{cfg: cfg, st: st, fake: fake, exp: exp, ig: ig, vol: vol}
}

func (f *fixture) run(t *testing.T, args ...string) error {
	t.Helper()
	return dispatch(f.cfg, args)
}

func TestLoginForcedPasswordChangeAndPasswd(t *testing.T) {
	f := newFixture(t)
	if err := f.run(t, "login", "--username", "admin", "--password", "bootstrap-pass-1"); err != nil {
		t.Fatal(err)
	}
	f.cfg.token = loadSavedToken()
	err := f.run(t, "volume", "list")
	if err == nil || !strings.Contains(err.Error(), "veloctl passwd") {
		t.Fatalf("API use before the password change must point at veloctl passwd, got %v", err)
	}
	if err := f.run(t, "passwd", "--current", "bootstrap-pass-1", "--new", "a-real-password"); err != nil {
		t.Fatal(err)
	}
	// passwd saved the fresh session, since the old one was revoked.
	f.cfg.token = loadSavedToken()
	if err := f.run(t, "volume", "list"); err != nil {
		t.Fatalf("after passwd the saved session must work: %v", err)
	}
}

func signedIn(t *testing.T, f *fixture) {
	t.Helper()
	u, _ := f.st.GetAdminUser("admin")
	u.MustChangePassword = false
	_ = f.st.SaveAdminUser(u)
	if err := f.run(t, "login", "--password", "bootstrap-pass-1"); err != nil {
		t.Fatal(err)
	}
	f.cfg.token = loadSavedToken()
}

func TestExportAccessAndConnections(t *testing.T) {
	f := newFixture(t)
	signedIn(t, f)

	if err := f.run(t, "export", "access", "e1"); err == nil {
		t.Fatalf("access with nothing to change must be refused")
	}
	if err := f.run(t, "export", "access", "e1", "--initiator-groups", ""); err == nil {
		t.Fatalf("emptying the ACL without allow-any-host must be refused by the API")
	}
	if err := f.run(t, "export", "access", "e1", "--allow-any-host", "--initiator-groups", ""); err != nil {
		t.Fatal(err)
	}
	if sub, _ := f.fake.Subsystem("nqn.test:e1"); !sub.AllowAnyHost {
		t.Fatalf("allow-any-host not applied: %+v", sub)
	}
	if err := f.run(t, "export", "access", "--allow-any-host=false", "--initiator-groups", "ig1", "e1"); err != nil {
		t.Fatal(err)
	}
	if sub, _ := f.fake.Subsystem("nqn.test:e1"); sub.AllowAnyHost || len(sub.Hosts) != 1 {
		t.Fatalf("ACL not restored: %+v", sub)
	}
	f.fake.ConnectHost("nqn.test:e1", "nqn.2026-01.io.example:host-a")
	for _, args := range [][]string{{"export", "connections", "e1"}, {"export", "get", "e1"}, {"volume", "usage", "v1"}, {"volume", "get", "v1"}, {"system", "status"}} {
		if err := f.run(t, args...); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
}

func TestInitiatorGroupUpdateSendsOnlyGivenLists(t *testing.T) {
	f := newFixture(t)
	signedIn(t, f)
	if _, err := f.st.MutateInitiatorGroup("ig1", func(g *store.InitiatorGroup) error { g.InitiatorIQNs = []string{"iqn.keep"}; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := f.run(t, "initiator-group", "update", "ig1", "--nqns", "nqn.2026-01.io.example:host-b, nqn.2026-01.io.example:host-c"); err != nil {
		t.Fatal(err)
	}
	g, _ := f.st.GetInitiatorGroup("ig1")
	if len(g.HostNQNs) != 2 || len(g.InitiatorIQNs) != 1 || g.InitiatorIQNs[0] != "iqn.keep" {
		t.Fatalf("update changed lists it wasn't given: %+v", g)
	}
	if sub, _ := f.fake.Subsystem("nqn.test:e1"); len(sub.Hosts) != 2 {
		t.Fatalf("group change not re-applied to the export: %+v", sub.Hosts)
	}
	if err := f.run(t, "initiator-group", "update", "ig1"); err == nil {
		t.Fatalf("update with no lists must be refused")
	}
}

func TestTLSConfigVerifiesByDefault(t *testing.T) {
	cfg, err := tlsConfig(false, "")
	if err != nil || cfg.InsecureSkipVerify {
		t.Fatalf("verification must be on by default: %+v %v", cfg, err)
	}
	if _, err := tlsConfig(false, filepath.Join(t.TempDir(), "missing.pem")); err == nil {
		t.Fatalf("a missing CA file must be an error")
	}
	bad := filepath.Join(t.TempDir(), "bad.pem")
	_ = os.WriteFile(bad, []byte("not a cert"), 0o600)
	if _, err := tlsConfig(false, bad); err == nil {
		t.Fatalf("a non-PEM CA file must be an error")
	}
}
