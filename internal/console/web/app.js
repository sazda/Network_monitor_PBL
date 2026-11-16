"use strict";

// Svi podaci koji dolaze od agenata (ime računara, interfejsi...) se prikazuju
// isključivo preko textContent, nikad kao HTML.

const $ = (id) => document.getElementById(id);
const busy = new Set(); // agenti na kojima je merenje u toku
let agents = [];
const PAGE_SIZE = 9; // računara po strani u spisku agenata
let agentPage = 1;
let me = { name: "", role: "operator" }; // prijavljeni korisnik
const isAdmin = () => me.role === "admin";
const roleNames = { admin: "Administrator", operator: "Operater" };

function el(tag, attrs = {}, ...children) {
  const e = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (k === "class") e.className = v;
    else if (k.startsWith("on")) e.addEventListener(k.slice(2), v);
    else if (v !== undefined && v !== null && v !== false) e.setAttribute(k, v);
  }
  for (const c of children.flat()) {
    if (c === null || c === undefined) continue;
    e.append(c instanceof Node ? c : document.createTextNode(String(c)));
  }
  return e;
}

// onResponse (opciono) dobija HTTP odgovor, npr. za čitanje zaglavlja.
async function api(method, path, body, onResponse) {
  const opts = { method, headers: { "X-Requested-With": "fetch" } };
  if (body !== undefined) {
    opts.headers["Content-Type"] = "application/json";
    opts.body = JSON.stringify(body);
  }
  const r = await fetch(path, opts);
  if (r.status === 401) {
    // Sesija je istekla ili je lozinka promenjena: nazad na prijavu.
    location.reload();
    throw new Error("potrebna je prijava");
  }
  if (onResponse) onResponse(r);
  // Uspešno snimanje u prozoru podešavanja: nema više nesačuvanih izmena.
  if (r.ok && method !== "GET") settingsDirty = false;
  if (r.status === 204) return null;
  const data = await r.json().catch(() => ({}));
  if (!r.ok && !data.id) throw new Error(data.error || `HTTP ${r.status}`);
  return data;
}

function ago(iso) {
  if (!iso) return "—";
  const s = Math.round((Date.now() - new Date(iso).getTime()) / 1000);
  if (s < 60) return `pre ${s}s`;
  if (s < 3600) return `pre ${Math.round(s / 60)} min`;
  if (s < 86400) return `pre ${Math.round(s / 3600)} h`;
  return new Date(iso).toLocaleString("sr-RS");
}

const ms = (v) => (v === undefined || v === null ? "—" : `${v.toFixed(1)} ms`);
const mbps = (t) => (t ? `${t.mbps.toFixed(1)} Mb/s` : "—");
const name = (id) => (agents.find((a) => a.agent_id === id) || {}).hostname || id;

// Poslednja poruka o vašim aktivnostima (između zaglavlja i spiska agenata).
// Stoji dok je ne zatvorite ili je ne zameni nova poruka.
const ACT_ICONS = { ok: "✓", busy: "⏳", bad: "✕", muted: "•" };

function setStatus(text, cls = "muted") {
  const box = $("activity");
  box.className = `activity ${cls}`;
  box.querySelector(".act-icon").textContent = ACT_ICONS[cls] || "•";
  box.querySelector(".act-time").textContent = new Date().toLocaleTimeString("sr-RS");
  $("status").textContent = text;
  $("activity-close").hidden = false;
}

function clearStatus() {
  const box = $("activity");
  box.className = "activity empty";
  box.querySelector(".act-icon").textContent = "";
  box.querySelector(".act-time").textContent = "";
  $("status").textContent = "Nema aktivnosti";
  $("activity-close").hidden = true;
}
$("activity-close").addEventListener("click", clearStatus);

function intVal(id) {
  const v = parseInt($(id).value, 10);
  return Number.isFinite(v) ? v : 0;
}

async function run(agentId, label, fn) {
  busy.add(agentId);
  renderAgents();
  kickLive();
  setStatus(`${label} na ${name(agentId)} u toku…`, "busy");
  try {
    const res = await fn();
    setStatus(res.ok ? `${label} na ${name(agentId)} završen.` : `${label}: ${res.error}`, res.ok ? "ok" : "bad");
  } catch (e) {
    setStatus(`${label}: ${e.message}`, "bad");
  } finally {
    busy.delete(agentId);
    renderAgents();
    resultsPage = 1; // novo merenje je na prvoj strani
    loadResults();
  }
}

function latency(id, target, method) {
  const label = target === "gateway" ? "Latencija do gateway-a" : "Merenje latencije";
  run(id, label, () =>
    api("POST", `/api/agents/${encodeURIComponent(id)}/latency`, {
      target: target ?? $("p-target").value.trim(),
      method: method ?? $("p-method").value,
      count: intVal("p-count"),
    }));
}

function speedtest(id) {
  run(id, "Test brzine", () =>
    api("POST", `/api/agents/${encodeURIComponent(id)}/speedtest`, {
      direction: $("p-direction").value,
      seconds: intVal("p-seconds"),
      streams: intVal("p-streams"),
    }));
}

// ---------- Ping ----------

// Ping računara sa konzole: na adresu sa koje se računar javlja konzoli.
let pingFrom = null;
const PING_DURATIONS = [[30, "30 s"], [60, "1 min"], [300, "5 min"], [900, "15 min"], [1800, "30 min"], [3600, "1 h"], [7200, "2 h"], [14400, "4 h"], [28800, "8 h"]];
const PING_INTERVALS = [[500, "0,5 s"], [1000, "1 s"], [2000, "2 s"], [5000, "5 s"], [10000, "10 s"]];

function openPing(id) {
  const a = agents.find((x) => x.agent_id === id);
  if (!a) return;
  showPingDialog({ kind: "agent", id, name: a.hostname, ip: agentIP(a), location: a.location,
    tail: a.online ? "." : ". Agent trenutno nije na vezi, ali računar možda radi." });
}

// showPingDialog otvara prozor za ping; t: { kind: "agent" | "device", id, name, ip, location, tail }.
function showPingDialog(t) {
  pingFrom = t;
  $("ping-title").textContent = `Ping: ${t.name}`;
  $("ping-from").textContent = `Konzola šalje ping ${t.kind === "device" ? "uređaju" : "računaru"} ${t.name} na adresu ${t.ip}` +
    (t.location ? ` (${t.location})` : "") + (t.tail || ".");
  const dur = $("ping-duration"), iv = $("ping-interval");
  if (!dur.options.length) {
    for (const [v, l] of PING_DURATIONS) dur.append(el("option", { value: v }, l));
    dur.append(el("option", { value: "custom" }, "Proizvoljno…"));
    dur.append(el("option", { value: "continuous" }, "Neprekidno (kao ping -t), do zaustavljanja"));
    for (const [v, l] of PING_INTERVALS) iv.append(el("option", { value: v }, l));
    dur.value = stored("nm.pingDur") || "60";
    iv.value = stored("nm.pingIv") || "1000";
  }
  $("ping-msg").textContent = "";
  updatePingInfo();
  $("ping-dlg").showModal();
  $("ping-start").focus();
}

const PING_MAX_SEC = 24 * 3600;

// Trajanje iz prozora u sekundama (0 = neispravno), ili "continuous".
function pingDuration() {
  const v = $("ping-duration").value;
  if (v === "continuous") return v;
  if (v !== "custom") return +v;
  const n = (id) => Math.max(0, Math.floor(+$(id).value || 0));
  return n("ping-h") * 3600 + n("ping-m") * 60 + n("ping-s");
}

function updatePingInfo() {
  if (!pingFrom) return;
  const v = $("ping-duration").value;
  $("ping-custom").hidden = v !== "custom";
  const dur = pingDuration(), iv = +$("ping-interval").value;
  const tail = "Ping teče na serveru: možete zatvoriti pretraživač. Po završetku je izveštaj u tabeli Rezultati, dostupan svim korisnicima.";
  let ok = true;
  if (dur === "continuous") {
    $("ping-info").textContent = `Ping se šalje svakih ${fmtSec(iv)} dok ga ne zaustavite dugmetom „Zaustavi ping” u panelu Merenje uživo ` +
      `(ili ga zaustavi drugi tehničar / administrator). Iz bezbednosnih razloga se sam zaustavlja posle 24 h. ${tail}`;
  } else if (!(dur >= 5 && dur <= PING_MAX_SEC)) {
    $("ping-info").textContent = "Trajanje mora biti između 5 sekundi i 24 sata.";
    ok = false;
  } else {
    const n = Math.floor((dur * 1000) / iv);
    $("ping-info").textContent = `Biće poslato ${n} pingova (svakih ${fmtSec(iv)}, ukupno ${fmtDuration(dur)}). ${tail}`;
  }
  $("ping-info").className = ok ? "muted" : "bad";
  $("ping-start").disabled = !ok;
}

// pingTarget pravi cilj pinga iz testa u panelu uživo (za „Ponovi”).
function pingTarget(t) {
  if (t.device) return { kind: "device", id: t.agent_id.replace(/^netdev:/, ""), name: t.host };
  return { kind: "agent", id: t.agent_id, name: t.host };
}

async function startPing(target, req) {
  const what = target.kind === "device" ? "uređaja" : "računara";
  const path = target.kind === "device" ? `/api/netdev/${encodeURIComponent(target.id)}/ping`
    : `/api/agents/${encodeURIComponent(target.id)}/ping`;
  try {
    const r = await api("POST", path, req);
    liveSelected = r.live_id;
    myPings.add(r.result_id);
    // Tok pinga i izveštaj su na stranici „Računari” (panel Merenje uživo, tabela Rezultati).
    const where = target.kind === "device" && location.hash === "#mreza" ? " Tok i izveštaj su na stranici Računari." : "";
    setStatus((req.continuous
      ? `Neprekidni ping ${what} ${target.name} je pokrenut – zaustavite ga u panelu Merenje uživo.`
      : `Ping ${what} ${target.name} je pokrenut (${fmtDuration(req.duration_sec)}).`) + where, "ok");
    kickLive();
    return true;
  } catch (e) {
    setStatus(`Ping: ${e.message}`, "bad");
    return false;
  }
}

for (const id of ["ping-duration", "ping-interval"]) $(id).addEventListener("change", updatePingInfo);
for (const id of ["ping-h", "ping-m", "ping-s"]) $(id).addEventListener("input", updatePingInfo);
$("ping-cancel").addEventListener("click", () => $("ping-dlg").close());
$("ping-start").addEventListener("click", async () => {
  const d = pingDuration();
  const req = d === "continuous" ? { continuous: true, interval_ms: +$("ping-interval").value }
    : { duration_sec: d, interval_ms: +$("ping-interval").value };
  store("nm.pingDur", $("ping-duration").value);
  store("nm.pingIv", String(req.interval_ms));
  if (await startPing(pingFrom, req)) $("ping-dlg").close();
});

// ---------- LAN test: izbor drugog računara ----------

let lanFrom = null; // agent sa kog se meri
let lanPeer = null; // izabrani drugi računar

const agentIP = (a) => (a.remote_addr || "").replace(/:\d+$/, "").replace(/^\[|\]$/g, "");

function lantest(id) {
  lanFrom = agents.find((a) => a.agent_id === id);
  if (!lanFrom) return;
  lanPeer = null;
  // Lokacije: iste kao u filteru spiska agenata.
  const sel = $("lan-loc");
  const cur = sel.value;
  sel.replaceChildren(el("option", { value: "" }, "sve lokacije"));
  for (const n of locationNames) sel.append(el("option", { value: n }, n));
  if (agents.some((a) => !a.location)) sel.append(el("option", { value: NO_LOCATION }, "nepoznata lokacija"));
  sel.value = [...sel.options].some((o) => o.value === cur) ? cur : "";
  $("lan-search").value = "";
  // Podrazumevani parametri iz „Parametara merenja”.
  $("lan-count-lat").value = 5;
  $("lan-seconds").value = intVal("p-seconds") || 5;
  $("lan-streams").value = intVal("p-streams") || 4;
  $("lan-direction").value = $("p-direction").value;
  $("lan-addr").value = "";
  lanStep(1);
  $("lan-dlg").showModal();
  $("lan-search").focus();
}

function lanStep(n) {
  const from = `Merenje sa računara ${lanFrom.hostname}` + (lanFrom.location ? ` (${lanFrom.location})` : "");
  $("lan-from").textContent = n === 1 ? `${from}. Izaberite drugi računar:` : `${from}. Podesite parametre testa:`;
  $("lan-step1").hidden = n !== 1;
  $("lan-step2").hidden = n !== 2;
  $("lan-back").hidden = n !== 2;
  $("lan-start").hidden = n !== 2;
  if (n === 1) renderLanList();
}

function renderLanList() {
  const q = $("lan-search").value.trim().toLowerCase();
  const loc = $("lan-loc").value;
  const list = $("lan-list");
  list.replaceChildren();
  const candidates = agents.filter((a) => a.online && a.agent_id !== lanFrom.agent_id);
  const shown = candidates.filter((a) => {
    if (loc && (a.location || NO_LOCATION) !== loc) return false;
    if (!q) return true;
    const hay = [a.hostname, a.location, agentIP(a), ...(a.interfaces || []).flatMap((i) => i.addrs)].join(" ").toLowerCase();
    return q.split(/\s+/).every((w) => hay.includes(w));
  });
  const rbusy = remoteBusy();
  for (const a of shown) {
    const bt = rbusy.get(a.agent_id);
    list.append(el("button", { type: "button", class: "pick", role: "option", disabled: !!bt, title: bt ? busyLabel(bt) : null, onclick: () => pickLanPeer(a) },
      el("span", { class: "h" }, a.hostname || a.agent_id),
      el("span", { class: "l" }, a.location || "—"),
      el("span", { class: "a" }, agentIP(a)),
      el("span", { class: "a" }, bt ? "zauzet – merenje u toku" : needsUpgrade(a) ? "⚠ stara verzija agenta" : ""),
    ));
  }
  if (!shown.length) {
    list.append(el("div", { class: "empty" }, candidates.length ? "Nijedan računar ne odgovara pretrazi." : "Nema drugih računara na vezi."));
  }
  $("lan-count").textContent = `Na vezi: ${candidates.length} · prikazano: ${shown.length}. Prikazani su samo računari koji su na vezi.`;
}

function pickLanPeer(a) {
  lanPeer = a;
  const pair = $("lan-pair");
  pair.replaceChildren(
    el("strong", {}, lanFrom.hostname), el("span", { class: "arrow" }, "↔"), el("strong", {}, a.hostname),
    el("div", { class: "muted" }, `${lanFrom.location || "nepoznata lokacija"} ↔ ${a.location || "nepoznata lokacija"}`),
  );
  const notes = [];
  if (!(lanFrom.features || []).includes("live")) notes.push(`${lanFrom.hostname} ima stariju verziju agenta: broj merenja latencije se ne primenjuje (uvek 5) i nema prikaza uživo.`);
  $("lan-msg").textContent = notes.join(" ");
  lanStep(2);
  $("lan-seconds").focus();
}

$("lan-search").addEventListener("input", renderLanList);
$("lan-loc").addEventListener("change", renderLanList);
$("lan-search").addEventListener("keydown", (ev) => {
  // Enter bira prvi računar sa spiska.
  if (ev.key === "Enter") {
    ev.preventDefault();
    const first = $("lan-list").querySelector(".pick:not(:disabled)");
    if (first) first.click();
  }
});
$("lan-back").addEventListener("click", () => lanStep(1));
$("lan-cancel").addEventListener("click", () => $("lan-dlg").close());
$("lan-start").addEventListener("click", () => {
  const from = lanFrom, peer = lanPeer;
  if (!from || !peer) return;
  const body = {
    client_id: from.agent_id,
    server_id: peer.agent_id,
    server_addr: $("lan-addr").value.trim(),
    direction: $("lan-direction").value,
    seconds: intVal("lan-seconds"),
    streams: intVal("lan-streams"),
    latency_count: intVal("lan-count-lat"),
  };
  $("lan-dlg").close();
  run(from.agent_id, "LAN test", () => api("POST", "/api/lan-test", body));
});

async function forget(id) {
  if (!confirm(`Ukloniti agenta ${name(id)} iz spiska?`)) return;
  try {
    await api("DELETE", `/api/agents/${encodeURIComponent(id)}`);
    loadAgents();
  } catch (e) {
    setStatus(e.message, "bad");
  }
}

// ---------- pretraga i filter po lokaciji ----------

const NO_LOCATION = "__none__";
let locationNames = []; // redosled iz podešavanja

function store(key, value) {
  try { localStorage.setItem(key, value); } catch { /* nije bitno */ }
}
function stored(key) {
  try { return localStorage.getItem(key) || ""; } catch { return ""; }
}

// Izabrane lokacije (prazno = sve). NO_LOCATION označava agente bez lokacije.
let selectedLocations = new Set((() => {
  try { return JSON.parse(stored("nm.locations") || "[]"); } catch { return []; }
})());

function saveSelectedLocations() {
  store("nm.locations", JSON.stringify([...selectedLocations]));
}

function agentMatches(a) {
  if (selectedLocations.size && !selectedLocations.has(a.location || NO_LOCATION)) return false;
  if ($("a-online").checked && !a.online) return false;
  const q = $("a-search").value.trim().toLowerCase();
  if (!q) return true;
  const hay = [
    a.hostname, a.agent_id, a.remote_addr, a.gateway, a.location, a.os,
    ...(a.interfaces || []).flatMap((i) => i.addrs),
  ].join(" ").toLowerCase();
  return q.split(/\s+/).every((w) => hay.includes(w));
}

function updateLocationSummary() {
  const names = [...selectedLocations].map((n) => (n === NO_LOCATION ? "nepoznata" : n));
  let text = "Lokacije: sve";
  if (names.length === 1) text = `Lokacija: ${names[0]}`;
  else if (names.length > 1 && names.length <= 3) text = `Lokacije: ${names.join(", ")}`;
  else if (names.length > 3) text = `Lokacije: ${names.length} izabrane`;
  $("a-loc-summary").textContent = text;
}

function fillLocationSelect() {
  const list = $("a-loc-list");
  const count = (name) => agents.filter((a) => (a.location || "") === name).length;
  list.replaceChildren();
  if (!locationNames.length) {
    list.append(el("div", { class: "muted" }, "Lokacije nisu podešene (Podešavanja → Lokacije)."));
  }
  const items = locationNames.map((n) => [n, n, count(n)]);
  const unknown = count("");
  if (unknown > 0 || selectedLocations.has(NO_LOCATION)) items.push([NO_LOCATION, "nepoznata lokacija", unknown]);
  // Izbor lokacije koja više ne postoji se zaboravlja.
  for (const v of [...selectedLocations]) {
    if (!items.some(([key]) => key === v)) selectedLocations.delete(v);
  }
  for (const [key, label, n] of items) {
    const cb = el("input", { type: "checkbox" });
    cb.checked = selectedLocations.has(key);
    cb.addEventListener("change", () => {
      if (cb.checked) selectedLocations.add(key); else selectedLocations.delete(key);
      saveSelectedLocations();
      updateLocationSummary();
      filtersChanged();
    });
    list.append(el("label", {}, cb, label, el("span", { class: "n" }, String(n))));
  }
  updateLocationSummary();
}

// ---------- verzija agenta ----------

// Nadogradnja se traži po izdanju agenta (broj koji se povećava samo kada se
// agent promeni), a ne po oznaci build-a: nova konzola ne znači da agenti
// moraju da se nadograde. Konzola računa „outdated”.
const needsUpgrade = (a) => !!a.outdated;
// Verzije iz git-a su „1.2.3”, a iz ZIP build-a „build-…”: „v” samo ispred broja.
const fmtVersion = (v) => (/^\d/.test(v || "") ? `v${v}` : v || "?");

function upgradeReason(a) {
  const lines = [a.release
    ? `Izdanje agenta je ${a.release}, a konzola očekuje izdanje ${a.want_release}: nova verzija agenta donosi izmene.`
    : `Agent (${a.version || "?"}) je iz verzije pre uvođenja izdanja agenta; konzola očekuje izdanje ${a.want_release}. Nadogradnja je potrebna jednom.`];
  if (!(a.features || []).includes("live")) {
    lines.push("Ova verzija ne šalje međurezultate: LAN test i latencija se ne prikazuju uživo.");
  }
  lines.push("Nadogradnja: Podešavanja → Instalacija agenata → „samo nadogradnja”.");
  return lines.join("\n");
}

function renderUpgradeBanner() {
  const old = agents.filter(needsUpgrade);
  const b = $("a-upgrade");
  b.hidden = old.length === 0;
  b.replaceChildren();
  if (!old.length) return;
  const n = old.length;
  const noun = n === 1 ? "agent ima" : n < 5 ? "agenta imaju" : "agenata ima";
  const noLive = old.some((a) => !(a.features || []).includes("live"));
  b.append(el("span", { class: "t" },
    el("strong", { class: "busy" }, `⚠ ${n} ${noun} staru verziju. `),
    "Označeni su u spisku. Stare verzije rade, ali " +
      (noLive ? "ne prikazuju LAN test i latenciju uživo i nemaju najnovije izmene agenta." : "nemaju najnovije izmene agenta.")));
  if (isAdmin()) {
    b.append(el("button", { type: "button", onclick: () => prepareUpgrade(old) }, "Pripremi nadogradnju"));
  } else {
    b.append(el("span", { class: "muted" }, "Nadogradnju pokreće administrator."));
  }
}

// Otvara podešavanja sa popunjenim spiskom računara za nadogradnju.
// Dodaju se samo računari koji su na vezi: na ostale instalacija ne može da
// stigne (biće dodati kada se pojave).
function prepareUpgrade(list) {
  const online = list.filter((a) => a.online);
  openSettings("deploy");
  $("d-kind").value = "";
  deployKindChanged();
  $("d-hosts").value = "";
  addHosts(online.map((a) => a.hostname));
  $("d-upgrade").checked = true;
  $("d-url").disabled = true;
  setStatus(upgradeMsg(online.length, list.length - online.length, "računara") +
    (online.length ? " Unesite administratorski nalog i kliknite Instaliraj." : ""), online.length ? "ok" : "bad");
}

// upgradeMsg: koliko je dodato za nadogradnju i koliko je preskočeno (nisu na vezi).
function upgradeMsg(added, offline, what) {
  if (!added && !offline) return `Svi agenti imaju istu verziju kao konzola.`;
  return `Dodato ${what} za nadogradnju (na vezi): ${added}.` + (offline ? ` Nisu na vezi (preskočeni): ${offline}.` : "");
}

function renderAgents() {
  const tb = $("agents").tBodies[0];
  tb.replaceChildren();
  $("no-agents").hidden = agents.length > 0;
  renderUpgradeBanner();
  const shown = agents.filter(agentMatches);
  const pages = Math.max(1, Math.ceil(shown.length / PAGE_SIZE));
  // Spisak se osvežava svakih 5 s; ako se broj računara smanjio, ostajemo na poslednjoj strani.
  agentPage = Math.min(Math.max(1, agentPage), pages);
  const from = (agentPage - 1) * PAGE_SIZE;
  const page = shown.slice(from, from + PAGE_SIZE);
  let count = "";
  if (agents.length) {
    count = page.length < shown.length
      ? `prikazano ${from + 1}–${from + page.length} od ${shown.length}`
      : `prikazano ${shown.length}`;
    if (!shown.length) count = `prikazano 0 od ${agents.length}`;
    else if (shown.length !== agents.length) count += ` (ukupno ${agents.length})`;
  }
  $("a-count").textContent = count;
  renderPager($("a-pager"), agentPage, pages, (p) => { agentPage = p; renderAgents(); });
  if (agents.length && !shown.length) {
    tb.append(el("tr", {}, el("td", { colspan: 10, class: "empty" }, "Nijedan računar ne odgovara pretrazi ili filteru.")));
  }
  for (const a of page) {
    const addrs = (a.interfaces || []).flatMap((i) => i.addrs.map((x) => `${i.name}: ${x}`));
    const rb = remoteBusy().get(a.agent_id);
    const pb = pingBusy().get(a.agent_id);
    const dis = !a.online || busy.has(a.agent_id) || !!rb;
    const upg = needsUpgrade(a);
    tb.append(el("tr", { class: upg ? "outdated" : "" },
      el("td", {}, el("span", { class: a.online ? "dot on" : "dot", title: a.online ? "na vezi" : "nije na vezi" })),
      el("td", {}, el("strong", {}, a.hostname || "?"),
        a.watched ? el("span", { class: "bell", title: "Obaveštenje kada nije na vezi" }, "🔔") : null,
        el("div", { class: "muted" }, a.agent_id),
        rb ? el("div", { class: "busy", style: "font-size:12px" }, `⏳ ${busyLabel(rb)}`) : null,
        pb ? el("div", { class: "busy", style: "font-size:12px" }, `⏳ ${pb.continuous ? "neprekidni ping" : "ping"} u toku (pokrenuo ${pb.user === me.name ? "vi" : pb.user || "?"})`) : null),
      el("td", { class: "loc" }, a.location || el("span", { class: "muted" }, "—")),
      el("td", {}, `${a.os}/${a.arch}`, el("div", { class: "muted", title: a.release ? `izdanje agenta ${a.release}` : null }, fmtVersion(a.version)),
        upg ? el("div", {}, el("span", { class: "upg", title: upgradeReason(a) }, "⚠ potrebna nadogradnja")) : null),
      el("td", { class: "addrs" }, addrs.join("\n")),
      el("td", { class: "addrs" }, a.gateway || "—"),
      el("td", { class: "addrs" }, a.remote_addr || "—"),
      el("td", {}, a.online && a.control_rtt_ms ? ms(a.control_rtt_ms) : "—"),
      el("td", {}, a.online ? "sada" : ago(a.last_seen)),
      el("td", {}, el("div", { class: "actions" },
        el("button", { disabled: dis, onclick: () => latency(a.agent_id) }, "Latencija"),
        el("button", {
          // Ping šalje konzola, pa radi i kada agent nije na vezi (računar može da radi).
          disabled: !a.remote_addr || !!pb,
          onclick: () => openPing(a.agent_id),
          title: pb ? busyLabel(pb) : "Konzola pinguje ovaj računar u zadatom trajanju (ili neprekidno); izveštaj je u Rezultatima",
        }, "Ping"),
        el("button", { disabled: dis, onclick: () => speedtest(a.agent_id) }, "Brzina"),
        el("button", { disabled: dis, onclick: () => lantest(a.agent_id), title: "Izaberite drugi računar i izmerite brzinu mreže između njih" }, "LAN test"),
        isAdmin() ? watchButton(a) : null,
        a.online || !isAdmin() ? null : el("button", { onclick: () => forget(a.agent_id) }, "Ukloni"),
      )),
    ));
  }
  applyColumns("agents");
}

// Brojevi strana za navigaciju: prva, poslednja i dve oko trenutne; null = "…".
function pageNumbers(pages, cur) {
  const out = [];
  for (let p = 1; p <= pages; p++) {
    if (p === 1 || p === pages || Math.abs(p - cur) <= 2) out.push(p);
    else if (out[out.length - 1] !== null) out.push(null);
  }
  return out;
}

// renderPager crta navigaciju po stranama u elementu pager; goTo(p) prikazuje stranu p.
function renderPager(pager, cur, pages, goTo) {
  pager.replaceChildren();
  pager.hidden = pages <= 1;
  if (pages <= 1) return;
  pager.append(el("button", { type: "button", disabled: cur === 1, onclick: () => goTo(cur - 1) }, "‹ Prethodna"));
  for (const p of pageNumbers(pages, cur)) {
    if (p === null) {
      pager.append(el("span", { class: "muted" }, "…"));
    } else {
      pager.append(el("button", {
        type: "button",
        class: p === cur ? "current" : "",
        "aria-current": p === cur ? "page" : null,
        onclick: () => goTo(p),
      }, String(p)));
    }
  }
  pager.append(el("button", { type: "button", disabled: cur === pages, onclick: () => goTo(cur + 1) }, "Sledeća ›"));
  pager.append(el("span", { class: "muted" }, `strana ${cur} od ${pages}`));
}

// Promena pretrage ili filtera vraća spisak na prvu stranu.
function filtersChanged() {
  agentPage = 1;
  renderAgents();
}

$("a-search").addEventListener("input", filtersChanged);
// Prazan izbor = sve lokacije (bez filtera).
$("a-loc-all").addEventListener("click", () => {
  selectedLocations = new Set();
  saveSelectedLocations();
  fillLocationSelect();
  filtersChanged();
});
// Zatvaranje menija klikom van njega ili tasterom Esc.
document.addEventListener("click", (ev) => {
  const d = $("a-locations");
  if (d.open && !d.contains(ev.target)) d.open = false;
});
document.addEventListener("keydown", (ev) => {
  if (ev.key === "Escape") $("a-locations").open = false;
});
$("a-online").addEventListener("change", () => { store("nm.online", $("a-online").checked ? "1" : ""); filtersChanged(); });
$("a-online").checked = stored("nm.online") === "1";

async function loadLocations() {
  try {
    const r = await api("GET", "/api/locations");
    locationNames = r.locations.map((l) => l.name);
    if (isAdmin() && document.activeElement !== $("loc-text")) $("loc-text").value = r.text;
  } catch (e) {
    setStatus(`Lokacije: ${e.message}`, "bad");
  }
}

$("loc-save").addEventListener("click", async () => {
  const msg = $("loc-msg");
  try {
    const r = await api("POST", "/api/locations", { text: $("loc-text").value });
    locationNames = r.locations.map((l) => l.name);
    $("loc-text").value = r.text;
    msg.className = "ok";
    msg.textContent = `Sačuvano: ${r.locations.length} lokacija.`;
    loadAgents();
  } catch (e) {
    msg.className = "bad";
    msg.textContent = e.message;
  }
});

function fillSelect(sel, items, firstLabel) {
  const cur = sel.value;
  sel.replaceChildren(el("option", { value: "" }, firstLabel));
  for (const a of items) {
    const where = a.location ? ` – ${a.location}` : "";
    sel.append(el("option", { value: a.agent_id }, `${a.hostname}${where} (${a.agent_id.slice(0, 6)})`));
  }
  sel.value = items.some((a) => a.agent_id === cur) ? cur : "";
}

async function loadAgents() {
  try {
    agents = await api("GET", "/api/agents");
    fillLocationSelect();
    renderAgents();
    fillSelect($("filter"), agents, "svi agenti");
  } catch (e) {
    setStatus(`Greška pri učitavanju agenata: ${e.message}`, "bad");
  }
}

function summary(r) {
  const d = r.data || {};
  const lat = r.kind === "latency" || r.kind === "ping" ? d : d.latency;
  return {
    lat: lat
      ? `${ms(lat.avg_ms)} (jitter ${ms(lat.jitter_ms)}, gubitak ${lat.loss_pct}%)` +
        (r.kind === "latency" || r.kind === "ping" ? ` → ${lat.target} [${lat.method || "tcp"}]` : "")
      : "—",
    down: mbps(d.download),
    up: mbps(d.upload),
  };
}

const kinds = { latency: "latencija", speedtest: "brzina → konzola", lan: "LAN", ping: "ping" };

// Detalji rezultata bez dugih nizova (ping od više sati ima hiljade vrednosti).
function trimDetail(r) {
  const data = r.data ? { ...r.data } : r.data;
  for (const k of ["samples_ms", "times_ms", "lost"]) {
    if (data && Array.isArray(data[k]) && data[k].length > 30) data[k] = `[${data[k].length} vrednosti – pogledajte izveštaj]`;
  }
  return JSON.stringify({ params: r.params, data, error: r.error }, null, 2);
}

function reportLinks(r) {
  if (!r.ok || (r.kind !== "ping" && r.kind !== "latency")) return null;
  const stop = (ev) => ev.stopPropagation();
  const base = `/api/results/${encodeURIComponent(r.id)}/report`;
  return el("div", { class: "rep-links" },
    el("a", { href: `${base}.html`, download: "", onclick: stop, title: "Izveštaj za čitanje i štampu (PDF)" }, "izveštaj"),
    " · ",
    el("a", { href: `${base}.csv`, download: "", onclick: stop, title: "Svi pingovi, za Excel" }, "CSV"));
}

const RESULTS_PAGE_SIZE = 10; // merenja po strani u istoriji
let resultsPage = 1;
let resultsSeq = 0; // odbacuje odgovore na zastarele zahteve (brzo kucanje u pretrazi)

async function loadResults() {
  const seq = ++resultsSeq;
  try {
    const qs = new URLSearchParams({ limit: RESULTS_PAGE_SIZE, offset: (resultsPage - 1) * RESULTS_PAGE_SIZE });
    const f = $("filter").value;
    if (f) qs.set("agent", f);
    const q = $("r-search").value.trim();
    if (q) qs.set("q", q);
    let total = 0;
    const list = await api("GET", `/api/results?${qs}`, undefined, (r) => {
      total = parseInt(r.headers.get("X-Total-Count") || "0", 10) || 0;
    });
    if (seq !== resultsSeq) return;
    const pages = Math.max(1, Math.ceil(total / RESULTS_PAGE_SIZE));
    if (resultsPage > pages) {
      // Strana više ne postoji (npr. posle nove pretrage): prikaži poslednju.
      resultsPage = pages;
      return loadResults();
    }
    const from = (resultsPage - 1) * RESULTS_PAGE_SIZE;
    $("r-count").textContent = total
      ? `prikazano ${from + 1}–${from + list.length} od ${total}`
      : (q || f ? "nema merenja za izabrani filter" : "");
    renderPager($("r-pager"), resultsPage, pages, (p) => { resultsPage = p; loadResults(); });
    queueMicrotask(() => applyColumns("results"));
    const tb = $("results").tBodies[0];
    tb.replaceChildren();
    if (!list.length && (q || f)) {
      tb.append(el("tr", {}, el("td", { colspan: 8, class: "empty" }, "Nijedno merenje ne odgovara pretrazi.")));
    }
    for (const r of list) {
      const s = summary(r);
      const peer = r.peer_hostname || name(r.peer_id);
      const who = r.kind === "lan" ? `${r.hostname || r.agent_id} ↔ ${peer}` : r.hostname || r.agent_id;
      // Ping i latencija: klik na red prikazuje izveštaj; ostalo: tehnički detalji.
      const hasReport = r.ok && (r.kind === "ping" || r.kind === "latency");
      const box = el("div", { class: "report" });
      const detail = el("tr", { class: hasReport ? "detail rep" : "detail", hidden: true },
        el("td", { colspan: 8 }, hasReport ? [box, el("details", { class: "raw" }, el("summary", {}, "tehnički detalji"), el("pre", {}, trimDetail(r)))] : trimDetail(r)));
      const toggle = () => {
        detail.hidden = !detail.hidden;
        if (detail.hidden) openReports.delete(r.id);
        else {
          openReports.add(r.id);
          if (hasReport && !box.childElementCount) loadReportInto(box, r.id);
        }
      };
      if (openReports.has(r.id)) setTimeout(toggle, 0);
      tb.append(
        el("tr", { class: "row", "data-id": r.id, onclick: toggle },
          el("td", {}, new Date(r.time).toLocaleString("sr-RS")),
          el("td", {}, who),
          el("td", {}, kinds[r.kind] || r.kind, reportLinks(r)),
          el("td", {}, r.ok ? s.lat : "—"),
          el("td", {}, r.ok ? s.down : "—"),
          el("td", {}, r.ok ? s.up : "—"),
          el("td", {}, r.user || "—"),
          el("td", { class: r.ok ? "ok" : "bad" }, r.ok ? "OK" : r.error),
        ),
        detail,
      );
    }
  } catch (e) {
    setStatus(`Greška pri učitavanju rezultata: ${e.message}`, "bad");
  }
}

// ---------- podesive kolone (širina i sakrivanje) ----------

// Ključevi kolona po redu (kao u zaglavlju tabele). locked = ne može se sakriti.
const COLUMNS = {
  agents: [
    { key: "dot", label: "", locked: true, fixed: true },
    { key: "host", label: "Računar", locked: true },
    { key: "loc", label: "Lokacija" },
    { key: "os", label: "OS i verzija agenta" },
    { key: "ip", label: "IP adrese" },
    { key: "gw", label: "Gateway" },
    { key: "from", label: "Povezan sa" },
    { key: "rtt", label: "RTT kontrolnog kanala" },
    { key: "seen", label: "Poslednji put viđen" },
    { key: "actions", label: "Akcije", locked: true },
  ],
  results: [
    { key: "time", label: "Vreme", locked: true },
    { key: "agent", label: "Agent", locked: true },
    { key: "kind", label: "Tip" },
    { key: "lat", label: "Latencija" },
    { key: "down", label: "Download" },
    { key: "up", label: "Upload" },
    { key: "user", label: "Pokrenuo" },
    { key: "status", label: "Status" },
  ],
};
const COL_MIN = 50; // najmanja širina kolone (px)

// Podešavanja se čuvaju samo u ovom pretraživaču: {w: {ključ: px}, hidden: [ključevi]}.
function colPrefs(table) {
  try {
    const p = JSON.parse(stored(`nm.cols.${table}`) || "{}");
    return { w: p.w || {}, hidden: p.hidden || [] };
  } catch {
    return { w: {}, hidden: [] };
  }
}
function saveColPrefs(table, p) {
  store(`nm.cols.${table}`, JSON.stringify(p));
}

// applyColumns primenjuje sakrivene kolone i širine. Bez sačuvanih širina
// tabela sama raspoređuje kolone; čim se neka kolona ručno podesi, tabela
// prelazi na fiksni raspored (ostale kolone zadržavaju prirodnu širinu).
function applyColumns(table) {
  const t = $(table);
  if (!t) return;
  const cols = COLUMNS[table];
  const p = colPrefs(table);
  // Sakrivanje: CSS pravila po rednom broju kolone.
  let st = document.getElementById(`cols-style-${table}`);
  if (!st) {
    st = el("style", { id: `cols-style-${table}` });
    document.head.append(st);
  }
  st.textContent = cols.map((c, i) => (p.hidden.includes(c.key)
    ? `#${table} > thead > tr > :nth-child(${i + 1}), #${table} > tbody > tr:not(.detail) > :nth-child(${i + 1}) { display: none; }`
    : "")).join("\n");
  // Širine.
  const ths = [...t.tHead.rows[0].cells];
  ths.forEach((th, i) => {
    th.dataset.col = cols[i].key;
    if (!cols[i].fixed && !th.querySelector(".col-grip")) addGrip(table, th);
  });
  t.classList.remove("cols-fixed");
  t.style.width = "";
  ths.forEach((th) => (th.style.width = ""));
  if (!Object.keys(p.w).length) return;
  // Prirodne širine (auto raspored), pa ručne preko njih.
  const natural = ths.map((th) => th.getBoundingClientRect().width);
  let total = 0;
  ths.forEach((th, i) => {
    if (p.hidden.includes(cols[i].key)) return;
    const w = Math.max(cols[i].fixed ? natural[i] : COL_MIN, p.w[cols[i].key] || natural[i]);
    th.style.width = `${Math.round(w)}px`;
    total += w;
  });
  t.style.width = `${Math.round(total)}px`;
  t.classList.add("cols-fixed");
}

function addGrip(table, th) {
  const grip = el("span", { class: "col-grip", title: "Prevucite da promenite širinu; dupli klik vraća automatsku širinu" });
  grip.addEventListener("pointerdown", (ev) => {
    ev.preventDefault();
    ev.stopPropagation();
    const t = $(table);
    const ths = [...t.tHead.rows[0].cells];
    const cols = COLUMNS[table];
    const p = colPrefs(table);
    // Zamrzni trenutne širine svih vidljivih kolona, pa menjaj samo ovu.
    ths.forEach((h, i) => {
      if (!p.hidden.includes(cols[i].key) && !cols[i].fixed) p.w[cols[i].key] = Math.round(h.getBoundingClientRect().width);
    });
    const key = th.dataset.col;
    const startX = ev.clientX, startW = p.w[key];
    grip.setPointerCapture(ev.pointerId);
    document.body.classList.add("col-resizing");
    const move = (e) => {
      p.w[key] = Math.max(COL_MIN, Math.round(startW + e.clientX - startX));
      th.style.width = `${p.w[key]}px`;
      let total = 0;
      ths.forEach((h) => { if (h.offsetParent !== null) total += h === th ? p.w[key] : h.getBoundingClientRect().width; });
      t.style.width = `${Math.round(total)}px`;
      t.classList.add("cols-fixed");
    };
    const up = () => {
      grip.removeEventListener("pointermove", move);
      grip.removeEventListener("pointerup", up);
      document.body.classList.remove("col-resizing");
      saveColPrefs(table, p);
      applyColumns(table);
    };
    grip.addEventListener("pointermove", move);
    grip.addEventListener("pointerup", up);
  });
  grip.addEventListener("dblclick", (ev) => {
    ev.stopPropagation();
    const p = colPrefs(table);
    delete p.w[th.dataset.col];
    saveColPrefs(table, p);
    applyColumns(table);
  });
  th.append(grip);
}

// Meni „Kolone”: sakrivanje kolona i vraćanje podrazumevanih širina.
function renderColsMenu(details) {
  const table = details.dataset.table;
  const menu = details.querySelector(".menu");
  const p = colPrefs(table);
  menu.replaceChildren(el("div", { class: "menu-actions" },
    el("button", { type: "button", onclick: () => { const q = colPrefs(table); q.w = {}; saveColPrefs(table, q); applyColumns(table); } }, "Podrazumevane širine"),
    el("button", { type: "button", onclick: () => { const q = colPrefs(table); q.hidden = []; saveColPrefs(table, q); applyColumns(table); renderColsMenu(details); } }, "Prikaži sve")));
  for (const c of COLUMNS[table]) {
    if (c.locked) continue;
    const cb = el("input", { type: "checkbox" });
    cb.checked = !p.hidden.includes(c.key);
    cb.addEventListener("change", () => {
      const q = colPrefs(table);
      q.hidden = q.hidden.filter((k) => k !== c.key);
      if (!cb.checked) q.hidden.push(c.key);
      saveColPrefs(table, q);
      applyColumns(table);
    });
    menu.append(el("label", {}, cb, c.label));
  }
  menu.append(el("div", { class: "muted cols-hint" }, "Širina: prevucite ivicu zaglavlja kolone; dupli klik na ivicu vraća automatsku širinu. Podešavanja važe u ovom pretraživaču."));
}
for (const d of document.querySelectorAll("details.cols-menu")) {
  d.addEventListener("toggle", () => { if (d.open) renderColsMenu(d); });
}
document.addEventListener("click", (ev) => {
  for (const d of document.querySelectorAll("details.cols-menu[open]")) if (!d.contains(ev.target)) d.open = false;
});
applyColumns("agents");
applyColumns("results");

// ---------- izveštaj u tabeli Rezultati ----------

const openReports = new Set(); // otvoreni redovi (ostaju otvoreni posle osvežavanja)
const myPings = new Set(); // pingovi pokrenuti iz ovog prozora (izveštaj se sam otvori)

async function loadReportInto(box, id) {
  box.replaceChildren(el("p", { class: "muted" }, "Učitavam izveštaj…"));
  try {
    const rep = await api("GET", `/api/results/${encodeURIComponent(id)}/report.json`);
    renderReport(box, rep);
  } catch (e) {
    box.replaceChildren(el("p", { class: "bad" }, `Izveštaj: ${e.message}`));
  }
}

function renderReport(box, rep) {
  try { rep.request = typeof rep.request === "string" ? JSON.parse(rep.request) : rep.request; } catch { rep.request = {}; }
  const s = latencyStats(rep);
  const v = latencyVerdict(s);
  const t0 = new Date(rep.started), t1 = new Date(rep.finished);
  const fmtT = (d) => d.toLocaleTimeString("sr-RS");
  const head = el("div", { class: "rep-head" },
    el("strong", {}, `${rep.kind === "ping" ? "Ping" : "Latencija"}: ${rep.title}`),
    el("div", { class: "muted" }, `${t0.toLocaleDateString("sr-RS")} ${fmtT(t0)} – ${fmtT(t1)} · ${fmtDuration((t1 - t0) / 1000)} · ` +
      `svakih ${fmtSec(rep.interval_ms)} · ${(rep.method || "").toUpperCase()} · pokrenuo ${rep.user || "?"}`),
  );
  const notes = [];
  if (rep.stop_note) notes.push(rep.stop_note);
  if (!rep.exact) notes.push("Vremena paketa su procenjena iz razmaka (starija verzija agenta).");
  const lossTxt = s.sent ? `${((100 * s.lost) / s.sent).toFixed(s.lost ? 1 : 0)}% (${s.lost}/${s.sent})` : "—";
  const tiles = el("div", { class: "rep-tiles" },
    tile("prosek", s.n ? `${fmtMs(s.avg)} ms` : "—"),
    tile("min / max", s.n ? `${fmtMs(s.min)} / ${fmtMs(s.max)} ms` : "—"),
    tile("jitter", s.n ? `${fmtMs(s.jitter)} ms` : "—"),
    tile("95% ispod", s.n ? `${fmtMs(s.p95)} ms` : "—", "", "95% odgovora je bilo brže od ove vrednosti"),
    tile("gubitak", s.lost ? `⚠ ${lossTxt}` : lossTxt, s.lost ? "bad" : ""),
    tile("prekida", rep.lost_seq ? String(s.outages || 0) : "—", s.outages ? "bad" : ""),
    tile("najduži prekid", s.longest ? `${s.longest.count} paketa (~${fmtDuration(Math.round((s.longest.count * rep.interval_ms) / 1000))})` : "—", s.longest ? "bad" : ""),
  );
  const svg = svgEl("svg", { viewBox: "0 0 280 130", class: "lat-chart" });
  const hover = el("div", { class: "muted rep-hover" });
  const legend = el("div", { class: "muted lat-legend" });
  drawLatencyChart(rep, s, { svg, hover, legend });
  const advice = el("ul", { class: "rep-advice" }, latencyAdvice(rep, s).map((l) => el("li", {}, l)));
  const outs = rep.outages || [];
  const outBlock = outs.length
    ? el("div", {}, el("h4", {}, "Prekidi (uzastopno izgubljeni paketi)"),
      el("table", { class: "rep-out" },
        el("tr", {}, el("th", {}, "Od"), el("th", {}, "Do"), el("th", {}, "Paketa"), el("th", {}, "Trajanje (približno)")),
        outs.map((o) => el("tr", {},
          el("td", {}, o.from ? fmtT(new Date(o.from)) : `#${o.from_seq}`),
          el("td", {}, o.to ? fmtT(new Date(o.to)) : `#${o.to_seq}`),
          el("td", {}, String(o.count)),
          el("td", {}, fmtDuration(Math.round((o.count * rep.interval_ms) / 1000)))))),
      rep.outages_more ? el("p", { class: "muted" }, `… i još ${rep.outages_more} kraćih prekida (svi su u CSV izveštaju).`) : null)
    : null;
  const base = `/api/results/${encodeURIComponent(rep.id)}/report`;
  box.replaceChildren(...[
    head,
    notes.length ? el("p", { class: "busy" }, notes.join(" ")) : null,
    el("p", { class: `rep-verdict ${v.cls}` }, `${v.icon} ${v.label}`, el("span", { class: "muted" }, ` – ${v.why}`)),
    tiles,
    el("div", { class: "rep-body" },
      el("div", {}, svg, hover, legend),
      el("div", {}, advice)),
    outBlock,
    el("div", { class: "actions rep-actions" },
      el("a", { class: "btnlink", href: `${base}.html`, download: "" }, "⤓ Izveštaj (HTML, za štampu/PDF)"),
      el("a", { class: "btnlink", href: `${base}.csv`, download: "" }, "⤓ Svi pingovi (CSV, Excel)")),
  ].filter(Boolean));
  box.addEventListener("click", (ev) => ev.stopPropagation()); // klik u izveštaju ne zatvara red
}

// ---------- merenje uživo (skazaljka) ----------

const SVGNS = "http://www.w3.org/2000/svg";
const svgEl = (tag, attrs = {}) => {
  const e = document.createElementNS(SVGNS, tag);
  for (const [k, v] of Object.entries(attrs)) e.setAttribute(k, v);
  return e;
};
const SCALES = [100, 200, 500, 1000, 2000, 5000, 10000, 20000, 50000, 100000]; // Mb/s
let liveTests = [];
let liveSelected = null; // ID testa koji je korisnik izabrao
let liveTimer = null;
let liveScale = { id: null, max: 100 };

const fmtMbps = (v) => (v >= 100 ? v.toFixed(0) : v.toFixed(1));
const isActive = (t) => t.phase !== "done" && t.phase !== "error";

function scaleFor(t) {
  let peak = Math.max(t.mbps || 0, t.download_mbps || 0, t.upload_mbps || 0, ...t.points.map((p) => p.mbps));
  const max = SCALES.find((s) => s >= peak * 1.05) || SCALES[SCALES.length - 1];
  // Skala tokom jednog testa samo raste (skazaljka ne „skače” nazad).
  if (liveScale.id !== t.id) liveScale = { id: t.id, max };
  else liveScale.max = Math.max(liveScale.max, max);
  return liveScale.max;
}

function drawTicks(max) {
  const g = $("g-ticks");
  if (g.dataset.max === String(max)) return;
  g.dataset.max = String(max);
  g.replaceChildren();
  for (let i = 0; i <= 4; i++) {
    const f = i / 4;
    const a = Math.PI - f * Math.PI;
    const cx = (r) => 120 + r * Math.cos(a);
    const cy = (r) => 120 - r * Math.sin(a);
    g.append(svgEl("line", { x1: cx(84), y1: cy(84), x2: cx(79), y2: cy(79) }));
    // Krajnje oznake idu ispod krajeva luka, da ih skazaljka ne prekrije.
    const end = i === 0 || i === 4;
    const t = end
      ? svgEl("text", { x: i === 0 ? 24 : 216, y: 140, "text-anchor": "middle" })
      : svgEl("text", { x: cx(68), y: cy(68) + 3, "text-anchor": "middle" });
    t.textContent = String((max * f) >= 1000 ? `${(max * f) / 1000}G` : max * f);
    g.append(t);
  }
}

function setGauge(value, max, upload) {
  const f = Math.max(0, Math.min(1, value / max));
  $("g-needle").style.transform = `rotate(${-90 + 180 * f}deg)`;
  // 101: zaobljeni kraj nulte crtice se inače ponavlja i na kraju luka.
  $("g-fill").style.strokeDasharray = `${(f * 100).toFixed(2)} 101`;
  $("g-fill").style.opacity = f > 0 ? "1" : "0";
  $("g-fill").classList.toggle("ul", upload);
  $("g-track").classList.toggle("ul", upload);
  drawTicks(max);
}

function drawChart(t, max) {
  const svg = $("live-chart");
  svg.replaceChildren();
  const W = 280, H = 74;
  const dirs = t.direction === "both" ? 2 : 1;
  const expected = 1.5 + dirs * t.seconds * 1.25; // latencija + zagrevanje + merenje
  const t0 = 0;
  const t1 = Math.max(expected, t.points.length ? t.points[t.points.length - 1].t : 0);
  const x = (v) => ((v - t0) / (t1 - t0)) * W;
  const y = (v) => H - 2 - (Math.min(v, max) / max) * (H - 6);
  svg.append(svgEl("line", { class: "grid", x1: 0, x2: W, y1: y(max / 2), y2: y(max / 2) }));
  for (const phase of ["download", "upload"]) {
    const pts = t.points.filter((p) => p.phase === phase);
    if (pts.length < 2) continue;
    svg.append(svgEl("polyline", {
      class: phase === "download" ? "dl" : "ul",
      points: pts.map((p) => `${x(p.t).toFixed(1)},${y(p.mbps).toFixed(1)}`).join(" "),
    }));
  }
  const cross = svgEl("line", { class: "cross", y1: 0, y2: H, x1: -10, x2: -10 });
  svg.append(cross);
  svg.onmousemove = (ev) => {
    if (!t.points.length) return;
    const r = svg.getBoundingClientRect();
    const tv = t0 + ((ev.clientX - r.left) / r.width) * (t1 - t0);
    let best = t.points[0];
    for (const p of t.points) if (Math.abs(p.t - tv) < Math.abs(best.t - tv)) best = p;
    cross.setAttribute("x1", x(best.t));
    cross.setAttribute("x2", x(best.t));
    $("live-hover").textContent = `${best.t.toFixed(1)} s · ${flowLabels(t)[best.phase]}: ${fmtMbps(best.mbps)} Mb/s`;
  };
  svg.onmouseleave = () => {
    cross.setAttribute("x1", -10);
    cross.setAttribute("x2", -10);
    $("live-hover").textContent = "";
  };
}

function tile(k, v, cls = "", title = "") {
  return el("div", { class: "tile", title: title || null }, el("div", { class: "k" }, k), el("div", { class: `v ${cls}` }, v));
}

// Smer podataka sa nazivima računara: download = drugi računar (ili konzola)
// šalje računaru koji meri, upload = obrnuto.
function flowLabels(t) {
  const other = t.kind === "lan" ? t.peer_host || "drugi računar" : "konzola";
  return { download: `${other} → ${t.host}`, upload: `${t.host} → ${other}` };
}

// Ponavlja test sa istim parametrima.
function repeatTest(t) {
  if (t.kind === "lan") {
    run(t.agent_id, "LAN test", () => api("POST", "/api/lan-test", t.request));
  } else {
    run(t.agent_id, "Test brzine", () => api("POST", `/api/agents/${encodeURIComponent(t.agent_id)}/speedtest`, t.request));
  }
}

// ---------- merenje latencije uživo ----------

const fmtMs = (v) => (v >= 100 ? v.toFixed(0) : v >= 10 ? v.toFixed(1) : v.toFixed(2));

// niceCeil zaokružuje naviše na 1, 2 ili 5 × 10^n (granica ose grafikona).
function niceCeil(v) {
  if (v <= 0) return 1;
  const p = 10 ** Math.floor(Math.log10(v));
  return [1, 2, 5, 10].map((m) => m * p).find((x) => x >= v);
}

function latencyStats(t) {
  // Statistiku računa konzola (kod dugog pinga grafikon dobija samo sažetak).
  const st = t.stats;
  if (st) {
    return {
      n: st.received, sent: st.sent, lost: st.lost, avg: st.avg_ms, min: st.min_ms, max: st.max_ms,
      jitter: st.jitter_ms, median: st.median_ms, p95: st.p95_ms, last: st.last_ms, lastLost: st.last_lost,
      outages: st.outages, longest: st.longest,
    };
  }
  const probes = t.probes || [];
  const rtts = probes.filter((p) => !p.lost).map((p) => p.rtt_ms);
  const sent = t.phase === "done" ? t.sent : probes.length;
  const lost = t.phase === "done" ? t.sent - t.received : probes.filter((p) => p.lost).length;
  if (!rtts.length) return { sent, lost, n: 0 };
  const sorted = [...rtts].sort((a, b) => a - b);
  const q = (f) => sorted[Math.min(sorted.length - 1, Math.floor(f * sorted.length))];
  let jitter = 0;
  for (let i = 1; i < rtts.length; i++) jitter += Math.abs(rtts[i] - rtts[i - 1]);
  if (rtts.length > 1) jitter /= rtts.length - 1;
  const done = t.phase === "done";
  return {
    n: rtts.length, sent, lost,
    avg: done ? t.latency_ms : rtts.reduce((a, b) => a + b, 0) / rtts.length,
    min: done ? t.min_ms : sorted[0],
    max: done ? t.max_ms : sorted[sorted.length - 1],
    jitter: done ? t.jitter_ms : jitter,
    median: q(0.5),
    p95: q(0.95),
    last: rtts[rtts.length - 1],
  };
}

// Ocena za rad u realnom vremenu (glas, video, udaljeni rad). Granice su
// uobičajene preporuke (ITU-T G.114 / Cisco: latencija < 150 ms, jitter < 30 ms,
// gubitak < 1%), sa strožim pragovima za „odlično” u lokalnoj mreži.
function latencyVerdict(s) {
  const loss = s.sent ? (100 * s.lost) / s.sent : 0;
  if (!s.n) return { cls: "bad", icon: "✕", label: "Nema odgovora", why: "Nijedan paket nije dobio odgovor." };
  if (s.avg < 20 && s.jitter < 5 && loss === 0) return { cls: "ok", icon: "●", label: "Odlično", why: "niska latencija, stabilna veza, bez gubitaka" };
  if (s.avg < 50 && s.jitter < 15 && loss < 1) {
    return { cls: "ok", icon: "●", label: "Dobro", why: s.jitter >= 5 ? "latencija niska, ali veza malo varira (jitter)" : "uobičajene vrednosti" };
  }
  if (s.avg < 150 && s.jitter < 30 && loss < 3) return { cls: "busy", icon: "▲", label: "Prihvatljivo", why: "rad je moguć, ali glas i video mogu povremeno da trpe" };
  return { cls: "bad", icon: "✕", label: "Loše", why: "visoka latencija, jitter ili gubitak paketa" };
}

function latencyAdvice(t, s) {
  const out = [];
  const req = t.request || {};
  const reqTarget = (req.target || "").trim().toLowerCase();
  const kind = t.device ? "uredjaj" : t.from_console ? "racunar" : reqTarget === "" ? "konzola" : reqTarget === "gateway" ? "gateway" : "drugo";
  const agent = agents.find((a) => a.agent_id === t.agent_id) || {};
  const wifi = (agent.interfaces || []).some((i) => /wi-?fi|wlan|wireless|bežič/i.test(i.name));
  const method = t.method === "icmp" ? "ICMP ping" : t.method === "tcp" ? "TCP konekcija" : "merenje";
  if (!s.n && kind === "racunar") {
    out.push("Računar nije odgovorio ni na jedan ping: isključen je, nije u mreži, ili Windows firewall na njemu blokira ping (ICMP). " +
      "Nova verzija agenta pri instalaciji dozvoljava ping na domenskoj mreži; za računare sa starijim agentom ping se može dozvoliti i preko GPO-a " +
      "(pravilo „File and Printer Sharing (Echo Request – ICMPv4-In)”).");
    return out;
  }
  if (!s.n && kind === "uredjaj") {
    out.push("Uređaj nije odgovorio ni na jedan ping: isključen je, veza do njega (ili do cele lokacije) je prekinuta, " +
      "ili ACL na uređaju ne dozvoljava ping sa konzole.");
    return out;
  }
  if (!s.n) {
    out.push("Odredište nije odgovorilo ni na jedan paket: računar je isključen ili nedostupan, adresa nije ispravna, ili firewall blokira " +
      (t.method === "icmp" ? "ping (ICMP). Probajte TCP metodu sa portom, npr. server:445." : "taj port."));
    return out;
  }
  const expect = {
    gateway: "Do gateway-a (prvi ruter u mreži) kablom se očekuje ispod 1 ms, preko Wi-Fi-ja 1–10 ms.",
    konzola: "Do konzole sa iste lokacije kablom očekuje se ispod 2 ms; sa druge lokacije (WAN) obično 5–30 ms, a preko VPN-a i više.",
    drugo: "U lokalnoj mreži očekuje se ispod 2 ms, između lokacija obično 5–30 ms.",
    racunar: "Od konzole do računara na istoj lokaciji kablom očekuje se ispod 2 ms; sa druge lokacije (WAN) obično 5–30 ms, a preko VPN-a i više.",
    uredjaj: "Do mrežnog uređaja na istoj lokaciji očekuje se ispod 2 ms, a na drugoj lokaciji (MPLS) obično 3–20 ms. " +
      "Switch-evi i ruteri na ping odgovaraju sa niskim prioritetom, pa pojedinačni skok ili izgubljen paket ne znači da kasni i saobraćaj koji prolazi kroz njih.",
  }[kind];
  out.push(`Latencija ${fmtMs(s.avg)} ms je prosečno vreme od slanja paketa do odgovora (${method}). ${expect}`);
  let j = `Jitter ${fmtMs(s.jitter)} ms je prosečna razlika između dva uzastopna merenja i pokazuje koliko je veza stabilna. `;
  if (s.jitter < 1 || s.jitter < 0.2 * s.avg) j += "Veza je stabilna.";
  else if (s.jitter < 10) j += "Postoji umerena nestabilnost: za glas i video je još u redu (preporuka ispod 30 ms), ali obično ukazuje na Wi-Fi ili opterećenu vezu.";
  else j += "Veza je nestabilna – glas i video mogu da seckaju.";
  out.push(j);
  if (s.n >= 5 && s.max > 3 * s.median && s.max - s.median > 10) {
    out.push(`Povremeni skokovi: najsporiji odgovor je ${fmtMs(s.max)} ms, a uobičajeni (medijana) ${fmtMs(s.median)} ms. ` +
      "Tipični uzroci su Wi-Fi smetnje, prelazak između pristupnih tačaka ili trenutno zagušenje.");
  }
  if (t.kind === "ping" && s.outages && s.longest) {
    out.push(`Bilo je ${s.outages} ${s.outages === 1 ? "prekid" : "prekida"} (uzastopno izgubljenih paketa); najduži je ${s.longest.count} ` +
      `${s.longest.count === 1 ? "paket" : "paketa"}, oko ${fmtDuration(Math.round((s.longest.count * t.interval_ms) / 1000))}. ` +
      "Pojedinačni izgubljeni paketi su obično smetnje, a duži prekidi znače da je veza stvarno pucala — vreme svakog prekida je u izveštaju.");
  }
  if (s.lost === 0) out.push("Nijedan paket nije izgubljen.");
  else {
    let l = `Izgubljeno je ${s.lost} od ${s.sent} paketa (${((100 * s.lost) / s.sent).toFixed(1)}%). Gubitak iznad 1% primetno smeta glasu i videu.`;
    if (kind === "gateway") l += " Gubitak već do gateway-a znači problem u lokalnoj mreži (Wi-Fi signal, kabl, svič).";
    if (t.method === "icmp") l += " Neki uređaji namerno ograničavaju odgovore na ping, pa mali gubitak prema ruteru ne mora da znači problem.";
    out.push(l);
  }
  if (wifi) {
    out.push("Računar ima Wi-Fi vezu, koja obično dodaje nekoliko ms i jitter. Za poređenje izmerite latenciju do gateway-a (Parametri merenja → Cilj za latenciju: gateway): ako je i tamo jitter visok, uzrok je Wi-Fi, a ne ostatak mreže.");
  }
  if (t.method === "tcp") out.push("TCP merenje obuhvata i otvaranje konekcije na odredištu, pa je obično malo veće od ICMP pinga.");
  return out;
}

function drawLatencyChart(t, s, els) {
  const { svg, hover, legend } = els || { svg: $("lat-chart"), hover: $("lat-hover"), legend: $("lat-legend") };
  svg.replaceChildren();
  const W = 280, H = 130, L = 30, R = 6, T = 16, B = 16;
  const probes = t.probes || [];
  const count = Math.max(t.count || 0, t.sent || 0, probes.length, 2);
  const yMax = niceCeil((s.max || 1) * 1.15);
  const x = (seq) => L + ((seq - 1) / (count - 1)) * (W - L - R);
  const y = (v) => T + (1 - Math.min(v, yMax) / yMax) * (H - T - B);
  for (const v of [0, yMax / 2, yMax]) {
    svg.append(svgEl("line", { class: "grid", x1: L, x2: W - R, y1: y(v), y2: y(v) }));
    const tx = svgEl("text", { class: "axis", x: L - 4, y: y(v) + 3, "text-anchor": "end" });
    tx.textContent = `${+v.toFixed(2)}`;
    svg.append(tx);
  }
  const ax = svgEl("text", { class: "axis", x: 2, y: 8, "text-anchor": "start" });
  ax.textContent = "ms";
  svg.append(ax);
  for (const seq of [1, count]) {
    const tx = svgEl("text", { class: "axis", x: x(seq), y: H - 3, "text-anchor": seq === 1 ? "start" : "end" });
    tx.textContent = `#${seq}`;
    svg.append(tx);
  }
  if (s.n) svg.append(svgEl("line", { class: "avg", x1: L, x2: W - R, y1: y(s.avg), y2: y(s.avg) }));
  const ok = probes.filter((p) => !p.lost);
  if (ok.length > 1) {
    svg.append(svgEl("polyline", { class: "rtt", points: ok.map((p) => `${x(p.seq).toFixed(1)},${y(p.rtt_ms).toFixed(1)}`).join(" ") }));
  }
  const r = count > 50 ? 1.6 : 2.4;
  if (probes.length <= 150) for (const p of ok) svg.append(svgEl("circle", { class: "dot", cx: x(p.seq), cy: y(p.rtt_ms), r }));
  const showLost = t.phase !== "done" || t.lost_seq;
  if (showLost) {
    for (const p of probes.filter((q) => q.lost || q.lost_n)) {
      const cx = x(p.seq), cy = T + 4;
      svg.append(svgEl("path", { class: "lost", d: `M${cx - 3},${cy - 3}L${cx + 3},${cy + 3}M${cx + 3},${cy - 3}L${cx - 3},${cy + 3}` }));
    }
  }
  const cross = svgEl("line", { class: "cross", y1: T, y2: H - B, x1: -10, x2: -10 });
  const hl = svgEl("circle", { class: "hl", r: 4, cx: -10, cy: -10 });
  svg.append(cross, hl);
  svg.onmousemove = (ev) => {
    if (!probes.length) return;
    const rect = svg.getBoundingClientRect();
    const sx = ((ev.clientX - rect.left) / rect.width) * W;
    let best = probes[0];
    for (const p of probes) if (Math.abs(x(p.seq) - sx) < Math.abs(x(best.seq) - sx)) best = p;
    cross.setAttribute("x1", x(best.seq));
    cross.setAttribute("x2", x(best.seq));
    hl.setAttribute("cx", best.lost ? -10 : x(best.seq));
    hl.setAttribute("cy", best.lost ? -10 : y(best.rtt_ms));
    if (best.seq_end) {
      // Dug ping: tačka je sažetak grupe paketa.
      hover.textContent = `paketi #${best.seq}–#${best.seq_end}: ` +
        (best.lost ? "svi izgubljeni" : `prosek ${fmtMs(best.rtt_ms)} ms, najviše ${fmtMs(best.max_ms)} ms`) +
        (best.lost_n && !best.lost ? `, izgubljeno ${best.lost_n}` : "");
    } else {
      hover.textContent = best.lost
        ? `paket #${best.seq}: izgubljen (nema odgovora)`
        : `paket #${best.seq}: ${fmtMs(best.rtt_ms)} ms` + (s.n ? ` (prosek ${fmtMs(s.avg)} ms)` : "");
    }
  };
  svg.onmouseleave = () => {
    cross.setAttribute("x1", -10);
    cross.setAttribute("x2", -10);
    hl.setAttribute("cx", -10);
    hover.textContent = "";
  };
  legend.replaceChildren(
    el("span", {}, el("i", { class: "sw" }), "vreme odgovora"),
    el("span", {}, el("i", { class: "sw avg" }), "prosek"),
  );
  if (showLost && probes.some((p) => p.lost || p.lost_n)) legend.append(el("span", {}, el("b", { class: "x" }, "✕"), "izgubljen paket"));
}

function renderLatency(t) {
  const s = latencyStats(t);
  const active = isActive(t);
  const ping = t.kind === "ping";
  $("lat-title").textContent = t.from_console ? `Ping: konzola → ${t.host} (${t.target})` : `${ping ? "Ping" : "Latencija"}: ${t.host} → ${t.target}`;
  const method = t.method === "icmp" ? "ICMP" : t.method === "tcp" ? "TCP" : "";
  $("lat-sub").textContent = `${new Date(t.started).toLocaleTimeString("sr-RS")}` + (t.user ? ` · pokrenuo ${t.user}` : "") +
    (method ? ` · ${method}` : "") +
    (ping ? ` · ${t.continuous ? "neprekidno" : fmtDuration(t.duration_sec)}, svakih ${fmtSec(t.interval_ms)}` : ` · ${t.count} merenja`);
  $("lat-num").textContent = s.n ? fmtMs(s.avg) : active ? "…" : "—";
  const elapsed = Math.round((Date.now() - new Date(t.started)) / 1000);
  let cap = "prosečna latencija";
  if (active) {
    // Neprekidni ping nema unapred poznat broj paketa.
    const of = t.continuous ? ` · ${s.sent} paketa` : ` · ${s.sent}/${t.count}`;
    if (s.sent && s.lastLost) cap = `poslednji paket izgubljen${of}`;
    else if (s.n) cap = `poslednje: ${fmtMs(s.last)} ms${of}`;
    else if (s.sent) cap = `bez odgovora${of}`;
    else cap = `merenje u toku (${elapsed} s)`;
  }
  $("lat-caption").textContent = cap;
  if (active && ping && t.continuous) {
    // Neprekidni ping: nema procenta, samo koliko traje (do zaustavljanja, najduže 24 h).
    const p = $("lat-progress");
    setProgress("lat-progress", 100, t.stopping ? "zaustavljanje…" : `neprekidno · traje ${fmtDuration(elapsed)}`);
    p.classList.add("running");
  } else if (active && ping) {
    // Ping: napredak po vremenu (paketi koji ne dobiju odgovor čekaju do 2 s).
    const total = t.duration_sec;
    const pct = Math.min(99, (100 * elapsed) / total);
    setProgress("lat-progress", t.stopping ? pct : pct, t.stopping ? "zaustavljanje…" :
      `${pct.toFixed(0)}% · još ${fmtDuration(Math.max(0, total - elapsed))} · ${s.sent}/${t.count} paketa`);
  } else if (active) {
    const pct = t.count ? (100 * s.sent) / t.count : 0;
    if (s.sent || t.live) setProgress("lat-progress", pct, s.sent ? `${pct.toFixed(0)}% · ${s.sent}/${t.count} paketa` : "počinje…");
    else setProgress("lat-progress", Math.min(95, (100 * elapsed) / (t.count * 0.25 + 1)), `u toku ${elapsed} s`); // stariji agent
  } else if (t.phase === "done") {
    setProgress("lat-progress", 100, (t.stopped ? "■ Zaustavljeno " : "✓ Merenje završeno ") + new Date(t.finished).toLocaleTimeString("sr-RS") +
      (t.continuous ? ` · trajalo ${fmtDuration((new Date(t.finished) - new Date(t.started)) / 1000)}` : ""),
      t.stopped ? "" : "done");
  } else {
    setProgress("lat-progress", 100, "✕ Merenje prekinuto", "error");
  }

  const verdict = $("lat-verdict");
  verdict.replaceChildren();
  if (t.phase === "done") {
    const v = latencyVerdict(s);
    verdict.append(el("span", { class: v.cls }, `${v.icon} ${v.label}`), el("span", { class: "why" }, v.why));
  } else if (t.phase === "error") {
    verdict.append(el("span", { class: "bad" }, `✕ Greška: ${t.error}`));
  }

  const tiles = $("lat-tiles");
  tiles.replaceChildren();
  if (s.n) {
    const lossTxt = `${((100 * s.lost) / s.sent).toFixed(s.lost ? 1 : 0)}% (${s.lost}/${s.sent})`;
    tiles.append(
      tile("prosek", `${fmtMs(s.avg)} ms`),
      tile("min", `${fmtMs(s.min)} ms`),
      tile("max", `${fmtMs(s.max)} ms`),
      tile("jitter", `${fmtMs(s.jitter)} ms`),
      tile(s.n >= 20 ? "95% ispod" : "medijana", `${fmtMs(s.n >= 20 ? s.p95 : s.median)} ms`, "",
        s.n >= 20 ? "95% odgovora je bilo brže od ove vrednosti" : "srednja vrednost po veličini"),
      tile("gubitak", s.lost ? `⚠ ${lossTxt}` : lossTxt, s.lost ? "bad" : ""),
    );
    if (ping && t.lost_seq) {
      const lg = s.longest;
      tiles.append(
        tile("prekida", String(s.outages || 0), s.outages ? "bad" : "", "Prekid = jedan ili više uzastopnih izgubljenih paketa"),
        tile("najduži prekid", lg ? `${lg.count} paketa (~${fmtDuration(Math.round((lg.count * t.interval_ms) / 1000))})` : "—",
          lg ? "bad" : "", lg ? `paketi #${lg.from_seq}–#${lg.to_seq}` : ""),
      );
    }
  }
  // Neprekidni ping: osa grafikona ide do broja poslatih paketa (a ne do
  // najvećeg mogućeg broja za 24 h), pa se grafikon širi kako ping traje.
  drawLatencyChart(t.continuous ? { ...t, count: Math.max(s.sent, 2) } : t, s);
  const ex = $("lat-explain");
  ex.replaceChildren();
  if (t.phase === "done") for (const line of latencyAdvice(t, s)) ex.append(el("li", {}, line));

  let note = "";
  if (t.phase === "done" && !t.lost_seq && s.lost) {
    note = "Starija verzija agenta ne javlja koji su tačno paketi izgubljeni, pa nisu označeni na grafikonu.";
  } else if (active && !t.live && elapsed > 6) {
    // Paket bez odgovora čeka do 2 s, pa i novi agent ume da ćuti nekoliko sekundi.
    note = "Agent ne šalje međurezultate (starija verzija agenta): grafikon se prikazuje kada se merenje završi.";
  }
  $("lat-note").textContent = note;

  const btn = $("lat-repeat");
  const online = (agents.find((a) => a.agent_id === t.agent_id) || {}).online;
  btn.disabled = active || !online || busy.has(t.agent_id) || remoteBusy().has(t.agent_id) || !t.request;
  btn.textContent = ping ? "Ponovi ping" : "Ponovi merenje";
  btn.onclick = () => ping ? startPing(pingTarget(t), t.request)
    : run(t.agent_id, "Merenje latencije", () => api("POST", `/api/agents/${encodeURIComponent(t.agent_id)}/latency`, t.request));

  // Ping: zaustavljanje (ko ga je pokrenuo ili administrator) i izveštaj.
  const stop = $("lat-stop");
  stop.hidden = !(ping && active && t.can_stop && (isMine(t) || isAdmin()));
  stop.disabled = !!t.stopping;
  stop.textContent = t.stopping ? "Zaustavljanje…" : "Zaustavi ping";
  stop.classList.toggle("primary", !!t.continuous);
  stop.onclick = async () => {
    try {
      await api("POST", `/api/live/${encodeURIComponent(t.id)}/stop`, {});
      kickLive();
    } catch (e) {
      setStatus(`Zaustavljanje: ${e.message}`, "bad");
    }
  };
  const canReport = t.phase === "done" && t.result_id && s.sent > 0;
  for (const [id, ext] of [["lat-report-html", "html"], ["lat-report-csv", "csv"]]) {
    $(id).hidden = !canReport;
    if (canReport) $(id).href = `/api/results/${encodeURIComponent(t.result_id)}/report.${ext}`;
  }
}

const fmtSec = (ms) => `${(ms / 1000).toLocaleString("sr-RS")} s`;

function fmtDuration(sec) {
  sec = Math.round(sec);
  const h = Math.floor(sec / 3600), m = Math.floor((sec % 3600) / 60), s2 = sec % 60;
  if (h) return m ? `${h} h ${m} min` : `${h} h`;
  if (m) return s2 ? `${m} min ${s2} s` : `${m} min`;
  return `${s2} s`;
}

// setProgress prikazuje traku napretka (pct 0–100) sa natpisom.
function setProgress(id, pct, label, state) {
  const p = $(id);
  p.className = `lv-progress ${state || ""}`;
  p.querySelector(".bar i").style.width = `${Math.max(0, Math.min(100, pct)).toFixed(1)}%`;
  p.querySelector(".pct").textContent = label;
}

// Procena napretka testa brzine: latencija (~1 s), pa za svaki smer
// zagrevanje + trajanje. Unutar smera se računa od prve tačke tog smera.
function speedProgress(t) {
  const dirs = t.direction === "both" ? ["download", "upload"] : [t.direction];
  const seg = t.seconds + Math.min(t.seconds * 0.2, 1);
  const total = 1 + seg * dirs.length;
  const elapsed = (Date.now() - new Date(t.started)) / 1000;
  let done = Math.min(1, elapsed);
  const idx = dirs.indexOf(t.phase);
  if (idx >= 0) {
    const first = t.points.find((p) => p.phase === t.phase);
    const inPhase = first ? Math.max(0, (Date.now() - new Date(t.started)) / 1000 - first.t + 0.5) : 0;
    done = 1 + idx * seg + Math.min(seg, inPhase);
  } else if (!t.live && elapsed > 1) {
    done = elapsed; // bez međurezultata: samo po vremenu
  }
  return { pct: Math.min(99, (100 * done) / total), left: Math.max(0, Math.ceil(total - done)) };
}

// Merenja drugih korisnika se u panelu prikazuju samo ako je to uključeno.
let liveShowOthers = stored("nm.liveOthers") === "1";
$("live-others").checked = liveShowOthers;
$("live-others").addEventListener("change", () => {
  liveShowOthers = $("live-others").checked;
  store("nm.liveOthers", liveShowOthers ? "1" : "");
  liveSelected = null;
  renderLive();
});
const isMine = (t) => t.user === me.name;

// Agenti na kojima je merenje u toku (bilo čije), za onemogućavanje dugmadi.
// Ping sa konzole ne opterećuje računar, pa ne blokira ostala merenja na njemu
// (samo drugi ping istog računara) – zato je u posebnoj mapi.
function remoteBusy() {
  const m = new Map();
  for (const t of liveTests.filter((x) => isActive(x) && !x.from_console)) {
    for (const id of [t.agent_id, t.peer_id]) if (id) m.set(id, t);
  }
  return m;
}
function pingBusy() {
  return new Map(liveTests.filter((t) => isActive(t) && t.from_console).map((t) => [t.agent_id, t]));
}
const busyLabel = (t) => `U toku: ${kindsLive[t.kind] || t.kind} (pokrenuo ${t.user === me.name ? "vi" : t.user || "?"})`;
const kindsLive = { latency: "merenje latencije", speedtest: "test brzine", lan: "LAN test", ping: "ping" };

function renderLive() {
  const visible = liveTests.filter((t) => liveShowOthers || isMine(t));
  const othersActive = liveTests.filter((t) => !isMine(t) && isActive(t));
  const on = $("live-others-note");
  on.replaceChildren();
  on.hidden = liveShowOthers || !othersActive.length;
  if (!on.hidden) {
    const users = [...new Set(othersActive.map((t) => t.user || "?"))].join(", ");
    on.append(`U toku ${othersActive.length === 1 ? "je 1 merenje" : `su ${othersActive.length} merenja`} drugih korisnika (${users}).`,
      el("button", { type: "button", onclick: () => { $("live-others").checked = true; $("live-others").dispatchEvent(new Event("change")); } }, "Prikaži"));
  }
  const shown = visible.find((t) => t.id === liveSelected) || visible.find(isActive) || visible[0];
  $("live-empty").hidden = !!shown;
  $("live-body").hidden = !shown;
  $("lat-body").hidden = true;
  const more = $("live-more");
  more.replaceChildren();
  const others = visible.filter((t) => t !== shown && isActive(t));
  if (others.length) {
    more.append("Takođe u toku: ");
    for (const o of others) {
      more.append(el("button", { type: "button", onclick: () => { liveSelected = o.id; renderLive(); } },
        (o.kind === "lan" ? `${o.host} ↔ ${o.peer_host}` : o.kind === "latency" ? `${o.host} (latencija)` : o.kind === "ping" ? `${o.host} (ping)` : o.host) +
          (isMine(o) ? "" : ` · ${o.user}`)));
    }
  }
  if (!shown) return;
  const latKind = shown.kind === "latency" || shown.kind === "ping";
  $("lat-body").hidden = !latKind;
  $("live-body").hidden = latKind;
  if (latKind) {
    renderLatency(shown);
    return;
  }

  const lan = shown.kind === "lan";
  $("live-title").textContent = lan ? `LAN: ${shown.host} ↔ ${shown.peer_host}` : `Brzina: ${shown.host} ↔ konzola`;
  $("live-sub").textContent = `${new Date(shown.started).toLocaleTimeString("sr-RS")}` +
    (shown.user ? ` · pokrenuo ${shown.user}` : "") + ` · ${shown.seconds} s po smeru`;
  const max = scaleFor(shown);
  const active = isActive(shown);
  let value = 0;
  let upload = shown.phase === "upload";
  let phase = "";
  let num = "0";
  const elapsed = Math.round((Date.now() - new Date(shown.started)) / 1000);

  if (active) {
    if (shown.phase === "download" || shown.phase === "upload") {
      value = shown.mbps;
      num = fmtMbps(value);
      phase = shown.phase === "download" ? "↓ download" : "↑ upload";
    } else if (!shown.live && shown.phase !== "latency" && elapsed > 3) {
      num = "…";
      phase = `merenje u toku (${elapsed} s)`;
    } else {
      num = "…";
      phase = shown.phase === "latency" || shown.phase === "start" ? "merenje latencije…" : shown.phase;
    }
  } else if (shown.phase === "done") {
    upload = !shown.download_mbps && !!shown.upload_mbps;
    value = upload ? shown.upload_mbps : shown.download_mbps;
    num = fmtMbps(value);
    phase = upload ? "rezultat (↑ upload)" : "rezultat (↓ download)";
  } else {
    num = "—";
    phase = "greška";
  }
  // Po završetku skazaljka se vraća na nulu; rezultat ostaje kao broj i u poljima ispod.
  setGauge(active ? value : 0, max, upload);
  if (active) {
    const pr = speedProgress(shown);
    setProgress("sp-progress", pr.pct, `${pr.pct.toFixed(0)}% · još ~${pr.left} s`);
  } else if (shown.phase === "done") {
    setProgress("sp-progress", 100, `✓ Test završen ${new Date(shown.finished).toLocaleTimeString("sr-RS")}`, "done");
  } else {
    setProgress("sp-progress", 100, "✕ Test prekinut", "error");
  }
  $("live-num").textContent = num;
  $("live-phase").textContent = phase;
  $("live-phase").className = shown.phase === "error" ? "bad" : "";
  drawChart(shown, max);
  const flow = flowLabels(shown);
  const legend = $("live-legend");
  legend.replaceChildren();
  for (const [phase, cls, arrow] of [["download", "dl", "↓"], ["upload", "ul", "↑"]]) {
    if (shown.direction !== "both" && shown.direction !== phase) continue;
    legend.append(el("span", { title: flow[phase] }, el("i", { class: `sw ${cls}` }), `${arrow} ${flow[phase]}`));
  }

  // Istovremeni testovi brzine (bilo čiji) dele vezu ka konzoli ili između
  // lokacija, pa jedan drugom smanjuju rezultat.
  const overlap = liveTests.filter((o) => o !== shown && isActive(o) && (o.kind === "speedtest" || o.kind === "lan"));
  const ov = $("live-overlap");
  ov.hidden = !active || !overlap.length;
  if (!ov.hidden) {
    ov.textContent = "⚠ Istovremeno teče i: " +
      overlap.map((o) => (o.kind === "lan" ? `${o.host} ↔ ${o.peer_host}` : `${o.host} ↔ konzola`) + ` (${o.user || "?"})`).join(", ") +
      ". Ako testovi dele istu vezu, rezultati mogu biti niži od stvarnih; po potrebi ponovite test.";
  }

  const fin = $("live-final");
  fin.replaceChildren();
  if (shown.phase === "done") {
    const loss = shown.sent ? `${shown.loss_pct.toFixed(0)}% (${shown.sent - shown.received}/${shown.sent})` : "—";
    fin.append(
      tile("↓ download", shown.download_mbps ? `${fmtMbps(shown.download_mbps)} Mb/s` : "—", "", flow.download),
      tile("↑ upload", shown.upload_mbps ? `${fmtMbps(shown.upload_mbps)} Mb/s` : "—", "", flow.upload),
      tile("latencija", shown.latency_ms ? `${shown.latency_ms.toFixed(1)} ms` : "—", "",
        shown.latency_ms ? `jitter ${shown.jitter_ms.toFixed(1)} ms` : ""),
      tile("gubitak paketa", shown.loss_pct > 0 ? `⚠ ${loss}` : loss, shown.loss_pct > 0 ? "bad" : "",
        "Izgubljeni probni paketi pri merenju latencije pre testa brzine"),
    );
  }
  // „Ponovi test”: isti računari i parametri; samo kada su računari na vezi i slobodni.
  const btn = $("live-repeat");
  const ids = [shown.agent_id, shown.peer_id].filter(Boolean);
  const rbusy = remoteBusy();
  const ready = ids.every((id) => (agents.find((a) => a.agent_id === id) || {}).online && !busy.has(id) && !rbusy.has(id));
  btn.disabled = active || !ready || !shown.request;
  btn.title = active ? "Test je u toku" : !ready ? "Računar nije na vezi ili je merenje u toku" : "Pokreni isti test ponovo";
  btn.onclick = () => repeatTest(shown);
  let note = "";
  if (shown.phase === "error") note = shown.error;
  else if (lan && !shown.live && (active ? elapsed > 4 : true)) {
    note = "Agent ne šalje međurezultate (starija verzija agenta): vidi se samo konačan rezultat. Nadogradite agenta za prikaz uživo.";
  }
  $("live-note").textContent = note;
  $("live-note").className = shown.phase === "error" ? "bad" : "muted";
}

let lastBusyKey = "";
let lastActive = new Set();

async function pollLive() {
  clearTimeout(liveTimer);
  try {
    const r = await api("GET", "/api/live");
    liveTests = r.tests || [];
    if (liveSelected && !liveTests.some((t) => t.id === liveSelected && isActive(t))) liveSelected = null;
    renderLive();
    // Kada se neko merenje završi (npr. ping u pozadini), osveži Rezultate.
    const activeNow = new Set(liveTests.filter(isActive).map((t) => t.id));
    if ([...lastActive].some((id) => !activeNow.has(id))) {
      // Ping pokrenut iz ovog prozora: izveštaj se sam otvori u Rezultatima.
      const done = liveTests.filter((t) => !activeNow.has(t.id) && t.result_id && myPings.has(t.result_id));
      for (const t of done) {
        myPings.delete(t.result_id);
        openReports.add(t.result_id);
        resultsPage = 1;
        setStatus(`Ping ${t.device ? "uređaja" : "računara"} ${t.host} je završen – izveštaj je u tabeli Rezultati.`, "ok");
      }
      loadResults().then(() => {
        if (done.length) {
          const row = document.querySelector(`#results tr.row[data-id="${CSS.escape(done[0].result_id)}"]`);
          if (row) row.scrollIntoView({ behavior: "smooth", block: "center" });
        }
      });
    }
    lastActive = activeNow;
    const key = [...remoteBusy().keys()].sort().join(",") + "|" + [...pingBusy().keys()].sort().join(",");
    if (key !== lastBusyKey) {
      lastBusyKey = key;
      renderAgents();
      if (typeof renderNetTable === "function") renderNetTable();
    }
  } catch {
    // Prikaz uživo nije bitan; sledeći pokušaj za par sekundi.
  }
  const fast = busy.size > 0 || liveTests.some(isActive);
  liveTimer = setTimeout(pollLive, fast ? 500 : 3000);
}

function kickLive() {
  clearTimeout(liveTimer);
  liveTimer = setTimeout(pollLive, 300);
}

// ---------- obaveštenja ----------

let notifyCfg = null;

function watchButton(a) {
  const all = notifyCfg && notifyCfg.all_agents;
  return el("button", {
    type: "button",
    class: a.watched ? "watch on" : "watch",
    disabled: all,
    title: all
      ? "Obaveštenja su uključena za sve računare (Podešavanja → Obaveštenja)"
      : a.watched ? "Isključi obaveštenje kada ovaj računar nije na vezi" : "Obavesti me kada ovaj računar nije na vezi",
    "aria-pressed": a.watched ? "true" : "false",
    onclick: () => toggleWatch(a),
  }, "🔔");
}

async function toggleWatch(a) {
  try {
    await api("POST", `/api/agents/${encodeURIComponent(a.agent_id)}/watch`, { watch: !a.watched });
    setStatus(a.watched ? `${a.hostname}: obaveštenja isključena.` : `${a.hostname}: dobićete obaveštenje kada nije na vezi.`, "ok");
    if (!a.watched && notifyCfg && !notifyCfg.enabled) {
      setStatus(`${a.hostname} je označen, ali obaveštenja nisu uključena (Podešavanja → Obaveštenja).`, "busy");
    }
    await loadAgents();
  } catch (e) {
    setStatus(`Obaveštenja: ${e.message}`, "bad");
  }
}

function fmtTime(iso) {
  return new Date(iso).toLocaleString("sr-RS");
}

const CHANNELS = ["email", "sms", "teams", "ntfy"];

// chConfigured odgovara proveri na serveru: kanal ima potrebne podatke.
function chConfigured(c, key) {
  switch (key) {
    case "email": return !!(c.email_host && c.email_to);
    case "sms": return !!(c.email_host && c.sms_email);
    case "teams": return !!c.teams_url;
    case "ntfy": return !!c.ntfy_url;
  }
  return false;
}

function chStateText(c, key) {
  const o = (c.channels || {})[key] || {};
  if (!o.on) return ["isključen", "muted"];
  if (!chConfigured(c, key)) return ["uključen, ali nije podešen", "bad"];
  if (!c.enabled) return ["ne šalje se – glavni prekidač je isključen", "warn-text"];
  const srv = o.servers === undefined || o.servers === null ? o.net : o.servers;
  const what = [o.agents ? "računari" : "", o.net ? "mrežna oprema" : "", srv ? "serveri" : ""].filter(Boolean);
  if (!what.length) return ["uključen, ali nije izabrano za šta", "warn-text"];
  return [`šalje se: ${what.join(", ")}`, "ok"];
}

function showNotify(c) {
  notifyCfg = c;
  $("n-enabled").checked = c.enabled;
  $("n-scope").value = c.all_agents ? "all" : "watched";
  $("n-delay").value = c.delay_minutes;
  $("n-recovery").checked = !c.skip_recovery;
  $("n-host").value = c.email_host;
  $("n-port").value = c.email_port;
  $("n-from").value = c.email_from;
  $("n-to").value = c.email_to;
  $("n-user").value = c.email_user;
  $("n-pass").value = "";
  $("n-pass").placeholder = c.email_password_set ? "sačuvana (prazno = bez promene)" : "";
  $("n-tls").value = c.email_tls;
  $("n-teams").value = c.teams_url;
  $("n-ntfy").value = c.ntfy_url;
  $("n-ntfy-token").value = "";
  $("n-ntfy-token").placeholder = c.ntfy_token_set ? "sačuvan (prazno = bez promene)" : "";
  $("n-proxy").value = c.proxy;
  $("n-sms").value = c.sms_email || "";
  $("n-sms-time").value = c.sms_email ? c.sms_check_time || "" : "08:00";
  $("n-sms-q").checked = !!c.sms_queries;
  $("n-imap-host").value = c.imap_host || "";
  $("n-imap-port").value = c.imap_port || 993;
  $("n-sms-aliases").value = c.sms_aliases || "";
  $("n-sms-srv-aliases").value = c.sms_server_aliases || "";
  $("n-srv-delay").value = c.server_delay_minutes || 2;
  $("n-srv-high").value = c.server_high_minutes || 10;
  $("n-srv-cpu").value = c.server_cpu_pct || 90;
  $("n-srv-mem").value = c.server_mem_pct || 90;
  const st = c.status || {};
  const chs = st.channels || {};
  for (const k of CHANNELS) {
    const o = (c.channels || {})[k] || {};
    $(`ch-${k}-on`).checked = !!o.on;
    $(`ch-${k}-agents`).checked = !!o.agents;
    $(`ch-${k}-net`).checked = !!o.net;
    $(`ch-${k}-srv`).checked = o.servers === undefined || o.servers === null ? !!o.net : !!o.servers;
    const [txt, cls] = chStateText(c, k);
    const cell = $(`ch-${k}-st`);
    cell.replaceChildren(el("span", { class: cls }, txt));
    const cs = chs[k];
    if (cs && cs.last_sent) cell.append(el("div", { class: "muted small" }, `poslednje slanje: ${fmtTime(cs.last_sent)}`));
    if (cs && cs.last_error) cell.append(el("div", { class: "bad small" }, `greška (${fmtTime(cs.last_error_at)}): ${cs.last_error}`));
  }
  const q = $("n-sms-q-st");
  q.className = "span2 small " + (st.query_error ? "bad" : "muted");
  q.textContent = !c.sms_queries ? "" : st.query_error
    ? `Čitanje upita nije uspelo (${fmtTime(st.query_error_at)}): ${st.query_error}`
    : st.query_at ? `Sanduče pročitano: ${fmtTime(st.query_at)} (čita se na 20 s).` : "Sanduče još nije pročitano.";
  const parts = [c.all_agents ? "Prate se svi računari." : `Označenih računara: ${c.watched}.`];
  if (st.last_sent) parts.push(`Poslednje obaveštenje: ${fmtTime(st.last_sent)} – ${st.last_title}.`);
  $("notify-status").textContent = parts.join(" ");
}

async function loadNotify() {
  try {
    showNotify(await api("GET", "/api/notify"));
  } catch (e) {
    $("notify-msg").className = "bad";
    $("notify-msg").textContent = e.message;
  }
}

async function saveNotify() {
  const channels = {};
  for (const k of CHANNELS) {
    channels[k] = { on: $(`ch-${k}-on`).checked, agents: $(`ch-${k}-agents`).checked, net: $(`ch-${k}-net`).checked,
      servers: $(`ch-${k}-srv`).checked };
  }
  const body = {
    enabled: $("n-enabled").checked,
    all_agents: $("n-scope").value === "all",
    delay_minutes: intVal("n-delay"),
    skip_recovery: !$("n-recovery").checked,
    email_host: $("n-host").value.trim(),
    email_port: intVal("n-port"),
    email_tls: $("n-tls").value,
    email_user: $("n-user").value.trim(),
    email_from: $("n-from").value.trim(),
    email_to: $("n-to").value.trim(),
    teams_url: $("n-teams").value.trim(),
    ntfy_url: $("n-ntfy").value.trim(),
    proxy: $("n-proxy").value.trim(),
    sms_email: $("n-sms").value.trim(),
    sms_check_time: $("n-sms").value.trim() ? $("n-sms-time").value : "",
    sms_queries: $("n-sms-q").checked,
    imap_host: $("n-imap-host").value.trim(),
    imap_port: intVal("n-imap-port"),
    sms_aliases: $("n-sms-aliases").value,
    sms_server_aliases: $("n-sms-srv-aliases").value,
    server_delay_minutes: intVal("n-srv-delay"),
    server_high_minutes: intVal("n-srv-high"),
    server_cpu_pct: intVal("n-srv-cpu"),
    server_mem_pct: intVal("n-srv-mem"),
    channels,
  };
  if ($("n-pass").value) body.email_password = $("n-pass").value;
  if ($("n-ntfy-token").value) body.ntfy_token = $("n-ntfy-token").value;
  showNotify(await api("POST", "/api/notify", body));
  renderAgents();
}

$("notify-form").addEventListener("submit", async (ev) => {
  ev.preventDefault();
  const msg = $("notify-msg");
  try {
    await saveNotify();
    msg.className = "ok";
    msg.textContent = "Sačuvano.";
  } catch (e) {
    msg.className = "bad";
    msg.textContent = e.message;
  }
});

for (const btn of document.querySelectorAll("#notify-form .ch-test")) {
  btn.addEventListener("click", async () => {
    const msg = $("notify-msg");
    const ch = btn.dataset.ch;
    const name = btn.closest("tr").firstElementChild.textContent;
    btn.disabled = true;
    msg.className = "busy";
    msg.textContent = `Čuvam podešavanja i šaljem probu (${name})…`;
    try {
      await saveNotify();
      const r = await api("POST", "/api/notify/test", { channel: ch });
      const res = r.results || [];
      const ok = res.length > 0 && res.every((x) => x.ok);
      msg.className = ok ? "ok" : "bad";
      msg.textContent = res.length
        ? res.map((x) => `${x.channel}: ${x.ok ? "poslato" : x.error}`).join(" · ") +
          (ch === "sms" && ok ? " – telefon treba da pošalje SMS na sve brojeve iz aplikacije." : "")
        : "Dolina nije poslato.";
      loadNotify();
    } catch (e) {
      msg.className = "bad";
      msg.textContent = e.message;
    } finally {
      btn.disabled = false;
    }
  });
}

// ---------- podešavanja ----------

let settings = null;

function consoleURL() {
  // Ako je konzola otvorena preko localhost-a, agentima treba pravo ime servera.
  const h = location.hostname;
  if (h === "localhost" || h === "127.0.0.1" || h === "[::1]") {
    return `${location.protocol}//<ime-servera>${location.port ? ":" + location.port : ""}`;
  }
  return location.origin;
}

function fmtDate(iso) {
  return new Date(iso).toLocaleDateString("sr-RS");
}

async function loadSettings() {
  try {
    settings = await api("GET", "/api/settings");
  } catch (e) {
    setStatus(`Podešavanja: ${e.message}`, "bad");
    return;
  }
  $("token").textContent = settings.agent_token;
  deployKindChanged();
  if (!$("d-url").value) $("d-url").value = settings.agent_url || (consoleURL().includes("<") ? "" : location.origin);
  $("msi-cmd").textContent =
    `msiexec /i NetworkMonitorAgent.msi /qn CONSOLE_URL=${consoleURL()} AGENT_TOKEN=${settings.agent_token}`;
  const c = settings.certificate;
  const info = $("cert-info");
  info.replaceChildren();
  if (!settings.tls) {
    info.append("Konzola radi bez TLS-a (insecure_http).");
    $("cert-form").hidden = true;
  } else if (c) {
    info.append(
      el("div", {}, "Izdat za: ", el("strong", {}, c.subject)),
      el("div", {}, `Izdavač: ${c.issuer}`),
      el("div", {}, `Imena: ${(c.dns_names || []).join(", ") || "—"}`),
      el("div", { class: new Date(c.not_after) - Date.now() < 30 * 864e5 ? "warn" : "" }, `Važi do: ${fmtDate(c.not_after)}`),
    );
  }
  $("cert-warning").hidden = !(c && c.self_signed);
}

// ---------- prozor podešavanja ----------

var settingsDirty = false; // var: koristi se i u api() pre ove linije

function selectSettingsTab(tab) {
  for (const b of document.querySelectorAll("#settings-dlg .sd-tabs button")) {
    const on = b.dataset.tab === tab;
    b.classList.toggle("active", on);
    b.setAttribute("aria-selected", on ? "true" : "false");
  }
  for (const p of document.querySelectorAll("#settings-dlg .tab-panel")) p.classList.toggle("active", p.dataset.tab === tab);
  try { localStorage.setItem("netmon.settingsTab", tab); } catch { /* bez trajnog pamćenja */ }
}

// openSettings otvara prozor podešavanja na zadatoj kartici (bez kartice:
// poslednja otvarana).
function openSettings(tab) {
  const dlg = $("settings-dlg");
  if (!tab) {
    try { tab = localStorage.getItem("netmon.settingsTab"); } catch { tab = null; }
  }
  if (!isAdmin() || !document.querySelector(`#settings-dlg .sd-tabs button[data-tab="${tab}"]`)) tab = "general";
  selectSettingsTab(tab);
  if (!dlg.open) dlg.showModal();
  if (isAdmin()) {
    loadSettings();
    loadUsers();
    loadNotify();
    loadSNMP();
  }
  settingsDirty = false;
}

function closeSettings() {
  if (settingsDirty && !confirm("Imate nesačuvane izmene. Zatvoriti podešavanja bez čuvanja?")) return;
  settingsDirty = false;
  $("settings-dlg").close();
}

$("settings-toggle").addEventListener("click", () => openSettings());
$("settings-close").addEventListener("click", closeSettings);
$("settings-dlg").addEventListener("cancel", (ev) => { // Esc
  ev.preventDefault();
  closeSettings();
});
for (const b of document.querySelectorAll("#settings-dlg .sd-tabs button")) {
  b.addEventListener("click", () => selectSettingsTab(b.dataset.tab));
}
for (const ev of ["input", "change"]) {
  $("settings-dlg").addEventListener(ev, (e) => {
    if (e.target.matches("input, select, textarea")) settingsDirty = true;
  });
}


$("token-show").addEventListener("click", () => {
  $("token").hidden = !$("token").hidden;
  $("token-show").textContent = $("token").hidden ? "Prikaži" : "Sakrij";
});

$("token-copy").addEventListener("click", async () => {
  try {
    await navigator.clipboard.writeText(settings ? settings.agent_token : "");
    setStatus("Token je kopiran.", "ok");
  } catch {
    setStatus("Kopiranje nije uspelo; prikažite token i kopirajte ga ručno.", "bad");
  }
});

function fileToBase64(file) {
  return new Promise((resolve, reject) => {
    const r = new FileReader();
    r.onload = () => resolve(String(r.result).split(",", 2)[1] || "");
    r.onerror = () => reject(r.error);
    r.readAsDataURL(file);
  });
}

$("cert-form").addEventListener("submit", async (ev) => {
  ev.preventDefault();
  const msg = $("cert-msg");
  const file = $("cert-file").files[0];
  if (!file) return;
  msg.className = "busy";
  msg.textContent = "Otpremanje…";
  try {
    const info = await api("POST", "/api/settings/certificate", {
      pfx_base64: await fileToBase64(file),
      password: $("cert-pass").value,
    });
    msg.className = "ok";
    msg.textContent = `Sertifikat je primenjen (${info.subject}). Osvežite stranicu preko imena iz sertifikata.`;
    $("cert-form").reset();
    loadSettings();
  } catch (e) {
    msg.className = "bad";
    msg.textContent = e.message;
  }
});

$("pass-form").addEventListener("submit", async (ev) => {
  ev.preventDefault();
  const msg = $("pass-msg");
  if ($("pass-new").value !== $("pass-new2").value) {
    msg.className = "bad";
    msg.textContent = "Nove lozinke se ne poklapaju.";
    return;
  }
  try {
    await api("POST", "/api/settings/password", { current: $("pass-cur").value, new_password: $("pass-new").value });
    msg.className = "ok";
    msg.textContent = "Lozinka je promenjena. Prijavite se ponovo novom lozinkom.";
    setTimeout(() => location.reload(), 1500);
    $("pass-form").reset();
  } catch (e) {
    msg.className = "bad";
    msg.textContent = e.message;
  }
});

// ---------- instalacija agenata iz konzole ----------

const deployLabels = { pending: "čeka", running: "instalira se…", ok: "uspešno", error: "greška" };
let deployTimer = null;

function addHosts(names) {
  const cur = $("d-hosts").value.split(/[\s,;]+/).filter(Boolean);
  const all = [...new Set([...cur, ...names])];
  $("d-hosts").value = all.join("\n");
}
$("d-fill-offline").addEventListener("click", () => addHosts(agents.filter((a) => !a.online).map((a) => a.hostname)));
$("d-fill-all").addEventListener("click", () => addHosts(agents.map((a) => a.hostname)));
$("d-fill-old").addEventListener("click", () => {
  const all = agents.filter(needsUpgrade);
  const old = all.filter((a) => a.online);
  addHosts(old.map((a) => a.hostname));
  if (old.length) {
    $("d-upgrade").checked = true;
    $("d-url").disabled = true;
  }
  setStatus(upgradeMsg(old.length, all.length - old.length, "računara"), "ok");
});
$("d-upgrade").addEventListener("change", () => { $("d-url").disabled = $("d-upgrade").checked; });

// Serveri (agent za servere) se uzimaju sa stranice „Serveri”.
async function serverList() {
  try {
    return (await api("GET", "/api/servers")).servers || [];
  } catch {
    return [];
  }
}
$("d-fill-srv-offline").addEventListener("click", async () => addHosts((await serverList()).filter((s) => !s.online).map((s) => s.name)));
$("d-fill-srv-all").addEventListener("click", async () => addHosts((await serverList()).map((s) => s.name)));
$("d-fill-srv-old").addEventListener("click", async () => {
  const all = (await serverList()).filter(needsUpgrade);
  const old = all.filter((s) => s.online);
  addHosts(old.map((s) => s.name));
  if (old.length) {
    $("d-upgrade").checked = true;
    $("d-url").disabled = true;
  }
  setStatus(upgradeMsg(old.length, all.length - old.length, "servera"), "ok");
});

// deployKindChanged prilagođava formu izabranoj vrsti agenta.
function deployKindChanged() {
  const srv = $("d-kind").value === "server";
  $("d-pc-fill").hidden = srv;
  $("d-srv-fill").hidden = !srv;
  $("d-hosts-lbl").textContent = srv ? "Serveri" : "Računari";
  $("d-hosts").placeholder = srv ? "SQL01\nAPP02\nSRV-MONITOR" : "INF-PC01\nINF-PC02\nfin-racunar";
  $("deploy-nomsi").hidden = !settings || (srv ? settings.server_agent_msi : settings.agent_msi);
  $("deploy-nomsi").textContent = srv
    ? "MSI agenta za servere nije pronađen na serveru konzole. Instalirajte novu verziju konzole (njen MSI sadrži i MSI agenta za servere)."
    : "MSI agenta nije pronađen na serveru. Instalirajte novu verziju konzole (njen MSI sadrži i MSI agenta).";
}
$("d-kind").addEventListener("change", deployKindChanged);

function renderDeploy(job) {
  $("deploy-result").hidden = false;
  const counts = { ok: 0, error: 0 };
  const tb = $("deploy-table").tBodies[0];
  tb.replaceChildren();
  for (const it of job.items) {
    if (it.status in counts) counts[it.status]++;
    const name = it.target && it.target.toLowerCase() !== it.host.toLowerCase() ? `${it.host} (${it.target})` : it.host;
    tb.append(el("tr", {},
      el("td", {}, name),
      el("td", { class: `st-${it.status}` }, deployLabels[it.status] || it.status),
      el("td", {}, it.message || ""),
    ));
  }
  const kind = job.upgrade ? "Nadogradnja" : "Instalacija";
  $("deploy-summary").textContent = job.finished
    ? `${kind} završena: uspešno ${counts.ok}, greška ${counts.error}.`
    : `${kind} u toku… (uspešno ${counts.ok}, greška ${counts.error}, ukupno ${job.items.length})`;
}

async function pollDeploy(id) {
  clearTimeout(deployTimer);
  try {
    const job = await api("GET", `/api/deploy/${encodeURIComponent(id)}`);
    renderDeploy(job);
    if (!job.finished) {
      deployTimer = setTimeout(() => pollDeploy(id), 2000);
    } else {
      $("d-start").disabled = false;
      loadAgents();
    }
  } catch (e) {
    $("deploy-summary").textContent = e.message;
    $("d-start").disabled = false;
  }
}

$("deploy-form").addEventListener("submit", async (ev) => {
  ev.preventDefault();
  const hosts = $("d-hosts").value.trim();
  if (!hosts) {
    $("deploy-result").hidden = false;
    $("deploy-summary").textContent = $("d-kind").value === "server" ? "Upišite bar jedan server." : "Upišite bar jedan računar.";
    return;
  }
  $("d-start").disabled = true;
  try {
    const job = await api("POST", "/api/deploy", {
      hosts: [hosts],
      console_url: $("d-url").value.trim(),
      user: $("d-user").value.trim(),
      password: $("d-pass").value,
      upgrade: $("d-upgrade").checked,
      kind: $("d-kind").value,
    });
    $("d-pass").value = ""; // lozinka se ne zadržava u stranici
    renderDeploy(job);
    pollDeploy(job.id);
  } catch (e) {
    $("deploy-result").hidden = false;
    $("deploy-table").tBodies[0].replaceChildren();
    $("deploy-summary").textContent = e.message;
    $("d-start").disabled = false;
  }
});

// ---------- korisnici ----------

async function loadUsers() {
  let list;
  try {
    list = await api("GET", "/api/users");
  } catch (e) {
    setStatus(`Korisnici: ${e.message}`, "bad");
    return;
  }
  const tb = $("users").tBodies[0];
  tb.replaceChildren();
  for (const u of list) {
    const self = u.name === me.name;
    const roleSel = el("select", { disabled: u.primary || self },
      el("option", { value: "operator" }, roleNames.operator),
      el("option", { value: "admin" }, roleNames.admin));
    roleSel.value = u.role;
    roleSel.addEventListener("change", () => updateUser(u.name, { role: roleSel.value }));
    tb.append(el("tr", {},
      el("td", {}, el("strong", {}, u.name), u.primary ? el("div", { class: "muted" }, "glavni administrator") : null),
      el("td", {}, roleSel),
      el("td", {}, u.primary ? el("span", { class: "muted" }, "lozinka: odeljak „Moja lozinka”") : el("div", { class: "actions" },
        el("button", { type: "button", onclick: () => resetPassword(u.name) }, "Nova lozinka"),
        self ? null : el("button", { type: "button", onclick: () => deleteUser(u.name) }, "Obriši"),
      )),
    ));
  }
}

function userMsg(text, cls) {
  $("user-msg").className = cls;
  $("user-msg").textContent = text;
}

async function updateUser(name, body) {
  try {
    await api("POST", `/api/users/${encodeURIComponent(name)}`, body);
    userMsg(`Korisnik ${name} je izmenjen.`, "ok");
  } catch (e) {
    userMsg(e.message, "bad");
  }
  loadUsers();
}

function resetPassword(name) {
  const p = prompt(`Nova lozinka za korisnika ${name} (najmanje 8 karaktera):`);
  if (p === null) return;
  updateUser(name, { password: p });
}

async function deleteUser(name) {
  if (!confirm(`Obrisati korisnika ${name}?`)) return;
  try {
    await api("DELETE", `/api/users/${encodeURIComponent(name)}`);
    userMsg(`Korisnik ${name} je obrisan.`, "ok");
  } catch (e) {
    userMsg(e.message, "bad");
  }
  loadUsers();
}

$("user-form").addEventListener("submit", async (ev) => {
  ev.preventDefault();
  try {
    const u = await api("POST", "/api/users", {
      name: $("u-name").value.trim(),
      password: $("u-pass").value,
      role: $("u-role").value,
    });
    userMsg(`Korisnik ${u.name} (${roleNames[u.role]}) je dodat.`, "ok");
    $("user-form").reset();
  } catch (e) {
    userMsg(e.message, "bad");
  }
  loadUsers();
});

// ---------- pokretanje ----------

async function init() {
  try {
    me = await api("GET", "/api/me");
  } catch (e) {
    setStatus(`Prijava: ${e.message}`, "bad");
  }
  $("whoami").textContent = `${me.name} · ${roleNames[me.role] || me.role}`;
  $("whoami").title = `Verzija konzole: ${me.version || "?"}`;
  $("version").textContent = me.version ? fmtVersion(me.version) : "";
  for (const n of document.querySelectorAll(".admin-only")) n.hidden = !isAdmin();
  if (typeof showPage === "function") showPage(); // stranica „Mrežna oprema” (netdev.js)
  await loadLocations();
  if (isAdmin()) await loadNotify(); // pre spiska agenata (dugme 🔔 zavisi od podešavanja)
  await loadAgents();
  loadResults();
  pollLive();
  if (isAdmin()) {
    await loadSettings();
    if (settings && settings.certificate && settings.certificate.self_signed) {
      setStatus("Privremeni sertifikat: agenti ne mogu da se povežu dok se ne otpremi sertifikat (Podešavanja).", "busy");
    }
  }
}

$("logout").addEventListener("click", async () => {
  try {
    await fetch("/api/logout", { method: "POST", headers: { "X-Requested-With": "fetch" } });
  } finally {
    location.replace("/");
  }
});

$("filter").addEventListener("change", () => { resultsPage = 1; loadResults(); });
let resultsSearchTimer = null;
$("r-search").addEventListener("input", () => {
  clearTimeout(resultsSearchTimer);
  resultsSearchTimer = setTimeout(() => { resultsPage = 1; loadResults(); }, 250);
});
init();
setInterval(loadAgents, 5000);
