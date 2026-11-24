package store

import (
	"path/filepath"
	"testing"

	bolt "go.etcd.io/bbolt"
)

// A database written before the K-2 renames opens with its records' fields where they now are.
func TestLegacyKeysUpgraded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := bolt.Open(path, 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	old := map[string]map[string]string{
		"disks":     {"d1": `{"id":"d1","name":"d1","type":"NVME","spdk_bdev_name":"Nvme_0000_01_00_0n1","bdev_uuid":"u1","claimed":true}`},
		"pools":     {"p1": `{"id":"p1","name":"p","raid_level":"RAID1","base_bdev_name":"raid_p1","effective_bdev_name":"raid_p1","lvs_uuid":"lu","lvs_name":"lvs_p1","cluster_size_bytes":1048576,"meta_lvol_uuid":"mu"}`},
		"volumes":   {"v1": `{"id":"v1","name":"v","pool_id":"p1","lvol_bdev_name":"lvs_p1/v1","effective_bdev_name":"lvs_p1/v1"}`},
		"snapshots": {"s1": `{"id":"s1","name":"s","volume_id":"v1","lvol_bdev_name":"lvs_p1/s1"}`},
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		for bucket, recs := range old {
			b, err := tx.CreateBucketIfNotExists([]byte(bucket))
			if err != nil {
				return err
			}
			for k, v := range recs {
				if err := b.Put([]byte(k), []byte(v)); err != nil {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	st, err := OpenBoltStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	d, _ := st.GetDisk("d1")
	p, _ := st.GetPool("p1")
	v, _ := st.GetVolume("v1")
	s, _ := st.GetSnapshot("s1")
	if d.Handle != "Nvme_0000_01_00_0n1" {
		t.Errorf("disk %+v", d)
	}
	if p.ArrayName != "raid_p1" || p.Device != "raid_p1" || p.ThinPoolUUID != "lu" || p.ThinPoolName != "lvs_p1" || p.ChunkSizeBytes != 1<<20 || p.MetaVolumeUUID != "mu" {
		t.Errorf("pool %+v", p)
	}
	if v.Handle != "lvs_p1/v1" || v.Device != "lvs_p1/v1" || s.Handle != "lvs_p1/s1" {
		t.Errorf("volume %+v, snapshot %+v", v, s)
	}
	if string(UpgradeJSON([]byte(`{"handle":"x"}`))) != `{"handle":"x"}` {
		t.Error("current JSON changed")
	}
}
