package spdk

import (
	"context"
	"strings"
)

// RaidBaseBdev describes a member base bdev inside an SPDK RAID bdev.
type RaidBaseBdev struct {
	Name         string `json:"name"`
	UUID         string `json:"uuid"`
	IsConfigured bool   `json:"is_configured"`
	DataOffset   uint64 `json:"data_offset"`
	DataSize     uint64 `json:"data_size"`
}

// RaidBdevInfo describes an SPDK RAID bdev returned by bdev_raid_get_bdevs.
type RaidBdevInfo struct {
	Name                   string `json:"name"`
	UUID                   string `json:"uuid"`
	StripSizeKb            uint32 `json:"strip_size_kb"`
	State                  string `json:"state"`
	RaidLevel              string `json:"raid_level"`
	Superblock             bool   `json:"superblock"`
	NumBaseBdevs           uint32 `json:"num_base_bdevs"`
	NumBaseBdevsDiscovered uint32 `json:"num_base_bdevs_discovered"`
	// NumBaseBdevsOperational is how many members the array is running (or, while
	// configuring, waiting) with: fewer than NumBaseBdevs means degraded.
	NumBaseBdevsOperational uint32       `json:"num_base_bdevs_operational"`
	Process                 *RaidProcess `json:"process,omitempty"`
	// Scrub is the running or last scrub since the array was configured (patch 0004).
	Scrub         *RaidScrub     `json:"scrub,omitempty"`
	BaseBdevsList []RaidBaseBdev `json:"base_bdevs_list"`
}

// Scrub states reported by SPDK (patch 0004).
const (
	ScrubRunning   = "running"
	ScrubCompleted = "completed"
	ScrubStopped   = "stopped" // on request, by a member removal or a rebuild
	ScrubFailed    = "failed"
)

// RaidScrub is a RAID bdev's running or last scrub (SPDK patch 0004). Offsets and counts are in
// blocks of the members' data area; the counters cover the run since StartOffsetBlocks.
type RaidScrub struct {
	State                  string `json:"state"`
	Repair                 bool   `json:"repair"`
	Mirrored               bool   `json:"mirrored"`
	Status                 int32  `json:"status"`
	StartOffsetBlocks      uint64 `json:"start_offset_blocks"`
	OffsetBlocks           uint64 `json:"offset_blocks"`
	TotalBlocks            uint64 `json:"total_blocks"`
	RepairedBlocks         uint64 `json:"repaired_blocks"`
	MismatchedBlocks       uint64 `json:"mismatched_blocks"`
	MismatchRepairedBlocks uint64 `json:"mismatch_repaired_blocks"`
	UnrecoverableBlocks    uint64 `json:"unrecoverable_blocks"`
	WriteErrors            uint64 `json:"write_errors"`
}

// BdevRaidScrubStart starts a background scrub of a RAID bdev (patch 0004): RAID1 copies are
// compared, unreadable ones rewritten, and mismatches counted and (with repair) rewritten;
// other levels get a read-only surface scan. maxMBps 0 is unlimited; startOffset resumes an
// interrupted scrub.
func (c *Client) BdevRaidScrubStart(ctx context.Context, name string, repair bool, maxMBps uint32, startOffset uint64) error {
	params := map[string]any{"name": name, "repair": repair, "max_bandwidth_mb_sec": maxMBps, "start_offset_blocks": startOffset}
	return c.Call(ctx, "bdev_raid_scrub_start", params, nil)
}

// BdevRaidScrubStop stops a running scrub after its current window.
func (c *Client) BdevRaidScrubStop(ctx context.Context, name string) error {
	return c.Call(ctx, "bdev_raid_scrub_stop", map[string]any{"name": name}, nil)
}

// RaidProcess is a background process (a rebuild) running on a RAID bdev.
type RaidProcess struct {
	Type     string `json:"type"`   // "rebuild"
	Target   string `json:"target"` // base bdev being rebuilt
	Progress struct {
		Blocks  uint64 `json:"blocks"`
		Percent uint32 `json:"percent"`
	} `json:"progress"`
}

// BdevRaidGetBdevs queries SPDK for RAID bdevs by category ("all", "online", "configuring", "offline").
func (c *Client) BdevRaidGetBdevs(ctx context.Context, category string) ([]RaidBdevInfo, error) {
	if category == "" {
		category = "all"
	}
	params := map[string]any{"category": category}
	var res []RaidBdevInfo
	if err := c.Call(ctx, "bdev_raid_get_bdevs", params, &res); err != nil {
		return nil, err
	}
	return res, nil
}

// NormalizeRaidLevel converts Velocor RAID enum strings (e.g., "RAID0", "RAID5F")
// to SPDK bdev_raid_create level strings ("raid0", "raid1", "raid5f"; SPDK v26.09 has no raid10).
func NormalizeRaidLevel(level string) string {
	l := strings.ToLower(strings.TrimSpace(level))
	switch l {
	case "0", "raid0":
		return "raid0"
	case "1", "raid1":
		return "raid1"
	case "10", "raid10":
		return "raid10"
	case "5f", "raid5f":
		return "raid5f"
	default:
		return l
	}
}

// BdevRaidCreate creates an SPDK RAID bdev across the specified base bdevs.
//
// When superblock is true SPDK writes RAID metadata to every member, so the array is
// re-assembled automatically by bdev examine after a restart (including in degraded
// mode for redundant levels). NEVER pass superblock=true for members of an existing
// array that was created without one: the superblock shifts the data offset and
// destroys the existing contents.
func (c *Client) BdevRaidCreate(ctx context.Context, name, raidLevel string, stripSizeKB uint32, baseBdevs []string, superblock bool) error {
	normLevel := NormalizeRaidLevel(raidLevel)
	params := map[string]any{
		"name":       name,
		"raid_level": normLevel,
		"base_bdevs": baseBdevs,
		"superblock": superblock,
	}
	// SPDK RAID1 does not require strip_size_kb unless non-zero is provided for striped levels.
	if stripSizeKB > 0 && normLevel != "raid1" {
		params["strip_size_kb"] = stripSizeKB
	}
	return c.Call(ctx, "bdev_raid_create", params, nil)
}

// BdevRaidDelete destroys an SPDK RAID bdev by name. With clearSuperblock, the RAID
// superblock is wiped from every member; otherwise SPDK examine re-assembles the deleted
// array as soon as its disks are attached again (verified against SPDK v26.09).
func (c *Client) BdevRaidDelete(ctx context.Context, name string, clearSuperblock bool) error {
	params := map[string]any{"name": name}
	if clearSuperblock {
		params["clear_sb"] = true
	}
	return c.Call(ctx, "bdev_raid_delete", params, nil)
}

// BdevRaidAddBaseBdev adds a base bdev to an empty slot of a RAID bdev. On an online
// redundant array SPDK then rebuilds it from the other members in the background (see
// RaidBdevInfo.Process); the member is recorded in the superblock once the rebuild completes.
func (c *Client) BdevRaidAddBaseBdev(ctx context.Context, raidBdev, baseBdev string) error {
	return c.Call(ctx, "bdev_raid_add_base_bdev", map[string]any{"raid_bdev": raidBdev, "base_bdev": baseBdev}, nil)
}

// BdevRaidRemoveBaseBdev removes a member from its RAID bdev (the array degrades, or goes
// offline when too few members remain). The superblock records the slot as missing.
func (c *Client) BdevRaidRemoveBaseBdev(ctx context.Context, baseBdev string) error {
	return c.Call(ctx, "bdev_raid_remove_base_bdev", map[string]any{"name": baseBdev}, nil)
}

// BdevRaidStartDegraded starts a superblock RAID that is waiting for absent members without
// them (Velocor SPDK patch 0002). The absent members are recorded as missing; one that
// returns later never resumes as an up-to-date member (SPDK would re-add and rebuild it, and
// velod doesn't re-attach disks that left their pool).
func (c *Client) BdevRaidStartDegraded(ctx context.Context, name string) error {
	return c.Call(ctx, "bdev_raid_start_degraded", map[string]any{"name": name}, nil)
}

// BdevRaidSetOptions sets the background process (rebuild) bandwidth limit for every RAID
// bdev; 0 means unlimited.
func (c *Client) BdevRaidSetOptions(ctx context.Context, maxBandwidthMBps uint32) error {
	return c.Call(ctx, "bdev_raid_set_options", map[string]any{"process_max_bandwidth_mb_sec": maxBandwidthMBps}, nil)
}

// BdevRaidGetMaxBandwidth returns the rebuild bandwidth limit SPDK currently applies
// (0 = unlimited), read from the bdev subsystem's configuration.
func (c *Client) BdevRaidGetMaxBandwidth(ctx context.Context) (uint32, error) {
	var cfg []struct {
		Method string `json:"method"`
		Params struct {
			MaxBandwidth uint32 `json:"process_max_bandwidth_mb_sec"`
		} `json:"params"`
	}
	if err := c.Call(ctx, "framework_get_config", map[string]any{"name": "bdev"}, &cfg); err != nil {
		return 0, err
	}
	for _, m := range cfg {
		if m.Method == "bdev_raid_set_options" {
			return m.Params.MaxBandwidth, nil
		}
	}
	return 0, nil
}
