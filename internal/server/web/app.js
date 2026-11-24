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

// raw sends a file as the request body (database restore).
async function raw(method, path, file) {
  const headers = { Accept: "application/json", "Content-Type": "application/octet-stream" };
  const t = token();
  if (t) headers.Authorization = "Bearer " + t;
  const resp = await fetch(API + path, { method, headers, cache: "no-store", body: file });
  if (resp.status === 401) throw new Unauthorized();
  const answer = await resp.json().catch(() => ({}));
  if (!resp.ok) throw new Error(answer.error || `The API answered with status ${resp.status}`);
  return answer;
}

// download saves what path returns as a file (it needs the session token, so not a plain link).
async function download(path, filename) {
  const t = token();
  const resp = await fetch(API + path, { headers: t ? { Authorization: "Bearer " + t } : {}, cache: "no-store" });
  if (resp.status === 401) throw new Unauthorized();
  if (!resp.ok) {
    const answer = await resp.json().catch(() => ({}));
    throw new Error(answer.error || `The API answered with status ${resp.status}`);
  }
  const url = URL.createObjectURL(await resp.blob());
  const a = el("a", { href: url, download: filename });
  document.body.append(a);
  a.click();
  a.remove();
  setTimeout(() => URL.revokeObjectURL(url), 10000);
}
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
const healthKind = (h) => ({ ONLINE: "ok", DEGRADED: "warn", RECONCILING: "warn", REBUILDING: "warn", FAULTED: "bad", OFFLINE: "bad", UNAVAILABLE: "bad" }[h] || "quiet");
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

const SPACE_KIND = { OK: "ok", WARNING: "warn", CRITICAL: "bad", FULL: "bad" };
const SPACE_HINT = {
  WARNING: "Getting full.",
  CRITICAL: "New volumes, snapshots, clones and expansions are refused until space is freed.",
  FULL: "Writes to new space are failing now. Free space immediately.",
};
const spacePill = (p) => (p.space_state ? pill(p.space_state.toLowerCase(), SPACE_KIND[p.space_state]) : el("span", { class: "muted" }, "-"));
const policyOf = (p) => ({ max_overcommit: 1, reserve_percent: 5, warning_percent: 80, critical_percent: 90, ...(p.space_policy || {}) });

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

function fileField(field) {
  const input = el("input", { type: "file", id: "field-" + field.name, required: field.required });
  return { node: input, focus: input, value: () => input.files[0] };
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
  const makers = { checks: checksField, tick: tickField, radio: radioField, lines: linesField, file: fileField };
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
      ? "The drive is handed to the data plane. With SPDK it is taken from the Linux nvme driver and polled directly, and the host can no longer see it; with the kernel data plane it stays on the nvme driver."
      : "The data plane takes the disk (SPDK opens it through io_uring). Nothing else on the host should use it afterwards.",
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
        intro: "Claiming it anyway gives the whole disk to the data plane, and everything on it is lost.",
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
    intro: "The data plane lets go of it and the Linux kernel has it to itself again.",
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

// Write cache and durability (docs/review-history.md P0-4): SPDK doesn't pass flushes through volumes, so a
// drive caching writes in volatile memory can lose acknowledged writes on power failure.
function durabilityCell(d) {
  const notes = [d.write_cache_policy === "DISABLED" && (d.write_cache_error ? "can't be turned off: " + d.write_cache_error : "kept off by Velocor"), d.power_loss_protected && "declared power-loss protected"].filter(Boolean).join(", ");
  const cache = { WRITE_BACK: "Cache on", WRITE_THROUGH: "No volatile cache" }[d.write_cache] || "Cache unknown";
  const p = { DURABLE: pill("Durable", "ok"), AT_RISK: pill("At risk", "warn"), UNKNOWN: pill("Unknown", "quiet") }[d.durability] || "-";
  return [p, el("span", { class: "sub" }, notes ? `${cache}; ${notes}` : cache)];
}

function writeCacheDialog(d) {
  openDialog({
    title: `Write cache of ${d.name || d.id}`,
    intro: "Velocor's volumes don't pass flushes down to the drives yet, so a drive that keeps writes in a volatile cache can lose writes the host was told are safe if power fails. Either turn the cache off (safe, but writes can get much slower and wear consumer drives faster) or, if the drive has power-loss protection, declare it.",
    submit: "Save",
    fields: [
      { name: "policy", kind: "radio", label: "Write cache", value: d.write_cache_policy || "", options: [
        { value: "", label: "Leave it to the drive" + (d.write_cache === "WRITE_BACK" ? " (currently on)" : "") },
        { value: "DISABLED", label: "Turn it off and keep it off" },
      ] },
      { name: "plp", kind: "tick", value: !!d.power_loss_protected, text: "This drive has power-loss protection (capacitors that flush its cache on power failure)" },
    ],
    action: (x) => send("PATCH", `/disks/${encodeURIComponent(d.id)}`, { write_cache_policy: x.policy, power_loss_protected: x.plp }),
    done: () => { notify(`Updated ${d.name || d.id}.`); rerender(); },
  });
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
      { title: "Durability", cell: (d) => (d.is_boot_device ? "-" : durabilityCell(d)), text: (d) => d.durability || "" },
      { title: "", cell: (d) => el("div", { class: "row-actions" },
        can.admin && !d.is_boot_device && el("button", { onclick: () => writeCacheDialog(d) }, "Write cache"),
        can.admin && !d.claimed && !d.is_boot_device && el("button", { onclick: () => claimDialog(d) }, "Claim"),
        can.admin && d.claimed && !d.pool_id && el("button", { class: "danger-quiet", onclick: () => releaseDialog(d) }, "Release")) },
    ], items, { empty: "No disks found. Scan for disks.", state: stateOf("disks") }),
  ];
}

// --- Pools ---

const RAID_LEVELS = [
  { value: "RAID0", label: "RAID 0: striped, no redundancy" },
  { value: "RAID1", label: "RAID 1: mirrored (2 or more disks)" },
];

async function createPoolDialog() {
  const free = (await list("/disks", "disks")).filter((d) => d.claimed && !d.pool_id && !d.is_boot_device);
  openDialog({
    title: "Create a pool",
    intro: "This builds a RAID array across the disks (SPDK RAID, or md with the kernel data plane), with its layout recorded on them so it reassembles after a restart, and a thin pool on top.",
    submit: "Create",
    fields: [
      { name: "name", label: "Name", required: true },
      { name: "level", kind: "radio", label: "RAID level", value: "RAID1", options: RAID_LEVELS },
      { name: "disks", kind: "checks", label: "Disks", options: free.map((d) => ({ value: d.id, label: `${d.name || d.id} (${d.type}, ${humanSize(d.size_bytes)})` })), empty: "No claimed disks are free. Claim some on the Disks screen first." },
      { name: "strip", label: "Strip size", when: (v) => v.level !== "RAID1", value: "64", options: ["16", "32", "64", "128", "256"].map((k) => ({ value: k, label: `${k} KiB` })) },
      { name: "cluster", label: "Chunk size", value: String(1 << 20), hint: "The unit of thin allocation (SPDK; the kernel data plane always uses 64 KiB). Smaller chunks cost more metadata and make recovery after a crash slower.",
        options: [[64 * 1024, "64 KiB"], [1 << 20, "1 MiB"], [4 << 20, "4 MiB"]].map(([v, l]) => ({ value: String(v), label: l })) },
    ],
    action: (v) => send("POST", "/pools", { name: v.name, raid_level: v.level, disk_ids: v.disks, strip_size_kb: Number(v.strip), chunk_size_bytes: Number(v.cluster) }),
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

// A pool's durability: whether a power cut can lose writes its member disks acknowledged.
const DURABILITY = {
  DURABLE: ["durable", "ok", "Every member disk writes to durable media (no volatile write cache, or it is off, or the disk is power-loss protected)."],
  AT_RISK: ["writes at risk", "warn", "A member disk has its volatile write cache on and isn't declared power-loss protected: a power cut can lose acknowledged writes. Turn the disk's cache off or declare it protected on its disk page."],
  UNKNOWN: ["unknown", "", "The write cache of a member disk could not be determined."],
};
const durabilityPill = (p) => DURABILITY[p.durability] && pill(DURABILITY[p.durability][0], DURABILITY[p.durability][1]);

function poolTable(items, volumes, options = {}, deleted = []) {
  const count = (id) => volumes.filter((v) => v.pool_id === id).length;
  // Deleted volumes keep their space until they're purged: shown, so a pool doesn't look empty.
  const trash = (id) => deleted.filter((v) => v.pool_id === id).length;
  return table([
    { title: "Name", cell: (p) => el("strong", {}, link(p.name, "pool", p.id)), text: (p) => p.name },
    { title: "Health", cell: health, text: (p) => p.health_status },
    { title: "RAID", cell: (p) => [p.raid_level, el("span", { class: "sub" }, `${(p.disk_ids || []).length} disks`)], text: (p) => p.raid_level },
    { title: "Durability", cell: (p) => durabilityPill(p) || "", text: (p) => p.durability || "" },
    { title: "Space", cell: (p) => [spacePill(p), poolBar(p)], text: (p) => p.space_state || "" },
    { title: "Committed", num: true, cell: (p) => [humanSize(p.committed_bytes || 0), el("span", { class: "sub" }, `${humanSize(p.available_bytes || 0)} more accepted`)] },
    { title: "Volumes", num: true, cell: (p) => [link(String(count(p.id)), "volumes", p.id), trash(p.id) > 0 && el("span", { class: "sub" }, `${trash(p.id)} deleted`)] },
  ], items, { empty: "No pools yet.", state: stateOf("pools"), ...options });
}

async function pools() {
  const [items, volumes, deleted] = await Promise.all([list("/pools", "pools"), list("/volumes", "volumes"),
    list("/deleted-volumes", "volumes").catch((err) => { if (err instanceof Unauthorized) throw err; return []; })]);
  return [
    el("h1", {}, "Pools"),
    el("div", { class: "toolbar" },
      action(can.admin, { class: "primary", id: "create-pool", onclick: createPoolDialog }, "Create pool"),
      action(can.admin, { id: "import-pool", onclick: importPoolDialog }, "Import pool")),
    poolTable(items, volumes, {}, deleted),
  ];
}

async function importPoolDialog() {
  const found = (await api("/pool-imports")).candidates || [];
  if (!found.length) {
    notify("No pools found on the unclaimed disks.");
    return;
  }
  const label = (c) => `${c.pool_id}: ${c.raid_level}, ${c.present} of ${c.members} disks, ${humanSize(c.size_bytes)}${c.reason ? ` (${c.reason})` : ""}`;
  const first = found.find((c) => c.importable);
  openDialog({
    title: "Import a pool",
    intro: "These disks carry a Velocor pool this appliance doesn't know: after a reinstall, a lost boot disk, or disks moved from another appliance. Importing claims them without wiping and brings the pool back from the copy of its configuration kept on it. Users and API tokens aren't on the pool; they stay as they are here.",
    submit: "Import",
    fields: [
      { name: "pool", kind: "radio", label: "Pool", value: first && first.pool_id, options: found.map((c) => ({ value: c.pool_id, label: label(c), disabled: !c.importable })) },
      { name: "name", label: "New name (only if its name is taken here)", placeholder: "keep its name" },
      { name: "degraded", kind: "tick", text: "Start it without its missing disks (RAID1 only; replace them afterwards)",
        when: (v) => { const c = found.find((x) => x.pool_id === v.pool); return c && !c.complete; } },
    ],
    action: (v) => {
      if (!v.pool) throw new Error("Choose a pool.");
      return send("POST", "/pool-imports", { pool_id: v.pool, name: v.name || undefined, allow_degraded: !!v.degraded });
    },
    done: importResult,
  });
}

function importResult(r) {
  const from = r.source === "manifest"
    ? `from the copy on the pool (generation ${r.generation}, written ${humanTime(r.written_at)})`
    : "from the data plane only: the pool had no readable copy of its configuration";
  openDialog({
    title: `Imported ${r.pool.name}`,
    intro: `Recovered ${from}: ${r.volumes} volumes, ${r.snapshots} snapshots; ${r.exports_enabled} exports served, ${r.exports_disabled} restored disabled.`,
    warning: (r.warnings || []).length > 0 && el("div", { class: "panel-notice warn" }, el("ul", {}, r.warnings.map((w) => el("li", {}, w)))),
  });
  location.hash = hash("pool", r.pool.id);
}

const MEMBER_KIND = { ACTIVE: "ok", REBUILDING: "warn", FAILED: "bad", MISSING: "bad" };

// What a pool's lost data held (lost_data, from the reconciler), a line each.
const LOST_KIND = {
  "thin-metadata": "the thin pool's metadata (the volumes' block mappings)",
  "pool-metadata": "the on-pool configuration copy (rewritten on the next change)",
  "lvm-metadata": "LVM's metadata (the pool may not be found after a restart)",
  unlocated: "somewhere in the pool (not worked out for RAID0 over disks of different sizes)",
};

function lostList(p, vols) {
  const byID = Object.fromEntries(vols.map((v) => [v.id, v]));
  const items = (p.lost_data || []).map((l) => {
    const size = humanSize(l.length_bytes);
    if (l.volume_id) {
      const v = byID[l.volume_id];
      return el("li", {}, ["Volume ", link(v ? v.name : l.volume_id, "volume", l.volume_id), `: ${size} from ${humanSize(l.offset_bytes)}`]);
    }
    if (l.snapshot_id) return el("li", {}, ["Snapshot ", mono(l.snapshot_id), `: ${size} from ${humanSize(l.offset_bytes)}`]);
    return el("li", {}, `${LOST_KIND[l.kind] || l.name || l.kind}: ${size}`);
  });
  return items.length > 0 && el("ul", { class: "sub" }, items);
}

function memberState(m) {
  const state = pill(m.state.toLowerCase(), MEMBER_KIND[m.state]);
  // md's bad-block log: ranges this disk couldn't read or rewrite, read from another copy since.
  const bad = m.bad_block_bytes > 0 && el("span", { class: "sub" }, `${humanSize(m.bad_block_bytes)} of bad blocks recorded`);
  if (m.state !== "REBUILDING") return bad ? [state, bad] : state;
  const pct = Math.min(m.rebuild_percent || 0, 100);
  const fill = el("div");
  fill.style.width = pct + "%";
  return [state, el("span", { class: "sub" }, `${pct}% rebuilt`), el("div", { class: "bar", role: "img", "aria-label": `${pct} percent rebuilt` }, fill)];
}

async function replaceDiskDialog(p, m) {
  const working = m.state === "ACTIVE" || m.state === "REBUILDING";
  const candidates = (await list("/disks", "disks")).filter((d) => !d.pool_id && !d.is_boot_device);
  if (!candidates.length) {
    notify("No free disks: add a disk, or release one, first.");
    return;
  }
  openDialog({
    title: m.disk_id ? `Replace ${m.disk_id} in ${p.name}` : `Fill slot ${m.slot} of ${p.name}`,
    intro: "The new disk is added to the RAID and rebuilt from the other disks; the pool serves I/O meanwhile. A disk that isn't claimed yet is wiped first.",
    warning: working && el("div", { class: "panel-notice warn" }, "This disk still works. Replacing it removes it first, so the pool has no redundancy until the rebuild completes."),
    submit: "Replace",
    danger: working,
    fields: [
      { name: "disk", kind: "radio", label: "New disk", options: candidates.map((d) => ({ value: d.id, label: `${d.name || d.id} (${d.type}, ${humanSize(d.size_bytes)}${d.claimed ? ", claimed" : ""})` })) },
      working && { name: "ok", kind: "tick", text: "I understand the pool runs without redundancy until the rebuild completes" },
    ].filter(Boolean),
    action: (v) => {
      if (!v.disk) throw new Error("Choose the new disk.");
      if (working && !v.ok) throw new Error("Confirm that the pool may run without redundancy.");
      return send("POST", "/pools/" + encodeURIComponent(p.id) + "/replace",
        { new_disk_id: v.disk, ...(m.disk_id ? { old_disk_id: m.disk_id } : {}), allow_degraded: working });
    },
    done: () => { notify(`Rebuilding ${p.name}.`); rerender(); },
  });
}

function startDegradedDialog(p) {
  confirmDialog({
    title: `Start ${p.name} without its missing disk`,
    intro: p.status_message,
    warning: el("div", { class: "panel-notice warn" }, "The missing disk is dropped from the RAID: if it comes back, Velocor leaves it out until you release it or reuse it as a replacement (which rebuilds it). Use this when the disk is dead; if it's only disconnected, reconnect it instead. Afterwards, replace it to restore redundancy."),
    submit: "Start degraded",
    confirmText: p.name,
    action: () => send("POST", "/pools/" + encodeURIComponent(p.id) + "/start-degraded", { confirm: p.name }),
    done: () => { notify(`Started ${p.name} degraded.`); rerender(); },
  });
}

function releasePoolDiskDialog(p, did) {
  confirmDialog({
    title: `Release ${did} from ${p.name}`,
    intro: "The disk left this pool's RAID. Releasing it wipes the start and end of the disk so it can't be mistaken for a member, and returns it to the free disks.",
    submit: "Release and wipe",
    action: () => send("DELETE", "/pools/" + encodeURIComponent(p.id) + "/disks/" + encodeURIComponent(did)),
    done: (r) => { notify(r.wiped ? `Released and wiped ${did}.` : `Released ${did}; it could not be wiped, wipe it before reusing it.`); rerender(); },
  });
}

function spacePolicyDialog(p) {
  const cur = policyOf(p);
  openDialog({
    title: `Space policy of ${p.name}`,
    intro: "Velocor refuses new volumes, snapshots, clones and expansions that would commit more than the pool may hold. Committed counts every volume as if fully written, plus the space snapshots hold.",
    warning: el("div", { class: "panel-notice warn" }, "Overcommit above 1× lets thin volumes promise more than the pool holds. If they really fill it, every thin volume in the pool gets write errors at the same time."),
    submit: "Save",
    fields: [
      { name: "overcommit", label: "Overcommit limit (× usable capacity)", value: String(cur.max_overcommit), required: true, hint: "1 means everything committed fits." },
      { name: "reserve", label: "Reserve (% never committed)", value: String(cur.reserve_percent), required: true },
      { name: "warning", label: "Warning at (% used)", value: String(cur.warning_percent), required: true },
      { name: "critical", label: "Critical at (% used); growth refused", value: String(cur.critical_percent), required: true },
    ],
    action: (v) => {
      const n = (x) => { const f = Number(x); if (!Number.isFinite(f)) throw new Error("Enter numbers."); return f; };
      return send("PATCH", "/pools/" + encodeURIComponent(p.id), { space_policy: {
        max_overcommit: n(v.overcommit), reserve_percent: n(v.reserve), warning_percent: n(v.warning), critical_percent: n(v.critical) } });
    },
    done: () => { notify("Saved."); rerender(); },
  });
}

// A pool's scrub (docs/review-history.md P2-2): the current or last run and what it found.
function scrubFact(p, sv) {
  if (!sv) return null;
  const sc = sv.scrub;
  const t = (sc && sc.total) || {};
  const found = [
    t.repaired_blocks && `${t.repaired_blocks} unreadable block(s) rewritten from a good copy`,
    t.mismatched_blocks && `${t.mismatched_blocks} differing block(s)${t.mismatch_repaired_blocks ? `, ${t.mismatch_repaired_blocks} made equal` : ", not changed (check only)"}`,
    t.unrecoverable_blocks && `${t.unrecoverable_blocks} block(s) no copy could supply: data there is lost`,
    t.write_errors && `${t.write_errors} repair write(s) failed; the disk was failed out`,
  ].filter(Boolean);
  const tone = t.unrecoverable_blocks || t.write_errors ? "bad" : found.length ? "warn" : "ok";
  const running = sc && sc.state === "RUNNING";
  const state = !sc ? el("span", { class: "muted" }, "never scrubbed")
    : [pill(running ? `running, ${(sc.percent || 0).toFixed(1)}%` : sc.state.toLowerCase(), running ? "quiet" : sc.state === "FAILED" ? "bad" : sc.state === "COMPLETED" ? tone : "warn"),
      el("span", { class: "sub" }, [sc.manual ? "Started by hand" : "Scheduled", sc.repair ? "compare and repair" : "check only", `started ${humanTime(sc.started_at)}`].join(", ")),
      sc.note && el("span", { class: "sub" }, sc.note),
      el("span", { class: "sub" }, found.length ? found.join("; ") : "Nothing found so far."),
      sc.last_completed_at && el("span", { class: "sub" }, `Last complete pass ${humanTime(sc.last_completed_at)}.`)];
  return fact("Scrub", [state,
    sv.next_due && el("span", { class: "sub" }, `Next scheduled scrub due ${humanTime(sv.next_due)}.`),
    sv.unavailable && !running && el("span", { class: "sub" }, sv.unavailable),
    can.admin && (running
      ? el("button", { id: "stop-scrub", onclick: async () => { try { await send("DELETE", "/pools/" + encodeURIComponent(p.id) + "/scrub"); notify("Stopping after the current window."); rerender(); } catch (err) { notify(err.message); } } }, "Stop")
      : el("button", { id: "start-scrub", disabled: !!sv.unavailable, onclick: () => startScrubDialog(p) }, "Scrub now"))]);
}

function startScrubDialog(p) {
  openDialog({
    title: `Scrub ${p.name}`,
    intro: "Reads every disk of the pool. Mirror copies are compared; a copy a disk can't read is rewritten from a good one. Hosts keep working; their I/O to the part being checked waits briefly.",
    submit: "Start",
    fields: [
      { name: "check", kind: "tick", text: "Check only: count differing copies without making them equal" },
      { name: "limit", label: "Read limit (MB/s, 0 = unlimited; empty: the schedule's)", value: "" },
    ],
    action: (v) => {
      const body = { repair: !v.check };
      if (v.limit !== "") {
        const n = Number(v.limit);
        if (!Number.isInteger(n) || n < 0) throw new Error("Enter a whole number of MB/s, or 0 for unlimited.");
        body.max_mb_per_sec = n;
      }
      return send("POST", "/pools/" + encodeURIComponent(p.id) + "/scrub", body);
    },
    done: () => { notify("Scrub started."); rerender(); },
  });
}

async function pool(id) {
  const [p, disksAll, volumes, deletedVols, scrubView] = await Promise.all([
    api("/pools/" + encodeURIComponent(id)).then((r) => r.pool), list("/disks", "disks"), list("/volumes?pool_id=" + encodeURIComponent(id), "volumes"),
    list("/deleted-volumes?pool_id=" + encodeURIComponent(id), "volumes").catch((err) => { if (err instanceof Unauthorized) throw err; return []; }),
    api("/pools/" + encodeURIComponent(id) + "/scrub").catch((err) => { if (err instanceof Unauthorized) throw err; return null; }),
  ]);
  const diskOf = (did) => disksAll.find((d) => d.id === did) || {};
  const slots = (p.members && p.members.length ? p.members : (p.disk_ids || []).map((did, i) => ({ slot: i, disk_id: did, state: "ACTIVE" })))
    .map((m) => ({ ...m, disk: diskOf(m.disk_id) }));
  const rebuilding = slots.some((m) => m.state === "REBUILDING");
  const redundant = p.raid_level === "RAID1";
  const detached = (p.detached_disk_ids || []).map((did) => ({ id: did, ...diskOf(did) }));
  return [
    el("p", { class: "crumb" }, el("a", { href: "#/pools" }, "Pools")),
    el("h1", {}, p.name),
    el("div", { class: "toolbar" },
      action(can.provision, { class: "primary", disabled: p.health_status === "UNAVAILABLE" || p.space_state === "CRITICAL" || p.space_state === "FULL", onclick: () => createVolumeDialog(p.id) }, "Create volume"),
      p.health_status === "UNAVAILABLE" && action(can.admin, { class: "danger", id: "start-degraded", onclick: () => startDegradedDialog(p) }, "Start degraded"),
      action(can.admin, { class: "danger-quiet", id: "delete-pool", disabled: volumes.length > 0, onclick: () => deletePoolDialog(p) }, "Delete"),
      volumes.length > 0 && can.admin && el("span", { class: "muted" }, "It can't be deleted while it has volumes.")),
    el("dl", { class: "facts" },
      fact("Health", health(p)),
      p.unreadable_bytes > 0 && fact("Lost data", [pill(humanSize(p.unreadable_bytes), "bad"), el("span", { class: "sub" },
        "Bad blocks are recorded on every copy of this much of the pool: reading it fails until it is written again. Restore the affected volumes from backups."),
        lostList(p, [...volumes, ...deletedVols])]),
      fact("Space", [spacePill(p), poolBar(p), SPACE_HINT[p.space_state] && el("span", { class: "sub" }, SPACE_HINT[p.space_state])]),
      fact("Committed", [`${humanSize(p.committed_bytes || 0)} promised`, el("span", { class: "sub" },
        `${humanSize(p.available_bytes || 0)} more accepted. Committed counts every volume as if fully written, plus what snapshots hold.`)]),
      fact("Space policy", [
        `Overcommit up to ${policyOf(p).max_overcommit}×, ${policyOf(p).reserve_percent}% reserved; warning at ${policyOf(p).warning_percent}%, critical at ${policyOf(p).critical_percent}%`,
        can.admin && el("button", { id: "edit-space-policy", onclick: () => spacePolicyDialog(p) }, "Change")]),
      fact("RAID", `${p.raid_level}${p.raid_level !== "RAID1" ? `, ${p.strip_size_kb} KiB strips` : ""}`),
      DURABILITY[p.durability] && fact("Durability", [durabilityPill(p), el("span", { class: "sub" }, DURABILITY[p.durability][2])]),
      scrubFact(p, scrubView),
      fact("Chunk size", humanSize(p.chunk_size_bytes)),
      fact("Array", mono(p.array_name)),
      fact("Thin pool", [mono(p.thin_pool_name), el("span", { class: "sub mono" }, p.thin_pool_uuid)]),
      fact("Configuration copy", p.meta_error
        ? [pill("out of date", "warn"), el("span", { class: "sub" }, p.meta_error)]
        : p.meta_generation
          ? [pill("current", "ok"), el("span", { class: "sub" }, `Generation ${p.meta_generation}, written ${humanTime(p.meta_written_at)}. Kept on the pool, so it can be imported if this appliance's database is lost.`)]
          : el("span", { class: "muted" }, "not written yet")),
      fact("Created", humanTime(p.created_at)),
      fact("ID", mono(p.id))),
    el("h2", {}, "Disks"),
    table([
      { title: "Slot", num: true, cell: (m) => String(m.slot) },
      { title: "Disk", cell: (m) => (m.disk_id ? [el("strong", {}, m.disk.name || m.disk_id), el("span", { class: "sub mono" }, m.disk_id)] : el("span", { class: "muted" }, "empty")) },
      { title: "State", cell: memberState },
      { title: "Model", cell: (m) => m.disk.model || "-" },
      { title: "Size", num: true, cell: (m) => (m.disk.size_bytes ? humanSize(m.disk.size_bytes) : "-") },
      { title: "", cell: (m) => redundant && can.admin && !rebuilding && p.health_status !== "UNAVAILABLE" &&
        el("button", { class: m.state === "ACTIVE" ? "danger-quiet" : "primary", onclick: () => replaceDiskDialog(p, m) }, "Replace") },
    ], slots, { filter: false, empty: "No disks.", state: stateOf("pool-disks") }),
    detached.length > 0 && [
      el("h3", {}, "Disks that left the pool"),
      el("p", { class: "muted" }, "Failed, removed or replaced disks stay tied to the pool until you release them, so they can't be reused by accident."),
      table([
        { title: "Disk", cell: (d) => [el("strong", {}, d.name || d.id), el("span", { class: "sub mono" }, d.id)] },
        { title: "Health", cell: (d) => (d.health_status ? health(d) : "-") },
        { title: "", cell: (d) => can.admin && el("button", { class: "danger-quiet", onclick: () => releasePoolDiskDialog(p, d.id) }, "Release") },
      ], detached, { filter: false }),
    ],
    el("h2", {}, "Volumes"),
    volumeTable(volumes, [p]),
    deletedVols.length > 0 && el("p", { class: "muted" }, [`${deletedVols.length} deleted ${deletedVols.length === 1 ? "volume is" : "volumes are"} kept, with their space, until purged: `,
      el("a", { href: hash("volumes", p.id) }, "see Deleted volumes"), "."]),
    el("h2", {}, "History"),
    graphPanel([{ name: "pool-space", title: "Space allocated" }, { name: "pool-iops", title: "IOPS" },
      { name: "pool-throughput", title: "Throughput" }, { name: "pool-latency", title: "Average latency" }], { pool: p.name }),
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
      { name: "protect", kind: "tick", value: false, text: "Deletion protection: refuse deletes until an administrator turns it off" },
    ],
    action: (v) => send("POST", "/volumes", { name: v.name, pool_id: v.pool || poolId, size_bytes: v.size, thin_provision: v.thin, deletion_protection: v.protect }),
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

// Changed-block tracking (docs/review-history.md L-2): the regions written between two snapshots of a volume.
function changedBlocksDialog(snap, older) {
  openDialog({
    title: `What changed up to ${snap.name}`,
    intro: "Changed-block tracking lists the regions written between two snapshots of a volume: what an incremental backup copies.",
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

async function deleteVolumeDialog(v) {
  const hours = ((await api("/system/volume-retention").catch(() => null)) || { hours: 24 }).hours;
  confirmDialog({
    title: `Delete ${v.name}`,
    intro: hours
      ? `Its data is kept for ${retentionText(hours)}: until then an administrator can restore it from Deleted volumes, and it still takes up its space in the pool. After that it's destroyed. Its snapshots are kept either way.`
      : "This destroys its data at once, and can't be undone (deleted volumes aren't kept on this appliance). Its snapshots are kept, and can be cloned later.",
    submit: "Delete volume",
    confirmText: v.name,
    action: () => send("DELETE", "/volumes/" + encodeURIComponent(v.id)),
    done: () => { notify(hours ? `Deleted ${v.name}; it can be restored for ${retentionText(hours)}.` : `Deleted ${v.name}.`); location.hash = "#/volumes"; },
  });
}

function retentionText(hours) {
  return hours % 24 === 0 && hours >= 48 ? `${hours / 24} days` : hours === 1 ? "1 hour" : `${hours} hours`;
}

function protectionDialog(v) {
  const on = !v.deletion_protection;
  openDialog({
    title: on ? `Protect ${v.name}` : `Remove protection from ${v.name}`,
    intro: on
      ? "Deletes are refused, from this page and from the CSI driver, until an administrator turns protection off."
      : "It can be deleted again, including by the CSI driver when its claim is deleted.",
    submit: on ? "Protect" : "Remove protection",
    danger: !on,
    action: () => send("PUT", `/volumes/${encodeURIComponent(v.id)}/protection`, { deletion_protection: on }),
    done: () => { notify(on ? `${v.name} is protected.` : `${v.name} is no longer protected.`); rerender(); },
  });
}

function restoreVolumeDialog(v) {
  openDialog({
    title: `Restore ${v.name}`,
    intro: "It comes back as it was deleted, with its data, not exported. If a volume with its name exists now, give it another name.",
    submit: "Restore",
    fields: [{ name: "name", label: "Name", required: true, value: v.name }],
    action: (x) => send("POST", `/deleted-volumes/${encodeURIComponent(v.id)}/restore`, x.name === v.name ? {} : { name: x.name }),
    done: (r) => { notify(`Restored ${r.volume.name}.`); location.hash = hash("volume", r.volume.id); },
  });
}

function purgeVolumeDialog(v) {
  confirmDialog({
    title: `Purge ${v.name}`,
    intro: `This destroys its data now rather than at ${humanTime(v.purge_at)}, and frees its space. It can't be undone.`,
    submit: "Purge",
    confirmText: v.name,
    action: () => send("DELETE", "/deleted-volumes/" + encodeURIComponent(v.id)),
    done: () => { notify(`Purged ${v.name}.`); rerender(); },
  });
}

function deletedVolumeTable(items, poolList) {
  const poolsById = byId(poolList);
  return table([
    { title: "Name", cell: (v) => [el("strong", {}, v.name), el("span", { class: "sub mono" }, v.id)], text: (v) => `${v.name} ${v.id}` },
    { title: "Pool", cell: (v) => (poolsById[v.pool_id] ? link(poolsById[v.pool_id].name, "pool", v.pool_id) : mono(v.pool_id)) },
    { title: "Size", num: true, cell: (v) => [humanSize(v.size_bytes), el("span", { class: "sub" }, `${humanSize(v.allocated_bytes || 0)} held`)] },
    { title: "Deleted", cell: (v) => [humanTime(v.deleted_at), v.deleted_by && el("span", { class: "sub" }, `by ${v.deleted_by}`)] },
    { title: "Purged", cell: (v) => (v.purging ? pill("Purging", "warn") : humanTime(v.purge_at)) },
    { title: "", cell: (v) => el("div", { class: "row-actions" },
      action(can.admin && !v.purging, { onclick: () => restoreVolumeDialog(v) }, "Restore"),
      action(can.admin, { class: "danger-quiet", onclick: () => purgeVolumeDialog(v) }, "Purge")) },
  ], items, { empty: "No deleted volumes.", state: stateOf("deleted-volumes") });
}

function volumeTable(items, poolList, options = {}) {
  const poolsById = byId(poolList);
  return table([
    { title: "Name", cell: (v) => [el("strong", {}, link(v.name, "volume", v.id)), v.parent_snapshot_id && el("span", { class: "sub" }, "clone of a snapshot")], text: (v) => `${v.name} ${v.id} ${labelsText(v.labels)}` },
    { title: "Health", cell: health, text: (v) => v.health_status },
    { title: "Pool", cell: (v) => (poolsById[v.pool_id] ? link(poolsById[v.pool_id].name, "pool", v.pool_id) : mono(v.pool_id)), text: (v) => (poolsById[v.pool_id] || {}).name || "" },
    { title: "Size", num: true, cell: (v) => [humanSize(v.size_bytes), el("span", { class: "sub" }, v.thin_provision ? "thin" : "thick")] },
    { title: "Allocated", num: true, cell: (v) => humanSize(v.allocated_bytes || 0) },
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
  const q = poolId ? "?pool_id=" + encodeURIComponent(poolId) : "";
  const [items, poolList, deleted] = await Promise.all([list("/volumes" + q, "volumes"), list("/pools", "pools"), list("/deleted-volumes" + q, "volumes")]);
  const exported = items.filter((v) => v.attachment_state === "BLOCK_EXPORT").length;
  return [
    el("h1", {}, poolId ? `Volumes in ${(poolList.find((p) => p.id === poolId) || {}).name || poolId}` : "Volumes"),
    el("div", { class: "toolbar" }, poolPicker(poolList, poolId),
      poolList.length
        ? action(can.provision, { class: "primary", id: "create-volume", onclick: () => createVolumeDialog(poolId) }, "Create volume")
        : el("span", { class: "muted" }, ["Create a pool to make volumes: see ", el("a", { href: "#/pools" }, "Pools")])),
    volumeTable(items, poolList),
    items.length > 0 && el("p", { class: "count" }, `${exported} of ${items.length} are exported.`),
    deleted.length > 0 && [
      el("h2", { id: "deleted-volumes" }, "Deleted volumes"),
      el("p", { class: "muted" }, "Deleted volumes keep their data, and their space in the pool, until they're purged. An administrator can restore one until then, or purge it now to free the space."),
      deletedVolumeTable(deleted, poolList),
    ],
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
      action(can.admin, { id: "protect-volume", onclick: () => protectionDialog(v) }, v.deletion_protection ? "Unprotect" : "Protect"),
      action(can.provision, { class: "danger-quiet", id: "delete-volume", disabled: exps.length > 0 || v.deletion_protection, onclick: () => deleteVolumeDialog(v) }, "Delete"),
      exps.length > 0 && can.provision && el("span", { class: "muted" }, "It can't be deleted while it's exported."),
      !exps.length && v.deletion_protection && can.provision && el("span", { class: "muted" }, "It can't be deleted while it's protected.")),
    el("dl", { class: "facts" },
      fact("Health", health(v)),
      fact("Space", usage ? [bar(usage.allocated_bytes, usage.size_bytes), el("span", { class: "sub" }, "Includes what it shares with its snapshots.")] : el("span", { class: "muted" }, "The data plane can't say at the moment.")),
      fact("Pool", p ? link(p.name, "pool", p.id) : mono(v.pool_id)),
      fact("Provisioning", v.thin_provision ? "Thin" : "Thick"),
      fact("Exported", exps.length ? exps.map((e) => el("div", {}, link(e.name, "export", e.id), ` (${protoName(e.protocol)})`)) : pill("Not exported", "quiet")),
      v.parent_snapshot_id && fact("Cloned from", `snapshot ${v.parent_snapshot_id}`),
      fact("Limits", qos || "None"),
      fact("Deletion protection", v.deletion_protection ? pill("On", "ok") : "Off"),
      fact("UUID", mono(v.uuid)),
      fact("Data plane name", mono(v.handle)),
      fact("Created", humanTime(v.created_at)),
      fact("Labels", labelsText(v.labels) || "-"),
      fact("ID", mono(v.id))),
    el("h2", {}, "History"),
    graphPanel([{ name: "volume-iops", title: "IOPS" }, { name: "volume-throughput", title: "Throughput" },
      { name: "volume-latency", title: "Average latency" }, { name: "volume-space", title: "Space" }],
    { volume: v.name, pool: (poolList.find((x) => x.id === v.pool_id) || {}).name || "" }),
    el("h2", {}, "Exports"),
    exportTable(exps, { [v.id]: v }, byId(groups), { filter: false, empty: "Not exported. Export it to let hosts connect." }),
    el("h2", {}, "Snapshots"),
    table([
      { title: "Name", cell: (s) => el("strong", {}, s.name), text: (s) => s.name },
      { title: "Size", num: true, cell: (s) => humanSize(s.size_bytes) },
      { title: "Taken", cell: (s) => humanTime(s.created_at) },
      { title: "Group", cell: (s) => (groupOf[s.id] ? groupOf[s.id].name : "-") },
      { title: "", cell: (s) => el("div", { class: "row-actions" },
        snaps.some((o) => o.created_at < s.created_at) && el("button", { onclick: () => changedBlocksDialog(s,
          snaps.filter((o) => o.created_at < s.created_at).sort((a, b) => (a.created_at < b.created_at ? -1 : 1))) }, "Changed blocks"),
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
    intro: "This can't be undone. If volumes were cloned from it, it disappears now and its data goes once they've been deleted and purged.",
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
    intro: "Hosts that lose access are cut off at once. It's applied to the data plane before it's saved, so if it can't be applied nothing changes.",
    submit: "Save",
    danger: true,
    fields: accessFields(groups, e),
    action: (x) => send("PATCH", "/exports/" + encodeURIComponent(e.id), { allow_any_host: x.anyHost, initiator_group_ids: x.anyHost ? [] : x.groups }),
    done: () => { notify("Access saved."); rerender(); },
  });
}

function portalsDialog(e, portalList) {
  const usable = portalList.filter((p) => p.protocol === e.protocol);
  openDialog({
    title: `Portals of ${e.name}`,
    intro: "The addresses hosts reach the export on. Hosts connected through a portal you remove are cut off.",
    submit: "Save",
    danger: true,
    fields: [{ name: "portals", kind: "checks", label: "Portals", value: e.portal_ids || [],
      options: usable.map((p) => ({ value: p.id, label: `${p.name} (${p.ip_address}:${p.port})` })), empty: "No portals for this protocol. Add one on the Portals screen." }],
    action: (x) => {
      if (!x.portals.length) throw new Error("Choose at least one portal.");
      return send("PATCH", "/exports/" + encodeURIComponent(e.id), { portal_ids: x.portals });
    },
    done: () => { notify("Portals saved."); rerender(); },
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

function setExportEnabled(e, enabled) {
  const go = () => send("PATCH", "/exports/" + encodeURIComponent(e.id), { enabled });
  if (enabled) {
    go().then(() => { notify(`Enabled ${e.name}.`); rerender(); }).catch((err) => notify(err.message));
    return;
  }
  confirmDialog({
    title: `Disable ${e.name}`,
    intro: "Hosts are disconnected and can't reach the volume through this export until you enable it again. The volume and its data are not touched.",
    submit: "Disable",
    action: go,
    done: () => { notify(`Disabled ${e.name}.`); rerender(); },
  });
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
    el("div", { class: "toolbar" },
      e.disabled
        ? action(can.provision, { class: "primary", id: "enable-export", onclick: () => setExportEnabled(e, true) }, "Enable")
        : action(can.provision, { id: "disable-export", onclick: () => setExportEnabled(e, false) }, "Disable"),
      action(can.provision, { class: "danger-quiet", id: "delete-export", onclick: () => deleteExportDialog(e) }, "Delete")),
    e.disabled && el("div", { class: "panel-notice warn" }, `Not served: ${e.disabled_reason || "disabled"}.`),
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
    el("div", { class: "section-head" },
      el("h2", {}, "Portals"),
      action(can.admin, { id: "change-portals", onclick: () => portalsDialog(e, portalList) }, "Change")),
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
    intro: "An address and port that hosts connect to. The data plane starts listening when an export uses it.",
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

function interfaceDialog(i, portalsHere) {
  openDialog({
    title: `Configure ${i.name}`,
    intro: "The change is applied for a trial period: confirm it on this page from a connection that still works, or it's rolled back automatically, also if this session is cut off.",
    warning: portalsHere.length > 0 && el("div", { class: "panel-notice warn" },
      `${i.name} carries the portals ${portalsHere.map((p) => p.name).join(", ")}: changing it interrupts every host connected through them.`),
    submit: "Apply",
    danger: true,
    fields: [
      { name: "method", kind: "radio", label: "Addressing", value: i.method === "manual" ? "manual" : i.method === "disabled" ? "disabled" : "dhcp",
        options: [{ value: "dhcp", label: "DHCP" }, { value: "manual", label: "Static" }, { value: "disabled", label: "Disabled" }] },
      { name: "address", label: "Address", required: true, when: (v) => v.method === "manual", value: (i.addresses || [])[0], placeholder: "10.0.0.20/24" },
      { name: "gateway", label: "Gateway", when: (v) => v.method === "manual", value: i.gateway },
      { name: "dns", label: "DNS servers", when: (v) => v.method === "manual", value: (i.dns || []).join(", "), placeholder: "Comma separated" },
      { name: "mtu", label: "MTU", number: true, when: (v) => v.method !== "disabled", value: i.mtu ? String(i.mtu) : "", hint: "9000 for jumbo frames on a storage network." },
      { name: "timeout", label: "Seconds to confirm (60 to 600)", value: "120" },
      portalsHere.length > 0 && { name: "force", kind: "tick", text: "Change it anyway, interrupting the hosts using these portals", value: false },
    ].filter(Boolean),
    action: (x) => {
      if (portalsHere.length > 0 && !x.force) throw new Error("Tick the box to change an interface that carries portals.");
      return send("PUT", "/system/network/interfaces/" + encodeURIComponent(i.name), {
        method: x.method, address: x.address, gateway: x.gateway, dns: x.dns, mtu: Number(x.mtu) || 0,
        confirm_timeout_seconds: Number(x.timeout) || 120, force: !!x.force });
    },
    done: (r) => { notify(r.message || `Applied to ${i.name}; confirm it to keep it.`); rerender(); },
  });
}

// pendingChangeBanner asks to confirm a network change before NetworkManager rolls it back.
function pendingChangeBanner(c) {
  const left = Math.max(0, Math.round((new Date(c.deadline) - Date.now()) / 1000));
  const newURL = c.new_address && !c.old_addresses.some((a) => a.split("/")[0] === c.new_address)
    ? `${location.protocol}//${c.new_address}${location.port ? ":" + location.port : ""}/#/network` : "";
  const act = (path, msg) => async () => {
    try { await send("POST", path, { id: c.id }); notify(msg); } catch (err) { notify(err.message); }
    rerender();
  };
  return el("div", { class: "panel-notice warn", role: "alert" },
    el("strong", {}, `Confirm the change to ${c.interface}`),
    el("p", {}, `It's rolled back automatically in ${left} seconds unless confirmed. Confirm only if you can still reach the appliance the way you need to.`),
    newURL && el("p", {}, ["If this page stops answering, open ", el("a", { href: newURL }, newURL), " (sign in again there) and confirm from that page."]),
    el("div", { class: "row-actions" },
      action(can.admin, { class: "primary", id: "confirm-network", onclick: act("/system/network/confirm", "Change confirmed and saved.") }, "Confirm"),
      action(can.admin, { class: "danger-quiet", id: "rollback-network", onclick: act("/system/network/rollback", "Change rolled back.") }, "Roll back now")));
}

async function network() {
  const [net, portalList] = await Promise.all([api("/system/network"), list("/portals", "portals")]);
  const portalsOn = (i) => portalList.filter((p) => (i.addresses || []).some((a) => a.split("/")[0] === p.ip_address));
  const last = net.last_change;
  return [
    el("h1", {}, "Network"),
    !net.available && el("div", { class: "panel-notice warn" }, net.message || "Network information isn't available."),
    net.available && !net.nmcli_available && el("div", { class: "panel-notice warn" }, "NetworkManager isn't reachable, so interfaces can only be viewed."),
    net.pending_change && pendingChangeBanner(net.pending_change),
    !net.pending_change && last && last.state !== "CONFIRMED" && (Date.now() - new Date(last.finished_at)) < 3600e3 &&
      el("div", { class: "panel-notice" }, `The change to ${last.interface} was ${last.state === "EXPIRED" ? "not confirmed in time and was rolled back" : "rolled back"} ${humanTime(last.finished_at)}.`),
    el("dl", { class: "facts" }, fact("Hostname", net.hostname)),
    el("h2", {}, "Interfaces"),
    table([
      { title: "Interface", cell: (i) => [el("strong", {}, i.name), el("span", { class: "sub" }, i.type)], text: (i) => i.name },
      { title: "Link", cell: (i) => (i.link_up ? pill("Up", "ok") : pill("Down", "quiet")) },
      { title: "Addressing", cell: (i) => i.method || "-" },
      { title: "Addresses", cell: (i) => (i.addresses || []).map((a) => el("div", { class: "mono" }, a)) },
      { title: "Portals", cell: (i) => portalsOn(i).map((p) => p.name).join(", ") || "-" },
      { title: "Gateway", cell: (i) => mono(i.gateway || "-") },
      { title: "DNS", cell: (i) => (i.dns || []).join(", ") || "-" },
      { title: "MTU", num: true, cell: (i) => i.mtu },
      { title: "MAC", cell: (i) => mono(i.mac || "-") },
      { title: "", cell: (i) => el("div", { class: "row-actions" }, can.admin && net.nmcli_available && i.type !== "loopback" &&
        el("button", { disabled: !!net.pending_change, onclick: () => interfaceDialog(i, portalsOn(i)) }, "Configure")) },
    ], net.interfaces || [], { filter: false, empty: "No interfaces.", state: stateOf("ifaces") }),
  ];
}

// --- System ---

function upgradeDialog(os) {
  openDialog({
    title: "Upgrade the OS",
    intro: "bootc downloads the new image and stages it for the next boot. Nothing changes until the appliance reboots, and the current image stays available to roll back to.",
    submit: "Start upgrade",
    fields: [{ name: "image", label: "Image", placeholder: (os.booted || {}).image || "", hint: "Leave empty to update to the newest build of the image it's running." }],
    action: (x) => send("POST", "/system/upgrade", { image_ref: x.image }),
    done: () => { notify("Upgrade started: it downloads in the background; follow it below."); rerender(); },
  });
}

// upgradeJobView shows the running or last OS upgrade (docs/review-history.md P1-7).
function upgradeJobView(j) {
  const state = j.state === "RUNNING" ? pill("Downloading", "warn") : j.state === "SUCCEEDED" ? pill("Staged", "ok") : pill("Failed", "bad");
  const lines = (j.output || "").split("\n");
  return el("details", { class: "upgrade-job", open: j.state !== "SUCCEEDED" },
    el("summary", {}, "Upgrade ", state, " ", el("span", { class: "sub" },
      [j.image || "newest build", `started ${humanTime(j.started_at)} by ${j.by}`, j.finished_at && `finished ${humanTime(j.finished_at)}`].filter(Boolean).join(" · "))),
    j.error && el("div", { class: "panel-notice bad" }, j.error),
    j.state === "SUCCEEDED" && el("p", {}, j.staged ? "It takes effect at the next reboot." : "Nothing new to stage: already on the newest image."),
    el("pre", { class: "job-output" }, lines.slice(-15).join("\n")));
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
    fields: [{ name: "force", kind: "tick", text: "Reboot even if hosts are connected (they lose their storage until it's back)", value: false }],
    action: (x) => send("POST", "/system/reboot", { force: !!x.force }),
    done: () => notify("Rebooting."),
  });
}


async function system() {
  const soft = (err) => { if (err instanceof Unauthorized) throw err; return null; };
  const [st, os, containers, rebuild, backups, retention, mon, upg, scrubCfg] = await Promise.all([api("/system/status"), api("/system/os").catch(soft), api("/system/containers").catch(soft), api("/system/rebuild").catch(soft),
    can.admin ? api("/system/backups").catch(soft) : null, api("/system/volume-retention").catch(soft), api("/system/monitoring").catch(soft), api("/system/upgrade").catch(soft),
    api("/system/scrub").then((r) => r.scrub).catch(soft)]);
  const ctr = containers || { available: false };
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
            can.admin && os && os.available && el("button", { disabled: !!(upg && upg.job && upg.job.state === "RUNNING"), onclick: () => upgradeDialog(os) }, "Upgrade"),
            can.admin && os && os.rollback && el("button", { class: "danger-quiet", onclick: rollbackDialog }, "Roll back"),
            can.admin && el("button", { class: "danger-quiet", onclick: () => rebootDialog(st.node && st.node.hostname) }, "Reboot"))),
        os && os.available
          ? el("dl", { class: "facts" }, fact("Booted", dep(os.booted)), fact("Staged", dep(os.staged)), fact("Rollback", dep(os.rollback)))
          : el("div", { class: "panel-notice" }, (os && os.message) || "bootc isn't available on this host."),
        upg && upg.warning && el("div", { class: "panel-notice warn" }, upg.warning, " ",
          action(can.admin, { onclick: async () => { try { await send("DELETE", "/system/upgrade/warning"); rerender(); } catch (err) { notify(err.message); } } }, "Dismiss")),
        upg && upg.job && upgradeJobView(upg.job))),
    (rec.unmanaged_objects || []).length > 0 && [
      el("div", { class: "section-head" }, el("h2", { id: "unmanaged" }, "Not managed by Velocor")),
      el("div", { class: "panel-notice warn" }, "The data plane holds these, but no Velocor record accounts for them. Velocor leaves them alone. They may be leftovers of a failed operation, or, after the database was lost, the only copy of data: import the pool or restore a database backup if they're yours. Subsystems and targets may give hosts access that no export controls."),
      table([
        { title: "Kind", cell: (o) => o.kind },
        { title: "Name", cell: (o) => [mono(o.name), o.uuid && el("span", { class: "sub mono" }, o.uuid)], text: (o) => `${o.name} ${o.uuid || ""}` },
        { title: "Details", cell: (o) => o.detail || "-" },
        { title: "Since", cell: (o) => humanTime(o.first_seen) },
      ], rec.unmanaged_objects, { empty: "", state: stateOf("unmanaged") }),
    ],
    rebuild && [
      el("div", { class: "section-head" }, el("h2", {}, "Rebuild speed"),
        action(can.admin, { id: "rebuild-speed", onclick: () => rebuildSpeedDialog(rebuild.max_mb_per_sec) }, "Change")),
      el("p", {}, rebuild.max_mb_per_sec ? `Each rebuilding pool uses at most ${rebuild.max_mb_per_sec} MB/s.` : "Unlimited: rebuilds run as fast as the disks allow."),
      el("p", { class: "muted" }, "A limit leaves more bandwidth for hosts while a disk is rebuilt, but the pool stays without full redundancy for longer. A change applies to rebuilds that start after it; one already running keeps its speed."),
    ],
    scrubCfg && [
      el("div", { class: "section-head" }, el("h2", {}, "Scrubs"),
        action(can.admin, { id: "scrub-schedule", onclick: () => scrubScheduleDialog(scrubCfg) }, "Change")),
      el("p", {}, scrubCfg.disabled ? "Scheduled scrubs are off. Pools can still be scrubbed by hand from their page."
        : `Each pool is scrubbed every ${scrubCfg.interval_days} days, ${scrubCfg.window_start_hour === scrubCfg.window_end_hour ? "at any time" : `between ${String(scrubCfg.window_start_hour).padStart(2, "0")}:00 and ${String(scrubCfg.window_end_hour).padStart(2, "0")}:00 (appliance time)`}, ${scrubCfg.max_mb_per_sec ? `reading at most ${scrubCfg.max_mb_per_sec} MB/s` : "as fast as the disks allow"}.`),
      el("p", { class: "muted" }, "A scrub reads every disk of a pool so that a damaged copy is found and rewritten while a good one exists, not discovered during a rebuild. Mirror copies that differ (after an unclean shutdown) are made equal. One pool is scrubbed at a time; a scrub still running when the window ends pauses and resumes in the next one."),
    ],
    retention && [
      el("div", { class: "section-head" }, el("h2", {}, "Deleted volumes"),
        action(can.admin, { id: "volume-retention", onclick: () => retentionDialog(retention.hours) }, "Change")),
      el("p", {}, retention.hours ? `A deleted volume is kept for ${retentionText(retention.hours)}, then purged.` : "Deleted volumes are destroyed at once."),
      el("p", { class: "muted" }, "While kept, a deleted volume can be restored by an administrator, and it still takes up its space in the pool. A change applies to volumes deleted after it."),
    ],
    mon && mon.available && monitoringSection(mon.monitoring),
    backups && backupSection(backups),
    el("div", { class: "section-head" }, el("h2", {}, "Containers")),
    el("p", { class: "muted" }, "The containers are part of the operating system image, pinned to exact versions. They change only when you upgrade the operating system."),
    !ctr.available
      ? el("div", { class: "panel-notice warn" }, ctr.message || "podman isn't available on this host.")
      : table([
        { title: "Unit", cell: (c) => [el("strong", {}, c.unit), el("span", { class: "sub mono" }, c.image)] },
        { title: "Running", cell: (c) => c.running_status || "-" },
      ], ctr.containers || [], { filter: false, empty: "No containers found." }),
  ];
}

const BACKUP_KIND = { hourly: "Hourly", daily: "Daily", "pre-upgrade": "Before an upgrade", "pre-restore": "Before a restore", manual: "Taken by hand" };

function backupSection(b) {
  const stamp = () => new Date().toISOString().replace(/[-:]/g, "").slice(0, 15) + "Z";
  return [
    el("div", { class: "section-head" }, el("h2", {}, "Configuration backups"),
      el("div", { class: "row-actions" },
        el("button", { id: "backup-now", onclick: async () => {
          try { const r = await send("POST", "/system/backups"); notify(`Backed up as ${r.backup.name}.`); rerender(); } catch (err) { notify(err.message); }
        } }, "Back up now"),
        el("button", { id: "backup-download", onclick: () => download("/system/backup", `velocor-${stamp()}.db`).catch((err) => notify(err.message)) }, "Download"),
        el("button", { class: "danger-quiet", id: "restore-file", onclick: restoreFileDialog }, "Restore from a file"))),
    el("p", { class: "muted" }, `Copies of the appliance's database: hourly for a day, daily for a week, and before every upgrade and restore, in ${b.directory} on the boot disk. Each pool also keeps a copy of its own configuration, for when the boot disk is lost. A backup holds password hashes and secrets: keep downloads safe.`),
    table([
      { title: "Taken", cell: (x) => [humanTime(x.created_at), el("span", { class: "sub mono" }, x.name)], text: (x) => x.name },
      { title: "Kind", cell: (x) => BACKUP_KIND[x.kind] || x.kind },
      { title: "Size", num: true, cell: (x) => humanSize(x.size_bytes) },
      { title: "", cell: (x) => el("div", { class: "row-actions" },
        el("button", { onclick: () => download("/system/backup?name=" + encodeURIComponent(x.name), x.name).catch((err) => notify(err.message)) }, "Download"),
        el("button", { class: "danger-quiet", onclick: () => restoreDialog(x) }, "Restore")) },
    ], b.backups || [], { empty: "No backups yet; the first is taken within minutes of velod starting.", state: stateOf("backups") }),
  ];
}

const RESTORE_WARNING = "This replaces the whole configuration (pools, volumes, exports, access control, users and tokens) with the backup's, then restarts the control plane: sign in again afterwards. The current database is backed up first. Data on the pools isn't touched, but objects created after the backup are no longer known.";

function afterRestore() {
  notify("Restoring: the control plane restarts. Sign in again in a few seconds.");
  setTimeout(() => { saveToken(""); location.reload(); }, 4000);
}

function restoreDialog(x) {
  confirmDialog({
    title: `Restore the backup of ${humanTime(x.created_at)}`,
    intro: RESTORE_WARNING,
    submit: "Restore",
    confirmText: "restore",
    action: () => send("POST", "/system/restore?name=" + encodeURIComponent(x.name)),
    done: afterRestore,
  });
}

function restoreFileDialog() {
  openDialog({
    title: "Restore from a file",
    intro: RESTORE_WARNING,
    danger: true,
    submit: "Restore",
    confirmText: "restore",
    fields: [{ name: "file", kind: "file", label: "Database file", required: true }],
    action: (v) => {
      if (!v.file) throw new Error("Choose the file.");
      return raw("POST", "/system/restore", v.file);
    },
    done: afterRestore,
  });
}

function monitoringSection(m) {
  const svc = (s) => (s.ready ? pill("Running", "ok") : s.active ? pill("Starting", "warn") : pill(m.enabled ? "Not running" : "Off", m.enabled ? "bad" : ""));
  return [
    el("div", { class: "section-head" }, el("h2", {}, "Monitoring and alerts"),
      action(can.admin, { id: "monitoring", onclick: () => monitoringDialog(m.enabled) }, m.enabled ? "Turn off" : "Turn on")),
    el("p", {}, m.enabled
      ? `Built-in Prometheus and Alertmanager are on. They keep ${m.retention_days} days of history (at most ${humanSize(m.retention_bytes)}) and evaluate Velocor's alert rules.`
      : "Built-in monitoring is off. Turn it on to evaluate Velocor's alert rules and keep history on the appliance, or scrape /metrics with your own Prometheus."),
    m.enabled && el("dl", { class: "facts" }, fact("Prometheus", svc(m.prometheus)), fact("Alertmanager", svc(m.alertmanager))),
    el("p", { class: "muted" }, "Alerts sent from the appliance can't report that the appliance itself is down: also monitor it from outside (see docs/monitoring.md)."),
  ];
}

function monitoringDialog(enabled) {
  openDialog({
    title: enabled ? "Turn off monitoring" : "Turn on monitoring",
    intro: enabled
      ? "Prometheus and Alertmanager stop, and no alerts are evaluated until monitoring is turned on again. Their history stays on disk."
      : `Starts Prometheus and Alertmanager on the appliance, listening only on localhost. They use some memory and CPU, kept off the cores the data plane polls on (SPDK), and up to 2 GB of the boot disk for history.`,
    submit: enabled ? "Turn off" : "Turn on",
    danger: enabled,
    fields: [],
    action: () => send("PUT", "/system/monitoring", { enabled: !enabled }),
    done: () => { notify(enabled ? "Monitoring is off." : "Monitoring is on."); rerender(); },
  });
}

function retentionDialog(current) {
  openDialog({
    title: "Keep deleted volumes",
    intro: "How long a deleted volume's data is kept, so it can be restored, before it is destroyed. 0 destroys it at once.",
    submit: "Save",
    fields: [{ name: "hours", label: "Hours (0 to 720)", value: String(current ?? 24), required: true }],
    action: (v) => {
      const n = Number(v.hours);
      if (!Number.isInteger(n) || n < 0 || n > 720) throw new Error("Enter a whole number of hours from 0 to 720.");
      return send("PUT", "/system/volume-retention", { hours: n });
    },
    done: () => { notify("Saved."); rerender(); },
  });
}

function scrubScheduleDialog(cur) {
  openDialog({
    title: "Scrub schedule",
    submit: "Save",
    fields: [
      { name: "enabled", kind: "tick", text: "Scrub pools on a schedule", value: !cur.disabled },
      { name: "interval", label: "Every (days)", value: String(cur.interval_days), required: true },
      { name: "start", label: "Window start (hour, 0-23, appliance time)", value: String(cur.window_start_hour), required: true },
      { name: "end", label: "Window end (hour, 0-23; same as start: any time)", value: String(cur.window_end_hour), required: true },
      { name: "limit", label: "Read limit per pool (MB/s, 0 = unlimited)", value: String(cur.max_mb_per_sec), required: true },
    ],
    action: (v) => {
      const n = (x, lo, hi, what) => { const f = Number(x); if (!Number.isInteger(f) || f < lo || f > hi) throw new Error(`${what} must be a whole number from ${lo} to ${hi}.`); return f; };
      return send("PUT", "/system/scrub", { disabled: !v.enabled, interval_days: n(v.interval, 1, 365, "The interval"),
        window_start_hour: n(v.start, 0, 23, "The window start"), window_end_hour: n(v.end, 0, 23, "The window end"),
        max_mb_per_sec: n(v.limit, 0, 100000, "The limit") });
    },
    done: () => { notify("Saved."); rerender(); },
  });
}

function rebuildSpeedDialog(current) {
  openDialog({
    title: "Rebuild speed",
    submit: "Save",
    fields: [{ name: "limit", label: "Limit (MB/s, 0 = unlimited)", value: String(current || 0), required: true }],
    action: (v) => {
      const n = Number(v.limit);
      if (!Number.isInteger(n) || n < 0) throw new Error("Enter a whole number of MB/s, or 0 for unlimited.");
      return send("PUT", "/system/rebuild", { max_mb_per_sec: n });
    },
    done: () => { notify("Saved."); rerender(); },
  });
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
  const node = st.node || {}, spdk = st.data_plane || {}, rec = st.reconciler || {};
  const unhealthy = [
    ...poolList.map((x) => ["Pool", "pool", x]), ...vols.map((x) => ["Volume", "volume", x]), ...exps.map((x) => ["Export", "export", x]),
  ].filter(([, , x]) => x.health_status && x.health_status !== "ONLINE");
  const badDisks = diskList.filter((d) => d.claimed && d.health_status && d.health_status !== "ONLINE");
  // SPDK names a volume's device by its UUID, the kernel data plane by its handle (vg/lv).
  const volByDevice = Object.fromEntries(vols.flatMap((v) => [[v.uuid, v], v.handle && [v.handle, v]].filter(Boolean)));
  const rates = io && ioRates(io.stats || []);
  const volRates = (rates || []).filter((r) => volByDevice[r.name]).map((r) => ({ ...r, vol: volByDevice[r.name] }));
  return [
    el("h1", {}, "Overview"),
    !spdk.connected && el("div", { class: "panel-notice bad" }, `The data plane isn't reachable (${spdk.endpoint || "unknown"}), so nothing is being served.`),
    spdk.connected && spdk.restoring && el("div", { class: "panel-notice warn" }, "The data plane has just started: velod is restoring pools, volumes and exports. Hosts reconnect once their exports are back."),
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
    el("h2", {}, "History"),
    graphPanel([{ name: "appliance-iops", title: "IOPS, all pools" }, { name: "appliance-throughput", title: "Throughput, all pools" }]),
    el("h2", {}, "This appliance"),
    el("dl", { class: "facts" },
      fact("Hostname", node.hostname), fact("Node ID", mono(node.node_id)),
      fact("Version", `${node.version || "-"}${node.commit ? ` (${String(node.commit).slice(0, 12)})` : ""}`),
      fact("Data plane", spdk.connected ? [pill("Connected", "ok"), " ", node.data_plane_version] : pill("Not reachable", "bad")),
      fact("Hugepages", `${spdk.hugepages_free_mb ?? "-"} of ${spdk.hugepages_total_mb ?? "-"} MiB free`),
      fact("Up for", humanDuration(node.uptime_seconds)),
      fact("Reconciled", [humanTime(rec.last_reconciled_at), el("span", { class: "sub" }, `${rec.drift_repaired_count || 0} repairs so far`)])),
  ];
}

// --- Routing ---

let screenState = {};
const stateOf = (key) => (screenState[key] ||= { text: "", page: 0 });


// ---- History graphs (built-in monitoring, docs/review-history.md P1-1) ----------------------

const GRAPH_RANGES = ["1h", "6h", "24h", "7d", "15d"];
let graphRange = (() => { try { return localStorage.getItem("velocor-graph-range") || "6h"; } catch { return "6h"; } })();

function graphValue(unit, v) {
  if (v == null || !Number.isFinite(v)) return "-";
  switch (unit) {
    case "ratio": return (v * 100).toFixed(v < 0.1 ? 1 : 0) + "%";
    case "bytes": return humanSize(v);
    case "bytes_per_sec": return perSecond(v);
    case "iops": return Math.round(v).toLocaleString() + "/s";
    case "seconds": return v < 0.001 ? (v * 1e6).toFixed(0) + " µs" : v < 1 ? (v * 1000).toFixed(v < 0.01 ? 2 : 1) + " ms" : v.toFixed(2) + " s";
    default: return String(v);
  }
}

// svgChart draws lines [{name, points:[{t,v}]}] as an SVG line chart.
function svgChart(title, unit, lines) {
  const NS = "http://www.w3.org/2000/svg";
  const W = 600, H = 170, L = 8, R = 8, T = 10, B = 22;
  const all = lines.flatMap((l) => l.points);
  const svg = document.createElementNS(NS, "svg");
  svg.setAttribute("viewBox", `0 0 ${W} ${H}`);
  svg.setAttribute("class", "chart");
  svg.setAttribute("role", "img");
  svg.setAttribute("aria-label", title);
  const mk = (tag, attrs, text) => { const n = document.createElementNS(NS, tag); for (const [k, v] of Object.entries(attrs)) n.setAttribute(k, v); if (text != null) n.textContent = text; return n; };
  if (!all.length) {
    svg.append(mk("text", { x: W / 2, y: H / 2, "text-anchor": "middle", class: "chart-empty" }, "No data yet"));
  } else {
    const t0 = Math.min(...all.map((p) => p.t)), t1 = Math.max(...all.map((p) => p.t));
    let vmax = Math.max(...all.map((p) => p.v));
    if (unit === "ratio") vmax = Math.max(vmax, 1);
    if (!(vmax > 0)) vmax = 1;
    const x = (t) => L + (t1 > t0 ? (t - t0) / (t1 - t0) : 0.5) * (W - L - R);
    const y = (v) => T + (1 - v / vmax) * (H - T - B);
    for (const f of [0, 0.5, 1]) {
      svg.append(mk("line", { x1: L, x2: W - R, y1: y(vmax * f), y2: y(vmax * f), class: "chart-grid" }));
      svg.append(mk("text", { x: L + 2, y: y(vmax * f) - 3, class: "chart-axis" }, graphValue(unit, vmax * f)));
    }
    const fmt = (t) => { const d = new Date(t * 1000); return t1 - t0 > 2 * 86400 ? d.toLocaleDateString(undefined, { month: "short", day: "numeric" }) : d.toLocaleTimeString(undefined, { hour: "2-digit", minute: "2-digit" }); };
    svg.append(mk("text", { x: L, y: H - 6, class: "chart-axis" }, fmt(t0)), mk("text", { x: W - R, y: H - 6, "text-anchor": "end", class: "chart-axis" }, fmt(t1)));
    lines.forEach((l, i) => {
      // Break the line where samples are missing (more than 3 steps apart).
      const pts = l.points;
      const gap = pts.length > 1 ? 3 * Math.min(...pts.slice(1).map((p, j) => p.t - pts[j].t)) : Infinity;
      let d = "";
      pts.forEach((p, j) => { d += (j === 0 || p.t - pts[j - 1].t > gap ? "M" : "L") + x(p.t).toFixed(1) + "," + y(p.v).toFixed(1); });
      if (d) svg.append(mk("path", { d, class: `chart-line chart-line-${i}` }));
    });
  }
  const last = (l) => (l.points.length ? l.points[l.points.length - 1].v : null);
  return el("figure", { class: "graph" },
    el("figcaption", {}, el("span", {}, title),
      el("span", { class: "graph-legend" }, lines.map((l, i) => el("span", { class: `legend legend-${i}` }, `${l.name} ${graphValue(unit, last(l))}`)))),
    svg);
}

// graphPanel shows named graphs [{name, title}] for params ({pool} or {volume, pool}), with a
// range selector. Without built-in monitoring it says how to turn it on.
// Pages redraw every few seconds; graphs are fetched at most once a minute per view.
const graphCache = new Map();

function graphPanel(specs, params = {}) {
  const body = el("div", { class: "graphs" }, el("p", { class: "muted" }, "Loading history..."));
  const draw = (results) => body.replaceChildren(...results.map((r, i) => svgChart(specs[i].title, r.unit, r.lines)));
  const load = async () => {
    const q = new URLSearchParams({ ...params, range: graphRange }).toString();
    const key = specs.map((g) => g.name).join(",") + "?" + q;
    const hit = graphCache.get(key);
    if (hit) draw(hit.results);
    if (hit && Date.now() - hit.at < 60000) return;
    try {
      const results = await Promise.all(specs.map((g) => api(`/monitoring/graphs/${g.name}?${q}`)));
      graphCache.set(key, { at: Date.now(), results });
      draw(results);
    } catch (err) {
      if (err instanceof Unauthorized) throw err;
      if (hit) return; // keep the graphs already shown; the next refresh tries again
      body.replaceChildren(el("p", { class: "muted" },
        /off|isn't available/.test(err.message)
          ? ["History graphs need built-in monitoring: turn it on on the ", el("a", { href: "#/system" }, "System"), " page."]
          : `History isn't available: ${err.message}`));
    }
  };
  const buttons = GRAPH_RANGES.map((r) => el("button", { class: r === graphRange ? "range on" : "range", onclick: () => {
    graphRange = r;
    try { localStorage.setItem("velocor-graph-range", r); } catch { /* per-viewer convenience only */ }
    buttons.forEach((b) => b.classList.toggle("on", b.textContent === r));
    load();
  } }, r));
  load();
  return el("div", { class: "graph-panel" }, el("div", { class: "range-picker", role: "group", "aria-label": "Time range" }, buttons), body);
}

// ---- Alerts (built-in monitoring) -------------------------------------------------------------

const ALERT_LABELS = ["pool", "disk", "volume", "export", "slot", "kind", "status", "state"];

async function alertsScreen() {
  const soft = (err) => { if (err instanceof Unauthorized) throw err; return { error: err }; };
  const [mon, data, rulesResp, settings] = await Promise.all([
    api("/system/monitoring").catch(soft), api("/monitoring/alerts").catch(soft), api("/monitoring/rules").catch(soft),
    can.admin ? api("/monitoring/settings").catch(soft) : null,
  ]);
  const head = el("h1", {}, "Alerts");
  if (!mon.available) return [head, el("div", { class: "panel-notice" }, "Built-in monitoring isn't available on this host. Scrape /metrics with your own Prometheus and load Velocor's alert rules (docs/monitoring.md).")];
  const m = mon.monitoring || {};
  const off = !m.enabled;
  const alerts = (data && data.alerts) || [], silences = (data && data.silences) || [];
  const a = (settings && settings.alerting) || {};
  const rules = (rulesResp && rulesResp.rules) || [];
  const sev = (s) => pill(s === "critical" ? "Critical" : s === "warning" ? "Warning" : s, s === "critical" ? "bad" : "warn");
  const where = (x) => ALERT_LABELS.filter((k) => x.labels && x.labels[k]).map((k) => `${k} ${x.labels[k]}`).join(", ");
  return [
    head,
    off && el("div", { class: "panel-notice warn" }, ["Built-in monitoring is off, so no alerts are evaluated here. Turn it on on the ", el("a", { href: "#/system" }, "System"), " page, or scrape /metrics with your own Prometheus."]),
    !off && data.error && el("div", { class: "panel-notice warn" }, `Alertmanager doesn't answer: ${data.error.message}`),
    !off && !data.error && [
      el("div", { class: "section-head" }, el("h2", {}, "Firing"),
        action(can.admin, { id: "test-alert", onclick: async () => { try { await send("POST", "/monitoring/test"); notify("Test alert sent; it shows here and goes to the receivers within a minute."); } catch (err) { notify(err.message); } } }, "Send test alert")),
      table([
        { title: "Severity", cell: (x) => sev(x.severity), text: (x) => x.severity },
        { title: "Alert", cell: (x) => [el("div", {}, x.summary || x.name), el("span", { class: "sub" }, [x.name, where(x)].filter(Boolean).join(" · "))], text: (x) => `${x.name} ${x.summary}` },
        { title: "Since", cell: (x) => humanTime(x.starts_at) },
        { title: "State", cell: (x) => (x.state === "suppressed" ? pill("Silenced", "") : pill("Active", x.severity === "critical" ? "bad" : "warn")) },
        { title: "", cell: (x) => action(can.admin && x.state !== "suppressed", { onclick: () => silenceDialog(x) }, "Silence") },
      ], alerts, { empty: "Nothing is firing.", state: stateOf("alerts") }),
      silences.length > 0 && [
        el("h2", {}, "Silences"),
        table([
          { title: "Silences", cell: (x) => x.matchers.map((mt) => `${mt.name}=${mt.value}`).join(", ") },
          { title: "Until", cell: (x) => humanTime(x.ends_at) },
          { title: "By", cell: (x) => [x.created_by, x.comment && el("span", { class: "sub" }, x.comment)] },
          { title: "", cell: (x) => action(can.admin, { onclick: async () => { try { await send("DELETE", "/monitoring/silences/" + encodeURIComponent(x.id)); notify("Silence ended."); rerender(); } catch (err) { notify(err.message); } } }, "End") },
        ], silences, { empty: "" }),
      ],
    ],
    can.admin && settings && !settings.error && [
      el("div", { class: "section-head" }, el("h2", {}, "Where alerts go")),
      el("dl", { class: "facts" },
        fact("Email", [a.email ? `${a.email.to.join(", ")} via ${a.email.smtp_host}:${a.email.smtp_port}, ${a.email.min_severity === "critical" ? "critical only" : "warnings and critical"}` : el("span", { class: "muted" }, "Not set"),
          el("button", { onclick: () => emailDialog(a.email) }, a.email ? "Change" : "Set up"),
          a.email && el("button", { class: "danger-quiet", onclick: () => removeReceiver("email", "Email") }, "Remove")]),
        fact("Webhook", [a.webhook ? `${a.webhook.url_host}, ${a.webhook.min_severity === "critical" ? "critical only" : "warnings and critical"}` : el("span", { class: "muted" }, "Not set"),
          el("button", { onclick: () => webhookDialog(a.webhook) }, a.webhook ? "Change" : "Set up"),
          a.webhook && el("button", { class: "danger-quiet", onclick: () => removeReceiver("webhook", "Webhook") }, "Remove")]),
        fact("Heartbeat", [a.heartbeat_url_set ? `Every 5 minutes to ${a.heartbeat_url_host}` : el("span", { class: "muted" }, "Not set"),
          el("button", { onclick: () => heartbeatDialog(a.heartbeat_url_set) }, a.heartbeat_url_set ? "Change" : "Set up"),
          el("span", { class: "sub" }, "A dead man's switch: a service that raises an alarm when these stop arriving, which is how you learn the appliance itself is down.")])),
      !a.email && !a.webhook && el("div", { class: "panel-notice warn" }, "No receivers are set up: alerts show on this page only."),
    ],
    el("h2", {}, "Rules"),
    el("p", { class: "muted" }, "Velocor's shipped rules (docs/monitoring.md). Delays are how long a condition must last before the alert fires."),
    table([
      { title: "Rule", cell: (r) => [el("div", {}, r.name), el("span", { class: "sub" }, r.summary.replace(/\{\{[^}]*\}\}/g, "…"))], text: (r) => `${r.name} ${r.summary}` },
      { title: "Severity", cell: (r) => sev(r.severity), text: (r) => r.severity },
      { title: "Delay", cell: (r) => (r.setting.for ? [r.setting.for, el("span", { class: "sub" }, `default ${r.default_for || "none"}`)] : r.default_for || "none") },
      { title: "Threshold", cell: (r) => (r.tunable ? thresholdText(r.tunable, r.setting.threshold ?? r.tunable.default) : "-") },
      { title: "", cell: (r) => (r.setting.disabled ? pill("Off", "") : pill("On", "ok")) },
      { title: "", cell: (r) => action(can.admin, { onclick: () => ruleDialog(r, rules) }, "Change") },
    ], rules, { empty: "No rules.", state: stateOf("rules") }),
  ];
}

function thresholdText(t, v) {
  return t.unit === "ratio" ? `${t.label} ${Math.round(v * 100)}%` : t.unit === "celsius" ? `${t.label} ${v} °C` : `${t.label} ${v}`;
}

function silenceDialog(x) {
  const matchers = [{ name: "alertname", value: x.name }, ...ALERT_LABELS.filter((k) => x.labels && x.labels[k] && k !== "status" && k !== "state").map((k) => ({ name: k, value: x.labels[k] }))];
  openDialog({
    title: "Silence alert",
    intro: `Mutes ${x.name}${matchers.length > 1 ? ` for ${matchers.slice(1).map((m) => `${m.name} ${m.value}`).join(", ")}` : ""}: it still shows here, but isn't sent. The condition isn't fixed by this.`,
    submit: "Silence",
    fields: [
      { name: "hours", label: "For how many hours", value: "4", required: true },
      { name: "comment", label: "Why (optional)", placeholder: "e.g. replacing the disk" },
    ],
    action: (v) => {
      const n = Number(v.hours);
      if (!(n > 0 && n <= 720)) throw new Error("Enter a number of hours from 1 to 720.");
      return send("POST", "/monitoring/silences", { matchers, duration_hours: n, comment: v.comment });
    },
    done: () => { notify("Silenced."); rerender(); },
  });
}

const SEVERITY_OPTIONS = [{ value: "warning", label: "Warnings and critical alerts" }, { value: "critical", label: "Critical alerts only" }];

function emailDialog(e) {
  e = e || {};
  openDialog({
    title: "Email alerts",
    submit: "Save",
    fields: [
      { name: "to", label: "Send to", kind: "lines", value: e.to, placeholder: "ops@example.com" },
      { name: "from", label: "From", value: e.from, required: true, placeholder: "velocor@example.com" },
      { name: "smtp_host", label: "SMTP server", value: e.smtp_host, required: true },
      { name: "smtp_port", label: "Port", value: String(e.smtp_port || 587), required: true },
      { name: "username", label: "User name (optional)", value: e.username },
      { name: "password", label: e.password_set ? "Password (leave empty to keep the current one)" : "Password (optional)", secret: true },
      { name: "require_tls", kind: "tick", text: "Require TLS (STARTTLS; port 465 always uses TLS)", value: e.require_tls ?? true },
      { name: "min_severity", label: "Send", options: SEVERITY_OPTIONS, value: e.min_severity || "warning" },
    ],
    action: (v) => {
      const body = { to: v.to, from: v.from, smtp_host: v.smtp_host, smtp_port: Number(v.smtp_port), username: v.username, require_tls: v.require_tls, min_severity: v.min_severity };
      if (v.password) body.password = v.password;
      return send("PUT", "/monitoring/settings", { email: body });
    },
    done: () => { notify("Saved. Use Send test alert to check delivery."); rerender(); },
  });
}

function webhookDialog(w) {
  w = w || {};
  openDialog({
    title: "Webhook alerts",
    intro: "Alerts are posted as JSON in Alertmanager's webhook format (Slack, Teams and PagerDuty accept it through their integrations or a small relay).",
    submit: "Save",
    fields: [
      { name: "url", label: w.url_set ? "URL (leave empty to keep the current one)" : "URL", secret: true, required: !w.url_set, placeholder: "https://..." },
      { name: "min_severity", label: "Send", options: SEVERITY_OPTIONS, value: w.min_severity || "warning" },
    ],
    action: (v) => send("PUT", "/monitoring/settings", { webhook: v.url ? { url: v.url, min_severity: v.min_severity } : { min_severity: v.min_severity } }),
    done: () => { notify("Saved. Use Send test alert to check delivery."); rerender(); },
  });
}

function heartbeatDialog(isSet) {
  openDialog({
    title: "Heartbeat",
    intro: "Every 5 minutes, the always-firing watchdog alert is posted to this URL. Point it at a dead man's switch service, which raises an alarm when the posts stop. Leave it empty to stop sending.",
    submit: "Save",
    fields: [{ name: "url", label: isSet ? "URL (empty stops the heartbeat)" : "URL", secret: true, placeholder: "https://..." }],
    action: (v) => send("PUT", "/monitoring/settings", { heartbeat_url: v.url }),
    done: () => { notify("Saved."); rerender(); },
  });
}

function removeReceiver(key, label) {
  confirmDialog({
    title: `Remove ${label.toLowerCase()} alerts`,
    intro: `Alerts are no longer sent by ${label.toLowerCase()}.`,
    submit: "Remove",
    action: () => send("PUT", "/monitoring/settings", { [key]: null }),
    done: () => { notify("Removed."); rerender(); },
  });
}

function ruleDialog(r, all) {
  const t = r.tunable;
  openDialog({
    title: r.name,
    intro: r.summary.replace(/\{\{[^}]*\}\}/g, "…"),
    submit: "Save",
    fields: [
      { name: "on", kind: "tick", text: "Evaluate this rule", value: !r.setting.disabled },
      { name: "for", label: `Delay (default ${r.default_for || "none"}; e.g. 10m, 2h; empty for the default)`, value: r.setting.for || "" },
      t && { name: "threshold", label: t.unit === "ratio" ? `${t.label} (%; default ${t.default * 100})` : `${t.label} (°C; default ${t.default})`,
        value: r.setting.threshold == null ? "" : String(t.unit === "ratio" ? Math.round(r.setting.threshold * 100) : r.setting.threshold) },
    ].filter(Boolean),
    action: (v) => {
      const rules = Object.fromEntries(all.filter((x) => x.name !== r.name && (x.setting.disabled || x.setting.for || x.setting.threshold != null)).map((x) => [x.name, x.setting]));
      const s = {};
      if (!v.on) s.disabled = true;
      if (v.for) s.for = v.for;
      if (t && v.threshold !== "" && v.threshold != null) {
        const n = Number(v.threshold);
        if (!Number.isFinite(n)) throw new Error("The threshold must be a number.");
        s.threshold = t.unit === "ratio" ? n / 100 : n;
      }
      if (Object.keys(s).length) rules[r.name] = s;
      return send("PUT", "/monitoring/settings", { rules });
    },
    done: () => { notify("Saved."); rerender(); },
  });
}

const NAV = [["Overview", ""], ["Disks", "disks"], ["Pools", "pools"], ["Volumes", "volumes"], ["Snapshots", "snapshots"],
  ["Exports", "exports"], ["Hosts", "hosts"], ["Portals", "portals"], ["Network", "network"], ["Alerts", "alerts"], ["System", "system"], ["Access", "access"]];

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
    case "alerts": return { nav: "alerts", draw: alertsScreen };
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
  // replaceChildren, unlike el, would show a skipped (false) child as the text "false".
  account.replaceChildren(...[
    el("span", { class: "who" }, me.username.replace(/^token:/, "")),
    el("span", { class: "role" }, me.role.toLowerCase().replace("_", " ")),
    !me.username.startsWith("token:") && el("button", { id: "change-password", onclick: passwordDialog }, "Password"),
    el("button", { id: "signout", onclick: signOut }, "Sign out")].filter(Boolean));
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
