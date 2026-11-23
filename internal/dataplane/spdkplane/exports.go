package spdkplane

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/velocor/velocor/internal/dataplane"
	"github.com/velocor/velocor/internal/spdk"
	"github.com/velocor/velocor/internal/store"
)

// anyACL is the SPDK iSCSI wildcard.
const anyACL = "ANY"

func bdevMatches(actual string, vol *store.Volume) bool {
	if actual == vol.Device {
		return true
	}
	// SPDK reports lvol bdevs by their UUID name rather than the lvs/name alias.
	return vol.UUID != "" && actual == vol.UUID && vol.Device == vol.Handle
}

func ensureNVMe(ctx context.Context, c *spdk.Client, exp *store.Export, vol *store.Volume, acl dataplane.ACL, portals []*store.Portal) error {
	trtype := "TCP"
	if exp.Protocol == store.ExportProtocolNVMeoFRDMA {
		trtype = "RDMA"
	}
	transports, err := c.NvmfGetTransports(ctx)
	if err != nil {
		return fmt.Errorf("list nvmf transports: %w", err)
	}
	if !slices.ContainsFunc(transports, func(t spdk.NvmfTransport) bool { return strings.EqualFold(t.Trtype, trtype) }) {
		if err := c.NvmfCreateTransport(ctx, trtype); err != nil && !strings.Contains(strings.ToLower(err.Error()), "already exists") {
			return fmt.Errorf("create nvmf transport %s: %w", trtype, err)
		}
	}

	subs, err := c.NvmfGetSubsystems(ctx)
	if err != nil {
		return fmt.Errorf("list nvmf subsystems: %w", err)
	}
	idx := slices.IndexFunc(subs, func(s spdk.NvmfSubsystem) bool { return s.Nqn == exp.SubsystemNQN })
	var sub spdk.NvmfSubsystem
	if idx < 0 {
		serial := "VELO" + strings.ToUpper(strings.ReplaceAll(exp.ID, "-", ""))
		if len(serial) > 20 {
			serial = serial[:20]
		}
		if err := c.NvmfCreateSubsystem(ctx, exp.SubsystemNQN, serial, "", exp.AllowAnyHost, 32); err != nil {
			return fmt.Errorf("create nvmf subsystem %s: %w", exp.SubsystemNQN, err)
		}
		sub = spdk.NvmfSubsystem{Nqn: exp.SubsystemNQN, AllowAnyHost: exp.AllowAnyHost}
	} else {
		sub = subs[idx]
	}

	// Lock down access before exposing data: hosts first, namespace last.
	if sub.AllowAnyHost != exp.AllowAnyHost {
		if err := c.NvmfSubsystemAllowAnyHost(ctx, exp.SubsystemNQN, exp.AllowAnyHost); err != nil {
			return fmt.Errorf("set allow_any_host on %s: %w", exp.SubsystemNQN, err)
		}
	}
	for _, h := range sub.Hosts {
		if !slices.Contains(acl.HostNQNs, h.Nqn) {
			if err := c.NvmfSubsystemRemoveHost(ctx, exp.SubsystemNQN, h.Nqn); err != nil && !spdk.IsNotFound(err) {
				return fmt.Errorf("revoke host %s on %s: %w", h.Nqn, exp.SubsystemNQN, err)
			}
		}
	}
	for _, nqn := range acl.HostNQNs {
		if !slices.ContainsFunc(sub.Hosts, func(h spdk.NvmfHost) bool { return h.Nqn == nqn }) {
			if err := c.NvmfSubsystemAddHost(ctx, exp.SubsystemNQN, nqn); err != nil {
				return fmt.Errorf("allow host %s on %s: %w", nqn, exp.SubsystemNQN, err)
			}
		}
	}

	nsid := exp.NamespaceID
	if nsid == 0 {
		nsid = 1
	}
	if i := slices.IndexFunc(sub.Namespaces, func(n spdk.NvmfNamespace) bool { return n.Nsid == nsid }); i >= 0 {
		if !bdevMatches(sub.Namespaces[i].BdevName, vol) {
			return fmt.Errorf("nvmf subsystem %s nsid %d is backed by %s, expected %s; refusing to repoint a live namespace",
				exp.SubsystemNQN, nsid, sub.Namespaces[i].BdevName, vol.Device)
		}
	} else {
		// The namespace UUID is pinned to the lvol UUID so hosts can verify the device identity.
		if _, err := c.NvmfSubsystemAddNs(ctx, exp.SubsystemNQN, vol.Device, nsid, vol.UUID); err != nil {
			return fmt.Errorf("add namespace %s to %s: %w", vol.Device, exp.SubsystemNQN, err)
		}
	}

	same := func(a, b spdk.NvmfListenAddress) bool {
		return strings.EqualFold(a.Trtype, b.Trtype) && a.Traddr == b.Traddr && a.Trsvcid == b.Trsvcid
	}
	var want []spdk.NvmfListenAddress
	var missing []error
	for _, p := range portals {
		la := spdk.NvmfListenAddress{Trtype: trtype, Adrfam: dataplane.AddressFamily(p.IPAddress), Traddr: p.IPAddress, Trsvcid: strconv.Itoa(int(p.Port))}
		want = append(want, la)
		if slices.ContainsFunc(sub.ListenAddresses, func(l spdk.NvmfListenAddress) bool { return same(l, la) }) {
			continue // kept even if its address went away for a moment: it serves again when it's back
		}
		if !dataplane.AddressPresent(p.IPAddress) {
			missing = append(missing, &dataplane.AddressMissingError{Portal: p}) // served on the other portals meanwhile
			continue
		}
		if err := c.NvmfSubsystemAddListener(ctx, exp.SubsystemNQN, la.Trtype, la.Adrfam, la.Traddr, la.Trsvcid); err != nil {
			return fmt.Errorf("add listener %s:%s to %s: %w", la.Traddr, la.Trsvcid, exp.SubsystemNQN, err)
		}
	}
	// Listeners of portals the export no longer uses (its portals were changed).
	for _, l := range sub.ListenAddresses {
		if slices.ContainsFunc(want, func(w spdk.NvmfListenAddress) bool { return same(l, w) }) {
			continue
		}
		if err := c.NvmfSubsystemRemoveListener(ctx, exp.SubsystemNQN, l.Trtype, l.Adrfam, l.Traddr, l.Trsvcid); err != nil && !spdk.IsNotFound(err) {
			return fmt.Errorf("remove listener %s:%s from %s: %w", l.Traddr, l.Trsvcid, exp.SubsystemNQN, err)
		}
	}
	return errors.Join(missing...)
}

func iscsiPortalHost(ip string) string {
	if dataplane.AddressFamily(ip) == "IPv6" && !strings.HasPrefix(ip, "[") {
		return "[" + ip + "]"
	}
	return ip
}

func diff(have, want []string) (missing, extra []string) {
	for _, w := range want {
		if !slices.Contains(have, w) {
			missing = append(missing, w)
		}
	}
	for _, h := range have {
		if !slices.Contains(want, h) {
			extra = append(extra, h)
		}
	}
	return missing, extra
}

// desiredIscsiACL maps the resolved ACL to SPDK initiator-group initiators and netmasks.
func desiredIscsiACL(exp *store.Export, acl dataplane.ACL) (initiators, netmasks []string) {
	if exp.AllowAnyHost {
		return []string{anyACL}, []string{anyACL}
	}
	initiators, netmasks = acl.IQNs, acl.CIDRs
	if len(initiators) == 0 {
		initiators = []string{anyACL} // restricted by netmask only
	}
	if len(netmasks) == 0 {
		netmasks = []string{anyACL} // restricted by IQN only
	}
	return initiators, netmasks
}

func ensureIscsi(ctx context.Context, c *spdk.Client, exp *store.Export, vol *store.Volume, acl dataplane.ACL, portals []*store.Portal) error {
	if exp.IscsiIgTag == 0 {
		return fmt.Errorf("iscsi export %s has no initiator group tag", exp.ID)
	}
	if len(portals) == 0 {
		return fmt.Errorf("%w: iscsi export %s has no portals", store.ErrInvalidArgument, exp.ID)
	}

	// 1. Portal groups: one per Velocor portal, shared by every export on that portal.
	pgs, err := c.IscsiGetPortalGroups(ctx)
	if err != nil {
		return fmt.Errorf("list iscsi portal groups: %w", err)
	}
	var maps []spdk.IscsiPgIgMap
	var missing []error
	for _, p := range portals {
		if p.IscsiPgTag == 0 {
			return fmt.Errorf("portal %s has no iscsi portal group tag", p.ID)
		}
		want := spdk.IscsiPortal{Host: iscsiPortalHost(p.IPAddress), Port: strconv.Itoa(int(p.Port))}
		if i := slices.IndexFunc(pgs, func(g spdk.IscsiPortalGroup) bool { return g.Tag == p.IscsiPgTag }); i >= 0 {
			if !slices.Contains(pgs[i].Portals, want) {
				return fmt.Errorf("iscsi portal group %d does not contain %s:%s", p.IscsiPgTag, want.Host, want.Port)
			}
		} else if !dataplane.AddressPresent(p.IPAddress) {
			missing = append(missing, &dataplane.AddressMissingError{Portal: p}) // served on the other portals meanwhile
			continue
		} else if err := c.IscsiCreatePortalGroup(ctx, p.IscsiPgTag, []spdk.IscsiPortal{want}); err != nil {
			return fmt.Errorf("create iscsi portal group %d for %s:%s: %w",
				p.IscsiPgTag, want.Host, want.Port, err)
		} else {
			pgs = append(pgs, spdk.IscsiPortalGroup{Tag: p.IscsiPgTag, Portals: []spdk.IscsiPortal{want}})
		}
		maps = append(maps, spdk.IscsiPgIgMap{PgTag: p.IscsiPgTag, IgTag: exp.IscsiIgTag})
	}
	if len(maps) == 0 { // no portal can be served yet (a target node needs at least one)
		return errors.Join(missing...)
	}

	// 2. Initiator group owned exclusively by this export.
	wantInit, wantMask := desiredIscsiACL(exp, acl)
	igs, err := c.IscsiGetInitiatorGroups(ctx)
	if err != nil {
		return fmt.Errorf("list iscsi initiator groups: %w", err)
	}
	if i := slices.IndexFunc(igs, func(g spdk.IscsiInitiatorGroup) bool { return g.Tag == exp.IscsiIgTag }); i >= 0 {
		addI, rmI := diff(igs[i].Initiators, wantInit)
		addM, rmM := diff(igs[i].Netmasks, wantMask)
		// Revoke first: a briefly empty group denies everyone, which is the safe direction.
		if len(rmI) > 0 || len(rmM) > 0 {
			if err := c.IscsiInitiatorGroupRemoveInitiators(ctx, exp.IscsiIgTag, rmI, rmM); err != nil {
				return fmt.Errorf("revoke iscsi initiators on group %d: %w", exp.IscsiIgTag, err)
			}
		}
		if len(addI) > 0 || len(addM) > 0 {
			if err := c.IscsiInitiatorGroupAddInitiators(ctx, exp.IscsiIgTag, addI, addM); err != nil {
				return fmt.Errorf("grant iscsi initiators on group %d: %w", exp.IscsiIgTag, err)
			}
		}
	} else if err := c.IscsiCreateInitiatorGroup(ctx, exp.IscsiIgTag, wantInit, wantMask); err != nil {
		return fmt.Errorf("create iscsi initiator group %d: %w", exp.IscsiIgTag, err)
	}

	// 3. Target node.
	nodes, err := c.IscsiGetTargetNodes(ctx)
	if err != nil {
		return fmt.Errorf("list iscsi target nodes: %w", err)
	}
	i := slices.IndexFunc(nodes, func(n spdk.IscsiTargetNode) bool { return n.Name == exp.TargetIQN })
	if i < 0 {
		if err := c.IscsiCreateTargetNode(ctx, exp.TargetIQN, exp.Name,
			[]spdk.IscsiLun{{BdevName: vol.Device, LunID: int(exp.LUNID)}},
			maps, 64, true, false, false, 0); err != nil {
			return fmt.Errorf("create iscsi target %s: %w", exp.TargetIQN, err)
		}
		return errors.Join(missing...)
	}
	node := nodes[i]
	if !slices.ContainsFunc(node.Luns, func(l spdk.IscsiLun) bool { return l.LunID == int(exp.LUNID) && bdevMatches(l.BdevName, vol) }) {
		return fmt.Errorf("iscsi target %s LUN %d is not backed by %s; refusing to repoint a live target", exp.TargetIQN, exp.LUNID, vol.Device)
	}
	var addMaps, rmMaps []spdk.IscsiPgIgMap
	for _, m := range maps {
		if !slices.Contains(node.PgIgMaps, m) {
			addMaps = append(addMaps, m)
		}
	}
	for _, m := range node.PgIgMaps {
		if !slices.Contains(maps, m) {
			rmMaps = append(rmMaps, m)
		}
	}
	if len(rmMaps) > 0 {
		if err := c.IscsiTargetNodeRemovePgIgMaps(ctx, exp.TargetIQN, rmMaps); err != nil {
			return fmt.Errorf("remove stale ACL mappings from %s: %w", exp.TargetIQN, err)
		}
	}
	if len(addMaps) > 0 {
		if err := c.IscsiTargetNodeAddPgIgMaps(ctx, exp.TargetIQN, addMaps); err != nil {
			return fmt.Errorf("add ACL mappings to %s: %w", exp.TargetIQN, err)
		}
	}
	return errors.Join(missing...)
}
