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
//
// TRANSPOSED (t, or ⇄ Transpose): the grid on its side — each record a
// column, each result column a line with its name in the gutter, as psql's
// \x shows a wide row. Only the PICTURE turns. The cursor, the range, the
// sort and the hidden set stay in the coordinates above (row = record,
// col = result column), so hiding, sorting, inspecting and copying need no
// second implementation; drawing, hit-testing and the arrow keys swap axes,
// and a copy or export asks the server to turn its piece the same way.
//
//   upright              transposed
//   # │ id │ name        column │ 1   │ 2
//   1 │ 1  │ ann    ⇒    id     │ 1   │ 2
//   2 │ 2  │ bob         name   │ ann │ bob
//
// Every record column has ONE width (the widest of the shown columns' auto
// widths, until a drag or a fit sets it), so the columns in view are found
// by division rather than a walk — a 50,000-row result is 50,000 columns
// here. Dragging any record's border resizes them all (flipResizeMove).
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
  const NAME_CH = 40;    // transposed: the widest the gutter of column names grows, in characters
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
    flip: false,       // transposed: records across, columns down
    fnW: 0, recW: 0,   // transposed: the names gutter's width and every record column's, px
    recFit: 0,         // transposed: a record width set by a drag or "fit", px; 0 = auto
    fnFit: 0,          // transposed: a names-gutter width set by a drag or a fit, px; 0 = auto
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
    const ws = dbc.state.ws;
    try {
      d = await query(0, g.seq ? g.sort : -1, g.desc);
      if (d && d.seq !== g.seq && d.sort !== -1) d = await query(0, -1, false); // a new result starts unsorted
    } catch (e) {
      dbc.log("err", "could not load the result: " + e.message);
      return;
    }
    if (ws !== dbc.state.ws) return; // the user switched query tabs meanwhile
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
    if (!same) { g.hidden = new Set(); g.userW = new Map(); g.recFit = 0; g.fnFit = 0; }
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

  // ── query tabs ─────────────────────────────────────────────────────────
  // The grid shows one query tab's result at a time. Switching away takes a
  // snapshot of its view — sort, hidden columns, hand-set widths, cursor,
  // scroll — and switching back restores it, if the result is still the one
  // the snapshot was of (its seq): a result that changed in the background
  // starts fresh, as any new result does.
  function snapshot() {
    if (!g.seq) return null;
    return { seq: g.seq, sort: g.sort, desc: g.desc, hidden: [...g.hidden], userW: [...g.userW],
      cur: Object.assign({}, g.cur), top: root.scrollTop, left: root.scrollLeft, flip: g.flip, recFit: g.recFit, fnFit: g.fnFit };
  }

  async function restore(snap) {
    clear();
    // the orientation belongs to the tab whatever became of its result: a
    // tab left transposed comes back transposed, even to a rerun's rows
    g.flip = !!(snap && snap.flip);
    syncFlip();
    const ws = dbc.state.ws;
    let d;
    try {
      d = await query(0, snap ? snap.sort : -1, snap ? snap.desc : false);
      if (d && (!snap || d.seq !== snap.seq) && d.sort !== -1) d = await query(0, -1, false);
    } catch (e) {
      dbc.log("err", "could not load the result: " + e.message);
      return;
    }
    if (ws !== dbc.state.ws) return;
    if (!d) { clear(); return; }
    adopt(d);
    if (snap && d.seq === snap.seq && d.columns.length) {
      Object.assign(g, { sort: snap.sort, desc: snap.desc, hidden: new Set(snap.hidden), userW: new Map(snap.userW),
        recFit: snap.recFit || 0, fnFit: snap.fnFit || 0 });
      g.cur = { row: Math.min(snap.cur.row, Math.max(g.total - 1, 0)), col: snap.cur.col };
      g.anc = Object.assign({}, g.cur);
      rebuildVis();
      render();
      root.scrollTop = snap.top;
      root.scrollLeft = snap.left;
    }
    viewChanged();
  }

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

    // transposed: the gutter fits the longest shown name (plus room for a
    // sort arrow), capped so one long name cannot crowd out the records;
    // a record column fits the widest shown column — the auto widths are
    // already capped, so one long TEXT value cannot make every record wide
    let nameCh = 0, recCh = String(Math.max(g.total, 1)).length + 1;
    for (const rc of g.vis) {
      nameCh = Math.max(nameCh, g.cols[rc].length + 2);
      recCh = Math.max(recCh, g.auto[rc]);
    }
    g.fnW = g.fnFit || Math.round(Math.min(nameCh, NAME_CH) * charW) + PAD;
    g.recW = g.recFit || Math.round(recCh * charW) + PAD;
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
    if (g.flip) { drawFlip(); return; }
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

  // drawFlip is draw on its side: screen line i is display column i (its
  // name in the sticky gutter), screen column j is display row j. The
  // header numbers the records; a click there selects one, as a click on a
  // row number does upright, and a click on a name sorts by it, as a click
  // on a header does.
  function drawFlip() {
    const W = g.recW, gw = g.fnW, nf = g.vis.length;
    head.style.width = body.style.width = gw + g.total * W + "px";
    body.style.height = nf * ROW_H + "px";

    // the records in view: uniform widths make this a division
    const left = root.scrollLeft, vw = root.clientWidth - gw;
    const j0 = Math.min(Math.floor(left / W), Math.max(g.total - 1, 0));
    const j1 = Math.min(g.total - 1, Math.floor((left + vw) / W));
    // the columns (lines) in view
    const top = root.scrollTop, vh = root.clientHeight - ROW_H;
    const i0 = Math.max(0, Math.floor(top / ROW_H) - OVERSCAN);
    const i1 = Math.min(nf - 1, Math.ceil((top + vh) / ROW_H) + OVERSCAN);

    const [s0, sc0, s1, sc1] = bounds();
    const focused = document.activeElement === root;

    // the pages the records in view live on; page() fetches each once
    if (j1 >= j0) {
      for (let p = Math.floor(j0 / PAGE); p <= Math.floor(j1 / PAGE); p++) if (!g.pages.has(p)) page(p);
    }

    // resize handles, as upright: the corner's widens the names gutter
    // (data-rzn), a record's widens EVERY record (data-rzr = its index,
    // which the drag needs to keep that record's left edge in place)
    let h = '<div class="rn hrn fn" style="width:' + gw + 'px">column' +
      '<span class="rz" data-rzn="1" title="Drag to resize the names · double-click to fit the longest"></span></div>';
    for (let j = j0; j <= j1; j++) {
      const cls = "hc rec" + (g.sel && j >= s0 && j <= s1 ? " in" : "");
      h += '<div class="' + cls + '" data-r="' + j + '" style="left:' + (gw + j * W) + "px;width:" + W +
        'px" title="Row ' + (j + 1) + ' — click to select it, right-click for more">' + (j + 1) +
        '<span class="rz" data-rzr="' + j + '" title="Drag to resize every row · double-click to fit"></span></div>';
    }
    if (h !== lastHead) head.innerHTML = lastHead = h;

    let b = "";
    for (let i = i0; i <= i1; i++) {
      const rc = g.vis[i];
      let ncls = "rn fn";
      if (rc === g.sort) ncls += " sorted";
      if (g.sel && i >= sc0 && i <= sc1) ncls += " in";
      const arrow = rc === g.sort ? (g.desc ? " ▼" : " ▲") : "";
      // a hidden column between this line and the next: the upright ║,
      // turned — a heavier rule under the line (.gr.gap)
      b += '<div class="gr' + (i % 2 ? " odd" : "") + (hiddenAfter(i) ? " gap" : "") + '" style="top:' + i * ROW_H +
        'px"><div class="' + ncls + '" style="width:' + gw + 'px" data-fn="' + i + '" title="' + esc(g.cols[rc]) +
        ' — click to sort, right-click for more">' + esc(g.cols[rc]) + arrow + "</div>";
      const inCols = i >= sc0 && i <= sc1;
      for (let j = j0; j <= j1; j++) {
        const rows = g.pages.get(Math.floor(j / PAGE));
        const row = rows && rows[j % PAGE];
        // values stay left-aligned on their side: a record column mixes
        // numbers and text, and right-aligning only some would be ragged
        let cls = "gc";
        let text;
        if (!row) { text = "…"; cls += " wait"; }
        else if (row[rc] === null) { text = "NULL"; cls += " null"; }
        else text = flat(row[rc]);
        if (g.sel && inCols && j >= s0 && j <= s1) cls += " in";
        if (j === g.cur.row && i === g.cur.col) cls += focused ? " cur" : " cur blur";
        b += '<div class="' + cls + '" style="left:' + (gw + j * W) + "px;width:" + W + 'px">' + esc(text) + "</div>";
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
    if (g.flip) parts.push("transposed");
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
    if (g.flip) {
      // on its side: the record is the screen column, the result column the line
      const y = g.cur.col * ROW_H, vh = root.clientHeight - ROW_H;
      if (y < root.scrollTop) root.scrollTop = y;
      else if (y + ROW_H > root.scrollTop + vh) root.scrollTop = y + ROW_H - vh;
      const x = g.cur.row * g.recW, w = g.recW, vw = root.clientWidth - g.fnW;
      if (x < root.scrollLeft) root.scrollLeft = x;
      else if (x + w > root.scrollLeft + vw) root.scrollLeft = Math.min(x, x + w - vw);
      return;
    }
    const y = g.cur.row * ROW_H, vh = root.clientHeight - ROW_H;
    if (y < root.scrollTop) root.scrollTop = y;
    else if (y + ROW_H > root.scrollTop + vh) root.scrollTop = y + ROW_H - vh;
    const x = g.colX[g.cur.col], w = g.colW[g.cur.col], vw = root.clientWidth - g.rnW;
    if (x < root.scrollLeft) root.scrollLeft = x;
    else if (x + w > root.scrollLeft + vw) root.scrollLeft = Math.min(x, x + w - vw);
  }

  // hit finds the display cell under a pointer. The gutter is judged on
  // SCREEN x, against the grid's own left edge: it is sticky, so once the
  // grid scrolls sideways it sits over cells whose content x is far past
  // it, and a content-x test would read a click on a row number (or, on
  // its side, a column name) as a click on the cell underneath.
  function hit(e) {
    const r = body.getBoundingClientRect();
    const gutter = e.clientX - root.getBoundingClientRect().left < (g.flip ? g.fnW : g.rnW);
    if (g.flip) {
      const x = e.clientX - r.left - g.fnW, y = e.clientY - r.top;
      const col = Math.max(0, Math.min(Math.floor(y / ROW_H), g.vis.length - 1));
      // in the gutter the record stays the cursor's: a name names a line, not a record
      const row = gutter ? g.cur.row : Math.max(0, Math.min(Math.floor(x / g.recW), g.total - 1));
      return { row, col, gutter };
    }
    const x = e.clientX - r.left - g.rnW, y = e.clientY - r.top;
    const row = Math.max(0, Math.min(Math.floor(y / ROW_H), g.total - 1));
    let col = 0;
    while (col < g.vis.length - 1 && x >= g.colX[col] + g.colW[col]) col++;
    return { row, col, gutter };
  }

  // selectRow selects display row r whole — a click on its number upright,
  // on its header transposed.
  function selectRow(r) {
    g.anc = { row: r, col: 0 };
    g.cur = { row: r, col: g.vis.length - 1 };
    g.sel = g.vis.length > 1;
    render();
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
    if (g.flip) {
      // on its side every record shares one width: fit it to the widest
      // shown column's content, past the auto cap, as an upright fit does
      let ch = 0;
      for (const rc of g.vis) ch = Math.max(ch, g.content[rc]);
      g.recFit = clampW(Math.round(ch * charW) + PAD);
      layout();
      render();
      return;
    }
    const rc = g.vis[col];
    if (rc === undefined) return;
    g.userW.set(rc, clampW(Math.round(g.content[rc] * charW) + PAD));
    layout();
    render();
  }

  // ── transposing ────────────────────────────────────────────────────────
  const flipBtn = document.getElementById("flip-btn");

  // syncFlip shows the orientation on the ⇄ Transpose button.
  function syncFlip() {
    flipBtn.setAttribute("aria-pressed", g.flip ? "true" : "false");
  }

  // transpose turns the grid on its side, or back. Allowed with no result:
  // it is then the orientation the next result arrives in. The scroll goes
  // home — its axes just swapped, so the old offsets mean nothing — and
  // the cursor's cell is brought back into view.
  function transpose() {
    g.flip = !g.flip;
    syncFlip();
    root.scrollTop = 0;
    root.scrollLeft = 0;
    if (g.seq) {
      layout();
      ensureVisible();
      render();
    }
    viewChanged();
    dbc.log("info", g.flip ? "transposed — each row is a column now; copies and exports come out the same way (t turns it back)"
      : "upright again — rows across");
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
    // transpose: the piece comes back on its side, as the grid shows it
    const req = { seq: g.seq, sort: g.sort, desc: g.desc, format, hidden: g.hidden.size, transpose: g.flip };
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
        : " — as aligned text (the browser would not take the HTML flavor)";
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
      "&desc=" + (g.desc ? 1 : 0) + "&cols=" + g.vis.join(",") + (g.flip ? "&t=1" : ""));
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
      if (g.flip) what += ", transposed";
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
        { head: g.flip ? "copy selection, transposed, as" : "copy selection as" }, ...copyItems("sel"));
    } else {
      items.push({ label: "Copy value", key: "y", why: why(), act: () => copy("plain", "sel") },
        { label: "Copy row", key: "Y", why: why(), act: () => copy("plain", "row") },
        { label: "Inspect value", key: "Enter", why: why(), act: inspect });
    }
    items.push({ head: wholeHead() }, ...copyItems("whole"),
      { head: "" }, { label: "Sort by this column", why: why(), act: () => sortBy(g.cur.col) },
      ...columnItems(),
      { head: "" }, { label: g.flip ? "Turn upright (rows across)" : "Transpose (each row a column)", key: "t", act: transpose });
    if (dbc.cmd.showPlan && dbc.cmd.hasPlan && dbc.cmd.hasPlan()) {
      items.push({ head: "" }, { label: "◈ Show the plan", key: "p", act: dbc.cmd.showPlan });
    }
    items.push({ head: "" }, { label: "Export to file…", key: "^E", why: why(), act: () => exportMenuAt(x, y) },
      { label: "✦ Ask the assistant about this result", key: "^I", why: why(),
        act: () => dbc.cmd.askAbout("Explain this result — anything notable in it?") });
    dbc.menu.open(x, y, items);
  }

  // wholeHead heads the whole-result copies, saying so when they will come
  // out on their side — a copy that does not look like the result it was
  // pasted from should not be a surprise.
  const wholeHead = () => (g.flip ? "copy whole result, transposed, as" : "copy whole result as");

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
    const items = g.sel ? [{ head: g.flip ? "copy selection, transposed, as" : "copy selection as" }, ...copyItems("sel"),
      { head: wholeHead() }]
      : [{ head: g.flip ? "copy result, transposed, as" : "copy result as" }];
    items.push(...copyItems("whole"));
    dbc.menu.open(x, y, items);
  }

  function exportMenuAt(x, y) {
    dbc.menu.open(x, y, [{ head: g.flip ? "download the result, transposed, as" : "download the result as" },
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
    if (h.gutter) {
      // transposed, the gutter holds the column names — the header's
      // part, so a click sorts; upright it holds row numbers: the whole row
      if (g.flip) sortBy(h.col);
      else selectRow(h.row);
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
    // on its side the rows run sideways, so a range grows past the side edges too
    if (g.flip && e.clientX > r.right - 4) root.scrollLeft += g.recW;
    else if (g.flip && e.clientX < r.left + g.fnW + 4) root.scrollLeft -= g.recW;
    if (h.row !== g.cur.row || h.col !== g.cur.col) moveTo(h.row, h.col, true);
  });
  const endDrag = () => { drag = null; };
  body.addEventListener("pointerup", endDrag);
  body.addEventListener("pointercancel", endDrag);

  root.addEventListener("contextmenu", (e) => {
    e.preventDefault();
    root.focus({ preventScroll: true });
    const hc = e.target.closest(".hc");
    if (hc && g.flip) {
      // a record's header: "this row" in the menu means that one
      const r = +hc.dataset.r;
      if (!g.sel || r < bounds()[0] || r > bounds()[2]) { g.cur.row = r; g.anc.row = r; g.sel = false; render(); }
    } else if (hc) {
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
    if (e.button !== 0) return;
    if (g.flip && flipResizeStart(e)) return;
    const rz = e.target.closest("[data-rz]");
    if (!rz) return;
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
    if (resize.kind) { flipResizeMove(e); return; }
    g.userW.set(resize.rc, clampW(resize.w0 + e.clientX - resize.x0));
    layout();
    render();
  });

  // Transposed resizing. Two borders can be dragged, and a double-click
  // on either fits it, as upright:
  //
  //   column ┃ 1       │ 2       │ 3 …     ┃ = the names gutter's border (data-rzn)
  //   id     ┃ 1       │ 2       │         │ = a record's border (data-rzr = j)
  //
  // Every record shares ONE width (that is what lets drawFlip find the
  // records in view by division), so dragging record j's border widens all
  // of them — and records 0…j-1 widen too, which would carry j's border
  // away from the pointer, j times faster than it moves. The drag scrolls
  // to compensate: scrollLeft grows by j × the width change, so record j's
  // left edge stays put on screen and its right border tracks the pointer,
  // the way an upright column's does.
  function flipResizeStart(e) {
    const rn = e.target.closest("[data-rzn]"), rr = e.target.closest("[data-rzr]");
    if (!rn && !rr) return false;
    e.preventDefault();
    if (rn) {
      if (isDouble(e, "rzn")) { fitNames(); return true; }
      resize = { kind: "names", x0: e.clientX, w0: g.fnW };
    } else {
      const j = +rr.dataset.rzr;
      if (isDouble(e, "rzr" + j)) { fit(g.cur.col); return true; }
      resize = { kind: "rec", j, x0: e.clientX, w0: g.recW, sl0: root.scrollLeft };
    }
    head.setPointerCapture(e.pointerId);
    head.classList.add("resizing");
    return true;
  }

  //
  // The scroll cannot always compensate: a result narrower than the pane
  // has nothing to scroll, and narrowing near either end hits a limit.
  // Then the records left of j move the border as well, so the drag is
  // shared among the j+1 widths that carry it, less whatever scroll the
  // browser did give: with s the scroll it settled on,
  //
  //   border on screen = (j+1)·W − s   (plus constants)
  //   ⇒ W = w0 + (dx + s − sl0) / (j+1)   keeps it under the pointer
  //
  // One correction pass is enough: s barely moves between the two.
  function flipResizeMove(e) {
    const dx = e.clientX - resize.x0;
    if (resize.kind === "names") {
      g.fnFit = clampW(resize.w0 + dx);
      layout();
      render();
      return;
    }
    const { j, w0, sl0 } = resize;
    const apply = (w) => {
      g.recFit = Math.round(w); // whole px: drawFlip divides by it
      layout();
      // grow the scroll area now — draw runs a frame later — or the
      // browser clamps the compensating scrollLeft to the old width
      head.style.width = body.style.width = g.fnW + g.total * g.recW + "px";
      root.scrollLeft = sl0 + j * (g.recW - w0);
    };
    const want = clampW(w0 + dx);
    apply(want);
    const s = root.scrollLeft;
    if (Math.abs(s - (sl0 + j * (want - w0))) > 1) apply(clampW(w0 + (dx + s - sl0) / (j + 1)));
    render();
  }

  // fitNames widens (or narrows) the names gutter to the longest shown name
  // plus room for the sort arrow — past the auto cap, as a fit is.
  function fitNames() {
    let ch = 0;
    for (const rc of g.vis) ch = Math.max(ch, g.cols[rc].length + 2);
    g.fnFit = clampW(Math.round(ch * charW) + PAD);
    layout();
    render();
  }
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
    if (justResized || e.target.closest(".rz")) return;
    const rec = e.target.closest("[data-r]");
    if (rec) { // transposed: a record's number selects it whole
      root.focus({ preventScroll: true });
      selectRow(+rec.dataset.r);
      return;
    }
    const hc = e.target.closest(".hc");
    if (hc) sortBy(+hc.dataset.c);
  });

  // ── keys (the grid focused) ────────────────────────────────────────────
  root.addEventListener("keydown", (e) => {
    if (!g.seq || e.defaultPrevented) return;
    const k = e.key, shift = e.shiftKey, mod = e.ctrlKey || e.metaKey;
    const pageRows = Math.max(1, Math.floor((root.clientHeight - ROW_H) / ROW_H) - 1);
    // The arrows follow the SCREEN: down is the next line, whatever that
    // is. Upright a line is a row; transposed it is a column, and the rows
    // run across. vert/horiz move n that way (moveTo clamps, so ±Infinity
    // is "to the far end").
    const vert = (n) => (g.flip ? moveTo(g.cur.row, g.cur.col + n, shift) : moveTo(g.cur.row + n, g.cur.col, shift));
    const horiz = (n) => (g.flip ? moveTo(g.cur.row + n, g.cur.col, shift) : moveTo(g.cur.row, g.cur.col + n, shift));
    let done = true;
    if (mod) {
      if (k === "c" && !String(window.getSelection())) copy("plain", "sel");
      else if (k === "a") { g.anc = { row: 0, col: 0 }; g.cur = { row: g.total - 1, col: g.vis.length - 1 }; g.sel = true; render(); }
      else if (k === "Home") vert(-Infinity);
      else if (k === "End") vert(Infinity);
      else done = false;
    } else switch (k) {
      case "ArrowUp": vert(-1); break;
      case "ArrowDown": vert(1); break;
      case "ArrowLeft": horiz(-1); break;
      case "ArrowRight": horiz(1); break;
      case "PageUp": vert(-pageRows); break;
      case "PageDown": vert(pageRows); break;
      case "Home": horiz(-Infinity); break;
      case "End": horiz(Infinity); break;
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
      case "t": transpose(); break;
      case "p": dbc.cmd.showPlan(); break; // the TUI's p: over to the plan
      case "ContextMenu": case "c": {
        const r = root.getBoundingClientRect();
        if (g.flip) {
          gridMenu(r.left + g.fnW + g.cur.row * g.recW - root.scrollLeft + 10,
            r.top + ROW_H * (g.cur.col + 2) - root.scrollTop);
        } else {
          gridMenu(r.left + g.rnW + g.colX[g.cur.col] - root.scrollLeft + 10,
            r.top + ROW_H * (g.cur.row + 2) - root.scrollTop);
        }
        break;
      }
      default: done = false;
    }
    if (done) e.preventDefault();
  });

  document.getElementById("copy-btn").addEventListener("click", (e) => copyMenuAt(...under(e.currentTarget)));
  document.getElementById("export-btn").addEventListener("click", (e) => exportMenuAt(...under(e.currentTarget)));
  flipBtn.addEventListener("click", transpose);

  dbc.grid = {
    load, clear, snapshot, restore,
    focus: () => { if (!root.hidden) root.focus(); },
    hasResult: () => !!g.seq,
    onView: (fn) => viewFns.push(fn),
    exportMenu: () => exportMenuAt(...under(document.getElementById("export-btn"))),
    transpose,
    // the view, for tests and for the assistant's context (chat.js)
    view: () => ({ seq: g.seq, sort: g.sort, desc: g.desc, hidden: [...g.hidden], vis: g.vis.slice(), flip: g.flip, recW: g.recW, fnW: g.fnW,
      cur: Object.assign({}, g.cur), sel: g.sel, bounds: bounds(), widths: g.vis.map(widthOf) }),
  };
  clear();
})();
