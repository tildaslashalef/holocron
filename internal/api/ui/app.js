// holocron's dashboard (SPEC.md §8.3). It reads /v1/stats every few
// seconds and builds the page with DOM calls only: every value from the
// server, queries and URLs above all, goes in as text, never as markup.
"use strict";

(() => {
  const REFRESH_MS = 5000;
  const EP = { "/v1/search": "search", "/v1/context": "context", "/v1/fetch": "fetch" };
  const SVG = "http://www.w3.org/2000/svg";

  // h builds an element; string children become text nodes.
  function h(tag, attrs, ...children) {
    const el = document.createElement(tag);
    for (const [k, v] of Object.entries(attrs || {})) {
      if (v === undefined || v === null || v === false) continue;
      if (k === "class") el.className = v;
      else if (k.startsWith("on")) el.addEventListener(k.slice(2), v);
      else el.setAttribute(k, String(v));
    }
    for (const c of children.flat()) {
      if (c === undefined || c === null || c === false) continue;
      el.append(c instanceof Node ? c : document.createTextNode(String(c)));
    }
    return el;
  }
  function s(tag, attrs) {
    const el = document.createElementNS(SVG, tag);
    for (const [k, v] of Object.entries(attrs || {})) el.setAttribute(k, String(v));
    return el;
  }
  const $ = (id) => document.getElementById(id);
  const fill = (el, ...children) => { el.replaceChildren(...children.flat().filter(Boolean)); };

  const nf = new Intl.NumberFormat();
  const num = (n) => nf.format(n || 0);
  const count = (n, one, many) => num(n) + " " + (n === 1 ? one : many);
  function usd(v) {
    v = v || 0;
    if (v === 0) return "$0";
    if (v < 0.01) return "$" + v.toFixed(4);
    if (v < 10) return "$" + v.toFixed(3);
    return "$" + v.toFixed(2);
  }
  function bytes(n) {
    if (!n) return "0 B";
    const u = ["B", "KiB", "MiB", "GiB"];
    let i = 0;
    while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
    return (n >= 10 || i === 0 ? n.toFixed(0) : n.toFixed(1)) + " " + u[i];
  }
  function ms(n) { return n >= 1000 ? (n / 1000).toFixed(n >= 10000 ? 0 : 1) + " s" : Math.round(n) + " ms"; }
  function pct(a, b) { return b > 0 ? Math.round((100 * a) / b) + "%" : "–"; }
  function duration(sec) {
    const d = Math.floor(sec / 86400), hh = Math.floor((sec % 86400) / 3600), mm = Math.floor((sec % 3600) / 60);
    if (d) return d + "d " + hh + "h";
    if (hh) return hh + "h " + mm + "m";
    return mm + "m";
  }
  function ago(iso, now) {
    const d = Math.max(0, (now - new Date(iso)) / 1000);
    if (d < 60) return Math.round(d) + "s ago";
    if (d < 3600) return Math.round(d / 60) + "m ago";
    return Math.round(d / 3600) + "h ago";
  }
  const clock = (d) => d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });

  // spark draws values as a soft area, for a KPI tile.
  function spark(values, color) {
    const svg = s("svg", { class: "spark", viewBox: "0 0 100 30", preserveAspectRatio: "none", "aria-hidden": "true" });
    const max = Math.max(1, ...values);
    const n = values.length;
    if (n < 2) return svg;
    let d = "M0,30";
    values.forEach((v, i) => { d += " L" + ((i / (n - 1)) * 100).toFixed(2) + "," + (30 - (v / max) * 26).toFixed(2); });
    d += " L100,30 Z";
    svg.append(s("path", { d, fill: color, opacity: "0.14" }));
    let line = "";
    values.forEach((v, i) => { line += (i ? " L" : "M") + ((i / (n - 1)) * 100).toFixed(2) + "," + (30 - (v / max) * 26).toFixed(2); });
    svg.append(s("path", { d: line, fill: "none", stroke: color, "stroke-width": "1.4", "vector-effect": "non-scaling-stroke", opacity: "0.8" }));
    return svg;
  }
  const cssVar = (name) => getComputedStyle(document.documentElement).getPropertyValue(name).trim();

  // rebucket sums n consecutive steps, so a long series reads at a glance.
  function rebucket(a, n) {
    const out = [];
    for (let i = 0; i < a.length; i += n) out.push(a.slice(i, i + n).reduce((x, y) => x + y, 0));
    return out;
  }

  function renderHeader(d) {
    const started = new Date(d.started);
    $("sub").textContent = "v" + d.version + " · up " + duration(d.uptime_s) + " · since " + started.toLocaleString();
    document.title = "holocron · " + num(d.last_24h.requests) + " requests today";
  }

  function renderRoutes(d) {
    const state = Object.fromEntries(d.providers.map((p) => [p.name, p.state]));
    fill($("routes"), Object.keys(EP).map((ep) => {
      const names = d.routes[ep] || [];
      const parts = [h("span", { class: "ep" }, ep)];
      if (!names.length) parts.push(h("span", { class: "pill none" }, "not configured"));
      names.forEach((n, i) => {
        parts.push(h("span", { class: "arrow" }, i === 0 ? "→" : "then"));
        parts.push(h("span", { class: "pill " + (state[n] || "") }, h("span", { class: "s" }), n));
      });
      return h("div", { class: "route" }, parts);
    }));
  }

  function renderKPIs(d) {
    const total = (d.series.requests["/v1/search"] || []).map((_, i) =>
      Object.values(d.series.requests).reduce((a, arr) => a + (arr[i] || 0), 0));
    const t = d.last_24h;
    const worst = d.endpoints.reduce((m, e) => (e.latency_samples && e.p95_ms > m.p95_ms ? e : m), { p95_ms: 0, endpoint: "" });
    const slotsUp = d.slots.find((x) => x.pool === "upstream") || { in_use: 0, max: 0 };
    const tiles = [
      { label: "Requests · 24 h", value: num(t.requests), note: num(d.totals.requests) + " since start", series: rebucket(total, 3), color: cssVar("--accent") },
      { label: "Spend · 24 h", value: usd(t.cost_usd), note: usd(d.totals.cost_usd) + " since start · estimate", series: rebucket(d.series.cost_usd, 3), color: cssVar("--cost") },
      { label: "Saved by cache · 24 h", value: usd(t.saved_usd), note: count(t.cache_hits, "answer", "answers") + " from cache", color: cssVar("--ok") },
      { label: "Cache hit rate · 24 h", value: pct(t.cache_hits, t.requests), note: "hits and stale answers", color: cssVar("--ok") },
      { label: "Errors · 24 h", value: num(t.errors), note: pct(t.errors, t.requests) + " of requests", series: rebucket(d.series.errors, 3), color: cssVar("--bad") },
      { label: "Slowest p95", value: worst.endpoint ? ms(worst.p95_ms) : "–", note: worst.endpoint ? worst.endpoint + " · slots " + slotsUp.in_use + "/" + slotsUp.max : "no requests yet" },
    ];
    fill($("kpis"), tiles.map((k) => h("div", { class: "kpi" },
      h("div", { class: "label" }, k.label),
      h("div", { class: "value" }, k.value),
      h("div", { class: "note" }, k.note),
      k.series ? spark(k.series, k.color) : null)));
  }

  function renderChart(d) {
    const chart = $("chart");
    const tip = $("tip");
    const eps = Object.keys(EP);
    const step = 3; // 15-minute bars from 5-minute steps
    const series = eps.map((ep) => rebucket(d.series.requests[ep] || [], step));
    const errs = rebucket(d.series.errors, step);
    const costs = rebucket(d.series.cost_usd, step);
    const n = series[0].length;
    const totals = Array.from({ length: n }, (_, i) => series.reduce((a, sr) => a + sr[i], 0));
    const max = Math.max(4, ...totals);
    const W = 1000, H = 180, bw = W / n;
    const svg = s("svg", { viewBox: `0 0 ${W} ${H}`, preserveAspectRatio: "none", role: "img", "aria-label": "Requests per 15 minutes over the last 24 hours" });
    const grid = s("g", { class: "grid" });
    for (let g = 1; g <= 3; g++) grid.append(s("line", { x1: 0, x2: W, y1: H - (H * g) / 4, y2: H - (H * g) / 4 }));
    svg.append(grid);
    const colors = { search: cssVar("--search"), context: cssVar("--context"), fetch: cssVar("--fetch") };
    for (let i = 0; i < n; i++) {
      let y = H;
      eps.forEach((ep, k) => {
        const v = series[k][i];
        if (!v) return;
        const hgt = (v / max) * (H - 6);
        y -= hgt;
        svg.append(s("rect", { x: i * bw + bw * 0.14, y, width: bw * 0.72, height: Math.max(hgt, 1), rx: Math.min(2, bw * 0.2), fill: colors[EP[ep]] }));
      });
      if (errs[i]) svg.append(s("rect", { x: i * bw + bw * 0.3, y: H - 3, width: bw * 0.4, height: 3, fill: cssVar("--bad") }));
    }
    const start = new Date(d.series.start).getTime();
    const stepMs = d.series.step_s * 1000 * step;
    for (let i = 0; i < n; i++) {
      const r = s("rect", { class: "hover", x: i * bw, y: 0, width: bw, height: H });
      r.addEventListener("mouseenter", () => {
        const from = new Date(start + i * stepMs);
        fill(tip, h("b", {}, clock(from) + " – " + clock(new Date(from.getTime() + stepMs))),
          eps.map((ep, k) => h("div", {}, h("span", {}, ep), h("span", {}, num(series[k][i])))),
          h("div", {}, h("span", {}, "errors"), h("span", {}, num(errs[i]))),
          h("div", {}, h("span", {}, "cost"), h("span", {}, usd(costs[i]))));
        tip.hidden = false;
        const box = chart.getBoundingClientRect();
        const x = ((i + 0.5) / n) * box.width;
        tip.style.left = Math.min(Math.max(x - 80, 0), box.width - 170) + "px";
        tip.style.top = "4px";
      });
      r.addEventListener("mouseleave", () => { tip.hidden = true; });
      svg.append(r);
    }
    fill(chart, svg, tip);
    fill($("legend"), eps.map((ep) => {
      const i = h("i", {});
      i.style.background = colors[EP[ep]];
      return h("span", {}, i, EP[ep]);
    }), (() => { const i = h("i", {}); i.style.background = cssVar("--bad"); return h("span", {}, i, "errors"); })());
    const axis = [];
    for (let k = 0; k <= 4; k++) axis.push(h("span", {}, k === 4 ? "now" : clock(new Date(start + (k / 4) * n * stepMs))));
    fill($("axis"), axis);
  }

  function renderProviders(d) {
    if (!d.providers.length) {
      fill($("providers"), h("div", { class: "panel empty" }, "No provider is configured. Give holocron a key (holocron key set parallel)."));
      return;
    }
    fill($("providers"), d.providers.map((p) => {
      const fb = Object.entries(p.fallbacks || {});
      const stats = [h("div", { class: "stat" }, h("div", { class: "k" }, "answered"), h("div", { class: "v" }, num(p.answered)))];
      if (p.metered) {
        stats.push(h("div", { class: "stat" }, h("div", { class: "k" }, p.endpoints.includes("/v1/fetch") && p.endpoints.length === 1 ? "billed URLs" : "billed"), h("div", { class: "v" }, num(p.billed_units))));
        stats.push(h("div", { class: "stat" }, h("div", { class: "k" }, "spend · 24 h"), h("div", { class: "v" }, usd(p.cost_24h_usd))));
      } else {
        stats.push(h("div", { class: "stat" }, h("div", { class: "k" }, "billing"), h("div", { class: "v" }, "free")));
      }
      return h("article", { class: "prov" },
        h("div", { class: "prov-head" }, h("h3", {}, p.name), h("span", { class: "badge " + p.state }, p.state === "ok" ? "healthy" : "down")),
        p.why ? h("p", { class: "why" }, p.why) : null,
        h("div", { class: "eps" }, p.endpoints.map((e) => h("span", { class: "ep-chip" }, e))),
        h("div", { class: "stats" }, stats),
        h("div", { class: "fallbacks" },
          p.metered ? "Since start: " + usd(p.cost_usd) + " estimated. " : "",
          fb.length ? "Fallen back from " + fb.map(([r, c]) => num(c) + "× " + r.replace("_", " ")).join(", ") + "." : "Never fallen back from.",
          p.quota ? " Quota: " + num(p.quota.remaining) + " left, resets in " + duration(p.quota.resets_in_s) + "." : ""));
    }));
  }

  function renderEndpoints(d) {
    fill($("endpoints").tBodies[0], d.endpoints.map((e) => h("tr", {},
      h("td", {}, h("span", { class: "tag " + EP[e.endpoint] }, e.endpoint)),
      h("td", { class: "num" }, num(e.requests)),
      h("td", { class: "num" }, e.errors ? num(e.errors) : h("span", { class: "muted" }, "0")),
      h("td", { class: "num" }, pct(e.cache_hits, e.requests)),
      h("td", { class: "num" }, e.latency_samples ? ms(e.p50_ms) : "–"),
      h("td", { class: "num" }, e.latency_samples ? ms(e.p95_ms) : "–"))));
    const codes = Object.entries(d.codes).sort((a, b) => b[1] - a[1]);
    fill($("codes"), codes.length ? codes.map(([c, n]) => h("span", { class: "code-chip" }, c + " × " + num(n))) : [h("span", { class: "muted" }, "No errors since start.")]);
  }

  function bar(parts) {
    const total = parts.reduce((a, p) => a + p.v, 0);
    return h("div", { class: "bar" }, parts.map((p) => {
      const sp = h("span", { class: p.c, title: p.t });
      sp.style.width = (total > 0 ? (100 * p.v) / total : 0) + "%";
      return sp;
    }));
  }
  function useBar(used, max) {
    const sp = h("span", { class: "use" });
    sp.style.width = Math.min(100, max > 0 ? (100 * used) / max : 0) + "%";
    return h("div", { class: "bar" }, sp);
  }

  function renderCache(d) {
    fill($("cache"), h("div", { class: "sub-h" }, "Cache since start"), d.cache.map((c) => {
      const looked = c.hit + c.miss + c.stale;
      return h("div", { class: "bar-row" },
        h("span", { class: "name" }, c.kind),
        looked ? bar([{ v: c.hit, c: "hit", t: "hits" }, { v: c.stale, c: "stale", t: "stale" }, { v: c.miss, c: "miss", t: "misses" }]) : h("div", { class: "bar" }),
        h("span", { class: "meta" }, pct(c.hit + c.stale, looked) + " · " + num(c.entries) + " · " + bytes(c.bytes)));
    }));
    const m = d.memory;
    const limit = m.limit_bytes > 0 && m.limit_bytes < 2 ** 52 ? m.limit_bytes : 0;
    fill($("capacity"), h("div", { class: "sub-h" }, "Capacity now"),
      d.slots.map((sl) => h("div", { class: "bar-row" },
        h("span", { class: "name" }, sl.pool), useBar(sl.in_use, sl.max), h("span", { class: "meta" }, sl.in_use + " of " + sl.max + " slots"))),
      h("div", { class: "bar-row" },
        h("span", { class: "name" }, "memory"), useBar(m.total_bytes, limit || m.total_bytes * 1.5),
        h("span", { class: "meta" }, bytes(m.total_bytes) + (limit ? " of " + bytes(limit) : "") + " · heap " + bytes(m.heap_bytes))),
      h("div", { class: "bar-row" }, h("span", { class: "name" }, "busy"), h("span", { class: "meta" }, num(d.busy) + " requests refused for want of a slot · " + num(m.goroutines) + " goroutines"), h("span")));
  }

  const open = new Set();
  function renderRecent(d) {
    const now = new Date(d.now);
    $("recent-hint").textContent = d.log_queries ? "Click a row for the full query or URL." : "Queries and URLs are hidden (--log-queries=false).";
    const body = $("recent").tBodies[0];
    if (!d.recent.length) {
      fill(body, h("tr", {}, h("td", { colspan: 8, class: "empty" }, "No requests yet.")));
      return;
    }
    const rows = [];
    d.recent.forEach((r) => {
      const id = r.at + r.endpoint + (r.query || r.url || r.host || "");
      const kind = EP[r.endpoint];
      const what = r.endpoint === "/v1/fetch" ? (r.url || r.host || "") : (r.query || "");
      const sub = r.endpoint === "/v1/fetch" && r.query ? "q: " + r.query : (r.endpoint === "/v1/fetch" && r.url ? "" : r.host || "");
      const result = r.code ? h("span", { class: "tag err" }, r.code) : (r.endpoint === "/v1/fetch" ? "page" : num(r.results));
      const row = h("tr", { class: "row", tabindex: 0, "aria-expanded": open.has(id) ? "true" : "false" },
        h("td", { class: "muted", title: new Date(r.at).toLocaleString() }, ago(r.at, now)),
        h("td", {}, h("span", { class: "tag " + kind }, kind)),
        h("td", { class: "q" }, h("span", { class: "qtext" }, what || h("span", { class: "muted" }, "hidden")), sub ? h("span", { class: "qhost" }, sub) : null),
        h("td", {}, r.provider || h("span", { class: "muted" }, "–"), r.fallback && r.fallback.length ? h("span", { class: "qhost" }, " after " + r.fallback.join(", ")) : null),
        h("td", {}, r.cache ? h("span", { class: "tag " + r.cache }, r.cache) : h("span", { class: "muted" }, "–")),
        h("td", { class: "num" }, result),
        h("td", { class: "num" }, ms(r.ms)),
        h("td", { class: "num" }, r.cost_usd ? usd(r.cost_usd) : h("span", { class: "muted" }, "–")));
      const toggle = () => { open.has(id) ? open.delete(id) : open.add(id); renderRecent(last); };
      row.addEventListener("click", toggle);
      row.addEventListener("keydown", (ev) => { if (ev.key === "Enter" || ev.key === " ") { ev.preventDefault(); toggle(); } });
      rows.push(row);
      if (open.has(id)) {
        const lines = [];
        if (r.query) lines.push(h("div", {}, "query: ", h("span", { class: "full" }, r.query)));
        if (r.url) lines.push(h("div", {}, "url: ", h("span", { class: "full" }, r.url)));
        if (r.host) lines.push(h("div", {}, "host: ", h("span", { class: "full" }, r.host)));
        lines.push(h("div", {}, "status " + r.status + " at " + new Date(r.at).toLocaleString()));
        rows.push(h("tr", { class: "detail" }, h("td", { colspan: 8 }, lines)));
      }
    });
    fill(body, rows);
  }


  // --- Try the API ---------------------------------------------------------
  // The playground calls /v1 like an agent would and shows the answer
  // rendered or raw. Like the rest of the page, every value goes in as
  // text; a link is made only for an http(s) URL.

  function safeURL(u) {
    try {
      const p = new URL(u);
      return p.protocol === "http:" || p.protocol === "https:" ? p.href : null;
    } catch (e) { return null; }
  }
  function link(u, cls, label) {
    const href = safeURL(u);
    return href ? h("a", { class: cls, href, target: "_blank", rel: "noopener noreferrer" }, label || u) : h("span", { class: cls }, label || u);
  }

  const play = { ep: "search", raw: false, last: null };

  function setupPlayground() {
    const form = $("play-form");
    document.querySelectorAll(".tab").forEach((t) => t.addEventListener("click", () => selectTab(t.dataset.ep, true)));
    document.querySelector(".tabs").addEventListener("keydown", (ev) => {
      if (ev.key !== "ArrowRight" && ev.key !== "ArrowLeft") return;
      const order = ["search", "context", "fetch"];
      const i = (order.indexOf(play.ep) + (ev.key === "ArrowRight" ? 1 : 2)) % 3;
      selectTab(order[i], true);
      document.querySelector('.tab[data-ep="' + order[i] + '"]').focus();
    });
    form.addEventListener("submit", (ev) => { ev.preventDefault(); run(0); });
    selectTab("search", false);
  }

  function selectTab(ep, focus) {
    play.ep = ep;
    document.querySelectorAll(".tab").forEach((t) => {
      const on = t.dataset.ep === ep;
      t.setAttribute("aria-selected", on ? "true" : "false");
      t.tabIndex = on ? 0 : -1;
    });
    document.querySelectorAll("#play-form [data-for]").forEach((f) => { f.hidden = !f.dataset.for.split(" ").includes(ep); });
    $("p-go").textContent = { search: "Search", context: "Get context", fetch: "Fetch" }[ep];
    if (focus) (ep === "fetch" ? $("p-url") : $("p-q")).focus();
  }

  function buildPath(start) {
    const p = new URLSearchParams();
    const fresh = $("p-fresh").value;
    if (play.ep === "fetch") {
      p.set("url", $("p-url").value.trim());
      const focus = $("p-focus").value.trim();
      if (focus) p.set("q", focus);
      if (start) p.set("start", String(start));
      p.set("max_chars", $("p-max").value);
    } else {
      p.set("q", $("p-q").value.trim());
      if (play.ep === "search") p.set("n", $("p-n").value);
      else { p.set("tokens", $("p-tokens").value); p.set("urls", $("p-urls").value); }
      if (fresh) p.set("freshness", fresh);
    }
    return "/v1/" + play.ep + "?" + p.toString();
  }

  async function run(start) {
    const out = $("play-out");
    const needed = play.ep === "fetch" ? $("p-url") : $("p-q");
    if (!needed.value.trim()) { needed.focus(); return; }
    const path = buildPath(start);
    const noCache = $("p-fresh-cache").checked;
    const go = $("p-go");
    go.disabled = true;
    fill(out, h("div", { class: "req" }, h("span", { class: "line" }, "GET " + path), h("span", {}, "waiting…")));
    const t0 = performance.now();
    let res, body, text;
    try {
      res = await fetch(path, { headers: Object.assign({ Accept: "application/json" }, noCache ? { "Cache-Control": "no-cache" } : {}) });
      text = await res.text();
      try { body = JSON.parse(text); } catch (e) { body = null; }
    } catch (e) {
      go.disabled = false;
      fill(out, h("div", { class: "problem" }, h("b", {}, "unreachable"), String(e.message || e)));
      return;
    }
    go.disabled = false;
    play.last = { ep: play.ep, path, status: res.status, ms: performance.now() - t0, cache: res.headers.get("X-Search-Cache"), age: res.headers.get("Age"), body, text, start };
    showResult();
    setTimeout(tick, 300); // the stats now include this request
  }

  function showResult() {
    const r = play.last;
    const out = $("play-out");
    const b = r.body || {};
    const chips = [h("span", { class: "chip " + (r.status < 400 ? "ok" : "bad") }, String(r.status)), h("span", { class: "chip" }, ms(r.ms))];
    if (r.cache) chips.push(h("span", { class: "chip " + (r.cache === "miss" ? "" : "ok") }, "cache " + r.cache + (r.age ? " · " + r.age + " s old" : "")));
    if (b.provider) chips.push(h("span", { class: "chip acc" }, "by " + b.provider));
    if (b.providers_failed && b.providers_failed.length) chips.push(h("span", { class: "chip bad" }, "skipped " + b.providers_failed.join(", ")));
    const curl = "curl -s '" + location.origin + r.path.replace(/'/g, "'\\''") + "'";
    const copy = h("button", { type: "button", class: "ghost", title: "Copy as a curl command" }, "Copy curl");
    copy.addEventListener("click", async () => {
      try { await navigator.clipboard.writeText(curl); copy.textContent = "Copied"; } catch (e) { copy.textContent = "Copy failed"; }
      setTimeout(() => { copy.textContent = "Copy curl"; }, 1500);
    });
    const rawBtn = h("button", { type: "button", class: "ghost", "aria-pressed": play.raw ? "true" : "false" }, "JSON");
    rawBtn.addEventListener("click", () => { play.raw = !play.raw; showResult(); });
    const head = h("div", { class: "req" }, h("span", { class: "line" }, "GET " + r.path), h("div", { class: "chips" }, chips), copy, rawBtn);

    let view;
    if (play.raw) view = h("pre", { class: "raw" }, r.body ? JSON.stringify(r.body, null, 2) : r.text);
    else if (r.status >= 400 || !r.body) view = h("div", { class: "problem" }, h("b", {}, (b.error && b.error.code) || "error"), (b.error && b.error.message) || r.text || "no answer");
    else if (r.ep === "search") view = renderSearchResult(b);
    else if (r.ep === "context") view = renderContextResult(b);
    else view = renderFetchResult(b);
    fill(out, head, view);
  }

  function renderSearchResult(b) {
    const items = b.results || [];
    const parts = [];
    if (b.altered_query) parts.push(h("div", { class: "hint" }, "Searched for: " + b.altered_query));
    if (!items.length) parts.push(h("div", { class: "empty" }, "No results."));
    items.forEach((it, i) => parts.push(h("article", { class: "res" },
      h("div", { class: "res-top" }, h("span", { class: "n" }, String(i + 1)), link(it.url, "title", it.title || it.url),
        it.published ? h("span", { class: "date" }, it.published) : null),
      h("div", { class: "url" }, it.url),
      it.snippet ? h("p", {}, it.snippet) : null,
      it.snippets && it.snippets.length ? h("details", { class: "more" },
        h("summary", {}, count(it.snippets.length, "passage", "passages") + " from the page"),
        h("ul", {}, it.snippets.map((sn) => h("li", {}, sn)))) : null)));
    return h("div", { class: "results" }, parts);
  }

  function renderContextResult(b) {
    const srcs = b.sources || [];
    let chars = 0;
    srcs.forEach((sr) => (sr.snippets || []).forEach((sn) => { chars += sn.length; }));
    const parts = [h("div", { class: "hint" }, count(srcs.length, "source", "sources") + " · about " + num(Math.round(chars / 4)) + " tokens of passages")];
    if (!srcs.length) parts.push(h("div", { class: "empty" }, "No sources."));
    srcs.forEach((sr, i) => parts.push(h("article", { class: "res" },
      h("div", { class: "res-top" }, h("span", { class: "n" }, String(i + 1)), link(sr.url, "title", sr.title || sr.url),
        sr.published ? h("span", { class: "date" }, sr.published) : null),
      h("div", { class: "url" }, sr.url),
      (sr.snippets || []).map((sn) => h("blockquote", {}, sn)))));
    return h("div", { class: "results" }, parts);
  }

  function renderFetchResult(b) {
    const shown = (b.text || "").length;
    const end = (b.start || 0) + [...(b.text || "")].length;
    const meta = [];
    if (b.focused) meta.push(h("span", { class: "chip acc" }, "focused on the question"));
    if (b.content_type) meta.push(h("span", { class: "chip" }, b.content_type));
    if (b.status) meta.push(h("span", { class: "chip" }, "page answered " + b.status));
    if (b.published) meta.push(h("span", { class: "chip" }, "published " + b.published));
    meta.push(h("span", { class: "chip" }, "characters " + num(b.start || 0) + "–" + num(end) + " of " + num(b.chars)));
    const head = h("div", { class: "doc-head" },
      h("span", { class: "t" }, b.title || "(no title)"),
      link(b.final_url || b.url, "", b.final_url || b.url),
      h("div", { class: "chips" }, meta));
    const doc = h("pre", { class: "doc" }, shown ? b.text : "(no text)");
    const pager = [];
    if (b.truncated && end < b.chars) {
      const next = h("button", { type: "button", class: "ghost" }, "Next " + num(Math.min(Number($("p-max").value), b.chars - end)) + " characters");
      next.addEventListener("click", () => run(end));
      pager.push(next, h("span", {}, "from the cache: no new upstream call"));
    } else if (b.truncated) {
      pager.push(h("span", {}, "The source was cut at the provider's limit; this is all that was read."));
    } else {
      pager.push(h("span", {}, "The whole " + (b.focused ? "focused answer" : "page") + "."));
    }
    return h("div", { class: "results" }, head, doc, h("div", { class: "pager" }, pager));
  }

  // The page's two views, Metrics and Try the API, follow the URL's hash,
  // so a reload or a shared link opens the same one.
  function setupViews() {
    window.addEventListener("hashchange", showView);
    document.querySelector(".views").addEventListener("keydown", (ev) => {
      if (ev.key !== "ArrowRight" && ev.key !== "ArrowLeft") return;
      const next = location.hash === "#try" ? "#metrics" : "#try";
      history.replaceState(null, "", next);
      showView();
      $(next === "#try" ? "tab-try" : "tab-metrics").focus();
    });
    showView();
  }
  function showView() {
    const tryIt = location.hash === "#try";
    $("view-metrics").hidden = tryIt;
    $("view-try").hidden = !tryIt;
    document.querySelector(".foot").hidden = tryIt;
    for (const [id, on] of [["tab-metrics", !tryIt], ["tab-try", tryIt]]) {
      $(id).setAttribute("aria-selected", on ? "true" : "false");
      $(id).tabIndex = on ? 0 : -1;
    }
    if (tryIt) (play.ep === "fetch" ? $("p-url") : $("p-q")).focus({ preventScroll: true });
    else if (last) render(last); // a chart drawn while hidden has no width
  }

  let last = null;
  let failures = 0;
  function render(d) {
    last = d;
    renderHeader(d);
    renderRoutes(d);
    renderKPIs(d);
    renderChart(d);
    renderProviders(d);
    renderEndpoints(d);
    renderCache(d);
    renderRecent(d);
  }

  async function tick() {
    if (document.hidden) return;
    try {
      const res = await fetch("/v1/stats", { cache: "no-store", headers: { Accept: "application/json" } });
      if (!res.ok) throw new Error("status " + res.status);
      render(await res.json());
      failures = 0;
      $("live").className = "live on";
      $("live-text").textContent = "live · " + new Date().toLocaleTimeString();
    } catch (e) {
      failures++;
      $("live").className = "live off";
      $("live-text").textContent = "unreachable" + (failures > 1 ? " (" + failures + " tries)" : "");
    }
  }

  tick();
  setInterval(tick, REFRESH_MS);
  document.addEventListener("visibilitychange", () => { if (!document.hidden) tick(); });
  setupPlayground();
  setupViews();
})();
