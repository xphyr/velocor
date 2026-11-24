package reconciler

import (
	"errors"
	"strings"
	"testing"

	"github.com/velocor/velocor/internal/meta"
	"github.com/velocor/velocor/internal/store"
)

func TestMetaCopyWrittenOnChangeOnly(t *testing.T) {
	e := newEnv(t)
	dev := &meta.MemDevice{}
	e.rec.SetMetaDevice(dev, "node-1", "test")
	pool, vol := e.provision(store.RaidLevel1, "0000:01:00.0", "0000:02:00.0")

	e.reconcile()
	p := e.pool()
	if p.MetaVolumeUUID == "" || p.MetaGeneration != 1 || p.MetaHash == "" || p.MetaError != "" {
		t.Fatalf("after the first pass: lvol %q gen %d hash %q err %q", p.MetaVolumeUUID, p.MetaGeneration, p.MetaHash, p.MetaError)
	}
	read := func() *meta.Manifest {
		t.Helper()
		slots, _ := dev.ReadSlots(e.ctx, p.MetaVolumeUUID)
		m, err := meta.Newest(slots...)
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	if m := read(); m.Generation != 1 || len(m.Volumes) != 1 || m.Volumes[0].ID != vol.ID || m.NodeID != "node-1" {
		t.Fatalf("manifest on the pool: %+v", m)
	}

	// Nothing changed: no new write, even though live figures move.
	e.reconcile()
	e.reconcile()
	if dev.Writes != 1 {
		t.Fatalf("%d writes without a configuration change", dev.Writes)
	}

	// A rename is a change.
	if _, err := e.st.MutateVolume(vol.ID, func(v *store.Volume) error { v.Name = "renamed"; return nil }); err != nil {
		t.Fatal(err)
	}
	e.reconcile()
	if m := read(); m.Generation != 2 || m.Volumes[0].Name != "renamed" {
		t.Fatalf("after a rename: gen %d name %q", m.Generation, m.Volumes[0].Name)
	}

	// A torn write of the next generation leaves the previous one readable.
	if _, err := e.st.MutateVolume(vol.ID, func(v *store.Volume) error { v.Name = "again"; return nil }); err != nil {
		t.Fatal(err)
	}
	e.reconcile()
	dev.Corrupt(p.MetaVolumeUUID, meta.SlotFor(3))
	if m := read(); m.Generation != 2 {
		t.Fatalf("after a torn generation 3: read generation %d, want 2", m.Generation)
	}

	// A failing device flags the pool, and the copy catches up once it works again.
	dev.Fail = errors.New("device gone")
	if _, err := e.st.MutateVolume(vol.ID, func(v *store.Volume) error { v.Name = "later"; return nil }); err != nil {
		t.Fatal(err)
	}
	st := e.reconcile()
	if p := e.pool(); p.MetaError == "" || !strings.Contains(strings.Join(st.DegradedResources, " "), "pool:"+pool.ID) {
		t.Fatalf("failed write not reported: err %q degraded %v", p.MetaError, st.DegradedResources)
	}
	dev.Fail = nil
	e.reconcile()
	if p := e.pool(); p.MetaError != "" || p.MetaGeneration != 4 {
		t.Fatalf("after recovery: gen %d err %q", p.MetaGeneration, p.MetaError)
	}
	if m := read(); m.Volumes[0].Name != "later" {
		t.Fatalf("pool copy has %q", m.Volumes[0].Name)
	}
}

func TestMetaGenerationNeverGoesBackwards(t *testing.T) {
	e := newEnv(t)
	dev := &meta.MemDevice{}
	e.rec.SetMetaDevice(dev, "node-1", "test")
	e.provision(store.RaidLevel1, "0000:01:00.0", "0000:02:00.0")
	e.reconcile()
	p := e.pool()

	// The pool holds generation 7 from a newer database than the one now in use.
	m, _ := meta.Build(e.st, p.ID, "node-1", "test")
	m.Generation = 7
	img, _ := meta.EncodeSlot(m)
	if err := dev.WriteSlot(e.ctx, p.MetaVolumeUUID, meta.SlotFor(7), img); err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.MutatePool(p.ID, func(c *store.Pool) error { c.Name = "restored"; return nil }); err != nil {
		t.Fatal(err)
	}
	// A fresh velod process (new reconciler on the same store).
	e.rec = New(e.st, e.fake, nil, nil)
	e.rec.SetMetaDevice(dev, "node-1", "test")
	e.reconcile()
	if p := e.pool(); p.MetaGeneration != 8 {
		t.Fatalf("generation %d, want 8 (above the pool's 7)", p.MetaGeneration)
	}
}
