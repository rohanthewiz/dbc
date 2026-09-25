// dbc web — boot, the event stream, the commands and the keys. Loaded last:
// core.js, ui.js, editor.js and grid.js have set up their pieces of
// window.dbc by now.
//
// The server owns the rules and the rendering; the page owns only what must
// be live. The flow:
//
//   boot ─► workspace: reattach (id kept in sessionStorage, so a reload
//           keeps the same pinned session) or open a new one
//        ─► EventSource on /api/v1/ws/<id>/events
//        ─► on the first open: connect to the tab's saved connection
//
//   Run ─► POST …/run {buffer, caret, selection, all}
//          409/400 → refused; the server has already logged why
//          200     → notes, ticks and the outcome arrive as events:
//                    "busy" … "tick"* … "run" {hasResult} ─► grid.load()
//
// Every event on the stream is {type, data} (rweb's SSE hub wraps them so),
// handled by one switch in onEvent.
(function () {
  "use strict";

  const dbc = window.dbc;
  const { api, log, setStatus, state, el } = dbc;

  const TAB_ID = "1"; // one query tab until Phase 6's tabs
  const WS_KEY = "dbc.ws";

  const $ = (id) => document.getElementById(id);
  const els = {
    conns: $("conns"), tables: $("tables"), tableCount: $("table-count"),
    active: $("active-conn"), stateful: $("stateful"), busy: $("busy"),
    run: $("run"), runAll: $("run-all"), stop: $("stop"), history: $("history-btn"),
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
        break;
      case "result": // a script's s.Show, mid-run
        dbc.grid.load();
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
        markActive(d.active, "");
        showTables(d.tables || []);
        if (d.status) setStatus(d.status);
        else if (!state.busy) setStatus("ready on " + d.active);
        if (d.changed) saveTab();
        break;
    }
  }

  function attach() {
    const src = new EventSource(dbc.wsPath("/events"));
    state.source = src;
    src.onmessage = (e) => {
      try { onEvent(JSON.parse(e.data)); } catch (err) { console.error("bad event", e.data, err); }
    };
    src.onopen = () => {
      if (!state.attached) {
        state.attached = true;
        firstAttach();
      } else {
        resync(); // back after a drop: catch up on anything missed
      }
    };
    src.onerror = async () => {
      setStatus("disconnected — reconnecting…", "warn");
      // EventSource retries by itself, but not past a 404 or 401: find out
      // which it is. A workspace the server no longer has (a restart)
      // means starting over; a lost session means signing in again.
      try {
        await api("GET", dbc.wsPath(""));
      } catch (e) {
        if (e.status === 404) {
          src.close();
          sessionStorage.removeItem(WS_KEY);
          location.reload();
        } else if (e.status === 401) {
          src.close();
          setStatus("signed out — open the link dbc web printed in the terminal", "err");
        }
      }
    };
  }

  // firstAttach runs once the stream is open, so the connect's events have
  // somewhere to go. A reattached workspace already has its connection.
  async function firstAttach() {
    const st = await api("GET", dbc.wsPath(""));
    if (st.connected || st.connecting) {
      applyState(st);
      return;
    }
    const want = savedTab.conn && [...els.conns.querySelectorAll(".conn-item")]
      .some((b) => b.dataset.conn === savedTab.conn) ? savedTab.conn : st.active;
    connect(want);
  }

  async function resync() {
    try {
      applyState(await api("GET", dbc.wsPath("")));
    } catch (_) { /* onerror handles a lost workspace */ }
  }

  function applyState(st) {
    state.active = st.active;
    markActive(st.active, st.connecting || "");
    showTables(st.tables || []);
    setBusy(st.busy);
    if (!st.busy) els.stateful.hidden = !st.stateful;
    setStatus(st.busy ? st.status : "ready on " + st.active, st.busy ? "warn" : "");
    if (st.hasResult) dbc.grid.load();
    if (dbc.cmd.onState) dbc.cmd.onState(st);
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

  Object.assign(dbc.cmd, {
    run, stop, history, preview, editorState,
    exportMenu: () => dbc.grid.exportMenu(),
  });

  // ── keys ───────────────────────────────────────────────────────────────
  // The TUI's chords where the browser lets a page take them (Ctrl+R would
  // otherwise reload, Ctrl+P print), and the web's usual Ctrl+Enter beside
  // them. On a Mac Cmd works as Ctrl. Monaco binds the same chords itself
  // (editor.js) and marks the event handled, so they do not fire twice.
  document.addEventListener("keydown", (e) => {
    if (e.defaultPrevented || dbc.modal.isOpen() || dbc.menu.isOpen()) return;
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
    }
  });

  els.run.addEventListener("click", () => run(false));
  els.runAll.addEventListener("click", () => run(true));
  els.stop.addEventListener("click", stop);
  els.history.addEventListener("click", history);

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

  // ── saving the tab: the buffer and connection survive a restart ────────
  let savedTab = { conn: "" };
  let saveTimer = 0;

  function scheduleSave() {
    clearTimeout(saveTimer);
    saveTimer = setTimeout(saveTab, 600);
  }

  function saveTab(keepalive) {
    clearTimeout(saveTimer);
    const body = JSON.stringify({ title: "Query 1", conn: state.active, buffer: dbc.editor.text() });
    // keepalive lets the last save outlive the page when it is closing
    return fetch("/api/v1/tabs/" + TAB_ID, {
      method: "PUT", headers: { "Content-Type": "application/json" }, body, keepalive: !!keepalive,
    }).catch(() => { /* best effort: the next edit saves again */ });
  }

  dbc.editor.onChange(scheduleSave);
  window.addEventListener("pagehide", () => saveTab(true));

  // ── boot ───────────────────────────────────────────────────────────────
  async function boot() {
    try {
      const [tabs, layout] = await Promise.all([api("GET", "/api/v1/tabs"), api("GET", "/api/v1/layout")]);
      const t = tabs.find((x) => x.id === TAB_ID);
      if (t) {
        savedTab = t;
        dbc.editor.setText(t.buffer);
      }
      if (layout.editorHeight) setEditorHeight(Number(layout.editorHeight));

      let st = null;
      const kept = sessionStorage.getItem(WS_KEY);
      if (kept) {
        try { st = await api("GET", "/api/v1/ws/" + kept); } catch (_) { st = null; }
      }
      if (!st) {
        st = await api("POST", "/api/v1/ws");
        for (const w of st.warnings || []) log("warn", w);
      }
      state.ws = st.id;
      sessionStorage.setItem(WS_KEY, st.id);
      attach();
      dbc.editor.focus();
    } catch (e) {
      setStatus(e.message, "err");
      log("err", "could not start: " + e.message);
    }
  }

  boot();
})();
