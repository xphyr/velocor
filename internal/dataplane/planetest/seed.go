package planetest

import (
	"context"
	"testing"

	"github.com/velocor/velocor/internal/store"
)

// Seed adds disks to the data plane and claims them, builds pool on them (array and thin pool),
// and records all of it in st, as the API would. The pool as stored is returned.
func Seed(t testing.TB, f *Fake, st store.Store, pool *store.Pool, disks ...*store.Disk) *store.Pool {
	t.Helper()
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	var handles []string
	pool.DiskIDs = nil
	for _, d := range disks {
		key := d.DevicePath
		if d.Type == store.DiskTypeNVMe {
			key = d.PCIAddress
		}
		f.AddDisk(key)
		must(st.UpsertDisk(d))
		handle, drv, err := f.AttachDisk(ctx, d)
		must(err)
		_, err = st.ClaimDisk(d.ID, drv, handle)
		must(err)
		pool.DiskIDs, handles = append(pool.DiskIDs, d.ID), append(handles, handle)
	}
	must(st.CreatePool(pool))
	must(f.CreateArray(ctx, pool.ArrayName, pool.RaidLevel, pool.StripSizeKB, handles))
	uuid, err := f.CreateThinPool(ctx, pool.ArrayName, pool.ThinPoolName, 0)
	must(err)
	out, err := st.MutatePool(pool.ID, func(p *store.Pool) error { p.ThinPoolUUID = uuid; return nil })
	must(err)
	return out
}

// SeedVolume creates a volume in a seeded pool and records it in st (vol.UUID, vol.Handle set).
func SeedVolume(t testing.TB, f *Fake, st store.Store, pool *store.Pool, vol *store.Volume) *store.Volume {
	t.Helper()
	uuid, err := f.CreateVolume(context.Background(), pool.ThinPoolName, vol.ID, vol.SizeBytes, true)
	if err != nil {
		t.Fatal(err)
	}
	vol.PoolID, vol.UUID, vol.Handle = pool.ID, uuid, pool.ThinPoolName+"/"+vol.ID
	if err := st.CreateVolume(vol); err != nil {
		t.Fatal(err)
	}
	return vol
}
