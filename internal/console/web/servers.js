"use strict";
// Stranica „Serveri”: agent za servere javlja stanje (na vezi, uptime, CPU,
// RAM, diskovi). Spisak, detalji sa grafikonom i događaji.

let srvData = { servers: [], cpu_pct: 90, mem_pct: 90, high_min: 10, delay_min: 2, notifying: false };
let srvEvents = [];
let srvSelected = stored("nm.srvSel") || null;
let srvRange = validRange(stored("nm.srvRange"));
let srvEvPage = 1;

const SRV_EV_ICONS = {
  down: ["✕", "bad"], up: ["✓", "ok"], reboot: ["↻", "warn"],
  cpu_high: ["⚠", "warn"], cpu_ok: ["✓", "ok"], mem_high: ["⚠", "warn"], mem_ok: ["✓", "ok"],
};

const fmtGB = (b) => (b >= 10 * 2 ** 30 ? `${Math.round(b / 2 ** 30)} GB` : `${(b / 2 ** 30).toFixed(1)} GB`);
// fmtMem: memorija procesa (MB ispod 1 GB).
const fmtMem = (b) => (b < 2 ** 30 ? `${Math.round(b / 2 ** 20)} MB` : fmtGB(b));

// procTable: procesi koji najviše opterećuju procesor (CPU u % celog
// servera, RAM je radni skup procesa).
function procTable(procs, memTotal) {
  return el("div", { class: "table-wrap" }, el("table", { class: "disks procs" },
    el("thead", {}, el("tr", {}, ...["Proces", "PID", "CPU", "RAM"].map((x) => el("th", {}, x)))),
    el("tbody", {}, ...procs.map((p) => el("tr", {},
      el("td", {}, el("strong", {}, p.name)),
      el("td", { class: "num muted" }, String(p.pid)),
      pctCell(p.cpu_pct, srvData.cpu_pct),
      el("td", { class: "num" }, fmtMem(p.mem),
        memTotal ? el("div", { class: "muted small" }, `${(100 * p.mem / memTotal).toFixed(1)}% RAM-a`) : null))))));
}

// procDetails: spisak procesa sačuvan uz događaj (visok CPU/RAM).
function procDetails(e) {
  if (!e.procs || !e.procs.length) return null;
  return el("details", { class: "ev-procs" },
    el("summary", { class: "muted small" }, `procesi u tom trenutku (najviše: ${e.procs[0].name} ${Math.round(e.procs[0].cpu_pct)}%)`),
    el("ul", { class: "small" }, ...e.procs.map((p) => el("li", {}, `${p.name} (PID ${p.pid}): CPU ${Math.round(p.cpu_pct)}%, RAM ${fmtMem(p.mem)}`))));
}

const pctCls = (v, limit) => (v >= limit ? "bad" : v >= limit - 15 ? "warn-text" : "");

// pctCell: procenat sa trakom (crveno iznad praga).
function pctCell(v, limit, extra) {
  if (v === undefined || v === null) return el("td", { class: "muted" }, "—");
  return el("td", { class: "num" },
    el("div", { class: pctCls(v, limit) }, `${Math.round(v)}%`),
    el("div", { class: "pbar" }, el("i", { style: `width:${Math.min(100, v)}%`, class: v >= limit ? "bad" : "" })),
    extra ? el("div", { class: "muted small" }, extra) : null);
}

function diskText(m) {
  return (m && m.disks || []).map((d) => `${d.name} ${Math.round(100 * (d.total - d.free) / d.total)}%`).join(" · ");
}

// loadServers: tabela se osvežava na 5 s (CPU i RAM sa agenta na 5 s);
// detalji (grafikon iz istorije) samo kada je detail=true (na 30 s).
async function loadServers(detail = true) {
  try {
    srvData = await api("GET", "/api/servers");
  } catch (e) {
    if (currentPage() === "srv") setStatus(`Serveri: ${e.message}`, "bad");
    return;
  }
  updateSrvBadge();
  if (currentPage() !== "srv") return;
  renderServers();
  if (srvSelected && detail) renderSrvDetail();
}

function updateSrvBadge() {
  const off = srvData.servers.filter((s) => !s.online).length;
  const high = srvData.servers.filter((s) => s.cpu_high || s.mem_high).length;
  const b = $("srv-badge");
  b.hidden = off + high === 0;
  b.textContent = `⚠ ${off + high}`;
  b.className = `nav-badge ${off ? "bad" : "warn"}`;
  b.title = [off ? `${off} nije na vezi` : "", high ? `${high} sa visokim opterećenjem` : ""].filter(Boolean).join(", ");
}

function srvMatches(s) {
  const q = $("srv-search").value.trim().toLowerCase();
  return !q || [s.name, s.ip, s.location].some((x) => (x || "").toLowerCase().includes(q));
}

function renderServers() {
  const tb = $("servers").tBodies[0];
  tb.replaceChildren();
  const shown = srvData.servers.filter(srvMatches);
  $("srv-count").textContent = `prikazano ${shown.length}`;
  $("srv-empty").hidden = srvData.servers.length > 0;
  $("srv-note").textContent = `CPU i RAM se osvežavaju na 5 s (CPU je prosek za tih 5 s; agent izdanja 2 i novijeg). Obaveštenje: posle ${srvData.delay_min} min bez veze; CPU iznad ${srvData.cpu_pct}% ili RAM iznad ${srvData.mem_pct}% duže od ${srvData.high_min} min.` +
    (srvData.notifying ? "" : " Nijedan kanal trenutno ne šalje obaveštenja za servere (Podešavanja → Obaveštenja, kolona „Serveri”).");
  for (const s of shown) {
    const m = s.metrics;
    tb.append(el("tr", { class: `clickable${s.agent_id === srvSelected ? " selected" : ""}${s.online ? "" : " row-down"}`, onclick: () => selectServer(s.agent_id) },
      el("td", {}, el("span", { class: `dot ${s.online ? "on" : "down"}` })),
      el("td", {}, el("strong", {}, s.name), el("div", { class: "muted small" }, [s.location, s.ip].filter(Boolean).join(" · "))),
      el("td", {}, el("span", { class: s.online ? "ok" : "bad" }, s.online ? "na vezi" : "nije na vezi"),
        el("div", { class: "muted small" }, fmtSince(s.since))),
      el("td", {}, m ? fmtUptime(m.uptime_sec) : "—"),
      pctCell(m && m.cpu_pct, srvData.cpu_pct),
      pctCell(s.mem_pct, srvData.mem_pct, m ? `${fmtGB(m.mem_used)} / ${fmtGB(m.mem_total)}` : ""),
      el("td", { class: "small" }, diskText(m) || "—")));
  }
}

function selectServer(id) {
  srvSelected = srvSelected === id ? null : id;
  store("nm.srvSel", srvSelected || "");
  renderServers();
  renderSrvDetail();
  if (srvSelected) $("srv-detail").scrollIntoView({ behavior: "smooth", block: "start" });
}

function closeSrvDetail() {
  srvSelected = null;
  store("nm.srvSel", "");
  $("srv-detail").hidden = true;
  $("srv-detail").replaceChildren();
  renderServers();
}

async function renderSrvDetail() {
  const box = $("srv-detail");
  const s = srvData.servers.find((x) => x.agent_id === srvSelected);
  if (!s || currentPage() !== "srv") {
    box.hidden = true;
    return;
  }
  box.hidden = false;
  const m = s.metrics;
  const tiles = [
    tileKV("Stanje", s.online ? "na vezi" : "nije na vezi", s.online ? "ok" : "bad", fmtSince(s.since)),
    tileKV("Uptime", m ? fmtUptime(m.uptime_sec) : "—"),
    tileKV("CPU", m ? `${Math.round(m.cpu_pct)}%` : "—", m ? pctCls(m.cpu_pct, srvData.cpu_pct) : ""),
    tileKV("RAM", s.mem_pct !== undefined ? `${Math.round(s.mem_pct)}% (${fmtGB(m.mem_used)} / ${fmtGB(m.mem_total)})` : "—",
      s.mem_pct !== undefined ? pctCls(s.mem_pct, srvData.mem_pct) : ""),
    tileKV("IP adresa", s.ip || "—"),
    tileKV("Poslednji put viđen", s.last_seen ? ago(s.last_seen) : "—"),
    tileKV("Verzija agenta", [s.version || "—", s.outdated ? el("div", { class: "small warn-text" }, "⚠ potrebna nadogradnja") : null], "",
      s.outdated ? `Izdanje agenta ${s.release}, konzola očekuje ${s.want_release}. Podešavanja → Instalacija agenata → Serveri, „samo nadogradnja”.` : `izdanje agenta ${s.release}`),
  ];
  const disks = (m && m.disks || []).map((d) => {
    const used = 100 * (d.total - d.free) / d.total;
    return el("tr", {}, el("td", {}, el("strong", {}, d.name)),
      pctCell(used, 90), el("td", { class: "num" }, fmtGB(d.total - d.free)), el("td", { class: "num" }, fmtGB(d.free)),
      el("td", { class: "num" }, fmtGB(d.total)));
  });
  const evs = srvEvents.filter((e) => e.device_id === s.agent_id).slice(0, 10);
  box.replaceChildren(...[
    el("div", { class: "nd-head" },
      el("h2", {}, el("span", { class: `dot ${s.online ? "on" : "down"}` }), s.name, el("span", { class: "muted h-note" }, "server")),
      el("span", { class: "spacer" }),
      el("div", { class: "actions" },
        isAdmin() && !s.online ? el("button", { type: "button", onclick: () => removeServer(s) }, "Ukloni") : null,
        maxButton(box),
        el("button", { type: "button", onclick: closeSrvDetail, title: "Zatvori detalje", "aria-label": "Zatvori detalje" }, "×"))),
    el("p", { class: "muted nd-sub" }, [s.location, s.ip].filter(Boolean).join(" · ")),
    !s.online ? el("p", { class: "warn" }, "Agent se ne javlja; prikazani su poslednji podaci iz istorije.") : null,
    el("div", { class: "nd-tiles" }, tiles),
    rangeBar(srvRange, (r) => { srvRange = r; store("nm.srvRange", r); renderSrvDetail(); }),
    el("h3", {}, "CPU i RAM (%)"),
    chartBox("srv-chart"),
    disks.length ? el("h3", {}, "Diskovi") : null,
    disks.length ? el("div", { class: "table-wrap" }, el("table", { class: "disks" },
      el("thead", {}, el("tr", {}, ...["Disk", "Zauzeće", "Zauzeto", "Slobodno", "Ukupno"].map((x) => el("th", {}, x)))),
      el("tbody", {}, ...disks))) : null,
    m && s.online ? el("h3", {}, "Procesi sa najvećim zauzećem procesora") : null,
    m && s.online ? (m.procs && m.procs.length ? procTable(m.procs, m.mem_total)
      : el("p", { class: "muted" }, "Agent ne šalje procese – nadogradite agenta na ovom serveru (Podešavanja → Instalacija agenata → Serveri, „samo nadogradnja”).")) : null,
    el("h3", {}, "Poslednji događaji servera"),
    el("ul", { class: "nd-ev-list" }, ...(evs.length ? evs.map((e) => {
      const [icon, cls] = SRV_EV_ICONS[e.kind] || ["•", "muted"];
      return el("li", {}, el("span", { class: `ev-icon ${cls}` }, icon), el("span", { class: "muted" }, fmtEvTime(e.time)), " ", e.text, procDetails(e));
    }) : [el("li", { class: "muted" }, "Nema događaja.")])),
  ].filter(Boolean)); // null bi se prikazao kao tekst
  try {
    const url = `/api/servers/${encodeURIComponent(s.agent_id)}/history?${rangeQS(srvRange)}`;
    const h = await api("GET", url);
    const svg = $("srv-chart");
    if (!svg) return;
    // Agregat (više od 24 h): [prosek CPU, prosek RAM, max CPU, max RAM].
    const cpu = h.points.map((p) => [p[0], p[1]]);
    const mem = h.points.map((p) => [p[0], p[2]]);
    drawTimeChart(svg, $("srv-chart-hover"), {
      from: h.from, to: h.to, height: 200,
      series: [{ pts: cpu, cls: "s1", label: h.agg ? "CPU (prosek)" : "CPU" }, { pts: mem, cls: "s2", label: h.agg ? "RAM (prosek)" : "RAM" }],
      fmt: (v) => `${Math.round(v)}%`, empty: "Još nema podataka (agent šalje stanje na 30 s).",
    });
    $("srv-chart-legend").replaceChildren(
      el("span", {}, el("span", { class: "sw" }), "CPU"),
      el("span", {}, el("span", { class: "sw s2" }), "RAM"),
      el("span", {}, `prag za obaveštenje: CPU ${srvData.cpu_pct}%, RAM ${srvData.mem_pct}%, duže od ${srvData.high_min} min`),
      h.agg ? el("span", {}, "proseci po 15 min") : "", xlsxButton(url));
  } catch (e) {
    setStatus(`Istorija servera: ${e.message}`, "bad");
  }
}

async function removeServer(s) {
  if (!confirm(`Ukloniti server ${s.name} iz konzole (sa istorijom)? Ako se agent ponovo javi, server se vraća u spisak.`)) return;
  try {
    await api("DELETE", `/api/servers/${encodeURIComponent(s.agent_id)}`);
    closeSrvDetail();
    loadServers();
  } catch (e) {
    setStatus(`Uklanjanje servera: ${e.message}`, "bad");
  }
}

async function loadSrvEvents() {
  try {
    srvEvents = await api("GET", "/api/servers/events");
  } catch {
    return;
  }
  renderSrvEvents();
}

function renderSrvEvents() {
  const tb = $("srv-events").tBodies[0];
  tb.replaceChildren();
  const size = 15;
  const pages = Math.max(1, Math.ceil(srvEvents.length / size));
  srvEvPage = Math.min(Math.max(1, srvEvPage), pages);
  renderPager($("srv-ev-pager"), srvEvPage, pages, (p) => { srvEvPage = p; renderSrvEvents(); });
  if (!srvEvents.length) {
    tb.append(el("tr", {}, el("td", { colspan: 3, class: "empty" }, "Još nema događaja.")));
    return;
  }
  for (const ev of srvEvents.slice((srvEvPage - 1) * size, srvEvPage * size)) {
    const [icon, cls] = SRV_EV_ICONS[ev.kind] || ["•", "muted"];
    tb.append(el("tr", {},
      el("td", { class: "nowrap" }, fmtEvTime(ev.time)),
      el("td", {}, el("span", { class: `ev-icon ${cls}` }, icon),
        el("button", { type: "button", class: "linklike", onclick: () => selectServer(ev.device_id) }, ev.text), procDetails(ev)),
      el("td", {}, ev.duration_sec ? fmtDuration(ev.duration_sec) : "—")));
  }
}

$("srv-search").addEventListener("input", renderServers);

let srvTicks = 0;
setInterval(() => {
  srvTicks++;
  if (currentPage() === "srv") {
    loadServers(srvTicks % 6 === 0);
    if (srvTicks % 6 === 0) loadSrvEvents();
  } else if (srvTicks % 6 === 0) {
    loadServers(); // samo oznaka u navigaciji
  }
}, 5000);
loadServers();
