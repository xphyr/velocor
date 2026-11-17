package spdktest

import (
	"strings"

	"github.com/velocor/velocor/internal/spdk"
)

// RAID model, following SPDK v26.09's bdev_raid (module/bdev/raid):
//   - each member carries its own copy of the superblock (slots + sequence number), so a disk
//     that left the array keeps an older copy;
//   - an array created or assembled with all its members is online; a redundant array that
//     loses a member while online stays online, degraded, and records the slot as failed (the
//     remaining members get a newer superblock);
//   - at startup an array is assembled from the newest superblock found and waits
//     ("configuring") until every member recorded as configured is present; members with an
//     older superblock are ignored;
//   - bdev_raid_add_base_bdev fills an empty slot of an online array and rebuilds it; the slot
//     is recorded as configured only when the rebuild completes;
//   - bdev_raid_start_degraded (Velocor SPDK patch 0002) starts a configuring array without
//     its absent members.

const (
	sbConfigured = "configured"
	sbMissing    = "missing"
	sbFailed     = "failed"
)

type sbSlot struct {
	Key   string // media key of the member ("" if none)
	UUID  string // member bdev UUID
	State string
}

type raidSuperblock struct {
	Name  string
	UUID  string
	Level string
	Strip uint32
	Seq   uint64
	Slots []sbSlot
}

func (sb *raidSuperblock) clone() *raidSuperblock {
	c := *sb
	c.Slots = append([]sbSlot(nil), sb.Slots...)
	return &c
}

type raidRebuild struct {
	Slot    int
	Percent uint32
}

type raidBdev struct {
	Name       string
	UUID       string
	Level      string
	Strip      uint32
	Superblock bool
	Members    []string // base bdev name per slot ("" = empty)
	MediaKey   string   // key into lvsMedia
	// online: registered and serving I/O. operational: members the array runs with (or, while
	// configuring, waits for).
	online      bool
	failed      bool // lost too many members while online
	operational int
	rebuild     *raidRebuild
	sb          *raidSuperblock // the array's current superblock (nil without one)
	// deleting: bdev_raid_delete was called while an lvstore was loaded on top. Real spdk_tgt
	// then leaves the RAID half-deleted ("offline") until the lvstore goes away.
	deleting, deleteClearSB bool
}

func minOperational(level string, n int) int {
	if strings.EqualFold(level, "raid1") {
		return 1
	}
	return n
}

func (r *raidBdev) discovered() int {
	n := 0
	for _, m := range r.Members {
		if m != "" {
			n++
		}
	}
	return n
}

func (r *raidBdev) state() string {
	switch {
	case r.deleting || r.failed:
		return "offline"
	case r.online:
		return "online"
	default:
		return "configuring"
	}
}

func (f *Fake) raidOnline(r *raidBdev) bool { return r.online && !r.deleting }

func (f *Fake) memberOfRaid(bdev string) bool {
	_, _, ok := f.raidOfMember(bdev)
	return ok
}

func (f *Fake) raidOfMember(bdev string) (*raidBdev, int, bool) {
	for _, r := range f.raids {
		for i, m := range r.Members {
			if m == bdev {
				return r, i, true
			}
		}
	}
	return nil, 0, false
}

// writeSuperblockLocked bumps the array's superblock and writes it to every present member.
func (f *Fake) writeSuperblockLocked(r *raidBdev) {
	if r.sb == nil {
		return
	}
	r.sb.Seq++
	for _, m := range r.Members {
		if m != "" {
			f.superblock[f.baseBdevs[m]] = r.sb.clone()
		}
	}
}

// goOnlineLocked configures an array; like SPDK, configuring writes its superblock.
func (f *Fake) goOnlineLocked(r *raidBdev) {
	r.online = true
	f.writeSuperblockLocked(r)
	f.raidAppearedLocked(r)
}

// memberGoneLocked handles a member leaving a running array: hot-removed (failed) or removed
// with bdev_raid_remove_base_bdev.
func (f *Fake) memberGoneLocked(r *raidBdev, slot int, state string) {
	r.Members[slot] = ""
	if !r.online {
		return // configuring: the slot simply isn't discovered (yet)
	}
	if r.rebuild != nil && r.rebuild.Slot == slot {
		r.rebuild = nil // the rebuild target left; its slot was never recorded as configured
	} else if r.sb != nil {
		r.sb.Slots[slot].State = state
	}
	r.operational--
	if r.operational < minOperational(r.Level, len(r.Members)) {
		r.online, r.failed = false, true
		for name, key := range f.loadedLvs {
			if key == r.MediaKey {
				delete(f.loadedLvs, name)
			}
		}
	}
	f.writeSuperblockLocked(r)
}

// baseBdevRemovedLocked is called when a base bdev disappears (deleted, controller detached,
// media failed): SPDK fails it out of any array it belongs to.
func (f *Fake) baseBdevRemovedLocked(bdev string) {
	if r, slot, ok := f.raidOfMember(bdev); ok {
		f.memberGoneLocked(r, slot, sbFailed)
	}
}

// FailMedia simulates a drive failing while spdk_tgt runs: its media disappears and its base
// bdevs are hot-removed (failing them out of their arrays).
func (f *Fake) FailMedia(key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.nvmeMedia, key)
	delete(f.fileMedia, key)
	for name, k := range f.baseBdevs {
		if k == key {
			f.baseBdevRemovedLocked(name)
			delete(f.baseBdevs, name)
			for c, traddr := range f.nvmeCtrls {
				if traddr == key {
					delete(f.nvmeCtrls, c)
				}
			}
		}
	}
}

// SetRebuildProgress sets the progress of the rebuild running on raid; 100 completes it.
func (f *Fake) SetRebuildProgress(raid string, percent uint32) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.raids[raid]
	if r == nil || r.rebuild == nil {
		f.t.Fatalf("spdktest: SetRebuildProgress: no rebuild running on %s", raid)
	}
	f.advanceRebuildLocked(r, percent)
}

// SetRebuildStep makes a running rebuild advance by percent per bdev_raid_get_bdevs call
// (0, the default: only through SetRebuildProgress).
func (f *Fake) SetRebuildStep(percent uint32) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rebuildStep = percent
}

func (f *Fake) advanceRebuildLocked(r *raidBdev, percent uint32) {
	if percent < 100 {
		r.rebuild.Percent = percent
		return
	}
	slot := r.rebuild.Slot
	r.rebuild = nil
	if r.sb != nil {
		m := r.Members[slot]
		r.sb.Slots[slot] = sbSlot{Key: f.baseBdevs[m], UUID: f.bdevUUID[m], State: sbConfigured}
		f.writeSuperblockLocked(r)
	}
}

// SuperblockSeq returns the sequence number of the RAID superblock on the given media.
func (f *Fake) SuperblockSeq(key string) (seq uint64, ok bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	sb := f.superblock[key]
	if sb == nil {
		return 0, false
	}
	return sb.Seq, true
}

// WipeMedia clears RAID superblocks and lvstores from a drive, as zeroing its first
// megabytes would.
func (f *Fake) WipeMedia(key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.superblock, key)
}

// assembleFromSuperblocksLocked is SPDK's examine for a new base bdev: it joins (or creates)
// the array its superblock describes.
func (f *Fake) assembleFromSuperblocksLocked(newBdev string) {
	key := f.baseBdevs[newBdev]
	sb := f.superblock[key]
	if sb == nil {
		return
	}
	r, ok := f.raids[sb.Name]
	switch {
	case ok && r.UUID != sb.UUID:
		return // another array of that name
	case ok && r.sb != nil && sb.Seq < r.sb.Seq:
		// A member that left the array: SPDK checks it against the array's current superblock
		// and, if the array runs and the member's slot is still empty, re-adds it and rebuilds
		// it ("Re-adding bdev ... to raid bdev"). A slot filled by another disk refuses it.
		f.readdStaleMemberLocked(r, newBdev)
		return
	case ok && r.sb != nil && sb.Seq > r.sb.Seq && !r.online:
		// A newer superblock while configuring: SPDK re-creates the array from it.
		delete(f.raids, sb.Name)
		ok = false
	case ok && r.sb != nil && sb.Seq > r.sb.Seq:
		return // newer superblock on a member of a running array: not handled (logged by SPDK)
	}
	if !ok {
		r = &raidBdev{
			Name: sb.Name, UUID: sb.UUID, Level: sb.Level, Strip: sb.Strip, Superblock: true,
			Members: make([]string, len(sb.Slots)), MediaKey: "sb:" + sb.UUID, sb: sb.clone(),
		}
		for _, s := range sb.Slots {
			if s.State == sbConfigured {
				r.operational++
			}
		}
		f.raids[sb.Name] = r
		// Re-examine the other present bdevs carrying this array's superblock.
		for name, k := range f.baseBdevs {
			if name != newBdev && f.superblock[k] != nil && f.superblock[k].UUID == sb.UUID {
				f.joinSlotLocked(r, name)
			}
		}
	}
	f.joinSlotLocked(r, newBdev)
}

func (f *Fake) readdStaleMemberLocked(r *raidBdev, bdev string) {
	if !r.online || r.rebuild != nil {
		return
	}
	for i, s := range r.sb.Slots {
		if s.State != sbConfigured && s.UUID == f.bdevUUID[bdev] && r.Members[i] == "" {
			r.Members[i] = bdev
			r.operational++
			r.rebuild = &raidRebuild{Slot: i}
			f.writeSuperblockLocked(r)
			return
		}
	}
}

func (f *Fake) joinSlotLocked(r *raidBdev, bdev string) {
	if r.online {
		return
	}
	// Real SPDK only accepts a member whose bdev UUID matches a configured slot
	// (bdev_raid.c, "Failed to examine bdev ...: Invalid argument" otherwise).
	for i, s := range r.sb.Slots {
		if s.State == sbConfigured && s.UUID == f.bdevUUID[bdev] && r.Members[i] == "" {
			r.Members[i] = bdev
			break
		}
	}
	if r.discovered() == r.operational && r.operational >= minOperational(r.Level, len(r.Members)) {
		f.goOnlineLocked(r)
	}
}

func (f *Fake) finishRaidDelete(name string, clearSB bool) {
	if r := f.raids[name]; r != nil && clearSB {
		for k, sb := range f.superblock {
			if sb.UUID == r.UUID {
				delete(f.superblock, k)
			}
		}
	}
	delete(f.raids, name)
}

func init() {
	handlers["bdev_raid_get_bdevs"] = func(f *Fake, _ map[string]any) (any, *rpcErr) {
		var out []spdk.RaidBdevInfo
		for _, r := range f.raids {
			if r.rebuild != nil && f.rebuildStep > 0 {
				f.advanceRebuildLocked(r, r.rebuild.Percent+f.rebuildStep)
			}
			var list []spdk.RaidBaseBdev
			for i, m := range r.Members {
				b := spdk.RaidBaseBdev{Name: m, IsConfigured: m != ""}
				if m != "" {
					b.UUID = f.bdevUUID[m]
				} else if r.sb != nil && r.sb.Slots[i].State == sbConfigured {
					b.UUID = r.sb.Slots[i].UUID // awaited member
				}
				list = append(list, b)
			}
			info := spdk.RaidBdevInfo{
				Name: r.Name, UUID: r.UUID, RaidLevel: r.Level, State: r.state(), Superblock: r.Superblock,
				NumBaseBdevs: uint32(len(r.Members)), NumBaseBdevsDiscovered: uint32(r.discovered()),
				NumBaseBdevsOperational: uint32(r.operational), BaseBdevsList: list,
			}
			if r.rebuild != nil {
				info.Process = &spdk.RaidProcess{Type: "rebuild", Target: r.Members[r.rebuild.Slot]}
				info.Process.Progress.Percent = r.rebuild.Percent
			}
			out = append(out, info)
		}
		return out, nil
	}

	handlers["bdev_raid_create"] = func(f *Fake, p map[string]any) (any, *rpcErr) {
		name := str(p, "name")
		if _, ok := f.raids[name]; ok {
			return nil, errf(codeEEXIST, "raid bdev %s already exists", name)
		}
		members := strs(p, "base_bdevs")
		var keys []string
		for _, m := range members {
			key, ok := f.baseBdevs[m]
			if !ok {
				return nil, errf(codeENODEV, "base bdev %s not found", m)
			}
			if f.memberOfRaid(m) {
				return nil, errf(codeEBUSY, "base bdev %s already claimed", m)
			}
			keys = append(keys, key)
		}
		withSB := boolean(p, "superblock")
		r := &raidBdev{
			Name: name, UUID: f.newUUID(), Level: str(p, "raid_level"), Strip: uint32(num(p, "strip_size_kb")),
			Superblock: withSB, Members: members, MediaKey: "nosb:" + strings.Join(keys, ","),
			operational: len(members),
		}
		if withSB {
			// Writing a superblock destroys any pre-existing data layout on the members.
			r.MediaKey = "sb:" + r.UUID
			for _, k := range keys {
				if old := f.superblock[k]; old != nil {
					delete(f.lvsMedia, "sb:"+old.UUID)
				}
			}
			delete(f.lvsMedia, "nosb:"+strings.Join(keys, ","))
			r.sb = &raidSuperblock{Name: name, UUID: r.UUID, Level: r.Level, Strip: r.Strip}
			for i, m := range members {
				r.sb.Slots = append(r.sb.Slots, sbSlot{Key: keys[i], UUID: f.bdevUUID[m], State: sbConfigured})
			}
		}
		f.raids[name] = r
		f.goOnlineLocked(r)
		return true, nil
	}

	handlers["bdev_raid_add_base_bdev"] = func(f *Fake, p map[string]any) (any, *rpcErr) {
		r := f.raids[str(p, "raid_bdev")]
		bdev := str(p, "base_bdev")
		switch {
		case r == nil:
			return nil, errf(codeENODEV, "raid bdev %s is not found in config", str(p, "raid_bdev"))
		case r.rebuild != nil:
			return nil, errf(-1, "raid bdev '%s' is in process", r.Name)
		}
		if _, ok := f.baseBdevs[bdev]; !ok {
			return nil, errf(codeENODEV, "No such device")
		}
		if f.memberOfRaid(bdev) {
			return nil, errf(codeEBUSY, "base bdev %s already claimed", bdev)
		}
		if !r.online {
			return nil, errf(-22, "raid bdev %s is not online", r.Name)
		}
		for i, m := range r.Members {
			if m == "" && (r.sb == nil || r.sb.Slots[i].State != sbConfigured) {
				r.Members[i] = bdev
				r.operational++
				r.rebuild = &raidRebuild{Slot: i}
				f.writeSuperblockLocked(r) // the target gets the array's superblock, its slot not yet configured
				return true, nil
			}
		}
		return nil, errf(-22, "no empty slot found in raid bdev '%s' for new base bdev '%s'", r.Name, bdev)
	}

	handlers["bdev_raid_remove_base_bdev"] = func(f *Fake, p map[string]any) (any, *rpcErr) {
		r, slot, ok := f.raidOfMember(str(p, "name"))
		if !ok {
			return nil, errf(codeENODEV, "No such device")
		}
		f.memberGoneLocked(r, slot, sbMissing)
		return true, nil
	}

	handlers["bdev_raid_start_degraded"] = func(f *Fake, p map[string]any) (any, *rpcErr) {
		r := f.raids[str(p, "name")]
		switch {
		case r == nil:
			return nil, errf(codeENODEV, "raid bdev %s is not found in config", str(p, "name"))
		case r.online || r.deleting:
			return nil, errf(-22, "raid bdev '%s' is not configuring", r.Name)
		case r.sb == nil:
			return nil, errf(-22, "raid bdev '%s' has no superblock", r.Name)
		case r.discovered() == r.operational:
			return nil, errf(-114, "raid bdev '%s' is not missing any base bdev", r.Name)
		case r.discovered() < minOperational(r.Level, len(r.Members)):
			return nil, errf(codeENODEV, "raid bdev '%s' needs at least %d base bdevs, %d present",
				r.Name, minOperational(r.Level, len(r.Members)), r.discovered())
		}
		for i, m := range r.Members {
			if m == "" && r.sb.Slots[i].State == sbConfigured {
				r.sb.Slots[i].State = sbMissing
			}
		}
		r.operational = r.discovered()
		f.goOnlineLocked(r)
		return true, nil
	}

	handlers["framework_get_config"] = func(f *Fake, p map[string]any) (any, *rpcErr) {
		if str(p, "name") != "bdev" {
			return []any{}, nil
		}
		return []map[string]any{{
			"method": "bdev_raid_set_options",
			"params": map[string]any{"process_window_size_kb": 1024, "process_max_bandwidth_mb_sec": f.rebuildMaxMBps},
		}}, nil
	}

	handlers["bdev_raid_set_options"] = func(f *Fake, p map[string]any) (any, *rpcErr) {
		f.rebuildMaxMBps = uint32(num(p, "process_max_bandwidth_mb_sec"))
		return true, nil
	}
}

// RebuildMaxMBps returns the rebuild bandwidth limit last set with bdev_raid_set_options.
func (f *Fake) RebuildMaxMBps() uint32 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rebuildMaxMBps
}
