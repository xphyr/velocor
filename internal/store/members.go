package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	bolt "go.etcd.io/bbolt"
)

var keySettings = []byte("settings")

// PoolMembers returns a pool's RAID slots. Pools created before member tracking have none
// recorded: their slots are their disks, in creation (slot) order, assumed active until the
// reconciler reads the real state from SPDK.
func PoolMembers(p *Pool) []PoolMember {
	if len(p.Members) > 0 {
		return slices.Clone(p.Members)
	}
	out := make([]PoolMember, len(p.DiskIDs))
	for i, id := range p.DiskIDs {
		out[i] = PoolMember{Slot: i, DiskID: id, State: MemberStateActive}
	}
	return out
}

// SetPoolMembers records the slot states and derives DiskIDs (the disks in a slot).
func SetPoolMembers(p *Pool, members []PoolMember) {
	p.Members = members
	p.DiskIDs = p.DiskIDs[:0:0]
	for _, m := range members {
		if m.DiskID != "" && (m.State == MemberStateActive || m.State == MemberStateRebuilding) {
			p.DiskIDs = append(p.DiskIDs, m.DiskID)
		}
	}
}

// ReplacePoolMember puts newDiskID into slot of the pool as REBUILDING, binding it to the
// pool. The disk previously recorded for the slot (if any) becomes detached: still bound to
// the pool until released. The new disk must be claimed, not the boot disk and in no pool.
func (s *BoltStore) ReplacePoolMember(poolID string, slot int, newDiskID string) (*Pool, error) {
	var out Pool
	err := s.db.Update(func(tx *bolt.Tx) error {
		poolsB, disksB := tx.Bucket(bucketPools), tx.Bucket(bucketDisks)
		if err := getJSON(poolsB, []byte(poolID), &out); err != nil {
			return err
		}
		members := PoolMembers(&out)
		if slot < 0 || slot >= len(members) {
			return fmt.Errorf("%w: pool %s has no slot %d", ErrInvalidArgument, poolID, slot)
		}
		var d Disk
		if err := getJSON(disksB, []byte(newDiskID), &d); err != nil {
			return fmt.Errorf("disk %s: %w", newDiskID, err)
		}
		switch {
		case d.IsBootDevice:
			return fmt.Errorf("disk %s: %w", newDiskID, ErrBootDeviceProtected)
		case !d.Claimed:
			return fmt.Errorf("%w: disk %s must be claimed first", ErrInvalidArgument, newDiskID)
		case d.PoolID != "":
			return fmt.Errorf("%w: disk %s belongs to pool %s", ErrInUse, newDiskID, d.PoolID)
		}
		if old := members[slot].DiskID; old != "" && old != newDiskID && !slices.Contains(out.DetachedDiskIDs, old) {
			out.DetachedDiskIDs = append(out.DetachedDiskIDs, old)
		}
		members[slot] = PoolMember{Slot: slot, DiskID: newDiskID, State: MemberStateRebuilding}
		SetPoolMembers(&out, members)
		d.PoolID = poolID
		d.UpdatedAt = time.Now().UTC()
		if err := putJSON(disksB, []byte(d.ID), &d); err != nil {
			return err
		}
		return putJSON(poolsB, []byte(poolID), &out)
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// RelinkPoolMember moves a pool member to the record of the same disk under a new ID (docs/review-history.md
// P1-14): newDiskID takes oldDiskID's place in the pool's slots, keeps the bdev UUID the RAID
// superblock knows the member by, and inherits the operator's settings for the disk. It is
// claimed (the reconciler attaches it to SPDK). The old record, of a disk that is no longer
// there under that ID, is deleted.
func (s *BoltStore) RelinkPoolMember(poolID, oldDiskID, newDiskID, bdevUUID string) (*Pool, error) {
	var out Pool
	err := s.db.Update(func(tx *bolt.Tx) error {
		poolsB, disksB := tx.Bucket(bucketPools), tx.Bucket(bucketDisks)
		if err := getJSON(poolsB, []byte(poolID), &out); err != nil {
			return err
		}
		var old, d Disk
		if err := getJSON(disksB, []byte(oldDiskID), &old); err != nil {
			return fmt.Errorf("disk %s: %w", oldDiskID, err)
		}
		if err := getJSON(disksB, []byte(newDiskID), &d); err != nil {
			return fmt.Errorf("disk %s: %w", newDiskID, err)
		}
		switch {
		case bdevUUID == "":
			return fmt.Errorf("%w: a bdev UUID is required", ErrInvalidArgument)
		case old.PoolID != poolID:
			return fmt.Errorf("%w: disk %s is not in pool %s", ErrInvalidArgument, oldDiskID, poolID)
		case d.IsBootDevice:
			return fmt.Errorf("disk %s: %w", newDiskID, ErrBootDeviceProtected)
		case d.Claimed || d.PoolID != "":
			return fmt.Errorf("%w: disk %s is already in use", ErrInUse, newDiskID)
		}
		members := PoolMembers(&out)
		found := false
		for i := range members {
			if members[i].DiskID == oldDiskID {
				members[i].DiskID, found = newDiskID, true
			}
		}
		if !found {
			return fmt.Errorf("%w: disk %s holds no slot of pool %s", ErrInvalidArgument, oldDiskID, poolID)
		}
		SetPoolMembers(&out, members)
		if i := slices.Index(out.DetachedDiskIDs, oldDiskID); i >= 0 {
			out.DetachedDiskIDs[i] = newDiskID
		}
		d.Claimed, d.PoolID, d.BdevUUID = true, poolID, bdevUUID
		d.DriverState, d.SPDKBdevName, d.HealthStatus = DiskDriverStateUnspecified, "", HealthStatusOnline
		d.WriteCachePolicy, d.PowerLossProtected = old.WriteCachePolicy, old.PowerLossProtected
		d.UpdatedAt = time.Now().UTC()
		if err := putJSON(disksB, []byte(d.ID), &d); err != nil {
			return err
		}
		if err := disksB.Delete([]byte(oldDiskID)); err != nil {
			return err
		}
		return putJSON(poolsB, []byte(poolID), &out)
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// ReleasePoolDisk unbinds a detached disk (one that left the pool's RAID) from the pool.
func (s *BoltStore) ReleasePoolDisk(poolID, diskID string) (*Pool, error) {
	var out Pool
	err := s.db.Update(func(tx *bolt.Tx) error {
		poolsB, disksB := tx.Bucket(bucketPools), tx.Bucket(bucketDisks)
		if err := getJSON(poolsB, []byte(poolID), &out); err != nil {
			return err
		}
		i := slices.Index(out.DetachedDiskIDs, diskID)
		if i < 0 {
			if slices.Contains(out.DiskIDs, diskID) {
				return fmt.Errorf("%w: disk %s is an active member of pool %s; replace it first", ErrInUse, diskID, poolID)
			}
			return fmt.Errorf("%w: disk %s is not detached from pool %s", ErrNotFound, diskID, poolID)
		}
		out.DetachedDiskIDs = slices.Delete(out.DetachedDiskIDs, i, i+1)
		members := PoolMembers(&out)
		for j := range members {
			if members[j].DiskID == diskID && members[j].State != MemberStateActive && members[j].State != MemberStateRebuilding {
				members[j].DiskID = "" // the empty slot no longer names it
			}
		}
		SetPoolMembers(&out, members)
		var d Disk
		if err := getJSON(disksB, []byte(diskID), &d); err == nil && d.PoolID == poolID {
			d.PoolID = ""
			d.UpdatedAt = time.Now().UTC()
			if err := putJSON(disksB, []byte(d.ID), &d); err != nil {
				return err
			}
		}
		return putJSON(poolsB, []byte(poolID), &out)
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// GetSettings returns the node-wide settings (defaults if never saved).
func (s *BoltStore) GetSettings() (*Settings, error) {
	var out Settings
	err := s.db.View(func(tx *bolt.Tx) error {
		if v := tx.Bucket(bucketSystem).Get(keySettings); v != nil {
			return json.Unmarshal(v, &out)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// SaveSettings persists the node-wide settings.
func (s *BoltStore) SaveSettings(st *Settings) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		return putJSON(tx.Bucket(bucketSystem), keySettings, st)
	})
}

// GetScrubRecord returns a pool's scrub record.
func (s *BoltStore) GetScrubRecord(poolID string) (*ScrubRecord, error) {
	var out ScrubRecord
	err := s.db.View(func(tx *bolt.Tx) error {
		return getJSON(tx.Bucket(bucketScrubs), []byte(poolID), &out)
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// MutateScrubRecord applies fn to a pool's scrub record in one transaction.
func (s *BoltStore) MutateScrubRecord(poolID string, fn func(*ScrubRecord) error) (*ScrubRecord, error) {
	var out ScrubRecord
	err := s.db.Update(func(tx *bolt.Tx) error {
		if tx.Bucket(bucketPools).Get([]byte(poolID)) == nil {
			return fmt.Errorf("%w: pool %s", ErrNotFound, poolID)
		}
		b := tx.Bucket(bucketScrubs)
		if err := getJSON(b, []byte(poolID), &out); err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
		out.PoolID = poolID
		if err := fn(&out); err != nil {
			return err
		}
		return putJSON(b, []byte(poolID), &out)
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}
