// dbc web — boot, the event stream, the commands and the keys. Loaded last:
// core.js, ui.js, editor.js and grid.js have set up their pieces of
// window.dbc by now.
//
// The server owns the rules and the rendering; the page owns only what must
// be live. The flow:
//
//   boot ─► the window: reattach (ids kept in sessionStorage, so a reload
//           keeps every tab's pinned session) or open a new one
//        ─► claim the saved query tabs (web.bytdb) no other browser tab of
//           dbc web holds, and show them in their saved order — or, when
//           another holds them all, a fresh tab (see web/claims.go)
//        ─► EventSource on /api/v1/win/<id>/events — ONE per window, for
//           every query tab (see web/hub.go for why)
//        ─► on the first open: activate the saved active tab
//
//   activate(tab) ─► its editor document; its workspace opened on first
//                    use (lazily: a saved tab costs nothing until shown);
//                    its state, result (the grid's view restored) and plan
//
//   Run ─► POST /api/v1/ws/<active ws>/run {buffer, caret, selection, all}
//          409/400 → refused; the server has already logged why
//          200     → notes, ticks and the outcome arrive as events:
//                    "busy" … "tick"* … "run" {hasResult} ─► grid.load()
//
// Every event on the stream is {type, ws, data}, handled by one switch in
// onEvent: the active query tab's are drawn; a background tab's mark its
// tab (busy, done) and prefix their log lines with its title; the
// assistant's (no ws, chat.*) go to chat.js.
(function () {
  "use strict";

  const dbc = window.dbc;
  const { api, log, setStatus, state, el } = dbc;

  // sessionStorage keys: the window, and each query tab's workspace. Per
  // browser tab, so two browser tabs of dbc web are two windows.
  const WIN_KEY = "dbc.win";
  const wsKey = (key) => "dbc.ws." + key;

  // tabs are the query tabs, in strip order:
  //   {key, title, conn, buffer, ws, busy, done, failed, stateful, status, level, grid, planOpen, lost,
  //    cdb, console}
  // key is the saved tab's id (web.bytdb); ws its workspace, "" until
  // first shown. buffer is kept only while the tab is in the background.
  // cdb and console: the SQL console the tab shows — its database
  // {host, database, label, names} and its name — unset before the tab's
  // connection has said which database it is on (see "consoles" below).
  // planOpen: its results pane was on the plan — saved in the layout's
  // "plans" key (see savePlans), so a reload lands back on it.
  // lost: another browser tab of dbc web took this tab over (the server
  // refused a save, or a reclaim, with "held"), so its copy here is no
  // longer saved — see markLost.
  let tabs = [];
  const tabOf = (ws) => tabs.find((t) => t.ws && t.ws === ws);

  const $ = (id) => document.getElementById(id);
  const els = {
    conns: $("conns"), tables: $("tables"), tableCount: $("table-count"),
    tableFilter: $("table-filter"), tableSchema: $("table-schema"), schemaList: $("schema-list"),
    dbFilter: $("db-filter"), tableDb: $("table-db"), dbList: $("db-list"),
    active: $("active-conn"), stateful: $("stateful"), busy: $("busy"),
    run: $("run"), runAll: $("run-all"), stop: $("stop"), history: $("history-btn"), scripts: $("scripts-btn"),
    qtabs: $("qtabs"), theme: $("theme-btn"), help: $("help-btn"),
    splitter: $("splitter"), work: document.querySelector(".work"),
    app: document.querySelector(".app"), sidebar: document.querySelector(".sidebar"),
    sideSplit: $("side-split"), sideFold: $("side-fold"),
    sideConns: $("side-conns"), sideHsplit: $("side-hsplit"),
    logSplit: $("log-split"), log: $("log"), results: $("results"),
  };

  function setBusy(busy) {
    state.busy = busy;
    els.busy.hidden = !busy;
    els.stop.disabled = !busy && !document.querySelector(".conn-item.connecting");
  }

  // ── the sidebar ────────────────────────────────────────────────────────
  // connItem is the Connections list's row for name: its own, or, for a
  // connection derived onto another of a Postgres or MySQL server's
  // databases ("ProdDr/analytics"), its base's — the longest row name that,
  // with a "/", starts it — or null. The server resolves derived names the
  // same way (config.ConnByName), and DERIVES is its SupportsDatabases.
  const DERIVES = /^(postgres|postgresql|pg|pgx|mysql|mariadb)$/i;
  function connItem(name) {
    let best = null;
    for (const b of els.conns.querySelectorAll(".conn-item")) {
      const c = b.dataset.conn;
      if (c === name) return b;
      if (DERIVES.test(b.dataset.driver || "") && name.length > c.length + 1 &&
          name.startsWith(c + "/") && (!best || c.length > best.dataset.conn.length)) best = b;
    }
    return best;
  }

  // markActive marks the active connection's row, and the row of the one
  // being connected to. A connection onto another of a server's databases
  // ("ProdDr/analytics", see config.DatabaseSep) has no row of its own:
  // its base's row is the one marked, and the header names the database.
  function markActive(name, connecting) {
    const base = connItem(name), toward = connecting ? connItem(connecting) : null;
    for (const b of els.conns.querySelectorAll(".conn-item")) {
      b.classList.toggle("active", b === base && !connecting);
      b.classList.toggle("connecting", b === toward);
      if (b === base) {
        state.driver = b.dataset.driver || "";
        dbc.editor.setDriver(state.driver);
      }
    }
    if (!connecting) {
      dbc.editor.warm(name);
      // a disconnected tab (Disconnect in the Connections menu) says so,
      // muted, rather than leaving the header blank
      els.active.textContent = name || "not connected";
      els.active.classList.toggle("none", !name);
    }
    els.stop.disabled = !state.busy && !connecting;
    markInUse();
  }

  // markInUse marks the rows OTHER query tabs are on — in this window or
  // another — from the server's latest "inuse" snapshot (web/inuse.go): a
  // dimmer copy of the active row's bar, and a tooltip naming them. Such a
  // connection is open even when this tab is not on it, and this tab's
  // Disconnect leaves it open (handleDisconnect), so a row without the
  // bright bar is not "not connected" when it has this one.
  //
  // The snapshot lists every tab, the one on screen included: which tab
  // that is changes without a round trip (a click on the strip), so it is
  // left out here, by state.ws, and redrawn on each switch (renderTabs).
  // A tab on another of a server's databases ("ProdDr/analytics") marks
  // its base's row, as markActive does, and the tooltip names the database.
  //
  //   ┃ local-pg     postgres    this tab's connection (.active)
  //   ╎ reports      postgres    another tab's (.inuse): "also open in Query 2"
  //     demo-lite      sqlite    no tab on it
  let inuse = [];
  function markInUse() {
    const on = new Map(); // row → {names: [own tabs' titles], away: other windows' tab count}
    for (const u of inuse) {
      if (u.ws && u.ws === state.ws) continue; // the tab on screen: markActive's bar
      const b = connItem(u.conn);
      if (!b) continue;
      if (!on.has(b)) on.set(b, { names: [], away: 0 });
      const who = on.get(b);
      const db = u.conn !== b.dataset.conn ? " (" + u.conn.slice(b.dataset.conn.length + 1) + ")" : "";
      const t = u.ws ? tabOf(u.ws) : null;
      if (t) who.names.push(t.title + db);
      else who.away++; // another window's tab: its title is that window's to know
    }
    for (const b of els.conns.querySelectorAll(".conn-item")) {
      // the row's own tooltip (conns.js: "added here; …"), kept the first
      // time through so redraws do not stack the in-use line onto it
      if (!("baseTitle" in b.dataset)) b.dataset.baseTitle = b.title || "";
      const who = on.get(b);
      b.classList.toggle("inuse", !!who);
      let line = "";
      if (who) {
        const parts = who.names.slice();
        if (who.away) parts.push(who.away === 1 ? "a tab in another browser window" : who.away + " tabs in other browser windows");
        const n = who.names.length + who.away;
        line = "also open in " + listOf(parts) + " — its connection stays open while " +
          (n > 1 ? "they are" : "that tab is") + " on it";
      }
      b.title = [line, b.dataset.baseTitle].filter(Boolean).join("\n");
    }
  }

  // listOf joins words as a sentence does: "a", "a and b", "a, b and c".
  function listOf(xs) {
    return xs.length < 2 ? xs.join("") : xs.slice(0, -1).join(", ") + " and " + xs[xs.length - 1];
  }

  // The tables list: a click selects, a double-click (or Enter) previews
  // the first 100 rows — the TUI's gestures — c lists the table's
  // information_schema.columns in the grid, e diagrams it and its
  // neighbours (erdview.js), and the right-click menu offers those and
  // inserts or copies the name. The heading's ERD button diagrams them all.
  //
  // The sidebar below the Connections list is drawn from the server's
  // sideState (web/hub.go): the database and schema pickers, then the
  // tables. It comes in two shapes:
  //
  //   - side.navigable (Postgres): the tables are ONE schema's (side.schema,
  //     "" for every schema, which side.allowAll says the database is small
  //     enough for). Picking a schema asks the server for its tables (POST
  //     /schema); they arrive as a "conn" event, as a connect's do. Picking
  //     a database is a connect to that database's connection.
  //   - otherwise: the tables are the whole catalog, and picking a schema
  //     filters them on the page with no round trip.
  //
  //	┌ Tables · 12 / 340 ───── ERD ┐
  //	│ db     [analytics_______] │  #db-filter: hidden with one database
  //	│ schema [sales___________] │  #table-filter: hidden with one schema
  //	│ orders (~1.2M)            │  #tables: the picked schema's tables
  //	└───────────────────────────┘
  let side = { tables: [] };
  let allTables = [];
  // loadingSchema: a schema pick is in flight; the list says so until the
  // "conn" with its tables lands (or the request fails)
  let loadingSchema = false;

  function showSide(s) {
    side = s || { tables: [] };
    allTables = side.tables || [];
    loadingSchema = false;
    dbPicker.draw();
    drawSchemaFilter();
    drawTables();
  }

  // schemaTotal is how many tables the database has: the server's per-schema
  // counts where it sends them, else the list itself, which is then whole.
  // null when the server could not count them in time and sent the schema
  // names alone (each count -1, db.TablesUnknown): a big database's
  // fallback, where a sum of what is known would claim a total it is not.
  function schemaTotal() {
    if (!side.navigable) return allTables.length;
    const schemas = side.schemas || [];
    if (schemas.some((s) => s.tables < 0)) return null;
    return schemas.reduce((n, s) => n + s.tables, 0);
  }

  // drawTables (re)draws the list: allTables narrowed to the picked
  // schema. The count beside the heading says "· shown / all" while a
  // schema is picked, so a narrowed list is never mistaken for the whole.
  function drawTables() {
    const pick = schemaPick();
    const shown = side.navigable || pick === null ? allTables : allTables.filter((t) => t.schema === pick);
    const total = schemaTotal();
    els.tables.replaceChildren();
    // uncounted (total null): the shown tables alone, with no "/ all"
    els.tableCount.textContent = total === null ? (shown.length ? "· " + shown.length : "")
      : !total ? "" : pick === null ? "· " + total : "· " + shown.length + " / " + total;
    if (loadingSchema) {
      els.tables.append(el("li", "none", "loading " + schemaLabel(pick === null ? "all schemas" : pick) + "…"));
      return;
    }
    if (!shown.length) {
      els.tables.append(el("li", "none", "no tables"));
      return;
    }
    for (const t of shown) {
      const li = el("li", { class: t.view ? "view" : "", tabindex: "-1", "data-name": t.qname });
      // the schema prefix is muted, and left off altogether while the
      // list is narrowed to one schema: every row would repeat it.
      // data-name keeps the qualified name either way — it is what
      // previews, inserts and copies use
      const dot = t.qname.lastIndexOf(".");
      if (dot > 0 && pick === null) li.append(el("span", "schema", t.qname.slice(0, dot + 1)), t.qname.slice(dot + 1));
      else if (dot > 0) li.append(t.qname.slice(dot + 1));
      else li.append(t.qname);
      setRowCount(li, t);
      els.tables.append(li);
    }
  }

  // ── the pickers ────────────────────────────────────────────────────────
  // combo wires one of the sidebar's type-to-filter pickers: a search box
  // whose focus opens a list under it, typing narrows the list, and Enter
  // (or a click) picks the highlighted row.
  //
  // A combobox drawn by the page rather than a <select>: a native select's
  // popup is the browser's (or the Mac app's WKWebView's) to draw, and on
  // a server with hundreds of schemas or databases it neither scrolls well
  // nor lets you type more than a letter or two to jump. The list is laid
  // out in the column's flow rather than floated over it, so the sidebar's
  // overflow can never clip it.
  //
  // o describes the picker. Its rows are { value, label, count, cls }:
  //   items()       every row, in order
  //   lead()        a row to put first while nothing is typed ("all
  //                 schemas"), or null
  //   current()     the value picked now, for the highlight on opening
  //   label()       the text the box shows when not being typed in
  //   placeholder() the box's placeholder (shown when label() is "")
  //   narrowed()    whether the pick narrows the list (accent border)
  //   choose(value) act on a pick
  //   enter()       Enter with the list closed, i.e. just after a pick
  // Matching ignores case; rows whose label starts with the text come
  // before those that only contain it, so "sa" lists sales above
  // analytics_sandbox — once there is text, Enter takes the first match.
  function combo(o) {
    const { input, list, wrap } = o;
    let rows = [], hi = -1;

    function show() {
      input.value = o.label();
      input.placeholder = o.placeholder();
      wrap.classList.toggle("on", o.narrowed());
    }

    // open (re)draws the list for the text typed. fresh: the box was just
    // focused, so the current pick is highlighted (Enter keeps it) rather
    // than the first row.
    function open(text, fresh) {
      const q = text.trim().toLowerCase();
      const starts = [], contains = [];
      for (const r of o.items()) {
        const l = r.label.toLowerCase();
        if (!q || l.startsWith(q)) starts.push(r);
        else if (l.includes(q)) contains.push(r);
      }
      const lead = q ? null : o.lead();
      rows = lead ? [lead, ...starts] : [...starts, ...contains];
      hi = rows.length ? 0 : -1;
      if (fresh) hi = Math.max(0, rows.findIndex((r) => r.value === o.current()));

      list.replaceChildren();
      if (!rows.length) list.append(el("li", "none", o.none));
      rows.forEach((r, i) => {
        list.append(el("li", { id: o.idPrefix + i, role: "option", "data-i": String(i), class: r.cls || "" },
          el("span", "sname", r.label), el("span", "scount", r.count === undefined ? "" : String(r.count))));
      });
      list.hidden = false;
      input.setAttribute("aria-expanded", "true");
      mark();
    }

    function close() {
      list.hidden = true;
      list.replaceChildren();
      input.setAttribute("aria-expanded", "false");
      input.removeAttribute("aria-activedescendant");
      rows = [];
      hi = -1;
    }

    // mark shows the highlight and keeps it in view as the arrows walk a
    // list taller than its box.
    function mark() {
      for (const li of list.querySelectorAll("li[data-i]")) {
        const on = Number(li.dataset.i) === hi;
        li.classList.toggle("hi", on);
        li.setAttribute("aria-selected", on ? "true" : "false");
        if (on) {
          li.scrollIntoView({ block: "nearest" });
          input.setAttribute("aria-activedescendant", li.id);
        }
      }
    }

    function pick(i) {
      const r = rows[i];
      close();
      o.choose(r.value);
      show();
    }

    input.addEventListener("focus", () => {
      input.select(); // typing replaces the pick's name
      open("", true);
    });
    input.addEventListener("input", () => open(input.value, false));
    // leaving the box drops what was typed: the pick stands until another
    // is chosen. A click on a row is not a leave (mousedown below).
    input.addEventListener("blur", () => { close(); show(); });
    input.addEventListener("keydown", (e) => {
      const isOpen = !list.hidden;
      if (e.key === "ArrowDown" || e.key === "ArrowUp") {
        e.preventDefault();
        if (!isOpen) { open(input.value, false); return; }
        if (!rows.length) return;
        const step = e.key === "ArrowDown" ? 1 : -1;
        hi = (hi + step + rows.length) % rows.length;
        mark();
      } else if (e.key === "PageDown" || e.key === "PageUp") {
        if (!isOpen || !rows.length) return;
        e.preventDefault();
        const step = e.key === "PageDown" ? 10 : -10;
        hi = Math.min(rows.length - 1, Math.max(0, hi + step));
        mark();
      } else if (e.key === "Enter") {
        e.preventDefault();
        if (isOpen && hi >= 0) pick(hi);
        else if (!isOpen && o.enter) o.enter();
      } else if (e.key === "Escape") {
        // first press drops the typing, a second leaves the box
        e.preventDefault();
        if (isOpen) { close(); show(); } else input.blur();
      }
    });
    // the search box's own clear (×): o.cleared, where the picker has an
    // "everything" to go back to. "search" fires for that and for Enter,
    // which keydown has already handled (and whose pick leaves the box
    // non-empty)
    input.addEventListener("search", () => {
      if (input.value === "" && o.cleared) o.cleared();
    });
    // a click in the box after a pick (focus never left it) reopens the list
    input.addEventListener("click", () => {
      if (list.hidden) { input.select(); open("", true); }
    });
    // mousedown would take focus from the box (closing the list under the
    // click); kept, the click lands on the row
    list.addEventListener("mousedown", (e) => e.preventDefault());
    list.addEventListener("click", (e) => {
      const li = e.target.closest("li[data-i]");
      if (li) pick(Number(li.dataset.i));
    });
    return { show, close };
  }

  // ── the schema picker ──────────────────────────────────────────────────
  // Shown when the database has more than one schema. Each row has its
  // table count — on a navigable server the server's, empty schemas
  // included (0); otherwise counted from the catalog on the page.
  //
  // The pick is per connection — a warehouse's "analytics" means nothing
  // to another connection, and each of a server's databases is a
  // connection of its own here ("ProdDr/analytics") — and kept in
  // schemaPicks, saved in the layout as "tableSchema.<conn>": "=<schema>",
  // or "" for every schema (the "=" keeps a driver's empty schema apart
  // from "all"). A connect sends it (pickFor), so the server opens on it;
  // a schema the database no longer has falls back to the default.
  const schemaPicks = {};
  const PICK_KEY = "tableSchema.";
  // picked: on a page-filtered catalog, the schema the list is narrowed
  // to, null for every schema. schemaCounts: schema → table count, sorted
  // by name.
  let picked = null;
  let schemaCounts = new Map();

  const schemaLabel = (name) => name || "(none)";

  // schemaPick is the schema the list shows, or null for all.
  function schemaPick() {
    if (els.tableFilter.hidden) return null;
    if (side.navigable) return side.schema ? side.schema : null;
    return picked;
  }

  // pickFor is what a connect to name asks to open on: the saved pick.
  function pickFor(name) {
    const v = schemaPicks[name];
    if (v === undefined) return {};
    return v === "" ? { all: true } : { schema: v.slice(1) };
  }

  // drawSchemaFilter reconciles the box with a newly drawn sidebar: shown
  // or hidden, and on a page-filtered catalog the connection's saved pick
  // applied if it still exists.
  function drawSchemaFilter() {
    let counts;
    if (side.navigable) {
      counts = new Map((side.schemas || []).map((s) => [s.name, s.tables]));
    } else {
      counts = new Map();
      for (const t of allTables) counts.set(t.schema, (counts.get(t.schema) || 0) + 1);
      counts = new Map([...counts].sort((x, y) => (x[0] < y[0] ? -1 : x[0] > y[0] ? 1 : 0)));
    }
    schemaCounts = counts;
    const multi = schemaCounts.size > 1;
    els.tableFilter.hidden = !multi;
    schemaPicker.close();
    if (!side.navigable) {
      const saved = schemaPicks[state.active];
      picked = multi && saved && schemaCounts.has(saved.slice(1)) ? saved.slice(1) : null;
    }
    schemaPicker.show();
  }

  // offersAll: whether "all schemas" is a row — always on a page-filtered
  // catalog, and on a navigable one only while it is small enough to list
  // whole (side.allowAll)
  const offersAll = () => !side.navigable || !!side.allowAll;

  // chooseSchema shows name's tables (null: every schema's) and remembers
  // the pick for the connection.
  function chooseSchema(name) {
    const v = name === null ? "" : "=" + name;
    schemaPicks[state.active] = v;
    saveLayout({ [PICK_KEY + state.active]: v });
    if (!side.navigable) {
      picked = name;
      drawTables();
      els.tables.scrollTop = 0;
      return;
    }
    if (name === schemaPick()) return;
    // optimistic: the box and the heading move to the pick at once, and
    // the list says "loading…" until its tables land
    side.schema = name === null ? "" : name;
    loadingSchema = true;
    drawTables();
    els.tables.scrollTop = 0;
    api("POST", dbc.wsPath("/schema"), name === null ? { all: true } : { schema: name }).catch((e) => {
      log("err", e.message);
      loadingSchema = false;
      drawTables();
    });
  }

  const schemaPicker = combo({
    input: els.tableSchema, list: els.schemaList, wrap: els.tableFilter,
    idPrefix: "schema-opt-", none: "no schema matches",
    // an uncounted schema (-1) shows no count rather than a wrong one
    items: () => [...schemaCounts].map(([name, n]) => ({ value: name, label: schemaLabel(name), count: n < 0 ? undefined : n })),
    lead: () => (offersAll() ? { value: null, label: "all schemas", count: schemaTotal(), cls: "all" } : null),
    current: () => schemaPick(),
    label: () => (schemaPick() === null ? "" : schemaLabel(schemaPick())),
    placeholder: () => "all " + schemaCounts.size + " schemas · type to filter",
    narrowed: () => schemaPick() !== null,
    choose: chooseSchema,
    // with the list closed (just picked), Enter moves on to the tables
    enter: () => {
      const first = els.tables.querySelector("li[data-name]");
      if (first) pickTable(first);
    },
    // the box's × shows every schema again, where that is on offer
    cleared: () => { if (schemaPick() !== null && offersAll()) chooseSchema(null); },
  });

  // ── the database picker ────────────────────────────────────────────────
  // Shown when the server holds more than one database the user may
  // connect to (Postgres and MySQL, whose servers list theirs — on MySQL a
  // database is its one schema, so no schema picker follows it, unless its
  // tables happen to span more). Each row is a database; picking
  // one connects the tab to its connection — the configured one for the
  // database its DSN opens, "<conn>/<database>" for the others — which
  // closes the session on the database left, as any switch does.
  const dbPicker = (() => {
    const p = combo({
      input: els.tableDb, list: els.dbList, wrap: els.dbFilter,
      idPrefix: "db-opt-", none: "no database matches",
      items: () => (side.databases || []).map((d) => ({ value: d.conn, label: d.name })),
      lead: () => null,
      current: () => state.active,
      label: () => ((side.databases || []).find((d) => d.current) || { name: "" }).name,
      placeholder: () => (side.databases || []).length + " databases · type to filter",
      narrowed: () => false,
      choose: (conn) => { if (conn !== state.active) connect(conn); },
      // Enter moves on to the next level down: the schema picker, or, with
      // none shown (MySQL, a one-schema database), the tables themselves
      enter: () => {
        if (!els.tableFilter.hidden) {
          els.tableSchema.focus();
          return;
        }
        const first = els.tables.querySelector("li[data-name]");
        if (first) pickTable(first);
      },
    });
    return {
      draw() {
        els.dbFilter.hidden = (side.databases || []).length < 2;
        p.close();
        p.show();
      },
    };
  })();

  // setRowCount shows a table's row count after its name, "cats (1,234)",
  // and leads its tooltip with the count in words, "cats with 1,234 rows".
  // The counts land seconds after the list (a "counts" event; see
  // db/rowcount.go), so this also runs on a row already drawn — patching
  // it in place, which keeps the selection and focus a redraw would drop.
  // The words come from the server, so they read as the TUI's do.
  function setRowCount(li, t) {
    const help = "double-click previews its rows, c shows its columns, e diagrams it, right-click for more";
    li.title = t.rowsHint ? t.rowsHint + "\n" + help : t.qname + " — " + help;
    let span = li.querySelector(".rows");
    if (!t.rows) { if (span) span.remove(); return; }
    if (!span) li.append(span = el("span", "rows"));
    span.textContent = " (" + t.rows + ")";
  }

  // showCounts patches the counts into the rows on screen, by qname. A
  // table the list does not show (it has changed since) is skipped.
  // The counts are also merged into allTables, so the rows of schemas the
  // filter hides have theirs when it is changed.
  function showCounts(tables) {
    const counted = new Map(tables.map((t) => [t.qname, t]));
    for (const t of allTables) {
      const c = counted.get(t.qname);
      if (c) { t.rows = c.rows; t.rowsHint = c.rowsHint; }
    }
    const byName = new Map([...els.tables.querySelectorAll("li[data-name]")].map((li) => [li.dataset.name, li]));
    for (const t of tables) {
      const li = byName.get(t.qname);
      if (li) setRowCount(li, t);
    }
  }

  function pickTable(li) {
    for (const x of els.tables.querySelectorAll("li.sel")) x.classList.remove("sel");
    li.classList.add("sel");
    li.focus();
  }

  els.tables.addEventListener("click", (e) => {
    const li = e.target.closest("li[data-name]");
    if (li) pickTable(li);
  });
  els.tables.addEventListener("dblclick", (e) => {
    const li = e.target.closest("li[data-name]");
    if (li) preview(li.dataset.name);
  });
  els.tables.addEventListener("keydown", (e) => {
    const li = e.target.closest("li[data-name]");
    if (!li) return;
    let next = null;
    if (e.key === "Enter") { e.preventDefault(); preview(li.dataset.name); return; }
    // plain c only: a chord with it (⌘C copying a selection) is not ours
    if (e.key === "c" && !e.ctrlKey && !e.metaKey && !e.altKey) {
      e.preventDefault(); columns(li.dataset.name); return;
    }
    if (e.key === "e" && !e.ctrlKey && !e.metaKey && !e.altKey) {
      e.preventDefault(); dbc.erd.open(li.dataset.name); return;
    }
    if (e.key === "ArrowDown") next = li.nextElementSibling;
    else if (e.key === "ArrowUp") next = li.previousElementSibling;
    if (next) { e.preventDefault(); pickTable(next); }
  });
  document.getElementById("erd-all").addEventListener("click", () => dbc.erd.open(""));
  els.tables.addEventListener("contextmenu", (e) => {
    const li = e.target.closest("li[data-name]");
    if (!li) return;
    e.preventDefault();
    pickTable(li);
    const name = li.dataset.name;
    dbc.menu.open(e.clientX, e.clientY, [
      { head: name },
      { label: "Preview rows", key: "Enter", act: () => preview(name) },
      { label: "Show columns", key: "c", act: () => columns(name) },
      { label: "Diagram around it (ERD)", key: "e", act: () => dbc.erd.open(name) },
      { label: "Insert the name at the caret", act: () => dbc.editor.insert(name) },
      { label: "Copy name", act: () => dbc.clip.copyText(name, "the table name") },
    ]);
  });

  // ── the event stream ───────────────────────────────────────────────────
  function onEvent(ev) {
    const d = ev.data;
    if (!ev.ws) {
      // window-level: the assistant's, or a connection added, edited or
      // removed (in this window or another — every window redraws its
      // sidebar, and follows a rename)
      if (ev.type === "conns") {
        dbc.conns.draw(d.conns);
        if (d.renamed) connRenamed(d.renamed.from, d.renamed.to);
        markInUse(); // the rows are new: their marks and tooltips with them
      } else if (ev.type === "inuse") {
        inuse = d.tabs || [];
        markInUse();
      } else if (ev.type === "console") {
        onConsoleSaved(d);
      } else if (ev.type === "consoles") {
        onConsolesChanged(d);
      } else if (ev.type.startsWith("chat.")) dbc.chat.onEvent(ev.type, d);
      return;
    }
    const t = tabOf(ev.ws);
    if (!t) return; // a tab closed since
    if (t !== state.tab) { onBackground(t, ev.type, d); return; }
    switch (ev.type) {
      case "log":
        log(d.level, d.text);
        break;
      case "busy":
      case "tick":
        setBusy(true);
        setStatus(d.status || d.tag + "…", "warn");
        break;
      case "run":
        setBusy(false);
        els.stateful.hidden = !d.stateful;
        setStatus(d.status || (d.ok ? "done" : "failed"), d.ok ? "" : d.stopped ? "warn" : "err");
        if (d.hasResult) {
          dbc.grid.load();
          if (dbc.cmd.showResults) dbc.cmd.showResults();
        }
        if (dbc.cmd.onRunPlan) dbc.cmd.onRunPlan(d);
        dbc.chat.refresh(); // the last statement, error or result moved
        break;
      case "result": // a script's s.Show, mid-run
        dbc.grid.load();
        if (dbc.cmd.showResults) dbc.cmd.showResults();
        break;
      case "explain":
        setBusy(false);
        if (dbc.cmd.onExplain) dbc.cmd.onExplain(d);
        break;
      case "connecting":
        markActive(state.active, d.name);
        setStatus("connecting to " + d.name + "…", "warn");
        break;
      case "conn":
        state.active = d.active;
        t.conn = d.active;
        markActive(d.active, "");
        if (!d.active) { els.stateful.hidden = true; t.stateful = false; } // its session goes with it
        showSide(d);
        if (d.status) setStatus(d.status);
        else if (!state.busy) setStatus("ready on " + d.active);
        if (d.changed) saveTab(t);
        followConsole(t, d.console);
        dbc.chat.refresh(); // another catalog: other tables' schema
        break;
      case "counts":
        // for a connection this tab has since left: the next "conn" and
        // its own "counts" redraw the list
        if (d.active === state.active) showCounts(d.tables || []);
        break;
    }
    trackTab(t, ev.type, d);
  }

  // trackTab keeps a tab's strip marks in step with its events: busy while
  // it runs, the session-state mark after each run.
  function trackTab(t, type, d) {
    const was = [t.busy, t.stateful, t.done].join();
    if (type === "busy" || type === "tick") t.busy = true;
    else if (type === "run" || type === "explain") {
      t.busy = false;
      t.stateful = !!d.stateful;
      t.failed = !d.ok && !d.stopped;
    }
    if ([t.busy, t.stateful, t.done].join() !== was) renderTabs();
  }

  // onBackground handles a query tab's event while another is on screen:
  // its log lines go to the one log, named; its outcome marks the tab; the
  // rest (its result, its plan) is fetched when the tab is shown.
  function onBackground(t, type, d) {
    switch (type) {
      case "log":
        log(d.level, "[" + t.title + "] " + d.text);
        break;
      case "run":
      case "explain":
        t.done = true;
        t.status = d.status || (d.ok ? "done" : "failed");
        t.level = d.ok ? "" : d.stopped ? "warn" : "err";
        // as the foreground would leave it: an explain shows its plan; a
        // run shows a plan it produced, else its result (onEvent's "run"
        // switches to the grid), else leaves the pane as it was
        if (type === "explain") t.planOpen = !!(d.ok && d.hasPlan);
        else if (d.hasPlan) t.planOpen = true;
        else if (d.hasResult) t.planOpen = false;
        savePlans();
        break;
      case "conn":
        t.conn = d.active;
        if (d.changed) saveTab(t);
        followConsole(t, d.console);
        break;
    }
    trackTab(t, type, d);
  }

  function attach() {
    const src = new EventSource("/api/v1/win/" + state.win + "/events");
    state.source = src;
    src.onmessage = (e) => {
      try { onEvent(JSON.parse(e.data)); } catch (err) { console.error("bad event", e.data, err); }
    };
    src.onopen = () => {
      // the "inuse" announcements made before this stream was on (at boot,
      // or while it was down) went to nobody here: start from the snapshot
      api("GET", "/api/v1/win/" + state.win).then((w) => {
        inuse = (w.inUse && w.inUse.tabs) || [];
        markInUse();
      }, () => {});
      if (!state.attached) {
        state.attached = true;
        activate(activeAtBoot);
      } else {
        resync(); // back after a drop: catch up on anything missed
      }
    };
    src.onerror = async () => {
      setStatus("disconnected — reconnecting…", "warn");
      // EventSource retries by itself, but not past a 404 or 401: find out
      // which it is. A window the server no longer has (a restart) means
      // starting over; a lost session means signing in again.
      try {
        await api("GET", "/api/v1/win/" + state.win);
      } catch (e) {
        if (e.status === 404) {
          src.close();
          forgetSession();
          location.reload();
        } else if (e.status === 401) {
          src.close();
          setStatus("signed out — open the link dbc web printed in the terminal", "err");
        }
      }
    };
  }

  // forgetSession drops the window and every workspace id this browser tab
  // kept. By prefix rather than by the strip's keys: at boot the strip is
  // not built yet, and a duplicated browser tab's copied ids name tabs it
  // may never show.
  function forgetSession() {
    sessionStorage.removeItem(WIN_KEY);
    for (let i = sessionStorage.length - 1; i >= 0; i--) {
      const k = sessionStorage.key(i);
      if (k && k.startsWith(wsKey(""))) sessionStorage.removeItem(k);
    }
  }

  // activate puts query tab t on screen: its document in the editor, its
  // workspace (opened now if this is its first showing), its connection,
  // tables, badge and status, its result with the grid's view as it was
  // left, its plan. Each await re-checks that t is still the one wanted —
  // a quick Alt+2 Alt+3 must end on tab 3, not on whichever answered last.
  let activeAtBoot = null;
  async function activate(t) {
    const prev = state.tab;
    if (prev && prev !== t) {
      prev.buffer = dbc.editor.text();
      const pc = cons.get(docOf(prev));
      if (pc) pc.text = prev.buffer; // what a textarea editor reopens it with
      prev.grid = dbc.grid.snapshot();
      prev.planOpen = dbc.cmd.planOpen();
      saveTab(prev);
      saveConsole(docOf(prev));
    }
    state.tab = t;
    state.ws = t.ws;
    t.done = false;
    const c = cons.get(docOf(t));
    dbc.editor.useDoc(docOf(t), c ? c.text : t.buffer || "");
    renderTabs();
    saveLayout(Object.assign({ tab: t.key }, plansChanged()));
    dbc.cmd.resetPlan();
    dbc.grid.clear();
    setBusy(false);
    els.stateful.hidden = true;
    dbc.editor.focus();

    let st = null;
    if (t.ws) {
      try { st = await api("GET", "/api/v1/ws/" + t.ws); } catch (_) { st = null; } // forgotten: open anew
    }
    if (state.tab !== t) return;
    // A workspace this page already had, with no active connection, was
    // disconnected on purpose (a new one always starts on the default
    // connection): it is shown as it is rather than connected again
    // behind the user's back. A new workspace — the first showing, or one
    // the server forgot (a restart) — connects as before.
    const disconnected = st && !st.active && !st.connecting;
    if (!st) {
      try {
        st = await api("POST", "/api/v1/ws", { win: state.win });
      } catch (e) {
        log("err", "could not open a workspace for " + t.title + ": " + e.message);
        return;
      }
      t.ws = st.id;
      sessionStorage.setItem(wsKey(t.key), t.ws);
    }
    if (state.tab !== t) return;
    state.ws = t.ws;
    if (st.connected || st.connecting || disconnected) {
      applyState(st, true);
      return;
    }
    dbc.chat.onState(st);
    const known = (name) => connItem(name) !== null;
    connect(t.conn && known(t.conn) ? t.conn : state.active && known(state.active) ? state.active : st.active);
  }

  async function resync() {
    reclaim(); // a long drop may have let another browser tab take ours
    // a connection added or removed while the stream was down sent its
    // "conns" event to nobody here
    api("GET", "/api/v1/conns").then((r) => { dbc.conns.draw(r.conns); markInUse(); }, () => {});
    const t = state.tab;
    try {
      const st = await api("GET", dbc.wsPath(""));
      if (state.tab === t) applyState(st, false);
    } catch (_) { /* onerror handles a lost window */ }
    // a background tab may have finished while the stream was down
    for (const b of tabs) {
      if (b === state.tab || !b.ws) continue;
      try {
        const st = await api("GET", "/api/v1/ws/" + b.ws);
        b.busy = st.busy;
        b.stateful = st.stateful;
      } catch (_) { b.ws = ""; }
    }
    renderTabs();
  }

  // applyState draws a workspace's state. fresh: the tab just came on
  // screen, so its result is restored with the grid's view as it was left
  // (and its plan reopened if it was showing); otherwise the view on screen
  // is kept (a reattach after a dropped stream).
  function applyState(st, fresh) {
    const t = state.tab;
    state.active = st.active;
    t.conn = st.active;
    followConsole(t, st.console);
    markActive(st.active, st.connecting || "");
    showSide(st);
    setBusy(st.busy);
    t.busy = st.busy;
    if (!st.busy) { els.stateful.hidden = !st.stateful; t.stateful = st.stateful; }
    if (st.busy) setStatus(st.status, "warn");
    else if (fresh && t.status) setStatus(t.status, t.level);
    else setStatus(st.active ? "ready on " + st.active : "not connected", "");
    if (st.hasResult) {
      if (fresh) dbc.grid.restore(t.grid); else dbc.grid.load();
    }
    if (dbc.cmd.onState) dbc.cmd.onState(Object.assign({}, st, { openPlan: fresh && t.planOpen }));
    dbc.chat.onState(st);
    renderTabs();
  }

  // ── commands ───────────────────────────────────────────────────────────
  // connect switches the tab to name, opening its sidebar on the schema
  // last picked there (pickFor).
  async function connect(name) {
    try {
      await api("POST", dbc.wsPath("/connect"), Object.assign({ name }, pickFor(name)));
    } catch (e) {
      log("err", e.message);
    }
  }

  // disconnect takes the tab off its connection, which stays in the list
  // (POST /disconnect; the "conn" with no active connection empties the
  // sidebar). The server closes the connection's pool too unless another
  // query tab is still on it. A session that may hold a transaction is
  // asked about first, as closing the tab is: the disconnect rolls it back.
  function disconnect() {
    const t = state.tab;
    const go = async () => {
      try {
        await api("POST", dbc.wsPath("/disconnect"));
      } catch (e) {
        log(e.status === 409 ? "warn" : "err", e.message);
      }
    };
    if (!t.stateful) { go(); return; }
    const yes = el("button", { type: "button", class: "primary" }, "Disconnect");
    const no = el("button", { type: "button" }, "Stay connected");
    yes.addEventListener("click", () => { dbc.modal.close(); go(); });
    no.addEventListener("click", () => dbc.modal.close());
    dbc.modal.open({
      title: "Disconnect from " + state.active + "?", focus: no,
      body: el("div", "confirm", el("p", null, t.title + "'s session may hold a transaction, SET values or temp tables. " +
        "Disconnecting closes the session — an open transaction is rolled back.")),
      foot: el("div", "mfoot", yes, no),
    });
  }

  // editorState is what a run or an explain sends: the buffer, the caret
  // and the selection. The server picks the statement from them.
  const editorState = () => ({ buffer: dbc.editor.text(), caret: dbc.editor.caret(), selection: dbc.editor.selection() });

  async function run(all) {
    try {
      await api("POST", dbc.wsPath("/run"), Object.assign(editorState(), { all }));
    } catch (e) {
      // the server logged the refusal's words already (busy, nothing to run)
      setStatus(e.message, e.status === 409 ? "warn" : "err");
    }
  }

  async function preview(name) {
    try {
      await api("POST", dbc.wsPath("/preview"), { name });
    } catch (e) {
      setStatus(e.message, e.status === 409 ? "warn" : "err");
    }
  }

  // columns is "Show columns": the table's information_schema.columns rows,
  // run into the grid like a preview, so the grid's copies take them out
  async function columns(name) {
    try {
      await api("POST", dbc.wsPath("/columns"), { name });
    } catch (e) {
      setStatus(e.message, e.status === 409 ? "warn" : "err");
    }
  }

  async function stop() {
    try {
      await api("POST", dbc.wsPath("/cancel"));
    } catch (e) {
      log("err", e.message);
    }
  }

  // history is Ctrl+P: every statement run here or in the TUI (they share
  // the file), newest first, filtered as you type. Enter puts the chosen
  // one in the editor at the caret — never runs it.
  //
  // It opens scoped to the tab's database when that database has history
  // (the server decides: scope "auto"), and the scope button — or Tab —
  // flips between that database and all of them, keeping the filter. The
  // TUI's picker does the same (tui/modals.go historyModal).
  function history() {
    const input = el("input", { type: "search", class: "hfilter", placeholder: "filter by SQL or connection…",
      "aria-label": "Filter history", spellcheck: "false", autocomplete: "off" });
    const list = el("ul", { class: "hlist", role: "listbox" });
    const count = el("span", "hint");
    const scopeBtn = el("button", { type: "button", class: "linkish", title: "This database, or every database (Tab)" });
    let entries = [], cur = 0, timer = 0, seq = 0;
    // scope is "auto" until the first answer, then whatever it settled on;
    // db is the tab's database ("" with no connection: no toggle then)
    let scope = "auto", db = "";

    function drawScope() {
      scopeBtn.hidden = !db;
      scopeBtn.textContent = scope === "db" ? "● " + db + " · ○ all databases" : "○ " + db + " · ● all databases";
    }
    function flipScope() {
      if (!db) return;
      scope = scope === "db" ? "all" : "db";
      fetchList();
    }
    scopeBtn.addEventListener("click", () => { flipScope(); input.focus(); });

    function draw() {
      list.replaceChildren();
      entries.forEach((h, i) => {
        const when = new Date(h.at);
        const li = el("li", { class: i === cur ? "cur" : "", role: "option", "data-i": String(i), title: h.sql },
          el("span", "hwhen", when.toLocaleDateString() + " " + when.toTimeString().slice(0, 5)),
          el("span", "hconn", h.conn),
          el("span", "hsql", h.sql.replace(/\s+/g, " ").trim()));
        list.append(li);
      });
      count.textContent = entries.length ? dbc.plural(entries.length, "statement") + " · ↑↓ pick · Enter inserts · Tab: scope · Esc closes"
        : scope === "db" && input.value.trim() ? "no statements match on " + db + " — Tab searches every database"
        : "no statements match";
      const c = list.children[cur];
      if (c) c.scrollIntoView({ block: "nearest" });
    }
    async function fetchList() {
      const n = ++seq;
      try {
        const got = await api("GET", dbc.wsPath("/history?q=" + encodeURIComponent(input.value) + "&scope=" + scope));
        if (n !== seq) return;
        entries = got.entries; scope = got.scope; db = got.db; cur = 0;
        drawScope(); draw();
      } catch (e) { log("err", "history: " + e.message); }
    }
    function pick(i) {
      const h = entries[i];
      if (!h) return;
      dbc.modal.close();
      dbc.editor.insert(h.sql);
      log("ok", "inserted a statement from the history — Ctrl+Enter runs it");
    }
    input.addEventListener("input", () => { clearTimeout(timer); timer = setTimeout(fetchList, 120); });
    list.addEventListener("click", (e) => { const li = e.target.closest("li[data-i]"); if (li) { cur = +li.dataset.i; draw(); } });
    list.addEventListener("dblclick", (e) => { const li = e.target.closest("li[data-i]"); if (li) pick(+li.dataset.i); });

    dbc.modal.open({
      title: "History", cls: "wide", focus: input,
      body: el("div", "history", input, list), foot: el("div", "mfoot", count, scopeBtn),
      onKey: (e) => {
        if (e.key === "ArrowDown") { cur = Math.min(cur + 1, entries.length - 1); draw(); return true; }
        if (e.key === "ArrowUp") { cur = Math.max(cur - 1, 0); draw(); return true; }
        if (e.key === "Enter") { pick(cur); return true; }
        // Tab flips the scope rather than walking focus out of the filter:
        // the filter is the only field, and the button is a click away
        if (e.key === "Tab" && db) { flipScope(); return true; }
        return false;
      },
      onClose: () => dbc.editor.focus(),
    });
    fetchList();
  }

  // scripts is Ctrl+O: the Go scripts in scripts_dir (the TUI's picker).
  // Picking one runs it — its s.Print lines reach the log and its s.Show
  // results the grid as they happen; Ctrl+K stops it like any run.
  async function scripts() {
    let got;
    try {
      got = await api("GET", "/api/v1/scripts");
    } catch (e) { log("err", "scripts: " + e.message); return; }
    if (!got.scripts.length) {
      log("warn", "no scripts found in " + got.dir + " — add .go files with func Run(s *sdb.S) error");
      return;
    }
    let cur = 0;
    const list = el("ul", { class: "hlist", role: "listbox" });
    const draw = () => {
      list.replaceChildren(...got.scripts.map((n, i) => el("li", { class: i === cur ? "cur" : "", role: "option", "data-i": String(i) },
        el("span", "hsql", n), el("span", "hwhen", "▶ run"))));
      const c = list.children[cur];
      if (c) c.scrollIntoView({ block: "nearest" });
    };
    const runIt = async (i) => {
      dbc.modal.close();
      try {
        await api("POST", dbc.wsPath("/script"), { name: got.scripts[i] });
      } catch (e) { setStatus(e.message, e.status === 409 ? "warn" : "err"); }
    };
    list.addEventListener("click", (e) => { const li = e.target.closest("li[data-i]"); if (li) runIt(+li.dataset.i); });
    draw();
    dbc.modal.open({
      title: "Scripts · Enter or click runs", body: el("div", "history", list),
      foot: el("div", "mfoot", el("span", "hint", got.dir)),
      onKey: (e) => {
        if (e.key === "ArrowDown") { cur = Math.min(cur + 1, got.scripts.length - 1); draw(); return true; }
        if (e.key === "ArrowUp") { cur = Math.max(cur - 1, 0); draw(); return true; }
        if (e.key === "Enter") { runIt(cur); return true; }
        return false;
      },
      onClose: () => dbc.editor.focus(),
    });
  }

  // connRenamed moves this window's query tabs from a connection's old name
  // to its new one. The server refuses a rename while a tab with a
  // workspace is on the connection, so the tabs this finds are ones not
  // shown yet: their conn is only a note of what to connect to when they
  // are, and left on the old name they would fall back to another
  // connection. The server has moved the saved copies already; this keeps
  // the page's next save from writing the old name back. The "conns" event
  // and the edit's own response may both call it.
  //
  // A connection's other databases ("<from>/analytics", see
  // config.DatabaseSep) follow it too, to "<to>/analytics": their tabs and
  // their schema picks are as much the renamed connection's as its own, and
  // the server retags their saved tabs the same way (handleConnEdit). Left
  // on the old name, such a tab finds no row when shown (connItem) and
  // falls back to the window's connection, and its schema pick is never
  // asked for again.
  //
  // Idempotent by remembering the rename last applied, rather than by the
  // names it leaves: a rename to a name under the old one ("a" to "a/b")
  // would otherwise move "a/x" on to "a/b/x" and then, on the second call,
  // to "a/b/b/x". The same rename cannot rightly arrive twice in a row —
  // after "a" → "b" there is no "a" until something renames "b" back.
  let lastRename = "";
  function connRenamed(from, to) {
    const key = from + "\n" + to;
    if (key === lastRename) return;
    lastRename = key;
    const move = (name) => renamedConn(name, from, to);
    for (const t of tabs) if (t.conn) t.conn = move(t.conn);
    if (state.active) state.active = move(state.active);
    // connection groups follow too (the server has moved the saved ones,
    // moveGroupConns; this keeps this window's in step)
    if (groups.renameConn(move)) { saveGroups(); renderTabs(); }
    // the schema picks follow the connection to its new name; an old key
    // is blanked, as the layout has no delete. One write for them all, so
    // a rename is one layout save however many databases had a pick. The
    // server has already moved the saved ones (moveSchemaPicks), including
    // picks no open window holds; this keeps this window's in step.
    const values = {};
    for (const name of Object.keys(schemaPicks)) {
      const next = move(name);
      if (next === name) continue;
      schemaPicks[next] = schemaPicks[name];
      delete schemaPicks[name];
      values[PICK_KEY + next] = schemaPicks[next];
      values[PICK_KEY + name] = "";
    }
    if (Object.keys(values).length) saveLayout(values);
  }

  // renamedConn is name after the connection from is renamed to to: to
  // itself for from, "<to>/<database>" for a connection derived from it
  // ("<from>/<database>"), and name unchanged otherwise.
  //
  // A configured connection that only happens to be named "<from>/x" is
  // its own connection, not one of from's databases, and keeps its name —
  // the server's derivedFrom draws the same line. It is told apart by
  // having a row of its own in the Connections list, which the "conns"
  // event (or the edit's response) has redrawn before this runs.
  function renamedConn(name, from, to) {
    if (name === from) return to;
    if (!derivedConn(name, from)) return name;
    return to + "/" + name.slice(from.length + 1);
  }

  // derivedConn reports whether name is base's connection onto another of
  // its server's databases, "<base>/<database>" — and not a configured
  // connection that only happens to be named so, which has a row of its own
  // in the Connections list (the server's derivedFrom draws the same line).
  function derivedConn(name, base) {
    const prefix = base + "/";
    if (!name.startsWith(prefix) || name.length === prefix.length) return false;
    for (const b of els.conns.querySelectorAll(".conn-item")) {
      if (b.dataset.conn === name) return false;
    }
    return true;
  }

  // connUnder: a connection group on base holds a tab on conn (tabgroups.js).
  const connUnder = (conn, base) => conn === base || derivedConn(conn, base);

  Object.assign(dbc.cmd, {
    run, stop, history, preview, editorState, scripts, help, newTab, pickTab, connect, disconnect, connRenamed,
    closeTab: () => closeTab(state.tab),
    newConsole: () => newConsole(state.tab),
    nextConsole: () => nextConsole(state.tab),
    exportMenu: () => dbc.grid.exportMenu(),
  });

  // ── keys ───────────────────────────────────────────────────────────────
  // The TUI's chords where the browser lets a page take them (Ctrl+R would
  // otherwise reload, Ctrl+P print), and the web's usual Ctrl+Enter beside
  // them. On a Mac Cmd works as Ctrl. Monaco binds the same chords itself
  // (editor.js) and marks the event handled, so they do not fire twice.
  document.addEventListener("keydown", (e) => {
    if (e.defaultPrevented || dbc.modal.isOpen() || dbc.menu.isOpen()) return;
    // Query tabs are on Alt: a page cannot have Ctrl+T, Ctrl+W or
    // Ctrl+1…9 — the browser keeps its own tabs' keys. e.code, not e.key:
    // on a Mac, Alt+T types "†".
    if (e.altKey && !e.ctrlKey && !e.metaKey) {
      if (e.code === "KeyT") { e.preventDefault(); newTab(); return; }
      if (e.code === "KeyW") { e.preventDefault(); closeTab(state.tab); return; }
      if (e.code === "KeyN") { e.preventDefault(); newConsole(state.tab); return; }
      if (e.code === "KeyC") { e.preventDefault(); nextConsole(state.tab); return; }
      const n = /^Digit([1-9])$/.exec(e.code);
      if (n) { e.preventDefault(); pickTab(+n[1] - 1); return; }
    }
    if (e.key === "F1" || (e.key === "?" && !typing(e.target))) {
      e.preventDefault();
      help();
      return;
    }
    const mod = e.ctrlKey || e.metaKey;
    if (!mod) return;
    const k = e.key.toLowerCase();
    if (k === "enter" || k === "r") {
      e.preventDefault();
      run(e.shiftKey);
    } else if (k === "k") {
      e.preventDefault();
      stop();
    } else if (k === "p") {
      e.preventDefault();
      history();
    } else if (k === "e") {
      e.preventDefault();
      dbc.grid.exportMenu();
    } else if (k === "b" && !e.shiftKey && !e.altKey) {
      // the sidebar, folded and back — VS Code's key for its own side bar
      e.preventDefault();
      setSideHidden(!sideHidden());
    } else if (k === "i" && !e.shiftKey) {
      e.preventDefault();
      dbc.cmd.assistant();
    } else if (k === "o" && !e.shiftKey) {
      e.preventDefault();
      scripts();
    } else if (k === "x" && (e.shiftKey || !dbc.editor.selection())) {
      // explain; with a selection and no Shift it is cut, as ever (the
      // plain editor's path — Monaco binds these itself, see editor.js)
      if (!dbc.editor.hasFocus()) return;
      e.preventDefault();
      dbc.cmd.explain(e.shiftKey);
    }
  });

  // typing is whether a key is going into text — where "?" is a character,
  // not a request for help.
  const typing = (t) => t && (t.isContentEditable || /^(INPUT|TEXTAREA|SELECT)$/.test(t.tagName));

  els.run.addEventListener("click", () => run(false));
  els.runAll.addEventListener("click", () => run(true));
  els.stop.addEventListener("click", stop);
  els.history.addEventListener("click", history);
  els.scripts.addEventListener("click", scripts);

  els.conns.addEventListener("click", (e) => {
    const b = e.target.closest(".conn-item");
    if (b) connect(b.dataset.conn);
  });

  // ── the splitter: drag to size the editor; the height is remembered ────
  els.splitter.addEventListener("pointerdown", (e) => {
    e.preventDefault();
    const startY = e.clientY;
    const startH = dbc.editor.height();
    els.splitter.setPointerCapture(e.pointerId);
    els.splitter.classList.add("dragging");
    const move = (m) => setEditorHeight(startH + m.clientY - startY);
    const up = () => {
      els.splitter.removeEventListener("pointermove", move);
      els.splitter.removeEventListener("pointerup", up);
      els.splitter.classList.remove("dragging");
      api("PUT", "/api/v1/layout", { editorHeight: String(Math.round(dbc.editor.height())) })
        .catch((err) => log("warn", "layout not saved: " + err.message));
    };
    els.splitter.addEventListener("pointermove", move);
    els.splitter.addEventListener("pointerup", up);
  });

  // double-click: back to the stylesheet's share of the column
  els.splitter.addEventListener("dblclick", () => {
    dbc.editor.element.style.removeProperty("height");
    saveLayout({ editorHeight: "" });
  });

  function setEditorHeight(px) {
    // the log sits below the results now at a height of its own choosing,
    // so the editor's room is what the column has less the log's
    const max = els.work.getBoundingClientRect().height - els.log.getBoundingClientRect().height - 120;
    dbc.editor.setHeight(Math.max(60, Math.min(px, max)));
  }

  // ── row bars: results | log, and Connections | Tables ─────────────────
  // dragRows wires a horizontal bar that sizes the pane on one side of it.
  // start() reads that pane's height when the press lands; set(h) applies a
  // height (clamping is set's own business); dir is +1 when the pane is
  // above the bar (dragging down grows it), -1 when below. done() runs on a
  // release that actually moved — a bare click must not turn a natural
  // height into a fixed one — and reset() on a double-click.
  function dragRows(bar, { start, set, dir, done, reset }) {
    bar.addEventListener("pointerdown", (e) => {
      if (e.button !== 0) return;
      e.preventDefault();
      const startY = e.clientY, startH = start();
      let moved = false;
      bar.setPointerCapture(e.pointerId);
      bar.classList.add("dragging");
      document.body.classList.add("row-dragging");
      const move = (m) => {
        if (Math.abs(m.clientY - startY) > 2) moved = true;
        if (moved) set(startH + dir * (m.clientY - startY));
      };
      const up = () => {
        bar.removeEventListener("pointermove", move);
        bar.removeEventListener("pointerup", up);
        bar.removeEventListener("pointercancel", up);
        bar.classList.remove("dragging");
        document.body.classList.remove("row-dragging");
        if (moved) done();
      };
      bar.addEventListener("pointermove", move);
      bar.addEventListener("pointerup", up);
      bar.addEventListener("pointercancel", up);
    });
    bar.addEventListener("dblclick", reset);
  }

  // The log: at least a few lines, at most what leaves the results pane
  // its bar and a couple of rows. Measured live — the editor's height and
  // the window's both move that ceiling.
  function setLogHeight(px) {
    const room = els.log.getBoundingClientRect().height + els.results.getBoundingClientRect().height - 90;
    els.work.style.setProperty("--log-h", Math.round(Math.max(40, Math.min(px, room))) + "px");
  }
  dragRows(els.logSplit, {
    start: () => els.log.getBoundingClientRect().height,
    set: setLogHeight,
    dir: -1, // the log is below its bar: dragging up grows it
    done: () => saveLayout({ logHeight: String(Math.round(els.log.getBoundingClientRect().height)) }),
    reset: () => { els.work.style.removeProperty("--log-h"); saveLayout({ logHeight: "" }); },
  });

  // Connections: from one row under its heading to all but a sliver of the
  // column, which Tables keeps for its own heading. A height here replaces
  // the stylesheet's cap (.sized); the reset hands the cap back.
  function setConnsHeight(px) {
    const h2 = els.sideConns.querySelector("h2");
    const min = (h2 ? h2.offsetHeight : 0) + 30;
    const max = Math.max(min, els.sidebar.clientHeight - 70);
    els.sideConns.style.setProperty("--conns-h", Math.round(Math.max(min, Math.min(px, max))) + "px");
    els.sideConns.classList.add("sized");
  }
  // a saved height the boot could not apply because the column was folded;
  // the first reveal applies it (setSideHidden)
  let pendingConnsH = 0;
  dragRows(els.sideHsplit, {
    start: () => els.sideConns.getBoundingClientRect().height,
    set: setConnsHeight,
    dir: 1,
    done: () => saveLayout({ connsHeight: String(Math.round(els.sideConns.getBoundingClientRect().height)) }),
    reset: () => {
      pendingConnsH = 0;
      els.sideConns.classList.remove("sized");
      els.sideConns.style.removeProperty("--conns-h");
      saveLayout({ connsHeight: "" });
    },
  });

  // ── the sidebar: drag its edge to size it, ‹ or Ctrl+B to fold it ──────
  // Both are saved with the layout ("sideWidth", "sideHidden"), as the
  // assistant pane's width and openness are. The fold is also rendered into
  // the page by the server (Workbench.SideHidden), so a reload does not
  // flash the column before hiding it; the width is applied at boot.
  //
  // The width goes on the root element as --side-w, not on .app: folding
  // sets --side-w on .app (.app.side-off), which has to win while folded yet
  // leave the chosen width untouched, so revealing lands back on it.
  const SIDE_MIN = 150;
  // Dragging the edge this far left of the floor folds the column — live,
  // under the pointer, and dragging back past the same mark brings it
  // straight back. Folding as a preview rather than on release makes the
  // gesture explain itself: the column dragged into nothing stays gone.
  const SIDE_FOLD = Math.round(SIDE_MIN * 0.55);
  // Room for the work column: the widest the sidebar may grow still leaves
  // the editor this much.
  const sideMax = () => Math.max(SIDE_MIN, innerWidth - 400);

  function setSideWidth(px) {
    const w = Math.round(Math.max(SIDE_MIN, Math.min(px, sideMax())));
    document.documentElement.style.setProperty("--side-w", w + "px");
  }

  const sideHidden = () => els.app.classList.contains("side-off");

  // setSideHidden folds or reveals the column. save is false while a drag
  // is still under way: the drag's release saves once, rather than every
  // pass across the fold mark.
  function setSideHidden(hide, save) {
    if (hide === sideHidden()) return;
    els.app.classList.toggle("side-off", hide);
    syncSideTitle();
    if (!hide && pendingConnsH) { setConnsHeight(pendingConnsH); pendingConnsH = 0; }
    // focus inside the column would be left on an element that is gone
    if (hide && els.sidebar.contains(document.activeElement)) dbc.editor.focus();
    if (save !== false) saveLayout({ sideHidden: hide ? "1" : "" });
  }

  // the edge's tooltip says what a press on it does; the server renders
  // the same wording (pages.sideSplitTitle) for the first paint
  function syncSideTitle() {
    els.sideSplit.title = sideHidden()
      ? "Show the sidebar (Ctrl+B)"
      : "Drag to resize the sidebar (double-click to reset, Ctrl+B to hide)";
  }

  // ‹ lives in the column, so it only ever folds; the › tab that takes its
  // place on the edge is what brings the column back
  els.sideFold.addEventListener("click", () => setSideHidden(true));

  // Ctrl+B / ⌘B from inside the editor, which Monaco must be given as a
  // command of its own (editor.js bindKeys): on a Mac it takes Ctrl+B for
  // its emacs-style cursor-left, so the key never reaches the document
  // listener above.
  dbc.cmd.toggleSidebar = () => setSideHidden(!sideHidden());

  // A press that only revealed the column must not have its second half
  // read as a double-click, which would also reset the width it revealed.
  let sideRevealedAt = 0;
  els.sideSplit.addEventListener("pointerdown", (e) => {
    if (e.button !== 0) return;
    e.preventDefault();
    els.sideSplit.setPointerCapture(e.pointerId);
    els.sideSplit.classList.add("dragging");
    document.body.classList.add("side-dragging");
    // Where on the edge it was grabbed, measured from the column's right
    // border (0 when folded — the tab is the whole column), so the edge
    // stays under the pointer rather than jumping by the grab offset on
    // the first move. The sidebar starts at the window's left edge, so the
    // wanted width is simply the pointer's x less that offset.
    const startHidden = sideHidden(), startX = e.clientX;
    const edge = startHidden ? 0 : els.sidebar.getBoundingClientRect().right;
    const grab = startHidden ? 0 : e.clientX - edge;
    // the width before the drag: a drag that ends folded passed through
    // every narrower width on the way down, and the reveal should land on
    // this one, not the last of those
    const startW = document.documentElement.style.getPropertyValue("--side-w");
    let moved = false;
    const move = (m) => {
      if (Math.abs(m.clientX - startX) > 3) moved = true;
      if (!moved) return;
      const want = m.clientX - grab;
      if (want < SIDE_FOLD) { setSideHidden(true, false); return; }
      setSideHidden(false, false); // dragged back out of the fold zone
      setSideWidth(want);
    };
    const up = () => {
      els.sideSplit.removeEventListener("pointermove", move);
      els.sideSplit.removeEventListener("pointerup", up);
      els.sideSplit.removeEventListener("pointercancel", up);
      els.sideSplit.classList.remove("dragging");
      document.body.classList.remove("side-dragging");
      // A press on the folded tab that never became a drag is a click,
      // and the one thing a click there can mean is "show it again".
      if (startHidden && !moved) {
        setSideHidden(false, false);
        sideRevealedAt = Date.now();
      }
      if (sideHidden() && !startHidden) {
        if (startW) document.documentElement.style.setProperty("--side-w", startW);
        else document.documentElement.style.removeProperty("--side-w");
      }
      const values = {};
      if (sideHidden() !== startHidden) values.sideHidden = sideHidden() ? "1" : "";
      // Only a real drag that ended with the column showing is a new
      // width: folding it is its own setting, not a width of 0.
      if (moved && !sideHidden()) values.sideWidth = String(Math.round(els.sidebar.getBoundingClientRect().width));
      if (Object.keys(values).length) saveLayout(values);
    };
    els.sideSplit.addEventListener("pointermove", move);
    els.sideSplit.addEventListener("pointerup", up);
    els.sideSplit.addEventListener("pointercancel", up);
  });
  // double-click hands the column back to the stylesheet's default width;
  // "" because the layout has no delete
  els.sideSplit.addEventListener("dblclick", () => {
    if (sideHidden() || Date.now() - sideRevealedAt < 600) return; // the reveal click, twice
    document.documentElement.style.removeProperty("--side-w");
    saveLayout({ sideWidth: "" });
  });

  // ── consoles: a tab's text is one of its database's SQL consoles ───────
  // The running .sql files the TUI keeps too (web/consoles.go), namespaced
  // host ─► database ─► name. A tab shows ONE console of the database its
  // connection is on, and follows the connection:
  //
  //   "conn" {console: {host, database, label, names}} ─► followConsole(t)
  //      same database as the tab's console ─► nothing
  //      another ─► save the old console, then show, of the new database,
  //                 the first console no other tab here shows — or a new
  //                 one ("console-2"…), which gets its file once typed in
  //
  // A tab from before consoles (its own buffer, never moved in because it
  // had no connection then) seeds a new console with that buffer instead.
  //
  // Documents are per console, not per tab (docOf): two tabs here showing
  // one console share it, edits and all. The files are shared with other
  // windows, the TUI and any editor, so a save says the revision it was
  // made from, and the server refuses to overwrite a file that moved on:
  //
  //   PUT {text, base: rev} ─► {rev}                 saved
  //                         ─► {conflict, text, rev} not saved: the file's
  //                            text is loaded as ONE undoable edit, so
  //                            Ctrl+Z gets this tab's text back (and the
  //                            next save, from the new rev, writes it)
  //
  // Another window's save arrives as "console" and is loaded the same way
  // when this window has no unsaved edits to that console; with some, the
  // next save meets the conflict above.
  //
  // cons: doc key ─► {cdb, name, rev, saved, text, saving, again}
  //   rev: the file's revision as last loaded or saved ("" = no file)
  //   saved: the text at that revision; text: the text when last seen
  //   (what a textarea editor reopens it with); saving: the PUT in flight
  const cons = new Map();
  const ckey = (cdb, name) => "c:" + cdb.host + "/" + cdb.database + "/" + name;
  const cpath = (cdb, name) => "/api/v1/consoles/" + encodeURIComponent(cdb.host) + "/" +
    encodeURIComponent(cdb.database) + (name === undefined ? "" : "/" + encodeURIComponent(name));
  const sameDB = (a, b) => !!a && !!b && a.host === b.host && a.database === b.database;
  // docOf is the editor document a tab shows: its console's, else its own
  const docOf = (t) => (t.console && t.cdb ? ckey(t.cdb, t.console) : t.key);

  // textOf is a tab's text: its document's, wherever that is
  function textOf(t) {
    const v = dbc.editor.docText(docOf(t));
    if (v !== null) return v;
    const c = cons.get(docOf(t));
    return c ? c.text : t.buffer || "";
  }

  // nextConsoleName is userdata.NextConsoleName: "console" if free, else
  // the lowest free "console-n"
  function nextConsoleName(taken) {
    if (!taken.includes("console")) return "console";
    let n = 2;
    while (taken.includes("console-" + n)) n++;
    return "console-" + n;
  }

  // loadConsole reads a console once; later calls get the copy here, which
  // the saves and "console" events keep current.
  async function loadConsole(cdb, name) {
    const k = ckey(cdb, name);
    if (cons.has(k)) return cons.get(k);
    const r = await api("GET", cpath(cdb, name));
    if (!cons.has(k)) cons.set(k, { cdb, name, rev: r.rev, saved: r.text, text: r.text, saving: null, again: false });
    return cons.get(k);
  }

  // saveConsole writes document k's console if it changed. One PUT at a
  // time per console: a second save waits for the first's revision rather
  // than send a stale base and meet a conflict of its own making.
  function saveConsole(k, keepalive) {
    const c = cons.get(k);
    if (!c) return Promise.resolve(); // not a console's document
    if (c.saving) { c.again = true; return c.saving; }
    const text = dbc.editor.docText(k) ?? c.text;
    c.text = text;
    if (text === c.saved) return Promise.resolve();
    c.saving = fetch(cpath(c.cdb, c.name) + winQuery(), {
      method: "PUT", headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ text, base: c.rev }), keepalive: !!keepalive,
    }).then((res) => res.json()).then((env) => {
      if (!env.success) { log("warn", "console " + c.name + " not saved: " + env.error); return; }
      const r = env.data;
      if (r.conflict) {
        adopt(c, r.text, r.rev);
        log("warn", "console " + c.cdb.label + " · " + c.name + " was changed elsewhere (another window, the TUI or " +
          "an editor) — this tab now shows that version; Ctrl+Z brings back yours, which then saves over it");
        return;
      }
      c.rev = r.rev;
      c.saved = text;
    }).catch(() => { /* best effort: the next edit saves again */ }).finally(() => {
      c.saving = null;
      if (c.again) { c.again = false; saveConsole(ckey(c.cdb, c.name)); } // its key may have been renamed meanwhile
    });
    return c.saving;
  }

  // adopt makes text, at rev, the console's — in its document as one edit.
  function adopt(c, text, rev) {
    c.rev = rev;
    c.saved = text;
    c.text = text;
    dbc.editor.replaceDoc(ckey(c.cdb, c.name), text);
  }

  // onConsoleSaved is another window's save: loaded here unless this window
  // has edits of its own to the console still unsaved.
  function onConsoleSaved(d) {
    if (d.win === state.win) return;
    const c = cons.get(ckey(d, d.name));
    if (!c || c.saving || (dbc.editor.docText(ckey(d, d.name)) ?? c.text) !== c.saved) return;
    adopt(c, d.text, d.rev);
  }

  // leaveDoc is for document k once no tab here shows it. Its text is
  // saved first (saveConsole reads it now, so nothing typed is lost). A
  // console's document is then KEPT for the page's life, so a tab that
  // comes back to the console — a connect back to its database, Alt+C, the
  // tab menu, a new tab — finds its caret, scroll and undo history where
  // they were, as the TUI does. That is safe because the document stays
  // current while hidden: another window's save lands in it ("console" ─►
  // adopt ─► replaceDoc works on any document, shown or not), and a save
  // from it that meets a newer file loads that file as one undoable edit.
  // The cost is one Monaco model per console opened, a handful of small
  // texts. A tab's own document (no console) has no one to come back to
  // it, and a deleted console's goes with it (onConsolesChanged), so those
  // are dropped.
  function leaveDoc(k) {
    saveConsole(k);
    if (!cons.has(k)) dbc.editor.dropDoc(k);
  }

  // followConsole keeps tab t on a console of the database its connection
  // is on (ref, from a sidebar; null with no connection, when the tab keeps
  // the console it has). Chained per tab, so two connects in a row swap in
  // order.
  function followConsole(t, ref) {
    if (!ref) return;
    if (t.console && sameDB(t.cdb, ref)) {
      t.cdb.names = ref.names; // the freshest list
      return;
    }
    t.chain = (t.chain || Promise.resolve()).then(() => {
      if (!tabs.includes(t) || (t.console && sameDB(t.cdb, ref))) return;
      const text = t.console ? "" : textOf(t);
      const seed = text.trim() !== "" ? text : "";
      const used = tabs.filter((o) => o !== t && o.console && sameDB(o.cdb, ref)).map((o) => o.console);
      let name = seed ? "" : ref.names.find((n) => !used.includes(n));
      if (!name) name = nextConsoleName(ref.names.concat(used));
      return showConsole(t, ref, name, seed).then((shown) => {
        if (shown && t === state.tab) log("info", "console: " + ref.label + " · " + name);
      });
    });
  }

  // showConsole puts console name of database cdb in tab t, saving the one
  // it showed. seed, for a console with no text yet, becomes its first
  // text (and is saved). It reports whether the console could be read.
  async function showConsole(t, cdb, name, seed) {
    const oldKey = docOf(t);
    if (t.console) await saveConsole(oldKey);
    let c;
    try {
      c = await loadConsole(cdb, name);
    } catch (e) {
      log("err", "could not open console " + name + ": " + e.message);
      return false;
    }
    t.cdb = cdb;
    t.console = name;
    const k = docOf(t);
    if (seed && c.text === "" && dbc.editor.docText(k) === null) c.text = seed;
    t.buffer = c.text;
    if (t === state.tab) dbc.editor.useDoc(k, c.text);
    // the document left, if no tab here shows it now: anything typed into
    // it while this was loading is saved (leaveDoc)
    if (oldKey !== k && !tabs.some((o) => docOf(o) === oldKey)) leaveDoc(oldKey);
    saveTab(t);
    if (seed) saveConsole(k);
    renderTabs();
    return true;
  }

  // refreshNames re-reads a database's console list into cdb.names, giving
  // up quietly: the list on hand is only a little stale.
  function refreshNames(cdb) {
    return api("GET", cpath(cdb)).then((r) => { cdb.names = r.names; }, () => {});
  }

  // consoleNames is the console list a menu or Alt+C offers for t: the
  // database's files, plus consoles tabs here show that have none yet.
  function consoleNames(t) {
    const names = (t.cdb.names || []).slice();
    for (const o of tabs) {
      if (o.console && sameDB(o.cdb, t.cdb) && !names.includes(o.console)) names.push(o.console);
    }
    return names;
  }

  // consoleItems are the tab menu's console rows: the database's consoles
  // (the tab's marked, others' tabs named), and new, rename and delete.
  function consoleItems(t) {
    if (!t.cdb) return [];
    const items = [{ head: "consoles · " + t.cdb.label }];
    for (const n of consoleNames(t)) {
      const other = tabs.find((o) => o !== t && o.console === n && sameDB(o.cdb, t.cdb));
      items.push({ label: (n === t.console ? "● " : "   ") + n + (other ? "  · in " + other.title : ""),
        why: n === t.console ? "it is the one in this tab" : "", act: () => showConsole(t, t.cdb, n, "") });
    }
    return items.concat([
      { label: "+ New console", key: "Alt+N", act: () => newConsole(t) },
      { label: "Next console", key: "Alt+C", act: () => nextConsole(t) },
      { label: "Rename console…", act: () => renameConsole(t) },
      { label: "Delete console…", act: () => deleteConsole(t) },
    ]);
  }

  // newConsole (Alt+N) puts a fresh console of the tab's database in it.
  async function newConsole(t) {
    if (!t || !t.cdb) { log("warn", "no console to add to: connect to a database first"); return; }
    await refreshNames(t.cdb);
    await showConsole(t, t.cdb, nextConsoleName(consoleNames(t)), "");
    if (t === state.tab) { log("info", "console: " + t.cdb.label + " · " + t.console); dbc.editor.focus(); }
  }

  // nextConsole (Alt+C) moves the tab to its database's next console.
  async function nextConsole(t) {
    if (!t || !t.cdb) { log("warn", "no consoles: connect to a database first"); return; }
    await refreshNames(t.cdb);
    const names = consoleNames(t);
    if (names.length < 2) { log("info", "this database has one console — Alt+N adds another"); return; }
    await showConsole(t, t.cdb, names[(names.indexOf(t.console) + 1) % names.length], "");
    if (t === state.tab) { log("info", "console: " + t.cdb.label + " · " + t.console); dbc.editor.focus(); }
  }

  // renameConsole asks for a new name. The server tells every window, and
  // each moves its tabs along (onConsolesChanged).
  function renameConsole(t) {
    if (!t.cdb || !t.console) return;
    const input = el("input", { class: "hfilter", value: t.console, maxlength: "64", "aria-label": "Console name", spellcheck: "false" });
    const go = el("button", { type: "button", class: "primary" }, "Rename");
    const no = el("button", { type: "button" }, "Cancel");
    const submit = async () => {
      const to = input.value.trim();
      dbc.modal.close();
      if (!to || to === t.console) return;
      await saveConsole(docOf(t)); // its latest text goes with it
      api("POST", cpath(t.cdb, t.console) + "/rename", { to }).catch((e) => log("err", "rename: " + e.message));
    };
    go.addEventListener("click", submit);
    no.addEventListener("click", () => dbc.modal.close());
    input.addEventListener("keydown", (e) => { if (e.key === "Enter") { e.preventDefault(); submit(); } });
    dbc.modal.open({ title: "Rename console " + t.console, focus: input,
      body: el("div", "confirm", el("p", null, "Letters, digits, '.', '-' and '_'. Every tab showing it follows."), input),
      foot: el("div", "mfoot", go, no) });
  }

  // deleteConsole removes the tab's console file, after asking. Tabs
  // showing it, in every window, move to another console of the database.
  function deleteConsole(t) {
    if (!t.cdb || !t.console) return;
    const name = t.console, cdb = t.cdb;
    const yes = el("button", { type: "button", class: "primary" }, "Delete");
    const no = el("button", { type: "button" }, "Keep it");
    yes.addEventListener("click", () => {
      dbc.modal.close();
      api("DELETE", cpath(cdb, name)).catch((e) => log("err", "delete: " + e.message));
    });
    no.addEventListener("click", () => dbc.modal.close());
    dbc.modal.open({ title: "Delete console " + name + "?", focus: no,
      body: el("div", "confirm", el("p", null, "The console " + cdb.label + " · " + name + " and the SQL in it are deleted, " +
        "here, in other windows and for the TUI. Tabs showing it move to another console.")),
      foot: el("div", "mfoot", yes, no) });
  }

  // onConsolesChanged is the "consoles" event: a database's consoles were
  // renamed or deleted, by this window or another.
  function onConsolesChanged(d) {
    for (const t of tabs) if (t.cdb && sameDB(t.cdb, d)) t.cdb.names = d.names;
    if (d.renamed) {
      const from = ckey(d, d.renamed.from), to = ckey(d, d.renamed.to);
      const c = cons.get(from);
      if (c) { cons.delete(from); c.name = d.renamed.to; cons.set(to, c); }
      dbc.editor.renameDoc(from, to);
      for (const t of tabs) {
        if (t.cdb && sameDB(t.cdb, d) && t.console === d.renamed.from) { t.console = d.renamed.to; saveTab(t); }
      }
      renderTabs();
    }
    if (d.deleted) {
      const k = ckey(d, d.deleted);
      cons.delete(k); // nothing more is saved to it
      // a document no tab here shows (kept by leaveDoc) goes now; one a
      // tab shows goes once that tab has moved to another console
      if (!tabs.some((o) => docOf(o) === k)) dbc.editor.dropDoc(k);
      for (const t of tabs.filter((o) => docOf(o) === k)) {
        const cdb = t.cdb;
        t.console = "";
        t.buffer = "";
        // as a connect would, but with nothing to seed: the SQL went with
        // the console, on purpose
        const used = tabs.filter((o) => o.console && sameDB(o.cdb, cdb)).map((o) => o.console);
        const name = d.names.find((n) => !used.includes(n)) || nextConsoleName(d.names.concat(used));
        t.chain = (t.chain || Promise.resolve()).then(() => showConsole(t, cdb, name, "")).then(() => {
          if (!tabs.some((o) => docOf(o) === k)) dbc.editor.dropDoc(k);
        });
      }
    }
  }

  // ── saving tabs: every tab's buffer, title and connection survive a restart
  let saveTimer = 0;

  function scheduleSave() {
    clearTimeout(saveTimer);
    saveTimer = setTimeout(() => { saveTab(state.tab); saveConsole(docOf(state.tab)); }, 600);
  }

  // tabBody is what a save sends for t: its text (the editor's when it is
  // on screen) and the console it shows. With a console the text is the
  // console's, kept in the tab's buffer too, so a dbc from before consoles
  // still opens the tab as it was.
  function tabBody(t) {
    const active = t === state.tab;
    return { title: t.title, conn: (active ? state.active : t.conn) || "",
      buffer: active ? dbc.editor.text() : textOf(t), console: t.console || "" };
  }

  // winQuery names this window on a save, a delete or a layout write: the
  // server checks it against the tab's claim (web/claims.go).
  const winQuery = () => "?win=" + encodeURIComponent(state.win);

  function saveTab(t, keepalive) {
    if (!t) return;
    if (t === state.tab) clearTimeout(saveTimer);
    if (t.lost) return; // not ours to save any more
    // keepalive lets the last save outlive the page when it is closing
    return fetch("/api/v1/tabs/" + encodeURIComponent(t.key) + winQuery(), {
      method: "PUT", headers: { "Content-Type": "application/json" }, body: JSON.stringify(tabBody(t)),
      keepalive: !!keepalive,
    }).then((res) => {
      if (res.status === 409) markLost(t); // another browser tab holds it
    }).catch(() => { /* best effort: the next edit saves again */ });
  }

  function saveLayout(values) {
    api("PUT", "/api/v1/layout" + winQuery(), values).catch((err) => log("warn", "layout not saved: " + err.message));
  }

  // ── claims: which browser tab of dbc web shows which saved tab ─────────
  // A saved tab is shown — and saved — by one window at a time; the server
  // keeps the claims (web/claims.go). Boot claims the free ones; a save
  // claims a new one; the page's goodbye (pagehide) releases them all, so a
  // browser tab opened next, or this one reloading, takes them.

  // markLost: another window holds t now (this one's stream was gone long
  // enough for it to be taken). Its copy here stays usable — its session
  // may hold a transaction to finish — but is no longer saved: saving it
  // would overwrite the other window's edits, the very loss claims exist
  // to prevent.
  function markLost(t) {
    if (!t || t.lost) return;
    t.lost = true;
    log("warn", t.title + " is open in another browser tab of dbc web — edits to it here are no longer saved " +
      "(close it here, or reload this page once that one is closed)");
    renderTabs();
  }

  // reclaim re-asserts this window's claims after the stream was away (or
  // the page came back from the back-forward cache, after its pagehide let
  // them go). A tab another window took meanwhile is marked lost. Lost tabs
  // are not asked for again: their text here may be older than the saved.
  async function reclaim() {
    if (!state.win) return;
    const keys = tabs.filter((t) => !t.lost).map((t) => t.key);
    try {
      const got = await api("POST", "/api/v1/win/" + state.win + "/tabs", { keys });
      for (const k of got.held) markLost(tabs.find((t) => t.key === k));
    } catch (_) { /* a lost window: the stream's onerror starts over */ }
  }

  // release is the page's goodbye: the active tab's last save and the
  // window's claims let go, in one request, so the save cannot land after
  // the release and claim the tab straight back.
  function release() {
    if (!state.win) return;
    const t = state.tab;
    clearTimeout(saveTimer);
    const save = t && !t.lost ? Object.assign({ id: t.key }, tabBody(t)) : undefined;
    // the shown console's last edits, if any are unsaved (a background
    // tab's were saved when it was left)
    const c = t && cons.get(docOf(t));
    const text = c && dbc.editor.text();
    const console = c && text !== c.saved && !c.saving
      ? { host: c.cdb.host, database: c.cdb.database, name: c.name, text, base: c.rev } : undefined;
    fetch("/api/v1/win/" + encodeURIComponent(state.win) + "/release", {
      method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ save, console }), keepalive: true,
    }).catch(() => { /* the claims lapse on their own after the grace */ });
  }

  // ── saving which tabs show their plan ──────────────────────────────────
  // One layout key, "plans": the keys of the query tabs whose results pane
  // was on the plan, comma-separated. A layout key rather than a column on
  // the tabs table: bytdb has no ADD COLUMN IF NOT EXISTS, so a column
  // would need a hand-rolled migration, and the order and the active tab
  // already live in the layout. One key rather than one per tab: it is
  // rewritten whole, so a closed tab drops out of it instead of leaving a
  // key behind (the layout has no delete).
  //
  // Only the tab's wish is kept, not the plan: after a reload the page
  // reattaches to the same workspace, whose plan onState reloads and, with
  // planOpen set, shows. After a server restart the workspace is new and
  // has no plan, so the flag is simply not acted on.
  let savedPlans = null; // the value last sent; null before boot reads it

  function plansValue() {
    return tabs.filter((t) => t.planOpen).map((t) => t.key).join(",");
  }

  // plansChanged is {plans} when the value differs from the one last
  // sent, else {} — for merging into another layout write.
  function plansChanged() {
    const v = plansValue();
    if (savedPlans === null || v === savedPlans) return {};
    savedPlans = v;
    return { plans: v };
  }

  function savePlans() {
    const v = plansChanged();
    if (v.plans !== undefined) saveLayout(v);
  }

  // onPlanPane: the active tab's results pane switched between the grid
  // and the plan (a click, p, an explain landing, a run's result).
  dbc.cmd.onPlanPane = (open) => {
    const t = state.tab;
    if (!t || t.planOpen === open) return;
    t.planOpen = open;
    savePlans();
  };

  dbc.editor.onChange(scheduleSave);
  window.addEventListener("pagehide", release);
  window.addEventListener("pageshow", (e) => { if (e.persisted) reclaim(); });

  // ── the query tab strip ────────────────────────────────────────────────
  //   [Query 1 ●][Query 2 •][Query 3 ×] [+]
  // ● running · • finished while in the background (red when it failed) ·
  // ◆ the session may hold state (a transaction, SET values, temp tables).
  // Click switches; double-click renames; × (or Alt+W) closes; + (Alt+T)
  // opens one; Alt+1…9 picks by position. The order and the active tab
  // are saved with the layout.
  //
  // Tab groups (tabgroups.js) put a chip in front of a group's tabs, which
  // sit together and are underlined in its colour; the chip folds them
  // (click) and opens the group's menu (right-click):
  //   [prod][Query 1 ×][Query 3 ×] [wip +2][Query 4 ×] [+]
  // The + is drawn in the colour of the group a new tab would join, and a
  // right-click on it opens the tab in any group instead.
  // renaming is the tab whose title is being edited. The strip is not
  // redrawn under it — a double-click on a background tab starts the rename
  // while that tab's activation is still loading, and its redraw would
  // throw the field away mid-word — but once the rename ends.
  let renaming = null;

  // groups are the strip's tab groups. The host is what the module needs
  // of the strip; changed() is its one way back — every group edit saves
  // the groups, and the redraw re-arranges (and saves the order if a tab
  // moved).
  const groups = dbc.groups.create({
    tabs: () => tabs,
    active: () => state.tab,
    under: connUnder,
    activate: (t) => activate(t),
    newTab: (g) => newTab(g),
    close: (t) => reallyClose(t),
    changed: () => { saveGroups(); renderTabs(); },
    log,
  });

  // saveGroups writes the groups whole, as the "groups" layout key (see
  // web/groups.go for its shape and the merge across windows).
  function saveGroups() {
    saveLayout({ groups: groups.encode() });
  }

  function renderTabs() {
    markInUse(); // the tab on screen, or a title, may have changed
    if (renaming) return;
    // Groups are contiguous because the array itself is reordered (see
    // tabgroups.js), here at every redraw — a tab that switched onto a
    // grouped connection, say, moves in beside its group. Only an actual
    // move is saved.
    const arranged = groups.arrange(tabs);
    if (arranged !== tabs) { tabs = arranged; saveOrder(); }
    const plus = $("qnew");
    els.qtabs.replaceChildren();
    tabs.forEach((t, i) => {
      const c = groups.chip(tabs, i);
      if (c) {
        els.qtabs.append(el("button", { type: "button", class: "qchip g" + groups.color(c.g) + (c.g.collapsed ? " folded" : ""),
          "data-group": c.g.name, "aria-expanded": c.g.collapsed ? "false" : "true",
          title: c.g.name + " — " + groups.describe(c.g) + " · click " + (c.g.collapsed ? "expands" : "collapses") +
            ", right-click for the group's menu" }, c.text));
      }
      if (groups.hidden(t)) return; // folded into its chip
      const g = groups.groupOf(t);
      const marks = el("span", "qmark");
      if (t.busy) marks.append(el("span", { class: "qbusy", title: "running" }, "●"));
      else if (t.done) marks.append(el("span", { class: t.failed ? "qfail" : "qdone", title: "finished in the background" }, "•"));
      if (t.stateful) marks.append(el("span", { class: "qstate", title: "its session may hold a transaction, SET values or temp tables" }, "◆"));
      if (t.lost) marks.append(el("span", { class: "qlost", title: "open in another browser tab of dbc web — not saved here" }, "⊘"));
      const b = el("div", { class: "qtab" + (t === state.tab ? " on" : "") + (t.lost ? " lost" : "") +
          (g ? " grp g" + groups.color(g) : ""), role: "tab", tabindex: "-1",
        "aria-selected": t === state.tab ? "true" : "false", "data-key": t.key,
        title: t.title + (i < 9 ? " (Alt+" + (i + 1) + ")" : "") +
          (t.console ? " — console " + t.cdb.label + " · " + t.console : "") + (g ? " — group " + g.name : "") +
          " — double-click renames" },
      el("span", "qt", t.title), t.console ? el("span", "qcon", t.console) : null, marks,
      tabs.length > 1 ? el("button", { type: "button", class: "qx", title: "Close (Alt+W)", "data-close": t.key }, "×") : null);
      els.qtabs.append(b);
    });
    els.qtabs.append(plus);
    markNew(plus);
  }

  // markNew tells, on the + itself, where Alt+T or a click puts the new
  // tab: the button takes the landing group's colour (and underline, as its
  // tabs have) and its tooltip names the group and the connection. With
  // several groups on the strip, a plain + read as "somewhere".
  function markNew(plus) {
    const g = groups.landing(state.tab, state.active);
    plus.className = "qnew" + (g ? " grp g" + groups.color(g) : "");
    plus.title = "New query tab" + (g ? " in group " + g.name : "") +
      " on " + (state.active || "the default connection") + " (Alt+T)" +
      (groups.count() ? " · right-click picks another group" : "");
  }

  // the chip: a click folds or unfolds its group; a right-click opens the
  // group's menu (the tab's own menu carries the same rows)
  const chipGroup = (e) => {
    const c = e.target.closest(".qchip");
    return c ? groups.byName(c.dataset.group) : null;
  };

  els.qtabs.addEventListener("click", (e) => {
    if (e.target.closest(".qchip")) {
      const g = chipGroup(e);
      if (g) groups.toggle(g);
      return;
    }
    const x = e.target.closest("[data-close]");
    if (x) { closeTab(tabs.find((t) => t.key === x.dataset.close)); return; }
    const b = e.target.closest(".qtab");
    if (b && !b.querySelector("input")) {
      const t = tabs.find((x) => x.key === b.dataset.key);
      if (t && t !== state.tab) activate(t);
    }
  });
  els.qtabs.addEventListener("dblclick", (e) => {
    const b = e.target.closest(".qtab");
    if (b && !e.target.closest("[data-close]")) rename(tabs.find((x) => x.key === b.dataset.key));
  });
  els.qtabs.addEventListener("auxclick", (e) => { // middle-click closes, as in a browser
    const b = e.target.closest(".qtab");
    if (b && e.button === 1) closeTab(tabs.find((x) => x.key === b.dataset.key));
  });
  els.qtabs.addEventListener("contextmenu", (e) => {
    if (e.target.closest("#qnew")) { e.preventDefault(); openNewMenu(e.clientX, e.clientY); return; }
    const g = chipGroup(e);
    if (g) { e.preventDefault(); groups.openGroupMenu(g, e.clientX, e.clientY); return; }
    const b = e.target.closest(".qtab");
    if (!b) return;
    e.preventDefault();
    const t = tabs.find((x) => x.key === b.dataset.key);
    const x = e.clientX, y = e.clientY;
    const items = () => [
      { head: t.title },
      { label: "Rename…", act: () => rename(t) },
      { label: "Close tab", key: "Alt+W", why: tabs.length > 1 ? "" : "the last tab stays — clear its editor instead",
        act: () => closeTab(t) },
      { head: "" },
      { label: "New query tab", key: "Alt+T", act: newTab },
    ].concat(groups.tabItems(t, x, y), consoleItems(t));
    // the database's consoles as they are now — another window or the
    // TUI may have added one — but the menu does not wait long for them
    if (!t.cdb) { dbc.menu.open(x, y, items()); return; }
    refreshNames(t.cdb).finally(() => dbc.menu.open(x, y, items()));
  });
  $("qnew").addEventListener("click", () => newTab());

  // openNewMenu is the + button's right-click menu: a new tab in any group,
  // listed as the strip shows them (left to right), the one a plain click
  // would pick marked ●; then the plain click itself, beside the tab on
  // screen, for when no group is wanted to steer it.
  function openNewMenu(x, y) {
    const land = groups.landing(state.tab, state.active);
    const items = [{ head: "new query tab in" }];
    for (const g of groups.ordered()) {
      items.push({ label: (g === land ? "● " : "") + g.name + "  · " + groups.describe(g), act: () => newTab(g) });
    }
    if (!groups.count()) items.push({ label: "no groups yet — right-click a tab → Add to group…", why: "nothing to pick" });
    items.push({ head: "" }, { label: "Beside " + state.tab.title + (land ? " (" + land.name + ")" : ""), key: "Alt+T", act: () => newTab() });
    dbc.menu.open(x, y, items);
  }

  // newKey mints a saved tab's key: it sorts after the saved ones. Unique
  // enough — one person, one click at a time.
  const newKey = () => Date.now().toString(36);

  // newTab opens a query tab on the active tab's connection — or, given a
  // group g (the group menu, the + button's right-click), in g, on g's
  // connection (tabgroups.js connFor). Its title is the lowest "Query N"
  // not in use; its first save claims it for this window.
  function newTab(g) {
    const used = new Set(tabs.map((t) => t.title));
    let n = 1;
    while (used.has("Query " + n)) n++;
    const t = { key: newKey(), title: "Query " + n, conn: g ? groups.connFor(g, state.active) : state.active, buffer: "", ws: "" };
    // Beside the tab on screen; into a group, after its last tab — where
    // the redraw's arrange would gather it anyway, and a connection group
    // with no tab here gets it beside the tab on screen.
    const ms = g ? tabs.filter((x) => groups.groupOf(x) === g) : [];
    tabs.splice(ms.length ? tabs.indexOf(ms[ms.length - 1]) + 1 : tabs.indexOf(state.tab) + 1, 0, t);
    if (g) {
      groups.place(t, g);
      saveGroups();
    } else if (groups.joinNew(state.tab, t)) {
      // opened from a tab in an ad-hoc group, it joins that group (a
      // connection group takes it by its connection anyway)
      saveGroups();
    }
    saveOrder();
    saveTab(t);
    activate(t);
  }

  // closeTab closes a query tab and releases its session. A session that
  // may hold state — an open transaction — is asked about first: closing
  // rolls it back, which is not a thing to do by a stray click.
  function closeTab(t) {
    if (!t) return;
    if (tabs.length === 1) { log("warn", "the last tab stays — clear its editor instead"); return; }
    if (!t.stateful) { reallyClose(t); return; }
    const yes = el("button", { type: "button", class: "primary" }, "Close and release");
    const no = el("button", { type: "button" }, "Keep it");
    yes.addEventListener("click", () => { dbc.modal.close(); reallyClose(t); });
    no.addEventListener("click", () => dbc.modal.close());
    dbc.modal.open({
      title: "Close " + t.title + "?", focus: no,
      body: el("div", "confirm", el("p", null, t.title + "'s session may hold a transaction, SET values or temp tables. " +
        "Closing the tab releases the session — an open transaction is rolled back.")),
      foot: el("div", "mfoot", yes, no),
    });
  }

  async function reallyClose(t) {
    const i = tabs.indexOf(t);
    if (i < 0) return;
    tabs.splice(i, 1);
    if (groups.forgetTab(t)) saveGroups();
    if (t === state.tab) activate(tabs[Math.min(i, tabs.length - 1)]);
    else renderTabs();
    saveOrder();
    sessionStorage.removeItem(wsKey(t.key));
    // a console another tab here still shows keeps its document
    const k = docOf(t);
    if (!tabs.some((o) => docOf(o) === k)) leaveDoc(k);
    if (t.ws) api("DELETE", "/api/v1/ws/" + t.ws).catch(() => { /* already gone */ });
    // a lost tab's saved copy is the other window's: closing it here
    // leaves that alone
    if (!t.lost) api("DELETE", "/api/v1/tabs/" + encodeURIComponent(t.key) + winQuery()).catch(() => {});
    log("info", "closed " + t.title + (t.ws ? " — its session was released" : ""));
  }

  function rename(t) {
    if (!t) return;
    const b = els.qtabs.querySelector('.qtab[data-key="' + t.key + '"]');
    if (!b) return;
    const input = el("input", { class: "qrename", value: t.title, maxlength: "40", "aria-label": "Tab name", spellcheck: "false" });
    const label = b.querySelector(".qt");
    label.replaceWith(input);
    input.select();
    renaming = t;
    let done = false;
    const finish = (keep) => {
      if (done) return;
      done = true;
      renaming = null;
      const v = input.value.trim();
      if (keep && v && v !== t.title) { t.title = v; saveTab(t); }
      renderTabs();
      if (t === state.tab) dbc.editor.focus();
    };
    input.addEventListener("keydown", (e) => {
      e.stopPropagation();
      if (e.key === "Enter") { e.preventDefault(); finish(true); }
      else if (e.key === "Escape") { e.preventDefault(); finish(false); }
    });
    input.addEventListener("blur", () => finish(true));
  }

  // saveOrder writes the strip's order — and the plans key with it, so a
  // closed tab leaves that too.
  function saveOrder() {
    saveLayout(Object.assign({ tabs: tabs.map((t) => t.key).join(",") }, plansChanged()));
  }

  function pickTab(n) {
    const t = tabs[n];
    if (t && t !== state.tab) activate(t);
  }

  // ── light and dark ─────────────────────────────────────────────────────
  // The palettes are /theme.css's (the theme package's); the choice is the
  // root's data-theme, rendered into the page by the server so a reload
  // does not flash the other one.
  function toggleTheme() {
    const t = document.documentElement.dataset.theme === "light" ? "dark" : "light";
    document.documentElement.dataset.theme = t;
    dbc.editor.retheme();
    dbc.cmd.planTheme(t);
    saveLayout({ theme: t });
  }
  els.theme.addEventListener("click", toggleTheme);

  // ── keyboard help ──────────────────────────────────────────────────────
  // F1, ?, or the ⌨ button. Grouped by where the keys work; the chords
  // bent to fit a browser (Ctrl+I, Alt+T, …) are the ones listed.
  const KEYS = [
    ["Editor", [
      ["Ctrl+Enter · Ctrl+R", "run the statement under the caret (or the selection)"],
      ["Ctrl+Shift+Enter · Ctrl+Shift+R", "run every statement"],
      ["Ctrl+X (nothing selected)", "explain the statement"],
      ["Ctrl+Shift+X · Alt+X", "explain analyze — runs it to time each step"],
      ["Ctrl+K", "stop the run or the connect"],
      ["Ctrl+P", "history of the tab's database (Tab: every database) — insert a past statement"],
      ["Ctrl+E", "export the result"],
      ["Ctrl+O", "scripts — run a Go script from scripts_dir"],
      ["Ctrl+I", "the assistant — and back"],
      ["Ctrl+B", "hide the sidebar — and back"],
      ["Ctrl+Space", "suggestions from the schema (also as you type, and after “.”)"],
      ["Tab · Enter · Esc", "in the suggestions: pick · pick · close"],
      ["F12 · Ctrl+click", "on a table alias or a CTE name: go to where it is declared"],
      ["Shift+F12", "list its uses in the statement"],
      ["F2", "rename it everywhere in the statement (also in the right-click menu)"],
    ]],
    ["Query tabs", [
      ["Alt+T", "new tab"], ["Alt+W", "close the tab"], ["Alt+1 … Alt+9", "go to tab N"],
      ["Alt+N · Alt+C", "new console · next console of the tab's database (right-click a tab for the list)"],
      ["double-click a tab", "rename it"],
      ["right-click a tab → Add to group…", "group tabs: by hand, or every tab on a connection"],
      ["right-click +", "new tab in a group of your choice (+ is coloured for the group Alt+T joins)"],
      ["click · right-click a group's chip", "collapse or expand it · its menu (rename, ungroup, its tabs)"],
    ]],
    ["Results grid", [
      ["arrows · Shift+arrows", "move · extend the range"], ["g · G", "first · last row"],
      ["Enter · double-click", "inspect the value"], ["y · Y", "copy the value or range · the row"],
      ["- · + · =", "hide the column · show all · fit it"], ["click a header", "sort: asc, desc, off"],
      ["t · ⇄ Transpose", "turn the grid on its side — each row a column; copies and exports follow"],
      ["p", "the plan, when there is one"],
    ]],
    ["Plan", [
      ["←↑↓→ · Enter", "walk the steps · fold"], ["1–4", "the metric"], ["f · g", "fit · graph"],
      ["e · a", "explain again · analyze"], ["y · Y", "copy as text · the engine's output"],
      ["b", "open as a page"], ["p", "back to the results"],
    ]],
    ["Assistant", [
      ["Enter · Shift+Enter", "send · new line"], ["Ctrl+K", "stop the answer"], ["Esc", "back to the editor"],
    ]],
    ["Sidebar", [
      ["click a connection", "switch this tab to it"], ["+ beside Connections", "add a connection"],
      ["right-click a connection", "connect · remove one added here"],
      ["‹ beside Connections · Ctrl+B", "hide it; the › tab on the left edge brings it back"],
      ["drag its right edge", "resize it (double-click the edge: the default width)"],
      ["drag the bar above Tables", "share the column between the lists"],
      ["on a table: Enter · c · e", "preview its rows · show its columns · diagram it and its neighbours (ERD)"],
      ["ERD beside Tables", "diagram every table and key: PNG, JPEG or Mermaid"],
    ]],
    ["Splitters", [
      ["drag a bar", "resize the panes either side — the size is saved"],
      ["double-click a bar", "back to the default size"],
    ]],
    ["Anywhere", [["F1 · ?", "this list"]]],
  ];

  function help() {
    const body = el("div", "keyhelp");
    for (const [group, rows] of KEYS) {
      const dl = el("dl");
      for (const [k, what] of rows) dl.append(el("dt", null, k), el("dd", null, what));
      body.append(el("section", null, el("h3", null, group), dl));
    }
    dbc.modal.open({ title: "Keys", cls: "wide", body,
      foot: el("div", "mfoot", el("span", "hint", "On a Mac, ⌘ works wherever Ctrl is listed.")) });
  }
  els.help.addEventListener("click", help);

  // ── boot ───────────────────────────────────────────────────────────────
  async function boot() {
    try {
      const layout = await api("GET", "/api/v1/layout");
      // the log first: the editor's ceiling is measured against it
      if (Number(layout.logHeight) > 0) setLogHeight(Number(layout.logHeight));
      if (layout.editorHeight) setEditorHeight(Number(layout.editorHeight));
      if (Number(layout.sideWidth) > 0) setSideWidth(Number(layout.sideWidth));
      // skipped while folded: the column has no height to measure against
      // (setConnsHeight clamps to it), and the stylesheet's cap serves until
      // the next drag
      if (Number(layout.connsHeight) > 0) {
        if (sideHidden()) pendingConnsH = Number(layout.connsHeight);
        else setConnsHeight(Number(layout.connsHeight));
      }
      dbc.chat.boot(layout);
      // the Tables list's schema picks, before the first list is drawn
      for (const [k, v] of Object.entries(layout)) {
        if (k.startsWith(PICK_KEY) && v) schemaPicks[k.slice(PICK_KEY.length)] = v;
      }

      // the window: this browser tab's, if the server still has it — and
      // if it is not being shown by another browser tab right now, which
      // is what a duplicated tab (sessionStorage copied) looks like. The
      // boot claim tells: 409 for a copy, which then opens its own window.
      // The window comes first because the claim is made in its name.
      let win = sessionStorage.getItem(WIN_KEY) || "", live = [], first = "", claim = null;
      if (win) {
        try { live = (await api("GET", "/api/v1/win/" + win)).tabs; } catch (_) { win = ""; }
      }
      if (win) {
        try {
          claim = await api("POST", "/api/v1/win/" + win + "/tabs", { boot: true });
        } catch (e) {
          if (e.status !== 409) throw e;
          log("info", "this browser tab is a copy of another one of dbc web — it gets a window (and sessions) of its own");
          win = "";
          live = [];
        }
      }
      if (!win) {
        forgetSession();
        const st = await api("POST", "/api/v1/ws", {});
        for (const w of st.warnings || []) log("warn", w);
        win = st.win;
        first = st.id;
        claim = await api("POST", "/api/v1/win/" + win + "/tabs", { boot: true });
      }
      state.win = win;
      sessionStorage.setItem(WIN_KEY, win);

      // the claimed tabs, in the saved order; any the order does not name
      // (saved by an older page) after them. The order may name tabs
      // another window holds; they are skipped.
      const saved = claim.tabs;
      const byKey = new Map(saved.map((t) => [t.id, t]));
      const order = (layout.tabs || "").split(",").filter((k) => byKey.has(k));
      for (const t of saved) if (!order.includes(t.id)) order.push(t.id);
      const plans = new Set((layout.plans || "").split(","));
      tabs = order.map((k) => {
        const t = byKey.get(k);
        return { key: t.id, title: t.title || "Query", conn: t.conn, buffer: t.buffer, ws: "", planOpen: plans.has(t.id),
          cdb: t.console && t.consoleDb ? t.consoleDb : null, console: t.console && t.consoleDb ? t.console : "" };
      });
      // each tab's console, read before any is shown; one that cannot be
      // read leaves its tab on the buffer saved with it, and the tab's
      // connection gives it a console once it lands
      await Promise.all(tabs.filter((t) => t.console).map((t) => loadConsole(t.cdb, t.console)
        .then((c) => { t.buffer = c.text; }, () => { t.console = ""; t.cdb = null; })));
      // no tab to show: the first boot, or every saved tab is open in
      // another browser tab of dbc web — this one starts a fresh tab of
      // its own. Its key is new, never "1": a key another window holds
      // would be refused on the first save.
      if (!tabs.length) {
        tabs = [{ key: newKey(), title: "Query 1", conn: "", buffer: "", ws: "", planOpen: false }];
      }
      if (claim.held.length) {
        log("info", dbc.plural(claim.held.length, "saved query tab") + " open in another browser tab of dbc web " +
          (claim.held.length === 1 ? "stays" : "stay") + " there — this one shows " +
          (saved.length ? "the rest" : "a fresh tab"));
      }
      savedPlans = layout.plans || "";
      // the groups, against the tabs this window claimed; the first
      // renderTabs gathers each group's tabs together
      groups.restore(layout.groups);
      activeAtBoot = tabs.find((t) => t.key === layout.tab) || tabs[0];

      for (const t of tabs) {
        const ws = sessionStorage.getItem(wsKey(t.key));
        if (ws && live.includes(ws)) t.ws = ws;
      }
      if (first) {
        activeAtBoot.ws = first;
        sessionStorage.setItem(wsKey(activeAtBoot.key), first);
      }
      state.tab = null;
      renderTabs();
      attach(); // its first open activates activeAtBoot
    } catch (e) {
      setStatus(e.message, "err");
      log("err", "could not start: " + e.message);
    }
  }

  boot();
})();
