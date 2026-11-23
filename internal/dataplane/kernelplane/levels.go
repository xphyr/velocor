package kernelplane

import (
	"fmt"

	"github.com/velocor/velocor/internal/store"
)

// CheckLevel: md RAID0, RAID1 and RAID10 (near-2 layout: two copies; at least four disks, where
// it stripes over mirrored pairs). Parity levels aren't offered yet (TODO.md L-3).
func (p *Plane) CheckLevel(l store.RaidLevel, disks int) error {
	minDisks := map[store.RaidLevel]int{store.RaidLevel0: 1, store.RaidLevel1: 2, store.RaidLevel10: 4}
	n, ok := minDisks[l]
	if !ok {
		return fmt.Errorf("%w: unsupported raid_level %q (the kernel data plane builds RAID0, RAID1 and RAID10)", store.ErrInvalidArgument, l)
	}
	if disks < n {
		return fmt.Errorf("%w: %s requires at least %d disks", store.ErrInvalidArgument, l, n)
	}
	return nil
}
