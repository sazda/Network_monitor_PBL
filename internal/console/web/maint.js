"use strict";
// Održavanje: preuzimanje loga i backup-a konfiguracije sa switch-eva preko
// SSH-a (samo administrator). Nalog i lozinka ostaju samo u polju forme dok
// je stranica otvorena; brišu se pri odlasku sa stranice.

const MAINT_MODELS = /C9200L|C2960L/i;
let maintDevices = [];
const maintBusy = new Set();
const maintSel = new Set(); // izabrani switch-evi (device id)
let maintJob = null; // { id, timer, view }

function leaveMaint() {
  const p = $("m-pass");
  if (p) p.value = "";
}

async function loadMaint() {
  $("maint-noadmin").hidden = isAdmin();
  $("maint-form").hidden = !isAdmin();
  if (!isAdmin()) {
    $("maint-table").tBodies[0].replaceChildren();
    return;
  }
  try {
    const d = await api("GET", "/api/netdev");
    maintDevices = d.devices.filter((x) => x.snmp_info && MAINT_MODELS.test(x.snmp_info.model || ""));
  } catch (e) {
    maintMsg(`Mrežna oprema: ${e.message}`, "bad");
    return;
  }
  renderMaint();
}

function maintMsg(text, cls = "muted", extra = null) {
  const m = $("maint-msg");
  m.className = cls;
  m.replaceChildren(text, ...(extra ? [" ", extra] : []));
}

function maintVisible() {
  const q = $("maint-search").value.trim().toLowerCase();
  return maintDevices.filter((d) => !q || [d.name, d.ip, d.location_name, d.place].join(" ").toLowerCase().includes(q))
    .sort((a, b) => (a.location_name || "").localeCompare(b.location_name || "") || a.name.localeCompare(b.name));
}

function renderMaint() {
  const list = maintVisible();
  for (const id of [...maintSel]) if (!maintDevices.some((d) => d.id === id)) maintSel.delete(id);
  const running = maintJob && !(maintJob.view && maintJob.view.done);
  $("maint-count").textContent = `${list.length} od ${maintDevices.length} switch-eva` + (maintSel.size ? ` · izabrano ${maintSel.size}` : "");
  const all = $("maint-all");
  const selVisible = list.filter((d) => maintSel.has(d.id)).length;
  all.checked = list.length > 0 && selVisible === list.length;
  all.indeterminate = selVisible > 0 && selVisible < list.length;
  all.disabled = running || !list.length;
  $("maint-bulk-log").disabled = $("maint-bulk-backup").disabled = running || !maintSel.size;
  const tb = $("maint-table").tBodies[0];
  tb.replaceChildren(...(list.length ? list.map((d) => el("tr", {},
    el("td", { class: "sel" }, el("input", { type: "checkbox", checked: maintSel.has(d.id), disabled: running, "aria-label": `Izaberi ${d.name}`,
      onchange: (e) => { if (e.target.checked) maintSel.add(d.id); else maintSel.delete(d.id); renderMaint(); } })),
    el("td", {}, el("strong", {}, d.name), d.status !== "up" ? el("div", { class: "warn-text small" }, "nije na vezi") : null),
    el("td", {}, d.location_name || "—", d.place ? el("div", { class: "muted small" }, d.place) : null),
    el("td", { class: "addrs" }, d.ip),
    el("td", {}, d.snmp_info.model),
    el("td", { class: "actions" },
      el("button", { type: "button", disabled: maintBusy.has(d.id), onclick: () => maintRun(d, "log") }, "Preuzmi log"),
      el("button", { type: "button", disabled: maintBusy.has(d.id), onclick: () => maintRun(d, "backup") }, "Preuzmi backup"))))
    : [el("tr", {}, el("td", { colspan: 6, class: "empty" },
      "Nema switch-eva C9200L ili C2960L sa pročitanim modelom (model se čita preko SNMP-a, u Mrežnoj opremi)."))]));
}

async function maintRun(d, action) {
  const user = $("m-user").value.trim(), password = $("m-pass").value;
  if (!user || !password) {
    maintMsg("Upišite nalog i lozinku.", "bad");
    $(user ? "m-pass" : "m-user").focus();
    return;
  }
  const label = action === "log" ? "log" : "backup konfiguracije";
  maintBusy.add(d.id);
  renderMaint();
  maintMsg(`Preuzimam ${label} sa ${d.name}…`);
  try {
    const r = await fetch(`/api/maint/${encodeURIComponent(d.id)}/${action}`, {
      method: "POST",
      headers: { "X-Requested-With": "fetch", "Content-Type": "application/json" },
      body: JSON.stringify({ user, password }),
    });
    if (r.status === 401) { location.reload(); return; }
    if (!r.ok) {
      const e = await r.json().catch(() => ({}));
      if (r.status === 422) $("m-pass").value = ""; // domen je odbio nalog
      const msg = e.error || `HTTP ${r.status}`;
      // Promenjen SSH ključ switch-a: posle zamene uređaja ključ se briše ručno.
      const fix = r.status === 409 ? el("button", { type: "button", onclick: () => maintForgetKey(d) }, "Switch je zamenjen – zaboravi stari ključ") : null;
      maintMsg(`${d.name}: ${msg}`, "bad", fix);
      return;
    }
    const name = await maintSave(r, `${d.name}-${action}.txt`);
    maintMsg(`${d.name}: ${label} je preuzet (${name}).`, "ok");
  } catch (e) {
    maintMsg(`${d.name}: ${e.message}`, "bad");
  } finally {
    maintBusy.delete(d.id);
    renderMaint();
  }
}

// maintSave: čuva odgovor kao fajl na računaru; vraća naziv fajla.
async function maintSave(r, fallback) {
  const blob = await r.blob();
  const cd = r.headers.get("Content-Disposition") || "";
  const name = (cd.match(/filename="([^"]+)"/) || [])[1] || fallback;
  const a = el("a", { href: URL.createObjectURL(blob), download: name });
  document.body.append(a);
  a.click();
  a.remove();
  setTimeout(() => URL.revokeObjectURL(a.href), 10000);
  return name;
}

// ---------- više switch-eva odjednom ----------

const MAINT_STATE = { waiting: ["čeka", "muted"], running: ["preuzima se…", ""], ok: ["preuzeto", "ok"], error: ["greška", "bad"], skipped: ["preskočeno", "warn-text"] };

async function maintBulk(action) {
  const user = $("m-user").value.trim(), password = $("m-pass").value;
  if (!user || !password) {
    maintMsg("Upišite nalog i lozinku.", "bad");
    $(user ? "m-pass" : "m-user").focus();
    return;
  }
  const ids = maintDevices.filter((d) => maintSel.has(d.id)).map((d) => d.id);
  if (!ids.length) return;
  const label = action === "log" ? "log" : "backup konfiguracije";
  maintMsg(`Proveravam nalog u domenu, pa preuzimam ${label} sa ${ids.length} switch-eva…`);
  $("maint-bulk-log").disabled = $("maint-bulk-backup").disabled = true;
  let v;
  try {
    const r = await fetch("/api/maintjobs", {
      method: "POST",
      headers: { "X-Requested-With": "fetch", "Content-Type": "application/json" },
      body: JSON.stringify({ action, ids, user, password }),
    });
    if (r.status === 401) { location.reload(); return; }
    const body = await r.json().catch(() => ({}));
    if (!r.ok) {
      if (r.status === 422) $("m-pass").value = "";
      maintMsg(body.error || `HTTP ${r.status}`, "bad");
      renderMaint();
      return;
    }
    v = body;
  } catch (e) {
    maintMsg(e.message, "bad");
    renderMaint();
    return;
  }
  maintJob = { id: v.id, view: v, timer: null };
  maintMsg(`Nalog je proveren. Preuzimam ${label} sa ${v.total} switch-eva (po 4 istovremeno)…`);
  renderMaint();
  renderMaintJob();
  maintJob.timer = setTimeout(maintJobTick, 1000);
}

async function maintJobTick() {
  const j = maintJob;
  if (!j) return;
  try {
    j.view = await api("GET", `/api/maintjobs/${j.id}`);
  } catch (e) {
    maintMsg(`Preuzimanje: ${e.message}`, "bad");
    maintJob = null;
    renderMaint();
    return;
  }
  renderMaintJob();
  if (!j.view.done) {
    j.timer = setTimeout(maintJobTick, 1000);
    return;
  }
  renderMaint();
  const v = j.view;
  const sum = `Preuzeto sa ${v.ok} od ${v.total} switch-eva` + (v.failed ? `, ${v.failed} sa greškom` : "") + (v.skipped ? `, ${v.skipped} preskočeno` : "") + ".";
  try {
    const r = await fetch(`/api/maintjobs/${j.id}/zip`, { headers: { "X-Requested-With": "fetch" } });
    if (r.status === 401) { location.reload(); return; }
    if (!r.ok) throw new Error((await r.json().catch(() => ({}))).error || `HTTP ${r.status}`);
    const name = await maintSave(r, "switch.zip");
    maintMsg(`${sum} Sačuvan je ${name} (fajl za svaki switch i izvestaj.txt).`, v.ok === v.total ? "ok" : "warn-text");
  } catch (e) {
    maintMsg(`${sum} ZIP nije preuzet: ${e.message}`, "bad");
  }
}

function renderMaintJob() {
  const box = $("maint-job");
  const v = maintJob && maintJob.view;
  box.hidden = !v;
  if (!v) return;
  const done = v.ok + v.failed + v.skipped;
  const rows = v.results.filter((r) => r.state !== "ok" || !v.done);
  box.replaceChildren(
    el("div", {}, el("strong", {}, `${v.action === "log" ? "Log" : "Backup"}: ${done} od ${v.total}`),
      v.done ? el("span", { class: "muted" }, " · završeno") : el("span", { class: "muted" }, " · u toku…"),
      v.done ? el("button", { type: "button", class: "linklike", onclick: () => { maintJob = null; renderMaintJob(); } }, " zatvori") : null),
    el("div", { class: "pbar" }, el("i", { style: `width:${v.total ? Math.round(done * 100 / v.total) : 0}%`, class: v.failed ? "bad" : "" })),
    v.stopped ? el("p", { class: "bad" }, v.stopped) : null,
    rows.length ? el("ul", { class: "nd-ev-list" }, ...rows.map((r) => {
      const [t, cls] = MAINT_STATE[r.state] || [r.state, ""];
      return el("li", {}, el("strong", {}, r.name), ` (${r.ip}) – `, el("span", { class: cls }, t), r.error ? el("span", { class: "small" }, `: ${r.error}`) : null);
    })) : null);
}

async function maintForgetKey(d) {
  if (!confirm(`SSH ključ switch-a ${d.name} se promenio od prve prijave. To je očekivano samo ako je switch zamenjen ili mu je ponovo generisan ključ.\n\nZaboraviti stari ključ? Sledeća prijava zapamtiće novi.`)) return;
  try {
    await api("DELETE", `/api/maint/${encodeURIComponent(d.id)}/hostkey`);
    maintMsg(`${d.name}: stari ključ je zaboravljen; pokrenite radnju ponovo.`, "ok");
  } catch (e) {
    maintMsg(`${d.name}: ${e.message}`, "bad");
  }
}

$("maint-search").addEventListener("input", renderMaint);
$("maint-all").addEventListener("change", (e) => {
  for (const d of maintVisible()) if (e.target.checked) maintSel.add(d.id); else maintSel.delete(d.id);
  renderMaint();
});
$("maint-bulk-log").addEventListener("click", () => maintBulk("log"));
$("maint-bulk-backup").addEventListener("click", () => maintBulk("backup"));
