"use strict";

document.getElementById("login").addEventListener("submit", async (ev) => {
  ev.preventDefault();
  const msg = document.getElementById("msg");
  msg.className = "muted";
  msg.textContent = "Prijavljivanje…";
  try {
    const r = await fetch("/api/login", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        user: document.getElementById("user").value.trim(),
        password: document.getElementById("pass").value,
      }),
    });
    const d = await r.json().catch(() => ({}));
    if (!r.ok) throw new Error(d.error || `HTTP ${r.status}`);
    location.replace("/");
  } catch (e) {
    msg.className = "bad";
    msg.textContent = e.message;
    document.getElementById("pass").select();
  }
});
