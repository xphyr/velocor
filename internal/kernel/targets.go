package kernel

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// Both targets are configured through configfs directly (no nvmetcli or targetcli): a directory
// per object, attribute files, and symlinks for relations. Every Ensure call is idempotent, so the
// reconciler can apply the desired state on every pass and after a reboot, when configfs is empty.

func (p *Plane) configfs(parts ...string) string {
	return filepath.Join(append([]string{p.SysRoot, "kernel", "config"}, parts...)...)
}

func mkdir(path string) error {
	if err := os.Mkdir(path, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	return nil
}

func readAttr(path string) string {
	b, _ := os.ReadFile(path)
	return strings.TrimSpace(string(b))
}

// writeAttr writes an attribute only when it differs: many can't be written while the object is
// in use, even with the same value.
func writeAttr(path, value string) error {
	if readAttr(path) == value {
		return nil
	}
	if err := os.WriteFile(path, []byte(value), 0o644); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

func symlink(target, link string) error {
	if cur, err := os.Readlink(link); err == nil {
		if filepath.Base(cur) == filepath.Base(target) {
			return nil
		}
		if err := os.Remove(link); err != nil {
			return err
		}
	}
	return os.Symlink(target, link)
}

func dirNames(dir string) []string {
	es, _ := os.ReadDir(dir)
	var out []string
	for _, e := range es {
		out = append(out, e.Name())
	}
	return out
}

// LoadTargetModules loads the kernel targets' modules (configfs directories appear with them).
func (p *Plane) LoadTargetModules(ctx context.Context) error {
	_, err := p.Run.Run(ctx, "modprobe", "-a", "nvmet", "nvmet-tcp", "target_core_mod", "target_core_iblock", "iscsi_target_mod")
	return err
}

// ---- NVMe/TCP (nvmet) ----------------------------------------------------------------------

// NVMePort is a listener: an address and TCP port.
type NVMePort struct {
	ID      int
	Address string
	Port    int
}

// EnsureNVMePort creates a listener. Its address can't change while subsystems use it.
func (p *Plane) EnsureNVMePort(pt NVMePort) error {
	dir := p.configfs("nvmet", "ports", strconv.Itoa(pt.ID))
	if err := mkdir(dir); err != nil {
		return err
	}
	fam := "ipv4"
	if strings.Contains(pt.Address, ":") {
		fam = "ipv6"
	}
	for _, kv := range [][2]string{{"addr_trtype", "tcp"}, {"addr_adrfam", fam}, {"addr_traddr", pt.Address}, {"addr_trsvcid", strconv.Itoa(pt.Port)}} {
		if err := writeAttr(filepath.Join(dir, kv[0]), kv[1]); err != nil {
			return err
		}
	}
	return nil
}

// RemoveNVMePort removes a listener no subsystem uses any more.
func (p *Plane) RemoveNVMePort(id int) error {
	return os.Remove(p.configfs("nvmet", "ports", strconv.Itoa(id)))
}

// NVMeExport is a volume exported over NVMe/TCP: one subsystem, one namespace.
type NVMeExport struct {
	NQN    string
	Serial string
	// UUID identifies the namespace to hosts; the CSI node matches it (docs/review-history.md C5).
	UUID   string
	Device string // the volume's wrapper
	Hosts  []string
	Ports  []int
}

// EnsureNVMeExport creates or updates an export: only the listed hosts may connect, on the
// listed ports.
func (p *Plane) EnsureNVMeExport(e NVMeExport) error {
	sub := p.configfs("nvmet", "subsystems", e.NQN)
	if err := mkdir(sub); err != nil {
		return err
	}
	if err := writeAttr(filepath.Join(sub, "attr_allow_any_host"), "0"); err != nil {
		return err
	}
	if e.Serial != "" {
		if err := writeAttr(filepath.Join(sub, "attr_serial"), e.Serial); err != nil {
			return err
		}
	}
	if err := writeAttr(filepath.Join(sub, "attr_model"), "Velocor"); err != nil {
		return err
	}
	// Hosts: global objects, linked into the subsystem's allowed list.
	for _, h := range e.Hosts {
		if err := mkdir(p.configfs("nvmet", "hosts", h)); err != nil {
			return err
		}
		if err := symlink(p.configfs("nvmet", "hosts", h), filepath.Join(sub, "allowed_hosts", h)); err != nil {
			return err
		}
	}
	for _, h := range dirNames(filepath.Join(sub, "allowed_hosts")) {
		if !slices.Contains(e.Hosts, h) {
			if err := os.Remove(filepath.Join(sub, "allowed_hosts", h)); err != nil {
				return err
			}
		}
	}
	// The namespace: its device and UUID can only change while it is disabled.
	ns := filepath.Join(sub, "namespaces", "1")
	if err := mkdir(ns); err != nil {
		return err
	}
	if readAttr(filepath.Join(ns, "device_path")) != e.Device || (e.UUID != "" && readAttr(filepath.Join(ns, "device_uuid")) != e.UUID) {
		if err := writeAttr(filepath.Join(ns, "enable"), "0"); err != nil {
			return err
		}
		if err := writeAttr(filepath.Join(ns, "device_path"), e.Device); err != nil {
			return err
		}
		if e.UUID != "" {
			if err := writeAttr(filepath.Join(ns, "device_uuid"), e.UUID); err != nil {
				return err
			}
		}
	}
	if err := writeAttr(filepath.Join(ns, "enable"), "1"); err != nil {
		return err
	}
	// Ports.
	for _, pt := range dirNames(p.configfs("nvmet", "ports")) {
		link := p.configfs("nvmet", "ports", pt, "subsystems", e.NQN)
		id, _ := strconv.Atoi(pt)
		if slices.Contains(e.Ports, id) {
			if err := symlink(sub, link); err != nil {
				return err
			}
		} else if _, err := os.Lstat(link); err == nil {
			if err := os.Remove(link); err != nil {
				return err
			}
		}
	}
	return nil
}

// RemoveNVMeExport removes an export; hosts no other subsystem allows are removed too.
func (p *Plane) RemoveNVMeExport(nqn string) error {
	sub := p.configfs("nvmet", "subsystems", nqn)
	if _, err := os.Stat(sub); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	for _, pt := range dirNames(p.configfs("nvmet", "ports")) {
		link := p.configfs("nvmet", "ports", pt, "subsystems", nqn)
		if _, err := os.Lstat(link); err == nil {
			if err := os.Remove(link); err != nil {
				return err
			}
		}
	}
	for _, n := range dirNames(filepath.Join(sub, "namespaces")) {
		_ = writeAttr(filepath.Join(sub, "namespaces", n, "enable"), "0")
		if err := os.Remove(filepath.Join(sub, "namespaces", n)); err != nil {
			return err
		}
	}
	hosts := dirNames(filepath.Join(sub, "allowed_hosts"))
	for _, h := range hosts {
		if err := os.Remove(filepath.Join(sub, "allowed_hosts", h)); err != nil {
			return err
		}
	}
	if err := os.Remove(sub); err != nil {
		return err
	}
	for _, h := range hosts {
		inUse := false
		for _, s := range dirNames(p.configfs("nvmet", "subsystems")) {
			if _, err := os.Lstat(p.configfs("nvmet", "subsystems", s, "allowed_hosts", h)); err == nil {
				inUse = true
				break
			}
		}
		if !inUse {
			_ = os.Remove(p.configfs("nvmet", "hosts", h))
		}
	}
	return nil
}

// ---- iSCSI (LIO) ---------------------------------------------------------------------------

// ISCSIExport is a volume exported over iSCSI: one target, one portal group, LUN 0.
type ISCSIExport struct {
	IQN        string
	Backstore  string // LIO backstore name (the volume's ID)
	Serial     string // the LUN's unit serial (VPD 0x80), stable across restarts
	Device     string // the volume's wrapper
	Portals    []string
	Initiators []string // initiator IQNs allowed to log in
}

func (p *Plane) lio(parts ...string) string {
	return p.configfs(append([]string{"target"}, parts...)...)
}

// EnsureISCSIExport creates or updates an export: an iblock backstore on the device, a target
// with one portal group listening on the portals, LUN 0, and an ACL per initiator mapping it.
// UNMAP is advertised, so hosts can return space to the thin pool.
func (p *Plane) EnsureISCSIExport(e ISCSIExport) error {
	bs := p.lio("core", "iblock_0", e.Backstore)
	if err := mkdir(p.lio("core", "iblock_0")); err != nil {
		return err
	}
	if err := mkdir(bs); err != nil {
		return err
	}
	if readAttr(filepath.Join(bs, "enable")) != "1" {
		if err := os.WriteFile(filepath.Join(bs, "control"), []byte("udev_path="+e.Device), 0o644); err != nil {
			return fmt.Errorf("backstore %s: %w", e.Backstore, err)
		}
		if err := writeAttr(filepath.Join(bs, "enable"), "1"); err != nil {
			return err
		}
	}
	if e.Serial != "" {
		// Reads back as "T10 VPD Unit Serial Number: <serial>"; it can't be written once exported.
		f := filepath.Join(bs, "wwn", "vpd_unit_serial")
		if cur := strings.TrimSpace(strings.TrimPrefix(readAttr(f), "T10 VPD Unit Serial Number:")); cur != e.Serial {
			if err := os.WriteFile(f, []byte(e.Serial), 0o644); err != nil {
				return fmt.Errorf("%s: %w", f, err)
			}
		}
	}
	for _, a := range []string{"emulate_tpu", "emulate_tpws"} {
		_ = writeAttr(filepath.Join(bs, "attrib", a), "1") // best effort: refused once exported
	}
	tpg := p.lio("iscsi", e.IQN, "tpgt_1")
	if err := mkdir(p.lio("iscsi")); err != nil {
		return err
	}
	if err := mkdir(p.lio("iscsi", e.IQN)); err != nil {
		return err
	}
	if err := mkdir(tpg); err != nil {
		return err
	}
	for _, kv := range [][2]string{{"authentication", "0"}, {"generate_node_acls", "0"}, {"demo_mode_write_protect", "1"}} {
		if err := writeAttr(filepath.Join(tpg, "attrib", kv[0]), kv[1]); err != nil {
			return err
		}
	}
	for _, pt := range e.Portals {
		if err := mkdir(filepath.Join(tpg, "np", pt)); err != nil {
			return fmt.Errorf("portal %s: %w", pt, err)
		}
	}
	for _, pt := range dirNames(filepath.Join(tpg, "np")) {
		if !slices.Contains(e.Portals, pt) {
			if err := os.Remove(filepath.Join(tpg, "np", pt)); err != nil {
				return err
			}
		}
	}
	lun := filepath.Join(tpg, "lun", "lun_0")
	if err := mkdir(lun); err != nil {
		return err
	}
	if err := symlink(bs, filepath.Join(lun, e.Backstore)); err != nil {
		return err
	}
	for _, ini := range e.Initiators {
		acl := filepath.Join(tpg, "acls", ini)
		if err := mkdir(acl); err != nil {
			return err
		}
		if err := mkdir(filepath.Join(acl, "lun_0")); err != nil {
			return err
		}
		if err := symlink(lun, filepath.Join(acl, "lun_0", "lun")); err != nil {
			return err
		}
	}
	for _, ini := range dirNames(filepath.Join(tpg, "acls")) {
		if !slices.Contains(e.Initiators, ini) {
			if err := removeACL(filepath.Join(tpg, "acls", ini)); err != nil {
				return err
			}
		}
	}
	return writeAttr(filepath.Join(tpg, "enable"), "1")
}

func removeACL(acl string) error {
	for _, l := range dirNames(acl) {
		if !strings.HasPrefix(l, "lun_") {
			continue
		}
		for _, link := range dirNames(filepath.Join(acl, l)) {
			if fi, err := os.Lstat(filepath.Join(acl, l, link)); err == nil && fi.Mode()&os.ModeSymlink != 0 {
				if err := os.Remove(filepath.Join(acl, l, link)); err != nil {
					return err
				}
			}
		}
		if err := os.Remove(filepath.Join(acl, l)); err != nil {
			return err
		}
	}
	return os.Remove(acl)
}

// RemoveISCSIExport removes a target and its backstore, which ends its sessions.
func (p *Plane) RemoveISCSIExport(iqn, backstore string) error {
	tpg := p.lio("iscsi", iqn, "tpgt_1")
	if _, err := os.Stat(tpg); err == nil {
		_ = writeAttr(filepath.Join(tpg, "enable"), "0")
		for _, ini := range dirNames(filepath.Join(tpg, "acls")) {
			if err := removeACL(filepath.Join(tpg, "acls", ini)); err != nil {
				return err
			}
		}
		for _, l := range dirNames(filepath.Join(tpg, "lun")) {
			for _, link := range dirNames(filepath.Join(tpg, "lun", l)) {
				if fi, err := os.Lstat(filepath.Join(tpg, "lun", l, link)); err == nil && fi.Mode()&os.ModeSymlink != 0 {
					if err := os.Remove(filepath.Join(tpg, "lun", l, link)); err != nil {
						return err
					}
				}
			}
			if err := os.Remove(filepath.Join(tpg, "lun", l)); err != nil {
				return err
			}
		}
		for _, pt := range dirNames(filepath.Join(tpg, "np")) {
			if err := os.Remove(filepath.Join(tpg, "np", pt)); err != nil {
				return err
			}
		}
		if err := os.Remove(tpg); err != nil {
			return err
		}
		if err := os.Remove(p.lio("iscsi", iqn)); err != nil {
			return err
		}
	}
	if _, err := os.Stat(p.lio("core", "iblock_0", backstore)); err == nil {
		return os.Remove(p.lio("core", "iblock_0", backstore))
	}
	return nil
}
