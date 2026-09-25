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
  //   {key, title, conn, buffer, ws, busy, done, failed, stateful, status, level, grid, planOpen, lost}
  // key is the saved tab's id (web.bytdb); ws its workspace, "" until
  // first shown. buffer is kept only while the tab is in the background.
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
    active: $("active-conn"), stateful: $("stateful"), busy: $("busy"),
    run: $("run"), runAll: $("run-all"), stop: $("stop"), history: $("history-btn"), scripts: $("scripts-btn"),
    qtabs: $("qtabs"), theme: $("theme-btn"), help: $("help-btn"),
    splitter: $("splitter"), work: document.querySelector(".work"),
  };

  function setBusy(busy) {
    state.busy = busy;
    els.busy.hidden = !busy;
    els.stop.disabled = !busy && !document.querySelector(".conn-item.connecting");
  }

  // ── the sidebar ────────────────────────────────────────────────────────
  function markActive(name, connecting) {
    for (const b of els.conns.querySelectorAll(".conn-item")) {
      b.classList.toggle("active", b.dataset.conn === name && !connecting);
      b.classList.toggle("connecting", b.dataset.conn === connecting);
      if (b.dataset.conn === name) {
        state.driver = b.dataset.driver || "";
        dbc.editor.setDriver(state.driver);
      }
    }
    if (!connecting) els.active.textContent = name;
    els.stop.disabled = !state.busy && !connecting;
  }

  // The tables list: a click selects, a double-click (or Enter) previews
  // the first 100 rows — the TUI's gestures — and the right-click menu
  // inserts or copies the name.
  function showTables(tables) {
    els.tables.replaceChildren();
    els.tableCount.textContent = tables.length ? "· " + tables.length : "";
    if (!tables.length) {
      els.tables.append(el("li", "none", "no tables"));
      return;
    }
    for (const t of tables) {
      const li = el("li", { class: t.view ? "view" : "", tabindex: "-1", "data-name": t.qname,
        title: t.qname + " — double-click previews its rows, right-click for more" });
      const dot = t.qname.lastIndexOf(".");
      if (dot > 0) li.append(el("span", "schema", t.qname.slice(0, dot + 1)), t.qname.slice(dot + 1));
      else li.append(t.qname);
      els.tables.append(li);
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
    if (e.key === "ArrowDown") next = li.nextElementSibling;
    else if (e.key === "ArrowUp") next = li.previousElementSibling;
    if (next) { e.preventDefault(); pickTable(next); }
  });
  els.tables.addEventListener("contextmenu", (e) => {
    const li = e.target.closest("li[data-name]");
    if (!li) return;
    e.preventDefault();
    pickTable(li);
    const name = li.dataset.name;
    dbc.menu.open(e.clientX, e.clientY, [
      { head: name },
      { label: "Preview rows", key: "Enter", act: () => preview(name) },
      { label: "Insert the name at the caret", act: () => dbc.editor.insert(name) },
      { label: "Copy name", act: () => dbc.clip.copyText(name, "the table name") },
    ]);
  });

  // ── the event stream ───────────────────────────────────────────────────
  function onEvent(ev) {
    const d = ev.data;
    if (!ev.ws) {
      if (ev.type.startsWith("chat.")) dbc.chat.onEvent(ev.type, d);
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
        showTables(d.tables || []);
        if (d.status) setStatus(d.status);
        else if (!state.busy) setStatus("ready on " + d.active);
        if (d.changed) saveTab(t);
        dbc.chat.refresh(); // another catalog: other tables' schema
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
      prev.grid = dbc.grid.snapshot();
      prev.planOpen = dbc.cmd.planOpen();
      saveTab(prev);
    }
    state.tab = t;
    state.ws = t.ws;
    t.done = false;
    dbc.editor.useDoc(t.key, t.buffer || "");
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
    if (st.connected || st.connecting) {
      applyState(st, true);
      return;
    }
    dbc.chat.onState(st);
    const known = (name) => [...els.conns.querySelectorAll(".conn-item")].some((b) => b.dataset.conn === name);
    connect(t.conn && known(t.conn) ? t.conn : state.active && known(state.active) ? state.active : st.active);
  }

  async function resync() {
    reclaim(); // a long drop may have let another browser tab take ours
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
    markActive(st.active, st.connecting || "");
    showTables(st.tables || []);
    setBusy(st.busy);
    t.busy = st.busy;
    if (!st.busy) { els.stateful.hidden = !st.stateful; t.stateful = st.stateful; }
    if (st.busy) setStatus(st.status, "warn");
    else if (fresh && t.status) setStatus(t.status, t.level);
    else setStatus("ready on " + st.active, "");
    if (st.hasResult) {
      if (fresh) dbc.grid.restore(t.grid); else dbc.grid.load();
    }
    if (dbc.cmd.onState) dbc.cmd.onState(Object.assign({}, st, { openPlan: fresh && t.planOpen }));
    dbc.chat.onState(st);
    renderTabs();
  }

  // ── commands ───────────────────────────────────────────────────────────
  async function connect(name) {
    try {
      await api("POST", dbc.wsPath("/connect"), { name });
    } catch (e) {
      log("err", e.message);
    }
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
  function history() {
    const input = el("input", { type: "search", class: "hfilter", placeholder: "filter by SQL or connection…",
      "aria-label": "Filter history", spellcheck: "false", autocomplete: "off" });
    const list = el("ul", { class: "hlist", role: "listbox" });
    const count = el("span", "hint");
    let entries = [], cur = 0, timer = 0, seq = 0;

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
      count.textContent = entries.length ? dbc.plural(entries.length, "statement") + " · ↑↓ pick · Enter inserts · Esc closes"
        : "no statements match";
      const c = list.children[cur];
      if (c) c.scrollIntoView({ block: "nearest" });
    }
    async function fetchList() {
      const n = ++seq;
      try {
        const got = await api("GET", "/api/v1/history?q=" + encodeURIComponent(input.value));
        if (n !== seq) return;
        entries = got; cur = 0; draw();
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
      body: el("div", "history", input, list), foot: el("div", "mfoot", count),
      onKey: (e) => {
        if (e.key === "ArrowDown") { cur = Math.min(cur + 1, entries.length - 1); draw(); return true; }
        if (e.key === "ArrowUp") { cur = Math.max(cur - 1, 0); draw(); return true; }
        if (e.key === "Enter") { pick(cur); return true; }
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

  Object.assign(dbc.cmd, {
    run, stop, history, preview, editorState, scripts, help, newTab, pickTab,
    closeTab: () => closeTab(state.tab),
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

  function setEditorHeight(px) {
    const max = els.work.getBoundingClientRect().height - 120;
    dbc.editor.setHeight(Math.max(60, Math.min(px, max)));
  }

  // ── saving tabs: every tab's buffer, title and connection survive a restart
  let saveTimer = 0;

  function scheduleSave() {
    clearTimeout(saveTimer);
    saveTimer = setTimeout(() => saveTab(state.tab), 600);
  }

  // tabBody is what a save sends for t: the editor's text when it is on
  // screen, else the buffer kept for it.
  function tabBody(t) {
    const active = t === state.tab;
    return { title: t.title, conn: (active ? state.active : t.conn) || "",
      buffer: active ? dbc.editor.text() : t.buffer || "" };
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
    fetch("/api/v1/win/" + encodeURIComponent(state.win) + "/release", {
      method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ save }), keepalive: true,
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
  // renaming is the tab whose title is being edited. The strip is not
  // redrawn under it — a double-click on a background tab starts the rename
  // while that tab's activation is still loading, and its redraw would
  // throw the field away mid-word — but once the rename ends.
  let renaming = null;

  function renderTabs() {
    if (renaming) return;
    const plus = $("qnew");
    els.qtabs.replaceChildren();
    tabs.forEach((t, i) => {
      const marks = el("span", "qmark");
      if (t.busy) marks.append(el("span", { class: "qbusy", title: "running" }, "●"));
      else if (t.done) marks.append(el("span", { class: t.failed ? "qfail" : "qdone", title: "finished in the background" }, "•"));
      if (t.stateful) marks.append(el("span", { class: "qstate", title: "its session may hold a transaction, SET values or temp tables" }, "◆"));
      if (t.lost) marks.append(el("span", { class: "qlost", title: "open in another browser tab of dbc web — not saved here" }, "⊘"));
      const b = el("div", { class: "qtab" + (t === state.tab ? " on" : "") + (t.lost ? " lost" : ""), role: "tab", tabindex: "-1",
        "aria-selected": t === state.tab ? "true" : "false", "data-key": t.key,
        title: t.title + (i < 9 ? " (Alt+" + (i + 1) + ")" : "") + " — double-click renames" },
      el("span", "qt", t.title), marks,
      tabs.length > 1 ? el("button", { type: "button", class: "qx", title: "Close (Alt+W)", "data-close": t.key }, "×") : null);
      els.qtabs.append(b);
    });
    els.qtabs.append(plus);
  }

  els.qtabs.addEventListener("click", (e) => {
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
    const b = e.target.closest(".qtab");
    if (!b) return;
    e.preventDefault();
    const t = tabs.find((x) => x.key === b.dataset.key);
    dbc.menu.open(e.clientX, e.clientY, [
      { head: t.title },
      { label: "Rename…", act: () => rename(t) },
      { label: "Close tab", key: "Alt+W", why: tabs.length > 1 ? "" : "the last tab stays — clear its editor instead",
        act: () => closeTab(t) },
      { head: "" },
      { label: "New query tab", key: "Alt+T", act: newTab },
    ]);
  });
  $("qnew").addEventListener("click", newTab);

  // newKey mints a saved tab's key: it sorts after the saved ones. Unique
  // enough — one person, one click at a time.
  const newKey = () => Date.now().toString(36);

  // newTab opens a query tab on the active tab's connection. Its title is
  // the lowest "Query N" not in use; its first save claims it for this
  // window.
  function newTab() {
    const used = new Set(tabs.map((t) => t.title));
    let n = 1;
    while (used.has("Query " + n)) n++;
    const t = { key: newKey(), title: "Query " + n, conn: state.active, buffer: "", ws: "" };
    tabs.splice(tabs.indexOf(state.tab) + 1, 0, t);
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
    if (t === state.tab) activate(tabs[Math.min(i, tabs.length - 1)]);
    else renderTabs();
    saveOrder();
    sessionStorage.removeItem(wsKey(t.key));
    dbc.editor.dropDoc(t.key);
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
      ["Ctrl+P", "history — insert a past statement"],
      ["Ctrl+E", "export the result"],
      ["Ctrl+O", "scripts — run a Go script from scripts_dir"],
      ["Ctrl+I", "the assistant — and back"],
    ]],
    ["Query tabs", [
      ["Alt+T", "new tab"], ["Alt+W", "close the tab"], ["Alt+1 … Alt+9", "go to tab N"],
      ["double-click a tab", "rename it"],
    ]],
    ["Results grid", [
      ["arrows · Shift+arrows", "move · extend the range"], ["g · G", "first · last row"],
      ["Enter · double-click", "inspect the value"], ["y · Y", "copy the value or range · the row"],
      ["- · + · =", "hide the column · show all · fit it"], ["click a header", "sort: asc, desc, off"],
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
      if (layout.editorHeight) setEditorHeight(Number(layout.editorHeight));
      dbc.chat.boot(layout);

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
        return { key: t.id, title: t.title || "Query", conn: t.conn, buffer: t.buffer, ws: "", planOpen: plans.has(t.id) };
      });
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
