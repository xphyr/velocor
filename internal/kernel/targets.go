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

// checkNames refuses names that aren't a single path component: they become directories in
// configfs, and one with a slash (or "..") would reach outside its own.
func checkNames(names ...string) error {
	for _, n := range names {
		if n == "" || n == "." || n == ".." || strings.ContainsAny(n, "/\x00") {
			return fmt.Errorf("invalid name %q", n)
		}
	}
	return nil
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
	loaded := true
	for _, m := range []string{"nvmet", "nvmet_tcp", "target_core_mod", "target_core_iblock", "iscsi_target_mod"} {
		if _, err := os.Stat(filepath.Join(p.SysRoot, "module", m)); err != nil {
			loaded = false // or built in without parameters: modprobe knows
			break
		}
	}
	if loaded {
		return nil
	}
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
	// AllowAnyHost lets every host connect (Hosts is then ignored).
	AllowAnyHost bool
	Ports        []int
}

// EnsureNVMeExport creates or updates an export: only the listed hosts may connect, on the
// listed ports.
func (p *Plane) EnsureNVMeExport(e NVMeExport) error {
	if err := checkNames(append([]string{e.NQN}, e.Hosts...)...); err != nil {
		return err
	}
	for _, id := range e.Ports {
		if _, err := os.Stat(p.configfs("nvmet", "ports", strconv.Itoa(id))); err != nil {
			return fmt.Errorf("nvmet port %d: %w", id, err)
		}
	}
	sub := p.configfs("nvmet", "subsystems", e.NQN)
	if err := mkdir(sub); err != nil {
		return err
	}
	anyHost, hosts := "0", e.Hosts
	if e.AllowAnyHost {
		anyHost, hosts = "1", nil
	} else if err := writeAttr(filepath.Join(sub, "attr_allow_any_host"), anyHost); err != nil {
		return err // no host can be linked while any may connect
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
	for _, h := range hosts {
		if err := mkdir(p.configfs("nvmet", "hosts", h)); err != nil {
			return err
		}
		if err := symlink(p.configfs("nvmet", "hosts", h), filepath.Join(sub, "allowed_hosts", h)); err != nil {
			return err
		}
	}
	var dropped []string
	for _, h := range dirNames(filepath.Join(sub, "allowed_hosts")) {
		if !slices.Contains(hosts, h) {
			if err := os.Remove(filepath.Join(sub, "allowed_hosts", h)); err != nil {
				return err
			}
			dropped = append(dropped, h)
		}
	}
	p.pruneNVMeHosts(dropped)
	// Nor can any host be let in while some are linked, so this comes after unlinking them.
	if err := writeAttr(filepath.Join(sub, "attr_allow_any_host"), anyHost); err != nil {
		return err
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
	if checkNames(nqn) != nil {
		return nil // can't have been exported
	}
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
	p.pruneNVMeHosts(hosts)
	return nil
}

// pruneNVMeHosts removes the host objects of hosts that no subsystem allows any more.
func (p *Plane) pruneNVMeHosts(hosts []string) {
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
}

// ---- iSCSI (LIO) ---------------------------------------------------------------------------

// ISCSIExport is a volume exported over iSCSI: one target, one portal group, LUN 0.
type ISCSIExport struct {
	IQN string
	// Backstore is the LIO backstore's name (the volume's ID). LIO opens the device exclusively, so
	// a volume has one, shared by its exports.
	Backstore  string
	Serial     string // the LUN's unit serial (VPD 0x80): of the volume, stable across restarts
	Device     string // the volume's wrapper
	Portals    []string
	Initiators []string // initiator IQNs allowed to log in
	// AnyInitiator lets every initiator log in (Initiators is then ignored).
	AnyInitiator bool
}

func (p *Plane) lio(parts ...string) string {
	return p.configfs(append([]string{"target"}, parts...)...)
}

// EnsureISCSIExport creates or updates an export: an iblock backstore on the device, a target
// with one portal group listening on the portals, LUN 0, and an ACL per initiator mapping it.
// UNMAP is advertised, so hosts can return space to the thin pool.
func (p *Plane) EnsureISCSIExport(e ISCSIExport) error {
	if err := checkNames(append(append([]string{e.IQN, e.Backstore}, e.Initiators...), e.Portals...)...); err != nil {
		return err
	}
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
	if e.Serial != "" && !p.backstoreExported(e.Backstore) {
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
	gen, wp := "0", "1"
	initiators := e.Initiators
	if e.AnyInitiator {
		gen, wp, initiators = "1", "0", nil
	}
	for _, kv := range [][2]string{{"authentication", "0"}, {"generate_node_acls", gen}, {"demo_mode_write_protect", wp}} {
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
	for _, ini := range initiators {
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
		if !slices.Contains(initiators, ini) {
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

// backstoreExported reports whether a target's LUN links to the backstore.
func (p *Plane) backstoreExported(backstore string) bool {
	for _, iqn := range p.ISCSITargets() {
		for _, lun := range dirNames(p.lio("iscsi", iqn, "tpgt_1", "lun")) {
			if _, err := os.Lstat(p.lio("iscsi", iqn, "tpgt_1", "lun", lun, backstore)); err == nil {
				return true
			}
		}
	}
	return false
}

// RemoveISCSIExport removes a target, which ends its sessions, and the backstore if no other
// target exports it.
func (p *Plane) RemoveISCSIExport(iqn, backstore string) error {
	if checkNames(iqn, backstore) != nil {
		return nil // can't have been exported
	}
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
	}
	if _, err := os.Stat(p.lio("iscsi", iqn)); err == nil { // also without its portal group
		if err := os.Remove(p.lio("iscsi", iqn)); err != nil {
			return err
		}
	}
	if _, err := os.Stat(p.lio("core", "iblock_0", backstore)); err == nil && !p.backstoreExported(backstore) {
		return os.Remove(p.lio("core", "iblock_0", backstore))
	}
	return nil
}

// ---- inventory -----------------------------------------------------------------------------

// NVMePorts lists the nvmet listeners.
func (p *Plane) NVMePorts() []NVMePort {
	var out []NVMePort
	for _, id := range dirNames(p.configfs("nvmet", "ports")) {
		n, err := strconv.Atoi(id)
		if err != nil {
			continue
		}
		dir := p.configfs("nvmet", "ports", id)
		port, _ := strconv.Atoi(readAttr(filepath.Join(dir, "addr_trsvcid")))
		out = append(out, NVMePort{ID: n, Address: readAttr(filepath.Join(dir, "addr_traddr")), Port: port})
	}
	return out
}

// NVMeSubsystems lists the nvmet subsystems' NQNs.
func (p *Plane) NVMeSubsystems() []string { return dirNames(p.configfs("nvmet", "subsystems")) }

// NVMeSubsystemDevice is the device behind a subsystem's namespace 1.
func (p *Plane) NVMeSubsystemDevice(nqn string) string {
	return readAttr(p.configfs("nvmet", "subsystems", nqn, "namespaces", "1", "device_path"))
}

// RevalidateNVMe makes nvmet re-read the size of a subsystem's namespace (after a resize), so
// connected hosts see it.
func (p *Plane) RevalidateNVMe(nqn string) error {
	f := p.configfs("nvmet", "subsystems", nqn, "namespaces", "1", "revalidate_size")
	if _, err := os.Stat(f); err != nil {
		return nil // no namespace, or a kernel without the attribute
	}
	return os.WriteFile(f, []byte("1"), 0o644)
}

// NVMeSubsystemPorts lists the listeners a subsystem is linked to.
func (p *Plane) NVMeSubsystemPorts(nqn string) []NVMePort {
	var out []NVMePort
	for _, pt := range p.NVMePorts() {
		if _, err := os.Lstat(p.configfs("nvmet", "ports", strconv.Itoa(pt.ID), "subsystems", nqn)); err == nil {
			out = append(out, pt)
		}
	}
	return out
}

// ISCSITargets lists the LIO iSCSI targets' IQNs.
func (p *Plane) ISCSITargets() []string {
	var out []string
	for _, n := range dirNames(p.lio("iscsi")) {
		if strings.HasPrefix(n, "iqn.") || strings.HasPrefix(n, "eui.") || strings.HasPrefix(n, "naa.") {
			out = append(out, n)
		}
	}
	return out
}

// NVMeHosts lists the host NQNs connected to a subsystem, from nvmet's debugfs (kernel 6.10+);
// nil when debugfs isn't there, empty when no host is connected.
func (p *Plane) NVMeHosts(nqn string) []string {
	dir := filepath.Join(p.SysRoot, "kernel", "debug", "nvmet", nqn)
	if _, err := os.Stat(dir); err != nil {
		return nil
	}
	out := []string{}
	for _, c := range dirNames(dir) {
		if !strings.HasPrefix(c, "ctrl") {
			continue
		}
		if h := readAttr(filepath.Join(dir, c, "hostnqn")); h != "" {
			out = append(out, h)
		}
	}
	return out
}

// ISCSISessions lists the initiators logged in to a target, with their addresses where LIO
// reports them: not for initiators without an ACL of their own (a target any may log in to).
func (p *Plane) ISCSISessions(iqn string) (initiators, addresses []string) {
	acls := p.lio("iscsi", iqn, "tpgt_1", "acls")
	for _, ini := range dirNames(acls) {
		info := readAttr(filepath.Join(acls, ini, "info"))
		if !strings.Contains(info, "LOGGED_IN") {
			continue
		}
		addr := ""
		for _, line := range strings.Split(info, "\n") {
			if _, rest, ok := strings.Cut(line, "Address "); ok {
				if f := strings.Fields(rest); len(f) > 0 {
					addr = f[0]
				}
				break
			}
		}
		initiators, addresses = append(initiators, ini), append(addresses, addr)
	}
	// One name per session, each ended by a newline and a NUL.
	dyn := readAttr(p.lio("iscsi", iqn, "tpgt_1", "dynamic_sessions"))
	for _, ini := range strings.FieldsFunc(dyn, func(r rune) bool { return r == '\n' || r == 0 }) {
		if !slices.Contains(initiators, ini) {
			initiators, addresses = append(initiators, ini), append(addresses, "")
		}
	}
	return initiators, addresses
}
