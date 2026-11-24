package kernel

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// inventory keeps LVM's reports (lvs, vgs) for InventoryTTL, as a reconciler pass asks for the
// same ones many times. Any command of this Plane that changes something (every other LVM command)
// drops them: before it runs, and again after, so a report made in between isn't kept.
type inventory struct {
	mu   sync.Mutex
	gen  uint64
	kept map[string]report
}

type report struct {
	at  time.Time
	out []byte
}

func (i *inventory) get(key string, ttl time.Duration) (out []byte, gen uint64, ok bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	r, ok := i.kept[key]
	if !ok || time.Since(r.at) >= ttl {
		return nil, i.gen, false
	}
	return r.out, i.gen, true
}

// put keeps a report, unless something changed since gen.
func (i *inventory) put(key string, gen uint64, at time.Time, out []byte) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.gen != gen {
		return
	}
	if i.kept == nil {
		i.kept = map[string]report{}
	}
	i.kept[key] = report{at: at, out: out}
}

func (i *inventory) drop() {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.gen++
	i.kept = nil
}

// lvm runs an LVM command with velod's own configuration.
func (p *Plane) lvm(ctx context.Context, cmd string, args ...string) ([]byte, error) {
	full := append([]string{cmd, "--config", lvmConfig}, args...)
	if cmd != "lvs" && cmd != "vgs" && cmd != "pvs" {
		p.inv.drop()
		defer p.inv.drop()
		return p.Run.Run(ctx, "lvm", full...)
	}
	if p.InventoryTTL <= 0 {
		return p.Run.Run(ctx, "lvm", full...)
	}
	key := strings.Join(full, "\x00")
	out, gen, ok := p.inv.get(key, p.InventoryTTL)
	if ok {
		return out, nil
	}
	at := time.Now()
	out, err := p.Run.Run(ctx, "lvm", full...)
	if err == nil {
		p.inv.put(key, gen, at, out)
	}
	return out, err
}

// DropInventory forgets LVM's reports: something they depend on (an array) changed.
func (p *Plane) DropInventory() { p.inv.drop() }

// dmKernelName is the kernel's name (dm-3) of a device-mapper device node, "" if it isn't one.
func (p *Plane) dmKernelName(path string) string {
	t, err := filepath.EvalSymlinks(path)
	if err != nil {
		return ""
	}
	if n := filepath.Base(t); strings.HasPrefix(n, "dm-") {
		return n
	}
	return ""
}

func (p *Plane) sysBlockSectors(kname string) (uint64, error) {
	b, err := os.ReadFile(filepath.Join(p.SysRoot, "block", kname, "size"))
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
}

// WrapperCurrent reports, from sysfs alone, whether the volume's wrapper maps all of lower: it is
// as large, on lower and nothing else. False when that can't be told (EnsureWrapper then looks).
func (p *Plane) WrapperCurrent(volume, lower string) bool {
	w, l := p.dmKernelName(p.WrapperPath(volume)), p.dmKernelName(lower)
	if w == "" || l == "" {
		return false
	}
	slaves := dirNames(filepath.Join(p.SysRoot, "block", w, "slaves"))
	if len(slaves) != 1 || slaves[0] != l {
		return false
	}
	ws, err1 := p.sysBlockSectors(w)
	ls, err2 := p.sysBlockSectors(l)
	return errors.Join(err1, err2) == nil && ws == ls && ws > 0
}

// ArrayMemberOf reports whether a disk (by its kernel name) is held by an md array now.
func (p *Plane) ArrayMemberOf(kname string) bool {
	for _, h := range dirNames(filepath.Join(p.SysRoot, "block", kname, "holders")) {
		if strings.HasPrefix(h, "md") {
			return true
		}
	}
	return false
}

// DiskSeq is a disk's sequence number, which changes when it is unplugged and plugged in again.
func (p *Plane) DiskSeq(kname string) string {
	return readAttr(filepath.Join(p.SysRoot, "block", kname, "diskseq"))
}
