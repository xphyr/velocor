// Package dataplane is velod's interface to the data plane (Plane) and the backend-neutral
// logic around it: resolving export ACLs and converging exports, portal addresses, and
// matching moved disks to pool members. The SPDK backend is in spdkplane.
//
// The export functions here are idempotent and convergent: through the Plane they inspect the
// live state, create what is missing, repair what has drifted and return an error (never
// silently succeed) when the desired state cannot be reached. They are shared by the REST API
// handlers and the reconciler so both paths apply the exact same ACL policy.
package dataplane

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"sync"

	"github.com/velocor/velocor/internal/store"
)

// ErrNoInitiators is returned when an export would be reachable by nobody. Callers must either supply
// initiator groups that resolve to at least one initiator, or set AllowAnyHost.
var ErrNoInitiators = errors.New("export has no initiators; add an initiator group or set allow_any_host explicitly")

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

// ValidateNQN checks an NVMe Qualified Name, so a bad one is refused when it is entered rather
// than at export time.
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
	// A subsystem's or host's directory in configfs is named after it.
	for _, r := range nqn {
		if r <= ' ' || r > '~' || r == '/' || r == '\\' {
			return fmt.Errorf("NQN %q contains invalid character %q", nqn, r)
		}
	}
	return nil
}

// ValidateIQN checks an iSCSI name (RFC 3720): iqn.yyyy-mm.reverse.domain[:anything], eui. and
// 16 hex digits, or naa. and 16 or 32 hex digits.
func ValidateIQN(iqn string) error {
	hex := func(s string, lens ...int) bool {
		return slices.Contains(lens, len(s)) && strings.Trim(strings.ToLower(s), "0123456789abcdef") == ""
	}
	if len(iqn) > 223 {
		return fmt.Errorf("iSCSI name %q is longer than 223 bytes", iqn)
	}
	if s, ok := strings.CutPrefix(iqn, "eui."); ok && hex(s, 16) {
		return nil
	}
	if s, ok := strings.CutPrefix(iqn, "naa."); ok && hex(s, 16, 32) {
		return nil
	}
	if len(iqn) < 13 || !strings.HasPrefix(iqn, "iqn.") || iqn[8] != '-' || iqn[11] != '.' ||
		strings.Trim(iqn[4:8]+iqn[9:11], "0123456789") != "" {
		return fmt.Errorf("iSCSI name %q must look like iqn.yyyy-mm.reverse.domain:name, eui.<16 hex digits> or naa.<16 or 32 hex digits>", iqn)
	}
	// A target's or initiator's directory in configfs is named after it.
	for _, r := range iqn {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == ':', r == '_':
		default:
			return fmt.Errorf("iSCSI name %q contains invalid character %q (allowed: A-Z a-z 0-9 . - : _)", iqn, r)
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

// EnsureExport converges the data plane so that the export with exp.ID exposes vol with exactly
// its configured ACL. The export is re-read from the store under the per-export lock, so a
// caller holding a stale copy (such as a reconciler pass) can never re-apply an ACL that
// has since been changed. A deleted export is a no-op.
func EnsureExport(ctx context.Context, p Plane, st store.Store, exp *store.Export, vol *store.Volume) error {
	defer lockExport(exp.ID)()
	cur, err := st.GetExport(exp.ID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return ensureLocked(ctx, p, st, cur, vol)
}

// UpdateExport applies mutate to the export's access settings, converges the data plane to the result
// and only then persists it, all under the per-export lock. If it cannot be converged the
// previous configuration is re-applied and the store is left unchanged.
func UpdateExport(ctx context.Context, p Plane, st store.Store, id string, vol *store.Volume, mutate func(*store.Export) error) (*store.Export, error) {
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
	if err := ensureLocked(ctx, p, st, &next, vol); err != nil && !keepsMissing(old, &next, err) {
		_ = ensureLocked(context.WithoutCancel(ctx), p, st, old, vol)
		return nil, err
	}
	return st.MutateExport(id, func(e *store.Export) error {
		e.InitiatorGroupIDs, e.AllowAnyHost, e.NodeIDs = next.InitiatorGroupIDs, next.AllowAnyHost, next.NodeIDs
		e.Disabled, e.DisabledReason = next.Disabled, next.DisabledReason
		e.PortalIDs = next.PortalIDs
		return nil
	})
}

// keepsMissing: the change can be applied although err reports portals without their
// address, because those portals were already on the enabled export: an unrelated change
// (its hosts, say) must not fail because an address is gone. A change that enables the
// export or adds such a portal is refused.
func keepsMissing(old, next *store.Export, err error) bool {
	ids, only := missingPortals(err)
	if !only || old.Disabled {
		return false
	}
	for _, id := range ids {
		if !slices.Contains(old.PortalIDs, id) {
			return false
		}
	}
	return true
}

func ensureLocked(ctx context.Context, p Plane, st store.Store, exp *store.Export, vol *store.Volume) error {
	if exp.Disabled {
		return p.RemoveTarget(ctx, exp)
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
	if !IsNVMe(exp.Protocol) && exp.Protocol != store.ExportProtocolISCSI {
		return fmt.Errorf("%w: unsupported export protocol %q", store.ErrInvalidArgument, exp.Protocol)
	}
	return p.ApplyExport(ctx, exp, vol, acl, portals)
}

// Connection is a host session on an export.
type Connection struct {
	Host    string `json:"host,omitempty"`    // host NQN (NVMe-oF); initiator IQN where its address isn't known (iSCSI)
	Address string `json:"address,omitempty"` // initiator address (iSCSI)
	Queues  int    `json:"queues,omitempty"`  // I/O queue pairs (NVMe-oF)
}

// Connections lists the hosts currently connected to exp.
func Connections(ctx context.Context, p Plane, exp *store.Export) ([]Connection, error) {
	return p.Connections(ctx, exp)
}

// RemoveExport tears down the data-plane objects owned by exp. Missing objects are not an error.
func RemoveExport(ctx context.Context, p Plane, exp *store.Export) error {
	defer lockExport(exp.ID)()
	return p.RemoveTarget(ctx, exp)
}

// MaxNameLen is the longest export name: it is embedded in NQNs, IQNs and data-plane object
// names, and SPDK caps those at 63 characters.
const MaxNameLen = 63

// ValidateObjectName checks that name is safe to embed in NQNs, IQNs and data-plane object
// names: 1..MaxNameLen characters from [A-Za-z0-9._-].
func ValidateObjectName(name string) error {
	if name == "" {
		return errors.New("name must not be empty")
	}
	if len(name) > MaxNameLen {
		return fmt.Errorf("name %q is longer than %d characters", name, MaxNameLen)
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
		default:
			return fmt.Errorf("name %q contains invalid character %q (allowed: A-Z a-z 0-9 . _ -)", name, r)
		}
	}
	return nil
}
