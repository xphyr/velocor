package store

import (
	"bytes"
	"fmt"

	bolt "go.etcd.io/bbolt"
)

// legacyKeys are JSON keys from the SPDK-only data plane, renamed with the move to a data plane
// interface (TODO.md K-2): the data plane's handles, not SPDK's names.
var legacyKeys = [][2]string{
	{"spdk_bdev_name", "handle"},
	{"bdev_uuid", "member_uuid"},
	{"base_bdev_name", "array_name"},
	{"effective_bdev_name", "device"},
	{"lvs_uuid", "thin_pool_uuid"},
	{"lvs_name", "thin_pool_name"},
	{"cluster_size_bytes", "chunk_size_bytes"},
	{"meta_lvol_uuid", "meta_volume_uuid"},
	{"lvol_bdev_name", "handle"},
}

// UpgradeJSON renames the legacy keys in a disk, pool, volume or snapshot record (or a manifest
// holding them); JSON already in the current form comes back unchanged.
func UpgradeJSON(b []byte) []byte {
	for _, k := range legacyKeys {
		old := []byte(`"` + k[0] + `":`)
		if bytes.Contains(b, old) {
			b = bytes.ReplaceAll(b, old, []byte(`"`+k[1]+`":`))
		}
	}
	return b
}

// upgradeKeys rewrites records stored with legacy keys, once, when the database is opened.
func upgradeKeys(tx *bolt.Tx) error {
	for _, name := range [][]byte{bucketDisks, bucketPools, bucketVolumes, bucketSnapshots} {
		b := tx.Bucket(name)
		var keys, vals [][]byte
		if err := b.ForEach(func(k, v []byte) error {
			if nv := UpgradeJSON(bytes.Clone(v)); !bytes.Equal(nv, v) {
				keys, vals = append(keys, bytes.Clone(k)), append(vals, nv)
			}
			return nil
		}); err != nil {
			return err
		}
		for i := range keys {
			if err := b.Put(keys[i], vals[i]); err != nil {
				return fmt.Errorf("upgrade %s/%s: %w", name, keys[i], err)
			}
		}
	}
	return nil
}
