//go:build kernelintegration

// Integration tests for the kernel data plane against real md, LVM, device-mapper, nvmet and LIO.
// They need root, the kernel modules and the tools (mdadm, lvm2, device-mapper-persistent-data,
// nvme-cli, iscsi-initiator-utils); the lab VM and CI's privileged job provide them. Disks are
// sparse files on loop devices (dm-dust on top where a bad sector is needed). Every object gets a
// unique name and is removed afterwards.
//
//	go test -c -tags kernelintegration -o kernel.test ./internal/kernel && sudo ./kernel.test -test.v
package kernel

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

const (
	mib      = 1 << 20
	hostNQN  = "nqn.2026-11.io.velocor.test:host1"
	nvmePort = 14420
	iscsiPt  = "127.0.0.1:3260" // SELinux lets iscsid connect only to iSCSI-labelled ports (3260, 3205)
)

type env struct {
	t   *testing.T
	p   *Plane
	ctx context.Context
	id  string
	dir string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	dir := os.Getenv("VELOCOR_IT_DIR")
	if dir == "" {
		dir = "/var/tmp/velocor-kit"
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, p: New(Exec{}), ctx: context.Background(), id: fmt.Sprintf("kit%06d", rand.IntN(1000000)), dir: dir}
	if err := e.p.LoadTargetModules(e.ctx); err != nil {
		t.Fatal(err)
	}
	_, _ = e.p.Run.Run(e.ctx, "modprobe", "-a", "dm_dust", "nvme_tcp", "iscsi_tcp")
	return e
}

func (e *env) run(name string, args ...string) string {
	e.t.Helper()
	out, err := e.p.Run.Run(e.ctx, name, args...)
	if err != nil {
		e.t.Fatalf("%v", err)
	}
	return strings.TrimSpace(string(out))
}

// loop returns a loop device on a new sparse file of size bytes.
func (e *env) loop(size int64) string {
	e.t.Helper()
	f := filepath.Join(e.dir, fmt.Sprintf("%s-%d.img", e.id, rand.IntN(1<<30)))
	if err := os.WriteFile(f, nil, 0o600); err != nil {
		e.t.Fatal(err)
	}
	if err := os.Truncate(f, size); err != nil {
		e.t.Fatal(err)
	}
	dev := e.run("losetup", "--find", "--show", f)
	e.t.Cleanup(func() {
		_, _ = e.p.Run.Run(e.ctx, "losetup", "-d", dev)
		_ = os.Remove(f)
	})
	return dev
}

// dust returns a dm-dust device over dev, which fails reads of the sectors it is told are bad
// (and forgets one once it is written, as a drive remaps a sector).
func (e *env) dust(dev string) string {
	e.t.Helper()
	name := fmt.Sprintf("%s-dust%d", e.id, rand.IntN(1<<20))
	sz := e.run("blockdev", "--getsz", dev)
	e.run("dmsetup", "create", name, "--table", "0 "+sz+" dust "+dev+" 0 512")
	e.t.Cleanup(func() { _, _ = e.p.Run.Run(e.ctx, "dmsetup", "remove", "--retry", name) })
	return "/dev/mapper/" + name
}

type pool struct {
	e       *env
	name    string // array
	vg      string
	thin    string
	uuid    string
	devices []string
	stopped bool
}

// newPool builds the production layout on devices: md RAID1, a VG, a thin pool.
func (e *env) newPool(devices ...string) *pool {
	e.t.Helper()
	pl := &pool{e: e, name: e.id, vg: "vg" + e.id, thin: "thin", devices: devices}
	uuid, err := e.p.CreateArray(e.ctx, ArraySpec{Name: pl.name, Level: "raid1", Devices: devices, AssumeClean: true})
	if err != nil {
		e.t.Fatal(err)
	}
	pl.uuid = uuid
	e.t.Cleanup(pl.destroy)
	if err := e.p.CreatePool(e.ctx, PoolSpec{VG: pl.vg, Device: e.p.ArrayPath(pl.name), Thin: pl.thin, DataPercent: 90}); err != nil {
		e.t.Fatal(err)
	}
	return pl
}

func (pl *pool) destroy() {
	p, ctx := pl.e.p, pl.e.ctx
	if !pl.stopped {
		_ = p.RemovePool(ctx, pl.vg, p.ArrayPath(pl.name))
		_ = p.StopArray(ctx, pl.name)
	}
	for _, d := range pl.devices {
		_, _ = p.Run.Run(ctx, "mdadm", "--zero-superblock", d)
	}
}

// volume creates a thin volume with its wrapper and returns the wrapper's device.
func (pl *pool) volume(name string, size uint64) string {
	e := pl.e
	e.t.Helper()
	if err := e.p.CreateVolume(e.ctx, pl.vg, pl.thin, name, size, true); err != nil {
		e.t.Fatal(err)
	}
	if err := e.p.EnsureWrapper(e.ctx, name, e.p.LVPath(pl.vg, name)); err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { _ = e.p.RemoveWrapper(e.ctx, name) })
	return e.p.WrapperPath(name)
}

func (pl *pool) lv(name string) LV {
	pl.e.t.Helper()
	lvs, err := pl.e.p.ListVolumes(pl.e.ctx, pl.vg)
	if err != nil {
		pl.e.t.Fatal(err)
	}
	for _, lv := range lvs {
		if lv.Name == name {
			return lv
		}
	}
	pl.e.t.Fatalf("no LV %s", name)
	return LV{}
}

// ---- direct I/O helpers ------------------------------------------------------------------------

func alignedBuf(t *testing.T, n int) []byte {
	t.Helper()
	b, err := unix.Mmap(-1, 0, n, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_ANON|unix.MAP_PRIVATE)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Munmap(b) })
	return b
}

// pattern fills b with data unique to (seed, offset).
func pattern(b []byte, seed, off uint64) {
	for i := 0; i < len(b); i += 8 {
		binary.LittleEndian.PutUint64(b[i:], (off+uint64(i))*0x9E3779B97F4A7C15^seed)
	}
}

func writeAt(dev string, off int64, b []byte) error {
	f, err := os.OpenFile(dev, os.O_WRONLY|syscall.O_DIRECT, 0)
	if err != nil {
		return err
	}
	if _, err := f.WriteAt(b, off); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func readAt(dev string, off int64, b []byte) error {
	f, err := os.OpenFile(dev, os.O_RDONLY|syscall.O_DIRECT, 0)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	_, err = f.ReadAt(b, off)
	return err
}

// fill writes a pattern over [0,size) of dev in 1 MiB writes.
func fill(t *testing.T, dev string, size int64, seed uint64) {
	t.Helper()
	b := alignedBuf(t, mib)
	for off := int64(0); off < size; off += mib {
		pattern(b, seed, uint64(off))
		if err := writeAt(dev, off, b); err != nil {
			t.Fatalf("write %s at %d: %v", dev, off, err)
		}
	}
}

// verify checks the pattern over [0,size) of dev.
func verify(t *testing.T, dev string, size int64, seed uint64) {
	t.Helper()
	b, want := alignedBuf(t, mib), make([]byte, mib)
	for off := int64(0); off < size; off += mib {
		if err := readAt(dev, off, b); err != nil {
			t.Fatalf("read %s at %d: %v", dev, off, err)
		}
		pattern(want, seed, uint64(off))
		if !bytes.Equal(b, want) {
			t.Fatalf("%s: data differs at %d", dev, off)
		}
	}
}

func sum(t *testing.T, dev string, size int64) [32]byte {
	t.Helper()
	b := alignedBuf(t, mib)
	h := sha256.New()
	for off := int64(0); off < size; off += mib {
		if err := readAt(dev, off, b); err != nil {
			t.Fatalf("read %s at %d: %v", dev, off, err)
		}
		h.Write(b)
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	end := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(end) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// ---- initiators --------------------------------------------------------------------------------

// nvmeConnect connects to an export as hostNQN (or another host) and returns the namespace device.
func (e *env) nvmeConnect(nqn, host string) (string, error) {
	_, err := e.p.Run.Run(e.ctx, "nvme", "connect", "-t", "tcp", "-a", "127.0.0.1", "-s", strconv.Itoa(nvmePort),
		"-n", nqn, "--hostnqn", host, "--hostid", "8b0b9c58-7d2a-4f3e-9c1d-"+fmt.Sprintf("%012d", rand.IntN(1e12)))
	if err != nil {
		return "", err
	}
	e.t.Cleanup(func() { _, _ = e.p.Run.Run(e.ctx, "nvme", "disconnect", "-n", nqn) })
	nsRe := regexp.MustCompile(`^nvme\d+n\d+$`)
	var dev string
	waitFor(e.t, "the NVMe namespace", 15*time.Second, func() bool {
		subs, _ := filepath.Glob("/sys/class/nvme-subsystem/*")
		for _, s := range subs {
			if readAttr(filepath.Join(s, "subsysnqn")) != nqn {
				continue
			}
			for _, n := range dirNames(s) {
				if nsRe.MatchString(n) {
					dev = "/dev/" + n
					_, err := os.Stat(dev) // udev may not have made the node yet
					return err == nil
				}
			}
		}
		return false
	})
	return dev, nil
}

// iscsiLogin logs in to an export as initiator and returns the LUN's device.
func (e *env) iscsiLogin(iqn, initiator string) (string, error) {
	e.run("systemctl", "start", "iscsid")
	iface := fmt.Sprintf("%s-%d", e.id, rand.IntN(1000))
	e.run("iscsiadm", "-m", "iface", "-I", iface, "-o", "new")
	e.t.Cleanup(func() { _, _ = e.p.Run.Run(e.ctx, "iscsiadm", "-m", "iface", "-I", iface, "-o", "delete") })
	e.run("iscsiadm", "-m", "iface", "-I", iface, "-o", "update", "-n", "iface.initiatorname", "-v", initiator)
	e.run("iscsiadm", "-m", "discovery", "-t", "st", "-p", iscsiPt, "-I", iface)
	e.t.Cleanup(func() {
		_, _ = e.p.Run.Run(e.ctx, "iscsiadm", "-m", "node", "-T", iqn, "-p", iscsiPt, "-I", iface, "-u")
		_, _ = e.p.Run.Run(e.ctx, "iscsiadm", "-m", "node", "-T", iqn, "-p", iscsiPt, "-I", iface, "-o", "delete")
	})
	for _, kv := range [][2]string{{"node.conn[0].timeo.login_timeout", "10"}, {"node.session.initial_login_retry_max", "1"}} {
		e.run("iscsiadm", "-m", "node", "-T", iqn, "-p", iscsiPt, "-I", iface, "-o", "update", "-n", kv[0], "-v", kv[1])
	}
	if _, err := e.p.Run.Run(e.ctx, "iscsiadm", "-m", "node", "-T", iqn, "-p", iscsiPt, "-I", iface, "-l"); err != nil {
		return "", err
	}
	link := "/dev/disk/by-path/ip-" + iscsiPt + "-iscsi-" + iqn + "-lun-0"
	var dev string
	waitFor(e.t, "the iSCSI LUN", 15*time.Second, func() bool {
		d, err := filepath.EvalSymlinks(link)
		dev = d
		return err == nil
	})
	return dev, nil
}

// ---- tests -------------------------------------------------------------------------------------

// A volume's data, written through its wrapper, reads back over NVMe/TCP and iSCSI; data written
// over iSCSI reads back through the wrapper; hosts not allowed are refused by both targets.
func TestExportsNVMeAndISCSI(t *testing.T) {
	e := newEnv(t)
	pl := e.newPool(e.loop(1<<30), e.loop(1<<30))
	const size = 64 * mib
	dev := pl.volume("v1", size)
	fill(t, dev, size, 1)

	nqn := "nqn.2026-11.io.velocor.test:" + e.id
	if err := e.p.EnsureNVMePort(NVMePort{ID: 9, Address: "127.0.0.1", Port: nvmePort}); err != nil {
		t.Fatal(err)
	}
	exp := NVMeExport{NQN: nqn, Serial: e.id, UUID: "6c1a0000-0000-4000-8000-" + fmt.Sprintf("%012d", rand.IntN(1e12)), Device: dev, Hosts: []string{hostNQN}, Ports: []int{9}}
	if err := e.p.EnsureNVMeExport(exp); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.p.RemoveNVMeExport(nqn); _ = e.p.RemoveNVMePort(9) })
	if err := e.p.EnsureNVMeExport(exp); err != nil { // idempotent
		t.Fatalf("second ensure: %v", err)
	}
	if _, err := e.nvmeConnect(nqn, "nqn.2026-11.io.velocor.test:intruder"); err == nil {
		t.Fatal("a host not allowed connected")
	}
	ndev, err := e.nvmeConnect(nqn, hostNQN)
	if err != nil {
		t.Fatal(err)
	}
	verify(t, ndev, size, 1)
	if got := readAttr("/sys/block/" + filepath.Base(ndev) + "/uuid"); got != exp.UUID {
		t.Errorf("namespace UUID %q, want %q", got, exp.UUID)
	}

	iqn := "iqn.2026-11.io.velocor.test:" + e.id
	iexp := ISCSIExport{IQN: iqn, Backstore: e.id, Serial: e.id, Device: dev, Portals: []string{iscsiPt}, Initiators: []string{"iqn.2026-11.io.velocor.test:init1"}}
	if err := e.p.EnsureISCSIExport(iexp); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := e.p.RemoveISCSIExport(iqn, e.id); err != nil {
			t.Errorf("remove iSCSI export: %v", err)
		}
	})
	if err := e.p.EnsureISCSIExport(iexp); err != nil {
		t.Fatalf("second ensure: %v", err)
	}
	if _, err := e.iscsiLogin(iqn, "iqn.2026-11.io.velocor.test:intruder"); err == nil {
		t.Fatal("an initiator not allowed logged in")
	}
	idev, err := e.iscsiLogin(iqn, "iqn.2026-11.io.velocor.test:init1")
	if err != nil {
		t.Fatal(err)
	}
	verify(t, idev, size, 1)
	fill(t, idev, 8*mib, 2)
	verify(t, dev, 8*mib, 2)
	verify(t, ndev, 8*mib, 2)
}

// After a reboot (exports, wrappers, volume group and array all gone from the kernel), the
// desired state is applied again: the array assembles from its members, the volumes and their
// snapshots are intact, and the export serves the same data.
func TestRestoreAfterReboot(t *testing.T) {
	e := newEnv(t)
	devs := []string{e.loop(1 << 30), e.loop(1 << 30)}
	pl := e.newPool(devs...)
	const size = 64 * mib
	dev := pl.volume("v1", size)
	fill(t, dev, size, 3)
	if err := e.p.Snapshot(e.ctx, pl.vg, "v1", "s1"); err != nil {
		t.Fatal(err)
	}
	fill(t, dev, 4*mib, 4)
	before := sum(t, dev, size)
	nqn := "nqn.2026-11.io.velocor.test:" + e.id
	exp := NVMeExport{NQN: nqn, Device: dev, Hosts: []string{hostNQN}, Ports: []int{9}}
	if err := e.p.EnsureNVMePort(NVMePort{ID: 9, Address: "127.0.0.1", Port: nvmePort}); err != nil {
		t.Fatal(err)
	}
	if err := e.p.EnsureNVMeExport(exp); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.p.RemoveNVMeExport(nqn); _ = e.p.RemoveNVMePort(9) })

	// "Reboot": nothing is left in the kernel but the disks.
	if err := e.p.RemoveNVMeExport(nqn); err != nil {
		t.Fatal(err)
	}
	if err := e.p.RemoveWrapper(e.ctx, "v1"); err != nil {
		t.Fatal(err)
	}
	if err := e.p.DeactivatePool(e.ctx, pl.vg); err != nil {
		t.Fatal(err)
	}
	if err := e.p.StopArray(e.ctx, pl.name); err != nil {
		t.Fatal(err)
	}

	// Restore from desired state.
	if err := e.p.AssembleArray(e.ctx, pl.name, pl.uuid, devs, false); err != nil {
		t.Fatal(err)
	}
	if err := e.p.ActivatePool(e.ctx, pl.vg); err != nil {
		t.Fatal(err)
	}
	if lv := pl.lv("s1"); lv.Active {
		t.Error("a snapshot was activated by the pool's activation")
	}
	if err := e.p.EnsureWrapper(e.ctx, "v1", e.p.LVPath(pl.vg, "v1")); err != nil {
		t.Fatal(err)
	}
	if err := e.p.EnsureNVMeExport(exp); err != nil {
		t.Fatal(err)
	}
	ndev, err := e.nvmeConnect(nqn, hostNQN)
	if err != nil {
		t.Fatal(err)
	}
	if sum(t, ndev, size) != before {
		t.Fatal("data differs after the restore")
	}
	if err := e.p.ActivateLV(e.ctx, pl.vg, "s1", true); err != nil {
		t.Fatal(err)
	}
	verify(t, e.p.LVPath(pl.vg, "s1"), size, 3)
}

// A failed member is replaced and rebuilt while the volume is written; then the original
// survivor fails too and the data, written before and during the degraded period, is read from
// the rebuilt member alone.
func TestRebuildOntoReplacement(t *testing.T) {
	e := newEnv(t)
	a, b, c := e.loop(1<<30), e.loop(1<<30), e.loop(1<<30)
	pl := e.newPool(a, b)
	pl.devices = append(pl.devices, c)
	const size = 256 * mib
	dev := pl.volume("v1", size)
	fill(t, dev, size, 5)
	if err := e.p.FailMember(e.ctx, pl.name, b); err != nil {
		t.Fatal(err)
	}
	if err := e.p.RemoveMember(e.ctx, pl.name, b); err != nil {
		t.Fatal(err)
	}
	st, _ := e.p.ArrayStatus(pl.name)
	if st.Degraded != 1 {
		t.Fatalf("not degraded after a failure: %+v", st)
	}
	fill(t, dev, 64*mib, 6) // written while degraded
	if err := e.p.SetSyncSpeedMax(pl.name, "20000"); err != nil {
		t.Fatal(err)
	}
	if err := e.p.AddMember(e.ctx, pl.name, c); err != nil {
		t.Fatal(err)
	}
	var sawProgress bool
	waitFor(t, "the rebuild", 5*time.Minute, func() bool {
		st, err := e.p.ArrayStatus(pl.name)
		if err != nil {
			return false
		}
		if st.SyncAction == "recover" && st.SyncTotal > 0 && st.SyncDone > 0 {
			sawProgress = true
		}
		return st.Degraded == 0 && st.SyncAction == "idle"
	})
	_ = e.p.SetSyncSpeedMax(pl.name, "system")
	if !sawProgress {
		t.Error("never saw rebuild progress in sysfs")
	}
	if err := e.p.FailMember(e.ctx, pl.name, a); err != nil {
		t.Fatal(err)
	}
	if err := e.p.RemoveMember(e.ctx, pl.name, a); err != nil {
		t.Fatal(err)
	}
	verify(t, dev, 64*mib, 6)
	b2 := alignedBuf(t, mib)
	want := make([]byte, mib)
	for off := int64(64 * mib); off < size; off += mib {
		if err := readAt(dev, off, b2); err != nil {
			t.Fatal(err)
		}
		pattern(want, 5, uint64(off))
		if !bytes.Equal(b2, want) {
			t.Fatalf("data from before the failure differs at %d", off)
		}
	}
}

// Group snapshots of two volumes taken while a writer updates both with dependent writes (n to A,
// then once that completed n to B) are one instant: B = A or B = A-1 in every pair.
func TestGroupSnapshotConsistent(t *testing.T) {
	e := newEnv(t)
	pl := e.newPool(e.loop(1<<30), e.loop(1<<30))
	da, db := pl.volume("va", 16*mib), pl.volume("vb", 16*mib)

	var stop atomic.Bool
	var writes atomic.Int64
	var werr error
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		b := alignedBuf(t, 4096)
		fa, err := os.OpenFile(da, os.O_WRONLY|syscall.O_DIRECT, 0)
		if err != nil {
			werr = err
			return
		}
		defer func() { _ = fa.Close() }()
		fb, err := os.OpenFile(db, os.O_WRONLY|syscall.O_DIRECT, 0)
		if err != nil {
			werr = err
			return
		}
		defer func() { _ = fb.Close() }()
		for n := uint64(1); !stop.Load(); n++ {
			binary.LittleEndian.PutUint64(b, n)
			if _, err := fa.WriteAt(b, 0); err != nil {
				werr = err
				return
			}
			if _, err := fb.WriteAt(b, 0); err != nil {
				werr = err
				return
			}
			writes.Add(1)
		}
	}()
	time.Sleep(300 * time.Millisecond)
	var durations []time.Duration
	for i := 1; i <= 8; i++ {
		sa, sb := fmt.Sprintf("va-g%d", i), fmt.Sprintf("vb-g%d", i)
		ctx, cancel := context.WithTimeout(e.ctx, 10*time.Second)
		start := time.Now()
		err := e.p.GroupSnapshot(ctx, []GroupMember{{Volume: "va", VG: pl.vg, LV: "va", Snap: sa}, {Volume: "vb", VG: pl.vg, LV: "vb", Snap: sb}})
		durations = append(durations, time.Since(start))
		cancel()
		if err != nil {
			stop.Store(true)
			wg.Wait()
			t.Fatalf("group snapshot %d: %v", i, err)
		}
		time.Sleep(200 * time.Millisecond)
	}
	stop.Store(true)
	wg.Wait()
	if werr != nil {
		t.Fatalf("the writer failed: %v", werr)
	}
	for _, v := range []string{"va", "vb"} {
		if s, _ := e.p.WrapperSuspended(e.ctx, v); s {
			t.Errorf("wrapper %s left suspended", v)
		}
	}
	b := alignedBuf(t, 4096)
	read := func(lv string) uint64 {
		if err := e.p.ActivateLV(e.ctx, pl.vg, lv, true); err != nil {
			t.Fatal(err)
		}
		if err := readAt(e.p.LVPath(pl.vg, lv), 0, b); err != nil {
			t.Fatal(err)
		}
		return binary.LittleEndian.Uint64(b)
	}
	for i := 1; i <= 8; i++ {
		na, nb := read(fmt.Sprintf("va-g%d", i)), read(fmt.Sprintf("vb-g%d", i))
		t.Logf("group %d: A=%d B=%d (%v)", i, na, nb, durations[i-1].Round(time.Millisecond))
		if nb != na && nb+1 != na {
			t.Errorf("group %d inconsistent: A=%d B=%d", i, na, nb)
		}
		if na == 0 {
			t.Errorf("group %d caught nothing written", i)
		}
	}
	t.Logf("%d dependent write pairs, no errors", writes.Load())
}

// Changed and allocated blocks from the pool's own mappings are exactly the chunks written.
func TestChangedBlocksExact(t *testing.T) {
	e := newEnv(t)
	pl := e.newPool(e.loop(1<<30), e.loop(1<<30))
	const size = 64 * mib
	dev := pl.volume("v1", size)
	chunk := int64(ChunkSize)
	b := alignedBuf(t, 4096)
	write := func(off int64) {
		pattern(b, uint64(off), 0)
		if err := writeAt(dev, off, b); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []int64{0, 1, 2, 100, 1023} {
		write(c * chunk)
	}
	if err := e.p.Snapshot(e.ctx, pl.vg, "v1", "s1"); err != nil {
		t.Fatal(err)
	}
	write(1*chunk + 8192) // overwrite inside a written chunk
	write(500 * chunk)    // new chunk
	write(501*chunk + 4096)
	if err := e.p.Snapshot(e.ctx, pl.vg, "v1", "s2"); err != nil {
		t.Fatal(err)
	}
	s1, s2 := pl.lv("s1"), pl.lv("s2")
	got, err := e.p.ChangedBlocks(e.ctx, pl.vg, pl.thin, s1.ThinID, s2.ThinID, size)
	if err != nil {
		t.Fatal(err)
	}
	want := []Extent{{uint64(chunk), uint64(chunk)}, {uint64(500 * chunk), uint64(2 * chunk)}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("changed %v, want %v", got, want)
	}
	all, err := e.p.AllocatedBlocks(e.ctx, pl.vg, pl.thin, s1.ThinID, size)
	if err != nil {
		t.Fatal(err)
	}
	wantAll := []Extent{{0, uint64(3 * chunk)}, {uint64(100 * chunk), uint64(chunk)}, {uint64(1023 * chunk), uint64(chunk)}}
	if fmt.Sprint(all) != fmt.Sprint(wantAll) {
		t.Errorf("allocated %v, want %v", all, wantAll)
	}
}

// A full pool fails writes with ENOSPC at once (dm-thin's error_if_no_space; TODO.md L-6) rather
// than queueing them.
func TestFullPoolGivesENOSPC(t *testing.T) {
	e := newEnv(t)
	pl := e.newPool(e.loop(256*mib), e.loop(256*mib))
	dev := pl.volume("v1", 512*mib) // twice what the pool holds
	b := alignedBuf(t, mib)
	var off int64
	start := time.Now()
	var err error
	for off = 0; off < 512*mib; off += mib {
		if err = writeAt(dev, off, b); err != nil {
			break
		}
	}
	if !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("write at %d MiB: %v, want ENOSPC", off/mib, err)
	}
	t.Logf("ENOSPC at %d MiB after %v", off/mib, time.Since(start).Round(time.Millisecond))
	if lv := pl.lv(pl.thin); lv.DataPercent < 99 {
		t.Errorf("pool %.1f%% full when writes failed", lv.DataPercent)
	}
}

// memberSector maps a byte offset of a thin volume to the sector on an md member holding it: the
// member's data offset, the PV's data start, the thin pool data LV's first extent, the chunk the
// volume's block is mapped to.
func (pl *pool) memberSector(t *testing.T, volume string, off uint64, memberKernel string) uint64 {
	e := pl.e
	st, err := e.p.ArrayStatus(pl.name)
	if err != nil {
		t.Fatal(err)
	}
	dataOffset, _ := strconv.ParseUint(readAttr(filepath.Join(e.p.SysRoot, "block", st.Kernel, "md", "dev-"+memberKernel, "offset")), 10, 64)
	peStart, _ := strconv.ParseUint(strings.TrimSuffix(e.run("lvm", "pvs", "--config", lvmConfig, "--noheadings", "--units", "s", "--nosuffix", "-o", "pe_start", e.p.ArrayPath(pl.name)), "S"), 10, 64)
	extent, _ := strconv.ParseUint(e.run("lvm", "vgs", "--config", lvmConfig, "--noheadings", "--units", "s", "--nosuffix", "-o", "vg_extent_size", pl.vg), 10, 64)
	ranges := e.run("lvm", "lvs", "--config", lvmConfig, "-a", "--noheadings", "-o", "seg_pe_ranges", pl.vg+"/"+pl.thin+"_tdata")
	m := regexp.MustCompile(`:(\d+)-\d+`).FindStringSubmatch(ranges)
	if m == nil || strings.Count(ranges, ":") != 1 {
		t.Fatalf("thin pool data in one segment expected, got %q", ranges)
	}
	firstPE, _ := strconv.ParseUint(m[1], 10, 64)
	// The volume's mapping for its chunk.
	thinID := pl.lv(volume).ThinID
	var dataBlock uint64
	found := false
	err = e.p.withMetadataSnap(e.ctx, pl.vg, pl.thin, func(meta string) error {
		out, err := e.p.Run.Run(e.ctx, "thin_dump", "--metadata-snap", "--dev-id", strconv.Itoa(thinID), meta)
		if err != nil {
			return err
		}
		chunk := off / ChunkSize
		for _, mm := range regexp.MustCompile(`origin_begin="(\d+)" data_begin="(\d+)" length="(\d+)"`).FindAllStringSubmatch(string(out), -1) {
			ob, _ := strconv.ParseUint(mm[1], 10, 64)
			db, _ := strconv.ParseUint(mm[2], 10, 64)
			n, _ := strconv.ParseUint(mm[3], 10, 64)
			if chunk >= ob && chunk < ob+n {
				dataBlock, found = db+(chunk-ob), true
			}
		}
		for _, mm := range regexp.MustCompile(`origin_block="(\d+)" data_block="(\d+)"`).FindAllStringSubmatch(string(out), -1) {
			ob, _ := strconv.ParseUint(mm[1], 10, 64)
			db, _ := strconv.ParseUint(mm[2], 10, 64)
			if chunk == ob {
				dataBlock, found = db, true
			}
		}
		return nil
	})
	if err != nil || !found {
		t.Fatalf("no mapping for offset %d: %v", off, err)
	}
	return dataOffset + peStart + firstPE*extent + dataBlock*(ChunkSize/512) + (off%ChunkSize)/512
}

// TODO.md P0-7: with both copies, a bad sector on one is read from the other and rewritten (the
// drive forgets it); on the last copy of a degraded mirror, only the reads touching it fail: the
// array, the pool and every other block stay up, and writing the block again heals it.
func TestLastCopyReadErrorKeepsThePool(t *testing.T) {
	e := newEnv(t)
	d0, d1 := e.dust(e.loop(512*mib)), e.dust(e.loop(512*mib))
	pl := e.newPool(d0, d1)
	const size = 128 * mib
	dev := pl.volume("v1", size)
	fill(t, dev, size, 7)
	st, _ := e.p.ArrayStatus(pl.name)
	kernel0 := ""
	for _, m := range st.Members {
		if m.Slot == 0 {
			kernel0 = m.Device
		}
	}
	dust0 := strings.TrimPrefix(d0, "/dev/mapper/")
	b := alignedBuf(t, 4096)

	// Both copies: a bad sector on member 0 at 32 MiB is served from member 1 and repaired.
	bad := pl.memberSector(t, "v1", 32*mib, kernel0)
	e.run("dmsetup", "message", dust0, "0", "addbadblock", strconv.FormatUint(bad, 10))
	e.run("dmsetup", "message", dust0, "0", "enable")
	for i := 0; i < 4; i++ { // the read balancer may pick either member
		if err := readAt(dev, 32*mib, b); err != nil {
			t.Fatalf("read with a good copy available: %v", err)
		}
	}
	verify(t, dev, size, 7)
	if st, _ := e.p.ArrayStatus(pl.name); st.Degraded != 0 {
		t.Fatalf("a repairable read error degraded the array: %+v", st)
	}

	// The last copy: member 1 fails, then member 0 can't read a sector at 64 MiB.
	if err := e.p.FailMember(e.ctx, pl.name, d1); err != nil {
		t.Fatal(err)
	}
	if err := e.p.RemoveMember(e.ctx, pl.name, d1); err != nil {
		t.Fatal(err)
	}
	bad = pl.memberSector(t, "v1", 64*mib, kernel0)
	e.run("dmsetup", "message", dust0, "0", "addbadblock", strconv.FormatUint(bad, 10))
	err := readAt(dev, 64*mib, b)
	if !errors.Is(err, syscall.EIO) {
		t.Fatalf("read of the unreadable sector: %v, want EIO", err)
	}
	if err := readAt(dev, 64*mib+ChunkSize, b); err != nil {
		t.Fatalf("the next chunk is unreadable too: %v", err)
	}
	st, err = e.p.ArrayStatus(pl.name)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("after the unrecoverable read: array_state=%s degraded=%d members=%+v", st.ArrayState, st.Degraded, st.Members)
	if st.ArrayState == "inactive" || st.ArrayState == "broken" || len(st.Members) != 1 || st.Members[0].Has("faulty") {
		t.Fatalf("the array went down: %+v", st)
	}
	if lv := pl.lv("v1"); !lv.Active {
		t.Error("the volume was deactivated")
	}
	// Everything else is readable and writable.
	for off := int64(0); off < size; off += mib {
		if off == 64*mib {
			continue
		}
		if err := readAt(dev, off, b); err != nil {
			t.Fatalf("read at %d after the error: %v", off, err)
		}
	}
	fill(t, dev, 16*mib, 8)
	verify(t, dev, 16*mib, 8)
	// Writing the lost block again (a restore from backup) makes it readable, as a drive remaps
	// a sector it is asked to write.
	pattern(b, 9, 0)
	if err := writeAt(dev, 64*mib, b); err != nil {
		t.Fatalf("rewrite of the lost block: %v", err)
	}
	got := alignedBuf(t, 4096)
	if err := readAt(dev, 64*mib, got); err != nil || !bytes.Equal(got, b) {
		t.Fatalf("lost block after the rewrite: %v", err)
	}
}
