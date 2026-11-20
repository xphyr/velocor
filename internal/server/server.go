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
	"sync"
	"time"

	"github.com/velocor/velocor/internal/auth"
	"github.com/velocor/velocor/internal/backup"
	"github.com/velocor/velocor/internal/dataplane"
	"github.com/velocor/velocor/internal/hardware"
	"github.com/velocor/velocor/internal/meta"
	"github.com/velocor/velocor/internal/monitoring"
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
	space      spaceReservations
	startedAt  time.Time
	mux        *http.ServeMux

	loginLimiter  *loginLimiter
	metricsPublic bool
	insecureHTTP  bool
	bootstrapFile string
	backups       *backup.Manager
	restart       func()
	metaDev       meta.Device
	sbReader      func(*store.Disk) (*hardware.RaidSuperblock, error) // tests
	monitoring    *monitoring.Manager
	mon           monitoringState
	net           *netChanges
	os            osLifecycle
	// scrubMu serializes the scrub scheduler and the scrub API (TODO.md P2-2).
	scrubMu  sync.Mutex
	netQuery func(context.Context) NetworkStatus // tests
}

// WithInsecureHTTP records that the API is served over plain HTTP, so status can warn about it.
func WithInsecureHTTP(insecure bool) Option {
	return func(s *Server) { s.insecureHTTP = insecure }
}

// WithBootstrapPasswordFile names the file the generated bootstrap password was written to:
// velod deletes it once the admin password has been changed, and status warns while it exists.
func WithBootstrapPasswordFile(path string) Option {
	return func(s *Server) { s.bootstrapFile = path }
}

// removeStaleBootstrapFile deletes the bootstrap password file once the password in it no
// longer opens the admin account (it was changed, also before velod learnt to do this).
func (s *Server) removeStaleBootstrapFile() {
	if s.bootstrapFile == "" || s.store == nil {
		return
	}
	data, err := os.ReadFile(s.bootstrapFile)
	if err != nil {
		return // gone already, or unreadable: status still warns while it exists
	}
	admin, err := s.store.GetAdminUser(auth.DefaultAdminUsername)
	if err != nil || auth.VerifyPassword(strings.TrimSpace(string(data)), admin.PasswordHash) {
		return
	}
	if err := os.Remove(s.bootstrapFile); err != nil {
		s.logger.Warn("cannot delete the initial admin password file", "path", s.bootstrapFile, "err", err)
		return
	}
	s.logger.Info("deleted the initial admin password file: the password has been changed", "path", s.bootstrapFile)
}

// warnings lists configuration problems an operator should fix, for every UI screen.
func (s *Server) warnings() []string {
	out := []string{}
	if s.bootstrapFile != "" {
		if _, err := os.Stat(s.bootstrapFile); err == nil {
			out = append(out, "The initial admin password file "+s.bootstrapFile+" still exists. It is deleted when the admin password is changed.")
		}
	}
	if s.insecureHTTP {
		out = append(out, "The API is served over plain HTTP, so passwords and tokens cross the network unencrypted.")
	}
	if s.metricsPublic {
		out = append(out, "/metrics is served without authentication.")
	}
	out = append(out, s.durabilityWarnings()...)
	if s.reconciler != nil {
		if sum := reconciler.UnmanagedSummary(s.reconciler.State().Unmanaged); sum != "" {
			out = append(out, "spdk_tgt holds objects that Velocor doesn't manage ("+sum+"), listed on the System page. "+
				"They are left alone: they may be leftovers of a failed operation, or the only copy of data after the database was lost. "+
				"Import the pool or restore a database backup if they are yours; remove them by hand only once you know what they are.")
		}
	}
	if s.hw != nil && s.hw.UnsafeNoIOMMUAllowed() {
		out = append(out, "Unsafe no-IOMMU mode is allowed: SPDK can DMA anywhere in host memory. Use this only in lab VMs.")
	}
	if w := s.monitoringWarning(); w != "" {
		out = append(out, w)
	}
	if w := s.osWarning(); w != "" {
		out = append(out, w)
	}
	return out
}

// Option customises a Server.
type Option func(*Server)

// WithPublicMetrics serves /metrics without authentication (for scrapers that cannot send
// a bearer token). By default /metrics requires a READONLY token.
func WithPublicMetrics(public bool) Option {
	return func(s *Server) { s.metricsPublic = public }
}

// New constructs a new control-plane Server and registers all routes.
func New(
	st store.Store,
	spdkClient *spdk.Client,
	hwMgr *hardware.Manager,
	authMgr *auth.Manager,
	rec *reconciler.Reconciler,
	logger *slog.Logger,
	opts ...Option,
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

		loginLimiter: newLoginLimiter(),
		net:          &netChanges{backend: nmHost{}},
	}
	for _, o := range opts {
		o(s)
	}
	if m := s.monitoring; m != nil {
		if m.NewToken == nil {
			m.NewToken = s.rotateScrapeToken
		}
		if m.ReactorCPUs == nil {
			m.ReactorCPUs = s.reactorCPUs
		}
		if m.Run == nil {
			m.Run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
				return hostCommand(ctx, name, args...).CombinedOutput()
			}
		}
		if m.Logger == nil {
			m.Logger = s.logger
		}
	}
	s.removeStaleBootstrapFile()
	s.registerRoutes()
	return s
}

// maxRequestBodyBytes bounds every request body; no API payload legitimately exceeds it.
const maxRequestBodyBytes = 1 << 20

// Handler returns the root http.Handler for the server.
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		limit := int64(maxRequestBodyBytes)
		if r.URL.Path == restorePath {
			limit = maxRestoreBytes
		}
		r.Body = http.MaxBytesReader(w, r.Body, limit)
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			// A client that disconnects or times out must not abort a change halfway: SPDK
			// finishes what it was asked to do regardless, and a rollback based on a cancelled
			// call acts on a state it no longer knows. SPDK calls carry their own timeouts.
			r = r.WithContext(context.WithoutCancel(r.Context()))
		}
		s.mux.ServeHTTP(w, r)
	})
}

func (s *Server) registerRoutes() {
	s.mux.HandleFunc("GET /healthz", s.handleHealthz)
	s.mux.HandleFunc("GET /readyz", s.handleReadyz)
	// Public routes (UI, OpenAPI spec, login, Prometheus scrape)
	s.mux.HandleFunc("GET /{$}", s.handleDashboard)
	s.mux.Handle("GET /assets/", s.handleWebAssets())
	s.mux.HandleFunc("GET /api/v1/openapi.json", s.handleOpenAPI)
	if s.metricsPublic {
		s.mux.HandleFunc("GET /metrics", s.handleMetrics)
	} else {
		s.mux.HandleFunc("GET /metrics", s.withAuth(store.RoleReadOnly, s.handleMetrics))
	}
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
	s.mux.HandleFunc("PATCH /api/v1/disks/{id}", s.withAuth(store.RoleAdmin, s.handleUpdateDisk))
	s.mux.HandleFunc("GET /api/v1/disks/{id}", s.withAuth(store.RoleReadOnly, s.handleGetDisk))

	// Pools
	s.mux.HandleFunc("GET /api/v1/pools", s.withAuth(store.RoleReadOnly, s.handleListPools))
	s.mux.HandleFunc("POST /api/v1/pools", s.withAuth(store.RoleAdmin, s.handleCreatePool))
	s.mux.HandleFunc("GET /api/v1/pools/{id}", s.withAuth(store.RoleReadOnly, s.handleGetPool))
	s.mux.HandleFunc("DELETE /api/v1/pools/{id}", s.withAuth(store.RoleAdmin, s.handleDeletePool))
	s.mux.HandleFunc("PATCH /api/v1/pools/{id}", s.withAuth(store.RoleAdmin, s.handleUpdatePool))
	s.mux.HandleFunc("POST /api/v1/pools/{id}/replace", s.withAuth(store.RoleAdmin, s.handleReplacePoolDisk))
	s.mux.HandleFunc("POST /api/v1/pools/{id}/start-degraded", s.withAuth(store.RoleAdmin, s.handleStartPoolDegraded))
	s.mux.HandleFunc("GET /api/v1/pools/{id}/scrub", s.withAuth(store.RoleReadOnly, s.handleGetPoolScrub))
	s.mux.HandleFunc("POST /api/v1/pools/{id}/scrub", s.withAuth(store.RoleAdmin, s.handleStartPoolScrub))
	s.mux.HandleFunc("DELETE /api/v1/pools/{id}/scrub", s.withAuth(store.RoleAdmin, s.handleStopPoolScrub))
	s.mux.HandleFunc("DELETE /api/v1/pools/{id}/disks/{disk_id}", s.withAuth(store.RoleAdmin, s.handleReleasePoolDisk))

	// Volumes & Snapshots (CSI_PROVISIONER / READONLY)
	s.mux.HandleFunc("GET /api/v1/volumes", s.withAuth(store.RoleReadOnly, s.handleListVolumes))
	s.mux.HandleFunc("POST /api/v1/volumes", s.withAuth(store.RoleCSIProvisioner, s.handleCreateVolume))
	s.mux.HandleFunc("GET /api/v1/volumes/{id}", s.withAuth(store.RoleReadOnly, s.handleGetVolume))
	s.mux.HandleFunc("PUT /api/v1/volumes/{id}/qos", s.withAuth(store.RoleCSIProvisioner, s.handleSetVolumeQoS))
	s.mux.HandleFunc("POST /api/v1/volumes/{volume_id}/snapshots", s.withAuth(store.RoleCSIProvisioner, s.handleCreateSnapshot))
	s.mux.HandleFunc("POST /api/v1/volumes/", s.withAuth(store.RoleCSIProvisioner, s.handleVolumeAction))
	s.mux.HandleFunc("DELETE /api/v1/volumes/{id}", s.withAuth(store.RoleCSIProvisioner, s.handleDeleteVolume))
	s.mux.HandleFunc("PUT /api/v1/volumes/{id}/protection", s.withAuth(store.RoleAdmin, s.handleSetVolumeProtection))
	s.mux.HandleFunc("GET /api/v1/deleted-volumes", s.withAuth(store.RoleReadOnly, s.handleListDeletedVolumes))
	s.mux.HandleFunc("POST /api/v1/deleted-volumes/{id}/restore", s.withAuth(store.RoleAdmin, s.handleRestoreVolume))
	s.mux.HandleFunc("DELETE /api/v1/deleted-volumes/{id}", s.withAuth(store.RoleAdmin, s.handlePurgeVolume))
	s.mux.HandleFunc("GET /api/v1/system/monitoring", s.withAuth(store.RoleReadOnly, s.handleGetMonitoring))
	s.mux.HandleFunc("GET /api/v1/monitoring/alerts", s.withAuth(store.RoleReadOnly, s.handleListAlerts))
	s.mux.HandleFunc("POST /api/v1/monitoring/silences", s.withAuth(store.RoleAdmin, s.handleCreateSilence))
	s.mux.HandleFunc("DELETE /api/v1/monitoring/silences/{id}", s.withAuth(store.RoleAdmin, s.handleExpireSilence))
	s.mux.HandleFunc("GET /api/v1/monitoring/graphs/{name}", s.withAuth(store.RoleReadOnly, s.handleGraph))
	s.mux.HandleFunc("GET /api/v1/monitoring/settings", s.withAuth(store.RoleAdmin, s.handleGetAlerting))
	s.mux.HandleFunc("PUT /api/v1/monitoring/settings", s.withAuth(store.RoleAdmin, s.handlePutAlerting))
	s.mux.HandleFunc("GET /api/v1/monitoring/rules", s.withAuth(store.RoleReadOnly, s.handleListRules))
	s.mux.HandleFunc("POST /api/v1/monitoring/test", s.withAuth(store.RoleAdmin, s.handleTestAlert))
	s.mux.HandleFunc("PUT /api/v1/system/monitoring", s.withAuth(store.RoleAdmin, s.handlePutMonitoring))
	s.mux.HandleFunc("GET /api/v1/system/volume-retention", s.withAuth(store.RoleReadOnly, s.handleGetVolumeRetention))
	s.mux.HandleFunc("PUT /api/v1/system/volume-retention", s.withAuth(store.RoleAdmin, s.handlePutVolumeRetention))

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
	s.mux.HandleFunc("PATCH /api/v1/exports/{id}", s.withAuth(store.RoleCSIProvisioner, s.handleUpdateExport))
	s.mux.HandleFunc("GET /api/v1/exports/{id}/connections", s.withAuth(store.RoleReadOnly, s.handleExportConnections))
	s.mux.HandleFunc("PATCH /api/v1/initiator-groups/{id}", s.withAuth(store.RoleCSIProvisioner, s.handleUpdateInitiatorGroup))
	s.mux.HandleFunc("GET /api/v1/volumes/{id}/usage", s.withAuth(store.RoleReadOnly, s.handleVolumeUsage))

	// System
	s.mux.HandleFunc("GET /api/v1/system/status", s.withAuth(store.RoleReadOnly, s.handleSystemStatus))
	s.mux.HandleFunc("POST /api/v1/system/reconcile", s.withAuth(store.RoleAdmin, s.handleTriggerReconcile))
	s.mux.HandleFunc("GET /api/v1/system/rebuild", s.withAuth(store.RoleReadOnly, s.handleGetRebuildSettings))
	s.mux.HandleFunc("PUT /api/v1/system/rebuild", s.withAuth(store.RoleAdmin, s.handlePutRebuildSettings))
	s.mux.HandleFunc("GET /api/v1/system/scrub", s.withAuth(store.RoleReadOnly, s.handleGetScrubSettings))
	s.mux.HandleFunc("PUT /api/v1/system/scrub", s.withAuth(store.RoleAdmin, s.handlePutScrubSettings))
	s.mux.HandleFunc("GET /api/v1/system/iostats", s.withAuth(store.RoleReadOnly, s.handleGetIoStats))
	s.mux.HandleFunc("GET /api/v1/system/os", s.withAuth(store.RoleReadOnly, s.handleGetOSStatus))
	s.mux.HandleFunc("GET /api/v1/pool-imports", s.withAuth(store.RoleAdmin, s.handleListImportCandidates))
	s.mux.HandleFunc("POST /api/v1/pool-imports", s.withAuth(store.RoleAdmin, s.handleImportPool))
	s.mux.HandleFunc("GET /api/v1/system/backup", s.withAuth(store.RoleAdmin, s.handleDownloadBackup))
	s.mux.HandleFunc("GET /api/v1/system/backups", s.withAuth(store.RoleAdmin, s.handleListBackups))
	s.mux.HandleFunc("POST /api/v1/system/backups", s.withAuth(store.RoleAdmin, s.handleTakeBackup))
	s.mux.HandleFunc("POST "+restorePath, s.withAuth(store.RoleAdmin, s.handleRestore))
	s.mux.HandleFunc("POST /api/v1/system/upgrade", s.withAuth(store.RoleAdmin, s.handleStageUpgrade))
	s.mux.HandleFunc("GET /api/v1/system/upgrade", s.withAuth(store.RoleReadOnly, s.handleGetUpgrade))
	s.mux.HandleFunc("DELETE /api/v1/system/upgrade/warning", s.withAuth(store.RoleAdmin, s.handleDismissUpgradeWarning))
	s.mux.HandleFunc("POST /api/v1/system/rollback", s.withAuth(store.RoleAdmin, s.handleRollbackUpgrade))
	s.mux.HandleFunc("POST /api/v1/system/reboot", s.withAuth(store.RoleAdmin, s.handleRebootSystem))
	s.mux.HandleFunc("GET /api/v1/system/containers", s.withAuth(store.RoleReadOnly, s.handleListContainers))
	s.mux.HandleFunc("GET /api/v1/system/network", s.withAuth(store.RoleReadOnly, s.handleGetNetwork))
	s.mux.HandleFunc("PUT /api/v1/system/network/interfaces/{name}", s.withAuth(store.RoleAdmin, s.handleUpdateNetworkInterface))
	s.mux.HandleFunc("POST /api/v1/system/network/confirm", s.withAuth(store.RoleAdmin, s.handleConfirmNetworkChange))
	s.mux.HandleFunc("POST /api/v1/system/network/rollback", s.withAuth(store.RoleAdmin, s.handleRollbackNetworkChange))
}

type contextKey string

const (
	subjectContextKey    contextKey = "subject"
	roleContextKey       contextKey = "role"
	mustChangeContextKey contextKey = "must_change_password"
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
		p, err := s.auth.Authenticate(rawToken)
		if err != nil {
			writeError(w, err)
			return
		}
		if p.MustChangePassword && r.URL.Path != "/api/v1/auth/password" && r.URL.Path != "/api/v1/auth/me" {
			writeError(w, auth.ErrPasswordChangeRequired)
			return
		}
		if err := auth.Authorize(p.Role, requiredRole); err != nil {
			writeError(w, err)
			return
		}
		ctx := context.WithValue(r.Context(), subjectContextKey, p.Subject)
		ctx = context.WithValue(ctx, roleContextKey, p.Role)
		ctx = context.WithValue(ctx, mustChangeContextKey, p.MustChangePassword)
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
	ip := clientIP(r)
	if wait := s.loginLimiter.blocked(ip, req.Username); wait > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "too many failed login attempts; try again later"})
		return
	}
	sess, err := s.auth.LoginSession(req.Username, req.Password)
	if err != nil {
		s.loginLimiter.fail(ip, req.Username)
		s.logger.Warn("failed login", "username", req.Username, "remote", ip)
		writeError(w, err)
		return
	}
	s.loginLimiter.succeed(ip, req.Username)
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token":         sess.Token,
		"expires_at":           sess.ExpiresAt,
		"username":             req.Username,
		"role":                 sess.Role,
		"must_change_password": sess.MustChangePassword,
	})
}

func (s *Server) handleAuthMe(w http.ResponseWriter, r *http.Request) {
	mustChange, _ := r.Context().Value(mustChangeContextKey).(bool)
	writeJSON(w, http.StatusOK, map[string]any{
		"username":             subjectFromContext(r.Context()),
		"role":                 roleFromContext(r.Context()),
		"must_change_password": mustChange,
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
	sess, err := s.auth.ChangePassword(sub, req.CurrentPassword, req.NewPassword)
	if err != nil {
		writeError(w, err)
		return
	}
	s.removeStaleBootstrapFile()
	// All earlier sessions (including the caller's) were revoked; hand back a fresh one.
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token": sess.Token,
		"expires_at":   sess.ExpiresAt,
	})
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

// --- Volume & Snapshot Handlers ---

func (s *Server) handleListVolumes(w http.ResponseWriter, r *http.Request) {
	poolID := r.URL.Query().Get("pool_id")
	all, err := s.store.ListVolumes(poolID)
	if err != nil {
		writeError(w, err)
		return
	}
	vols := make([]*store.Volume, 0, len(all))
	for _, v := range all {
		if !v.Deleted() {
			vols = append(vols, v)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"volumes": vols})
}

func (s *Server) handleGetVolume(w http.ResponseWriter, r *http.Request) {
	vol, err := s.liveVolume(r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"volume": vol})
}

func (s *Server) handleListSnapshots(w http.ResponseWriter, r *http.Request) {
	poolID := r.URL.Query().Get("pool_id")
	volID := r.URL.Query().Get("volume_id")
	all, err := s.store.ListSnapshots(poolID, volID)
	if err != nil {
		writeError(w, err)
		return
	}
	snaps := make([]*store.Snapshot, 0, len(all))
	for _, sn := range all {
		if !sn.Deleted() {
			snaps = append(snaps, sn)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"snapshots": snaps})
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
			"length_bytes": e.SizeBytes,
		})
	}
	return out
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

func (s *Server) resolveGroupSnapshots(snapIDs []string) []*store.Snapshot {
	snaps := make([]*store.Snapshot, 0, len(snapIDs))
	for _, snapID := range snapIDs {
		if snap, err := s.liveSnapshot(snapID); err == nil {
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
	if nqn := strings.TrimSpace(req.HostNQN); nqn != "" {
		if err := dataplane.ValidateNQN(nqn); err != nil {
			writeError(w, fmt.Errorf("%w: %v", store.ErrInvalidArgument, err))
			return
		}
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
	for _, nqn := range req.HostNQNs {
		if err := dataplane.ValidateNQN(strings.TrimSpace(nqn)); err != nil {
			writeError(w, fmt.Errorf("%w: %v", store.ErrInvalidArgument, err))
			return
		}
	}
	if strings.TrimSpace(req.CHAPUser) != "" {
		// Accepting CHAP settings without enforcing them would be a false sense of security.
		writeError(w, fmt.Errorf("%w: CHAP authentication is not implemented yet", store.ErrInvalidArgument))
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
	restoring := false
	if s.reconciler != nil {
		recState = s.reconciler.State()
		restoring = spdkConnected && s.reconciler.Restoring()
	}

	overall := store.HealthStatusOnline
	switch {
	case !spdkConnected || len(recState.DegradedResources) > 0:
		overall = store.HealthStatusDegraded
	case restoring:
		// spdk_tgt answers, but it was (re)started after the last completed pass: pools,
		// volumes and exports aren't back yet (TODO.md P2-11).
		overall = store.HealthStatusReconciling
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
			"restoring":          restoring,
			"socket_path":        socketPath,
			"hugepages_total_mb": hpTotalMB,
			"hugepages_free_mb":  hpFreeMB,
		},
		"reconciler": recState,
		"warnings":   s.warnings(),
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
	// Encode first: a value JSON can't carry (NaN, say) must not turn into a 200 with an
	// empty body.
	b, err := json.Marshal(payload)
	if err != nil {
		slog.Error("encoding a response failed", "err", err)
		status = http.StatusInternalServerError
		b, _ = json.Marshal(map[string]any{"error": "encoding the response failed: " + err.Error()})
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(append(b, '\n'))
}

func writeError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, auth.ErrUnauthorized):
		status = http.StatusUnauthorized
	case errors.Is(err, auth.ErrForbidden), errors.Is(err, auth.ErrPasswordChangeRequired):
		status = http.StatusForbidden
	case errors.Is(err, store.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, store.ErrAlreadyExists):
		status = http.StatusConflict
	case errors.Is(err, store.ErrInUse), errors.Is(err, store.ErrAttachmentConflict), errors.Is(err, store.ErrBootDeviceProtected):
		status = http.StatusConflict
	case errors.Is(err, store.ErrInvalidArgument):
		status = http.StatusBadRequest
	case errors.Is(err, store.ErrInsufficientCapacity):
		status = http.StatusInsufficientStorage
	case errors.Is(err, errDataPlaneUnavailable):
		status = http.StatusServiceUnavailable
	case errors.Is(err, spdk.ErrCBTUnsupported):
		status = http.StatusNotImplemented
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

func spaceStateValue(st store.SpaceState) int {
	switch st {
	case store.SpaceStateWarning:
		return 1
	case store.SpaceStateCritical:
		return 2
	case store.SpaceStateFull:
		return 3
	}
	return 0
}
