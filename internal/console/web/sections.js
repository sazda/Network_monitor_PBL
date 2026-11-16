"use strict";
// Skupljanje/širenje delova stranice (▾/▸) i prikaz preko celog ekrana (⤢).
// Koji su delovi skupljeni pamti pregledač (po korisniku računara).

const SEC_KEY = "netmon.collapsed";
const SECTIONS = ["net-locs", "netdevs-sec", "nd-events-sec", "srv-sec", "srv-events-sec", "agents-sec", "live", "params", "results-sec"];

function secLoad() {
  try {
    return new Set(JSON.parse(localStorage.getItem(SEC_KEY) || "[]"));
  } catch {
    return new Set();
  }
}

function secSave(set) {
  try {
    localStorage.setItem(SEC_KEY, JSON.stringify([...set]));
  } catch { /* bez trajnog pamćenja */ }
}

// Kratak sažetak u naslovu skupljenog dela, da se važno ne propusti.
const SEC_SUMMARY = {
  "net-locs": () => $("net-badge").hidden ? "sve lokacije rade" : $("net-badge").title,
  "netdevs-sec": () => [$("nd-count").textContent, $("net-badge").hidden ? "" : $("net-badge").title].filter(Boolean).join(" · "),
  "nd-events-sec": () => {
    const r = document.querySelector("#nd-events tbody tr");
    return r ? `poslednji: ${r.cells[0]?.textContent || ""} ${r.cells[2]?.textContent || ""}`.trim() : "";
  },
  "agents-sec": () => $("a-count").textContent,
  "srv-sec": () => $("srv-badge").hidden ? `${$("srv-count").textContent}, svi na vezi` : $("srv-badge").title,
  "srv-events-sec": () => {
    const r = document.querySelector("#srv-events tbody tr");
    return r && r.cells.length > 1 ? `poslednji: ${r.cells[0].textContent} ${r.cells[1].textContent}` : "";
  },
  "results-sec": () => $("r-count").textContent,
};

function secUpdateSummary(sec) {
  const s = sec.querySelector(":scope > h2 .sec-sum");
  if (!s) return;
  const f = SEC_SUMMARY[sec.id];
  s.textContent = sec.classList.contains("collapsed") && f ? f() : "";
}

function secToggleCollapse(sec, btn) {
  const set = secLoad();
  const on = !sec.classList.contains("collapsed");
  sec.classList.toggle("collapsed", on);
  btn.textContent = on ? "▸" : "▾";
  btn.title = on ? "Raširi" : "Skupi";
  btn.setAttribute("aria-expanded", on ? "false" : "true");
  if (on) set.add(sec.id); else set.delete(sec.id);
  secSave(set);
  secUpdateSummary(sec);
}

function secToggleMax(sec, btn) {
  const on = !sec.classList.contains("maximized");
  for (const other of document.querySelectorAll(".maximized")) {
    if (other !== sec) other.querySelector(".sec-max")?.click();
  }
  if (on && sec.classList.contains("collapsed")) sec.querySelector(".sec-collapse")?.click();
  sec.classList.toggle("maximized", on);
  document.body.classList.toggle("has-max", on);
  btn.textContent = on ? "⤡" : "⤢";
  btn.title = on ? "Vrati (Esc)" : "Preko celog ekrana";
  // Grafikoni se crtaju po širini: neka se prilagode.
  window.dispatchEvent(new Event("resize"));
}

// sectionTools pravi dugmad za jedan deo; collapse=false za delove koji
// imaju svoje dugme za zatvaranje (detalji uređaja).
function sectionTools(sec, collapse = true) {
  const box = el("span", { class: "sec-tools" });
  if (collapse) {
    const sum = el("span", { class: "sec-sum muted" });
    const c = el("button", { type: "button", class: "sec-btn sec-collapse", title: "Skupi", "aria-expanded": "true" }, "▾");
    c.addEventListener("click", () => secToggleCollapse(sec, c));
    box.append(sum, c);
  }
  const m = el("button", { type: "button", class: "sec-btn sec-max", title: "Preko celog ekrana" }, "⤢");
  m.addEventListener("click", () => secToggleMax(sec, m));
  box.append(m);
  return box;
}

function initSections() {
  const collapsed = secLoad();
  for (const id of SECTIONS) {
    const sec = $(id);
    const h2 = sec && sec.querySelector(":scope > h2");
    if (!h2 || h2.querySelector(".sec-tools")) continue;
    h2.append(sectionTools(sec));
    if (collapsed.has(id)) secToggleCollapse(sec, h2.querySelector(".sec-collapse"));
  }
  // Sažeci skupljenih delova se osvežavaju sa podacima.
  setInterval(() => {
    for (const id of SECTIONS) {
      const sec = $(id);
      if (sec && sec.classList.contains("collapsed")) secUpdateSummary(sec);
    }
  }, 3000);
  document.addEventListener("keydown", (ev) => {
    if (ev.key !== "Escape" || document.querySelector("dialog[open]")) return;
    document.querySelector(".maximized .sec-max")?.click();
  });
}

initSections();
