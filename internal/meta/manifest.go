// Package meta keeps a copy of each pool's metadata on the pool itself (TODO P0-3;
// docs/design/p0-3-metadata-recovery.md): a small thick lvol, velocor-meta, holds the pool's
// manifest in two alternating slots, so a pool's disks carry what is needed to bring it back
// on any appliance. Accounts, tokens and node secrets are never part of it.
package meta

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/velocor/velocor/internal/store"
)

const (
	// LvolName is the name of the metadata lvol in every pool's volume store.
	LvolName = "velocor-meta"
	// LvolBytes is its size: two slots.
	LvolBytes = store.MetaLvolBytes
	// SlotBytes is one slot: a header block plus the manifest.
	SlotBytes = LvolBytes / 2
	// HeaderBytes is the header block at the start of a slot (one direct-I/O block).
	HeaderBytes = 4096
	// FormatVersion of the manifest layout.
	FormatVersion = 1
)

var magic = [8]byte{'V', 'E', 'L', 'O', 'M', 'E', 'T', 'A'}

// ErrNoManifest: neither slot holds a valid manifest.
var ErrNoManifest = errors.New("no valid metadata manifest")

// Manifest is everything needed to bring one pool back.
type Manifest struct {
	Format         int                         `json:"format"`
	Generation     uint64                      `json:"generation"`
	WrittenAt      time.Time                   `json:"written_at"`
	NodeID         string                      `json:"node_id"`
	VelodVersion   string                      `json:"velod_version"`
	Pool           store.Pool                  `json:"pool"`
	Disks          []store.Disk                `json:"disks"`
	Volumes        []store.Volume              `json:"volumes"`
	Snapshots      []store.Snapshot            `json:"snapshots"`
	GroupSnapshots []store.VolumeGroupSnapshot `json:"group_snapshots"`
	Exports        []store.Export              `json:"exports"`
	Hosts          []store.Host                `json:"hosts"`
	InitiatorGroup []store.InitiatorGroup      `json:"initiator_groups"`
	Portals        []store.Portal              `json:"portals"`
}

// Content returns the manifest without the fields that change on every write (generation,
// time) and the pool's live figures, as a stable digest of what it describes: a new copy is
// written only when this changes.
func (m *Manifest) Content() []byte {
	c := *m
	c.Generation, c.WrittenAt = 0, time.Time{}
	p := c.Pool
	p.TotalBytes, p.FreeBytes, p.AllocatedBytes, p.CommittedBytes, p.AvailableBytes = 0, 0, 0, 0, 0
	p.SpaceState, p.HealthStatus, p.StatusMessage, p.Members = "", "", "", nil
	p.ThinMetadataBytes, p.ThinMetadataUsedBytes, p.UnreadableBytes, p.LostData, p.Durability = 0, 0, 0, nil, ""
	p.MetaGeneration, p.MetaHash, p.MetaWrittenAt, p.MetaError = 0, "", time.Time{}, ""
	c.Pool = p
	disks := make([]store.Disk, len(c.Disks))
	for i, d := range c.Disks {
		disks[i] = identityOnly(d)
	}
	c.Disks = disks
	vols := make([]store.Volume, len(c.Volumes))
	for i, v := range c.Volumes {
		v.AllocatedBytes, v.HealthStatus, v.StatusMessage = 0, "", ""
		vols[i] = v
	}
	c.Volumes = vols
	snaps := make([]store.Snapshot, len(c.Snapshots))
	for i, s := range c.Snapshots {
		s.AllocatedBytes = 0
		snaps[i] = s
	}
	c.Snapshots = snaps
	exps := make([]store.Export, len(c.Exports))
	for i, e := range c.Exports {
		e.HealthStatus, e.StatusMessage = "", ""
		exps[i] = e
	}
	c.Exports = exps
	b, _ := json.Marshal(c)
	return b
}

// Digest of Content.
func (m *Manifest) Digest() string {
	sum := sha256.Sum256(m.Content())
	return fmt.Sprintf("%x", sum)
}

// identityOnly keeps what identifies a disk and its place in the pool, not its live state.
func identityOnly(d store.Disk) store.Disk {
	return store.Disk{
		ID: d.ID, Name: d.Name, Type: d.Type, SerialNumber: d.SerialNumber, Model: d.Model,
		SizeBytes: d.SizeBytes, BlockSize: d.BlockSize, PCIAddress: d.PCIAddress, DevicePath: d.DevicePath,
		PoolID: d.PoolID, PowerLossProtected: d.PowerLossProtected, WriteCachePolicy: d.WriteCachePolicy,
	}
}

// Build collects a pool's manifest from the store.
func Build(st store.Store, poolID, nodeID, version string) (*Manifest, error) {
	pool, err := st.GetPool(poolID)
	if err != nil {
		return nil, err
	}
	m := &Manifest{Format: FormatVersion, NodeID: nodeID, VelodVersion: version, Pool: *pool}
	for _, id := range slices.Concat(pool.DiskIDs, pool.DetachedDiskIDs) {
		if d, err := st.GetDisk(id); err == nil {
			m.Disks = append(m.Disks, identityOnly(*d))
		}
	}
	vols, err := st.ListVolumes(poolID)
	if err != nil {
		return nil, err
	}
	volIDs := map[string]bool{}
	for _, v := range vols {
		m.Volumes = append(m.Volumes, *v)
		volIDs[v.ID] = true
	}
	snaps, err := st.ListSnapshots(poolID, "")
	if err != nil {
		return nil, err
	}
	snapIDs := map[string]bool{}
	for _, s := range snaps {
		m.Snapshots = append(m.Snapshots, *s)
		snapIDs[s.ID] = true
	}
	if groups, err := st.ListVolumeGroupSnapshots(); err == nil {
		for _, g := range groups {
			if slices.ContainsFunc(g.SnapshotIDs, func(id string) bool { return snapIDs[id] }) {
				m.GroupSnapshots = append(m.GroupSnapshots, *g)
			}
		}
	}
	exports, err := st.ListExports("", "")
	if err != nil {
		return nil, err
	}
	igIDs, portalIDs := map[string]bool{}, map[string]bool{}
	for _, e := range exports {
		if !volIDs[e.VolumeID] {
			continue
		}
		m.Exports = append(m.Exports, *e)
		for _, id := range e.InitiatorGroupIDs {
			igIDs[id] = true
		}
		for _, id := range e.PortalIDs {
			portalIDs[id] = true
		}
	}
	hostIDs := map[string]bool{}
	if igs, err := st.ListInitiatorGroups(); err == nil {
		for _, g := range igs {
			if igIDs[g.ID] {
				// CHAP secrets stay off the pool (no secrets in pool copies); an import asks
				// for them again.
				g.CHAPSecretEncrypted, g.MutualCHAPSecretEncrypted = nil, nil
				m.InitiatorGroup = append(m.InitiatorGroup, *g)
				for _, h := range g.HostIDs {
					hostIDs[h] = true
				}
			}
		}
	}
	if hosts, err := st.ListHosts(); err == nil {
		for _, h := range hosts {
			if hostIDs[h.ID] {
				m.Hosts = append(m.Hosts, *h)
			}
		}
	}
	if portals, err := st.ListPortals(""); err == nil {
		for _, p := range portals {
			if portalIDs[p.ID] {
				m.Portals = append(m.Portals, *p)
			}
		}
	}
	return m, nil
}

// header is the first block of a slot.
type header struct {
	Magic      [8]byte
	Format     uint32
	_          uint32
	Generation uint64
	Length     uint64
	SHA256     [32]byte
}

// EncodeSlot returns a slot image: header block, then the manifest JSON, padded to a
// multiple of the header block size (direct I/O).
func EncodeSlot(m *Manifest) ([]byte, error) {
	body, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	if HeaderBytes+len(body) > SlotBytes {
		return nil, fmt.Errorf("manifest of pool %s is %d bytes, more than a slot holds (%d)", m.Pool.ID, len(body), SlotBytes-HeaderBytes)
	}
	h := header{Magic: magic, Format: FormatVersion, Generation: m.Generation, Length: uint64(len(body)), SHA256: sha256.Sum256(body)}
	var hb bytes.Buffer
	if err := binary.Write(&hb, binary.LittleEndian, h); err != nil {
		return nil, err
	}
	size := HeaderBytes + len(body)
	size = (size + HeaderBytes - 1) / HeaderBytes * HeaderBytes
	out := make([]byte, size)
	copy(out, hb.Bytes())
	copy(out[HeaderBytes:], body)
	return out, nil
}

// SlotOffset is where slot i (0 or 1) starts in the metadata lvol.
func SlotOffset(i int) int64 { return int64(i) * SlotBytes }

// SlotFor is the slot a generation is written to: generations alternate, so the previous one
// stays intact while a new one is written.
func SlotFor(generation uint64) int { return int(generation % 2) }

// DecodeSlot parses one slot image; ok is false if it holds no valid manifest.
func DecodeSlot(b []byte) (*Manifest, bool) {
	if len(b) < HeaderBytes {
		return nil, false
	}
	var h header
	if err := binary.Read(bytes.NewReader(b[:binary.Size(h)]), binary.LittleEndian, &h); err != nil {
		return nil, false
	}
	if h.Magic != magic || h.Format != FormatVersion || h.Length == 0 || h.Length > uint64(len(b)-HeaderBytes) {
		return nil, false
	}
	body := b[HeaderBytes : HeaderBytes+h.Length]
	if sha256.Sum256(body) != h.SHA256 {
		return nil, false
	}
	var m Manifest
	// A copy written before the K-2 renames has the legacy keys (checked as written, above).
	if err := json.Unmarshal(store.UpgradeJSON(bytes.Clone(body)), &m); err != nil || m.Generation != h.Generation {
		return nil, false
	}
	return &m, true
}

// Newest returns the newest valid manifest of the two slot images.
func Newest(slots ...[]byte) (*Manifest, error) {
	var best *Manifest
	for _, s := range slots {
		if m, ok := DecodeSlot(s); ok && (best == nil || m.Generation > best.Generation) {
			best = m
		}
	}
	if best == nil {
		return nil, ErrNoManifest
	}
	return best, nil
}
