//go:build kernelintegration

// Integration tests for the kernel data plane as velod sees it (dataplane.Plane), against real
// md, LVM, device-mapper, nvmet and LIO on loop devices. Root, the kernel modules and the tools
// are needed (the lab VM, CI's kernel job). Every object gets a unique name and is removed.
//
//	go test -c -tags kernelintegration -o kernelplane.test ./internal/dataplane/kernelplane && sudo ./kernelplane.test -test.v
package kernelplane

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/velocor/velocor/internal/dataplane"
	"github.com/velocor/velocor/internal/kernel"
	"github.com/velocor/velocor/internal/store"
)

const (
	mib      = 1 << 20
	hostNQN  = "nqn.2026-11.io.velocor.test:kp-host"
	nvmePort = 14421
	iscsiIP  = "127.0.0.1"
	iscsiPt  = "127.0.0.1:3260" // SELinux: iscsid connects only to iSCSI-labelled ports
)

type env struct {
	t     *testing.T
	p     *Plane
	k     *kernel.Plane
	ctx   context.Context
	id    string
	dir   string
	array string
	vg    string
	poolU string
	disks []string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	dir := "/var/tmp/velocor-kpit"
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	k := kernel.New(kernel.Exec{})
	e := &env{t: t, k: k, p: New(k, nil), ctx: context.Background(), id: fmt.Sprintf("kp%06d", rand.IntN(1000000)), dir: dir}
	e.p.StateDir = t.TempDir()
	e.array, e.vg = "raid_pool-"+e.id, "lvs_pool-"+e.id
	_, _ = k.Run.Run(e.ctx, "modprobe", "-a", "nvme_tcp", "iscsi_tcp")
	return e
}

func (e *env) run(name string, args ...string) string {
	e.t.Helper()
	out, err := e.k.Run.Run(e.ctx, name, args...)
	if err != nil {
		e.t.Fatalf("%v", err)
	}
	return strings.TrimSpace(string(out))
}

// disk attaches a loop device on a new sparse file through Plane.AttachDisk and returns its
// handle and backing file.
func (e *env) disk(size int64) (string, string) {
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
		_, _ = e.k.Run.Run(e.ctx, "losetup", "-d", dev)
		_ = os.Remove(f)
	})
	h, st, err := e.p.AttachDisk(e.ctx, &store.Disk{ID: "d-" + filepath.Base(dev), Type: store.DiskTypeSSD, DevicePath: dev})
	if err != nil || st != store.DiskDriverStateKernel {
		e.t.Fatalf("attach %s: %q %q %v", dev, h, st, err)
	}
	return h, f
}

// pool builds a pool the way velod does: an array of two disks and a thin pool on it.
func (e *env) pool() {
	e.t.Helper()
	a, _ := e.disk(1 << 30)
	b, _ := e.disk(1 << 30)
	e.disks = []string{a, b}
	if err := e.p.CreateArray(e.ctx, e.array, store.RaidLevel1, 0, e.disks); err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(e.destroy)
	u, err := e.p.CreateThinPool(e.ctx, e.array, e.vg, 1<<20)
	if err != nil {
		e.t.Fatal(err)
	}
	e.poolU = u
}

// destroy removes whatever the test left, ignoring errors.
func (e *env) destroy() {
	ctx := context.WithoutCancel(e.ctx)
	if lvs, err := e.k.ListVolumes(ctx, e.vg); err == nil {
		for _, lv := range lvs {
			_, _ = e.k.Run.Run(ctx, "dmsetup", "remove", "--retry", kernel.WrapperName(lv.Name))
		}
	}
	_, _ = e.k.Run.Run(ctx, "lvm", "vgremove", "--config", `devices { use_devicesfile = 0 filter = [ "a|^/dev/md|", "r|.*|" ] }`, "-q", "-f", e.vg)
	_ = e.p.DeleteArray(ctx, e.array)
}

func (e *env) volume(name string, size uint64) (string, *store.Volume) {
	e.t.Helper()
	u, err := e.p.CreateVolume(e.ctx, e.vg, name, size, true)
	if err != nil {
		e.t.Fatal(err)
	}
	return e.k.WrapperPath(name), &store.Volume{ID: name, UUID: u, PoolID: "pool-" + e.id, SizeBytes: size,
		Handle: e.vg + "/" + name, Device: e.vg + "/" + name}
}

func (e *env) arrayOf() dataplane.Array {
	e.t.Helper()
	arrays, err := e.p.Arrays(e.ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	for _, a := range arrays {
		if a.Name == e.array {
			return a
		}
	}
	e.t.Fatalf("array %s not listed", e.array)
	return dataplane.Array{}
}

// --- data helpers (O_DIRECT, so nothing is served from a cache) ---

func aligned(n int) []byte {
	b := make([]byte, n+4096)
	off := 4096 - int(uintptr(unsafe.Pointer(&b[0]))%4096)
	return b[off : off+n]
}

func pattern(b []byte, seed, off uint64) {
	for i := 0; i < len(b); i += 8 {
		binary.LittleEndian.PutUint64(b[i:], seed*0x9e3779b97f4a7c15^(off+uint64(i)))
	}
}

func rw(dev string, off int64, b []byte, write bool) error {
	flag := os.O_RDONLY
	if write {
		flag = os.O_WRONLY
	}
	f, err := os.OpenFile(dev, flag|syscall.O_DIRECT, 0)
	if err != nil {
		return err
	}
	if write {
		_, err = f.WriteAt(b, off)
	} else {
		_, err = f.ReadAt(b, off)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

func fill(t *testing.T, dev string, from, size int64, seed uint64) {
	t.Helper()
	b := aligned(mib)
	for off := from; off < from+size; off += mib {
		pattern(b, seed, uint64(off))
		if err := rw(dev, off, b, true); err != nil {
			t.Fatalf("write %s at %d: %v", dev, off, err)
		}
	}
}

func verify(t *testing.T, dev string, from, size int64, seed uint64) {
	t.Helper()
	b, want := aligned(mib), make([]byte, mib)
	for off := from; off < from+size; off += mib {
		if err := rw(dev, off, b, false); err != nil {
			t.Fatalf("read %s at %d: %v", dev, off, err)
		}
		pattern(want, seed, uint64(off))
		if !bytes.Equal(b, want) {
			t.Fatalf("%s: data differs at %d", dev, off)
		}
	}
}

func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(timeout); !cond(); time.Sleep(200 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// --- tests ---

// Volumes, snapshots, CBT, a group snapshot, a clone and a resize, all through the Plane.
func TestVolumeLifecycle(t *testing.T) {
	e := newEnv(t)
	e.pool()

	pools, err := e.p.ThinPools(e.ctx, e.vg, "")
	if err != nil || len(pools) != 1 {
		t.Fatalf("thin pools %v %v", pools, err)
	}
	if tp := pools[0]; tp.UUID != e.poolU || tp.Array != e.array || tp.ChunkBytes != kernel.ChunkSize || tp.TotalBytes == 0 || tp.FreeBytes != tp.TotalBytes {
		t.Fatalf("thin pool %+v", tp)
	}
	if a := e.arrayOf(); a.State != dataplane.ArrayOnline || a.Slots != 2 || a.Members[0].Name != e.disks[0] || a.Members[1].Name != e.disks[1] {
		t.Fatalf("array %+v", a)
	}

	const size = 64 * mib
	dev, vol := e.volume("vol-a", size)
	if !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(vol.UUID) {
		t.Fatalf("volume UUID %q is not RFC 4122", vol.UUID)
	}
	fill(t, dev, 0, 16*mib, 1)

	vols, err := e.p.Volumes(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(vols, func(v dataplane.Volume) bool { return v.UUID == vol.UUID })
	if i < 0 || vols[i].Pool != e.poolU || vols[i].Alias != e.vg+"/vol-a" || !vols[i].Thin || vols[i].Snapshot || vols[i].AllocatedBytes < 16*mib {
		t.Fatalf("volumes %+v", vols)
	}
	devs, err := e.p.Devices(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(devs, func(d dataplane.Device) bool { return d.Name == e.vg+"/vol-a" && slices.Contains(d.Aliases, vol.UUID) }) ||
		!slices.ContainsFunc(devs, func(d dataplane.Device) bool { return d.Name == e.array }) ||
		!slices.ContainsFunc(devs, func(d dataplane.Device) bool { return d.Name == e.disks[0] }) {
		t.Fatalf("devices miss the volume, array or disk: %+v", devs)
	}

	// Snapshots and CBT: allocated covers what was written; changed is exactly the rewrite.
	s1U, err := e.p.Snapshot(e.ctx, vol.UUID, "snap-1")
	if err != nil {
		t.Fatal(err)
	}
	s1 := &store.Snapshot{ID: "snap-1", UUID: s1U, VolumeID: "vol-a", SizeBytes: size}
	fill(t, dev, 32*mib, 2*mib, 2)
	s2U, err := e.p.Snapshot(e.ctx, e.vg+"/vol-a", "snap-2")
	if err != nil {
		t.Fatal(err)
	}
	s2 := &store.Snapshot{ID: "snap-2", UUID: s2U, VolumeID: "vol-a", SizeBytes: size}
	pool := &store.Pool{ThinPoolName: e.vg}
	alloc, err := e.p.AllocatedBlocks(e.ctx, pool, s1)
	if err != nil {
		t.Fatal(err)
	}
	if len(alloc) != 1 || alloc[0] != (dataplane.Extent{OffsetBytes: 0, SizeBytes: 16 * mib}) {
		t.Errorf("allocated %+v, want [0,16MiB)", alloc)
	}
	changed, err := e.p.ChangedBlocks(e.ctx, pool, s1, s2)
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 1 || changed[0] != (dataplane.Extent{OffsetBytes: 32 * mib, SizeBytes: 2 * mib}) {
		t.Errorf("changed %+v, want [32MiB,34MiB)", changed)
	}
	if _, err := e.p.ChangedBlocks(e.ctx, pool, s2, s1); !errors.Is(err, store.ErrInvalidArgument) {
		t.Errorf("changed blocks from the newer snapshot: %v", err)
	}
	vols, _ = e.p.Volumes(e.ctx)
	if i := slices.IndexFunc(vols, func(v dataplane.Volume) bool { return v.UUID == s1U }); i < 0 || !vols[i].Snapshot {
		t.Errorf("snapshot not listed as one: %+v", vols)
	}

	// A group snapshot of two volumes; a clone of the first snapshot reads its data.
	_, volB := e.volume("vol-b", size)
	gu, err := e.p.SnapshotGroup(e.ctx, []dataplane.GroupEntry{{Volume: vol.UUID, Snapshot: "gsnap-a"}, {Volume: volB.UUID, Snapshot: "gsnap-b"}}, 5*time.Second)
	if err != nil || len(gu) != 2 {
		t.Fatalf("group snapshot %v %v", gu, err)
	}
	if s, _ := e.k.WrapperSuspended(e.ctx, "vol-a"); s {
		t.Fatal("wrapper left suspended")
	}
	cU, err := e.p.Clone(e.ctx, s1U, "vol-clone")
	if err != nil {
		t.Fatal(err)
	}
	verify(t, e.k.WrapperPath("vol-clone"), 0, 16*mib, 1)
	// Snapshots are read-only, clones writable.
	if lvs, err := e.k.ListVolumes(e.ctx, e.vg); err == nil {
		for _, lv := range lvs {
			if (lv.Name == "snap-1" || lv.Name == "gsnap-a") && lv.Attr[1] != 'r' {
				t.Errorf("snapshot %s is writable (%s)", lv.Name, lv.Attr)
			}
			if lv.Name == "vol-clone" && lv.Attr[1] != 'w' {
				t.Errorf("the clone isn't writable (%s)", lv.Attr)
			}
		}
	}
	fill(t, e.k.WrapperPath("vol-clone"), 40*mib, mib, 8)
	b := aligned(mib)
	if err := rw(e.k.WrapperPath("vol-clone"), 32*mib, b, false); err != nil || !bytes.Equal(b, make([]byte, mib)) {
		t.Errorf("the clone has data written after its snapshot (%v)", err)
	}

	// Resize: LV and wrapper grow.
	if err := e.p.ResizeVolume(e.ctx, vol.UUID, 2*size); err != nil {
		t.Fatal(err)
	}
	if got := e.run("blockdev", "--getsize64", dev); got != strconv.Itoa(2*size) {
		t.Errorf("wrapper size %s after resize", got)
	}

	st, err := e.p.IOStats(e.ctx, e.vg+"/vol-a")
	if err != nil || len(st.Devices) != 1 || st.Devices[0].BytesWritten < 18*mib || st.TickRate != 1000 {
		t.Errorf("io stats %+v %v", st, err)
	}

	// Unmanaged: with nothing known, our array and VG are reported; with all known, nothing of ours.
	un, err := e.p.Unmanaged(e.ctx, dataplane.Known{})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(un, func(o dataplane.UnmanagedObject) bool { return o.Kind == "MD_ARRAY" && o.Name == e.array }) ||
		!slices.ContainsFunc(un, func(o dataplane.UnmanagedObject) bool { return o.Kind == "VOLUME_GROUP" && o.Name == e.vg }) {
		t.Errorf("unmanaged with nothing known: %+v", un)
	}
	known := dataplane.Known{ThinPools: map[string]bool{e.poolU: true}, Devices: map[string]bool{e.array: true},
		Volumes: map[string]bool{vol.UUID: true, volB.UUID: true, s1U: true, s2U: true, gu[0]: true, gu[1]: true, cU: true}}
	un, _ = e.p.Unmanaged(e.ctx, known)
	for _, o := range un {
		if strings.Contains(o.Name, e.id) {
			t.Errorf("reported although known: %+v", o)
		}
	}

	// Deleting: a missing volume is NotFound; everything else goes.
	for _, u := range []string{cU, gu[0], gu[1], s1U, s2U, volB.UUID, vol.UUID} {
		if err := e.p.DeleteVolume(e.ctx, u); err != nil {
			t.Fatalf("delete %s: %v", u, err)
		}
	}
	if err := e.p.DeleteVolume(e.ctx, vol.UUID); !dataplane.IsNotFound(err) {
		t.Errorf("deleting a deleted volume: %v", err)
	}
	if _, err := os.Stat(dev); err == nil {
		t.Error("wrapper left after delete")
	}
	if err := e.p.DeleteThinPool(e.ctx, e.poolU, ""); err != nil {
		t.Fatal(err)
	}
	if err := e.p.DeleteArray(e.ctx, e.array); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(e.k.ArrayPath(e.array)); err == nil {
		t.Error("array still running")
	}
}

func nvmeConnect(e *env, nqn, host string) (string, error) {
	_, err := e.k.Run.Run(e.ctx, "nvme", "connect", "-t", "tcp", "-a", "127.0.0.1", "-s", strconv.Itoa(nvmePort),
		"-n", nqn, "--hostnqn", host, "--hostid", "8b0b9c58-7d2a-4f3e-9c1d-"+fmt.Sprintf("%012d", rand.IntN(1e12)))
	if err != nil {
		return "", err
	}
	e.t.Cleanup(func() { _, _ = e.k.Run.Run(e.ctx, "nvme", "disconnect", "-n", nqn) })
	re := regexp.MustCompile(`^nvme\d+n\d+$`)
	var dev string
	waitFor(e.t, "the NVMe namespace", 15*time.Second, func() bool {
		subs, _ := filepath.Glob("/sys/class/nvme-subsystem/*")
		for _, s := range subs {
			if b, _ := os.ReadFile(filepath.Join(s, "subsysnqn")); strings.TrimSpace(string(b)) != nqn {
				continue
			}
			es, _ := os.ReadDir(s)
			for _, n := range es {
				if re.MatchString(n.Name()) {
					dev = "/dev/" + n.Name()
					_, err := os.Stat(dev)
					return err == nil
				}
			}
		}
		return false
	})
	return dev, nil
}

func iscsiLogin(e *env, iqn, initiator string) (string, error) {
	e.run("systemctl", "start", "iscsid")
	iface := fmt.Sprintf("%s-%d", e.id, rand.IntN(1000))
	e.run("iscsiadm", "-m", "iface", "-I", iface, "-o", "new")
	e.t.Cleanup(func() { _, _ = e.k.Run.Run(e.ctx, "iscsiadm", "-m", "iface", "-I", iface, "-o", "delete") })
	e.run("iscsiadm", "-m", "iface", "-I", iface, "-o", "update", "-n", "iface.initiatorname", "-v", initiator)
	e.run("iscsiadm", "-m", "discovery", "-t", "st", "-p", iscsiPt, "-I", iface)
	e.t.Cleanup(func() {
		_, _ = e.k.Run.Run(e.ctx, "iscsiadm", "-m", "node", "-T", iqn, "-p", iscsiPt, "-I", iface, "-u")
		_, _ = e.k.Run.Run(e.ctx, "iscsiadm", "-m", "node", "-T", iqn, "-p", iscsiPt, "-I", iface, "-o", "delete")
	})
	for _, kv := range [][2]string{{"node.conn[0].timeo.login_timeout", "10"}, {"node.session.initial_login_retry_max", "1"}} {
		e.run("iscsiadm", "-m", "node", "-T", iqn, "-p", iscsiPt, "-I", iface, "-o", "update", "-n", kv[0], "-v", kv[1])
	}
	if _, err := e.k.Run.Run(e.ctx, "iscsiadm", "-m", "node", "-T", iqn, "-p", iscsiPt, "-I", iface, "-l"); err != nil {
		return "", err
	}
	var dev string
	waitFor(e.t, "the iSCSI LUN", 15*time.Second, func() bool {
		d, err := filepath.EvalSymlinks("/dev/disk/by-path/ip-" + iscsiPt + "-iscsi-" + iqn + "-lun-0")
		dev = d
		return err == nil
	})
	return dev, nil
}

// Exports through ApplyExport: the right hosts get the volume's data over NVMe/TCP and iSCSI,
// others are refused; a portal without its address is reported and the rest still converge;
// an address-only iSCSI ACL is refused.
func TestExports(t *testing.T) {
	e := newEnv(t)
	e.pool()
	const size = 32 * mib
	dev, vol := e.volume("vol-x", size)
	fill(t, dev, 0, size, 3)

	nvme := &store.Export{ID: "exp-n" + e.id, VolumeID: vol.ID, Protocol: store.ExportProtocolNVMeoFTCP, SubsystemNQN: "nqn.2026-11.io.velocor.test:" + e.id}
	portals := []*store.Portal{
		{ID: "pt-1", IPAddress: "127.0.0.1", Port: nvmePort},
		{ID: "pt-gone", IPAddress: "203.0.113.77", Port: nvmePort},
	}
	err := e.p.ApplyExport(e.ctx, nvme, vol, dataplane.ACL{HostNQNs: []string{hostNQN}}, portals)
	var miss *dataplane.AddressMissingError
	if !errors.As(err, &miss) || miss.Portal.ID != "pt-gone" {
		t.Fatalf("apply NVMe export: %v (want the missing portal reported)", err)
	}
	t.Cleanup(func() { _ = e.p.RemoveTarget(context.Background(), nvme) })
	if err := e.p.ApplyExport(e.ctx, nvme, vol, dataplane.ACL{HostNQNs: []string{hostNQN}}, portals[:1]); err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if _, err := nvmeConnect(e, nvme.SubsystemNQN, "nqn.2026-11.io.velocor.test:intruder"); err == nil {
		t.Fatal("a host not allowed connected")
	}
	ndev, err := nvmeConnect(e, nvme.SubsystemNQN, hostNQN)
	if err != nil {
		t.Fatal(err)
	}
	verify(t, ndev, 0, size, 3)
	if b, _ := os.ReadFile("/sys/block/" + filepath.Base(ndev) + "/uuid"); strings.TrimSpace(string(b)) != vol.UUID {
		t.Errorf("namespace UUID %q, want the volume's %q", strings.TrimSpace(string(b)), vol.UUID)
	}
	conns, err := e.p.Connections(e.ctx, nvme)
	if err != nil || !slices.ContainsFunc(conns, func(c dataplane.Connection) bool { return c.Host == hostNQN || c.Address == "127.0.0.1" }) {
		t.Errorf("NVMe connections %+v %v: the connected host is missing", conns, err)
	}

	other, _ := e.volume("vol-y", size)
	if err := e.p.ApplyExport(e.ctx, nvme, &store.Volume{ID: "vol-y", UUID: "", Handle: e.vg + "/vol-y"}, dataplane.ACL{HostNQNs: []string{hostNQN}}, portals[:1]); err == nil {
		t.Errorf("an export was repointed to %s", other)
	}

	iscsi := &store.Export{ID: "exp-i" + e.id, VolumeID: vol.ID, Protocol: store.ExportProtocolISCSI, TargetIQN: "iqn.2026-11.io.velocor.test:" + e.id}
	ipt := []*store.Portal{{ID: "pt-i", IPAddress: iscsiIP, Port: 3260}}
	if err := e.p.ApplyExport(e.ctx, iscsi, vol, dataplane.ACL{CIDRs: []string{"127.0.0.1/32"}}, ipt); !errors.Is(err, store.ErrInvalidArgument) {
		t.Fatalf("an address-only iSCSI ACL: %v", err)
	}
	init1 := "iqn.2026-11.io.velocor.test:kp-init1"
	if err := e.p.ApplyExport(e.ctx, iscsi, vol, dataplane.ACL{IQNs: []string{init1}, CIDRs: []string{"127.0.0.1/32"}}, ipt); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.p.RemoveTarget(context.Background(), iscsi) })
	if _, err := iscsiLogin(e, iscsi.TargetIQN, "iqn.2026-11.io.velocor.test:intruder"); err == nil {
		t.Fatal("an initiator not allowed logged in")
	}
	idev, err := iscsiLogin(e, iscsi.TargetIQN, init1)
	if err != nil {
		t.Fatal(err)
	}
	verify(t, idev, 0, size, 3)
	fill(t, idev, 0, 4*mib, 4)
	verify(t, ndev, 0, 4*mib, 4)
	conns, err = e.p.Connections(e.ctx, iscsi)
	if err != nil || len(conns) != 1 || conns[0].Address != "127.0.0.1" {
		t.Errorf("iSCSI connections %+v %v", conns, err)
	}

	// A volume can't be deleted while exported (its wrapper is busy).
	if err := e.p.DeleteVolume(e.ctx, vol.UUID); err == nil {
		t.Fatal("deleted an exported volume")
	}
	un, _ := e.p.Unmanaged(e.ctx, dataplane.Known{Targets: map[string]bool{nvme.SubsystemNQN: true}})
	if !slices.ContainsFunc(un, func(o dataplane.UnmanagedObject) bool { return o.Kind == "ISCSI_TARGET" && o.Name == iscsi.TargetIQN }) ||
		slices.ContainsFunc(un, func(o dataplane.UnmanagedObject) bool { return o.Name == nvme.SubsystemNQN }) {
		t.Errorf("unmanaged targets: %+v", un)
	}
}

// Replacing a member rebuilds onto the new disk; a scrub runs to completion.
func TestReplaceAndScrub(t *testing.T) {
	e := newEnv(t)
	e.pool()
	dev, _ := e.volume("vol-r", 64*mib)
	fill(t, dev, 0, 64*mib, 5)
	if st, err := e.k.ArrayStatus(e.array); err == nil {
		t.Logf("removing a member during %q (md lets go of it late while it resyncs)", st.SyncAction)
	}

	if err := e.p.RemoveMember(e.ctx, e.disks[1]); err != nil {
		t.Fatal(err)
	}
	a := e.arrayOf()
	if a.Members[1].Name != "" || a.Operational != 1 || a.State != dataplane.ArrayOnline {
		t.Fatalf("after removing a member: %+v", a)
	}
	c, _ := e.disk(1 << 30)
	if err := e.p.AddMember(e.ctx, e.array, c); err != nil {
		t.Fatal(err)
	}
	sawRebuild := false
	waitFor(t, "the rebuild", 180*time.Second, func() bool {
		a := e.arrayOf()
		if a.Rebuild != nil && a.Rebuild.Target == c {
			sawRebuild = true
		}
		return a.Operational == 2 && a.Rebuild == nil && a.Members[1].Name == c && a.Members[1].Configured
	})
	t.Logf("rebuild seen in progress: %v", sawRebuild)
	verify(t, dev, 0, 64*mib, 5)

	if err := e.p.SetRebuildLimit(e.ctx, 0); err != nil {
		t.Fatal(err)
	}
	if l, err := e.p.RebuildLimit(e.ctx); err != nil || l != 0 {
		t.Errorf("rebuild limit %d %v", l, err)
	}

	if err := e.p.StartScrub(e.ctx, e.array, false, 0, 0); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the scrub", 180*time.Second, func() bool {
		sc := e.arrayOf().Scrub
		return sc != nil && sc.State == dataplane.ScrubCompleted
	})
	if sc := e.arrayOf().Scrub; sc.MismatchedBlocks != 0 || sc.OffsetBlocks != sc.TotalBlocks || sc.TotalBlocks == 0 {
		t.Errorf("scrub %+v", sc)
	}
	// After a velod restart (a new plane on the same state), the scrub is still known.
	restarted := New(e.k, nil)
	restarted.StateDir = e.p.StateDir
	if err := restarted.LoadState(); err != nil {
		t.Fatal(err)
	}
	arrays, err := restarted.Arrays(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if i := slices.IndexFunc(arrays, func(a dataplane.Array) bool { return a.Name == e.array }); i < 0 || arrays[i].Scrub == nil || arrays[i].Scrub.State != dataplane.ScrubCompleted {
		t.Errorf("scrub after a restart: %+v", arrays)
	}
}

// After a reboot (everything gone from the kernel), Settle brings back the array, the volume
// group and the wrappers with the data. With a member missing, the array waits until it is
// started degraded.
func TestSettleAfterReboot(t *testing.T) {
	e := newEnv(t)
	e.pool()
	dev, vol := e.volume("vol-s", 32*mib)
	fill(t, dev, 0, 32*mib, 6)
	if _, err := e.p.Snapshot(e.ctx, vol.UUID, "snap-s"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the initial resync", 120*time.Second, func() bool {
		st, err := e.k.ArrayStatus(e.array)
		return err == nil && st.SyncAction == "idle"
	})

	reboot := func() {
		t.Helper()
		e.run("dmsetup", "remove", "--retry", kernel.WrapperName("vol-s"))
		if err := e.k.DeactivatePool(e.ctx, e.vg); err != nil {
			t.Fatal(err)
		}
		if err := e.k.StopArray(e.ctx, e.array); err != nil {
			t.Fatal(err)
		}
	}
	reboot()
	if err := e.p.Settle(e.ctx, e.disks); err != nil {
		t.Fatal(err)
	}
	if a := e.arrayOf(); a.State != dataplane.ArrayOnline || a.Operational != 2 {
		t.Fatalf("after settle: %+v", a)
	}
	verify(t, dev, 0, 32*mib, 6)
	if _, err := os.Stat(e.k.WrapperPath("snap-s")); err == nil {
		t.Error("a snapshot got a wrapper")
	}

	// A member missing: the array waits.
	reboot()
	lost := e.run("losetup", "--noheadings", "-O", "BACK-FILE", e.disks[1])
	e.run("losetup", "-d", e.disks[1])
	if err := e.p.Settle(e.ctx, e.disks); err != nil {
		t.Fatal(err)
	}
	a := e.arrayOf()
	if a.State != dataplane.ArrayConfiguring || a.Discovered != 1 || a.Members[0].Name != e.disks[0] || a.Members[1].Name != "" || a.Members[1].UUID == "" {
		t.Fatalf("waiting array %+v", a)
	}
	if err := e.p.StartDegraded(e.ctx, e.array); err != nil {
		t.Fatal(err)
	}
	if err := e.p.Settle(e.ctx, e.disks); err != nil {
		t.Fatal(err)
	}
	if a := e.arrayOf(); a.State != dataplane.ArrayOnline || a.Operational != 1 {
		t.Fatalf("started degraded: %+v", a)
	}
	verify(t, dev, 0, 32*mib, 6)

	// It ran degraded and was written to, so it comes back degraded by itself after another
	// reboot (until the first write md may not have recorded that the member is gone).
	fill(t, dev, 0, 32*mib, 8)
	reboot()
	if err := e.p.Settle(e.ctx, e.disks); err != nil {
		t.Fatal(err)
	}
	if a := e.arrayOf(); a.State != dataplane.ArrayOnline || a.Operational != 1 {
		t.Fatalf("degraded before the reboot: %+v", a)
	}
	verify(t, dev, 0, 32*mib, 8)

	// The disk that missed all that comes back stale: it takes no part, and alone (its metadata
	// still says both members are active) it can't start the array.
	_, _ = e.k.Run.Run(e.ctx, "losetup", e.disks[1], lost)
	reboot()
	if err := e.p.Settle(e.ctx, e.disks); err != nil {
		t.Fatal(err)
	}
	if a := e.arrayOf(); a.State != dataplane.ArrayOnline || a.Operational != 1 || a.Members[0].Name != e.disks[0] {
		t.Fatalf("with the stale member back: %+v", a)
	}
	verify(t, dev, 0, 32*mib, 8)
	reboot()
	if err := e.p.Settle(e.ctx, e.disks[1:]); err != nil {
		t.Fatal(err)
	}
	if a := e.arrayOf(); a.State != dataplane.ArrayConfiguring {
		t.Fatalf("the stale member alone started the array: %+v", a)
	}
	if err := e.p.Settle(e.ctx, e.disks); err != nil { // running again, for the cleanup
		t.Fatal(err)
	}
}

// ExamineDisks finds a stopped pool's array on its disks, each member in its slot, and a member
// replaced earlier as stale. Settle assembles only arrays on claimed disks, from the claimed
// ones: an unclaimed copy takes no part.
func TestExamineDisksAndClaimedSettle(t *testing.T) {
	e := newEnv(t)
	e.pool()
	dev, _ := e.volume("vol-e", 16*mib)
	fill(t, dev, 0, 16*mib, 7)
	disk := func(h string) *store.Disk {
		return &store.Disk{ID: "d-" + filepath.Base(h), Type: store.DiskTypeSSD, DevicePath: h}
	}

	// Replace slot 1: the old disk keeps an older copy of the metadata.
	old := e.disks[1]
	if err := e.p.RemoveMember(e.ctx, old); err != nil {
		t.Fatal(err)
	}
	c, _ := e.disk(1 << 30)
	if err := e.p.AddMember(e.ctx, e.array, c); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the rebuild", 180*time.Second, func() bool { return e.arrayOf().Operational == 2 && e.arrayOf().Rebuild == nil })

	stop := func() {
		t.Helper()
		e.run("dmsetup", "remove", "--retry", kernel.WrapperName("vol-e"))
		if err := e.k.DeactivatePool(e.ctx, e.vg); err != nil {
			t.Fatal(err)
		}
		if err := e.k.StopArray(e.ctx, e.array); err != nil {
			t.Fatal(err)
		}
	}
	stop()
	arrays, err := e.p.ExamineDisks(e.ctx, []*store.Disk{disk(e.disks[0]), disk(old), disk(c)})
	if err != nil {
		t.Fatal(err)
	}
	if len(arrays) != 1 {
		t.Fatalf("arrays %+v", arrays)
	}
	a := arrays[0]
	if a.Name != e.array || a.Level != store.RaidLevel1 || a.Slots != 2 || a.Current != 2 || a.SizeBytes == 0 {
		t.Fatalf("array %+v", a)
	}
	got := map[string]dataplane.DiskMember{}
	for _, m := range a.Members {
		got[m.Disk.DevicePath] = m
	}
	if m := got[e.disks[0]]; m.Slot != 0 || m.Stale {
		t.Errorf("first member %+v", m)
	}
	if m := got[c]; m.Slot != 1 || m.Stale {
		t.Errorf("replacement %+v", m)
	}
	if m := got[old]; !m.Stale {
		t.Errorf("the replaced disk isn't stale: %+v", m)
	}

	// Nothing claimed: nothing assembled.
	if err := e.p.Settle(e.ctx, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(e.k.ArrayPath(e.array)); err == nil {
		t.Fatal("assembled an array on unclaimed disks")
	}
	// The current members claimed (and the old disk not): assembled from them, data intact.
	if err := e.p.Settle(e.ctx, []string{e.disks[0], c}); err != nil {
		t.Fatal(err)
	}
	if a := e.arrayOf(); a.State != dataplane.ArrayOnline || a.Operational != 2 || a.Members[1].Name != c {
		t.Fatalf("after settle: %+v", a)
	}
	verify(t, dev, 0, 16*mib, 7)
	e.disks = []string{e.disks[0], c}
}

// RAID10 over four disks: a thin volume's data survives losing a disk and the rebuild onto a
// new one.
func TestRAID10(t *testing.T) {
	e := newEnv(t)
	if err := e.p.CheckLevel(store.RaidLevel10, 3); err == nil {
		t.Error("RAID10 on three disks accepted")
	}
	for range 4 {
		h, _ := e.disk(512 << 20)
		e.disks = append(e.disks, h)
	}
	if err := e.p.CheckLevel(store.RaidLevel10, len(e.disks)); err != nil {
		t.Fatal(err)
	}
	if err := e.p.CreateArray(e.ctx, e.array, store.RaidLevel10, 64, e.disks); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.destroy)
	if _, err := e.p.CreateThinPool(e.ctx, e.array, e.vg, 1<<20); err != nil {
		t.Fatal(err)
	}
	if a := e.arrayOf(); a.Level != "raid10" || a.Slots != 4 || a.State != dataplane.ArrayOnline {
		t.Fatalf("array %+v", a)
	}
	dev, _ := e.volume("vol-10", 64*mib)
	fill(t, dev, 0, 64*mib, 9)
	if err := e.p.RemoveMember(e.ctx, e.disks[2]); err != nil {
		t.Fatal(err)
	}
	verify(t, dev, 0, 64*mib, 9)
	n, _ := e.disk(512 << 20)
	if err := e.p.AddMember(e.ctx, e.array, n); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the rebuild", 180*time.Second, func() bool {
		a := e.arrayOf()
		return a.Operational == 4 && a.Rebuild == nil && a.Members[2].Name == n
	})
	verify(t, dev, 0, 64*mib, 9)
}

// Thin metadata grows once it passes the threshold, doubling while the volume group has room;
// once there is none, growing fails and says so.
func TestThinMetadataGrows(t *testing.T) {
	e := newEnv(t)
	e.pool()
	before, err := e.p.ThinPools(e.ctx, e.vg, "")
	if err != nil || len(before) != 1 || before[0].MetadataBytes == 0 || before[0].MetadataUsedBytes == 0 {
		t.Fatalf("thin pool %+v %v", before, err)
	}
	e.p.MetadataGrowAt = 101 // never full enough
	if n, err := e.p.GrowMetadata(e.ctx, e.poolU); n != 0 || err != nil {
		t.Fatalf("grew below the threshold: %d %v", n, err)
	}
	e.p.MetadataGrowAt = 0.001
	size := before[0].MetadataBytes
	var growErr error
	for range 10 {
		n, err := e.p.GrowMetadata(e.ctx, e.poolU)
		if err != nil {
			growErr = err
			break
		}
		if n <= size || n > 2*size {
			t.Fatalf("grew from %d to %d", size, n)
		}
		size = n
	}
	after, _ := e.p.ThinPools(e.ctx, e.vg, "")
	if after[0].MetadataBytes != size || size == before[0].MetadataBytes {
		t.Errorf("metadata %d -> %d (thin pool says %d)", before[0].MetadataBytes, size, after[0].MetadataBytes)
	}
	if growErr == nil || !strings.Contains(growErr.Error(), "no room") {
		t.Errorf("growing with no room left: %v", growErr)
	}
	t.Logf("metadata grown %d -> %d bytes, then: %v", before[0].MetadataBytes, size, growErr)
	dev, _ := e.volume("vol-m", 16*mib) // the pool still works
	fill(t, dev, 0, 16*mib, 11)
}
