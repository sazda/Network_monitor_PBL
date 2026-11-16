"use strict";

// Stranica „Mrežna oprema”: switch-evi, ruteri i ostala oprema koju konzola
// sama proverava pingom i čita preko SNMP-a. Koristi pomoćne funkcije iz
// app.js ($, el, api, renderPager, showPingDialog...). Podaci sa uređaja
// (nazivi, opisi portova) se prikazuju isključivo kao tekst.

const ND_PAGE_SIZE = 20;
const EV_PAGE_SIZE = 12;
const DEV_STATUS = {
  up: ["na vezi", "ok"], down: ["nije na vezi", "bad"], unknown: ["provera…", "muted"], inactive: ["neaktivan", "muted"],
};
const LOC_STATUS = {
  ok: ["✓", "sve radi", "ok"], partial: ["⚠", "neki uređaji ne rade", "warn"], backup: ["⚠", "radi preko rezervne veze", "warn"],
  down: ["✕", "nedostupna", "bad"], unknown: ["•", "provera…", "muted"],
};
const EV_ICONS = {
  down: ["✕", "bad"], up: ["✓", "ok"], loc_down: ["✕", "bad"], loc_backup: ["⚠", "warn"], loc_ok: ["✓", "ok"],
  reboot: ["↻", "warn"], snmp_error: ["⚠", "warn"], snmp_ok: ["✓", "muted"], temp_high: ["🌡", "bad"], temp_ok: ["🌡", "ok"], reload_req: ["↻", "muted"],
};
const RANGES = [["15m", "15 min"], ["1h", "1 h"], ["6h", "6 h"], ["24h", "24 h"], ["7d", "7 dana"], ["30d", "30 dana"]];

// ---------- period grafikona (zajedničko za sve kartice) ----------
// Period je ključ iz RANGES ili „c:<od>:<do>” (unix sekunde) za proizvoljan.

function validRange(r) {
  return RANGES.some(([k]) => k === r) || /^c:\d+:\d+$/.test(r || "") ? r : "24h";
}

// rangeQS: deo upita za API istorije.
function rangeQS(r) {
  if (r.startsWith("c:")) {
    const [, f, t] = r.split(":");
    return `range=custom&from=${f}&to=${t}`;
  }
  return `range=${r}`;
}

const pad2 = (n) => String(n).padStart(2, "0");
// Vrednost za <input type="datetime-local"> u lokalnom vremenu.
function toLocalInput(unix) {
  const d = new Date(unix * 1000);
  return `${d.getFullYear()}-${pad2(d.getMonth() + 1)}-${pad2(d.getDate())}T${pad2(d.getHours())}:${pad2(d.getMinutes())}`;
}
const fmtRangeTime = (unix) => new Date(unix * 1000).toLocaleString("sr-RS", { day: "2-digit", month: "2-digit", year: "numeric", hour: "2-digit", minute: "2-digit" });

// rangeBar: dugmad za period i „Proizvoljno” (od – do); set(r) menja period.
function rangeBar(cur, set) {
  cur = validRange(cur);
  const custom = cur.startsWith("c:");
  const now = Math.floor(Date.now() / 1000);
  const [cf, ct] = custom ? cur.split(":").slice(1).map(Number) : [now - 86400, now];
  const fromIn = el("input", { type: "datetime-local", value: toLocalInput(cf), max: toLocalInput(now), "aria-label": "Početak perioda" });
  const toIn = el("input", { type: "datetime-local", value: toLocalInput(ct), max: toLocalInput(now + 60), "aria-label": "Kraj perioda" });
  const msg = el("span", { class: "bad small" });
  const form = el("span", { class: "nd-custom", hidden: custom ? null : "hidden" },
    "od ", fromIn, " do ", toIn, " ",
    el("button", { type: "button", onclick: () => {
      const f = Math.floor(new Date(fromIn.value).getTime() / 1000), t = Math.floor(new Date(toIn.value).getTime() / 1000);
      if (!f || !t || t - f < 60) { msg.textContent = "Početak mora biti bar minut pre kraja."; return; }
      if (t < Math.floor(Date.now() / 1000) - 31 * 86400) { msg.textContent = "Istorija se čuva 31 dan."; return; }
      set(`c:${f}:${t}`);
    } }, "Prikaži"), " ", msg);
  return el("div", { class: "nd-range", role: "group", "aria-label": "Period grafikona" },
    el("span", { class: "muted" }, "Period:"),
    ...RANGES.map(([k, l]) => el("button", {
      type: "button", class: k === cur ? "current" : "", "aria-pressed": k === cur ? "true" : "false",
      onclick: () => set(k),
    }, l)),
    el("button", { type: "button", class: custom ? "current" : "", "aria-pressed": custom ? "true" : "false",
      onclick: () => { form.hidden = !form.hidden; } }, custom ? `${fmtRangeTime(cf)} – ${fmtRangeTime(ct)}` : "Proizvoljno…"),
    form);
}

// xlsxButton: preuzimanje prikazane istorije u Excel (url = API istorije sa upitom).
function xlsxButton(url) {
  return el("button", { type: "button", class: "linklike", title: "Istorija za izabrani period kao Excel (.xlsx)",
    onclick: () => downloadFile(`${url}&format=xlsx`, "istorija.xlsx") }, "⤓ Preuzmi (Excel)");
}

async function downloadFile(url, fallback) {
  try {
    const r = await fetch(url, { headers: { "X-Requested-With": "fetch" } });
    if (r.status === 401) { location.reload(); return; }
    if (!r.ok) throw new Error((await r.json().catch(() => ({}))).error || `HTTP ${r.status}`);
    const blob = await r.blob();
    const cd = r.headers.get("Content-Disposition") || "";
    const name = (cd.match(/filename="([^"]+)"/) || [])[1] || fallback;
    const a = el("a", { href: URL.createObjectURL(blob), download: name });
    document.body.append(a);
    a.click();
    a.remove();
    setTimeout(() => URL.revokeObjectURL(a.href), 10000);
  } catch (e) {
    setStatus(`Preuzimanje: ${e.message}`, "bad");
  }
}

let netData = { devices: [], locations: [], types: {}, snmp_configured: false };
let ndPage = 1;
let evPage = 1;
let netEvents = [];
let ndSelected = stored("nm.ndSel") || null;
let ndDetail = null; // poslednji odgovor za izabrani uređaj
let ndRange = validRange(stored("nm.ndRange"));
let ndPort = null; // ifIndex izabranog porta
let ndPortsUpOnly = stored("nm.ndPortsUp") !== "0";
let ndEditing = null; // uređaj u prozoru za izmenu (null = novi)
let ndDetailMsg = null; // poruka u detaljima (npr. rezultat provere SNMP-a)

// ---------- formatiranje ----------

function fmtBps(v) {
  if (v === null || v === undefined) return "—";
  if (v >= 1e9) return `${(v / 1e9).toFixed(2)} Gb/s`;
  if (v >= 1e6) return `${(v / 1e6).toFixed(v >= 1e8 ? 0 : 1)} Mb/s`;
  if (v >= 1e3) return `${(v / 1e3).toFixed(0)} kb/s`;
  return `${Math.round(v)} b/s`;
}

function fmtUptime(sec) {
  if (sec === undefined || sec === null) return "—";
  const d = Math.floor(sec / 86400), h = Math.floor((sec % 86400) / 3600), m = Math.floor((sec % 3600) / 60);
  if (d > 0) return `${d} d ${h} h`;
  if (h > 0) return `${h} h ${m} min`;
  return `${m} min`;
}

function fmtSince(iso) {
  if (!iso) return "";
  const t = new Date(iso);
  const sec = Math.max(0, Math.round((Date.now() - t.getTime()) / 1000));
  const today = t.toDateString() === new Date().toDateString();
  const at = today ? t.toLocaleTimeString("sr-RS", { hour: "2-digit", minute: "2-digit" })
    : t.toLocaleString("sr-RS", { day: "numeric", month: "numeric", hour: "2-digit", minute: "2-digit" });
  return `od ${at} (${fmtDuration(sec)})`;
}

const fmtSpeed = (mbps) => (!mbps ? "—" : mbps >= 1000 ? `${mbps / 1000} Gb/s` : `${mbps} Mb/s`);
const typeLabel = (t) => netData.types[t] || t;
const devPingBusy = (d) => pingBusy().get("netdev:" + d.id);
const fmtEvTime = (iso) => new Date(iso).toLocaleString("sr-RS", { day: "numeric", month: "numeric", hour: "2-digit", minute: "2-digit", second: "2-digit" });

// ---------- stranice ----------

function currentPage() {
  return { "#mreza": "net", "#serveri": "srv", "#mpls": "mpls", "#odrzavanje": "maint" }[location.hash] || "agents";
}

function showPage() {
  const page = currentPage();
  for (const n of document.querySelectorAll(".page-net")) n.hidden = page !== "net" || (n.id === "nd-detail" && !ndSelected);
  for (const n of document.querySelectorAll(".page-agents")) n.hidden = page !== "agents";
  for (const n of document.querySelectorAll(".page-srv")) n.hidden = page !== "srv" || (n.id === "srv-detail" && !srvSelected);
  for (const n of document.querySelectorAll(".page-mpls")) n.hidden = page !== "mpls";
  for (const n of document.querySelectorAll(".page-maint")) n.hidden = page !== "maint";
  for (const a of document.querySelectorAll("nav.pages a")) {
    if (a.dataset.page === page) a.setAttribute("aria-current", "page");
    else a.removeAttribute("aria-current");
  }
  if (page !== "net") stopDetailTimers();
  if (page === "srv" && typeof loadServers === "function") {
    loadServers();
    loadSrvEvents();
  }
  if (page === "maint" && typeof loadMaint === "function") loadMaint();
  for (const n of document.querySelectorAll(".page-mpls")) if (n.id === "mpls-detail") n.hidden = page !== "mpls" || typeof mplsSel === "undefined" || (!mplsSel && !mplsRouter);
  if (page !== "mpls" && typeof stopMPLSLive === "function") stopMPLSLive();
  if (page === "mpls" && typeof loadMPLS === "function") loadMPLS();
  if (page !== "maint" && typeof leaveMaint === "function") leaveMaint();
  if (page === "net") {
    loadNet();
    loadNetEvents();
    if (ndSelected) loadDetail();
  }
}

window.addEventListener("hashchange", showPage);

// ---------- spisak ----------

async function loadNet() {
  try {
    netData = await api("GET", "/api/netdev");
    netData.devices = netData.devices.filter((d) => !d.mpls_link); // CE ruteri su u kartici MPLS
  } catch (e) {
    if (currentPage() === "net") setStatus(`Mrežna oprema: ${e.message}`, "bad");
    return;
  }
  updateBadge();
  if (currentPage() !== "net") return;
  fillNetFilters();
  renderLocTiles();
  renderNetTable();
}

function updateBadge() {
  const down = netData.devices.filter((d) => d.status === "down").length;
  const locDown = netData.locations.filter((l) => l.status === "down").length;
  const backup = netData.locations.filter((l) => l.status === "backup").length;
  const b = $("net-badge");
  const n = down + backup;
  b.hidden = n === 0;
  b.textContent = `⚠ ${n}`;
  b.className = `nav-badge ${down || locDown ? "bad" : "warn"}`;
  const parts = [];
  if (down) parts.push(`${down} ${down === 1 ? "uređaj nije" : "uređaja nije"} na vezi`);
  if (backup) parts.push(`${backup} ${backup === 1 ? "lokacija radi" : "lokacije rade"} preko rezervne veze`);
  b.title = parts.join(", ");
}

function fillNetFilters() {
  const keep = (sel, items, first) => {
    const v = sel.value;
    sel.replaceChildren(el("option", { value: "" }, first), ...items.map(([k, l]) => el("option", { value: k }, l)));
    sel.value = items.some(([k]) => k === v) ? v : "";
  };
  keep($("nd-loc"), netData.locations.map((l) => [l.name, l.name]), "sve lokacije");
  const types = [...new Set(netData.devices.map((d) => d.type))].sort();
  keep($("nd-type"), types.map((t) => [t, typeLabel(t)]), "sve vrste");
}

function renderLocTiles() {
  const box = $("net-loc-tiles");
  box.replaceChildren();
  if (!netData.locations.length) {
    box.append(el("p", { class: "muted" }, "Još nema uređaja. " +
      (isAdmin() ? "Dodajte ih dugmetom „Dodaj uređaj” ili „Uvoz (CSV)”." : "Uređaje dodaje administrator.")));
    return;
  }
  const order = { down: 0, backup: 1, partial: 2, unknown: 3, ok: 4 };
  const locs = [...netData.locations].sort((a, b) => (order[a.status] - order[b.status]) || a.name.localeCompare(b.name, "sr"));
  const sel = $("nd-loc").value;
  for (const l of locs) {
    let [icon, label, cls] = LOC_STATUS[l.status] || LOC_STATUS.unknown;
    // Jedan uređaj koji ne odgovara: ne zna se da li je pao uređaj ili veza.
    if (l.status === "partial" && l.up === 0) [icon, label, cls] = ["✕", "ne odgovara", "bad"];
    const facts = [`${l.up}/${l.total} na vezi`];
    if (l.rtt_ms !== undefined) facts.push(`odziv ${fmtMs(l.rtt_ms)} ms`);
    if (l.loss_pct) facts.push(`gubitak ${l.loss_pct}%`);
    box.append(el("button", {
      type: "button", class: `loc-tile ${cls}${sel === l.name ? " selected" : ""}`,
      title: sel === l.name ? "Prikaži sve lokacije" : `Prikaži samo uređaje lokacije ${l.name}`,
      onclick: () => { $("nd-loc").value = sel === l.name ? "" : l.name; ndPage = 1; renderLocTiles(); renderNetTable(); },
    },
    el("div", { class: "lt-name" }, l.name),
    el("div", { class: `lt-status ${cls}` }, `${icon} ${label}`),
    el("div", { class: "lt-facts muted" }, facts.join(" · ")),
    l.wan ? el("div", { class: "lt-facts muted" }, `MPLS: ${l.wan} ${l.wan_up === false ? "✕" : l.wan_up ? "✓" : ""}`) : null,
    l.status !== "ok" && l.status !== "unknown" && l.since ? el("div", { class: "lt-facts muted" }, fmtSince(l.since)) : null));
  }
}

function devMatches(d) {
  const loc = $("nd-loc").value, type = $("nd-type").value;
  if (loc && d.location_name !== loc) return false;
  if (type && d.type !== type) return false;
  if ($("nd-problems").checked && !(d.status === "down" || (d.snmp_info && !d.snmp_info.ok))) return false;
  const words = $("nd-search").value.toLowerCase().split(/\s+/).filter(Boolean);
  if (!words.length) return true;
  const s = d.snmp_info || {};
  const hay = [d.name, d.ip, d.place, d.location_name, d.note, s.sys_name, s.model].filter(Boolean).join(" ").toLowerCase();
  return words.every((w) => hay.includes(w));
}

function snmpCell(d) {
  if (!d.snmp) return el("span", { class: "muted" }, "isključeno");
  if (!d.active) return el("span", { class: "muted" }, "—");
  if (!netData.snmp_configured) return el("span", { class: "muted", title: "Administrator podešava SNMP dugmetom „SNMP podešavanja”" }, "nije podešen");
  const s = d.snmp_info;
  if (!s) return el("span", { class: "muted" }, d.status === "down" ? "—" : "čeka čitanje…");
  if (!s.ok) return el("span", { class: "bad", title: s.error || "" }, "✕ ne odgovara");
  return el("span", {},
    s.model || s.sys_name || "radi",
    el("div", { class: "muted" }, [s.version ? `v${s.version}` : "", `radi ${fmtUptime(s.uptime_sec)}`,
      s.ports_total ? `${s.ports_up}/${s.ports_total} portova` : ""].filter(Boolean).join(" · ")),
    // Samo greška VLAN konteksta: ruteri i uređaji bez MAC tabele nisu problem.
    s.fdb_error && /VLAN/.test(s.fdb_error) ? el("div", { class: "warn-text small", title: s.fdb_error }, "⚠ MAC tabela po VLAN-ovima nije pročitana") : null);
}

// ---------- temperatura (switch-evi) ----------

const TEMP_CLS = { ok: "ok", warning: "warn-text", critical: "bad" };
const fmtC = (c) => `${Math.round(c)} °C`;

function tempCell(d) {
  const t = d.snmp_info && d.snmp_info.temp;
  if (d.type !== "switch" || !d.active || !t) return el("span", { class: "muted" }, "—");
  if (t.max_c === undefined) return el("span", { class: "muted", title: t.error || "" }, "—");
  const high = t.state === "warning" || t.state === "critical";
  return el("span", { class: `${TEMP_CLS[t.state] || ""}${high ? " strong" : ""}`, title: `${t.max_name || ""}${high ? " – iznad praga switch-a" : ""}` },
    `${high ? "⚠ " : ""}${fmtC(t.max_c)}`);
}

function tempTile(t) {
  if (!t) return null;
  if (t.max_c === undefined) return tileKV("Temperatura", "—", "muted", t.error || "");
  const lbl = { warning: "iznad praga upozorenja", critical: "iznad kritičnog praga" }[t.state];
  return tileKV("Temperatura", fmtC(t.max_c), TEMP_CLS[t.state] || "", [t.max_name, lbl, t.high_since ? `od ${fmtSince(t.high_since)}` : ""].filter(Boolean).join(" · "));
}

function tempSensorsTable(t) {
  if (!t || !(t.sensors || []).length) return null;
  const rank = { critical: 0, warning: 1, ok: 2, unknown: 3 };
  const list = [...t.sensors].sort((a, b) => (a.port - b.port) || (rank[a.state] - rank[b.state]) || a.name.localeCompare(b.name));
  const st = { ok: "u redu", warning: "upozorenje", critical: "kritično", unknown: "ne radi" };
  return el("details", { class: "mr-ifaces", open: t.state === "warning" || t.state === "critical" ? "open" : null },
    el("summary", {}, `Senzori temperature (${list.length})`),
    el("div", { class: "table-wrap" }, el("table", {},
      el("thead", {}, el("tr", {}, ...["Senzor", "Temperatura", "Prag upozorenja", "Kritičan prag", "Stanje"].map((x) => el("th", {}, x)))),
      el("tbody", {}, ...list.map((s) => el("tr", {},
        el("td", {}, s.name, s.port ? el("span", { class: "muted small" }, " (SFP modul)") : null),
        el("td", { class: `num ${TEMP_CLS[s.state] || ""}` }, s.c !== undefined ? fmtC(s.c) : "—"),
        el("td", { class: "num" }, s.warn !== undefined ? fmtC(s.warn) : "—"),
        el("td", { class: "num" }, s.crit !== undefined ? fmtC(s.crit) : "—"),
        el("td", { class: TEMP_CLS[s.state] || "muted" }, st[s.state] || s.state)))))));
}

async function drawTemp(group) {
  const id = ndSelected;
  let h;
  try {
    h = await loadHistory("temp");
  } catch (e) {
    if ($("nd-temp-hover")) $("nd-temp-hover").textContent = e.message;
    return;
  }
  if (id !== ndSelected || !$("nd-temp")) return;
  drawTimeChart($("nd-temp"), $("nd-temp-hover"), {
    from: h.from, to: h.to, height: 120, group,
    series: [{ label: h.agg ? "prosek" : "temperatura", cls: "s1", pts: h.points.map((p) => [p[0], p[1]]) },
      ...(h.agg ? [{ label: "maks", cls: "s2", pts: h.points.map((p) => [p[0], p[2]]) }] : [])],
    fmt: (v) => `${v.toFixed(1)} °C`,
    empty: "Još nema merenja temperature za ovaj period.",
  });
  $("nd-temp-legend").replaceChildren(
    el("span", {}, el("span", { class: "sw" }), h.agg ? "prosek po 15 min" : "najviša temperatura kućišta"),
    h.agg ? el("span", {}, el("span", { class: "sw s2" }), "maksimum") : "",
    xlsxButton(historyURL(id, "temp", ndRange)));
}

function renderNetTable() {
  if (currentPage() !== "net") return;
  const tb = $("netdevs").tBodies[0];
  tb.replaceChildren();
  const rank = { down: 0, unknown: 1, up: 2, inactive: 3 };
  const shown = netData.devices.filter(devMatches).sort((a, b) =>
    (rank[a.status] - rank[b.status]) || a.location_name.localeCompare(b.location_name, "sr") ||
    (a.place || "").localeCompare(b.place || "", "sr") || a.name.localeCompare(b.name, "sr"));
  const pages = Math.max(1, Math.ceil(shown.length / ND_PAGE_SIZE));
  ndPage = Math.min(Math.max(1, ndPage), pages);
  const from = (ndPage - 1) * ND_PAGE_SIZE;
  const page = shown.slice(from, from + ND_PAGE_SIZE);
  const total = netData.devices.length;
  $("nd-count").textContent = !total ? "" : page.length < shown.length
    ? `prikazano ${from + 1}–${from + page.length} od ${shown.length}${shown.length !== total ? ` (ukupno ${total})` : ""}`
    : `prikazano ${shown.length}${shown.length !== total ? ` od ${total}` : ""}`;
  renderPager($("nd-pager"), ndPage, pages, (p) => { ndPage = p; renderNetTable(); });
  $("nd-empty").hidden = total > 0;
  $("nd-empty").textContent = isAdmin()
    ? "Nema uređaja. Dodajte ih dugmetom „Dodaj uređaj” ili ih uvezite iz CSV-a (dugme „Uvoz”)."
    : "Nema uređaja. Uređaje dodaje administrator.";
  if (total && !shown.length) {
    tb.append(el("tr", {}, el("td", { colspan: 10, class: "empty" }, "Nijedan uređaj ne odgovara pretrazi ili filteru.")));
  }
  for (const d of page) {
    const [stLabel, stCls] = DEV_STATUS[d.status] || DEV_STATUS.unknown;
    const pb = devPingBusy(d);
    const role = d.role === "wan" ? "glavna veza (MPLS)" : d.role === "backup" ? "rezervna veza" : "";
    let stNote = d.status === "up" || d.status === "down" ? fmtSince(d.since) : "";
    if (d.status === "down" && d.loc_status === "down") stNote = `lokacija nedostupna · ${stNote}`;
    if (d.status === "unknown" && d.misses) stNote = `bez odgovora ${d.misses}×`;
    tb.append(el("tr", { class: `${d.status === "down" ? "row-down" : ""}${d.id === ndSelected ? " selected" : ""}`, "data-id": d.id },
      el("td", {}, el("span", { class: `dot ${d.status === "up" ? "on" : d.status === "down" ? "down" : ""}`, title: stLabel })),
      el("td", {},
        el("button", { type: "button", class: "linklike", onclick: () => selectDevice(d.id), title: "Detalji: odziv, portovi, saobraćaj" }, d.name),
        d.notify && d.active ? el("span", { class: "bell", title: "Obaveštenja: pad, restart, temperatura switch-a" }, "🔔") : null,
        el("div", { class: "muted" }, typeLabel(d.type) + (role ? ` · ${role}` : "")),
        pb ? el("div", { class: "busy", style: "font-size:12px" },
          `⏳ ${pb.continuous ? "neprekidni ping" : "ping"} u toku (pokrenuo ${pb.user === me.name ? "vi" : pb.user || "?"})`) : null),
      el("td", {}, d.location_name, d.place ? el("div", { class: "muted" }, d.place) : null),
      el("td", { class: "addrs" }, d.ip),
      el("td", { class: "num" }, d.rtt_ms !== undefined ? `${fmtMs(d.rtt_ms)} ms` : "—"),
      el("td", { class: `num ${d.loss_pct > 0 ? "warn-text" : ""}` }, d.loss_pct !== undefined && d.active ? `${d.loss_pct}%` : "—"),
      el("td", {}, el("span", { class: stCls }, stLabel), stNote ? el("div", { class: "muted" }, stNote) : null),
      el("td", {}, snmpCell(d)),
      el("td", { class: "num" }, tempCell(d)),
      el("td", {}, el("div", { class: "actions" },
        el("button", { type: "button", onclick: () => selectDevice(d.id) }, "Detalji"),
        el("button", {
          type: "button", disabled: !!pb, onclick: () => openDevicePing(d),
          title: pb ? busyLabel(pb) : "Konzola pinguje uređaj u zadatom trajanju (ili neprekidno); izveštaj je u Rezultatima",
        }, "Ping"),
        isAdmin() ? el("button", { type: "button", onclick: () => openDeviceDialog(d) }, "Izmeni") : null)),
    ));
  }
}

function openDevicePing(d) {
  showPingDialog({ kind: "device", id: d.id, name: d.name, ip: d.ip,
    location: [d.location_name, d.place].filter(Boolean).join(", "), tail: "." });
}

for (const id of ["nd-loc", "nd-type", "nd-problems"]) {
  $(id).addEventListener("change", () => { ndPage = 1; renderLocTiles(); renderNetTable(); });
}
let ndSearchTimer = null;
$("nd-search").addEventListener("input", () => {
  clearTimeout(ndSearchTimer);
  ndSearchTimer = setTimeout(() => { ndPage = 1; renderNetTable(); }, 200);
});

// ---------- detalji uređaja ----------

function selectDevice(id) {
  if (ndSelected !== id) {
    stopDetailTimers();
    ndPort = null;
    ndDetailMsg = null;
    ndDetail = null;
    $("nd-detail").replaceChildren(el("p", { class: "muted" }, "Učitavam…"));
  }
  ndSelected = id;
  store("nm.ndSel", id || "");
  showPage();
  renderNetTable();
  $("nd-detail").scrollIntoView({ behavior: "smooth", block: "start" });
}

function closeDetail() {
  stopDetailTimers();
  ndSelected = null;
  ndDetail = null;
  store("nm.ndSel", "");
  if ($("nd-detail").classList.contains("maximized")) {
    $("nd-detail").classList.remove("maximized");
    document.body.classList.remove("has-max");
  }
  $("nd-detail").hidden = true;
  $("nd-detail").replaceChildren();
  renderNetTable();
}

async function loadDetail() {
  if (!ndSelected) return;
  const id = ndSelected;
  try {
    const d = await api("GET", `/api/netdev/${encodeURIComponent(id)}`);
    if (id !== ndSelected) return;
    ndDetail = d;
  } catch (e) {
    if (/nije pronađen/.test(e.message)) closeDetail();
    return;
  }
  renderDetail();
}

function tileKV(k, v, cls = "", title = "") {
  return el("div", { class: "tile", title }, el("div", { class: "k" }, k), el("div", { class: `v ${cls}` }, v));
}

// maxButton: dugme „preko celog ekrana” za detalje uređaja (sections.js).
function maxButton(box) {
  const on = box.classList.contains("maximized");
  const b = el("button", { type: "button", class: "sec-max", title: on ? "Vrati (Esc)" : "Preko celog ekrana" }, on ? "⤡" : "⤢");
  b.addEventListener("click", () => secToggleMax(box, b));
  return b;
}

function renderDetail() {
  const d = ndDetail;
  const box = $("nd-detail");
  if (!d || currentPage() !== "net") return;
  box.hidden = false;
  const [stLabel, stCls] = DEV_STATUS[d.status] || DEV_STATUS.unknown;
  const s = d.snmp_info;
  const tiles = [
    tileKV("Stanje", stLabel, stCls, d.since ? fmtSince(d.since) : ""),
    tileKV("Odziv", d.rtt_ms !== undefined ? `${fmtMs(d.rtt_ms)} ms` : "—"),
    tileKV("Gubitak (posl. 10 min)", d.loss_pct !== undefined ? `${d.loss_pct}%` : "—", d.loss_pct > 0 ? "warn-text" : ""),
    tileKV("Poslednja provera", d.last_check ? ago(d.last_check) : "—"),
  ];
  if (s && s.ok) {
    // Uređaj često vraća naziv sa domenom (HQ_FIN_S-I.firma.local): poredi se deo pre tačke.
    const short = (s.sys_name || "").split(".")[0].toLowerCase();
    const mismatch = !!s.sys_name && short !== d.name.toLowerCase();
    tiles.push(
      tileKV("Naziv na uređaju", s.sys_name || "—", mismatch ? "warn-text" : "",
        mismatch ? "Naziv u konzoli se razlikuje od naziva podešenog na uređaju (hostname)." : ""),
      tileKV("Model", s.model || "—"),
      tileKV("Serijski broj", s.serial || "—"),
      tileKV("Verzija softvera", s.version || "—"),
      tileKV("Radi bez restarta", fmtUptime(s.uptime_sec)),
      tileKV("Portovi aktivni", s.ports_total ? `${s.ports_up} od ${s.ports_total}` : "—"),
    );
    if (d.type === "switch" && s.temp) tiles.push(tempTile(s.temp));
  }
  // Poruka (npr. ishod provere SNMP-a) se prikazuje najviše 30 s.
  const msg = ndDetailMsg && Date.now() - ndDetailMsg.at < 30000 ? ndDetailMsg : null;
  const role = d.role === "wan" ? " · glavna veza lokacije (MPLS)" : d.role === "backup" ? " · rezervna veza lokacije" : "";
  const ports = d.ports || [];
  const uplink = ports.find((p) => p.index === d.uplink_index);
  const children = [
    el("div", { class: "nd-head" },
      el("h2", {}, el("span", { class: `dot ${d.status === "up" ? "on" : d.status === "down" ? "down" : ""}` }), d.name,
        el("span", { class: "muted h-note" }, `${typeLabel(d.type)}${role}`)),
      el("span", { class: "spacer" }),
      el("div", { class: "actions" },
        el("button", { type: "button", disabled: !!devPingBusy(d), onclick: () => openDevicePing(d) }, "Ping"),
        isAdmin() && d.snmp ? el("button", { type: "button", onclick: () => testSNMP(d) }, "Proveri SNMP") : null,
        isAdmin() ? el("button", { type: "button", onclick: () => openDeviceDialog(d) }, "Izmeni") : null,
        maxButton(box),
        el("button", { type: "button", onclick: closeDetail, title: "Zatvori detalje", "aria-label": "Zatvori detalje" }, "×"))),
    el("p", { class: "muted nd-sub" }, [d.location_name, d.place, d.ip, d.note].filter(Boolean).join(" · ")),
    msg ? el("p", { class: msg.cls }, msg.text) : null,
    !d.active ? el("p", { class: "muted" }, "Uređaj je neaktivan: ne proverava se i ne šalju se obaveštenja.") : null,
    s && !s.ok ? el("p", { class: "warn" }, `SNMP čitanje ne radi: ${s.error}`) : null,
    s && s.ok && s.fdb_error ? el("p", { class: "warn" }, `MAC tabela (koji računar je na kom portu): ${s.fdb_error}`) : null,
    d.snmp && !netData.snmp_configured ? el("p", { class: "muted" }, "SNMP nije podešen, pa se portovi i saobraćaj ne čitaju" +
      (isAdmin() ? " (dugme „SNMP podešavanja” iznad spiska)." : " (podešava ga administrator).")) : null,
    el("div", { class: "nd-tiles" }, tiles),
  ];
  // Levo grafikoni (odziv, uplink), desno tabela računara na ovom switch-u.
  const charts = [rangeButtons(), el("h3", {}, "Odziv (ping sa konzole)"), chartBox("nd-rtt")];
  const temp = d.type === "switch" && s && s.ok && s.temp;
  if (temp) charts.push(el("h3", {}, "Temperatura"), chartBox("nd-temp"), tempSensorsTable(temp));
  if (uplink) {
    charts.push(
      el("div", { class: "nd-uplink-head" },
        el("h3", {}, `Saobraćaj uređaja – uplink ${uplink.name}${uplink.alias ? ` (${uplink.alias})` : ""}`),
        uplinkPicker(d, ports)),
      el("div", { id: "nd-live", class: "nd-live" }),
      chartBox("nd-up"),
      el("div", { id: "nd-up-stats" }),
      el("div", { id: "nd-up-err" }),
    );
  } else if (ports.length) {
    charts.push(el("p", { class: "muted" }, "Uplink port nije prepoznat (nijedan port nije aktivan)."));
  }
  const showHosts = !!(s && s.ok && s.fdb_method);
  const wifiBox = s && s.wifi ? wifiAside(s.wifi) : null;
  children.push(el("div", { class: `nd-main${showHosts || wifiBox ? " with-hosts" : ""}` },
    el("div", { class: "nd-charts" }, ...charts),
    showHosts ? el("aside", { id: "nd-hosts", class: "nd-hosts" }) : wifiBox));
  if (ports.length) {
    const sel = ports.find((p) => p.index === ndPort);
    children.push(
      el("h3", {}, sel ? `Saobraćaj porta: ${sel.name}${sel.alias ? ` (${sel.alias})` : ""}` : "Saobraćaj pojedinačnog porta"),
      sel ? chartBox("nd-traffic") : el("p", { class: "muted" }, "Kliknite na port u tabeli ispod da biste videli grafikon njegovog saobraćaja."),
      sel ? el("div", { id: "nd-traffic-stats" }) : null,
      sel ? el("div", { id: "nd-traffic-err" }) : null,
      portsTable(ports, d.uplink_index),
    );
  }
  children.push(el("h3", {}, "Poslednji događaji uređaja"), el("ul", { id: "nd-dev-events", class: "nd-ev-list" }));
  box.replaceChildren(...children.filter(Boolean));
  // Odziv i saobraćaj uplink-a dele krstić (isti trenutak na oba grafikona).
  const group = [];
  drawRTT(group);
  if (temp) drawTemp(group);
  if (uplink) drawTraffic(d.uplink_index, "nd-up", group, d.uplink_name);
  const selPort = ports.find((p) => p.index === ndPort);
  if (selPort) drawTraffic(ndPort, "nd-traffic", null, selPort.name);
  syncLive(d, uplink);
  syncHosts(d, showHosts);
  loadDeviceEvents(d.id);
}

// wifiAside: Wi-Fi klijenti sa WLC-a (detalji WLC-a) – računari sa agentom
// i AP na kome su.
function wifiAside(w) {
  return el("aside", { class: "nd-hosts" },
    el("h3", {}, "Računari na Wi-Fi-ju (WLC)"),
    el("p", { class: "muted" }, `${w.clients} Wi-Fi ${w.clients === 1 ? "klijent" : "klijenata"} na ${w.aps} AP-ova` +
      (w.at ? ` · pročitano ${ago(w.at)}` : "") + " · osvežava se na 5 min"),
    w.error ? el("p", { class: "warn" }, `Čitanje Wi-Fi klijenata: ${w.error}`) : null,
    !(w.agents || []).length ? el("p", { class: "muted" }, "Nijedan računar sa agentom nije trenutno na Wi-Fi-ju.")
      : el("div", { class: "table-wrap" }, el("table", { class: "hosts" },
        el("thead", {}, el("tr", {}, el("th", {}, "Računar"), el("th", {}, "AP"), el("th", {}, "SSID"))),
        el("tbody", {}, ...w.agents.map((a) => el("tr", {},
          el("td", {}, el("strong", {}, a.name)), el("td", {}, a.ap), el("td", { class: "muted" }, a.ssid || "—")))))),
    el("p", { class: "muted small" }, "Na switch-u na kome je AP računar se vidi u delu „Računari na ovom switch-u” (AP mora biti CDP/LLDP sused na portu)."));
}

function uplinkPicker(d, ports) {
  if (!isAdmin()) return el("span", { class: "muted" }, d.uplink_auto ? "prepoznat automatski" : "izabrao administrator");
  const sel = el("select", {
    "aria-label": "Uplink port",
    onchange: async (e) => {
      try {
        await api("POST", `/api/netdev/${encodeURIComponent(d.id)}/uplink`, { port: e.target.value });
        stopLive();
        loadDetail();
      } catch (err) {
        setStatus(`Uplink: ${err.message}`, "bad");
      }
    },
  },
  el("option", { value: "" }, d.uplink_auto ? `automatski (${d.uplink_name})` : "automatski"),
  ...ports.filter((p) => p.oper === "up" || p.name === d.uplink).map((p) => el("option", { value: p.name }, `${p.name}${p.alias ? ` – ${p.alias}` : ""}`)));
  sel.value = d.uplink_auto ? "" : d.uplink_name;
  return el("label", { class: "check" }, "Uplink:", sel);
}

function rangeButtons() {
  return rangeBar(ndRange, (r) => { ndRange = r; store("nm.ndRange", r); renderDetail(); });
}

function chartBox(id) {
  return el("div", { class: "ts-wrap" },
    svgEl("svg", { id, class: "ts-chart", role: "img", "aria-label": "grafikon" }),
    el("div", { id: `${id}-hover`, class: "muted ts-hover" }),
    el("div", { id: `${id}-legend`, class: "muted lat-legend" }));
}

// Skraćeni nazivi Cisco portova (TenGigabitEthernet1/1/1 → Te1/1/1).
const PORT_ABBR = [["TwentyFiveGigE", "Twe"], ["HundredGigE", "Hu"], ["FortyGigabitEthernet", "Fo"], ["TenGigabitEthernet", "Te"],
  ["GigabitEthernet", "Gi"], ["FastEthernet", "Fa"], ["Port-channel", "Po"]];
const shortPort = (p) => PORT_ABBR.reduce((s, [a, b]) => s.replace(a, b), p || "");

function neighborKind(n) {
  if (/phone/i.test(n.platform || "")) return "telefon";
  if (/AIR-|C91\d\d|access point/i.test(n.platform || "")) return "AP";
  return "";
}

// attachedCell: šta je priključeno na port – susedi iz CDP/LLDP-a (switch,
// ruter, AP, telefon) i računari iz MAC tabele i podataka agenata. Opis
// interfejsa i MAC adrese su u opisu pri prelasku mišem.
function attachedCell(p, fdbOK) {
  const desc = p.alias ? `Opis interfejsa: ${p.alias}` : "";
  const nbs = p.neighbors || [];
  if (!fdbOK && !nbs.length) return el("td", { class: "muted", title: "MAC tabela još nije pročitana; prikazan je opis interfejsa" }, p.alias || "");
  if (p.oper !== "up") return el("td", { class: "muted", title: desc }, "—");
  const macs = (p.attached || []).map((a) => a.mac);
  const title = [desc, p.mac_count ? `MAC adresa na portu: ${p.mac_count}` : "", macs.length && p.mac_count <= 4 ? `MAC: ${macs.join(", ")}` : ""]
    .filter(Boolean).join("\n");
  // CDP stiže preko uređaja koji ga ne razume: niko od njih nije direktno na portu.
  const indirect = nbs.length > 0 && nbs.every((n) => n.indirect);
  const nbRows = indirect ? [el("div", {
    class: "muted", title: `CDP poruke stižu preko switch-a koji ne podržava CDP:\n${nbs.map((n) => `${n.name} · ${shortPort(n.port)}`).join("\n")}`,
  }, `više uređaja preko switch-a koji ne podržava CDP (${nbs.length})`)] : nbs.map((n) => {
    const kind = neighborKind(n);
    const name = n.device_id
      ? el("button", { type: "button", class: "linklike", onclick: (e) => { e.stopPropagation(); selectDevice(n.device_id); } }, n.name)
      : el("strong", {}, n.name);
    return el("div", { title: [n.platform, n.ip].filter(Boolean).join(" · ") },
      kind ? el("span", { class: "muted" }, `${kind} `) : null, name,
      n.port ? el("span", { class: "muted" }, ` · ${shortPort(n.port)}`) : null);
  });
  // Računari na Wi-Fi-ju preko AP-a na ovom portu (podaci sa WLC-a).
  const wifi = (p.wifi || []).map((a) => el("div", { title: `Wi-Fi preko ${a.ap}${a.ssid ? ` (${a.ssid})` : ""} · ${a.mac}` },
    el("span", { class: "muted" }, "📶 "), el("strong", {}, a.name)));
  // Trunk (veza ka drugom switch-u): kroz port se vide svi uređaji iza
  // drugog switch-a, pa se računari ne navode.
  if (p.trunk) {
    // Računari (sa agentom) iza manjeg switch-a koji nije u konzoli.
    const behind = (p.behind || []).map((a) => el("div", { title: `iza ${p.via || "manjeg switch-a"} · ${a.mac}` },
      el("span", { class: "muted" }, "↳ "), el("strong", {}, a.name)));
    return portItemsCell(p, title, [...(nbRows.length ? nbRows : [el("span", { class: "muted" }, p.alias || "")]), ...behind, ...wifi]);
  }
  const pcs = (p.attached || []).map((a) => el("div", {},
    a.name ? el("strong", {}, a.name) : el("span", { class: "muted" }, "nepoznat uređaj")));
  // Pristupni port sa više uređaja (npr. mali switch ispod stola).
  if (p.shared) {
    pcs.push(el("div", { class: "muted" },
      `${p.unknown ? `još ${p.unknown} ${p.unknown === 1 ? "nepoznat uređaj" : "nepoznatih"} · ` : ""}${p.mac_count} uređaja na portu (mali switch?)`));
  }
  pcs.push(...wifi);
  if (!nbRows.length && !pcs.length) {
    return el("td", { class: "muted", title: [desc, "Switch trenutno nema MAC adresu na ovom portu."].filter(Boolean).join("\n") }, "nepoznat uređaj");
  }
  return portItemsCell(p, title, [...nbRows, ...pcs]);
}

// Portovi sa više od PORT_COLLAPSE_AT uređaja mogu da se skupe (▸/▾); prvi
// red (obično sused: AP, telefon, switch) se uvek vidi. Pregledač pamti
// koje portove je korisnik raširio.
const PORT_COLLAPSE_AT = 4;
const PORT_OPEN_KEY = "nm.portOpen";

function portOpenSet() {
  try { return new Set(JSON.parse(stored(PORT_OPEN_KEY) || "[]")); } catch { return new Set(); }
}

function portItemsCell(p, title, items) {
  if (items.length <= PORT_COLLAPSE_AT) return el("td", { title }, ...items);
  const key = `${ndSelected}:${p.index}`;
  const open = portOpenSet().has(key);
  const btn = el("button", {
    type: "button", class: "sec-btn", "aria-expanded": open ? "true" : "false", title: open ? "Skupi" : "Raširi",
    onclick: (e) => {
      e.stopPropagation(); // ne otvara grafikon porta
      const set = portOpenSet();
      if (set.has(key)) set.delete(key); else set.add(key);
      store(PORT_OPEN_KEY, JSON.stringify([...set].slice(-200)));
      renderDetail();
    },
  }, open ? "▾" : "▸");
  return el("td", { title }, items[0],
    el("div", { class: "port-collapse" }, btn, el("span", { class: "muted" }, ` ${items.length} uređaja na portu`)),
    ...(open ? items.slice(1) : []));
}

function portsTable(ports, uplinkIndex) {
  const fdbOK = !!(ndDetail && ndDetail.snmp_info && ndDetail.snmp_info.fdb_method);
  const list = ports.filter((p) => !ndPortsUpOnly || p.oper === "up" || p.index === ndPort);
  const rows = list.map((p) => {
    const errs = (p.in_err || 0) + (p.out_err || 0);
    const disc = (p.in_disc || 0) + (p.out_disc || 0);
    const state = !p.admin_up ? ["isključen", "muted"] : p.oper === "up" ? ["up", "ok"] : ["down", "muted"];
    return el("tr", {
      class: `clickable${p.index === ndPort ? " selected" : ""}`,
      onclick: () => { ndPort = p.index === ndPort ? null : p.index; renderDetail(); },
    },
    el("td", {}, el("strong", {}, p.name), p.index === uplinkIndex ? el("span", { class: "tag" }, "uplink") : null),
    attachedCell(p, fdbOK),
    el("td", { class: state[1] }, state[0]),
    el("td", { class: "num" }, p.oper === "up" ? fmtSpeed(p.speed_mbps) : "—"),
    el("td", { class: "num" }, fmtBps(p.in_bps)),
    el("td", { class: "num" }, fmtBps(p.out_bps)),
    el("td", { class: `num ${errs ? "bad" : ""}`, title: `ukupno od pokretanja uređaja: ${p.err_total}` }, String(errs)),
    el("td", { class: `num ${disc ? "warn-text" : ""}` }, String(disc)),
    el("td", {}, p.changed_sec !== undefined ? `pre ${fmtUptime(p.changed_sec)}` : "—"));
  });
  return el("div", {},
    el("div", { class: "toolbar" },
      el("h3", { style: "margin:0" }, "Portovi"),
      el("label", { class: "check" }, el("input", {
        type: "checkbox", checked: ndPortsUpOnly ? "checked" : null,
        onchange: (e) => { ndPortsUpOnly = e.target.checked; store("nm.ndPortsUp", ndPortsUpOnly ? "1" : "0"); renderDetail(); },
      }), " samo aktivni portovi"),
      el("span", { class: "muted" }, `prikazano ${list.length} od ${ports.length}; greške i odbačeni = u poslednjem intervalu čitanja`)),
    el("div", { class: "table-wrap" }, el("table", { class: "ports" },
      el("thead", {}, el("tr", {}, ...["Port", "Priključeno", "Stanje", "Brzina", "Primljeno", "Poslato", "Greške", "Odbačeni", "Promena stanja"]
        .map((h) => el("th", {}, h)))),
      el("tbody", {}, rows))));
}

function historyURL(dev, series, range, label) {
  return `/api/netdev/${encodeURIComponent(dev)}/history?series=${encodeURIComponent(series)}&${rangeQS(range)}` + (label ? `&label=${encodeURIComponent(label)}` : "");
}

async function loadHistory(series) {
  return api("GET", historyURL(ndSelected, series, ndRange));
}

async function drawRTT(group) {
  const id = ndSelected;
  let h;
  try {
    h = await loadHistory("rtt");
  } catch (e) {
    if ($("nd-rtt-hover")) $("nd-rtt-hover").textContent = e.message;
    return;
  }
  if (id !== ndSelected || !$("nd-rtt")) return;
  // raw: [t, odziv]; agg: [t, prosek, max, gubitak%]
  const pts = h.points.map((p) => [p[0], p[1]]);
  const lost = h.points.filter((p) => (h.agg ? p[3] > 0 : p[1] === null)).map((p) => p[0]);
  drawTimeChart($("nd-rtt"), $("nd-rtt-hover"), {
    from: h.from, to: h.to, height: 150, group,
    series: [{ label: h.agg ? "prosečan odziv" : "odziv", cls: "s1", pts }],
    fmt: (v) => `${fmtMs(v)} ms`,
    lost,
    extra: h.agg ? (i) => { const p = h.points[i]; return p && p[3] ? ` · gubitak ${p[3].toFixed(1)}%` : ""; } : null,
    empty: "Još nema merenja za ovaj period.",
  });
  $("nd-rtt-legend").replaceChildren(
    el("span", {}, el("span", { class: "sw" }), h.agg ? "prosečan odziv po 15 min" : "odziv na ping (svakih 30 s)"),
    el("span", {}, el("span", { class: "x" }, "|"), h.agg ? "interval sa gubitkom" : "bez odgovora"),
    xlsxButton(historyURL(id, "rtt", ndRange)));
}

const fmtBytes = (b) => (b >= 1e9 ? `${(b / 1e9).toFixed(2)} GB` : b >= 1e6 ? `${(b / 1e6).toFixed(1)} MB` : `${Math.round(b / 1e3)} kB`);

// trafficSummary računa poslednje/min/prosek/max i količinu podataka za
// istoriju porta. raw: [t, ul, izl, gr.ul, gr.izl, odb.ul, odb.izl];
// agg (15 min): [t, pr.ul, pr.izl, max ul, max izl, gr.ul, gr.izl, odb.ul, odb.izl].
function trafficSummary(h) {
  const pts = h.points;
  const step = h.agg ? 900 : medianStep(pts.map((p) => p[0]));
  const one = (col, maxCol) => {
    const vals = pts.map((p) => p[col]).filter((v) => v !== null && v !== undefined);
    if (!vals.length) return null;
    const maxVals = h.agg ? pts.map((p) => p[maxCol]).filter((v) => v !== null && v !== undefined) : vals;
    let bytes = 0;
    pts.forEach((p, i) => {
      if (p[col] === null || p[col] === undefined) return;
      const dt = h.agg ? 900 : i > 0 ? Math.min(p[0] - pts[i - 1][0], 3 * step) : step;
      bytes += (p[col] * dt) / 8;
    });
    return { last: vals[vals.length - 1], min: Math.min(...vals), avg: vals.reduce((a, b) => a + b, 0) / vals.length, max: Math.max(...maxVals), bytes };
  };
  const o = h.agg ? 5 : 3;
  const sum = [0, 0, 0, 0];
  for (const p of pts) for (let k = 0; k < 4; k++) sum[k] += p[o + k] || 0;
  return { in: one(1, 3), out: one(2, 4), err: [sum[0], sum[1]], disc: [sum[2], sum[3]], o };
}

function medianStep(ts) {
  const gaps = ts.slice(1).map((t, i) => t - ts[i]).sort((a, b) => a - b);
  return gaps.length ? gaps[Math.floor(gaps.length / 2)] : 60;
}

function statsTable(sm, agg) {
  const row = (label, cls, s) => el("tr", {},
    el("td", {}, el("span", { class: `sw ${cls}` }), label),
    ...(s ? [fmtBps(s.last), fmtBps(s.min), fmtBps(s.avg), fmtBps(s.max), fmtBytes(s.bytes)] : ["—", "—", "—", "—", "—"])
      .map((v) => el("td", { class: "num" }, v)));
  const bad = sm.err[0] + sm.err[1] > 0;
  return el("div", {},
    el("div", { class: "table-wrap" }, el("table", { class: "ts-stats" },
      el("thead", {}, el("tr", {}, ...["", "poslednje", "min", "prosek", agg ? "max (vrh)" : "max", "količina u periodu"].map((x) => el("th", {}, x)))),
      el("tbody", {}, row("primljeno", "", sm.in), row("poslato", "s2", sm.out)))),
    el("p", { class: bad ? "warn" : "muted" },
      `Greške u periodu: primljeno ${sm.err[0]}, poslato ${sm.err[1]} · odbačeni: primljeno ${sm.disc[0]}, poslato ${sm.disc[1]}.` +
      (bad ? " Greške na portu najčešće znače loš kabl, konektor ili neusklađen duplex." : "")));
}

// drawTraffic crta saobraćaj porta idx u elementu id (nd-up ili nd-traffic),
// sa tabelom statistike i, ako ih je bilo, grafikonom grešaka.
async function drawTraffic(idx, id, group, portName) {
  const dev = ndSelected;
  let h;
  try {
    h = await loadHistory(`if:${idx}`);
  } catch (e) {
    if ($(`${id}-hover`)) $(`${id}-hover`).textContent = e.message;
    return;
  }
  if (dev !== ndSelected || !$(id)) return;
  const sm = trafficSummary(h);
  drawTimeChart($(id), $(`${id}-hover`), {
    from: h.from, to: h.to, height: 200, group, area: true,
    series: [
      { label: "primljeno", cls: "s1", pts: h.points.map((p) => [p[0], p[1]]) },
      { label: "poslato", cls: "s2", pts: h.points.map((p) => [p[0], p[2]]) },
    ],
    fmt: fmtBps,
    extra: (i) => {
      const p = h.points[i];
      if (!p) return "";
      const e = (p[sm.o] || 0) + (p[sm.o + 1] || 0), dsc = (p[sm.o + 2] || 0) + (p[sm.o + 3] || 0);
      return (e ? ` · greške ${e}` : "") + (dsc ? ` · odbačeni ${dsc}` : "") + (h.agg ? ` · vrh ${fmtBps(Math.max(p[3] || 0, p[4] || 0))}` : "");
    },
    empty: "Još nema podataka za ovaj period (saobraćaj se računa od drugog SNMP čitanja).",
  });
  $(`${id}-legend`).replaceChildren(
    el("span", {}, el("span", { class: "sw" }), "primljeno (ulaz na port)"),
    el("span", {}, el("span", { class: "sw s2" }), "poslato (izlaz sa porta)"),
    h.agg ? el("span", {}, "proseci po 15 min") : "",
    xlsxButton(historyURL(dev, `if:${idx}`, ndRange, portName)));
  const statsBox = $(id === "nd-up" ? "nd-up-stats" : "nd-traffic-stats");
  if (statsBox && h.points.length) statsBox.replaceChildren(statsTable(sm, h.agg));
  // Greške i odbačeni: poseban mali grafikon (ne druga osa na grafikonu saobraćaja).
  const errBox = $(id === "nd-up" ? "nd-up-err" : "nd-traffic-err");
  if (errBox) {
    errBox.replaceChildren();
    if (sm.err[0] + sm.err[1] + sm.disc[0] + sm.disc[1] > 0) {
      errBox.append(el("h3", {}, "Greške i odbačeni paketi (broj po intervalu)"), chartBox(`${id}-e`));
      drawTimeChart($(`${id}-e`), $(`${id}-e-hover`), {
        from: h.from, to: h.to, height: 100,
        series: [
          { label: "greške", cls: "err", pts: h.points.map((p) => [p[0], (p[sm.o] || 0) + (p[sm.o + 1] || 0)]) },
          { label: "odbačeni", cls: "disc", pts: h.points.map((p) => [p[0], (p[sm.o + 2] || 0) + (p[sm.o + 3] || 0)]) },
        ],
        fmt: (v) => String(Math.round(v)),
      });
      $(`${id}-e-legend`).replaceChildren(
        el("span", {}, el("span", { class: "sw err" }), "greške (primljeno + poslato)"),
        el("span", {}, el("span", { class: "sw disc" }), "odbačeni (primljeno + poslato)"));
    }
  }
}

// drawTimeChart crta vremensku liniju (jedna osa, do 2 serije) sa krstićem i
// opisom pri prelasku mišem. Tačke: [unix, vrednost | null]. Grafikoni sa
// istim o.group pomeraju krstić zajedno.
function drawTimeChart(svg, hover, o) {
  const W = Math.max(320, Math.round(svg.getBoundingClientRect().width || 800));
  const H = o.height || 180;
  const L = 64, R = 10, T = 10, B = 22;
  svg.setAttribute("viewBox", `0 0 ${W} ${H}`);
  svg.setAttribute("height", H);
  svg.replaceChildren();
  const text = (attrs, s) => { const t = svgEl("text", attrs); t.textContent = s; return t; };
  const all = o.series.flatMap((s) => s.pts.map((p) => p[1])).filter((v) => v !== null && v !== undefined);
  if (!all.length) {
    svg.append(text({ x: W / 2, y: H / 2, class: "axis", "text-anchor": "middle" }, o.empty || "Nema podataka."));
    hover.textContent = "";
    svg.onmousemove = svg.onmouseleave = null;
    return;
  }
  const yMax = niceCeil(Math.max(...all) * 1.05 || 1);
  const span = o.to - o.from;
  const x = (t) => L + ((t - o.from) / span) * (W - L - R);
  const y = (v) => T + (1 - v / yMax) * (H - T - B);
  for (let i = 0; i <= 4; i++) {
    const v = (yMax * i) / 4;
    svg.append(svgEl("line", { x1: L, x2: W - R, y1: y(v), y2: y(v), class: "grid" }));
    svg.append(text({ x: L - 6, y: y(v) + 3, class: "axis", "text-anchor": "end" }, o.fmt(v)));
  }
  const tick = [[600, 120], [3600, 600], [6 * 3600, 3600], [24 * 3600, 4 * 3600], [7 * 86400, 86400], [Infinity, 5 * 86400]].find(([s]) => span <= s)[1];
  for (let t = Math.ceil(o.from / tick) * tick; t <= o.to; t += tick) {
    const d = new Date(t * 1000);
    const label = tick >= 86400 ? d.toLocaleDateString("sr-RS", { day: "numeric", month: "numeric" })
      : d.toLocaleTimeString("sr-RS", { hour: "2-digit", minute: "2-digit" });
    svg.append(text({ x: x(t), y: H - 6, class: "axis", "text-anchor": "middle" }, label));
  }
  // Prekid u podacima (npr. konzola nije radila) prekida liniju.
  const ts = o.series[0].pts.map((p) => p[0]);
  const step = medianStep(ts);
  for (const s of o.series) {
    // Deonice bez prekida; za svaku linija i (opciono) blago popunjena površina.
    const runs = [];
    let cur = [], prev = null;
    for (const [t, v] of s.pts) {
      if (v === null || v === undefined || (prev !== null && t - prev > 3 * step)) {
        if (cur.length) runs.push(cur);
        cur = [];
      }
      if (v !== null && v !== undefined) cur.push([x(t), y(v)]);
      prev = v === null || v === undefined ? null : t;
    }
    if (cur.length) runs.push(cur);
    for (const r of runs) {
      const line = r.map(([px, py], i) => `${i ? "L" : "M"}${px.toFixed(1)},${py.toFixed(1)}`).join("");
      if (o.area && r.length > 1) {
        svg.append(svgEl("path", { d: `${line}L${r[r.length - 1][0].toFixed(1)},${y(0)}L${r[0][0].toFixed(1)},${y(0)}Z`, class: `area ${s.cls}` }));
      }
      svg.append(svgEl("path", { d: line, class: `line ${s.cls}` }));
    }
  }
  for (const t of o.lost || []) svg.append(svgEl("line", { x1: x(t), x2: x(t), y1: H - B, y2: H - B - 6, class: "lost" }));
  const cross = svgEl("line", { y1: T, y2: H - B, class: "cross", visibility: "hidden" });
  const marks = o.series.map((s) => svgEl("circle", { r: 4, class: `hl ${s.cls}`, visibility: "hidden" }));
  svg.append(cross, ...marks);
  const fmtT = (t) => new Date(t * 1000).toLocaleString("sr-RS",
    { day: "numeric", month: "numeric", hour: "2-digit", minute: "2-digit", second: span <= 6 * 3600 ? "2-digit" : undefined });
  const idle = () => { hover.textContent = "Pređite mišem preko grafikona za tačne vrednosti."; };
  idle();
  const ctl = {
    hide() {
      cross.setAttribute("visibility", "hidden");
      marks.forEach((m) => m.setAttribute("visibility", "hidden"));
      idle();
    },
    showAt(t) {
      let lo = 0, hi = ts.length - 1;
      while (lo < hi) { const mid = (lo + hi) >> 1; if (ts[mid] < t) lo = mid + 1; else hi = mid; }
      if (lo > 0 && Math.abs(ts[lo - 1] - t) < Math.abs(ts[lo] - t)) lo--;
      const tt = ts[lo];
      if (Math.abs(tt - t) > 3 * step) { ctl.hide(); return; } // nema merenja u tom trenutku
      cross.setAttribute("x1", x(tt));
      cross.setAttribute("x2", x(tt));
      cross.setAttribute("visibility", "visible");
      const parts = [];
      o.series.forEach((s, k) => {
        const v = s.pts[lo] && s.pts[lo][1];
        if (v === null || v === undefined) {
          marks[k].setAttribute("visibility", "hidden");
          parts.push(`${s.label}: bez odgovora`);
          return;
        }
        marks[k].setAttribute("cx", x(tt));
        marks[k].setAttribute("cy", y(v));
        marks[k].setAttribute("visibility", "visible");
        parts.push(`${s.label}: ${o.fmt(v)}`);
      });
      hover.textContent = `${fmtT(tt)} · ${parts.join(" · ")}${o.extra ? o.extra(lo) : ""}`;
    },
  };
  const group = o.group || [ctl];
  if (o.group) o.group.push(ctl);
  svg.onmouseleave = () => group.forEach((c) => c.hide());
  svg.onmousemove = (ev) => {
    const r = svg.getBoundingClientRect();
    const px = ((ev.clientX - r.left) / r.width) * W;
    const t = o.from + ((px - L) / (W - L - R)) * span;
    group.forEach((c) => c.showAt(t));
  };
  return ctl;
}

// ---------- režim uživo (saobraćaj uplink-a na 5 s) ----------

let ndLive = null; // { dev, idx, timer, pts: [[t, ul, izl]], bytesIn, bytesOut, since, err }

function stopLive() {
  if (ndLive) clearInterval(ndLive.timer);
  ndLive = null;
}

// stopDetailTimers zaustavlja sva očitavanja otvorenih detalja.
function stopDetailTimers() {
  stopLive();
  stopHosts();
}

// ---------- računari na ovom switch-u (na 10 s) ----------

let ndHosts = null; // { dev, timer, data, err }

function stopHosts() {
  if (ndHosts) clearInterval(ndHosts.timer);
  ndHosts = null;
}

function syncHosts(d, show) {
  if (!show) { if (ndHosts) stopHosts(); return; }
  if (!ndHosts || ndHosts.dev !== d.id) {
    if (ndHosts) clearInterval(ndHosts.timer);
    ndHosts = { dev: d.id, data: null, err: "" };
    ndHosts.timer = setInterval(hostsTick, 10000);
    hostsTick();
  }
  renderHostsBox();
}

async function hostsTick() {
  const h = ndHosts;
  if (!h || document.hidden || currentPage() !== "net" || ndSelected !== h.dev) return;
  try {
    const r = await api("GET", `/api/netdev/${encodeURIComponent(h.dev)}/hosts`);
    if (h !== ndHosts) return;
    h.data = r;
    h.err = r.error || "";
  } catch (e) {
    if (h === ndHosts) h.err = e.message;
  }
  renderHostsBox();
}

// rateCells: kolone Šalje/Prima – brzina samog računara (meri agent); ako
// agent ne šalje brzinu, saobraćaj porta sivom bojom sa oznakom „port”.
function rateCells(r) {
  const title = pcRateTitle(r);
  if (r.pc_send_bps !== undefined && r.pc_send_bps !== null) {
    return [el("td", { class: "num", title }, fmtBps(r.pc_send_bps)), el("td", { class: "num", title }, fmtBps(r.pc_recv_bps))];
  }
  const port = (v) => el("td", { class: "num muted", title }, fmtBps(v), el("div", { class: "small" }, r.ap ? "port (ceo AP)" : "port"));
  return [port(r.send_bps), port(r.recv_bps)];
}

// pcRateTitle: brzina same mrežne kartice računara (meri agent) i saobraćaj
// porta, za prikaz pri prelasku mišem.
function pcRateTitle(r) {
  const pc = r.pc_send_bps !== undefined && r.pc_send_bps !== null
    ? `Računar ${r.name}: šalje ${fmtBps(r.pc_send_bps)}, prima ${fmtBps(r.pc_recv_bps)} (meri agent, poslednjih 10 s)`
    : r.pc_no_stats ? `Brzina računara ${r.name}: agent je stare verzije (brzinu šalje izdanje 2 i novije) – nadogradite agenta.`
      : `Brzina računara ${r.name} nije dostupna (agent nije na vezi).`;
  const port = `Port ${r.port}: šalje ${fmtBps(r.send_bps)}, prima ${fmtBps(r.recv_bps)}` +
    (r.ap ? " (ukupno ceo AP)" : r.shared ? " (ukupno svi uređaji na portu)" : "");
  return `${pc}\n${port}`;
}

function renderHostsBox() {
  const box = $("nd-hosts");
  const h = ndHosts;
  if (!box || !h) return;
  const rows = h.data ? h.data.hosts : null;
  box.replaceChildren(
    el("h3", {}, "Računari na ovom switch-u"),
    el("p", { class: "muted" }, h.data && h.data.live ? "iz ugla računara · osvežava se na 10 s"
      : "iz ugla računara · do prvog očitavanja na 10 s prikazane su vrednosti iz čitanja na 60 s"),
    h.err ? el("p", { class: "warn" }, h.err) : "",
    !rows ? el("p", { class: "muted" }, "Učitavam…")
      : !rows.length ? el("p", { class: "muted" }, "Na ovom switch-u nema prepoznatih računara (sa agentom).")
        : el("div", { class: "table-wrap" }, el("table", { class: "hosts" },
          el("thead", {}, el("tr", {}, el("th", {}, "Računar"), el("th", {}, "Port"),
            el("th", { class: "num", title: "računar šalje (switch na tom portu prima)" }, "Šalje"),
            el("th", { class: "num", title: "računar prima (switch na tom portu šalje)" }, "Prima"))),
          el("tbody", {}, rows.map((r) => el("tr", {
            class: `clickable${r.index === ndPort ? " selected" : ""}`,
            title: "Prikaži grafikon saobraćaja ovog porta",
            onclick: () => {
              ndPort = r.index;
              renderDetail();
              const t = $("nd-traffic");
              if (t) t.scrollIntoView({ behavior: "smooth", block: "center" });
            },
          },
          el("td", { title: pcRateTitle(r) }, el("strong", {}, r.name)),
          el("td", { class: "muted", title: r.shared ? "Port deli više uređaja (npr. mali switch): brzina je zbir svih uređaja na njemu." : "" },
            r.port, r.ap ? el("div", { class: "small", title: `Wi-Fi preko AP-a ${r.ap}${r.wlc ? `, preko kontrolera ${r.wlc}` : ""}${r.ssid ? `, mreža ${r.ssid}` : ""}.\nBrzina je ukupan saobraćaj AP-a, ne samo ovog računara.` }, `📶 Wi-Fi · ${r.ap}`)
              : r.behind ? el("div", { class: "small" }, `iza ${r.via || "manjeg switch-a"}`)
              : r.shared ? el("div", { class: "small" }, "zajednički port") : ""),
          ...rateCells(r))))),
        ));
}

// syncLive pokreće (ili nastavlja) očitavanje uživo za uplink otvorenog uređaja.
function syncLive(d, uplink) {
  const can = uplink && d.active && d.snmp && d.snmp_info && d.snmp_info.ok;
  if (!can) { stopLive(); return; }
  if (!ndLive || ndLive.dev !== d.id || ndLive.idx !== uplink.index) {
    stopLive();
    ndLive = { dev: d.id, idx: uplink.index, pts: [], bytesIn: 0, bytesOut: 0, since: Date.now(), err: "" };
    ndLive.timer = setInterval(liveTick, 5000);
    liveTick();
  }
  renderLiveBox();
}

async function liveTick() {
  const lv = ndLive;
  if (!lv || document.hidden || currentPage() !== "net" || ndSelected !== lv.dev) return;
  try {
    const r = await api("GET", `/api/netdev/${encodeURIComponent(lv.dev)}/live?port=${lv.idx}`);
    if (lv !== ndLive) return;
    lv.err = "";
    if (r.in_bps !== undefined && r.out_bps !== undefined) {
      const last = lv.pts[lv.pts.length - 1];
      if (!last || r.t > last[0]) {
        if (last) {
          const dt = Math.min(r.t - last[0], 15);
          lv.bytesIn += (r.in_bps * dt) / 8;
          lv.bytesOut += (r.out_bps * dt) / 8;
        }
        lv.pts.push([r.t, r.in_bps, r.out_bps]);
        lv.pts = lv.pts.filter((p) => p[0] >= r.t - 300);
      }
    }
  } catch (e) {
    if (lv === ndLive) lv.err = e.message;
  }
  renderLiveBox();
}

function renderLiveBox() {
  const box = $("nd-live");
  const lv = ndLive;
  if (!box || !lv) return;
  const last = lv.pts[lv.pts.length - 1];
  const mins = Math.max(1, Math.round((Date.now() - lv.since) / 60000));
  box.replaceChildren(
    el("div", { class: "nd-live-vals" },
      el("span", { class: "live-dot", title: "očitava se na 5 s dok su detalji otvoreni" }),
      el("strong", {}, "Uživo"), el("span", { class: "muted" }, "(na 5 s)"),
      el("span", {}, el("span", { class: "sw" }), "primljeno ", el("strong", {}, last ? fmtBps(last[1]) : "…")),
      el("span", {}, el("span", { class: "sw s2" }), "poslato ", el("strong", {}, last ? fmtBps(last[2]) : "…")),
      el("span", { class: "muted" }, `od otvaranja (${mins} min): primljeno ${fmtBytes(lv.bytesIn)}, poslato ${fmtBytes(lv.bytesOut)}`)),
    lv.err ? el("p", { class: "warn" }, `Očitavanje uživo: ${lv.err}`) : "",
    lv.pts.length > 1 ? chartBox("nd-live-c") : el("p", { class: "muted" }, "Prvo očitavanje uživo… (brzina se računa od drugog očitavanja, posle 5 s)"),
  );
  if (lv.pts.length > 1) {
    const now = lv.pts[lv.pts.length - 1][0];
    drawTimeChart($("nd-live-c"), $("nd-live-c-hover"), {
      from: now - 300, to: now, height: 110, area: true,
      series: [
        { label: "primljeno", cls: "s1", pts: lv.pts.map((p) => [p[0], p[1]]) },
        { label: "poslato", cls: "s2", pts: lv.pts.map((p) => [p[0], p[2]]) },
      ],
      fmt: fmtBps,
    });
    $("nd-live-c-legend").textContent = "poslednjih 5 minuta, očitavanje na 5 s";
  }
}

async function loadDeviceEvents(id) {
  let evs = [];
  try {
    evs = await api("GET", `/api/netdev/events?device=${encodeURIComponent(id)}&limit=10`);
  } catch {
    return;
  }
  const ul = $("nd-dev-events");
  if (!ul || id !== ndSelected) return;
  ul.replaceChildren(...(evs.length ? evs.map(evItem) : [el("li", { class: "muted" }, "Nema događaja.")]));
}

function evItem(ev) {
  const [icon, cls] = EV_ICONS[ev.kind] || ["•", "muted"];
  return el("li", {}, el("span", { class: `ev-icon ${cls}` }, icon), el("span", { class: "muted" }, fmtEvTime(ev.time)), " ", ev.text);
}

async function testSNMP(d) {
  ndDetailMsg = { cls: "muted", text: "Proveravam SNMP…", at: Date.now() };
  renderDetail();
  try {
    const r = await api("POST", `/api/netdev/${encodeURIComponent(d.id)}/snmp-test`, {});
    ndDetailMsg = r.ok
      ? { cls: r.fdb_error || r.wifi_error ? "warn" : "ok", text: `SNMP radi: ${r.system.sys_name || d.name}${r.system.model ? `, ${r.system.model}` : ""}, ${r.ports} portova (odgovor za ${r.ms} ms). ` +
        (r.wifi_error ? `Wi-Fi klijenti: ${r.wifi_error}` : r.wifi_clients !== undefined ? `Wi-Fi: ${r.wifi_clients} klijenata na ${r.wifi_aps} AP-ova.`
          : r.fdb_skipped ? "MAC tabela se ne čita za rutere."
          : r.fdb_error ? `MAC tabela: ${r.fdb_error}` : `MAC tabela: ${r.fdb_macs} adresa (${r.fdb_method}).`) +
        ` Susedi (CDP/LLDP): ${r.neighbors || 0}.` }
      : { cls: "bad", text: `SNMP ne radi: ${r.error}` };
  } catch (e) {
    ndDetailMsg = { cls: "bad", text: `SNMP: ${e.message}` };
  }
  ndDetailMsg.at = Date.now();
  if (ndSelected === d.id) {
    renderDetail();
    setTimeout(loadDetail, 2000);
  }
}

// ---------- događaji ----------

async function loadNetEvents() {
  try {
    netEvents = await api("GET", "/api/netdev/events?limit=500");
  } catch {
    return;
  }
  renderNetEvents();
}

function renderNetEvents() {
  const tb = $("nd-events").tBodies[0];
  tb.replaceChildren();
  const pages = Math.max(1, Math.ceil(netEvents.length / EV_PAGE_SIZE));
  evPage = Math.min(Math.max(1, evPage), pages);
  renderPager($("nd-ev-pager"), evPage, pages, (p) => { evPage = p; renderNetEvents(); });
  if (!netEvents.length) {
    tb.append(el("tr", {}, el("td", { colspan: 4, class: "empty" }, "Još nema događaja.")));
    return;
  }
  for (const ev of netEvents.slice((evPage - 1) * EV_PAGE_SIZE, evPage * EV_PAGE_SIZE)) {
    const [icon, cls] = EV_ICONS[ev.kind] || ["•", "muted"];
    tb.append(el("tr", {},
      el("td", { class: "nowrap" }, fmtEvTime(ev.time)),
      el("td", {}, ev.location || "—"),
      el("td", {}, el("span", { class: `ev-icon ${cls}` }, icon),
        ev.device_id && netData.devices.some((d) => d.id === ev.device_id)
          ? el("button", { type: "button", class: "linklike", onclick: () => selectDevice(ev.device_id) }, ev.text)
          : ev.text),
      el("td", {}, ev.duration_sec ? fmtDuration(ev.duration_sec) : "—")));
  }
}

// ---------- dodavanje i izmena uređaja (administrator) ----------

const ROLE_HELP = {
  "": "",
  wan: "Glavna veza lokacije: obično WAN (MPLS) adresa rutera operatera, dostupna samo kroz MPLS. Kada ona ne odgovara, " +
    "a ostali uređaji na lokaciji odgovaraju, lokacija radi preko rezervne veze.",
  backup: "Rezervna veza lokacije (npr. LTE ruter). Prikazuje se uz lokaciju; ne utiče na ocenu da li je lokacija dostupna.",
};

function openDeviceDialog(d, preset) {
  ndEditing = d || null;
  $("nd-dlg-title").textContent = d ? `Izmena: ${d.name}` : "Novi uređaj";
  const types = $("nd-f-type");
  if (!types.options.length) {
    for (const [k, l] of Object.entries(netData.types).sort((a, b) => a[1].localeCompare(b[1], "sr"))) types.append(el("option", { value: k }, l));
  }
  const locs = [...new Set([...locationNames, ...netData.devices.map((x) => x.location).filter(Boolean)])];
  $("nd-f-loc").replaceChildren(el("option", { value: "" }, "automatski (po IP adresi)"), ...locs.map((n) => el("option", { value: n }, n)));
  $("nd-name").value = d ? d.name : "";
  $("nd-ip").value = d ? d.ip : "";
  types.value = d ? d.type : "switch";
  $("nd-f-loc").value = d ? d.location || "" : "";
  $("nd-place").value = d ? d.place || "" : "";
  $("nd-role").value = d ? d.role || "" : "";
  $("nd-active").checked = d ? d.active : true;
  $("nd-f-snmp").checked = d ? d.snmp : true;
  fillProfileSelect(d ? d.snmp_profile || "" : "");
  $("nd-notify").checked = d ? d.notify : true;
  $("nd-note").value = d ? d.note || "" : "";
  const src = d || preset || {};
  if (preset && !d) {
    for (const [k, id] of [["type", "nd-f-type"], ["location", "nd-f-loc"]]) if (preset[k] !== undefined) $(id).value = preset[k];
    $("nd-notify").checked = false;
  }
  $("nd-mpls-link").value = src.mpls_link || "";
  $("nd-mpls-if").value = src.mpls_if || "";
  $("nd-mpls-speed").value = src.mpls_speed_mbps || "";
  $("nd-mpls-kind").value = src.mpls_kind || "";
  $("nd-mpls-ifaces").replaceChildren(...(src.ifaces || []).map((f) => el("option", { value: f.name }, f.alias || "")));
  $("nd-delete").hidden = !d;
  $("nd-msg").textContent = "";
  $("nd-role-help").textContent = ROLE_HELP[$("nd-role").value];
  $("nd-dlg").showModal();
  $("nd-name").focus();
}

$("nd-role").addEventListener("change", () => { $("nd-role-help").textContent = ROLE_HELP[$("nd-role").value]; });
$("nd-add").addEventListener("click", () => openDeviceDialog(null));
$("nd-cancel").addEventListener("click", () => $("nd-dlg").close());
$("nd-form").addEventListener("submit", async (ev) => {
  ev.preventDefault();
  const body = {
    name: $("nd-name").value.trim(), ip: $("nd-ip").value.trim(), type: $("nd-f-type").value,
    location: $("nd-f-loc").value, place: $("nd-place").value.trim(), role: $("nd-role").value,
    active: $("nd-active").checked, snmp: $("nd-f-snmp").checked, notify: $("nd-notify").checked, note: $("nd-note").value.trim(),
    snmp_profile: $("nd-f-snmp-profile").value,
    mpls_link: $("nd-mpls-link").value, mpls_if: $("nd-mpls-if").value.trim(),
    mpls_speed_mbps: parseInt($("nd-mpls-speed").value, 10) || 0, mpls_kind: $("nd-mpls-kind").value,
  };
  try {
    const path = ndEditing ? `/api/netdev/${encodeURIComponent(ndEditing.id)}` : "/api/netdev";
    const d = await api("POST", path, body);
    $("nd-dlg").close();
    setStatus(ndEditing ? `Uređaj ${d.name} je izmenjen.` : `Uređaj ${d.name} je dodat; prva provera za najviše 30 sekundi.`, "ok");
    if (d.mpls_link) {
      // CE ruter: prikaz u kartici MPLS.
      if (typeof loadMPLS === "function") { location.hash = "#mpls"; loadMPLS(); }
    } else {
      await loadNet();
      selectDevice(d.id);
    }
  } catch (e) {
    $("nd-msg").className = "bad";
    $("nd-msg").textContent = e.message;
  }
});
$("nd-delete").addEventListener("click", async () => {
  const d = ndEditing;
  if (!d || !confirm(`Obrisati uređaj ${d.name} (${d.ip})? Briše se i njegova istorija odziva i saobraćaja.`)) return;
  try {
    await api("DELETE", `/api/netdev/${encodeURIComponent(d.id)}`);
    $("nd-dlg").close();
    if (ndSelected === d.id) closeDetail();
    setStatus(`Uređaj ${d.name} je obrisan.`, "ok");
    loadNet();
  } catch (e) {
    $("nd-msg").className = "bad";
    $("nd-msg").textContent = e.message;
  }
});

// ---------- uvoz ----------

$("nd-import").addEventListener("click", () => {
  $("nd-import-msg").textContent = "";
  $("nd-import-dlg").showModal();
});
$("nd-import-cancel").addEventListener("click", () => $("nd-import-dlg").close());
$("nd-import-file").addEventListener("change", async (ev) => {
  const f = ev.target.files[0];
  if (!f) return;
  if (f.size > 512 * 1024) {
    $("nd-import-msg").className = "bad";
    $("nd-import-msg").textContent = "Fajl je prevelik (najviše 512 KB).";
    return;
  }
  // Excel na srpskom Windows-u često snima CSV u kodnoj strani 1250.
  const buf = await f.arrayBuffer();
  let text = new TextDecoder("utf-8").decode(buf);
  if (text.includes("�")) text = new TextDecoder("windows-1250").decode(buf);
  $("nd-import-text").value = text;
});
$("nd-import-go").addEventListener("click", async () => {
  const msg = $("nd-import-msg");
  try {
    const r = await api("POST", "/api/netdev/import", { text: $("nd-import-text").value });
    const errs = r.errors || [];
    msg.className = errs.length ? "warn" : "ok";
    msg.replaceChildren(`Dodato: ${r.added}, izmenjeno: ${r.updated}.`,
      ...errs.slice(0, 20).map((e) => el("div", { class: "bad" }, e)),
      errs.length > 20 ? el("div", {}, `… i još ${errs.length - 20} grešaka`) : "");
    loadNet();
  } catch (e) {
    msg.className = "bad";
    msg.textContent = e.message;
  }
});

// ---------- SNMP podešavanja ----------

// Izbor SNMP naloga u Izmeni uređaja (spisak iz SNMP podešavanja).
async function fillProfileSelect(value) {
  const sel = $("nd-f-snmp-profile");
  sel.replaceChildren(el("option", { value: "" }, "podrazumevani"));
  if (value) sel.append(el("option", { value }, value));
  sel.value = value;
  try {
    const c = await api("GET", "/api/snmp");
    sel.replaceChildren(el("option", { value: "" }, "podrazumevani"), ...(c.profiles || []).map((p) => el("option", { value: p.name }, `${p.name} (${p.version})`)));
    sel.value = value;
  } catch { /* ostaje trenutni izbor */ }
}

// ---------- dodatni SNMP nalozi ----------

let snmpProfiles = []; // [{name, version, user, auth_proto, priv_proto, *_set, devices}]

const AUTH_OPTS = [["SHA", "SHA"], ["SHA256", "SHA-256"], ["SHA384", "SHA-384"], ["SHA512", "SHA-512"], ["MD5", "MD5 (zastarelo)"]];
const PRIV_OPTS = [["AES", "AES-128"], ["AES256C", "AES-256 (Cisco)"], ["AES192C", "AES-192 (Cisco)"], ["AES256", "AES-256"], ["AES192", "AES-192"], ["DES", "DES (zastarelo)"]];

function renderSNMPProfiles() {
  const box = $("snmp-profiles");
  box.replaceChildren(...snmpProfiles.map((p, i) => {
    const sel = (opts, v, f) => {
      const s = el("select", { onchange: (e) => { p[f] = e.target.value; if (f === "version") renderSNMPProfiles(); } },
        ...opts.map(([k, l]) => el("option", { value: k }, l)));
      s.value = v;
      return s;
    };
    const txt = (v, f, attrs = {}) => el("input", Object.assign({ value: v || "", autocomplete: "off", oninput: (e) => { p[f] = e.target.value; } }, attrs));
    const pass = (f, set) => el("input", { type: "password", autocomplete: "new-password", placeholder: set ? "postavljena – prazno = bez promene" : "",
      oninput: (e) => { p[f] = e.target.value; } });
    const v3 = p.version === "v3";
    return el("fieldset", { class: "snmp-profile" },
      el("legend", {}, p.name || "novi nalog", p.devices ? el("span", { class: "muted small" }, ` · koristi ${p.devices} uređaj(a)`) : null),
      el("div", { class: "grid lan-params" },
        el("label", {}, "Naziv", txt(p.name, "name", { placeholder: "operater CE ruteri", maxlength: 40 })),
        el("label", {}, "Verzija", sel([["v2c", "SNMPv2c (community)"], ["v3", "SNMPv3"]], p.version, "version")),
        ...(v3 ? [
          el("label", {}, "Korisnik", txt(p.user, "user")),
          el("label", {}, "Autentifikacija", sel(AUTH_OPTS, p.auth_proto || "SHA", "auth_proto")),
          el("label", {}, "Lozinka za autentifikaciju", pass("auth_pass", p.auth_pass_set)),
          el("label", {}, "Šifrovanje", sel(PRIV_OPTS, p.priv_proto || "AES", "priv_proto")),
          el("label", {}, "Lozinka za šifrovanje", pass("priv_pass", p.priv_pass_set)),
        ] : [el("label", {}, "Community", pass("community", p.community_set))])),
      el("div", { class: "actions" }, el("button", {
        type: "button", disabled: p.devices > 0, title: p.devices ? "Nalog koriste uređaji – prvo im izaberite drugi nalog" : "",
        onclick: () => { snmpProfiles.splice(i, 1); renderSNMPProfiles(); },
      }, "Ukloni nalog")));
  }));
}

$("snmp-add-profile").addEventListener("click", () => {
  snmpProfiles.push({ name: "", version: "v2c", auth_proto: "SHA", priv_proto: "AES" });
  renderSNMPProfiles();
});

function snmpVersionChanged() {
  const v3 = $("snmp-version").value === "v3";
  for (const n of document.querySelectorAll("#snmp-form .snmp-v3")) n.hidden = !v3;
  for (const n of document.querySelectorAll("#snmp-form .snmp-v2c")) n.hidden = v3;
}

$("snmp-version").addEventListener("change", snmpVersionChanged);
// loadSNMP popunjava karticu „SNMP” u prozoru podešavanja.
async function loadSNMP() {
  $("snmp-msg").textContent = "";
  try {
    const c = await api("GET", "/api/snmp");
    $("snmp-version").value = c.version || "v3";
    $("snmp-user").value = c.user || "";
    $("snmp-auth-proto").value = c.auth_proto || "SHA";
    $("snmp-priv-proto").value = c.priv_proto || "AES";
    $("snmp-port").value = c.port || 161;
    $("snmp-interval").value = c.interval_sec || 60;
    $("snmp-timeout").value = c.timeout_sec || 3;
    for (const [id, set] of [["snmp-auth-pass", c.auth_pass_set], ["snmp-priv-pass", c.priv_pass_set], ["snmp-community", c.community_set]]) {
      $(id).value = "";
      $(id).placeholder = set ? "postavljena – prazno = bez promene" : "";
    }
    snmpProfiles = (c.profiles || []).map((p) => Object.assign({}, p));
    renderSNMPProfiles();
  } catch (e) {
    $("snmp-msg").className = "bad";
    $("snmp-msg").textContent = e.message;
  }
  snmpVersionChanged();
}

$("nd-snmp").addEventListener("click", () => openSettings("snmp"));
$("snmp-form").addEventListener("submit", async (ev) => {
  ev.preventDefault();
  const body = {
    version: $("snmp-version").value, user: $("snmp-user").value.trim(),
    auth_proto: $("snmp-auth-proto").value, priv_proto: $("snmp-priv-proto").value,
    port: intVal("snmp-port"), interval_sec: intVal("snmp-interval"), timeout_sec: intVal("snmp-timeout"),
  };
  if ($("snmp-auth-pass").value) body.auth_pass = $("snmp-auth-pass").value;
  if ($("snmp-priv-pass").value) body.priv_pass = $("snmp-priv-pass").value;
  if ($("snmp-community").value) body.community = $("snmp-community").value;
  // Lozinke dodatnih naloga idu samo ako su upisane (inače ostaju stare).
  body.profiles = snmpProfiles.map((p) => {
    const o = { name: (p.name || "").trim(), version: p.version, user: (p.user || "").trim(), auth_proto: p.auth_proto || "", priv_proto: p.priv_proto || "" };
    if (p.community) o.community = p.community;
    if (p.auth_pass) o.auth_pass = p.auth_pass;
    if (p.priv_pass) o.priv_pass = p.priv_pass;
    return o;
  });
  try {
    await api("POST", "/api/snmp", body);
    ndDetailMsg = null; // stara poruka („SNMP nije podešen”) više ne važi
    await loadSNMP();
    $("snmp-msg").className = "ok";
    $("snmp-msg").textContent = "Sačuvano. Uređaji se čitaju u sledećem krugu (do 1 min); odmah možete proveriti " +
      "dugmetom „Proveri SNMP” u detaljima uređaja.";
    if (currentPage() === "net") loadNet();
  } catch (e) {
    $("snmp-msg").className = "bad";
    $("snmp-msg").textContent = e.message;
  }
});

// ---------- osvežavanje ----------

let netTicks = 0;
setInterval(() => {
  netTicks++;
  if (currentPage() === "net") {
    loadNet();
    if (netTicks % 3 === 0) {
      loadNetEvents();
      if (ndSelected) loadDetail();
    }
  } else if (netTicks % 3 === 0) {
    loadNet(); // samo oznaka u navigaciji
  }
}, 10000);

showPage();
