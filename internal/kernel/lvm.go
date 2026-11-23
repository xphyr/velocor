package kernel

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// lvmConfig is passed to every LVM command, so the host's lvm.conf and devices file don't decide
// what velod sees: no devices file (pvcreate would otherwise start one and from then on hide every
// device not in it), PVs only on md arrays, never on the volumes' own devices or clients' data.
const lvmConfig = `devices { use_devicesfile = 0 filter = [ "a|^/dev/md|", "r|.*|" ] }`

// ChunkSize is the thin pools' chunk size: 64 KiB with zeroing on, measured 2026-11-20
// (docs/performance.md "LVM thin chunk size and zeroing").
const ChunkSize = 64 << 10

func (p *Plane) lvm(ctx context.Context, cmd string, args ...string) ([]byte, error) {
	return p.Run.Run(ctx, "lvm", append([]string{cmd, "--config", lvmConfig}, args...)...)
}

// PoolSpec describes the volume group and thin pool on an md array.
type PoolSpec struct {
	VG     string // volume group name
	Device string // the md array
	Thin   string // thin pool LV name
	// DataPercent of the volume group the thin pool takes; the rest stays free for thick
	// volumes, the pool's metadata (and LVM's spare copy of it) and growing the pool.
	DataPercent int
}

// MetadataSize is the thin pool metadata size for a pool of dataBytes: about 2 MB per GiB was
// measured at 64 KiB chunks; this gives four times that, between 64 MiB and LVM's 15.81 GiB limit.
func MetadataSize(dataBytes uint64) uint64 {
	const mib = 1 << 20
	m := dataBytes / (1 << 30) * 8 * mib
	switch {
	case m < 64*mib:
		m = 64 * mib
	case m > MaxMetadataSize:
		m = MaxMetadataSize
	}
	return m
}

// CreatePool puts a volume group on the array and a thin pool in it: 64 KiB chunks, new chunks
// zeroed (a partial write must not expose a deleted volume's data), and a full pool fails writes
// with ENOSPC at once instead of queueing them (TODO.md L-6).
func (p *Plane) CreatePool(ctx context.Context, s PoolSpec) error {
	if s.DataPercent <= 0 || s.DataPercent > 95 {
		return fmt.Errorf("data percent %d out of range 1-95", s.DataPercent)
	}
	if _, err := p.lvm(ctx, "pvcreate", "--quiet", "--yes", s.Device); err != nil {
		return err
	}
	if _, err := p.lvm(ctx, "vgcreate", "--quiet", s.VG, s.Device); err != nil {
		return err
	}
	// Size the data from the VG's real size: LVM puts the pool's metadata and a spare copy of it
	// (for repairs) beside the data, and "--extents N%VG" doesn't leave room for them (on a small
	// VG lvm2 2.03.43 even fails an internal assertion).
	out, err := p.lvm(ctx, "vgs", "--noheadings", "--units", "b", "--nosuffix", "-o", "vg_size,vg_extent_size", s.VG)
	if err != nil {
		return err
	}
	f := strings.Fields(string(out))
	if len(f) != 2 {
		return fmt.Errorf("vgs %s: unexpected output %q", s.VG, out)
	}
	vgSize, _ := strconv.ParseUint(f[0], 10, 64)
	extent, _ := strconv.ParseUint(f[1], 10, 64)
	if extent == 0 {
		return fmt.Errorf("vgs %s: extent size 0", s.VG)
	}
	share := vgSize * uint64(s.DataPercent) / 100
	meta := (MetadataSize(share) + extent - 1) / extent * extent
	if share <= 2*meta+extent {
		return fmt.Errorf("volume group %s (%d bytes) is too small for a thin pool", s.VG, vgSize)
	}
	data := (share - 2*meta) / extent * extent
	if _, err := p.lvm(ctx, "lvcreate", "--quiet", "--type", "thin-pool", "--name", s.Thin,
		"--size", fmt.Sprintf("%db", data), "--chunksize", fmt.Sprintf("%db", ChunkSize),
		"--zero", "y", "--poolmetadatasize", fmt.Sprintf("%db", meta), s.VG); err != nil {
		return err
	}
	// lvcreate refuses --errorwhenfull for a thin pool (lvm2 2.03.43); lvchange sets it.
	_, err = p.lvm(ctx, "lvchange", "--quiet", "--errorwhenfull", "y", s.VG+"/"+s.Thin)
	return err
}

// GrowPoolMetadata grows a thin pool's metadata to sizeBytes, online (LVM grows its spare copy
// with it).
func (p *Plane) GrowPoolMetadata(ctx context.Context, vg, thin string, sizeBytes uint64) error {
	_, err := p.lvm(ctx, "lvextend", "--quiet", "--poolmetadatasize", fmt.Sprintf("%db", sizeBytes), vg+"/"+thin)
	return err
}

// MaxMetadataSize is LVM's limit for a thin pool's metadata (15.81 GiB).
const MaxMetadataSize = 16188 << 20

// RemovePool removes the volume group (with every LV in it) and the PV label.
func (p *Plane) RemovePool(ctx context.Context, vg, device string) error {
	if _, err := p.lvm(ctx, "vgremove", "--quiet", "--force", vg); err != nil {
		return err
	}
	_, err := p.lvm(ctx, "pvremove", "--quiet", "--force", device)
	return err
}

// ActivatePool activates the volume group's LVs; snapshots keep LVM's skip-activation flag and
// stay inactive until needed. DeactivatePool deactivates everything in it.
func (p *Plane) ActivatePool(ctx context.Context, vg string) error {
	_, err := p.lvm(ctx, "vgchange", "--quiet", "--activate", "y", vg)
	return err
}

func (p *Plane) DeactivatePool(ctx context.Context, vg string) error {
	_, err := p.lvm(ctx, "vgchange", "--quiet", "--activate", "n", vg)
	return err
}

// CreateVolume creates a volume: a thin LV in the pool, or a thick one beside it, with LVM tags.
func (p *Plane) CreateVolume(ctx context.Context, vg, thinPool, name string, sizeBytes uint64, thin bool, tags ...string) error {
	size := fmt.Sprintf("%db", sizeBytes)
	if thin {
		_, err := p.lvm(ctx, "lvcreate", append([]string{"--quiet", "--virtualsize", size, "--thinpool", vg + "/" + thinPool, "--name", name}, tagArgs(tags)...)...)
		return err
	}
	_, err := p.lvm(ctx, "lvcreate", append(append([]string{"--quiet", "--size", size, "--name", name, "--yes", "--wipesignatures", "n", "--zero", "n"}, tagArgs(tags)...), vg)...)
	return err
}

func tagArgs(tags []string) []string {
	var out []string
	for _, t := range tags {
		out = append(out, "--addtag", t)
	}
	return out
}

// ResizeVolume grows a volume to sizeBytes.
func (p *Plane) ResizeVolume(ctx context.Context, vg, name string, sizeBytes uint64) error {
	_, err := p.lvm(ctx, "lvextend", "--quiet", "--size", fmt.Sprintf("%db", sizeBytes), vg+"/"+name)
	return err
}

// Snapshot takes a read-only thin snapshot of a thin volume. LVM gives it the skip-activation
// flag, so it has no device until ActivateLV with ignoreSkip.
func (p *Plane) Snapshot(ctx context.Context, vg, origin, name string, tags ...string) error {
	_, err := p.lvm(ctx, "lvcreate", append(append([]string{"--quiet", "--snapshot", "--permission", "r", "--name", name}, tagArgs(tags)...), vg+"/"+origin)...)
	return err
}

// Clone creates a writable thin volume from a snapshot (a thin snapshot of it without the
// skip-activation flag) and activates it.
func (p *Plane) Clone(ctx context.Context, vg, snapshot, name string, tags ...string) error {
	if _, err := p.lvm(ctx, "lvcreate", append(append([]string{"--quiet", "--snapshot", "--permission", "rw", "--setactivationskip", "n", "--name", name}, tagArgs(tags)...), vg+"/"+snapshot)...); err != nil {
		return err
	}
	return p.ActivateLV(ctx, vg, name, false)
}

// ActivateLV activates one LV (ignoreSkip for snapshots); DeactivateLV deactivates it.
func (p *Plane) ActivateLV(ctx context.Context, vg, name string, ignoreSkip bool) error {
	args := []string{"--quiet", "--activate", "y"}
	if ignoreSkip {
		args = append(args, "--ignoreactivationskip")
	}
	_, err := p.lvm(ctx, "lvchange", append(args, vg+"/"+name)...)
	return err
}

func (p *Plane) DeactivateLV(ctx context.Context, vg, name string) error {
	_, err := p.lvm(ctx, "lvchange", "--quiet", "--activate", "n", vg+"/"+name)
	return err
}

// RemoveVolume removes a volume or snapshot.
func (p *Plane) RemoveVolume(ctx context.Context, vg, name string) error {
	_, err := p.lvm(ctx, "lvremove", "--quiet", "--yes", vg+"/"+name)
	return err
}

// LV is one logical volume as LVM reports it.
type LV struct {
	Name, VG, UUID, Attr string
	Size                 uint64
	Pool, Origin         string
	ThinID               int     // thin volumes and snapshots; 0 otherwise
	DataPercent          float64 // thin volumes and the pool
	MetadataPercent      float64 // the pool
	ChunkSize            uint64  // the pool
	Active               bool
	SkipActivation       bool
	DMPath               string
	SegType              string // thin, thin-pool, linear
	Tags                 []string
	MetadataSize         uint64 // the thin pool's metadata
}

// ListVolumes lists a volume group's LVs, or every volume group's for vg "" (LVM's hidden ones,
// such as the pool's data and metadata, excluded).
func (p *Plane) ListVolumes(ctx context.Context, vg string) ([]LV, error) {
	args := []string{"--reportformat", "json", "--units", "b", "--nosuffix",
		"-o", "lv_name,vg_name,lv_uuid,lv_attr,lv_size,pool_lv,origin,thin_id,data_percent,metadata_percent,chunk_size,lv_active,lv_skip_activation,lv_dm_path,segtype,lv_tags,lv_metadata_size"}
	if vg != "" {
		args = append(args, vg)
	}
	out, err := p.lvm(ctx, "lvs", args...)
	if err != nil {
		return nil, err
	}
	return parseLVs(out)
}

func parseLVs(out []byte) ([]LV, error) {
	var rep struct {
		Report []struct {
			LV []map[string]string `json:"lv"`
		} `json:"report"`
	}
	if err := json.Unmarshal(out, &rep); err != nil {
		return nil, fmt.Errorf("lvs report: %w", err)
	}
	var lvs []LV
	for _, r := range rep.Report {
		for _, m := range r.LV {
			lv := LV{Name: m["lv_name"], VG: m["vg_name"], UUID: m["lv_uuid"], Attr: m["lv_attr"],
				Pool: m["pool_lv"], Origin: m["origin"], Active: m["lv_active"] == "active",
				SkipActivation: m["lv_skip_activation"] != "", DMPath: m["lv_dm_path"], SegType: m["segtype"]}
			if t := m["lv_tags"]; t != "" {
				lv.Tags = strings.Split(t, ",")
			}
			lv.Size, _ = strconv.ParseUint(m["lv_size"], 10, 64)
			lv.ThinID, _ = strconv.Atoi(m["thin_id"])
			lv.DataPercent, _ = strconv.ParseFloat(m["data_percent"], 64)
			lv.MetadataPercent, _ = strconv.ParseFloat(m["metadata_percent"], 64)
			lv.ChunkSize, _ = strconv.ParseUint(m["chunk_size"], 10, 64)
			lv.MetadataSize, _ = strconv.ParseUint(m["lv_metadata_size"], 10, 64)
			lvs = append(lvs, lv)
		}
	}
	return lvs, nil
}

// LVPath is a volume's device node.
func (p *Plane) LVPath(vg, name string) string { return p.DevRoot + "/" + vg + "/" + name }

// dmName is device-mapper's name for an LV: "-" in either name is doubled.
func dmName(vg, lv string) string {
	return strings.ReplaceAll(vg, "-", "--") + "-" + strings.ReplaceAll(lv, "-", "--")
}

// VG is one volume group as LVM reports it.
type VG struct {
	Name, UUID string
	Size, Free uint64
	ExtentSize uint64
	PV         string // the (single) physical volume's device
}

// ListVGs lists the volume groups on md arrays.
func (p *Plane) ListVGs(ctx context.Context) ([]VG, error) {
	out, err := p.lvm(ctx, "vgs", "--reportformat", "json", "--units", "b", "--nosuffix",
		"-o", "vg_name,vg_uuid,vg_size,vg_free,vg_extent_size,pv_name")
	if err != nil {
		return nil, err
	}
	return parseVGs(out)
}

func parseVGs(out []byte) ([]VG, error) {
	var rep struct {
		Report []struct {
			VG []map[string]string `json:"vg"`
		} `json:"report"`
	}
	if err := json.Unmarshal(out, &rep); err != nil {
		return nil, fmt.Errorf("vgs report: %w", err)
	}
	var vgs []VG
	for _, r := range rep.Report {
		for _, m := range r.VG {
			vg := VG{Name: m["vg_name"], UUID: m["vg_uuid"], PV: m["pv_name"]}
			vg.Size, _ = strconv.ParseUint(m["vg_size"], 10, 64)
			vg.Free, _ = strconv.ParseUint(m["vg_free"], 10, 64)
			vg.ExtentSize, _ = strconv.ParseUint(m["vg_extent_size"], 10, 64)
			vgs = append(vgs, vg)
		}
	}
	return vgs, nil
}
