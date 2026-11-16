"use strict";
// Kartica MPLS: veze lokacija sa Glavnim Centrom preko operatera. Za svaku
// lokaciju primarni i sekundarni (rezervni) CE ruter: stanje
// linka (WAN interfejs, BGP), aktivni link (VRRP ili saobraćaj) i zauzeće.

let mplsData = { sites: [], events: [] };
let mplsSel = stored("nm.mplsSel") || null;
let mplsRange = validRange(stored("nm.mplsRange"));
let mplsTimer = null;
let mplsRouter = stored("nm.mplsRouter") || null; // otvoreni detalji CE rutera (device_id)
let mplsLive = null; // { id, idx, pts: [[t, in, out]], timer, err }

// Tabela uživo: dok je kartica otvorena, saobraćaj i stanje WAN interfejsa
// svih CE rutera čitaju se na 5 s; prikazuje se prosek za poslednjih 10 s.
let mplsTab = { timer: null, by: {} }; // by[device_id] = { pts: [[t, in, out]], oper, err, t }

async function mplsTableTick() {
  clearTimeout(mplsTab.timer);
  mplsTab.timer = null;
  if (currentPage() !== "mpls") { mplsTab.by = {}; return; }
  let r = null;
  try { r = await api("GET", "/api/mpls/live"); } catch { /* sledeći pokušaj */ }
  if (currentPage() !== "mpls") { mplsTab.by = {}; return; }
  const now = Date.now() / 1000;
  for (const [id, x] of Object.entries(r || {})) {
    const l = mplsTab.by[id] || (mplsTab.by[id] = { pts: [] });
    l.oper = x.oper; l.err = x.error; l.t = x.t;
    if (x.in_bps !== undefined && x.in_bps !== null && (!l.pts.length || x.t > l.pts[l.pts.length - 1][0])) l.pts.push([x.t, x.in_bps, x.out_bps]);
    l.pts = l.pts.filter((p) => p[0] > now - 11);
  }
  if (r) renderMPLS();
  mplsTab.timer = setTimeout(mplsTableTick, 5000);
}

// mplsLiveOf: prosek očitavanja uživo (ako je svež) ili null.
function mplsLiveOf(id) {
  const l = mplsTab.by[id];
  if (!l || !l.t || Date.now() / 1000 - l.t > 15) return null;
  const n = l.pts.length;
  return {
    oper: l.oper, err: l.err,
    in_bps: n ? l.pts.reduce((a, p) => a + p[1], 0) / n : null,
    out_bps: n ? l.pts.reduce((a, p) => a + p[2], 0) / n : null,
  };
}

const MPLS_EV_ICONS = { mpls_down: ["✕", "bad"], mpls_up: ["✓", "ok"] };

async function loadMPLS() {
  clearTimeout(mplsTimer);
  try {
    mplsData = await api("GET", "/api/mpls");
  } catch (e) {
    if (currentPage() === "mpls") setStatus(`MPLS: ${e.message}`, "bad");
    return;
  }
  if (currentPage() !== "mpls") return;
  renderMPLS();
  if (mplsRouter) refreshRouterDetail(); else renderMPLSDetail(false);
  renderMPLSEvents();
  mplsTimer = setTimeout(loadMPLS, 30000);
  if (!mplsTab.timer) mplsTableTick();
}

const pctOf = (bps, mbps) => (bps === undefined || bps === null || !mbps ? null : Math.round(bps / (mbps * 10000)));

// mplsLinkCell: stanje jednog linka u tabeli.
function mplsLinkCell(v0) {
  if (!v0) return el("td", { class: "muted" }, "nije podešen");
  const live = mplsLiveOf(v0.device_id);
  const v = live && live.in_bps !== null ? { ...v0, in_bps: live.in_bps, out_bps: live.out_bps } : v0;
  const up = v.link_up === undefined || v.link_up === null ? null : v.link_up;
  const state = up === null ? el("span", { class: "muted" }, "čeka čitanje…")
    : up ? el("span", { class: "ok" }, "radi") : el("span", { class: "bad" }, `NE RADI${v.link_since ? ` od ${new Date(v.link_since).toLocaleTimeString("sr-RS", { hour: "2-digit", minute: "2-digit" })}` : ""}`);
  const rate = (bps) => {
    if (bps === undefined || bps === null) return "—";
    const p = pctOf(bps, v.speed_mbps);
    return p === null ? fmtBps(bps) : `${fmtBps(bps)} (${p}%)`;
  };
  const util = Math.max(pctOf(v.in_bps, v.speed_mbps) || 0, pctOf(v.out_bps, v.speed_mbps) || 0);
  return el("td", {},
    el("div", {}, el("button", { type: "button", class: "linklike", title: "Detalji rutera", onclick: (e) => { e.stopPropagation(); openMPLSRouter(v); } },
      el("strong", {}, v.name)), v.kind ? el("span", { class: "muted small" }, ` (${v.kind})`) : null, " ", state),
    v.reason && up === false ? el("div", { class: "bad small" }, v.reason) : null,
    live && live.oper && live.oper !== "up" && up !== false ? el("div", { class: "bad small" }, `WAN interfejs: ${live.oper} (uživo)`) : null,
    el("div", { class: "small" }, `↓ ${rate(v.in_bps)}  ↑ ${rate(v.out_bps)}`, v.speed_mbps ? el("span", { class: "muted" }, ` od ${v.speed_mbps} Mb/s`) : null),
    util ? el("div", { class: "pbar" }, el("i", { style: `width:${Math.min(100, util)}%`, class: util >= 80 ? "bad" : "" })) : null,
    el("div", { class: "muted small" }, [
      v.bgp_peers ? `BGP ${v.bgp_up}/${v.bgp_peers}` : "",
      vrrpText(v),
      v.rtt_ms !== undefined ? `ping ${ms(v.rtt_ms)}` : v.status === "down" ? "ne odgovara na ping" : "",
      v.if_name ? `${v.if_name}${v.if_auto ? " (auto)" : ""}` : "",
    ].filter(Boolean).join(" · ")),
    v.error ? el("div", { class: "warn-text small", title: v.error }, `SNMP: ${v.error}`) : null);
}

// vrrpText: ruter može imati više VRRP grupa; „master 3/8” = master za 3 od 8.
function vrrpText(v) {
  if (!v.vrrp) return "";
  if (v.vrrp_groups > 1 && v.vrrp !== "init") return `VRRP master ${v.vrrp_master}/${v.vrrp_groups}`;
  return `VRRP ${v.vrrp}`;
}

function renderMPLS() {
  const tb = $("mpls-table").tBodies[0];
  const sites = mplsData.sites || [];
  $("mpls-empty").hidden = sites.length > 0;
  const backup = sites.filter((s) => s.active === "secondary").length;
  const down = sites.filter((s) => [s.primary, s.secondary].some((v) => v && v.link_up === false)).length;
  const live = Object.keys(mplsTab.by).length > 0;
  $("mpls-count").textContent = `${sites.length} lokacija` + (backup ? ` · ${backup} preko sekundarnog` : "") + (down ? ` · ${down} sa linkom koji ne radi` : "") +
    (live ? " · saobraćaj uživo (prosek 10 s)" : "");
  tb.replaceChildren(...sites.map((s) => el("tr", {
    class: `clickable${s.location === mplsSel ? " selected" : ""}`,
    title: "Grafikon zauzeća oba linka",
    onclick: () => {
      mplsSel = s.location === mplsSel && !mplsRouter ? null : s.location;
      store("nm.mplsSel", mplsSel || "");
      closeRouter();
      renderMPLS();
      renderMPLSDetail(true);
    },
  },
  el("td", {}, el("strong", {}, s.location)),
  el("td", {}, s.active === "primary" ? el("span", { class: "ok" }, "primarnog")
    : s.active === "secondary" ? el("span", { class: "warn-text" }, "SEKUNDARNOG")
      : el("span", { class: "muted" }, "—")),
  mplsLinkCell(s.primary),
  mplsLinkCell(s.secondary))));
}

// ---------- detalji CE rutera (u kartici MPLS) ----------

function findLink(id) {
  for (const s of mplsData.sites || []) {
    for (const [k, v] of [["primary", s.primary], ["secondary", s.secondary]]) if (v && v.device_id === id) return { site: s, link: k, v };
  }
  return null;
}

function openMPLSRouter(v) {
  mplsRouter = v.device_id;
  store("nm.mplsRouter", mplsRouter);
  renderRouterDetail(true);
}

function closeRouter() {
  stopMPLSLive();
  mplsRouter = null;
  store("nm.mplsRouter", "");
}

function stopMPLSLive() {
  if (mplsLive) clearTimeout(mplsLive.timer);
  mplsLive = null;
}

// routerTiles: pločice sa stanjem rutera i linka.
function routerTiles(dv, v) {
  const s = dv.snmp_info || {};
  const up = v.link_up;
  return [
    tileKV("Stanje (ping)", dv.status === "up" ? "na vezi" : dv.status === "down" ? "nije na vezi" : "—", dv.status === "up" ? "ok" : dv.status === "down" ? "bad" : "", fmtSince(dv.since)),
    tileKV("MPLS link", up === true ? "radi" : up === false ? "NE RADI" : "—", up === true ? "ok" : up === false ? "bad" : "", v.reason || ""),
    tileKV("Odziv", dv.rtt_ms !== undefined ? ms(dv.rtt_ms) : "—"),
    tileKV("Gubitak (posl. 10 min)", dv.loss_pct !== undefined ? `${dv.loss_pct}%` : "—"),
    tileKV("BGP (operater)", v.bgp_peers ? `${v.bgp_up}/${v.bgp_peers} uspostavljeno` : "—", v.bgp_peers && !v.bgp_up ? "bad" : ""),
    tileKV("VRRP", v.vrrp ? vrrpText(v).replace(/^VRRP /, "") : "—", v.vrrp === "master" ? "ok" : "",
      v.vrrp === "partial" ? "master samo za deo grupa; aktivni link se određuje po saobraćaju" : ""),
    tileKV("WAN interfejs", v.if_name ? `${v.if_name}${v.if_auto ? " (auto)" : ""}` : "—", "", v.if_alias || ""),
    tileKV("Ugovorena brzina", v.speed_mbps ? `${v.speed_mbps} Mb/s` : "—"),
    tileKV("Model", s.model || "—"),
    tileKV("Verzija softvera", s.version || "—"),
    tileKV("Serijski broj", s.serial || "—"),
    tileKV("Radi bez restarta", s.uptime_sec ? fmtUptime(s.uptime_sec) : "—"),
  ];
}

async function renderRouterDetail(scroll) {
  const box = $("mpls-detail");
  const f = findLink(mplsRouter);
  if (!f || currentPage() !== "mpls") {
    box.hidden = true;
    stopMPLSLive();
    return;
  }
  const { site, link, v } = f;
  let dv;
  try {
    dv = await api("GET", `/api/netdev/${encodeURIComponent(v.device_id)}`);
  } catch (e) {
    setStatus(`MPLS: ${e.message}`, "bad");
    return;
  }
  mplsSel = site.location;
  renderMPLS();
  box.hidden = false;
  box.replaceChildren(...[
    el("div", { class: "nd-head" },
      el("h2", {}, el("span", { class: `dot ${dv.status === "up" ? "on" : dv.status === "down" ? "down" : ""}` }), v.name,
        el("span", { class: "muted h-note" }, `CE ruter · ${link === "primary" ? "primarni" : "sekundarni (rezervni)"} link${v.kind ? ` (${v.kind})` : ""} · ${site.location}`)),
      el("span", { class: "spacer" }),
      el("div", { class: "actions" },
        el("button", { type: "button", disabled: !!devPingBusy(dv), onclick: () => openDevicePing(dv) }, "Ping"),
        isAdmin() ? el("button", { type: "button", onclick: () => routerSNMPTest(dv) }, "Proveri SNMP") : null,
        isAdmin() ? el("button", { type: "button", onclick: () => editMPLSRouter(v, link, site.location) }, "Izmeni") : null,
        el("button", { type: "button", onclick: () => { closeRouter(); mplsSel = null; store("nm.mplsSel", ""); renderMPLS(); renderMPLSDetail(); }, title: "Zatvori detalje", "aria-label": "Zatvori detalje" }, "×"))),
    el("p", { class: "muted nd-sub" }, [site.location, dv.place, dv.ip, dv.note].filter(Boolean).join(" · ")),
    el("p", { id: "mr-msg", class: "muted" }),
    v.error ? el("p", { class: "warn" }, `SNMP: ${v.error}`) : null,
    el("div", { id: "mr-tiles", class: "nd-tiles" }, ...routerTiles(dv, v)),
    el("h3", {}, `Uživo – WAN interfejs ${v.if_name || ""}`),
    el("p", { class: "muted small" }, "očitavanje na 5 s dok su detalji otvoreni, prikaz proseka za 10 s, poslednjih 5 minuta"),
    el("div", { id: "mr-live-now", class: "small" }),
    chartBox("mr-live"),
    rangeBar(mplsRange, (r) => { mplsRange = r; store("nm.mplsRange", r); renderRouterDetail(); }),
    el("h3", {}, "Zauzeće WAN interfejsa"),
    chartBox("mr-wan"),
    el("h3", {}, "Odziv (ping sa konzole)"),
    chartBox("mr-rtt"),
    (v.ifaces || []).length ? el("details", { class: "mr-ifaces" },
      el("summary", {}, `Interfejsi rutera (${v.ifaces.length})`),
      el("div", { class: "table-wrap" }, el("table", {},
        el("thead", {}, el("tr", {}, el("th", {}, "Interfejs"), el("th", {}, "Opis"))),
        el("tbody", {}, ...v.ifaces.map((x) => el("tr", { class: x.index === v.if_index ? "selected" : "" },
          el("td", {}, x.name, x.index === v.if_index ? el("span", { class: "tag" }, "WAN") : null), el("td", { class: "muted" }, x.alias || ""))))))) : null,
  ].filter(Boolean)); // null bi se prikazao kao tekst
  if (scroll) box.scrollIntoView({ behavior: "smooth", block: "start" });
  startMPLSLive(v);
  drawRouterHistory(v);
}

// refreshRouterDetail: periodično osvežavanje samo pločica (grafikoni ostaju).
async function refreshRouterDetail() {
  const f = findLink(mplsRouter);
  if (!f || !$("mr-tiles")) { renderRouterDetail(false); return; }
  try {
    const dv = await api("GET", `/api/netdev/${encodeURIComponent(f.v.device_id)}`);
    if ($("mr-tiles")) $("mr-tiles").replaceChildren(...routerTiles(dv, f.v));
  } catch { /* sledeći put */ }
}

async function drawRouterHistory(v) {
  for (const [series, id, opts] of [
    ["mpls", "mr-wan", { fmt: fmtBps, empty: "Još nema merenja (ruter se čita na 30 s)." }],
    ["rtt", "mr-rtt", { fmt: (x) => `${x.toFixed(1)} ms`, empty: "Još nema merenja." }],
  ]) {
    try {
      const url = historyURL(v.device_id, series, mplsRange);
      const h = await api("GET", url);
      const svg = $(id);
      if (!svg) return;
      const s = series === "mpls"
        ? [{ pts: h.points.map((p) => [p[0], p[1]]), cls: "s1", label: "↓ ka lokaciji" }, { pts: h.points.map((p) => [p[0], p[2]]), cls: "s2", label: "↑ sa lokacije" }]
        : [{ pts: h.points.map((p) => [p[0], p[1]]), cls: "s1", label: "odziv" }];
      drawTimeChart(svg, $(`${id}-hover`), Object.assign({ from: h.from, to: h.to, height: 170, series: s }, opts));
      $(`${id}-legend`).replaceChildren(...s.map((x) => el("span", {}, el("span", { class: `sw ${x.cls === "s2" ? "s2" : ""}` }), x.label)),
        ...(series === "mpls" && v.speed_mbps ? [el("span", {}, `ugovorena brzina ${v.speed_mbps} Mb/s`)] : []),
        h.agg ? el("span", {}, "proseci po 15 min") : "", xlsxButton(url));
    } catch (e) {
      setStatus(`MPLS istorija: ${e.message}`, "bad");
    }
  }
}

// ---------- uživo (5 s) ----------

function startMPLSLive(v) {
  if (!v.if_index) {
    $("mr-live-now").textContent = "WAN interfejs još nije pročitan.";
    return;
  }
  if (!mplsLive || mplsLive.id !== v.device_id || mplsLive.idx !== v.if_index) {
    stopMPLSLive();
    mplsLive = { id: v.device_id, idx: v.if_index, speed: v.speed_mbps, pts: [], raw: [], err: "" };
  }
  liveMPLSTick();
}

async function liveMPLSTick() {
  const l = mplsLive;
  if (!l) return;
  clearTimeout(l.timer);
  if (document.hidden || currentPage() !== "mpls" || mplsRouter !== l.id) {
    if (currentPage() === "mpls" && mplsRouter === l.id) l.timer = setTimeout(liveMPLSTick, 5000);
    return;
  }
  try {
    const r = await api("GET", `/api/netdev/${encodeURIComponent(l.id)}/live?port=${l.idx}`);
    if (l !== mplsLive) return;
    l.err = "";
    if (r.in_bps !== undefined && r.in_bps !== null && (!l.raw.length || r.t > l.raw[l.raw.length - 1][0])) {
      // Ruter brojače podinterfejsa ne osvežava baš na svakih 5 s (pojedina
      // očitavanja pokažu 0, a sledeće dvostruko), pa se prikazuje prosek
      // poslednjih 10 s.
      l.raw.push([r.t, r.in_bps, r.out_bps]);
      l.raw = l.raw.filter((p) => p[0] >= r.t - 300);
      const win = l.raw.filter((p) => p[0] > r.t - 10);
      const avg = (i) => win.reduce((a, p) => a + p[i], 0) / win.length;
      l.pts.push([r.t, avg(1), avg(2)]);
      l.pts = l.pts.filter((p) => p[0] >= r.t - 300);
    }
  } catch (e) {
    l.err = e.message;
  }
  drawMPLSLive();
  if (l === mplsLive) l.timer = setTimeout(liveMPLSTick, 5000);
}

function drawMPLSLive() {
  const l = mplsLive;
  const now = $("mr-live-now");
  const svg = $("mr-live");
  if (!l || !now || !svg) return;
  const last = l.pts[l.pts.length - 1];
  const pct = (b) => (l.speed ? ` (${Math.round(b / (l.speed * 10000))}%)` : "");
  now.replaceChildren(l.err ? el("span", { class: "bad" }, l.err)
    : last ? el("span", {}, el("strong", {}, `↓ ${fmtBps(last[1])}${pct(last[1])}`), "  ", el("strong", {}, `↑ ${fmtBps(last[2])}${pct(last[2])}`),
      el("span", { class: "muted" }, l.speed ? ` od ${l.speed} Mb/s` : ""))
      : el("span", { class: "muted" }, "Prvo očitavanje za 5 s…"));
  const t = last ? last[0] : Math.floor(Date.now() / 1000);
  drawTimeChart(svg, $("mr-live-hover"), {
    from: t - 300, to: t, height: 150, fmt: fmtBps, empty: "Čekam očitavanja…",
    series: [{ pts: l.pts.map((p) => [p[0], p[1]]), cls: "s1", label: "↓ ka lokaciji" }, { pts: l.pts.map((p) => [p[0], p[2]]), cls: "s2", label: "↑ sa lokacije" }],
  });
  $("mr-live-legend").replaceChildren(el("span", {}, el("span", { class: "sw" }), "↓ ka lokaciji"), el("span", {}, el("span", { class: "sw s2" }), "↑ sa lokacije"));
}

async function routerSNMPTest(dv) {
  const msg = $("mr-msg");
  msg.className = "muted";
  msg.textContent = "Proveravam SNMP…";
  try {
    const r = await api("POST", `/api/netdev/${encodeURIComponent(dv.id)}/snmp-test`, {});
    msg.className = r.ok ? "ok" : "bad";
    msg.textContent = r.ok ? `SNMP radi: ${r.system.sys_name || dv.name}${r.system.model ? `, ${r.system.model}` : ""} (odgovor za ${r.ms} ms). Susedi (CDP/LLDP): ${r.neighbors || 0}.`
      : `SNMP ne radi: ${r.error}`;
  } catch (e) {
    msg.className = "bad";
    msg.textContent = `SNMP: ${e.message}`;
  }
}

async function renderMPLSDetail(scroll) {
  if (mplsRouter) return renderRouterDetail(scroll);
  const box = $("mpls-detail");
  const s = (mplsData.sites || []).find((x) => x.location === mplsSel);
  if (!s || currentPage() !== "mpls") {
    box.hidden = true;
    return;
  }
  box.hidden = false;
  const links = [["primary", "Primarni link", s.primary], ["secondary", "Sekundarni (rezervni) link", s.secondary]].filter((x) => x[2]);
  box.replaceChildren(
    el("div", { class: "nd-head" },
      el("h2", {}, `MPLS – ${s.location}`),
      el("span", { class: "spacer" }),
      el("div", { class: "actions" },
        isAdmin() ? el("button", { type: "button", onclick: () => editMPLSRouter(s.primary, "primary", s.location) }, s.primary ? "Izmeni primarni" : "Dodaj primarni") : null,
        isAdmin() ? el("button", { type: "button", onclick: () => editMPLSRouter(s.secondary, "secondary", s.location) }, s.secondary ? "Izmeni sekundarni" : "Dodaj sekundarni") : null,
        el("button", { type: "button", onclick: () => { mplsSel = null; store("nm.mplsSel", ""); renderMPLS(); renderMPLSDetail(); }, title: "Zatvori", "aria-label": "Zatvori" }, "×"))),
    rangeBar(mplsRange, (r) => { mplsRange = r; store("nm.mplsRange", r); renderMPLSDetail(); }),
    el("p", { class: "muted small" }, "Detalji rutera (stanje, uživo, interfejsi): klik na naziv rutera u tabeli."),
    ...links.flatMap(([k, title, v]) => [
      el("h3", {}, `${title}: ${v.name}${v.kind ? ` (${v.kind})` : ""}${v.speed_mbps ? ` · ${v.speed_mbps} Mb/s` : ""}`),
      chartBox(`mpls-chart-${k}`),
    ]));
  if (scroll) box.scrollIntoView({ behavior: "smooth", block: "start" });
  for (const [k, , v] of links) {
    try {
      const url = historyURL(v.device_id, "mpls", mplsRange);
      const h = await api("GET", url);
      const svg = $(`mpls-chart-${k}`);
      if (!svg) return;
      // Agregat (duži periodi): [prosek ↓, prosek ↑, max ↓, max ↑].
      const inP = h.points.map((p) => [p[0], p[1]]);
      const outP = h.points.map((p) => [p[0], p[2]]);
      drawTimeChart(svg, $(`mpls-chart-${k}-hover`), {
        from: h.from, to: h.to, height: 180,
        series: [{ pts: inP, cls: "s1", label: h.agg ? "↓ ka lokaciji (prosek)" : "↓ ka lokaciji" }, { pts: outP, cls: "s2", label: h.agg ? "↑ sa lokacije (prosek)" : "↑ sa lokacije" }],
        fmt: fmtBps, empty: "Još nema merenja (ruter se čita na 30 s).",
      });
      $(`mpls-chart-${k}-legend`).replaceChildren(
        el("span", {}, el("span", { class: "sw" }), "↓ ka lokaciji"),
        el("span", {}, el("span", { class: "sw s2" }), "↑ sa lokacije"),
        ...(v.speed_mbps ? [el("span", {}, `ugovorena brzina ${v.speed_mbps} Mb/s`)] : []),
        h.agg ? el("span", {}, "proseci po 15 min") : "", xlsxButton(url));
    } catch (e) {
      setStatus(`MPLS istorija: ${e.message}`, "bad");
    }
  }
}

// editMPLSRouter: izmena CE rutera (ili dodavanje za lokaciju).
async function editMPLSRouter(v, link, loc) {
  if (!Object.keys(netData.types || {}).length) await loadNet();
  if (!v) {
    openDeviceDialog(null, { type: "router", location: loc, mpls_link: link });
    return;
  }
  try {
    const d = await api("GET", `/api/netdev/${encodeURIComponent(v.device_id)}`);
    d.ifaces = v.ifaces;
    openDeviceDialog(d);
  } catch (e) {
    setStatus(`MPLS: ${e.message}`, "bad");
  }
}

function renderMPLSEvents() {
  const list = $("mpls-events");
  const evs = (mplsData.events || []).slice(0, 30);
  list.replaceChildren(...(evs.length ? evs.map((e) => {
    const [icon, cls] = MPLS_EV_ICONS[e.kind] || ["•", "muted"];
    return el("li", {}, el("span", { class: `ev-icon ${cls}` }, icon), el("span", { class: "muted" }, fmtEvTime(e.time)), " ", e.text);
  }) : [el("li", { class: "muted" }, "Nema događaja.")]));
}

$("mpls-add").addEventListener("click", async () => {
  if (!Object.keys(netData.types || {}).length) await loadNet();
  openDeviceDialog(null, { type: "router", mpls_link: "primary" });
});
