package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	bolt "go.etcd.io/bbolt"
)

// DefaultVolumeRetention is how long a deleted volume is kept when no retention is configured.
const DefaultVolumeRetention = 24 * time.Hour

// MaxVolumeRetentionHours bounds the configurable retention (30 days).
const MaxVolumeRetentionHours = 30 * 24

// VolumeRetention is how long deleted volumes are kept before they are purged; 0 means they
// are deleted at once.
func (st *Settings) VolumeRetention() time.Duration {
	if st == nil || st.VolumeRetentionHours == nil {
		return DefaultVolumeRetention
	}
	return time.Duration(*st.VolumeRetentionHours) * time.Hour
}

// TrashVolume marks a volume deleted: hidden and not exportable, its data kept until purgeAt.
// It refuses what DeleteVolume refuses (attached, exported, protected). A volume that is
// already deleted is returned unchanged, so retries are harmless.
func (s *BoltStore) TrashVolume(id string, now, purgeAt time.Time, by string) (*Volume, error) {
	var vol Volume
	err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketVolumes)
		if err := getJSON(b, []byte(id), &vol); err != nil {
			return err
		}
		if vol.Deleted() {
			return nil
		}
		if err := checkVolumeDeletableTx(tx, id); err != nil {
			return err
		}
		now, purgeAt := now.UTC(), purgeAt.UTC()
		vol.DeletedAt, vol.PurgeAt, vol.DeletedBy = &now, &purgeAt, by
		return putJSON(b, []byte(id), &vol)
	})
	if err != nil {
		return nil, err
	}
	return &vol, nil
}

// RestoreVolume brings a deleted volume back, under newName if given (its own name may have
// been reused since). It comes back unexported, as it was deleted.
func (s *BoltStore) RestoreVolume(id, newName string) (*Volume, error) {
	var vol Volume
	err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketVolumes)
		if err := getJSON(b, []byte(id), &vol); err != nil {
			return err
		}
		if !vol.Deleted() {
			return fmt.Errorf("%w: volume %s is not deleted", ErrInvalidArgument, id)
		}
		if vol.Purging {
			return fmt.Errorf("%w: volume %s is being purged", ErrInUse, id)
		}
		if newName != "" {
			vol.Name = newName
		}
		err := b.ForEach(func(_, v []byte) error {
			var other Volume
			if err := json.Unmarshal(v, &other); err != nil {
				return err
			}
			if other.ID != id && !other.Deleted() && other.PoolID == vol.PoolID && other.Name == vol.Name {
				return fmt.Errorf("%w: volume %s exists in pool %s; restore under another name", ErrAlreadyExists, vol.Name, vol.PoolID)
			}
			return nil
		})
		if err != nil {
			return err
		}
		vol.DeletedAt, vol.PurgeAt, vol.DeletedBy = nil, nil, ""
		return putJSON(b, []byte(id), &vol)
	})
	if err != nil {
		return nil, err
	}
	return &vol, nil
}

// ErrNotDue is returned by StartPurge for a deleted volume whose retention hasn't ended.
var ErrNotDue = errors.New("retention period not over")

// StartPurge marks a deleted volume as being purged, so it can no longer be restored, and
// returns it. With due set, only a volume whose retention ended by then qualifies (else
// ErrNotDue). Callers then delete the lvol and the record (DeleteVolume).
func StartPurge(st Store, id string, due time.Time) (*Volume, error) {
	return st.MutateVolume(id, func(v *Volume) error {
		if !v.Deleted() {
			return fmt.Errorf("%w: volume %s is not deleted; delete it first", ErrInvalidArgument, id)
		}
		if !due.IsZero() && (v.PurgeAt == nil || v.PurgeAt.After(due)) {
			return ErrNotDue
		}
		v.Purging = true
		return nil
	})
}

// errVolumeDeleted refuses an operation on a deleted volume.
func errVolumeDeleted(vol *Volume) error {
	return fmt.Errorf("%w: volume %s is deleted (restore it first)", ErrNotFound, vol.ID)
}

// DeferSnapshotDelete handles deleting a snapshot that cloned volumes depend on (live ones,
// or deleted ones waiting to be purged): it is marked deleted and hidden, and the reconciler
// deletes it once nothing depends on it (PurgeableSnapshot). It returns false, changing
// nothing, when no volume depends on the snapshot: the caller deletes it now. A snapshot that
// is already deleted returns true.
func (s *BoltStore) DeferSnapshotDelete(id string, now time.Time, by string) (bool, error) {
	deferred := false
	err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketSnapshots)
		var snap Snapshot
		if err := getJSON(b, []byte(id), &snap); err != nil {
			return err
		}
		if snap.Deleted() {
			deferred = true
			return nil
		}
		dependents, err := snapshotDependentsTx(tx, id)
		if err != nil || dependents == 0 {
			return err
		}
		now := now.UTC()
		snap.DeletedAt, snap.DeletedBy = &now, by
		deferred = true
		return putJSON(b, []byte(id), &snap)
	})
	return deferred, err
}

// PurgeableSnapshot reports whether a deleted snapshot no longer has volumes depending on it.
func (s *BoltStore) PurgeableSnapshot(id string) (bool, error) {
	ok := false
	err := s.db.View(func(tx *bolt.Tx) error {
		var snap Snapshot
		if err := getJSON(tx.Bucket(bucketSnapshots), []byte(id), &snap); err != nil {
			return err
		}
		n, err := snapshotDependentsTx(tx, id)
		ok = snap.Deleted() && n == 0
		return err
	})
	return ok, err
}

// snapshotDependentsTx counts the volumes (deleted or not) cloned from a snapshot.
func snapshotDependentsTx(tx *bolt.Tx, id string) (int, error) {
	n := 0
	err := tx.Bucket(bucketVolumes).ForEach(func(_, v []byte) error {
		var vol Volume
		if err := json.Unmarshal(v, &vol); err != nil {
			return err
		}
		if vol.ParentSnapshotID == id {
			n++
		}
		return nil
	})
	return n, err
}
