// dbc web — Go scripts: the scripts browser (Ctrl+O), the files script
// tabs edit, the check that marks errors as you type, and the editor's Go
// completion and hover from the sdb API.
//
// app.js owns the tabs; this module owns what a script tab edits. app.js
// makes it with dbc.scripts.create(host) — the host is what the module
// needs of the strip (open a tab, run in the tab on screen, redraw) — and
// calls back into it on every edit, save and "scripts" event.
//
// THE FILES. A script tab's text is a .go file in scripts_dir, read and
// written by name through web/scripts.go, never by path. Unlike a console
// it is saved only when asked (Ctrl+S, ✓ Save, or Run, which saves first):
// a half-typed script that autosaved would be what the TUI, cron or
// `dbc script` run next. The save is the console protocol:
//
//	PUT {text, base: rev} ─► {rev}                  saved
//	                      ─► {conflict, text, rev}  not saved: the file's
//	                         text loads as ONE undoable edit, so Ctrl+Z
//	                         gets this tab's text back, and the next save
//	                         (from the new rev) writes it over
//	                      ─► {conflict, rev: ""}    the file was deleted or
//	                         trashed: the tab keeps its text, marked
//	                         unsaved; the next save (base "") recreates it
//	                      ─► 409                    base "" over a file made
//	                         elsewhere meanwhile: loaded as a conflict
//
// DRAFTS. Unsaved text survives a reload, kept in this browser's
// localStorage under the script's name and the window that typed it, with
// the revision it was typed against:
//
//	dbc.script.draft.<owner>:<name> = {base, text, at}
//	dbc.draftSeen.<owner>           = ms of the owner's last heartbeat
//
// Opening the script again: a draft whose base is the file's revision is
// put back as it was (unsaved). One whose base is older — the file moved on
// meanwhile, in the TUI or vim — is put back too, but keeps its old base,
// so the first save meets the conflict above rather than silently writing
// over the newer file. localStorage, not the saved tab: the draft belongs to
// the script, wherever it is opened next, and a browser that refuses storage
// (a private window) only loses the safety net, not the editing.
//
// WHY PER WINDOW. One key per script name meant two windows with the same
// script open shared it, and the last to type took it: a reload of the
// other window then came back with the wrong window's text, and its own
// unsaved edits were gone. So each browser tab is a draft OWNER (an id in
// its sessionStorage — which a reload keeps, and which a duplicated tab
// copies, so app.js gives a copy a new one) and writes only its own key.
// "Wherever it is opened next" still holds, through orphans:
//
//	open a script ─► my own draft? ─yes─► use it
//	                      │no
//	                      ▼
//	   drafts of owners whose heartbeat stopped (closed windows), and the
//	   one-key draft an older page wrote ─► the newest is adopted: moved
//	   under my key, the rest dropped (logged) — as before, a closed
//	   window's draft is picked up by the next window to open the script
//	                      │none
//	                      ▼
//	   a LIVE window's draft is left alone: that window is still editing
//	   it, so this one shows the saved file and says so
//
// An owner beats every 30 s; a stopped beat is an orphan after 5 minutes
// (a hidden tab's timers can be throttled to once a minute). Closing the
// tab (pagehide) shortens that to 15 s rather than zero, since a reload
// fires pagehide too and comes straight back as the same owner.
//
// THE CHECK. ~600 ms after typing stops, the tab's text — unsaved — goes to
// POST /api/v1/script-check (script.Check: parse, Run's signature, yaegi's
// compile, the map comma-ok lint; it never runs the script), and what comes
// back becomes Monaco markers — and, as ced draws them, a dot in the gutter
// and the message after the line (editor.js setMarkers). ✓ Check does the
// same at once and also lists the findings in the log.
//
// GO TO DEFINITION AND USAGES. F12 (or Ctrl/⌘+click) and Shift+F12 on a
// name ask POST /api/v1/script-symbol (script.Resolve: go/types over the
// tab's text, saved or not) where the script declares it and where it uses
// it. Scope is honoured — a shadowing err is its own symbol — and a member
// of sdb or another import (s.Query) has no declaration in the script, so
// F12 finds none, but Shift+F12 still lists its uses from the same s.
//
// RENAME. F2 on a name renames every use of it the same scope-aware way
// (POST /api/v1/script-symbol-rename, script.Rename), as F2 in a SQL tab
// renames an alias. The box does not open on what cannot be renamed — an
// imported member, a builtin, Run — and a new name already taken in that
// scope, or one that would change what another name means, is refused
// with the server's sentence beside the box.
(function () {
  "use strict";

  const dbc = window.dbc;
  const { api, log, el } = dbc;

  // DRAFT + name is an older page's one-key draft; DRAFT + owner + ":" +
  // name is a window's own. A script name cannot hold ":" (letters, digits,
  // '.', '-' and '_'), so the two never collide.
  const DRAFT = "dbc.script.draft.";
  const OWNER = "dbc.draftOwner"; // sessionStorage: this browser tab's owner id
  const SEEN = "dbc.draftSeen.";  // localStorage: SEEN + owner = last heartbeat
  const BEAT = 30e3, ORPHAN = 5 * 60e3, CLOSING = 15e3;

  // Storage can throw (a private window, blocked site data) or come back
  // empty; a draft is a convenience, so every touch is wrapped.
  function readKey(k) {
    try {
      const v = localStorage.getItem(k);
      const d = v ? JSON.parse(v) : null;
      return d && typeof d.text === "string" ? d : null;
    } catch (_) { return null; }
  }
  function writeKey(k, d) {
    try {
      if (d) localStorage.setItem(k, JSON.stringify(d));
      else localStorage.removeItem(k);
    } catch (_) { /* no storage: the tab still edits, a reload just loses the draft */ }
  }
  function storageKeys() {
    try { return Object.keys(localStorage); } catch (_) { return []; }
  }

  // owner is this browser tab's draft owner id, made on first use. Without
  // sessionStorage it lives as long as the page — drafts then still keep
  // one window from overwriting another's, and a reload finds its old
  // draft as an orphan once the old page's beat has stopped.
  let pageOwner = "";
  function owner() {
    try {
      let id = sessionStorage.getItem(OWNER);
      if (!id) {
        id = pageOwner || Math.random().toString(36).slice(2, 10);
        sessionStorage.setItem(OWNER, id);
      }
      return (pageOwner = id);
    } catch (_) {
      return pageOwner || (pageOwner = Math.random().toString(36).slice(2, 10));
    }
  }
  // newOwner is for a duplicated browser tab (app.js: the boot claim's
  // 409): its sessionStorage, owner id included, is a copy of the
  // original's, and two live windows must not share a key.
  function newOwner() {
    pageOwner = "";
    try { sessionStorage.removeItem(OWNER); } catch (_) { /* owner() makes a page-only one */ }
    owner();
    beat();
  }

  const ownKey = (name) => DRAFT + owner() + ":" + name;
  const readDraft = (name) => readKey(ownKey(name));
  const writeDraft = (name, d) => writeKey(ownKey(name), d);

  function beat(at) {
    try { localStorage.setItem(SEEN + owner(), String(at ?? Date.now())); } catch (_) { /* no storage, no drafts */ }
  }
  // live: owner id has beaten within ORPHAN. An owner never seen (its
  // pages predate the beat, or storage was cleared) counts as gone.
  function live(id) {
    let t = 0;
    try { t = Number(localStorage.getItem(SEEN + id)) || 0; } catch (_) { /* gone */ }
    return Date.now() - t < ORPHAN;
  }

  // othersDrafts lists the drafts of name that are not this window's:
  // {key, d, live} — the older page's one-key draft counts as an orphan.
  function othersDrafts(name) {
    const me = owner(), out = [];
    for (const k of storageKeys()) {
      if (!k.startsWith(DRAFT)) continue;
      const rest = k.slice(DRAFT.length), i = rest.indexOf(":");
      const id = i < 0 ? "" : rest.slice(0, i), n = i < 0 ? rest : rest.slice(i + 1);
      if (n !== name || id === me) continue;
      const d = readKey(k);
      if (d) out.push({ key: k, d, live: id !== "" && live(id) });
    }
    return out;
  }

  // adoptOrphan moves the newest orphaned draft of name under this
  // window's key and drops the other orphans — see DRAFTS. It returns the
  // draft, or null, and how many live windows hold a draft of their own.
  function adoptOrphan(name) {
    const all = othersDrafts(name);
    const orphans = all.filter((o) => !o.live).sort((a, b) => (b.d.at || 0) - (a.d.at || 0));
    const liveN = all.length - orphans.length;
    if (!orphans.length) return { d: null, liveN };
    const d = orphans[0].d;
    writeDraft(name, d);
    for (const o of orphans) writeKey(o.key, null);
    if (orphans.length > 1) {
      const n = orphans.length - 1;
      log("warn", name + ": " + n + " older unsaved draft" + (n > 1 ? "s" : "") +
        " from closed windows " + (n > 1 ? "were" : "was") + " dropped for the newest one");
    }
    return { d, liveN };
  }

  // the heartbeat, and the forgetting of owners that are gone and left no
  // drafts behind (a window closed with everything saved)
  beat();
  setInterval(beat, BEAT);
  document.addEventListener("visibilitychange", () => { if (!document.hidden) beat(); });
  window.addEventListener("pagehide", () => beat(Date.now() - ORPHAN + CLOSING));
  (function forgetGone() {
    const keys = storageKeys();
    for (const k of keys) {
      if (!k.startsWith(SEEN)) continue;
      const id = k.slice(SEEN.length);
      if (!live(id) && !keys.some((x) => x.startsWith(DRAFT + id + ":"))) writeKey(k, null);
    }
  })();

  // PLUGIN FILES. A pipeline plugin file of plugins_dir (web/plugins.go)
  // is a .go file edited exactly as a script is, so it opens as a script
  // tab — one editor, one draft scheme, one conflict protocol — under the
  // name "plugin:<file>", which no script can have (":" is not in a
  // script name). Only the routes differ: its file, its check (the
  // plugin's shape, not Run's) and what Run does (save, then say what the
  // loader made of it: the plugin is in the pipeline palette, or why not).
  const PLUG = "plugin:";
  const isPlug = (name) => typeof name === "string" && name.startsWith(PLUG);
  const fileOf = (name) => (isPlug(name) ? name.slice(PLUG.length) : name);
  const path = (name) => (isPlug(name) ? "/api/v1/plugin-files/" + encodeURIComponent(fileOf(name)) :
    "/api/v1/scripts/" + encodeURIComponent(name));
  const KIND_GLYPH = { source: "⇥", transform: "ƒ", sink: "⇤", action: "▸" }; // as the palette draws them

  // ago is a short age for the browser's list: "now", "5m", "3h", "2d".
  function ago(iso) {
    const s = (Date.now() - new Date(iso).getTime()) / 1000;
    if (!(s >= 0)) return "";
    if (s < 60) return "now";
    if (s < 3600) return Math.floor(s / 60) + "m";
    if (s < 86400) return Math.floor(s / 3600) + "h";
    return Math.floor(s / 86400) + "d";
  }

  // goName makes what was typed a script name: ".go" added when left off.
  // The server has the last word on what is valid (userdata.ValidScriptName).
  const goName = (s) => (s && !/\.go$/i.test(s) ? s + ".go" : s);

  // freeName is base, or base-2.go, base-3.go … — the first not in taken.
  function freeName(base, taken) {
    const stem = base.replace(/\.go$/i, "");
    let n = stem + ".go", i = 2;
    while (taken.includes(n)) n = stem + "-" + i++ + ".go";
    return n;
  }

  // ask is a one-field prompt; it resolves to the trimmed text, or null
  // when cancelled. The stem of a "x.go" value is selected, so typing
  // replaces the name and keeps the extension.
  function ask(o) {
    return new Promise((resolve) => {
      let done = false;
      const finish = (v) => { if (!done) { done = true; resolve(v); } };
      const input = el("input", { class: "hfilter", value: o.value || "", maxlength: "68",
        "aria-label": o.title, spellcheck: "false", autocomplete: "off" });
      const go = el("button", { type: "button", class: "primary" }, o.ok || "OK");
      const no = el("button", { type: "button" }, "Cancel");
      const submit = () => { finish(input.value.trim() || null); dbc.modal.close(); };
      go.addEventListener("click", submit);
      no.addEventListener("click", () => dbc.modal.close());
      input.addEventListener("keydown", (e) => { if (e.key === "Enter") { e.preventDefault(); submit(); } });
      dbc.modal.open({ title: o.title, focus: input, onClose: () => finish(null),
        body: el("div", "confirm", o.hint ? el("p", null, o.hint) : null, input), foot: el("div", "mfoot", go, no) });
      const dot = input.value.search(/\.go$/i);
      if (dot > 0) input.setSelectionRange(0, dot);
    });
  }

  function create(host) {
    // files: script name → what this window knows of its file:
    //   rev    the revision the tab's text is based on ("" = no file: new,
    //          or deleted since)
    //   saved  the file's text at rev
    //   text   the tab's text when last seen (the editor has the live copy)
    //   missing  the file is gone (deleted or trashed); the tab keeps its text
    //   saving   the PUT in flight
    //   diags    the last check's findings
    // An entry lives while a tab shows the script (forget drops it).
    const files = new Map();
    const key = (name) => "s:" + name;
    const textOf = (e) => dbc.editor.docText(key(e.name)) ?? e.text;
    const isDirty = (e) => !!e && (e.missing || textOf(e) !== e.saved);

    // ── loading and saving ─────────────────────────────────────────────
    async function load(name) {
      if (files.has(name)) return files.get(name);
      let text = "", rev = "", missing = false;
      try {
        const r = await api("GET", path(name));
        text = r.text; rev = r.rev;
      } catch (err) {
        if (err.status !== 404) throw err;
        missing = true; // a saved tab whose script is gone: it opens empty, unsaved
      }
      if (files.has(name)) return files.get(name); // another load won the race
      const e = { name, rev, saved: text, text, missing, saving: null, diags: [], dirty: false };
      let d = readDraft(name);
      if (!d) {
        const o = adoptOrphan(name);
        d = o.d;
        if (!d && o.liveN) {
          log("info", name + " has unsaved edits in another dbc web window — this tab shows the saved file; " +
            "save there first to see them here");
        }
      }
      if (d && d.text !== text) {
        e.text = d.text;
        if (!missing && d.base !== rev) {
          // typed against an older file: keep that base, so the first save
          // meets the conflict (and loads the newer file as an undoable
          // edit) rather than writing over it
          e.rev = d.base || "";
          log("warn", name + " changed on disk since your unsaved edits here — they are kept; " +
            "saving first shows the file's version (Ctrl+Z then brings yours back)");
        }
      } else if (d) {
        writeDraft(name, null); // the draft is what the file now says
      }
      e.dirty = isDirty(e);
      files.set(name, e);
      return e;
    }

    // changed tells the host when a script's unsaved mark flips, so the
    // strip and the header redraw only then, not on every keystroke.
    function changed(e, force) {
      const d = isDirty(e);
      if (d === e.dirty && !force) return;
      e.dirty = d;
      host.changed(e.name);
    }

    // adopt makes text, at rev, the file's — in the document as one edit.
    function adopt(e, text, rev) {
      Object.assign(e, { rev, saved: text, text, missing: false });
      dbc.editor.replaceDoc(key(e.name), text);
      edited(e.name); // the draft and the mark follow (replaceDoc may not fire a change)
    }

    // save writes the script when it has unsaved changes. It resolves to
    // true when the file now holds the tab's text, false when it does not
    // (a conflict, a failure) — Run stops on false. One PUT at a time per
    // script: a second waits for the first's revision.
    async function save(name) {
      const e = files.get(name);
      if (!e) return false;
      while (e.saving) await e.saving.catch(() => {});
      const text = textOf(e);
      if (!e.missing && text === e.saved) return true;
      const p = put(e, text);
      e.saving = p;
      try { return await p; } finally { if (e.saving === p) e.saving = null; }
    }

    async function put(e, text) {
      let r;
      try {
        r = await api("PUT", path(e.name) + host.winQuery(), { text, base: e.rev });
      } catch (err) {
        if (err.status === 409) {
          // base "" (a new or deleted file) over one made elsewhere meanwhile
          try { const f = await api("GET", path(e.name)); adopt(e, f.text, f.rev); } catch (_) { /* reported below */ }
          log("warn", e.name + " was created elsewhere meanwhile — this tab now shows that version; " +
            "Ctrl+Z brings back yours, and Ctrl+S then saves it over");
        } else {
          log("err", "could not save " + e.name + ": " + err.message);
        }
        return false;
      }
      if (r.conflict && !r.rev) {
        Object.assign(e, { rev: "", missing: true });
        changed(e, true);
        log("warn", e.name + " was deleted or moved to the trash since it was opened — this tab keeps its text; Ctrl+S saves it again");
        return false;
      }
      if (r.conflict) {
        adopt(e, r.text, r.rev);
        log("warn", e.name + " was changed elsewhere (another window, the TUI or an editor) — this tab now shows that version; " +
          "Ctrl+Z brings back yours, and Ctrl+S then saves it over");
        return false;
      }
      Object.assign(e, { rev: r.rev, saved: text, text, missing: false });
      storeDraft(e);
      changed(e, true);
      if (isPlug(e.name) && r.load) sayLoad(e.name, r.load);
      return true;
    }

    // sayLoad logs what the loader made of a plugin file just saved: its
    // plugin, now in the pipeline palette, or why it is not.
    function sayLoad(name, l) {
      if (l.error) log("err", fileOf(name) + " did not load" + (l.plugin ? " as " + l.plugin : "") + ": " + l.error);
      else log("ok", fileOf(name) + " loaded: " + (KIND_GLYPH[l.kind] || "") + " " + l.plugin + " (" + l.kind +
        ") — in the pipeline palette under Yours");
    }

    // applyPlugin is Run (Ctrl+Enter) in a plugin file's tab: there is
    // nothing to run, so it saves, checks out loud and says what the
    // loader made of the file — also when nothing needed saving.
    async function applyPlugin(name) {
      const e = files.get(name);
      if (!e) return;
      const was = e.rev;
      if (!(await save(name))) return;
      await check(name, true);
      if (e.rev !== was) return; // the save's answer said it already
      let got;
      try { got = await api("GET", "/api/v1/plugin-files"); } catch (err) { log("err", err.message); return; }
      const f = got.plugins.find((x) => x.name === fileOf(name));
      if (f) sayLoad(name, { plugin: f.plugin, kind: f.kind, error: f.error });
    }

    // edited is the host's call on every change to a script's document:
    // the draft is kept (debounced), the check is scheduled, and the mark
    // follows.
    function edited(name) {
      const e = files.get(name);
      if (!e) return;
      e.text = textOf(e);
      clearTimeout(e.draftTimer);
      e.draftTimer = setTimeout(() => storeDraft(e), 400);
      clearTimeout(e.checkTimer);
      e.checkTimer = setTimeout(() => check(name, false), 600);
      changed(e);
    }

    // storeDraft writes e's draft now — or drops it once there is nothing
    // unsaved to keep.
    function storeDraft(e) {
      clearTimeout(e.draftTimer);
      e.draftTimer = 0;
      const text = textOf(e);
      // at: when it was typed, so of two orphans the newer is adopted
      writeDraft(e.name, isDirty(e) ? { base: e.rev, text, at: Date.now() } : null);
    }

    // flush writes every pending draft: the page is going away.
    function flush() {
      for (const e of files.values()) if (e.draftTimer) storeDraft(e);
    }

    // forget is for a script no tab shows any more. discard also throws
    // its unsaved text away (the close prompt's Discard).
    function forget(name, discard) {
      const e = files.get(name);
      if (!e) return;
      clearTimeout(e.checkTimer);
      if (discard) { clearTimeout(e.draftTimer); writeDraft(name, null); } else if (e.draftTimer) storeDraft(e);
      files.delete(name);
    }

    // ── the check ──────────────────────────────────────────────────────
    // check posts the tab's text — saved or not — to script-check and puts
    // the findings on the document as markers. loud (✓ Check) also lists
    // them in the log, or says the script is clean. It resolves to the
    // diags, or null when the answer was stale or failed.
    async function check(name, loud) {
      const e = files.get(name);
      if (!e) return null;
      clearTimeout(e.checkTimer);
      const text = textOf(e), seq = (e.checkSeq = (e.checkSeq || 0) + 1);
      let r;
      try {
        r = await api("POST", isPlug(name) ? "/api/v1/plugin-check" : "/api/v1/script-check", { name, text });
      } catch (err) {
        if (loud) log("err", "check " + name + ": " + err.message);
        return null;
      }
      if (seq !== e.checkSeq || !files.has(name)) return null; // typed on since; a newer check is coming
      e.diags = r.diags || [];
      dbc.editor.setMarkers(key(name), e.diags);
      host.checked(name, e.diags);
      if (loud) {
        if (!e.diags.length) {
          log("ok", isPlug(name) ? fileOf(name) + ": no problems — it compiles, and its funcs fit its kind" :
            name + ": no problems — it compiles, and Run has the right signature");
        }
        for (const d of e.diags) log(d.severity === "error" ? "err" : "warn", name + ":" + d.line + ":" + d.col + ": " + d.msg);
      }
      return e.diags;
    }

    // ── what other windows did ─────────────────────────────────────────
    // onEvent is the window-level "scripts" event (web/scripts.go
    // scriptsEvent): a save, rename, trash or restore, in any window.
    // Renames and trashes are applied here for this window's own too —
    // they are idempotent, and the event may beat the response.
    async function onEvent(d) {
      if (browser) browser.refresh();
      const e = files.get(d.name);
      if (d.op === "saved") {
        if (d.win === host.win() || !e || e.saving || isDirty(e) || d.rev === e.rev) return;
        try {
          const f = await api("GET", path(d.name));
          if (!e.saving && !isDirty(e)) adopt(e, f.text, f.rev);
        } catch (_) { /* the next save meets the conflict instead */ }
      } else if (d.op === "renamed") {
        renamed(d.name, d.to);
      } else if (d.op === "trashed") {
        if (!e || e.missing) return;
        Object.assign(e, { rev: "", missing: true });
        changed(e, true);
        storeDraft(e);
        log("warn", d.name + " was moved to the trash — its tab keeps the text; Ctrl+S saves it again, " +
          "or restore it from Ctrl+O → Trash");
      } else if (d.op === "restored") {
        if (!e || !e.missing) return;
        // back on disk: if it says what the tab says, the tab is saved
        // again; otherwise the next save meets the 409 and loads it
        try {
          const f = await api("GET", path(d.name));
          if (f.text === textOf(e)) { Object.assign(e, { rev: f.rev, saved: f.text, missing: false }); storeDraft(e); changed(e, true); }
        } catch (_) { /* left unsaved */ }
      }
    }

    // renamed moves a script's entry, document and draft to its new name,
    // and the host's tabs with them.
    function renamed(from, to) {
      const e = files.get(from);
      if (e && !files.has(to)) {
        files.delete(from);
        e.name = to;
        files.set(to, e);
        dbc.editor.renameDoc(key(from), key(to));
      }
      // This window's draft follows the script, and so do closed windows'
      // (orphans have nobody else to move them, and left under the old
      // name a later script of that name would adopt them). A live
      // window's draft is its own to move: it gets this same event.
      const d = readDraft(from);
      writeDraft(from, null);
      if (d) writeDraft(to, d);
      for (const o of othersDrafts(from)) {
        if (o.live) continue;
        writeKey(o.key, null);
        writeKey(o.key.slice(0, o.key.length - from.length) + to, o.d);
      }
      host.renamed(from, to);
    }

    // ── actions on a script, from the browser or a tab's menu ──────────
    async function list() {
      return api("GET", "/api/v1/scripts");
    }

    // makeScript writes text as a new script, asking for its name
    // (offered: suggest, made free). A name taken meanwhile asks again.
    // Then the script opens in a tab.
    async function makeScript(suggest, text, what) {
      let taken = [];
      try { taken = (await list()).scripts.map((s) => s.name); } catch (_) { /* the server will say */ }
      let name = freeName(suggest, taken), hint = "Letters, digits, '.', '-' and '_', ending in .go.";
      for (;;) {
        name = goName(await ask({ title: what, hint, value: name, ok: "Create" }));
        if (!name) return;
        try {
          await api("PUT", path(name) + host.winQuery(), { text, base: "" });
          break;
        } catch (err) {
          if (err.status !== 409 && err.status !== 400) { log("err", what + ": " + err.message); return; }
          hint = err.message; // taken, or not a script name: say so and ask again
        }
      }
      log("ok", "created " + name + " — Ctrl+S saves, Ctrl+Enter saves and runs");
      await edit(name);
    }

    // makePlugin is makeScript for a plugin file: written to plugins_dir
    // (where the server loads it at once), then opened in a tab.
    async function makePlugin(suggest, text, what) {
      let taken = [];
      try { taken = (await api("GET", "/api/v1/plugin-files")).plugins.map((s) => s.name); } catch (_) { /* the server will say */ }
      let name = freeName(fileOf(suggest), taken), hint = "A file in plugins_dir: letters, digits, '.', '-' and '_', ending in .go. " +
        "Two files may not declare one plugin name, so change Name in the copy before using both.";
      for (;;) {
        name = goName(await ask({ title: what, hint, value: name, ok: "Create" }));
        if (!name) return;
        try {
          await api("PUT", path(PLUG + name) + host.winQuery(), { text, base: "" });
          break;
        } catch (err) {
          if (err.status !== 409 && err.status !== 400) { log("err", what + ": " + err.message); return; }
          hint = err.message;
        }
      }
      log("ok", "created " + name + " in plugins_dir — Ctrl+S saves and loads it; its plugin is in the pipeline palette under Yours");
      await edit(PLUG + name);
    }

    async function copyPluginExample(name) {
      let r;
      try { r = await api("GET", "/api/v1/plugin-examples/" + encodeURIComponent(name)); } catch (err) { log("err", err.message); return; }
      await makePlugin(name, r.text, "Copy the example plugin " + name);
    }

    async function newFrom(tpl) {
      let r;
      try {
        r = await api("GET", "/api/v1/script-templates/" + encodeURIComponent(tpl.name) +
          "?conn=" + encodeURIComponent(host.conn() || ""));
      } catch (err) { log("err", "template " + tpl.name + ": " + err.message); return; }
      await makeScript(tpl.name === "blank" ? "script.go" : tpl.name + ".go", r.text, "New script · " + tpl.title);
    }

    async function duplicateExample(name) {
      let r;
      try { r = await api("GET", "/api/v1/script-examples/" + encodeURIComponent(name)); } catch (err) { log("err", err.message); return; }
      await makeScript(name, r.text, "Copy the example " + name);
    }

    async function duplicate(name) {
      const e = files.get(name);
      let text;
      if (e) text = textOf(e); // the tab's text, unsaved edits and all
      else {
        try { text = (await api("GET", path(name))).text; } catch (err) { log("err", err.message); return; }
      }
      if (isPlug(name)) await makePlugin(name, text, "Duplicate " + fileOf(name));
      else await makeScript(name, text, "Duplicate " + name);
    }

    async function edit(name) {
      try { await load(name); } catch (err) { log("err", "could not open " + name + ": " + err.message); return; }
      host.open(name);
    }

    async function rename(name) {
      const file = fileOf(name);
      const to = goName(await ask({ title: "Rename " + file, value: file, ok: "Rename",
        hint: "Letters, digits, '.', '-' and '_', ending in .go. An open tab follows it." }));
      if (!to || to === file) return;
      try {
        await api("POST", path(name) + "/rename" + host.winQuery(), { to });
      } catch (err) { log("err", "rename " + file + ": " + err.message); return; }
      renamed(name, isPlug(name) ? PLUG + to : to);
      log("ok", "renamed " + file + " to " + to);
    }

    // trash moves a script into .trash (restorable from the browser), no
    // question asked: nothing is lost, and an open tab keeps its text.
    async function trash(name) {
      try {
        await api("DELETE", path(name) + host.winQuery());
      } catch (err) { log("err", "trash " + name + ": " + err.message); return; }
      log("info", "moved " + fileOf(name) + " to the trash — Ctrl+O → Trash restores it");
    }

    // restorePlugin is restore for a trashed plugin file.
    async function restorePlugin(t) {
      let to = "";
      for (;;) {
        try {
          const r = await api("POST", "/api/v1/plugin-trash/" + encodeURIComponent(t.id) + "/restore" + host.winQuery(), { to });
          log("ok", "restored " + fileOf(r.name) + " to plugins_dir");
          return;
        } catch (err) {
          if (err.status !== 409) { log("err", "restore " + t.name + ": " + err.message); return; }
          to = goName(await ask({ title: "Restore " + t.name + " as…", value: freeName(t.name, [t.name]), ok: "Restore",
            hint: "A plugin file named " + (to || t.name) + " exists — restore this one under another name." }));
          if (!to) return;
        }
      }
    }

    async function restore(t) {
      let to = "";
      for (;;) {
        try {
          const r = await api("POST", "/api/v1/script-trash/" + encodeURIComponent(t.id) + "/restore" + host.winQuery(), { to });
          log("ok", "restored " + r.name);
          return;
        } catch (err) {
          if (err.status !== 409) { log("err", "restore " + t.name + ": " + err.message); return; }
          to = goName(await ask({ title: "Restore " + t.name + " as…", value: freeName(t.name, [t.name]), ok: "Restore",
            hint: "A script named " + (to || t.name) + " exists — restore this one under another name." }));
          if (!to) return;
        }
      }
    }

    async function copyPath(name) {
      let dir = "";
      try { dir = (isPlug(name) ? await api("GET", "/api/v1/plugin-files") : await list()).dir; } catch (err) { log("err", err.message); return; }
      dbc.clip.copyText(dir.replace(/\/$/, "") + "/" + fileOf(name), isPlug(name) ? "the plugin file's path" : "the script's path");
    }

    // scriptItems is the menu of things to do with a script (a tab's
    // right-click, the browser's ⋯).
    function scriptItems(name) {
      if (isPlug(name)) {
        return [
          { label: "Open in a tab", act: () => edit(name) },
          { label: "Save and load (Ctrl+Enter)", act: async () => { await edit(name); applyPlugin(name); } },
          { label: "Duplicate…", act: () => duplicate(name) },
          { label: "Rename…", key: "F2", act: () => rename(name) },
          { label: "Move to the trash", act: () => trash(name) },
          { label: "Copy path", act: () => copyPath(name) },
        ];
      }
      return [
        { label: "Open in a tab", act: () => edit(name) },
        { label: "Run", act: () => host.run(name) },
        { label: "Duplicate…", act: () => duplicate(name) },
        { label: "Rename…", key: "F2", act: () => rename(name) },
        { label: "Move to the trash", act: () => trash(name) },
        { label: "Copy path", act: () => copyPath(name) },
      ];
    }

    // ── the browser ────────────────────────────────────────────────────
    // Ctrl+O, ▷ Scripts. One list in sections, filtered by name and
    // description as you type:
    //
    //	┌ Scripts · ~/.config/dbc/scripts ─────────────────────────────┐
    //	│ ⌕ filter                                                      │
    //	│ SCRIPTS                                                       │
    //	│ copy_mytable.go  Copy myschema.mytable from ProdDr to dev  2h ▶ ✎ ⋯
    //	│ EXAMPLES · read-only — Enter makes your own copy              │
    //	│ copy_table.go    Copy a Postgres table …                   ⧉  │
    //	│ TRASH (2) ▸                                                   │
    //	├ [+ New ▾] ~/.config/dbc/scripts ⧉     Enter run · ⇧Enter edit │
    //	└───────────────────────────────────────────────────────────────┘
    //
    // An empty scripts dir lists the templates where the scripts would be.
    // The filter keeps the focus, so plain letters type; the commands are
    // chords: Enter runs (Ctrl+O's old muscle memory), Shift+Enter edits,
    // F2 renames, Ctrl/⌘+Delete trashes, Alt+N opens the New menu. On an
    // example, a template or a trashed script, Enter does that row's one
    // thing: copy it, start from it, restore it.
    let browser = null;

    // listAll is the browser's data: the scripts list, and the pipelines
    // and jobs lists beside it (got.p, got.j; empty when one cannot be read
    // — the scripts still show).
    async function listAll() {
      const pipes = host.pipes ? host.pipes() : null;
      const jobs = host.jobs ? host.jobs() : null;
      const [got, p, j, pl] = await Promise.all([list(), pipes ? pipes.list().catch(() => null) : null,
        jobs ? jobs.list().catch(() => null) : null, api("GET", "/api/v1/plugin-files").catch(() => null)]);
      got.p = p || { pipelines: [], examples: [], trash: [] };
      got.j = j || { jobs: [], examples: [], trash: [] };
      got.pl = pl || { plugins: [], examples: [], trash: [] };
      return got;
    }

    async function browse() {
      let got;
      try { got = await listAll(); } catch (err) { log("err", "scripts: " + err.message); return; }
      let rows = [], cur = 0, showTrash = false, seq = 0;
      const input = el("input", { type: "search", class: "hfilter", placeholder: "filter scripts, pipelines, jobs, plugins and examples…",
        "aria-label": "Filter scripts", spellcheck: "false", autocomplete: "off" });
      const ul = el("ul", { class: "hlist slist", role: "listbox" });
      const newBtn = el("button", { type: "button", class: "primary", title: "A new job or pipeline, or a script from a template (Alt+N)" }, "+ New ▾");
      const dirBtn = el("button", { type: "button", class: "linkish", title: "Copy the scripts directory's path" });
      const hint = el("span", "hint", "Enter run (a plugin: edit) · ⇧Enter edit · F2 rename · Ctrl+Del trash · Esc close");

      // build lays out the rows for the filter; a row with a kind can be
      // picked, the rest are section heads
      function build() {
        const q = input.value.trim().toLowerCase();
        const hit = (n, d) => !q || n.toLowerCase().includes(q) || (d || "").toLowerCase().includes(q);
        rows = [{ head: "Scripts" }];
        const mine = got.scripts.filter((s) => hit(s.name, s.desc));
        for (const s of mine) rows.push({ kind: "script", name: s.name, desc: s.desc, when: ago(s.mod) });
        if (!got.scripts.length) {
          rows.push({ note: "none yet in " + (got.short || got.dir) + " — start one from a template:" });
          for (const t of got.templates.filter((t) => hit(t.title, t.desc))) rows.push({ kind: "template", tpl: t, name: t.title, desc: t.desc });
        } else if (!mine.length) {
          rows.push({ note: "no script matches" });
        }
        const ex = got.examples.filter((x) => hit(x.name, x.desc));
        if (ex.length) {
          rows.push({ head: "Examples · read-only — Enter makes your own copy" });
          for (const x of ex) rows.push({ kind: "example", name: x.name, desc: x.desc });
        }
        // pipelines (pipelines.js): the user's, then the built-in ones
        rows.push({ head: "Pipelines · " + (got.p.short || got.p.dir || "pipelines_dir") });
        const pl = got.p.pipelines.filter((x) => hit(x.name, x.desc));
        for (const x of pl) {
          rows.push({ kind: "pipeline", name: x.name, when: ago(x.mod),
            desc: x.desc || (x.fragments ? dbc.plural(x.fragments, "fragment") : "does not parse") });
        }
        if (!got.p.pipelines.length) rows.push({ note: "none yet — copy an example below, or + New ▾ → Pipeline" });
        else if (!pl.length) rows.push({ note: "no pipeline matches" });
        const pex = got.p.examples.filter((x) => hit(x.name, x.desc));
        if (pex.length) {
          rows.push({ head: "Pipeline examples · Enter makes your own copy" });
          for (const x of pex) rows.push({ kind: "pexample", name: x.name, desc: x.desc });
        }
        // jobs (jobs.js): DAGs of those pipelines — the user's, with when
        // the scheduler fires each next, then the built-in ones
        rows.push({ head: "Jobs · " + (got.j.short || got.j.dir || "jobs_dir") });
        const jl = got.j.jobs.filter((x) => hit(x.name, x.desc));
        for (const x of jl) {
          const next = x.next ? "next " + new Date(x.next).toTimeString().slice(0, 5) : "";
          const last = x.last ? (x.last.status === "succeeded" ? "✓" : x.last.status === "running" ? "●" : "✗") + " " + ago(x.last.started) : "";
          rows.push({ kind: "job", name: x.name, when: [last, next].filter(Boolean).join(" · ") || ago(x.mod),
            desc: x.desc || (x.pipelines ? dbc.plural(x.pipelines, "pipeline") : "does not parse") });
        }
        if (!got.j.jobs.length) rows.push({ note: "none yet — copy an example below, or + New ▾ → Job" });
        else if (!jl.length) rows.push({ note: "no job matches" });
        const jex = got.j.examples.filter((x) => hit(x.name, x.desc));
        if (jex.length) {
          rows.push({ head: "Job examples · Enter makes your own copy" });
          for (const x of jex) rows.push({ kind: "jexample", name: x.name, desc: x.desc });
        }
        // plugin files (web/plugins.go): each one kind of pipeline node,
        // with what the loader made of it — its plugin, or why not
        rows.push({ head: "Plugins · " + (got.pl.short || got.pl.dir || "plugins_dir") });
        const pll = got.pl.plugins.filter((x) => hit(x.name, (x.plugin || "") + " " + (x.desc || "") + " " + (x.error || "")));
        for (const x of pll) {
          rows.push({ kind: "plugin", name: x.name, when: ago(x.mod), bad: !!x.error,
            desc: x.error ? "⚠ did not load: " + x.error : (KIND_GLYPH[x.kind] || "") + " " + x.plugin + (x.desc ? " — " + x.desc : "") });
        }
        if (!got.pl.plugins.length) rows.push({ note: "none yet — copy an example below, or + New ▾ → Plugin" });
        else if (!pll.length) rows.push({ note: "no plugin matches" });
        const plex = got.pl.examples.filter((x) => hit(x.name, x.desc));
        if (plex.length) {
          rows.push({ head: "Plugin examples · Enter makes your own copy" });
          for (const x of plex) rows.push({ kind: "plexample", name: x.name, desc: x.desc });
        }
        const trashed = got.trash.map((t) => ({ kind: "trash", t, name: t.name, at: t.trashed }))
          .concat(got.p.trash.map((t) => ({ kind: "ptrash", t, name: t.name, at: t.trashed })))
          .concat(got.j.trash.map((t) => ({ kind: "jtrash", t, name: t.name, at: t.trashed })))
          .concat(got.pl.trash.map((t) => ({ kind: "pltrash", t, name: t.name, at: t.trashed })))
          .sort((a, b) => String(b.at).localeCompare(String(a.at)));
        if (trashed.length) {
          rows.push({ head: "Trash (" + trashed.length + ") " + (showTrash ? "▾" : "▸"), toggle: true });
          if (showTrash) {
            for (const r of trashed.filter((r) => hit(r.name, ""))) {
              const at = ago(r.at);
              rows.push(Object.assign(r, { desc: at === "now" ? "trashed just now" : "trashed " + at + " ago", when: "" }));
            }
          }
        }
        const picks = rows.filter((r) => r.kind).length;
        cur = Math.min(cur, Math.max(0, picks - 1));
      }

      const picks = () => rows.filter((r) => r.kind);
      const current = () => picks()[cur] || null;

      function actions(r) {
        const b = (label, title, act) => {
          const x = el("button", { type: "button", title }, label);
          x.addEventListener("click", (ev) => { ev.stopPropagation(); act(ev); });
          return x;
        };
        if (r.kind === "script") {
          return [b("▶", "Run it (Enter)", () => go(r, "run")), b("✎", "Edit it in a tab (Shift+Enter)", () => go(r, "edit")),
            b("⋯", "More: duplicate, rename, trash, copy path", (ev) => {
              const at = ev.currentTarget.getBoundingClientRect();
              dbc.menu.open(at.left, at.bottom + 2, scriptItems(r.name).map((it) => ({ ...it, act: () => { dbc.modal.close(); it.act(); } })));
            })];
        }
        if (r.kind === "pipeline") {
          return [b("▶", "Open it and run it (Enter)", () => go(r, "run")), b("✎", "Open it on the canvas (Shift+Enter)", () => go(r, "edit")),
            b("⋯", "More: preview, export as Go, duplicate, rename, trash, copy path", (ev) => {
              const at = ev.currentTarget.getBoundingClientRect();
              dbc.menu.open(at.left, at.bottom + 2, host.pipes().items(r.name).map((it) => ({ ...it, act: () => { dbc.modal.close(); it.act(); } })));
            })];
        }
        if (r.kind === "job") {
          return [b("▶", "Open it and run it (Enter)", () => go(r, "run")), b("✎", "Open it on the canvas (Shift+Enter)", () => go(r, "edit")),
            b("⋯", "More: its runs, duplicate, rename, trash, copy path", (ev) => {
              const at = ev.currentTarget.getBoundingClientRect();
              dbc.menu.open(at.left, at.bottom + 2, host.jobs().items(r.name).map((it) => ({ ...it, act: () => { dbc.modal.close(); it.act(); } })));
            })];
        }
        if (r.kind === "plugin") {
          return [b("✎", "Edit it in a tab (Enter)", () => go(r, "edit")),
            b("⋯", "More: save and load, duplicate, rename, trash, copy path", (ev) => {
              const at = ev.currentTarget.getBoundingClientRect();
              dbc.menu.open(at.left, at.bottom + 2, scriptItems(PLUG + r.name).map((it) => ({ ...it, act: () => { dbc.modal.close(); it.act(); } })));
            })];
        }
        if (r.kind === "plexample") return [b("⧉ Copy", "Make an editable copy in plugins_dir, where it is loaded (Enter)", () => go(r, "run"))];
        if (r.kind === "pexample") return [b("⧉ Copy", "Make an editable copy in your pipelines (Enter)", () => go(r, "run"))];
        if (r.kind === "jexample") return [b("⧉ Copy", "Make an editable copy in your jobs (Enter)", () => go(r, "run"))];
        if (r.kind === "example") return [b("⧉ Copy", "Make an editable copy in your scripts (Enter)", () => go(r, "run"))];
        if (r.kind === "template") return [b("+ New", "A new script from this template (Enter)", () => go(r, "run"))];
        return [b("↺ Restore", "Put it back in the scripts directory (Enter)", () => go(r, "run"))];
      }

      function draw() {
        ul.replaceChildren();
        let i = 0;
        for (const r of rows) {
          if (r.head !== undefined) {
            const li = el("li", { class: "shead" + (r.toggle ? " toggle" : "") }, r.head);
            if (r.toggle) li.addEventListener("click", () => { showTrash = !showTrash; build(); draw(); input.focus(); });
            ul.append(li);
            continue;
          }
          if (r.note) { ul.append(el("li", "snote", r.note)); continue; }
          const n = i++;
          const li = el("li", { class: "srow " + r.kind + (r.bad ? " bad" : "") + (n === cur ? " cur" : ""), role: "option", "data-i": String(n),
            "data-name": r.name, title: r.desc || "" },
          el("span", "sname", r.name), el("span", "sdesc", r.desc || ""), el("span", "swhen", r.when || ""),
          el("span", "sact", ...actions(r)));
          ul.append(li);
        }
        const c = ul.querySelector("li.cur");
        if (c) c.scrollIntoView({ block: "nearest" });
      }

      // go does what a row is for: run/copy/start/restore ("run"), or edit
      async function go(r, what) {
        if (!r) return;
        dbc.modal.close();
        if (r.kind === "script") {
          if (what === "edit") await edit(r.name);
          else host.run(r.name);
        } else if (r.kind === "example") {
          await duplicateExample(r.name);
        } else if (r.kind === "template") {
          await newFrom(r.tpl);
        } else if (r.kind === "trash") {
          await restore(r.t);
        } else if (r.kind === "pipeline") {
          const pipes = host.pipes();
          await pipes.edit(r.name);
          if (what !== "edit") pipes.run(r.name, "");
        } else if (r.kind === "pexample") {
          await host.pipes().copyExample(r.name);
        } else if (r.kind === "ptrash") {
          await host.pipes().restore(r.t);
        } else if (r.kind === "job") {
          const jobs = host.jobs();
          await jobs.edit(r.name);
          if (what !== "edit") jobs.run(r.name);
        } else if (r.kind === "jexample") {
          await host.jobs().copyExample(r.name);
        } else if (r.kind === "jtrash") {
          await host.jobs().restore(r.t);
        } else if (r.kind === "plugin") {
          await edit(PLUG + r.name); // a plugin file has nothing to run: Enter edits it
        } else if (r.kind === "plexample") {
          await copyPluginExample(r.name);
        } else if (r.kind === "pltrash") {
          await restorePlugin(r.t);
        }
      }

      function newMenu(x, y) {
        const pipes = host.pipes ? host.pipes() : null, jobs = host.jobs ? host.jobs() : null;
        const head = (jobs ? [{ head: "new job" },
          { label: "Job — pipelines in a DAG, run on a schedule or by hand", act: () => { dbc.modal.close(); jobs.newJob(); } }] : [])
          .concat(pipes ? [{ head: "new pipeline" },
            { label: "Pipeline — a source into a preview, on the canvas", act: () => { dbc.modal.close(); pipes.newPipeline(); } }] : []);
        // a new plugin starts as a copy of the example of its kind
        const plugs = [{ head: "new plugin (a pipeline node in Go) from" }].concat(got.pl.examples.map((x) => ({
          label: "Plugin · " + x.name + " — " + x.desc, act: () => { dbc.modal.close(); copyPluginExample(x.name); },
        })));
        dbc.menu.open(x, y, head.concat(got.pl.examples.length ? plugs : [], [{ head: "new script from" }], got.templates.map((t) => ({
          label: t.title, act: () => { dbc.modal.close(); newFrom(t); },
        }))));
      }

      input.addEventListener("input", () => { cur = 0; build(); draw(); });
      ul.addEventListener("click", (ev) => {
        const li = ev.target.closest("li[data-i]");
        if (!li) return;
        cur = Number(li.dataset.i);
        draw();
      });
      ul.addEventListener("dblclick", (ev) => {
        const li = ev.target.closest("li[data-i]");
        if (li && !ev.target.closest(".sact")) go(picks()[Number(li.dataset.i)], "edit");
      });
      newBtn.addEventListener("click", () => {
        const at = newBtn.getBoundingClientRect();
        newMenu(at.left, at.bottom + 2);
      });
      dirBtn.textContent = (got.short || got.dir) + " ⧉";
      dirBtn.addEventListener("click", () => dbc.clip.copyText(got.dir, "the scripts directory's path"));

      build();
      draw();
      // the title names the resolved directory: scripts_dir used to be
      // cwd-relative, and a picker that never said where it looked hid
      // dbc.app looking in ~/scripts
      dbc.modal.open({
        title: "Scripts · " + (got.short || got.dir), cls: "wide scripts", focus: input,
        body: el("div", "history", input, ul),
        foot: el("div", "mfoot", newBtn, dirBtn, hint),
        onKey: (e) => {
          const n = picks().length;
          if (e.key === "ArrowDown") { cur = Math.min(cur + 1, n - 1); draw(); return true; }
          if (e.key === "ArrowUp") { cur = Math.max(cur - 1, 0); draw(); return true; }
          if (e.key === "Enter") { go(current(), e.shiftKey ? "edit" : "run"); return true; }
          const r = current();
          if (e.key === "F2" && r && r.kind === "script") { dbc.modal.close(); rename(r.name); return true; }
          if (e.key === "F2" && r && r.kind === "pipeline") { dbc.modal.close(); host.pipes().rename(r.name); return true; }
          if (e.key === "F2" && r && r.kind === "job") { dbc.modal.close(); host.jobs().rename(r.name); return true; }
          if (e.key === "F2" && r && r.kind === "plugin") { dbc.modal.close(); rename(PLUG + r.name); return true; }
          if ((e.key === "Delete" || e.key === "Backspace") && (e.ctrlKey || e.metaKey) && r && r.kind === "script") {
            trash(r.name);
            return true;
          }
          if ((e.key === "Delete" || e.key === "Backspace") && (e.ctrlKey || e.metaKey) && r && r.kind === "pipeline") {
            host.pipes().trash(r.name);
            return true;
          }
          if ((e.key === "Delete" || e.key === "Backspace") && (e.ctrlKey || e.metaKey) && r && r.kind === "job") {
            host.jobs().trash(r.name);
            return true;
          }
          if ((e.key === "Delete" || e.key === "Backspace") && (e.ctrlKey || e.metaKey) && r && r.kind === "plugin") {
            trash(PLUG + r.name);
            return true;
          }
          if (e.altKey && e.code === "KeyN") {
            const at = newBtn.getBoundingClientRect();
            newMenu(at.left, at.bottom + 2);
            return true;
          }
          return false;
        },
        onClose: () => { browser = null; host.focus(); },
      });
      // a change in any window (the "scripts" event) re-lists, keeping the
      // highlighted row by name
      browser = {
        async refresh() {
          const n = ++seq;
          let next;
          try { next = await listAll(); } catch (_) { return; }
          if (n !== seq || !browser) return;
          const was = current();
          got = next;
          build();
          const i = was ? picks().findIndex((r) => r.kind === was.kind && r.name === was.name) : -1;
          if (i >= 0) cur = i;
          draw();
        },
      };
    }

    // ── Go completion and hover, from the sdb API ──────────────────────
    // GET /api/v1/script-api is package sdb as data (sdb/sdbapi): S's
    // methods, the package funcs, and the types a script meets (Result,
    // CopyOpts, …) with fields and methods, each with its signature and doc
    // comment. Without gopls there are no real types, so the providers read
    // the text around the caret:
    //
    //	s.▮            S's methods (s: Run's parameter, whatever it is named)
    //	sdb.▮          the package's funcs and types
    //	res.▮          res's type's fields and methods, where res is
    //	               assigned from a call whose first result is a known
    //	               type (res, err := s.Query(…)), or declared as one
    //	               (var o sdb.CopyOpts, o := sdb.CopyOpts{…})
    //	sdb.CopyOpts{▮ the type's fields
    //	s.Query("▮     the connection names, inside a connection argument
    //	               (sdbapi's connArgs: Copy's src and dst, Query's conn …)
    let sdbAPI = null;
    const apiOnce = () => sdbAPI || (sdbAPI = api("GET", "/api/v1/script-api").catch((err) => { sdbAPI = null; throw err; }));

    function register(monaco) {
      // a script tab shown before Monaco loaded was checked into a
      // textarea, which cannot draw markers: check it again now
      for (const name of files.keys()) check(name, false);
      const K = monaco.languages.CompletionItemKind;
      monaco.languages.registerCompletionItemProvider("go", {
        triggerCharacters: [".", "\""],
        async provideCompletionItems(model, pos) {
          let A;
          try { A = await apiOnce(); } catch (_) { return { suggestions: [] }; }
          const line = model.getLineContent(pos.lineNumber).slice(0, pos.column - 1);
          const before = model.getValueInRange(new monaco.Range(1, 1, pos.lineNumber, pos.column));
          const recv = receiver(model.getValue());
          const S = typeOf(A, "S");
          const rangeBack = (n) => new monaco.Range(pos.lineNumber, pos.column - n, pos.lineNumber, pos.column);

          // inside a string: connection names where the argument is one,
          // nothing otherwise
          const ca = /\b([A-Za-z_]\w*)\.([A-Za-z_]\w*)\(\s*("(?:[^"\\]|\\.)*"\s*,\s*)?"([^"]*)$/.exec(line);
          if (ca && ca[1] === recv && S) {
            const m = (S.methods || []).find((f) => f.name === ca[2]);
            if (m && (m.connArgs || []).includes(ca[3] ? 1 : 0)) {
              return { suggestions: host.connNames().map((c) => ({ label: c, kind: K.Value, insertText: c,
                detail: "connection", range: rangeBack(ca[4].length) })) };
            }
          }
          if (inString(line)) return { suggestions: [] };

          const mem = /([A-Za-z_]\w*)\.(\w*)$/.exec(line);
          if (mem) {
            const range = rangeBack(mem[2].length);
            if (mem[1] === "sdb") {
              return { suggestions: (A.funcs || []).map((f) => funcItem(monaco, f, K.Function, range))
                .concat((A.types || []).map((t) => ({ label: t.name, kind: K.Struct, insertText: t.name, range,
                  detail: t.of ? "= " + t.of : t.kind, documentation: t.doc ? { value: t.doc } : undefined }))) };
            }
            const t = mem[1] === recv ? S : typeOf(A, varType(A, model.getValue(), mem[1], recv));
            if (!t) return { suggestions: [] };
            return { suggestions: (t.fields || []).map((f) => ({ label: f.name, kind: K.Field, insertText: f.name, range,
              detail: f.type, documentation: f.doc ? { value: f.doc } : undefined }))
              .concat((t.methods || []).map((f) => funcItem(monaco, f, K.Method, range))) };
          }

          // a composite literal's field names: sdb.CopyOpts{To: "x", ▮
          const lit = /sdb\.([A-Za-z_]\w*)\{([^{}]*)$/.exec(before);
          if (lit && /(^|[,{\n])\s*(\w*)$/.test(lit[2])) {
            const t = typeOf(A, lit[1]);
            const word = /(\w*)$/.exec(line)[1];
            if (t && t.fields) {
              return { suggestions: t.fields.map((f) => ({ label: f.name, kind: K.Field, insertText: f.name + ": ",
                detail: f.type, documentation: f.doc ? { value: f.doc } : undefined, range: rangeBack(word.length) })) };
            }
          }
          return { suggestions: [] };
        },
      });

      // go to definition and usages (see the top of the file): one
      // request answers both, so each provider asks for the symbol and
      // takes the half it needs. A failed request is no answer — Monaco
      // then says "no definition found" — and the status bar reports a
      // lost server on its own.
      const symbolAt = async (model, pos) => {
        try {
          const s = await api("POST", "/api/v1/script-symbol", { text: model.getValue(), caret: model.getOffsetAt(pos) });
          return s && s.kind ? s : null;
        } catch (_) {
          return null;
        }
      };
      const rangeOf = (model, sp) => {
        const a = model.getPositionAt(sp.from), b = model.getPositionAt(sp.to);
        return new monaco.Range(a.lineNumber, a.column, b.lineNumber, b.column);
      };
      monaco.languages.registerDefinitionProvider("go", {
        async provideDefinition(model, pos) {
          const s = await symbolAt(model, pos);
          return s && s.def ? { uri: model.uri, range: rangeOf(model, s.def) } : null;
        },
      });
      monaco.languages.registerReferenceProvider("go", {
        async provideReferences(model, pos, ctx) {
          const s = await symbolAt(model, pos);
          if (!s) return [];
          return s.uses
            .filter((u) => ctx.includeDeclaration || !s.def || u.from !== s.def.from)
            .map((u) => ({ uri: model.uri, range: rangeOf(model, u) }));
        },
      });
      // F2, as editor.js registerSymbols does it for SQL: the symbol
      // request says where the box opens and what it holds, or why it does
      // not open (s.fixed); the edits come from the server, which refuses
      // a taken name. versionId makes Monaco drop the edits if the text
      // changed while the request was out, rather than apply offsets that
      // no longer point at the name.
      monaco.languages.registerRenameProvider("go", {
        async resolveRenameLocation(model, pos) {
          const s = await symbolAt(model, pos);
          const here = new monaco.Range(pos.lineNumber, pos.column, pos.lineNumber, pos.column);
          if (!s) return { range: here, text: "", rejectReason: "Nothing to rename here: rename works on a name the script declares." };
          if (s.fixed) return { range: here, text: "", rejectReason: s.fixed };
          return { range: rangeOf(model, s.at), text: s.name };
        },
        async provideRenameEdits(model, pos, newName) {
          const versionId = model.getVersionId();
          let r;
          try {
            r = await api("POST", "/api/v1/script-symbol-rename", { text: model.getValue(), caret: model.getOffsetAt(pos), name: newName });
          } catch (err) {
            return { edits: [], rejectReason: err.message };
          }
          return {
            edits: (r.edits || []).map((e) => ({
              resource: model.uri, versionId, textEdit: { range: rangeOf(model, e), text: e.text },
            })),
          };
        },
      });

      monaco.languages.registerHoverProvider("go", {
        async provideHover(model, pos) {
          const w = model.getWordAtPosition(pos);
          if (!w) return null;
          let A;
          try { A = await apiOnce(); } catch (_) { return null; }
          const line = model.getLineContent(pos.lineNumber);
          const base = /([A-Za-z_]\w*)\.$/.exec(line.slice(0, w.startColumn - 1));
          if (!base) return null;
          const recv = receiver(model.getValue());
          let doc = null;
          if (base[1] === "sdb") {
            const f = (A.funcs || []).find((x) => x.name === w.word);
            const t = typeOf(A, w.word);
            if (f) doc = [f.sig, f.doc];
            else if (t) doc = ["type " + t.name + (t.of ? " = " + t.of : " " + t.kind), t.doc];
          } else {
            const t = base[1] === recv ? typeOf(A, "S") : typeOf(A, varType(A, model.getValue(), base[1], recv));
            const m = t && (t.methods || []).find((x) => x.name === w.word);
            const f = t && (t.fields || []).find((x) => x.name === w.word);
            if (m) doc = [m.sig, m.doc];
            else if (f) doc = [f.name + " " + f.type, f.doc];
          }
          if (!doc) return null;
          return {
            range: new monaco.Range(pos.lineNumber, w.startColumn, pos.lineNumber, w.endColumn),
            contents: [{ value: "```go\n" + doc[0] + "\n```" }].concat(doc[1] ? [{ value: doc[1] }] : []),
          };
        },
      });
    }

    // funcItem is a method or func as a completion: Name($0) with the
    // caret between the parentheses when it takes arguments.
    function funcItem(monaco, f, kind, range) {
      const takes = (f.params || []).length > 0;
      return {
        label: f.name, kind, range, detail: f.sig, documentation: f.doc ? { value: f.doc } : undefined,
        insertText: f.name + (takes ? "($0)" : "()"),
        insertTextRules: takes ? monaco.languages.CompletionItemInsertTextRule.InsertAsSnippet : undefined,
      };
    }

    return {
      key, load, save, edited, check, flush, forget, onEvent, browse, rename, duplicate, trash, copyPath, register,
      entry: (name) => files.get(name) || null,
      dirty: (name) => isDirty(files.get(name)),
      diags: (name) => (files.get(name) || {}).diags || [],
      items: scriptItems,
      // make writes text as a new script (asking its name) and opens it in
      // a tab: a pipeline's "Export as Go" (pipelines.js) lands this way
      make: makeScript,
      // refreshBrowser re-lists the open browser (a "pipelines" or
      // "plugins" event: its Pipelines and Plugins sections are drawn from
      // the same list)
      refreshBrowser: () => { if (browser) browser.refresh(); },
      // a plugin file's tab ("plugin:<file>"): what it is, and its Run
      isPlugin: isPlug, applyPlugin,
    };
  }

  // receiver is the name Run gives its *sdb.S, "s" when it cannot be read.
  function receiver(text) {
    const m = /func\s+Run\s*\(\s*([A-Za-z_]\w*)\s+\*\s*sdb\.S\s*\)/.exec(text);
    return m ? m[1] : "s";
  }

  const typeOf = (A, name) => (name ? (A.types || []).find((t) => t.name === name) || null : null);

  // inString: the line, up to the caret, ends inside a "…" or `…` literal
  // (or a // comment) — counted naively, which is right for the one-line
  // strings scripts mostly have.
  function inString(line) {
    let q = "";
    for (let i = 0; i < line.length; i++) {
      const c = line[i];
      if (q) {
        if (c === "\\" && q === "\"") i++;
        else if (c === q) q = "";
      } else if (c === "\"" || c === "`") q = c;
      else if (c === "/" && line[i + 1] === "/") return true;
    }
    return !!q;
  }

  // varType guesses the sdb type a variable holds from how it is made:
  //   v, err := s.Query(…)    the first result of S.Query's signature
  //   v := sdb.CopyOpts{…}    v := &sdb.CopyOpts{…}    var v sdb.CopyOpts
  // The last such line before the end wins; anything else is unknown ("").
  function varType(A, text, v, recv) {
    const id = v.replace(/[^\w]/g, "");
    if (!id) return "";
    let found = "";
    const call = new RegExp("\\b" + id + "\\s*(?:,\\s*\\w+\\s*)?:?=\\s*([A-Za-z_]\\w*)\\.([A-Za-z_]\\w*)\\(", "g");
    for (let m; (m = call.exec(text));) {
      const owner = m[1] === recv ? typeOf(A, "S") : null;
      const f = owner ? (owner.methods || []).find((x) => x.name === m[2])
        : m[1] === "sdb" ? (A.funcs || []).find((x) => x.name === m[2]) : null;
      if (f) found = firstResult(f.sig);
    }
    const decl = new RegExp("\\b" + id + "\\s*:=\\s*&?sdb\\.([A-Za-z_]\\w*)\\s*\\{|\\bvar\\s+" + id + "\\s+\\*?sdb\\.([A-Za-z_]\\w*)", "g");
    for (let m; (m = decl.exec(text));) found = m[1] || m[2];
    return found;
  }

  // firstResult is a signature's first result type, bare: "*Result" and
  // "(*Result, error)" both give "Result"; a slice or map gives "".
  function firstResult(sig) {
    // skip the receiver and the parameter list, by parentheses
    let i = sig.indexOf("(");
    if (sig.startsWith("func (")) i = sig.indexOf("(", sig.indexOf(")") + 1);
    let depth = 0, j = i;
    for (; j < sig.length; j++) {
      if (sig[j] === "(") depth++;
      else if (sig[j] === ")" && --depth === 0) break;
    }
    let rest = sig.slice(j + 1).trim();
    if (rest.startsWith("(")) rest = rest.slice(1).split(",")[0];
    rest = rest.trim().replace(/^\*/, "").replace(/^sdb\./, "");
    return /^[A-Za-z_]\w*$/.test(rest) ? rest : "";
  }

  dbc.scripts = { create, newOwner, firstResult, varType, receiver, inString, ask, ago };
})();
