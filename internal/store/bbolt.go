package store

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"
)

var (
	bucketSystem          = []byte("system")
	bucketUsers           = []byte("users")
	bucketTokens          = []byte("tokens")
	bucketDisks           = []byte("disks")
	bucketPools           = []byte("pools")
	bucketVolumes              = []byte("volumes")
	bucketSnapshots            = []byte("snapshots")
	bucketVolumeGroupSnapshots = []byte("volume_group_snapshots")
	bucketPortals              = []byte("portals")
	bucketHosts           = []byte("hosts")
	bucketInitiatorGroups = []byte("initiator_groups")
	bucketExports         = []byte("exports")

	keySystemConfig = []byte("config")
)

var allBuckets = [][]byte{
	bucketSystem,
	bucketUsers,
	bucketTokens,
	bucketDisks,
	bucketPools,
	bucketVolumes,
	bucketSnapshots,
	bucketVolumeGroupSnapshots,
	bucketPortals,
	bucketHosts,
	bucketInitiatorGroups,
	bucketExports,
}

// BoltStore implements Store backed by an embedded bbolt key-value database.
type BoltStore struct {
	db *bolt.DB
}

// OpenBoltStore opens or initializes a bbolt database at the specified path.
func OpenBoltStore(path string) (*BoltStore, error) {
	db, err := bolt.Open(path, 0600, &bolt.Options{Timeout: 3 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("open bbolt db %s: %w", path, err)
	}

	err = db.Update(func(tx *bolt.Tx) error {
		for _, b := range allBuckets {
			if _, err := tx.CreateBucketIfNotExists(b); err != nil {
				return fmt.Errorf("create bucket %s: %w", string(b), err)
			}
		}
		return nil
	})
	if err != nil {
		_ = db.Close()
		return nil, err
	}

	return &BoltStore{db: db}, nil
}

// Close closes the underlying bbolt database.
func (s *BoltStore) Close() error {
	return s.db.Close()
}

// GetSystemConfig retrieves the node-wide SystemConfig.
func (s *BoltStore) GetSystemConfig() (*SystemConfig, error) {
	var cfg SystemConfig
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketSystem)
		v := b.Get(keySystemConfig)
		if v == nil {
			return ErrNotFound
		}
		return json.Unmarshal(v, &cfg)
	})
	if err != nil {
		return nil, err
	}
	return &cfg, nil
}

// SaveSystemConfig persists the node-wide SystemConfig.
func (s *BoltStore) SaveSystemConfig(cfg *SystemConfig) error {
	if cfg == nil || cfg.NodeID == "" {
		return fmt.Errorf("%w: node_id is required", ErrInvalidArgument)
	}
	if cfg.InitializedAt.IsZero() {
		cfg.InitializedAt = time.Now().UTC()
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		return putJSON(tx.Bucket(bucketSystem), keySystemConfig, cfg)
	})
}

// GetAdminUser retrieves an AdminUser by username.
func (s *BoltStore) GetAdminUser(username string) (*AdminUser, error) {
	if username == "" {
		return nil, fmt.Errorf("%w: username is required", ErrInvalidArgument)
	}
	var user AdminUser
	err := s.db.View(func(tx *bolt.Tx) error {
		return getJSON(tx.Bucket(bucketUsers), []byte(username), &user)
	})
	if err != nil {
		return nil, err
	}
	if user.Role == "" {
		user.Role = RoleAdmin
	}
	return &user, nil
}

// SaveAdminUser creates or updates an AdminUser.
func (s *BoltStore) SaveAdminUser(user *AdminUser) error {
	if user == nil || user.Username == "" || user.PasswordHash == "" {
		return fmt.Errorf("%w: username and password_hash are required", ErrInvalidArgument)
	}
	now := time.Now().UTC()
	if user.Role == "" {
		user.Role = RoleAdmin
	}
	if user.CreatedAt.IsZero() {
		user.CreatedAt = now
	}
	if user.UpdatedAt.IsZero() {
		user.UpdatedAt = now
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		return putJSON(tx.Bucket(bucketUsers), []byte(user.Username), user)
	})
}

// ListUsers returns all persisted local users.
func (s *BoltStore) ListUsers() ([]*AdminUser, error) {
	var users []*AdminUser
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketUsers)
		return b.ForEach(func(_, v []byte) error {
			var u AdminUser
			if err := json.Unmarshal(v, &u); err != nil {
				return err
			}
			if u.Role == "" {
				u.Role = RoleAdmin
			}
			users = append(users, &u)
			return nil
		})
	})
	return users, err
}

// DeleteUser removes a local user by username.
func (s *BoltStore) DeleteUser(username string) error {
	if username == "" {
		return fmt.Errorf("%w: username is required", ErrInvalidArgument)
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketUsers)
		if b.Get([]byte(username)) == nil {
			return fmt.Errorf("%w: user %q", ErrNotFound, username)
		}
		return b.Delete([]byte(username))
	})
}

// CreateApiToken persists a new scoped API token.
func (s *BoltStore) CreateApiToken(token *ApiToken) error {
	if token == nil || token.ID == "" || token.Name == "" || token.TokenHash == "" {
		return fmt.Errorf("%w: token id, name, and token_hash are required", ErrInvalidArgument)
	}
	if token.CreatedAt.IsZero() {
		token.CreatedAt = time.Now().UTC()
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketTokens)
		if b.Get([]byte(token.ID)) != nil {
			return fmt.Errorf("%w: token %s", ErrAlreadyExists, token.ID)
		}
		return putJSON(b, []byte(token.ID), token)
	})
}

// GetApiToken retrieves an ApiToken by ID.
func (s *BoltStore) GetApiToken(id string) (*ApiToken, error) {
	var token ApiToken
	err := s.db.View(func(tx *bolt.Tx) error {
		return getJSON(tx.Bucket(bucketTokens), []byte(id), &token)
	})
	if err != nil {
		return nil, err
	}
	return &token, nil
}

// UpdateApiTokenLastUsed updates the LastUsedAt timestamp for an API token.
func (s *BoltStore) UpdateApiTokenLastUsed(id string, lastUsed time.Time) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketTokens)
		var token ApiToken
		if err := getJSON(b, []byte(id), &token); err != nil {
			return err
		}
		ts := lastUsed.UTC()
		token.LastUsedAt = &ts
		return putJSON(b, []byte(id), &token)
	})
}

// ListApiTokens returns all API tokens ordered by CreatedAt.
func (s *BoltStore) ListApiTokens() ([]*ApiToken, error) {
	var tokens []*ApiToken
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketTokens)
		return b.ForEach(func(_, v []byte) error {
			var t ApiToken
			if err := json.Unmarshal(v, &t); err != nil {
				return err
			}
			tokens = append(tokens, &t)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(tokens, func(i, j int) bool {
		return tokens[i].CreatedAt.Before(tokens[j].CreatedAt)
	})
	return tokens, nil
}

// DeleteApiToken removes an API token by ID.
func (s *BoltStore) DeleteApiToken(id string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketTokens)
		if b.Get([]byte(id)) == nil {
			return ErrNotFound
		}
		return b.Delete([]byte(id))
	})
}

// UpsertDisk inserts or updates a discovered Disk while preserving boot-device protection
// and existing claim/pool state if not explicitly overwritten.
func (s *BoltStore) UpsertDisk(disk *Disk) error {
	return s.upsertDisk(disk, false)
}

// SyncDiscoveredDisk updates disk metadata from an authoritative hardware scan,
// allowing IsBootDevice to be updated when boot-guard classification changes.
func (s *BoltStore) SyncDiscoveredDisk(disk *Disk) error {
	return s.upsertDisk(disk, true)
}

func (s *BoltStore) upsertDisk(disk *Disk, syncBoot bool) error {
	if disk == nil || disk.ID == "" {
		return fmt.Errorf("%w: disk id is required", ErrInvalidArgument)
	}
	cp := *disk
	if cp.UpdatedAt.IsZero() {
		cp.UpdatedAt = time.Now().UTC()
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketDisks)
		var existing Disk
		if err := getJSON(b, []byte(cp.ID), &existing); err == nil {
			// Once marked as a boot device, non-authoritative upserts preserve boot protection.
			if !syncBoot && existing.IsBootDevice {
				cp.IsBootDevice = true
			}
			if cp.IsBootDevice && cp.Claimed {
				return ErrBootDeviceProtected
			}
			// Preserve PoolID if the disk belongs to a pool.
			if existing.PoolID != "" && cp.PoolID == "" {
				cp.PoolID = existing.PoolID
			}
			// Preserve hardware identity & capacity when an NVMe drive is bound to vfio-pci/uio
			// and its kernel nvme sysfs node is no longer present.
			if cp.SizeBytes == 0 && existing.SizeBytes > 0 {
				cp.SizeBytes = existing.SizeBytes
			}
			if cp.BlockSize == 0 && existing.BlockSize > 0 {
				cp.BlockSize = existing.BlockSize
			}
			if cp.Model == "" && existing.Model != "" {
				cp.Model = existing.Model
			}
			if cp.SerialNumber == "" && existing.SerialNumber != "" {
				cp.SerialNumber = existing.SerialNumber
			}
			if cp.FirmwareRevision == "" && existing.FirmwareRevision != "" {
				cp.FirmwareRevision = existing.FirmwareRevision
			}
			if (cp.Name == "" || strings.HasPrefix(cp.Name, "nvme_")) && existing.Name != "" && !strings.HasPrefix(existing.Name, "nvme_") {
				cp.Name = existing.Name
			}
		} else if cp.IsBootDevice && cp.Claimed {
			return ErrBootDeviceProtected
		}
		return putJSON(b, []byte(cp.ID), &cp)
	})
}

// GetDisk retrieves a Disk by ID.
func (s *BoltStore) GetDisk(id string) (*Disk, error) {
	var disk Disk
	err := s.db.View(func(tx *bolt.Tx) error {
		return getJSON(tx.Bucket(bucketDisks), []byte(id), &disk)
	})
	if err != nil {
		return nil, err
	}
	return &disk, nil
}

// ListDisks returns all Disks ordered by ID.
func (s *BoltStore) ListDisks() ([]*Disk, error) {
	var disks []*Disk
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketDisks)
		return b.ForEach(func(_, v []byte) error {
			var d Disk
			if err := json.Unmarshal(v, &d); err != nil {
				return err
			}
			disks = append(disks, &d)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(disks, func(i, j int) bool {
		return disks[i].ID < disks[j].ID
	})
	return disks, nil
}

// ClaimDisk marks a non-boot Disk as claimed by SPDK.
func (s *BoltStore) ClaimDisk(id string, driverState DiskDriverState, spdkBdevName string) (*Disk, error) {
	var updated Disk
	err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketDisks)
		if err := getJSON(b, []byte(id), &updated); err != nil {
			return err
		}
		if updated.IsBootDevice {
			return ErrBootDeviceProtected
		}
		updated.Claimed = true
		if driverState != "" && driverState != DiskDriverStateUnspecified {
			updated.DriverState = driverState
		}
		if spdkBdevName != "" {
			updated.SPDKBdevName = spdkBdevName
		}
		if updated.HealthStatus == "" || updated.HealthStatus == HealthStatusUnspecified {
			updated.HealthStatus = HealthStatusOnline
		}
		updated.UpdatedAt = time.Now().UTC()
		return putJSON(b, []byte(id), &updated)
	})
	if err != nil {
		return nil, err
	}
	return &updated, nil
}

// ReleaseDisk releases a claimed Disk back to the Linux kernel if it is not part of a Pool.
func (s *BoltStore) ReleaseDisk(id string) (*Disk, error) {
	var updated Disk
	err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketDisks)
		if err := getJSON(b, []byte(id), &updated); err != nil {
			return err
		}
		if updated.PoolID != "" {
			return fmt.Errorf("%w: disk %s is assigned to pool %s", ErrInUse, id, updated.PoolID)
		}
		updated.Claimed = false
		updated.DriverState = DiskDriverStateKernel
		updated.SPDKBdevName = ""
		updated.UpdatedAt = time.Now().UTC()
		return putJSON(b, []byte(id), &updated)
	})
	if err != nil {
		return nil, err
	}
	return &updated, nil
}

// DeleteDisk removes a Disk record if it is not part of a Pool.
func (s *BoltStore) DeleteDisk(id string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketDisks)
		var disk Disk
		if err := getJSON(b, []byte(id), &disk); err != nil {
			return err
		}
		if disk.PoolID != "" {
			return fmt.Errorf("%w: disk %s belongs to pool %s", ErrInUse, id, disk.PoolID)
		}
		return b.Delete([]byte(id))
	})
}

// CreatePool creates a new Pool and atomically binds all member Disks to it.
func (s *BoltStore) CreatePool(pool *Pool) error {
	if pool == nil || pool.ID == "" || pool.Name == "" || len(pool.DiskIDs) == 0 {
		return fmt.Errorf("%w: pool id, name, and disk_ids are required", ErrInvalidArgument)
	}
	cp := *pool
	cp.DiskIDs = slices.Clone(pool.DiskIDs)
	if cp.ClusterSizeBytes == 0 {
		cp.ClusterSizeBytes = DefaultClusterSizeBytes
	}
	if cp.BaseBdevName == "" {
		cp.BaseBdevName = fmt.Sprintf("raid_%s", cp.Name)
	}
	if cp.EffectiveBdevName == "" {
		cp.EffectiveBdevName = cp.BaseBdevName
	}
	if cp.LVSName == "" {
		cp.LVSName = fmt.Sprintf("lvs_%s", cp.Name)
	}
	if cp.HealthStatus == "" || cp.HealthStatus == HealthStatusUnspecified {
		cp.HealthStatus = HealthStatusOnline
	}
	if cp.CreatedAt.IsZero() {
		cp.CreatedAt = time.Now().UTC()
	}

	return s.db.Update(func(tx *bolt.Tx) error {
		poolsB := tx.Bucket(bucketPools)
		if poolsB.Get([]byte(cp.ID)) != nil {
			return fmt.Errorf("%w: pool id %s", ErrAlreadyExists, cp.ID)
		}
		// Check name uniqueness across pools.
		err := poolsB.ForEach(func(_, v []byte) error {
			var existing Pool
			if err := json.Unmarshal(v, &existing); err != nil {
				return err
			}
			if existing.Name == cp.Name {
				return fmt.Errorf("%w: pool name %s", ErrAlreadyExists, cp.Name)
			}
			return nil
		})
		if err != nil {
			return err
		}

		// Verify and claim each disk for this pool.
		disksB := tx.Bucket(bucketDisks)
		seenDisks := make(map[string]struct{}, len(cp.DiskIDs))
		for _, diskID := range cp.DiskIDs {
			if _, dup := seenDisks[diskID]; dup {
				return fmt.Errorf("%w: duplicate disk_id %s in pool", ErrInvalidArgument, diskID)
			}
			seenDisks[diskID] = struct{}{}

			var d Disk
			if err := getJSON(disksB, []byte(diskID), &d); err != nil {
				return fmt.Errorf("disk %s: %w", diskID, err)
			}
			if d.IsBootDevice {
				return fmt.Errorf("disk %s: %w", diskID, ErrBootDeviceProtected)
			}
			if !d.Claimed {
				return fmt.Errorf("%w: disk %s must be claimed before adding to a pool", ErrInvalidArgument, diskID)
			}
			if d.PoolID != "" {
				return fmt.Errorf("%w: disk %s already belongs to pool %s", ErrInUse, diskID, d.PoolID)
			}
			d.PoolID = cp.ID
			d.UpdatedAt = time.Now().UTC()
			if err := putJSON(disksB, []byte(d.ID), &d); err != nil {
				return err
			}
		}

		if err := putJSON(poolsB, []byte(cp.ID), &cp); err != nil {
			return err
		}
		*pool = cp
		return nil
	})
}

// GetPool retrieves a Pool by ID.
func (s *BoltStore) GetPool(id string) (*Pool, error) {
	var pool Pool
	err := s.db.View(func(tx *bolt.Tx) error {
		return getJSON(tx.Bucket(bucketPools), []byte(id), &pool)
	})
	if err != nil {
		return nil, err
	}
	return &pool, nil
}

// GetPoolByName retrieves a Pool by its unique name.
func (s *BoltStore) GetPoolByName(name string) (*Pool, error) {
	var found *Pool
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketPools)
		return b.ForEach(func(_, v []byte) error {
			var p Pool
			if err := json.Unmarshal(v, &p); err != nil {
				return err
			}
			if p.Name == name {
				found = &p
			}
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	if found == nil {
		return nil, ErrNotFound
	}
	return found, nil
}

// UpdatePool updates mutable fields of an existing Pool (capacity, health, UUIDs, effective bdev).
func (s *BoltStore) UpdatePool(pool *Pool) error {
	if pool == nil || pool.ID == "" {
		return fmt.Errorf("%w: pool id is required", ErrInvalidArgument)
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketPools)
		var existing Pool
		if err := getJSON(b, []byte(pool.ID), &existing); err != nil {
			return err
		}
		return putJSON(b, []byte(pool.ID), pool)
	})
}

// ListPools returns all Pools ordered by CreatedAt.
func (s *BoltStore) ListPools() ([]*Pool, error) {
	var pools []*Pool
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketPools)
		return b.ForEach(func(_, v []byte) error {
			var p Pool
			if err := json.Unmarshal(v, &p); err != nil {
				return err
			}
			pools = append(pools, &p)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(pools, func(i, j int) bool {
		return pools[i].CreatedAt.Before(pools[j].CreatedAt)
	})
	return pools, nil
}

// DeletePool deletes a Pool if it has no Volumes or Snapshots, and clears PoolID on its member Disks.
func (s *BoltStore) DeletePool(id string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		poolsB := tx.Bucket(bucketPools)
		var pool Pool
		if err := getJSON(poolsB, []byte(id), &pool); err != nil {
			return err
		}

		// Ensure no volumes exist in this pool.
		volsB := tx.Bucket(bucketVolumes)
		err := volsB.ForEach(func(_, v []byte) error {
			var vol Volume
			if err := json.Unmarshal(v, &vol); err != nil {
				return err
			}
			if vol.PoolID == id {
				return fmt.Errorf("%w: pool %s contains volume %s", ErrInUse, id, vol.ID)
			}
			return nil
		})
		if err != nil {
			return err
		}

		// Ensure no snapshots exist in this pool.
		snapsB := tx.Bucket(bucketSnapshots)
		err = snapsB.ForEach(func(_, v []byte) error {
			var snap Snapshot
			if err := json.Unmarshal(v, &snap); err != nil {
				return err
			}
			if snap.PoolID == id {
				return fmt.Errorf("%w: pool %s contains snapshot %s", ErrInUse, id, snap.ID)
			}
			return nil
		})
		if err != nil {
			return err
		}

		// Release member disks from this pool.
		disksB := tx.Bucket(bucketDisks)
		for _, diskID := range pool.DiskIDs {
			var d Disk
			if err := getJSON(disksB, []byte(diskID), &d); err == nil {
				if d.PoolID == id {
					d.PoolID = ""
					d.UpdatedAt = time.Now().UTC()
					if err := putJSON(disksB, []byte(d.ID), &d); err != nil {
						return err
					}
				}
			}
		}

		return poolsB.Delete([]byte(id))
	})
}

// CreateVolume creates a new Volume inside an existing Pool.
func (s *BoltStore) CreateVolume(vol *Volume) error {
	if vol == nil || vol.ID == "" || vol.Name == "" || vol.PoolID == "" || vol.SizeBytes == 0 {
		return fmt.Errorf("%w: volume id, name, pool_id, and size_bytes are required", ErrInvalidArgument)
	}
	cp := *vol
	if cp.AttachmentState == "" || cp.AttachmentState == AttachmentStateUnspecified {
		cp.AttachmentState = AttachmentStateUnattached
	}
	if cp.HealthStatus == "" || cp.HealthStatus == HealthStatusUnspecified {
		cp.HealthStatus = HealthStatusOnline
	}
	if cp.CreatedAt.IsZero() {
		cp.CreatedAt = time.Now().UTC()
	}

	return s.db.Update(func(tx *bolt.Tx) error {
		// Verify parent Pool exists.
		var pool Pool
		if err := getJSON(tx.Bucket(bucketPools), []byte(cp.PoolID), &pool); err != nil {
			return fmt.Errorf("pool %s: %w", cp.PoolID, err)
		}

		// Verify parent Snapshot exists if this volume is a clone.
		if cp.ParentSnapshotID != "" {
			var snap Snapshot
			if err := getJSON(tx.Bucket(bucketSnapshots), []byte(cp.ParentSnapshotID), &snap); err != nil {
				return fmt.Errorf("parent snapshot %s: %w", cp.ParentSnapshotID, err)
			}
		}

		if cp.LvolBdevName == "" {
			cp.LvolBdevName = fmt.Sprintf("%s/%s", pool.LVSName, cp.Name)
		}
		if cp.EffectiveBdevName == "" {
			cp.EffectiveBdevName = cp.LvolBdevName
		}

		volsB := tx.Bucket(bucketVolumes)
		if volsB.Get([]byte(cp.ID)) != nil {
			return fmt.Errorf("%w: volume id %s", ErrAlreadyExists, cp.ID)
		}
		err := volsB.ForEach(func(_, v []byte) error {
			var existing Volume
			if err := json.Unmarshal(v, &existing); err != nil {
				return err
			}
			if existing.PoolID == cp.PoolID && existing.Name == cp.Name {
				return fmt.Errorf("%w: volume %s in pool %s", ErrAlreadyExists, cp.Name, cp.PoolID)
			}
			return nil
		})
		if err != nil {
			return err
		}

		if err := putJSON(volsB, []byte(cp.ID), &cp); err != nil {
			return err
		}
		*vol = cp
		return nil
	})
}

// GetVolume retrieves a Volume by ID.
func (s *BoltStore) GetVolume(id string) (*Volume, error) {
	var vol Volume
	err := s.db.View(func(tx *bolt.Tx) error {
		return getJSON(tx.Bucket(bucketVolumes), []byte(id), &vol)
	})
	if err != nil {
		return nil, err
	}
	return &vol, nil
}

// GetVolumeByName retrieves a Volume by PoolID (optional if empty) and Name.
func (s *BoltStore) GetVolumeByName(poolID, name string) (*Volume, error) {
	var found *Volume
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketVolumes)
		return b.ForEach(func(_, v []byte) error {
			var vol Volume
			if err := json.Unmarshal(v, &vol); err != nil {
				return err
			}
			if vol.Name == name && (poolID == "" || vol.PoolID == poolID) {
				found = &vol
			}
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	if found == nil {
		return nil, ErrNotFound
	}
	return found, nil
}

// UpdateVolume updates an existing Volume.
func (s *BoltStore) UpdateVolume(vol *Volume) error {
	if vol == nil || vol.ID == "" {
		return fmt.Errorf("%w: volume id is required", ErrInvalidArgument)
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketVolumes)
		var existing Volume
		if err := getJSON(b, []byte(vol.ID), &existing); err != nil {
			return err
		}
		return putJSON(b, []byte(vol.ID), vol)
	})
}

// SetVolumeAttachmentState transitions a Volume's AttachmentState while preventing conflicting attachments.
func (s *BoltStore) SetVolumeAttachmentState(id string, state AttachmentState) (*Volume, error) {
	var vol Volume
	err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketVolumes)
		if err := getJSON(b, []byte(id), &vol); err != nil {
			return err
		}
		if state != AttachmentStateUnattached &&
			vol.AttachmentState != AttachmentStateUnattached &&
			vol.AttachmentState != state {
			return fmt.Errorf("%w: volume %s is currently %s, cannot transition to %s",
				ErrAttachmentConflict, id, vol.AttachmentState, state)
		}
		vol.AttachmentState = state
		return putJSON(b, []byte(id), &vol)
	})
	if err != nil {
		return nil, err
	}
	return &vol, nil
}

// ListVolumes returns all Volumes, optionally filtered by PoolID.
func (s *BoltStore) ListVolumes(poolID string) ([]*Volume, error) {
	var vols []*Volume
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketVolumes)
		return b.ForEach(func(_, v []byte) error {
			var vol Volume
			if err := json.Unmarshal(v, &vol); err != nil {
				return err
			}
			if poolID == "" || vol.PoolID == poolID {
				vols = append(vols, &vol)
			}
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(vols, func(i, j int) bool {
		return vols[i].CreatedAt.Before(vols[j].CreatedAt)
	})
	return vols, nil
}

// DeleteVolume deletes a Volume if it is unattached, has no active Exports, and has no Snapshots.
func (s *BoltStore) DeleteVolume(id string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		volsB := tx.Bucket(bucketVolumes)
		var vol Volume
		if err := getJSON(volsB, []byte(id), &vol); err != nil {
			return err
		}
		if vol.AttachmentState != AttachmentStateUnattached && vol.AttachmentState != "" {
			return fmt.Errorf("%w: volume %s has active attachment state %s", ErrInUse, id, vol.AttachmentState)
		}

		// Ensure no exports reference this volume.
		expsB := tx.Bucket(bucketExports)
		err := expsB.ForEach(func(_, v []byte) error {
			var exp Export
			if err := json.Unmarshal(v, &exp); err != nil {
				return err
			}
			if exp.VolumeID == id {
				return fmt.Errorf("%w: volume %s is exported by %s", ErrInUse, id, exp.ID)
			}
			return nil
		})
		if err != nil {
			return err
		}

		// Ensure no snapshots reference this volume.
		snapsB := tx.Bucket(bucketSnapshots)
		err = snapsB.ForEach(func(_, v []byte) error {
			var snap Snapshot
			if err := json.Unmarshal(v, &snap); err != nil {
				return err
			}
			if snap.VolumeID == id {
				return fmt.Errorf("%w: volume %s has snapshot %s", ErrInUse, id, snap.ID)
			}
			return nil
		})
		if err != nil {
			return err
		}

		return volsB.Delete([]byte(id))
	})
}

// CreateSnapshot creates a point-in-time Snapshot of a Volume and automatically records
// its parent snapshot lineage and monotonic sequence number for Changed Block Tracking (CBT).
func (s *BoltStore) CreateSnapshot(snap *Snapshot) error {
	if snap == nil || snap.ID == "" || snap.Name == "" || snap.VolumeID == "" {
		return fmt.Errorf("%w: snapshot id, name, and volume_id are required", ErrInvalidArgument)
	}
	cp := *snap
	if cp.CreatedAt.IsZero() {
		cp.CreatedAt = time.Now().UTC()
	}

	return s.db.Update(func(tx *bolt.Tx) error {
		var vol Volume
		if err := getJSON(tx.Bucket(bucketVolumes), []byte(cp.VolumeID), &vol); err != nil {
			return fmt.Errorf("volume %s: %w", cp.VolumeID, err)
		}
		var pool Pool
		if err := getJSON(tx.Bucket(bucketPools), []byte(vol.PoolID), &pool); err != nil {
			return fmt.Errorf("pool %s: %w", vol.PoolID, err)
		}

		cp.PoolID = vol.PoolID
		if cp.SizeBytes == 0 {
			cp.SizeBytes = vol.SizeBytes
		}
		if cp.LvolBdevName == "" {
			cp.LvolBdevName = fmt.Sprintf("%s/%s", pool.LVSName, cp.Name)
		}

		snapsB := tx.Bucket(bucketSnapshots)
		if snapsB.Get([]byte(cp.ID)) != nil {
			return fmt.Errorf("%w: snapshot id %s", ErrAlreadyExists, cp.ID)
		}

		// Discover existing snapshots for this volume to validate name uniqueness and build CBT lineage.
		var latestSnap *Snapshot
		err := snapsB.ForEach(func(_, v []byte) error {
			var existing Snapshot
			if err := json.Unmarshal(v, &existing); err != nil {
				return err
			}
			if existing.VolumeID == cp.VolumeID {
				if existing.Name == cp.Name {
					return fmt.Errorf("%w: snapshot %s for volume %s", ErrAlreadyExists, cp.Name, cp.VolumeID)
				}
				if latestSnap == nil || existing.SequenceNumber > latestSnap.SequenceNumber {
					sCopy := existing
					latestSnap = &sCopy
				}
			}
			return nil
		})
		if err != nil {
			return err
		}

		if latestSnap != nil {
			if cp.ParentSnapshotID == "" {
				cp.ParentSnapshotID = latestSnap.ID
			}
			cp.SequenceNumber = latestSnap.SequenceNumber + 1
		} else {
			cp.SequenceNumber = 1
		}

		if err := putJSON(snapsB, []byte(cp.ID), &cp); err != nil {
			return err
		}
		*snap = cp
		return nil
	})
}

// GetSnapshot retrieves a Snapshot by ID.
func (s *BoltStore) GetSnapshot(id string) (*Snapshot, error) {
	var snap Snapshot
	err := s.db.View(func(tx *bolt.Tx) error {
		return getJSON(tx.Bucket(bucketSnapshots), []byte(id), &snap)
	})
	if err != nil {
		return nil, err
	}
	return &snap, nil
}

// GetSnapshotByName retrieves a Snapshot by VolumeID (optional if empty) and Name.
func (s *BoltStore) GetSnapshotByName(volumeID, name string) (*Snapshot, error) {
	var found *Snapshot
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketSnapshots)
		return b.ForEach(func(_, v []byte) error {
			var snap Snapshot
			if err := json.Unmarshal(v, &snap); err != nil {
				return err
			}
			if snap.Name == name && (volumeID == "" || snap.VolumeID == volumeID) {
				found = &snap
			}
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	if found == nil {
		return nil, ErrNotFound
	}
	return found, nil
}

// ListSnapshots returns Snapshots filtered by PoolID and/or VolumeID, ordered by SequenceNumber.
func (s *BoltStore) ListSnapshots(poolID, volumeID string) ([]*Snapshot, error) {
	var snaps []*Snapshot
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketSnapshots)
		return b.ForEach(func(_, v []byte) error {
			var snap Snapshot
			if err := json.Unmarshal(v, &snap); err != nil {
				return err
			}
			if (poolID == "" || snap.PoolID == poolID) && (volumeID == "" || snap.VolumeID == volumeID) {
				snaps = append(snaps, &snap)
			}
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(snaps, func(i, j int) bool {
		if snaps[i].VolumeID == snaps[j].VolumeID {
			return snaps[i].SequenceNumber < snaps[j].SequenceNumber
		}
		return snaps[i].CreatedAt.Before(snaps[j].CreatedAt)
	})
	return snaps, nil
}

// DeleteSnapshot deletes a Snapshot if no cloned Volume depends on it, and stitches
// any child Snapshot's ParentSnapshotID to preserve CBT lineage continuity.
func (s *BoltStore) DeleteSnapshot(id string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		snapsB := tx.Bucket(bucketSnapshots)
		var snap Snapshot
		if err := getJSON(snapsB, []byte(id), &snap); err != nil {
			return err
		}

		// Ensure no cloned volume depends on this snapshot.
		volsB := tx.Bucket(bucketVolumes)
		err := volsB.ForEach(func(_, v []byte) error {
			var vol Volume
			if err := json.Unmarshal(v, &vol); err != nil {
				return err
			}
			if vol.ParentSnapshotID == id {
				return fmt.Errorf("%w: snapshot %s is parent of cloned volume %s", ErrInUse, id, vol.ID)
			}
			return nil
		})
		if err != nil {
			return err
		}

		// Stitch child snapshots whose ParentSnapshotID points to this snapshot.
		var childrenToUpdate []*Snapshot
		err = snapsB.ForEach(func(_, v []byte) error {
			var s Snapshot
			if err := json.Unmarshal(v, &s); err != nil {
				return err
			}
			if s.ParentSnapshotID == id {
				s.ParentSnapshotID = snap.ParentSnapshotID
				childrenToUpdate = append(childrenToUpdate, &s)
			}
			return nil
		})
		if err != nil {
			return err
		}
		for _, child := range childrenToUpdate {
			if err := putJSON(snapsB, []byte(child.ID), child); err != nil {
				return err
			}
		}

		return snapsB.Delete([]byte(id))
	})
}

// CreateVolumeGroupSnapshot persists a consistent group of snapshots.
func (s *BoltStore) CreateVolumeGroupSnapshot(vgs *VolumeGroupSnapshot) error {
	if vgs == nil || vgs.ID == "" || vgs.Name == "" || len(vgs.VolumeIDs) == 0 {
		return fmt.Errorf("%w: group snapshot id, name, and volume_ids are required", ErrInvalidArgument)
	}
	cp := *vgs
	cp.VolumeIDs = append([]string(nil), vgs.VolumeIDs...)
	cp.SnapshotIDs = append([]string(nil), vgs.SnapshotIDs...)
	if cp.CreatedAt.IsZero() {
		cp.CreatedAt = time.Now().UTC()
	}

	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketVolumeGroupSnapshots)
		if b.Get([]byte(cp.ID)) != nil {
			return fmt.Errorf("%w: volume group snapshot id %s", ErrAlreadyExists, cp.ID)
		}
		err := b.ForEach(func(_, v []byte) error {
			var existing VolumeGroupSnapshot
			if err := json.Unmarshal(v, &existing); err != nil {
				return err
			}
			if existing.Name == cp.Name {
				return fmt.Errorf("%w: volume group snapshot %s", ErrAlreadyExists, cp.Name)
			}
			return nil
		})
		if err != nil {
			return err
		}
		for _, snapID := range cp.SnapshotIDs {
			var snap Snapshot
			if err := getJSON(tx.Bucket(bucketSnapshots), []byte(snapID), &snap); err != nil {
				return fmt.Errorf("snapshot %s: %w", snapID, err)
			}
		}
		if err := putJSON(b, []byte(cp.ID), &cp); err != nil {
			return err
		}
		*vgs = cp
		return nil
	})
}

// GetVolumeGroupSnapshot retrieves a VolumeGroupSnapshot by ID.
func (s *BoltStore) GetVolumeGroupSnapshot(id string) (*VolumeGroupSnapshot, error) {
	var vgs VolumeGroupSnapshot
	err := s.db.View(func(tx *bolt.Tx) error {
		return getJSON(tx.Bucket(bucketVolumeGroupSnapshots), []byte(id), &vgs)
	})
	if err != nil {
		return nil, err
	}
	return &vgs, nil
}

// GetVolumeGroupSnapshotByName retrieves a VolumeGroupSnapshot by Name.
func (s *BoltStore) GetVolumeGroupSnapshotByName(name string) (*VolumeGroupSnapshot, error) {
	var found *VolumeGroupSnapshot
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketVolumeGroupSnapshots).ForEach(func(_, v []byte) error {
			var vgs VolumeGroupSnapshot
			if err := json.Unmarshal(v, &vgs); err != nil {
				return err
			}
			if vgs.Name == name {
				cp := vgs
				found = &cp
				return nil
			}
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	if found == nil {
		return nil, fmt.Errorf("%w: volume group snapshot %s", ErrNotFound, name)
	}
	return found, nil
}

// ListVolumeGroupSnapshots returns all VolumeGroupSnapshots ordered by CreatedAt.
func (s *BoltStore) ListVolumeGroupSnapshots() ([]*VolumeGroupSnapshot, error) {
	var out []*VolumeGroupSnapshot
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketVolumeGroupSnapshots).ForEach(func(_, v []byte) error {
			var vgs VolumeGroupSnapshot
			if err := json.Unmarshal(v, &vgs); err != nil {
				return err
			}
			out = append(out, &vgs)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, nil
}

// DeleteVolumeGroupSnapshot removes a VolumeGroupSnapshot record by ID.
func (s *BoltStore) DeleteVolumeGroupSnapshot(id string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketVolumeGroupSnapshots)
		if b.Get([]byte(id)) == nil {
			return fmt.Errorf("%w: volume group snapshot %s", ErrNotFound, id)
		}
		return b.Delete([]byte(id))
	})
}

// CreatePortal creates a new network Portal listener definition.
func (s *BoltStore) CreatePortal(portal *Portal) error {
	if portal == nil || portal.ID == "" || portal.Name == "" || portal.IPAddress == "" || portal.Port == 0 {
		return fmt.Errorf("%w: portal id, name, ip_address, and port are required", ErrInvalidArgument)
	}
	cp := *portal
	if cp.CreatedAt.IsZero() {
		cp.CreatedAt = time.Now().UTC()
	}

	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketPortals)
		if b.Get([]byte(cp.ID)) != nil {
			return fmt.Errorf("%w: portal id %s", ErrAlreadyExists, cp.ID)
		}
		err := b.ForEach(func(_, v []byte) error {
			var existing Portal
			if err := json.Unmarshal(v, &existing); err != nil {
				return err
			}
			if existing.Name == cp.Name {
				return fmt.Errorf("%w: portal name %s", ErrAlreadyExists, cp.Name)
			}
			if existing.Protocol == cp.Protocol && existing.IPAddress == cp.IPAddress && existing.Port == cp.Port {
				return fmt.Errorf("%w: portal %s:%d (%s)", ErrAlreadyExists, cp.IPAddress, cp.Port, cp.Protocol)
			}
			return nil
		})
		if err != nil {
			return err
		}
		if err := putJSON(b, []byte(cp.ID), &cp); err != nil {
			return err
		}
		*portal = cp
		return nil
	})
}

// GetPortal retrieves a Portal by ID.
func (s *BoltStore) GetPortal(id string) (*Portal, error) {
	var portal Portal
	err := s.db.View(func(tx *bolt.Tx) error {
		return getJSON(tx.Bucket(bucketPortals), []byte(id), &portal)
	})
	if err != nil {
		return nil, err
	}
	return &portal, nil
}

// GetPortalByName retrieves a Portal by Name.
func (s *BoltStore) GetPortalByName(name string) (*Portal, error) {
	var found *Portal
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketPortals)
		return b.ForEach(func(_, v []byte) error {
			var p Portal
			if err := json.Unmarshal(v, &p); err != nil {
				return err
			}
			if p.Name == name {
				found = &p
			}
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	if found == nil {
		return nil, ErrNotFound
	}
	return found, nil
}

// ListPortals returns all Portals, optionally filtered by protocol.
func (s *BoltStore) ListPortals(protocolFilter ExportProtocol) ([]*Portal, error) {
	var portals []*Portal
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketPortals)
		return b.ForEach(func(_, v []byte) error {
			var p Portal
			if err := json.Unmarshal(v, &p); err != nil {
				return err
			}
			if protocolFilter == "" || protocolFilter == ExportProtocolUnspecified || p.Protocol == protocolFilter {
				portals = append(portals, &p)
			}
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(portals, func(i, j int) bool {
		return portals[i].CreatedAt.Before(portals[j].CreatedAt)
	})
	return portals, nil
}

// DeletePortal deletes a Portal if no Export references it.
func (s *BoltStore) DeletePortal(id string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		portalsB := tx.Bucket(bucketPortals)
		if portalsB.Get([]byte(id)) == nil {
			return ErrNotFound
		}

		expsB := tx.Bucket(bucketExports)
		err := expsB.ForEach(func(_, v []byte) error {
			var exp Export
			if err := json.Unmarshal(v, &exp); err != nil {
				return err
			}
			if slices.Contains(exp.PortalIDs, id) {
				return fmt.Errorf("%w: portal %s is used by export %s", ErrInUse, id, exp.ID)
			}
			return nil
		})
		if err != nil {
			return err
		}

		return portalsB.Delete([]byte(id))
	})
}

// CreateHost registers a SAN initiator Host record.
func (s *BoltStore) CreateHost(host *Host) error {
	if host == nil || host.ID == "" || host.Name == "" {
		return fmt.Errorf("%w: host id and name are required", ErrInvalidArgument)
	}
	cp := *host
	if cp.CreatedAt.IsZero() {
		cp.CreatedAt = time.Now().UTC()
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketHosts)
		if b.Get([]byte(cp.ID)) != nil {
			return fmt.Errorf("%w: host id %s", ErrAlreadyExists, cp.ID)
		}
		err := b.ForEach(func(_, v []byte) error {
			var existing Host
			if err := json.Unmarshal(v, &existing); err != nil {
				return err
			}
			if existing.Name == cp.Name {
				return fmt.Errorf("%w: host name %s", ErrAlreadyExists, cp.Name)
			}
			return nil
		})
		if err != nil {
			return err
		}
		if err := putJSON(b, []byte(cp.ID), &cp); err != nil {
			return err
		}
		*host = cp
		return nil
	})
}

// GetHost retrieves a Host by ID.
func (s *BoltStore) GetHost(id string) (*Host, error) {
	var host Host
	err := s.db.View(func(tx *bolt.Tx) error {
		return getJSON(tx.Bucket(bucketHosts), []byte(id), &host)
	})
	if err != nil {
		return nil, err
	}
	return &host, nil
}

// GetHostByName retrieves a Host by Name.
func (s *BoltStore) GetHostByName(name string) (*Host, error) {
	var found *Host
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketHosts)
		return b.ForEach(func(_, v []byte) error {
			var h Host
			if err := json.Unmarshal(v, &h); err != nil {
				return err
			}
			if h.Name == name {
				found = &h
			}
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	if found == nil {
		return nil, ErrNotFound
	}
	return found, nil
}

// ListHosts returns all registered Hosts ordered by CreatedAt.
func (s *BoltStore) ListHosts() ([]*Host, error) {
	var hosts []*Host
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketHosts)
		return b.ForEach(func(_, v []byte) error {
			var h Host
			if err := json.Unmarshal(v, &h); err != nil {
				return err
			}
			hosts = append(hosts, &h)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(hosts, func(i, j int) bool {
		return hosts[i].CreatedAt.Before(hosts[j].CreatedAt)
	})
	return hosts, nil
}

// DeleteHost deletes a Host if no InitiatorGroup references it.
func (s *BoltStore) DeleteHost(id string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		hostsB := tx.Bucket(bucketHosts)
		if hostsB.Get([]byte(id)) == nil {
			return ErrNotFound
		}
		groupsB := tx.Bucket(bucketInitiatorGroups)
		err := groupsB.ForEach(func(_, v []byte) error {
			var g InitiatorGroup
			if err := json.Unmarshal(v, &g); err != nil {
				return err
			}
			if slices.Contains(g.HostIDs, id) {
				return fmt.Errorf("%w: host %s is used by initiator group %s", ErrInUse, id, g.ID)
			}
			return nil
		})
		if err != nil {
			return err
		}
		return hostsB.Delete([]byte(id))
	})
}

// CreateInitiatorGroup creates a host InitiatorGroup ACL record.
func (s *BoltStore) CreateInitiatorGroup(group *InitiatorGroup) error {
	if group == nil || group.ID == "" || group.Name == "" {
		return fmt.Errorf("%w: initiator group id and name are required", ErrInvalidArgument)
	}
	cp := *group
	cp.HostIDs = slices.Clone(group.HostIDs)
	if cp.CreatedAt.IsZero() {
		cp.CreatedAt = time.Now().UTC()
	}

	return s.db.Update(func(tx *bolt.Tx) error {
		hostsB := tx.Bucket(bucketHosts)
		for _, hid := range cp.HostIDs {
			if hostsB.Get([]byte(hid)) == nil {
				return fmt.Errorf("%w: host %s not found", ErrNotFound, hid)
			}
		}
		b := tx.Bucket(bucketInitiatorGroups)
		if b.Get([]byte(cp.ID)) != nil {
			return fmt.Errorf("%w: initiator group id %s", ErrAlreadyExists, cp.ID)
		}
		err := b.ForEach(func(_, v []byte) error {
			var existing InitiatorGroup
			if err := json.Unmarshal(v, &existing); err != nil {
				return err
			}
			if existing.Name == cp.Name {
				return fmt.Errorf("%w: initiator group name %s", ErrAlreadyExists, cp.Name)
			}
			return nil
		})
		if err != nil {
			return err
		}
		if err := putJSON(b, []byte(cp.ID), &cp); err != nil {
			return err
		}
		*group = cp
		return nil
	})
}

// GetInitiatorGroup retrieves an InitiatorGroup by ID.
func (s *BoltStore) GetInitiatorGroup(id string) (*InitiatorGroup, error) {
	var group InitiatorGroup
	err := s.db.View(func(tx *bolt.Tx) error {
		return getJSON(tx.Bucket(bucketInitiatorGroups), []byte(id), &group)
	})
	if err != nil {
		return nil, err
	}
	return &group, nil
}

// GetInitiatorGroupByName retrieves an InitiatorGroup by Name.
func (s *BoltStore) GetInitiatorGroupByName(name string) (*InitiatorGroup, error) {
	var found *InitiatorGroup
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketInitiatorGroups)
		return b.ForEach(func(_, v []byte) error {
			var g InitiatorGroup
			if err := json.Unmarshal(v, &g); err != nil {
				return err
			}
			if g.Name == name {
				found = &g
			}
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	if found == nil {
		return nil, ErrNotFound
	}
	return found, nil
}

// ListInitiatorGroups returns all InitiatorGroups ordered by CreatedAt.
func (s *BoltStore) ListInitiatorGroups() ([]*InitiatorGroup, error) {
	var groups []*InitiatorGroup
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketInitiatorGroups)
		return b.ForEach(func(_, v []byte) error {
			var g InitiatorGroup
			if err := json.Unmarshal(v, &g); err != nil {
				return err
			}
			groups = append(groups, &g)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(groups, func(i, j int) bool {
		return groups[i].CreatedAt.Before(groups[j].CreatedAt)
	})
	return groups, nil
}

// DeleteInitiatorGroup deletes an InitiatorGroup if no Export references it.
func (s *BoltStore) DeleteInitiatorGroup(id string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		groupsB := tx.Bucket(bucketInitiatorGroups)
		if groupsB.Get([]byte(id)) == nil {
			return ErrNotFound
		}

		expsB := tx.Bucket(bucketExports)
		err := expsB.ForEach(func(_, v []byte) error {
			var exp Export
			if err := json.Unmarshal(v, &exp); err != nil {
				return err
			}
			if slices.Contains(exp.InitiatorGroupIDs, id) {
				return fmt.Errorf("%w: initiator group %s is used by export %s", ErrInUse, id, exp.ID)
			}
			return nil
		})
		if err != nil {
			return err
		}

		return groupsB.Delete([]byte(id))
	})
}

// CreateExport creates a SAN Export and atomically transitions the backing Volume's
// AttachmentState to BLOCK_EXPORT (rejecting if the Volume is attached as a FILESYSTEM).
func (s *BoltStore) CreateExport(exp *Export) error {
	if exp == nil || exp.ID == "" || exp.Name == "" || exp.VolumeID == "" || exp.Protocol == "" {
		return fmt.Errorf("%w: export id, name, volume_id, and protocol are required", ErrInvalidArgument)
	}
	cp := *exp
	cp.PortalIDs = slices.Clone(exp.PortalIDs)
	cp.InitiatorGroupIDs = slices.Clone(exp.InitiatorGroupIDs)
	if cp.HealthStatus == "" || cp.HealthStatus == HealthStatusUnspecified {
		cp.HealthStatus = HealthStatusOnline
	}
	if (cp.Protocol == ExportProtocolNVMeoFTCP || cp.Protocol == ExportProtocolNVMeoFRDMA) &&
		(cp.AnaState == "" || cp.AnaState == AnaStateUnspecified) {
		cp.AnaState = AnaStateOptimized
	}
	if cp.CreatedAt.IsZero() {
		cp.CreatedAt = time.Now().UTC()
	}

	return s.db.Update(func(tx *bolt.Tx) error {
		volsB := tx.Bucket(bucketVolumes)
		var vol Volume
		if err := getJSON(volsB, []byte(cp.VolumeID), &vol); err != nil {
			return fmt.Errorf("volume %s: %w", cp.VolumeID, err)
		}
		if vol.AttachmentState == AttachmentStateFilesystem {
			return fmt.Errorf("%w: volume %s is attached to a local filesystem", ErrAttachmentConflict, cp.VolumeID)
		}

		// Validate all referenced Portals exist.
		portalsB := tx.Bucket(bucketPortals)
		for _, pid := range cp.PortalIDs {
			if portalsB.Get([]byte(pid)) == nil {
				return fmt.Errorf("portal %s: %w", pid, ErrNotFound)
			}
		}

		// Validate all referenced InitiatorGroups exist.
		groupsB := tx.Bucket(bucketInitiatorGroups)
		for _, gid := range cp.InitiatorGroupIDs {
			if groupsB.Get([]byte(gid)) == nil {
				return fmt.Errorf("initiator group %s: %w", gid, ErrNotFound)
			}
		}

		expsB := tx.Bucket(bucketExports)
		if expsB.Get([]byte(cp.ID)) != nil {
			return fmt.Errorf("%w: export id %s", ErrAlreadyExists, cp.ID)
		}
		err := expsB.ForEach(func(_, v []byte) error {
			var existing Export
			if err := json.Unmarshal(v, &existing); err != nil {
				return err
			}
			if existing.Name == cp.Name {
				return fmt.Errorf("%w: export name %s", ErrAlreadyExists, cp.Name)
			}
			return nil
		})
		if err != nil {
			return err
		}

		// Transition volume attachment state to BLOCK_EXPORT.
		if vol.AttachmentState != AttachmentStateBlockExport {
			vol.AttachmentState = AttachmentStateBlockExport
			if err := putJSON(volsB, []byte(vol.ID), &vol); err != nil {
				return err
			}
		}

		if err := putJSON(expsB, []byte(cp.ID), &cp); err != nil {
			return err
		}
		*exp = cp
		return nil
	})
}

// GetExport retrieves an Export by ID.
func (s *BoltStore) GetExport(id string) (*Export, error) {
	var exp Export
	err := s.db.View(func(tx *bolt.Tx) error {
		return getJSON(tx.Bucket(bucketExports), []byte(id), &exp)
	})
	if err != nil {
		return nil, err
	}
	return &exp, nil
}

// GetExportByName retrieves an Export by Name.
func (s *BoltStore) GetExportByName(name string) (*Export, error) {
	var found *Export
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketExports)
		return b.ForEach(func(_, v []byte) error {
			var exp Export
			if err := json.Unmarshal(v, &exp); err != nil {
				return err
			}
			if exp.Name == name {
				found = &exp
			}
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	if found == nil {
		return nil, ErrNotFound
	}
	return found, nil
}

// UpdateExport updates an existing Export.
func (s *BoltStore) UpdateExport(exp *Export) error {
	if exp == nil || exp.ID == "" {
		return fmt.Errorf("%w: export id is required", ErrInvalidArgument)
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketExports)
		var existing Export
		if err := getJSON(b, []byte(exp.ID), &existing); err != nil {
			return err
		}
		return putJSON(b, []byte(exp.ID), exp)
	})
}

// ListExports returns all Exports, optionally filtered by VolumeID and/or ExportProtocol.
func (s *BoltStore) ListExports(volumeID string, protocolFilter ExportProtocol) ([]*Export, error) {
	var exps []*Export
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketExports)
		return b.ForEach(func(_, v []byte) error {
			var exp Export
			if err := json.Unmarshal(v, &exp); err != nil {
				return err
			}
			if (volumeID == "" || exp.VolumeID == volumeID) &&
				(protocolFilter == "" || protocolFilter == ExportProtocolUnspecified || exp.Protocol == protocolFilter) {
				exps = append(exps, &exp)
			}
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(exps, func(i, j int) bool {
		return exps[i].CreatedAt.Before(exps[j].CreatedAt)
	})
	return exps, nil
}

// DeleteExport deletes an Export and transitions its backing Volume back to UNATTACHED
// if no other Exports remain for that Volume.
func (s *BoltStore) DeleteExport(id string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		expsB := tx.Bucket(bucketExports)
		var exp Export
		if err := getJSON(expsB, []byte(id), &exp); err != nil {
			return err
		}

		if err := expsB.Delete([]byte(id)); err != nil {
			return err
		}

		// Check whether any other exports still reference exp.VolumeID.
		stillExported := false
		err := expsB.ForEach(func(_, v []byte) error {
			var other Export
			if err := json.Unmarshal(v, &other); err != nil {
				return err
			}
			if other.VolumeID == exp.VolumeID {
				stillExported = true
			}
			return nil
		})
		if err != nil {
			return err
		}

		if !stillExported {
			volsB := tx.Bucket(bucketVolumes)
			var vol Volume
			if err := getJSON(volsB, []byte(exp.VolumeID), &vol); err == nil {
				if vol.AttachmentState == AttachmentStateBlockExport {
					vol.AttachmentState = AttachmentStateUnattached
					if err := putJSON(volsB, []byte(vol.ID), &vol); err != nil {
						return err
					}
				}
			}
		}

		return nil
	})
}

func putJSON(b *bolt.Bucket, key []byte, val any) error {
	data, err := json.Marshal(val)
	if err != nil {
		return fmt.Errorf("marshal json: %w", err)
	}
	return b.Put(key, data)
}

func getJSON(b *bolt.Bucket, key []byte, dest any) error {
	data := b.Get(key)
	if data == nil {
		return ErrNotFound
	}
	if err := json.Unmarshal(data, dest); err != nil {
		return fmt.Errorf("unmarshal json: %w", err)
	}
	return nil
}
