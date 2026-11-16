"use strict";

document.getElementById("setup").addEventListener("submit", async (ev) => {
  ev.preventDefault();
  const msg = document.getElementById("msg");
  const user = document.getElementById("user").value.trim();
  const pass = document.getElementById("pass").value;
  if (pass !== document.getElementById("pass2").value) {
    msg.className = "bad";
    msg.textContent = "Lozinke se ne poklapaju.";
    return;
  }
  try {
    const r = await fetch("/api/setup", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ user, password: pass }),
    });
    const d = await r.json().catch(() => ({}));
    if (!r.ok) throw new Error(d.error || `HTTP ${r.status}`);
    msg.className = "ok";
    msg.textContent = "Lozinka je postavljena. Otvaram konzolu…";
    setTimeout(() => location.replace("/"), 1200);
  } catch (e) {
    msg.className = "bad";
    msg.textContent = e.message;
  }
});
