package meta

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/velocor/velocor/internal/store"
)

func manifest(gen uint64, name string) *Manifest {
	return &Manifest{Format: FormatVersion, Generation: gen, Pool: store.Pool{ID: "pool-1", Name: name},
		Volumes: []store.Volume{{ID: "vol-1", Name: "db", PoolID: "pool-1", SizeBytes: 1 << 30}}}
}

func TestSlotRoundTripAndNewest(t *testing.T) {
	a, err := EncodeSlot(manifest(4, "p"))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := EncodeSlot(manifest(5, "p-renamed"))
	if len(a)%HeaderBytes != 0 {
		t.Fatalf("slot image %d bytes is not block aligned", len(a))
	}
	m, err := Newest(a, b)
	if err != nil || m.Generation != 5 || m.Pool.Name != "p-renamed" || m.Volumes[0].Name != "db" {
		t.Fatalf("newest: %+v %v", m, err)
	}
	if SlotFor(4) == SlotFor(5) {
		t.Fatal("consecutive generations must go to different slots")
	}
}

func TestTornOrCorruptSlotFallsBack(t *testing.T) {
	old, _ := EncodeSlot(manifest(7, "old"))
	cur, _ := EncodeSlot(manifest(8, "new"))
	torn := append([]byte(nil), cur...)
	copy(torn[HeaderBytes+10:], []byte("garbage"))
	if m, err := Newest(old, torn); err != nil || m.Generation != 7 {
		t.Fatalf("a torn newer slot must fall back to the older one: %+v %v", m, err)
	}
	if _, err := Newest(make([]byte, SlotBytes), torn); !errors.Is(err, ErrNoManifest) {
		t.Fatalf("no valid slot: %v", err)
	}
	if _, ok := DecodeSlot(nil); ok {
		t.Fatal("empty slot decoded")
	}
}

func TestManifestTooLarge(t *testing.T) {
	m := manifest(1, "p")
	m.Volumes[0].Labels = map[string]string{"big": strings.Repeat("x", SlotBytes)}
	if _, err := EncodeSlot(m); err == nil {
		t.Fatal("a manifest larger than a slot must be refused")
	}
}

func TestDigestIgnoresLiveFigures(t *testing.T) {
	a := manifest(1, "p")
	b := manifest(9, "p")
	b.Pool.FreeBytes, b.Pool.SpaceState, b.Pool.MetaGeneration = 123, store.SpaceStateWarning, 9
	b.Volumes[0].AllocatedBytes = 1 << 20
	if a.Digest() != b.Digest() {
		t.Fatal("generation and live figures must not change the digest")
	}
	b.Volumes[0].Name = "db2"
	if a.Digest() == b.Digest() {
		t.Fatal("a renamed volume must change the digest")
	}
}

func TestBuildCollectsOnlyThePool(t *testing.T) {
	st, err := store.OpenBoltStore(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"d1", "d2"} {
		must(st.UpsertDisk(&store.Disk{ID: id, Type: store.DiskTypeSSD, SerialNumber: "SN-" + id}))
		_, err := st.ClaimDisk(id, store.DiskDriverStateUringClaimed, "uring_"+id)
		must(err)
	}
	must(st.CreatePool(&store.Pool{ID: "p1", Name: "p1", DiskIDs: []string{"d1"}}))
	must(st.CreatePool(&store.Pool{ID: "p2", Name: "p2", DiskIDs: []string{"d2"}}))
	must(st.CreateVolume(&store.Volume{ID: "v1", Name: "a", PoolID: "p1", SizeBytes: 1 << 30}))
	must(st.CreateVolume(&store.Volume{ID: "v2", Name: "b", PoolID: "p2", SizeBytes: 1 << 30}))
	must(st.CreateSnapshot(&store.Snapshot{ID: "s1", Name: "s", VolumeID: "v1", PoolID: "p1"}))
	must(st.CreateHost(&store.Host{ID: "h1", Name: "h1", HostNQN: "nqn.h1"}))
	must(st.CreateHost(&store.Host{ID: "h2", Name: "h2", HostNQN: "nqn.h2"}))
	must(st.CreateInitiatorGroup(&store.InitiatorGroup{ID: "g1", Name: "g1", HostIDs: []string{"h1"},
		CHAPUser: "u", CHAPSecretEncrypted: []byte("SECRET-1"), MutualCHAPSecretEncrypted: []byte("SECRET-2")}))
	must(st.CreatePortal(&store.Portal{ID: "pt1", Name: "pt1", Protocol: store.ExportProtocolNVMeoFTCP, IPAddress: "10.0.0.1", Port: 4420}))
	must(st.CreatePortal(&store.Portal{ID: "pt2", Name: "pt2", Protocol: store.ExportProtocolNVMeoFTCP, IPAddress: "10.0.0.2", Port: 4420}))
	must(st.CreateExport(&store.Export{ID: "e1", Name: "e1", VolumeID: "v1", Protocol: store.ExportProtocolNVMeoFTCP,
		SubsystemNQN: "nqn.e1", NamespaceID: 1, PortalIDs: []string{"pt1"}, InitiatorGroupIDs: []string{"g1"}}))

	m, err := Build(st, "p1", "node-1", "test")
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Volumes) != 1 || m.Volumes[0].ID != "v1" || len(m.Snapshots) != 1 || len(m.Disks) != 1 || m.Disks[0].SerialNumber != "SN-d1" {
		t.Fatalf("pool content: %+v", m)
	}
	if len(m.Exports) != 1 || len(m.InitiatorGroup) != 1 || len(m.Hosts) != 1 || m.Hosts[0].ID != "h1" ||
		len(m.Portals) != 1 || m.Portals[0].ID != "pt1" {
		t.Fatalf("access objects: exports %d groups %d hosts %+v portals %+v", len(m.Exports), len(m.InitiatorGroup), m.Hosts, m.Portals)
	}
	img, err := EncodeSlot(m)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"password", "token", "jwt", "secret_encrypted\":\"", "U0VDUkVU"} {
		if strings.Contains(strings.ToLower(string(img)), secret) {
			t.Errorf("manifest mentions %q: no account data may be on the pool", secret)
		}
	}
}
