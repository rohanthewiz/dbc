// dbc web — the shared core every other module builds on: the API call, the
// log, the status bar, and the page's state. Loaded first; each module after
// it hangs its piece on window.dbc:
//
//   core.js    dbc.api, dbc.putLayout, dbc.log (one per connection), dbc.setStatus, dbc.state, dbc.cmd
//   ui.js      dbc.menu, dbc.modal, dbc.clip        (menus, dialogs, clipboard)
//   editor.js  dbc.editor                           (textarea → Monaco)
//   grid.js    dbc.grid                             (the virtualized results grid)
//   plan.js    DbcPlan                              (the plan view, shared with the standalone page)
//   hl.js      dbc.hl                               (code-block syntax highlighting)
//   chat.js    dbc.chat                             (the assistant pane)
//   tabgroups.js dbc.groups                         (query-tab groups: the model and its menus)
//   app.js     boot, query tabs, the event stream, commands, keys
//
// dbc.cmd is the command table (run, stop, explain, history, …). app.js
// fills it; the editor's key bindings and the menus call through it, so a
// module never needs to know which other module implements a command.
(function () {
  "use strict";

  const dbc = (window.dbc = window.dbc || {});
  const $ = (id) => document.getElementById(id);

  dbc.state = {
    win: "",         // the window (this browser tab): its stream and assistant
    tab: null,       // the active query tab (app.js); its workspace is ws
    ws: "",          // the active query tab's workspace id
    active: "",      // the active connection
    driver: "",      // its driver, for the editor's SQL dialect
    busy: false,
    source: null,    // the EventSource
    attached: false, // the stream has opened at least once
  };

  dbc.cmd = {};

  // wsPath is a path under this tab's workspace.
  dbc.wsPath = (p) => "/api/v1/ws/" + dbc.state.ws + p;

  // ── the API ────────────────────────────────────────────────────────────
  // Every response is the {success, data, error} envelope. A refusal or a
  // failure throws an Error carrying the status and the server's words.
  dbc.api = async function (method, path, body) {
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
  };

  // putLayout writes layout values (PUT /api/v1/layout plus query, e.g.
  // app.js's ?win=), one write at a time and in the order asked. Each
  // write is a separate request, and the browser spreads requests over
  // several connections, so two sent back to back can reach the server in
  // either order — and the store keeps whichever lands last. Ctrl+B twice
  // in quick succession could leave "sideHidden" saved as "1" under an
  // open sidebar (folded again on the next load); a run of strip changes
  // could save an older "tabs" order or "groups" after a newer one
  // (N-120). Chaining them costs one round trip on localhost per queued
  // write. A write that fails does not stop the ones after it; its caller
  // still sees the error.
  //
  //   putLayout(a); putLayout(b); putLayout(c)   (in one tick)
  //   PUT a ──answered──► PUT b ──answered──► PUT c
  let layoutWrites = Promise.resolve();
  dbc.putLayout = function (values, query) {
    const p = layoutWrites.then(() => dbc.api("PUT", "/api/v1/layout" + (query || ""), values));
    layoutWrites = p.catch(() => {});
    return p;
  };

  // ── the log and the status bar ─────────────────────────────────────────
  // A LOG PER CONNECTION. The log pane shows the log of the active query
  // tab's connection, so what was said about pg is under pg and what was
  // said about lite under lite, and switching connections (or tabs) swaps
  // the lines shown. Each log is an array of line nodes kept off-screen
  // while another is shown — swapping is a replaceChildren, not a rebuild —
  // and lives for the page's life.
  //
  //   logs: "pg"   ─► [div, div, …]   ◄── shown (#log holds these nodes)
  //         "lite" ─► [div, …]
  //         ""     ─► the holding log: lines about no connection
  //
  // A line names its log (the server stamps a run's lines with the
  // connection it ran on, so a pg run's outcome lands in pg's log even
  // after the tab moved to lite); without one, it goes to the log on
  // screen (dbc.logKey, set by app.js).
  //
  // The "" log is a HOLDING log: it takes lines only while it is the one
  // shown (no connection yet, or after a Disconnect), and when the pane
  // moves on to a connection its lines are folded into that connection's
  // log. Nothing is ever left in a log no screen shows: the page's first
  // words, before any connection, read on in the first connection's log.
  const LOG_MAX = 500; // per log: a long session must not grow the DOM forever
  const logs = new Map();
  let shownLog = "";
  const linesOf = (key) => {
    let l = logs.get(key);
    if (!l) logs.set(key, (l = []));
    return l;
  };
  // trim drops a log's oldest lines past LOG_MAX (from the screen too, when
  // it is the one shown)
  const trim = (lines) => { while (lines.length > LOG_MAX) lines.shift().remove(); };

  // logKey is the log a line with no connection of its own goes to: the
  // one on screen. app.js replaces it with the active query tab's.
  dbc.logKey = () => shownLog;

  dbc.log = function (level, text, key) {
    if (key === undefined || key === null || key === "") key = dbc.logKey();
    if (key === "" && shownLog !== "") key = shownLog; // see the holding log above
    const line = document.createElement("div");
    const t = document.createElement("span");
    t.className = "t";
    t.textContent = new Date().toTimeString().slice(0, 8);
    const msg = document.createElement("span");
    msg.className = level || "info";
    msg.textContent = text;
    line.append(t, msg);
    const lines = linesOf(key);
    lines.push(line);
    if (key === shownLog) {
      const log = $("log");
      log.append(line);
      trim(lines);
      log.scrollTop = log.scrollHeight;
    } else {
      trim(lines);
    }
  };

  // showLog puts log key on screen, its header naming it (title: the
  // connection, a script's name; "" for none).
  dbc.showLog = function (key, title) {
    key = key || "";
    if (key !== shownLog && shownLog === "" && key !== "") {
      // leaving the holding log for a connection's: its lines go along
      const held = linesOf("");
      if (held.length) {
        const into = linesOf(key);
        into.push(...held.splice(0));
        trim(into);
      }
    }
    shownLog = key;
    const log = $("log");
    log.replaceChildren(...linesOf(key));
    log.scrollTop = log.scrollHeight;
    const h = $("log-conn");
    if (h) h.textContent = title ? "· " + title : "";
  };

  // logText is the shown log as plain text, a line each: "HH:MM:SS message".
  dbc.logText = () => linesOf(shownLog).map((l) => l.textContent.slice(0, 8) + " " + l.textContent.slice(8)).join("\n");

  // clearLog empties the shown log, and only it.
  dbc.clearLog = function () {
    linesOf(shownLog).length = 0;
    $("log").replaceChildren();
  };

  // setStatus writes the status bar — the active query tab's — and keeps
  // it with the tab, so switching back shows what it last said.
  dbc.setStatus = function (text, level) {
    const s = $("status");
    s.textContent = text;
    s.className = level || "";
    if (dbc.state.tab) Object.assign(dbc.state.tab, { status: text, level: level || "" });
  };

  // el builds an element: el("div", "cls", "text") or el("div", {class, title, …}, child…)
  dbc.el = function (tag, attrs, ...kids) {
    const e = document.createElement(tag);
    if (typeof attrs === "string") e.className = attrs;
    else if (attrs) for (const k in attrs) {
      if (k === "class") e.className = attrs[k];
      else if (k === "text") e.textContent = attrs[k];
      else if (attrs[k] !== undefined && attrs[k] !== null && attrs[k] !== false) e.setAttribute(k, attrs[k]);
    }
    for (const k of kids) if (k !== null && k !== undefined) e.append(k);
    return e;
  };

  dbc.plural = (n, word) => n === 1 ? "1 " + word : n + " " + word + "s";
})();
