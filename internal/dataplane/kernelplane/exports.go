package kernelplane

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/velocor/velocor/internal/dataplane"
	"github.com/velocor/velocor/internal/kernel"
	"github.com/velocor/velocor/internal/store"
)

// ApplyExport exports the volume's wrapper: an nvmet subsystem with its allowed hosts and a
// listener per portal, or a LIO target with an ACL per initiator and a network portal per
// portal. Portals whose address isn't on this host are skipped and reported, as with SPDK.
func (p *Plane) ApplyExport(ctx context.Context, exp *store.Export, vol *store.Volume, acl dataplane.ACL, portals []*store.Portal) error {
	if err := p.k.LoadTargetModules(ctx); err != nil {
		return fmt.Errorf("load target modules: %w", err)
	}
	lv, err := p.resolve(ctx, vol.UUID)
	if err != nil {
		if lv, err = p.resolve(ctx, vol.Handle); err != nil {
			return err
		}
	}
	if err := p.k.EnsureWrapper(ctx, lv.Name, p.k.LVPath(lv.VG, lv.Name)); err != nil {
		return fmt.Errorf("wrapper of %s: %w", lv.Name, err)
	}
	device := p.k.WrapperPath(lv.Name)
	var missing []error
	present := portals[:0:0]
	for _, pt := range portals {
		if !dataplane.AddressPresent(pt.IPAddress) {
			missing = append(missing, &dataplane.AddressMissingError{Portal: pt})
			continue
		}
		present = append(present, pt)
	}

	switch {
	case exp.Protocol == store.ExportProtocolNVMeoFRDMA:
		return fmt.Errorf("%w: NVMe over RDMA", dataplane.ErrUnsupported)
	case dataplane.IsNVMe(exp.Protocol):
		if cur := p.k.NVMeSubsystemDevice(exp.SubsystemNQN); cur != "" && cur != device {
			return fmt.Errorf("nvmet subsystem %s exports %s, not %s; refusing to repoint it", exp.SubsystemNQN, cur, device)
		}
		var ports []int
		for _, pt := range present {
			id, err := p.nvmePort(pt.IPAddress, int(pt.Port))
			if err != nil {
				return err
			}
			ports = append(ports, id)
		}
		err = p.k.EnsureNVMeExport(kernelNVMeExport(exp, vol, device, acl, ports))
	default:
		if !exp.AllowAnyHost && len(acl.IQNs) == 0 {
			return fmt.Errorf("%w: the kernel iSCSI target allows initiators by IQN only, and export %s has none (only addresses)",
				store.ErrInvalidArgument, exp.ID)
		}
		var nps []string
		for _, pt := range present {
			nps = append(nps, net.JoinHostPort(pt.IPAddress, strconv.FormatUint(uint64(pt.Port), 10)))
		}
		err = p.k.EnsureISCSIExport(kernelISCSIExport(exp, vol, device, acl, nps))
	}
	if err != nil {
		return err
	}
	return errors.Join(missing...)
}

// nvmePort returns the listener on addr:port, creating it (with the next free ID) if needed.
func (p *Plane) nvmePort(addr string, port int) (int, error) {
	next := 1
	for _, pt := range p.k.NVMePorts() {
		if pt.Address == addr && pt.Port == port {
			return pt.ID, nil
		}
		next = max(next, pt.ID+1)
	}
	return next, p.k.EnsureNVMePort(kernelNVMePort(next, addr, port))
}

// serial is the export's NVMe serial / SCSI unit serial: "VELO" and the export ID, as SPDK had.
func serial(exp *store.Export) string {
	s := "VELO" + strings.ToUpper(strings.ReplaceAll(exp.ID, "-", ""))
	if len(s) > 20 {
		s = s[:20]
	}
	return s
}

// RemoveTarget removes the export's subsystem or target; nvmet listeners no subsystem uses any
// more go with it.
func (p *Plane) RemoveTarget(_ context.Context, exp *store.Export) error {
	if !dataplane.IsNVMe(exp.Protocol) {
		return p.k.RemoveISCSIExport(exp.TargetIQN, exp.VolumeID)
	}
	if err := p.k.RemoveNVMeExport(exp.SubsystemNQN); err != nil {
		return err
	}
	used := map[int]bool{}
	for _, nqn := range p.k.NVMeSubsystems() {
		for _, pt := range p.k.NVMeSubsystemPorts(nqn) {
			used[pt.ID] = true
		}
	}
	for _, pt := range p.k.NVMePorts() {
		if !used[pt.ID] {
			if err := p.k.RemoveNVMePort(pt.ID); err != nil {
				return fmt.Errorf("remove unused nvmet port %d: %w", pt.ID, err)
			}
		}
	}
	return nil
}

// Connections lists the connected hosts: NVMe host NQNs from nvmet's debugfs, else the peers of
// its listeners' TCP connections; iSCSI initiators' addresses from LIO's sessions.
func (p *Plane) Connections(ctx context.Context, exp *store.Export) ([]dataplane.Connection, error) {
	out := []dataplane.Connection{}
	if dataplane.IsNVMe(exp.Protocol) {
		if hosts := p.k.NVMeHosts(exp.SubsystemNQN); hosts != nil {
			for _, h := range hosts {
				out = append(out, dataplane.Connection{Host: h})
			}
			return out, nil
		}
		// Without nvmet's debugfs (Fedora builds without CONFIG_NVME_TARGET_DEBUGFS), the
		// network side: TCP connections to the export's listeners. Exports sharing a listener
		// all show its connections, so this can over-report, never miss one.
		// A host's controller has one TCP connection per queue: one entry per address, with
		// its I/O queues (all but the admin queue).
		count := map[string]int{}
		var order []string
		for _, pt := range p.k.NVMeSubsystemPorts(exp.SubsystemNQN) {
			peers, err := p.tcpPeers(ctx, pt.Address, pt.Port)
			if err != nil {
				return nil, err
			}
			for _, a := range peers {
				if count[a] == 0 {
					order = append(order, a)
				}
				count[a]++
			}
		}
		for _, a := range order {
			out = append(out, dataplane.Connection{Address: a, Queues: count[a] - 1})
		}
		return out, nil
	}
	_, addrs := p.k.ISCSISessions(exp.TargetIQN)
	for _, a := range addrs {
		out = append(out, dataplane.Connection{Address: a})
	}
	return out, nil
}

// Unmanaged reports md arrays, volume groups, volumes, nvmet subsystems and LIO targets velod
// has no record of. Disks aren't reported: the host has plenty of block devices of its own.
func (p *Plane) Unmanaged(ctx context.Context, known dataplane.Known) ([]dataplane.UnmanagedObject, error) {
	var out []dataplane.UnmanagedObject
	for _, name := range p.k.ListArrays() {
		if known.Devices[name] {
			continue
		}
		detail := ""
		if st, err := p.k.ArrayStatus(name); err == nil {
			detail = fmt.Sprintf("%s, %s, %d of %d members", st.Level, st.ArrayState, st.RaidDisks-st.Degraded, st.RaidDisks)
		}
		out = append(out, dataplane.UnmanagedObject{Kind: "MD_ARRAY", Name: name, Detail: detail})
	}
	vgs, err := p.k.ListVGs(ctx)
	if err != nil {
		return nil, err
	}
	for _, vg := range vgs {
		if !known.ThinPools[vg.UUID] {
			out = append(out, dataplane.UnmanagedObject{Kind: "VOLUME_GROUP", Name: vg.Name, UUID: vg.UUID,
				Detail: fmt.Sprintf("on %s, %s used", vg.PV, humanBytes(vg.Size-vg.Free))})
		}
	}
	lvs, err := p.k.ListVolumes(ctx, "")
	if err != nil {
		return nil, err
	}
	vgUUID := map[string]string{}
	for _, vg := range vgs {
		vgUUID[vg.Name] = vg.UUID
	}
	for _, lv := range lvs {
		// Volumes in an unmanaged volume group are covered by its report.
		if !isVolume(lv) || !known.ThinPools[vgUUID[lv.VG]] || known.Volumes[volumeUUID(lv)] || known.Skip[lv.Name] {
			continue
		}
		kind := "volume"
		if isSnapshot(lv) {
			kind = "snapshot"
		}
		out = append(out, dataplane.UnmanagedObject{Kind: "LV", Name: lv.VG + "/" + lv.Name, UUID: volumeUUID(lv),
			Detail: fmt.Sprintf("a %s in volume group %s", kind, lv.VG)})
	}
	for _, nqn := range p.k.NVMeSubsystems() {
		if !known.Targets[nqn] {
			out = append(out, dataplane.UnmanagedObject{Kind: "NVMET_SUBSYSTEM", Name: nqn,
				Detail: "may give hosts access to a volume no export controls"})
		}
	}
	for _, iqn := range p.k.ISCSITargets() {
		if !known.Targets[iqn] {
			out = append(out, dataplane.UnmanagedObject{Kind: "ISCSI_TARGET", Name: iqn,
				Detail: "may give hosts access to a volume no export controls"})
		}
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

func kernelNVMePort(id int, addr string, port int) kernel.NVMePort {
	return kernel.NVMePort{ID: id, Address: addr, Port: port}
}

func kernelNVMeExport(exp *store.Export, vol *store.Volume, device string, acl dataplane.ACL, ports []int) kernel.NVMeExport {
	return kernel.NVMeExport{NQN: exp.SubsystemNQN, Serial: serial(exp), UUID: vol.UUID, Device: device,
		Hosts: acl.HostNQNs, AllowAnyHost: exp.AllowAnyHost, Ports: ports}
}

func kernelISCSIExport(exp *store.Export, vol *store.Volume, device string, acl dataplane.ACL, portals []string) kernel.ISCSIExport {
	return kernel.ISCSIExport{IQN: exp.TargetIQN, Backstore: exp.VolumeID, Serial: serial(exp), Device: device,
		Portals: portals, Initiators: acl.IQNs, AnyInitiator: exp.AllowAnyHost}
}

// tcpPeers lists the peer addresses of established TCP connections to addr:port (ss).
func (p *Plane) tcpPeers(ctx context.Context, addr string, port int) ([]string, error) {
	out, err := p.k.Run.Run(ctx, "ss", "-Htn", "state", "established", "( sport = :"+strconv.Itoa(port)+" )")
	if err != nil {
		return nil, err
	}
	return parseSSPeers(string(out), addr, port), nil
}

// parseSSPeers reads "ss -Htn state established" lines (Recv-Q Send-Q Local:Port Peer:Port),
// keeping connections whose local end is addr:port (any local address for a wildcard addr).
func parseSSPeers(out, addr string, port int) []string {
	var peers []string
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 4 {
			continue
		}
		lh, lp, err1 := net.SplitHostPort(f[2])
		ph, _, err2 := net.SplitHostPort(f[3])
		if err1 != nil || err2 != nil || lp != strconv.Itoa(port) {
			continue
		}
		if ip := net.ParseIP(addr); ip != nil && !ip.IsUnspecified() && !ip.Equal(net.ParseIP(strings.TrimPrefix(lh, "::ffff:"))) {
			continue
		}
		peers = append(peers, strings.TrimPrefix(ph, "::ffff:"))
	}
	return peers
}
