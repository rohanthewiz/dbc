// dbc web — the assistant pane: a conversation with an ACP agent (Copilot
// by default) about the query in the editor and the result in the grid.
//
//   ┌ ✦ Copilot · Fast Model ▾ ─────────── ■ stop  ⟲ new  ✕ ┐  header
//   │ ❯ why is this slow?                                   │
//   │ ▤ sent: schema of cats, query, column names           │  transcript
//   │ The planner scans cats because …                      │  (the server's;
//   │ ┌ sql ─────────────────────── ⤓ insert  ⧉ copy ┐       │   mirrored here)
//   │ │ CREATE INDEX ON cats (breed);                │       │
//   │ ⧉ copy reply                                          │
//   │ [⎆ sign in to Copilot]                                │  sign-in row
//   │ [✓] with: query, column names · rows not sent — …     │  context chip
//   │ ask about the query or result…              ⏎ send    │  composer
//   └───────────────────────────────────────────────────────┘
//
// THE SERVER OWNS THE CONVERSATION (web/chat.go): the transcript, the agent,
// what goes with a question. This module draws it and forwards gestures.
// Events on the tab's stream keep it in step:
//
//   chat.msg {i, role, text}   append message i — or, if i is not the next
//                              index (events were missed), fetch it all
//   chat.text {i, text}        append a streamed chunk to message i
//   chat.state {…}             redraw the header, stop and sign-in chips
//   chat.reset                 the transcript was replaced: fetch it
//
// WHAT GOES WITH A QUESTION is decided on the server by the rules the TUI
// uses (workspace.ChatContext): the page sends only what the server cannot
// know — the editor (buffer, caret, selection) and the grid's view (the
// result's seq, the sort, the hidden columns). The chip's forecast comes
// from the same code, so it cannot promise what the question will not send.
//
// SQL IN ANSWERS IS ACTIONABLE, never runnable: ⤓ insert puts a block in
// the editor at the caret, where the user reads it before Ctrl+Enter.
//
// Every message is drawn with the DOM (textContent), never innerHTML: an
// answer is text from a model, and markup in it must stay text.
(function () {
  "use strict";

  const dbc = window.dbc;
  const { api, log, el, state } = dbc;
  const $ = (id) => document.getElementById(id);

  const els = {
    app: document.querySelector(".app"), pane: $("chat"), split: $("chat-split"),
    model: $("chat-model"), stop: $("chat-stop"), newBtn: $("chat-new"), close: $("chat-close"),
    trans: $("chat-trans"), sign: $("chat-sign"), attach: $("chat-attach"), ctx: $("chat-ctx"),
    input: $("chat-input"), send: $("chat-send"), toggle: $("chat-btn"),
  };

  const c = {
    open: false,
    view: null,     // the server's chatView
    msgs: [],
    agents: [],
    recent: null,   // saved conversations, as last listed (shown in the empty pane)
    follow: true,   // keep the transcript scrolled to the end
  };

  // SQL_LANGS are the fence languages that get ⤓ insert — the TUI's list
  // (tui/chat.go isSQLLang); an unlabeled fence counts, since the preamble
  // asks for ```sql and models often drop the label.
  const SQL_LANGS = new Set(["", "sql", "postgres", "postgresql", "psql", "mysql", "sqlite", "pgsql", "plpgsql"]);

  // ── the transcript ─────────────────────────────────────────────────────
  function atBottom() {
    const t = els.trans;
    return t.scrollHeight - t.scrollTop - t.clientHeight < 24;
  }

  function scrollEnd() {
    if (c.follow) els.trans.scrollTop = els.trans.scrollHeight;
  }

  els.trans.addEventListener("scroll", () => { c.follow = atBottom(); });

  function renderAll() {
    els.trans.replaceChildren();
    if (!c.msgs.length) {
      els.trans.append(emptyPane());
      return;
    }
    c.msgs.forEach((m, i) => els.trans.append(renderMsg(m, i)));
    scrollEnd();
  }

  // emptyPane is the hint for someone new — and, first, the saved
  // conversations for someone returning.
  function emptyPane() {
    const box = el("div", "cempty");
    if (c.recent && c.recent.length) {
      box.append(el("div", "chead2", "recent conversations"));
      for (const r of c.recent.slice(0, 5)) box.append(recentRow(r));
      if (c.recent.length > 5) {
        const all = el("button", { type: "button", class: "linkish" }, "all " + c.recent.length + " recent…");
        all.addEventListener("click", recentModal);
        box.append(all);
      }
    }
    for (const p of [
      "Ask about the query in the editor or the result in the grid.",
      "The query, any error, and the columns of the tables it or your question names go with each question. " +
        "Result rows go only on connections with ai_rows = true (up to ai_context_rows of them), " +
        "in the grid's sort order and without its hidden columns.",
      "SQL in answers gets ⤓ insert, which puts it in the editor — nothing an answer says ever runs by itself.",
      "Enter sends · Shift+Enter adds a line · Ctrl+K stops an answer · Esc returns to the editor · Ctrl+I toggles",
    ]) box.append(el("p", "hint", p));
    return box;
  }

  function renderMsg(m, i) {
    const row = el("div", { class: "cmsg " + m.role, "data-i": String(i) });
    switch (m.role) {
      case "user":
        row.append(el("span", "cprompt", "❯ "), m.text);
        break;
      case "agent":
        row.append(renderAnswer(m.text));
        {
          const cp = el("button", { type: "button", class: "linkish", title: "Copy the whole reply" }, "⧉ copy reply");
          cp.addEventListener("click", () => dbc.clip.copyText(c.msgs[i].text, "the reply"));
          row.append(el("div", "cfoot", cp));
        }
        break;
      default:
        row.textContent = m.text;
    }
    return row;
  }

  // renderAnswer draws a reply: fenced code blocks with their actions, and
  // paragraphs and lists with `code` and **bold**. A fence still open while
  // the answer streams is drawn as a block already.
  function renderAnswer(text) {
    const box = el("div", "answer");
    const lines = text.split("\n");
    let para = [], list = null;
    const flushPara = () => {
      if (para.length) box.append(inline(el("p"), para.join("\n")));
      para = [];
    };
    const flushList = () => { list = null; };
    for (let i = 0; i < lines.length; i++) {
      const line = lines[i];
      const fence = line.match(/^\s*```\s*([\w+-]*)\s*$/);
      if (fence) {
        flushPara(); flushList();
        const body = [];
        for (i++; i < lines.length && !/^\s*```\s*$/.test(lines[i]); i++) body.push(lines[i]);
        box.append(codeBlock(fence[1].toLowerCase(), body.join("\n")));
        continue;
      }
      const item = line.match(/^\s*(?:[-*•]|(\d+)[.)])\s+(.*)$/);
      if (item) {
        flushPara();
        const tag = item[1] ? "ol" : "ul";
        if (!list || list.tagName.toLowerCase() !== tag) { list = el(tag); box.append(list); }
        list.append(inline(el("li"), item[2]));
        continue;
      }
      const head = line.match(/^\s*#{1,6}\s+(.*)$/);
      if (head) {
        flushPara(); flushList();
        box.append(inline(el("p", "ahead"), head[1]));
        continue;
      }
      if (line.trim() === "") { flushPara(); flushList(); continue; }
      flushList();
      para.push(line);
    }
    flushPara();
    return box;
  }

  // inline appends text to node with `code` and **bold** spans, as text.
  function inline(node, text) {
    for (const part of text.split(/(`[^`\n]+`|\*\*[^*\n]+\*\*)/)) {
      if (!part) continue;
      if (part.length > 2 && part.startsWith("`") && part.endsWith("`")) node.append(el("code", null, part.slice(1, -1)));
      else if (part.length > 4 && part.startsWith("**") && part.endsWith("**")) node.append(el("strong", null, part.slice(2, -2)));
      else node.append(part);
    }
    return node;
  }

  function codeBlock(lang, code) {
    const head = el("div", "chead3", el("span", "clang", lang || "code"));
    if (SQL_LANGS.has(lang)) {
      const ins = el("button", { type: "button", class: "linkish", title: "Put it in the editor at the caret — nothing runs" }, "⤓ insert");
      ins.addEventListener("click", () => {
        dbc.editor.insert(code.replace(/\n+$/, ""));
        log("ok", "inserted the assistant's SQL at the caret — review it, then Ctrl+Enter runs it");
      });
      head.append(ins);
    }
    const cp = el("button", { type: "button", class: "linkish" }, "⧉ copy");
    cp.addEventListener("click", () => dbc.clip.copyText(code, "the code block"));
    head.append(cp);
    return el("div", "cblock", head, el("pre", null, code));
  }

  // A streamed chunk redraws its message once per frame, not per chunk.
  let redrawIdx = -1, redrawQueued = false;
  function redrawMsg(i) {
    redrawIdx = i;
    if (redrawQueued) return;
    redrawQueued = true;
    requestAnimationFrame(() => {
      redrawQueued = false;
      const old = els.trans.querySelector('.cmsg[data-i="' + redrawIdx + '"]');
      const fresh = renderMsg(c.msgs[redrawIdx], redrawIdx);
      if (old) old.replaceWith(fresh); else els.trans.append(fresh);
      scrollEnd();
    });
  }

  // ── the chrome: header, stop, sign-in ──────────────────────────────────
  function renderChrome() {
    const v = c.view;
    if (!v) return;
    let label = "✦ " + v.agent.name;
    if (v.state === "starting") label += " · connecting…";
    else if (v.model) label += " · " + v.model;
    else if (v.state === "dead") label += " · not connected";
    els.model.textContent = label + " ▾";
    els.stop.hidden = !v.streaming;
    els.send.disabled = v.streaming;
    els.pane.classList.toggle("streaming", v.streaming);
    if (els.toggle) els.toggle.classList.toggle("on", c.open);

    els.sign.replaceChildren();
    els.sign.hidden = !v.signIn;
    if (v.signIn === "offer") {
      const b = el("button", { type: "button", class: "primary" }, "⎆ sign in to " + v.agent.name);
      b.addEventListener("click", signIn);
      els.sign.append(b);
    } else if (v.signIn === "starting") {
      els.sign.append(el("span", "hint", "asking GitHub for a sign-in code…"));
    } else if (v.signIn === "waiting") {
      const cp = el("button", { type: "button", class: "primary" }, "⧉ copy code");
      cp.addEventListener("click", () => dbc.clip.copyText(v.code, "the sign-in code"));
      const open = el("a", { class: "btnlink", href: v.url, target: "_blank", rel: "noopener noreferrer" }, "↗ open page");
      const cancel = el("button", { type: "button" }, "✕ cancel");
      cancel.addEventListener("click", () => post("/chat/signin/cancel"));
      els.sign.append(el("code", "ccode", v.code), cp, open, cancel);
    }
  }

  // ── talking to the server ──────────────────────────────────────────────
  async function post(path, body) {
    try {
      return await api("POST", dbc.wsPath(path), body);
    } catch (e) {
      log(e.status === 409 ? "warn" : "err", e.message);
      throw e;
    }
  }

  // fetchAll replaces the mirror with the server's transcript.
  async function fetchAll() {
    let snap;
    try {
      snap = await api("GET", dbc.wsPath("/chat"));
    } catch (_) { return; } // a lost workspace is app.js's to handle
    c.view = snap.view;
    c.msgs = snap.msgs;
    c.agents = snap.agents;
    if (!c.msgs.length) await loadRecent();
    renderAll();
    renderChrome();
  }

  async function loadRecent() {
    try {
      c.recent = await api("GET", "/api/v1/chats?ws=" + encodeURIComponent(state.ws));
    } catch (_) { c.recent = []; }
  }

  function onEvent(type, d) {
    switch (type) {
      case "chat.msg":
        if (d.i !== c.msgs.length) { fetchAll(); return; }
        if (!c.msgs.length) els.trans.replaceChildren(); // the empty pane's hint goes
        c.msgs.push({ role: d.role, text: d.text });
        els.trans.append(renderMsg(c.msgs[d.i], d.i));
        if (d.role === "user") c.follow = true;
        scrollEnd();
        break;
      case "chat.text":
        if (d.i !== c.msgs.length - 1) { fetchAll(); return; }
        c.msgs[d.i].text += d.text;
        redrawMsg(d.i);
        break;
      case "chat.state":
        c.view = d;
        renderChrome();
        break;
      case "chat.reset":
        c.follow = true;
        fetchAll();
        break;
    }
  }

  // ── opening and closing ────────────────────────────────────────────────
  function saveLayout(values) {
    api("PUT", "/api/v1/layout", values).catch((e) => log("warn", "layout not saved: " + e.message));
  }

  async function show(focusInput) {
    if (!c.open) {
      c.open = true;
      els.pane.hidden = false;
      els.app.classList.add("chat-on");
      saveLayout({ chatOpen: "1" });
    }
    renderChrome();
    if (focusInput) els.input.focus();
    if (!state.ws) return; // boot opens it once the workspace is known
    if (!c.view || c.view.state === "idle") {
      // the agent starts now, lazily — most sessions never open the pane.
      // A dead one is left for ⟲ new: restarting it on every open would
      // only be refused again (a missing binary, a signed-out account).
      try {
        const snap = await api("POST", dbc.wsPath("/chat/open"));
        c.view = snap.view; c.msgs = snap.msgs; c.agents = snap.agents;
        if (!c.msgs.length) await loadRecent();
        renderAll();
        renderChrome();
      } catch (e) { log("err", "the assistant: " + e.message); }
    }
    refreshContext();
  }

  function hide() {
    if (!c.open) return;
    c.open = false;
    els.pane.hidden = true;
    els.app.classList.remove("chat-on");
    if (els.toggle) els.toggle.classList.remove("on");
    saveLayout({ chatOpen: "" });
    if (els.pane.contains(document.activeElement)) dbc.editor.focus();
  }

  // toggle is Ctrl+I and the ✦ button: open the pane with the keyboard in
  // it, or — from inside it — go back to the editor. Closing is ✕.
  function toggle() {
    if (c.open && els.pane.contains(document.activeElement)) { dbc.editor.focus(); return; }
    show(true);
  }

  // askAbout drafts a question in the composer — not sent, so the user can
  // add to it first (the TUI's rule for every "✦ ask" menu row).
  function askAbout(q) {
    show(true);
    els.input.value = q;
    growInput();
    els.input.setSelectionRange(q.length, q.length);
    refreshContext();
  }

  // ── asking ─────────────────────────────────────────────────────────────
  function gridView() {
    if (!dbc.grid.hasResult()) return null;
    const v = dbc.grid.view();
    return { seq: v.seq, sort: v.sort, desc: v.desc, hidden: v.hidden };
  }

  const request = (question) => ({
    question, attach: els.attach.checked, editor: dbc.cmd.editorState(), view: gridView(),
  });

  async function submit() {
    const q = els.input.value.trim();
    if (!q) return;
    if (c.view && c.view.streaming) {
      log("warn", "the assistant is still answering — Ctrl+K stops it");
      return;
    }
    els.input.value = "";
    growInput();
    try {
      await api("POST", dbc.wsPath("/chat/ask"), request(q));
    } catch (e) {
      if (e.status === 409 && /ask again/.test(e.message)) {
        // the grid was drawn from a result a rerun replaced: catch up and
        // ask once more, with the view of the result that is there now
        await dbc.grid.load();
        try {
          await api("POST", dbc.wsPath("/chat/ask"), request(q));
          return;
        } catch (e2) { e = e2; }
      }
      els.input.value = q; // nothing was sent; the question is not lost
      growInput();
      log(e.status === 409 ? "warn" : "err", e.message);
    }
  }

  // The chip's forecast, debounced: the composer, the editor and the grid
  // each move it, and one request per pause is plenty.
  let ctxTimer = 0, ctxSeq = 0;
  function refreshContext() {
    if (!c.open || !state.ws) return;
    clearTimeout(ctxTimer);
    ctxTimer = setTimeout(async () => {
      if (!els.attach.checked) { els.ctx.textContent = "context off — question only"; return; }
      const n = ++ctxSeq;
      try {
        const d = await api("POST", dbc.wsPath("/chat/context"), request(els.input.value));
        if (n === ctxSeq) els.ctx.textContent = "with: " + d.note;
      } catch (_) { /* a stale view: the grid's reload refreshes it */ }
    }, 250);
  }

  function growInput() {
    els.input.style.height = "auto";
    els.input.style.height = Math.min(els.input.scrollHeight, 6 * 20 + 10) + "px";
  }

  els.input.addEventListener("input", () => { growInput(); refreshContext(); });
  els.input.addEventListener("keydown", (e) => {
    if (e.key === "Enter" && !e.shiftKey && !e.altKey && !e.isComposing) {
      e.preventDefault();
      submit();
    } else if (e.key === "Escape") {
      e.preventDefault();
      dbc.editor.focus();
    }
  });
  els.send.addEventListener("click", submit);
  els.attach.addEventListener("change", refreshContext);
  dbc.editor.onChange(refreshContext);
  dbc.grid.onView(refreshContext);

  // Ctrl+K inside the pane stops the answer, not the run — the key goes to
  // what the user is looking at. Marked handled so app.js leaves it alone.
  els.pane.addEventListener("keydown", (e) => {
    if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === "k" && c.view && c.view.streaming) {
      e.preventDefault();
      stop();
    }
  });

  function stop() { post("/chat/stop").catch(() => {}); }
  function newChat() { post("/chat/new").catch(() => {}); }
  function signIn() { post("/chat/signin").catch(() => {}); }

  els.stop.addEventListener("click", stop);
  els.newBtn.addEventListener("click", newChat);
  els.close.addEventListener("click", hide);
  if (els.toggle) els.toggle.addEventListener("click", toggle);

  // ── menus ──────────────────────────────────────────────────────────────
  // modelMenu lists the agent's models and the other assistants — the
  // TUI's model chip menu.
  function modelMenu(x, y) {
    const v = c.view;
    const items = [];
    if (v && v.models.length) {
      items.push({ head: "model" });
      for (const m of v.models) {
        items.push({ label: (m.id === v.modelId ? "● " : "  ") + m.name, key: m.usage,
          act: () => post("/chat/model", { id: m.id }).catch(() => {}) });
      }
    }
    items.push({ head: "assistant" });
    for (const a of c.agents) {
      items.push({ label: (v && a.id === v.agent.id ? "● " : "  ") + a.name, key: a.binary,
        why: a.installed ? "" : a.name + " is not installed — " + a.install,
        act: () => post("/chat/agent", { id: a.id }).catch(() => {}) });
    }
    dbc.menu.open(x, y, items);
  }

  els.model.addEventListener("click", () => {
    const r = els.model.getBoundingClientRect();
    modelMenu(r.left, r.bottom + 2);
  });

  // transcriptMenu is the transcript's right-click menu, row for row the
  // TUI's (tui/chat.go openChatMenu).
  els.trans.addEventListener("contextmenu", (e) => {
    if (e.target.closest("a")) return; // a link keeps the browser's own menu
    e.preventDefault();
    const last = [...c.msgs].reverse().find((m) => m.role === "agent");
    const v = c.view || { agent: { name: "the assistant", canSignIn: false } };
    dbc.menu.open(e.clientX, e.clientY, [
      { label: "Copy last reply", why: last ? "" : "no reply to copy yet", act: () => dbc.clip.copyText(last.text, "the reply") },
      { label: "Copy conversation", act: () => dbc.clip.copyText(transcriptText(), "the conversation") },
      { head: "" },
      { label: "⟲ New conversation", act: newChat },
      { label: "Recent conversations…", act: recentModal },
      { label: "Delete this conversation", why: c.msgs.length || v.archiveId ? "" : "no conversation to delete",
        act: () => post("/chat/delete").catch(() => {}) },
      { label: "Model and assistant…", act: () => modelMenu(e.clientX, e.clientY) },
      // Always offered for an agent dbc can sign in to: it is also how to
      // check which account is signed in ("already signed in as …").
      { label: "Sign in to " + v.agent.name + "…", why: v.agent.canSignIn ? "" : "dbc cannot sign in to " + v.agent.name + " — " + (v.agent.auth || ""),
        act: signIn },
    ]);
  });

  function transcriptText() {
    return c.msgs.map((m) => m.role === "user" ? "> " + m.text + "\n\n" : m.role === "agent" ? m.text + "\n\n" : "  " + m.text + "\n").join("");
  }

  // ── saved conversations ────────────────────────────────────────────────
  // whenText is the TUI's: the clock for today, the date before that, the
  // year once it is not this one — a row is scanned, not read.
  function whenText(iso) {
    const t = new Date(iso), now = new Date();
    if (isNaN(t)) return "";
    const hm = t.toTimeString().slice(0, 5);
    if (t.toDateString() === now.toDateString()) return hm;
    const md = t.toLocaleDateString(undefined, { month: "short", day: "numeric" });
    return t.getFullYear() === now.getFullYear() ? md : md + " " + t.getFullYear();
  }

  function recentDetail(r) {
    const parts = [whenText(r.updated), dbc.plural(r.count, "msg")];
    if (r.conn && r.conn !== state.active) parts.push(r.conn);
    return parts.join(" · ");
  }

  function recentRow(r, after) {
    const b = el("button", { type: "button", class: "crecent", title: r.title },
      el("span", "ct", "◷ " + r.title), el("span", "cd", recentDetail(r)));
    b.addEventListener("click", () => { if (after) after(); openSaved(r.id); });
    b.addEventListener("contextmenu", (e) => { e.preventDefault(); savedMenu(e.clientX, e.clientY, r, after); });
    return b;
  }

  // savedMenu: deleting a saved conversation is the one destructive gesture
  // here, so it is a named row of a menu — never a single stray click.
  function savedMenu(x, y, r, after) {
    dbc.menu.open(x, y, [
      { head: r.title },
      { label: "Open conversation", act: () => { if (after) after(); openSaved(r.id); } },
      { label: "Delete conversation", act: () => deleteSaved(r) },
    ]);
  }

  async function openSaved(id) {
    try {
      await post("/chat/load", { id });
      show(true);
    } catch (_) { /* logged */ }
  }

  async function deleteSaved(r) {
    try {
      await api("DELETE", "/api/v1/chats/" + encodeURIComponent(r.id));
      log("ok", "deleted the saved conversation \"" + r.title + "\"");
    } catch (e) {
      log("err", e.message);
      return;
    }
    c.recent = (c.recent || []).filter((x) => x.id !== r.id);
    if (!c.msgs.length) renderAll();
    if (dbc.modal.isOpen() && recentList) drawRecentList();
  }

  let recentList = null;
  function drawRecentList() {
    recentList.replaceChildren();
    if (!c.recent.length) recentList.append(el("p", "hint", "no saved conversations"));
    for (const r of c.recent) recentList.append(recentRow(r, () => dbc.modal.close()));
  }

  async function recentModal() {
    await loadRecent();
    recentList = el("div", "clist");
    drawRecentList();
    dbc.modal.open({
      title: "Recent conversations", cls: "wide", body: el("div", "history", recentList),
      foot: el("div", "mfoot", el("span", "hint", "click reopens (as a transcript — the assistant starts fresh) · right-click to delete")),
      onClose: () => { recentList = null; },
    });
  }

  // ── resizing the pane ──────────────────────────────────────────────────
  function setWidth(px) {
    const w = Math.max(260, Math.min(px, innerWidth - 360));
    els.app.style.setProperty("--chat-w", w + "px");
  }
  els.split.addEventListener("pointerdown", (e) => {
    e.preventDefault();
    const startX = e.clientX, startW = els.pane.getBoundingClientRect().width;
    els.split.setPointerCapture(e.pointerId);
    els.split.classList.add("dragging");
    const move = (m) => setWidth(startW - (m.clientX - startX));
    const up = () => {
      els.split.removeEventListener("pointermove", move);
      els.split.removeEventListener("pointerup", up);
      els.split.classList.remove("dragging");
      saveLayout({ chatWidth: String(Math.round(els.pane.getBoundingClientRect().width)) });
    };
    els.split.addEventListener("pointermove", move);
    els.split.addEventListener("pointerup", up);
  });

  // ── hooks app.js calls ─────────────────────────────────────────────────
  dbc.chat = {
    onEvent,
    // boot: the saved layout, before the workspace is known
    boot(layout) {
      if (layout.chatWidth) setWidth(Number(layout.chatWidth));
      if (layout.chatOpen === "1") show(false);
    },
    // onState: the workspace is attached (new or reattached). A reattached
    // one may already hold a conversation — a reload mid-answer finds it.
    async onState() {
      await fetchAll();
      if (c.open) show(false);
    },
    refresh: refreshContext,
    isOpen: () => c.open,
  };

  Object.assign(dbc.cmd, { assistant: toggle, askAbout });
})();
