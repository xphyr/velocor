package spdk

import (
	"context"
	"strings"
)

// ublk exposes SPDK bdevs to the host kernel as /dev/ublkb<id> (SPDK built --with-ublk; the
// kernel's ublk_drv). velod uses it only to read and write the per-pool metadata copy (P0-3).

// UblkDisk is one ublk device served by spdk_tgt.
type UblkDisk struct {
	BdevName   string `json:"bdev_name"`
	ID         uint32 `json:"id"`
	UblkDevice string `json:"ublk_device"`
}

// UblkCreateTarget starts SPDK's ublk target (once per spdk_tgt start). It reports success if
// the target already exists.
func (c *Client) UblkCreateTarget(ctx context.Context) error {
	err := c.Call(ctx, "ublk_create_target", nil, nil)
	if err != nil && (strings.Contains(err.Error(), "busy") || strings.Contains(err.Error(), "exist")) {
		return nil
	}
	return err
}

// UblkStartDisk exposes bdev as /dev/ublkb<id>.
func (c *Client) UblkStartDisk(ctx context.Context, bdev string, id uint32) error {
	return c.Call(ctx, "ublk_start_disk", map[string]any{"bdev_name": bdev, "ublk_id": id}, nil)
}

// UblkRecoverDisk takes over a ublk device whose previous server (an earlier spdk_tgt) died,
// serving it from bdev again: I/O waiting on the device then completes.
func (c *Client) UblkRecoverDisk(ctx context.Context, bdev string, id uint32) error {
	return c.Call(ctx, "ublk_recover_disk", map[string]any{"bdev_name": bdev, "ublk_id": id}, nil)
}

// UblkStopDisk removes /dev/ublkb<id>.
func (c *Client) UblkStopDisk(ctx context.Context, id uint32) error {
	return c.Call(ctx, "ublk_stop_disk", map[string]any{"ublk_id": id}, nil)
}

// UblkGetDisks lists the ublk devices spdk_tgt serves.
func (c *Client) UblkGetDisks(ctx context.Context) ([]UblkDisk, error) {
	var out []UblkDisk
	if err := c.Call(ctx, "ublk_get_disks", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}
