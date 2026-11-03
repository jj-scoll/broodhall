// memory UI. Vanilla JS, GET-only. Every row from the API is untrusted text and is
// rendered with textContent / text nodes only; no HTML is ever built from data.
"use strict";

const ROUTES = ["overview", "memory", "sessions", "work", "observer", "privacy"];
const TYPES = ["discovery", "change", "bugfix", "feature", "decision", "security", "refactor"];
const TITLES = {
  overview: "Overview", memory: "Memory", sessions: "Sessions",
  work: "Work", observer: "Observer", privacy: "Privacy",
};

const state = {
  route: "overview",
  health: null,
  offline: false,
  counters: null,
  gen: 0,          // bumped per render so stale async results are dropped
  pending: false,  // new data arrived while the user was reading
  mem: { q: "", project: "", types: new Set(), items: [], cursor: "", total: 0 },
};

// ---- helpers ----

function h(tag, attrs, ...kids) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v === false || v == null) continue;
    if (k === "text") el.textContent = v;
    else el.setAttribute(k, v === true ? "" : String(v));
  }
  for (const kid of kids.flat()) {
    if (kid == null || kid === false) continue;
    el.append(kid instanceof Node ? kid : document.createTextNode(String(kid)));
  }
  return el;
}

async function api(path) {
  const res = await fetch("/api/v1/" + path);
  if (!res.ok) throw new Error(path + ": " + res.status);
  return res.json();
}

const usd = (n) => "$" + (n || 0).toFixed(n >= 100 ? 0 : 2);
const num = (n) => (n || 0).toLocaleString();
function bytes(n) {
  const u = ["B", "KB", "MB", "GB"];
  let i = 0;
  while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
  return (i ? n.toFixed(1) : n) + " " + u[i];
}
function ago(iso) {
  if (!iso) return "";
  const s = Math.max(0, (Date.now() - new Date(iso).getTime()) / 1000);
  if (s < 90) return "just now";
  if (s < 5400) return Math.round(s / 60) + " min ago";
  if (s < 129600) return Math.round(s / 3600) + " h ago";
  return Math.round(s / 86400) + " d ago";
}
function dur(a, b) {
  const s = Math.max(0, (new Date(b) - new Date(a)) / 1000);
  return s < 90 ? Math.round(s) + " s" : s < 5400 ? Math.round(s / 60) + " min" : (s / 3600).toFixed(1) + " h";
}

function table(head, rows, numCols) {
  const nc = new Set(numCols || []);
  return h("div", { class: "tablewrap" }, h("table", {},
    h("thead", {}, h("tr", {}, head.map((t, i) => h("th", { scope: "col", class: nc.has(i) ? "num" : false, text: t })))),
    h("tbody", {}, rows.map((r) => h("tr", {}, r.map((c, i) => h("td", { class: nc.has(i) ? "num" : false, text: c })))))));
}

function card(label, big, note) {
  return h("div", { class: "card" }, h("div", { class: "label", text: label }), h("div", { class: "big", text: big }),
    note ? h("div", { class: "muted", text: note }) : null);
}

function setMain(...nodes) {
  const main = document.getElementById("main");
  main.replaceChildren(...nodes);
}

// ---- health pill, banner ----

function paintHealth() {
  const pill = document.getElementById("health-pill");
  const banner = document.getElementById("banner");
  let cls = "pill pill-wait", text = "Checking...", ban = null;
  if (state.offline) {
    cls = "pill pill-err"; text = "memory is not responding";
    ban = [h("strong", { text: "memory is not responding. " }), "This page cannot reach the daemon; it will reconnect on its own."];
  } else if (state.health) {
    const ps = state.health.problems || [];
    if (ps.length === 0) { cls = "pill pill-ok"; text = "All healthy"; }
    else {
      cls = "pill pill-err";
      text = ps.length + (ps.length === 1 ? " problem: " : " problems: ") + ps[0].text;
      ban = ps.flatMap((p) => [h("strong", { text: p.text + ". " }), p.fix ? h("span", {}, "To fix: ", h("code", { text: p.fix })) : null, " "]);
    }
  }
  pill.className = cls;
  pill.textContent = text;
  banner.replaceChildren(...(ban || []));
  banner.hidden = !ban;
}

async function refreshHealth() {
  try {
    state.health = await api("health");
    state.offline = false;
  } catch (e) {
    state.offline = true;
  }
  paintHealth();
}

// ---- views ----

const views = {
  async overview(gen) {
    const [today, d7, d30, stats, proj, recent, priv] = await Promise.all([
      api("cost?range=today"), api("cost?range=7d"), api("cost?range=30d&group=day"),
      api("stats?since=today"), api("cost?range=7d&group=project"),
      api("observations?limit=3"), api("privacy"),
    ]);
    if (gen !== state.gen) return;
    const perMem = stats.memories ? usd(today.usd / stats.memories) : "n/a";
    const kinds = d7.usd_by_kind || {};
    const kindNames = { input: "Input", cached_input: "Cached input", cache_write: "Cache write", output: "Output" };
    const order = Object.keys(kindNames);
    const total = order.reduce((s, k) => s + (kinds[k] || 0), 0) || 1;
    const bar = h("div", { class: "bar", role: "img", "aria-label": "Cost by kind, 7 days: " +
      order.map((k) => kindNames[k] + " " + usd(kinds[k])).join(", ") },
      order.map((k) => { const s = h("span"); s.style.width = ((kinds[k] || 0) / total * 100).toFixed(1) + "%"; return s; }));
    setMain(
      h("h1", { text: "Overview" }),
      h("div", { class: "grid" },
        card("Spent today (Opus-equivalent, estimated)", usd(today.usd), "7 days " + usd(d7.usd) + " / 30 days " + usd(d30.usd)),
        card("Memories written today", num(stats.memories), perMem + " per memory"),
        card("Model calls today", num(stats.model_calls), stats.duplicate_call_keys + " duplicate call keys (0 is healthy)"),
        card("Memory size", bytes(priv.db_bytes), num(stats.totals.observations) + " observations")),
      h("h2", { text: "Cost by kind, 7 days" }), bar,
      h("details", {}, h("summary", { text: "Show as table" }),
        table(["Kind", "USD", "Tokens"], order.map((k) => [kindNames[k], usd(kinds[k]), num((d7.tokens || {})[k])]), [1, 2])),
      h("h2", { text: "Cost by project, 7 days" }),
      proj.rows.length ? table(["Project", "Memories", "USD"], proj.rows.map((r) => [r.key, num(r.memories), usd(r.usd)]), [1, 2])
        : h("p", { class: "muted", text: "No usage recorded in this period." }),
      h("h2", { text: "Latest memories" }),
      h("ol", { class: "feed" }, recent.items.map(obsCard)));
  },

  async memory(gen) {
    const projects = (await api("projects")).items;
    if (gen !== state.gen) return;
    const m = state.mem;
    const search = h("input", { type: "search", id: "mem-q", "aria-label": "Search memories", placeholder: "Search memories", value: m.q });
    const sel = h("select", { id: "mem-project", "aria-label": "Project" }, h("option", { value: "", text: "All projects" }),
      projects.map((p) => h("option", { value: p.name, text: p.name + " (" + p.count + ")", selected: p.name === m.project })));
    const feed = h("ol", { class: "feed", id: "mem-feed" });
    const more = h("button", { type: "button", id: "mem-more", text: "Load 50 more", hidden: true });
    const status = h("p", { class: "muted", role: "status" });
    const typeBtns = TYPES.map((t) => {
      const b = h("button", { type: "button", "aria-pressed": m.types.has(t), text: t });
      b.addEventListener("click", () => {
        if (m.types.has(t)) m.types.delete(t); else m.types.add(t);
        b.setAttribute("aria-pressed", m.types.has(t));
        load(true);
      });
      return b;
    });
    const form = h("form", { class: "filters", role: "search" }, search, sel, h("button", { type: "submit", text: "Search" }));
    form.addEventListener("submit", (e) => { e.preventDefault(); m.q = search.value; m.project = sel.value; load(true); });
    sel.addEventListener("change", () => { m.project = sel.value; load(true); });
    const load = async (reset) => {
      if (reset) { m.items = []; m.cursor = ""; feed.replaceChildren(); }
      const qs = new URLSearchParams({ limit: "50" });
      if (m.q) qs.set("q", m.q);
      if (m.project) qs.set("project", m.project);
      if (m.types.size) qs.set("type", [...m.types].join(","));
      if (m.cursor) qs.set("cursor", m.cursor);
      try {
        const page = await api("observations?" + qs);
        if (gen !== state.gen) return;
        feed.append(...page.items.map(obsCard));
        m.cursor = page.next_cursor;
        m.total = page.total;
        more.hidden = !m.cursor;
        status.textContent = m.total + " matching memories";
      } catch (e) { status.textContent = "Could not load memories."; }
    };
    more.addEventListener("click", () => load(false));
    setMain(h("h1", { text: "Memory" }), form, h("div", { class: "filters", role: "group", "aria-label": "Types" }, typeBtns),
      status, feed, more);
    await load(true);
  },

  async sessions(gen) {
    const page = await api("sessions?limit=50");
    if (gen !== state.gen) return;
    setMain(h("h1", { text: "Sessions" }),
      page.items.length ? h("ol", { class: "feed" }, page.items.map(sessionCard)) : h("p", { class: "muted", text: "No sessions yet." }));
  },

  async work(gen) {
    const [ws, dl] = await Promise.all([api("work-state"), api("deliverables")]);
    if (gen !== state.gen) return;
    const lists = ws.lists.map((l) => h("section", {}, h("h2", { text: l.name + (l.project ? "  (" + l.project + ")" : "") }),
      h("ul", { class: "feed" }, l.items.map((it) => h("li", {}, workItem(l.name, it))))));
    setMain(h("h1", { text: "Work" }),
      lists.length ? lists : h("p", { class: "muted", text: "Nothing open. Lists written with work_state_write appear here." }),
      h("h2", { text: "Deliverables" }),
      dl.items.length ? table(["Title", "Location", "Project", "When"], dl.items.map((d) => [d.title, d.location, d.project, ago(d.at)]))
        : h("p", { class: "muted", text: "No deliverables recorded." }));
  },

  async observer(gen) {
    const o = await api("observer");
    if (gen !== state.gen) return;
    const q = o.queue;
    const age = q.oldest_queued_age_s ? Math.round(q.oldest_queued_age_s / 60) + " min" : "none waiting";
    setMain(h("h1", { text: "Observer" }),
      h("div", { class: "grid" },
        card("Sign-in", o.auth === "ok" ? "Signed in" : "Signed out", o.auth === "ok" ? "" : "Run claude, then /login"),
        card("Queued", num(q.queued), "oldest: " + age),
        card("Done", num(q.done), num(q.claimed) + " in flight"),
        card("Rejected", num(q.rejected), num(q.unknown) + " unknown (retrying)"),
        card("Duplicate call keys", num(o.dedupe.keys_with_multiple_calls), "0 is healthy")),
      h("h2", { text: "Rejection reasons" }),
      o.rejections.length ? table(["Reason", "Count"], o.rejections.map((r) => [r.reason, num(r.count)]), [1])
        : h("p", { class: "muted", text: "No rejections." }),
      o.rejected_items.length ? h("details", {}, h("summary", { text: "Latest rejected items" }),
        table(["When", "Tool", "Reason"], o.rejected_items.map((r) => [ago(r.at), r.tool, r.reason]))) : null,
      h("h2", { text: "Calls and cost by model, " + o.models_range }),
      o.models.length ? table(["Model", "Calls", "USD"], o.models.map((m) => [m.model, num(m.calls), usd(m.usd)]), [1, 2])
        : h("p", { class: "muted", text: "No model calls recorded." }));
  },

  async privacy(gen) {
    const p = await api("privacy");
    if (gen !== state.gen) return;
    setMain(h("h1", { text: "Privacy" }),
      h("div", { class: "card" }, h("dl", {},
        h("dt", { text: "Bind address" }), h("dd", { text: p.bind }),
        h("dt", { text: "Raw tool uses" }), h("dd", { text: p.raw_tool_uses_enabled ? "enabled" : "off: never sent to this page" }),
        h("dt", { text: "Database" }), h("dd", { text: p.db_path }),
        h("dt", { text: "Database size" }), h("dd", { text: bytes(p.db_bytes) }))),
      h("p", { class: "muted", text: "Stored text is masked for secrets before it is written. This page can only read." }));
  },
};

function workItem(list, it) {
  const keys = Object.keys(it).filter((k) => k !== "task" && k !== "status" && k !== "work_product");
  if (it.task === undefined) { // list-level state or a facts row: key / value pairs
    return h("dl", {}, Object.entries(it).flatMap(([k, v]) => [h("dt", { text: k }), h("dd", { text: String(v) })]));
  }
  return h("div", {}, h("div", { class: "row" }, h("span", { class: "badge", text: it.status || "todo" }), h("span", { class: "title", text: it.task })),
    it.work_product ? h("div", { class: "sub" }, "Work product: ", h("code", { text: String(it.work_product) })) : null,
    keys.length ? h("dl", {}, keys.flatMap((k) => [h("dt", { text: k }), h("dd", { text: String(it[k]) })])) : null);
}

function listOrNone(label, arr) {
  return arr && arr.length ? [h("dt", { text: label }), h("dd", {}, arr.map((x) => h("div", {}, h("code", { text: x }))))] : [];
}

function obsCard(o) {
  return h("li", {},
    h("div", { class: "row" }, h("span", { class: "badge", text: o.type || "unknown" }), h("span", { class: "muted", text: o.project }),
      h("span", { class: "muted", text: ago(o.created_at) })),
    h("div", { class: "title", text: o.title }),
    o.subtitle ? h("div", { class: "sub", text: o.subtitle }) : null,
    h("details", {}, h("summary", { text: "Details" }), h("dl", {},
      listOrNone("Facts", o.facts), listOrNone("Concepts", o.concepts),
      listOrNone("Files read", o.files_read), listOrNone("Files modified", o.files_modified),
      h("dt", { text: "Model" }), h("dd", { text: o.generated_by_model || "unknown" }),
      h("dt", { text: "Prompt version" }), h("dd", { text: o.prompt_version || "unknown" }),
      h("dt", { text: "Raw type" }), h("dd", { text: o.raw_type || o.type }))));
}

function sessionCard(s) {
  const sm = s.summary || {};
  const sections = [["Request", sm.request], ["Investigated", sm.investigated], ["Learned", sm.learned],
    ["Completed", sm.completed], ["Next steps", sm.next_steps], ["Notes", sm.notes]].filter(([, v]) => v);
  const body = h("div", {});
  const li = h("li", {},
    h("div", { class: "row" }, h("span", { class: "title", text: s.project || "(no project)" }),
      h("span", { class: "muted", text: ago(s.started_at) + ", " + dur(s.started_at, s.ended_at) }),
      h("span", { class: "muted", text: s.tool_uses + " tool uses, " + usd(s.usd) + ", " + s.prompt_count + " prompts" })),
    sections.length ? h("dl", {}, sections.flatMap(([k, v]) => [h("dt", { text: k }), h("dd", { text: v })]))
      : h("div", { class: "sub", text: "No summary yet." }),
    h("details", {}, h("summary", { text: "Prompts" }), body));
  li.querySelector("details").addEventListener("toggle", async (e) => {
    if (!e.target.open || body.childNodes.length) return;
    try {
      const d = await api("sessions/" + encodeURIComponent(s.id));
      body.replaceChildren(...(d.prompts.length ? d.prompts.map((p) => h("pre", { text: p.text })) : [h("span", { class: "muted", text: "No prompts." })]));
    } catch (err) { body.textContent = "Could not load prompts."; }
  });
  return li;
}

// ---- routing and rendering ----

function currentRoute() {
  const r = (location.hash.replace(/^#\/?/, "") || "overview").split("/")[0];
  return ROUTES.includes(r) ? r : "overview";
}

async function render(moveFocus) {
  state.route = currentRoute();
  state.pending = false;
  const gen = ++state.gen;
  for (const a of document.querySelectorAll("#nav a")) {
    if (a.dataset.route === state.route) a.setAttribute("aria-current", "page"); else a.removeAttribute("aria-current");
  }
  document.title = TITLES[state.route] + " - memory";
  try {
    await views[state.route](gen);
  } catch (e) {
    if (gen === state.gen) setMain(h("h1", { text: TITLES[state.route] }), h("p", { class: "muted", role: "status", text: "Could not load this view. It will retry when memory is reachable." }));
  }
  if (moveFocus && gen === state.gen) document.getElementById("main").focus();
}

// Re-render on new data unless the user is reading an expanded card; then offer a button.
function dataChanged() {
  const main = document.getElementById("main");
  if (main.querySelector("details[open]") || (state.route === "memory" && document.activeElement && document.activeElement.id === "mem-q")) {
    if (state.pending) return;
    state.pending = true;
    const b = h("button", { type: "button", class: "newbtn", text: "New data available. Refresh" });
    b.addEventListener("click", () => render(false));
    main.prepend(b);
    return;
  }
  render(false);
}

// ---- live updates ----

function connect() {
  if (typeof EventSource === "undefined") return;
  const es = new EventSource("/api/v1/events");
  es.addEventListener("open", async () => {
    const was = state.offline;
    await refreshHealth();
    if (was) render(false);
  });
  es.addEventListener("health", (e) => { state.health = JSON.parse(e.data); state.offline = false; paintHealth(); });
  es.addEventListener("counters", (e) => {
    state.counters = JSON.parse(e.data);
    if (state.route === "observer") dataChanged();
  });
  es.addEventListener("invalidate", (e) => {
    const v = JSON.parse(e.data).views || [];
    if (v.includes(state.route)) dataChanged();
  });
  es.addEventListener("error", () => { state.offline = true; paintHealth(); });
  // Fallback: if the stream is down, poll health every 30 s while the tab is visible.
  setInterval(() => {
    if (document.visibilityState === "visible" && es.readyState !== 1) refreshHealth();
  }, 30000);
}

// ---- theme ----

function applyTheme(t) {
  if (t === "light" || t === "dark") document.documentElement.setAttribute("data-theme", t);
  else document.documentElement.removeAttribute("data-theme");
  const btn = document.getElementById("theme-toggle");
  btn.textContent = "Theme: " + t;
  btn.setAttribute("aria-label", "Theme: " + t);
}

function initTheme() {
  let t = "system";
  try { t = localStorage.getItem("memory-theme") || "system"; } catch (e) { /* storage blocked */ }
  if (!["system", "light", "dark"].includes(t)) t = "system";
  applyTheme(t);
  document.getElementById("theme-toggle").addEventListener("click", () => {
    t = t === "system" ? "light" : t === "light" ? "dark" : "system";
    applyTheme(t);
    try { localStorage.setItem("memory-theme", t); } catch (e) { /* storage blocked */ }
  });
}

initTheme();
window.addEventListener("hashchange", () => render(true));
refreshHealth();
render(false);
connect();
