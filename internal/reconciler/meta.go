package reconciler

import (
	"context"
	"fmt"
	"time"

	"github.com/velocor/velocor/internal/dataplane"
	"github.com/velocor/velocor/internal/meta"
	"github.com/velocor/velocor/internal/store"
)

// metaSync keeps a copy of each pool's configuration on the pool itself (TODO P0-3).
type metaSync struct {
	dev     meta.Device
	nodeID  string
	version string
	// synced records pools whose on-pool generation was read since velod started, so a
	// database restored from an older backup never writes a generation below one on the pool.
	synced map[string]bool
}

// SetMetaDevice enables the on-pool metadata copy. Without it (tests, or no ublk) the
// reconciler writes no copies.
func (r *Reconciler) SetMetaDevice(dev meta.Device, nodeID, version string) {
	r.passMu.Lock()
	defer r.passMu.Unlock()
	r.meta = &metaSync{dev: dev, nodeID: nodeID, version: version, synced: map[string]bool{}}
}

// reconcileMeta creates each online pool's velocor-meta lvol if needed and writes a new
// manifest generation whenever the pool's configuration changed.
func (r *Reconciler) reconcileMeta(ctx context.Context, p *pass, poolOnline map[string]bool) {
	if r.meta == nil {
		return
	}
	if err := r.meta.dev.Recover(ctx); err != nil {
		p.errs = append(p.errs, fmt.Sprintf("metadata device: %v", err))
		return
	}
	pools, err := r.store.ListPools()
	if err != nil || len(pools) == 0 {
		return
	}
	lvols, err := r.dp.Volumes(ctx)
	if err != nil {
		p.errs = append(p.errs, fmt.Sprintf("list lvols: %v", err))
		return
	}
	for _, pool := range pools {
		if !poolOnline[pool.ID] || pool.ThinPoolUUID == "" {
			continue
		}
		if err := r.syncPoolMeta(ctx, pool, lvols); err != nil {
			p.degrade("pool:"+pool.ID, "the on-pool metadata copy is out of date: "+err.Error())
			if _, merr := r.store.MutatePool(pool.ID, func(cur *store.Pool) error {
				if cur.MetaError == err.Error() {
					return store.ErrSkipWrite
				}
				cur.MetaError = err.Error()
				return nil
			}); merr != nil {
				p.errs = append(p.errs, fmt.Sprintf("record metadata error of %s: %v", pool.ID, merr))
			}
		}
	}
}

func (r *Reconciler) syncPoolMeta(ctx context.Context, pool *store.Pool, lvols []dataplane.Volume) error {
	uuid := ""
	for _, l := range lvols {
		if l.Pool == pool.ThinPoolUUID && l.Name == meta.LvolName {
			uuid = l.UUID
		}
	}
	if uuid == "" {
		// Thick, so a full pool can never fail a metadata write.
		created, err := r.dp.CreateVolume(ctx, pool.ThinPoolName, meta.LvolName, meta.LvolBytes, false)
		if err != nil {
			return fmt.Errorf("create the %s volume: %w", meta.LvolName, err)
		}
		uuid = created
		r.logger.Info("created the metadata volume", "pool", pool.ID, "volume", uuid)
	}

	m, err := meta.Build(r.store, pool.ID, r.meta.nodeID, r.meta.version)
	if err != nil {
		return fmt.Errorf("build the manifest: %w", err)
	}
	m.Pool.MetaVolumeUUID = uuid
	digest := m.Digest()
	if digest == pool.MetaHash && uuid == pool.MetaVolumeUUID && pool.MetaError == "" && r.meta.synced[pool.ID] {
		return nil
	}

	gen := pool.MetaGeneration
	if !r.meta.synced[pool.ID] {
		slots, err := r.meta.dev.ReadSlots(ctx, uuid)
		if err != nil {
			return fmt.Errorf("read the metadata volume: %w", err)
		}
		if onPool, err := meta.Newest(slots...); err == nil {
			if onPool.Generation > gen {
				r.logger.Warn("the pool holds a newer metadata generation than the database (restored from a backup?)",
					"pool", pool.ID, "pool_generation", onPool.Generation, "db_generation", gen)
				gen = onPool.Generation
			}
			if onPool.Generation == gen && onPool.Digest() == digest && uuid == pool.MetaVolumeUUID {
				r.meta.synced[pool.ID] = true
				return r.recordMeta(pool.ID, uuid, gen, digest, onPool.WrittenAt)
			}
		}
	}

	gen++
	m.Generation, m.WrittenAt = gen, time.Now().UTC()
	img, err := meta.EncodeSlot(m)
	if err != nil {
		return err
	}
	if err := r.meta.dev.WriteSlot(ctx, uuid, meta.SlotFor(gen), img); err != nil {
		return fmt.Errorf("write generation %d: %w", gen, err)
	}
	r.meta.synced[pool.ID] = true
	return r.recordMeta(pool.ID, uuid, gen, digest, m.WrittenAt)
}

func (r *Reconciler) recordMeta(poolID, uuid string, gen uint64, digest string, at time.Time) error {
	_, err := r.store.MutatePool(poolID, func(cur *store.Pool) error {
		cur.MetaVolumeUUID, cur.MetaGeneration, cur.MetaHash, cur.MetaWrittenAt, cur.MetaError = uuid, gen, digest, at, ""
		return nil
	})
	return err
}
