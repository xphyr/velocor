package store

import (
	"errors"
	"time"
)

var (
	// ErrNotFound is returned when a requested resource does not exist in the store.
	ErrNotFound = errors.New("resource not found")
	// ErrAlreadyExists is returned when creating a resource that violates an ID or name uniqueness constraint.
	ErrAlreadyExists = errors.New("resource already exists")
	// ErrInUse is returned when attempting to delete or release a resource that has active dependents.
	ErrInUse = errors.New("resource is in use by dependent resources")
	// ErrInvalidArgument is returned when required resource fields are missing or invalid.
	ErrInvalidArgument = errors.New("invalid argument")
	// ErrBootDeviceProtected is returned when attempting to claim or modify a disk marked as a boot device.
	ErrBootDeviceProtected = errors.New("boot device is protected and cannot be claimed or modified")
	// ErrSkipWrite may be returned by a Mutate* callback to indicate that nothing changed
	// and the record should not be rewritten. Mutate* then returns the current record and nil.
	ErrSkipWrite = errors.New("skip write")
	// ErrAttachmentConflict is returned when a volume's attachment state conflicts with the requested operation.
	ErrAttachmentConflict = errors.New("volume attachment state conflict")
)

// Store defines the transactional persistence interface for velod desired and observed state.
type Store interface {
	// Lifecycle
	Close() error

	// System & Auth
	GetSystemConfig() (*SystemConfig, error)
	SaveSystemConfig(cfg *SystemConfig) error
	GetAdminUser(username string) (*AdminUser, error)
	SaveAdminUser(user *AdminUser) error
	ListUsers() ([]*AdminUser, error)
	DeleteUser(username string) error
	CreateApiToken(token *ApiToken) error
	GetApiToken(id string) (*ApiToken, error)
	UpdateApiTokenLastUsed(id string, lastUsed time.Time) error
	ListApiTokens() ([]*ApiToken, error)
	DeleteApiToken(id string) error

	// Disks
	UpsertDisk(disk *Disk) error
	SyncDiscoveredDisk(disk *Disk) error
	GetDisk(id string) (*Disk, error)
	ListDisks() ([]*Disk, error)
	// MutateDisk atomically applies fn to the current Disk record inside one transaction.
	MutateDisk(id string, fn func(*Disk) error) (*Disk, error)
	ClaimDisk(id string, driverState DiskDriverState, spdkBdevName string) (*Disk, error)
	ReleaseDisk(id string) (*Disk, error)
	DeleteDisk(id string) error

	// Pools
	CreatePool(pool *Pool) error
	GetPool(id string) (*Pool, error)
	GetPoolByName(name string) (*Pool, error)
	// MutatePool atomically applies fn to the current Pool record inside one transaction.
	MutatePool(id string, fn func(*Pool) error) (*Pool, error)
	// CheckPoolDeletable returns nil if DeletePool(id) would currently succeed.
	CheckPoolDeletable(id string) error
	ListPools() ([]*Pool, error)
	DeletePool(id string) error

	// Volumes
	CreateVolume(vol *Volume) error
	GetVolume(id string) (*Volume, error)
	GetVolumeByName(poolID, name string) (*Volume, error)
	// MutateVolume atomically applies fn to the current Volume record inside one transaction.
	MutateVolume(id string, fn func(*Volume) error) (*Volume, error)
	// CheckVolumeDeletable returns nil if DeleteVolume(id) would currently succeed.
	CheckVolumeDeletable(id string) error
	SetVolumeAttachmentState(id string, state AttachmentState) (*Volume, error)
	ListVolumes(poolID string) ([]*Volume, error)
	DeleteVolume(id string) error

	// Snapshots
	CreateSnapshot(snap *Snapshot) error
	GetSnapshot(id string) (*Snapshot, error)
	GetSnapshotByName(volumeID, name string) (*Snapshot, error)
	ListSnapshots(poolID, volumeID string) ([]*Snapshot, error)
	DeleteSnapshot(id string) error
	// CheckSnapshotDeletable returns nil if DeleteSnapshot(id) would currently succeed.
	CheckSnapshotDeletable(id string) error

	// Volume Group Snapshots (Consistency Groups)
	CreateVolumeGroupSnapshot(vgs *VolumeGroupSnapshot) error
	GetVolumeGroupSnapshot(id string) (*VolumeGroupSnapshot, error)
	GetVolumeGroupSnapshotByName(name string) (*VolumeGroupSnapshot, error)
	ListVolumeGroupSnapshots() ([]*VolumeGroupSnapshot, error)
	DeleteVolumeGroupSnapshot(id string) error

	// Portals
	CreatePortal(portal *Portal) error
	GetPortal(id string) (*Portal, error)
	GetPortalByName(name string) (*Portal, error)
	ListPortals(protocolFilter ExportProtocol) ([]*Portal, error)
	DeletePortal(id string) error

	// Hosts
	CreateHost(host *Host) error
	GetHost(id string) (*Host, error)
	GetHostByName(name string) (*Host, error)
	ListHosts() ([]*Host, error)
	DeleteHost(id string) error

	// InitiatorGroups
	CreateInitiatorGroup(group *InitiatorGroup) error
	GetInitiatorGroup(id string) (*InitiatorGroup, error)
	GetInitiatorGroupByName(name string) (*InitiatorGroup, error)
	ListInitiatorGroups() ([]*InitiatorGroup, error)
	DeleteInitiatorGroup(id string) error
	// MutateInitiatorGroup atomically applies fn to the InitiatorGroup record.
	MutateInitiatorGroup(id string, fn func(*InitiatorGroup) error) (*InitiatorGroup, error)

	// Exports
	CreateExport(exp *Export) error
	GetExport(id string) (*Export, error)
	GetExportByName(name string) (*Export, error)
	// MutateExport atomically applies fn to the current Export record inside one transaction.
	MutateExport(id string, fn func(*Export) error) (*Export, error)
	ListExports(volumeID string, protocolFilter ExportProtocol) ([]*Export, error)
	DeleteExport(id string) error
}
