package spdkplane

import (
	"context"
	"fmt"

	"github.com/velocor/velocor/internal/dataplane"
	"github.com/velocor/velocor/internal/hardware"
	"github.com/velocor/velocor/internal/store"
)

// readSuperblock reads a disk's SPDK RAID superblock through the kernel (nil: none), or through
// p.ReadSuperblock (tests).
func (p *Plane) readSuperblock(d *store.Disk) (*hardware.RaidSuperblock, error) {
	if p.ReadSuperblock != nil {
		return p.ReadSuperblock(d)
	}
	if d.DevicePath == "" {
		return nil, nil // e.g. an NVMe drive bound to vfio-pci: no kernel view
	}
	return hardware.ReadRaidSuperblock(d.DevicePath)
}

// RaidLevelName maps SPDK's superblock enum raid_level to the store's names.
func RaidLevelName(level uint32) store.RaidLevel {
	switch level {
	case 0:
		return store.RaidLevel0
	case 1:
		return store.RaidLevel1
	case 95:
		return store.RaidLevel5F
	case 99:
		return "CONCAT"
	}
	return store.RaidLevel(fmt.Sprintf("LEVEL-%d", level))
}

// ExamineDisks reads the SPDK RAID superblock on each disk. Every member carries the same copy,
// listing the members by bdev UUID (derived from the disk ID, dataplane.DiskBdevUUID), so an
// SSD's slot is where its bdev UUID is; SSDs that came back under new IDs match no member and
// are matched to the members nobody claims (dataplane.MatchMovedMembers, docs/review-history.md
// P1-14). NVMe drives keep their IDs (PCI address) and get no slot.
func (p *Plane) ExamineDisks(_ context.Context, disks []*store.Disk) ([]dataplane.DiskArray, error) {
	type found struct {
		disk *store.Disk
		sb   *hardware.RaidSuperblock
	}
	byUUID := map[string][]found{}
	var order []string
	for _, d := range disks {
		sb, err := p.readSuperblock(d)
		if err != nil {
			Logger.Warn("cannot read the RAID superblock", "disk", d.ID, "err", err)
			continue
		}
		if sb == nil {
			continue
		}
		if byUUID[sb.UUID] == nil {
			order = append(order, sb.UUID)
		}
		byUUID[sb.UUID] = append(byUUID[sb.UUID], found{d, sb})
	}
	var out []dataplane.DiskArray
	for _, uuid := range order {
		fs := byUUID[uuid]
		// The newest superblock describes the array; members with an older one left it.
		newest := fs[0].sb
		for _, f := range fs {
			if f.sb.SeqNumber > newest.SeqNumber {
				newest = f.sb
			}
		}
		configured := map[string]int{}
		for _, m := range newest.Members {
			if m.State == hardware.RaidSBMemberConfigured {
				configured[m.BdevUUID] = m.Slot
			}
		}
		var moved []dataplane.MovedDisk
		matched := map[string]bool{}
		for _, f := range fs {
			if f.disk.Type != store.DiskTypeSSD {
				continue
			}
			if _, ok := configured[dataplane.DiskBdevUUID(f.disk)]; ok {
				matched[dataplane.DiskBdevUUID(f.disk)] = true
			} else if !f.disk.Claimed {
				moved = append(moved, dataplane.MovedDisk{Disk: f.disk, SB: f.sb})
			}
		}
		var unmatched []string
		for u := range configured {
			if !matched[u] {
				unmatched = append(unmatched, u)
			}
		}
		relinked, merr := dataplane.MatchMovedMembers(newest, unmatched, moved)
		a := dataplane.DiskArray{
			UUID: uuid, Name: newest.Name, Level: RaidLevelName(newest.Level),
			Slots: newest.NumMembers, Current: len(configured), Generation: newest.SeqNumber,
			SizeBytes: newest.RaidBlocks * uint64(newest.BlockSize),
		}
		if merr != nil {
			a.Unplaced = merr.Error()
		}
		for _, f := range fs {
			m := dataplane.DiskMember{Disk: f.disk, Slot: -1, Stale: f.sb.SeqNumber < newest.SeqNumber && !f.disk.Claimed}
			if f.disk.Type == store.DiskTypeSSD {
				id := dataplane.DiskBdevUUID(f.disk)
				if u, ok := relinked[f.disk.ID]; ok {
					id, m.Identity = u, u
				}
				if slot, ok := configured[id]; ok {
					m.Slot = slot
				} else {
					m.Stale = true // not a configured member of the current array
				}
			}
			a.Members = append(a.Members, m)
		}
		out = append(out, a)
	}
	return out, nil
}
