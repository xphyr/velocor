package kernel

import "context"

// A volume's device stack, bottom to top:
//
//	md array ─ [pool features: LUKS] ─ PV/VG ─ thin pool ─ [pool features: VDO data] ─ LV
//	  ─ [volume layers: dm-crypt, dm-integrity, ...] ─ dm-linear wrapper ─ nvmet / LIO export
//
// The wrapper is always on top: it is the quiesce point for group snapshots and the device the
// targets export, so neither depends on which optional layers sit underneath. Optional features
// plug in at two seams, both off unless a plugin is configured, and none is implemented in the
// spike (docs/design/kernel-dataplane.md, "Optional layers"):
//
//   - PoolFeature changes how a pool is built: encryption of the whole array (LUKS between md
//     and the PV) or VDO deduplication and compression of the thin pool's data
//     (`lvcreate --type thin-pool --pooldatavdo y`, which needs vdoformat from the vdo package
//     in the OS image).
//   - VolumeLayer stacks a device-mapper target on one volume between its LV and the wrapper,
//     such as dm-crypt with a per-volume key, or dm-integrity checksums.

// PoolFeature is an optional pool-wide plugin.
type PoolFeature interface {
	// Name identifies the feature in configuration ("luks", "vdo").
	Name() string
	// Available reports whether this host can provide it (modules, tools), with the reason if not.
	Available(ctx context.Context, p *Plane) (bool, string)
	// UnderPV wraps the array before it becomes a PV and returns the device to use instead;
	// Open does the same when the pool is assembled again; Close undoes it on stop.
	UnderPV(ctx context.Context, p *Plane, pool, array string) (string, error)
	Open(ctx context.Context, p *Plane, pool, array string) (string, error)
	Close(ctx context.Context, p *Plane, pool string) error
	// ThinPoolArgs are extra lvcreate arguments for the thin pool (e.g. --pooldatavdo y).
	ThinPoolArgs() []string
}

// VolumeLayer is an optional per-volume plugin between the LV and the wrapper.
type VolumeLayer interface {
	Name() string
	Available(ctx context.Context, p *Plane) (bool, string)
	// Create sets the layer up on a new volume's device and returns the device above it; Open
	// does the same for an existing volume (after a restart); Close takes it down; Remove
	// destroys it with the volume (e.g. erases the key).
	Create(ctx context.Context, p *Plane, volume, lower string) (string, error)
	Open(ctx context.Context, p *Plane, volume, lower string) (string, error)
	Close(ctx context.Context, p *Plane, volume string) error
	Remove(ctx context.Context, p *Plane, volume string) error
}

// OpenStack opens a volume's layers in order above its LV and returns the device the wrapper goes
// on: the LV itself when there are no layers.
func (p *Plane) OpenStack(ctx context.Context, volume, lv string, layers []VolumeLayer) (string, error) {
	dev := lv
	for i, l := range layers {
		up, err := l.Open(ctx, p, volume, dev)
		if err != nil {
			for j := i - 1; j >= 0; j-- {
				_ = layers[j].Close(ctx, p, volume)
			}
			return "", err
		}
		dev = up
	}
	return dev, nil
}
