// dbc web — the results grid.
//
// VIRTUALIZED, both ways: only the rows and columns in view are in the DOM,
// and rows arrive from the server a page at a time (GET …/result), so a
// 50,000-row result costs what a 50-row one does. The TUI's grid (tui/grid.go)
// is the model for everything it does, and its words are reused.
//
//   #grid  (the scroll container, focusable)
//   ├ .gh  header, sticky to the top: ┌ # ┬ name ▲ ┬ age ┬ …   (.rz = resize handle)
//   └ .gb  body, as tall as every row: rows positioned absolutely at row × ROW_H,
//          each holding only the visible columns' cells
//
// COORDINATES, as in the TUI. A "row" is a DISPLAY row: an index into the
// server's sorted order — the page never holds the order itself, it asks
// for display rows and gets them sorted. A "col" is a DISPLAY column: an
// index into vis, the result's columns minus the hidden ones. What belongs
// to the data (widths, numeric, hidden) is kept by RESULT column, so it
// survives a column being hidden and shown again.
//
//   result cols   0    1    2    3    4        hidden = {1, 3}
//   vis         [ 0,        2,        4 ]      display col 1 → result col 2
//
// The VIEW (sort, hidden, widths, cursor, range) lives here and travels
// with each request that needs it — a copy names its columns and rows, the
// sort and the result's seq, and the server projects exactly that.
(function () {
  "use strict";

  const dbc = window.dbc;
  const el = dbc.el;

  const ROW_H = 22;      // px per row; fixed, which is what makes the virtual scroll arithmetic
  const PAGE = 200;      // rows per fetch
  const OVERSCAN = 6;    // rows drawn beyond the view each way, so a scroll step rarely shows blanks
  const PAD = 18;        // a cell's padding (8 + 8) and its border (1), plus 1 px of slack, so a value that fits is not ellipsized
  const MIN_CH = 3;      // the narrowest a column can be dragged, in characters
  const MAX_CH = 400;    // the widest (a runaway drag cannot build absurd layouts)
  const NO_RESULT = "nothing to copy yet — run a query first";

  const root = document.getElementById("grid");
  const msg = document.getElementById("grid-msg");
  const info = document.getElementById("grid-info");
  const head = el("div", "gh");
  const body = el("div", "gb");
  root.append(head, body);

  // g is the grid's whole state.
  const g = {
    seq: 0,            // the result on show (the server's number); 0 = none
    conn: "",
    cols: [], numeric: [], auto: [], content: [],
    total: 0,          // display rows (max_display_rows applied)
    rows: 0,           // the result's rows
    sort: -1, desc: false,
    hidden: new Set(), // result columns
    userW: new Map(),  // result column → width set by hand, in px
    vis: [],           // display col → result col
    colX: [], colW: [], width: 0, // layout of the visible columns, px
    rnW: 40,           // the row-number gutter's width
    pages: new Map(),  // page index → rows (arrays of cells; null is NULL)
    pending: new Set(),
    cur: { row: 0, col: 0 }, anc: { row: 0, col: 0 }, sel: false,
  };

  // charW is the width of one character of the grid's font — measured, not
  // assumed, so widths in characters (the server's, the TUI's) become px.
  // Measured in the grid itself, with the font it actually renders: a
  // canvas measure would resolve "ui-monospace" differently (or not at
  // all) and size every column a few characters short. Needs the grid on
  // screen, so it is taken on the first layout and kept.
  let charW = 0;
  function measureChar() {
    if (charW) return;
    const probe = el("span", "probe", "0000000000");
    root.append(probe);
    charW = probe.getBoundingClientRect().width / 10 || 7.2;
    probe.remove();
  }

  const esc = (s) => String(s).replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c]));
  // flatten keeps one value on one line, as the TUI draws it
  const flat = (s) => (/[\n\r\t]/.test(s) ? s.replace(/\r\n|\n|\r/g, "↵").replace(/\t/g, " ") : s);

  // ── loading ────────────────────────────────────────────────────────────
  function query(from, sort, desc) {
    return dbc.api("GET", dbc.wsPath("/result?from=" + from + "&n=" + PAGE + "&sort=" + sort + "&desc=" + (desc ? 1 : 0)));
  }

  // load fetches the tab's result — after a run, a script's s.Show, or a
  // reattach. The same result (same seq) keeps its view; a new one resets
  // it, keeping hidden columns and hand-set widths when its columns are the
  // same as the last one's: the common loop is edit-the-WHERE-and-rerun,
  // and losing the layout on every run would make hiding not worth doing.
  async function load() {
    let d;
    try {
      d = await query(0, g.seq ? g.sort : -1, g.desc);
      if (d && d.seq !== g.seq && d.sort !== -1) d = await query(0, -1, false); // a new result starts unsorted
    } catch (e) {
      dbc.log("err", "could not load the result: " + e.message);
      return;
    }
    if (!d) { clear(); return; }
    if (d.seq === g.seq) {
      g.pages.clear();
      g.pages.set(0, d.cells);
      render();
      return;
    }
    adopt(d);
    viewChanged();
  }

  function adopt(d) {
    const same = g.cols.length === d.columns.length && g.cols.every((c, i) => c === d.columns[i]);
    Object.assign(g, {
      seq: d.seq, conn: d.conn, cols: d.columns, numeric: d.numeric, auto: d.widths, content: d.content,
      total: d.total, rows: d.rows, sort: -1, desc: false,
      cur: { row: 0, col: 0 }, anc: { row: 0, col: 0 }, sel: false,
    });
    if (!same) { g.hidden = new Set(); g.userW = new Map(); }
    g.pages = new Map([[0, d.cells]]);
    g.pending = new Set();
    if (d.exec) {
      showMsg(d.affected + " rows affected", "exec");
      info.textContent = "";
      return;
    }
    if (!d.columns.length) {
      showMsg("the statement returned no columns", "");
      return;
    }
    msg.hidden = true;
    root.hidden = false;
    rebuildVis();
    root.scrollTop = 0;
    root.scrollLeft = 0;
    render();
  }

  // viewFns hear about every change to the grid's view of its result — a
  // new result, a sort, a column hidden or shown — which is what the
  // assistant's context chip forecasts from.
  const viewFns = [];
  const viewChanged = () => { for (const f of viewFns) f(); };

  function clear() {
    Object.assign(g, { seq: 0, cols: [], vis: [], total: 0, rows: 0, pages: new Map() });
    viewChanged();
    showMsg("Ctrl+Enter runs the statement under the caret; Ctrl+Shift+Enter runs them all.", "");
    info.textContent = "";
  }

  function showMsg(text, cls) {
    msg.textContent = text;
    msg.className = "grid-msg " + cls;
    msg.hidden = false;
    root.hidden = true;
  }

  // page fetches one page of display rows under the current sort; drawn
  // when it lands, unless the view moved on meanwhile.
  async function page(p) {
    if (g.pending.has(p)) return;
    g.pending.add(p);
    const seq = g.seq, sort = g.sort, desc = g.desc;
    try {
      const d = await query(p * PAGE, sort, desc);
      if (!d || d.seq !== seq) { load(); return; } // a new result landed: start over
      if (seq !== g.seq || sort !== g.sort || desc !== g.desc) return; // re-sorted since
      g.pages.set(p, d.cells);
      render();
    } catch (e) {
      dbc.log("err", "could not load rows: " + e.message);
    } finally {
      g.pending.delete(p);
    }
  }

  function cellAt(row, col) {
    const rows = g.pages.get(Math.floor(row / PAGE));
    if (!rows) return undefined;
    const r = rows[row % PAGE];
    return r ? r[g.vis[col]] : undefined;
  }

  // ── columns ────────────────────────────────────────────────────────────
  const widthOf = (rc) => g.userW.get(rc) || Math.round(g.auto[rc] * charW) + PAD;
  const clampW = (px) => Math.max(MIN_CH * charW + PAD, Math.min(px, MAX_CH * charW + PAD));

  // rebuildVis recomputes vis from hidden, keeping the cursor and the
  // range's anchor on the RESULT columns they were on — display indices
  // shift when a column before them comes or goes. One that was itself
  // hidden moves to the next visible column to its right (or the last one),
  // the way deleting a spreadsheet column moves the cursor.
  function rebuildVis() {
    const curRC = g.vis[g.cur.col], ancRC = g.vis[g.anc.col];
    g.vis = [];
    g.cols.forEach((_, c) => { if (!g.hidden.has(c)) g.vis.push(c); });
    g.cur.col = displayOf(curRC);
    g.anc.col = displayOf(ancRC);
    layout();
  }

  function displayOf(rc) {
    if (rc === undefined) return 0;
    const i = g.vis.findIndex((c) => c >= rc);
    return i >= 0 ? i : Math.max(g.vis.length - 1, 0);
  }

  function layout() {
    measureChar();
    g.rnW = Math.round(String(Math.max(g.total, 1)).length * charW) + PAD;
    let x = 0;
    g.colX = []; g.colW = [];
    for (const rc of g.vis) {
      const w = widthOf(rc);
      g.colX.push(x); g.colW.push(w);
      x += w;
    }
    g.width = x;
  }

  // ── drawing ────────────────────────────────────────────────────────────
  let raf = 0, lastHead = "", lastBody = "";
  function render() {
    if (!raf) raf = requestAnimationFrame(() => { raf = 0; draw(); });
  }

  function bounds() {
    if (!g.sel) return [g.cur.row, g.cur.col, g.cur.row, g.cur.col];
    return [Math.min(g.cur.row, g.anc.row), Math.min(g.cur.col, g.anc.col),
      Math.max(g.cur.row, g.anc.row), Math.max(g.cur.col, g.anc.col)];
  }

  function draw() {
    if (!g.seq || root.hidden) return;
    const full = g.rnW + g.width;
    head.style.width = body.style.width = full + "px";
    body.style.height = g.total * ROW_H + "px";

    // the columns in view: the scroll position less the gutter, which stays put
    const left = root.scrollLeft, vw = root.clientWidth - g.rnW;
    let c0 = 0;
    while (c0 < g.vis.length - 1 && g.colX[c0] + g.colW[c0] <= left) c0++;
    let c1 = c0;
    while (c1 < g.vis.length - 1 && g.colX[c1 + 1] < left + vw) c1++;

    // the rows in view
    const top = root.scrollTop, vh = root.clientHeight - ROW_H;
    const r0 = Math.max(0, Math.floor(top / ROW_H) - OVERSCAN);
    const r1 = Math.min(g.total - 1, Math.ceil((top + vh) / ROW_H) + OVERSCAN);

    const [s0, sc0, s1, sc1] = bounds();
    const focused = document.activeElement === root;

    // header
    let h = '<div class="rn hrn"' + ' style="width:' + g.rnW + 'px">#</div>';
    for (let c = c0; c <= c1; c++) {
      const rc = g.vis[c];
      let cls = "hc";
      if (g.numeric[rc]) cls += " num";
      if (rc === g.sort) cls += " sorted";
      if (c >= sc0 && c <= sc1 && g.sel) cls += " in";
      if (hiddenAfter(c)) cls += " gap";
      const arrow = rc === g.sort ? (g.desc ? " ▼" : " ▲") : "";
      h += '<div class="' + cls + '" data-c="' + c + '" style="left:' + (g.rnW + g.colX[c]) + "px;width:" + g.colW[c] +
        'px" title="' + esc(g.cols[rc]) + ' — click to sort, right-click for more"><span class="hn">' + esc(g.cols[rc]) + arrow +
        '</span><span class="rz" data-rz="' + c + '" title="Drag to resize · double-click to fit"></span></div>';
    }
    // written only when it changed: replacing nodes needlessly would
    // detach the one under a pointer mid-gesture
    if (h !== lastHead) head.innerHTML = lastHead = h;

    // body
    let b = "";
    for (let r = r0; r <= r1; r++) {
      const rows = g.pages.get(Math.floor(r / PAGE));
      if (!rows) page(Math.floor(r / PAGE));
      const row = rows && rows[r % PAGE];
      const inRows = r >= s0 && r <= s1;
      b += '<div class="gr' + (r % 2 ? " odd" : "") + '" style="top:' + r * ROW_H + 'px"><div class="rn" style="width:' +
        g.rnW + 'px" data-rn="' + r + '">' + (r + 1) + "</div>";
      for (let c = c0; c <= c1; c++) {
        const rc = g.vis[c];
        let cls = "gc";
        let text;
        if (!row) { text = "…"; cls += " wait"; }
        else if (row[rc] === null) { text = "NULL"; cls += " null"; }
        else { text = flat(row[rc]); if (g.numeric[rc]) cls += " num"; }
        if (inRows && c >= sc0 && c <= sc1) cls += g.sel ? " in" : "";
        if (r === g.cur.row && c === g.cur.col) cls += focused ? " cur" : " cur blur";
        if (hiddenAfter(c)) cls += " gap";
        b += '<div class="' + cls + '" style="left:' + (g.rnW + g.colX[c]) + "px;width:" + g.colW[c] + 'px">' + esc(text) + "</div>";
      }
      b += "</div>";
    }
    if (b !== lastBody) body.innerHTML = lastBody = b;
    drawInfo();
  }

  // hiddenAfter: a hidden column sits between display col c and the next
  // one, which the TUI marks with ║ — here a heavier border.
  function hiddenAfter(c) {
    const next = c + 1 < g.vis.length ? g.vis[c + 1] : g.cols.length;
    return next - g.vis[c] > 1;
  }

  function drawInfo() {
    const parts = [g.total < g.rows ? "showing " + g.total + " of " + g.rows + " rows" : dbc.plural(g.rows, "row")];
    if (g.sort >= 0) parts.push("sorted by " + g.cols[g.sort] + (g.desc ? " desc" : " asc"));
    if (g.hidden.size) parts.push(g.hidden.size + " hidden");
    if (g.sel) {
      const [r0, c0, r1, c1] = bounds();
      parts.push((r1 - r0 + 1) + "×" + (c1 - c0 + 1) + " selected");
    }
    info.textContent = parts.join(" · ");
  }

  root.addEventListener("scroll", render, { passive: true });
  new ResizeObserver(render).observe(root);
  root.addEventListener("focus", render);
  root.addEventListener("blur", render);

  // ── the cursor and the range ───────────────────────────────────────────
  function moveTo(row, col, extend) {
    if (!g.seq || !g.total) return;
    row = Math.max(0, Math.min(row, g.total - 1));
    col = Math.max(0, Math.min(col, g.vis.length - 1));
    if (!extend) { g.anc = { row, col }; g.sel = false; }
    else g.sel = true;
    g.cur = { row, col };
    if (g.sel && g.cur.row === g.anc.row && g.cur.col === g.anc.col) g.sel = false;
    ensureVisible();
    render();
  }

  // ensureVisible scrolls the cursor's cell into view, clear of the sticky
  // header and gutter.
  function ensureVisible() {
    const y = g.cur.row * ROW_H, vh = root.clientHeight - ROW_H;
    if (y < root.scrollTop) root.scrollTop = y;
    else if (y + ROW_H > root.scrollTop + vh) root.scrollTop = y + ROW_H - vh;
    const x = g.colX[g.cur.col], w = g.colW[g.cur.col], vw = root.clientWidth - g.rnW;
    if (x < root.scrollLeft) root.scrollLeft = x;
    else if (x + w > root.scrollLeft + vw) root.scrollLeft = Math.min(x, x + w - vw);
  }

  // hit finds the display cell under a pointer.
  function hit(e) {
    const r = body.getBoundingClientRect();
    const x = e.clientX - r.left - g.rnW, y = e.clientY - r.top;
    const row = Math.max(0, Math.min(Math.floor(y / ROW_H), g.total - 1));
    let col = 0;
    while (col < g.vis.length - 1 && x >= g.colX[col] + g.colW[col]) col++;
    return { row, col, gutter: x < 0 };
  }

  // ── sorting, hiding, resizing ──────────────────────────────────────────
  // sortBy cycles a (display) column through ascending → descending →
  // result order, the three states a header click steps through. The sort
  // is kept by result column, so hiding the sorted column leaves the rows
  // as they are. The server sorts; the loaded pages are dropped.
  function sortBy(col) {
    const rc = g.vis[col];
    if (rc === undefined) return;
    if (g.sort !== rc) { g.sort = rc; g.desc = false; }
    else if (!g.desc) g.desc = true;
    else { g.sort = -1; g.desc = false; }
    g.pages = new Map();
    g.pending = new Set();
    render();
    viewChanged();
  }

  // hide hides display columns c0…c1. It refuses to hide every visible
  // column: an empty grid with a result behind it reads as a failed query,
  // and there would be no header left to right-click to get them back.
  function hide() {
    if (!g.seq) { dbc.log("warn", NO_RESULT); return; }
    const [, c0, , c1] = bounds();
    if (c1 - c0 + 1 >= g.vis.length) {
      dbc.log("warn", "can't hide every column — show some first (+), or narrow the range");
      return;
    }
    const what = c1 > c0 ? dbc.plural(c1 - c0 + 1, "column") : g.cols[g.vis[c0]];
    for (let c = c0; c <= c1; c++) g.hidden.add(g.vis[c]);
    g.sel = false; // the range spanned what just vanished; a shrunken one would mislead
    rebuildVis();
    render();
    viewChanged();
    dbc.log("info", "hid " + what + " — + or the right-click menu shows it again; copies leave hidden columns out");
  }

  // show brings result column rc back and puts the cursor on it, so the
  // user sees where it came back.
  function show(rc) {
    if (!g.hidden.delete(rc)) return;
    rebuildVis();
    g.cur.col = displayOf(rc);
    g.sel = false;
    ensureVisible();
    render();
    viewChanged();
  }

  function showAll() {
    const n = g.hidden.size;
    if (!n) { dbc.log("info", "no columns are hidden"); return; }
    g.hidden.clear();
    rebuildVis();
    render();
    viewChanged();
    dbc.log("info", "showing " + dbc.plural(n, "hidden column") + " again");
  }

  // fit sizes a column to its content — not capped as auto-sizing is:
  // asking to see a column whole is exactly when the cap is in the way.
  function fit(col) {
    const rc = g.vis[col];
    if (rc === undefined) return;
    g.userW.set(rc, clampW(Math.round(g.content[rc] * charW) + PAD));
    layout();
    render();
  }

  // ── copying, exporting, inspecting ─────────────────────────────────────
  const FORMATS = [
    ["Table for Teams / Outlook / Docs (HTML)", "html"],
    ["Markdown table", "markdown"],
    ["CSV", "csv"],
    ["TSV (pastes into a spreadsheet)", "tsv"],
    ["JSON", "json"],
  ];

  // copy renders a piece of the grid on the server and puts it on the
  // clipboard. scope: "sel" (the range, or the cursor's cell), "row" (the
  // cursor's row), "whole" (every row, visible columns). format "plain" is
  // the y key's copy: values, tab-separated, no header.
  function copy(format, scope) {
    if (!g.seq || !g.total && scope !== "whole") { dbc.log("warn", NO_RESULT); return; }
    const req = { seq: g.seq, sort: g.sort, desc: g.desc, format, hidden: g.hidden.size };
    if (scope === "whole") req.cols = g.vis.slice();
    else if (scope === "row") { req.cols = g.vis.slice(); req.rows = [g.cur.row, g.cur.row]; req.row = true; }
    else {
      const [r0, c0, r1, c1] = bounds();
      req.cols = g.vis.slice(c0, c1 + 1);
      req.rows = [r0, r1];
    }
    const html = format === "html";
    const p = dbc.api("POST", dbc.wsPath("/copy"), req);
    dbc.clip.copy(p, html).then(async ({ rich }) => {
      const o = await p;
      let how = "";
      if (html) how = rich ? " — paste into Teams, Outlook or a doc for a formatted table"
        : " — as plain text (the browser would not take the HTML flavor)";
      dbc.log("ok", "copied " + o.what + how);
    }, (e) => dbc.log("err", "copy failed: " + e.message));
  }

  const EXPORTS = [
    ["CSV", "csv"], ["TSV", "tsv"], ["Markdown", "markdown"], ["HTML page", "html"],
    ["JSON", "json"], ["Aligned text", "text"],
  ];

  // exportAs downloads the whole result — display order, hidden columns
  // out, the view a whole-result copy takes — as a file. Fetched rather
  // than linked, so a refusal lands in the log instead of a saved error.
  async function exportAs(format, label) {
    if (!g.seq) { dbc.log("warn", "no result to export — run a query first"); return; }
    const url = dbc.wsPath("/export?format=" + format + "&seq=" + g.seq + "&sort=" + g.sort +
      "&desc=" + (g.desc ? 1 : 0) + "&cols=" + g.vis.join(","));
    try {
      const res = await fetch(url);
      if (!res.ok) {
        let m = res.status + " " + res.statusText;
        try { m = (await res.json()).error || m; } catch (_) { /* not JSON */ }
        throw new Error(m);
      }
      const blob = await res.blob();
      const name = (/filename="([^"]+)"/.exec(res.headers.get("Content-Disposition") || "") || [])[1] || "result." + format;
      const a = el("a", { href: URL.createObjectURL(blob), download: name });
      document.body.append(a);
      a.click();
      a.remove();
      setTimeout(() => URL.revokeObjectURL(a.href), 10000);
      let what = "the result (" + dbc.plural(g.rows, "row");
      if (g.hidden.size) what += ", " + dbc.plural(g.hidden.size, "column") + " hidden";
      dbc.log("ok", "downloaded " + name + " — " + what + ") as " + label);
    } catch (e) {
      dbc.log("err", "export failed: " + e.message);
    }
  }

  // inspect shows one value in full — the grid truncates and flattens,
  // which is right for scanning and wrong for reading a JSON document or a
  // long text. JSON is pretty-printed; anything else is shown as it is.
  function inspect() {
    const v = cellAt(g.cur.row, g.cur.col);
    if (v === undefined) { dbc.log("warn", "no cell selected"); return; }
    const col = g.cols[g.vis[g.cur.col]], row = g.cur.row;
    let text = v, pretty = false;
    if (v !== null) {
      const t = v.trim();
      if (t.startsWith("{") || t.startsWith("[")) {
        try { text = JSON.stringify(JSON.parse(t), null, 2); pretty = true; } catch (_) { /* not JSON */ }
      }
    }
    const pre = el("pre", "ival" + (v === null ? " null" : ""), v === null ? "NULL" : text);
    const cp = el("button", { type: "button", class: "primary" }, "⧉ Copy value");
    const doCopy = () => dbc.clip.copyText(v === null ? "NULL" : v, col + " of row " + (row + 1));
    cp.addEventListener("click", doCopy);
    // the TUI inspector's question; the row number is the one on screen
    const ask = el("button", { type: "button", title: "Draft a question about this value in the assistant" }, "✦ Ask");
    ask.addEventListener("click", () => {
      dbc.modal.close();
      dbc.cmd.askAbout("Explain the " + col + " value in row " + (row + 1) + " of this result.");
    });
    const bodyEl = el("div", "inspect",
      el("div", "ihead", col + " · row " + (row + 1) + (pretty ? " · JSON, formatted" : "")), pre);
    const foot = el("div", "mfoot", cp, ask, el("span", "hint", "y copies · Esc closes"));
    dbc.modal.open({
      title: "Inspect", body: bodyEl, foot, focus: cp, cls: "wide",
      onKey: (e) => {
        if (e.key === "y" && !e.ctrlKey && !e.metaKey) { doCopy(); return true; }
        if (e.key === "Enter" || e.key === "q") { dbc.modal.close(); return true; }
        return false;
      },
    });
  }

  // ── menus ──────────────────────────────────────────────────────────────
  const why = () => (g.seq ? "" : NO_RESULT);
  const copyItems = (scope) => FORMATS.map(([label, f]) => ({ label, why: why(), act: () => copy(f, scope) }));

  // gridMenu is the results pane's context menu — the TUI's, row for row.
  function gridMenu(x, y) {
    const items = [];
    if (g.sel) {
      const [r0, c0, r1, c1] = bounds();
      items.push({ label: "Copy " + (r1 - r0 + 1) + "×" + (c1 - c0 + 1) + " cells", key: "y", why: why(), act: () => copy("plain", "sel") },
        { head: "copy selection as" }, ...copyItems("sel"));
    } else {
      items.push({ label: "Copy value", key: "y", why: why(), act: () => copy("plain", "sel") },
        { label: "Copy row", key: "Y", why: why(), act: () => copy("plain", "row") },
        { label: "Inspect value", key: "Enter", why: why(), act: inspect });
    }
    items.push({ head: "copy whole result as" }, ...copyItems("whole"),
      { head: "" }, { label: "Sort by this column", why: why(), act: () => sortBy(g.cur.col) },
      ...columnItems());
    if (dbc.cmd.showPlan && dbc.cmd.hasPlan && dbc.cmd.hasPlan()) {
      items.push({ head: "" }, { label: "◈ Show the plan", key: "p", act: dbc.cmd.showPlan });
    }
    items.push({ head: "" }, { label: "Export to file…", key: "^E", why: why(), act: () => exportMenuAt(x, y) },
      { label: "✦ Ask the assistant about this result", key: "^I", why: why(),
        act: () => dbc.cmd.askAbout("Explain this result — anything notable in it?") });
    dbc.menu.open(x, y, items);
  }

  // columnItems: hide the cursor's column (or the range's), fit it, and
  // bring hidden ones back — by name, since after hiding several the user
  // remembers names, not positions.
  function columnItems() {
    const [, c0, , c1] = bounds();
    let hideLabel = "Hide column " + (g.cols[g.vis[g.cur.col]] || "");
    if (g.sel && c1 > c0) hideLabel = "Hide " + dbc.plural(c1 - c0 + 1, "column");
    let hideWhy = why();
    if (!hideWhy && c1 - c0 + 1 >= g.vis.length) hideWhy = "can't hide every column — at least one has to stay";
    const items = [
      { label: hideLabel, key: "-", why: hideWhy, act: hide },
      { label: "Fit column to its content", key: "=", why: why(), act: () => fit(g.cur.col) },
    ];
    if (!g.hidden.size) return items;
    items.push({ head: "hidden columns" });
    const hidden = [...g.hidden].sort((a, b) => a - b);
    hidden.slice(0, 8).forEach((rc) => items.push({ label: "Show " + g.cols[rc], act: () => show(rc) }));
    if (hidden.length > 8) items.push({ label: "  … and " + (hidden.length - 8) + " more", why: "use Show all" });
    items.push({ label: "Show all columns", key: "+", act: showAll });
    return items;
  }

  // copyMenuAt is the ⧉ Copy dropdown: the selection when there is one,
  // and the whole result.
  function copyMenuAt(x, y) {
    const items = g.sel ? [{ head: "copy selection as" }, ...copyItems("sel"), { head: "copy whole result as" }]
      : [{ head: "copy result as" }];
    items.push(...copyItems("whole"));
    dbc.menu.open(x, y, items);
  }

  function exportMenuAt(x, y) {
    dbc.menu.open(x, y, [{ head: "download the result as" },
      ...EXPORTS.map(([label, f]) => ({ label, why: g.seq ? "" : "no result to export — run a query first", act: () => exportAs(f, label) }))]);
  }

  const under = (btn) => { const r = btn.getBoundingClientRect(); return [r.left, r.bottom + 2]; };

  // ── mouse ──────────────────────────────────────────────────────────────
  // DOUBLE-CLICKS ARE COUNTED HERE, not left to the browser's dblclick: a
  // press moves the cursor, which redraws the rows under the pointer, and
  // a click whose press and release land on different nodes is not a
  // click (nor a double one) to the browser. Presses are counted by the
  // cell they hit, which a redraw does not change.
  const DOUBLE_MS = 400;
  let lastPress = { t: 0, key: "" };
  function isDouble(e, key) {
    const double = e.timeStamp - lastPress.t < DOUBLE_MS && lastPress.key === key;
    lastPress = double ? { t: 0, key: "" } : { t: e.timeStamp, key };
    return double;
  }

  let drag = null;   // a range drag in progress
  let resize = null; // a column-border drag in progress
  let justResized = false;

  body.addEventListener("pointerdown", (e) => {
    if (e.button !== 0 || !g.total) return;
    root.focus({ preventScroll: true });
    const h = hit(e);
    if (!h.gutter && !e.shiftKey && isDouble(e, "c" + h.row + ":" + h.col)) {
      moveTo(h.row, h.col, false);
      inspect();
      return;
    }
    if (h.gutter) { // a row number: the whole row
      g.anc = { row: h.row, col: 0 };
      g.cur = { row: h.row, col: g.vis.length - 1 };
      g.sel = g.vis.length > 1;
      render();
      return;
    }
    moveTo(h.row, h.col, e.shiftKey);
    drag = { id: e.pointerId };
    body.setPointerCapture(e.pointerId);
  });
  body.addEventListener("pointermove", (e) => {
    if (!drag) return;
    const h = hit(e);
    // past an edge: scroll that way, so a range can outgrow the view
    const r = root.getBoundingClientRect();
    if (e.clientY > r.bottom - 4) root.scrollTop += ROW_H;
    else if (e.clientY < r.top + ROW_H + 4) root.scrollTop -= ROW_H;
    if (h.row !== g.cur.row || h.col !== g.cur.col) moveTo(h.row, h.col, true);
  });
  const endDrag = () => { drag = null; };
  body.addEventListener("pointerup", endDrag);
  body.addEventListener("pointercancel", endDrag);

  root.addEventListener("contextmenu", (e) => {
    e.preventDefault();
    root.focus({ preventScroll: true });
    const hc = e.target.closest(".hc");
    if (hc) {
      const c = +hc.dataset.c;
      if (!g.sel || c < bounds()[1] || c > bounds()[3]) { g.cur.col = c; g.anc.col = c; g.sel = false; render(); }
    } else if (body.contains(e.target) && g.total) {
      const h = hit(e);
      const [r0, c0, r1, c1] = bounds();
      // a right-click outside the range moves the cursor there first, so
      // the menu's "this column" means the one under the pointer
      if (!g.sel || h.row < r0 || h.row > r1 || h.col < c0 || h.col > c1) moveTo(h.row, h.col, false);
    }
    gridMenu(e.clientX, e.clientY);
  });

  head.addEventListener("pointerdown", (e) => {
    const rz = e.target.closest("[data-rz]");
    if (!rz || e.button !== 0) return;
    e.preventDefault();
    const c = +rz.dataset.rz;
    if (isDouble(e, "rz" + c)) { fit(c); return; } // double-click a border: fit
    // the column's left edge stays put for the whole drag — only its own
    // width changes — so the new width is simply pointer x minus that edge
    resize = { c, rc: g.vis[c], x0: e.clientX, w0: g.colW[c] };
    head.setPointerCapture(e.pointerId);
    head.classList.add("resizing");
  });
  head.addEventListener("pointermove", (e) => {
    if (!resize) return;
    g.userW.set(resize.rc, clampW(resize.w0 + e.clientX - resize.x0));
    layout();
    render();
  });
  const endResize = () => {
    if (!resize) return;
    resize = null;
    head.classList.remove("resizing");
    justResized = true; // the click that ends a drag must not sort
    setTimeout(() => { justResized = false; }, 0);
  };
  head.addEventListener("pointerup", endResize);
  head.addEventListener("pointercancel", endResize);
  head.addEventListener("click", (e) => {
    if (justResized || e.target.closest("[data-rz]")) return;
    const hc = e.target.closest(".hc");
    if (hc) sortBy(+hc.dataset.c);
  });

  // ── keys (the grid focused) ────────────────────────────────────────────
  root.addEventListener("keydown", (e) => {
    if (!g.seq || e.defaultPrevented) return;
    const k = e.key, shift = e.shiftKey, mod = e.ctrlKey || e.metaKey;
    const pageRows = Math.max(1, Math.floor((root.clientHeight - ROW_H) / ROW_H) - 1);
    let done = true;
    if (mod) {
      if (k === "c" && !String(window.getSelection())) copy("plain", "sel");
      else if (k === "a") { g.anc = { row: 0, col: 0 }; g.cur = { row: g.total - 1, col: g.vis.length - 1 }; g.sel = true; render(); }
      else if (k === "Home") moveTo(0, g.cur.col, shift);
      else if (k === "End") moveTo(g.total - 1, g.cur.col, shift);
      else done = false;
    } else switch (k) {
      case "ArrowUp": moveTo(g.cur.row - 1, g.cur.col, shift); break;
      case "ArrowDown": moveTo(g.cur.row + 1, g.cur.col, shift); break;
      case "ArrowLeft": moveTo(g.cur.row, g.cur.col - 1, shift); break;
      case "ArrowRight": moveTo(g.cur.row, g.cur.col + 1, shift); break;
      case "PageUp": moveTo(g.cur.row - pageRows, g.cur.col, shift); break;
      case "PageDown": moveTo(g.cur.row + pageRows, g.cur.col, shift); break;
      case "Home": moveTo(g.cur.row, 0, shift); break;
      case "End": moveTo(g.cur.row, g.vis.length - 1, shift); break;
      case "g": moveTo(0, g.cur.col, false); break;
      case "G": moveTo(g.total - 1, g.cur.col, false); break;
      case "Escape": if (g.sel) { g.sel = false; render(); } else done = false; break;
      case "Enter": inspect(); break;
      case "y": copy("plain", "sel"); break;
      case "Y": copy("plain", "row"); break;
      case "-": hide(); break;
      case "+": showAll(); break;
      case "=": fit(g.cur.col); break;
      case "s": sortBy(g.cur.col); break;
      case "p": dbc.cmd.showPlan(); break; // the TUI's p: over to the plan
      case "ContextMenu": case "c": {
        const r = root.getBoundingClientRect();
        gridMenu(r.left + g.rnW + g.colX[g.cur.col] - root.scrollLeft + 10,
          r.top + ROW_H * (g.cur.row + 2) - root.scrollTop);
        break;
      }
      default: done = false;
    }
    if (done) e.preventDefault();
  });

  document.getElementById("copy-btn").addEventListener("click", (e) => copyMenuAt(...under(e.currentTarget)));
  document.getElementById("export-btn").addEventListener("click", (e) => exportMenuAt(...under(e.currentTarget)));

  dbc.grid = {
    load, clear,
    focus: () => { if (!root.hidden) root.focus(); },
    hasResult: () => !!g.seq,
    onView: (fn) => viewFns.push(fn),
    exportMenu: () => exportMenuAt(...under(document.getElementById("export-btn"))),
    // the view, for tests and for the assistant's context (chat.js)
    view: () => ({ seq: g.seq, sort: g.sort, desc: g.desc, hidden: [...g.hidden], vis: g.vis.slice(),
      cur: Object.assign({}, g.cur), sel: g.sel, bounds: bounds(), widths: g.vis.map(widthOf) }),
  };
  clear();
})();
