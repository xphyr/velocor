package spdk

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync/atomic"
	"time"
)

const (
	// DefaultSocketPath is the default UNIX domain socket used by spdk_tgt.
	DefaultSocketPath = "/var/tmp/spdk.sock"
	// DefaultTimeout is the default per-RPC timeout when context has no deadline.
	DefaultTimeout = 15 * time.Second
)

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
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.timeout)
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
