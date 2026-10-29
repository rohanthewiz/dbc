// dbc web — the query designer: a design tab's canvas. The page draws and
// edits a design (package designer's Design, kept on the tab as t.design);
// the server judges it on every edit and answers with everything to draw
// (web/design.go):
//
//   edit ─► s.design changed ─► (150 ms) POST …/design ─► analysis:
//             SQL · findings · story · cards · edges · related tables
//                                      └► (600 ms, when valid) POST …/design/plan
//                                           the engine's EXPLAIN, painted on the cards
//
// THE PAGE DECIDES NOTHING about the query: which joins fan out, what the
// SQL is, what is wrong with it — all of that is the designer package's,
// so it is tested in Go and cannot drift from what the server runs. What
// is here is drawing, editing, and keeping the user's place:
//
//   bar      health chips (✕ errors · ⚠ warnings · ⓘ notes) · the plan's
//            estimate · ↶ ↷ · ⟳ re-read the catalog · ⇪ open as a query
//   palette  the connection's tables; "related" first (one foreign key
//            from the design) — a click adds one, a drag places it
//   canvas   a card per table, an edge per join (glyph and 1:N on it);
//            drag a header to move, a column onto another card's to join,
//            the background to pan; wheel pans, Ctrl+wheel zooms
//   side     Query (columns, filters, sort, rows; the selected card or
//            join on top) · Guide (findings with fixes, the plan's view) ·
//            Story (the query in words) · SQL
//
// The tab's editor document holds the SQL (app.js keeps it there, via
// hooks.changed), so ▶ Run, ◈ Explain, history and the assistant all work
// on a design tab exactly as on a query tab.
//
// Undo is the designer's own (Ctrl+Z / Ctrl+Shift+Z on the canvas): a
// stack of designs, pushed before each edit — a fix applied from the Guide
// is one step, so trying one is safe.
(function () {
  "use strict";

  const dbc = window.dbc;
  const { api, el, log } = dbc;

  // the join kinds, as the edge shows them and the inspector explains them
  const KINDS = [
    ["inner", "⋈", "inner join", "rows that match on both sides"],
    ["left", "⟕", "left join", "every row of the left table, matched or not (unmatched: its columns empty)"],
    ["full", "⟗", "full join", "every row of both sides, matched or not"],
    ["exists", "∃", "has any", "keep left rows that have a match — EXISTS: each once, however many match; the right table is only tested"],
    ["notexists", "∄", "has none", "keep left rows with no match — NOT EXISTS, which a NULL cannot empty (unlike NOT IN)"],
    ["summary", "Σ", "summarize", "aggregate the right table per left row first, then attach it: totals without repeating rows"],
  ];
  const GLYPH = Object.fromEntries(KINDS.map((k) => [k[0], k[1]]));

  const AGGS = [["", "—"], ["count", "count"], ["count_distinct", "count distinct"], ["sum", "sum"],
    ["avg", "avg"], ["min", "min"], ["max", "max"]];

  // the filter operators, labelled; which apply depends on the column's
  // family (the server's designer.familyOf) or on an aggregate
  const OPS = {
    eq: "=", ne: "≠ is not", lt: "<", le: "≤", gt: ">", ge: "≥", between: "between", range: "from … up to",
    in: "is one of", notin: "is none of", null: "is empty", notnull: "is not empty",
    contains: "contains", starts: "starts with", ends: "ends with",
    year: "in year", on: "on day", lastdays: "in the last … days",
  };
  const OPS_BY = {
    int: ["eq", "ne", "lt", "le", "gt", "ge", "between", "in", "notin", "null", "notnull"],
    num: ["eq", "ne", "lt", "le", "gt", "ge", "between", "null", "notnull"],
    text: ["eq", "ne", "contains", "starts", "ends", "in", "notin", "lt", "gt", "null", "notnull"],
    date: ["on", "year", "lastdays", "range", "between", "lt", "le", "gt", "ge", "null", "notnull"],
    time: ["on", "year", "lastdays", "range", "between", "lt", "le", "gt", "ge", "null", "notnull"],
    bool: ["eq", "null", "notnull"],
    agg: ["gt", "ge", "lt", "le", "eq", "ne", "between"],
    "": Object.keys(OPS),
  };
  const ARITY = { null: 0, notnull: 0, between: 2, range: 2, in: -1, notin: -1 };
  const LIKE_OPS = ["contains", "starts", "ends"]; // the ops "ignore case" applies to (designer.likeOp)
  const SEV = { error: "✕", warn: "⚠", info: "ⓘ" };
  const CARD_W = 232;

  const emptyDesign = () => ({ tables: [], joins: [], columns: [], filters: [], sorts: [] });
  const LISTS = ["tables", "joins", "columns", "filters", "sorts"];
  // filled gives a design every list, as the page draws it: one the server
  // made (an imported query, designer.Import) writes an empty list as null
  function filled(d) {
    for (const k of LISTS) d[k] = d[k] || [];
    return d;
  }

  // rough is a row count as the cards say it
  function rough(n) {
    if (n < 1000) return String(n);
    if (n < 1e6) return (n < 1e4 ? (n / 1e3).toFixed(1) : Math.round(n / 1e3)) + "K";
    if (n < 1e9) return (n < 1e7 ? (n / 1e6).toFixed(1) : Math.round(n / 1e6)) + "M";
    return (n / 1e9).toFixed(1) + "B";
  }

  // words draws a story or finding line: `code` spans as <code>, the rest
  // as text — never as markup
  function words(text) {
    const f = document.createDocumentFragment();
    String(text).split("`").forEach((part, i) => f.append(i % 2 ? el("code", null, part) : part));
    return f;
  }

  function create(hooks) {
    const states = new Map(); // tab key → state
    let cur = null;           // the tab on screen
    let dom = null;           // the mounted elements
    let stg = null;           // its stage (stage.js)
    let analyzeTimer = 0, planTimer = 0;
    const palettes = new Map(); // ws|conn → {tables, catalog}

    function st(t) {
      let s = states.get(t.key);
      if (!s) {
        s = {
          design: filled(t.design || emptyDesign()), analysis: null, facts: null, plan: null, planNote: "",
          hist: [], fut: [], gen: 0, sel: null, pane: "query", hot: null, view: null, err: "",
          editing: false,
        };
        states.set(t.key, s);
      }
      return s;
    }
    const wsPath = (t, p) => "/api/v1/ws/" + t.ws + p;
    const tableOf = (s, id) => (s.design.tables || []).find((x) => x.id === id);
    const viewOf = (s, id) => s.analysis && (s.analysis.tables || []).find((x) => x.id === id);
    const joinView = (s, id) => s.analysis && (s.analysis.joins || []).find((x) => x.id === id);
    const aliasOf = (s, id) => { const t = tableOf(s, id); return t ? t.alias || t.name : "?"; };

    // ── editing ────────────────────────────────────────────────────────
    // edit changes the design through f, after pushing the undo step, and
    // asks for a new analysis. quiet: no new analysis (a card moved).
    function edit(t, f, quiet) {
      const s = st(t);
      s.hist.push(JSON.stringify(s.design));
      if (s.hist.length > 100) s.hist.shift();
      s.fut = [];
      f(s.design);
      t.design = s.design;
      s.gen++;
      if (quiet) { hooks.saved(t); return; }
      scheduleAnalyze(t);
      if (t === cur) drawSide(); // the inspector follows at once; the rest when the analysis lands
    }
    function undo(t, back) {
      const s = st(t);
      const from = back ? s.hist : s.fut, to = back ? s.fut : s.hist;
      if (!from.length) { hooks.status(back ? "nothing to undo" : "nothing to redo", ""); return; }
      to.push(JSON.stringify(s.design));
      s.design = JSON.parse(from.pop());
      t.design = s.design;
      s.gen++;
      s.sel = null;
      scheduleAnalyze(t, 0);
    }

    function scheduleAnalyze(t, ms, fresh) {
      clearTimeout(analyzeTimer);
      analyzeTimer = setTimeout(() => analyze(t, fresh), ms === undefined ? 150 : ms);
    }

    // analyze asks for the analysis of the design as it is now. An answer
    // to an older design (an edit landed meanwhile) is dropped: another
    // analysis is already on its way.
    async function analyze(t, fresh) {
      const s = st(t);
      if (!t.ws) return; // the tab's workspace is not open yet: connChanged asks again
      const gen = s.gen;
      let r;
      try {
        r = await api("POST", wsPath(t, "/design"), { design: s.design, fresh: !!fresh });
      } catch (e) {
        if (gen !== s.gen) return;
        s.err = e.message;
        if (t === cur) drawAll();
        return;
      }
      if (gen !== s.gen) return;
      land(t, r);
    }

    // land takes an analysis in: its normalized design (aliases, ids) —
    // but with the cards where they are now, since one may have moved while
    // the answer was on its way — and everything drawn from it.
    function land(t, r) {
      const s = st(t);
      const pos = new Map((s.design.tables || []).map((x) => [x.id, x]));
      const d = r.analysis.design;
      for (const x of d.tables || []) {
        const p = pos.get(x.id);
        if (p) { x.x = p.x; x.y = p.y; }
      }
      filled(d);
      s.design = d;
      t.design = d;
      s.analysis = r.analysis;
      s.analyzed = s.gen; // the design this analysis is of
      s.facts = r.catalog || null;
      s.err = "";
      if (s.sel && s.sel.table && !tableOf(s, s.sel.table)) s.sel = null;
      if (s.sel && s.sel.join && !d.joins.some((j) => j.id === s.sel.join)) s.sel = null;
      hooks.changed(t, r.analysis.sql || "");
      if (t === cur) drawAll();
      schedulePlan(t);
    }

    function schedulePlan(t) {
      const s = st(t);
      clearTimeout(planTimer);
      if (!s.analysis || !s.analysis.valid) {
        s.plan = null;
        s.planNote = s.analysis && s.analysis.design.tables.length ? "fix the errors to see the engine's plan" : "";
        return;
      }
      s.planNote = "planning…";
      planTimer = setTimeout(() => plan(t), 600);
    }

    // plan asks the engine how it would run the statement (estimates
    // only: it is planned, never run) and paints that on the cards
    async function plan(t) {
      const s = st(t);
      const gen = s.gen;
      try {
        const p = await api("POST", wsPath(t, "/design/plan"), { design: s.design });
        if (gen !== s.gen) return;
        s.plan = p;
        s.planNote = "";
      } catch (e) {
        if (gen !== s.gen) return;
        s.plan = null;
        s.planNote = "no plan: " + e.message;
      }
      if (t === cur) { drawBar(); drawCanvas(); if (s.pane === "guide") drawSide(); }
    }

    // addTable puts a table on the canvas, joined by the server
    async function addTable(t, schema, name, x, y) {
      const s = st(t);
      if (!t.ws) { hooks.status("the tab is still connecting", "warn"); return; }
      const auto = x === undefined || x === null;
      if (auto) ({ x, y } = freeSpot(s, null));
      const before = JSON.stringify(s.design);
      let r;
      try {
        r = await api("POST", wsPath(t, "/design/add"), { design: s.design, schema, name, x, y });
      } catch (e) {
        hooks.status(e.message, "err");
        return;
      }
      s.hist.push(before);
      s.fut = [];
      s.gen++;
      land(t, r);
      const added = s.design.tables[s.design.tables.length - 1];
      if (added) {
        s.sel = { table: added.id };
        // placed by the page, not dropped: beside the table the server
        // joined it to, so its edge is short and reads left to right
        const j = auto && s.design.joins.find((x) => x.right === added.id || x.left === added.id);
        if (j) {
          const spot = freeSpot(s, j.left === added.id ? j.right : j.left, added.id);
          added.x = spot.x;
          added.y = spot.y;
          hooks.saved(t);
        }
      }
      for (const n of r.notes || []) log("info", "design: " + n);
      hooks.status(r.notes && r.notes.length ? r.notes[0] : "added " + name, "");
      if (t === cur) {
        drawAll();
        if (added) bringIn(added.id);
      }
    }

    // bringIn shows a card just added. While the whole design fits at a
    // readable zoom it is fitted — the main table must not slide out of
    // view because a third table went in on the right; past that, the view
    // pans the least that shows the new card.
    function bringIn(id) {
      const c = dom.stage.querySelector('.qdcard[data-id="' + id + '"]');
      if (!c) return;
      const cr = c.getBoundingClientRect(), vr = dom.canvas.getBoundingClientRect();
      if (cr.left >= vr.left && cr.right <= vr.right && cr.top >= vr.top && cr.bottom <= vr.bottom) return;
      const b = bounds();
      const k = Math.min((vr.width - 32) / (b.w + 40), (vr.height - 32) / (b.h + 40), 1);
      if (k >= 0.6) fit();
      else reveal(id);
    }

    // bounds is the cards' extent, in stage coordinates
    function bounds() {
      let x0 = Infinity, y0 = Infinity, x1 = 0, y1 = 0;
      for (const c of dom.stage.querySelectorAll(".qdcard")) {
        x0 = Math.min(x0, c.offsetLeft); y0 = Math.min(y0, c.offsetTop);
        x1 = Math.max(x1, c.offsetLeft + c.offsetWidth); y1 = Math.max(y1, c.offsetTop + c.offsetHeight);
      }
      return x0 === Infinity ? { x: 0, y: 0, w: 0, h: 0 } : { x: x0, y: y0, w: x1 - x0, h: y1 - y0 };
    }

    // reveal pans the least that brings a card wholly into view
    function reveal(id) {
      const c = dom.stage.querySelector('.qdcard[data-id="' + id + '"]');
      if (!c) return;
      const cr = c.getBoundingClientRect(), vr = dom.canvas.getBoundingClientRect(), pad = 16;
      let dx = 0, dy = 0;
      if (cr.right > vr.right - pad) dx = vr.right - pad - cr.right;
      if (cr.left + dx < vr.left + pad) dx = vr.left + pad - cr.left;
      if (cr.bottom > vr.bottom - pad) dy = Math.min(vr.bottom - pad - cr.bottom, 0);
      if (cr.top + dy < vr.top + pad) dy = vr.top + pad - cr.top;
      if (dx || dy) stg.panBy(dx, dy);
    }

    // freeSpot is where a new card goes: right of the table it relates to
    // (or of everything) while that is still in view at the zoom the
    // canvas has — else below it, so a design grows down the canvas's
    // height rather than shrinking every card to fit a long row — stepped
    // down past cards already there
    function freeSpot(s, fromID, self) {
      const ts = (s.design.tables || []).filter((c) => c.id !== self);
      const from = fromID && ts.find((x) => x.id === fromID);
      const h = (id) => {
        const c = dom && dom.stage.querySelector('.qdcard[data-id="' + id + '"]');
        return c ? c.offsetHeight : 220;
      };
      let x = from ? from.x + CARD_W + 60 : ts.reduce((m, c) => Math.max(m, c.x + CARD_W + 60), 40);
      let y = from ? from.y : 40;
      if (!ts.length) x = 40;
      const edge = dom ? (dom.canvas.clientWidth - stg.view.tx) / stg.view.k - 16 : Infinity;
      if (ts.length && x + CARD_W > edge) {
        const under = from || ts[ts.length - 1];
        x = under.x + (from ? 40 : 0);
        y = under.y + h(under.id) + 50;
      }
      for (let i = 0; i < 20; i++) {
        const hit = ts.find((c) => x < c.x + CARD_W && x + CARD_W > c.x && y < c.y + h(c.id) + 20 && y + 160 > c.y);
        if (!hit) break;
        y = hit.y + h(hit.id) + 30;
      }
      return { x: Math.round(x), y: Math.round(y) };
    }

    // ── refs: outputs, filters, sorts ──────────────────────────────────
    const sameRef = (a, b) => a.table === b.table && a.column === b.column && (a.agg || "") === (b.agg || "");
    function toggleOutput(t, ref) {
      edit(t, (d) => {
        const i = d.columns.findIndex((o) => sameRef(o, ref));
        if (i >= 0) d.columns.splice(i, 1);
        else d.columns.push(Object.assign({}, ref));
      });
    }
    function addFilter(t, ref) {
      const s = st(t);
      const fam = famOf(s, ref);
      const op = ref.agg ? "gt" : fam === "time" || fam === "date" ? "on" : fam === "bool" ? "eq" : "eq";
      edit(t, (d) => d.filters.push(Object.assign({ op, values: fam === "bool" ? ["true"] : [""] }, ref)));
      s.pane = "query";
      s.focusFilter = s.design.filters.length - 1;
      drawSide();
    }
    function addSort(t, ref, desc) {
      edit(t, (d) => {
        d.sorts = d.sorts.filter((x) => !sameRef(x, ref));
        d.sorts.push(Object.assign({ desc: !!desc }, ref));
      });
    }
    function famOf(s, ref) {
      if (ref.agg === "count" || ref.agg === "count_distinct") return "int";
      if (ref.agg) return "num";
      const v = viewOf(s, ref.table);
      const c = v && v.cols.find((x) => x.name === ref.column);
      return c ? c.fam : "";
    }
    function refLabel(s, ref) {
      if (!ref.table) return ref.agg === "count" ? "count(*)" : "?";
      const a = aliasOf(s, ref.table);
      const col = ref.column ? a + "." + ref.column : a + " rows";
      if (!ref.agg) return col;
      if (!ref.column) return "count of " + a;
      return (ref.agg === "count_distinct" ? "count distinct" : ref.agg) + "(" + col + ")";
    }

    // join connects two columns: into an existing join of the two tables
    // (a composite key), or as a new inner join. A new join is drawn from
    // the table already in the query to the one not yet joined, whichever
    // way the drag went: the left side is the one a left join keeps, and
    // "keep every order, with its notes if any" is what joining notes to
    // orders means — not a RIGHT JOIN that keeps every note.
    function join(t, a, ac, b, bc) {
      const s0 = st(t), va = viewOf(s0, a), vb = viewOf(s0, b);
      if (va && vb && va.role === "cross" && vb.role !== "cross") [a, ac, b, bc] = [b, bc, a, ac];
      edit(t, (d) => {
        const j = d.joins.find((x) => (x.left === a && x.right === b) || (x.left === b && x.right === a));
        if (j) {
          const p = j.left === a ? { l: ac, r: bc } : { l: bc, r: ac };
          if (!j.on.some((q) => q.l === p.l && q.r === p.r)) j.on.push(p);
          j.fk = "";
          return;
        }
        let n = 1;
        while (d.joins.some((x) => x.id === "j" + n)) n++;
        d.joins.push({ id: "j" + n, left: a, right: b, kind: "inner", on: [{ l: ac, r: bc }] });
      });
      st(t).sel = null;
    }
    function removeTable(t, id) {
      edit(t, (d) => {
        d.tables = d.tables.filter((x) => x.id !== id);
        d.joins = d.joins.filter((j) => j.left !== id && j.right !== id);
        for (const k of ["columns", "filters", "sorts"]) d[k] = d[k].filter((x) => x.table !== id);
        if (d.main === id) d.main = "";
      });
      st(t).sel = null;
    }
    function removeJoin(t, id) {
      edit(t, (d) => { d.joins = d.joins.filter((j) => j.id !== id); });
      st(t).sel = null;
    }

    // ── the DOM ────────────────────────────────────────────────────────
    function mount() {
      if (dom) return dom;
      const root = document.getElementById("qd");
      const bar = el("div", "pbar qdbar");
      const pal = el("div", "ppal qdpal");
      const find = el("input", { class: "pfilter", placeholder: "find a table…", spellcheck: "false", "aria-label": "Find a table" });
      const plist = el("div", "plist");
      pal.append(find, plist);
      const canvas = el("div", { class: "pcanvas qdcanvas", tabindex: "0", "aria-label": "Query design canvas" });
      const stage = el("div", "pstage qdstage");
      const svg = document.createElementNS("http://www.w3.org/2000/svg", "svg");
      svg.setAttribute("class", "qdedges");
      stage.append(svg);
      const hint = el("div", "qdhint");
      canvas.append(stage, hint);
      const split = el("div", { class: "pisplit", role: "separator", title: "Drag to resize (double-click to reset)" });
      const side = el("div", "pinsp qdside");
      root.append(bar, el("div", "pbody", pal, canvas, split, side));
      dbc.inspector.split(split, side);
      dom = { root, bar, pal, find, plist, canvas, stage, svg, hint, side };
      stg = dbc.stage(canvas, stage, { min: 0.3, max: 2, onChange: (v) => { if (cur) st(cur).view = { tx: v.tx, ty: v.ty, k: v.k }; } });
      find.addEventListener("input", drawPalette);
      find.addEventListener("keydown", (e) => {
        if (e.key === "Enter") {
          const first = plist.querySelector(".pitem");
          if (first) first.dispatchEvent(new Event("pick"));
        }
      });
      // an overflow:hidden box still scrolls when something inside it is
      // focused or scrolled into view (a checkbox clicked at the edge): the
      // stage's own pan takes the movement over, so the view stays the
      // stage's and the edges stay under the cards
      canvas.addEventListener("scroll", () => {
        const dx = canvas.scrollLeft, dy = canvas.scrollTop;
        if (!dx && !dy) return;
        canvas.scrollLeft = 0;
        canvas.scrollTop = 0;
        stg.panBy(-dx, -dy);
      });
      wireCanvas();
      return dom;
    }

    // ── the bar ────────────────────────────────────────────────────────
    function drawBar() {
      const s = st(cur), a = s.analysis;
      const b = dom.bar;
      b.replaceChildren();
      b.append(el("span", "pname", "◇ Design"), el("span", "qdconn", cur.conn || "not connected"));
      // kept under a name (web/designs.go), or only in this tab
      if (cur.designName) {
        b.append(el("span", { class: "qdsaved", title: "Kept as the saved design “" + cur.designName + "”: every change is saved " +
          "there, so closing the tab loses nothing — Alt+Shift+D lists the saved designs" }, "✓ saved"));
      } else if ((s.design.tables || []).length) {
        b.append(el("span", { class: "qdunsaved", title: "Kept only in this tab: closing it loses the design — ⤓ Save as… keeps it under a name" }, "in this tab only"));
      }
      const n = { error: 0, warn: 0, info: 0 };
      for (const l of (a && a.lints) || []) n[l.sev]++;
      const health = el("button", { type: "button", class: "qdhealth", title: "The guide: what the designer found, and fixes" });
      if (!a || !a.design.tables.length) health.append(el("span", "qdok", "guide"));
      else if (!n.error && !n.warn) health.append(el("span", "qdok", "✓ looks right"));
      for (const k of ["error", "warn", "info"]) if (n[k]) health.append(el("span", "qds-" + k, SEV[k] + " " + n[k]));
      health.addEventListener("click", () => { s.pane = "guide"; drawSide(); });
      b.append(health);
      const p = el("span", "qdplan");
      if (s.plan) {
        const parts = [];
        if (s.plan.hasEst) parts.push("≈ " + rough(Math.round(s.plan.estRows)) + " rows");
        if (s.plan.hasCost) parts.push("cost " + rough(Math.round(s.plan.cost)));
        const reads = Object.values(s.plan.tables || {});
        const full = reads.filter((x) => x.full).length;
        if (reads.length) parts.push(full ? full + " full scan" + (full > 1 ? "s" : "") : "all by index");
        p.textContent = "plan: " + (parts.join(" · ") || s.plan.engine);
        p.title = "The engine's own estimate for this statement (EXPLAIN, not run)";
      } else p.textContent = s.planNote;
      b.append(p, el("span", "spacer"));
      const btn = (text, title, fn, off) => {
        const x = el("button", { type: "button", title }, text);
        if (off) x.disabled = true;
        x.addEventListener("click", fn);
        b.append(x);
        return x;
      };
      btn("↶", "Undo (Ctrl+Z on the canvas)", () => undo(cur, true), !s.hist.length);
      btn("↷", "Redo (Ctrl+Shift+Z)", () => undo(cur, false), !s.fut.length);
      btn("⊡", "Fit the tables in view (f)", fit);
      btn("⟳", "Read the catalog again — after creating an index, say", () => { palettes.clear(); loadPalette(cur, true); scheduleAnalyze(cur, 0, true); });
      btn("⇪ Query", "Open the SQL in a new query tab, to edit by hand", () => a && a.sql && hooks.openQuery(a.sql), !(a && a.sql));
      btn("⤓ Save as…", cur.designName ? "Save a copy under another name, which this tab then keeps" :
        "Keep the design under a name, so it outlives the tab", () => hooks.saveAs(cur));
      btn("⤒ Open…", "The saved designs: open one in a tab, or delete one (Alt+Shift+D)", () => pickSaved());
    }

    // ── the palette ────────────────────────────────────────────────────
    async function loadPalette(t, fresh) {
      if (!t.ws || !t.conn) return;
      const key = t.ws + "|" + t.conn;
      if (!fresh && palettes.has(key)) { if (t === cur) drawPalette(); return; }
      try {
        const r = await api("GET", wsPath(t, "/design/tables") + (fresh ? "?fresh=1" : ""));
        palettes.set(key, r);
      } catch (e) {
        palettes.set(key, { tables: [], error: e.message });
      }
      if (t === cur) drawPalette();
      if (t.pendingAdd) {
        const want = t.pendingAdd;
        t.pendingAdd = "";
        addByLabel(t, want);
      }
    }
    const paletteOf = (t) => palettes.get(t.ws + "|" + t.conn);

    function drawPalette() {
      if (!cur) return;
      const s = st(cur), p = paletteOf(cur), q = dom.find.value.trim().toLowerCase();
      const list = dom.plist;
      list.replaceChildren();
      const item = (label, meta, title, pick) => {
        const it = el("div", { class: "pitem qditem", title, "data-label": label }, el("span", "pn", label), el("span", "pl", meta));
        it.addEventListener("pick", pick);
        wirePaletteDrag(it, pick);
        return it;
      };
      const rel = ((s.analysis && s.analysis.related) || []).filter((r) => !q || r.label.toLowerCase().includes(q));
      if (rel.length) {
        list.append(el("div", "phead", "related to the design"));
        for (const r of rel) {
          list.append(item(r.label, (r.many ? "many · " : "one · ") + aliasOf(s, r.from),
            "via " + r.via + (r.many ? " — many per " : " — one per ") + aliasOf(s, r.from) + " row",
            (e) => {
              const at = e.detail && e.detail.at;
              const spot = at || freeSpot(s, r.from);
              addTable(cur, r.schema, r.name, spot.x, spot.y);
            }));
        }
      }
      if (!p) { list.append(el("div", "pnote", cur.conn ? "reading tables…" : "connect to list tables")); return; }
      if (p.error) { list.append(el("div", "pnote", p.error)); return; }
      list.append(el("div", "phead", "tables"));
      let shown = 0;
      for (const tb of p.tables) {
        if (q && !tb.label.toLowerCase().includes(q)) continue;
        if (++shown > 400) { list.append(el("div", "pnote", "…type to narrow the list")); break; }
        const meta = [tb.view ? "view" : tb.known ? "~" + rough(tb.rows) : "", tb.keys ? "↔" + tb.keys : ""].filter(Boolean).join(" · ");
        list.append(item(tb.label, meta, tb.label + " — " + tb.cols + " columns" +
          (tb.known ? ", about " + rough(tb.rows) + " rows" : "") + (tb.keys ? ", " + tb.keys + " foreign keys in or out" : ""),
        (e) => {
          const at = e.detail && e.detail.at;
          addTable(cur, tb.schema, tb.name, at && at.x, at && at.y);
        }));
      }
      if (!shown) list.append(el("div", "pnote", "no table matches"));
    }

    // a palette item: a click adds it where it fits; a drag drops it where
    // the pointer lets go on the canvas
    function wirePaletteDrag(it, pick) {
      it.addEventListener("pointerdown", (e) => {
        if (e.button !== 0) return;
        const x0 = e.clientX, y0 = e.clientY;
        let ghost = null;
        it.setPointerCapture(e.pointerId);
        const move = (m) => {
          if (!ghost && Math.hypot(m.clientX - x0, m.clientY - y0) < 5) return;
          if (!ghost) { ghost = el("div", "pghost", it.querySelector(".pn").textContent); document.body.append(ghost); }
          ghost.style.left = m.clientX + 10 + "px";
          ghost.style.top = m.clientY + 6 + "px";
        };
        const up = (u) => {
          it.removeEventListener("pointermove", move);
          it.removeEventListener("pointerup", up);
          it.removeEventListener("pointercancel", up);
          if (!ghost) { pick({}); return; }
          ghost.remove();
          const r = dom.canvas.getBoundingClientRect();
          if (u.type === "pointercancel" || u.clientX < r.left || u.clientX > r.right || u.clientY < r.top || u.clientY > r.bottom) return;
          const p = stg.toStage(u.clientX, u.clientY);
          pick({ detail: { at: { x: Math.max(0, Math.round(p.x - 40)), y: Math.max(0, Math.round(p.y - 12)) } } });
        };
        it.addEventListener("pointermove", move);
        it.addEventListener("pointerup", up);
        it.addEventListener("pointercancel", up);
      });
    }

    // ── the canvas ─────────────────────────────────────────────────────
    function drawCanvas() {
      const s = st(cur), a = s.analysis;
      const stage = dom.stage;
      for (const c of stage.querySelectorAll(".qdcard, .qdjl")) c.remove();
      const tables = s.design.tables || [];
      dom.hint.hidden = tables.length > 0;
      dom.hint.replaceChildren(
        el("div", "qdhint-t", "Start with a table"),
        el("div", null, "Click one in the list on the left, or drag it here. Tables you add are joined by their foreign keys; " +
          "drag a column onto another table's column to join by hand."),
        el("div", null, "The guide checks the query as you build it — fan-out, filters that undo a left join, " +
          "missing indexes — and the story tells it back in plain words."),
      );
      const lintsOn = new Map();
      for (const l of (a && a.lints) || []) {
        if (l.sev === "info") continue;
        for (const id of l.tables || []) lintsOn.set(id, (lintsOn.get(id) || 0) + 1);
      }
      for (const tb of tables) stage.append(card(s, tb, lintsOn.get(tb.id) || 0));
      drawEdges();
    }

    function card(s, tb, nLints) {
      const v = viewOf(s, tb.id) || { cols: [], role: "", rows: 0, rowsKnown: false };
      const sel = s.sel && s.sel.table === tb.id;
      const hot = s.hot && (s.hot.tables || []).includes(tb.id);
      const c = el("div", { class: "qdcard r-" + (v.role || "joined") + (sel ? " sel" : "") + (hot ? " hot" : "") +
        (v.missing ? " missing" : ""), "data-id": tb.id, "data-alias": tb.alias || "" });
      c.style.left = tb.x + "px";
      c.style.top = tb.y + "px";
      c.style.width = CARD_W + "px";
      const head = el("div", { class: "qdch", title: "Drag to move · right-click for its menu" });
      if (v.role === "main") head.append(el("span", { class: "qdstar", title: "The query starts here (FROM)" }, "★"));
      head.append(el("span", "qdal", tb.alias || "?"), el("span", "qdtn", v.label || tb.name));
      if (v.rowsKnown) head.append(el("span", { class: "qdrows", title: "about " + v.rows + " rows (the engine's estimate)" }, "~" + rough(v.rows)));
      if (nLints) head.append(el("span", { class: "qdlc", title: nLints + " finding(s) — see the guide" }, "⚠" + nLints));
      const menuB = el("button", { type: "button", class: "qdmb", title: "Menu" }, "⋯");
      menuB.addEventListener("click", (e) => { e.stopPropagation(); tableMenu(e.clientX, e.clientY, tb); });
      head.append(menuB);
      head.addEventListener("contextmenu", (e) => { e.preventDefault(); tableMenu(e.clientX, e.clientY, tb); });
      c.append(head);

      // the role line: what this table is in the query
      const role = roleLine(s, tb, v);
      if (role) c.append(role);
      // the engine's read of it
      const tp = s.plan && s.plan.tables && s.plan.tables[tb.id];
      if (tp) {
        const big = (v.rowsKnown && v.rows >= 10000) || (tp.hasEst && tp.estRows * (tp.loops || 1) >= 10000);
        c.append(el("div", { class: "qdpb" + (tp.full ? " full" + (big ? " big" : "") : " idx"),
          title: "The plan reads it with: " + tp.op + (tp.index ? " using " + tp.index : "") +
            (tp.hasEst ? " — about " + Math.round(tp.estRows) + " rows" + (tp.loops > 1 ? " per loop, " + tp.loops + " loops" : "") : "") },
        tp.full ? "full scan" + (tp.hasEst ? " · ~" + rough(Math.round(tp.estRows)) : "") : "index " + (tp.index || tp.op)));
      }

      const cols = el("div", "qdcols");
      const used = new Set(), filtered = new Set(), sorted = new Map(), aggd = new Set();
      for (const o of s.design.columns) if (o.table === tb.id) (o.agg ? aggd : used).add(o.column);
      for (const f of s.design.filters) if (f.table === tb.id) filtered.add(f.column);
      for (const x of s.design.sorts) if (x.table === tb.id) sorted.set(x.column, x.desc);
      for (const col of v.cols) {
        const r = el("div", { class: "qdc", "data-col": col.name });
        const box = el("input", { type: "checkbox", title: "Show it as a column of the result" });
        box.checked = used.has(col.name);
        box.addEventListener("change", () => toggleOutput(cur, { table: tb.id, column: col.name }));
        r.append(box);
        r.append(el("span", { class: "qdk", title: col.pk ? "primary key" : col.fk ? "foreign key" : col.unique ? "unique" : "" },
          col.pk ? "⚿" : col.fk ? "↗" : col.unique ? "◆" : ""));
        r.append(el("span", "qdcn", col.name));
        r.append(el("span", "qdct", col.type + (col.nullable ? "?" : "")));
        const marks = el("span", "qdcm");
        if (col.indexed) marks.append(el("span", { class: "qdix", title: "an index starts with it: a filter on it can use the index" }, "⚡"));
        if (aggd.has(col.name)) marks.append(el("span", { title: "aggregated in the result" }, "Σ"));
        if (filtered.has(col.name)) marks.append(el("span", { title: "filtered" }, "⏷"));
        if (sorted.has(col.name)) marks.append(el("span", { title: "sorted" }, sorted.get(col.name) ? "↓" : "↑"));
        r.append(marks);
        r.title = col.name + " " + col.type + (col.nullable ? " (may be empty)" : " (never empty)") +
          " — click for its menu; drag onto another table's column to join on it";
        cols.append(r);
      }
      if (!v.cols.length && v.missing) cols.append(el("div", "pnote", "not on this connection"));
      c.append(cols);
      cols.addEventListener("scroll", drawEdges);
      return c;
    }

    function roleLine(s, tb, v) {
      const pa = (id) => {
        const j = (s.design.joins || []).find((x) => (x.right === id || x.left === id));
        return j ? aliasOf(s, j.left === id ? j.right : j.left) : "";
      };
      let text = "", cls = "";
      switch (v.role) {
        case "exists": text = "∃ only tested: " + pa(tb.id) + " rows that have one"; cls = "t"; break;
        case "notexists": text = "∄ only tested: " + pa(tb.id) + " rows with none"; cls = "t"; break;
        case "summary": text = "Σ summarized: one row per " + pa(tb.id); cls = "t"; break;
        case "cross": text = "✕ not joined — every row pairs with every row"; cls = "bad"; break;
        default:
          if (v.missing) { text = "not on this connection"; cls = "bad"; }
          else if (v.grain) { text = "◆ each result row is one of these"; cls = "g"; }
          else if (v.repeated) { text = "× its rows repeat: a join fans out"; cls = "rep"; }
          else if (v.optional) { text = "may be empty (outer join)"; cls = ""; }
      }
      return text ? el("div", "qdrole " + cls, text) : null;
    }

    // edges: an SVG path per join, from the first pair's column on one
    // card to its column on the other, with its glyph and shape at the
    // middle (a button: a click selects the join)
    function drawEdges() {
      if (!cur || !dom) return;
      const s = st(cur);
      const svg = dom.svg;
      svg.replaceChildren();
      for (const l of dom.stage.querySelectorAll(".qdjl")) l.remove();
      const NS = "http://www.w3.org/2000/svg";
      let maxX = 0, maxY = 0;
      for (const j of s.design.joins || []) {
        const a = anchor(j.left, j.on[0] && j.on[0].l), b = anchor(j.right, j.on[0] && j.on[0].r);
        if (!a || !b) continue;
        // Side by side: right edge to left edge, an S between them. One
        // above the other (their columns overlap): both right edges, the
        // curve bulging out to the right, so it never crosses a card. The
        // label sits at the curve's middle (t = ½ of the cubic).
        let d, mx, my = (a.y + b.y) / 2;
        if (Math.abs(a.x - b.x) < CARD_W) {
          const ax = a.x + CARD_W, bx = b.x + CARD_W, bulge = Math.max(ax, bx) + 56;
          d = "M" + ax + "," + a.y + " C" + bulge + "," + a.y + " " + bulge + "," + b.y + " " + bx + "," + b.y;
          mx = 0.75 * bulge + 0.125 * (ax + bx);
        } else {
          const rightward = a.x < b.x;
          const ax = rightward ? a.x + CARD_W : a.x, bx = rightward ? b.x : b.x + CARD_W;
          const dx = Math.max(40, Math.abs(bx - ax) / 2) * (rightward ? 1 : -1);
          d = "M" + ax + "," + a.y + " C" + (ax + dx) + "," + a.y + " " + (bx - dx) + "," + b.y + " " + bx + "," + b.y;
          mx = (ax + bx) / 2;
        }
        const jv = joinView(s, j.id) || {};
        const sel = s.sel && s.sel.join === j.id;
        const hot = s.hot && s.hot.join === j.id;
        const path = document.createElementNS(NS, "path");
        path.setAttribute("d", d);
        path.setAttribute("class", "qde k-" + j.kind + (jv.fans ? " fans" : "") + (sel ? " sel" : "") + (hot ? " hot" : "") +
          (jv.keyword === "AND" ? " loop" : ""));
        svg.append(path);
        const lab = el("button", { type: "button", class: "qdjl k-" + j.kind + (jv.fans ? " fans" : "") + (sel ? " sel" : "") + (hot ? " hot" : ""),
          title: (jv.text || "") + (j.fk ? " — foreign key " + j.fk : j.guess ? " — guessed from the names" : " — drawn by hand") + ". Click to change it.",
          "data-join": j.id }, GLYPH[j.kind] + " " + (jv.card || "?:?"));
        lab.style.left = mx + "px";
        lab.style.top = my + "px";
        lab.addEventListener("click", (e) => { e.stopPropagation(); s.sel = { join: j.id }; s.pane = "query"; drawCanvas(); drawSide(); });
        dom.stage.append(lab);
        maxX = Math.max(maxX, mx + 80, a.x + CARD_W, b.x + CARD_W);
        maxY = Math.max(maxY, a.y, b.y);
      }
      svg.setAttribute("width", maxX + 400);
      svg.setAttribute("height", maxY + 400);
    }

    // anchor is where an edge meets a card: its column's row, held inside
    // the column list's visible part when the row is scrolled away
    function anchor(id, col) {
      const c = dom.stage.querySelector('.qdcard[data-id="' + id + '"]');
      if (!c) return null;
      const x = c.offsetLeft, top = c.offsetTop;
      const cols = c.querySelector(".qdcols");
      const row = col && cols && cols.querySelector('.qdc[data-col="' + CSS.escape(col) + '"]');
      if (!row) return { x, y: top + 14 };
      // .qdcols is positioned, so the row's offsetTop is within the list
      const y = cols.offsetTop + row.offsetTop - cols.scrollTop + row.offsetHeight / 2;
      const lo = cols.offsetTop + 6, hi = cols.offsetTop + cols.clientHeight - 6;
      return { x, y: top + Math.min(Math.max(y, lo), hi) };
    }

    function fit() {
      if (!cur) return;
      const s = st(cur);
      const b = bounds();
      if (!b.w) { stg.set({ tx: 0, ty: 0, k: 1 }); return; }
      stg.fit(b.w + 40, b.h + 40, 16);
      stg.panBy(-(b.x - 20) * stg.view.k, -(b.y - 20) * stg.view.k);
      s.view = Object.assign({}, stg.view);
    }

    function tableMenu(x, y, tb) {
      const s = st(cur), v = viewOf(s, tb.id) || {};
      const items = [{ head: (tb.alias || "") + " · " + (v.label || tb.name) }];
      if (v.role !== "main") items.push({ label: "Start the query here (main table)", act: () => edit(cur, (d) => { d.main = tb.id; }) });
      items.push(
        { label: "Show all its columns", act: () => edit(cur, (d) => {
          for (const c of v.cols || []) if (!d.columns.some((o) => o.table === tb.id && o.column === c.name && !o.agg)) d.columns.push({ table: tb.id, column: c.name });
        }) },
        { label: "Hide all its columns", act: () => edit(cur, (d) => { d.columns = d.columns.filter((o) => o.table !== tb.id || o.agg); }) },
        { label: "Count its rows", act: () => toggleOutput(cur, { table: tb.id, column: "", agg: "count" }) },
        { label: "Rename the alias…", act: () => { s.sel = { table: tb.id }; s.pane = "query"; s.focusAlias = true; drawCanvas(); drawSide(); } },
        { head: "" },
        { label: "Remove it", key: "Delete", act: () => removeTable(cur, tb.id) },
      );
      dbc.menu.open(x, y, items);
    }

    function columnMenu(x, y, tb, col) {
      const s = st(cur);
      const ref = { table: tb.id, column: col.name };
      const shown = s.design.columns.some((o) => sameRef(o, ref));
      const items = [{ head: (tb.alias || "") + "." + col.name + " · " + col.type }];
      items.push({ label: shown ? "Hide it" : "Show it", act: () => toggleOutput(cur, ref) });
      const num = col.fam === "int" || col.fam === "num";
      for (const [agg, label] of AGGS.slice(1)) {
        if ((agg === "sum" || agg === "avg") && !num) continue;
        items.push({ label: "Show " + label + "(" + col.name + ")", act: () => toggleOutput(cur, Object.assign({ agg }, ref)) });
      }
      items.push({ head: "" },
        { label: "Filter on it…", act: () => addFilter(cur, ref) },
        { label: "Sort by it ↑", act: () => addSort(cur, ref, false) },
        { label: "Sort by it ↓", act: () => addSort(cur, ref, true) });
      dbc.menu.open(x, y, items);
    }

    // wireCanvas: presses on the canvas — a card's header drags it, a
    // column drags a join (or, released where it was, opens its menu), the
    // background pans. Keys: Delete, Ctrl+Z / Ctrl+Shift+Z, f, Esc.
    function wireCanvas() {
      const cv = dom.canvas;
      cv.addEventListener("pointerdown", (e) => {
        if (e.button !== 0 || !cur) return;
        if (e.target.closest(".qdjl, input, button")) return;
        cv.focus({ preventScroll: true });
        const s = st(cur);
        const cardEl = e.target.closest(".qdcard");
        const x0 = e.clientX, y0 = e.clientY;
        let moved = false;
        cv.setPointerCapture(e.pointerId);
        let move, up;
        if (cardEl && e.target.closest(".qdch")) {
          // move a card
          const tb = tableOf(s, cardEl.dataset.id);
          const sx = tb.x, sy = tb.y;
          move = (m) => {
            const dx = (m.clientX - x0) / stg.view.k, dy = (m.clientY - y0) / stg.view.k;
            if (!moved && Math.hypot(dx, dy) < 3) return;
            if (!moved) { moved = true; s.hist.push(JSON.stringify(s.design)); s.fut = []; cardEl.classList.add("lifted"); }
            tb.x = Math.max(0, Math.round(sx + dx));
            tb.y = Math.max(0, Math.round(sy + dy));
            cardEl.style.left = tb.x + "px";
            cardEl.style.top = tb.y + "px";
            drawEdges();
          };
          up = () => {
            cardEl.classList.remove("lifted");
            if (moved) hooks.saved(cur);
            else { s.sel = { table: tb.id }; s.pane = "query"; drawCanvas(); drawSide(); }
          };
        } else if (cardEl && e.target.closest(".qdc")) {
          // drag a join from a column
          const row = e.target.closest(".qdc");
          const tb = tableOf(s, cardEl.dataset.id);
          const col = ((viewOf(s, tb.id) || {}).cols || []).find((c) => c.name === row.dataset.col);
          if (!col) return;
          const NS = "http://www.w3.org/2000/svg";
          let line = null, over = null;
          const from = () => {
            const r = row.getBoundingClientRect();
            return stg.toStage(r.right, r.top + r.height / 2);
          };
          move = (m) => {
            if (!moved && Math.hypot(m.clientX - x0, m.clientY - y0) < 5) return;
            if (!moved) {
              moved = true;
              line = document.createElementNS(NS, "path");
              line.setAttribute("class", "qde live");
              dom.svg.append(line);
            }
            const a = from(), b = stg.toStage(m.clientX, m.clientY);
            line.setAttribute("d", "M" + a.x + "," + a.y + " L" + b.x + "," + b.y);
            const hit = document.elementFromPoint(m.clientX, m.clientY);
            const target = hit && hit.closest(".qdc");
            const tcard = target && target.closest(".qdcard");
            if (over) over.classList.remove("drop");
            over = target && tcard && tcard !== cardEl ? target : null;
            if (over) over.classList.add("drop");
          };
          up = (u) => {
            if (line) line.remove();
            if (over) over.classList.remove("drop");
            if (!moved) { columnMenu(u.clientX, u.clientY, tb, col); return; }
            if (over && u.type !== "pointercancel") {
              join(cur, tb.id, col.name, over.closest(".qdcard").dataset.id, over.dataset.col);
            }
          };
        } else if (!cardEl) {
          // pan
          let lx = x0, ly = y0;
          move = (m) => {
            if (!moved && Math.hypot(m.clientX - x0, m.clientY - y0) < 3) return;
            moved = true;
            cv.classList.add("panning");
            stg.panBy(m.clientX - lx, m.clientY - ly);
            lx = m.clientX; ly = m.clientY;
          };
          up = () => {
            cv.classList.remove("panning");
            if (!moved && s.sel) { s.sel = null; drawCanvas(); drawSide(); }
          };
        } else return;
        const done = (u) => {
          cv.removeEventListener("pointermove", move);
          cv.removeEventListener("pointerup", done);
          cv.removeEventListener("pointercancel", done);
          up(u);
        };
        cv.addEventListener("pointermove", move);
        cv.addEventListener("pointerup", done);
        cv.addEventListener("pointercancel", done);
      });
      cv.addEventListener("keydown", (e) => {
        if (!cur || e.target !== cv) return;
        const s = st(cur);
        const mod = e.ctrlKey || e.metaKey;
        if (mod && e.key.toLowerCase() === "z") { e.preventDefault(); undo(cur, !e.shiftKey); return; }
        if (mod && e.key.toLowerCase() === "y") { e.preventDefault(); undo(cur, false); return; }
        if (mod || e.altKey) return;
        if ((e.key === "Delete" || e.key === "Backspace") && s.sel) {
          e.preventDefault();
          if (s.sel.table) removeTable(cur, s.sel.table);
          else if (s.sel.join) removeJoin(cur, s.sel.join);
        } else if (e.key === "f") { e.preventDefault(); fit(); }
        else if (e.key === "Escape" && s.sel) { s.sel = null; drawCanvas(); drawSide(); }
      });
    }

    // ── the side: Query · Guide · Story · SQL ──────────────────────────
    // A redraw keeps the field being typed in focused, caret and all (by
    // its data-k), so an analysis landing mid-word does not steal it.
    function drawSide() {
      if (!cur || !dom) return;
      const s = st(cur), a = s.analysis;
      const side = dom.side;
      const act = document.activeElement;
      const keep = act && side.contains(act) && act.dataset.k ? { k: act.dataset.k, a: act.selectionStart, b: act.selectionEnd } : null;
      const scroll = side.scrollTop;
      side.replaceChildren();
      const tabs = el("div", "qdtabs");
      const n = { error: 0, warn: 0 };
      for (const l of (a && a.lints) || []) if (n[l.sev] !== undefined) n[l.sev]++;
      for (const [key, label] of [["query", "Query"], ["guide", "Guide" + (n.error + n.warn ? " " + (n.error + n.warn) : "")], ["story", "Story"], ["sql", "SQL"]]) {
        const b = el("button", { type: "button", class: (s.pane === key ? "on" : "") + (key === "guide" && n.error ? " bad" : key === "guide" && n.warn ? " warn" : "") }, label);
        b.addEventListener("click", () => { s.pane = key; drawSide(); });
        tabs.append(b);
      }
      side.append(tabs);
      if (s.err) side.append(el("div", "qdbanner bad", s.err));
      const pane = el("div", "qdpane");
      side.append(pane);
      ({ query: drawQuery, guide: drawGuide, story: drawStory, sql: drawSQL })[s.pane](pane, s, a);
      if (keep) {
        const f = side.querySelector('[data-k="' + CSS.escape(keep.k) + '"]');
        if (f) {
          f.focus({ preventScroll: true });
          try { if (keep.a !== null && keep.a !== undefined) f.setSelectionRange(keep.a, keep.b); } catch (_) { /* a select */ }
        }
      }
      side.scrollTop = scroll;
      if (s.focusFilter !== undefined) {
        const f = side.querySelector('[data-k="f' + s.focusFilter + 'v0"]');
        if (f) f.focus();
        s.focusFilter = undefined;
      }
      if (s.focusAlias) {
        const f = side.querySelector('[data-k="alias"]');
        if (f) { f.focus(); f.select(); }
        s.focusAlias = false;
      }
    }

    const sect = (title, ...kids) => el("div", "qdsect", el("div", "phead", title), ...kids);
    const small = (cls, text, title) => {
      const b = el("button", { type: "button", class: "qdsm " + (cls || ""), title: title || "" }, text);
      return b;
    };
    function select(options, value, k, onChange, title) {
      const sel = el("select", { "data-k": k, title: title || "" });
      for (const [v, label] of options) {
        const o = el("option", { value: v }, label);
        if (v === value) o.selected = true;
        sel.append(o);
      }
      sel.addEventListener("change", () => onChange(sel.value));
      return sel;
    }
    // field is a text input whose typing edits the design (one undo step
    // per field visit, not per key)
    function field(value, k, placeholder, onInput, cls) {
      const inp = el("input", { type: "text", value: value || "", "data-k": k, placeholder: placeholder || "", spellcheck: "false", class: cls || "" });
      let pushed = false, timer = 0;
      inp.addEventListener("focus", () => { pushed = false; });
      inp.addEventListener("input", () => {
        const t = cur, s = st(t);
        if (!pushed) { s.hist.push(JSON.stringify(s.design)); s.fut = []; pushed = true; }
        onInput(inp.value, s.design);
        t.design = s.design;
        s.gen++;
        clearTimeout(timer);
        // the tab typed in, not whichever is on screen when the timer fires
        timer = setTimeout(() => { if (t === cur) scheduleAnalyze(t, 0); }, 250);
      });
      return inp;
    }

    function drawQuery(pane, s, a) {
      if (s.sel && s.sel.join) pane.append(joinInspector(s, s.sel.join));
      else if (s.sel && s.sel.table) pane.append(tableInspector(s, s.sel.table));
      const d = s.design;
      if (!d.tables.length) { pane.append(el("p", "pnote", "Add a table from the list on the left to begin.")); return; }

      // columns
      const cl = el("div", "qdlist");
      d.columns.forEach((o, i) => {
        const r = el("div", "qdrow" + (hotIs(s, "column", i) ? " hot" : "") + (badIs(a, "column", i) ? " bad" : ""));
        r.append(el("span", "qdref", refLabel(s, o)));
        if (o.table) {
          r.append(select(AGGS, o.agg || "", "c" + i + "agg", (v) => edit(cur, (dd) => { dd.columns[i].agg = v; }), "aggregate"));
        }
        r.append(field(o.alias, "c" + i + "as", "name", (v, dd) => { dd.columns[i].alias = v.trim(); }, "qdas"));
        const upB = small("", "↑", "Move up"), rm = small("", "✕", "Remove");
        upB.disabled = i === 0;
        upB.addEventListener("click", () => edit(cur, (dd) => { const [x] = dd.columns.splice(i, 1); dd.columns.splice(i - 1, 0, x); }));
        rm.addEventListener("click", () => edit(cur, (dd) => { dd.columns.splice(i, 1); }));
        r.append(upB, rm);
        cl.append(r);
      });
      if (!d.columns.length) cl.append(el("div", "pnote", "Tick columns on the cards. Nothing ticked returns every column."));
      const cnt = small("", "+ count(*)", "Count the result's rows");
      cnt.addEventListener("click", () => toggleOutput(cur, { table: "", column: "", agg: "count" }));
      pane.append(sect("Columns" + (a && a.grouped ? " · grouped by the plain ones" : ""), cl, el("div", "qdadd", cnt)));

      // filters
      const fl = el("div", "qdlist");
      d.filters.forEach((f, i) => fl.append(filterRow(s, a, f, i)));
      if (!d.filters.length) fl.append(el("div", "pnote", "Click a column on a card → Filter on it."));
      pane.append(sect("Filters", fl));

      // sorts
      const sl = el("div", "qdlist");
      d.sorts.forEach((x, i) => {
        const r = el("div", "qdrow" + (badIs(a, "sort", i) ? " bad" : "") + (hotIs(s, "sort", i) ? " hot" : ""));
        r.append(el("span", "qdref", refLabel(s, x)));
        const dir = small("", x.desc ? "↓ high first" : "↑ low first", "Change the direction");
        dir.addEventListener("click", () => edit(cur, (dd) => { dd.sorts[i].desc = !dd.sorts[i].desc; }));
        const upB = small("", "↑", "Sort by this first"), rm = small("", "✕", "Remove");
        upB.disabled = i === 0;
        upB.addEventListener("click", () => edit(cur, (dd) => { const [y] = dd.sorts.splice(i, 1); dd.sorts.splice(i - 1, 0, y); }));
        rm.addEventListener("click", () => edit(cur, (dd) => { dd.sorts.splice(i, 1); }));
        r.append(dir, upB, rm);
        sl.append(r);
      });
      if (!d.sorts.length) sl.append(el("div", "pnote", "Click a column on a card → Sort by it."));
      pane.append(sect("Sort", sl));

      // rows
      const rows = el("div", "qdrowsb");
      const dist = el("input", { type: "checkbox", "data-k": "distinct" });
      dist.checked = !!d.distinct;
      dist.addEventListener("change", () => edit(cur, (dd) => { dd.distinct = dist.checked; }));
      const num = (k, v, ph, set) => field(v ? String(v) : "", k, ph, (x, dd) => { set(dd, Math.max(0, parseInt(x, 10) || 0)); }, "qdnum");
      rows.append(el("label", "qdck", dist, " distinct rows"),
        el("label", null, "limit ", num("limit", d.limit, "all", (dd, n) => { dd.limit = n; })),
        el("label", null, "skip ", num("offset", d.offset, "0", (dd, n) => { dd.offset = n; })));
      pane.append(sect("Rows", rows));
    }

    const hotIs = (s, what, i) => s.hot && s.hot[what] === i;
    const badIs = (a, what, i) => !!a && (a.lints || []).some((l) => l.sev === "error" && l[what] === i);

    function filterRow(s, a, f, i) {
      const r = el("div", "qdfil" + (badIs(a, "filter", i) ? " bad" : "") + (hotIs(s, "filter", i) ? " hot" : ""));
      const fam = famOf(s, f);
      const ops = OPS_BY[f.agg ? "agg" : fam] || OPS_BY[""];
      const line = el("div", "qdrow");
      line.append(el("span", "qdref", refLabel(s, Object.assign({}, f, { agg: "" }))));
      if (f.table) line.append(select(AGGS, f.agg || "", "f" + i + "agg", (v) => edit(cur, (dd) => {
        dd.filters[i].agg = v;
        if (v && !OPS_BY.agg.includes(dd.filters[i].op)) dd.filters[i].op = "gt";
      }), "filter on an aggregate of it (a condition on groups: HAVING)"));
      line.append(select(ops.map((o) => [o, OPS[o]]), f.op, "f" + i + "op", (v) => edit(cur, (dd) => {
        const x = dd.filters[i];
        x.op = v;
        if (!LIKE_OPS.includes(v)) delete x.ignoreCase; // only a LIKE has a case rule to change
        const n = ARITY[v] === undefined ? 1 : ARITY[v];
        if (n === 0) x.values = [];
        else if (n === 2) x.values = [x.values[0] || "", x.values[1] || ""];
        else if (!x.values.length) x.values = [""];
      })));
      const rm = small("", "✕", "Remove the filter");
      rm.addEventListener("click", () => edit(cur, (dd) => { dd.filters.splice(i, 1); }));
      line.append(rm);
      r.append(line);

      const n = ARITY[f.op] === undefined ? 1 : ARITY[f.op];
      if (n !== 0) {
        const vals = el("div", "qdrow");
        if (fam === "bool" && f.op === "eq" && !f.agg) {
          vals.append(select([["true", "true"], ["false", "false"]], (f.values[0] || "true").toLowerCase(), "f" + i + "v0",
            (v) => edit(cur, (dd) => { dd.filters[i].values = [v]; })));
        } else if (n === -1) {
          vals.append(field((f.values || []).join(", "), "f" + i + "v0", "values, comma separated", (v, dd) => {
            dd.filters[i].values = v.split(",").map((x) => x.trim()).filter((x) => x !== "");
          }, "qdval"));
        } else {
          const ph = f.op === "year" ? "2024" : f.op === "lastdays" ? "days" : f.op === "on" || fam === "date" || fam === "time" ? "YYYY-MM-DD" : "value";
          for (let k = 0; k < n; k++) {
            if (k) vals.append(el("span", "qdand", f.op === "range" ? "up to" : "and"));
            vals.append(field((f.values || [])[k], "f" + i + "v" + k, ph, (v, dd) => {
              const x = dd.filters[i];
              while (x.values.length <= k) x.values.push("");
              x.values[k] = v;
            }, "qdval"));
          }
        }
        r.append(vals);
      }
      const opts = el("div", "qdrow qdopts");
      // any-of groups: A to C, and any other letter the design already
      // has (an imported query's fourth OR group is D)
      const grps = ["A", "B", "C"];
      for (const x of s.design.filters || []) if (x.group && !grps.includes(x.group)) grps.push(x.group);
      opts.append(select([["", "all of these"]].concat(grps.map((g) => [g, "any of " + g])), f.group || "", "f" + i + "grp",
        (v) => edit(cur, (dd) => { dd.filters[i].group = v; }), "Filters in the same group: any one of them is enough (OR); groups and the rest: all (AND)"));
      const v = viewOf(s, f.table);
      const c = v && v.cols.find((x) => x.name === f.column);
      if (c && c.nullable && !f.agg && f.op !== "null" && f.op !== "notnull") {
        const ck = el("input", { type: "checkbox", "data-k": "f" + i + "orNull" });
        ck.checked = !!f.orNull;
        ck.addEventListener("change", () => edit(cur, (dd) => { dd.filters[i].orNull = ck.checked; }));
        opts.append(el("label", { class: "qdck", title: "Also keep rows where it is empty (NULL fails every comparison)" }, ck, " or empty"));
      }
      if (LIKE_OPS.includes(f.op) && !f.agg) {
        const ck = el("input", { type: "checkbox", "data-k": "f" + i + "icase" });
        ck.checked = !!f.ignoreCase;
        ck.addEventListener("change", () => edit(cur, (dd) => { dd.filters[i].ignoreCase = ck.checked; }));
        opts.append(el("label", { class: "qdck", title: "Match letters whatever their case: ILIKE on Postgres, " +
          "lower(…) LIKE lower(…) elsewhere. Unticked, the engine's own rule applies (Postgres: case matters; " +
          "SQLite: A–Z ignored; MySQL: the column's collation)" }, ck, " ignore case"));
      }
      if (v && v.role === "joined" && !f.agg) {
        const ck = el("input", { type: "checkbox", "data-k": "f" + i + "inJoin" });
        ck.checked = !!f.inJoin;
        ck.addEventListener("change", () => edit(cur, (dd) => { dd.filters[i].inJoin = ck.checked; }));
        opts.append(el("label", { class: "qdck", title: "Write it into the join's ON: on a left join, rows without a match are kept" }, ck, " in the join"));
      }
      r.append(opts);
      return r;
    }

    function tableInspector(s, id) {
      const tb = tableOf(s, id), v = viewOf(s, id) || {};
      if (!tb) return el("div");
      const box = el("div", "qdinsp");
      box.append(el("div", "phead qdih", "table · " + (v.label || tb.name)));
      const row = el("div", "qdrow");
      row.append(el("span", "qdlab", "alias"), field(tb.alias, "alias", "alias", (val, dd) => {
        const x = dd.tables.find((y) => y.id === id);
        if (x) x.alias = val.trim();
      }, "qdas"));
      const rm = small("", "Remove", "Remove the table, its joins and everything on it");
      rm.addEventListener("click", () => removeTable(cur, id));
      row.append(rm);
      box.append(row);
      if (v.role !== "main") {
        const m = small("", "★ Start here", "Make it the main table: the query starts FROM it");
        m.addEventListener("click", () => edit(cur, (d) => { d.main = id; }));
        box.append(el("div", "qdrow", m));
      }
      const facts = [];
      if (v.rowsKnown) facts.push("about " + rough(v.rows) + " rows");
      if (v.indexes && v.indexes.length) facts.push("indexes: " + v.indexes.map((x) => x.name + " (" + x.cols.join(", ") + ")").join("; "));
      if (facts.length) box.append(el("div", "qdfacts", facts.join(" · ")));
      return box;
    }

    function joinInspector(s, id) {
      const j = (s.design.joins || []).find((x) => x.id === id), jv = joinView(s, id) || {};
      if (!j) return el("div");
      const box = el("div", "qdinsp");
      const L = aliasOf(s, j.left), R = aliasOf(s, j.right);
      box.append(el("div", "phead qdih", "join · " + L + " " + GLYPH[j.kind] + " " + R + " · " + (jv.card || "")));
      if (jv.text) box.append(el("div", "qdfacts", jv.text));
      const kinds = el("div", "qdkinds");
      for (const [k, g, name, why] of KINDS) {
        const b = el("button", { type: "button", class: "qdkind" + (j.kind === k ? " on" : ""), title: why },
          el("span", "qdkg", g), el("span", "qdkn", name), el("span", "qdkw", why.replace(/left|right/g, (w) => (w === "left" ? L : R))));
        b.addEventListener("click", () => edit(cur, (d) => { d.joins.find((x) => x.id === id).kind = k; }));
        kinds.append(b);
      }
      box.append(kinds);
      const pairs = el("div", "qdlist");
      j.on.forEach((p, i) => {
        const r = el("div", "qdrow");
        r.append(el("span", "qdref", L + "." + p.l + " = " + R + "." + p.r));
        const rm = small("", "✕", "Remove this pair");
        rm.addEventListener("click", () => edit(cur, (d) => { d.joins.find((x) => x.id === id).on.splice(i, 1); }));
        r.append(rm);
        pairs.append(r);
      });
      if (!j.on.length) pairs.append(el("div", "pnote", "no columns: drag one card's column onto the other's"));
      box.append(el("div", "phead", "matched on"), pairs);
      if ((jv.choices || []).length) {
        box.append(el("div", "phead", "foreign keys between them"));
        for (const c of jv.choices) {
          const b = small(j.fk === c.fk ? "on" : "", (j.fk === c.fk ? "● " : "○ ") + c.label, c.fk);
          b.addEventListener("click", () => edit(cur, (d) => { const x = d.joins.find((y) => y.id === id); x.on = c.on; x.fk = c.fk; x.guess = false; }));
          box.append(el("div", "qdrow", b));
        }
      }
      const act = el("div", "qdrow");
      const swap = small("", "⇄ Swap sides", "The left side is the one a left join keeps, and the one has-any / summarize tests from");
      swap.addEventListener("click", () => edit(cur, (d) => {
        const x = d.joins.find((y) => y.id === id);
        [x.left, x.right] = [x.right, x.left];
        x.on = x.on.map((p) => ({ l: p.r, r: p.l }));
      }));
      const rm = small("", "Remove the join");
      rm.addEventListener("click", () => removeJoin(cur, id));
      act.append(swap, rm);
      box.append(act);
      return box;
    }

    function drawGuide(pane, s, a) {
      if (!a) { pane.append(el("p", "pnote", s.err ? "" : "…")); return; }
      const lints = a.lints || [];
      if (!a.design.tables.length) { pane.append(el("p", "pnote", "Add a table: the guide checks the query as you build it.")); return; }
      if (!lints.some((l) => l.sev !== "info")) {
        pane.append(el("div", "qdbanner good", "✓ Nothing to fix: the joins keep their rows' meaning and the filters say what they mean" +
          (s.facts && s.facts.indexes && s.facts.estimates ? "; no big table is read without an index to help." : ".")));
      }
      for (const area of ["correctness", "performance"]) {
        const ls = lints.filter((l) => l.area === area);
        if (!ls.length) continue;
        pane.append(el("div", "phead", area === "correctness" ? "Is it right?" : "Is it fast?"));
        for (const l of ls) pane.append(lintCard(s, l));
      }
      // the engine's own view
      pane.append(el("div", "phead", "The engine's plan"));
      if (s.plan) {
        const p = s.plan;
        const lines = el("div", "qdlist");
        for (const tb of s.design.tables) {
          const tp = p.tables[tb.id];
          if (!tp) continue;
          lines.append(el("div", "qdrow", el("span", "qdref", tb.alias), el("span", tp.full ? "qdwarnt" : "qdgoodt",
            tp.op + (tp.index ? " · " + tp.index : "") + (tp.hasEst ? " · ~" + rough(Math.round(tp.estRows)) + " rows" : ""))));
        }
        pane.append(lines);
        for (const pi of p.insights || []) {
          const c = el("div", "qdl sev-" + (pi.severity === "crit" ? "error" : pi.severity === "warn" ? "warn" : "info"));
          c.append(el("div", "qdlt", (pi.severity === "crit" ? "✕ " : pi.severity === "warn" ? "⚠ " : "ⓘ ") + pi.title));
          if (pi.detail) c.append(el("div", "qdlw", pi.detail));
          if (pi.fix) c.append(el("div", "qdlw", pi.fix));
          if (pi.sql) c.append(copyBtn(pi.sql, "Copy " + pi.sql.split(" ").slice(0, 2).join(" ")));
          if (pi.table) hoverHot(c, { tables: [pi.table] });
          pane.append(c);
        }
      } else pane.append(el("p", "pnote", s.planNote || "the plan comes once the design is valid"));
      if (s.facts && !s.facts.indexes) pane.append(el("p", "pnote", "Index rules are quiet: this engine's indexes are not listed (the plan still shows what it uses)."));
      if (s.facts && !s.facts.estimates) pane.append(el("p", "pnote", "Size rules are quiet: this engine keeps no row estimates."));
    }

    function copyBtn(sql, label) {
      const b = small("", label || "⧉ Copy", sql);
      b.addEventListener("click", () => dbc.clip.copyText(sql, "the statement"));
      return b;
    }

    // hoverHot lights what a finding is about while the pointer is on it:
    // the cards and edges read s.hot as they draw
    function hoverHot(node, hot) {
      node.addEventListener("pointerenter", () => { if (!cur) return; st(cur).hot = hot; drawCanvas(); });
      node.addEventListener("pointerleave", () => { if (!cur) return; st(cur).hot = null; drawCanvas(); });
    }

    function lintCard(s, l) {
      const c = el("div", { class: "qdl sev-" + l.sev, "data-rule": l.rule });
      c.append(el("div", "qdlt", SEV[l.sev] + " " + l.title));
      c.append(el("div", "qdlw", words(l.why)));
      if ((l.fixes || []).length) {
        const fx = el("div", "qdfix");
        for (const f of l.fixes) {
          if (f.design) {
            const b = small("primary", f.label, "Apply — Ctrl+Z on the canvas takes it back");
            b.addEventListener("click", () => {
              edit(cur, (d) => {
                const nd = JSON.parse(JSON.stringify(f.design));
                // keep the cards where they are; a fix's new tables keep theirs
                const pos = new Map(d.tables.map((x) => [x.id, x]));
                for (const x of nd.tables || []) { const p = pos.get(x.id); if (p) { x.x = p.x; x.y = p.y; } }
                for (const k of Object.keys(d)) delete d[k];
                Object.assign(d, nd);
                for (const k of ["tables", "joins", "columns", "filters", "sorts"]) d[k] = d[k] || [];
              });
              hooks.status("applied: " + f.label + " — Ctrl+Z on the canvas undoes it", "");
            });
            fx.append(b);
          } else if (f.sql) fx.append(copyBtn(f.sql, f.label));
        }
        c.append(fx);
      }
      const hot = { tables: l.tables || [], join: l.join || "" };
      if (l.filter >= 0) hot.filter = l.filter;
      if (l.column >= 0) hot.column = l.column;
      if (l.sort >= 0) hot.sort = l.sort;
      hoverHot(c, hot);
      c.addEventListener("click", (e) => {
        if (e.target.closest("button")) return;
        const s2 = st(cur);
        if (l.join) s2.sel = { join: l.join };
        else if ((l.tables || []).length) s2.sel = { table: l.tables[0] };
        if (l.filter >= 0 || l.column >= 0 || l.sort >= 0 || l.join) s2.pane = "query";
        s2.hot = hot;
        drawCanvas();
        drawSide();
      });
      return c;
    }

    function drawStory(pane, s, a) {
      const ol = el("ol", "qdstory");
      for (const line of (a && a.story) || []) ol.append(el("li", null, words(line)));
      pane.append(ol);
      if (a && (a.names || []).length) pane.append(el("div", "qdfacts", "Result columns: " + a.names.join(", ")));
    }

    function drawSQL(pane, s, a) {
      const sql = (a && a.sql) || "";
      if (a && !a.valid && sql) pane.append(el("div", "qdbanner bad", "Fix the errors in the guide before running it."));
      const pre = el("pre", "qdsql");
      pre.append(sql ? dbc.hl("sql", sql) : "");
      const acts = el("div", "qdrow");
      const run = small("primary", "▶ Run", "Run it into the grid below (Ctrl+Enter)");
      run.disabled = !(a && a.valid);
      run.addEventListener("click", () => hooks.run());
      const ex = small("", "◈ Explain", "The full plan in the results pane (Ctrl+X)");
      ex.disabled = !(a && a.valid);
      ex.addEventListener("click", () => hooks.explain());
      const open = small("", "⇪ Open as a query", "A new query tab with this SQL, to edit by hand");
      open.disabled = !sql;
      open.addEventListener("click", () => hooks.openQuery(sql));
      acts.append(run, ex, copyBtn(sql, "⧉ Copy"), open);
      pane.append(acts, pre);
      if (a && a.dialect) pane.append(el("div", "qdfacts", "Written for " + a.dialect + "."));
    }

    function drawAll() {
      if (!cur || !dom) return;
      drawBar();
      drawPalette();
      drawCanvas();
      drawSide();
    }

    // ── saved designs ──────────────────────────────────────────────────
    // pickSaved is the saved designs dialog (Alt+Shift+D, the bar's
    // ⤒ Open…, the tab menu): the designs kept under a name
    // (web/designs.go), filtered by name, connection or table as you type.
    // Enter opens one (hooks.openSaved: the tab here that keeps it, else a
    // new design tab), Alt+D deletes one after asking. sel puts the cursor
    // on that name. An open dialog follows a save or delete in any window
    // (savedChanged, on the "designs" event).
    let savedRedraw = null;
    async function pickSaved(sel) {
      let list;
      try { list = (await api("GET", "/api/v1/designs")).designs || []; } catch (err) { log("err", "saved designs: " + err.message); return; }
      const input = el("input", { type: "search", class: "hfilter", placeholder: "type to filter by name, connection or table",
        "aria-label": "Filter saved designs", spellcheck: "false", autocomplete: "off" });
      const ul = el("ul", { class: "hlist snlist", role: "listbox" });
      const preview = el("pre", "ival snprev");
      const count = el("span", "hint");
      let shown = [], pos = 0;
      const line = (d) => (d.conn || "no connection") + " · " + (d.tables.length ? d.tables.join(", ") : "no tables");
      function draw() {
        const q = input.value.trim().toLowerCase();
        shown = list.filter((d) => !q || d.name.toLowerCase().includes(q) || line(d).toLowerCase().includes(q));
        if (sel) {
          const i = shown.findIndex((d) => d.name === sel);
          if (i >= 0) pos = i;
          sel = "";
        }
        pos = Math.max(0, Math.min(pos, shown.length - 1));
        ul.replaceChildren();
        shown.forEach((d, i) => {
          ul.append(el("li", { class: i === pos ? "pos" : "", role: "option", "data-i": String(i), title: d.name },
            el("span", "hconn", d.name), el("span", "hsql", line(d)),
            el("span", "hint", d.updated ? new Date(d.updated).toLocaleString() : "")));
        });
        const d = shown[pos];
        preview.textContent = d ? d.sql || "(no SQL yet)" : "";
        preview.hidden = !d;
        count.textContent = !list.length ? "none yet — ⤓ Save as… in a design tab keeps its design here"
          : shown.length ? "↑↓ pick · Enter opens · Alt+D delete · Esc closes" : "no saved design matches";
        const c = ul.children[pos];
        if (c) c.scrollIntoView({ block: "nearest" });
      }
      function pick(i) {
        const d = shown[i];
        if (!d) return;
        savedRedraw = null;
        dbc.modal.close();
        hooks.openSaved(d);
      }
      input.addEventListener("input", () => { pos = 0; draw(); });
      ul.addEventListener("click", (e) => { const li = e.target.closest("li[data-i]"); if (li) { pos = +li.dataset.i; draw(); input.focus(); } });
      ul.addEventListener("dblclick", (e) => { const li = e.target.closest("li[data-i]"); if (li) pick(+li.dataset.i); });
      savedRedraw = async () => {
        try { list = (await api("GET", "/api/v1/designs")).designs || []; } catch (_) { return; }
        const was = shown[pos];
        if (was) sel = was.name;
        if (savedRedraw) draw();
      };
      dbc.modal.open({
        title: "Saved designs", cls: "wide", focus: input,
        body: el("div", "history", input, ul, preview), foot: el("div", "mfoot", count),
        onKey: (e) => {
          if (e.key === "ArrowDown") { pos = Math.min(pos + 1, shown.length - 1); draw(); return true; }
          if (e.key === "ArrowUp") { pos = Math.max(pos - 1, 0); draw(); return true; }
          if (e.key === "Enter") { pick(pos); return true; }
          // Alt and e.code: on a Mac Alt+D types "∂"
          if (e.altKey && !e.ctrlKey && !e.metaKey && e.code === "KeyD" && shown[pos]) { confirmDelete(shown[pos].name); return true; }
          return false;
        },
        onClose: () => { savedRedraw = null; },
      });
      draw();
    }

    // confirmDelete asks before deleting a saved design: a delete cannot be
    // undone. A tab that keeps it keeps its own copy, and lets go of the
    // name at its next save (app.js designNameLost).
    function confirmDelete(name) {
      savedRedraw = null;
      const yes = el("button", { type: "button", class: "primary" }, "Delete " + name);
      const no = el("button", { type: "button" }, "Keep it");
      yes.addEventListener("click", async () => {
        try {
          await api("POST", "/api/v1/designs/delete", { name });
          log("ok", "deleted the saved design " + name);
        } catch (err) {
          log("err", "saved designs: " + err.message);
        }
        pickSaved();
      });
      no.addEventListener("click", () => pickSaved(name));
      dbc.modal.open({
        title: "Delete the saved design " + name + "?", focus: no,
        body: el("div", "confirm", el("p", null, "It is deleted for every window. A tab showing it keeps its own copy, " +
          "which is then kept only in that tab.")),
        foot: el("div", "mfoot", yes, no),
      });
    }

    // ── the kit ────────────────────────────────────────────────────────
    async function addByLabel(t, label) {
      const p = paletteOf(t);
      const tb = p && (p.tables || []).find((x) => x.label === label || x.name === label);
      if (!tb) { hooks.status("no table " + label + " in the designer's list", "warn"); return; }
      await addTable(t, tb.schema, tb.name);
    }

    return {
      // show puts tab t's design on the canvas
      show(t) {
        mount();
        cur = t;
        const s = st(t);
        dom.root.hidden = false;
        // a design that arrives whole (opened from a query, a saved one,
        // a reload) has no view yet: fitted, rather than shown from the
        // corner with its far tables under the side pane. Asked before
        // set, whose onChange records the view it sets.
        const unseen = !s.view;
        stg.set(s.view || { tx: 0, ty: 0, k: 1 });
        drawAll();
        if (unseen && s.design.tables.length > 1) fit();
        // never analyzed, or edited since (a field typed in just before
        // the tab was left: its debounce then asked for the other tab)
        if (!s.analysis || s.analyzed !== s.gen) scheduleAnalyze(t, 0);
        loadPalette(t, false);
      },
      hide() {
        if (dom) dom.root.hidden = true;
        cur = null;
        dbc.menu.close();
      },
      // connChanged: the tab's workspace landed on a connection (or
      // re-read its catalog after a DDL): the palette and the analysis
      // follow
      connChanged(t, fresh) {
        if (t !== cur) return;
        loadPalette(t, !!fresh);
        scheduleAnalyze(t, 0, !!fresh);
        drawBar();
      },
      addByLabel,
      valid: (t) => { const s = states.get(t.key); return !!(s && s.analysis && s.analysis.valid); },
      focus() { if (dom) dom.canvas.focus({ preventScroll: true }); },
      forget(t) { states.delete(t.key); },
      // redraw the bar: the tab's saved name changed (app.js)
      named(t) { if (t === cur && dom) drawBar(); },
      // tables: how many tables tab t's design has (an empty design is
      // not worth asking about when its tab closes)
      tables: (t) => ((states.get(t.key) || {}).design || t.design || emptyDesign()).tables.length,
      pickSaved,
      // savedChanged: a saved design was saved or deleted, in any window
      savedChanged() { if (savedRedraw) savedRedraw(); },
      emptyDesign,
    };
  }

  dbc.designer = { create };
})();
