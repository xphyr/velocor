// Package dataplane realises Velocor's desired SAN export state in spdk_tgt.
//
// Every function here is idempotent and convergent: it inspects the live SPDK
// state, creates what is missing, repairs what has drifted and returns an error
// (never silently succeeds) when the desired state cannot be reached. It is
// shared by the REST API handlers and the reconciler so both paths apply the
// exact same ACL policy.
package dataplane

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/velocor/velocor/internal/spdk"
	"github.com/velocor/velocor/internal/store"
)

// ErrNoInitiators is returned when an export would be reachable by nobody. Callers must either supply
// initiator groups that resolve to at least one initiator, or set AllowAnyHost.
var ErrNoInitiators = errors.New("export has no initiators; add an initiator group or set allow_any_host explicitly")

// anyACL is the SPDK iSCSI wildcard.
const anyACL = "ANY"

var exportLocks sync.Map // export ID -> *sync.Mutex

func lockExport(id string) func() {
	m, _ := exportLocks.LoadOrStore(id, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// ACL is the resolved set of initiators allowed to reach an export.
type ACL struct {
	HostNQNs []string
	IQNs     []string
	CIDRs    []string
}

// ResolveACL expands initiator groups (and the hosts they reference) into a deduplicated ACL.
func ResolveACL(st store.Store, groupIDs []string) (ACL, error) {
	var acl ACL
	for _, gid := range groupIDs {
		ig, err := st.GetInitiatorGroup(gid)
		if err != nil {
			return ACL{}, fmt.Errorf("initiator group %s: %w", gid, err)
		}
		g, err := ResolveGroup(st, ig)
		if err != nil {
			return ACL{}, err
		}
		acl.merge(g)
	}
	return acl, nil
}

// ResolveGroup expands a single initiator group (and the hosts it references).
func ResolveGroup(st store.Store, ig *store.InitiatorGroup) (ACL, error) {
	var acl ACL
	for _, hid := range ig.HostIDs {
		h, err := st.GetHost(hid)
		if err != nil {
			return ACL{}, fmt.Errorf("host %s in initiator group %s: %w", hid, ig.ID, err)
		}
		addUnique(&acl.HostNQNs, h.HostNQN)
		addUnique(&acl.IQNs, h.InitiatorIQN)
		if ip := strings.TrimSpace(h.IPAddress); ip != "" {
			addUnique(&acl.CIDRs, hostCIDR(ip))
		}
	}
	for _, v := range ig.HostNQNs {
		addUnique(&acl.HostNQNs, v)
	}
	for _, v := range ig.InitiatorIQNs {
		addUnique(&acl.IQNs, v)
	}
	for _, v := range ig.AllowedCIDRs {
		addUnique(&acl.CIDRs, v)
	}
	return acl, nil
}

func (a *ACL) merge(b ACL) {
	for _, v := range b.HostNQNs {
		addUnique(&a.HostNQNs, v)
	}
	for _, v := range b.IQNs {
		addUnique(&a.IQNs, v)
	}
	for _, v := range b.CIDRs {
		addUnique(&a.CIDRs, v)
	}
}

func addUnique(list *[]string, v string) {
	v = strings.TrimSpace(v)
	if v != "" && !slices.Contains(*list, v) {
		*list = append(*list, v)
	}
}

func hostCIDR(ip string) string {
	if strings.Contains(ip, "/") {
		return ip
	}
	if parsed := net.ParseIP(ip); parsed != nil && parsed.To4() == nil {
		return ip + "/128"
	}
	return ip + "/32"
}

// ValidateNQN checks an NVMe Qualified Name the way spdk_tgt does, so a bad host NQN is
// refused when it is entered rather than as an opaque "Internal error" at export time.
func ValidateNQN(nqn string) error {
	const uuidPrefix = "nqn.2014-08.org.nvmexpress:uuid:"
	switch {
	case len(nqn) > 223:
		return fmt.Errorf("NQN %q is longer than 223 bytes", nqn)
	case strings.HasPrefix(nqn, uuidPrefix):
		u := strings.TrimPrefix(nqn, uuidPrefix)
		if len(u) != 36 || u[8] != '-' || u[13] != '-' || u[18] != '-' || u[23] != '-' {
			return fmt.Errorf("NQN %q: the part after \"uuid:\" must be a UUID (8-4-4-4-12 hex digits)", nqn)
		}
		for i, r := range u {
			if i == 8 || i == 13 || i == 18 || i == 23 {
				continue
			}
			if !strings.ContainsRune("0123456789abcdefABCDEF", r) {
				return fmt.Errorf("NQN %q: the part after \"uuid:\" must be a UUID", nqn)
			}
		}
		return nil
	case strings.HasPrefix(nqn, "nqn.2014-08.org.nvmexpress.discovery"):
		return fmt.Errorf("NQN %q is the discovery NQN, not a host", nqn)
	}
	// nqn.yyyy-mm.reverse.domain[:anything]
	if len(nqn) < 12 || !strings.HasPrefix(nqn, "nqn.") || nqn[8] != '-' || nqn[11] != '.' {
		return fmt.Errorf("NQN %q must look like nqn.yyyy-mm.reverse.domain:name or %s<uuid>", nqn, uuidPrefix)
	}
	for _, r := range nqn[4:8] + nqn[9:11] {
		if r < '0' || r > '9' {
			return fmt.Errorf("NQN %q must start with nqn.yyyy-mm.<reverse domain>", nqn)
		}
	}
	return nil
}

// IsNVMe reports whether p is an NVMe-oF protocol.
func IsNVMe(p store.ExportProtocol) bool {
	return p == store.ExportProtocolNVMeoFTCP || p == store.ExportProtocolNVMeoFRDMA
}

// CheckACL verifies that exp would admit at least one initiator.
func CheckACL(exp *store.Export, acl ACL) error {
	if exp.AllowAnyHost {
		return nil
	}
	switch {
	case IsNVMe(exp.Protocol):
		if len(acl.HostNQNs) == 0 {
			return ErrNoInitiators
		}
	case exp.Protocol == store.ExportProtocolISCSI:
		if len(acl.IQNs) == 0 && len(acl.CIDRs) == 0 {
			return ErrNoInitiators
		}
	}
	return nil
}

// AddressFamily returns the SPDK adrfam ("IPv4" / "IPv6") for an IP literal.
func AddressFamily(ip string) string {
	if parsed := net.ParseIP(strings.Trim(ip, "[]")); parsed != nil && parsed.To4() == nil {
		return "IPv6"
	}
	return "IPv4"
}

// EnsureExport converges spdk_tgt so that the export with exp.ID exposes vol with exactly
// its configured ACL. The export is re-read from the store under the per-export lock, so a
// caller holding a stale copy (such as a reconciler pass) can never re-apply an ACL that
// has since been changed. A deleted export is a no-op.
func EnsureExport(ctx context.Context, c *spdk.Client, st store.Store, exp *store.Export, vol *store.Volume) error {
	defer lockExport(exp.ID)()
	cur, err := st.GetExport(exp.ID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return ensureLocked(ctx, c, st, cur, vol)
}

// UpdateExport applies mutate to the export's access settings, converges SPDK to the result
// and only then persists it, all under the per-export lock. If SPDK cannot be converged the
// previous configuration is re-applied and the store is left unchanged.
func UpdateExport(ctx context.Context, c *spdk.Client, st store.Store, id string, vol *store.Volume, mutate func(*store.Export) error) (*store.Export, error) {
	defer lockExport(id)()
	old, err := st.GetExport(id)
	if err != nil {
		return nil, err
	}
	next := *old
	next.InitiatorGroupIDs = slices.Clone(old.InitiatorGroupIDs)
	next.PortalIDs = slices.Clone(old.PortalIDs)
	next.NodeIDs = slices.Clone(old.NodeIDs)
	if err := mutate(&next); err != nil {
		return nil, err
	}
	if err := ensureLocked(ctx, c, st, &next, vol); err != nil {
		_ = ensureLocked(context.WithoutCancel(ctx), c, st, old, vol)
		return nil, err
	}
	return st.MutateExport(id, func(e *store.Export) error {
		e.InitiatorGroupIDs, e.AllowAnyHost, e.NodeIDs = next.InitiatorGroupIDs, next.AllowAnyHost, next.NodeIDs
		e.Disabled, e.DisabledReason = next.Disabled, next.DisabledReason
		return nil
	})
}

func ensureLocked(ctx context.Context, c *spdk.Client, st store.Store, exp *store.Export, vol *store.Volume) error {
	if exp.Disabled {
		return removeLocked(ctx, c, exp)
	}
	acl, err := ResolveACL(st, exp.InitiatorGroupIDs)
	if err != nil {
		return err
	}
	if err := CheckACL(exp, acl); err != nil {
		return err
	}
	portals := make([]*store.Portal, 0, len(exp.PortalIDs))
	for _, pid := range exp.PortalIDs {
		p, err := st.GetPortal(pid)
		if err != nil {
			return fmt.Errorf("portal %s: %w", pid, err)
		}
		portals = append(portals, p)
	}
	switch {
	case IsNVMe(exp.Protocol):
		return ensureNVMe(ctx, c, exp, vol, acl, portals)
	case exp.Protocol == store.ExportProtocolISCSI:
		return ensureIscsi(ctx, c, exp, vol, acl, portals)
	default:
		return fmt.Errorf("%w: unsupported export protocol %q", store.ErrInvalidArgument, exp.Protocol)
	}
}

// Connection is a host session on an export.
type Connection struct {
	Host    string `json:"host,omitempty"`    // host NQN (NVMe-oF)
	Address string `json:"address,omitempty"` // initiator address (iSCSI)
	Queues  int    `json:"queues,omitempty"`  // I/O queue pairs (NVMe-oF)
}

// Connections lists the hosts currently connected to exp.
func Connections(ctx context.Context, c *spdk.Client, exp *store.Export) ([]Connection, error) {
	out := []Connection{}
	switch {
	case IsNVMe(exp.Protocol):
		ctrls, err := c.NvmfSubsystemGetControllers(ctx, exp.SubsystemNQN)
		if spdk.IsNotFound(err) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		for _, ct := range ctrls {
			out = append(out, Connection{Host: ct.HostNQN, Queues: int(ct.NumIOQpairs)})
		}
	case exp.Protocol == store.ExportProtocolISCSI:
		conns, err := c.IscsiGetConnections(ctx)
		if err != nil {
			return nil, err
		}
		for _, cn := range conns {
			if cn.TargetNodeName == exp.TargetIQN {
				out = append(out, Connection{Address: cn.InitiatorAddr})
			}
		}
	}
	return out, nil
}

// RemoveExport tears down the SPDK objects owned by exp. Missing objects are not an error.
func RemoveExport(ctx context.Context, c *spdk.Client, exp *store.Export) error {
	defer lockExport(exp.ID)()
	return removeLocked(ctx, c, exp)
}

func removeLocked(ctx context.Context, c *spdk.Client, exp *store.Export) error {
	switch {
	case IsNVMe(exp.Protocol):
		if err := c.NvmfDeleteSubsystem(ctx, exp.SubsystemNQN); err != nil && !spdk.IsNotFound(err) {
			return fmt.Errorf("delete nvmf subsystem %s: %w", exp.SubsystemNQN, err)
		}
	case exp.Protocol == store.ExportProtocolISCSI:
		if err := c.IscsiDeleteTargetNode(ctx, exp.TargetIQN); err != nil && !spdk.IsNotFound(err) {
			return fmt.Errorf("delete iscsi target %s: %w", exp.TargetIQN, err)
		}
		if exp.IscsiIgTag != 0 {
			if err := c.IscsiDeleteInitiatorGroup(ctx, exp.IscsiIgTag); err != nil && !spdk.IsNotFound(err) {
				return fmt.Errorf("delete iscsi initiator group %d: %w", exp.IscsiIgTag, err)
			}
		}
	}
	return nil
}

func bdevMatches(actual string, vol *store.Volume) bool {
	if actual == vol.EffectiveBdevName {
		return true
	}
	// SPDK reports lvol bdevs by their UUID name rather than the lvs/name alias.
	return vol.UUID != "" && actual == vol.UUID && vol.EffectiveBdevName == vol.LvolBdevName
}

func ensureNVMe(ctx context.Context, c *spdk.Client, exp *store.Export, vol *store.Volume, acl ACL, portals []*store.Portal) error {
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
				exp.SubsystemNQN, nsid, sub.Namespaces[i].BdevName, vol.EffectiveBdevName)
		}
	} else {
		// The namespace UUID is pinned to the lvol UUID so hosts can verify the device identity.
		if _, err := c.NvmfSubsystemAddNs(ctx, exp.SubsystemNQN, vol.EffectiveBdevName, nsid, vol.UUID); err != nil {
			return fmt.Errorf("add namespace %s to %s: %w", vol.EffectiveBdevName, exp.SubsystemNQN, err)
		}
	}

	for _, p := range portals {
		la := spdk.NvmfListenAddress{Trtype: trtype, Adrfam: AddressFamily(p.IPAddress), Traddr: p.IPAddress, Trsvcid: strconv.Itoa(int(p.Port))}
		if slices.ContainsFunc(sub.ListenAddresses, func(l spdk.NvmfListenAddress) bool {
			return strings.EqualFold(l.Trtype, la.Trtype) && l.Traddr == la.Traddr && l.Trsvcid == la.Trsvcid
		}) {
			continue
		}
		if err := c.NvmfSubsystemAddListener(ctx, exp.SubsystemNQN, la.Trtype, la.Adrfam, la.Traddr, la.Trsvcid); err != nil {
			return fmt.Errorf("add listener %s:%s to %s: %w", la.Traddr, la.Trsvcid, exp.SubsystemNQN, err)
		}
	}
	return nil
}

func iscsiPortalHost(ip string) string {
	if AddressFamily(ip) == "IPv6" && !strings.HasPrefix(ip, "[") {
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
func desiredIscsiACL(exp *store.Export, acl ACL) (initiators, netmasks []string) {
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

func ensureIscsi(ctx context.Context, c *spdk.Client, exp *store.Export, vol *store.Volume, acl ACL, portals []*store.Portal) error {
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
	for _, p := range portals {
		if p.IscsiPgTag == 0 {
			return fmt.Errorf("portal %s has no iscsi portal group tag", p.ID)
		}
		want := spdk.IscsiPortal{Host: iscsiPortalHost(p.IPAddress), Port: strconv.Itoa(int(p.Port))}
		if i := slices.IndexFunc(pgs, func(g spdk.IscsiPortalGroup) bool { return g.Tag == p.IscsiPgTag }); i >= 0 {
			if !slices.Contains(pgs[i].Portals, want) {
				return fmt.Errorf("iscsi portal group %d does not contain %s:%s", p.IscsiPgTag, want.Host, want.Port)
			}
		} else if err := c.IscsiCreatePortalGroup(ctx, p.IscsiPgTag, []spdk.IscsiPortal{want}); err != nil {
			return fmt.Errorf("create iscsi portal group %d for %s:%s: %w",
				p.IscsiPgTag, want.Host, want.Port, err)
		} else {
			pgs = append(pgs, spdk.IscsiPortalGroup{Tag: p.IscsiPgTag, Portals: []spdk.IscsiPortal{want}})
		}
		maps = append(maps, spdk.IscsiPgIgMap{PgTag: p.IscsiPgTag, IgTag: exp.IscsiIgTag})
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
			[]spdk.IscsiLun{{BdevName: vol.EffectiveBdevName, LunID: int(exp.LUNID)}},
			maps, 64, true, false, false, 0); err != nil {
			return fmt.Errorf("create iscsi target %s: %w", exp.TargetIQN, err)
		}
		return nil
	}
	node := nodes[i]
	if !slices.ContainsFunc(node.Luns, func(l spdk.IscsiLun) bool { return l.LunID == int(exp.LUNID) && bdevMatches(l.BdevName, vol) }) {
		return fmt.Errorf("iscsi target %s LUN %d is not backed by %s; refusing to repoint a live target", exp.TargetIQN, exp.LUNID, vol.EffectiveBdevName)
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
	return nil
}
