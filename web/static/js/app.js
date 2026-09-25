// dbc web — the workbench page's script.
//
// The server owns the rules and the rendering; this owns only what must be
// live: the editor, the log, and the event stream. The flow:
//
//   boot ─► workspace: reattach (id kept in sessionStorage, so a reload
//           keeps the same pinned session) or open a new one
//        ─► EventSource on /api/v1/ws/<id>/events
//        ─► on the first open: connect to the tab's saved connection
//
//   Run ─► POST …/run {buffer, caret, selection, all}
//          409/400 → refused; the server has already logged why
//          200     → notes, ticks and the outcome arrive as events:
//                    "busy" … "tick"* … "run" {hasResult} ─► GET …/result
//
// Every event on the stream is {type, data} (rweb's SSE hub wraps them so),
// handled by one switch in onEvent.
(function () {
  "use strict";

  const TAB_ID = "1"; // Phase 2 has one query tab; tabs are Phase 6
  const WS_KEY = "dbc.ws";

  const $ = (id) => document.getElementById(id);
  const els = {
    editor: $("editor"), results: $("results"), log: $("log"), status: $("status"),
    conns: $("conns"), tables: $("tables"), tableCount: $("table-count"),
    active: $("active-conn"), stateful: $("stateful"), busy: $("busy"),
    run: $("run"), runAll: $("run-all"), stop: $("stop"), splitter: $("splitter"),
  };

  const state = {
    ws: "",          // the workspace id
    active: "",      // the active connection
    busy: false,
    source: null,    // the EventSource
    attached: false, // the stream has opened at least once
  };

  // ── the API ────────────────────────────────────────────────────────────
  // Every response is the {success, data, error} envelope. A refusal or a
  // failure throws an Error carrying the status and the server's words.
  async function api(method, path, body) {
    const opts = { method, headers: {} };
    if (body !== undefined) {
      opts.headers["Content-Type"] = "application/json";
      opts.body = JSON.stringify(body);
    }
    const res = await fetch(path, opts);
    let env = {};
    try { env = await res.json(); } catch (_) { /* not JSON: a 403 text page */ }
    if (!res.ok || !env.success) {
      const err = new Error(env.error || res.status + " " + res.statusText);
      err.status = res.status;
      throw err;
    }
    return env.data;
  }

  // ── the log and the status bar ─────────────────────────────────────────
  function log(level, text) {
    const line = document.createElement("div");
    const t = document.createElement("span");
    t.className = "t";
    t.textContent = new Date().toTimeString().slice(0, 8);
    const msg = document.createElement("span");
    msg.className = level || "info";
    msg.textContent = text;
    line.append(t, msg);
    els.log.append(line);
    // keep the log bounded; a long session would otherwise grow the DOM forever
    while (els.log.childElementCount > 500) els.log.firstElementChild.remove();
    els.log.scrollTop = els.log.scrollHeight;
  }

  function setStatus(text, level) {
    els.status.textContent = text;
    els.status.className = level || "";
  }

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
    }
    if (!connecting) els.active.textContent = name;
    els.stop.disabled = !state.busy && !connecting;
  }

  function showTables(tables) {
    els.tables.replaceChildren();
    els.tableCount.textContent = tables.length ? "· " + tables.length : "";
    if (!tables.length) {
      const li = document.createElement("li");
      li.className = "none";
      li.textContent = "no tables";
      els.tables.append(li);
      return;
    }
    for (const t of tables) {
      const li = document.createElement("li");
      if (t.view) li.className = "view";
      li.title = "Insert " + qualified(t) + " at the caret";
      li.dataset.name = qualified(t);
      if (t.schema && t.schema !== "public" && t.schema !== "main") {
        const s = document.createElement("span");
        s.className = "schema";
        s.textContent = t.schema + ".";
        li.append(s);
      }
      li.append(t.name);
      els.tables.append(li);
    }
  }

  function qualified(t) {
    return t.schema && t.schema !== "public" && t.schema !== "main" ? t.schema + "." + t.name : t.name;
  }

  // ── results ────────────────────────────────────────────────────────────
  // The table arrives as HTML the server rendered with element, which
  // escaped every cell, so it goes in as markup.
  async function loadResult() {
    try {
      const r = await api("GET", "/api/v1/ws/" + state.ws + "/result");
      if (r.html) els.results.innerHTML = r.html;
    } catch (e) {
      log("err", "could not load the result: " + e.message);
    }
  }

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
        if (d.hasResult) loadResult();
        break;
      case "result": // a script's s.Show, mid-run
        loadResult();
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
    const src = new EventSource("/api/v1/ws/" + state.ws + "/events");
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
        await api("GET", "/api/v1/ws/" + state.ws);
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
    const st = await api("GET", "/api/v1/ws/" + state.ws);
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
      applyState(await api("GET", "/api/v1/ws/" + state.ws));
    } catch (_) { /* onerror handles a lost workspace */ }
  }

  function applyState(st) {
    state.active = st.active;
    markActive(st.active, st.connecting || "");
    showTables(st.tables || []);
    setBusy(st.busy);
    if (!st.busy) els.stateful.hidden = !st.stateful;
    setStatus(st.busy ? st.status : "ready on " + st.active, st.busy ? "warn" : "");
    if (st.hasResult) loadResult();
  }

  // ── commands ───────────────────────────────────────────────────────────
  async function connect(name) {
    try {
      await api("POST", "/api/v1/ws/" + state.ws + "/connect", { name });
    } catch (e) {
      log("err", e.message);
    }
  }

  async function run(all) {
    const ed = els.editor;
    const sel = ed.value.slice(ed.selectionStart, ed.selectionEnd);
    try {
      await api("POST", "/api/v1/ws/" + state.ws + "/run", {
        buffer: ed.value, caret: ed.selectionStart, selection: sel, all,
      });
    } catch (e) {
      // the server logged the refusal's words already (busy, nothing to run)
      setStatus(e.message, e.status === 409 ? "warn" : "err");
    }
  }

  async function stop() {
    try {
      await api("POST", "/api/v1/ws/" + state.ws + "/cancel");
    } catch (e) {
      log("err", e.message);
    }
  }

  // ── keys ───────────────────────────────────────────────────────────────
  // The TUI's chords where the browser lets a page take them (Ctrl+R would
  // otherwise reload), and the web's usual Ctrl+Enter beside them. On a Mac
  // Cmd works as Ctrl.
  document.addEventListener("keydown", (e) => {
    const mod = e.ctrlKey || e.metaKey;
    if (!mod) return;
    const k = e.key.toLowerCase();
    if (k === "enter" || k === "r") {
      e.preventDefault();
      run(e.shiftKey);
    } else if (k === "k") {
      e.preventDefault();
      stop();
    }
  });

  // Tab indents in the editor instead of leaving it.
  els.editor.addEventListener("keydown", (e) => {
    if (e.key !== "Tab" || e.ctrlKey || e.metaKey || e.altKey) return;
    e.preventDefault();
    els.editor.setRangeText("    ", els.editor.selectionStart, els.editor.selectionEnd, "end");
    scheduleSave();
  });

  els.run.addEventListener("click", () => run(false));
  els.runAll.addEventListener("click", () => run(true));
  els.stop.addEventListener("click", stop);

  els.conns.addEventListener("click", (e) => {
    const b = e.target.closest(".conn-item");
    if (b) connect(b.dataset.conn);
  });

  els.tables.addEventListener("click", (e) => {
    const li = e.target.closest("li[data-name]");
    if (!li) return;
    els.editor.focus();
    els.editor.setRangeText(li.dataset.name, els.editor.selectionStart, els.editor.selectionEnd, "end");
    scheduleSave();
  });

  // ── the splitter: drag to size the editor; the height is remembered ────
  els.splitter.addEventListener("pointerdown", (e) => {
    e.preventDefault();
    const startY = e.clientY;
    const startH = els.editor.getBoundingClientRect().height;
    els.splitter.setPointerCapture(e.pointerId);
    els.splitter.classList.add("dragging");
    const move = (m) => setEditorHeight(startH + m.clientY - startY);
    const up = () => {
      els.splitter.removeEventListener("pointermove", move);
      els.splitter.removeEventListener("pointerup", up);
      els.splitter.classList.remove("dragging");
      api("PUT", "/api/v1/layout", { editorHeight: String(Math.round(els.editor.getBoundingClientRect().height)) })
        .catch((err) => log("warn", "layout not saved: " + err.message));
    };
    els.splitter.addEventListener("pointermove", move);
    els.splitter.addEventListener("pointerup", up);
  });

  function setEditorHeight(px) {
    const max = els.editor.parentElement.getBoundingClientRect().height - 120;
    els.editor.style.height = Math.max(60, Math.min(px, max)) + "px";
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
    const body = JSON.stringify({ title: "Query 1", conn: state.active, buffer: els.editor.value });
    // keepalive lets the last save outlive the page when it is closing
    return fetch("/api/v1/tabs/" + TAB_ID, {
      method: "PUT", headers: { "Content-Type": "application/json" }, body, keepalive: !!keepalive,
    }).catch(() => { /* best effort: the next edit saves again */ });
  }

  els.editor.addEventListener("input", scheduleSave);
  window.addEventListener("pagehide", () => saveTab(true));

  // ── boot ───────────────────────────────────────────────────────────────
  async function boot() {
    try {
      const [tabs, layout] = await Promise.all([api("GET", "/api/v1/tabs"), api("GET", "/api/v1/layout")]);
      const t = tabs.find((x) => x.id === TAB_ID);
      if (t) {
        savedTab = t;
        els.editor.value = t.buffer;
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
      els.editor.focus();
    } catch (e) {
      setStatus(e.message, "err");
      log("err", "could not start: " + e.message);
    }
  }

  boot();
})();
