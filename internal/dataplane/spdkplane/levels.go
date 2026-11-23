package spdkplane

import (
	"fmt"

	"github.com/velocor/velocor/internal/store"
)

// CheckLevel: SPDK v26.09 builds RAID0 and RAID1 that can hold a volume store.
func (p *Plane) CheckLevel(l store.RaidLevel, disks int) error {
	if l == store.RaidLevel5F {
		// SPDK's raid5f only accepts full-stripe writes, and the lvol blobstore issues smaller
		// ones, so a volume store can't even be created on it (verified against SPDK v26.09).
		return fmt.Errorf("%w: RAID5F is not supported: SPDK raid5f cannot hold a volume store", store.ErrInvalidArgument)
	}
	if l == store.RaidLevel10 {
		// SPDK v26.09's bdev_raid has raid0, raid1, raid5f and concat: no raid10. It has to be
		// built as raid0 over raid1 pairs (TODO.md L-4).
		return fmt.Errorf("%w: RAID10 is not supported yet: SPDK has no raid10 level (use RAID1)", store.ErrInvalidArgument)
	}
	minDisks := map[store.RaidLevel]int{store.RaidLevel0: 1, store.RaidLevel1: 2}
	n, ok := minDisks[l]
	if !ok {
		return fmt.Errorf("%w: unsupported raid_level %q", store.ErrInvalidArgument, l)
	}
	if disks < n {
		return fmt.Errorf("%w: %s requires at least %d disks", store.ErrInvalidArgument, l, n)
	}
	return nil
}
