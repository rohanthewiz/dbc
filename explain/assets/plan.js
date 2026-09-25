// dbc's interactive plan view: a tidy-tree graph of the steps with
// heat-colored cards and data-flow edges, a flame (icicle) view, the
// insights with copyable fixes, and a detail panel for the selected step.
//
// ONE MODULE, TWO HOMES. The standalone page (Plan.HTML: `dbc explain
// --open`, the TUI's `b`, dbc web's "open as page") inlines this file, and
// dbc web's Plan tab loads it as /static/js/plan.js — so the two run the
// same code and cannot drift. It draws only inside the element it is given
// (every lookup is scoped to it, every rule of plan.css is under
// .dbc-plan), so it can live inside the workbench.
//
//   DbcPlan.mount(root, doc, opts) → { destroy, select, setTab, fit }
//
// doc is the Document JSON (explain/json.go): the plan plus its headline
// and the metrics it can be viewed by. Each node carries `weights` — its own
// share of every metric, computed in Go — so sizes here match the terminal.
//
// opts, all optional (the standalone page passes only hash):
//   hash      "#flame" opens on the flame view (a deep link)
//   embedded  sized to fill a pane, not a window
//   keys(e)   false when a key is not the view's (the host's editor has it)
//   copy(text, what)   the host's clipboard — it logs the copy
//   onInsert(sql)      offer "⤓ Insert" beside a finding's Copy: the host
//                      puts the SQL in its editor (never runs it)
//   compare   {text, good}: this plan against the last one of the statement
//   actions   [{label, title, act}] buttons for the header (the host's
//             re-explain, copy, open-as-page …)
//   onAsk(title)       offer "✦ Ask" on the selected step's detail: the
//                      host opens its assistant with a question drafted
//                      about the step (title is the TUI's Node.Title)
//
// The standalone page inlines this file in a script element, so
// it must never contain the characters that close one.
(function (global) {
  "use strict";

  // The skeleton every mount draws. data-p names the parts; nothing has an
  // id, so two views (or a view and the workbench) never collide.
  const SKELETON =
    '<header>' +
    '<div class="hrow"><span class="badge">◈ dbc</span><h1 data-p="headline"></h1>' +
    '<div class="acts" data-p="acts"></div>' +
    '<button class="iconbtn" data-p="themeBtn" title="Switch light / dark">◐</button></div>' +
    '<div class="meta" data-p="meta"></div>' +
    '<div class="notes" data-p="notes"></div>' +
    '<details class="stmt" data-p="stmtBox" hidden><summary>Statement</summary><pre data-p="stmt"></pre></details>' +
    '</header>' +
    '<main>' +
    '<section class="viz">' +
    '<div class="toolbar">' +
    '<div class="seg" data-p="tabs"><button data-tab="graph" class="on">Graph</button><button data-tab="flame">Flame</button></div>' +
    '<div class="seg" data-p="metrics" title="What bars, colors and flame widths measure"></div>' +
    '<span class="spacer"></span>' +
    '<div class="legend"><span>cool</span><span class="ramp" data-p="ramp"></span><span>hot</span><span data-p="legendWhat"></span></div>' +
    '<div class="seg" data-p="zoomCtl"><button data-z="out" title="Zoom out">−</button><button data-z="in" title="Zoom in">+</button><button data-z="fit" title="Fit (f)">Fit</button></div>' +
    '</div>' +
    '<div class="pane" data-p="graph">' +
    '<div data-p="stage"><svg data-p="edges"></svg><div data-p="cards"></div></div>' +
    '<div class="hint">drag to pan · wheel to zoom · ←↑↓→ to walk · Enter folds</div>' +
    '</div>' +
    '<div class="pane" data-p="flame" hidden><div class="crumbs" data-p="crumbs"></div><div data-p="fbox"></div></div>' +
    '</section>' +
    '<aside>' +
    '<section class="box" data-p="detailBox"><h2>Step</h2><div data-p="detail" class="empty">Select a step.</div></section>' +
    '<section class="box"><h2 data-p="insHead">Insights</h2><div data-p="insights"></div></section>' +
    '</aside>' +
    '</main>';

  // ==========================================================================
  // Formatting — ports of explain/plan.go's Fmt* so the page and the terminal
  // agree to the digit. Pure, so shared by every mount.
  // ==========================================================================
  const trimZero = s => s.includes(".") ? s.replace(/0+$/, "").replace(/\.$/, "") : s;
  const goRound = v => Math.sign(v) * Math.round(Math.abs(v));
  function fmtRows(v) {
    const a = Math.abs(v);
    if (a >= 1e9) return trimZero((v / 1e9).toFixed(1)) + "G";
    if (a >= 1e6) return trimZero((v / 1e6).toFixed(1)) + "M";
    if (a >= 1e4) return trimZero((v / 1e3).toFixed(1)) + "k";
    if (a >= 10 || v === Math.trunc(v)) return v.toFixed(0);
    return trimZero(v.toFixed(1));
  }
  function fmtCount(v) {
    const s = String(Math.abs(goRound(v)));
    return (v < 0 && goRound(v) !== 0 ? "-" : "") + s.replace(/\B(?=(\d{3})+(?!\d))/g, ",");
  }
  function fmtMs(ms) {
    if (ms <= 0) return "0 ms";
    if (ms < 1) return (ms * 1000).toFixed(0) + " µs";
    if (ms < 100) return trimZero(ms.toFixed(1)) + " ms";
    if (ms < 1000) return ms.toFixed(0) + " ms";
    if (ms < 60000) return (ms / 1000).toFixed(2) + " s";
    const s = Math.floor(ms / 1000);
    return Math.floor(s / 60) + "m" + String(s % 60).padStart(2, "0") + "s";
  }
  const fmtCost = c => c < 10 ? trimZero(c.toFixed(2)) : fmtRows(c);
  function fmtKB(kb) {
    if (kb >= 1024 * 1024) return trimZero((kb / 1024 / 1024).toFixed(1)) + " GB";
    if (kb >= 1024) return trimZero((kb / 1024).toFixed(1)) + " MB";
    return kb.toFixed(0) + " kB";
  }
  function fmtMetric(m, v) {
    if (m === "time") return fmtMs(v);
    if (m === "cost") return fmtCost(v);
    if (m === "rows") return fmtRows(v);
    return "";
  }
  const fmtRowCount = v => (v > 0 && v < 10 && v !== Math.trunc(v)) ? fmtRows(v) : fmtCount(v);
  const METRIC_LABEL = { time: "Time", cost: "Cost", rows: "Rows", shape: "Shape" };
  const SEV_RANK = { crit: 0, warn: 1, info: 2 };
  const SEV_GLYPH = { crit: "✖", warn: "▲", info: "●" };

  // ==========================================================================
  // Node helpers — mirrors of Node.Target / Summary / Inclusive in plan.go.
  // ==========================================================================
  const GLYPH = { scan: "▤", index: "◇", join: "⋈", sort: "⇅", agg: "Σ", limit: "≤", filter: "⊃", hash: "#",
    subquery: "↻", set: "∪", parallel: "∥", modify: "✎", result: "◈" };
  const glyph = n => GLYPH[n.kind] || "•";
  function target(n) {
    let s = "";
    if (n.relation && n.alias && n.alias !== n.relation) s = n.relation + " " + n.alias;
    else s = n.relation || n.alias || "";
    if (n.index) s += (s ? " using " : "") + n.index;
    return s;
  }
  const title = n => target(n) ? n.op + " · " + target(n) : n.op;
  const SUMMARY_KEYS = ["Hash Cond", "Merge Cond", "Join Filter", "Index Cond", "Condition", "Recheck Cond", "Filter",
    "Key", "Lookup", "Sort Key", "Group Key", "Cache Key", "Limit", "Range", "Extra"];
  function prop(n, k) { const p = (n.props || []).find(p => p.key === k); return p ? p.value : null; }
  function summary(n) { for (const k of SUMMARY_KEYS) { const v = prop(n, k); if (v != null) return v; } return ""; }
  const selfW = (n, m) => (n.weights && n.weights[m]) || 0;
  function countBelow(n) { let c = 0; (n.children || []).forEach(k => c += 1 + countBelow(k)); return c; }

  function hexRGB(h) {
    h = h.replace("#", "");
    if (h.length === 3) h = h.split("").map(c => c + c).join("");
    const v = parseInt(h, 16);
    return [(v >> 16) & 255, (v >> 8) & 255, v & 255];
  }
  function mix(a, b, t) {
    const x = hexRGB(a), y = hexRGB(b);
    return "rgb(" + x.map((c, i) => Math.round(c + (y[i] - c) * t)).join(",") + ")";
  }
  const esc = s => String(s == null ? "" : s).replace(/[&<>"']/g, c => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));

  function highlightSQL(sql) {
    const KW = new Set(("select from where and or not in is null join inner left right full outer cross on using group by order " +
      "having limit offset as with union all distinct insert into values update set delete returning case when then else end " +
      "exists between like ilike asc desc create index table view explain analyze materialized lateral over partition window " +
      "true false any some fetch first rows only for recursive").split(" "));
    const re = /('(?:[^']|'')*')|(--[^\n]*|\/\*[\s\S]*?\*\/)|(\b\d+(?:\.\d+)?\b)|([A-Za-z_][A-Za-z0-9_$]*)|([\s\S])/g;
    let out = "", m;
    while ((m = re.exec(sql))) {
      if (m[1]) out += '<span class="str">' + esc(m[1]) + "</span>";
      else if (m[2]) out += '<span class="cm">' + esc(m[2]) + "</span>";
      else if (m[3]) out += '<span class="nm">' + esc(m[3]) + "</span>";
      else if (m[4]) out += KW.has(m[4].toLowerCase()) ? '<span class="kw">' + esc(m[4]) + "</span>" : esc(m[4]);
      else out += esc(m[5]);
    }
    return out;
  }

  function fallbackCopy(text) {
    const ta = document.createElement("textarea");
    ta.value = text; ta.style.position = "fixed"; ta.style.opacity = "0";
    document.body.appendChild(ta); ta.select();
    let ok = false; try { ok = document.execCommand("copy"); } catch (e) { ok = false; }
    document.body.removeChild(ta);
    return ok;
  }

  // ==========================================================================
  // mount draws one plan into root and wires it up. Everything a view
  // remembers lives in this closure, so views are independent.
  // ==========================================================================
  function mount(root, DOC, opts) {
    opts = opts || {};
    root.classList.add("dbc-plan");
    if (opts.embedded) root.classList.add("embedded");
    if (!root.dataset.theme) root.dataset.theme = "dark";
    root.innerHTML = SKELETON;
    const $ = name => root.querySelector('[data-p="' + name + '"]');
    const $$ = sel => root.querySelectorAll(sel);

    const ROOT = DOC.root;
    const NODES = [];               // by id, preorder
    (function index(n, parent) {
      n.parent = parent; NODES[n.id] = n;
      (n.children || []).forEach(c => index(c, n));
    })(ROOT, null);
    const INSIGHTS = DOC.insights || [];
    const BY_NODE = {};
    INSIGHTS.forEach(i => { if (i.node >= 0) (BY_NODE[i.node] = BY_NODE[i.node] || []).push(i); });

    const state = {
      metric: DOC.metric || (DOC.metrics || ["shape"])[0],
      sel: null, tab: "graph",
      collapsed: new Set(),
      flameRoot: ROOT.id,
      view: { k: 1, tx: 0, ty: 0 },
      moved: false, // the user panned or zoomed: stop refitting on resize
    };
    // A very large plan starts folded below depth 5, so the first view is a
    // map rather than a wall; every fold is one click (or Enter) to open.
    if (NODES.length > 120) NODES.forEach(n => { if (n.depth === 5 && (n.children || []).length) state.collapsed.add(n.id); });

    const factorText = n => {
      const f = n.misestimate || 0;
      if (!DOC.analyzed || !f) return "";
      if (f >= 2) return "×" + fmtRows(Math.round(f)) + "↑";
      if (f <= 0.5) return "×" + fmtRows(Math.round(1 / f)) + "↓";
      return "";
    };
    function rowsText(n) {
      if (n.never_executed) return "never ran";
      if (n.has_actual) return fmtRowCount(n.rows_out || 0) + " rows";
      if (n.has_est) return "~" + fmtRowCount(n.rows_out || 0) + " rows";
      return "";
    }
    const inclCache = {};
    function incl(n, m) {
      const key = m + ":" + n.id;
      if (key in inclCache) return inclCache[key];
      let t = selfW(n, m);
      (n.children || []).forEach(c => t += incl(c, m));
      return inclCache[key] = t;
    }
    function share(n, m) { const t = incl(ROOT, m); return t > 0 ? selfW(n, m) / t : 0; }
    function worstSev(n) {
      const list = BY_NODE[n.id]; if (!list) return null;
      return list.reduce((a, i) => SEV_RANK[i.severity] < SEV_RANK[a] ? i.severity : a, "info");
    }
    const kids = n => state.collapsed.has(n.id) ? [] : (n.children || []);

    // Color: heat by a step's own share of the metric, accent → warn → err,
    // read from the live CSS variables on this view, so the light theme
    // re-colors it for free.
    const cssVar = name => getComputedStyle(root).getPropertyValue("--" + name).trim();
    function heat(s) {
      const acc = cssVar("accent"), warn = cssVar("warn"), err = cssVar("err");
      if (s <= 0.4) return mix(acc, warn, s / 0.4);
      return mix(warn, err, Math.min((s - 0.4) / 0.4, 1));
    }

    // ========================================================================
    // Header
    // ========================================================================
    function renderHeader() {
      $("headline").textContent = DOC.headline || "Plan";
      const meta = [];
      meta.push("<span>engine <b>" + esc(DOC.engine) + "</b></span>");
      if (DOC.conn) meta.push("<span>connection <b>" + esc(DOC.conn) + "</b></span>");
      meta.push('<span class="chip ' + (DOC.analyzed ? "on" : "") + '">' +
        (DOC.analyzed ? "analyzed — measured per step" : DOC.measured ? "estimated plan · measured run" : "estimated — not executed") + "</span>");
      if (opts.compare && opts.compare.text) {
        meta.push('<span class="chip ' + (opts.compare.good ? "good" : "bad") + '" title="Against the last plan of this statement">vs last: ' +
          esc(opts.compare.text) + "</span>");
      }
      if (DOC.command) meta.push('<span class="mono" title="The EXPLAIN dbc sent: ' + esc(DOC.command) + '">' +
        esc(DOC.command.length > 90 ? DOC.command.slice(0, 89) + "…" : DOC.command) + "</span>");
      $("meta").innerHTML = meta.join("");
      $("notes").innerHTML = (DOC.notes || []).map(n => '<div class="note">' + esc(n) + "</div>").join("");
      if (DOC.statement) {
        const box = $("stmtBox");
        box.hidden = false;
        box.open = DOC.statement.length < 600;
        $("stmt").innerHTML = highlightSQL(DOC.statement);
      }
      const ms = $("metrics");
      ms.innerHTML = (DOC.metrics || []).map((m, i) =>
        '<button data-m="' + m + '" title="' + (m === "shape"
          ? "The engine reported no costs or timings for this; sizes are a rough heuristic by kind of step (key " + (i + 1) + ")"
          : "Size and color by " + m + " (key " + (i + 1) + ")") + '">' + METRIC_LABEL[m] + (m === "shape" ? " ≈" : "") + "</button>").join("");
      ms.onclick = e => { const b = e.target.closest("button"); if (b) setMetric(b.dataset.m); };
      $("ramp").style.background = "linear-gradient(90deg," + heat(0) + "," + heat(0.4) + "," + heat(0.8) + ")";
    }
    function renderActions() {
      const acts = $("acts");
      acts.replaceChildren();
      (opts.actions || []).forEach(a => {
        const b = document.createElement("button");
        b.className = "iconbtn";
        b.type = "button";
        b.textContent = a.label;
        if (a.title) b.title = a.title;
        b.onclick = () => a.act();
        acts.append(b);
      });
    }
    function setMetric(m) {
      if (!(DOC.metrics || []).includes(m)) return;
      state.metric = m;
      $$('[data-p="metrics"] button').forEach(b => b.classList.toggle("on", b.dataset.m === m));
      $("legendWhat").textContent = "· own share of " + (m === "shape" ? "work (heuristic)" : m);
      renderGraph(false); renderFlame(); renderDetail();
    }

    // ========================================================================
    // Insights
    // ========================================================================
    function copyText(text, btn) {
      const done = () => { btn.textContent = "Copied"; btn.classList.add("done");
        setTimeout(() => { btn.textContent = "Copy"; btn.classList.remove("done"); }, 1400); };
      if (opts.copy) { opts.copy(text, "the suggested statement"); done(); return; }
      if (navigator.clipboard && window.isSecureContext) {
        navigator.clipboard.writeText(text).then(done, () => fallbackCopy(text) && done());
      } else if (fallbackCopy(text)) done();
    }
    function insightHTML(i, idx) {
      const insert = opts.onInsert ? '<button class="copy" data-insert="' + idx + '" title="Put it at the end of the editor — review it there; nothing runs">⤓ Insert</button>' : "";
      return '<div class="ins ' + i.severity + (i.node < 0 ? " nofocus" : "") + '" data-i="' + idx + '">' +
        '<div class="ititle"><i>' + SEV_GLYPH[i.severity] + "</i>" + esc(i.title) + "</div>" +
        (i.detail ? '<div class="idet">' + esc(i.detail) + "</div>" : "") +
        (i.fix ? '<div class="ifix">' + esc(i.fix) + "</div>" : "") +
        (i.sql ? '<div class="sqlbox"><code>' + esc(i.sql) + '</code><div class="sqlbtns"><button class="copy" data-copy="' + idx + '">Copy</button>' +
          insert + "</div></div>" : "") +
        "</div>";
    }
    function bindInsights(el) {
      el.onclick = e => {
        const cp = e.target.closest("[data-copy]");
        if (cp) { copyText(INSIGHTS[+cp.dataset.copy].sql, cp); e.stopPropagation(); return; }
        const ins = e.target.closest("[data-insert]");
        if (ins) { opts.onInsert(INSIGHTS[+ins.dataset.insert].sql); e.stopPropagation(); return; }
        const card = e.target.closest(".ins");
        if (!card) return;
        const found = INSIGHTS[+card.dataset.i];
        if (found.node >= 0) { reveal(found.node); select(found.node, true); }
      };
    }
    function renderInsights() {
      const serious = INSIGHTS.filter(i => i.severity !== "info").length;
      $("insHead").textContent = "Insights" + (INSIGHTS.length ? " · " + INSIGHTS.length +
        (serious ? " (" + serious + " to act on)" : "") : "");
      const el = $("insights");
      el.innerHTML = INSIGHTS.length ? INSIGHTS.map(insightHTML).join("") : '<div class="empty">No findings.</div>';
      bindInsights(el);
    }

    // ========================================================================
    // Graph: a top-down tidy tree. Each subtree is given the width its visible
    // leaves need; a parent is centered over its first and last child. That
    // never overlaps (subtrees own disjoint horizontal spans) and keeps every
    // edge short.
    // ========================================================================
    const CW = 250, CH = 108, HG = 30, VG = 64;
    let layout = { pos: {}, w: 0, h: 0 };
    function computeLayout() {
      const span = {}, pos = {};
      (function measure(n) {
        const ks = kids(n);
        let s = 0;
        ks.forEach((c, i) => s += measure(c) + (i ? HG : 0));
        return span[n.id] = Math.max(CW, s);
      })(ROOT);
      let maxY = 0;
      (function place(n, x0, depth) {
        const ks = kids(n), y = depth * (CH + VG);
        maxY = Math.max(maxY, y);
        if (!ks.length) { pos[n.id] = { x: x0 + (span[n.id] - CW) / 2, y }; return; }
        let total = 0; ks.forEach((c, i) => total += span[c.id] + (i ? HG : 0));
        let cx = x0 + (span[n.id] - total) / 2;
        ks.forEach(c => { place(c, cx, depth + 1); cx += span[c.id] + HG; });
        const a = pos[ks[0].id], b = pos[ks[ks.length - 1].id];
        pos[n.id] = { x: (a.x + b.x) / 2, y };
      })(ROOT, 0, 0);
      layout = { pos, w: span[ROOT.id], h: maxY + CH + 30 };
    }
    function cardHTML(n) {
      const m = state.metric, s = share(n, m), h = heat(s), sev = worstSev(n);
      // a step with none of the metric (a label step, a synthetic root) shows
      // nothing rather than a row of "0 · 0%" that reads like a measurement
      const val = m === "shape" || selfW(n, m) <= 0 ? "" : fmtMetric(m, selfW(n, m)) + " · " + Math.round(s * 100) + "%";
      const hot = s >= 0.02;
      const p = layout.pos[n.id];
      const style = "left:" + p.x + "px;top:" + p.y + "px;border-color:" + (hot ? h : "var(--line)") + ";background:" +
        (hot ? "color-mix(in srgb," + h + " " + Math.round(8 + 22 * Math.min(s / 0.6, 1)) + "%, var(--panel))" : "var(--panel)");
      const sum = summary(n), tg = target(n), f = factorText(n);
      const tip = title(n) + (sum ? "\n" + sum : "") + (n.relationship ? "\n(" + n.relationship + ")" : "");
      return '<div class="card' + (n.never_executed ? " never" : "") + ((n.children || []).length ? " fold" : "") + '" data-id="' + n.id + '" style="' + style +
        '" title="' + esc(tip) + '">' +
        '<div class="c-top"><span class="glyph" style="color:' + (hot ? h : "var(--accent)") + '">' + glyph(n) + "</span>" +
        '<span class="op">' + esc(n.op) + "</span>" + (sev ? '<span class="sev ' + sev + '">' + SEV_GLYPH[sev] + "</span>" : "") + "</div>" +
        '<div class="c-target">' + (tg ? esc(tg) : "&nbsp;") + "</div>" +
        '<div class="c-sum mono">' + (sum ? esc(sum) : "&nbsp;") + "</div>" +
        '<div class="c-nums num"><span>' + esc(rowsText(n)) + "</span>" + (f ? '<span class="factor" title="actual vs estimated rows">' + f + "</span>" : "") +
        '<span class="val">' + esc(val) + "</span></div>" +
        '<div class="c-bar"><i style="width:' + (s * 100).toFixed(1) + "%;background:" + h + '"></i></div>' +
        ((n.children || []).length ? '<button class="tog" data-tog="' + n.id + '" title="Fold / unfold (Enter)">' +
          (state.collapsed.has(n.id) ? "▸" : "▾") + "</button>" : "") +
        "</div>";
    }
    function renderGraph(refit) {
      computeLayout();
      const cards = [], paths = [];
      const P = layout.pos;
      (function walk(n) {
        const p = P[n.id];
        cards.push(cardHTML(n));
        if (state.collapsed.has(n.id)) {
          cards.push('<div class="more" data-tog="' + n.id + '" style="left:' + (p.x + CW / 2 - 40) + "px;top:" + (p.y + CH + 16) +
            'px">+' + countBelow(n) + " steps</div>");
        }
        kids(n).forEach(c => {
          const q = P[c.id];
          const x1 = p.x + CW / 2, y1 = p.y + CH, x2 = q.x + CW / 2, y2 = q.y, my = (y1 + y2) / 2;
          // edge weight: log of the rows flowing up from the child, so a
          // 200,000-row stream is visibly fatter than a 5-row one without
          // swamping the drawing
          const rows = c.rows_out || 0;
          const w = rows > 0 ? Math.min(1.5 + Math.log10(rows + 1) * 1.4, 10) : 1.5;
          paths.push('<path data-e="' + c.id + '" d="M' + x1 + "," + y1 + " C" + x1 + "," + my + " " + x2 + "," + my + " " + x2 + "," + y2 +
            '" stroke-width="' + w.toFixed(1) + '"><title>' + esc((rows ? fmtCount(rows) + " rows" : "") + (c.relationship ? " · " + c.relationship : "")) + "</title></path>");
          // "Outer" on an only child says nothing (every single input is the
          // outer one); on a join's two inputs, or a SubPlan, it is the point
          const rel = (n.children.length > 1 || !/^(Outer|Inner)$/.test(c.relationship || "")) ? c.relationship : "";
          const lbl = [rel, rows ? fmtRows(rows) + " rows" : ""].filter(Boolean).join(" · ");
          if (lbl) paths.push('<text x="' + ((x1 + x2) / 2 + 6) + '" y="' + (my + 4) + '">' + esc(lbl) + "</text>");
          walk(c);
        });
      })(ROOT);
      $("cards").innerHTML = cards.join("");
      const svg = $("edges");
      svg.setAttribute("width", layout.w); svg.setAttribute("height", layout.h);
      svg.innerHTML = paths.join("");
      paintSelection();
      if (refit) fit();
    }
    function applyView() {
      const v = state.view;
      $("stage").style.transform = "translate(" + v.tx + "px," + v.ty + "px) scale(" + v.k + ")";
    }
    function fit() {
      const g = $("graph"), W = g.clientWidth, H = g.clientHeight;
      if (!W || !H) return;
      const pad = 30, k = Math.min((W - 2 * pad) / layout.w, (H - 2 * pad) / layout.h, 1.15);
      state.view.k = Math.max(k, 0.12);
      state.view.tx = (W - layout.w * state.view.k) / 2;
      state.view.ty = Math.max(pad, (H - layout.h * state.view.k) / 2);
      applyView();
    }
    function zoomAt(f, px, py) {
      state.moved = true;
      const v = state.view, k = Math.min(Math.max(v.k * f, 0.08), 3);
      const r = k / v.k;
      v.tx = px - (px - v.tx) * r; v.ty = py - (py - v.ty) * r; v.k = k;
      applyView();
    }
    // center brings a card into view if it is (partly) off screen.
    function center(id, force) {
      const p = layout.pos[id]; if (!p) return;
      const g = $("graph"), v = state.view;
      const x = p.x * v.k + v.tx, y = p.y * v.k + v.ty;
      if (!force && x > 10 && y > 10 && x + CW * v.k < g.clientWidth - 10 && y + CH * v.k < g.clientHeight - 10) return;
      v.tx = g.clientWidth / 2 - (p.x + CW / 2) * v.k;
      v.ty = g.clientHeight / 2 - (p.y + CH / 2) * v.k;
      const st = $("stage");
      st.style.transition = "transform .25s ease"; applyView();
      setTimeout(() => st.style.transition = "", 260);
    }
    function paintSelection() {
      $$(".card.sel").forEach(c => c.classList.remove("sel"));
      if (state.sel != null) {
        const c = root.querySelector('.card[data-id="' + state.sel + '"]');
        if (c) c.classList.add("sel");
      }
    }
    function paintPath(id) {
      $$(".card.path").forEach(c => c.classList.remove("path"));
      $$('[data-p="edges"] path.hot').forEach(e => e.classList.remove("hot"));
      if (id == null) return;
      for (let n = NODES[id]; n; n = n.parent) {
        const c = root.querySelector('.card[data-id="' + n.id + '"]'); if (c) c.classList.add("path");
        const e = root.querySelector('[data-p="edges"] path[data-e="' + n.id + '"]'); if (e) e.classList.add("hot");
      }
    }
    // reveal unfolds every ancestor of a step, so selecting it from an
    // insight or the flame never selects something hidden inside a fold.
    function reveal(id) {
      let changed = false;
      for (let n = NODES[id].parent; n; n = n.parent) if (state.collapsed.delete(n.id)) changed = true;
      if (changed) renderGraph(false);
    }
    function toggle(id) {
      if (!(NODES[id].children || []).length) return;
      if (state.collapsed.has(id)) state.collapsed.delete(id); else state.collapsed.add(id);
      renderGraph(false); center(id, false);
    }
    function select(id, scroll) {
      state.sel = id;
      paintSelection(); renderDetail(); renderFlame();
      if (scroll && state.tab === "graph") center(id, false);
    }

    function bindGraph() {
      const g = $("graph");
      let drag = null;
      g.addEventListener("pointerdown", e => {
        if (e.button !== 0) return;
        const tog = e.target.closest("[data-tog]");
        if (tog) { toggle(+tog.dataset.tog); return; }
        const card = e.target.closest(".card");
        if (card) { select(+card.dataset.id, false); return; }
        drag = { x: e.clientX, y: e.clientY, tx: state.view.tx, ty: state.view.ty };
        g.classList.add("panning"); g.setPointerCapture(e.pointerId);
      });
      g.addEventListener("pointermove", e => {
        if (drag) {
          state.view.tx = drag.tx + e.clientX - drag.x; state.view.ty = drag.ty + e.clientY - drag.y; applyView();
          state.moved = true;
          return;
        }
        const card = e.target.closest(".card");
        paintPath(card ? +card.dataset.id : null);
      });
      const end = () => { drag = null; g.classList.remove("panning"); };
      g.addEventListener("pointerup", end); g.addEventListener("pointercancel", end);
      g.addEventListener("pointerleave", () => paintPath(null));
      g.addEventListener("wheel", e => {
        e.preventDefault();
        const r = g.getBoundingClientRect();
        zoomAt(Math.exp(-e.deltaY * 0.0015), e.clientX - r.left, e.clientY - r.top);
      }, { passive: false });
      g.addEventListener("dblclick", e => { const card = e.target.closest(".card"); if (card) toggle(+card.dataset.id); });
      $("zoomCtl").onclick = e => {
        const b = e.target.closest("button"); if (!b) return;
        const W = g.clientWidth / 2, H = g.clientHeight / 2;
        if (b.dataset.z === "in") zoomAt(1.25, W, H); else if (b.dataset.z === "out") zoomAt(0.8, W, H); else { state.moved = false; fit(); }
      };
    }

    // ========================================================================
    // Flame (icicle): each block as wide as its INCLUSIVE weight within the
    // current zoom root; a parent's own share is the gap its children leave.
    // ========================================================================
    function renderFlame() {
      if (state.tab !== "flame") return;
      const m = state.metric, froot = NODES[state.flameRoot];
      let total = incl(froot, m);
      // a metric that is zero everywhere (a plan with no rows reported) would
      // draw nothing: fall back to counting steps so the shape is still visible
      const byCount = total <= 0;
      const w = n => byCount ? 1 + countBelow(n) : incl(n, m);
      if (byCount) total = w(froot);
      const ROW = 32, blocks = [];
      let maxD = 0;
      (function place(n, x, d) {
        const wd = w(n) / total;
        if (wd <= 0) return;
        maxD = Math.max(maxD, d);
        const s = share(n, m), h = heat(s);
        const val = m === "shape" || byCount ? "" : " " + fmtMetric(m, incl(n, m));
        const tg = target(n);
        const tip = title(n) + "\n" + (m === "shape" ? "" : "self " + fmtMetric(m, selfW(n, m)) + " (" + Math.round(s * 100) + "%) · total" + val) +
          (rowsText(n) ? "\n" + rowsText(n) : "");
        blocks.push('<div class="fb' + (n.id === state.sel ? " sel" : "") + (n.never_executed ? " never" : "") + '" data-id="' + n.id +
          '" style="left:' + (x * 100).toFixed(3) + "%;width:calc(" + (wd * 100).toFixed(3) + "% - 2px);top:" + d * ROW +
          "px;background:" + h + '" title="' + esc(tip) + '">' + glyph(n) + " " + esc(n.op) + (tg ? " · " + esc(tg) : "") +
          "<b>" + esc(val) + "</b></div>");
        let cx = x;
        (n.children || []).forEach(c => { place(c, cx, d + 1); cx += w(c) / total; });
      })(froot, 0, 0);
      const box = $("fbox");
      box.style.height = (maxD + 1) * ROW + "px";
      box.innerHTML = blocks.join("");
      const trail = [];
      for (let n = froot; n; n = n.parent) trail.unshift(n);
      $("crumbs").innerHTML = "<span>" + (byCount ? "width = steps below" :
        "width = total " + (m === "shape" ? "work (heuristic)" : m) + " incl. children") + " ·</span>" +
        trail.map((n, i) => i === trail.length - 1 ? "<span>" + esc(n.op) + "</span>" :
          '<a data-z="' + n.id + '">' + esc(n.op) + "</a> ›").join(" ") +
        '<span class="grow"></span><span>click a block to zoom in</span>';
    }
    function bindFlame() {
      $("fbox").onclick = e => {
        const b = e.target.closest(".fb"); if (!b) return;
        const id = +b.dataset.id;
        // clicking the current root zooms back out one level; any other block
        // with children becomes the new root
        if (id === state.flameRoot && NODES[id].parent) state.flameRoot = NODES[id].parent.id;
        else if ((NODES[id].children || []).length) state.flameRoot = id;
        reveal(id); select(id, false);
      };
      $("crumbs").onclick = e => {
        const a = e.target.closest("[data-z]"); if (!a) return;
        state.flameRoot = +a.dataset.z; renderFlame();
      };
    }

    // ========================================================================
    // Detail panel
    // ========================================================================
    function renderDetail() {
      const el = $("detail");
      const n = NODES[state.sel];
      if (!n) { el.className = "empty"; el.textContent = "Select a step."; return; }
      el.className = "";
      const m = state.metric, rows = [];
      const add = (k, v) => { if (v !== "" && v != null) rows.push("<dt>" + k + "</dt><dd class=\"num\">" + v + "</dd>"); };
      const bar = s => '<span class="share"><i style="width:' + (s * 100).toFixed(1) + "%;background:" + heat(s) + '"></i></span>';
      if (n.has_actual || n.total_ms) {
        const st = share(n, "time");
        add("Time", fmtMs(n.total_ms || 0) + " total · " + fmtMs(n.self_ms || 0) + " self (" + Math.round(st * 100) + "%)" + bar(st));
      }
      if (n.never_executed) add("Rows", "never executed");
      else if (n.has_actual) {
        let r = fmtCount(n.rows_out || 0) + " actual";
        if (n.has_est) r += " · " + fmtCount((n.est_rows || 0) * Math.max(n.loops || 1, 1)) + " estimated";
        const f = factorText(n);
        if (f) r += ' <span class="factor">' + f + "</span>";
        add("Rows", r);
      } else if (n.has_est) add("Rows", "~" + fmtRowCount(n.rows_out || 0) + " estimated");
      if ((n.loops || 0) > 1) add("Loops", fmtCount(n.loops) + (n.participants > 1 ? " (" + fmtCount(n.participants) + " parallel workers)" : ""));
      if (n.rows_removed) add("Removed", fmtCount(n.rows_removed * Math.max(n.loops || 1, 1)) + " rows by filter");
      if (n.has_cost) {
        const sc = share(n, "cost");
        add("Cost", fmtCost(n.startup_cost || 0) + " → " + fmtCost(n.total_cost || 0) + " · self " + fmtCost(n.self_cost || 0) + bar(sc));
      }
      if (n.width) add("Width", fmtCount(n.width) + " bytes/row");
      if (n.shared_hit || n.shared_read) add("Buffers", fmtCount(n.shared_hit || 0) + " hit · " + fmtCount(n.shared_read || 0) + " read");
      if (n.temp_blocks) add("Temp", fmtCount(n.temp_blocks) + " blocks written");
      if (n.sort_spill || n.spill_kb) add("Spill", '<span class="factor">' + (n.spill_kb ? fmtKB(n.spill_kb) : "yes") + " to disk</span>");
      if ((n.batches || 0) > 1) add("Batches", fmtCount(n.batches));
      if (n.workers_planned) add("Workers", fmtCount(n.workers_launched || 0) + " of " + fmtCount(n.workers_planned) + " launched");
      if (n.table_rows) add("Table", fmtCount(n.table_rows) + " rows");
      if (m === "shape" || m === "rows") add("Share", (m === "rows" ? fmtRows(selfW(n, m)) + " · " : "") + Math.round(share(n, m) * 100) + "%" + bar(share(n, m)));
      const props = (n.props || []).map(p => '<div><span class="k">' + esc(p.key) + '</span><span class="v mono">' + esc(p.value) + "</span></div>").join("");
      const ins = (BY_NODE[n.id] || []).map(i => insightHTML(i, INSIGHTS.indexOf(i))).join("");
      const ask = opts.onAsk ? '<button class="copy dask" data-ask="1" title="Ask the assistant what this step is doing">✦ Ask</button>' : "";
      el.innerHTML =
        '<div class="dtitle"><span class="glyph">' + glyph(n) + "</span>" + esc(n.op) + ask + "</div>" +
        '<div class="dsub">' + esc([target(n), n.kind, n.relationship].filter(Boolean).join(" · ")) + "</div>" +
        (rows.length ? '<dl class="stats">' + rows.join("") + "</dl>" : "") +
        (props ? '<div class="props">' + props + "</div>" : "") +
        (ins ? '<div class="dins">' + ins + "</div>" : "");
      const askBtn = el.querySelector("[data-ask]");
      if (askBtn) askBtn.addEventListener("click", () => opts.onAsk(target(n) ? n.op + " · " + target(n) : n.op));
      bindInsights(el);
    }

    // ========================================================================
    // Tabs, keys, theme, boot
    // ========================================================================
    function setTab(t) {
      state.tab = t;
      $$('[data-p="tabs"] button').forEach(b => b.classList.toggle("on", b.dataset.tab === t));
      $("graph").hidden = t !== "graph";
      $("flame").hidden = t !== "flame";
      $("zoomCtl").style.visibility = t === "graph" ? "visible" : "hidden";
      if (t === "flame") renderFlame(); else fit();
    }
    $("tabs").onclick = e => { const b = e.target.closest("button"); if (b) setTab(b.dataset.tab); };
    $("themeBtn").onclick = () => {
      root.dataset.theme = root.dataset.theme === "light" ? "dark" : "light";
      renderHeader(); setMetric(state.metric);
    };
    function onKey(e) {
      if (e.metaKey || e.ctrlKey || e.altKey || /input|textarea|select/i.test(e.target.tagName)) return;
      if (opts.keys && !opts.keys(e)) return;
      const n = NODES[state.sel];
      const go = id => { if (id == null) return; reveal(id); select(id, true); e.preventDefault(); };
      switch (e.key) {
        case "ArrowUp": if (n && n.parent) go(n.parent.id); break;
        case "ArrowDown":
          if (n && (n.children || []).length) { state.collapsed.delete(n.id); go(n.children[0].id); renderGraph(false); paintSelection(); }
          else if (!n) go(ROOT.id);
          break;
        case "ArrowLeft": case "ArrowRight": {
          if (!n || !n.parent) break;
          const sib = n.parent.children, i = sib.indexOf(n) + (e.key === "ArrowLeft" ? -1 : 1);
          if (i >= 0 && i < sib.length) go(sib[i].id);
          break;
        }
        case "Enter": if (n) { toggle(n.id); e.preventDefault(); } break;
        case "f": if (state.tab === "graph") fit(); break;
        case "g": setTab("graph"); break;
        case "Escape": if (state.tab === "flame" && state.flameRoot !== ROOT.id) { state.flameRoot = ROOT.id; renderFlame(); } break;
        default:
          if (/^[1-4]$/.test(e.key)) { const m = (DOC.metrics || [])[+e.key - 1]; if (m) setMetric(m); }
      }
    }
    document.addEventListener("keydown", onKey);
    // Refit whenever the graph pane changes size — a window resize, the
    // layout switching to one column, fonts settling, the tab being shown —
    // until the user takes over the view by panning or zooming; after that
    // their view is left alone.
    const ro = new ResizeObserver(() => { if (state.tab === "graph" && !state.moved) fit(); });
    ro.observe($("graph"));

    renderHeader();
    renderActions();
    renderInsights();
    bindGraph();
    bindFlame();
    // Start on the step the most serious finding is about — that is the
    // question the reader opened the page with — or the root.
    const first = INSIGHTS.find(i => i.node >= 0 && i.severity !== "info");
    state.sel = first ? first.node : ROOT.id;
    if (first) reveal(first.node);
    setMetric(state.metric);
    renderGraph(true);
    requestAnimationFrame(fit);
    // deep link: plan.html#flame opens on the flame view
    if (opts.hash === "#flame") setTab("flame");

    return {
      // destroy lets go of what the view hooked outside its root
      destroy() { document.removeEventListener("keydown", onKey); ro.disconnect(); },
      select(id) { reveal(id); select(id, true); },
      setTab, fit,
      get state() { return { tab: state.tab, sel: state.sel, metric: state.metric, flameRoot: state.flameRoot, collapsed: [...state.collapsed] }; },
    };
  }

  global.DbcPlan = { mount };
})(window);
