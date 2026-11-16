package store

import (
	"encoding/json"
	"time"
)

// HealthStatus represents the operational health of a resource.
type HealthStatus string

const (
	HealthStatusUnspecified HealthStatus = "UNSPECIFIED"
	HealthStatusOnline      HealthStatus = "ONLINE"
	HealthStatusDegraded    HealthStatus = "DEGRADED"
	HealthStatusOffline     HealthStatus = "OFFLINE"
	HealthStatusFaulted     HealthStatus = "FAULTED"
	HealthStatusReconciling HealthStatus = "RECONCILING"
)

// DiskType categorizes physical flash media tiers.
type DiskType string

const (
	DiskTypeUnspecified DiskType = "UNSPECIFIED"
	DiskTypeNVMe        DiskType = "NVME"
	DiskTypeSSD         DiskType = "SSD"
)

// DiskDriverState indicates kernel vs SPDK user-space driver binding.
type DiskDriverState string

const (
	DiskDriverStateUnspecified   DiskDriverState = "UNSPECIFIED"
	DiskDriverStateKernel        DiskDriverState = "KERNEL"
	DiskDriverStateVfioPCI       DiskDriverState = "VFIO_PCI"
	DiskDriverStateUioPCIGeneric DiskDriverState = "UIO_PCI_GENERIC"
	DiskDriverStateUringClaimed  DiskDriverState = "URING_CLAIMED"
	DiskDriverStateAIOClaimed    DiskDriverState = "AIO_CLAIMED"
)

// RaidLevel enumerates SPDK bdev_raid levels supported for Pools.
type RaidLevel string

const (
	RaidLevelUnspecified RaidLevel = "UNSPECIFIED"
	RaidLevel0           RaidLevel = "RAID0"
	RaidLevel1           RaidLevel = "RAID1"
	RaidLevel10          RaidLevel = "RAID10"
	RaidLevel5F          RaidLevel = "RAID5F"
)

// CompressionAlgorithm defines supported SPDK bdev_compress algorithms.
type CompressionAlgorithm string

const (
	CompressionAlgorithmUnspecified CompressionAlgorithm = "UNSPECIFIED"
	CompressionAlgorithmLZ4         CompressionAlgorithm = "LZ4"
	CompressionAlgorithmDeflate     CompressionAlgorithm = "DEFLATE"
)

// AttachmentState enforces exclusive ownership of a Volume between SAN exports
// and Phase 2 local NAS filesystem attachment (SPDK ublk).
type AttachmentState string

const (
	AttachmentStateUnspecified AttachmentState = "UNSPECIFIED"
	AttachmentStateUnattached  AttachmentState = "UNATTACHED"
	AttachmentStateBlockExport AttachmentState = "BLOCK_EXPORT"
	AttachmentStateFilesystem  AttachmentState = "FILESYSTEM"
)

// ExportProtocol specifies the SAN fabric transport used to expose a Volume.
type ExportProtocol string

const (
	ExportProtocolUnspecified ExportProtocol = "UNSPECIFIED"
	ExportProtocolNVMeoFTCP   ExportProtocol = "NVMEOF_TCP"
	ExportProtocolNVMeoFRDMA  ExportProtocol = "NVMEOF_RDMA"
	ExportProtocolISCSI       ExportProtocol = "ISCSI"
)

// AnaState defines Asymmetric Namespace Access state for NVMe-oF multipath failover.
type AnaState string

const (
	AnaStateUnspecified  AnaState = "UNSPECIFIED"
	AnaStateOptimized    AnaState = "OPTIMIZED"
	AnaStateNonOptimized AnaState = "NON_OPTIMIZED"
	AnaStateInaccessible AnaState = "INACCESSIBLE"
)

// Role defines RBAC permission tiers for human sessions and service tokens.
type Role string

const (
	RoleUnspecified    Role = "UNSPECIFIED"
	RoleAdmin          Role = "ADMIN"
	RoleCSIProvisioner Role = "CSI_PROVISIONER"
	RoleReadOnly       Role = "READONLY"
)

// DefaultClusterSizeBytes is the default SPDK lvol_store cluster size (64 KiB)
// used to optimize thin provisioning and Changed Block Tracking (CBT).
const DefaultClusterSizeBytes uint32 = 64 * 1024

// FilterPolicy defines stackable SPDK filter bdev layers (crypto / compression).
type FilterPolicy struct {
	EncryptionEnabled  bool                 `json:"encryption_enabled"`
	KeyID              string               `json:"key_id,omitempty"`
	CompressionEnabled bool                 `json:"compression_enabled"`
	CompressionAlgo    CompressionAlgorithm `json:"compression_algo,omitempty"`
}

// QosPolicy configures SPDK bdev rate limits (IOPS and bandwidth).
type QosPolicy struct {
	RwIosPerSec    uint64 `json:"rw_ios_per_sec"`
	RwMbytesPerSec uint64 `json:"rw_mbytes_per_sec"`
	RMbytesPerSec  uint64 `json:"r_mbytes_per_sec"`
	WMbytesPerSec  uint64 `json:"w_mbytes_per_sec"`
}

// SmartHealth captures NVMe/SSD S.M.A.R.T. health telemetry.
type SmartHealth struct {
	TemperatureCelsius             int32  `json:"temperature_celsius"`
	AvailableSparePercent          uint32 `json:"available_spare_percent"`
	AvailableSpareThresholdPercent uint32 `json:"available_spare_threshold_percent"`
	PercentageUsed                 uint32 `json:"percentage_used"`
	DataUnitsRead                  uint64 `json:"data_units_read"`
	DataUnitsWritten               uint64 `json:"data_units_written"`
	MediaErrors                    uint64 `json:"media_errors"`
	PowerOnHours                   uint64 `json:"power_on_hours"`
	UnsafeShutdowns                uint64 `json:"unsafe_shutdowns"`
	CriticalWarning                bool   `json:"critical_warning"`
}

// Disk represents a physical NVMe drive or SATA/SAS SSD on the node.
type Disk struct {
	ID               string          `json:"id"`
	Name             string          `json:"name"`
	Type             DiskType        `json:"type"`
	PCIAddress       string          `json:"pci_address,omitempty"`
	DevicePath       string          `json:"device_path,omitempty"`
	SerialNumber     string          `json:"serial_number"`
	Model            string          `json:"model"`
	FirmwareRevision string          `json:"firmware_revision"`
	NUMANode         int32           `json:"numa_node"`
	SizeBytes        uint64          `json:"size_bytes"`
	BlockSize        uint32          `json:"block_size"`
	DriverState      DiskDriverState `json:"driver_state"`
	IsBootDevice     bool            `json:"is_boot_device"`
	Claimed          bool            `json:"claimed"`
	PoolID           string          `json:"pool_id,omitempty"`
	SPDKBdevName     string          `json:"spdk_bdev_name,omitempty"`
	HealthStatus     HealthStatus    `json:"health_status"`
	Smart            SmartHealth     `json:"smart"`
	// WriteCache is what the hardware reports (refreshed by every scan; the last known value is
	// kept while SPDK owns the device and the kernel can't see it).
	WriteCache WriteCacheState `json:"write_cache"`
	// PowerLossProtected is declared by an operator: there is no standard field for it, and
	// drives with power-loss protection usually still report a volatile write cache.
	PowerLossProtected bool `json:"power_loss_protected"`
	// WriteCachePolicy is the operator's choice; DISABLED makes velod switch the drive's
	// volatile write cache off and keep it off.
	WriteCachePolicy WriteCachePolicy `json:"write_cache_policy,omitempty"`
	UpdatedAt        time.Time        `json:"updated_at"`
}

// WriteCacheState is a disk's volatile write cache as reported by the hardware.
type WriteCacheState string

const (
	WriteCacheUnknown      WriteCacheState = ""              // not determined (yet)
	WriteCacheWriteBack    WriteCacheState = "WRITE_BACK"    // volatile cache present and in use
	WriteCacheWriteThrough WriteCacheState = "WRITE_THROUGH" // no volatile cache in use
)

// WriteCachePolicy is what Velocor does with a disk's volatile write cache.
type WriteCachePolicy string

const (
	WriteCachePolicyLeave    WriteCachePolicy = ""         // leave the drive's setting alone
	WriteCachePolicyDisabled WriteCachePolicy = "DISABLED" // turn it off and keep it off
)

// Durability says whether a write the drive has acknowledged survives a power cut.
type Durability string

const (
	DurabilityDurable Durability = "DURABLE"
	// DurabilityAtRisk: the drive caches writes in volatile memory and isn't declared
	// power-loss protected. SPDK v26.09 never passes FLUSH through lvols and RAID (TODO P0-4,
	// L-1), so acknowledged writes can be lost on power failure.
	DurabilityAtRisk  Durability = "AT_RISK"
	DurabilityUnknown Durability = "UNKNOWN"
)

// Durability derives the disk's durability from its write cache and the operator's
// power-loss-protection declaration.
func (d *Disk) Durability() Durability {
	switch {
	case d.PowerLossProtected, d.WriteCache == WriteCacheWriteThrough:
		return DurabilityDurable
	case d.WriteCache == WriteCacheWriteBack:
		return DurabilityAtRisk
	default:
		return DurabilityUnknown
	}
}

// MarshalJSON adds the derived "durability" to every JSON form of a Disk (API responses);
// it's recomputed on output, so a stored copy is never trusted.
func (d Disk) MarshalJSON() ([]byte, error) {
	type plain Disk
	return json.Marshal(struct {
		plain
		Durability Durability `json:"durability"`
	}{plain(d), d.Durability()})
}

// Pool aggregates one or more claimed Disks into an SPDK RAID bdev + lvol_store.
type Pool struct {
	ID                string       `json:"id"`
	Name              string       `json:"name"`
	RaidLevel         RaidLevel    `json:"raid_level"`
	DiskIDs           []string     `json:"disk_ids"`
	StripSizeKB       uint32       `json:"strip_size_kb"`
	ClusterSizeBytes  uint32       `json:"cluster_size_bytes"`
	BaseBdevName      string       `json:"base_bdev_name"`
	EffectiveBdevName string       `json:"effective_bdev_name"`
	LVSUUID           string       `json:"lvs_uuid"`
	LVSName           string       `json:"lvs_name"`
	TotalBytes        uint64       `json:"total_bytes"`
	FreeBytes         uint64       `json:"free_bytes"`
	AllocatedBytes    uint64       `json:"allocated_bytes"`
	FilterPolicy      FilterPolicy `json:"filter_policy"`
	HealthStatus      HealthStatus `json:"health_status"`
	StatusMessage     string       `json:"status_message,omitempty"`
	CreatedAt         time.Time    `json:"created_at"`
}

// Volume represents a logical volume (SPDK bdev_lvol) inside a Pool.
type Volume struct {
	ID                string            `json:"id"`
	Name              string            `json:"name"`
	PoolID            string            `json:"pool_id"`
	UUID              string            `json:"uuid"`
	LvolBdevName      string            `json:"lvol_bdev_name"`
	EffectiveBdevName string            `json:"effective_bdev_name"`
	SizeBytes         uint64            `json:"size_bytes"`
	AllocatedBytes    uint64            `json:"allocated_bytes"`
	ThinProvision     bool              `json:"thin_provision"`
	ReadOnly          bool              `json:"read_only"`
	ParentSnapshotID  string            `json:"parent_snapshot_id,omitempty"`
	FilterPolicy      FilterPolicy      `json:"filter_policy"`
	QosPolicy         QosPolicy         `json:"qos_policy"`
	AttachmentState   AttachmentState   `json:"attachment_state"`
	HealthStatus      HealthStatus      `json:"health_status"`
	StatusMessage     string            `json:"status_message,omitempty"`
	Labels            map[string]string `json:"labels,omitempty"`
	CreatedAt         time.Time         `json:"created_at"`
}

// Snapshot represents an immutable point-in-time SPDK bdev_lvol_snapshot.
type Snapshot struct {
	ID               string    `json:"id"`
	Name             string    `json:"name"`
	VolumeID         string    `json:"volume_id"`
	PoolID           string    `json:"pool_id"`
	UUID             string    `json:"uuid"`
	LvolBdevName     string    `json:"lvol_bdev_name"`
	SizeBytes        uint64    `json:"size_bytes"`
	AllocatedBytes   uint64    `json:"allocated_bytes"`
	ParentSnapshotID string    `json:"parent_snapshot_id,omitempty"`
	SequenceNumber   uint64    `json:"sequence_number"`
	CreatedAt        time.Time `json:"created_at"`
}

// VolumeGroupSnapshot represents a point-in-time consistent group of snapshots across multiple volumes.
type VolumeGroupSnapshot struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	VolumeIDs   []string  `json:"volume_ids"`
	SnapshotIDs []string  `json:"snapshot_ids"`
	ReadyToUse  bool      `json:"ready_to_use"`
	CreatedAt   time.Time `json:"created_at"`
}

// Portal defines an IP/Port listener endpoint for NVMe-oF or iSCSI traffic.
type Portal struct {
	ID        string         `json:"id"`
	Name      string         `json:"name"`
	Protocol  ExportProtocol `json:"protocol"`
	IPAddress string         `json:"ip_address"`
	Port      uint32         `json:"port"`
	// IscsiPgTag is the SPDK iSCSI portal group tag for ISCSI portals (unique, allocated by the store).
	IscsiPgTag int       `json:"iscsi_pg_tag,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

// Host represents a registered SAN initiator host (ESXi, Linux, Kubernetes worker)
// with its NVMe-oF Host NQN and/or iSCSI Initiator IQN.
type Host struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	HostNQN      string    `json:"host_nqn,omitempty"`
	InitiatorIQN string    `json:"initiator_iqn,omitempty"`
	IPAddress    string    `json:"ip_address,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
}

// InitiatorGroup defines host ACLs (registered Host IDs, NVMe-oF Host NQNs, iSCSI Initiator IQNs, CIDRs, CHAP).
type InitiatorGroup struct {
	ID                        string    `json:"id"`
	Name                      string    `json:"name"`
	HostIDs                   []string  `json:"host_ids,omitempty"`
	HostNQNs                  []string  `json:"host_nqns,omitempty"`
	InitiatorIQNs             []string  `json:"initiator_iqns,omitempty"`
	AllowedCIDRs              []string  `json:"allowed_cidrs,omitempty"`
	CHAPUser                  string    `json:"chap_user,omitempty"`
	CHAPSecretEncrypted       []byte    `json:"chap_secret_encrypted,omitempty"`
	MutualCHAPUser            string    `json:"mutual_chap_user,omitempty"`
	MutualCHAPSecretEncrypted []byte    `json:"mutual_chap_secret_encrypted,omitempty"`
	CreatedAt                 time.Time `json:"created_at"`
}

// Export maps a Volume's EffectiveBdevName to an NVMe-oF Subsystem/Namespace or iSCSI Target/LUN.
type Export struct {
	ID                string         `json:"id"`
	Name              string         `json:"name"`
	VolumeID          string         `json:"volume_id"`
	Protocol          ExportProtocol `json:"protocol"`
	SubsystemNQN      string         `json:"subsystem_nqn,omitempty"`
	NamespaceID       uint32         `json:"namespace_id,omitempty"`
	AnaState          AnaState       `json:"ana_state,omitempty"`
	TargetIQN         string         `json:"target_iqn,omitempty"`
	LUNID             uint32         `json:"lun_id,omitempty"`
	PortalIDs         []string       `json:"portal_ids"`
	InitiatorGroupIDs []string       `json:"initiator_group_ids,omitempty"`
	AllowAnyHost      bool           `json:"allow_any_host"`
	// NodeID is the CSI node this export was published to (empty for manually created exports).
	NodeID string `json:"node_id,omitempty"`
	// IscsiIgTag is the SPDK iSCSI initiator group tag owned by this export (unique, allocated by the store).
	IscsiIgTag    int          `json:"iscsi_ig_tag,omitempty"`
	HealthStatus  HealthStatus `json:"health_status"`
	StatusMessage string       `json:"status_message,omitempty"`
	CreatedAt     time.Time    `json:"created_at"`
}

// AdminUser stores local user credentials and RBAC role.
type AdminUser struct {
	Username     string `json:"username"`
	PasswordHash string `json:"password_hash"`
	Role         Role   `json:"role,omitempty"`
	// SessionsValidAfter revokes every session JWT issued before it (password change,
	// role change, user deletion and re-creation).
	SessionsValidAfter time.Time `json:"sessions_valid_after,omitempty"`
	// MustChangePassword restricts the account to changing its password (bootstrap admin).
	MustChangePassword bool      `json:"must_change_password,omitempty"`
	CreatedAt          time.Time `json:"created_at,omitempty"`
	UpdatedAt          time.Time `json:"updated_at"`
}

// ApiToken represents a scoped Personal/Service Access Token (velo_pat_...).
type ApiToken struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"`
	TokenHash  string     `json:"token_hash"`
	Role       Role       `json:"role"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
}

// SystemConfig stores node-wide persistent cryptographic and identity metadata.
type SystemConfig struct {
	NodeID        string    `json:"node_id"`
	JWTSecret     []byte    `json:"jwt_secret"`
	MasterKeySalt []byte    `json:"master_key_salt"`
	InitializedAt time.Time `json:"initialized_at"`
}
