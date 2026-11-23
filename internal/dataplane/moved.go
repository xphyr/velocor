package dataplane

import (
	"crypto/sha1"
	"fmt"
	"slices"
	"strings"

	"github.com/velocor/velocor/internal/hardware"
	"github.com/velocor/velocor/internal/store"
)

// Pool members under new disk IDs (docs/review-history.md P1-14). A disk's ID comes from what its controller
// reports (WWN, serial), so the same disk gets a new ID when it moves to another controller
// (SATA to virtio-scsi, another HBA, a changed controller mode). Its SSD bdev UUID is derived
// from the ID, and SPDK's RAID superblock knows members only by bdev UUID, so the array no
// longer assembles. The superblock on the disk still says which array it belongs to; what it
// can't say is which member a disk is, because every member carries the same copy.

// MovedDisk is an unclaimed disk carrying a superblock of the array in question.
type MovedDisk struct {
	Disk *store.Disk
	SB   *hardware.RaidSuperblock
}

// MatchMovedMembers assigns the member bdev UUIDs in want (members whose disks are gone) to
// the disks in found, returning disk ID -> bdev UUID. newest is the array's current
// superblock: members it doesn't list as configured left the array before and are dropped
// from want, and only disks whose copy has its sequence number count (an older copy also left
// the array). It assigns only when certain, and returns nil to wait otherwise:
//   - every wanted member must have come back: with fewer current disks, one of them could be
//     an out-of-date copy whose current counterpart hasn't returned yet;
//   - several members: only RAID1 whose members all have the same data layout, where members
//     are interchangeable (same data in the same place); a striped array can't be guessed.
//
// A disk too small for its member's data, or with another block size, is refused.
func MatchMovedMembers(newest *hardware.RaidSuperblock, want []string, found []MovedDisk) (map[string]string, error) {
	if newest == nil || len(want) == 0 {
		return nil, nil
	}
	byUUID := map[string]hardware.RaidSBMember{}
	for _, m := range newest.Members {
		if m.State == hardware.RaidSBMemberConfigured {
			byUUID[m.BdevUUID] = m
		}
	}
	want = slices.DeleteFunc(slices.Clone(want), func(u string) bool { _, ok := byUUID[u]; return !ok })
	if len(want) == 0 {
		return nil, nil
	}
	var disks []*store.Disk
	for _, f := range found {
		if f.SB != nil && f.SB.UUID == newest.UUID && f.SB.SeqNumber == newest.SeqNumber {
			disks = append(disks, f.Disk)
		}
	}
	if len(disks) < len(want) {
		return nil, nil
	}
	slices.SortFunc(disks, func(a, b *store.Disk) int { return strings.Compare(a.ID, b.ID) })
	slices.Sort(want)
	if len(disks) > len(want) {
		return nil, fmt.Errorf("%d disks carry RAID %s but only %d of its members are missing; can't tell which they are", len(disks), newest.Name, len(want))
	}
	if len(want) > 1 {
		if newest.Level != 1 {
			return nil, fmt.Errorf("%d members of RAID %s came back under new disk IDs, and a %s array can't tell which is which", len(want), newest.Name, raidLevelLabel(newest.Level))
		}
		var first *hardware.RaidSBMember
		for _, m := range byUUID {
			if first == nil {
				first = &m
			} else if m.DataOffset != first.DataOffset || m.DataSize != first.DataSize {
				return nil, fmt.Errorf("the members of RAID %s have different data layouts, so the disks that came back under new IDs can't be told apart", newest.Name)
			}
		}
	}
	out := map[string]string{}
	for i, d := range disks {
		m := byUUID[want[i]]
		if newest.BlockSize != 0 && d.BlockSize != 0 && d.BlockSize != newest.BlockSize {
			return nil, fmt.Errorf("disk %s has %d-byte blocks, RAID %s %d-byte", d.ID, d.BlockSize, newest.Name, newest.BlockSize)
		}
		if need := (m.DataOffset + m.DataSize) * uint64(newest.BlockSize); d.SizeBytes < need {
			return nil, fmt.Errorf("disk %s (%d bytes) is smaller than member %s of RAID %s (%d bytes)", d.ID, d.SizeBytes, m.BdevUUID, newest.Name, need)
		}
		out[d.ID] = m.BdevUUID
	}
	return out, nil
}

func raidLevelLabel(level uint32) string {
	switch level {
	case 0:
		return "RAID0"
	case 95:
		return "RAID5F"
	case 99:
		return "concatenated"
	}
	return fmt.Sprintf("level-%d", level)
}

// DiskBdevUUID returns the UUID given to a disk's io_uring/AIO bdev. It is derived from the
// disk's stable hardware ID (a name-based, version-5-style UUID), so it is the same on every
// restart, and recomputable without the metadata database. SPDK re-assembles a superblock RAID
// only when each member's bdev UUID matches the superblock: a random UUID per restart would
// leave every SSD pool offline after spdk_tgt restarts (found by the integration harness).
// A member that came back under a new ID keeps its old UUID in BdevUUID (MatchMovedMembers).
func DiskBdevUUID(d *store.Disk) string {
	if d.BdevUUID != "" {
		return d.BdevUUID
	}
	sum := sha1.Sum([]byte("velocor/disk-bdev/" + d.ID))
	sum[6] = (sum[6] & 0x0f) | 0x50
	sum[8] = (sum[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", sum[0:4], sum[4:6], sum[6:8], sum[8:10], sum[10:16])
}
