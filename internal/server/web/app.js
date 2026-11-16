// The Velocor console: screens over the public REST API, with no dependencies and no build step.
// Everything from the API reaches the page through textContent, by way of el(), so a name can't become markup.

const REFRESH_MS = 10000;
const TOKEN_KEY = "velocor-token";
const API = "/api/v1";

// --- The API ---

class Unauthorized extends Error {}
class MustChangePassword extends Error {}

let memoryToken = "";

function token() {
  try { return sessionStorage.getItem(TOKEN_KEY) || memoryToken; } catch { return memoryToken; }
}

function saveToken(value) {
  memoryToken = value;
  try {
    if (value) sessionStorage.setItem(TOKEN_KEY, value);
    else sessionStorage.removeItem(TOKEN_KEY);
  } catch { /* A private window can refuse this: the token then lasts until the page is reloaded. */ }
}

async function request(method, path, body) {
  const headers = { Accept: "application/json" };
  const t = token();
  if (t) headers.Authorization = "Bearer " + t;
  if (body !== undefined) headers["Content-Type"] = "application/json";
  const resp = await fetch(API + path, { method, headers, cache: "no-store", body: body === undefined ? undefined : JSON.stringify(body) });
  if (resp.status === 401) throw new Unauthorized();
  const answer = await resp.json().catch(() => ({}));
  if (resp.status === 403 && /initial password must be changed/.test(answer.error || "")) throw new MustChangePassword();
  if (!resp.ok) throw new Error(answer.error || `The API answered with status ${resp.status}`);
  return answer;
}

const api = (path) => request("GET", path);
const send = (method, path, body) => request(method, path, body);
// list returns the array the API wraps under key.
const list = async (path, key) => (await api(path))[key] || [];

// --- Building the page ---

// el makes an element. Strings become text, never markup.
function el(tag, attrs, ...children) {
  const node = document.createElement(tag);
  for (const [key, value] of Object.entries(attrs || {})) {
    if (value === undefined || value === null || value === false) continue;
    if (key === "class") node.className = value;
    else if (key.startsWith("on")) node.addEventListener(key.slice(2), value);
    else node.setAttribute(key, value === true ? "" : String(value));
  }
  for (const child of children.flat(Infinity)) {
    if (child === undefined || child === null || child === false) continue;
    node.append(child instanceof Node ? child : document.createTextNode(String(child)));
  }
  return node;
}

function humanSize(bytes) {
  if (bytes === undefined || bytes === null) return "-";
  const units = [["PiB", 2 ** 50], ["TiB", 2 ** 40], ["GiB", 2 ** 30], ["MiB", 2 ** 20], ["KiB", 2 ** 10]];
  for (const [name, size] of units) {
    if (bytes >= size) {
      const value = bytes / size;
      return (Number.isInteger(value) ? value : value.toFixed(1)) + " " + name;
    }
  }
  return bytes + " B";
}

function humanTime(iso) {
  if (!iso || iso.startsWith("0001-")) return "-";
  const when = new Date(iso);
  return Number.isNaN(when.getTime()) ? iso : when.toLocaleString();
}

function humanDuration(seconds) {
  const s = Number(seconds || 0);
  const d = Math.floor(s / 86400), h = Math.floor((s % 86400) / 3600), m = Math.floor((s % 3600) / 60);
  return d ? `${d}d ${h}h` : h ? `${h}h ${m}m` : `${m}m`;
}

const pill = (text, kind) => el("span", { class: "pill " + (kind || "") }, text);
const healthKind = (h) => ({ ONLINE: "ok", DEGRADED: "warn", RECONCILING: "warn", FAULTED: "bad", OFFLINE: "bad" }[h] || "quiet");
const health = (x) => [pill((x.health_status || "UNKNOWN").toLowerCase(), healthKind(x.health_status)), x.status_message && el("span", { class: "sub" }, x.status_message)];
const hash = (...parts) => "#/" + parts.map(encodeURIComponent).join("/");
const link = (text, ...parts) => el("a", { href: hash(...parts) }, text);
const mono = (text) => el("span", { class: "mono" }, text);
const labelsText = (labels) => Object.entries(labels || {}).map(([k, v]) => `${k}=${v}`).join(", ");
const protoName = (p) => ({ NVMEOF_TCP: "NVMe/TCP", NVMEOF_RDMA: "NVMe/RDMA", ISCSI: "iSCSI" }[p] || p);
const byId = (items) => Object.fromEntries(items.map((x) => [x.id, x]));

function bar(used, total) {
  const percent = total ? (100 * used) / total : 0;
  const kind = percent >= 95 ? "bad" : percent >= 80 ? "warn" : "";
  const fill = el("div");
  fill.style.width = Math.min(percent, 100) + "%";
  return el("div", {},
    `${humanSize(used)} of ${humanSize(total)} (${percent < 1 && percent > 0 ? "under 1" : percent.toFixed(0)}%)`,
    el("div", { class: "bar " + kind, role: "img", "aria-label": `${percent.toFixed(0)} percent used` }, fill));
}

const poolBar = (p) => (p.total_bytes ? bar(p.allocated_bytes || (p.total_bytes - p.free_bytes), p.total_bytes) : el("span", { class: "muted" }, "unknown"));

const PAGE_SIZES = [10, 25, 50, 100, 250];
const PAGE_SIZE_KEY = "velocor-page-size";
let fallbackPageSize = 0;

function pageSize() {
  try {
    const saved = Number(localStorage.getItem(PAGE_SIZE_KEY));
    if (PAGE_SIZES.includes(saved)) return saved;
  } catch { /* Fall through to the default. */ }
  return fallbackPageSize || PAGE_SIZES[0];
}

function savePageSize(size) {
  try { localStorage.setItem(PAGE_SIZE_KEY, String(size)); } catch { /* It then lasts until the page is reloaded. */ }
  fallbackPageSize = size;
}

// table draws rows a page at a time, with a filter box.
// A column is { title, cell(row), num, text(row) }, where text says what the filter searches.
function table(columns, rows, { empty, filter = rows.length > 5, state }) {
  const wrap = el("div", {});
  const view = state || { text: "", page: 0 };
  const haystack = (row) => columns.map((c) => (c.text ? c.text(row) : "")).join(" ").toLowerCase();
  const body = el("div", {});
  const count = el("span", { class: "count" });
  function draw() {
    const needle = view.text.trim().toLowerCase();
    const matching = needle ? rows.filter((row) => haystack(row).includes(needle)) : rows;
    count.textContent = needle ? `${matching.length} of ${rows.length}` : `${rows.length}`;
    if (!rows.length) { body.replaceChildren(el("div", { class: "table-wrap" }, el("div", { class: "empty" }, empty))); return; }
    if (!matching.length) { body.replaceChildren(el("div", { class: "table-wrap" }, el("div", { class: "empty" }, "Nothing matches that."))); return; }
    const size = pageSize();
    const pages = Math.max(1, Math.ceil(matching.length / size));
    view.page = Math.min(view.page || 0, pages - 1);
    const start = view.page * size;
    const shown = matching.slice(start, start + size);
    body.replaceChildren(el("div", { class: "table-wrap" }, el("table", {},
      el("thead", {}, el("tr", {}, columns.map((c) => el("th", { scope: "col", class: c.num ? "num" : "" }, c.title)))),
      el("tbody", {}, shown.map((row) => el("tr", {}, columns.map((c) => el("td", { class: c.num ? "num" : "" }, c.cell(row)))))))));
    if (matching.length > PAGE_SIZES[0]) {
      body.append(el("div", { class: "pager" },
        el("label", {}, "Rows ", el("select", { onchange: (e) => {
          const first = view.page * size;
          savePageSize(Number(e.target.value));
          view.page = Math.floor(first / Number(e.target.value));
          draw();
        } }, PAGE_SIZES.map((n) => el("option", { value: n, selected: n === size }, n)))),
        el("span", { class: "count" }, `${start + 1}–${start + shown.length} of ${matching.length}`),
        el("button", { disabled: view.page === 0, onclick: () => { view.page--; draw(); } }, "Previous"),
        el("button", { disabled: view.page === pages - 1, onclick: () => { view.page++; draw(); } }, "Next")));
    }
  }
  if (filter) {
    wrap.append(el("div", { class: "toolbar" },
      el("input", { type: "search", placeholder: "Filter", "aria-label": "Filter", value: view.text,
        oninput: (e) => { view.text = e.target.value; view.page = 0; draw(); } }), count));
  }
  wrap.append(body);
  draw();
  return wrap;
}

function fact(name, value) {
  return [el("dt", {}, name), el("dd", {}, value === undefined || value === null || value === "" ? "-" : value)];
}

// --- Dialogs ---

const SIZE_UNITS = [["MiB", 2 ** 20], ["GiB", 2 ** 30], ["TiB", 2 ** 40]];

function notify(message) {
  const old = document.getElementById("toast");
  if (old) old.remove();
  const toast = el("div", { id: "toast", class: "toast", role: "status" }, message);
  document.body.append(toast);
  setTimeout(() => toast.remove(), 5000);
}

function sizeField(field) {
  const [unitName, unitSize] = [...SIZE_UNITS].reverse().find(([, size]) => field.value && field.value % size === 0) || SIZE_UNITS[1];
  const number = el("input", { type: "number", id: "field-" + field.name, min: 1, step: "any", required: field.required, value: field.value ? field.value / unitSize : undefined });
  const unit = el("select", { "aria-label": `${field.label} unit` }, SIZE_UNITS.map(([name]) => el("option", { value: name, selected: name === unitName }, name)));
  return {
    node: el("div", { class: "size" }, number, unit),
    focus: number,
    value: () => (number.value === "" ? undefined : Math.round(Number(number.value) * SIZE_UNITS.find(([name]) => name === unit.value)[1])),
  };
}

function checksField(field) {
  const boxes = field.options.map((o) => el("input", { type: "checkbox", value: o.value, checked: (field.value || []).includes(o.value) }));
  return {
    node: el("div", { class: "checks", id: "field-" + field.name, role: "group", "aria-label": field.label },
      field.options.length ? boxes.map((box, i) => el("label", { class: "check" }, box, el("span", {}, field.options[i].label))) : el("p", { class: "hint" }, field.empty)),
    focus: boxes[0],
    value: () => boxes.filter((b) => b.checked).map((b) => b.value),
  };
}

function tickField(field) {
  const box = el("input", { type: "checkbox", id: "field-" + field.name, checked: field.value });
  return { node: el("label", { class: "check" }, box, el("span", {}, field.text)), focus: box, value: () => box.checked, noLabel: true };
}

function radioField(field) {
  const radios = field.options.map((o) => el("input", { type: "radio", name: "field-" + field.name, value: o.value, checked: o.value === field.value, disabled: o.disabled }));
  return {
    node: el("div", { id: "field-" + field.name, role: "radiogroup", "aria-label": field.label },
      radios.map((r, i) => el("label", { class: "check" }, r, el("span", {}, field.options[i].label)))),
    focus: radios.find((r) => r.checked) || radios[0],
    value: () => (radios.find((r) => r.checked) || {}).value,
  };
}

// A list typed one per line (or comma separated).
function linesField(field) {
  const area = el("textarea", { id: "field-" + field.name, placeholder: field.placeholder, spellcheck: "false" }, (field.value || []).join("\n"));
  return { node: area, focus: area, value: () => area.value.split(/[\n,]/).map((x) => x.trim()).filter(Boolean) };
}

function plainField(field) {
  const attrs = { id: "field-" + field.name, required: field.required, placeholder: field.placeholder, autocomplete: "off", spellcheck: "false" };
  const input = field.options
    ? el("select", attrs, field.options.map((o) => el("option", { value: o.value, selected: o.value === field.value }, o.label)))
    : el("input", { ...attrs, type: field.secret ? "password" : field.number ? "number" : "text", value: field.value, autocomplete: field.secret ? "new-password" : "off", list: field.suggestions ? `suggest-${field.name}` : undefined });
  const dl = field.suggestions && el("datalist", { id: `suggest-${field.name}` }, field.suggestions.map((value) => el("option", { value })));
  return { node: dl ? el("div", {}, input, dl) : input, focus: input, value: () => input.value.trim() };
}

// openDialog asks for something, and stays open showing the API's answer if it's refused, with what was typed still there.
// Without submit it only shows something, with a Close button.
function openDialog({ title, intro, warning, fields = [], submit, danger, confirmText, action, done }) {
  const makers = { checks: checksField, tick: tickField, radio: radioField, lines: linesField };
  const built = fields.map((field) => ({ field, ...(field.size ? sizeField(field) : (makers[field.kind] || plainField)(field)) }));
  const notice = el("div", {});
  const go = submit && el("button", { type: "submit", class: danger ? "danger" : "primary", id: "dialog-submit" }, submit);
  const typed = confirmText && el("input", { type: "text", id: "field-confirm", autocomplete: "off", spellcheck: "false", required: true });
  if (typed) {
    go.disabled = true;
    typed.addEventListener("input", () => { go.disabled = typed.value !== confirmText; });
  }
  let working = false;
  const dialog = el("dialog", {}, el("form", { method: "dialog", onsubmit: async (e) => {
    e.preventDefault();
    if (working || !go) return;
    working = true;
    go.disabled = true;
    const label = go.textContent;
    go.textContent = "Working...";
    notice.replaceChildren();
    try {
      const result = await action(currentValues());
      working = false;
      dialog.close();
      if (done) done(result);
    } catch (err) {
      working = false;
      if (err instanceof Unauthorized || err instanceof MustChangePassword) { dialog.close(); rerender(); return; }
      notice.replaceChildren(el("div", { class: "panel-notice bad", role: "alert" }, err.message));
      go.textContent = label;
      go.disabled = false;
    }
  } },
    el("h2", {}, title),
    intro && el("p", {}, intro),
    warning,
    notice,
    built.map((b) => {
      b.wrap = el("div", { class: "field" },
        !b.noLabel && el("label", { for: "field-" + b.field.name }, b.field.label),
        b.node,
        b.field.hint && el("p", { class: "hint muted" }, b.field.hint));
      return b.wrap;
    }),
    typed && [el("label", { for: "field-confirm" }, ["Type ", el("code", {}, confirmText), " to confirm"]), typed],
    el("div", { class: "dialog-actions" },
      el("button", { type: "button", id: "dialog-cancel", onclick: () => { if (!working) dialog.close(); } }, go ? "Cancel" : "Close"),
      go)));
  const currentValues = () => Object.fromEntries(built.map((b) => [b.field.name, b.value()]));
  const applyVisibility = () => {
    const values = currentValues();
    for (const b of built) {
      const show = !b.field.when || b.field.when(values);
      b.wrap.hidden = !show;
      if (b.field.required && b.focus) b.focus.required = show;
    }
  };
  dialog.addEventListener("change", applyVisibility);
  applyVisibility();
  dialog.addEventListener("cancel", (e) => { if (working) e.preventDefault(); });
  dialog.addEventListener("close", () => dialog.remove());
  // Outside main, so that a refresh underneath doesn't lose what's being typed.
  document.body.append(dialog);
  dialog.showModal();
  (built[0] ? built[0].focus : typed || go || dialog.querySelector("button")).focus();
}

const confirmDialog = (opts) => openDialog({ danger: true, ...opts });

// --- Who's signed in, and what they may do ---

let me = { username: "", role: "" };
const RANK = { READONLY: 1, CSI_PROVISIONER: 2, ADMIN: 3 };
const can = {
  get provision() { return (RANK[me.role] || 0) >= 2; },
  get admin() { return me.role === "ADMIN"; },
};

// A button only for those whose role allows the action. Others don't see it.
const action = (allowed, attrs, text) => (allowed ? el("button", attrs, text) : null);

// --- Disks ---

function diskState(d, poolsById) {
  if (d.is_boot_device) return pill("Protected", "quiet");
  if (d.pool_id) return [pill("In pool", "ok"), el("span", { class: "sub" }, link((poolsById[d.pool_id] || {}).name || d.pool_id, "pool", d.pool_id))];
  if (d.claimed) return pill("Claimed", "ok");
  return pill("Available", "quiet");
}

function claimDialog(d) {
  openDialog({
    title: `Claim ${d.name || d.id}`,
    intro: d.type === "NVME"
      ? "The drive is taken from the Linux nvme driver and handed to SPDK, which polls it directly. The host can no longer see it."
      : "SPDK opens the disk through io_uring. Nothing else on the host should use it afterwards.",
    submit: "Claim",
    action: async () => {
      try {
        return await send("POST", `/disks/${encodeURIComponent(d.id)}:claim`);
      } catch (err) {
        // A disk that holds data is refused; claiming it anyway destroys that data, so it asks again, harder.
        if (/force=true/.test(err.message)) return { needsForce: err.message };
        throw err;
      }
    },
    done: (r) => {
      if (!r.needsForce) { notify(`Claimed ${d.name || d.id}.`); rerender(); return; }
      confirmDialog({
        title: `Destroy the data on ${d.name || d.id}?`,
        warning: el("div", { class: "panel-notice bad" }, r.needsForce),
        intro: "Claiming it anyway gives the whole disk to SPDK, and everything on it is lost.",
        submit: "Claim and destroy data",
        confirmText: d.name || d.id,
        action: () => send("POST", `/disks/${encodeURIComponent(d.id)}:claim?force=true`),
        done: () => { notify(`Claimed ${d.name || d.id}.`); rerender(); },
      });
    },
  });
}

function releaseDialog(d) {
  openDialog({
    title: `Release ${d.name || d.id}`,
    intro: "SPDK lets go of it and it's handed back to the Linux kernel.",
    submit: "Release",
    action: () => send("POST", `/disks/${encodeURIComponent(d.id)}:release`),
    done: () => { notify(`Released ${d.name || d.id}.`); rerender(); },
  });
}

function smartCell(d) {
  if (d.type !== "NVME" || !d.claimed) return "-";
  const s = d.smart || {};
  return [`${s.temperature_celsius ?? "-"} °C`, el("span", { class: "sub" }, `${s.percentage_used ?? 0}% worn, ${s.media_errors ?? 0} media errors`),
    s.critical_warning && pill("Critical warning", "bad")];
}

async function disks() {
  const [items, poolList] = await Promise.all([list("/disks", "disks"), list("/pools", "pools")]);
  const poolsById = byId(poolList);
  return [
    el("h1", {}, "Disks"),
    el("div", { class: "toolbar" },
      action(can.admin, { class: "primary", id: "scan-disks", onclick: async () => {
        try { await send("POST", "/disks:scan"); notify("Scanned for disks."); rerender(); } catch (err) { notify(err.message); }
      } }, "Scan for disks"),
      el("span", { class: "muted" }, "Disks that hold the OS, are mounted, or are used as swap are protected and can't be claimed.")),
    table([
      { title: "Disk", cell: (d) => [el("strong", {}, d.name || d.id), d.name && d.name !== d.id && el("span", { class: "sub mono" }, d.id)], text: (d) => `${d.name} ${d.id} ${d.model} ${d.serial_number}` },
      { title: "Type", cell: (d) => d.type, text: (d) => d.type },
      { title: "Model", cell: (d) => [d.model || "-", el("span", { class: "sub mono" }, d.serial_number)] },
      { title: "Size", num: true, cell: (d) => humanSize(d.size_bytes) },
      { title: "Where", cell: (d) => mono(d.pci_address || d.device_path || "-"), text: (d) => `${d.pci_address || ""} ${d.device_path || ""}` },
      { title: "State", cell: (d) => diskState(d, poolsById), text: (d) => (d.is_boot_device ? "protected" : d.claimed ? "claimed" : "available") },
      { title: "Health", cell: (d) => (d.claimed ? health(d) : "-") },
      { title: "SMART", cell: smartCell },
      { title: "", cell: (d) => el("div", { class: "row-actions" },
        can.admin && !d.claimed && !d.is_boot_device && el("button", { onclick: () => claimDialog(d) }, "Claim"),
        can.admin && d.claimed && !d.pool_id && el("button", { class: "danger-quiet", onclick: () => releaseDialog(d) }, "Release")) },
    ], items, { empty: "No disks found. Scan for disks.", state: stateOf("disks") }),
  ];
}

// --- Pools ---

const RAID_LEVELS = [
  { value: "RAID0", label: "RAID 0: striped, no redundancy" },
  { value: "RAID1", label: "RAID 1: mirrored (2 or more disks)" },
  { value: "RAID10", label: "RAID 10: striped mirrors (4 or more, even)" },
  { value: "RAID5F", label: "RAID 5F: parity, full-stripe writes (3 or more)" },
];

async function createPoolDialog() {
  const free = (await list("/disks", "disks")).filter((d) => d.claimed && !d.pool_id && !d.is_boot_device);
  openDialog({
    title: "Create a pool",
    intro: "This builds an SPDK RAID across the disks, with its layout recorded on them so it reassembles after a restart, and a logical volume store on top.",
    submit: "Create",
    fields: [
      { name: "name", label: "Name", required: true },
      { name: "level", kind: "radio", label: "RAID level", value: "RAID1", options: RAID_LEVELS },
      { name: "disks", kind: "checks", label: "Disks", options: free.map((d) => ({ value: d.id, label: `${d.name || d.id} (${d.type}, ${humanSize(d.size_bytes)})` })), empty: "No claimed disks are free. Claim some on the Disks screen first." },
      { name: "strip", label: "Strip size", when: (v) => v.level !== "RAID1", value: "64", options: ["16", "32", "64", "128", "256"].map((k) => ({ value: k, label: `${k} KiB` })) },
      { name: "cluster", label: "Cluster size", value: String(64 * 1024), hint: "The unit of thin allocation and of changed-block tracking.",
        options: [[64 * 1024, "64 KiB"], [1 << 20, "1 MiB"], [4 << 20, "4 MiB"]].map(([v, l]) => ({ value: String(v), label: l })) },
    ],
    action: (v) => send("POST", "/pools", { name: v.name, raid_level: v.level, disk_ids: v.disks, strip_size_kb: Number(v.strip), cluster_size_bytes: Number(v.cluster) }),
    done: (r) => { notify(`Created ${r.pool.name}.`); location.hash = hash("pool", r.pool.id); },
  });
}

function deletePoolDialog(p) {
  confirmDialog({
    title: `Delete pool ${p.name}`,
    intro: "This destroys its RAID and volume store. A pool with volumes or snapshots is refused.",
    submit: "Delete pool",
    confirmText: p.name,
    action: () => send("DELETE", "/pools/" + encodeURIComponent(p.id)),
    done: () => { notify(`Deleted ${p.name}.`); location.hash = "#/pools"; },
  });
}

function poolTable(items, volumes, options = {}) {
  const count = (id) => volumes.filter((v) => v.pool_id === id).length;
  return table([
    { title: "Name", cell: (p) => el("strong", {}, link(p.name, "pool", p.id)), text: (p) => p.name },
    { title: "Health", cell: health, text: (p) => p.health_status },
    { title: "RAID", cell: (p) => [p.raid_level, el("span", { class: "sub" }, `${(p.disk_ids || []).length} disks`)], text: (p) => p.raid_level },
    { title: "Space", cell: poolBar },
    { title: "Volumes", num: true, cell: (p) => link(String(count(p.id)), "volumes", p.id) },
  ], items, { empty: "No pools yet.", state: stateOf("pools"), ...options });
}

async function pools() {
  const [items, volumes] = await Promise.all([list("/pools", "pools"), list("/volumes", "volumes")]);
  return [
    el("h1", {}, "Pools"),
    el("div", { class: "toolbar" }, action(can.admin, { class: "primary", id: "create-pool", onclick: createPoolDialog }, "Create pool")),
    poolTable(items, volumes),
  ];
}

async function pool(id) {
  const [p, disksAll, volumes] = await Promise.all([
    api("/pools/" + encodeURIComponent(id)).then((r) => r.pool), list("/disks", "disks"), list("/volumes?pool_id=" + encodeURIComponent(id), "volumes"),
  ]);
  const members = (p.disk_ids || []).map((did) => disksAll.find((d) => d.id === did) || { id: did, name: did, health_status: "OFFLINE", status_message: "not found" });
  return [
    el("p", { class: "crumb" }, el("a", { href: "#/pools" }, "Pools")),
    el("h1", {}, p.name),
    el("div", { class: "toolbar" },
      action(can.provision, { class: "primary", onclick: () => createVolumeDialog(p.id) }, "Create volume"),
      action(can.admin, { class: "danger-quiet", id: "delete-pool", disabled: volumes.length > 0, onclick: () => deletePoolDialog(p) }, "Delete"),
      volumes.length > 0 && can.admin && el("span", { class: "muted" }, "It can't be deleted while it has volumes.")),
    el("dl", { class: "facts" },
      fact("Health", health(p)),
      fact("Space", poolBar(p)),
      fact("RAID", `${p.raid_level}${p.raid_level !== "RAID1" ? `, ${p.strip_size_kb} KiB strips` : ""}`),
      fact("Cluster size", humanSize(p.cluster_size_bytes)),
      fact("RAID bdev", mono(p.base_bdev_name)),
      fact("Volume store", [mono(p.lvs_name), el("span", { class: "sub mono" }, p.lvs_uuid)]),
      fact("Created", humanTime(p.created_at)),
      fact("ID", mono(p.id))),
    el("h2", {}, "Disks"),
    table([
      { title: "Disk", cell: (d) => [el("strong", {}, d.name || d.id), el("span", { class: "sub mono" }, d.id)] },
      { title: "Model", cell: (d) => d.model || "-" },
      { title: "Size", num: true, cell: (d) => humanSize(d.size_bytes) },
      { title: "SPDK bdev", cell: (d) => mono(d.spdk_bdev_name || "-") },
      { title: "Health", cell: health },
    ], members, { filter: false, empty: "No disks.", state: stateOf("pool-disks") }),
    el("h2", {}, "Volumes"),
    volumeTable(volumes, [p]),
  ];
}

// --- Volumes ---

async function createVolumeDialog(poolId) {
  const items = (await list("/pools", "pools")).filter((p) => p.health_status !== "FAULTED");
  openDialog({
    title: "Create a volume",
    submit: "Create",
    fields: [
      { name: "name", label: "Name", required: true },
      ...(items.length > 1 || !poolId ? [{ name: "pool", label: "Pool", value: poolId || (items[0] || {}).id, options: items.map((p) => ({ value: p.id, label: `${p.name} (${humanSize(p.free_bytes)} free)` })) }] : []),
      { name: "size", label: "Size", size: true, required: true, value: 10 * 2 ** 30 },
      { name: "thin", kind: "tick", value: true, text: "Thin provisioned: space is only allocated as it's written" },
    ],
    action: (v) => send("POST", "/volumes", { name: v.name, pool_id: v.pool || poolId, size_bytes: v.size, thin_provision: v.thin }),
    done: (r) => { notify(`Created ${r.volume.name}.`); location.hash = hash("volume", r.volume.id); },
  });
}

function growDialog(v) {
  openDialog({
    title: `Grow ${v.name}`,
    intro: `It's ${humanSize(v.size_bytes)} now. It can only get larger. Hosts see the new size after a rescan, which the CSI driver does for you.`,
    submit: "Grow",
    fields: [{ name: "size", label: "New size", size: true, required: true, value: v.size_bytes }],
    action: (x) => send("POST", `/volumes/${encodeURIComponent(v.id)}:resize`, { new_size_bytes: x.size }),
    done: (r) => { notify(`${v.name} is now ${humanSize(r.volume.size_bytes)}.`); rerender(); },
  });
}

function qosDialog(v) {
  const q = v.qos_policy || {};
  const n = (x) => (x ? String(x) : "");
  openDialog({
    title: `Limits for ${v.name}`,
    intro: "Leave a limit empty, or 0, for none. They apply at once.",
    submit: "Save",
    fields: [
      { name: "iops", label: "Read + write IOPS", number: true, value: n(q.rw_ios_per_sec) },
      { name: "rw", label: "Read + write MB/s", number: true, value: n(q.rw_mbytes_per_sec) },
      { name: "r", label: "Read MB/s", number: true, value: n(q.r_mbytes_per_sec) },
      { name: "w", label: "Write MB/s", number: true, value: n(q.w_mbytes_per_sec) },
    ],
    action: (x) => send("PUT", `/volumes/${encodeURIComponent(v.id)}/qos`, { qos_policy: {
      rw_ios_per_sec: Number(x.iops) || 0, rw_mbytes_per_sec: Number(x.rw) || 0, r_mbytes_per_sec: Number(x.r) || 0, w_mbytes_per_sec: Number(x.w) || 0,
    } }),
    done: () => { notify("Limits saved."); rerender(); },
  });
}

function snapshotDialog(v) {
  openDialog({
    title: `Take a snapshot of ${v.name}`,
    intro: "It's instant and crash-consistent: as if the power had been cut at that moment.",
    submit: "Take snapshot",
    fields: [{ name: "name", label: "Name", required: true, value: `${v.name}-${new Date().toISOString().slice(0, 10)}` }],
    action: (x) => send("POST", `/volumes/${encodeURIComponent(v.id)}/snapshots`, { name: x.name }),
    done: (r) => { notify(`Took snapshot ${r.snapshot.name}.`); rerender(); },
  });
}

function cloneDialog(s) {
  openDialog({
    title: `Clone ${s.name}`,
    intro: "This makes a new, writable volume from the snapshot. It shares unchanged data with it, so it's instant.",
    submit: "Clone",
    fields: [{ name: "name", label: "New volume's name", required: true, value: `${s.name}-clone` }],
    action: (x) => send("POST", `/snapshots/${encodeURIComponent(s.id)}:clone`, { clone_name: x.name }),
    done: (r) => { notify(`Created ${r.volume.name}.`); location.hash = hash("volume", r.volume.id); },
  });
}

function deleteVolumeDialog(v) {
  confirmDialog({
    title: `Delete ${v.name}`,
    intro: "This destroys its data, and can't be undone. Its snapshots are kept, and can be cloned later.",
    submit: "Delete volume",
    confirmText: v.name,
    action: () => send("DELETE", "/volumes/" + encodeURIComponent(v.id)),
    done: () => { notify(`Deleted ${v.name}.`); location.hash = "#/volumes"; },
  });
}

function changedBlocksDialog(snap, older) {
  openDialog({
    title: `What changed up to ${snap.name}`,
    intro: "Changed-block tracking lists the regions written between two snapshots of a volume, which is what an incremental backup copies.",
    submit: "Show",
    fields: [{ name: "base", label: "Since snapshot", value: (older[older.length - 1] || {}).id, options: older.map((s) => ({ value: s.id, label: `${s.name} (${humanTime(s.created_at)})` })) }],
    action: (x) => api(`/snapshots/${encodeURIComponent(snap.id)}/blocks:changed?base_snapshot_id=${encodeURIComponent(x.base)}`),
    done: (r) => {
      const extents = r.extents || [];
      const total = extents.reduce((sum, e) => sum + e.length_bytes, 0);
      openDialog({
        title: "Changed blocks",
        intro: `${extents.length} regions, ${humanSize(total)} in all, of a ${humanSize(r.volume_size_bytes)} volume.`,
        warning: table([
          { title: "Offset", num: true, cell: (e) => humanSize(e.offset_bytes) },
          { title: "Length", num: true, cell: (e) => humanSize(e.length_bytes) },
        ], extents, { filter: false, empty: "Nothing changed." }),
      });
    },
  });
}

function volumeTable(items, poolList, options = {}) {
  const poolsById = byId(poolList);
  return table([
    { title: "Name", cell: (v) => [el("strong", {}, link(v.name, "volume", v.id)), v.parent_snapshot_id && el("span", { class: "sub" }, "clone of a snapshot")], text: (v) => `${v.name} ${v.id} ${labelsText(v.labels)}` },
    { title: "Health", cell: health, text: (v) => v.health_status },
    { title: "Pool", cell: (v) => (poolsById[v.pool_id] ? link(poolsById[v.pool_id].name, "pool", v.pool_id) : mono(v.pool_id)), text: (v) => (poolsById[v.pool_id] || {}).name || "" },
    { title: "Size", num: true, cell: (v) => [humanSize(v.size_bytes), el("span", { class: "sub" }, v.thin_provision ? "thin" : "thick")] },
    { title: "Exported", cell: (v) => (v.attachment_state === "BLOCK_EXPORT" ? pill("Exported", "ok") : pill("Not exported", "quiet")), text: (v) => v.attachment_state },
    { title: "Labels", cell: (v) => labelsText(v.labels) || "-" },
  ], items, { empty: "No volumes yet.", state: stateOf("volumes-table"), ...options });
}

function poolPicker(poolList, current) {
  if (poolList.length < 2) return null;
  return el("label", { class: "picker" }, "Pool ",
    el("select", { onchange: (e) => { location.hash = e.target.value ? hash("volumes", e.target.value) : "#/volumes"; } },
      el("option", { value: "", selected: !current }, "All pools"),
      poolList.map((p) => el("option", { value: p.id, selected: p.id === current }, p.name))));
}

async function volumes(poolId) {
  const [items, poolList] = await Promise.all([list("/volumes" + (poolId ? "?pool_id=" + encodeURIComponent(poolId) : ""), "volumes"), list("/pools", "pools")]);
  const exported = items.filter((v) => v.attachment_state === "BLOCK_EXPORT").length;
  return [
    el("h1", {}, poolId ? `Volumes in ${(poolList.find((p) => p.id === poolId) || {}).name || poolId}` : "Volumes"),
    el("div", { class: "toolbar" }, poolPicker(poolList, poolId),
      poolList.length
        ? action(can.provision, { class: "primary", id: "create-volume", onclick: () => createVolumeDialog(poolId) }, "Create volume")
        : el("span", { class: "muted" }, ["Create a pool to make volumes: see ", el("a", { href: "#/pools" }, "Pools")])),
    volumeTable(items, poolList),
    items.length > 0 && el("p", { class: "count" }, `${exported} of ${items.length} are exported.`),
  ];
}

async function volume(id) {
  const path = "/volumes/" + encodeURIComponent(id);
  const [v, usage, snaps, exps, poolList, groups, vgs] = await Promise.all([
    api(path).then((r) => r.volume),
    api(path + "/usage").catch((err) => { if (err instanceof Unauthorized) throw err; return null; }),
    list("/snapshots?volume_id=" + encodeURIComponent(id), "snapshots"),
    list("/exports?volume_id=" + encodeURIComponent(id), "exports"),
    list("/pools", "pools"),
    list("/initiator-groups", "initiator_groups"),
    list("/volume-group-snapshots", "volume_group_snapshots"),
  ]);
  const p = poolList.find((x) => x.id === v.pool_id);
  const q = v.qos_policy || {};
  const qos = [q.rw_ios_per_sec && `${q.rw_ios_per_sec} IOPS`, q.rw_mbytes_per_sec && `${q.rw_mbytes_per_sec} MB/s`,
    q.r_mbytes_per_sec && `${q.r_mbytes_per_sec} MB/s read`, q.w_mbytes_per_sec && `${q.w_mbytes_per_sec} MB/s write`].filter(Boolean).join(", ");
  const groupOf = groupBySnapshot(vgs);
  return [
    el("p", { class: "crumb" }, el("a", { href: "#/volumes" }, "Volumes")),
    el("h1", {}, v.name),
    el("div", { class: "toolbar" },
      action(can.provision, { class: "primary", id: "export-volume", onclick: () => exportDialog(v) }, "Export"),
      action(can.provision, { id: "take-snapshot", onclick: () => snapshotDialog(v) }, "Take snapshot"),
      action(can.provision, { id: "grow-volume", onclick: () => growDialog(v) }, "Grow"),
      action(can.provision, { id: "qos-volume", onclick: () => qosDialog(v) }, "Limits"),
      action(can.provision, { class: "danger-quiet", id: "delete-volume", disabled: exps.length > 0, onclick: () => deleteVolumeDialog(v) }, "Delete"),
      exps.length > 0 && can.provision && el("span", { class: "muted" }, "It can't be deleted while it's exported.")),
    el("dl", { class: "facts" },
      fact("Health", health(v)),
      fact("Space", usage ? [bar(usage.allocated_bytes, usage.size_bytes), el("span", { class: "sub" }, "Includes what it shares with its snapshots.")] : el("span", { class: "muted" }, "SPDK can't say at the moment.")),
      fact("Pool", p ? link(p.name, "pool", p.id) : mono(v.pool_id)),
      fact("Provisioning", v.thin_provision ? "Thin" : "Thick"),
      fact("Exported", exps.length ? exps.map((e) => el("div", {}, link(e.name, "export", e.id), ` (${protoName(e.protocol)})`)) : pill("Not exported", "quiet")),
      v.parent_snapshot_id && fact("Cloned from", `snapshot ${v.parent_snapshot_id}`),
      fact("Limits", qos || "None"),
      fact("UUID", mono(v.uuid)),
      fact("SPDK bdev", mono(v.lvol_bdev_name)),
      fact("Created", humanTime(v.created_at)),
      fact("Labels", labelsText(v.labels) || "-"),
      fact("ID", mono(v.id))),
    el("h2", {}, "Exports"),
    exportTable(exps, { [v.id]: v }, byId(groups), { filter: false, empty: "Not exported. Export it to let hosts connect." }),
    el("h2", {}, "Snapshots"),
    table([
      { title: "Name", cell: (s) => el("strong", {}, s.name), text: (s) => s.name },
      { title: "Size", num: true, cell: (s) => humanSize(s.size_bytes) },
      { title: "Taken", cell: (s) => humanTime(s.created_at) },
      { title: "Group", cell: (s) => (groupOf[s.id] ? groupOf[s.id].name : "-") },
      { title: "", cell: (s) => el("div", { class: "row-actions" },
        snaps.indexOf(s) > 0 && el("button", { onclick: () => changedBlocksDialog(s, snaps.slice(0, snaps.indexOf(s))) }, "Changed blocks"),
        snapshotActions(s, groupOf)) },
    ], snaps, { empty: "No snapshots of this volume.", state: stateOf("volume-snapshots") }),
  ];
}

// --- Snapshots ---

function groupBySnapshot(vgs) {
  const out = {};
  for (const g of vgs) for (const sid of g.snapshot_ids || []) out[sid] = g;
  return out;
}

function deleteSnapshotDialog(s) {
  confirmDialog({
    title: `Delete snapshot ${s.name}`,
    intro: "This can't be undone. A snapshot that volumes were cloned from is refused.",
    submit: "Delete snapshot",
    action: () => send("DELETE", "/snapshots/" + encodeURIComponent(s.id)),
    done: () => { notify(`Deleted snapshot ${s.name}.`); rerender(); },
  });
}

function deleteGroupDialog(g) {
  confirmDialog({
    title: `Delete group snapshot ${g.name}`,
    intro: `This deletes all ${(g.snapshot_ids || []).length} of its snapshots together. It can't be undone.`,
    submit: "Delete group",
    action: () => send("DELETE", "/volume-group-snapshots/" + encodeURIComponent(g.id)),
    done: () => { notify(`Deleted ${g.name} and its snapshots.`); rerender(); },
  });
}

// A snapshot in a group goes only with its group, so it offers to delete that instead.
function snapshotActions(s, groupOf) {
  const g = groupOf[s.id];
  return [
    can.provision && el("button", { onclick: () => cloneDialog(s) }, "Clone"),
    can.provision && (g
      ? el("button", { class: "danger-quiet", onclick: () => deleteGroupDialog(g) }, "Delete group")
      : el("button", { class: "danger-quiet", onclick: () => deleteSnapshotDialog(s) }, "Delete")),
  ];
}

async function createGroupDialog() {
  const vols = await list("/volumes", "volumes");
  openDialog({
    title: "Take a group snapshot",
    intro: "Snapshots of several volumes, taken at the same instant. NVMe-oF I/O to them is paused for the moment it takes, so they're consistent with each other.",
    submit: "Take group snapshot",
    fields: [
      { name: "name", label: "Name", required: true, value: `group-${new Date().toISOString().slice(0, 16).replace(/[:T]/g, "-")}` },
      { name: "volumes", kind: "checks", label: "Volumes", options: vols.map((v) => ({ value: v.id, label: v.name })), empty: "No volumes yet." },
    ],
    action: (x) => send("POST", "/volume-group-snapshots", { name: x.name, volume_ids: x.volumes }),
    done: (r) => { notify(`Took ${(r.snapshots || []).length} snapshots as ${r.volume_group_snapshot.name}.`); rerender(); },
  });
}

async function snapshots() {
  const [items, vols, poolList, vgs] = await Promise.all([list("/snapshots", "snapshots"), list("/volumes", "volumes"), list("/pools", "pools"), list("/volume-group-snapshots", "volume_group_snapshots")]);
  const volsById = byId(vols);
  const poolsById = byId(poolList);
  const groupOf = groupBySnapshot(vgs);
  return [
    el("h1", {}, "Snapshots"),
    el("div", { class: "toolbar" }, action(can.provision, { class: "primary", id: "create-group", onclick: createGroupDialog }, "Take group snapshot")),
    table([
      { title: "Name", cell: (s) => el("strong", {}, s.name), text: (s) => s.name },
      { title: "Volume", cell: (s) => (volsById[s.volume_id] ? link(volsById[s.volume_id].name, "volume", s.volume_id) : el("span", { class: "muted" }, "deleted volume")), text: (s) => (volsById[s.volume_id] || {}).name || "" },
      { title: "Pool", cell: (s) => (poolsById[s.pool_id] ? link(poolsById[s.pool_id].name, "pool", s.pool_id) : "-") },
      { title: "Size", num: true, cell: (s) => humanSize(s.size_bytes) },
      { title: "Taken", cell: (s) => humanTime(s.created_at) },
      { title: "Group", cell: (s) => (groupOf[s.id] ? groupOf[s.id].name : "-"), text: (s) => (groupOf[s.id] || {}).name || "" },
      { title: "", cell: (s) => el("div", { class: "row-actions" }, snapshotActions(s, groupOf)) },
    ], items, { empty: "No snapshots yet. Open a volume to take one.", state: stateOf("snapshots") }),
    el("h2", {}, "Group snapshots"),
    table([
      { title: "Name", cell: (g) => el("strong", {}, g.name), text: (g) => g.name },
      { title: "Volumes", cell: (g) => (g.volume_ids || []).map((id) => el("div", {}, volsById[id] ? link(volsById[id].name, "volume", id) : mono(id))) },
      { title: "Taken", cell: (g) => humanTime(g.created_at) },
      { title: "", cell: (g) => el("div", { class: "row-actions" }, can.provision && el("button", { class: "danger-quiet", onclick: () => deleteGroupDialog(g) }, "Delete group")) },
    ], vgs, { empty: "No group snapshots.", state: stateOf("groups") }),
  ];
}

// --- Exports ---

function whoCanConnect(e, groupsById) {
  if (e.allow_any_host) return pill("Any host", "warn");
  const names = (e.initiator_group_ids || []).map((id) => (groupsById[id] || {}).name || id);
  return names.length ? names.join(", ") : pill("Nobody", "bad");
}

function exportTable(items, volsById, groupsById, options = {}) {
  return table([
    { title: "Name", cell: (e) => el("strong", {}, link(e.name, "export", e.id)), text: (e) => `${e.name} ${e.subsystem_nqn || ""} ${e.target_iqn || ""}` },
    { title: "Health", cell: health, text: (e) => e.health_status },
    { title: "Volume", cell: (e) => (volsById[e.volume_id] ? link(volsById[e.volume_id].name, "volume", e.volume_id) : mono(e.volume_id)), text: (e) => (volsById[e.volume_id] || {}).name || "" },
    { title: "Protocol", cell: (e) => protoName(e.protocol), text: (e) => e.protocol },
    { title: "Target", cell: (e) => mono(e.subsystem_nqn || e.target_iqn) },
    { title: "Who can connect", cell: (e) => whoCanConnect(e, groupsById) },
    { title: "Node", cell: (e) => e.node_id || "-", text: (e) => e.node_id || "" },
  ], items, { empty: "No exports yet.", state: stateOf("exports-table"), ...options });
}

// The fields shared by creating an export and changing who may reach one.
function accessFields(groups, current = {}) {
  return [
    { name: "anyHost", kind: "tick", value: current.allow_any_host, text: "Let any host that can reach the portal connect, with no access control" },
    { name: "groups", kind: "checks", label: "Initiator groups that may connect", when: (v) => !v.anyHost, value: current.initiator_group_ids || [],
      options: groups.map((g) => ({ value: g.id, label: g.name })), empty: "No initiator groups yet. Make one on the Hosts screen." },
  ];
}

async function exportDialog(v) {
  const [portalList, groups] = await Promise.all([list("/portals", "portals"), list("/initiator-groups", "initiator_groups")]);
  const protos = ["NVMEOF_TCP", "NVMEOF_RDMA", "ISCSI"].filter((p) => portalList.some((x) => x.protocol === p));
  if (!protos.length) {
    openDialog({ title: `Export ${v.name}`, intro: "There are no portals to export on yet. Add one on the Portals screen first." });
    return;
  }
  const slug = v.name.toLowerCase().replace(/[^a-z0-9._-]+/g, "-").slice(0, 50);
  const ofProto = (p) => portalList.filter((x) => x.protocol === p);
  openDialog({
    title: `Export ${v.name}`,
    intro: "This lets hosts connect to it. Only the initiator groups you choose may, unless you allow any host.",
    submit: "Export",
    fields: [
      { name: "name", label: "Export name", required: true, value: slug, hint: "Used in the NVMe subsystem NQN or iSCSI target IQN." },
      { name: "protocol", kind: "radio", label: "Protocol", value: protos[0], options: protos.map((p) => ({ value: p, label: protoName(p) })) },
      ...protos.map((p) => ({ name: "portals_" + p, kind: "checks", label: "Portals", when: (x) => x.protocol === p,
        value: ofProto(p).length === 1 ? [ofProto(p)[0].id] : [],
        options: ofProto(p).map((x) => ({ value: x.id, label: `${x.name} (${x.ip_address}:${x.port})` })) })),
      ...accessFields(groups),
    ],
    action: (x) => send("POST", "/exports", {
      name: x.name, volume_id: v.id, protocol: x.protocol, portal_ids: x["portals_" + x.protocol],
      allow_any_host: x.anyHost, initiator_group_ids: x.anyHost ? [] : x.groups,
    }),
    done: (r) => { notify(`Exported ${v.name}.`); location.hash = hash("export", r.export.id); },
  });
}

async function createExportDialog() {
  const vols = (await list("/volumes", "volumes")).filter((v) => v.attachment_state !== "FILESYSTEM");
  if (!vols.length) { notify("Create a volume first."); return; }
  openDialog({
    title: "Export a volume",
    submit: "Next",
    fields: [{ name: "volume", label: "Volume", value: vols[0].id, options: vols.map((v) => ({ value: v.id, label: v.name })) }],
    action: async (x) => vols.find((v) => v.id === x.volume),
    done: (v) => exportDialog(v),
  });
}

async function connectionsOf(exp) {
  try { return await list(`/exports/${encodeURIComponent(exp.id)}/connections`, "connections"); } catch (err) { if (err instanceof Unauthorized) throw err; return null; }
}

function connectionsNotice(conns) {
  if (conns === null) return el("div", { class: "panel-notice warn" }, "Couldn't check who's connected.");
  if (!conns.length) return el("div", { class: "panel-notice" }, "Nothing is connected right now.");
  return el("div", { class: "panel-notice warn" }, `Connected right now: ${conns.map((c) => c.host || c.address).join(", ")}.`);
}

async function deleteExportDialog(e) {
  const warning = connectionsNotice(await connectionsOf(e));
  confirmDialog({
    title: `Delete export ${e.name}`,
    warning,
    intro: "The volume and its data are kept, but connected hosts lose access at once, so anything using it sees I/O errors.",
    submit: "Delete export",
    confirmText: e.name,
    action: () => send("DELETE", "/exports/" + encodeURIComponent(e.id)),
    done: () => { notify(`Deleted export ${e.name}.`); location.hash = "#/exports"; },
  });
}

async function accessDialog(e) {
  const groups = await list("/initiator-groups", "initiator_groups");
  openDialog({
    title: `Who can connect to ${e.name}`,
    intro: "Hosts that lose access are cut off at once. It's applied to SPDK before it's saved, so if it can't be applied nothing changes.",
    submit: "Save",
    danger: true,
    fields: accessFields(groups, e),
    action: (x) => send("PATCH", "/exports/" + encodeURIComponent(e.id), { allow_any_host: x.anyHost, initiator_group_ids: x.anyHost ? [] : x.groups }),
    done: () => { notify("Access saved."); rerender(); },
  });
}

function connectCommand(e, p) {
  const addr = p.ip_address.includes(":") ? `[${p.ip_address}]` : p.ip_address;
  if (e.protocol === "ISCSI") {
    return `iscsiadm -m discovery -t sendtargets -p ${addr}:${p.port} && iscsiadm -m node -T ${e.target_iqn} -p ${addr}:${p.port} --login`;
  }
  return `nvme connect -t ${e.protocol === "NVMEOF_RDMA" ? "rdma" : "tcp"} -a ${p.ip_address} -s ${p.port} -n ${e.subsystem_nqn}`;
}

async function exportsScreen() {
  const [items, vols, groups] = await Promise.all([list("/exports", "exports"), list("/volumes", "volumes"), list("/initiator-groups", "initiator_groups")]);
  const open = items.filter((e) => e.allow_any_host).length;
  return [
    el("h1", {}, "Exports"),
    el("div", { class: "toolbar" }, action(can.provision, { class: "primary", id: "create-export", onclick: createExportDialog }, "Export a volume")),
    open > 0 && el("div", { class: "panel-notice warn" }, `${open} export${open === 1 ? " lets" : "s let"} any host connect.`),
    exportTable(items, byId(vols), byId(groups)),
  ];
}

async function exportScreen(id) {
  const e = (await api("/exports/" + encodeURIComponent(id))).export;
  const [vol, portalList, groups, hostList, conns] = await Promise.all([
    api("/volumes/" + encodeURIComponent(e.volume_id)).then((r) => r.volume).catch((err) => { if (err instanceof Unauthorized) throw err; return null; }),
    list("/portals", "portals"), list("/initiator-groups", "initiator_groups"), list("/hosts", "hosts"), connectionsOf(e),
  ]);
  const offered = portalList.filter((p) => (e.portal_ids || []).includes(p.id));
  const mine = groups.filter((g) => (e.initiator_group_ids || []).includes(g.id));
  const hostsById = byId(hostList);
  return [
    el("p", { class: "crumb" }, el("a", { href: "#/exports" }, "Exports")),
    el("h1", {}, e.name),
    el("div", { class: "toolbar" }, action(can.provision, { class: "danger-quiet", id: "delete-export", onclick: () => deleteExportDialog(e) }, "Delete")),
    el("dl", { class: "facts" },
      fact("Health", health(e)),
      fact("Volume", vol ? link(vol.name, "volume", vol.id) : mono(e.volume_id)),
      fact("Protocol", protoName(e.protocol)),
      e.subsystem_nqn ? fact("Subsystem NQN", mono(e.subsystem_nqn)) : fact("Target IQN", mono(e.target_iqn)),
      e.subsystem_nqn ? fact("Namespace", e.namespace_id) : fact("LUN", String(e.lun_id)),
      e.node_id && fact("Kubernetes node", e.node_id),
      fact("Created", humanTime(e.created_at))),
    el("div", { class: "section-head" },
      el("h2", {}, "Who can connect"),
      action(can.provision, { class: "primary", id: "change-access", onclick: () => accessDialog(e) }, "Change")),
    e.allow_any_host
      ? el("div", { class: "panel-notice warn" }, "Any host that can reach its portals can connect.")
      : table([
        { title: "Initiator group", cell: (g) => el("strong", {}, g.name) },
        { title: "Members", cell: (g) => groupMembers(g, hostsById) },
      ], mine, { filter: false, empty: "Nobody, so no host can connect.", state: stateOf("export-groups") }),
    el("h2", {}, "Portals"),
    table([
      { title: "Portal", cell: (p) => [el("strong", {}, p.name), el("span", { class: "sub mono" }, `${p.ip_address}:${p.port}`)] },
      { title: "To connect", cell: (p) => el("code", { class: "command" }, connectCommand(e, p)) },
    ], offered, { filter: false, empty: "None, so hosts can't reach it.", state: stateOf("export-portals") }),
    el("h2", {}, "Connected now"),
    conns === null ? connectionsNotice(null) : table([
      { title: e.protocol === "ISCSI" ? "Initiator address" : "Host", cell: (c) => mono(c.host || c.address) },
      ...(e.protocol === "ISCSI" ? [] : [{ title: "Queues", num: true, cell: (c) => c.queues }]),
    ], conns, { filter: false, empty: "Nothing is connected right now.", state: stateOf("connections") }),
  ];
}

// --- Hosts and initiator groups ---

function groupMembers(g, hostsById) {
  const parts = [
    ...(g.host_ids || []).map((id) => (hostsById[id] ? `host ${hostsById[id].name}` : `host ${id}`)),
    ...(g.host_nqns || []), ...(g.initiator_iqns || []), ...(g.allowed_cidrs || []).map((c) => `from ${c}`),
  ];
  return parts.length ? parts.map((x) => el("div", { class: "mono" }, x)) : pill("Empty", "warn");
}

function addHostDialog() {
  openDialog({
    title: "Add a host",
    intro: "A machine that connects over NVMe-oF, iSCSI, or both. Put it in an initiator group to give it access.",
    submit: "Add",
    fields: [
      { name: "name", label: "Name", required: true },
      { name: "nqn", label: "NVMe host NQN", placeholder: "nqn.2014-08.org.nvmexpress:uuid:...", hint: "From /etc/nvme/hostnqn on the host." },
      { name: "iqn", label: "iSCSI initiator IQN", placeholder: "iqn.1994-05.com.redhat:...", hint: "From /etc/iscsi/initiatorname.iscsi on the host." },
      { name: "ip", label: "IP address", placeholder: "Optional: also limits iSCSI to this address" },
    ],
    action: (x) => send("POST", "/hosts", { name: x.name, host_nqn: x.nqn, initiator_iqn: x.iqn, ip_address: x.ip }),
    done: (r) => { notify(`Added ${r.host.name}.`); rerender(); },
  });
}

function deleteHostDialog(h) {
  confirmDialog({
    title: `Delete host ${h.name}`,
    intro: "A host that's in an initiator group is refused: take it out of the group first.",
    submit: "Delete host",
    action: () => send("DELETE", "/hosts/" + encodeURIComponent(h.id)),
    done: () => { notify(`Deleted ${h.name}.`); rerender(); },
  });
}

function groupFields(hostList, g = {}) {
  return [
    { name: "hosts", kind: "checks", label: "Hosts", value: g.host_ids || [], options: hostList.map((h) => ({ value: h.id, label: h.name })), empty: "No hosts yet." },
    { name: "nqns", kind: "lines", label: "Other NVMe host NQNs", value: g.host_nqns, placeholder: "One per line" },
    { name: "iqns", kind: "lines", label: "Other iSCSI initiator IQNs", value: g.initiator_iqns, placeholder: "One per line" },
    { name: "cidrs", kind: "lines", label: "iSCSI source networks", value: g.allowed_cidrs, placeholder: "10.0.0.0/24, one per line", hint: "iSCSI only. NVMe-oF access is by host NQN." },
  ];
}

async function createGroupACLDialog() {
  const hostList = await list("/hosts", "hosts");
  openDialog({
    title: "Create an initiator group",
    intro: "A set of hosts that exports can be shared with.",
    submit: "Create",
    fields: [{ name: "name", label: "Name", required: true }, ...groupFields(hostList)],
    action: (x) => send("POST", "/initiator-groups", { name: x.name, host_ids: x.hosts, host_nqns: x.nqns, initiator_iqns: x.iqns, allowed_cidrs: x.cidrs }),
    done: (r) => { notify(`Created ${r.initiator_group.name}.`); rerender(); },
  });
}

async function editGroupDialog(g, users) {
  const hostList = await list("/hosts", "hosts");
  openDialog({
    title: `Change ${g.name}`,
    intro: users ? `It's applied at once to the ${users} export${users === 1 ? "" : "s"} that use it. Hosts that are taken out are cut off.` : "No exports use it yet.",
    submit: "Save",
    danger: users > 0,
    fields: groupFields(hostList, g),
    action: (x) => send("PATCH", "/initiator-groups/" + encodeURIComponent(g.id), { host_ids: x.hosts, host_nqns: x.nqns, initiator_iqns: x.iqns, allowed_cidrs: x.cidrs }),
    done: () => { notify(`Saved ${g.name}.`); rerender(); },
  });
}

function deleteGroupACLDialog(g) {
  confirmDialog({
    title: `Delete initiator group ${g.name}`,
    intro: "A group that an export uses is refused.",
    submit: "Delete group",
    action: () => send("DELETE", "/initiator-groups/" + encodeURIComponent(g.id)),
    done: () => { notify(`Deleted ${g.name}.`); rerender(); },
  });
}

async function hosts() {
  const [items, groups, exps] = await Promise.all([list("/hosts", "hosts"), list("/initiator-groups", "initiator_groups"), list("/exports", "exports")]);
  const hostsById = byId(items);
  const usedBy = (gid) => exps.filter((e) => (e.initiator_group_ids || []).includes(gid)).length;
  const inGroups = (hid) => groups.filter((g) => (g.host_ids || []).includes(hid)).map((g) => g.name);
  return [
    el("h1", {}, "Hosts"),
    el("div", { class: "toolbar" }, action(can.provision, { class: "primary", id: "add-host", onclick: addHostDialog }, "Add host")),
    table([
      { title: "Name", cell: (h) => el("strong", {}, h.name), text: (h) => `${h.name} ${h.host_nqn || ""} ${h.initiator_iqn || ""} ${h.ip_address || ""}` },
      { title: "NVMe host NQN", cell: (h) => mono(h.host_nqn || "-") },
      { title: "iSCSI IQN", cell: (h) => mono(h.initiator_iqn || "-") },
      { title: "Address", cell: (h) => mono(h.ip_address || "-") },
      { title: "Groups", cell: (h) => inGroups(h.id).join(", ") || "-" },
      { title: "", cell: (h) => el("div", { class: "row-actions" }, can.provision && el("button", { class: "danger-quiet", onclick: () => deleteHostDialog(h) }, "Delete")) },
    ], items, { empty: "No hosts yet. The CSI driver adds its Kubernetes nodes itself.", state: stateOf("hosts") }),
    el("div", { class: "section-head" },
      el("h2", {}, "Initiator groups"),
      action(can.provision, { class: "primary", id: "create-ig", onclick: createGroupACLDialog }, "Create group")),
    table([
      { title: "Name", cell: (g) => el("strong", {}, g.name), text: (g) => g.name },
      { title: "Members", cell: (g) => groupMembers(g, hostsById) },
      { title: "Used by", num: true, cell: (g) => `${usedBy(g.id)} exports` },
      { title: "", cell: (g) => el("div", { class: "row-actions" },
        can.provision && el("button", { onclick: () => editGroupDialog(g, usedBy(g.id)) }, "Change"),
        can.provision && el("button", { class: "danger-quiet", disabled: usedBy(g.id) > 0, onclick: () => deleteGroupACLDialog(g) }, "Delete")) },
    ], groups, { empty: "No initiator groups yet.", state: stateOf("igs") }),
  ];
}

// --- Portals ---

async function addPortalDialog() {
  const net = await api("/system/network").catch(() => ({ interfaces: [] }));
  const ips = [...new Set((net.interfaces || []).flatMap((i) => (i.addresses || []).map((a) => a.split("/")[0])))];
  openDialog({
    title: "Add a portal",
    intro: "An address and port that hosts connect to. SPDK starts listening when an export uses it.",
    submit: "Add",
    fields: [
      { name: "name", label: "Name", required: true },
      { name: "protocol", kind: "radio", label: "Protocol", value: "NVMEOF_TCP", options: ["NVMEOF_TCP", "NVMEOF_RDMA", "ISCSI"].map((p) => ({ value: p, label: protoName(p) })) },
      { name: "ip", label: "Address", required: true, suggestions: ips, placeholder: ips[0] || "192.0.2.10", hint: "An address on this appliance." },
      { name: "port", label: "Port", number: true, placeholder: "4420 for NVMe-oF, 3260 for iSCSI" },
    ],
    action: (x) => send("POST", "/portals", { name: x.name, protocol: x.protocol, ip_address: x.ip, port: Number(x.port) || 0 }),
    done: (r) => { notify(`Added ${r.portal.name}.`); rerender(); },
  });
}

function deletePortalDialog(p) {
  confirmDialog({
    title: `Delete portal ${p.name}`,
    intro: "A portal that an export uses is refused.",
    submit: "Delete portal",
    action: () => send("DELETE", "/portals/" + encodeURIComponent(p.id)),
    done: () => { notify(`Deleted ${p.name}.`); rerender(); },
  });
}

async function portals() {
  const [items, exps] = await Promise.all([list("/portals", "portals"), list("/exports", "exports")]);
  const usedBy = (pid) => exps.filter((e) => (e.portal_ids || []).includes(pid)).length;
  return [
    el("h1", {}, "Portals"),
    el("div", { class: "toolbar" }, action(can.admin, { class: "primary", id: "add-portal", onclick: addPortalDialog }, "Add portal")),
    table([
      { title: "Name", cell: (p) => el("strong", {}, p.name), text: (p) => `${p.name} ${p.ip_address}` },
      { title: "Protocol", cell: (p) => protoName(p.protocol) },
      { title: "Address", cell: (p) => mono(`${p.ip_address}:${p.port}`) },
      { title: "Used by", num: true, cell: (p) => `${usedBy(p.id)} exports` },
      { title: "", cell: (p) => el("div", { class: "row-actions" }, can.admin && el("button", { class: "danger-quiet", disabled: usedBy(p.id) > 0, onclick: () => deletePortalDialog(p) }, "Delete")) },
    ], items, { filter: false, empty: "No portals yet.", state: stateOf("portals") }),
  ];
}

// --- Network ---

function interfaceDialog(i) {
  openDialog({
    title: `Configure ${i.name}`,
    intro: "Changing the interface you're connected through can cut this session off. Have console access ready.",
    submit: "Apply",
    danger: true,
    fields: [
      { name: "method", kind: "radio", label: "Addressing", value: i.method === "manual" ? "manual" : i.method === "disabled" ? "disabled" : "dhcp",
        options: [{ value: "dhcp", label: "DHCP" }, { value: "manual", label: "Static" }, { value: "disabled", label: "Disabled" }] },
      { name: "address", label: "Address", required: true, when: (v) => v.method === "manual", value: (i.addresses || [])[0], placeholder: "10.0.0.20/24" },
      { name: "gateway", label: "Gateway", when: (v) => v.method === "manual", value: i.gateway },
      { name: "dns", label: "DNS servers", when: (v) => v.method === "manual", value: (i.dns || []).join(", "), placeholder: "Comma separated" },
      { name: "mtu", label: "MTU", number: true, when: (v) => v.method !== "disabled", value: i.mtu ? String(i.mtu) : "", hint: "9000 for jumbo frames on a storage network." },
    ],
    action: (x) => send("PUT", "/system/network/interfaces/" + encodeURIComponent(i.name), { method: x.method, address: x.address, gateway: x.gateway, dns: x.dns, mtu: Number(x.mtu) || 0 }),
    done: () => { notify(`Applied to ${i.name}.`); rerender(); },
  });
}

async function network() {
  const net = await api("/system/network");
  return [
    el("h1", {}, "Network"),
    !net.available && el("div", { class: "panel-notice warn" }, net.message || "Network information isn't available."),
    net.available && !net.nmcli_available && el("div", { class: "panel-notice warn" }, "NetworkManager isn't reachable, so interfaces can only be viewed."),
    el("dl", { class: "facts" }, fact("Hostname", net.hostname)),
    el("h2", {}, "Interfaces"),
    table([
      { title: "Interface", cell: (i) => [el("strong", {}, i.name), el("span", { class: "sub" }, i.type)], text: (i) => i.name },
      { title: "Link", cell: (i) => (i.link_up ? pill("Up", "ok") : pill("Down", "quiet")) },
      { title: "Addressing", cell: (i) => i.method || "-" },
      { title: "Addresses", cell: (i) => (i.addresses || []).map((a) => el("div", { class: "mono" }, a)) },
      { title: "Gateway", cell: (i) => mono(i.gateway || "-") },
      { title: "DNS", cell: (i) => (i.dns || []).join(", ") || "-" },
      { title: "MTU", num: true, cell: (i) => i.mtu },
      { title: "MAC", cell: (i) => mono(i.mac || "-") },
      { title: "", cell: (i) => el("div", { class: "row-actions" }, can.admin && net.nmcli_available && i.type !== "loopback" && el("button", { onclick: () => interfaceDialog(i) }, "Configure")) },
    ], net.interfaces || [], { filter: false, empty: "No interfaces.", state: stateOf("ifaces") }),
  ];
}

// --- System ---

function upgradeDialog(os) {
  openDialog({
    title: "Upgrade the OS",
    intro: "bootc downloads the new image and stages it for the next boot. Nothing changes until the appliance reboots, and the current image stays available to roll back to.",
    submit: "Stage upgrade",
    fields: [{ name: "image", label: "Image", placeholder: (os.booted || {}).image || "", hint: "Leave empty to update to the newest build of the image it's running." }],
    action: (x) => send("POST", "/system/upgrade", { image_ref: x.image }),
    done: (r) => { notify(r.message || "Upgrade staged."); rerender(); },
  });
}

function rollbackDialog() {
  confirmDialog({
    title: "Roll back the OS",
    intro: "The previous image is made the one to boot next. It takes effect on the next reboot.",
    submit: "Roll back",
    action: () => send("POST", "/system/rollback"),
    done: (r) => { notify(r.message || "Rollback staged."); rerender(); },
  });
}

function rebootDialog(hostname) {
  confirmDialog({
    title: "Reboot the appliance",
    intro: "All SAN I/O stops until it's back. Hosts reconnect on their own if their timeouts allow.",
    submit: "Reboot",
    confirmText: hostname || "reboot",
    action: () => send("POST", "/system/reboot"),
    done: () => notify("Rebooting."),
  });
}

function containerUpdateDialog(unit) {
  const dataPlane = unit === "velocor-spdk.service";
  openDialog({
    title: unit ? `Update ${unit}` : "Apply container updates",
    intro: dataPlane
      ? "This restarts spdk_tgt, which stops ALL SAN I/O on this appliance until it's back. Do it in a maintenance window."
      : unit ? "The container restarts with its newest image. The data plane keeps serving I/O meanwhile."
        : "Every container with automatic updates restarts with its newest image. The SPDK data plane is never included: update it on its own.",
    submit: dataPlane ? "Restart the data plane" : "Update",
    danger: dataPlane,
    confirmText: dataPlane ? "velocor-spdk" : undefined,
    action: () => send("POST", "/system/containers/update", { unit: unit || "", allow_disruptive: dataPlane }),
    done: (r) => { notify(r.message || "Update started."); setTimeout(rerender, 1500); },
  });
}

let containerCheck = null;

async function system() {
  const [st, os] = await Promise.all([api("/system/status"), api("/system/os").catch((err) => { if (err instanceof Unauthorized) throw err; return null; })]);
  const rec = st.reconciler || {};
  const dep = (d) => (d ? [mono(d.image || "-"), el("span", { class: "sub" }, [d.version, d.timestamp && humanTime(d.timestamp)].filter(Boolean).join(", "))] : "-");
  return [
    el("h1", {}, "System"),
    el("div", { class: "grid-2" },
      el("div", {},
        el("div", { class: "section-head" }, el("h2", {}, "Reconciler"),
          action(can.admin, { id: "reconcile", onclick: async () => {
            try { await send("POST", "/system/reconcile"); notify("Reconciled."); rerender(); } catch (err) { notify(err.message); }
          } }, "Reconcile now")),
        el("dl", { class: "facts" },
          fact("Last pass", humanTime(rec.last_reconciled_at)),
          fact("Repairs so far", String(rec.drift_repaired_count || 0)),
          fact("Needing attention", (rec.degraded_resources || []).length ? rec.degraded_resources.map((r) => el("div", {}, resourceLink(r))) : pill("Nothing", "ok")),
          rec.last_error && fact("Last error", mono(rec.last_error)))),
      el("div", {},
        el("div", { class: "section-head" }, el("h2", {}, "Operating system"),
          el("div", { class: "row-actions" },
            can.admin && os && os.available && el("button", { onclick: () => upgradeDialog(os) }, "Upgrade"),
            can.admin && os && os.rollback && el("button", { class: "danger-quiet", onclick: rollbackDialog }, "Roll back"),
            can.admin && el("button", { class: "danger-quiet", onclick: () => rebootDialog(st.node && st.node.hostname) }, "Reboot"))),
        os && os.available
          ? el("dl", { class: "facts" }, fact("Booted", dep(os.booted)), fact("Staged", dep(os.staged)), fact("Rollback", dep(os.rollback)))
          : el("div", { class: "panel-notice" }, (os && os.message) || "bootc isn't available on this host."))),
    el("div", { class: "section-head" }, el("h2", {}, "Containers"),
      el("div", { class: "row-actions" },
        el("button", { id: "check-updates", onclick: async () => {
          try { containerCheck = await api("/system/containers/updates"); rerender(); } catch (err) { notify(err.message); }
        } }, containerCheck ? "Check again" : "Check for updates"),
        can.admin && containerCheck && containerCheck.updates_available && el("button", { class: "primary", onclick: () => containerUpdateDialog("") }, "Apply updates"))),
    containerCheck
      ? [
        containerCheck.message && el("div", { class: "panel-notice warn" }, containerCheck.message),
        containerCheck.last_update_state && el("div", { class: "panel-notice" }, `Last update: ${containerCheck.last_update_state}. ${containerCheck.last_update_message || ""}`),
        table([
          { title: "Unit", cell: (c) => [el("strong", {}, c.unit), el("span", { class: "sub mono" }, c.image)] },
          { title: "Running", cell: (c) => c.running_status || "-" },
          { title: "Update", cell: (c) => (c.update_available ? pill("Available", "warn") : pill(c.update_status || "Current", "quiet")) },
          { title: "", cell: (c) => el("div", { class: "row-actions" }, can.admin && el("button", { class: c.unit === "velocor-spdk.service" ? "danger-quiet" : "", onclick: () => containerUpdateDialog(c.unit) }, "Update")) },
        ], containerCheck.containers || [], { filter: false, empty: "No containers found." }),
      ]
      : el("p", { class: "muted" }, "Checking asks the registry for newer images, so it's only done when you ask."),
  ];
}

// --- Access ---

const ROLES = [
  { value: "ADMIN", label: "Admin: everything" },
  { value: "CSI_PROVISIONER", label: "Provisioner: volumes, snapshots and exports, as the CSI driver needs" },
  { value: "READONLY", label: "Read only" },
];

function createUserDialog() {
  openDialog({
    title: "Add a user",
    submit: "Add",
    fields: [
      { name: "username", label: "Username", required: true },
      { name: "password", label: "Password", secret: true, required: true, hint: "At least 8 characters." },
      { name: "role", kind: "radio", label: "Role", value: "READONLY", options: ROLES },
    ],
    action: (x) => send("POST", "/auth/users", { username: x.username, password: x.password, role: x.role }),
    done: (r) => { notify(`Added ${r.user.username}.`); rerender(); },
  });
}

function roleDialog(u) {
  openDialog({
    title: `Role of ${u.username}`,
    intro: "Their sessions end, and they sign in again with the new role.",
    submit: "Save",
    fields: [{ name: "role", kind: "radio", label: "Role", value: u.role, options: ROLES }],
    action: (x) => send("PATCH", "/auth/users/" + encodeURIComponent(u.username), { role: x.role }),
    done: () => { notify(`Changed ${u.username}'s role.`); rerender(); },
  });
}

function resetPasswordDialog(u) {
  openDialog({
    title: `Reset ${u.username}'s password`,
    intro: "Their sessions end, and they must choose their own password when they next sign in.",
    submit: "Reset",
    fields: [{ name: "password", label: "Temporary password", secret: true, required: true }],
    action: (x) => send("PATCH", "/auth/users/" + encodeURIComponent(u.username), { password: x.password }),
    done: () => { notify(`Reset ${u.username}'s password.`); rerender(); },
  });
}

function deleteUserDialog(u) {
  confirmDialog({
    title: `Delete ${u.username}`,
    intro: "Their sessions end at once. The last admin can't be deleted.",
    submit: "Delete user",
    action: () => send("DELETE", "/auth/users/" + encodeURIComponent(u.username)),
    done: () => { notify(`Deleted ${u.username}.`); rerender(); },
  });
}

function createTokenDialog() {
  openDialog({
    title: "Create an API token",
    intro: "For automation such as the CSI driver or veloctl. It's shown once, so copy it then.",
    submit: "Create",
    fields: [
      { name: "name", label: "Name", required: true, placeholder: "k8s-prod-csi" },
      { name: "role", kind: "radio", label: "Role", value: "CSI_PROVISIONER", options: ROLES },
      { name: "days", label: "Expires after", value: "0", options: [["0", "Never"], ["30", "30 days"], ["90", "90 days"], ["365", "1 year"]].map(([v, l]) => ({ value: v, label: l })) },
    ],
    action: (x) => send("POST", "/auth/tokens", { name: x.name, role: x.role, ttl_seconds: Number(x.days) * 86400 }),
    done: (r) => {
      rerender();
      openDialog({
        title: `Token ${r.token.name}`,
        intro: "Copy it now. It can't be shown again.",
        warning: [el("code", { class: "secret", id: "new-token" }, r.secret_token),
          el("button", { type: "button", onclick: () => navigator.clipboard.writeText(r.secret_token).then(() => notify("Copied.")).catch(() => notify("Select it and copy instead.")) }, "Copy")],
      });
    },
  });
}

function revokeTokenDialog(t) {
  confirmDialog({
    title: `Revoke ${t.name}`,
    intro: "Anything using it, such as a CSI driver, stops working at once.",
    submit: "Revoke",
    confirmText: t.name,
    action: () => send("DELETE", "/auth/tokens/" + encodeURIComponent(t.id)),
    done: () => { notify(`Revoked ${t.name}.`); rerender(); },
  });
}

async function access() {
  if (!can.admin) return [el("h1", {}, "Access"), el("p", { class: "muted" }, "Only admins manage users and tokens.")];
  const [users, tokens] = await Promise.all([list("/auth/users", "users"), list("/auth/tokens", "tokens")]);
  return [
    el("h1", {}, "Access"),
    el("div", { class: "section-head" }, el("h2", {}, "Users"), el("button", { class: "primary", id: "add-user", onclick: createUserDialog }, "Add user")),
    table([
      { title: "User", cell: (u) => [el("strong", {}, u.username), u.username === me.username && el("span", { class: "sub" }, "you")], text: (u) => u.username },
      { title: "Role", cell: (u) => u.role },
      { title: "Created", cell: (u) => humanTime(u.created_at) },
      { title: "", cell: (u) => el("div", { class: "row-actions" },
        el("button", { onclick: () => roleDialog(u) }, "Role"),
        el("button", { onclick: () => resetPasswordDialog(u) }, "Reset password"),
        el("button", { class: "danger-quiet", disabled: u.username === me.username, onclick: () => deleteUserDialog(u) }, "Delete")) },
    ], users, { empty: "No users.", state: stateOf("users") }),
    el("div", { class: "section-head" }, el("h2", {}, "API tokens"), el("button", { class: "primary", id: "add-token", onclick: createTokenDialog }, "Create token")),
    table([
      { title: "Name", cell: (t) => [el("strong", {}, t.name), el("span", { class: "sub mono" }, t.prefix)], text: (t) => t.name },
      { title: "Role", cell: (t) => t.role },
      { title: "Created", cell: (t) => humanTime(t.created_at) },
      { title: "Expires", cell: (t) => (t.expires_at ? humanTime(t.expires_at) : "Never") },
      { title: "Last used", cell: (t) => (t.last_used_at ? humanTime(t.last_used_at) : "Never") },
      { title: "", cell: (t) => el("div", { class: "row-actions" }, el("button", { class: "danger-quiet", onclick: () => revokeTokenDialog(t) }, "Revoke")) },
    ], tokens, { empty: "No tokens.", state: stateOf("tokens") }),
  ];
}

// --- Overview ---

// resourceLink turns the reconciler's "kind:id" into a link to that thing's screen.
function resourceLink(ref) {
  const i = ref.indexOf(":");
  const kind = i > 0 ? ref.slice(0, i) : ref, id = i > 0 ? ref.slice(i + 1) : "";
  const target = { pool: "pool", volume: "volume", export: "export" }[kind];
  if (target && id) return [`${kind} `, link(id, target, id)];
  if (kind === "disk") return ["disk ", link(id, "disks")];
  return ref;
}

// Rates are worked out between two refreshes of the cumulative counters.
let lastIO = null;

function ioRates(stats) {
  const now = performance.now();
  const sample = Object.fromEntries(stats.map((b) => [b.name, b]));
  const prev = lastIO;
  lastIO = { at: now, sample };
  if (!prev || now - prev.at < 1000) return null;
  const secs = (now - prev.at) / 1000;
  return stats.map((b) => {
    const p = prev.sample[b.name];
    if (!p) return null;
    const d = (k) => Math.max(0, (b[k] || 0) - (p[k] || 0));
    return { name: b.name, readIOPS: d("num_read_ops") / secs, writeIOPS: d("num_write_ops") / secs, readBps: d("bytes_read") / secs, writeBps: d("bytes_written") / secs };
  }).filter(Boolean);
}

const perSecond = (bytes) => humanSize(Math.round(bytes)) + "/s";

async function dashboard() {
  const [st, diskList, poolList, vols, snaps, exps, hostList, portalList, io] = await Promise.all([
    api("/system/status"), list("/disks", "disks"), list("/pools", "pools"), list("/volumes", "volumes"), list("/snapshots", "snapshots"),
    list("/exports", "exports"), list("/hosts", "hosts"), list("/portals", "portals"),
    api("/system/iostats").catch((err) => { if (err instanceof Unauthorized) throw err; return null; }),
  ]);
  const card = (label, value, href) => el(href ? "a" : "div", { class: "card", href }, el("div", { class: "value" }, value), el("div", { class: "label" }, label));
  const node = st.node || {}, spdk = st.spdk || {}, rec = st.reconciler || {};
  const unhealthy = [
    ...poolList.map((x) => ["Pool", "pool", x]), ...vols.map((x) => ["Volume", "volume", x]), ...exps.map((x) => ["Export", "export", x]),
  ].filter(([, , x]) => x.health_status && x.health_status !== "ONLINE");
  const badDisks = diskList.filter((d) => d.claimed && d.health_status && d.health_status !== "ONLINE");
  const volByUUID = Object.fromEntries(vols.map((v) => [v.uuid, v]));
  const rates = io && ioRates(io.stats || []);
  const volRates = (rates || []).filter((r) => volByUUID[r.name]).map((r) => ({ ...r, vol: volByUUID[r.name] }));
  return [
    el("h1", {}, "Overview"),
    !spdk.connected && el("div", { class: "panel-notice bad" }, `spdk_tgt isn't reachable at ${spdk.socket_path || "its socket"}, so nothing is being served.`),
    el("div", { class: "cards" },
      card("Disks claimed", `${diskList.filter((d) => d.claimed).length} / ${diskList.length}`, "#/disks"),
      card("Pools", poolList.length, "#/pools"), card("Volumes", vols.length, "#/volumes"), card("Snapshots", snaps.length, "#/snapshots"),
      card("Exports", exps.length, "#/exports"), card("Hosts", hostList.length, "#/hosts"), card("Portals", portalList.length, "#/portals")),
    el("h2", {}, "Problems"),
    unhealthy.length || badDisks.length || rec.last_error
      ? [
        badDisks.map((d) => el("div", { class: "panel-notice warn" }, "Disk ", link(d.name || d.id, "disks"), `: ${d.health_status.toLowerCase()}${d.smart && d.smart.critical_warning ? " (SMART critical warning)" : ""}`)),
        unhealthy.map(([kind, route, x]) => el("div", { class: `panel-notice ${x.health_status === "FAULTED" ? "bad" : "warn"}` },
          `${kind} `, link(x.name, route, x.id), `: ${x.health_status.toLowerCase()}${x.status_message ? ` — ${x.status_message}` : ""}`)),
        rec.last_error && !unhealthy.length && !badDisks.length && el("div", { class: "panel-notice warn" }, `Reconciler: ${rec.last_error}`),
      ]
      : el("p", { class: "muted" }, "None."),
    el("h2", {}, "Pools"),
    poolTable(poolList, vols, { filter: false }),
    el("h2", {}, "Volume I/O"),
    !io ? el("p", { class: "muted" }, "I/O statistics aren't available.")
      : !rates ? el("p", { class: "muted" }, "Measuring; rates appear on the next refresh.")
        : table([
          { title: "Volume", cell: (r) => link(r.vol.name, "volume", r.vol.id), text: (r) => r.vol.name },
          { title: "Read IOPS", num: true, cell: (r) => Math.round(r.readIOPS).toLocaleString() },
          { title: "Write IOPS", num: true, cell: (r) => Math.round(r.writeIOPS).toLocaleString() },
          { title: "Read", num: true, cell: (r) => perSecond(r.readBps) },
          { title: "Write", num: true, cell: (r) => perSecond(r.writeBps) },
        ], volRates.sort((a, b) => (b.readIOPS + b.writeIOPS) - (a.readIOPS + a.writeIOPS)), { empty: "No volumes.", state: stateOf("io") }),
    el("h2", {}, "This appliance"),
    el("dl", { class: "facts" },
      fact("Hostname", node.hostname), fact("Node ID", mono(node.node_id)),
      fact("Version", `${node.version || "-"}${node.commit ? ` (${String(node.commit).slice(0, 12)})` : ""}`),
      fact("SPDK", spdk.connected ? [pill("Connected", "ok"), " ", node.spdk_version] : pill("Not reachable", "bad")),
      fact("Hugepages", `${spdk.hugepages_free_mb ?? "-"} of ${spdk.hugepages_total_mb ?? "-"} MiB free`),
      fact("Up for", humanDuration(node.uptime_seconds)),
      fact("Reconciled", [humanTime(rec.last_reconciled_at), el("span", { class: "sub" }, `${rec.drift_repaired_count || 0} repairs so far`)])),
  ];
}

// --- Routing ---

let screenState = {};
const stateOf = (key) => (screenState[key] ||= { text: "", page: 0 });

const NAV = [["Overview", ""], ["Disks", "disks"], ["Pools", "pools"], ["Volumes", "volumes"], ["Snapshots", "snapshots"],
  ["Exports", "exports"], ["Hosts", "hosts"], ["Portals", "portals"], ["Network", "network"], ["System", "system"], ["Access", "access"]];

function route() {
  const [screen, ...rest] = location.hash.replace(/^#\/?/, "").split("/").map(decodeURIComponent);
  switch (screen) {
    case "": return { nav: "", draw: dashboard };
    case "disks": return { nav: "disks", draw: disks };
    case "pools": return { nav: "pools", draw: pools };
    case "pool": return { nav: "pools", draw: () => pool(rest[0]) };
    case "volumes": return { nav: "volumes", draw: () => volumes(rest[0]) };
    case "volume": return { nav: "volumes", draw: () => volume(rest[0]) };
    case "snapshots": return { nav: "snapshots", draw: snapshots };
    case "exports": return { nav: "exports", draw: exportsScreen };
    case "export": return { nav: "exports", draw: () => exportScreen(rest[0]) };
    case "hosts": return { nav: "hosts", draw: hosts };
    case "portals": return { nav: "portals", draw: portals };
    case "network": return { nav: "network", draw: network };
    case "system": return { nav: "system", draw: system };
    case "access": return { nav: "access", draw: access };
    default: return { nav: null, draw: async () => [el("h1", {}, "Nothing here"), el("p", {}, el("a", { href: "#/" }, "Back to the overview"))] };
  }
}

// --- Drawing, refreshing and signing in ---

const main = document.getElementById("main");
const nav = document.getElementById("nav");
const statusLine = document.getElementById("status");
const warnings = document.getElementById("warnings");
const account = document.getElementById("account");
let generation = 0;
// The password just typed at sign in, kept only until a forced change uses it.
let lastPassword = "";

function drawNav(current) {
  nav.replaceChildren(...NAV.filter(([, t]) => t !== "access" || can.admin)
    .map(([label, target]) => el("a", { href: "#/" + target, "aria-current": target === current ? "page" : undefined }, label)));
}

function clearChrome() {
  nav.replaceChildren();
  warnings.replaceChildren();
  statusLine.textContent = "";
  account.replaceChildren();
}

const alertBox = (message) => el("div", { class: "panel-notice bad", role: "alert" }, message);

function signIn(message) {
  clearChrome();
  const user = el("input", { type: "text", id: "username", autocomplete: "username", autocapitalize: "none", spellcheck: "false", required: true });
  const password = el("input", { type: "password", id: "password", autocomplete: "current-password", required: true });
  const notice = el("div", {}, message && alertBox(message));
  const button = el("button", { class: "primary", type: "submit" }, "Sign in");
  main.replaceChildren(el("form", { class: "signin", onsubmit: async (e) => {
    e.preventDefault();
    button.disabled = true;
    try {
      const resp = await fetch(API + "/auth/login", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ username: user.value, password: password.value }),
      });
      const body = await resp.json().catch(() => ({}));
      if (resp.ok) {
        saveToken(body.access_token);
        lastPassword = body.must_change_password ? password.value : "";
        rerender();
        return;
      }
      password.value = "";
      notice.replaceChildren(alertBox(resp.status === 401 ? "That username and password weren't accepted."
        : resp.status === 429 ? "Too many failed attempts. Wait a few minutes and try again." : body.error || `The appliance answered with status ${resp.status}`));
      password.focus();
    } catch (err) {
      notice.replaceChildren(alertBox("Can't reach the appliance: " + err.message));
    }
    button.disabled = false;
  } },
    el("h1", {}, "Sign in"), notice,
    el("label", { for: "username" }, "Username"), user,
    el("label", { for: "password" }, "Password"), password,
    button));
  user.focus();
}

// A password someone was given (the bootstrap one, or one an admin reset) has to be replaced before anything else.
function changePasswordScreen() {
  clearChrome();
  const current = el("input", { type: "password", id: "current", autocomplete: "current-password", required: true, value: lastPassword });
  const next = el("input", { type: "password", id: "new", autocomplete: "new-password", minlength: 8, required: true });
  const again = el("input", { type: "password", id: "again", autocomplete: "new-password", minlength: 8, required: true });
  const notice = el("div", {});
  const button = el("button", { class: "primary", type: "submit" }, "Set password");
  main.replaceChildren(el("form", { class: "signin", onsubmit: async (e) => {
    e.preventDefault();
    if (next.value !== again.value) { notice.replaceChildren(alertBox("The new passwords don't match.")); return; }
    button.disabled = true;
    try {
      const r = await send("POST", "/auth/password", { current_password: current.value, new_password: next.value });
      saveToken(r.access_token);
      lastPassword = "";
      notify("Password changed.");
      rerender();
      return;
    } catch (err) {
      notice.replaceChildren(alertBox(err instanceof Unauthorized ? "The current password isn't right." : err.message));
    }
    button.disabled = false;
  } },
    el("h1", {}, "Choose a new password"),
    el("p", { class: "muted" }, "This account is using a password it was given. Choose your own, of at least 8 characters, to carry on."),
    notice,
    !lastPassword && [el("label", { for: "current" }, "Current password"), current],
    el("label", { for: "new" }, "New password"), next,
    el("label", { for: "again" }, "New password again"), again,
    button,
    el("p", {}, el("a", { href: "#/", onclick: (e) => { e.preventDefault(); signOut(); } }, "Sign out instead"))));
  (lastPassword ? next : current).focus();
}

function passwordDialog() {
  openDialog({
    title: "Change your password",
    intro: "Your other sessions end.",
    submit: "Change",
    fields: [
      { name: "current", label: "Current password", secret: true, required: true },
      { name: "next", label: "New password", secret: true, required: true, hint: "At least 8 characters." },
    ],
    action: (x) => send("POST", "/auth/password", { current_password: x.current, new_password: x.next }),
    done: (r) => { saveToken(r.access_token); notify("Password changed."); },
  });
}

function signOut() {
  saveToken("");
  lastPassword = "";
  signIn();
}

function drawAccount() {
  account.replaceChildren(
    el("span", { class: "who" }, me.username.replace(/^token:/, "")),
    el("span", { class: "role" }, me.role.toLowerCase().replace("_", " ")),
    !me.username.startsWith("token:") && el("button", { id: "change-password", onclick: passwordDialog }, "Password"),
    el("button", { id: "signout", onclick: signOut }, "Sign out"));
}

async function rerender() {
  const mine = ++generation;
  const { nav: current, draw } = route();
  try {
    if (!token()) throw new Unauthorized();
    me = await api("/auth/me");
    if (me.must_change_password) throw new MustChangePassword();
    const [content, st] = await Promise.all([draw(), api("/system/status").catch(() => ({}))]);
    if (mine !== generation) return;
    drawNav(current);
    drawAccount();
    warnings.replaceChildren(...(st.warnings || []).map((w) => el("p", { class: "notice warn" }, w)));
    // Redrawing shouldn't lose someone's place, or what they're typing.
    const active = document.activeElement;
    const filter = active && active.type === "search" ? { start: active.selectionStart } : null;
    const focused = active && main.contains(active) && active.id ? active.id : "";
    main.replaceChildren(...[content].flat(Infinity).filter((n) => n instanceof Node));
    if (filter) {
      const box = main.querySelector('input[type="search"]');
      if (box) { box.focus(); box.setSelectionRange(filter.start, filter.start); }
    } else if (focused) {
      const again = document.getElementById(focused);
      if (again && !again.disabled) again.focus({ preventScroll: true });
    }
    statusLine.textContent = "Updated " + new Date().toLocaleTimeString();
  } catch (err) {
    if (mine !== generation) return;
    if (err instanceof Unauthorized) {
      const had = token();
      saveToken("");
      signIn(had ? "Your session has ended. Sign in again to carry on." : "");
      return;
    }
    if (err instanceof MustChangePassword) { changePasswordScreen(); return; }
    drawNav(current);
    // Keep what's on the screen if it's only a refresh that failed: it's still the best information there is.
    statusLine.textContent = "Can't reach the appliance: " + err.message;
    if (!main.childElementCount || main.querySelector("form.signin")) main.replaceChildren(el("div", { class: "panel-notice bad" }, err.message));
  }
}

window.addEventListener("hashchange", () => {
  screenState = {};
  main.replaceChildren();
  rerender();
  main.focus({ preventScroll: true });
});
// Not while someone's signing in or choosing a password, which would throw away what they've typed.
const signingIn = () => document.querySelector("form.signin");
setInterval(() => {
  if (!document.hidden && !signingIn()) rerender();
}, REFRESH_MS);
document.addEventListener("visibilitychange", () => {
  if (!document.hidden && !signingIn()) rerender();
});

rerender();
