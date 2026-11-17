package server

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed web
var webAssetsFS embed.FS

func (s *Server) handleDashboard(w http.ResponseWriter, _ *http.Request) {
	data, err := webAssetsFS.ReadFile("web/index.html")
	if err != nil {
		http.Error(w, "dashboard not found", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(data)
}

func (s *Server) handleWebAssets() http.Handler {
	sub, err := fs.Sub(webAssetsFS, "web")
	if err != nil {
		return http.NotFoundHandler()
	}
	return http.StripPrefix("/assets/", http.FileServer(http.FS(sub)))
}

const embeddedOpenAPIJSON = `{
  "openapi": "3.0.3",
  "info": {
    "title": "Velocor Control Plane API",
    "version": "v1",
    "description": "Unified REST/OpenAPI interface for Velocor SPDK SAN/NAS storage appliances."
  },
  "paths": {
    "/api/v1/auth/login": { "post": { "summary": "Authenticate admin session and issue JWT" } },
    "/api/v1/auth/password": { "post": { "summary": "Change administrator password" } },
    "/api/v1/auth/tokens": { "get": { "summary": "List API tokens" }, "post": { "summary": "Create scoped API token (velo_pat_...)" } },
    "/api/v1/disks": { "get": { "summary": "List physical NVMe and SSD disks" } },
    "/api/v1/disks:scan": { "post": { "summary": "Rescan PCIe and block devices" } },
    "/api/v1/disks/{id}": { "get": { "summary": "Get disk (incl. write_cache, durability)" }, "patch": { "summary": "Declare power_loss_protected / set write_cache_policy (DISABLED keeps the drive's volatile write cache off)" } },
    "/api/v1/pools": { "get": { "summary": "List SPDK RAID + lvol_store pools" }, "post": { "summary": "Create storage pool" } },
    "/api/v1/volumes": { "get": { "summary": "List logical volumes" }, "post": { "summary": "Create logical volume (idempotent)" } },
    "/api/v1/snapshots": { "get": { "summary": "List snapshots" } },
    "/api/v1/portals": { "get": { "summary": "List SAN portals" }, "post": { "summary": "Create SAN portal" } },
    "/api/v1/initiator-groups": { "get": { "summary": "List initiator ACL groups" }, "post": { "summary": "Create initiator ACL group" } },
    "/api/v1/exports": { "get": { "summary": "List NVMe-oF and iSCSI exports" }, "post": { "summary": "Create SAN export (idempotent)" } },
    "/api/v1/exports/{id}": { "get": { "summary": "Get export" }, "patch": { "summary": "Change initiator groups / allow_any_host on a live export, or enable / disable it (enabled)" }, "delete": { "summary": "Delete export" } },
    "/api/v1/exports/{id}/connections": { "get": { "summary": "List hosts connected to an export" } },
    "/api/v1/initiator-groups/{id}": { "patch": { "summary": "Change group members; re-applied to every export using it" } },
    "/api/v1/volumes/{id}/usage": { "get": { "summary": "Live allocated vs provisioned bytes of a volume" } },
    "/api/v1/pool-imports": { "get": { "summary": "Pools found on unclaimed disks (from their RAID superblocks)" }, "post": { "summary": "Import a pool {pool_id, name?, allow_degraded?} from the configuration copy on it, else from SPDK" } },
    "/api/v1/system/backup": { "get": { "summary": "Download the database (admin; ?name= a local backup); it holds password hashes and secrets" } },
    "/api/v1/system/backups": { "get": { "summary": "List local database backups" }, "post": { "summary": "Take a database backup now" } },
    "/api/v1/system/restore": { "post": { "summary": "Restore the database from the request body (or ?name= a local backup); the current one is backed up first and velod restarts" } },
    "/api/v1/system/status": { "get": { "summary": "Get node, SPDK, reconciler status and configuration warnings" } },
    "/api/v1/system/reconcile": { "post": { "summary": "Trigger immediate state reconciliation" } },
    "/api/v1/system/iostats": { "get": { "summary": "Query real-time SPDK bdev I/O statistics" } },
    "/api/v1/system/containers": { "get": { "summary": "List the appliance's containers (read-only; they change with the OS image)" } }
  }
}`
