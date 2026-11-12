package spdk

import (
	"context"
	"strings"
)

// RaidBaseBdev describes a member base bdev inside an SPDK RAID bdev.
type RaidBaseBdev struct {
	Name        string `json:"name"`
	UUID        string `json:"uuid"`
	IsConfigured bool  `json:"is_configured"`
	DataOffset  uint64 `json:"data_offset"`
	DataSize    uint64 `json:"data_size"`
}

// RaidBdevInfo describes an SPDK RAID bdev returned by bdev_raid_get_bdevs.
type RaidBdevInfo struct {
	Name             string         `json:"name"`
	UUID             string         `json:"uuid"`
	StripSizeKb      uint32         `json:"strip_size_kb"`
	State            string         `json:"state"`
	RaidLevel        string         `json:"raid_level"`
	Superblock       bool           `json:"superblock"`
	NumBaseBdevs     uint32         `json:"num_base_bdevs"`
	NumBaseBdevsDiscovered uint32   `json:"num_base_bdevs_discovered"`
	BaseBdevsList    []RaidBaseBdev `json:"base_bdevs_list"`
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
// to SPDK bdev_raid_create level strings ("raid0", "raid1", "raid10", "raid5f").
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

// BdevRaidCreate creates an SPDK RAID bdev across the specified base bdevs with superblock persistence.
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

// BdevRaidDelete destroys an SPDK RAID bdev by name.
func (c *Client) BdevRaidDelete(ctx context.Context, name string) error {
	params := map[string]any{"name": name}
	return c.Call(ctx, "bdev_raid_delete", params, nil)
}
