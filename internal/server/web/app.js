(() => {
  const TOKEN_KEY = "velocor_jwt";
  const state = {
    tab: "overview",
    system: null,
    disks: [],
    pools: [],
    volumes: [],
    snapshots: [],
    groupSnapshots: [],
    portals: [],
    hosts: [],
    initiatorGroups: [],
    exports: [],
    users: [],
    tokens: [],
    me: { username: "", role: "" },
    iostats: { tick_rate: 0, stats: [] },
    osStatus: null,
    containerUpdates: null,
    network: null,
    prevIostat: null,
    prevIostatAt: 0,
    pollTimer: null,
  };

  const $ = (id) => document.getElementById(id);

  function escapeHtml(val) {
    return String(val ?? "")
      .replace(/&/g, "&amp;")
      .replace(/</g, "&lt;")
      .replace(/>/g, "&gt;")
      .replace(/"/g, "&quot;")
      .replace(/'/g, "&#39;");
  }

  function toast(msg, kind = "ok") {
    const el = document.createElement("div");
    el.className = `toast ${kind}`;
    el.textContent = msg;
    $("toasts").appendChild(el);
    setTimeout(() => el.remove(), 4200);
  }

  function getToken() {
    return sessionStorage.getItem(TOKEN_KEY) || "";
  }

  function setToken(tok) {
    if (tok) {
      sessionStorage.setItem(TOKEN_KEY, tok);
    } else {
      sessionStorage.removeItem(TOKEN_KEY);
    }
  }

  async function api(method, path, body) {
    const headers = { "Content-Type": "application/json" };
    const tok = getToken();
    if (tok) headers["Authorization"] = `Bearer ${tok}`;
    const res = await fetch(path, {
      method,
      headers,
      body: body !== undefined ? JSON.stringify(body) : undefined,
    });
    if (res.status === 401) {
      setToken("");
      showLogin();
      throw new Error("Session expired, please sign in");
    }
    const text = await res.text();
    let data = {};
    try { data = text ? JSON.parse(text) : {}; } catch (_) {}
    if (!res.ok) {
      throw new Error(data.error || data.message || `HTTP ${res.status}`);
    }
    return data;
  }

  function fmtBytes(bytes) {
    const n = Number(bytes || 0);
    if (n === 0) return "0 B";
    const units = ["B", "KiB", "MiB", "GiB", "TiB", "PiB"];
    const i = Math.min(Math.floor(Math.log(n) / Math.log(1024)), units.length - 1);
    return `${(n / Math.pow(1024, i)).toFixed(i === 0 ? 0 : 2)} ${units[i]}`;
  }

  function healthBadge(status) {
    const s = String(status || "UNSPECIFIED").toUpperCase();
    const cls = s === "ONLINE" ? "badge-ok" : s === "DEGRADED" ? "badge-warn" : s === "FAULTED" || s === "OFFLINE" ? "badge-err" : "badge-muted";
    return `<span class="badge ${cls}">${escapeHtml(s)}</span>`;
  }

  function showLogin() {
    $("login-overlay").classList.remove("hidden");
  }

  function hideLogin() {
    $("login-overlay").classList.add("hidden");
    $("login-error").classList.add("hidden");
  }

  function openModal(title, html, onBind) {
    $("modal-title").textContent = title;
    $("modal-body").innerHTML = html;
    $("modal-overlay").classList.remove("hidden");
    if (onBind) onBind();
  }

  function closeModal() {
    $("modal-overlay").classList.add("hidden");
    $("modal-body").innerHTML = "";
  }

  function isAdmin() {
    return state.me && state.me.role === "ADMIN";
  }

  async function refreshAll() {
    if (!getToken()) {
      showLogin();
      return;
    }
    try {
      const [me, sys, disks, pools, vols, snaps, gsnaps, portals, hosts, igs, exps, iostats, osStatus, containerUpdates, network] = await Promise.all([
        api("GET", "/api/v1/auth/me").catch(() => ({ username: "", role: "" })),
        api("GET", "/api/v1/system/status"),
        api("GET", "/api/v1/disks"),
        api("GET", "/api/v1/pools"),
        api("GET", "/api/v1/volumes"),
        api("GET", "/api/v1/snapshots"),
        api("GET", "/api/v1/volume-group-snapshots").catch(() => ({ volume_group_snapshots: [] })),
        api("GET", "/api/v1/portals"),
        api("GET", "/api/v1/hosts"),
        api("GET", "/api/v1/initiator-groups"),
        api("GET", "/api/v1/exports"),
        api("GET", "/api/v1/system/iostats").catch(() => ({ tick_rate: 0, stats: [] })),
        api("GET", "/api/v1/system/os").catch(() => null),
        api("GET", "/api/v1/system/containers/updates").catch(() => null),
        api("GET", "/api/v1/system/network").catch(() => null),
      ]);
      state.me = me || { username: "", role: "" };
      state.system = sys;
      state.disks = disks.disks || [];
      state.pools = pools.pools || [];
      state.volumes = vols.volumes || [];
      state.snapshots = snaps.snapshots || [];
      state.groupSnapshots = gsnaps.volume_group_snapshots || [];
      state.portals = portals.portals || [];
      state.hosts = hosts.hosts || [];
      state.initiatorGroups = igs.initiator_groups || [];
      state.exports = exps.exports || [];
      state.osStatus = osStatus;
      state.containerUpdates = containerUpdates;
      state.network = network;
      updateIostats(iostats);

      if (state.tab === "access" && isAdmin()) {
        const [usersRes, tokRes] = await Promise.all([
          api("GET", "/api/v1/auth/users").catch(() => ({ users: [] })),
          api("GET", "/api/v1/auth/tokens").catch(() => ({ tokens: [] })),
        ]);
        state.users = usersRes.users || [];
        state.tokens = tokRes.tokens || [];
      } else if (state.tab === "access") {
        state.users = [];
        state.tokens = [];
      }
      renderAll();
    } catch (err) {
      if (getToken()) toast(err.message, "err");
    }
  }

  function updateIostats(next) {
    const now = performance.now();
    if (state.iostats && state.prevIostatAt > 0) {
      const prevMap = {};
      for (const b of state.iostats.stats || []) prevMap[b.name] = b;
      state.prevIostat = { map: prevMap, dt: Math.max((now - state.prevIostatAt) / 1000, 0.2) };
    }
    state.iostats = next || { tick_rate: 0, stats: [] };
    state.prevIostatAt = now;
  }

  function renderAll() {
    renderTopbar();
    renderOverview();
    renderLifecycle();
    renderDisks();
    renderPools();
    renderVolumes();
    renderSnapshots();
    renderGroupSnapshots();
    renderExports();
    renderPortals();
    renderHosts();
    renderInitiatorGroups();
    renderNetwork();
    if (state.tab === "access") {
      const admin = isAdmin();
      $("users-card").classList.toggle("hidden", !admin);
      $("tokens-card").classList.toggle("hidden", !admin);
      if (admin) {
        renderUsers();
        renderTokens();
      }
    }
  }

  function renderTopbar() {
    if (state.me && state.me.username) {
      $("user-badge").textContent = `${state.me.username} (${state.me.role || "READONLY"})`;
      $("user-badge").className = `badge ${state.me.role === "ADMIN" ? "badge-info" : "badge-muted"}`;
    } else {
      $("user-badge").className = "badge badge-muted hidden";
    }
    const s = state.system;
    if (!s) return;
    const n = s.node || {};
    const sp = s.spdk || {};
    $("badge-node").textContent = `${n.hostname || "node"} (${n.version || ""})`;
    $("badge-node").className = "badge badge-info";

    $("badge-spdk").textContent = sp.connected ? `SPDK ${n.spdk_version || "connected"}` : "SPDK: disconnected";
    $("badge-spdk").className = `badge ${sp.connected ? "badge-ok" : "badge-err"}`;

    const h = s.overall_health || "UNSPECIFIED";
    $("badge-health").textContent = h;
    $("badge-health").className = `badge ${h === "ONLINE" ? "badge-ok" : "badge-warn"}`;
  }

  function renderOverview() {
    const s = state.system;
    if (!s) return;
    const rec = s.reconciler || {};
    const totalCap = state.pools.reduce((acc, p) => acc + Number(p.total_bytes || 0), 0);
    const freeCap = state.pools.reduce((acc, p) => acc + Number(p.free_bytes || 0), 0);

    $("overview-kpis").innerHTML = `
      <div class="card">
        <div class="kpi-label">Appliance Health</div>
        <div class="kpi-value">${escapeHtml(s.overall_health || "--")}</div>
        <div class="kpi-sub">Node ${escapeHtml(s.node?.node_id || "--")}</div>
      </div>
      <div class="card">
        <div class="kpi-label">Pool Capacity</div>
        <div class="kpi-value">${fmtBytes(totalCap)}</div>
        <div class="kpi-sub">${fmtBytes(freeCap)} free across ${state.pools.length} pool(s)</div>
      </div>
      <div class="card">
        <div class="kpi-label">SAN Resources</div>
        <div class="kpi-value">${state.volumes.length} vols / ${state.exports.length} exports</div>
        <div class="kpi-sub">Drift repaired: ${rec.drift_repaired_count || 0}</div>
      </div>
    `;

    const tickRate = Number(state.iostats.tick_rate || 0);
    const stats = state.iostats.stats || [];
    $("iostat-updated").textContent = `Updated ${new Date().toLocaleTimeString()}`;

    if (stats.length === 0) {
      $("iostat-tbody").innerHTML = `<tr><td colspan="9" class="muted">No SPDK bdevs reporting I/O statistics yet.</td></tr>`;
      return;
    }

    $("iostat-tbody").innerHTML = stats.map((b) => {
      let rIops = "0", wIops = "0", rMBs = "0.00", wMBs = "0.00";
      if (state.prevIostat && state.prevIostat.map[b.name]) {
        const p = state.prevIostat.map[b.name];
        const dt = state.prevIostat.dt;
        rIops = Math.max(0, Math.round((b.num_read_ops - p.num_read_ops) / dt)).toString();
        wIops = Math.max(0, Math.round((b.num_write_ops - p.num_write_ops) / dt)).toString();
        rMBs = Math.max(0, (b.bytes_read - p.bytes_read) / dt / (1024 * 1024)).toFixed(2);
        wMBs = Math.max(0, (b.bytes_written - p.bytes_written) / dt / (1024 * 1024)).toFixed(2);
      }
      const avgRLatUs = tickRate > 0 && b.num_read_ops > 0 ? ((b.read_latency_ticks / b.num_read_ops / tickRate) * 1e6).toFixed(1) + " us" : "--";
      const avgWLatUs = tickRate > 0 && b.num_write_ops > 0 ? ((b.write_latency_ticks / b.num_write_ops / tickRate) * 1e6).toFixed(1) + " us" : "--";
      return `
        <tr>
          <td class="mono">${escapeHtml(b.name)}</td>
          <td class="mono">${rIops}</td>
          <td class="mono">${wIops}</td>
          <td class="mono">${rMBs}</td>
          <td class="mono">${wMBs}</td>
          <td class="mono">${avgRLatUs}</td>
          <td class="mono">${avgWLatUs}</td>
          <td class="mono">${fmtBytes(b.bytes_read)}</td>
          <td class="mono">${fmtBytes(b.bytes_written)}</td>
        </tr>
      `;
    }).join("");
  }

  function renderDisks() {
    if (state.disks.length === 0) {
      $("disks-tbody").innerHTML = `<tr><td colspan="9" class="muted">No physical disks discovered. Click Rescan Hardware.</td></tr>`;
      return;
    }
    $("disks-tbody").innerHTML = state.disks.map((d) => {
      const poolName = state.pools.find((p) => p.id === d.pool_id)?.name || "";
      const bootBadge = d.is_boot_device ? ` <span class="badge badge-warn">BOOT</span>` : "";
      const drvClass = d.driver_state === "KERNEL" ? "badge-muted" : "badge-info";
      let actionBtn = "";
      if (d.is_boot_device) {
        actionBtn = `<span class="muted">Protected</span>`;
      } else if (!d.claimed) {
        actionBtn = `<button class="btn btn-primary btn-sm" data-disk-claim="${escapeHtml(d.id)}">Claim</button>`;
      } else if (!d.pool_id) {
        actionBtn = `<button class="btn btn-sm" data-disk-release="${escapeHtml(d.id)}">Release</button>`;
      } else {
        actionBtn = `<span class="muted">In Pool</span>`;
      }
      return `
        <tr>
          <td class="mono">${escapeHtml(d.name)}${bootBadge}</td>
          <td><span class="badge badge-muted">${escapeHtml(d.type)}</span></td>
          <td class="mono">${escapeHtml(d.pci_address || d.device_path || "--")}</td>
          <td>${escapeHtml(d.model || "--")}<div class="muted mono">${escapeHtml(d.serial_number || "")}</div></td>
          <td class="mono">${fmtBytes(d.size_bytes)}</td>
          <td><span class="badge ${drvClass}">${escapeHtml(d.driver_state)}</span></td>
          <td class="mono">${escapeHtml(d.spdk_bdev_name || "--")}</td>
          <td class="mono">${escapeHtml(poolName || "--")}</td>
          <td><div class="actions-cell">${actionBtn}</div></td>
        </tr>
      `;
    }).join("");
  }

  function renderPools() {
    if (state.pools.length === 0) {
      $("pools-tbody").innerHTML = `<tr><td colspan="8" class="muted">No storage pools created yet.</td></tr>`;
      return;
    }
    $("pools-tbody").innerHTML = state.pools.map((p) => {
      const total = Number(p.total_bytes || 0);
      const alloc = Number(p.allocated_bytes || 0);
      const pct = total > 0 ? Math.min(100, Math.round((alloc / total) * 100)) : 0;
      return `
        <tr>
          <td class="mono">${escapeHtml(p.name)}<div class="muted">${escapeHtml(p.id)}</div></td>
          <td><span class="badge badge-info">${escapeHtml(p.raid_level)}</span></td>
          <td class="mono">${fmtBytes(p.cluster_size_bytes)}</td>
          <td class="mono">${fmtBytes(total)}<div class="muted">${fmtBytes(p.free_bytes)} free</div></td>
          <td>
            <div class="bar"><div class="bar-fill" style="width:${pct}%"></div></div>
            <div class="muted mono">${pct}%</div>
          </td>
          <td class="mono">${escapeHtml(p.lvs_name)}</td>
          <td>${healthBadge(p.health_status)}${p.status_message ? `<div class="muted" style="font-size:0.75rem;margin-top:0.2rem">${escapeHtml(p.status_message)}</div>` : ""}</td>
          <td><div class="actions-cell">
            <button class="btn btn-danger btn-sm" data-pool-delete="${escapeHtml(p.id)}">Delete</button>
          </div></td>
        </tr>
      `;
    }).join("");
  }

  function renderVolumes() {
    if (state.volumes.length === 0) {
      $("volumes-tbody").innerHTML = `<tr><td colspan="8" class="muted">No logical volumes created yet.</td></tr>`;
      return;
    }
    $("volumes-tbody").innerHTML = state.volumes.map((v) => {
      const poolName = state.pools.find((p) => p.id === v.pool_id)?.name || v.pool_id;
      const qos = v.qos_policy || {};
      const qosText = (qos.rw_ios_per_sec || qos.rw_mbytes_per_sec) ? `${qos.rw_ios_per_sec || "∞"} / ${qos.rw_mbytes_per_sec || "∞"}` : "Unlimited";
      return `
        <tr>
          <td class="mono">${escapeHtml(v.name)}<div class="muted">${escapeHtml(v.id)}</div></td>
          <td class="mono">${escapeHtml(poolName)}</td>
          <td class="mono">${fmtBytes(v.size_bytes)}</td>
          <td><span class="badge ${v.thin_provision ? "badge-info" : "badge-muted"}">${v.thin_provision ? "THIN" : "THICK"}</span></td>
          <td><span class="badge badge-muted">${escapeHtml(v.attachment_state)}</span></td>
          <td class="mono">${qosText}</td>
          <td>${healthBadge(v.health_status)}${v.status_message ? `<div class="muted" style="font-size:0.75rem;margin-top:0.2rem">${escapeHtml(v.status_message)}</div>` : ""}</td>
          <td><div class="actions-cell">
            <button class="btn btn-sm" data-vol-snap="${escapeHtml(v.id)}">Snapshot</button>
            <button class="btn btn-sm" data-vol-resize="${escapeHtml(v.id)}">Resize</button>
            <button class="btn btn-sm" data-vol-qos="${escapeHtml(v.id)}">QoS</button>
            <button class="btn btn-danger btn-sm" data-vol-delete="${escapeHtml(v.id)}">Delete</button>
          </div></td>
        </tr>
      `;
    }).join("");
  }

  function renderSnapshots() {
    if (state.snapshots.length === 0) {
      $("snapshots-tbody").innerHTML = `<tr><td colspan="7" class="muted">No CoW snapshots created yet.</td></tr>`;
      return;
    }
    $("snapshots-tbody").innerHTML = state.snapshots.map((sn) => {
      const volName = state.volumes.find((v) => v.id === sn.volume_id)?.name || sn.volume_id;
      const parentName = state.snapshots.find((p) => p.id === sn.parent_snapshot_id)?.name || sn.parent_snapshot_id || "--";
      return `
        <tr>
          <td class="mono">${escapeHtml(sn.name)}<div class="muted">${escapeHtml(sn.id)}</div></td>
          <td class="mono">${escapeHtml(volName)}</td>
          <td class="mono">${sn.sequence_number}</td>
          <td class="mono">${escapeHtml(parentName)}</td>
          <td class="mono">${fmtBytes(sn.size_bytes)}</td>
          <td class="mono">${escapeHtml(sn.lvol_bdev_name)}</td>
          <td><div class="actions-cell">
            <button class="btn btn-sm" data-snap-clone="${escapeHtml(sn.id)}">Clone</button>
            <button class="btn btn-danger btn-sm" data-snap-delete="${escapeHtml(sn.id)}">Delete</button>
          </div></td>
        </tr>
      `;
    }).join("");
  }

  function renderGroupSnapshots() {
    const tbody = $("gsnaps-tbody");
    if (!tbody) return;
    if (!state.groupSnapshots.length) {
      tbody.innerHTML = `<tr><td colspan="6" class="muted">No volume group snapshots created yet</td></tr>`;
      return;
    }
    const volById = {};
    for (const v of state.volumes) volById[v.id] = v.name;
    tbody.innerHTML = state.groupSnapshots.map((gs) => {
      const volNames = (gs.volume_ids || []).map((id) => volById[id] || id).join(", ");
      const snapCount = (gs.snapshot_ids || []).length;
      return `
        <tr>
          <td><strong>${escapeHtml(gs.name)}</strong></td>
          <td class="mono">${escapeHtml(gs.id)}</td>
          <td>${escapeHtml(volNames || "--")}</td>
          <td>${snapCount} snapshot(s)</td>
          <td class="mono">${escapeHtml(gs.created_at || "--")}</td>
          <td><div class="actions-cell">
            <button class="btn btn-danger btn-sm" data-gsnap-delete="${escapeHtml(gs.id)}">Delete</button>
          </div></td>
        </tr>
      `;
    }).join("");
  }

  function renderLifecycle() {
    const summaryEl = $("os-status-summary");
    const tbody = $("containers-tbody");
    if (!summaryEl || !tbody) return;

    const os = state.osStatus;
    const cu = state.containerUpdates;

    const booted = os && os.booted ? os.booted : null;
    const staged = os && os.staged ? os.staged : null;
    const rollback = os && os.rollback ? os.rollback : null;

    const osStateLabel = !os
      ? "Unavailable"
      : os.available
        ? (staged ? "Upgrade Staged" : "Booted")
        : "Not bootc";

    const updateSummary = !cu
      ? "Unavailable"
      : !cu.available
        ? "Not available"
        : cu.updates_available
          ? "Updates Available"
          : "Up to Date";

    summaryEl.innerHTML = `
      <div class="card">
        <div class="kpi-label">Bootc OS</div>
        <div class="kpi-value">${escapeHtml(booted ? (booted.version || "booted") : osStateLabel)}</div>
        <div class="kpi-sub mono">${escapeHtml(booted ? booted.image : (os && os.message ? os.message : "bootc status unavailable"))}</div>
      </div>
      <div class="card">
        <div class="kpi-label">Staged / Rollback</div>
        <div class="kpi-value">${escapeHtml(staged ? (staged.version || "staged") : (rollback ? (rollback.version || "rollback ready") : "None"))}</div>
        <div class="kpi-sub mono">${escapeHtml(staged ? staged.image : (rollback ? `Rollback: ${rollback.image}` : "No pending OS deployment"))}</div>
      </div>
      <div class="card">
        <div class="kpi-label">Container Updates</div>
        <div class="kpi-value">${escapeHtml(updateSummary)}</div>
        <div class="kpi-sub">${escapeHtml(cu && cu.last_update_state ? `Last apply: ${cu.last_update_state} — ${cu.last_update_message || ""}` : (cu && cu.message ? cu.message : "Checked via podman auto-update"))}</div>
      </div>
    `;

    const containers = (cu && cu.containers) || [];
    if (!containers.length) {
      tbody.innerHTML = `<tr><td colspan="7" class="muted">No Quadlet / AutoUpdate containers reported (${escapeHtml((cu && cu.message) || "unavailable")})</td></tr>`;
      return;
    }

    tbody.innerHTML = containers.map((c) => {
      const badgeClass = c.update_available ? "badge-warn" : "badge-ok";
      const updateText = c.update_available ? "Update available" : (c.update_status || "Up to date");
      return `
        <tr>
          <td class="mono">${escapeHtml(c.unit || "--")}</td>
          <td class="mono">${escapeHtml(c.container_name || "--")}</td>
          <td class="mono">${escapeHtml(c.image || "--")}</td>
          <td>${escapeHtml(c.policy || "--")}</td>
          <td>${escapeHtml(c.running_status || "--")}</td>
          <td><span class="badge ${badgeClass}">${escapeHtml(updateText)}</span></td>
          <td><div class="actions-cell">
            <button class="btn btn-sm" data-container-update="${escapeHtml(c.unit || "")}" ${c.unit ? "" : "disabled"}>Update</button>
          </div></td>
        </tr>
      `;
    }).join("");
  }

  function renderNetwork() {
    const summaryEl = $("network-summary");
    const tbody = $("network-tbody");
    if (!summaryEl || !tbody) return;

    const net = state.network;
    if (!net || !net.available) {
      summaryEl.textContent = (net && net.message) || "Network interface telemetry unavailable";
      tbody.innerHTML = `<tr><td colspan="9" class="muted">No network interfaces reported</td></tr>`;
      return;
    }

    const canEdit = Boolean(net.nmcli_available) && isAdmin();
    summaryEl.textContent = `Host: ${net.hostname || "--"} · NetworkManager: ${net.nmcli_available ? "active (configurable)" : "not detected (read-only)"}${!isAdmin() ? " · Admin role required to modify" : ""}`;

    const ifaces = net.interfaces || [];
    if (!ifaces.length) {
      tbody.innerHTML = `<tr><td colspan="9" class="muted">No physical or bridge interfaces detected</td></tr>`;
      return;
    }

    tbody.innerHTML = ifaces.map((iface) => {
      const badgeCls = iface.link_up ? "badge-ok" : "badge-muted";
      const addrs = (iface.addresses || []).join(", ") || "--";
      const gwDns = [
        iface.gateway ? `GW ${iface.gateway}` : "",
        (iface.dns && iface.dns.length) ? `DNS ${iface.dns.join(", ")}` : "",
      ].filter(Boolean).join(" · ") || "--";
      return `
        <tr>
          <td><strong>${escapeHtml(iface.name)}</strong>${iface.connection ? `<div class="muted" style="font-size:0.75rem">${escapeHtml(iface.connection)}</div>` : ""}</td>
          <td>${escapeHtml(iface.type || "--")}</td>
          <td><span class="badge ${badgeCls}">${escapeHtml(iface.state || "UNKNOWN")}</span></td>
          <td class="mono">${escapeHtml(iface.mac || "--")}</td>
          <td class="mono">${escapeHtml(iface.mtu || "--")}</td>
          <td>${escapeHtml(iface.method || "--")}</td>
          <td class="mono">${escapeHtml(addrs)}</td>
          <td class="mono">${escapeHtml(gwDns)}</td>
          <td><div class="actions-cell">
            <button class="btn btn-sm" data-net-config="${escapeHtml(iface.name)}" ${canEdit ? "" : "disabled"}>Configure</button>
          </div></td>
        </tr>
      `;
    }).join("");
  }

  function renderExports() {
    if (state.exports.length === 0) {
      $("exports-tbody").innerHTML = `<tr><td colspan="7" class="muted">No SAN exports configured yet.</td></tr>`;
      return;
    }
    $("exports-tbody").innerHTML = state.exports.map((e) => {
      const volName = state.volumes.find((v) => v.id === e.volume_id)?.name || e.volume_id;
      const target = e.protocol === "ISCSI" ? e.target_iqn : e.subsystem_nqn;
      const access = e.allow_any_host ? "ANY HOST" : `${(e.initiator_group_ids || []).length} group(s)`;
      return `
        <tr>
          <td class="mono">${escapeHtml(e.name)}</td>
          <td><span class="badge badge-info">${escapeHtml(e.protocol)}</span></td>
          <td class="mono">${escapeHtml(volName)}</td>
          <td class="mono">${escapeHtml(target)}</td>
          <td><span class="badge badge-muted">${escapeHtml(access)}</span></td>
          <td>${healthBadge(e.health_status)}${e.status_message ? `<div class="muted" style="font-size:0.75rem;margin-top:0.2rem">${escapeHtml(e.status_message)}</div>` : ""}</td>
          <td><div class="actions-cell">
            <button class="btn btn-sm" data-export-connect="${escapeHtml(e.id)}">Connect CLI</button>
            <button class="btn btn-danger btn-sm" data-export-delete="${escapeHtml(e.id)}">Delete</button>
          </div></td>
        </tr>
      `;
    }).join("");
  }

  function renderPortals() {
    if (state.portals.length === 0) {
      $("portals-tbody").innerHTML = `<tr><td colspan="5" class="muted">No portals created yet.</td></tr>`;
      return;
    }
    $("portals-tbody").innerHTML = state.portals.map((p) => `
      <tr>
        <td class="mono">${escapeHtml(p.name)}</td>
        <td><span class="badge badge-info">${escapeHtml(p.protocol)}</span></td>
        <td class="mono">${escapeHtml(p.ip_address)}</td>
        <td class="mono">${p.port}</td>
        <td><div class="actions-cell">
          <button class="btn btn-danger btn-sm" data-portal-delete="${escapeHtml(p.id)}">Delete</button>
        </div></td>
      </tr>
    `).join("");
  }

  function renderHosts() {
    if (state.hosts.length === 0) {
      $("hosts-tbody").innerHTML = `<tr><td colspan="5" class="muted">No SAN hosts registered yet.</td></tr>`;
      return;
    }
    $("hosts-tbody").innerHTML = state.hosts.map((h) => `
      <tr>
        <td class="mono">${escapeHtml(h.name)}<div class="muted">${escapeHtml(h.id)}</div></td>
        <td class="mono">${escapeHtml(h.host_nqn || "--")}</td>
        <td class="mono">${escapeHtml(h.initiator_iqn || "--")}</td>
        <td class="mono">${escapeHtml(h.ip_address || "--")}</td>
        <td><div class="actions-cell">
          <button class="btn btn-danger btn-sm" data-host-delete="${escapeHtml(h.id)}">Delete</button>
        </div></td>
      </tr>
    `).join("");
  }

  function renderInitiatorGroups() {
    if (state.initiatorGroups.length === 0) {
      $("igs-tbody").innerHTML = `<tr><td colspan="6" class="muted">No initiator groups created yet.</td></tr>`;
      return;
    }
    $("igs-tbody").innerHTML = state.initiatorGroups.map((g) => {
      const hostNames = (g.host_ids || []).map((id) => state.hosts.find((h) => h.id === id)?.name || id);
      return `
      <tr>
        <td class="mono">${escapeHtml(g.name)}</td>
        <td class="mono">${escapeHtml(hostNames.join(", ") || "--")}</td>
        <td class="mono">${escapeHtml((g.host_nqns || []).join(", ") || "--")}</td>
        <td class="mono">${escapeHtml((g.initiator_iqns || []).join(", ") || "--")}</td>
        <td class="mono">${escapeHtml((g.allowed_cidrs || []).join(", ") || "--")}</td>
        <td><div class="actions-cell">
          <button class="btn btn-danger btn-sm" data-ig-delete="${escapeHtml(g.id)}">Delete</button>
        </div></td>
      </tr>
    `;
    }).join("");
  }

  function roleBadge(role) {
    const r = String(role || "READONLY").toUpperCase();
    const cls = r === "ADMIN" ? "badge-info" : r === "CSI_PROVISIONER" ? "badge-ok" : "badge-muted";
    return `<span class="badge ${cls}">${escapeHtml(r)}</span>`;
  }

  function renderUsers() {
    if (state.users.length === 0) {
      $("users-tbody").innerHTML = `<tr><td colspan="5" class="muted">No users found.</td></tr>`;
      return;
    }
    const me = state.me ? state.me.username : "";
    $("users-tbody").innerHTML = state.users.map((u) => {
      const isSelf = u.username === me;
      return `
      <tr>
        <td class="mono">${escapeHtml(u.username)}${isSelf ? ' <span class="badge badge-muted">you</span>' : ""}</td>
        <td>${roleBadge(u.role)}</td>
        <td class="mono muted">${u.created_at ? new Date(u.created_at).toLocaleString() : "--"}</td>
        <td class="mono muted">${u.updated_at ? new Date(u.updated_at).toLocaleString() : "--"}</td>
        <td><div class="actions-cell">
          <button class="btn btn-sm" data-user-edit="${escapeHtml(u.username)}">Edit</button>
          <button class="btn btn-danger btn-sm" data-user-delete="${escapeHtml(u.username)}" ${isSelf ? "disabled" : ""}>Delete</button>
        </div></td>
      </tr>
    `;
    }).join("");
  }

  function renderTokens() {
    if (state.tokens.length === 0) {
      $("tokens-tbody").innerHTML = `<tr><td colspan="6" class="muted">No access tokens issued.</td></tr>`;
      return;
    }
    $("tokens-tbody").innerHTML = state.tokens.map((t) => `
      <tr>
        <td class="mono">${escapeHtml(t.name)}</td>
        <td class="mono">${escapeHtml(t.prefix)}...</td>
        <td><span class="badge badge-info">${escapeHtml(t.role)}</span></td>
        <td class="mono muted">${t.created_at ? new Date(t.created_at).toLocaleString() : "--"}</td>
        <td class="mono muted">${t.expires_at ? new Date(t.expires_at).toLocaleString() : "Never"}</td>
        <td><div class="actions-cell">
          <button class="btn btn-danger btn-sm" data-token-delete="${escapeHtml(t.id)}">Revoke</button>
        </div></td>
      </tr>
    `).join("");
  }

  function switchTab(tab) {
    state.tab = tab;
    document.querySelectorAll(".nav-item").forEach((btn) => {
      btn.classList.toggle("active", btn.dataset.tab === tab);
    });
    document.querySelectorAll("main > section").forEach((sec) => {
      sec.classList.toggle("hidden", sec.id !== `tab-${tab}`);
    });
    const activeBtn = document.querySelector(`.nav-item[data-tab="${tab}"]`);
    if (activeBtn) $("page-title").textContent = activeBtn.textContent;
    if (tab === "access") refreshAll();
  }

  // Event wiring
  $("nav").addEventListener("click", (e) => {
    const btn = e.target.closest(".nav-item");
    if (btn) switchTab(btn.dataset.tab);
  });

  $("modal-close").addEventListener("click", closeModal);
  $("modal-overlay").addEventListener("click", (e) => {
    if (e.target === $("modal-overlay")) closeModal();
  });

  $("form-login").addEventListener("submit", async (e) => {
    e.preventDefault();
    const fd = new FormData(e.target);
    try {
      const res = await fetch("/api/v1/auth/login", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ username: fd.get("username"), password: fd.get("password") }),
      });
      const data = await res.json();
      if (!res.ok) throw new Error(data.error || "Login failed");
      setToken(data.access_token);
      hideLogin();
      if (data.must_change_password) {
        promptInitialPasswordChange(fd.get("password"));
        return;
      }
      toast("Signed in to Velocor");
      await refreshAll();
    } catch (err) {
      $("login-error").textContent = err.message;
      $("login-error").classList.remove("hidden");
    }
  });

  function promptInitialPasswordChange(currentPassword) {
    openModal("Set a new administrator password", `
      <form id="form-initial-password" style="display:flex;flex-direction:column;gap:0.85rem">
        <p class="muted">This account is using its initial password. Choose a new one (at least 8 characters) to continue.</p>
        <div class="field"><label>New password</label><input type="password" name="new_password" minlength="8" required autocomplete="new-password" /></div>
        <div class="field"><label>Confirm new password</label><input type="password" name="confirm_password" minlength="8" required autocomplete="new-password" /></div>
        <div class="modal-footer"><button type="submit" class="btn btn-primary">Set password</button></div>
      </form>
    `, () => {
      $("form-initial-password").addEventListener("submit", async (e) => {
        e.preventDefault();
        const fd = new FormData(e.target);
        if (fd.get("new_password") !== fd.get("confirm_password")) {
          toast("Passwords do not match", "err");
          return;
        }
        try {
          const res = await api("POST", "/api/v1/auth/password", {
            current_password: currentPassword,
            new_password: fd.get("new_password"),
          });
          if (res.access_token) setToken(res.access_token);
          closeModal();
          toast("Password updated");
          await refreshAll();
        } catch (err) { toast(err.message, "err"); }
      });
    });
  }

  $("btn-logout").addEventListener("click", () => {
    setToken("");
    showLogin();
  });

  $("btn-refresh").addEventListener("click", refreshAll);

  $("btn-reconcile").addEventListener("click", async () => {
    try {
      await api("POST", "/api/v1/system/reconcile");
      toast("Reconciliation pass completed");
      await refreshAll();
    } catch (err) { toast(err.message, "err"); }
  });

  $("btn-scan-disks").addEventListener("click", async () => {
    try {
      await api("POST", "/api/v1/disks:scan");
      toast("Hardware scan completed");
      await refreshAll();
    } catch (err) { toast(err.message, "err"); }
  });

  // Delegated table actions
  document.body.addEventListener("click", async (e) => {
    const t = e.target;
    if (!(t instanceof HTMLElement)) return;
    try {
      if (t.dataset.diskClaim) {
        const claimPath = `/api/v1/disks/${encodeURIComponent(t.dataset.diskClaim)}:claim`;
        try {
          await api("POST", claimPath);
        } catch (err) {
          if (!/force=true/.test(err.message)) throw err;
          if (!confirm(`${err.message}\n\nClaiming this disk will DESTROY all data on it. Continue?`)) return;
          await api("POST", `${claimPath}?force=true`);
        }
        toast("Disk claimed for SPDK");
        await refreshAll();
      } else if (t.dataset.diskRelease) {
        await api("POST", `/api/v1/disks/${encodeURIComponent(t.dataset.diskRelease)}:release`);
        toast("Disk released back to Linux kernel");
        await refreshAll();
      } else if (t.dataset.poolDelete) {
        if (!confirm(`Are you sure you want to delete storage pool "${t.dataset.poolDelete}"?`)) return;
        await api("DELETE", `/api/v1/pools/${encodeURIComponent(t.dataset.poolDelete)}`);
        toast("Pool deleted");
        await refreshAll();
      } else if (t.dataset.volDelete) {
        if (!confirm(`Are you sure you want to delete volume "${t.dataset.volDelete}"?`)) return;
        await api("DELETE", `/api/v1/volumes/${encodeURIComponent(t.dataset.volDelete)}`);
        toast("Volume deleted");
        await refreshAll();
      } else if (t.dataset.snapDelete) {
        if (!confirm(`Are you sure you want to delete snapshot "${t.dataset.snapDelete}"?`)) return;
        await api("DELETE", `/api/v1/snapshots/${encodeURIComponent(t.dataset.snapDelete)}`);
        toast("Snapshot deleted");
        await refreshAll();
      } else if (t.dataset.exportDelete) {
        if (!confirm(`Are you sure you want to delete SAN export "${t.dataset.exportDelete}"?`)) return;
        await api("DELETE", `/api/v1/exports/${encodeURIComponent(t.dataset.exportDelete)}`);
        toast("Export deleted");
        await refreshAll();
      } else if (t.dataset.portalDelete) {
        if (!confirm(`Are you sure you want to delete SAN portal "${t.dataset.portalDelete}"?`)) return;
        await api("DELETE", `/api/v1/portals/${encodeURIComponent(t.dataset.portalDelete)}`);
        toast("Portal deleted");
        await refreshAll();
      } else if (t.dataset.hostDelete) {
        if (!confirm(`Are you sure you want to delete host "${t.dataset.hostDelete}"?`)) return;
        await api("DELETE", `/api/v1/hosts/${encodeURIComponent(t.dataset.hostDelete)}`);
        toast("Host deleted");
        await refreshAll();
      } else if (t.dataset.igDelete) {
        if (!confirm(`Are you sure you want to delete initiator group "${t.dataset.igDelete}"?`)) return;
        await api("DELETE", `/api/v1/initiator-groups/${encodeURIComponent(t.dataset.igDelete)}`);
        toast("Initiator group deleted");
        await refreshAll();
      } else if (t.dataset.userEdit) {
        openEditUserModal(t.dataset.userEdit);
      } else if (t.dataset.userDelete) {
        if (!confirm(`Are you sure you want to delete user "${t.dataset.userDelete}"?`)) return;
        await api("DELETE", `/api/v1/auth/users/${encodeURIComponent(t.dataset.userDelete)}`);
        toast("User deleted");
        await refreshAll();
      } else if (t.dataset.tokenDelete) {
        if (!confirm(`Are you sure you want to revoke token "${t.dataset.tokenDelete}"?`)) return;
        await api("DELETE", `/api/v1/auth/tokens/${encodeURIComponent(t.dataset.tokenDelete)}`);
        toast("Token revoked");
        await refreshAll();
      } else if (t.dataset.volSnap) {
        openSnapshotModal(t.dataset.volSnap);
      } else if (t.dataset.volResize) {
        openResizeModal(t.dataset.volResize);
      } else if (t.dataset.volQos) {
        openQosModal(t.dataset.volQos);
      } else if (t.dataset.snapClone) {
        openCloneModal(t.dataset.snapClone);
      } else if (t.dataset.exportConnect) {
        openConnectModal(t.dataset.exportConnect);
      }
    } catch (err) {
      toast(err.message, "err");
    }
  });

  // Modals
  $("btn-open-create-pool").addEventListener("click", () => {
    const avail = state.disks.filter((d) => d.claimed && !d.pool_id && !d.is_boot_device);
    const diskBoxes = avail.length
      ? avail.map((d) => `<label class="checkbox-row"><input type="checkbox" name="disk_ids" value="${escapeHtml(d.id)}" /> <span class="mono">${escapeHtml(d.name)} (${fmtBytes(d.size_bytes)}, ${escapeHtml(d.driver_state)})</span></label>`).join("")
      : `<div class="muted">No claimed, unassigned disks available. Claim disks in Physical Disks first.</div>`;

    openModal("Create Storage Pool", `
      <form id="form-create-pool" style="display:flex;flex-direction:column;gap:0.85rem">
        <div class="field"><label>Pool Name</label><input name="name" required placeholder="nvme-pool" /></div>
        <div class="form-grid">
          <div class="field">
            <label>RAID Level</label>
            <select name="raid_level">
              <option value="RAID1">RAID1 (Mirror)</option>
              <option value="RAID0">RAID0 (Stripe)</option>
              <option value="RAID10">RAID10</option>
              <option value="RAID5F">RAID5F</option>
            </select>
          </div>
          <div class="field">
            <label>Cluster Size</label>
            <select name="cluster_size_bytes">
              <option value="65536">64 KiB (CBT Optimal)</option>
              <option value="1048576">1 MiB</option>
              <option value="4194304">4 MiB</option>
            </select>
          </div>
        </div>
        <div class="field"><label>Select Claimed Disks</label>${diskBoxes}</div>
        <div class="modal-footer">
          <button type="submit" class="btn btn-primary">Create Pool</button>
        </div>
      </form>
    `, () => {
      $("form-create-pool").addEventListener("submit", async (e) => {
        e.preventDefault();
        const fd = new FormData(e.target);
        const diskIDs = fd.getAll("disk_ids");
        if (diskIDs.length === 0) return toast("Select at least one disk", "err");
        try {
          await api("POST", "/api/v1/pools", {
            name: fd.get("name"),
            raid_level: fd.get("raid_level"),
            cluster_size_bytes: Number(fd.get("cluster_size_bytes")),
            disk_ids: diskIDs,
          });
          closeModal();
          toast("Pool created");
          await refreshAll();
        } catch (err) { toast(err.message, "err"); }
      });
    });
  });

  $("btn-open-create-volume").addEventListener("click", () => {
    const poolOpts = state.pools.map((p) => `<option value="${escapeHtml(p.id)}">${escapeHtml(p.name)} (${fmtBytes(p.free_bytes)} free)</option>`).join("");
    openModal("Create Logical Volume", `
      <form id="form-create-vol" style="display:flex;flex-direction:column;gap:0.85rem">
        <div class="field"><label>Volume Name</label><input name="name" required placeholder="vol-app-01" /></div>
        <div class="field"><label>Pool</label><select name="pool_id" required>${poolOpts}</select></div>
        <div class="form-grid">
          <div class="field"><label>Size (GiB)</label><input type="number" name="size_gib" min="1" value="2" required /></div>
          <div class="field" style="justify-content:flex-end">
            <label class="checkbox-row"><input type="checkbox" name="thin_provision" checked /> Thin Provisioned</label>
          </div>
        </div>
        <div class="form-grid">
          <div class="field"><label>QoS Max IOPS (0 = unlimited)</label><input type="number" name="rw_ios_per_sec" min="0" value="0" /></div>
          <div class="field"><label>QoS Max MB/s (0 = unlimited)</label><input type="number" name="rw_mbytes_per_sec" min="0" value="0" /></div>
        </div>
        <div class="modal-footer"><button type="submit" class="btn btn-primary">Create Volume</button></div>
      </form>
    `, () => {
      $("form-create-vol").addEventListener("submit", async (e) => {
        e.preventDefault();
        const fd = new FormData(e.target);
        try {
          await api("POST", "/api/v1/volumes", {
            name: fd.get("name"),
            pool_id: fd.get("pool_id"),
            size_bytes: Number(fd.get("size_gib")) * 1024 * 1024 * 1024,
            thin_provision: fd.get("thin_provision") === "on",
            qos_policy: {
              rw_ios_per_sec: Number(fd.get("rw_ios_per_sec") || 0),
              rw_mbytes_per_sec: Number(fd.get("rw_mbytes_per_sec") || 0),
            },
          });
          closeModal();
          toast("Volume created");
          await refreshAll();
        } catch (err) { toast(err.message, "err"); }
      });
    });
  });

  function openSnapshotModal(volID) {
    openModal("Create CoW Snapshot", `
      <form id="form-snap" style="display:flex;flex-direction:column;gap:0.85rem">
        <div class="field"><label>Snapshot Name</label><input name="name" required placeholder="snap-${Date.now()}" /></div>
        <div class="modal-footer"><button type="submit" class="btn btn-primary">Create Snapshot</button></div>
      </form>
    `, () => {
      $("form-snap").addEventListener("submit", async (e) => {
        e.preventDefault();
        const fd = new FormData(e.target);
        try {
          await api("POST", `/api/v1/volumes/${encodeURIComponent(volID)}/snapshots`, { name: fd.get("name") });
          closeModal();
          toast("Snapshot created");
          await refreshAll();
        } catch (err) { toast(err.message, "err"); }
      });
    });
  }

  function openResizeModal(volID) {
    const v = state.volumes.find((x) => x.id === volID);
    const curGiB = Math.max(1, Math.ceil(Number(v?.size_bytes || 0) / (1024 ** 3)));
    openModal(`Resize ${v?.name || volID}`, `
      <form id="form-resize" style="display:flex;flex-direction:column;gap:0.85rem">
        <div class="field"><label>New Size (GiB)</label><input type="number" name="size_gib" min="${curGiB}" value="${curGiB + 1}" required /></div>
        <div class="modal-footer"><button type="submit" class="btn btn-primary">Resize Volume</button></div>
      </form>
    `, () => {
      $("form-resize").addEventListener("submit", async (e) => {
        e.preventDefault();
        const fd = new FormData(e.target);
        try {
          await api("POST", `/api/v1/volumes/${encodeURIComponent(volID)}:resize`, { new_size_bytes: Number(fd.get("size_gib")) * (1024 ** 3) });
          closeModal();
          toast("Volume resized");
          await refreshAll();
        } catch (err) { toast(err.message, "err"); }
      });
    });
  }

  function openQosModal(volID) {
    const v = state.volumes.find((x) => x.id === volID);
    const q = v?.qos_policy || {};
    openModal(`QoS Policy: ${v?.name || volID}`, `
      <form id="form-qos" style="display:flex;flex-direction:column;gap:0.85rem">
        <div class="form-grid">
          <div class="field"><label>RW IOPS</label><input type="number" name="rw_ios_per_sec" min="0" value="${q.rw_ios_per_sec || 0}" /></div>
          <div class="field"><label>RW MB/s</label><input type="number" name="rw_mbytes_per_sec" min="0" value="${q.rw_mbytes_per_sec || 0}" /></div>
        </div>
        <div class="modal-footer"><button type="submit" class="btn btn-primary">Save QoS</button></div>
      </form>
    `, () => {
      $("form-qos").addEventListener("submit", async (e) => {
        e.preventDefault();
        const fd = new FormData(e.target);
        try {
          await api("PUT", `/api/v1/volumes/${encodeURIComponent(volID)}/qos`, {
            qos_policy: {
              rw_ios_per_sec: Number(fd.get("rw_ios_per_sec") || 0),
              rw_mbytes_per_sec: Number(fd.get("rw_mbytes_per_sec") || 0),
            },
          });
          closeModal();
          toast("QoS policy updated");
          await refreshAll();
        } catch (err) { toast(err.message, "err"); }
      });
    });
  }

  function openCloneModal(snapID) {
    openModal("Clone Snapshot to Volume", `
      <form id="form-clone" style="display:flex;flex-direction:column;gap:0.85rem">
        <div class="field"><label>Clone Volume Name</label><input name="clone_name" required placeholder="clone-${Date.now()}" /></div>
        <div class="modal-footer"><button type="submit" class="btn btn-primary">Create Clone</button></div>
      </form>
    `, () => {
      $("form-clone").addEventListener("submit", async (e) => {
        e.preventDefault();
        const fd = new FormData(e.target);
        try {
          await api("POST", `/api/v1/snapshots/${encodeURIComponent(snapID)}:clone`, { clone_name: fd.get("clone_name") });
          closeModal();
          toast("Clone volume created");
          await refreshAll();
        } catch (err) { toast(err.message, "err"); }
      });
    });
  }

  $("btn-open-cbt").addEventListener("click", () => {
    const opts = state.snapshots.map((s) => `<option value="${escapeHtml(s.id)}">${escapeHtml(s.name)} (seq ${s.sequence_number})</option>`).join("");
    openModal("Changed Block Tracking (CBT)", `
      <form id="form-cbt" style="display:flex;flex-direction:column;gap:0.85rem">
        <div class="form-grid">
          <div class="field"><label>Base Snapshot</label><select name="base">${opts}</select></div>
          <div class="field"><label>Target Snapshot</label><select name="target">${opts}</select></div>
        </div>
        <button type="submit" class="btn btn-primary">Query Changed Extents</button>
        <div id="cbt-result" class="code-box hidden"></div>
      </form>
    `, () => {
      $("form-cbt").addEventListener("submit", async (e) => {
        e.preventDefault();
        const fd = new FormData(e.target);
        try {
          const data = await api("GET", `/api/v1/snapshots/${encodeURIComponent(fd.get("target"))}/blocks:changed?base_snapshot_id=${encodeURIComponent(fd.get("base"))}`);
          $("cbt-result").textContent = JSON.stringify(data, null, 2);
          $("cbt-result").classList.remove("hidden");
        } catch (err) { toast(err.message, "err"); }
      });
    });
  });

  $("btn-open-create-portal").addEventListener("click", () => {
    const defaultPorts = { NVMEOF_TCP: 4420, NVMEOF_RDMA: 4420, ISCSI: 3260 };
    const ipOptions = [];
    const seenIPs = new Set();
    for (const iface of ((state.network && state.network.interfaces) || [])) {
      for (const cidr of (iface.addresses || [])) {
        const ip = String(cidr).split("/")[0].trim();
        if (ip && !seenIPs.has(ip)) {
          seenIPs.add(ip);
          ipOptions.push(`<option value="${escapeHtml(ip)}">${escapeHtml(ip)} (${escapeHtml(iface.name)})</option>`);
        }
      }
    }
    ipOptions.push(`<option value="0.0.0.0">0.0.0.0 (All interfaces)</option>`);
    ipOptions.push(`<option value="__custom__">Custom IP address...</option>`);

    openModal("Create SAN Portal", `
      <form id="form-portal" style="display:flex;flex-direction:column;gap:0.85rem">
        <div class="field"><label>Portal Name</label><input name="name" required placeholder="nvmf-tcp-0" /></div>
        <div class="form-grid">
          <div class="field">
            <label>Protocol</label>
            <select name="protocol" id="portal-protocol">
              <option value="NVMEOF_TCP">NVMEOF_TCP</option>
              <option value="ISCSI">ISCSI</option>
              <option value="NVMEOF_RDMA">NVMEOF_RDMA</option>
            </select>
          </div>
          <div class="field"><label>Port</label><input type="number" name="port" id="portal-port" value="4420" required /></div>
        </div>
        <div class="field">
          <label>IP Address</label>
          <select name="ip_select" id="portal-ip-select">
            ${ipOptions.join("")}
          </select>
        </div>
        <div class="field hidden" id="portal-ip-custom-wrap">
          <label>Custom IP Address</label>
          <input name="ip_custom" id="portal-ip-custom" placeholder="10.10.10.20" />
        </div>
        <div class="modal-footer"><button type="submit" class="btn btn-primary">Create Portal</button></div>
      </form>
    `, () => {
      const protoEl = $("portal-protocol");
      const portEl = $("portal-port");
      const ipSel = $("portal-ip-select");
      const customWrap = $("portal-ip-custom-wrap");
      const customInput = $("portal-ip-custom");

      protoEl.addEventListener("change", () => {
        if (defaultPorts[protoEl.value]) {
          portEl.value = String(defaultPorts[protoEl.value]);
        }
      });

      const toggleCustomIP = () => {
        const isCustom = ipSel.value === "__custom__";
        customWrap.classList.toggle("hidden", !isCustom);
        customInput.required = isCustom;
      };
      ipSel.addEventListener("change", toggleCustomIP);
      toggleCustomIP();

      $("form-portal").addEventListener("submit", async (e) => {
        e.preventDefault();
        const fd = new FormData(e.target);
        const chosenIP = ipSel.value === "__custom__" ? String(fd.get("ip_custom") || "").trim() : ipSel.value;
        try {
          await api("POST", "/api/v1/portals", {
            name: fd.get("name"),
            protocol: fd.get("protocol"),
            ip_address: chosenIP,
            port: Number(fd.get("port")),
          });
          closeModal();
          toast("Portal created");
          await refreshAll();
        } catch (err) { toast(err.message, "err"); }
      });
    });
  });

  $("btn-open-create-host").addEventListener("click", () => {
    openModal("Add SAN Host", `
      <form id="form-host" style="display:flex;flex-direction:column;gap:0.85rem">
        <div class="field"><label>Host Name</label><input name="name" required placeholder="k8s-worker-01" /></div>
        <div class="field"><label>NVMe-oF Host NQN</label><input name="host_nqn" placeholder="nqn.2014-08.org.nvmexpress:uuid:..." /></div>
        <div class="field"><label>iSCSI Initiator IQN</label><input name="initiator_iqn" placeholder="iqn.1994-05.com.redhat:..." /></div>
        <div class="field"><label>IP Address / CIDR (optional)</label><input name="ip_address" placeholder="10.0.2.20" /></div>
        <div class="modal-footer"><button type="submit" class="btn btn-primary">Add Host</button></div>
      </form>
    `, () => {
      $("form-host").addEventListener("submit", async (e) => {
        e.preventDefault();
        const fd = new FormData(e.target);
        try {
          await api("POST", "/api/v1/hosts", {
            name: fd.get("name"),
            host_nqn: fd.get("host_nqn"),
            initiator_iqn: fd.get("initiator_iqn"),
            ip_address: fd.get("ip_address"),
          });
          closeModal();
          toast("Host registered");
          await refreshAll();
        } catch (err) { toast(err.message, "err"); }
      });
    });
  });

  $("btn-open-create-ig").addEventListener("click", () => {
    const hostBoxes = state.hosts.map((h) => `<label class="checkbox-row"><input type="checkbox" name="host_ids" value="${escapeHtml(h.id)}" /> <span class="mono">${escapeHtml(h.name)} (${escapeHtml(h.host_nqn || h.initiator_iqn || h.ip_address || h.id)})</span></label>`).join("") || `<div class="muted">No hosts registered yet (you can still enter raw NQNs/IQNs below).</div>`;
    openModal("Create Initiator Group", `
      <form id="form-ig" style="display:flex;flex-direction:column;gap:0.85rem">
        <div class="field"><label>Group Name</label><input name="name" required placeholder="k8s-workers" /></div>
        <div class="field"><label>Registered Hosts</label>${hostBoxes}</div>
        <div class="field"><label>Additional Host NQNs (comma-separated)</label><input name="host_nqns" placeholder="nqn.2014-08.org.nvmexpress:uuid:..." /></div>
        <div class="field"><label>Additional Initiator IQNs (comma-separated)</label><input name="initiator_iqns" placeholder="iqn.1994-05.com.redhat:..." /></div>
        <div class="field"><label>Allowed CIDRs (comma-separated)</label><input name="allowed_cidrs" placeholder="10.0.0.0/8" /></div>
        <div class="modal-footer"><button type="submit" class="btn btn-primary">Create Group</button></div>
      </form>
    `, () => {
      $("form-ig").addEventListener("submit", async (e) => {
        e.preventDefault();
        const fd = new FormData(e.target);
        const split = (s) => String(s || "").split(",").map((x) => x.trim()).filter(Boolean);
        try {
          await api("POST", "/api/v1/initiator-groups", {
            name: fd.get("name"),
            host_ids: fd.getAll("host_ids"),
            host_nqns: split(fd.get("host_nqns")),
            initiator_iqns: split(fd.get("initiator_iqns")),
            allowed_cidrs: split(fd.get("allowed_cidrs")),
          });
          closeModal();
          toast("Initiator group created");
          await refreshAll();
        } catch (err) { toast(err.message, "err"); }
      });
    });
  });

  $("btn-open-create-export").addEventListener("click", () => {
    const volOpts = state.volumes.map((v) => `<option value="${escapeHtml(v.id)}">${escapeHtml(v.name)}</option>`).join("");
    const igBoxes = state.initiatorGroups.map((g) => `<label class="checkbox-row"><input type="checkbox" name="initiator_group_ids" value="${escapeHtml(g.id)}" /> <span class="mono">${escapeHtml(g.name)}</span></label>`).join("") || `<div class="muted">No initiator groups defined.</div>`;
    openModal("Create SAN Export", `
      <form id="form-export" style="display:flex;flex-direction:column;gap:0.85rem">
        <div class="field"><label>Export Name</label><input name="name" required placeholder="exp-app-01" /></div>
        <div class="form-grid">
          <div class="field"><label>Volume</label><select name="volume_id" required>${volOpts}</select></div>
          <div class="field">
            <label>Protocol</label>
            <select name="protocol" id="export-protocol">
              <option value="NVMEOF_TCP">NVMEOF_TCP</option>
              <option value="ISCSI">ISCSI</option>
            </select>
          </div>
        </div>
        <div class="field"><label>Portals</label><div id="export-portals-wrap"></div></div>
        <label class="checkbox-row"><input type="checkbox" name="allow_any_host" id="chk-allow-any-host" /> Allow Any Host <span class="muted">(no access control: any initiator that can reach the portal may connect)</span></label>
        <div class="field" id="export-igs-wrap"><label>Allowed Initiator Groups</label>${igBoxes}</div>
        <div class="modal-footer"><button type="submit" class="btn btn-primary">Create Export</button></div>
      </form>
    `, () => {
      const renderExportPortals = () => {
        const proto = $("export-protocol").value;
        const matching = state.portals.filter((p) => p.protocol === proto);
        $("export-portals-wrap").innerHTML = matching.map((p) =>
          `<label class="checkbox-row"><input type="checkbox" name="portal_ids" value="${escapeHtml(p.id)}" /> <span class="mono">${escapeHtml(p.name)} (${escapeHtml(p.protocol)} ${escapeHtml(p.ip_address)}:${p.port})</span></label>`
        ).join("") || `<div class="muted">No ${escapeHtml(proto)} portals defined. Create one first.</div>`;
      };
      $("export-protocol").addEventListener("change", renderExportPortals);
      renderExportPortals();

      $("chk-allow-any-host").addEventListener("change", (e) => {
        $("export-igs-wrap").classList.toggle("hidden", e.target.checked);
      });
      $("form-export").addEventListener("submit", async (e) => {
        e.preventDefault();
        const fd = new FormData(e.target);
        const allowAny = fd.get("allow_any_host") === "on";
        try {
          await api("POST", "/api/v1/exports", {
            name: fd.get("name"),
            volume_id: fd.get("volume_id"),
            protocol: fd.get("protocol"),
            portal_ids: fd.getAll("portal_ids"),
            initiator_group_ids: allowAny ? [] : fd.getAll("initiator_group_ids"),
            allow_any_host: allowAny,
          });
          closeModal();
          toast("Export created");
          await refreshAll();
        } catch (err) { toast(err.message, "err"); }
      });
    });
  });

  function openConnectModal(exportID) {
    const exp = state.exports.find((x) => x.id === exportID);
    if (!exp) return;
    const p = state.portals.find((x) => (exp.portal_ids || []).includes(x.id)) || { ip_address: "<SAN_IP>", port: exp.protocol === "ISCSI" ? 3260 : 4420 };
    const snippet = exp.protocol === "ISCSI"
      ? `iscsiadm -m discovery -t sendtargets -p ${p.ip_address}:${p.port}\niscsiadm -m node -T ${exp.target_iqn} -p ${p.ip_address}:${p.port} --login`
      : `nvme discover -t tcp -a ${p.ip_address} -s ${p.port}\nnvme connect -t tcp -n ${exp.subsystem_nqn} -a ${p.ip_address} -s ${p.port}`;
    openModal(`Connect: ${exp.name}`, `<div class="code-box">${escapeHtml(snippet)}</div>`);
  }

  $("btn-open-create-user").addEventListener("click", () => {
    openModal("Add User", `
      <form id="form-user" style="display:flex;flex-direction:column;gap:0.85rem">
        <div class="field"><label>Username</label><input name="username" required placeholder="viewer" /></div>
        <div class="form-grid">
          <div class="field">
            <label>Role</label>
            <select name="role">
              <option value="READONLY">Read-Only (READONLY)</option>
              <option value="CSI_PROVISIONER">Provisioner (CSI_PROVISIONER)</option>
              <option value="ADMIN">Administrator (ADMIN)</option>
            </select>
          </div>
          <div class="field"><label>Password (min 8 chars)</label><input type="password" name="password" required minlength="8" /></div>
        </div>
        <div class="modal-footer"><button type="submit" class="btn btn-primary">Create User</button></div>
      </form>
    `, () => {
      $("form-user").addEventListener("submit", async (e) => {
        e.preventDefault();
        const fd = new FormData(e.target);
        try {
          await api("POST", "/api/v1/auth/users", {
            username: fd.get("username"),
            role: fd.get("role"),
            password: fd.get("password"),
          });
          closeModal();
          toast("User created");
          await refreshAll();
        } catch (err) { toast(err.message, "err"); }
      });
    });
  });

  function openEditUserModal(username) {
    const user = state.users.find((u) => u.username === username);
    if (!user) return;
    openModal(`Edit User: ${username}`, `
      <form id="form-edit-user" style="display:flex;flex-direction:column;gap:0.85rem">
        <div class="form-grid">
          <div class="field">
            <label>Role</label>
            <select name="role">
              <option value="READONLY" ${user.role === "READONLY" ? "selected" : ""}>Read-Only (READONLY)</option>
              <option value="CSI_PROVISIONER" ${user.role === "CSI_PROVISIONER" ? "selected" : ""}>Provisioner (CSI_PROVISIONER)</option>
              <option value="ADMIN" ${user.role === "ADMIN" ? "selected" : ""}>Administrator (ADMIN)</option>
            </select>
          </div>
          <div class="field"><label>New Password (optional, min 8 chars)</label><input type="password" name="password" minlength="8" placeholder="Leave blank to keep" /></div>
        </div>
        <div class="modal-footer"><button type="submit" class="btn btn-primary">Save Changes</button></div>
      </form>
    `, () => {
      $("form-edit-user").addEventListener("submit", async (e) => {
        e.preventDefault();
        const fd = new FormData(e.target);
        const body = { role: fd.get("role") };
        const pw = String(fd.get("password") || "");
        if (pw) body.password = pw;
        try {
          await api("PATCH", `/api/v1/auth/users/${encodeURIComponent(username)}`, body);
          closeModal();
          toast("User updated");
          await refreshAll();
        } catch (err) { toast(err.message, "err"); }
      });
    });
  }

  $("btn-open-create-token").addEventListener("click", () => {
    openModal("Create Access Token", `
      <form id="form-token" style="display:flex;flex-direction:column;gap:0.85rem">
        <div class="field"><label>Token Name</label><input name="name" required placeholder="k8s-csi-controller" /></div>
        <div class="form-grid">
          <div class="field">
            <label>Role</label>
            <select name="role">
              <option value="CSI_PROVISIONER">CSI_PROVISIONER</option>
              <option value="ADMIN">ADMIN</option>
              <option value="READONLY">READONLY</option>
            </select>
          </div>
          <div class="field"><label>TTL Seconds (0 = never expires)</label><input type="number" name="ttl_seconds" min="0" value="0" /></div>
        </div>
        <div class="modal-footer"><button type="submit" class="btn btn-primary">Issue Token</button></div>
      </form>
    `, () => {
      $("form-token").addEventListener("submit", async (e) => {
        e.preventDefault();
        const fd = new FormData(e.target);
        try {
          const res = await api("POST", "/api/v1/auth/tokens", {
            name: fd.get("name"),
            role: fd.get("role"),
            ttl_seconds: Number(fd.get("ttl_seconds") || 0),
          });
          $("new-token-secret").textContent = res.secret_token || "";
          $("new-token-banner").classList.remove("hidden");
          closeModal();
          toast("Token issued");
          await refreshAll();
        } catch (err) { toast(err.message, "err"); }
      });
    });
  });

  $("form-password").addEventListener("submit", async (e) => {
    e.preventDefault();
    const fd = new FormData(e.target);
    try {
      const res = await api("POST", "/api/v1/auth/password", {
        current_password: fd.get("current_password"),
        new_password: fd.get("new_password"),
      });
      if (res.access_token) setToken(res.access_token);
      e.target.reset();
      toast("Password updated");
    } catch (err) { toast(err.message, "err"); }
  });

  $("btn-open-create-gsnap").addEventListener("click", () => {
    const volChecks = state.volumes.map((v) => `
      <label style="display:flex;align-items:center;gap:0.5rem;padding:0.25rem 0">
        <input type="checkbox" name="volume_ids" value="${escapeHtml(v.id)}" />
        <span><strong>${escapeHtml(v.name)}</strong> <span class="muted mono">(${escapeHtml(v.id)})</span></span>
      </label>
    `).join("");
    openModal("Create Volume Group Snapshot", `
      <form id="form-gsnap" style="display:flex;flex-direction:column;gap:0.85rem">
        <div class="field"><label>Group Snapshot Name</label><input name="name" required placeholder="app-consistent-snap-01" /></div>
        <div class="field">
          <label>Member Volumes</label>
          <div style="max-height:180px;overflow:auto;border:1px solid var(--border);border-radius:6px;padding:0.5rem">
            ${volChecks || `<span class="muted">No volumes available</span>`}
          </div>
        </div>
        <div class="modal-footer"><button type="submit" class="btn btn-primary" ${state.volumes.length ? "" : "disabled"}>Create Group Snapshot</button></div>
      </form>
    `, () => {
      $("form-gsnap").addEventListener("submit", async (e) => {
        e.preventDefault();
        const fd = new FormData(e.target);
        const volumeIds = fd.getAll("volume_ids").map(String);
        if (!volumeIds.length) {
          toast("Select at least one volume", "err");
          return;
        }
        try {
          await api("POST", "/api/v1/volume-group-snapshots", {
            name: fd.get("name"),
            volume_ids: volumeIds,
          });
          closeModal();
          toast("Volume group snapshot created");
          await refreshAll();
        } catch (err) { toast(err.message, "err"); }
      });
    });
  });

  $("gsnaps-tbody").addEventListener("click", async (e) => {
    const delBtn = e.target.closest("[data-gsnap-delete]");
    if (!delBtn) return;
    const id = delBtn.dataset.gsnapDelete;
    if (!confirm(`Delete volume group snapshot ${id} and its member snapshots?`)) return;
    try {
      await api("DELETE", `/api/v1/volume-group-snapshots/${encodeURIComponent(id)}`);
      toast("Volume group snapshot deleted");
      await refreshAll();
    } catch (err) { toast(err.message, "err"); }
  });

  $("btn-check-updates").addEventListener("click", async () => {
    try {
      state.containerUpdates = await api("GET", "/api/v1/system/containers/updates");
      renderLifecycle();
      toast(state.containerUpdates.updates_available ? "Container updates available" : "Containers checked");
    } catch (err) { toast(err.message, "err"); }
  });

  async function triggerContainerUpdate(unit) {
    const label = unit ? `unit ${unit}` : "all AutoUpdate containers";
    const dataPlane = unit === "velocor-spdk.service";
    const warning = dataPlane
      ? `Updating ${unit} RESTARTS spdk_tgt and interrupts ALL SAN I/O on this appliance. Only do this in a maintenance window. Continue?`
      : `Apply container updates for ${label}? Running services may briefly restart.`;
    if (!confirm(warning)) return;
    try {
      const res = await api("POST", "/api/v1/system/containers/update", { unit: unit || "", allow_disruptive: dataPlane });
      toast(res.message || "Container update scheduled");
      setTimeout(() => refreshAll(), 1500);
    } catch (err) { toast(err.message, "err"); }
  }

  $("btn-apply-updates").addEventListener("click", () => triggerContainerUpdate(""));
  $("containers-tbody").addEventListener("click", (e) => {
    const btn = e.target.closest("[data-container-update]");
    if (!btn) return;
    triggerContainerUpdate(btn.dataset.containerUpdate);
  });

  $("btn-os-upgrade").addEventListener("click", () => {
    openModal("Upgrade / Switch velocor-os (bootc)", `
      <form id="form-os-upgrade" style="display:flex;flex-direction:column;gap:0.85rem">
        <div class="field">
          <label>OCI Image Reference (leave blank for bootc upgrade)</label>
          <input name="image_ref" placeholder="ghcr.io/markd/velocor-os:latest" />
        </div>
        <div class="modal-footer"><button type="submit" class="btn btn-primary">Stage OS Upgrade</button></div>
      </form>
    `, () => {
      $("form-os-upgrade").addEventListener("submit", async (e) => {
        e.preventDefault();
        const fd = new FormData(e.target);
        try {
          const res = await api("POST", "/api/v1/system/upgrade", {
            image_ref: String(fd.get("image_ref") || "").trim(),
          });
          closeModal();
          toast(res.message || "OS upgrade staged");
          await refreshAll();
        } catch (err) { toast(err.message, "err"); }
      });
    });
  });

  $("btn-os-rollback").addEventListener("click", async () => {
    if (!confirm("Stage rollback to the previous bootc OS deployment?")) return;
    try {
      const res = await api("POST", "/api/v1/system/rollback", {});
      toast(res.message || "OS rollback staged");
      await refreshAll();
    } catch (err) { toast(err.message, "err"); }
  });

  $("btn-os-reboot").addEventListener("click", async () => {
    if (!confirm("Reboot the velocor-os appliance now? Active storage I/O will be interrupted.")) return;
    try {
      const res = await api("POST", "/api/v1/system/reboot", {});
      toast(res.message || "Appliance reboot initiated");
    } catch (err) { toast(err.message, "err"); }
  });

  $("btn-refresh-network").addEventListener("click", async () => {
    try {
      state.network = await api("GET", "/api/v1/system/network");
      renderNetwork();
      toast("Network interfaces refreshed");
    } catch (err) { toast(err.message, "err"); }
  });

  $("network-tbody").addEventListener("click", (e) => {
    const btn = e.target.closest("[data-net-config]");
    if (!btn) return;
    const ifName = btn.dataset.netConfig;
    const iface = ((state.network && state.network.interfaces) || []).find((i) => i.name === ifName);
    if (!iface) return;
    const currentMethod = (iface.method === "manual" || iface.method === "disabled") ? iface.method : "dhcp";
    const currentAddr = (iface.addresses && iface.addresses[0]) || "";
    const currentDNS = (iface.dns || []).join(", ");

    openModal(`Configure Interface: ${ifName}`, `
      <form id="form-net-config" style="display:flex;flex-direction:column;gap:0.85rem">
        <div class="muted" style="font-size:0.85rem">
          Applying changes to the management interface may disconnect your active browser session.
        </div>
        <div class="form-grid">
          <div class="field">
            <label>IPv4 Method</label>
            <select name="method" id="net-method-select">
              <option value="dhcp" ${currentMethod === "dhcp" ? "selected" : ""}>DHCP (Automatic)</option>
              <option value="manual" ${currentMethod === "manual" ? "selected" : ""}>Static / Manual</option>
              <option value="disabled" ${currentMethod === "disabled" ? "selected" : ""}>Disabled</option>
            </select>
          </div>
          <div class="field">
            <label>MTU (576–9216, e.g. 9000 for SAN jumbo frames)</label>
            <input type="number" name="mtu" min="576" max="9216" value="${escapeHtml(iface.mtu || 1500)}" />
          </div>
        </div>
        <div class="field">
          <label>IPv4 Address / CIDR (required for Static)</label>
          <input name="address" id="net-address-input" value="${escapeHtml(currentAddr)}" placeholder="10.10.10.20/24" />
        </div>
        <div class="form-grid">
          <div class="field">
            <label>Gateway (optional)</label>
            <input name="gateway" id="net-gateway-input" value="${escapeHtml(iface.gateway || "")}" placeholder="10.10.10.1" />
          </div>
          <div class="field">
            <label>DNS Servers (comma-separated, optional)</label>
            <input name="dns" id="net-dns-input" value="${escapeHtml(currentDNS)}" placeholder="1.1.1.1, 8.8.8.8" />
          </div>
        </div>
        <div class="modal-footer"><button type="submit" class="btn btn-primary">Apply Interface Config</button></div>
      </form>
    `, () => {
      const toggleFields = () => {
        const m = $("net-method-select").value;
        const isManual = m === "manual";
        $("net-address-input").disabled = !isManual;
        $("net-gateway-input").disabled = !isManual;
        $("net-dns-input").disabled = m === "disabled";
      };
      $("net-method-select").addEventListener("change", toggleFields);
      toggleFields();

      $("form-net-config").addEventListener("submit", async (e) => {
        e.preventDefault();
        const fd = new FormData(e.target);
        try {
          const res = await api("PUT", `/api/v1/system/network/interfaces/${encodeURIComponent(ifName)}`, {
            method: String(fd.get("method") || "dhcp"),
            address: String(fd.get("address") || "").trim(),
            gateway: String(fd.get("gateway") || "").trim(),
            dns: String(fd.get("dns") || "").trim(),
            mtu: Number(fd.get("mtu") || 0),
          });
          if (res.network) state.network = res.network;
          closeModal();
          toast(res.message || `Interface ${ifName} updated`);
          renderNetwork();
        } catch (err) { toast(err.message, "err"); }
      });
    });
  });

  // Live polling loop
  state.pollTimer = setInterval(() => {
    if ($("auto-refresh").checked && getToken() && $("modal-overlay").classList.contains("hidden")) {
      refreshAll();
    }
  }, 3000);

  if (getToken()) {
    hideLogin();
    refreshAll();
  } else {
    showLogin();
  }
})();
