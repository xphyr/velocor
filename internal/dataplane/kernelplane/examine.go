package kernelplane

import (
	"context"
	"fmt"
	"strings"

	"github.com/velocor/velocor/internal/dataplane"
	"github.com/velocor/velocor/internal/kernel"
	"github.com/velocor/velocor/internal/store"
)

// ExamineDisks reads each disk's md metadata. Each member's copy names the array (UUID, name)
// and the member's own slot, and its event count: a member that left the array keeps a lower
// count than the members still in it. So, unlike SPDK's superblock, a disk that came back
// under a new ID needs no matching: it says itself which slot it holds.
func (p *Plane) ExamineDisks(ctx context.Context, disks []*store.Disk) ([]dataplane.DiskArray, error) {
	type found struct {
		disk *store.Disk
		info *kernel.MemberInfo
	}
	byUUID := map[string][]found{}
	var order []string
	for _, d := range disks {
		dev := d.Handle
		if dev == "" {
			dev = d.DevicePath
		}
		if dev == "" {
			continue
		}
		info, err := p.k.ExamineMember(ctx, dev)
		if err != nil {
			return nil, fmt.Errorf("examine %s: %w", d.ID, err)
		}
		if info == nil {
			continue
		}
		if byUUID[info.ArrayUUID] == nil {
			order = append(order, info.ArrayUUID)
		}
		byUUID[info.ArrayUUID] = append(byUUID[info.ArrayUUID], found{d, info})
	}
	var out []dataplane.DiskArray
	for _, uuid := range order {
		fs := byUUID[uuid]
		newest := fs[0].info
		for _, f := range fs {
			if f.info.Events > newest.Events {
				newest = f.info
			}
		}
		a := dataplane.DiskArray{
			UUID: uuid, Name: newest.Name, Level: store.RaidLevel(strings.ToUpper(newest.Level)),
			Slots: newest.RaidDevices, Current: strings.Count(newest.ArrayState, "A") + strings.Count(newest.ArrayState, "R"),
			Generation: newest.Events, SizeBytes: newest.ArrayKiB * 1024,
		}
		for _, f := range fs {
			a.Members = append(a.Members, dataplane.DiskMember{
				Disk: f.disk, Slot: f.info.Role,
				Stale: f.info.Events < newest.Events || f.info.Role < 0 || f.info.Role >= newest.RaidDevices,
			})
		}
		out = append(out, a)
	}
	return out, nil
}
