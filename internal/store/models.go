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
	// HealthStatusRebuilding: a pool is rebuilding a member; it serves I/O without full
	// redundancy until the rebuild completes.
	HealthStatusRebuilding HealthStatus = "REBUILDING"
	// HealthStatusUnavailable: a pool's RAID is waiting for a member that is missing (e.g. a
	// disk that failed while the appliance was down). It serves no I/O until the disk returns
	// or an operator starts it degraded.
	HealthStatusUnavailable HealthStatus = "UNAVAILABLE"
)

// MemberState is the state of one RAID slot of a pool.
type MemberState string

const (
	MemberStateActive     MemberState = "ACTIVE"     // in sync, serving I/O
	MemberStateRebuilding MemberState = "REBUILDING" // being rebuilt from the other members
	MemberStateFailed     MemberState = "FAILED"     // failed or removed while the pool ran; slot empty
	MemberStateMissing    MemberState = "MISSING"    // absent when the pool was assembled; slot empty
)

// PoolMember is one RAID slot of a pool. DiskID is the disk in the slot, or for an empty
// (FAILED/MISSING) slot the disk that was there, if known.
type PoolMember struct {
	Slot           int         `json:"slot"`
	DiskID         string      `json:"disk_id,omitempty"`
	State          MemberState `json:"state"`
	RebuildPercent uint32      `json:"rebuild_percent,omitempty"`
}

// Settings holds operator-adjustable, node-wide settings.
type Settings struct {
	// RebuildMaxMBps limits RAID rebuild bandwidth (SPDK applies it to every array); 0 means
	// unlimited.
	RebuildMaxMBps uint32 `json:"rebuild_max_mbps"`
	// VolumeRetentionHours is how long a deleted volume is kept (unexported, hidden, its data
	// intact) before the reconciler purges it; nil means DefaultVolumeRetention, 0 deletes at
	// once.
	VolumeRetentionHours *uint32 `json:"volume_retention_hours,omitempty"`
	// MonitoringEnabled runs the built-in Prometheus and Alertmanager (TODO.md P1-1).
	MonitoringEnabled bool `json:"monitoring_enabled,omitempty"`
	// OSUpgrade records the OS image velod last staged, so that after the reboot it can tell
	// whether the appliance actually booted it (TODO.md P1-7). nil: nothing pending.
	OSUpgrade *OSUpgradeRecord `json:"os_upgrade,omitempty"`
	// Alerting configures where the built-in Alertmanager sends alerts and how the shipped
	// rules are tuned. Its secrets stay in this database (and its backups), never in the pools'
	// metadata copies, and the API never returns them.
	Alerting AlertingSettings `json:"alerting,omitempty"`
}

// OSUpgradeRecord is an OS image staged (or a rollback queued) for the next boot.
type OSUpgradeRecord struct {
	Image    string    `json:"image"`
	Digest   string    `json:"digest"`
	StagedAt time.Time `json:"staged_at"`
	By       string    `json:"by"`
	// BootID is the kernel's boot ID when it was staged: a different one means the appliance
	// has rebooted since.
	BootID string `json:"boot_id"`
}

// AlertingSettings are the built-in Alertmanager's receivers and the rules' tuning.
type AlertingSettings struct {
	Email   *EmailReceiver   `json:"email,omitempty"`
	Webhook *WebhookReceiver `json:"webhook,omitempty"`
	// HeartbeatURL receives the always-firing watchdog alert every few minutes (a dead man's
	// switch service raises an alarm when it stops). Secret.
	HeartbeatURL string `json:"heartbeat_url,omitempty"`
	// Rules tunes shipped rules by alert name.
	Rules map[string]RuleSetting `json:"rules,omitempty"`
}

// EmailReceiver sends alerts by SMTP.
type EmailReceiver struct {
	To       []string `json:"to"`
	From     string   `json:"from"`
	SMTPHost string   `json:"smtp_host"`
	SMTPPort int      `json:"smtp_port"`
	Username string   `json:"username,omitempty"`
	Password string   `json:"password,omitempty"` // secret
	// RequireTLS refuses to send unless the server offers STARTTLS (port 465 uses TLS anyway).
	RequireTLS bool `json:"require_tls"`
	// MinSeverity is "warning" (default) or "critical".
	MinSeverity string `json:"min_severity,omitempty"`
}

// WebhookReceiver posts alerts in Alertmanager's webhook format.
type WebhookReceiver struct {
	URL         string `json:"url"` // secret: such URLs often embed a token
	MinSeverity string `json:"min_severity,omitempty"`
}

// RuleSetting tunes one shipped rule.
type RuleSetting struct {
	Disabled bool `json:"disabled,omitempty"`
	// For overrides how long the condition must hold ("5m", "1h"); empty keeps the default.
	For string `json:"for,omitempty"`
	// Threshold overrides the rule's tunable value, for the rules that have one.
	Threshold *float64 `json:"threshold,omitempty"`
}

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

// DefaultClusterSizeBytes is the default SPDK lvol_store cluster size (1 MiB).
//
// SPDK reserves one metadata page (4 KiB) per cluster, and after an unclean stop recovery reads
// all of them: with 64 KiB clusters a 500 GB pool carried ~30 GiB of metadata (6% of the pool)
// and took 16 minutes to recover on velocor-1, long enough for hosts to give up. 1 MiB keeps
// that to ~0.4% and about a minute, while thin allocation stays fine-grained.
const DefaultClusterSizeBytes uint32 = 1 << 20

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
	// BdevUUID is the UUID of the disk's SPDK base bdev when it differs from the one derived
	// from the disk ID: a pool member that came back under a new ID keeps the UUID its RAID
	// superblock knows it by (TODO.md P1-14). Empty: derived from the ID.
	BdevUUID     string       `json:"bdev_uuid,omitempty"`
	HealthStatus HealthStatus `json:"health_status"`
	Smart        SmartHealth  `json:"smart"`
	// WriteCache is what the hardware reports (refreshed by every scan; the last known value is
	// kept while SPDK owns the device and the kernel can't see it).
	WriteCache WriteCacheState `json:"write_cache"`
	// PowerLossProtected is declared by an operator: there is no standard field for it, and
	// drives with power-loss protection usually still report a volatile write cache.
	PowerLossProtected bool `json:"power_loss_protected"`
	// WriteCachePolicy is the operator's choice; DISABLED makes velod switch the drive's
	// volatile write cache off and keep it off.
	WriteCachePolicy WriteCachePolicy `json:"write_cache_policy,omitempty"`
	// WriteCacheError says why the DISABLED policy couldn't be applied (the drive refused);
	// the disk is reported once in the durability warnings instead of on every pass.
	WriteCacheError string    `json:"write_cache_error,omitempty"`
	UpdatedAt       time.Time `json:"updated_at"`
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
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	RaidLevel RaidLevel `json:"raid_level"`
	// DiskIDs are the disks in the pool's RAID slots (ACTIVE or REBUILDING), in slot order.
	DiskIDs []string `json:"disk_ids"`
	// Durability is the weakest of the member disks' (maintained by the reconciler): AT_RISK
	// when any member can lose acknowledged writes on power loss (TODO.md P0-4).
	Durability Durability `json:"durability,omitempty"`
	// Members is the state of every RAID slot (maintained by the reconciler from SPDK).
	Members []PoolMember `json:"members,omitempty"`
	// DetachedDiskIDs are disks that left the RAID (failed, removed or replaced) but stay
	// bound to the pool until an operator releases them, so they can't be reused by accident.
	DetachedDiskIDs   []string `json:"detached_disk_ids,omitempty"`
	StripSizeKB       uint32   `json:"strip_size_kb"`
	ClusterSizeBytes  uint32   `json:"cluster_size_bytes"`
	BaseBdevName      string   `json:"base_bdev_name"`
	EffectiveBdevName string   `json:"effective_bdev_name"`
	LVSUUID           string   `json:"lvs_uuid"`
	LVSName           string   `json:"lvs_name"`
	TotalBytes        uint64   `json:"total_bytes"`
	FreeBytes         uint64   `json:"free_bytes"`
	AllocatedBytes    uint64   `json:"allocated_bytes"`
	// Space accounting (P0-2), maintained by the reconciler: CommittedBytes is every volume's
	// full size plus what snapshots hold; AvailableBytes is how much more the pool accepts.
	SpacePolicy    SpacePolicy `json:"space_policy"`
	CommittedBytes uint64      `json:"committed_bytes"`
	AvailableBytes uint64      `json:"available_bytes"`
	SpaceState     SpaceState  `json:"space_state,omitempty"`
	// On-pool metadata copy (P0-3): the velocor-meta lvol and the last manifest written to it.
	MetaLvolUUID   string       `json:"meta_lvol_uuid,omitempty"`
	MetaGeneration uint64       `json:"meta_generation,omitempty"`
	MetaHash       string       `json:"meta_hash,omitempty"`
	MetaWrittenAt  time.Time    `json:"meta_written_at,omitempty"`
	MetaError      string       `json:"meta_error,omitempty"`
	FilterPolicy   FilterPolicy `json:"filter_policy"`
	HealthStatus   HealthStatus `json:"health_status"`
	StatusMessage  string       `json:"status_message,omitempty"`
	CreatedAt      time.Time    `json:"created_at"`
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
	// DeletionProtection refuses deletion until an administrator turns it off.
	DeletionProtection bool `json:"deletion_protection,omitempty"`
	// DeletedAt is set while a deleted volume waits for its retention period to end: it is
	// hidden from the API and can't be exported, but its data is intact and it can be restored.
	DeletedAt *time.Time `json:"deleted_at,omitempty"`
	// PurgeAt is when the reconciler deletes a deleted volume's data for good.
	PurgeAt *time.Time `json:"purge_at,omitempty"`
	// DeletedBy is the API subject that deleted the volume.
	DeletedBy string `json:"deleted_by,omitempty"`
	// Purging is set once the purge of a deleted volume has started; it can't be restored then.
	Purging bool `json:"purging,omitempty"`
}

// Deleted reports whether the volume is deleted and waiting to be purged.
func (v *Volume) Deleted() bool { return v.DeletedAt != nil }

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
	// DeletedAt is set when a snapshot was deleted while volumes cloned from it still depend
	// on it (live ones, or deleted ones waiting to be purged): it is hidden from the API and
	// the reconciler deletes it once nothing depends on it.
	DeletedAt *time.Time `json:"deleted_at,omitempty"`
	DeletedBy string     `json:"deleted_by,omitempty"`
}

// Deleted reports whether the snapshot is deleted and waiting for its clones to go.
func (s *Snapshot) Deleted() bool { return s.DeletedAt != nil }

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
	// Shared marks a CSI export that several nodes use at once (ReadWriteMany / ReadOnlyMany raw
	// block, e.g. a KubeVirt VM disk during live migration). One shared export per volume;
	// NodeIDs lists the nodes it's published to, each allowed in through its initiator group.
	Shared  bool     `json:"shared,omitempty"`
	NodeIDs []string `json:"node_ids,omitempty"`
	// Disabled exports are not served (SPDK objects removed). A pool import restores an export
	// disabled when it can't be served as recorded (its portal addresses aren't on this
	// appliance, or its CHAP secrets must be set again); DisabledReason says why.
	Disabled       bool   `json:"disabled,omitempty"`
	DisabledReason string `json:"disabled_reason,omitempty"`
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
