package spdk

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"time"
)

// LvolStoreInfo describes an SPDK logical volume store (lvol_store).
type LvolStoreInfo struct {
	UUID              string `json:"uuid"`
	Name              string `json:"name"`
	BaseBdev          string `json:"base_bdev"`
	TotalDataClusters uint64 `json:"total_data_clusters"`
	FreeClusters      uint64 `json:"free_clusters"`
	BlockSize         uint32 `json:"block_size"`
	ClusterSize       uint32 `json:"cluster_size"`
}

// BdevLvolGetLvstores queries SPDK for lvol_stores, optionally filtered by lvs_name or uuid.
func (c *Client) BdevLvolGetLvstores(ctx context.Context, lvsName, uuid string) ([]LvolStoreInfo, error) {
	params := make(map[string]any)
	if lvsName != "" {
		params["lvs_name"] = lvsName
	}
	if uuid != "" {
		params["uuid"] = uuid
	}
	var reqParams any
	if len(params) > 0 {
		reqParams = params
	}
	var res []LvolStoreInfo
	if err := c.Call(ctx, "bdev_lvol_get_lvstores", reqParams, &res); err != nil {
		return nil, err
	}
	return res, nil
}

// BdevLvolCreateLvstore initializes an SPDK lvol_store on top of bdevName with the specified clusterSize.
// Returns the UUID of the created lvol_store.
func (c *Client) BdevLvolCreateLvstore(ctx context.Context, bdevName, lvsName string, clusterSize uint32, clearMethod string) (string, error) {
	params := map[string]any{
		"bdev_name": bdevName,
		"lvs_name":  lvsName,
	}
	if clusterSize > 0 {
		params["cluster_sz"] = clusterSize
	}
	if clearMethod != "" {
		params["clear_method"] = clearMethod
	}
	var uuid string
	if err := c.Call(ctx, "bdev_lvol_create_lvstore", params, &uuid); err != nil {
		return "", err
	}
	return uuid, nil
}

// BdevLvolDeleteLvstore destroys an SPDK lvol_store by uuid or lvsName.
func (c *Client) BdevLvolDeleteLvstore(ctx context.Context, uuid, lvsName string) error {
	params := make(map[string]any)
	if uuid != "" {
		params["uuid"] = uuid
	}
	if lvsName != "" {
		params["lvs_name"] = lvsName
	}
	return c.Call(ctx, "bdev_lvol_delete_lvstore", params, nil)
}

// BytesToMiBCeil converts a byte count to MiB, rounding up so that SPDK never
// allocates less capacity than was requested.
func BytesToMiBCeil(sizeBytes uint64) uint64 {
	const mib = 1024 * 1024
	return (sizeBytes + mib - 1) / mib
}

// BdevLvolCreate creates a logical volume inside an lvol_store and returns its UUID.
func (c *Client) BdevLvolCreate(ctx context.Context, lvsName, lvolName string, sizeBytes uint64, thinProvision bool, clearMethod string) (string, error) {
	sizeMiB := BytesToMiBCeil(sizeBytes)
	params := map[string]any{
		"lvs_name":       lvsName,
		"lvol_name":      lvolName,
		"size_in_mib":    sizeMiB,
		"thin_provision": thinProvision,
	}
	if clearMethod != "" {
		params["clear_method"] = clearMethod
	}
	var uuid string
	if err := c.Call(ctx, "bdev_lvol_create", params, &uuid); err != nil {
		return "", err
	}
	return uuid, nil
}

// BdevLvolResize resizes an existing logical volume (name is "<lvs_name>/<lvol_name>" or UUID).
func (c *Client) BdevLvolResize(ctx context.Context, name string, sizeBytes uint64) error {
	sizeMiB := BytesToMiBCeil(sizeBytes)
	params := map[string]any{
		"name":        name,
		"size_in_mib": sizeMiB,
	}
	return c.Call(ctx, "bdev_lvol_resize", params, nil)
}

// BdevLvolDelete deletes a logical volume or snapshot by alias ("<lvs_name>/<lvol_name>") or UUID.
func (c *Client) BdevLvolDelete(ctx context.Context, name string) error {
	params := map[string]any{"name": name}
	return c.Call(ctx, "bdev_lvol_delete", params, nil)
}

// BdevLvolSnapshot creates an immutable point-in-time snapshot of lvolName and returns the snapshot UUID.
func (c *Client) BdevLvolSnapshot(ctx context.Context, lvolName, snapshotName string) (string, error) {
	params := map[string]any{
		"lvol_name":     lvolName,
		"snapshot_name": snapshotName,
	}
	var uuid string
	if err := c.Call(ctx, "bdev_lvol_snapshot", params, &uuid); err != nil {
		return "", err
	}
	return uuid, nil
}

// SnapshotGroupEntry is one member of BdevLvolSnapshotGroup.
type SnapshotGroupEntry struct {
	LvolName     string `json:"lvol_name"`
	SnapshotName string `json:"snapshot_name"`
}

// MaxSnapshotGroup is the most lvols one BdevLvolSnapshotGroup call may cover.
const MaxSnapshotGroup = 64

// BdevLvolSnapshotGroup snapshots several lvols at one point in time (patch 0003): SPDK quiesces
// I/O to all of them, snapshots each, then resumes I/O, all within the one RPC, so I/O can't be
// left paused. It returns the snapshot UUIDs in entry order. If I/O doesn't drain within
// quiesceTimeout no snapshot is taken (ETIMEDOUT). On other failures some snapshots may have been
// taken; the caller deletes them by name.
func (c *Client) BdevLvolSnapshotGroup(ctx context.Context, entries []SnapshotGroupEntry, quiesceTimeout time.Duration) ([]string, error) {
	params := map[string]any{
		"snapshots":          entries,
		"quiesce_timeout_ms": quiesceTimeout.Milliseconds(),
	}
	var uuids []string
	if err := c.Call(ctx, "bdev_lvol_snapshot_group", params, &uuids); err != nil {
		return nil, err
	}
	if len(uuids) != len(entries) {
		return nil, fmt.Errorf("bdev_lvol_snapshot_group returned %d snapshots for %d lvols", len(uuids), len(entries))
	}
	return uuids, nil
}

// BdevLvolClone creates a writable thin clone volume from snapshotName and returns the clone UUID.
func (c *Client) BdevLvolClone(ctx context.Context, snapshotName, cloneName string) (string, error) {
	params := map[string]any{
		"snapshot_name": snapshotName,
		"clone_name":    cloneName,
	}
	var uuid string
	if err := c.Call(ctx, "bdev_lvol_clone", params, &uuid); err != nil {
		return "", err
	}
	return uuid, nil
}

// BdevLvolDecoupleParent decouples a cloned volume from its parent snapshot.
func (c *Client) BdevLvolDecoupleParent(ctx context.Context, name string) error {
	params := map[string]any{"name": name}
	return c.Call(ctx, "bdev_lvol_decouple_parent", params, nil)
}

// FragmapResponse represents the output of bdev_lvol_get_fragmap used for Changed Block Tracking (CBT).
type FragmapResponse struct {
	ClusterSize uint32 `json:"cluster_size"`
	Offset      uint64 `json:"offset"`
	Size        uint64 `json:"size"`
	NumClusters uint64 `json:"num_allocated_clusters"`
	Bitmask     string `json:"bitmask"`
}

// BlockExtent represents a contiguous range of allocated clusters in bytes.
type BlockExtent struct {
	OffsetBytes uint64
	SizeBytes   uint64
}

// LvolInfo describes an SPDK logical volume or snapshot returned by bdev_lvol_get_lvols.
type LvolInfo struct {
	Alias                string `json:"alias"`
	UUID                 string `json:"uuid"`
	Name                 string `json:"name"`
	IsThinProvisioned    bool   `json:"is_thin_provisioned"`
	IsSnapshot           bool   `json:"is_snapshot"`
	IsClone              bool   `json:"is_clone"`
	NumAllocatedClusters uint64 `json:"num_allocated_clusters"`
	LVS                  struct {
		Name string `json:"name"`
		UUID string `json:"uuid"`
	} `json:"lvs"`
}

// BdevLvolGetLvols queries SPDK for all logical volumes and snapshots.
func (c *Client) BdevLvolGetLvols(ctx context.Context) ([]LvolInfo, error) {
	var res []LvolInfo
	if err := c.Call(ctx, "bdev_lvol_get_lvols", nil, &res); err != nil {
		return nil, err
	}
	return res, nil
}

// BdevLvolGetFragmap queries the cluster allocation bitmask of a logical volume or snapshot
// over the byte range [offset, offset+size).
// In SPDK's thin-provisioned lvol chain, a snapshot's fragmap marks only the clusters
// allocated specifically in that snapshot layer (delta since its parent), enabling O(metadata) CBT.
func (c *Client) BdevLvolGetFragmap(ctx context.Context, name string, offset, size uint64) (*FragmapResponse, error) {
	params := map[string]any{
		"name": name,
	}
	if offset > 0 {
		params["offset"] = offset
	}
	if size > 0 {
		params["size"] = size
	}
	var res FragmapResponse
	if err := c.Call(ctx, "bdev_lvol_get_fragmap", params, &res); err != nil {
		var rpcErr *RPCError
		if errors.As(err, &rpcErr) && rpcErr.Code == -32601 {
			// Never synthesise allocation data: a guessed bitmap would make incremental
			// backups silently skip changed blocks.
			return nil, fmt.Errorf("%w: spdk_tgt does not support bdev_lvol_get_fragmap", ErrCBTUnsupported)
		}
		return nil, err
	}
	return &res, nil
}

// ErrCBTUnsupported is returned when the running spdk_tgt cannot report per-cluster allocation.
var ErrCBTUnsupported = errors.New("changed block tracking unsupported")

// DecodeFragmapExtents decodes the base64-encoded bitmask from FragmapResponse into
// coalesced contiguous BlockExtent ranges.
func DecodeFragmapExtents(resp *FragmapResponse) ([]BlockExtent, error) {
	if resp == nil || resp.ClusterSize == 0 || resp.Bitmask == "" {
		return nil, nil
	}
	rawBytes, err := base64.StdEncoding.DecodeString(resp.Bitmask)
	if err != nil {
		return nil, fmt.Errorf("decode fragmap base64 bitmask: %w", err)
	}

	clusterSz := uint64(resp.ClusterSize)
	totalClusters := resp.Size / clusterSz
	maxBits := uint64(len(rawBytes)) * 8
	if totalClusters == 0 || totalClusters > maxBits {
		totalClusters = maxBits
	}

	var extents []BlockExtent
	var inRun bool
	var runStart uint64
	var runLen uint64

	for i := uint64(0); i < totalClusters; i++ {
		byteIdx := i / 8
		bitIdx := i % 8
		allocated := (rawBytes[byteIdx] & (1 << bitIdx)) != 0

		if allocated {
			if !inRun {
				inRun = true
				runStart = resp.Offset + i*clusterSz
				runLen = clusterSz
			} else {
				runLen += clusterSz
			}
		} else if inRun {
			extents = append(extents, BlockExtent{
				OffsetBytes: runStart,
				SizeBytes:   runLen,
			})
			inRun = false
		}
	}

	if inRun {
		extents = append(extents, BlockExtent{
			OffsetBytes: runStart,
			SizeBytes:   runLen,
		})
	}

	return extents, nil
}

// MergeBlockExtents merges and coalesces multiple BlockExtent slices (e.g., across a snapshot delta chain)
// into a sorted, non-overlapping list of contiguous extents.
func MergeBlockExtents(extentSets ...[]BlockExtent) []BlockExtent {
	var all []BlockExtent
	for _, set := range extentSets {
		all = append(all, set...)
	}
	if len(all) == 0 {
		return nil
	}

	// Insertion/quicksort by OffsetBytes.
	for i := 1; i < len(all); i++ {
		for j := i; j > 0 && all[j].OffsetBytes < all[j-1].OffsetBytes; j-- {
			all[j], all[j-1] = all[j-1], all[j]
		}
	}

	merged := []BlockExtent{all[0]}
	for _, cur := range all[1:] {
		last := &merged[len(merged)-1]
		lastEnd := last.OffsetBytes + last.SizeBytes
		curEnd := cur.OffsetBytes + cur.SizeBytes
		if cur.OffsetBytes <= lastEnd {
			if curEnd > lastEnd {
				last.SizeBytes = curEnd - last.OffsetBytes
			}
		} else {
			merged = append(merged, cur)
		}
	}
	return merged
}
