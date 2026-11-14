package server

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/velocor/velocor/internal/auth"
	"github.com/velocor/velocor/internal/hardware"
	"github.com/velocor/velocor/internal/reconciler"
	"github.com/velocor/velocor/internal/spdk"
	"github.com/velocor/velocor/internal/store"
	"github.com/velocor/velocor/internal/version"
)

// Server exposes the unified Velocor REST/OpenAPI API, Prometheus /metrics,
// and embedded Web UI over HTTP/HTTPS.
type Server struct {
	store      store.Store
	spdk       *spdk.Client
	hw         *hardware.Manager
	auth       *auth.Manager
	reconciler *reconciler.Reconciler
	logger     *slog.Logger
	startedAt  time.Time
	mux        *http.ServeMux
}

// New constructs a new control-plane Server and registers all routes.
func New(
	st store.Store,
	spdkClient *spdk.Client,
	hwMgr *hardware.Manager,
	authMgr *auth.Manager,
	rec *reconciler.Reconciler,
	logger *slog.Logger,
) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	s := &Server{
		store:      st,
		spdk:       spdkClient,
		hw:         hwMgr,
		auth:       authMgr,
		reconciler: rec,
		logger:     logger,
		startedAt:  time.Now().UTC(),
		mux:        http.NewServeMux(),
	}
	s.registerRoutes()
	return s
}

// Handler returns the root http.Handler for the server.
func (s *Server) Handler() http.Handler {
	return s.mux
}

func (s *Server) registerRoutes() {
	// Public routes (UI, OpenAPI spec, login, Prometheus scrape)
	s.mux.HandleFunc("GET /{$}", s.handleDashboard)
	s.mux.Handle("GET /assets/", s.handleWebAssets())
	s.mux.HandleFunc("GET /api/v1/openapi.json", s.handleOpenAPI)
	s.mux.HandleFunc("GET /metrics", s.handleMetrics)
	s.mux.HandleFunc("POST /api/v1/auth/login", s.handleLogin)

	// Auth, User & Token management
	s.mux.HandleFunc("GET /api/v1/auth/me", s.withAuth(store.RoleReadOnly, s.handleAuthMe))
	s.mux.HandleFunc("POST /api/v1/auth/password", s.withAuth(store.RoleReadOnly, s.handleChangePassword))
	s.mux.HandleFunc("GET /api/v1/auth/users", s.withAuth(store.RoleAdmin, s.handleListUsers))
	s.mux.HandleFunc("POST /api/v1/auth/users", s.withAuth(store.RoleAdmin, s.handleCreateUser))
	s.mux.HandleFunc("PATCH /api/v1/auth/users/{username}", s.withAuth(store.RoleAdmin, s.handleUpdateUser))
	s.mux.HandleFunc("DELETE /api/v1/auth/users/{username}", s.withAuth(store.RoleAdmin, s.handleDeleteUser))
	s.mux.HandleFunc("GET /api/v1/auth/tokens", s.withAuth(store.RoleAdmin, s.handleListTokens))
	s.mux.HandleFunc("POST /api/v1/auth/tokens", s.withAuth(store.RoleAdmin, s.handleCreateToken))
	s.mux.HandleFunc("DELETE /api/v1/auth/tokens/{id}", s.withAuth(store.RoleAdmin, s.handleDeleteToken))

	// Disks
	s.mux.HandleFunc("GET /api/v1/disks", s.withAuth(store.RoleReadOnly, s.handleListDisks))
	s.mux.HandleFunc("POST /api/v1/disks:scan", s.withAuth(store.RoleAdmin, s.handleScanDisks))
	s.mux.HandleFunc("POST /api/v1/disks/", s.withAuth(store.RoleAdmin, s.handleDiskAction))
	s.mux.HandleFunc("GET /api/v1/disks/{id}", s.withAuth(store.RoleReadOnly, s.handleGetDisk))

	// Pools
	s.mux.HandleFunc("GET /api/v1/pools", s.withAuth(store.RoleReadOnly, s.handleListPools))
	s.mux.HandleFunc("POST /api/v1/pools", s.withAuth(store.RoleAdmin, s.handleCreatePool))
	s.mux.HandleFunc("GET /api/v1/pools/{id}", s.withAuth(store.RoleReadOnly, s.handleGetPool))
	s.mux.HandleFunc("DELETE /api/v1/pools/{id}", s.withAuth(store.RoleAdmin, s.handleDeletePool))

	// Volumes & Snapshots (CSI_PROVISIONER / READONLY)
	s.mux.HandleFunc("GET /api/v1/volumes", s.withAuth(store.RoleReadOnly, s.handleListVolumes))
	s.mux.HandleFunc("POST /api/v1/volumes", s.withAuth(store.RoleCSIProvisioner, s.handleCreateVolume))
	s.mux.HandleFunc("GET /api/v1/volumes/{id}", s.withAuth(store.RoleReadOnly, s.handleGetVolume))
	s.mux.HandleFunc("PUT /api/v1/volumes/{id}/qos", s.withAuth(store.RoleCSIProvisioner, s.handleSetVolumeQoS))
	s.mux.HandleFunc("POST /api/v1/volumes/{volume_id}/snapshots", s.withAuth(store.RoleCSIProvisioner, s.handleCreateSnapshot))
	s.mux.HandleFunc("POST /api/v1/volumes/", s.withAuth(store.RoleCSIProvisioner, s.handleVolumeAction))
	s.mux.HandleFunc("DELETE /api/v1/volumes/{id}", s.withAuth(store.RoleCSIProvisioner, s.handleDeleteVolume))

	s.mux.HandleFunc("GET /api/v1/snapshots", s.withAuth(store.RoleReadOnly, s.handleListSnapshots))
	s.mux.HandleFunc("GET /api/v1/snapshots/", s.withAuth(store.RoleReadOnly, s.handleSnapshotGetOrBlocks))
	s.mux.HandleFunc("POST /api/v1/snapshots/", s.withAuth(store.RoleCSIProvisioner, s.handleSnapshotAction))
	s.mux.HandleFunc("DELETE /api/v1/snapshots/{id}", s.withAuth(store.RoleCSIProvisioner, s.handleDeleteSnapshot))

	// Volume Group Snapshots (Consistency Groups)
	s.mux.HandleFunc("POST /api/v1/volume-group-snapshots", s.withAuth(store.RoleCSIProvisioner, s.handleCreateVolumeGroupSnapshot))
	s.mux.HandleFunc("GET /api/v1/volume-group-snapshots", s.withAuth(store.RoleReadOnly, s.handleListVolumeGroupSnapshots))
	s.mux.HandleFunc("GET /api/v1/volume-group-snapshots/{id}", s.withAuth(store.RoleReadOnly, s.handleGetVolumeGroupSnapshot))
	s.mux.HandleFunc("DELETE /api/v1/volume-group-snapshots/{id}", s.withAuth(store.RoleCSIProvisioner, s.handleDeleteVolumeGroupSnapshot))

	// Portals, Hosts, InitiatorGroups & Exports
	s.mux.HandleFunc("GET /api/v1/portals", s.withAuth(store.RoleReadOnly, s.handleListPortals))
	s.mux.HandleFunc("POST /api/v1/portals", s.withAuth(store.RoleAdmin, s.handleCreatePortal))
	s.mux.HandleFunc("DELETE /api/v1/portals/{id}", s.withAuth(store.RoleAdmin, s.handleDeletePortal))

	s.mux.HandleFunc("GET /api/v1/hosts", s.withAuth(store.RoleReadOnly, s.handleListHosts))
	s.mux.HandleFunc("POST /api/v1/hosts", s.withAuth(store.RoleCSIProvisioner, s.handleCreateHost))
	s.mux.HandleFunc("GET /api/v1/hosts/{id}", s.withAuth(store.RoleReadOnly, s.handleGetHost))
	s.mux.HandleFunc("DELETE /api/v1/hosts/{id}", s.withAuth(store.RoleCSIProvisioner, s.handleDeleteHost))

	s.mux.HandleFunc("GET /api/v1/initiator-groups", s.withAuth(store.RoleReadOnly, s.handleListInitiatorGroups))
	s.mux.HandleFunc("POST /api/v1/initiator-groups", s.withAuth(store.RoleCSIProvisioner, s.handleCreateInitiatorGroup))
	s.mux.HandleFunc("GET /api/v1/initiator-groups/{id}", s.withAuth(store.RoleReadOnly, s.handleGetInitiatorGroup))
	s.mux.HandleFunc("DELETE /api/v1/initiator-groups/{id}", s.withAuth(store.RoleCSIProvisioner, s.handleDeleteInitiatorGroup))

	s.mux.HandleFunc("GET /api/v1/exports", s.withAuth(store.RoleReadOnly, s.handleListExports))
	s.mux.HandleFunc("POST /api/v1/exports", s.withAuth(store.RoleCSIProvisioner, s.handleCreateExport))
	s.mux.HandleFunc("GET /api/v1/exports/{id}", s.withAuth(store.RoleReadOnly, s.handleGetExport))
	s.mux.HandleFunc("DELETE /api/v1/exports/{id}", s.withAuth(store.RoleCSIProvisioner, s.handleDeleteExport))

	// System
	s.mux.HandleFunc("GET /api/v1/system/status", s.withAuth(store.RoleReadOnly, s.handleSystemStatus))
	s.mux.HandleFunc("POST /api/v1/system/reconcile", s.withAuth(store.RoleAdmin, s.handleTriggerReconcile))
	s.mux.HandleFunc("GET /api/v1/system/iostats", s.withAuth(store.RoleReadOnly, s.handleGetIoStats))
	s.mux.HandleFunc("GET /api/v1/system/os", s.withAuth(store.RoleReadOnly, s.handleGetOSStatus))
	s.mux.HandleFunc("POST /api/v1/system/upgrade", s.withAuth(store.RoleAdmin, s.handleStageUpgrade))
	s.mux.HandleFunc("POST /api/v1/system/rollback", s.withAuth(store.RoleAdmin, s.handleRollbackUpgrade))
	s.mux.HandleFunc("POST /api/v1/system/reboot", s.withAuth(store.RoleAdmin, s.handleRebootSystem))
	s.mux.HandleFunc("GET /api/v1/system/containers/updates", s.withAuth(store.RoleReadOnly, s.handleCheckContainerUpdates))
	s.mux.HandleFunc("POST /api/v1/system/containers/update", s.withAuth(store.RoleAdmin, s.handleApplyContainerUpdates))
	s.mux.HandleFunc("GET /api/v1/system/network", s.withAuth(store.RoleReadOnly, s.handleGetNetwork))
	s.mux.HandleFunc("PUT /api/v1/system/network/interfaces/{name}", s.withAuth(store.RoleAdmin, s.handleUpdateNetworkInterface))
}

type contextKey string

const (
	subjectContextKey contextKey = "subject"
	roleContextKey    contextKey = "role"
)

func subjectFromContext(ctx context.Context) string {
	v, _ := ctx.Value(subjectContextKey).(string)
	return v
}

func roleFromContext(ctx context.Context) store.Role {
	v, _ := ctx.Value(roleContextKey).(store.Role)
	return v
}

func (s *Server) withAuth(requiredRole store.Role, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		hdr := r.Header.Get("Authorization")
		if !strings.HasPrefix(hdr, "Bearer ") {
			writeError(w, auth.ErrUnauthorized)
			return
		}
		rawToken := strings.TrimPrefix(hdr, "Bearer ")
		sub, role, err := s.auth.AuthenticateBearer(rawToken)
		if err != nil {
			writeError(w, err)
			return
		}
		if err := auth.Authorize(role, requiredRole); err != nil {
			writeError(w, err)
			return
		}
		ctx := context.WithValue(r.Context(), subjectContextKey, sub)
		ctx = context.WithValue(ctx, roleContextKey, role)
		next(w, r.WithContext(ctx))
	}
}

func (s *Server) handleOpenAPI(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(embeddedOpenAPIJSON))
}

// --- Auth Handlers ---

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, fmt.Errorf("%w: invalid json body", store.ErrInvalidArgument))
		return
	}
	tok, exp, role, err := s.auth.Login(req.Username, req.Password)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token": tok,
		"expires_at":   exp,
		"username":     req.Username,
		"role":         role,
	})
}

func (s *Server) handleAuthMe(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"username": subjectFromContext(r.Context()),
		"role":     roleFromContext(r.Context()),
	})
}

func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	sub := subjectFromContext(r.Context())
	if sub == "" || strings.HasPrefix(sub, "token:") {
		writeError(w, auth.ErrForbidden)
		return
	}
	var req struct {
		CurrentPassword string `json:"current_password"`
		NewPassword     string `json:"new_password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, fmt.Errorf("%w: invalid json body", store.ErrInvalidArgument))
		return
	}
	if err := s.auth.ChangePassword(sub, req.CurrentPassword, req.NewPassword); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{})
}

type userView struct {
	Username  string     `json:"username"`
	Role      store.Role `json:"role"`
	CreatedAt time.Time  `json:"created_at,omitempty"`
	UpdatedAt time.Time  `json:"updated_at"`
}

func toUserView(u *store.AdminUser) userView {
	return userView{
		Username:  u.Username,
		Role:      u.Role,
		CreatedAt: u.CreatedAt,
		UpdatedAt: u.UpdatedAt,
	}
}

func (s *Server) handleListUsers(w http.ResponseWriter, _ *http.Request) {
	users, err := s.store.ListUsers()
	if err != nil {
		writeError(w, err)
		return
	}
	views := make([]userView, 0, len(users))
	for _, u := range users {
		views = append(views, toUserView(u))
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": views})
}

func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string     `json:"username"`
		Password string     `json:"password"`
		Role     store.Role `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, fmt.Errorf("%w: invalid json body", store.ErrInvalidArgument))
		return
	}
	user, err := s.auth.CreateUser(req.Username, req.Password, req.Role)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"user": toUserView(user)})
}

func (s *Server) handleUpdateUser(w http.ResponseWriter, r *http.Request) {
	username := r.PathValue("username")
	var req struct {
		Role     *store.Role `json:"role"`
		Password *string     `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, fmt.Errorf("%w: invalid json body", store.ErrInvalidArgument))
		return
	}
	user, err := s.auth.UpdateUser(username, req.Role, req.Password)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"user": toUserView(user)})
}

func (s *Server) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	username := r.PathValue("username")
	acting := subjectFromContext(r.Context())
	if err := s.auth.DeleteUser(username, acting); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{})
}

func (s *Server) handleCreateToken(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name       string     `json:"name"`
		Role       store.Role `json:"role"`
		TTLSeconds int64      `json:"ttl_seconds"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, fmt.Errorf("%w: invalid json body", store.ErrInvalidArgument))
		return
	}
	if req.Role == "" {
		req.Role = store.RoleCSIProvisioner
	}
	var ttl time.Duration
	if req.TTLSeconds > 0 {
		ttl = time.Duration(req.TTLSeconds) * time.Second
	}
	tok, rawSecret, err := s.auth.CreateApiToken(req.Name, req.Role, ttl)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"token":        tok,
		"secret_token": rawSecret,
	})
}

func (s *Server) handleListTokens(w http.ResponseWriter, _ *http.Request) {
	tokens, err := s.store.ListApiTokens()
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tokens": tokens})
}

func (s *Server) handleDeleteToken(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.store.DeleteApiToken(id); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{})
}

// --- Disk Handlers ---

func (s *Server) handleListDisks(w http.ResponseWriter, r *http.Request) {
	disks, err := s.store.ListDisks()
	if err != nil {
		writeError(w, err)
		return
	}
	typeFilter := strings.ToUpper(r.URL.Query().Get("type_filter"))
	unclaimedOnly := r.URL.Query().Get("unclaimed_only") == "true"

	var filtered []*store.Disk
	for _, d := range disks {
		if typeFilter != "" && typeFilter != "UNSPECIFIED" && string(d.Type) != typeFilter {
			continue
		}
		if unclaimedOnly && (d.Claimed || d.IsBootDevice) {
			continue
		}
		filtered = append(filtered, d)
	}
	writeJSON(w, http.StatusOK, map[string]any{"disks": filtered})
}

func (s *Server) handleGetDisk(w http.ResponseWriter, r *http.Request) {
	disk, err := s.store.GetDisk(r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"disk": disk})
}

func (s *Server) handleScanDisks(w http.ResponseWriter, _ *http.Request) {
	if s.hw != nil {
		_ = s.hw.RefreshBootGuard()
		discovered, err := s.hw.DiscoverDisks()
		if err != nil {
			writeError(w, err)
			return
		}
		for _, d := range discovered {
			if existing, err := s.store.GetDisk(d.ID); err == nil {
				if existing.Claimed && !d.IsBootDevice {
					d.Claimed = true
					d.DriverState = existing.DriverState
					d.SPDKBdevName = existing.SPDKBdevName
					d.PoolID = existing.PoolID
				}
			}
			if err := s.store.SyncDiscoveredDisk(d); err != nil {
				writeError(w, err)
				return
			}
		}
	}
	disks, err := s.store.ListDisks()
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"disks": disks})
}

func (s *Server) handleDiskAction(w http.ResponseWriter, r *http.Request) {
	// Matches POST /api/v1/disks/{id}:claim or POST /api/v1/disks/{id}:release
	tail := strings.TrimPrefix(r.URL.Path, "/api/v1/disks/")
	switch {
	case strings.HasSuffix(tail, ":claim"):
		id := strings.TrimSuffix(tail, ":claim")
		s.claimDisk(w, r, id)
	case strings.HasSuffix(tail, ":release"):
		id := strings.TrimSuffix(tail, ":release")
		s.releaseDisk(w, r, id)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) claimDisk(w http.ResponseWriter, r *http.Request, id string) {
	d, err := s.store.GetDisk(id)
	if err != nil {
		writeError(w, err)
		return
	}
	if d.IsBootDevice {
		writeError(w, store.ErrBootDeviceProtected)
		return
	}

	drvState := d.DriverState
	bdevName := d.SPDKBdevName
	switch d.Type {
	case store.DiskTypeNVMe:
		if s.hw != nil && d.PCIAddress != "" {
			boundState, err := s.hw.BindNVMeToUserspace(d.PCIAddress, "")
			if err != nil {
				writeError(w, err)
				return
			}
			drvState = boundState
		} else {
			drvState = store.DiskDriverStateVfioPCI
		}
		if s.spdk != nil {
			ctrlName := "Nvme_" + strings.ReplaceAll(strings.ReplaceAll(d.PCIAddress, ":", "_"), ".", "_")
			if attached, err := s.spdk.BdevNvmeAttachController(r.Context(), ctrlName, "PCIe", d.PCIAddress); err == nil && len(attached) > 0 {
				bdevName = attached[0]
			}
		}
		if bdevName == "" {
			bdevName = "Nvme_" + d.ID + "n1"
		}
	case store.DiskTypeSSD:
		drvState = store.DiskDriverStateUringClaimed
		bdevName = "uring_" + d.Name
		if s.spdk != nil && d.DevicePath != "" {
			if created, err := s.spdk.BdevUringCreate(r.Context(), bdevName, d.DevicePath, d.BlockSize); err == nil {
				bdevName = created
			}
		}
	}

	updated, err := s.store.ClaimDisk(id, drvState, bdevName)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"disk": updated})
}

func (s *Server) releaseDisk(w http.ResponseWriter, r *http.Request, id string) {
	d, err := s.store.GetDisk(id)
	if err != nil {
		writeError(w, err)
		return
	}
	if d.PoolID != "" {
		writeError(w, fmt.Errorf("%w: disk %s belongs to pool %s", store.ErrInUse, id, d.PoolID))
		return
	}
	if s.spdk != nil && d.SPDKBdevName != "" {
		switch d.Type {
		case store.DiskTypeNVMe:
			ctrlName := strings.TrimSuffix(d.SPDKBdevName, "n1")
			_ = s.spdk.BdevNvmeDetachController(r.Context(), ctrlName)
		case store.DiskTypeSSD:
			_ = s.spdk.BdevUringDelete(r.Context(), d.SPDKBdevName)
		}
	}
	if d.Type == store.DiskTypeNVMe && s.hw != nil && d.PCIAddress != "" {
		_ = s.hw.BindNVMeToKernel(d.PCIAddress)
	}
	updated, err := s.store.ReleaseDisk(id)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"disk": updated})
}

// --- Pool Handlers ---

func (s *Server) handleListPools(w http.ResponseWriter, _ *http.Request) {
	pools, err := s.store.ListPools()
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"pools": pools})
}

func (s *Server) handleGetPool(w http.ResponseWriter, r *http.Request) {
	pool, err := s.store.GetPool(r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"pool": pool})
}

func (s *Server) handleCreatePool(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name             string             `json:"name"`
		RaidLevel        store.RaidLevel    `json:"raid_level"`
		DiskIDs          []string           `json:"disk_ids"`
		StripSizeKB      uint32             `json:"strip_size_kb"`
		ClusterSizeBytes uint32             `json:"cluster_size_bytes"`
		FilterPolicy     store.FilterPolicy `json:"filter_policy"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, fmt.Errorf("%w: invalid json body", store.ErrInvalidArgument))
		return
	}
	if req.RaidLevel == "" {
		req.RaidLevel = store.RaidLevel0
	}
	if req.StripSizeKB == 0 {
		req.StripSizeKB = 64
	}
	if req.ClusterSizeBytes == 0 {
		req.ClusterSizeBytes = store.DefaultClusterSizeBytes
	}

	var totalRaw uint64
	var baseBdevs []string
	for _, diskID := range req.DiskIDs {
		d, err := s.store.GetDisk(diskID)
		if err != nil {
			writeError(w, fmt.Errorf("disk %s: %w", diskID, err))
			return
		}
		totalRaw += d.SizeBytes
		if d.SPDKBdevName != "" {
			baseBdevs = append(baseBdevs, d.SPDKBdevName)
		}
	}

	pool := &store.Pool{
		ID:                newResourceID("pool"),
		Name:              req.Name,
		RaidLevel:         req.RaidLevel,
		DiskIDs:           req.DiskIDs,
		StripSizeKB:       req.StripSizeKB,
		ClusterSizeBytes:  req.ClusterSizeBytes,
		BaseBdevName:      "raid_" + req.Name,
		EffectiveBdevName: "raid_" + req.Name,
		LVSName:           "lvs_" + req.Name,
		TotalBytes:        totalRaw,
		FreeBytes:         totalRaw,
		FilterPolicy:      req.FilterPolicy,
		HealthStatus:      store.HealthStatusOnline,
	}

	if err := s.store.CreatePool(pool); err != nil {
		writeError(w, err)
		return
	}

	if s.spdk != nil && len(baseBdevs) == len(req.DiskIDs) {
		if err := s.spdk.BdevRaidCreate(r.Context(), pool.BaseBdevName, string(pool.RaidLevel), pool.StripSizeKB, baseBdevs, false); err == nil {
			if lvsUUID, err := s.spdk.BdevLvolCreateLvstore(r.Context(), pool.EffectiveBdevName, pool.LVSName, pool.ClusterSizeBytes, ""); err == nil {
				pool.LVSUUID = lvsUUID
				_ = s.store.UpdatePool(pool)
			}
		}
	}

	writeJSON(w, http.StatusCreated, map[string]any{"pool": pool})
}

func (s *Server) handleDeletePool(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	pool, err := s.store.GetPool(id)
	if err != nil {
		writeError(w, err)
		return
	}
	if err := s.store.DeletePool(id); err != nil {
		writeError(w, err)
		return
	}
	if s.spdk != nil {
		_ = s.spdk.BdevLvolDeleteLvstore(r.Context(), pool.LVSUUID, pool.LVSName)
		_ = s.spdk.BdevRaidDelete(r.Context(), pool.BaseBdevName)
	}
	writeJSON(w, http.StatusOK, map[string]any{})
}

// --- Volume & Snapshot Handlers ---

func (s *Server) handleListVolumes(w http.ResponseWriter, r *http.Request) {
	poolID := r.URL.Query().Get("pool_id")
	vols, err := s.store.ListVolumes(poolID)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"volumes": vols})
}

func (s *Server) handleGetVolume(w http.ResponseWriter, r *http.Request) {
	vol, err := s.store.GetVolume(r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"volume": vol})
}

func (s *Server) handleCreateVolume(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name          string             `json:"name"`
		PoolID        string             `json:"pool_id"`
		SizeBytes     uint64             `json:"size_bytes"`
		ThinProvision bool               `json:"thin_provision"`
		FilterPolicy  store.FilterPolicy `json:"filter_policy"`
		QosPolicy     store.QosPolicy    `json:"qos_policy"`
		Labels        map[string]string  `json:"labels"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, fmt.Errorf("%w: invalid json body", store.ErrInvalidArgument))
		return
	}

	// Idempotency check by (PoolID, Name) for Kubernetes CSI CreateVolume retry semantics.
	if existing, err := s.store.GetVolumeByName(req.PoolID, req.Name); err == nil {
		if existing.SizeBytes == req.SizeBytes {
			writeJSON(w, http.StatusOK, map[string]any{"volume": existing})
			return
		}
		writeError(w, fmt.Errorf("%w: volume %s already exists with size %d", store.ErrAlreadyExists, req.Name, existing.SizeBytes))
		return
	}

	pool, err := s.store.GetPool(req.PoolID)
	if err != nil {
		writeError(w, err)
		return
	}

	lvolAlias := fmt.Sprintf("%s/%s", pool.LVSName, req.Name)
	vol := &store.Volume{
		ID:                newResourceID("vol"),
		Name:              req.Name,
		PoolID:            req.PoolID,
		LvolBdevName:      lvolAlias,
		EffectiveBdevName: lvolAlias,
		SizeBytes:         req.SizeBytes,
		ThinProvision:     req.ThinProvision,
		FilterPolicy:      req.FilterPolicy,
		QosPolicy:         req.QosPolicy,
		AttachmentState:   store.AttachmentStateUnattached,
		HealthStatus:      store.HealthStatusOnline,
		Labels:            req.Labels,
	}

	if err := s.store.CreateVolume(vol); err != nil {
		writeError(w, err)
		return
	}

	if s.spdk != nil {
		if uuid, err := s.spdk.BdevLvolCreate(r.Context(), pool.LVSName, vol.Name, vol.SizeBytes, vol.ThinProvision, ""); err == nil {
			vol.UUID = uuid
			_ = s.store.UpdateVolume(vol)
		}
		if vol.QosPolicy.RwIosPerSec > 0 || vol.QosPolicy.RwMbytesPerSec > 0 {
			_ = s.spdk.BdevSetQoSLimit(r.Context(), vol.EffectiveBdevName,
				vol.QosPolicy.RwIosPerSec,
				vol.QosPolicy.RwMbytesPerSec,
				vol.QosPolicy.RMbytesPerSec,
				vol.QosPolicy.WMbytesPerSec,
			)
		}
	}

	writeJSON(w, http.StatusCreated, map[string]any{"volume": vol})
}

func (s *Server) handleVolumeAction(w http.ResponseWriter, r *http.Request) {
	tail := strings.TrimPrefix(r.URL.Path, "/api/v1/volumes/")
	if strings.HasSuffix(tail, ":resize") {
		id := strings.TrimSuffix(tail, ":resize")
		s.resizeVolume(w, r, id)
		return
	}
	http.NotFound(w, r)
}

func (s *Server) resizeVolume(w http.ResponseWriter, r *http.Request, id string) {
	var req struct {
		NewSizeBytes uint64 `json:"new_size_bytes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, fmt.Errorf("%w: invalid json body", store.ErrInvalidArgument))
		return
	}
	vol, err := s.store.GetVolume(id)
	if err != nil {
		writeError(w, err)
		return
	}
	if req.NewSizeBytes < vol.SizeBytes {
		writeError(w, fmt.Errorf("%w: new_size_bytes (%d) must be >= current size (%d)", store.ErrInvalidArgument, req.NewSizeBytes, vol.SizeBytes))
		return
	}
	if s.spdk != nil {
		_ = s.spdk.BdevLvolResize(r.Context(), vol.LvolBdevName, req.NewSizeBytes)
	}
	vol.SizeBytes = req.NewSizeBytes
	if err := s.store.UpdateVolume(vol); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"volume": vol})
}

func (s *Server) handleSetVolumeQoS(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		QosPolicy store.QosPolicy `json:"qos_policy"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, fmt.Errorf("%w: invalid json body", store.ErrInvalidArgument))
		return
	}
	vol, err := s.store.GetVolume(id)
	if err != nil {
		writeError(w, err)
		return
	}
	if s.spdk != nil {
		_ = s.spdk.BdevSetQoSLimit(r.Context(), vol.EffectiveBdevName,
			req.QosPolicy.RwIosPerSec,
			req.QosPolicy.RwMbytesPerSec,
			req.QosPolicy.RMbytesPerSec,
			req.QosPolicy.WMbytesPerSec,
		)
	}
	vol.QosPolicy = req.QosPolicy
	if err := s.store.UpdateVolume(vol); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"volume": vol})
}

func (s *Server) handleDeleteVolume(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	vol, err := s.store.GetVolume(id)
	if err != nil {
		writeError(w, err)
		return
	}
	if err := s.store.DeleteVolume(id); err != nil {
		writeError(w, err)
		return
	}
	if s.spdk != nil {
		_ = s.spdk.BdevLvolDelete(r.Context(), vol.LvolBdevName)
	}
	writeJSON(w, http.StatusOK, map[string]any{})
}

func (s *Server) handleCreateSnapshot(w http.ResponseWriter, r *http.Request) {
	volID := r.PathValue("volume_id")
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, fmt.Errorf("%w: invalid json body", store.ErrInvalidArgument))
		return
	}

	// Idempotent by (VolumeID, Name) for Kubernetes CSI CreateSnapshot retries.
	if existing, err := s.store.GetSnapshotByName(volID, req.Name); err == nil {
		writeJSON(w, http.StatusOK, map[string]any{"snapshot": existing})
		return
	}

	vol, err := s.store.GetVolume(volID)
	if err != nil {
		writeError(w, err)
		return
	}
	pool, err := s.store.GetPool(vol.PoolID)
	if err != nil {
		writeError(w, err)
		return
	}

	snap := &store.Snapshot{
		ID:           newResourceID("snap"),
		Name:         req.Name,
		VolumeID:     vol.ID,
		PoolID:       pool.ID,
		LvolBdevName: fmt.Sprintf("%s/%s", pool.LVSName, req.Name),
		SizeBytes:    vol.SizeBytes,
	}
	if s.spdk != nil {
		if uuid, err := s.spdk.BdevLvolSnapshot(r.Context(), vol.LvolBdevName, req.Name); err == nil {
			snap.UUID = uuid
		}
	}
	if err := s.store.CreateSnapshot(snap); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"snapshot": snap})
}

func (s *Server) handleListSnapshots(w http.ResponseWriter, r *http.Request) {
	poolID := r.URL.Query().Get("pool_id")
	volID := r.URL.Query().Get("volume_id")
	snaps, err := s.store.ListSnapshots(poolID, volID)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"snapshots": snaps})
}

func (s *Server) handleSnapshotGetOrBlocks(w http.ResponseWriter, r *http.Request) {
	tail := strings.TrimPrefix(r.URL.Path, "/api/v1/snapshots/")
	switch {
	case strings.HasSuffix(tail, "/blocks:allocated"):
		snapID := strings.TrimSuffix(tail, "/blocks:allocated")
		s.handleGetSnapshotAllocatedBlocks(w, r, snapID)
	case strings.HasSuffix(tail, "/blocks:changed"):
		targetSnapID := strings.TrimSuffix(tail, "/blocks:changed")
		s.handleGetSnapshotChangedBlocks(w, r, targetSnapID)
	default:
		snap, err := s.store.GetSnapshot(tail)
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"snapshot": snap})
	}
}

func (s *Server) handleSnapshotAction(w http.ResponseWriter, r *http.Request) {
	tail := strings.TrimPrefix(r.URL.Path, "/api/v1/snapshots/")
	if strings.HasSuffix(tail, ":clone") {
		snapID := strings.TrimSuffix(tail, ":clone")
		s.cloneSnapshot(w, r, snapID)
		return
	}
	http.NotFound(w, r)
}

func (s *Server) cloneSnapshot(w http.ResponseWriter, r *http.Request, snapID string) {
	var req struct {
		CloneName    string             `json:"clone_name"`
		FilterPolicy store.FilterPolicy `json:"filter_policy"`
		QosPolicy    store.QosPolicy    `json:"qos_policy"`
		Labels       map[string]string  `json:"labels"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, fmt.Errorf("%w: invalid json body", store.ErrInvalidArgument))
		return
	}
	snap, err := s.store.GetSnapshot(snapID)
	if err != nil {
		writeError(w, err)
		return
	}
	pool, err := s.store.GetPool(snap.PoolID)
	if err != nil {
		writeError(w, err)
		return
	}

	lvolAlias := fmt.Sprintf("%s/%s", pool.LVSName, req.CloneName)
	clone := &store.Volume{
		ID:                newResourceID("vol"),
		Name:              req.CloneName,
		PoolID:            pool.ID,
		LvolBdevName:      lvolAlias,
		EffectiveBdevName: lvolAlias,
		SizeBytes:         snap.SizeBytes,
		ThinProvision:     true,
		ParentSnapshotID:  snap.ID,
		FilterPolicy:      req.FilterPolicy,
		QosPolicy:         req.QosPolicy,
		AttachmentState:   store.AttachmentStateUnattached,
		HealthStatus:      store.HealthStatusOnline,
		Labels:            req.Labels,
	}
	if s.spdk != nil {
		if uuid, err := s.spdk.BdevLvolClone(r.Context(), snap.LvolBdevName, req.CloneName); err == nil {
			clone.UUID = uuid
		}
	}
	if err := s.store.CreateVolume(clone); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"volume": clone})
}

func (s *Server) handleGetSnapshotAllocatedBlocks(w http.ResponseWriter, r *http.Request, snapID string) {
	snap, err := s.store.GetSnapshot(snapID)
	if err != nil {
		writeError(w, err)
		return
	}
	pool, err := s.store.GetPool(snap.PoolID)
	if err != nil {
		writeError(w, err)
		return
	}

	// Walk ancestor lineage to union all allocated clusters up to snapID.
	lineage, err := s.collectSnapshotLineage(snap, "")
	if err != nil {
		writeError(w, err)
		return
	}

	var extentSets [][]spdk.BlockExtent
	if s.spdk != nil {
		for _, layer := range lineage {
			frag, err := s.spdk.BdevLvolGetFragmap(r.Context(), layer.LvolBdevName, 0, layer.SizeBytes)
			if err != nil {
				writeError(w, err)
				return
			}
			ext, err := spdk.DecodeFragmapExtents(frag)
			if err != nil {
				writeError(w, err)
				return
			}
			extentSets = append(extentSets, ext)
		}
	}
	merged := spdk.MergeBlockExtents(extentSets...)
	writeJSON(w, http.StatusOK, map[string]any{
		"snapshot_id":        snap.ID,
		"volume_size_bytes":  snap.SizeBytes,
		"cluster_size_bytes": pool.ClusterSizeBytes,
		"extents":            formatBlockExtents(merged),
	})
}

func (s *Server) handleGetSnapshotChangedBlocks(w http.ResponseWriter, r *http.Request, targetSnapID string) {
	baseSnapID := r.URL.Query().Get("base_snapshot_id")
	if baseSnapID == "" {
		writeError(w, fmt.Errorf("%w: base_snapshot_id query parameter is required", store.ErrInvalidArgument))
		return
	}
	targetSnap, err := s.store.GetSnapshot(targetSnapID)
	if err != nil {
		writeError(w, err)
		return
	}
	baseSnap, err := s.store.GetSnapshot(baseSnapID)
	if err != nil {
		writeError(w, err)
		return
	}
	if targetSnap.VolumeID != baseSnap.VolumeID {
		writeError(w, fmt.Errorf("%w: snapshots %s and %s belong to different volumes", store.ErrInvalidArgument, baseSnapID, targetSnapID))
		return
	}
	pool, err := s.store.GetPool(targetSnap.PoolID)
	if err != nil {
		writeError(w, err)
		return
	}

	lineage, err := s.collectSnapshotLineage(targetSnap, baseSnap.ID)
	if err != nil {
		writeError(w, err)
		return
	}

	var extentSets [][]spdk.BlockExtent
	if s.spdk != nil {
		for _, layer := range lineage {
			frag, err := s.spdk.BdevLvolGetFragmap(r.Context(), layer.LvolBdevName, 0, layer.SizeBytes)
			if err != nil {
				writeError(w, err)
				return
			}
			ext, err := spdk.DecodeFragmapExtents(frag)
			if err != nil {
				writeError(w, err)
				return
			}
			extentSets = append(extentSets, ext)
		}
	}
	merged := spdk.MergeBlockExtents(extentSets...)
	writeJSON(w, http.StatusOK, map[string]any{
		"base_snapshot_id":   baseSnap.ID,
		"target_snapshot_id": targetSnap.ID,
		"volume_size_bytes":  targetSnap.SizeBytes,
		"cluster_size_bytes": pool.ClusterSizeBytes,
		"extents":            formatBlockExtents(merged),
	})
}

func (s *Server) collectSnapshotLineage(target *store.Snapshot, stopAtParentID string) ([]*store.Snapshot, error) {
	var chain []*store.Snapshot
	cur := target
	for cur != nil {
		chain = append(chain, cur)
		if cur.ParentSnapshotID == "" || cur.ParentSnapshotID == stopAtParentID {
			if stopAtParentID != "" && cur.ParentSnapshotID != stopAtParentID {
				return nil, fmt.Errorf("%w: snapshot %s is not an ancestor of %s", store.ErrInvalidArgument, stopAtParentID, target.ID)
			}
			break
		}
		parent, err := s.store.GetSnapshot(cur.ParentSnapshotID)
		if err != nil {
			return nil, fmt.Errorf("broken snapshot lineage at %s: %w", cur.ParentSnapshotID, err)
		}
		cur = parent
	}
	return chain, nil
}

func formatBlockExtents(extents []spdk.BlockExtent) []map[string]uint64 {
	out := make([]map[string]uint64, 0, len(extents))
	for _, e := range extents {
		out = append(out, map[string]uint64{
			"offset_bytes": e.OffsetBytes,
			"size_bytes":   e.SizeBytes,
		})
	}
	return out
}

func (s *Server) handleDeleteSnapshot(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	snap, err := s.store.GetSnapshot(id)
	if err != nil {
		writeError(w, err)
		return
	}
	if err := s.store.DeleteSnapshot(id); err != nil {
		writeError(w, err)
		return
	}
	if s.spdk != nil {
		_ = s.spdk.BdevLvolDelete(r.Context(), snap.LvolBdevName)
	}
	writeJSON(w, http.StatusOK, map[string]any{})
}

func (s *Server) handleCreateVolumeGroupSnapshot(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name      string   `json:"name"`
		VolumeIDs []string `json:"volume_ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, fmt.Errorf("%w: invalid json body", store.ErrInvalidArgument))
		return
	}
	if req.Name == "" || len(req.VolumeIDs) == 0 {
		writeError(w, fmt.Errorf("%w: name and non-empty volume_ids are required", store.ErrInvalidArgument))
		return
	}

	seen := make(map[string]struct{}, len(req.VolumeIDs))
	dedupVolIDs := make([]string, 0, len(req.VolumeIDs))
	for _, id := range req.VolumeIDs {
		if id == "" {
			writeError(w, fmt.Errorf("%w: volume_ids must not contain empty values", store.ErrInvalidArgument))
			return
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		dedupVolIDs = append(dedupVolIDs, id)
	}

	// Idempotent by Name when volume sets match; AlreadyExists on mismatch.
	if existing, err := s.store.GetVolumeGroupSnapshotByName(req.Name); err == nil {
		if !equalStringSets(existing.VolumeIDs, dedupVolIDs) {
			writeError(w, fmt.Errorf("%w: volume group snapshot %s already exists with different volumes", store.ErrAlreadyExists, req.Name))
			return
		}
		snaps := s.resolveGroupSnapshots(existing.SnapshotIDs)
		writeJSON(w, http.StatusOK, map[string]any{
			"volume_group_snapshot": existing,
			"snapshots":             snaps,
		})
		return
	}

	// Pre-validate all volumes and pools exist before creating any snapshots.
	type volPool struct {
		vol  *store.Volume
		pool *store.Pool
	}
	targets := make([]volPool, 0, len(dedupVolIDs))
	for _, volID := range dedupVolIDs {
		vol, err := s.store.GetVolume(volID)
		if err != nil {
			writeError(w, err)
			return
		}
		pool, err := s.store.GetPool(vol.PoolID)
		if err != nil {
			writeError(w, err)
			return
		}
		targets = append(targets, volPool{vol: vol, pool: pool})
	}

	// Take snapshots across all member volumes in a tight coordinated loop.
	createdSnaps := make([]*store.Snapshot, 0, len(targets))
	snapIDs := make([]string, 0, len(targets))
	rollback := func() {
		for _, cs := range createdSnaps {
			_ = s.store.DeleteSnapshot(cs.ID)
			if s.spdk != nil {
				_ = s.spdk.BdevLvolDelete(r.Context(), cs.LvolBdevName)
			}
		}
	}

	for _, t := range targets {
		snapName := fmt.Sprintf("%s-%s", req.Name, t.vol.ID)
		snap := &store.Snapshot{
			ID:           newResourceID("snap"),
			Name:         snapName,
			VolumeID:     t.vol.ID,
			PoolID:       t.pool.ID,
			LvolBdevName: fmt.Sprintf("%s/%s", t.pool.LVSName, snapName),
			SizeBytes:    t.vol.SizeBytes,
		}
		if s.spdk != nil {
			if uuid, err := s.spdk.BdevLvolSnapshot(r.Context(), t.vol.LvolBdevName, snapName); err == nil {
				snap.UUID = uuid
			}
		}
		if err := s.store.CreateSnapshot(snap); err != nil {
			rollback()
			writeError(w, err)
			return
		}
		createdSnaps = append(createdSnaps, snap)
		snapIDs = append(snapIDs, snap.ID)
	}

	vgs := &store.VolumeGroupSnapshot{
		ID:          newResourceID("vgs"),
		Name:        req.Name,
		VolumeIDs:   dedupVolIDs,
		SnapshotIDs: snapIDs,
		ReadyToUse:  true,
	}
	if err := s.store.CreateVolumeGroupSnapshot(vgs); err != nil {
		rollback()
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"volume_group_snapshot": vgs,
		"snapshots":             createdSnaps,
	})
}

func (s *Server) handleListVolumeGroupSnapshots(w http.ResponseWriter, r *http.Request) {
	list, err := s.store.ListVolumeGroupSnapshots()
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"volume_group_snapshots": list})
}

func (s *Server) handleGetVolumeGroupSnapshot(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	vgs, err := s.store.GetVolumeGroupSnapshot(id)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"volume_group_snapshot": vgs,
		"snapshots":             s.resolveGroupSnapshots(vgs.SnapshotIDs),
	})
}

func (s *Server) handleDeleteVolumeGroupSnapshot(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	vgs, err := s.store.GetVolumeGroupSnapshot(id)
	if err != nil {
		writeError(w, err)
		return
	}
	if err := s.store.DeleteVolumeGroupSnapshot(id); err != nil {
		writeError(w, err)
		return
	}
	for _, snapID := range vgs.SnapshotIDs {
		snap, err := s.store.GetSnapshot(snapID)
		if err != nil {
			continue
		}
		if err := s.store.DeleteSnapshot(snapID); err == nil && s.spdk != nil {
			_ = s.spdk.BdevLvolDelete(r.Context(), snap.LvolBdevName)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{})
}

func (s *Server) resolveGroupSnapshots(snapIDs []string) []*store.Snapshot {
	snaps := make([]*store.Snapshot, 0, len(snapIDs))
	for _, snapID := range snapIDs {
		if snap, err := s.store.GetSnapshot(snapID); err == nil {
			snaps = append(snaps, snap)
		}
	}
	return snaps
}

func equalStringSets(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	set := make(map[string]struct{}, len(a))
	for _, v := range a {
		set[v] = struct{}{}
	}
	for _, v := range b {
		if _, ok := set[v]; !ok {
			return false
		}
	}
	return true
}

// --- Portal, InitiatorGroup & Export Handlers ---

func (s *Server) handleListPortals(w http.ResponseWriter, r *http.Request) {
	protoFilter := store.ExportProtocol(strings.ToUpper(r.URL.Query().Get("protocol_filter")))
	portals, err := s.store.ListPortals(protoFilter)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"portals": portals})
}

func (s *Server) handleCreatePortal(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name      string               `json:"name"`
		Protocol  store.ExportProtocol `json:"protocol"`
		IPAddress string               `json:"ip_address"`
		Port      uint32               `json:"port"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, fmt.Errorf("%w: invalid json body", store.ErrInvalidArgument))
		return
	}
	if req.Protocol == "" {
		req.Protocol = store.ExportProtocolNVMeoFTCP
	}
	if req.Port == 0 {
		if req.Protocol == store.ExportProtocolISCSI {
			req.Port = 3260
		} else {
			req.Port = 4420
		}
	}
	portal := &store.Portal{
		ID:        newResourceID("portal"),
		Name:      req.Name,
		Protocol:  req.Protocol,
		IPAddress: req.IPAddress,
		Port:      req.Port,
	}
	if err := s.store.CreatePortal(portal); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"portal": portal})
}

func (s *Server) handleDeletePortal(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeletePortal(r.PathValue("id")); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{})
}

func (s *Server) handleListHosts(w http.ResponseWriter, _ *http.Request) {
	hosts, err := s.store.ListHosts()
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"hosts": hosts})
}

func (s *Server) handleGetHost(w http.ResponseWriter, r *http.Request) {
	host, err := s.store.GetHost(r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"host": host})
}

func (s *Server) handleCreateHost(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name         string `json:"name"`
		HostNQN      string `json:"host_nqn"`
		InitiatorIQN string `json:"initiator_iqn"`
		IPAddress    string `json:"ip_address"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, fmt.Errorf("%w: invalid json body", store.ErrInvalidArgument))
		return
	}
	host := &store.Host{
		ID:           newResourceID("host"),
		Name:         req.Name,
		HostNQN:      strings.TrimSpace(req.HostNQN),
		InitiatorIQN: strings.TrimSpace(req.InitiatorIQN),
		IPAddress:    strings.TrimSpace(req.IPAddress),
	}
	if err := s.store.CreateHost(host); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"host": host})
}

func (s *Server) handleDeleteHost(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeleteHost(r.PathValue("id")); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{})
}

func (s *Server) handleListInitiatorGroups(w http.ResponseWriter, _ *http.Request) {
	groups, err := s.store.ListInitiatorGroups()
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"initiator_groups": groups})
}

func (s *Server) handleGetInitiatorGroup(w http.ResponseWriter, r *http.Request) {
	group, err := s.store.GetInitiatorGroup(r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"initiator_group": group})
}

func (s *Server) handleCreateInitiatorGroup(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name          string   `json:"name"`
		HostIDs       []string `json:"host_ids"`
		HostNQNs      []string `json:"host_nqns"`
		InitiatorIQNs []string `json:"initiator_iqns"`
		AllowedCIDRs  []string `json:"allowed_cidrs"`
		CHAPUser      string   `json:"chap_user"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, fmt.Errorf("%w: invalid json body", store.ErrInvalidArgument))
		return
	}
	group := &store.InitiatorGroup{
		ID:            newResourceID("ig"),
		Name:          req.Name,
		HostIDs:       req.HostIDs,
		HostNQNs:      req.HostNQNs,
		InitiatorIQNs: req.InitiatorIQNs,
		AllowedCIDRs:  req.AllowedCIDRs,
		CHAPUser:      req.CHAPUser,
	}
	if err := s.store.CreateInitiatorGroup(group); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"initiator_group": group})
}

func (s *Server) handleDeleteInitiatorGroup(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeleteInitiatorGroup(r.PathValue("id")); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{})
}

func (s *Server) resolveInitiatorGroupACLs(ig *store.InitiatorGroup) (hostNQNs, initiatorIQNs, cidrs []string) {
	seenNQN := make(map[string]bool)
	seenIQN := make(map[string]bool)
	seenCIDR := make(map[string]bool)

	addNQN := func(v string) {
		v = strings.TrimSpace(v)
		if v != "" && !seenNQN[v] {
			seenNQN[v] = true
			hostNQNs = append(hostNQNs, v)
		}
	}
	addIQN := func(v string) {
		v = strings.TrimSpace(v)
		if v != "" && !seenIQN[v] {
			seenIQN[v] = true
			initiatorIQNs = append(initiatorIQNs, v)
		}
	}
	addCIDR := func(v string) {
		v = strings.TrimSpace(v)
		if v != "" && !seenCIDR[v] {
			seenCIDR[v] = true
			cidrs = append(cidrs, v)
		}
	}

	for _, hid := range ig.HostIDs {
		if h, err := s.store.GetHost(hid); err == nil {
			addNQN(h.HostNQN)
			addIQN(h.InitiatorIQN)
			if h.IPAddress != "" {
				if strings.Contains(h.IPAddress, "/") {
					addCIDR(h.IPAddress)
				} else {
					addCIDR(h.IPAddress + "/32")
				}
			}
		}
	}
	for _, nqn := range ig.HostNQNs {
		addNQN(nqn)
	}
	for _, iqn := range ig.InitiatorIQNs {
		addIQN(iqn)
	}
	for _, cidr := range ig.AllowedCIDRs {
		addCIDR(cidr)
	}
	return hostNQNs, initiatorIQNs, cidrs
}

func (s *Server) handleListExports(w http.ResponseWriter, r *http.Request) {
	volID := r.URL.Query().Get("volume_id")
	protoFilter := store.ExportProtocol(strings.ToUpper(r.URL.Query().Get("protocol_filter")))
	exps, err := s.store.ListExports(volID, protoFilter)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"exports": exps})
}

func (s *Server) handleGetExport(w http.ResponseWriter, r *http.Request) {
	exp, err := s.store.GetExport(r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"export": exp})
}

func (s *Server) handleCreateExport(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name              string               `json:"name"`
		VolumeID          string               `json:"volume_id"`
		Protocol          store.ExportProtocol `json:"protocol"`
		SubsystemNQN      string               `json:"subsystem_nqn"`
		NamespaceID       uint32               `json:"namespace_id"`
		TargetIQN         string               `json:"target_iqn"`
		LUNID             uint32               `json:"lun_id"`
		PortalIDs         []string             `json:"portal_ids"`
		InitiatorGroupIDs []string             `json:"initiator_group_ids"`
		AllowAnyHost      bool                 `json:"allow_any_host"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, fmt.Errorf("%w: invalid json body", store.ErrInvalidArgument))
		return
	}
	if req.Protocol == "" {
		req.Protocol = store.ExportProtocolNVMeoFTCP
	}

	// Idempotent by Name for Kubernetes CSI ControllerPublishVolume retries.
	if existing, err := s.store.GetExportByName(req.Name); err == nil {
		if existing.VolumeID == req.VolumeID {
			writeJSON(w, http.StatusOK, map[string]any{"export": existing})
			return
		}
		writeError(w, fmt.Errorf("%w: export %s already exists for volume %s", store.ErrAlreadyExists, req.Name, existing.VolumeID))
		return
	}

	vol, err := s.store.GetVolume(req.VolumeID)
	if err != nil {
		writeError(w, err)
		return
	}

	if (req.Protocol == store.ExportProtocolNVMeoFTCP || req.Protocol == store.ExportProtocolNVMeoFRDMA) && req.SubsystemNQN == "" {
		req.SubsystemNQN = "nqn.2026-11.io.velocor:" + req.Name
	}
	if req.NamespaceID == 0 {
		req.NamespaceID = 1
	}
	if req.Protocol == store.ExportProtocolISCSI && req.TargetIQN == "" {
		req.TargetIQN = "iqn.2026-11.io.velocor:" + req.Name
	}

	exp := &store.Export{
		ID:                newResourceID("exp"),
		Name:              req.Name,
		VolumeID:          req.VolumeID,
		Protocol:          req.Protocol,
		SubsystemNQN:      req.SubsystemNQN,
		NamespaceID:       req.NamespaceID,
		TargetIQN:         req.TargetIQN,
		LUNID:             req.LUNID,
		PortalIDs:         req.PortalIDs,
		InitiatorGroupIDs: req.InitiatorGroupIDs,
		AllowAnyHost:      req.AllowAnyHost,
		HealthStatus:      store.HealthStatusOnline,
	}

	if err := s.store.CreateExport(exp); err != nil {
		writeError(w, err)
		return
	}

	if s.spdk != nil && (exp.Protocol == store.ExportProtocolNVMeoFTCP || exp.Protocol == store.ExportProtocolNVMeoFRDMA) {
		trtype := "TCP"
		if exp.Protocol == store.ExportProtocolNVMeoFRDMA {
			trtype = "RDMA"
		}
		_ = s.spdk.NvmfCreateTransport(r.Context(), trtype)
		serial := "VELO" + strings.ToUpper(strings.ReplaceAll(exp.ID, "-", ""))
		if len(serial) > 20 {
			serial = serial[:20]
		}
		if err := s.spdk.NvmfCreateSubsystem(r.Context(), exp.SubsystemNQN, serial, "", exp.AllowAnyHost, 32); err == nil {
			_, _ = s.spdk.NvmfSubsystemAddNs(r.Context(), exp.SubsystemNQN, vol.EffectiveBdevName, exp.NamespaceID, "")
			for _, gid := range exp.InitiatorGroupIDs {
				if ig, err := s.store.GetInitiatorGroup(gid); err == nil {
					nqns, _, _ := s.resolveInitiatorGroupACLs(ig)
					for _, nqn := range nqns {
						_ = s.spdk.NvmfSubsystemAddHost(r.Context(), exp.SubsystemNQN, nqn)
					}
				}
			}
			for _, pid := range exp.PortalIDs {
				if p, err := s.store.GetPortal(pid); err == nil {
					_ = s.spdk.NvmfSubsystemAddListener(r.Context(), exp.SubsystemNQN, trtype, "IPv4", p.IPAddress, strconv.Itoa(int(p.Port)))
				}
			}
		}
	} else if s.spdk != nil && exp.Protocol == store.ExportProtocolISCSI {
		pgTag := 1
		igTag := 1
		var spdkPortals []spdk.IscsiPortal
		for _, pid := range exp.PortalIDs {
			if p, err := s.store.GetPortal(pid); err == nil {
				spdkPortals = append(spdkPortals, spdk.IscsiPortal{
					Host: p.IPAddress,
					Port: strconv.Itoa(int(p.Port)),
				})
			}
		}
		if len(spdkPortals) > 0 {
			_ = s.spdk.IscsiCreatePortalGroup(r.Context(), pgTag, spdkPortals)
		}
		iqns := []string{"ANY"}
		netmasks := []string{"0.0.0.0/1", "128.0.0.0/1"}
		if !exp.AllowAnyHost && len(exp.InitiatorGroupIDs) > 0 {
			var resolvedIQNs, resolvedCIDRs []string
			for _, gid := range exp.InitiatorGroupIDs {
				if ig, err := s.store.GetInitiatorGroup(gid); err == nil {
					_, gIQNs, gCIDRs := s.resolveInitiatorGroupACLs(ig)
					resolvedIQNs = append(resolvedIQNs, gIQNs...)
					resolvedCIDRs = append(resolvedCIDRs, gCIDRs...)
				}
			}
			if len(resolvedIQNs) > 0 {
				iqns = resolvedIQNs
			}
			if len(resolvedCIDRs) > 0 {
				netmasks = resolvedCIDRs
			}
		}
		_ = s.spdk.IscsiCreateInitiatorGroup(r.Context(), igTag, iqns, netmasks)
		_ = s.spdk.IscsiCreateTargetNode(
			r.Context(),
			exp.TargetIQN,
			exp.Name,
			[]spdk.IscsiLun{{BdevName: vol.EffectiveBdevName, LunID: int(exp.LUNID)}},
			[]spdk.IscsiPgIgMap{{PgTag: pgTag, IgTag: igTag}},
			64,
			true, false, false, 0,
		)
	}

	writeJSON(w, http.StatusCreated, map[string]any{"export": exp})
}

func (s *Server) handleDeleteExport(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	exp, err := s.store.GetExport(id)
	if err != nil {
		writeError(w, err)
		return
	}
	if err := s.store.DeleteExport(id); err != nil {
		writeError(w, err)
		return
	}
	if s.spdk != nil {
		switch exp.Protocol {
		case store.ExportProtocolNVMeoFTCP, store.ExportProtocolNVMeoFRDMA:
			_ = s.spdk.NvmfDeleteSubsystem(r.Context(), exp.SubsystemNQN)
		case store.ExportProtocolISCSI:
			_ = s.spdk.IscsiDeleteTargetNode(r.Context(), exp.TargetIQN)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{})
}

// --- System & Metrics Handlers ---

func (s *Server) handleSystemStatus(w http.ResponseWriter, r *http.Request) {
	hostname, _ := os.Hostname()
	var nodeID string
	if cfg, err := s.store.GetSystemConfig(); err == nil {
		nodeID = cfg.NodeID
	}

	spdkConnected := false
	spdkVersion := ""
	var socketPath string
	if s.spdk != nil {
		socketPath = s.spdk.SocketPath()
		if ver, err := s.spdk.GetVersion(r.Context()); err == nil {
			spdkConnected = true
			spdkVersion = ver
		}
	}

	var hpTotalMB, hpFreeMB uint64
	if s.hw != nil {
		if hp, err := s.hw.GetHugepageStatus(); err == nil {
			hpTotalMB = hp.TotalMB
			hpFreeMB = hp.FreeMB
		}
	}

	var recState reconciler.State
	if s.reconciler != nil {
		recState = s.reconciler.State()
	}

	overall := store.HealthStatusOnline
	if !spdkConnected || len(recState.DegradedResources) > 0 {
		overall = store.HealthStatusDegraded
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"overall_health": overall,
		"node": map[string]any{
			"node_id":        nodeID,
			"hostname":       hostname,
			"version":        version.Version,
			"commit":         version.Commit,
			"build_time":     version.BuildTime,
			"spdk_version":   spdkVersion,
			"uptime_seconds": uint64(time.Since(s.startedAt).Seconds()),
		},
		"spdk": map[string]any{
			"connected":          spdkConnected,
			"socket_path":        socketPath,
			"hugepages_total_mb": hpTotalMB,
			"hugepages_free_mb":  hpFreeMB,
		},
		"reconciler": recState,
	})
}

func (s *Server) handleTriggerReconcile(w http.ResponseWriter, r *http.Request) {
	if s.reconciler == nil {
		writeJSON(w, http.StatusOK, map[string]any{"reconciler": reconciler.State{}})
		return
	}
	state, _ := s.reconciler.ReconcileOnce(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{"reconciler": state})
}

func (s *Server) handleGetIoStats(w http.ResponseWriter, r *http.Request) {
	bdevName := r.URL.Query().Get("bdev_name")
	if s.spdk == nil {
		writeJSON(w, http.StatusOK, map[string]any{"tick_rate": 0, "stats": []any{}})
		return
	}
	res, err := s.spdk.BdevGetIostat(r.Context(), bdevName)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"tick_rate": res.TickRate,
		"stats":     res.Bdevs,
	})
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	var sb strings.Builder

	sb.WriteString("# HELP velocor_up Whether the Velocor control plane is up.\n")
	sb.WriteString("# TYPE velocor_up gauge\n")
	sb.WriteString("velocor_up 1\n")

	if s.reconciler != nil {
		st := s.reconciler.State()
		sb.WriteString("# HELP velocor_reconciler_drift_repaired_total Total resources repaired by reconciler.\n")
		sb.WriteString("# TYPE velocor_reconciler_drift_repaired_total counter\n")
		sb.WriteString(fmt.Sprintf("velocor_reconciler_drift_repaired_total %d\n", st.DriftRepairedCount))
		sb.WriteString("# HELP velocor_reconciler_degraded_resources Number of currently degraded resources.\n")
		sb.WriteString("# TYPE velocor_reconciler_degraded_resources gauge\n")
		sb.WriteString(fmt.Sprintf("velocor_reconciler_degraded_resources %d\n", len(st.DegradedResources)))
	}

	if pools, err := s.store.ListPools(); err == nil {
		sb.WriteString("# HELP velocor_pool_total_bytes Total capacity of storage pool in bytes.\n")
		sb.WriteString("# TYPE velocor_pool_total_bytes gauge\n")
		sb.WriteString("# HELP velocor_pool_free_bytes Free capacity of storage pool in bytes.\n")
		sb.WriteString("# TYPE velocor_pool_free_bytes gauge\n")
		for _, p := range pools {
			sb.WriteString(fmt.Sprintf("velocor_pool_total_bytes{pool=%q,raid=%q} %d\n", p.Name, p.RaidLevel, p.TotalBytes))
			sb.WriteString(fmt.Sprintf("velocor_pool_free_bytes{pool=%q,raid=%q} %d\n", p.Name, p.RaidLevel, p.FreeBytes))
		}
	}

	if disks, err := s.store.ListDisks(); err == nil {
		sb.WriteString("# HELP velocor_disk_temperature_celsius NVMe/SSD SMART temperature in Celsius.\n")
		sb.WriteString("# TYPE velocor_disk_temperature_celsius gauge\n")
		for _, d := range disks {
			sb.WriteString(fmt.Sprintf("velocor_disk_temperature_celsius{disk=%q,type=%q} %d\n", d.ID, d.Type, d.Smart.TemperatureCelsius))
		}
	}

	if s.spdk != nil {
		if iostat, err := s.spdk.BdevGetIostat(r.Context(), ""); err == nil {
			sb.WriteString("# HELP velocor_bdev_read_ops_total Total read operations on SPDK bdev.\n")
			sb.WriteString("# TYPE velocor_bdev_read_ops_total counter\n")
			sb.WriteString("# HELP velocor_bdev_write_ops_total Total write operations on SPDK bdev.\n")
			sb.WriteString("# TYPE velocor_bdev_write_ops_total counter\n")
			for _, b := range iostat.Bdevs {
				sb.WriteString(fmt.Sprintf("velocor_bdev_read_ops_total{bdev=%q} %d\n", b.Name, b.NumReadOps))
				sb.WriteString(fmt.Sprintf("velocor_bdev_write_ops_total{bdev=%q} %d\n", b.Name, b.NumWriteOps))
			}
		}
	}

	_, _ = w.Write([]byte(sb.String()))
}

// GenerateSelfSignedTLSConfig generates an ephemeral ECDSA P-256 TLS certificate in memory
// when no external TLS certificate is configured.
func GenerateSelfSignedTLSConfig() (*tls.Config, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	tmpl := x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			Organization: []string{"Velocor Storage Appliance"},
			CommonName:   "velocor.local",
		},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{"localhost", "velocor.local"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}
	derBytes, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		return nil, err
	}
	cert := tls.Certificate{
		Certificate: [][]byte{derBytes},
		PrivateKey:  priv,
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{cert},
	}, nil
}

// LoadOrGenerateSelfSignedTLSConfig loads an existing TLS certificate/key pair from disk,
// or generates and persists a new ECDSA P-256 self-signed certificate so restarts/upgrades
// preserve the appliance's TLS identity.
func LoadOrGenerateSelfSignedTLSConfig(certPath, keyPath string) (*tls.Config, error) {
	if cert, err := tls.LoadX509KeyPair(certPath, keyPath); err == nil {
		return &tls.Config{
			MinVersion:   tls.VersionTLS12,
			Certificates: []tls.Certificate{cert},
		}, nil
	}

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}

	dnsNames := []string{"localhost", "velocor.local"}
	if hn, err := os.Hostname(); err == nil && hn != "" {
		dnsNames = append(dnsNames, hn)
	}
	ips := []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")}
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if ipNet, ok := a.(*net.IPNet); ok && ipNet.IP != nil && !ipNet.IP.IsLoopback() {
				ips = append(ips, ipNet.IP)
			}
		}
	}

	tmpl := x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			Organization: []string{"Velocor Storage Appliance"},
			CommonName:   "velocor.local",
		},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(3650 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              dnsNames,
		IPAddresses:           ips,
	}
	derBytes, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		return nil, err
	}
	keyBytes, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return nil, err
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: derBytes})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes})

	if err := os.MkdirAll(filepath.Dir(certPath), 0o755); err == nil {
		_ = os.WriteFile(certPath, certPEM, 0o644)
		_ = os.WriteFile(keyPath, keyPEM, 0o600)
	}

	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{cert},
	}, nil
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, auth.ErrUnauthorized):
		status = http.StatusUnauthorized
	case errors.Is(err, auth.ErrForbidden):
		status = http.StatusForbidden
	case errors.Is(err, store.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, store.ErrAlreadyExists):
		status = http.StatusConflict
	case errors.Is(err, store.ErrInUse), errors.Is(err, store.ErrAttachmentConflict), errors.Is(err, store.ErrBootDeviceProtected):
		status = http.StatusConflict
	case errors.Is(err, store.ErrInvalidArgument):
		status = http.StatusBadRequest
	}
	writeJSON(w, status, map[string]any{
		"error": err.Error(),
	})
}

func newResourceID(prefix string) string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return prefix + "-" + hex.EncodeToString(b[:])
}
