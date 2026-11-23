package spdkplane

import (
	"context"
	"fmt"

	"github.com/velocor/velocor/internal/dataplane"
)

// Unmanaged takes SPDK's inventory (lvstores, lvols, RAIDs, bdevs, NVMe-oF subsystems, iSCSI
// targets) and reports what known doesn't account for.
func (p *Plane) Unmanaged(ctx context.Context, known dataplane.Known) ([]dataplane.UnmanagedObject, error) {
	lvstores, err1 := p.c.BdevLvolGetLvstores(ctx, "", "")
	lvols, err2 := p.c.BdevLvolGetLvols(ctx)
	raids, err3 := p.c.BdevRaidGetBdevs(ctx, "all")
	bdevs, err4 := p.c.BdevGetBdevs(ctx, "")
	subsystems, err5 := p.c.NvmfGetSubsystems(ctx)
	targets, err6 := p.c.IscsiGetTargetNodes(ctx)
	for _, err := range []error{err1, err2, err3, err4, err5, err6} {
		if err != nil {
			return nil, err
		}
	}

	var out []dataplane.UnmanagedObject
	lvsName := map[string]string{}
	for _, l := range lvstores {
		lvsName[l.UUID] = l.Name
		if !known.ThinPools[l.UUID] {
			out = append(out, dataplane.UnmanagedObject{Kind: "LVSTORE", Name: l.Name, UUID: l.UUID,
				Detail: fmt.Sprintf("on %s, %s used", l.BaseBdev, humanBytes((l.TotalDataClusters-l.FreeClusters)*uint64(l.ClusterSize)))})
		}
	}
	lvolBdevs := map[string]bool{}
	for _, l := range lvols {
		lvolBdevs[l.UUID] = true
		// Lvols in an unmanaged lvstore are covered by its report.
		if !known.ThinPools[l.LVS.UUID] || known.Volumes[l.UUID] || known.Skip[l.Name] {
			continue
		}
		kind := "volume"
		if l.IsSnapshot {
			kind = "snapshot"
		}
		out = append(out, dataplane.UnmanagedObject{Kind: "LVOL", Name: l.Alias, UUID: l.UUID,
			Detail: fmt.Sprintf("a %s in pool lvstore %s", kind, lvsName[l.LVS.UUID])})
	}
	raidNames := map[string]bool{}
	for _, rb := range raids {
		raidNames[rb.Name] = true
		if !known.Devices[rb.Name] {
			out = append(out, dataplane.UnmanagedObject{Kind: "RAID", Name: rb.Name, UUID: rb.UUID,
				Detail: fmt.Sprintf("%s, %s, %d of %d members", rb.RaidLevel, rb.State, rb.NumBaseBdevsDiscovered, rb.NumBaseBdevs)})
		}
	}
	for _, b := range bdevs {
		if lvolBdevs[b.Name] || lvolBdevs[b.UUID] || raidNames[b.Name] || known.Devices[b.Name] {
			continue
		}
		if len(b.Aliases) > 0 && known.Devices[b.Aliases[0]] {
			continue
		}
		detail := humanBytes(b.NumBlocks * uint64(b.BlockSize))
		if b.ProductName != "" {
			detail = b.ProductName + ", " + detail
		}
		out = append(out, dataplane.UnmanagedObject{Kind: "BDEV", Name: b.Name, UUID: b.UUID, Detail: detail})
	}
	for _, s := range subsystems {
		if s.Subtype == "Discovery" || known.Targets[s.Nqn] {
			continue
		}
		out = append(out, dataplane.UnmanagedObject{Kind: "NVMF_SUBSYSTEM", Name: s.Nqn,
			Detail: "may give hosts access to a volume no export controls"})
	}
	for _, t := range targets {
		if known.Targets[t.Name] {
			continue
		}
		out = append(out, dataplane.UnmanagedObject{Kind: "ISCSI_TARGET", Name: t.Name,
			Detail: "may give hosts access to a volume no export controls"})
	}
	return out, nil
}

func humanBytes(b uint64) string {
	switch {
	case b >= 1<<40:
		return fmt.Sprintf("%.1f TiB", float64(b)/(1<<40))
	case b >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(b)/(1<<30))
	default:
		return fmt.Sprintf("%.1f MiB", float64(b)/(1<<20))
	}
}
