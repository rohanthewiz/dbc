// dbc web — unsaved drafts of the files the page edits: scripts (and
// plugin files) in script tabs, pipelines and jobs in canvas tabs. Each
// kit keeps its unsaved text in this browser's localStorage under the
// file's name and the window that typed it, with the revision it was
// typed against:
//
//	<prefix><owner>:<name>  = {base, text, at}     dbc.script.draft.,
//	                                                dbc.pipe.draft., dbc.job.draft.
//	dbc.draftSeen.<owner>   = ms of the owner's last heartbeat
//
// Opening the file again: a draft whose base is the file's revision is put
// back as it was (unsaved). One whose base is older — the file moved on
// meanwhile, in the TUI or vim — is put back too, but keeps its old base,
// so the first save meets the conflict rather than silently writing over
// the newer file. localStorage, not the saved tab: the draft belongs to the
// file, wherever it is opened next, and a browser that refuses storage (a
// private window) only loses the safety net, not the editing.
//
// WHY PER WINDOW. One key per name meant two windows with the same file
// open shared it, and the last to type took it: a reload of the other
// window then came back with the wrong window's text, and its own unsaved
// edits were gone. So each browser tab is a draft OWNER (an id in its
// sessionStorage — which a reload keeps, and which a duplicated tab copies,
// so app.js gives a copy a new one: newOwner) and writes only its own key.
// "Wherever it is opened next" still holds, through orphans:
//
//	open a file ─► my own draft? ─yes─► use it
//	                    │no
//	                    ▼
//	   drafts of owners whose heartbeat stopped (closed windows), and the
//	   one-key draft an older page wrote (<prefix><name>) ─► the newest is
//	   adopted: moved under my key, the rest dropped (logged) — a closed
//	   window's draft is picked up by the next window to open the file
//	                    │none
//	                    ▼
//	   a LIVE window's draft is left alone: that window is still editing
//	   it, so this one shows the saved file and says so
//
// An owner beats every 30 s; a stopped beat is an orphan after 5 minutes
// (a hidden tab's timers can be throttled to once a minute). Closing the
// tab (pagehide) shortens that to 15 s rather than zero, since a reload
// fires pagehide too and comes straight back as the same owner.
//
// One owner and one heartbeat serve every kit: they are the window's, not
// the file kind's. Script tabs had this scheme first (N-135); pipeline and
// job tabs shared one key per name until it moved here (N-177).
(function () {
  "use strict";

  const dbc = window.dbc;

  // KITS are the kinds of file with drafts, by key prefix. A name holds no
  // ":" (scripts', pipelines' and jobs' names are letters, digits, '.',
  // '-' and '_'; a plugin file's "plugin:<file>" has one, after the owner's,
  // which the first ":" still splits off), so an owner's key and an older
  // page's one-key draft never collide.
  const KITS = ["dbc.script.draft.", "dbc.pipe.draft.", "dbc.job.draft."];
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

  // kit is one kind of file's drafts, under prefix (one of KITS). log is
  // where adopt says it dropped older orphans (the page's log).
  function kit(prefix, log) {
    const ownKey = (name) => prefix + owner() + ":" + name;
    const read = (name) => readKey(ownKey(name));
    const write = (name, d) => writeKey(ownKey(name), d);

    // others lists the drafts of name that are not this window's:
    // {key, d, live} — an older page's one-key draft counts as an orphan.
    function others(name) {
      const me = owner(), out = [];
      for (const k of storageKeys()) {
        if (!k.startsWith(prefix)) continue;
        const rest = k.slice(prefix.length), i = rest.indexOf(":");
        const id = i < 0 ? "" : rest.slice(0, i), n = i < 0 ? rest : rest.slice(i + 1);
        if (n !== name || id === me) continue;
        const d = readKey(k);
        if (d) out.push({ key: k, d, live: id !== "" && live(id) });
      }
      return out;
    }

    // adopt is the draft to put back when name opens: this window's own,
    // else the newest orphan's — moved under this window's key, the other
    // orphans dropped (see DRAFTS). liveN is how many live windows hold a
    // draft of name of their own (left alone), for the caller to say so.
    function adopt(name) {
      const mine = read(name);
      if (mine) return { d: mine, liveN: 0 };
      const all = others(name);
      const orphans = all.filter((o) => !o.live).sort((a, b) => (b.d.at || 0) - (a.d.at || 0));
      const liveN = all.length - orphans.length;
      if (!orphans.length) return { d: null, liveN };
      const d = orphans[0].d;
      write(name, d);
      for (const o of orphans) writeKey(o.key, null);
      if (orphans.length > 1 && log) {
        const n = orphans.length - 1;
        log("warn", name + ": " + n + " older unsaved draft" + (n > 1 ? "s" : "") +
          " from closed windows " + (n > 1 ? "were" : "was") + " dropped for the newest one");
      }
      return { d, liveN };
    }

    // rename moves this window's draft of from to to, and closed windows'
    // too (orphans have nobody else to move them, and left under the old
    // name a later file of that name would adopt them). A live window's
    // draft is its own to move: it hears of the rename itself.
    function rename(from, to) {
      const d = read(from);
      write(from, null);
      if (d) write(to, d);
      for (const o of others(from)) {
        if (o.live) continue;
        writeKey(o.key, null);
        writeKey(o.key.slice(0, o.key.length - from.length) + to, o.d);
      }
    }

    return { read, write, adopt, rename };
  }

  // the heartbeat, and the forgetting of owners that are gone and left no
  // draft of any kit behind (a window closed with everything saved)
  beat();
  setInterval(beat, BEAT);
  document.addEventListener("visibilitychange", () => { if (!document.hidden) beat(); });
  window.addEventListener("pagehide", () => beat(Date.now() - ORPHAN + CLOSING));
  (function forgetGone() {
    const keys = storageKeys();
    for (const k of keys) {
      if (!k.startsWith(SEEN)) continue;
      const id = k.slice(SEEN.length);
      if (!live(id) && !keys.some((x) => KITS.some((p) => x.startsWith(p + id + ":")))) writeKey(k, null);
    }
  })();

  dbc.drafts = { kit, newOwner, owner, KITS };
})();
