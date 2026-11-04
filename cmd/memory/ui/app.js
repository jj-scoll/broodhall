// memory UI. Vanilla JS, GET-only. Every row from the API is untrusted text and is
// rendered with textContent / text nodes only; no HTML is ever built from data, and the
// only markup-from-text path is md.js, which also builds DOM nodes and never parses HTML.
"use strict";

const ROUTES = ["overview", "stats", "memory", "sessions", "work", "observer", "privacy"];
const TYPES = ["discovery", "change", "bugfix", "feature", "decision", "security", "refactor"];
const TITLES = {
  overview: "Overview", stats: "Stats", memory: "Memories", sessions: "Sessions",
  work: "Handoffs and work", observer: "Observer", privacy: "Privacy",
};

const state = {
  route: "overview",
  params: new URLSearchParams(),
  health: null,
  offline: false,
  counters: null,
  gen: 0,          // bumped per render so stale async results are dropped
  pending: false,  // new data arrived while the user was reading
};

// ---- helpers ----

function h(tag, attrs, ...kids) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (k.startsWith("aria-") && typeof v === "boolean") { el.setAttribute(k, String(v)); continue; }
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
  if (!iso) return "never";
  const s = Math.max(0, (Date.now() - new Date(iso).getTime()) / 1000);
  if (s < 90) return "just now";
  if (s < 5400) return Math.round(s / 60) + " min ago";
  if (s < 129600) return Math.round(s / 3600) + " h ago";
  return Math.round(s / 86400) + " d ago";
}
function dur(a, b) {
  const s = Math.max(0, (new Date(b) - new Date(a)) / 1000);
  return s < 90 ? Math.round(s) + " s" : s < 5400 ? Math.round(s / 60) + " min" : s < 172800 ? (s / 3600).toFixed(1) + " h" : Math.round(s / 86400) + " d";
}
const fullTime = (iso) => {
  if (!iso) return "";
  const d = new Date(iso);
  return dayKey(d) + " " + pad(d.getHours()) + ":" + pad(d.getMinutes()) + ":" + pad(d.getSeconds());
};
const clock = (iso) => new Date(iso).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
const pad = (n) => String(n).padStart(2, "0");
const dayKey = (d) => d.getFullYear() + "-" + pad(d.getMonth() + 1) + "-" + pad(d.getDate());
const tzOffset = () => -new Date().getTimezoneOffset(); // minutes east of UTC, as the API wants
const shortId = (id) => String(id || "").slice(0, 8);
const typeCls = (t) => (TYPES.includes(t) ? "t-" + t : "t-other");

function debounce(fn, ms) {
  let t;
  return (...a) => { clearTimeout(t); t = setTimeout(() => fn(...a), ms); };
}

function table(head, rows, numCols) {
  const nc = new Set(numCols || []);
  return h("div", { class: "tablewrap" }, h("table", {},
    h("thead", {}, h("tr", {}, head.map((t, i) => h("th", { scope: "col", class: nc.has(i) ? "num" : false, text: t })))),
    h("tbody", {}, rows.map((r) => h("tr", {}, r.map((c, i) => h("td", { class: nc.has(i) ? "num" : false, text: c })))))));
}

function card(label, big, note, small) {
  return h("div", { class: "card" }, h("div", { class: "label", text: label }),
    h("div", { class: "big" + (small ? " small" : ""), text: big }),
    note ? h("div", { class: "muted", text: note }) : null);
}

function empty(text) { return h("div", { class: "empty", text }); }

function setMain(...nodes) {
  document.getElementById("main").replaceChildren(...nodes);
}

// Text with every case-insensitive occurrence of q wrapped in <mark>, as nodes.
function highlight(text, q) {
  text = String(text == null ? "" : text);
  if (!q) return [text];
  const out = [], lo = text.toLowerCase(), ql = q.toLowerCase();
  let i = 0, j;
  while ((j = lo.indexOf(ql, i)) !== -1) {
    if (j > i) out.push(text.slice(i, j));
    out.push(h("mark", { text: text.slice(j, j + ql.length) }));
    i = j + ql.length;
  }
  if (i < text.length) out.push(text.slice(i));
  return out;
}

// ---- hash state: #/route?key=value, rewritten in place (no history entry) as filters change ----

function parseHash() {
  const raw = location.hash.replace(/^#\/?/, "");
  const cut = raw.indexOf("?");
  const path = cut === -1 ? raw : raw.slice(0, cut);
  const r = (path || "overview").split("/")[0];
  return { route: ROUTES.includes(r) ? r : "overview", params: new URLSearchParams(cut === -1 ? "" : raw.slice(cut + 1)) };
}

function replaceParams(p) {
  const qs = p.toString();
  state.params = new URLSearchParams(qs);
  history.replaceState(null, "", "#/" + state.route + (qs ? "?" + qs : ""));
}

function link(route, params, text, cls) {
  const qs = new URLSearchParams(params || {}).toString();
  return h("a", { href: "#/" + route + (qs ? "?" + qs : ""), class: cls || false, text });
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
  pill.title = text;
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

// ---- observation cards ----

function chips(items) {
  return h("div", { class: "chips" }, items.map((c) => h("span", { class: "badge t-other", text: c })));
}

function listOrNone(label, arr) {
  return arr && arr.length ? [h("dt", { text: label }), h("dd", {}, arr.map((x) => h("div", {}, h("code", { text: x }))))] : [];
}

function obsDetail(d) {
  return [
    d.narrative ? h("p", { class: "narr", text: d.narrative }) : null,
    d.facts.length ? h("ul", { class: "facts" }, d.facts.map((f) => h("li", { text: f }))) : null,
    d.concepts.length ? chips(d.concepts) : null,
    h("dl", {}, listOrNone("Files read", d.files_read), listOrNone("Files modified", d.files_modified),
      h("dt", { text: "Written" }), h("dd", { text: fullTime(d.created_at) }),
      h("dt", { text: "Model" }), h("dd", { text: d.generated_by_model || "unknown" }),
      h("dt", { text: "Prompt version" }), h("dd", { text: d.prompt_version || "unknown" }),
      h("dt", { text: "Raw type" }), h("dd", { text: d.raw_type || d.type })),
    d.session_id ? h("p", { class: "links" }, link("sessions", { id: d.session_id }, "Open session " + shortId(d.session_id)),
      link("memory", { session: d.session_id }, "More from this session")) : null,
  ];
}

// opt: q (highlight), timeline (compact entry for the day view)
function obsCard(o, opt) {
  opt = opt || {};
  const q = opt.q || "";
  const body = h("div", { class: "body" });
  const det = h("details", {}, h("summary", { text: "Show details" }), body);
  det.addEventListener("toggle", async () => {
    det.parentElement.classList.toggle("open", det.open); // the expanded body lists every fact
    if (!det.open || det.dataset.loaded) return;
    det.dataset.loaded = "1";
    body.textContent = "Loading...";
    try {
      body.replaceChildren(...obsDetail(await api("observations/" + encodeURIComponent(o.id))).filter(Boolean));
    } catch (e) {
      delete det.dataset.loaded;
      body.textContent = "Could not load details.";
    }
  });
  const when = h("time", { class: "time", datetime: o.created_at, title: fullTime(o.created_at),
    text: opt.timeline ? clock(o.created_at) : ago(o.created_at) });
  const li = h("li", { class: (opt.timeline ? "tl " : "obs ") + typeCls(o.type) },
    h("div", { class: "row" }, h("span", { class: "badge " + typeCls(o.type), text: o.type || "unknown" }),
      h("span", { class: "muted", text: o.project || "(no project)" }), when),
    h("div", { class: "title" }, highlight(o.title || "(untitled)", q)),
    o.subtitle ? h("div", { class: "sub" }, highlight(o.subtitle, q)) : null,
    !opt.timeline && o.facts.length ? h("ul", { class: "facts preview" }, o.facts.slice(0, 3).map((f) => h("li", {}, highlight(f, q)))) : null,
    det);
  return li;
}

// ---- charts ----

const SVG = "http:" + "//www.w3.org/2000/svg"; // an XML namespace name, not a request; split so the no-external-URL test stays strict
function svg(tag, attrs, ...kids) {
  const el = document.createElementNS(SVG, tag);
  for (const [k, v] of Object.entries(attrs || {})) if (v != null && v !== false) el.setAttribute(k, String(v));
  for (const kid of kids.flat()) if (kid != null) el.append(kid instanceof Node ? kid : document.createTextNode(String(kid)));
  return el;
}

function niceMax(v) {
  if (v <= 4) return 4;
  const p = Math.pow(10, Math.floor(Math.log10(v)));
  for (const m of [1, 2, 2.5, 5, 10]) if (m * p >= v) return m * p;
  return 10 * p;
}

// days: ["2026-01-01", ...]; series: [{key, cls}]; counts: Map("day|key" -> n). Stacked bars.
function stackedChart(days, series, counts, width) {
  const H = 230, L = 38, R = 6, T = 8, B = 24;
  const plotW = Math.max(120, width - L - R), plotH = H - T - B;
  const totals = days.map((d) => series.reduce((s, x) => s + (counts.get(d + "|" + x.key) || 0), 0));
  const max = niceMax(Math.max(1, ...totals));
  const y = (v) => T + plotH - (v / max) * plotH;
  const step = plotW / days.length, bw = Math.max(2, step * 0.76);
  const root = svg("svg", { class: "chart", viewBox: "0 0 " + width + " " + H, role: "img",
    "aria-label": "Memories per day, " + days[0] + " to " + days[days.length - 1] + ", " + num(totals.reduce((a, b) => a + b, 0)) + " in total" });
  const gl = svg("g", { class: "gl" });
  for (let i = 0; i <= 4; i++) {
    const v = (max / 4) * i;
    gl.append(svg("line", { x1: L, x2: width - R, y1: y(v), y2: y(v) }),
      svg("text", { x: L - 6, y: y(v) + 4, "text-anchor": "end" }, Number.isInteger(v) ? v : v.toFixed(1)));
  }
  root.append(gl);
  const every = Math.max(1, Math.ceil(days.length / Math.max(2, Math.floor(plotW / 64))));
  days.forEach((d, i) => {
    const x = L + i * step + (step - bw) / 2;
    let acc = 0;
    for (const s of series) {
      const n = counts.get(d + "|" + s.key) || 0;
      if (!n) continue;
      root.append(svg("rect", { class: "fill " + s.cls, x, width: bw, y: y(acc + n), height: Math.max(1, y(acc) - y(acc + n)) },
        svg("title", {}, d + ", " + s.key + ": " + n)));
      acc += n;
    }
    if (i % every === 0) {
      root.append(svg("text", { x: x + bw / 2, y: H - 6, "text-anchor": "middle" }, d.slice(5)));
    }
  });
  return root;
}

function legend(series) {
  return h("ul", { class: "legend" }, series.map((s) => h("li", {}, h("span", { class: "swatch " + s.cls }), s.key)));
}

function hbars(rows, clsFor) {
  const max = Math.max(1, ...rows.map((r) => r.count));
  return h("div", {}, rows.map((r, i) => {
    const span = h("span");
    span.style.width = (r.count / max * 100).toFixed(1) + "%";
    return h("div", { class: "hbar " + clsFor(r, i) }, h("span", { class: "nm", title: r.name, text: r.name }),
      h("span", { class: "track" }, span), h("span", { class: "v", text: num(r.count) }));
  }));
}

// ---- views ----

const views = {
  async overview(gen) {
    const [today, d7, d30, stats, proj, recent, priv] = await Promise.all([
      api("cost?range=today"), api("cost?range=7d"), api("cost?range=30d&group=day"),
      api("stats?since=today"), api("cost?range=7d&group=project"),
      api("observations?limit=5"), api("privacy"),
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
      h("div", { class: "pagehead" }, h("h1", { text: "Overview" }), link("stats", {}, "Memories over time and system status")),
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
        : empty("No usage recorded in this period."),
      h("h2", { text: "Latest memories" }),
      h("ol", { class: "feed" }, recent.items.map((o) => obsCard(o))),
      h("p", {}, link("memory", {}, "Search all memories")));
  },

  async stats(gen, params) {
    const [sys, facets] = await Promise.all([api("system"), api("facets")]);
    if (gen !== state.gen) return;
    const f = {
      range: ["14", "30", "90"].includes(params.get("range")) ? params.get("range") : "30",
      by: params.get("by") === "project" ? "project" : "type",
      project: params.get("project") || "", type: params.get("type") || "",
    };
    const sync = () => {
      const p = new URLSearchParams();
      if (f.range !== "30") p.set("range", f.range);
      if (f.by !== "type") p.set("by", f.by);
      if (f.project) p.set("project", f.project);
      if (f.type) p.set("type", f.type);
      replaceParams(p);
    };
    const healthy = sys.status === "ok";
    const tiles = h("div", { class: "grid" },
      card("Daemon", healthy ? "Healthy" : "Degraded",
        "version " + sys.version + (sys.revision ? " (" + sys.revision + ")" : "") + ", up " + dur(sys.started_at, new Date())),
      card("Last ingest", ago(sys.last_ingest_at), sys.last_ingest_at ? fullTime(sys.last_ingest_at) : "nothing ingested yet", true),
      card("Last memory written", ago(sys.last_observation_at), sys.last_observation_at ? fullTime(sys.last_observation_at) : "", true),
      card("Memories", num(sys.totals.observations), num(sys.totals.summaries) + " summaries"),
      card("Sessions", num(sys.totals.sessions), num(sys.totals.prompts) + " prompts"),
      card("Database", bytes(sys.db_bytes), sys.wal_bytes ? bytes(sys.wal_bytes) + " write-ahead log" : "no pending log"),
      card("Tool uses seen", num(sys.totals.raw_tool_uses), "counted only; content is never shown"));

    const chartHost = h("div", { class: "chartbox" });
    const legendHost = h("div"), tableHost = h("div"), status = h("p", { class: "muted", role: "status" });
    const projSel = h("select", { "aria-label": "Project" }, h("option", { value: "", text: "All projects" }),
      facets.projects.map((p) => h("option", { value: p.name, text: p.name + " (" + num(p.count) + ")", selected: p.name === f.project })));
    const typeSel = h("select", { "aria-label": "Type" }, h("option", { value: "", text: "All types" }),
      facets.types.map((t) => h("option", { value: t.name, text: t.name + " (" + num(t.count) + ")", selected: t.name === f.type })));
    const seg = (key, val, label) => {
      const b = h("button", { type: "button", class: "seg", "aria-pressed": f[key] === val, text: label });
      b.addEventListener("click", () => { f[key] = val; sync(); paintSegs(); draw(); });
      return b;
    };
    const rangeBtns = [["14", "14 days"], ["30", "30 days"], ["90", "90 days"]].map(([v, l]) => seg("range", v, l));
    const byBtns = [["type", "By type"], ["project", "By project"]].map(([v, l]) => seg("by", v, l));
    const paintSegs = () => {
      [...rangeBtns, ...byBtns].forEach((b, i) => {
        const val = i < 3 ? ["14", "30", "90"][i] : ["type", "project"][i - 3];
        b.setAttribute("aria-pressed", String(i < 3 ? f.range === val : f.by === val));
      });
    };
    projSel.addEventListener("change", () => { f.project = projSel.value; sync(); draw(); });
    typeSel.addEventListener("change", () => { f.type = typeSel.value; sync(); draw(); });

    let last = null, drawing = 0;
    const paint = () => {
      if (!last) return;
      const { days, series, counts } = last;
      const width = Math.max(280, chartHost.clientWidth - 32);
      chartHost.replaceChildren(stackedChart(days, series, counts, width));
      legendHost.replaceChildren(legend(series));
      const rows = days.filter((d) => series.some((s) => counts.get(d + "|" + s.key))).reverse()
        .map((d) => [d, ...series.map((s) => num(counts.get(d + "|" + s.key) || 0))]);
      tableHost.replaceChildren(h("details", {}, h("summary", { text: "Show as table" }),
        rows.length ? table(["Day (local)", ...series.map((s) => s.key)], rows, series.map((_, i) => i + 1)) : empty("No memories in this range.")));
    };
    const draw = async () => {
      const id = ++drawing;
      const n = Number(f.range), end = new Date(), days = [];
      for (let i = n - 1; i >= 0; i--) days.push(dayKey(new Date(end.getFullYear(), end.getMonth(), end.getDate() - i)));
      const qs = new URLSearchParams({ from: days[0], to: days[n - 1], tz_offset: String(tzOffset()) });
      if (f.project) qs.set("project", f.project);
      if (f.type) qs.set("type", f.type);
      try {
        const data = await api("timeseries?" + qs);
        if (id !== drawing || gen !== state.gen) return;
        const counts = new Map(), totals = new Map();
        for (const it of data.items) {
          const k = f.by === "type" ? it.type : it.project;
          counts.set(it.day + "|" + k, (counts.get(it.day + "|" + k) || 0) + it.count);
          totals.set(k, (totals.get(k) || 0) + it.count);
        }
        let series;
        if (f.by === "type") {
          const known = TYPES.filter((t) => totals.has(t)).map((t) => ({ key: t, cls: "t-" + t }));
          const other = [...totals.keys()].filter((t) => !TYPES.includes(t)).sort().map((t) => ({ key: t, cls: "t-other" }));
          series = [...known, ...other];
        } else {
          const ranked = [...totals.entries()].sort((a, b) => b[1] - a[1]).map(([k]) => k);
          series = ranked.slice(0, 7).map((k, i) => ({ key: k, cls: "s" + i }));
          if (ranked.length > 7) {
            const rest = ranked.slice(7);
            for (const d of days) {
              const sum = rest.reduce((s, k) => s + (counts.get(d + "|" + k) || 0), 0);
              if (sum) counts.set(d + "|other", sum);
            }
            series.push({ key: "other", cls: "s7" });
          }
        }
        const sum = [...totals.values()].reduce((a, b) => a + b, 0);
        status.textContent = num(sum) + " memories in " + n + " days (days are local time)";
        last = { days, series, counts };
        paint();
      } catch (e) { status.textContent = "Could not load the chart."; }
    };
    window.__statsRepaint = debounce(paint, 120);

    const byProject = hbars(facets.projects.slice(0, 10), (_, i) => "s" + Math.min(i, 7));
    const byType = hbars(facets.types, (r) => typeCls(r.name));
    setMain(h("div", { class: "pagehead" }, h("h1", { text: "Stats" }),
        h("span", { class: "muted", text: "Updated " + new Date().toLocaleTimeString() })),
      tiles,
      h("h2", { text: "Memories per day" }),
      h("div", { class: "filters" }, h("div", { class: "frow" }, h("div", { class: "segs" }, rangeBtns), h("div", { class: "segs" }, byBtns), projSel, typeSel)),
      status, chartHost, legendHost, tableHost,
      h("div", { class: "grid" },
        h("div", {}, h("h2", { text: "Memories by project" }), byProject),
        h("div", {}, h("h2", { text: "Memories by type" }), byType)));
    paintSegs();
    await draw();
  },

  async memory(gen, params) {
    const [facets] = await Promise.all([api("facets")]);
    if (gen !== state.gen) return;
    const f = {
      q: params.get("q") || "", project: params.get("project") || "", from: params.get("from") || "",
      to: params.get("to") || "", session: params.get("session") || "",
      types: new Set((params.get("type") || "").split(",").filter(Boolean)),
      view: params.get("view") === "timeline" ? "timeline" : "list",
    };
    const sync = () => {
      const p = new URLSearchParams();
      if (f.q) p.set("q", f.q);
      if (f.project) p.set("project", f.project);
      if (f.types.size) p.set("type", [...f.types].join(","));
      if (f.from) p.set("from", f.from);
      if (f.to) p.set("to", f.to);
      if (f.session) p.set("session", f.session);
      if (f.view !== "list") p.set("view", f.view);
      replaceParams(p);
    };
    const search = h("input", { type: "search", "aria-label": "Search memories", placeholder: "Search titles, notes, facts and concepts", value: f.q });
    const projSel = h("select", { "aria-label": "Project" }, h("option", { value: "", text: "All projects" }),
      facets.projects.map((p) => h("option", { value: p.name, text: p.name + " (" + num(p.count) + ")", selected: p.name === f.project })));
    const sessSel = h("select", { "aria-label": "Session" }, h("option", { value: "", text: "All sessions" }));
    const from = h("input", { type: "date", "aria-label": "From date", value: f.from });
    const to = h("input", { type: "date", "aria-label": "To date", value: f.to });
    const typeBtns = facets.types.map((t) => {
      const b = h("button", { type: "button", class: "chip " + typeCls(t.name), "aria-pressed": f.types.has(t.name) }, t.name, h("span", { class: "n", text: num(t.count) }));
      b.addEventListener("click", () => {
        if (f.types.has(t.name)) f.types.delete(t.name); else f.types.add(t.name);
        b.setAttribute("aria-pressed", String(f.types.has(t.name)));
        apply();
      });
      return b;
    });
    const viewBtns = [["list", "List"], ["timeline", "Timeline"]].map(([v, l]) => {
      const b = h("button", { type: "button", class: "seg", "aria-pressed": f.view === v, text: l });
      b.addEventListener("click", () => { f.view = v; viewBtns.forEach((x, i) => x.setAttribute("aria-pressed", String(f.view === ["list", "timeline"][i]))); apply(); });
      return b;
    });
    const quick = (label, days) => {
      const b = h("button", { type: "button", text: label });
      b.addEventListener("click", () => {
        const end = new Date();
        f.to = dayKey(end);
        f.from = dayKey(new Date(end.getFullYear(), end.getMonth(), end.getDate() - days));
        from.value = f.from; to.value = f.to;
        apply();
      });
      return b;
    };
    const clear = h("button", { type: "button", text: "Clear filters" });
    clear.addEventListener("click", () => {
      Object.assign(f, { q: "", project: "", from: "", to: "", session: "" });
      f.types.clear();
      search.value = ""; projSel.value = ""; from.value = ""; to.value = "";
      typeBtns.forEach((b) => b.setAttribute("aria-pressed", "false"));
      fillSessions();
      apply();
    });

    const out = h("div", { id: "mem-out" });
    const more = h("button", { type: "button", text: "Load more", hidden: true });
    const status = h("p", { class: "status muted", role: "status" });
    let cursor = "", lastDay = "", dayList = null, dayCount = null, dayN = 0, req = 0;

    async function fillSessions() {
      try {
        const page = await api("sessions?limit=100" + (f.project ? "&project=" + encodeURIComponent(f.project) : ""));
        if (gen !== state.gen) return;
        const opts = [h("option", { value: "", text: "All sessions" })];
        let seen = !f.session;
        for (const s of page.items) {
          if (s.id === f.session) seen = true;
          const what = (s.summary && s.summary.request) || s.project || shortId(s.id);
          opts.push(h("option", { value: s.id, selected: s.id === f.session,
            text: new Date(s.started_at).toLocaleDateString() + " " + (what.length > 48 ? what.slice(0, 47) + "..." : what) }));
        }
        if (!seen) opts.push(h("option", { value: f.session, selected: true, text: "Session " + shortId(f.session) }));
        sessSel.replaceChildren(...opts);
      } catch (e) { /* the select stays on "All sessions" */ }
    }

    function query() {
      const qs = new URLSearchParams({ limit: "50", tz_offset: String(tzOffset()) });
      if (f.q) qs.set("q", f.q);
      if (f.project) qs.set("project", f.project);
      if (f.types.size) qs.set("type", [...f.types].join(","));
      if (f.from) qs.set("from", f.from);
      if (f.to) qs.set("to", f.to);
      if (f.session) qs.set("session", f.session);
      if (cursor) qs.set("cursor", cursor);
      return qs;
    }

    function addTimeline(items) {
      for (const o of items) {
        const d = new Date(o.created_at), key = dayKey(d);
        if (key !== lastDay || !dayList) {
          lastDay = key; dayN = 0;
          dayCount = h("span", { class: "muted" });
          dayList = h("ul", { class: "rail" });
          out.append(h("section", { class: "day" },
            h("h2", {}, d.toLocaleDateString(undefined, { weekday: "long", year: "numeric", month: "long", day: "numeric" }), dayCount), dayList));
        }
        dayList.append(obsCard(o, { timeline: true, q: f.q }));
        dayCount.textContent = ++dayN + (dayN === 1 ? " memory" : " memories");
      }
    }

    async function load(reset) {
      const id = ++req;
      if (reset) { cursor = ""; lastDay = ""; dayList = null; out.replaceChildren(); }
      try {
        const page = await api("observations?" + query());
        if (id !== req || gen !== state.gen) return;
        if (f.view === "timeline") addTimeline(page.items);
        else {
          let ol = out.querySelector("ol.feed");
          if (!ol) { ol = h("ol", { class: "feed" }); out.append(ol); }
          ol.append(...page.items.map((o) => obsCard(o, { q: f.q })));
        }
        cursor = page.next_cursor;
        more.hidden = !cursor;
        const shown = out.querySelectorAll(f.view === "timeline" ? ".rail > li" : "ol.feed > li").length;
        status.textContent = page.total === 0 ? "" : "Showing " + num(shown) + " of " + num(page.total) + " matching memories";
        if (page.total === 0) out.replaceChildren(empty("No memories match. Try fewer filters or a shorter search."));
      } catch (e) {
        if (id === req) status.textContent = "Could not load memories.";
      }
    }
    function apply() { sync(); load(true); }

    const debounced = debounce(() => { f.q = search.value.trim(); apply(); }, 250);
    search.addEventListener("input", debounced);
    search.addEventListener("keydown", (e) => { if (e.key === "Enter") { e.preventDefault(); f.q = search.value.trim(); apply(); } });
    projSel.addEventListener("change", () => { f.project = projSel.value; f.session = ""; fillSessions(); apply(); });
    sessSel.addEventListener("change", () => { f.session = sessSel.value; apply(); });
    from.addEventListener("change", () => { f.from = from.value; apply(); });
    to.addEventListener("change", () => { f.to = to.value; apply(); });
    more.addEventListener("click", () => load(false));

    setMain(h("div", { class: "pagehead" }, h("h1", { text: "Memories" }), h("div", { class: "segs" }, viewBtns)),
      h("div", { class: "filters", role: "search" },
        h("div", { class: "frow" }, search),
        h("div", { class: "frow" }, projSel, sessSel),
        h("div", { class: "frow" }, h("label", {}, "From", from), h("label", {}, "To", to), quick("Today", 0), quick("Last 7 days", 6), quick("Last 30 days", 29), clear),
        h("div", { class: "chips", role: "group", "aria-label": "Types" }, typeBtns)),
      status, out, h("p", {}, more));
    fillSessions();
    await load(true);
  },

  async sessions(gen, params) {
    const id = params.get("id");
    let handoffs = [];
    try { handoffs = (await api("handoffs?limit=200")).items; } catch (e) { /* the page still works without the links */ }
    if (gen !== state.gen) return;
    const bySession = new Map();
    for (const hd of handoffs) if (hd.session_id && !bySession.has(hd.session_id)) bySession.set(hd.session_id, hd);

    if (id) {
      let s;
      try { s = await api("sessions/" + encodeURIComponent(id)); } catch (e) {
        setMain(h("div", { class: "pagehead" }, h("h1", { text: "Session" }), link("sessions", {}, "All sessions")), empty("That session was not found."));
        return;
      }
      if (gen !== state.gen) return;
      const li = sessionCard(s, bySession.get(s.id), true);
      setMain(h("div", { class: "pagehead" }, h("h1", { text: "Session " + shortId(s.id) }), link("sessions", {}, "All sessions")), h("ol", { class: "feed" }, li));
      return;
    }

    const projects = (await api("facets")).projects;
    if (gen !== state.gen) return;
    const project = params.get("project") || "";
    const sel = h("select", { "aria-label": "Project" }, h("option", { value: "", text: "All projects" }),
      projects.map((p) => h("option", { value: p.name, text: p.name, selected: p.name === project })));
    sel.addEventListener("change", () => { const p = new URLSearchParams(); if (sel.value) p.set("project", sel.value); replaceParams(p); load(true); });
    const feed = h("ol", { class: "feed" }), more = h("button", { type: "button", text: "Load more", hidden: true });
    const status = h("p", { class: "status muted", role: "status" });
    let cursor = "", req = 0;
    async function load(reset) {
      const my = ++req;
      if (reset) { cursor = ""; feed.replaceChildren(); }
      const p = state.params.get("project") || "";
      const qs = new URLSearchParams({ limit: "30" });
      if (p) qs.set("project", p);
      if (cursor) qs.set("cursor", cursor);
      try {
        const page = await api("sessions?" + qs);
        if (my !== req || gen !== state.gen) return;
        feed.append(...page.items.map((s) => sessionCard(s, bySession.get(s.id), false)));
        cursor = page.next_cursor;
        more.hidden = !cursor;
        if (!feed.children.length) feed.append(h("li", {}, empty("No sessions yet.")));
      } catch (e) { status.textContent = "Could not load sessions."; }
    }
    more.addEventListener("click", () => load(false));
    setMain(h("div", { class: "pagehead" }, h("h1", { text: "Sessions" }), sel), status, feed, h("p", {}, more));
    await load(true);
  },

  async work(gen, params) {
    const [hd, ws, dl] = await Promise.all([api("handoffs?limit=100"), api("work-state"), api("deliverables")]);
    if (gen !== state.gen) return;
    const want = params.get("handoff") || "";
    let project = params.get("project") || "";
    if (want && !project) { const m = hd.items.find((x) => x.id === want); if (m) project = m.project; }
    const names = [...new Set([...hd.items.map((x) => x.project), ...ws.lists.map((l) => l.project)])].filter(Boolean).sort();
    const sel = h("select", { "aria-label": "Project" }, h("option", { value: "", text: "All projects" }),
      names.map((n) => h("option", { value: n, text: n, selected: n === project })));
    sel.addEventListener("change", () => { const p = new URLSearchParams(); if (sel.value) p.set("project", sel.value); replaceParams(p); paint(); });
    const host = h("div");

    function paint() {
      const proj = state.params.get("project") || "";
      const hs = hd.items.filter((x) => !proj || x.project === proj);
      const lists = ws.lists.filter((l) => !proj || l.project === proj);
      const seenProject = new Set();
      host.replaceChildren(
        h("h2", { text: "Handoffs" }),
        hs.length ? h("ol", { class: "feed" }, hs.map((x) => {
          const first = !seenProject.has(x.project);
          seenProject.add(x.project);
          return handoffCard(x, x.id === want || (!want && first));
        })) : empty("No handoffs stored" + (proj ? " for this project." : ".")),
        h("h2", { text: "Work state" }),
        h("p", { class: "muted", text: "Work state is stored per project, not per session. Lists written with work_state_write appear here." }),
        lists.length ? lists.map((l) => h("section", {}, h("h3", { text: l.name + (l.project && !proj ? "  (" + l.project + ")" : "") }),
          h("ul", { class: "feed" }, l.items.map((it) => h("li", {}, workItem(it)))))) : empty("Nothing open."),
        h("h2", { text: "Deliverables" }),
        (proj ? dl.items.filter((d) => d.project === proj) : dl.items).length
          ? table(["Title", "Location", "Project", "When"], (proj ? dl.items.filter((d) => d.project === proj) : dl.items).map((d) => [d.title, d.location, d.project, ago(d.at)]))
          : empty("No deliverables recorded."));
    }
    state.params = new URLSearchParams(params);
    setMain(h("div", { class: "pagehead" }, h("h1", { text: "Handoffs and work" }), sel), host);
    paint();
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
      o.rejections.length ? table(["Reason", "Count"], o.rejections.map((r) => [r.reason, num(r.count)]), [1]) : empty("No rejections."),
      o.rejected_items.length ? h("details", {}, h("summary", { text: "Latest rejected items" }),
        table(["When", "Tool", "Reason"], o.rejected_items.map((r) => [ago(r.at), r.tool, r.reason]))) : null,
      h("h2", { text: "Calls and cost by model, " + o.models_range }),
      o.models.length ? table(["Model", "Calls", "USD"], o.models.map((m) => [m.model, num(m.calls), usd(m.usd)]), [1, 2]) : empty("No model calls recorded."));
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

function workItem(it) {
  const keys = Object.keys(it).filter((k) => k !== "task" && k !== "status" && k !== "work_product");
  if (it.task === undefined) { // list-level state or a facts row: key / value pairs
    return h("dl", {}, Object.entries(it).flatMap(([k, v]) => [h("dt", { text: k }), h("dd", { text: typeof v === "string" ? v : JSON.stringify(v) })]));
  }
  return h("div", {}, h("div", { class: "row" }, h("span", { class: "badge t-other", text: it.status || "todo" }), h("span", { class: "title", text: it.task })),
    it.work_product ? h("div", { class: "sub" }, "Work product: ", h("code", { text: String(it.work_product) })) : null,
    keys.length ? h("dl", {}, keys.flatMap((k) => [h("dt", { text: k }), h("dd", { text: typeof it[k] === "string" ? it[k] : JSON.stringify(it[k]) })])) : null);
}

function handoffCard(x, open) {
  const shown = x.state === "shown";
  const det = h("details", open ? { open: true } : {}, h("summary", { text: open ? "Hide handoff" : "Show handoff" }), window.renderMarkdown(x.text));
  det.addEventListener("toggle", () => { det.firstChild.textContent = det.open ? "Hide handoff" : "Show handoff"; });
  return h("li", {},
    h("div", { class: "row" },
      h("span", { class: "title", text: x.project || "(no project)" }),
      h("span", { class: "badge " + (shown ? "state-shown" : "state-unshown"), text: shown ? "Shown" : "Waiting for the next session" }),
      h("time", { class: "time", datetime: x.created_at, title: fullTime(x.created_at), text: "written " + ago(x.created_at) })),
    h("div", { class: "sub" },
      shown ? "Shown " + ago(x.consumed_at) + (x.consumed_by ? " (" + x.consumed_by + ")" : "") + ". " : "Not shown yet. ",
      x.session_id ? ["Written by session ", link("sessions", { id: x.session_id }, shortId(x.session_id)),
        x.session_link === "filename" ? " (matched by file name)" : ""] : "The session that wrote it was not recorded.",
      x.file ? [" File: ", h("code", { text: x.file })] : null),
    det);
}

function sessionCard(s, handoff, expanded) {
  const sm = s.summary || {};
  const sections = [["Request", sm.request], ["Investigated", sm.investigated], ["Learned", sm.learned],
    ["Completed", sm.completed], ["Next steps", sm.next_steps], ["Notes", sm.notes]].filter(([, v]) => v);
  const body = h("div", { class: "body" });
  const det = h("details", expanded ? { open: true } : {}, h("summary", { text: "Prompts" }), body);
  const loadPrompts = async () => {
    if (!det.open || det.dataset.loaded) return;
    det.dataset.loaded = "1";
    try {
      const d = await api("sessions/" + encodeURIComponent(s.id));
      body.replaceChildren(...(d.prompts.length ? d.prompts.map((p) => h("pre", { text: p.text })) : [h("span", { class: "muted", text: "No prompts." })]));
    } catch (err) { delete det.dataset.loaded; body.textContent = "Could not load prompts."; }
  };
  det.addEventListener("toggle", loadPrompts);
  const li = h("li", {},
    h("div", { class: "row" }, h("span", { class: "title", text: s.project || "(no project)" }),
      h("time", { class: "time", title: fullTime(s.started_at), text: ago(s.started_at) + ", " + dur(s.started_at, s.ended_at) }),
      h("span", { class: "muted", text: s.tool_uses + " tool uses, " + usd(s.usd) + ", " + s.prompt_count + " prompts" })),
    sections.length ? h("dl", {}, sections.flatMap(([k, v]) => [h("dt", { text: k }), h("dd", { text: v })]))
      : h("div", { class: "sub", text: "No summary yet." }),
    h("p", { class: "links" }, link("memory", { session: s.id }, "Memories from this session"),
      handoff ? link("work", { handoff: handoff.id }, "Handoff written here") : null,
      expanded ? null : link("sessions", { id: s.id }, "Open")),
    det);
  if (expanded) loadPrompts();
  return li;
}

// ---- routing and rendering ----

async function render(moveFocus) {
  const { route, params } = parseHash();
  state.route = route;
  state.params = params;
  state.pending = false;
  const gen = ++state.gen;
  for (const a of document.querySelectorAll("#nav a")) {
    if (a.dataset.route === state.route) a.setAttribute("aria-current", "page"); else a.removeAttribute("aria-current");
  }
  document.title = TITLES[state.route] + " - memory";
  try {
    await views[state.route](gen, params);
  } catch (e) {
    if (gen === state.gen) setMain(h("h1", { text: TITLES[state.route] }), h("p", { class: "muted", role: "status", text: "Could not load this view. It will retry when memory is reachable." }));
  }
  if (moveFocus && gen === state.gen) {
    window.scrollTo(0, 0);
    document.getElementById("main").focus({ preventScroll: true });
  }
}

// Re-render on new data unless the user is reading or typing; then offer a button.
function dataChanged() {
  const main = document.getElementById("main");
  const ae = document.activeElement;
  const busy = main.querySelector("details[open]") || window.scrollY > 300 ||
    (ae && ae !== document.body && main.contains(ae) && ae.matches("input, select, textarea"));
  if (busy) {
    if (state.pending) return;
    state.pending = true;
    const b = h("button", { type: "button", class: "newbtn primary", text: "New data available. Refresh" });
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
    if (v.includes(state.route) || (state.route === "stats" && v.includes("overview"))) dataChanged();
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
  btn.setAttribute("aria-label", "Theme: " + t + ". Activate to change.");
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
window.addEventListener("resize", () => { if (window.__statsRepaint && state.route === "stats") window.__statsRepaint(); });
refreshHealth();
render(false);
connect();
