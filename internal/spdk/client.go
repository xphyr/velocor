package spdk

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync/atomic"
	"time"
)

const (
	// DefaultSocketPath is the default UNIX domain socket shared between the velocor-spdk
	// and velocor-velod containers on the host (/run/velocor/spdk.sock).
	DefaultSocketPath = "/run/velocor/spdk.sock"
	// DefaultTimeout is the default per-RPC timeout when context has no deadline.
	DefaultTimeout = 15 * time.Second
	// LongTimeout applies to methods whose duration grows with the device size (creating a
	// volume store unmaps the whole device; thick volumes and inflation write every cluster).
	// Giving up early is worse than waiting: spdk_tgt completes the request anyway, and the
	// caller would act on a state it no longer knows.
	LongTimeout = 30 * time.Minute
)

var longRunningMethods = map[string]bool{
	"bdev_lvol_create_lvstore":  true,
	"bdev_lvol_delete_lvstore":  true,
	"bdev_lvol_create":          true,
	"bdev_lvol_inflate":         true,
	"bdev_lvol_decouple_parent": true,
	"bdev_lvol_resize":          true,
	"bdev_raid_create":          true,
	"bdev_raid_delete":          true,
}

// RPCError represents a JSON-RPC 2.0 error returned by spdk_tgt.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string {
	if len(e.Data) > 0 {
		return fmt.Sprintf("spdk rpc error (code %d): %s (%s)", e.Code, e.Message, string(e.Data))
	}
	return fmt.Sprintf("spdk rpc error (code %d): %s", e.Code, e.Message)
}

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
	ID      uint64 `json:"id"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
	ID      uint64          `json:"id"`
}

// Client is a concurrency-safe JSON-RPC 2.0 client for communicating with spdk_tgt
// over its local UNIX domain socket.
type Client struct {
	socketPath string
	timeout    time.Duration
	nextID     atomic.Uint64
}

// NewClient creates a new SPDK JSON-RPC 2.0 client for the given UNIX socket path.
func NewClient(socketPath string) *Client {
	if socketPath == "" {
		socketPath = DefaultSocketPath
	}
	return &Client{
		socketPath: socketPath,
		timeout:    DefaultTimeout,
	}
}

// SocketPath returns the configured UNIX socket path.
func (c *Client) SocketPath() string {
	return c.socketPath
}

// Call executes a single JSON-RPC 2.0 method against spdk_tgt and unmarshals the response into result.
func (c *Client) Call(ctx context.Context, method string, params any, result any) error {
	if method == "" {
		return errors.New("spdk rpc method is required")
	}

	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		timeout := c.timeout
		if longRunningMethods[method] && timeout < LongTimeout {
			timeout = LongTimeout
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", c.socketPath)
	if err != nil {
		return fmt.Errorf("dial spdk socket %s: %w", c.socketPath, err)
	}
	defer func() {
		_ = conn.Close()
	}()

	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}

	reqID := c.nextID.Add(1)
	req := rpcRequest{
		JSONRPC: "2.0",
		Method:  method,
		Params:  params,
		ID:      reqID,
	}

	payload, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal spdk rpc %s: %w", method, err)
	}
	payload = append(payload, '\n')

	if _, err := conn.Write(payload); err != nil {
		return fmt.Errorf("write spdk rpc %s: %w", method, err)
	}

	var resp rpcResponse
	dec := json.NewDecoder(bufio.NewReader(conn))
	if err := dec.Decode(&resp); err != nil {
		return fmt.Errorf("decode spdk rpc %s response: %w", method, err)
	}

	if resp.Error != nil {
		return resp.Error
	}

	if result != nil && len(resp.Result) > 0 && string(resp.Result) != "null" {
		if err := json.Unmarshal(resp.Result, result); err != nil {
			return fmt.Errorf("unmarshal spdk rpc %s result: %w", method, err)
		}
	}

	return nil
}

// VersionResponse represents the output of spdk_get_version.
type VersionResponse struct {
	Version string `json:"version"`
}

// GetVersion queries spdk_tgt for its version string and verifies socket connectivity.
func (c *Client) GetVersion(ctx context.Context) (string, error) {
	var res VersionResponse
	if err := c.Call(ctx, "spdk_get_version", nil, &res); err != nil {
		return "", err
	}
	return res.Version, nil
}

// BdevWaitForExamine blocks until SPDK has finished examining all registered bdevs
// (RAID superblock assembly, lvstore load, ...). It must be called before deciding that
// on-disk metadata is "missing" after bdevs were (re)created.
func (c *Client) BdevWaitForExamine(ctx context.Context) error {
	return c.Call(ctx, "bdev_wait_for_examine", nil, nil)
}

// IsNotFound reports whether err is an SPDK JSON-RPC error indicating that the
// referenced object (bdev, lvol, subsystem, target node, ...) does not exist.
// Delete paths use it to stay idempotent.
func IsNotFound(err error) bool {
	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) {
		return false
	}
	switch rpcErr.Code {
	case -19, -2: // -ENODEV, -ENOENT
		return true
	}
	msg := strings.ToLower(rpcErr.Message + " " + string(rpcErr.Data))
	for _, needle := range []string{"no such device", "no such file", "not found", "does not exist", "unable to find", "could not find"} {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}

// MaxLvolNameLen is the longest lvol / lvstore name SPDK accepts (SPDK_LVOL_NAME_MAX - 1).
const MaxLvolNameLen = 63

// ValidateObjectName checks that name is safe to embed in SPDK bdev, lvstore, lvol,
// NQN and IQN identifiers: 1..MaxLvolNameLen characters from [A-Za-z0-9._-].
func ValidateObjectName(name string) error {
	if name == "" {
		return errors.New("name must not be empty")
	}
	if len(name) > MaxLvolNameLen {
		return fmt.Errorf("name %q is longer than %d characters", name, MaxLvolNameLen)
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
