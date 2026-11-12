package server

const embeddedDashboardHTML = `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8" />
  <meta name="viewport" content="width=device-width, initial-scale=1" />
  <title>Velocor All-Flash SAN Appliance</title>
  <style>
    :root {
      --bg: #0b0f19;
      --panel: #131b2e;
      --border: #23304d;
      --accent: #38bdf8;
      --text: #e2e8f0;
      --muted: #94a3b8;
      --ok: #22c55e;
      --warn: #f59e0b;
    }
    * { box-sizing: border-box; }
    body {
      margin: 0;
      font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, monospace;
      background: var(--bg);
      color: var(--text);
      padding: 2rem;
    }
    header {
      display: flex;
      justify-content: space-between;
      align-items: center;
      border-bottom: 1px solid var(--border);
      padding-bottom: 1rem;
      margin-bottom: 1.5rem;
    }
    h1 { margin: 0; font-size: 1.4rem; color: var(--accent); }
    .badge {
      padding: 0.25rem 0.65rem;
      border-radius: 999px;
      font-size: 0.78rem;
      font-weight: 600;
      background: rgba(34, 197, 94, 0.15);
      color: var(--ok);
      border: 1px solid rgba(34, 197, 94, 0.4);
    }
    .grid {
      display: grid;
      grid-template-columns: repeat(auto-fit, minmax(280px, 1fr));
      gap: 1rem;
    }
    .card {
      background: var(--panel);
      border: 1px solid var(--border);
      border-radius: 8px;
      padding: 1.1rem;
    }
    .card h2 {
      margin: 0 0 0.65rem 0;
      font-size: 0.9rem;
      text-transform: uppercase;
      letter-spacing: 0.05em;
      color: var(--muted);
    }
    code {
      color: var(--accent);
      font-size: 0.85rem;
    }
  </style>
</head>
<body>
  <header>
    <div>
      <h1>VELOCOR // SPDK All-Flash Appliance</h1>
      <small style="color: var(--muted)">Control Plane API &amp; Telemetry Endpoint</small>
    </div>
    <span class="badge">API v1 READY</span>
  </header>
  <div class="grid">
    <div class="card">
      <h2>REST &amp; OpenAPI</h2>
      <p>Base Path: <code>/api/v1</code></p>
      <p>OpenAPI Spec: <a href="/api/v1/openapi.json" style="color:var(--accent)">/api/v1/openapi.json</a></p>
    </div>
    <div class="card">
      <h2>Prometheus Telemetry</h2>
      <p>Scrape Endpoint: <a href="/metrics" style="color:var(--accent)">/metrics</a></p>
      <p>Includes SPDK bdev IOPS/latency, pool cluster usage, and NVMe SMART health.</p>
    </div>
    <div class="card">
      <h2>CLI Management</h2>
      <p>Authenticate with <code>veloctl login</code> or pass a scoped <code>velo_pat_...</code> service token.</p>
    </div>
  </div>
</body>
</html>
`

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
    "/api/v1/pools": { "get": { "summary": "List SPDK RAID + lvol_store pools" }, "post": { "summary": "Create storage pool" } },
    "/api/v1/volumes": { "get": { "summary": "List logical volumes" }, "post": { "summary": "Create logical volume (idempotent)" } },
    "/api/v1/snapshots": { "get": { "summary": "List snapshots" } },
    "/api/v1/portals": { "get": { "summary": "List SAN portals" }, "post": { "summary": "Create SAN portal" } },
    "/api/v1/initiator-groups": { "get": { "summary": "List initiator ACL groups" }, "post": { "summary": "Create initiator ACL group" } },
    "/api/v1/exports": { "get": { "summary": "List NVMe-oF and iSCSI exports" }, "post": { "summary": "Create SAN export (idempotent)" } },
    "/api/v1/system/status": { "get": { "summary": "Get node, SPDK, and reconciler status" } },
    "/api/v1/system/reconcile": { "post": { "summary": "Trigger immediate state reconciliation" } },
    "/api/v1/system/iostats": { "get": { "summary": "Query real-time SPDK bdev I/O statistics" } }
  }
}`
