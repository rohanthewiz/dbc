// dbc web — the shared core every other module builds on: the API call, the
// log, the status bar, and the page's state. Loaded first; each module after
// it hangs its piece on window.dbc:
//
//   core.js    dbc.api, dbc.log, dbc.setStatus, dbc.state, dbc.cmd
//   ui.js      dbc.menu, dbc.modal, dbc.clip        (menus, dialogs, clipboard)
//   editor.js  dbc.editor                           (textarea → Monaco)
//   grid.js    dbc.grid                             (the virtualized results grid)
//   plan.js    DbcPlan                              (the plan view, shared with the standalone page)
//   app.js     boot, the event stream, commands, keys
//
// dbc.cmd is the command table (run, stop, explain, history, …). app.js
// fills it; the editor's key bindings and the menus call through it, so a
// module never needs to know which other module implements a command.
(function () {
  "use strict";

  const dbc = (window.dbc = window.dbc || {});
  const $ = (id) => document.getElementById(id);

  dbc.state = {
    ws: "",          // the workspace id
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

  // ── the log and the status bar ─────────────────────────────────────────
  dbc.log = function (level, text) {
    const log = $("log");
    const line = document.createElement("div");
    const t = document.createElement("span");
    t.className = "t";
    t.textContent = new Date().toTimeString().slice(0, 8);
    const msg = document.createElement("span");
    msg.className = level || "info";
    msg.textContent = text;
    line.append(t, msg);
    log.append(line);
    // keep the log bounded; a long session would otherwise grow the DOM forever
    while (log.childElementCount > 500) log.firstElementChild.remove();
    log.scrollTop = log.scrollHeight;
  };

  dbc.setStatus = function (text, level) {
    const s = $("status");
    s.textContent = text;
    s.className = level || "";
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
