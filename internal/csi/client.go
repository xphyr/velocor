package csi

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/velocor/velocor/internal/store"
)

// ErrAPI represents an HTTP error returned by the Velocor control-plane API.
type ErrAPI struct {
	StatusCode int
	Message    string
}

func (e *ErrAPI) Error() string {
	return fmt.Sprintf("velocor api (%d): %s", e.StatusCode, e.Message)
}

// IsNotFound reports whether err represents a 404 Not Found response.
func IsNotFound(err error) bool {
	var apiErr *ErrAPI
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound
}

// IsAlreadyExists reports whether err represents a 409 Conflict response.
func IsAlreadyExists(err error) bool {
	var apiErr *ErrAPI
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusConflict
}

// VolumeGroupSnapshotResult represents a VolumeGroupSnapshot and its member Snapshots.
type VolumeGroupSnapshotResult struct {
	GroupSnapshot *store.VolumeGroupSnapshot `json:"volume_group_snapshot"`
	Snapshots     []*store.Snapshot          `json:"snapshots"`
}

// CreateVolumeRequest specifies parameters for creating a Velocor volume.
type CreateVolumeRequest struct {
	Name          string            `json:"name"`
	PoolID        string            `json:"pool_id"`
	SizeBytes     uint64            `json:"size_bytes"`
	ThinProvision bool              `json:"thin_provision"`
	Labels        map[string]string `json:"labels,omitempty"`
}

// CreateExportRequest specifies parameters for exporting a Velocor volume.
type CreateExportRequest struct {
	Name              string               `json:"name"`
	VolumeID          string               `json:"volume_id"`
	Protocol          store.ExportProtocol `json:"protocol"`
	PortalIDs         []string             `json:"portal_ids"`
	InitiatorGroupIDs []string             `json:"initiator_group_ids,omitempty"`
	AllowAnyHost      bool                 `json:"allow_any_host"`
	NodeID            string               `json:"node_id,omitempty"`
	Shared            bool                 `json:"shared,omitempty"`
	NodeIDs           []string             `json:"node_ids,omitempty"`
}

// UpdateExportRequest changes who may reach a live export (PATCH /api/v1/exports/{id}).
type UpdateExportRequest struct {
	InitiatorGroupIDs *[]string `json:"initiator_group_ids,omitempty"`
	NodeIDs           *[]string `json:"node_ids,omitempty"`
}

// VelocorClient defines the control-plane operations required by the CSI driver.
type VelocorClient interface {
	Ping(ctx context.Context) error
	ListPools(ctx context.Context) ([]*store.Pool, error)
	GetPool(ctx context.Context, id string) (*store.Pool, error)

	ListVolumes(ctx context.Context) ([]*store.Volume, error)
	GetVolume(ctx context.Context, id string) (*store.Volume, error)
	CreateVolume(ctx context.Context, req CreateVolumeRequest) (*store.Volume, error)
	DeleteVolume(ctx context.Context, id string) error
	ResizeVolume(ctx context.Context, id string, newSizeBytes uint64) (*store.Volume, error)

	ListSnapshots(ctx context.Context) ([]*store.Snapshot, error)
	GetSnapshot(ctx context.Context, id string) (*store.Snapshot, error)
	CreateSnapshot(ctx context.Context, volumeID, name string, labels map[string]string) (*store.Snapshot, error)
	DeleteSnapshot(ctx context.Context, id string) error
	CloneSnapshot(ctx context.Context, snapID, cloneName string, labels map[string]string) (*store.Volume, error)

	CreateVolumeGroupSnapshot(ctx context.Context, name string, volumeIDs []string) (*VolumeGroupSnapshotResult, error)
	GetVolumeGroupSnapshot(ctx context.Context, id string) (*VolumeGroupSnapshotResult, error)
	DeleteVolumeGroupSnapshot(ctx context.Context, id string) error

	ListPortals(ctx context.Context) ([]*store.Portal, error)
	ListHosts(ctx context.Context) ([]*store.Host, error)
	CreateHost(ctx context.Context, name, hostNQN, initiatorIQN, ip string) (*store.Host, error)
	ListInitiatorGroups(ctx context.Context) ([]*store.InitiatorGroup, error)
	CreateInitiatorGroup(ctx context.Context, name string, hostIDs []string) (*store.InitiatorGroup, error)
	ListExports(ctx context.Context) ([]*store.Export, error)
	CreateExport(ctx context.Context, req CreateExportRequest) (*store.Export, error)
	DeleteExport(ctx context.Context, id string) error
	UpdateExport(ctx context.Context, id string, req UpdateExportRequest) (*store.Export, error)
}

// HTTPClient implements VelocorClient over the velod REST/JSON API.
type HTTPClient struct {
	baseURL string
	token   string
	http    *http.Client
}

// NewHTTPClient creates a Velocor REST API client.
func NewHTTPClient(endpoint, token string, insecureSkipVerify bool) *HTTPClient {
	c, _ := NewHTTPClientWithCA(endpoint, token, insecureSkipVerify, "")
	return c
}

// NewHTTPClientWithCA creates a client that verifies the Velocor API certificate against
// caPEM (e.g. velod's self-signed /var/lib/velocor/tls.crt) in addition to the system roots.
func NewHTTPClientWithCA(endpoint, token string, insecureSkipVerify bool, caPEM string) (*HTTPClient, error) {
	endpoint = strings.TrimRight(strings.TrimSpace(endpoint), "/")
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: insecureSkipVerify} // #nosec G402 - explicit opt-in only
	if strings.TrimSpace(caPEM) != "" {
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM([]byte(caPEM)) {
			return nil, fmt.Errorf("VELOCOR_CA_CERT does not contain a valid PEM certificate")
		}
		tlsCfg.RootCAs = pool
	}
	tr := &http.Transport{TLSClientConfig: tlsCfg}
	return &HTTPClient{
		baseURL: endpoint,
		token:   strings.TrimSpace(token),
		// No client-wide timeout: each call carries the deadline of the CSI request (set by the
		// sidecars), with a fallback in doJSON. velod finishes a change even if we give up, and
		// every create is idempotent by name, so the sidecars' retry picks up the result.
		http: &http.Client{Transport: tr},
	}, nil
}

func (c *HTTPClient) doJSON(ctx context.Context, method, path string, reqBody, out any) error {
	var bodyReader io.Reader
	if reqBody != nil {
		b, err := json.Marshal(reqBody)
		if err != nil {
			return err
		}
		bodyReader = bytes.NewReader(b)
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bodyReader)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var errObj struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &errObj)
		msg := errObj.Error
		if msg == "" {
			msg = strings.TrimSpace(string(raw))
		}
		return &ErrAPI{StatusCode: resp.StatusCode, Message: msg}
	}
	if out != nil && len(raw) > 0 {
		return json.Unmarshal(raw, out)
	}
	return nil
}

func (c *HTTPClient) Ping(ctx context.Context) error {
	return c.doJSON(ctx, http.MethodGet, "/api/v1/system/status", nil, nil)
}

func (c *HTTPClient) ListPools(ctx context.Context) ([]*store.Pool, error) {
	var resp struct {
		Pools []*store.Pool `json:"pools"`
	}
	err := c.doJSON(ctx, http.MethodGet, "/api/v1/pools", nil, &resp)
	return resp.Pools, err
}

func (c *HTTPClient) GetPool(ctx context.Context, id string) (*store.Pool, error) {
	var resp struct {
		Pool *store.Pool `json:"pool"`
	}
	err := c.doJSON(ctx, http.MethodGet, "/api/v1/pools/"+url.PathEscape(id), nil, &resp)
	return resp.Pool, err
}

func (c *HTTPClient) ListVolumes(ctx context.Context) ([]*store.Volume, error) {
	var resp struct {
		Volumes []*store.Volume `json:"volumes"`
	}
	err := c.doJSON(ctx, http.MethodGet, "/api/v1/volumes", nil, &resp)
	return resp.Volumes, err
}

func (c *HTTPClient) GetVolume(ctx context.Context, id string) (*store.Volume, error) {
	var resp struct {
		Volume *store.Volume `json:"volume"`
	}
	err := c.doJSON(ctx, http.MethodGet, "/api/v1/volumes/"+url.PathEscape(id), nil, &resp)
	return resp.Volume, err
}

func (c *HTTPClient) CreateVolume(ctx context.Context, req CreateVolumeRequest) (*store.Volume, error) {
	var resp struct {
		Volume *store.Volume `json:"volume"`
	}
	err := c.doJSON(ctx, http.MethodPost, "/api/v1/volumes", req, &resp)
	return resp.Volume, err
}

func (c *HTTPClient) DeleteVolume(ctx context.Context, id string) error {
	return c.doJSON(ctx, http.MethodDelete, "/api/v1/volumes/"+url.PathEscape(id), nil, nil)
}

func (c *HTTPClient) ResizeVolume(ctx context.Context, id string, newSizeBytes uint64) (*store.Volume, error) {
	var resp struct {
		Volume *store.Volume `json:"volume"`
	}
	err := c.doJSON(ctx, http.MethodPost, "/api/v1/volumes/"+url.PathEscape(id)+":resize", map[string]any{
		"new_size_bytes": newSizeBytes,
	}, &resp)
	return resp.Volume, err
}

func (c *HTTPClient) ListSnapshots(ctx context.Context) ([]*store.Snapshot, error) {
	var resp struct {
		Snapshots []*store.Snapshot `json:"snapshots"`
	}
	err := c.doJSON(ctx, http.MethodGet, "/api/v1/snapshots", nil, &resp)
	return resp.Snapshots, err
}

func (c *HTTPClient) GetSnapshot(ctx context.Context, id string) (*store.Snapshot, error) {
	var resp struct {
		Snapshot *store.Snapshot `json:"snapshot"`
	}
	err := c.doJSON(ctx, http.MethodGet, "/api/v1/snapshots/"+url.PathEscape(id), nil, &resp)
	return resp.Snapshot, err
}

func (c *HTTPClient) CreateSnapshot(ctx context.Context, volumeID, name string, labels map[string]string) (*store.Snapshot, error) {
	var resp struct {
		Snapshot *store.Snapshot `json:"snapshot"`
	}
	err := c.doJSON(ctx, http.MethodPost, "/api/v1/volumes/"+url.PathEscape(volumeID)+"/snapshots", map[string]any{
		"name":   name,
		"labels": labels,
	}, &resp)
	return resp.Snapshot, err
}

func (c *HTTPClient) DeleteSnapshot(ctx context.Context, id string) error {
	return c.doJSON(ctx, http.MethodDelete, "/api/v1/snapshots/"+url.PathEscape(id), nil, nil)
}

func (c *HTTPClient) CreateVolumeGroupSnapshot(ctx context.Context, name string, volumeIDs []string) (*VolumeGroupSnapshotResult, error) {
	var resp VolumeGroupSnapshotResult
	err := c.doJSON(ctx, http.MethodPost, "/api/v1/volume-group-snapshots", map[string]any{
		"name":       name,
		"volume_ids": volumeIDs,
	}, &resp)
	return &resp, err
}

func (c *HTTPClient) GetVolumeGroupSnapshot(ctx context.Context, id string) (*VolumeGroupSnapshotResult, error) {
	var resp VolumeGroupSnapshotResult
	err := c.doJSON(ctx, http.MethodGet, "/api/v1/volume-group-snapshots/"+url.PathEscape(id), nil, &resp)
	return &resp, err
}

func (c *HTTPClient) DeleteVolumeGroupSnapshot(ctx context.Context, id string) error {
	return c.doJSON(ctx, http.MethodDelete, "/api/v1/volume-group-snapshots/"+url.PathEscape(id), nil, nil)
}

func (c *HTTPClient) CloneSnapshot(ctx context.Context, snapID, cloneName string, labels map[string]string) (*store.Volume, error) {
	var resp struct {
		Volume *store.Volume `json:"volume"`
	}
	err := c.doJSON(ctx, http.MethodPost, "/api/v1/snapshots/"+url.PathEscape(snapID)+":clone", map[string]any{
		"clone_name": cloneName,
		"labels":     labels,
	}, &resp)
	return resp.Volume, err
}

func (c *HTTPClient) ListPortals(ctx context.Context) ([]*store.Portal, error) {
	var resp struct {
		Portals []*store.Portal `json:"portals"`
	}
	err := c.doJSON(ctx, http.MethodGet, "/api/v1/portals", nil, &resp)
	return resp.Portals, err
}

func (c *HTTPClient) ListHosts(ctx context.Context) ([]*store.Host, error) {
	var resp struct {
		Hosts []*store.Host `json:"hosts"`
	}
	err := c.doJSON(ctx, http.MethodGet, "/api/v1/hosts", nil, &resp)
	return resp.Hosts, err
}

func (c *HTTPClient) CreateHost(ctx context.Context, name, hostNQN, initiatorIQN, ip string) (*store.Host, error) {
	var resp struct {
		Host *store.Host `json:"host"`
	}
	err := c.doJSON(ctx, http.MethodPost, "/api/v1/hosts", map[string]any{
		"name":          name,
		"host_nqn":      hostNQN,
		"initiator_iqn": initiatorIQN,
		"ip_address":    ip,
	}, &resp)
	return resp.Host, err
}

func (c *HTTPClient) ListInitiatorGroups(ctx context.Context) ([]*store.InitiatorGroup, error) {
	var resp struct {
		InitiatorGroups []*store.InitiatorGroup `json:"initiator_groups"`
	}
	err := c.doJSON(ctx, http.MethodGet, "/api/v1/initiator-groups", nil, &resp)
	return resp.InitiatorGroups, err
}

func (c *HTTPClient) CreateInitiatorGroup(ctx context.Context, name string, hostIDs []string) (*store.InitiatorGroup, error) {
	var resp struct {
		InitiatorGroup *store.InitiatorGroup `json:"initiator_group"`
	}
	err := c.doJSON(ctx, http.MethodPost, "/api/v1/initiator-groups", map[string]any{
		"name":     name,
		"host_ids": hostIDs,
	}, &resp)
	return resp.InitiatorGroup, err
}

func (c *HTTPClient) ListExports(ctx context.Context) ([]*store.Export, error) {
	var resp struct {
		Exports []*store.Export `json:"exports"`
	}
	err := c.doJSON(ctx, http.MethodGet, "/api/v1/exports", nil, &resp)
	return resp.Exports, err
}

func (c *HTTPClient) CreateExport(ctx context.Context, req CreateExportRequest) (*store.Export, error) {
	var resp struct {
		Export *store.Export `json:"export"`
	}
	err := c.doJSON(ctx, http.MethodPost, "/api/v1/exports", req, &resp)
	return resp.Export, err
}

func (c *HTTPClient) UpdateExport(ctx context.Context, id string, req UpdateExportRequest) (*store.Export, error) {
	var resp struct {
		Export *store.Export `json:"export"`
	}
	err := c.doJSON(ctx, http.MethodPatch, "/api/v1/exports/"+url.PathEscape(id), req, &resp)
	return resp.Export, err
}

func (c *HTTPClient) DeleteExport(ctx context.Context, id string) error {
	return c.doJSON(ctx, http.MethodDelete, "/api/v1/exports/"+url.PathEscape(id), nil, nil)
}
