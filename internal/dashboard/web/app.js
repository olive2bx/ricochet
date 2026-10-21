"use strict";

(() => {
  const MAX_ROWS = 2000; // rows rendered at once
  const events = new Map(); // id -> event
  let paused = false;
  let pendingWhilePaused = 0;
  let selectedId = null;
  let sortKey = "id";
  let sortDir = -1; // -1 desc, 1 asc
  let renderQueued = false;

  const $ = (sel) => document.querySelector(sel);
  const tbody = $("#tbl tbody");
  const filters = {
    text: $("#f-text"),
    route: $("#f-route"),
    scheme: $("#f-scheme"),
    errors: $("#f-errors"),
    min: $("#f-min"),
  };

  // ---- helpers ----------------------------------------------------------

  function el(tag, attrs, ...children) {
    const e = document.createElement(tag);
    if (attrs) {
      for (const [k, v] of Object.entries(attrs)) {
        if (v === undefined || v === null || v === false) continue;
        if (k === "class") e.className = v;
        else if (k === "text") e.textContent = v;
        else if (k.startsWith("on")) e.addEventListener(k.slice(2), v);
        else e.setAttribute(k, v);
      }
    }
    for (const c of children) {
      if (c === null || c === undefined) continue;
      e.append(c instanceof Node ? c : document.createTextNode(String(c)));
    }
    return e;
  }

  function fmtMs(ms) {
    if (ms === undefined || ms === null) return "";
    if (ms < 1) return ms.toFixed(2) + " ms";
    if (ms < 1000) return ms.toFixed(ms < 10 ? 1 : 0) + " ms";
    if (ms < 60000) return (ms / 1000).toFixed(2) + " s";
    const m = Math.floor(ms / 60000);
    return m + " min " + Math.round((ms % 60000) / 1000) + " s";
  }

  function fmtBytes(n) {
    if (!n) return "0";
    const u = ["o", "Ko", "Mo", "Go"];
    let i = 0;
    while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
    return (i === 0 ? n : n.toFixed(1)) + " " + u[i];
  }

  function fmtTime(iso) {
    const d = new Date(iso);
    const p = (x, n = 2) => String(x).padStart(n, "0");
    return `${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}.${p(d.getMilliseconds(), 3)}`;
  }

  function routeKind(route) {
    if (!route) return "direct";
    if (route.startsWith("override:")) return "override";
    if (route.startsWith("upstream:")) return "upstream";
    return "direct";
  }

  function routeBadge(route) {
    const kind = routeKind(route);
    const label = kind === "override" ? "⇢ " + route.slice(9) : kind === "upstream" ? "⇡ amont" : "→ direct";
    return el("span", { class: "badge " + kind, title: route }, label);
  }

  function elapsed(e) {
    if (e.done) return e.total_ms;
    return Date.now() - new Date(e.started_at).getTime();
  }

  function target(e) {
    if (e.scheme === "https-tunnel") return "CONNECT :" + e.port;
    return (e.method || "") + " " + (e.path || "");
  }

  // ---- filtering & sorting ---------------------------------------------

  function matches(e) {
    const text = filters.text.value.trim().toLowerCase();
    if (text) {
      const hay = [e.host, e.path, e.method, e.route, e.scheme, e.status, e.err, e.client_addr]
        .join(" ").toLowerCase();
      if (!text.split(/\s+/).every((t) => hay.includes(t))) return false;
    }
    if (filters.route.value && routeKind(e.route) !== filters.route.value) return false;
    if (filters.scheme.value && e.scheme !== filters.scheme.value) return false;
    if (filters.errors.checked && !(e.err || e.status >= 400)) return false;
    const min = parseFloat(filters.min.value);
    if (!isNaN(min) && min > 0 && elapsed(e) < min) return false;
    return true;
  }

  function sortValue(e, key) {
    switch (key) {
      case "total_ms": return elapsed(e);
      case "started_at": return e.id;
      case "path": return target(e);
      case "bytes_down": return (e.bytes_down || 0) + (e.bytes_up || 0);
      case "status": return e.status || 0;
      default: return e[key] ?? "";
    }
  }

  function sorted(list) {
    return list.sort((a, b) => {
      const va = sortValue(a, sortKey), vb = sortValue(b, sortKey);
      if (va < vb) return -sortDir;
      if (va > vb) return sortDir;
      return b.id - a.id;
    });
  }

  // ---- rendering --------------------------------------------------------

  function scheduleRender() {
    if (renderQueued) return;
    renderQueued = true;
    requestAnimationFrame(() => { renderQueued = false; render(); });
  }

  function durClass(ms) {
    if (ms >= 3000) return "dur-vslow";
    if (ms >= 1000) return "dur-slow";
    return "";
  }

  function row(e) {
    const ms = elapsed(e);
    const cls = [];
    if (e.err || e.status >= 500) cls.push("error");
    if (!e.done) cls.push("pending");
    if (e.id === selectedId) cls.push("selected");
    const status = e.status ? el("span", { class: "status-" + String(e.status)[0] }, e.status) : (e.err ? "ERR" : "");
    return el("tr", { class: cls.join(" "), "data-id": e.id },
      el("td", { class: "num mono" }, e.id),
      el("td", { class: "mono" }, fmtTime(e.started_at)),
      el("td", null, el("span", { class: "badge scheme" }, e.scheme)),
      el("td", { title: e.host + ":" + e.port }, e.host),
      el("td", { class: "mono", title: target(e) }, target(e)),
      el("td", { class: "num" }, status),
      el("td", null, routeBadge(e.route)),
      el("td", { class: "num mono dur " + (e.done ? durClass(ms) : "") }, (e.done ? "" : "… ") + fmtMs(ms)),
      el("td", { class: "num mono" }, fmtBytes(e.bytes_up) + " / " + fmtBytes(e.bytes_down)),
    );
  }

  function render() {
    const all = [...events.values()];
    const list = sorted(all.filter(matches));
    const frag = document.createDocumentFragment();
    for (const e of list.slice(0, MAX_ROWS)) frag.append(row(e));
    tbody.replaceChildren(frag);
    $("#empty").classList.toggle("hidden", events.size > 0);
    let c = `${list.length} / ${all.length}`;
    if (list.length > MAX_ROWS) c += ` (${MAX_ROWS} affichées)`;
    if (paused && pendingWhilePaused) c += ` — ${pendingWhilePaused} en attente`;
    $("#count").textContent = c;
    document.querySelectorAll("thead th").forEach((th) => {
      th.classList.toggle("sorted-asc", th.dataset.key === sortKey && sortDir === 1);
      th.classList.toggle("sorted-desc", th.dataset.key === sortKey && sortDir === -1);
    });
    if (selectedId !== null) {
      const e = events.get(selectedId);
      if (e) renderDetail(e, false);
    }
  }

  // ---- detail panel -----------------------------------------------------

  const PHASES = [
    ["blocked", "Attente", "blocked_ms"],
    ["dns", "DNS", "dns_ms"],
    ["connect", "Connexion", "connect_ms"],
    ["tls", "TLS", "tls_ms"],
    ["ttfb", "TTFB", "ttfb_ms"],
  ];

  function waterfall(e) {
    const total = elapsed(e) || 0;
    const parts = PHASES.map(([cls, label, key]) => [cls, label, e[key] || 0]);
    const known = parts.reduce((s, p) => s + p[2], 0);
    const transferLabel = e.scheme === "https-tunnel" ? "Tunnel ouvert" : "Transfert";
    parts.push(["transfer", transferLabel, Math.max(0, total - known)]);
    const wrap = el("div", { class: "waterfall" });
    let offset = 0;
    for (const [cls, label, ms] of parts) {
      const bar = el("div", { class: "bar p-" + cls });
      if (total > 0) {
        bar.style.left = (offset / total * 100) + "%";
        bar.style.width = (ms / total * 100) + "%";
      }
      offset += ms;
      wrap.append(
        el("div", { class: "lbl" }, el("span", { class: "sw p-" + cls }), label),
        el("div", { class: "track" }, bar),
        el("div", { class: "val" }, fmtMs(ms)),
      );
    }
    wrap.append(el("div", { class: "lbl" }, el("b", null, "Total")), el("div"), el("div", { class: "val" }, el("b", null, fmtMs(total))));
    return wrap;
  }

  function kv(pairs) {
    const dl = el("dl", { class: "kv" });
    for (const [k, v] of pairs) {
      if (v === undefined || v === null || v === "") continue;
      dl.append(el("dt", null, k), el("dd", null, v));
    }
    return dl;
  }

  function headersTable(h) {
    const t = el("table", { class: "headers" });
    for (const name of Object.keys(h).sort()) {
      for (const v of h[name]) {
        t.append(el("tr", null, el("td", null, name), el("td", { class: v === "[masked]" ? "masked" : "" }, v)));
      }
    }
    return t;
  }

  function decodeBody(b64) {
    try {
      const bin = atob(b64);
      const bytes = Uint8Array.from(bin, (c) => c.charCodeAt(0));
      return new TextDecoder("utf-8", { fatal: false }).decode(bytes);
    } catch (_) {
      return "(corps illisible)";
    }
  }

  function bodySection(title, b64, truncated) {
    if (!b64) return [];
    return [
      el("h3", null, title + (truncated ? " (tronqué)" : "")),
      el("pre", { class: "body" }, decodeBody(b64)),
    ];
  }

  let detailBodies = null; // {id, req_body, resp_body, ...} fetched on demand

  async function loadBodies(id) {
    try {
      const r = await fetch("api/requests/" + id);
      if (!r.ok) return;
      const full = await r.json();
      detailBodies = full;
      if (selectedId === id) renderDetail(events.get(id) || full, false);
    } catch (_) { /* ignore */ }
  }

  function renderDetail(e, fetchBodies) {
    $("#detail").classList.remove("hidden");
    $("#d-title").textContent = e.scheme === "https-tunnel"
      ? `CONNECT ${e.host}:${e.port}`
      : `${e.method} ${e.scheme === "https-mitm" ? "https" : "http"}://${e.host}${(e.port === 80 || e.port === 443) ? "" : ":" + e.port}${e.path}`;
    const body = $("#d-body");
    const nodes = [];
    if (e.err) nodes.push(el("div", { class: "err-box" }, e.err));
    nodes.push(el("h3", null, "Timings" + (e.done ? "" : " (en cours)")));
    nodes.push(waterfall(e));
    if (e.reused) nodes.push(el("div", { class: "note" }, "Connexion keep-alive réutilisée : pas de DNS / connexion / TLS pour cette requête (--no-keepalive pour les mesurer à chaque fois)."));
    if (e.scheme === "https-tunnel") nodes.push(el("div", { class: "note" }, "Tunnel chiffré : TTFB = délai entre le premier octet du client et le premier octet du serveur. Le détail HTTP nécessite --mitm."));
    nodes.push(el("h3", null, "Général"));
    nodes.push(kv([
      ["ID", e.id],
      ["Début", new Date(e.started_at).toLocaleString()],
      ["Client", e.client_addr],
      ["Type", e.scheme],
      ["Destination", e.host + ":" + e.port],
      ["Statut", e.status || ""],
      ["Route", routeBadge(e.route)],
      ["Règle", e.rule_match ? el("code", null, e.rule_match) : ""],
      ["Envoyé", fmtBytes(e.bytes_up)],
      ["Reçu", fmtBytes(e.bytes_down)],
    ]));
    if (e.req_header && Object.keys(e.req_header).length) {
      nodes.push(el("h3", null, "Headers requête"), headersTable(e.req_header));
    }
    if (e.resp_header && Object.keys(e.resp_header).length) {
      nodes.push(el("h3", null, "Headers réponse"), headersTable(e.resp_header));
    }
    if (detailBodies && detailBodies.id === e.id) {
      nodes.push(...bodySection("Corps requête", detailBodies.req_body, detailBodies.req_truncated));
      nodes.push(...bodySection("Corps réponse", detailBodies.resp_body, detailBodies.resp_truncated));
    }
    body.replaceChildren(...nodes);
    if (fetchBodies && e.has_bodies) loadBodies(e.id);
  }

  function select(id) {
    selectedId = id;
    detailBodies = null;
    const e = events.get(id);
    if (e) renderDetail(e, true);
    scheduleRender();
  }

  function closeDetail() {
    selectedId = null;
    detailBodies = null;
    $("#detail").classList.add("hidden");
    scheduleRender();
  }

  // ---- data -------------------------------------------------------------

  function upsert(e) {
    const prev = events.get(e.id);
    events.set(e.id, e);
    if (selectedId === e.id && e.done && e.has_bodies && !(prev && prev.done)) loadBodies(e.id);
  }

  async function loadHistory() {
    try {
      const r = await fetch("api/requests");
      const list = await r.json();
      for (const e of list) if (!events.has(e.id) || events.get(e.id).done === false) upsert(e);
      scheduleRender();
    } catch (err) {
      console.error("history", err);
    }
  }

  async function loadInfo() {
    try {
      const info = await (await fetch("api/info")).json();
      const box = $("#info");
      box.replaceChildren();
      for (const [k, v] of Object.entries(info)) box.append(el("span", null, k + " ", el("b", null, v)));
    } catch (_) { /* ignore */ }
  }

  async function loadRules() {
    try {
      const st = await (await fetch("api/rules")).json();
      const box = $("#rules-status");
      const n = st.rules.length;
      let txt = st.path ? `${n} règle${n > 1 ? "s" : ""}` : "aucun fichier de règles";
      if (st.loaded_at) txt += " · chargées à " + fmtTime(st.loaded_at);
      box.classList.toggle("error", !!st.last_error);
      if (st.last_error) txt += " · ⚠ erreur de rechargement";
      box.textContent = txt;
      box.title = (st.path || "") + "\n" +
        st.rules.map((r) => r.text).join("\n") +
        (st.last_error ? "\n\nDernière erreur :\n" + st.last_error : "");
    } catch (_) { /* ignore */ }
  }

  function connect() {
    const es = new EventSource("events");
    es.onopen = () => {
      $("#conn").className = "conn on";
      loadHistory();
    };
    es.onerror = () => { $("#conn").className = "conn off"; };
    es.addEventListener("event", (m) => {
      const e = JSON.parse(m.data);
      if (paused && !events.has(e.id)) {
        pendingWhilePaused++;
        queue.push(e);
        scheduleRender();
        return;
      }
      upsert(e);
      if (!paused) scheduleRender();
    });
    es.addEventListener("clear", () => {
      events.clear();
      queue.length = 0;
      pendingWhilePaused = 0;
      closeDetail();
    });
  }

  const queue = []; // new events received while paused

  // ---- UI wiring --------------------------------------------------------

  tbody.addEventListener("click", (ev) => {
    const tr = ev.target.closest("tr");
    if (tr) select(Number(tr.dataset.id));
  });
  $("#d-close").addEventListener("click", closeDetail);
  document.addEventListener("keydown", (ev) => {
    if (ev.key === "Escape") closeDetail();
  });
  document.querySelectorAll("thead th").forEach((th) => {
    th.addEventListener("click", () => {
      const key = th.dataset.key;
      if (sortKey === key) sortDir = -sortDir;
      else { sortKey = key; sortDir = key === "host" || key === "path" || key === "route" || key === "scheme" ? 1 : -1; }
      scheduleRender();
    });
  });
  for (const f of Object.values(filters)) {
    f.addEventListener("input", scheduleRender);
    f.addEventListener("change", scheduleRender);
  }
  $("#b-pause").addEventListener("click", (ev) => {
    paused = !paused;
    ev.currentTarget.classList.toggle("active", paused);
    ev.currentTarget.textContent = paused ? "▶ Reprendre" : "⏸ Pause";
    if (!paused) {
      for (const e of queue) upsert(e);
      queue.length = 0;
      pendingWhilePaused = 0;
    }
    scheduleRender();
  });
  $("#b-clear").addEventListener("click", async () => {
    if (!confirm("Vider l'historique des requêtes ?")) return;
    await fetch("api/clear", { method: "POST" });
  });

  // Refresh in-progress durations.
  setInterval(() => {
    if (paused) return;
    for (const e of events.values()) if (!e.done) { scheduleRender(); break; }
  }, 500);

  loadInfo();
  loadRules();
  setInterval(loadRules, 3000);
  connect();
})();
