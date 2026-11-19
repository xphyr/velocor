package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/velocor/velocor/internal/store"
)

// Network changes are applied with NetworkManager's checkpoints (TODO.md P1-3): velod takes a
// checkpoint of the interface, applies the change in memory only (nmcli --temporary), and an
// administrator must confirm within the timeout from a connection that still works. Without a
// confirmation NetworkManager itself restores the checkpoint, so an appliance that a change cut
// off comes back even if velod is the thing that can't be reached. A reboot during the window
// also brings the old configuration back, since nothing was saved to disk. Confirming saves
// the change and commits the checkpoint.

const (
	defaultConfirmTimeout = 120 * time.Second
	minConfirmTimeout     = 60 * time.Second
	maxConfirmTimeout     = 600 * time.Second
)

// nmBackend is what network changes need from NetworkManager (replaceable in tests).
type nmBackend interface {
	// Checkpoint snapshots iface's device and connection; NetworkManager restores it after
	// timeout unless it is committed. It returns the checkpoint's handle.
	Checkpoint(ctx context.Context, iface string, timeout time.Duration) (string, error)
	// CheckpointExists reports whether the checkpoint is still pending.
	CheckpointExists(ctx context.Context, cp string) (bool, error)
	// Commit drops the checkpoint, keeping the current state.
	Commit(ctx context.Context, cp string) error
	// Rollback restores the checkpoint now.
	Rollback(ctx context.Context, cp string) error
	// Apply configures iface; temporary leaves the saved profile alone.
	Apply(ctx context.Context, iface string, req UpdateNetworkInterfaceRequest, dns []string, temporary bool) error
}

// NetworkChange is a change waiting for confirmation, or the outcome of the last one.
type NetworkChange struct {
	ID        string                        `json:"id"`
	Interface string                        `json:"interface"`
	Request   UpdateNetworkInterfaceRequest `json:"request"`
	// OldAddresses are the interface's addresses before the change; NewAddress is where to
	// reach the appliance afterwards when it changes (manual addressing).
	OldAddresses []string  `json:"old_addresses"`
	NewAddress   string    `json:"new_address,omitempty"`
	AppliedAt    time.Time `json:"applied_at"`
	Deadline     time.Time `json:"deadline"`
	By           string    `json:"by"`
	// State is PENDING, CONFIRMED, ROLLED_BACK (by request) or EXPIRED (not confirmed in time:
	// NetworkManager restored the old configuration).
	State      string    `json:"state"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
	Message    string    `json:"message,omitempty"`

	checkpoint string
	dns        []string
}

type netChanges struct {
	mu      sync.Mutex
	backend nmBackend
	pending *NetworkChange
	last    *NetworkChange
	now     func() time.Time
}

func (n *netChanges) clock() time.Time {
	if n.now != nil {
		return n.now()
	}
	return time.Now().UTC()
}

// snapshot returns copies of the pending and last changes.
func (n *netChanges) snapshot() (pending, last *NetworkChange) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.pending != nil {
		c := *n.pending
		pending = &c
	}
	if n.last != nil {
		c := *n.last
		last = &c
	}
	return pending, last
}

// finish moves the pending change to last with a final state.
func (n *netChanges) finishLocked(state, msg string) *NetworkChange {
	c := n.pending
	c.State, c.Message, c.FinishedAt = state, msg, n.clock()
	n.last, n.pending = c, nil
	return c
}

// expire notices a change whose checkpoint NetworkManager has already restored.
func (s *Server) watchNetworkChange(id string, deadline time.Time) {
	n := s.net
	// Check a little after the deadline, then until NetworkManager has acted.
	time.Sleep(time.Until(deadline) + 3*time.Second)
	for i := 0; i < 20; i++ {
		n.mu.Lock()
		c := n.pending
		if c == nil || c.ID != id {
			n.mu.Unlock()
			return // confirmed or rolled back meanwhile
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		exists, err := n.backend.CheckpointExists(ctx, c.checkpoint)
		cancel()
		if err == nil && !exists {
			done := n.finishLocked("EXPIRED", "not confirmed in time: NetworkManager restored the previous configuration")
			n.mu.Unlock()
			s.logger.Warn("network change not confirmed; NetworkManager rolled it back", "interface", done.Interface, "change", done.ID, "by", done.By)
			return
		}
		n.mu.Unlock()
		time.Sleep(3 * time.Second)
	}
	s.logger.Error("a network change's deadline passed but its checkpoint is still there", "change", id)
}

// portalsOn lists the portals whose address is on iface.
func (s *Server) portalsOn(addrs []string) ([]*store.Portal, error) {
	portals, err := s.store.ListPortals("")
	if err != nil {
		return nil, err
	}
	have := map[string]bool{}
	for _, a := range addrs {
		ip, _, err := net.ParseCIDR(a)
		if err == nil {
			have[ip.String()] = true
		}
	}
	var out []*store.Portal
	for _, p := range portals {
		if ip := net.ParseIP(p.IPAddress); ip != nil && have[ip.String()] {
			out = append(out, p)
		}
	}
	return out, nil
}

func newChangeID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return "netchg-" + hex.EncodeToString(b)
}

// requestOnInterface reports whether r arrived on one of addrs (the caller is connected
// through the interface being changed).
func requestOnInterface(r *http.Request, addrs []string) bool {
	local, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	if !ok {
		return false
	}
	host, _, err := net.SplitHostPort(local.String())
	if err != nil {
		return false
	}
	for _, a := range addrs {
		if ip, _, err := net.ParseCIDR(a); err == nil && ip.String() == net.ParseIP(host).String() {
			return true
		}
	}
	return false
}

var errNoCheckpoints = errors.New("NetworkManager checkpoints aren't available")

func (s *Server) handleUpdateNetworkInterface(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var req UpdateNetworkInterfaceRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, fmt.Errorf("%w: invalid JSON payload", store.ErrInvalidArgument))
		return
	}
	dnsList, err := validateNetworkRequest(name, req)
	if err != nil {
		writeError(w, fmt.Errorf("%w: %s", store.ErrInvalidArgument, err.Error()))
		return
	}
	timeout := defaultConfirmTimeout
	if req.ConfirmTimeoutSeconds != 0 {
		timeout = time.Duration(req.ConfirmTimeoutSeconds) * time.Second
		if timeout < minConfirmTimeout || timeout > maxConfirmTimeout {
			writeError(w, fmt.Errorf("%w: confirm_timeout_seconds must be between 60 and 600", store.ErrInvalidArgument))
			return
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	cur := s.findInterface(ctx, name)
	if cur == nil {
		writeError(w, fmt.Errorf("%w: no interface %q", store.ErrNotFound, name))
		return
	}
	// Interfaces carrying portals serve the SAN: changing them disconnects hosts.
	portals, err := s.portalsOn(cur.Addresses)
	if err != nil {
		writeError(w, err)
		return
	}
	if len(portals) > 0 && !req.Force {
		names := make([]string, len(portals))
		for i, p := range portals {
			names[i] = fmt.Sprintf("%s (%s %s)", p.Name, p.Protocol, p.IPAddress)
		}
		writeError(w, fmt.Errorf("%w: %s carries portals %s: changing it interrupts every host using them; send \"force\": true to change it anyway",
			store.ErrInUse, name, strings.Join(names, ", ")))
		return
	}

	n := s.net
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.pending != nil {
		writeError(w, fmt.Errorf("%w: a change to %s is waiting for confirmation until %s; confirm or roll it back first",
			store.ErrInUse, n.pending.Interface, n.pending.Deadline.Format(time.RFC3339)))
		return
	}
	cp, err := n.backend.Checkpoint(ctx, name, timeout)
	if err != nil {
		writeError(w, fmt.Errorf("can't change the network safely: %w: %v", errNoCheckpoints, err))
		return
	}
	if err := n.backend.Apply(ctx, name, req, dnsList, true); err != nil {
		rbCtx, rbCancel := context.WithTimeout(context.WithoutCancel(r.Context()), 30*time.Second)
		defer rbCancel()
		if rbErr := n.backend.Rollback(rbCtx, cp); rbErr != nil {
			s.logger.Error("applying a network change failed, and so did rolling it back; NetworkManager restores it at the deadline", "err", rbErr)
		}
		writeError(w, fmt.Errorf("applying the change failed and it was rolled back: %w", err))
		return
	}
	now := n.clock()
	c := &NetworkChange{ID: newChangeID(), Interface: name, Request: req, OldAddresses: cur.Addresses, AppliedAt: now,
		Deadline: now.Add(timeout), By: subjectFromContext(r.Context()), State: "PENDING", checkpoint: cp, dns: dnsList}
	if strings.EqualFold(req.Method, "manual") {
		if ip, _, err := net.ParseCIDR(strings.TrimSpace(req.Address)); err == nil {
			c.NewAddress = ip.String()
		}
	}
	n.pending = c
	go s.watchNetworkChange(c.ID, c.Deadline)
	s.logger.Warn("network change applied; it is rolled back unless confirmed", "interface", name, "change", c.ID,
		"method", req.Method, "deadline", c.Deadline, "by", c.By, "portals_affected", len(portals))

	view := *c
	resp := map[string]any{
		"pending": view,
		"message": fmt.Sprintf("Applied to %s. Confirm within %d seconds from a connection that still works, or it is rolled back.", name, int(timeout.Seconds())),
	}
	if requestOnInterface(r, cur.Addresses) {
		resp["warning"] = "This request came in through the interface being changed: if you lose this connection, confirm from the new address."
	}
	writeJSON(w, http.StatusAccepted, resp)
}

func (s *Server) handleConfirmNetworkChange(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	if err := decodeJSON(r, &req); err != nil || req.ID == "" {
		writeError(w, fmt.Errorf("%w: body must be {\"id\": \"netchg-...\"}", store.ErrInvalidArgument))
		return
	}
	n := s.net
	n.mu.Lock()
	defer n.mu.Unlock()
	c := n.pending
	if c == nil || c.ID != req.ID {
		writeError(w, fmt.Errorf("%w: no change %s is waiting for confirmation (it may have been rolled back)", store.ErrNotFound, req.ID))
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 30*time.Second)
	defer cancel()
	if exists, err := n.backend.CheckpointExists(ctx, c.checkpoint); err == nil && !exists {
		done := n.finishLocked("EXPIRED", "not confirmed in time: NetworkManager restored the previous configuration")
		writeError(w, fmt.Errorf("%w: too late: change %s was already rolled back", store.ErrNotFound, done.ID))
		return
	}
	// Save the configuration now in effect, then drop the checkpoint.
	if err := n.backend.Apply(ctx, c.Interface, c.Request, c.dns, false); err != nil {
		writeError(w, fmt.Errorf("saving the change failed (it stays pending and is rolled back at the deadline): %w", err))
		return
	}
	if err := n.backend.Commit(ctx, c.checkpoint); err != nil {
		writeError(w, fmt.Errorf("the change is saved, but committing NetworkManager's checkpoint failed: %w", err))
		return
	}
	done := n.finishLocked("CONFIRMED", "")
	s.logger.Info("network change confirmed", "interface", done.Interface, "change", done.ID, "by", subjectFromContext(r.Context()))
	writeJSON(w, http.StatusOK, map[string]any{"change": *done})
}

func (s *Server) handleRollbackNetworkChange(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	if err := decodeJSON(r, &req); err != nil || req.ID == "" {
		writeError(w, fmt.Errorf("%w: body must be {\"id\": \"netchg-...\"}", store.ErrInvalidArgument))
		return
	}
	n := s.net
	n.mu.Lock()
	defer n.mu.Unlock()
	c := n.pending
	if c == nil || c.ID != req.ID {
		writeError(w, fmt.Errorf("%w: no change %s is waiting for confirmation", store.ErrNotFound, req.ID))
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 30*time.Second)
	defer cancel()
	if err := n.backend.Rollback(ctx, c.checkpoint); err != nil {
		if exists, xerr := n.backend.CheckpointExists(ctx, c.checkpoint); xerr != nil || exists {
			writeError(w, fmt.Errorf("rolling back failed (NetworkManager still rolls it back at the deadline): %w", err))
			return
		}
	}
	done := n.finishLocked("ROLLED_BACK", "rolled back by "+subjectFromContext(r.Context()))
	s.logger.Warn("network change rolled back", "interface", done.Interface, "change", done.ID, "by", subjectFromContext(r.Context()))
	writeJSON(w, http.StatusOK, map[string]any{"change": *done})
}

// findInterface returns iface's current state, or nil.
func (s *Server) findInterface(ctx context.Context, iface string) *NetworkInterface {
	st := s.queryNetwork(ctx)
	for i := range st.Interfaces {
		if st.Interfaces[i].Name == iface {
			return &st.Interfaces[i]
		}
	}
	return nil
}
