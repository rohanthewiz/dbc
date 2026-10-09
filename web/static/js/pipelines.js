// dbc web — pipeline tabs: a pipeline drawn and edited as a canvas — a
// palette of plugins, a lane per fragment with its nodes as cards wired
// port to port, an inspector generated from each plugin's fields — the
// file it edits, the check that marks what is wrong as you go, and the
// previews and runs the server's engine does for it.
//
// app.js owns the tabs; this module owns what a pipeline tab edits and
// draws. app.js makes it with dbc.pipelines.create(host) and calls back on
// every event, save and tab switch, as it does scripts.js.
//
//	┌ #pipe ─────────────────────────────────────────────────────────────────┐
//	│ ⛓ clean-and-load.json ●  · 1 error    rows [50] ◎ Preview  ⊡ Fit  { } JSON  ⇪ Go │
//	├ palette ─┬ canvas (stage.js: drag the background, wheel, Ctrl+wheel) ─┬ inspector ─┤
//	│ ⌕ filter │ ✓ clean · batch 500                      ◎ ▶ ⋯           │ sql.write  │
//	│ SOURCES  │ ┌────────┐   ┌────────┐   ┌──────────┐                     │ dst        │
//	│ ⇥ sql.read│ │⇥ src  ●┼──►●ƒ tidy ●┼─┬►●⇤ dst     │                     │ conn  [ ]  │
//	│ …        │ └────────┘   └────────┘ │ └──────────┘                     │ table [ ]  │
//	│ SINKS    │                          └►●⇤ peek     │                     │ …          │
//	│ ⇤ preview│ ○ stamp                                  ◎ ▶ ⋯           │            │
//	│ …        │ ┌──────────────┐                                            │            │
//	│          │ │▸ log  INSERT…│                                            │            │
//	│          │ + fragment — or drop a plugin here                         │            │
//	└──────────┴─────────────────────────────────────────────────────────────┴────────────┘
//	  the grid below shows a preview's rows; the log, the run's lines
//
// THE MODEL is the spec itself (pipeline.Spec as JSON): every canvas edit
// changes the parsed spec and writes its text again in the shape the Go
// side writes (Spec.JSON: two-space indent, map keys sorted, scalar arrays
// on one line), so a file saved from the canvas diffs like one saved by
// `dbc` — and the JSON view (the editor, toggled in place of the canvas)
// edits the same text. Positions are the spec's fragment "ui"; a node
// without one is placed by depth from its source and saved once moved.
//
// THE FILE is a pipeline in pipelines_dir, read and written by name
// (web/pipelines.go), and — as a script's — saved only when asked: Ctrl+S,
// ⤓ Save, or ▶ Run, which saves first, because what runs is the file, as
// `dbc pipeline run` and cron would run it. A save names the revision it
// was made from; a file changed since is not overwritten — the dialog
// offers to keep this version (writing it over) or to load the file's.
// Unsaved text survives a reload in this browser's localStorage
// (dbc.pipe.draft.<name>), until a save or a discard drops it.
//
// THE CHECK: ~500 ms after an edit, the text goes to POST
// /api/v1/pipeline-check (pipeline.Check: plugins, fields, ${…}
// references, connections, the fragment's shape; nothing runs). Its diags
// name where they are ("clean/dst.table"), which puts a ⚠ on the card, the
// lane or the header, lists them in the inspector, and — placed by line —
// marks them in the JSON view.
//
// RUNS are the server's engine's, not the tab's: ◎ Preview runs the
// canvas's text as it is — every sink a preview, the source stopped after
// N rows, actions skipped, nothing written — and its rows land in this
// tab's grid ("Result 1 · 2" for two branches); ▶ Run saves, then runs
// the file. The engine's events ("job.*", every window) put counters on
// the cards (rows in → out), a state on each lane (○ queued ● running ✓
// ✗ ↷ skipped ■ stopped), the run's lines in the pipeline's log, and the
// tab's busy mark on the tab that started it, whose ■ Stop stops it.
//
// THE POINTER. Everything is pointer events, not HTML5 drag and drop (its
// ghost image and cross-browser quirks are why the splitters avoid it):
//
//	press on…            drag does                       release does
//	a palette entry      a ghost follows the pointer     over a lane: the node is added
//	                                                     there; elsewhere on the canvas:
//	                                                     a new fragment holding it
//	                                                     (a click without a drag: added
//	                                                     to the selected fragment)
//	a card               moves it (snapped to 10 px)     no drag: selects it
//	an output port ●     a wire follows the pointer      over a card with an input:
//	                                                     wired (replacing its input)
//	a wire               —                               selects it (Delete removes)
//	the background       pans                            no drag: selects the pipeline
(function () {
  "use strict";

  const dbc = window.dbc;
  const { api, log, el } = dbc;

  // ── names, kinds, geometry ─────────────────────────────────────────────
  // NAME is pipeline.ValidName: what a fragment or node may be called (it
  // names a log prefix and a variable, frag.<name>.rows).
  const NAME = /^[A-Za-z0-9_][A-Za-z0-9_.\-]{0,99}$/;
  const validName = (s) => NAME.test(s) && !s.endsWith(".");
  const pipeName = (s) => (s && !/\.json$/i.test(s) ? s + ".json" : s);
  const stemOf = (name) => name.replace(/\.json$/i, "");

  const GLYPH = { source: "⇥", transform: "ƒ", sink: "⇤", action: "▸" };
  // the JSON view's language: editor.js's own JSON grammar (defineJSON),
  // not Monaco's "json", whose language service the vendored copy lacks
  const JSON_LANG = "dbcjson";
  const KINDS = [["source", "Sources"], ["transform", "Transforms"], ["sink", "Sinks"], ["action", "Actions"]];
  const STATE = { queued: "○", running: "●", succeeded: "✓", failed: "✗", canceled: "■", skipped: "↷" };

  const CARD_W = 172, CARD_H = 66; // a node card
  const HEAD_H = 30;               // a lane's header
  const LANE_GAP = 14, GRID = 10, MIN_BODY = 110, PAD = 24;
  const snap = (v) => Math.max(0, Math.round(v / GRID) * GRID);

  // ── the spec as text ───────────────────────────────────────────────────
  // specText writes a spec as Spec.JSON does: the struct fields in their
  // order, empty ones left out as omitempty leaves them, map keys sorted
  // (encoding/json sorts them), two-space indent, then every array of
  // scalars — an edge, a position — on one line, by the same pattern.
  const SCALARS = /\[\s*((?:"[^"\\\n]*"|-?[0-9.eE+\-]+)(?:,\s*(?:"[^"\\\n]*"|-?[0-9.eE+\-]+))*)\s*\]/g;
  function sorted(obj, f) {
    const out = {};
    for (const k of Object.keys(obj || {}).sort()) out[k] = f ? f(obj[k]) : obj[k];
    return out;
  }
  function specText(spec) {
    const o = { name: spec.name || "" };
    if (spec.desc) o.desc = spec.desc;
    if (spec.params && Object.keys(spec.params).length) {
      o.params = sorted(spec.params, (p) => (p && p.doc ? { default: p.default || "", doc: p.doc } : { default: (p && p.default) || "" }));
    }
    o.fragments = (spec.fragments || []).map((f) => {
      const g = { name: f.name || "" };
      if (f.batch) g.batch = f.batch;
      if (f.on_error) g.on_error = f.on_error;
      g.nodes = (f.nodes || []).map((n) => {
        const m = { id: n.id, plugin: n.plugin };
        if (n.cfg && Object.keys(n.cfg).length) m.cfg = sorted(n.cfg);
        return m;
      });
      if (f.edges && f.edges.length) g.edges = f.edges.map((e) => [e[0], e[1]]);
      if (f.ui && Object.keys(f.ui).length) g.ui = sorted(f.ui, (p) => [p[0], p[1]]);
      return g;
    });
    return JSON.stringify(o, null, 2).replace(SCALARS, (m) =>
      "[" + m.slice(1, -1).split(",").map((s) => s.trim()).join(", ") + "]") + "\n";
  }

  // parseSpec reads text into a spec the canvas can draw — every list and
  // map there, even where the file left it out — or says why it cannot.
  // Check's rules are the server's; this only needs the shape.
  function parseSpec(text) {
    let s;
    try { s = JSON.parse(text); } catch (e) { return { err: e.message }; }
    if (!s || typeof s !== "object" || Array.isArray(s)) return { err: "a pipeline is a JSON object" };
    if (!Array.isArray(s.fragments)) s.fragments = [];
    for (const f of s.fragments) {
      if (!f || typeof f !== "object") return { err: "a fragment is an object" };
      if (!Array.isArray(f.nodes)) f.nodes = [];
      if (!Array.isArray(f.edges)) f.edges = [];
      if (!f.ui || typeof f.ui !== "object") f.ui = {};
      for (const n of f.nodes) if (!n.cfg || typeof n.cfg !== "object") n.cfg = {};
    }
    if (!s.params || typeof s.params !== "object") s.params = {};
    return { spec: s };
  }

  // freeID is base, or base2, base3 … — the first not in taken.
  function freeID(base, taken) {
    let n = base, i = 2;
    while (taken.includes(n)) n = base + i++;
    return n;
  }

  // fmtRows is a counter as a card shows it: 1,234 · 12.3k · 4.5M.
  function fmtRows(n) {
    n = n || 0;
    if (n < 10000) return n.toLocaleString("en-US");
    if (n < 1e6) return (n / 1e3).toFixed(n < 1e5 ? 1 : 0) + "k";
    return (n / 1e6).toFixed(1) + "M";
  }
  const fmtDur = (ms) => (ms < 1000 ? Math.round(ms) + "ms" : ms < 60e3 ? (ms / 1000).toFixed(1) + "s" :
    Math.floor(ms / 60e3) + "m" + String(Math.round((ms % 60e3) / 1000)).padStart(2, "0") + "s");

  // ── the plugin registry, once per page — and again after a "plugins"
  // event, when the server reloaded the user's plugin files ────────────
  //   list      every plugin, the user's (p.file set) among them
  //   problems  plugin files that did not load: {file, name?, error}
  //   dir       plugins_dir, ~-form, for the palette's notes
  let regP = null;
  function registry() {
    if (!regP) {
      regP = api("GET", "/api/v1/plugins").then((r) => ({
        list: r.plugins, conns: r.conns || [], problems: r.problems || [], dir: r.dir || "plugins_dir",
        byName: new Map(r.plugins.map((p) => [p.name, p])),
      })).catch((err) => { regP = null; throw err; });
    }
    return regP;
  }

  // drafts: unsaved text kept per pipeline in this browser. Storage may
  // refuse (a private window); a draft is only a safety net, so a failure
  // costs nothing but the net.
  const DRAFT = "dbc.pipe.draft.";
  function readDraft(name) {
    try {
      const d = JSON.parse(localStorage.getItem(DRAFT + name) || "null");
      return d && typeof d.text === "string" ? d : null;
    } catch (_) { return null; }
  }
  function writeDraft(name, d) {
    try {
      if (d) localStorage.setItem(DRAFT + name, JSON.stringify(d));
      else localStorage.removeItem(DRAFT + name);
    } catch (_) { /* no storage: no net */ }
  }

  function create(host) {
    // files: pipeline name → what this window has of it:
    //   rev     the revision the text is based on ("" = no file: deleted since)
    //   saved   the text as last read or saved — dirty is text !== saved
    //   text    the current text (canvas edits write it; the JSON view types it)
    //   spec    text parsed, or null while the JSON view holds text that
    //           does not parse (the canvas then shows why, and waits)
    //   diags   the last check's findings
    //   json    the JSON view is on
    //   sel     the selection: {kind: "node"|"frag"|"edge", frag, id, from, to} or null
    //   view    the canvas's pan and zoom, kept across tab switches
    //   cols    columns seen in this pipeline's previews, per fragment — the
    //           inspector offers them for a columns field
    const files = new Map();
    const path = (name) => "/api/v1/pipelines/" + encodeURIComponent(name);
    const docKey = (name) => "p:" + name;
    const isDirty = (e) => !!e && e.text !== e.saved;

    // runs: run id → what this page knows of it (job.run, job.progress,
    // job.done; GET /api/v1/runs after a load); latest: pipeline file →
    // its newest run's id, whose counters its canvas shows
    const runs = new Map();
    const latest = new Map();

    let shown = null; // the entry on screen, or null when no pipeline tab is
    let dom = null;   // the canvas's elements, built on first show
    let stage = null;
    let reg = null;   // the registry, once loaded
    let paletteQ = "";
    let rows = 50;    // preview rows
    const lastParams = new Map(); // pipeline → the params last run with

    // ── the file ─────────────────────────────────────────────────────────
    async function load(name) {
      const f = await api("GET", path(name));
      let e = files.get(name);
      if (e) {
        if (!isDirty(e)) adopt(e, f.text, f.rev);
        return e;
      }
      e = { name, rev: f.rev, saved: f.text, text: f.text, spec: null, diags: [], json: false, sel: null, view: null, cols: {} };
      e.spec = parseSpec(f.text).spec || null;
      // a draft of unsaved edits from before a reload: put back, keeping
      // the base it was typed against — so if the file moved on since,
      // the first save meets the conflict rather than writing over it
      const d = readDraft(name);
      if (d && d.text !== f.text) {
        e.text = d.text;
        e.rev = d.base || "";
        e.spec = parseSpec(d.text).spec || null;
        log("info", name + ": unsaved changes from before were put back — Ctrl+S saves them", logKey(name));
      } else if (d) writeDraft(name, null);
      files.set(name, e);
      check(name);
      return e;
    }

    // adopt takes a file's text as the entry's own, saved.
    function adopt(e, text, rev) {
      Object.assign(e, { text, saved: text, rev, spec: parseSpec(text).spec || null, parseErr: "" });
      writeDraft(e.name, null);
      dbc.editor.replaceDoc(docKey(e.name), text);
      changedView(e);
      check(e.name);
    }

    // save writes the text, reporting whether it is now on disk.
    async function save(name) {
      const e = files.get(name);
      if (!e) return false;
      if (e.json) syncFromEditor(e);
      if (!isDirty(e) && e.rev) return true;
      const text = e.text;
      let r;
      try {
        r = await api("PUT", path(name) + host.winQuery(), { text, base: e.rev });
      } catch (err) {
        if (err.status === 409 && !e.rev) {
          // a create onto a file made meanwhile (or restored from the
          // trash): its revision now, then the conflict as below
          try {
            const f = await api("GET", path(name));
            return conflicted(e, text, f.text, f.rev);
          } catch (_) { /* fall through to the message */ }
        }
        log("err", name + " was not saved: " + err.message, logKey(name));
        return false;
      }
      if (r.conflict && !r.rev) {
        // deleted or trashed since: the tab keeps the text; the next save
        // (from no base) writes the file again
        e.rev = "";
        log("warn", name + " is gone from the pipelines directory — Ctrl+S writes it again", logKey(name));
        return save(name);
      }
      if (r.conflict) return conflicted(e, text, r.text, r.rev);
      Object.assign(e, { rev: r.rev, saved: text });
      writeDraft(name, null);
      changedView(e);
      return true;
    }

    // conflicted asks what to do about a file changed elsewhere since this
    // tab's text was based on it: keep this tab's (written over it), or
    // take the file's. Resolves to whether this tab's text was saved.
    function conflicted(e, mine, theirs, rev) {
      return new Promise((resolve) => {
        const keep = el("button", { type: "button", class: "primary" }, "Save mine over it");
        const take = el("button", { type: "button" }, "Load the file's");
        const no = el("button", { type: "button" }, "Cancel");
        let done = false;
        const finish = (v) => { if (!done) { done = true; resolve(v); } };
        keep.addEventListener("click", async () => {
          dbc.modal.close();
          e.rev = rev;
          finish(await save(e.name));
        });
        take.addEventListener("click", () => {
          dbc.modal.close();
          adopt(e, theirs, rev);
          log("info", e.name + ": the file's version is loaded", logKey(e.name));
          finish(false);
        });
        no.addEventListener("click", () => dbc.modal.close());
        dbc.modal.open({
          title: e.name + " changed on disk", focus: no, onClose: () => finish(false),
          body: el("div", "confirm", el("p", null, e.name + " was saved elsewhere — another window, the TUI, an editor — " +
            "since this tab read it. Saving this tab's version replaces that one.")),
          foot: el("div", "mfoot", keep, take, no),
        });
      });
    }

    // changed is every canvas edit's end: the text written again from the
    // spec, the draft kept, the check scheduled, the views redrawn.
    function changed(e) {
      e.text = specText(e.spec);
      // the JSON document follows (an edit, so the JSON view's Ctrl+Z
      // walks back through canvas edits too); a pipeline never shown has
      // none yet, and gets today's text when it is
      dbc.editor.replaceDoc(docKey(e.name), e.text);
      storeDraft(e);
      scheduleCheck(e.name);
      changedView(e);
    }

    // edited is the JSON view's edit (app.js forwards the editor's change):
    // the text is the editor's, the spec follows when it parses.
    function edited(name) {
      const e = files.get(name);
      if (!e || !e.json) return;
      syncFromEditor(e);
      storeDraft(e);
      scheduleCheck(name);
      host.changed(name);
      renderBar();
    }

    function syncFromEditor(e) {
      const t = dbc.editor.docText(docKey(e.name));
      if (t === null || t === undefined) return;
      e.text = t;
      const p = parseSpec(t);
      if (p.spec) e.spec = p.spec;
      e.parseErr = p.err || "";
    }

    let draftTimer = 0;
    function storeDraft(e) {
      clearTimeout(draftTimer);
      draftTimer = setTimeout(() => {
        writeDraft(e.name, isDirty(e) ? { base: e.rev, text: e.text, at: Date.now() } : null);
      }, 300);
    }
    // flush writes the pending draft now (a tab switch, the page going)
    function flush() {
      clearTimeout(draftTimer);
      for (const e of files.values()) writeDraft(e.name, isDirty(e) ? { base: e.rev, text: e.text, at: Date.now() } : null);
    }

    // forget lets a closed tab's pipeline go; discard drops its unsaved
    // edits (the close dialog's "Discard"), which a reload would put back.
    function forget(name, discard) {
      const e = files.get(name);
      if (!e) return;
      if (discard || !isDirty(e)) writeDraft(name, null);
      files.delete(name);
      dbc.editor.dropDoc(docKey(name));
      if (shown === e) shown = null;
    }

    // changedView redraws what shows e: the canvas when it is on screen,
    // the strip's ● and the header always.
    function changedView(e) {
      host.changed(e.name);
      if (shown === e) render();
    }

    // ── the check ────────────────────────────────────────────────────────
    const checkTimers = new Map();
    function scheduleCheck(name) {
      clearTimeout(checkTimers.get(name));
      checkTimers.set(name, setTimeout(() => check(name), 500));
    }
    const checkSeq = new Map();
    async function check(name, loud) {
      const e = files.get(name);
      if (!e) return [];
      const n = (checkSeq.get(name) || 0) + 1;
      checkSeq.set(name, n);
      let r;
      try { r = await api("POST", "/api/v1/pipeline-check", { text: e.text }); } catch (err) {
        if (loud) log("err", "check: " + err.message, logKey(name));
        return e.diags;
      }
      if (checkSeq.get(name) !== n || files.get(name) !== e) return e.diags; // a newer check is coming
      e.diags = r.diags || [];
      dbc.editor.setMarkers(docKey(name), e.diags.filter((d) => d.line > 0).map((d) => ({
        line: d.line, col: d.col, severity: d.severity, msg: (d.where ? d.where + ": " : "") + d.msg })));
      if (loud) {
        if (!e.diags.length) log("ok", "✓ " + name + " checks out", logKey(name));
        for (const d of e.diags) log(d.severity === "error" ? "err" : "warn", (d.where ? d.where + ": " : "") + d.msg, logKey(name));
      }
      host.changed(name);
      if (shown === e) { renderBar(); renderCanvas(); renderInspector(); }
      return e.diags;
    }

    // diagsAt splits a check's findings by what they are about: a node
    // ("frag/node", "frag/node.field"), a fragment ("frag", "frag:edge a→b",
    // "fragments[2]"), or the pipeline (the rest).
    function diagsAt(e) {
      const out = { node: new Map(), frag: new Map(), top: [] };
      const add = (m, k, d) => { if (!m.has(k)) m.set(k, []); m.get(k).push(d); };
      for (const d of e.diags || []) {
        const w = d.where || "";
        const slash = w.indexOf("/");
        if (slash > 0) {
          const frag = w.slice(0, slash), node = w.slice(slash + 1).split(".")[0];
          add(out.node, frag + "/" + node, d);
        } else if (w && w !== "name" && w !== "fragments" && !w.startsWith("params.")) {
          add(out.frag, w.split(":")[0], d);
        } else out.top.push(d);
      }
      return out;
    }

    // ── model edits ──────────────────────────────────────────────────────
    const fragOf = (e, name) => e.spec.fragments.find((f) => f.name === name) || null;
    const nodeOf = (f, id) => f.nodes.find((n) => n.id === id) || null;
    const kindOf = (n) => { const p = reg && reg.byName.get(n.plugin); return p ? p.kind : ""; };
    const parentOf = (f, id) => { const ed = f.edges.find((x) => x[1] === id); return ed ? ed[0] : ""; };

    // addNode puts a new node of plugin in fragment f at (x, y), named after
    // the plugin's verb ("sql.read" → read, read2 …), selected.
    function addNode(e, f, plugin, x, y) {
      const base = plugin.name.split(".").pop().replace(/[^A-Za-z0-9_]/g, "") || "node";
      const id = freeID(base, f.nodes.map((n) => n.id));
      const cfg = {};
      // a conn field starts on the tab's connection, or the first one: the
      // node is useful before the inspector is touched
      for (const fd of plugin.fields) {
        if (fd.type === "conn" && fd.required) cfg[fd.name] = host.conn() || (reg.conns[0] || "");
      }
      const at = freeSpot(f, snap(x), snap(y));
      f.nodes.push({ id, plugin: plugin.name, cfg });
      f.ui[id] = at;
      e.sel = { kind: "node", frag: f.name, id };
      changed(e);
      return id;
    }

    // freeSpot is (x, y), or the first place below it where a new card
    // covers no other (with a margin): a drop near a card puts the new one
    // beside it rather than on it, where neither could be read or grabbed.
    function freeSpot(f, x, y) {
      const boxes = f.nodes.map((n) => posOf(f, n.id));
      const m = 14;
      const hits = (yy) => boxes.some((p) => x < p[0] + CARD_W + m && x + CARD_W + m > p[0] && yy < p[1] + CARD_H + m && yy + CARD_H + m > p[1]);
      for (let i = 0; i < 50 && hits(y); i++) y += GRID * 2;
      return [x, y];
    }

    function addFragment(e, at, action) {
      const name = freeID("fragment", e.spec.fragments.map((f) => f.name));
      const f = { name, nodes: [], edges: [], ui: {} };
      if (at === undefined) e.spec.fragments.push(f);
      else e.spec.fragments.splice(at, 0, f);
      e.sel = { kind: "frag", frag: name };
      if (!action) changed(e);
      return f;
    }

    function removeNode(e, f, id) {
      f.nodes = f.nodes.filter((n) => n.id !== id);
      f.edges = f.edges.filter((x) => x[0] !== id && x[1] !== id);
      delete f.ui[id];
      e.sel = { kind: "frag", frag: f.name };
      changed(e);
    }

    function duplicateNode(e, f, id) {
      const n = nodeOf(f, id);
      if (!n) return;
      const nid = freeID(n.id, f.nodes.map((x) => x.id));
      f.nodes.push({ id: nid, plugin: n.plugin, cfg: Object.assign({}, n.cfg) });
      const p = posOf(f, id);
      f.ui[nid] = freeSpot(f, snap(p[0] + 20), snap(p[1] + CARD_H + 20));
      e.sel = { kind: "node", frag: f.name, id: nid };
      changed(e);
    }

    // connect wires from → to in f, by the fragment's rules: rows flow
    // from a source or transform into a transform or sink, one input per
    // node (a new wire replaces the old), never round in a circle. It
    // returns what to say when it would not.
    function connect(e, f, from, to) {
      const a = nodeOf(f, from), b = nodeOf(f, to);
      if (!a || !b || from === to) return "";
      const ka = kindOf(a), kb = kindOf(b);
      if (ka !== "source" && ka !== "transform") return a.id + " has no output — a " + (ka || "node") + " ends a branch";
      if (kb !== "transform" && kb !== "sink") return b.id + " takes no input — a " + (kb || "node") + " starts a branch, or stands alone";
      for (let p = from; p; p = parentOf(f, p)) {
        if (p === to) return "that would make a loop: " + to + " feeds " + from;
      }
      if (f.edges.some((x) => x[0] === from && x[1] === to)) return "";
      const old = f.edges.find((x) => x[1] === to);
      f.edges = f.edges.filter((x) => x[1] !== to);
      f.edges.push([from, to]);
      e.sel = { kind: "edge", frag: f.name, from, to };
      changed(e);
      if (old) host.status("rewired " + to + ": its input is now " + from + " (was " + old[0] + ")", "");
      return "";
    }

    function removeEdge(e, f, from, to) {
      f.edges = f.edges.filter((x) => !(x[0] === from && x[1] === to));
      e.sel = { kind: "frag", frag: f.name };
      changed(e);
    }

    // renameNode renames a node everywhere its id is: the node, its edges,
    // its position, the selection.
    function renameNode(e, f, id, to) {
      if (to === id) return "";
      if (!validName(to)) return "not a node name: letters, digits, . _ -";
      if (nodeOf(f, to)) return f.name + " already has a node " + to;
      nodeOf(f, id).id = to;
      f.edges = f.edges.map((x) => [x[0] === id ? to : x[0], x[1] === id ? to : x[1]]);
      if (f.ui[id]) { f.ui[to] = f.ui[id]; delete f.ui[id]; }
      e.sel = { kind: "node", frag: f.name, id: to };
      changed(e);
      return "";
    }

    // renameFragment renames a fragment, and the ${frag.<name>.…} values
    // later fragments read from it, so the pipeline still means the same.
    function renameFragment(e, f, to) {
      if (to === f.name) return "";
      if (!validName(to)) return "not a fragment name: letters, digits, . _ -";
      if (fragOf(e, to)) return "the pipeline already has a fragment " + to;
      const from = "${frag." + f.name + ".", into = "${frag." + to + ".";
      for (const g of e.spec.fragments) for (const n of g.nodes) {
        for (const k of Object.keys(n.cfg)) n.cfg[k] = n.cfg[k].split(from).join(into);
      }
      f.name = to;
      e.sel = { kind: "frag", frag: to };
      changed(e);
      return "";
    }

    function moveFragment(e, f, by) {
      const fs = e.spec.fragments, i = fs.indexOf(f), j = i + by;
      if (i < 0 || j < 0 || j >= fs.length) return;
      fs.splice(i, 1);
      fs.splice(j, 0, f);
      changed(e);
    }

    function removeFragment(e, f) {
      e.spec.fragments = e.spec.fragments.filter((g) => g !== f);
      e.sel = null;
      changed(e);
    }

    // ── layout ───────────────────────────────────────────────────────────
    // posOf is a node's position in its lane: the spec's, else one by depth
    // from the source (rows left to right) and order among its siblings.
    function posOf(f, id) {
      const p = f.ui[id];
      if (Array.isArray(p) && p.length === 2) return [Number(p[0]) || 0, Number(p[1]) || 0];
      return autoPos(f)[id] || [PAD, PAD];
    }
    function autoPos(f) {
      const out = {}, depth = {}, seen = new Set();
      const roots = f.nodes.filter((n) => !parentOf(f, n.id)).map((n) => n.id);
      let row = 0;
      const walk = (id, d) => {
        if (seen.has(id)) return;
        seen.add(id);
        depth[id] = d;
        out[id] = [PAD + d * (CARD_W + 56), PAD + row * (CARD_H + 26)];
        const kids = f.edges.filter((x) => x[0] === id).map((x) => x[1]);
        kids.forEach((k, i) => { if (i) row++; walk(k, d + 1); });
      };
      roots.forEach((r, i) => { if (i) row++; walk(r, 0); });
      return out;
    }

    // ── rendering ────────────────────────────────────────────────────────
    function mount() {
      if (dom) return;
      const root = document.getElementById("pipe");
      const name = el("span", "pname");
      const meta = el("span", "pmeta");
      const rowsIn = el("input", { class: "prows", type: "number", min: "1", max: "1000", value: String(rows),
        title: "Rows a preview takes from each source", "aria-label": "Preview rows" });
      const b = (label, title, act) => el("button", { type: "button", title, "data-act": act }, label);
      const bar = el("div", "pbar", name, meta, el("span", "spacer"),
        el("label", { class: "prowsl", title: "Rows a preview takes from each source" }, "rows", rowsIn),
        b("◎ Preview", "Run the pipeline on real data without writing anything: every sink shows its rows in the grid", "preview"),
        b("⊡ Fit", "Fit the canvas to the pane", "fit"),
        b("{ } JSON", "Edit the pipeline as JSON (the canvas and the file are the same text)", "json"),
        b("⇪ Go", "Export as a dbc script: the same pipeline in Go, in a script tab", "export"),
        b("⋯", "More: rename, duplicate, trash, copy path", "more"));
      const filter = el("input", { type: "search", class: "pfilter", placeholder: "filter plugins…",
        "aria-label": "Filter plugins", spellcheck: "false", autocomplete: "off" });
      const palList = el("div", "plist");
      const palette = el("aside", { class: "ppal", "aria-label": "Plugins" }, filter, palList);
      const layer = el("div", "pstage");
      const canvas = el("div", { class: "pcanvas", tabindex: "0", "aria-label":
        "Pipeline canvas — drag plugins in, wire output ● to input ●, Delete removes, Ctrl+D duplicates, f fits" }, layer);
      const insp = el("aside", { class: "pinsp", "aria-label": "Inspector" });
      root.replaceChildren(bar, el("div", "pbody", palette, canvas, insp));
      dom = { root, name, meta, rowsIn, bar, filter, palList, layer, canvas, insp };
      stage = dbc.stage(canvas, layer, { min: 0.25, max: 2, onChange: (v) => { if (shown) shown.view = { tx: v.tx, ty: v.ty, k: v.k }; } });

      rowsIn.addEventListener("change", () => { rows = Math.min(Math.max(parseInt(rowsIn.value, 10) || 50, 1), 1000); rowsIn.value = String(rows); });
      bar.addEventListener("click", (ev) => {
        const a = ev.target.closest("button[data-act]");
        if (!a || !shown) return;
        const act = a.dataset.act;
        if (act === "preview") preview(shown.name, "");
        else if (act === "fit") fit();
        else if (act === "json") toggleJSON(shown.name);
        else if (act === "export") exportGo(shown.name);
        else if (act === "more") {
          const r = a.getBoundingClientRect();
          dbc.menu.open(r.left, r.bottom + 2, items(shown.name));
        }
      });
      filter.addEventListener("input", () => { paletteQ = filter.value.trim().toLowerCase(); renderPalette(); });
      bindPointer();
      bindKeys();
    }

    // show puts pipeline name on the canvas (its tab came on screen).
    async function show(name) {
      mount();
      let e = files.get(name);
      if (!e) e = await load(name);
      shown = e;
      dom.root.hidden = false;
      try { reg = await registry(); } catch (err) { log("err", "plugins: " + err.message, logKey(name)); }
      if (shown !== e) return;
      // The editor holds the pipeline's JSON while its tab is on screen —
      // shown in the JSON view, hidden under the canvas otherwise — so it
      // is never left on another tab's SQL (which the assistant would read
      // as this tab's query), and the JSON view opens on today's text.
      dbc.editor.useDoc(docKey(name), e.text, JSON_LANG);
      dbc.editor.replaceDoc(docKey(name), e.text);
      host.setJSON(e.json);
      render();
      if (e.view) { stage.set(e.view); return; }
      // the first look: at full size from the top left — or, when the
      // pipeline is bigger than the pane, fitted, but never below 75%,
      // where card text stops being readable (pan for the rest)
      stage.set({ tx: 12, ty: 10, k: 1 });
      const g = dom.geo, W = dom.canvas.clientWidth, H = dom.canvas.clientHeight;
      if (g && W && H && (g.w + 24 > W || g.h + 20 > H)) {
        stage.fit(g.w, g.h, 12);
        if (stage.view.k < 0.75) stage.set({ tx: 12, ty: 10, k: 0.75 });
      }
    }

    // hide is the pipeline tab leaving the screen.
    function hide() {
      if (shown && shown.json) syncFromEditor(shown);
      shown = null;
      if (dom) dom.root.hidden = true;
      flush();
    }

    function render() {
      if (!dom || !shown) return;
      renderBar();
      renderPalette();
      renderCanvas();
      renderInspector();
    }

    function renderBar() {
      const e = shown;
      if (!e) return;
      const errs = (e.diags || []).filter((d) => d.severity === "error").length, warns = (e.diags || []).length - errs;
      dom.name.textContent = "⛓ " + e.name + (isDirty(e) ? " ●" : "");
      dom.name.title = isDirty(e) ? "unsaved changes — Ctrl+S saves (▶ Run saves first)" : "saved";
      dom.meta.replaceChildren();
      if (e.spec && e.spec.desc) dom.meta.append(el("span", "pdesc", e.spec.desc));
      if (errs) dom.meta.append(el("span", "perr", "· " + dbc.plural(errs, "error")));
      if (warns) dom.meta.append(el("span", "pwarn", "· " + dbc.plural(warns, "warning")));
      const jb = dom.bar.querySelector('[data-act="json"]');
      jb.classList.toggle("on", !!e.json);
      jb.textContent = e.json ? "⊞ Canvas" : "{ } JSON";
      jb.title = e.json ? "Back to the canvas (the JSON must parse)" : "Edit the pipeline as JSON (the canvas and the file are the same text)";
    }

    function renderPalette() {
      if (!dom) return;
      dom.palList.replaceChildren();
      if (!reg) { dom.palList.append(el("div", "pnote", "loading plugins…")); return; }
      const hit = (p) => !paletteQ || p.name.includes(paletteQ) || (p.label || "").toLowerCase().includes(paletteQ) ||
        (p.doc || "").toLowerCase().includes(paletteQ);
      const item = (p) => el("div", { class: "pitem k-" + p.kind, "data-plugin": p.name, title: p.doc,
        role: "button", tabindex: "-1" },
      el("span", "pg", GLYPH[p.kind]), el("span", "pn", p.name), el("span", "pl", p.label || ""));
      // Yours: the plugin files in plugins_dir, every kind together (their
      // kind shows in the glyph), first — they are what this user reaches
      // for. A file that did not load is listed too, dimmed, with why: it
      // cannot be dragged (no data-plugin), and the scripts browser opens it.
      const mine = reg.list.filter((p) => p.file && hit(p));
      const broken = reg.problems.filter((x) => !paletteQ || (x.name || x.file).toLowerCase().includes(paletteQ));
      if (mine.length || broken.length) {
        dom.palList.append(el("div", { class: "phead", title: "Your plugin files, in " + reg.dir }, "Yours"));
        for (const p of mine) dom.palList.append(item(p));
        for (const x of broken) {
          const file = x.file.split("/").pop();
          dom.palList.append(el("div", { class: "pitem broken", title: file + " did not load: " + x.error +
            " — Ctrl+O → Plugins opens it" }, el("span", "pg", "⚠"), el("span", "pn", x.name || file), el("span", "pl", "did not load")));
        }
      }
      for (const [kind, title] of KINDS) {
        const ps = reg.list.filter((p) => !p.file && p.kind === kind && hit(p));
        if (!ps.length) continue;
        dom.palList.append(el("div", "phead", title));
        for (const p of ps) dom.palList.append(item(p));
      }
      if (!dom.palList.children.length) dom.palList.append(el("div", "pnote", "no plugin matches"));
    }

    // lanes is the canvas's geometry: each fragment's lane, its top and
    // body height, its nodes' positions; and the stage's size.
    function lanes(e) {
      const out = [];
      let y = 0, w = 640;
      for (const f of e.spec.fragments) {
        const pos = {};
        let maxX = 0, maxY = 0;
        for (const n of f.nodes) {
          const p = posOf(f, n.id);
          pos[n.id] = p;
          maxX = Math.max(maxX, p[0] + CARD_W);
          maxY = Math.max(maxY, p[1] + CARD_H);
        }
        const h = Math.max(MIN_BODY, maxY + PAD);
        w = Math.max(w, maxX + PAD * 2);
        out.push({ f, top: y, h, pos });
        y += HEAD_H + h + LANE_GAP;
      }
      return { lanes: out, w, h: y + 44 };
    }

    // the run whose counters the canvas shows: the newest of this file
    const runOf = (e) => (latest.has(e.name) ? runs.get(latest.get(e.name)) : null);

    function renderCanvas() {
      const e = shown;
      if (!e || !dom) return;
      dom.layer.replaceChildren();
      if (!e.spec || (e.parseErr && e.json)) {
        dom.layer.append(el("div", "pempty", e.parseErr ? "The JSON does not parse: " + e.parseErr : "Not a pipeline."));
        return;
      }
      const geo = lanes(e), da = diagsAt(e), run = runOf(e);
      for (const L of geo.lanes) {
        const f = L.f;
        const fr = run && run.frags.get(f.name);
        const fd = da.frag.get(f.name) || [];
        const nodesWithDiags = f.nodes.filter((n) => da.node.has(f.name + "/" + n.id)).length;
        const selLane = e.sel && e.sel.kind === "frag" && e.sel.frag === f.name;
        const head = el("div", "lhead",
          el("span", { class: "lstate s-" + (fr ? fr.status : "none"), title: fr ? fr.status + (fr.error ? ": " + fr.error : "") : "not run here yet" },
            fr ? STATE[fr.status] || "○" : "○"),
          el("b", "lname", f.name),
          el("span", "lmeta", "batch " + (f.batch || 1000) + (f.on_error === "continue" ? " · on error: continue" : "") +
            (fr && fr.status !== "queued" && fr.status !== "skipped" ? " · " + fmtRows(fr.rows) + " rows" + (fr.direct ? " · direct COPY" : "") : "")),
          fd.length || nodesWithDiags ? el("span", { class: "ldiag", title: fd.map((d) => d.msg).join("\n") || "a node has problems" },
            "⚠ " + (fd.length + nodesWithDiags)) : null,
          el("span", "spacer"),
          el("button", { type: "button", "data-lact": "preview", title: "Preview only this fragment (writes nothing)" }, "◎"),
          el("button", { type: "button", "data-lact": "run", title: "Save, then run only this fragment" }, "▶"),
          el("button", { type: "button", "data-lact": "menu", title: "Move, add, delete" }, "⋯"));
        const svg = document.createElementNS("http://www.w3.org/2000/svg", "svg");
        svg.setAttribute("class", "ledges");
        svg.setAttribute("width", String(geo.w));
        svg.setAttribute("height", String(L.h));
        const body = el("div", { class: "lbody", style: "height:" + L.h + "px" }, svg);
        for (const n of f.nodes) body.append(card(e, f, n, L.pos[n.id], da.node.get(f.name + "/" + n.id), run));
        if (!f.nodes.length) body.append(el("div", "lhint", "drop a plugin here — a source to start, or an action alone"));
        const lane = el("section", { class: "plane" + (selLane ? " sel" : ""), "data-frag": f.name,
          style: "top:" + L.top + "px;width:" + geo.w + "px" }, head, body);
        dom.layer.append(lane);
        drawEdges(e, f, svg, L.pos);
      }
      dom.layer.append(el("div", { class: "padd", style: "top:" + (geo.h - 40) + "px;width:" + geo.w + "px", "data-add": "1" },
        "+ fragment — or drop a plugin here for a new one"));
      dom.layer.style.width = geo.w + "px";
      dom.layer.style.height = geo.h + "px";
      dom.geo = geo;
    }

    // card is one node: kind and plugin, its id, a line of its config, the
    // run's counters; ports where rows go in and out.
    function card(e, f, n, p, diags, run) {
      const kind = kindOf(n);
      const sel = e.sel && e.sel.kind === "node" && e.sel.frag === f.name && e.sel.id === n.id;
      const ns = run && run.frags.get(f.name) && (run.frags.get(f.name).nodes || []).find((x) => x.id === n.id);
      const errs = (diags || []).filter((d) => d.severity === "error");
      const c = el("div", { class: "pcard k-" + (kind || "none") + (sel ? " sel" : "") + (errs.length ? " bad" : diags && diags.length ? " warn" : "") +
          (ns && ns.error ? " failed" : ""), "data-id": n.id, style: "left:" + p[0] + "px;top:" + p[1] + "px",
        title: (diags || []).map((d) => d.msg).join("\n") || (reg && reg.byName.get(n.plugin) ? reg.byName.get(n.plugin).label : "") },
      el("div", "ctop", el("span", "cg", GLYPH[kind] || "?"), el("span", "cp", n.plugin),
        diags && diags.length ? el("span", "cd", "⚠" + diags.length) : null),
      el("div", "cid", n.id),
      el("div", "csum", summary(n)),
      el("div", "cnum", ns ? (kind === "source" ? fmtRows(ns.out) + " out" : kind === "action" ? fmtRows(ns.out) + " rows" :
        fmtRows(ns.in) + " → " + fmtRows(ns.out)) + (ns.batches ? " · " + ns.batches + (ns.batches === 1 ? " batch" : " batches") : "") : ""));
      if (kind === "transform" || kind === "sink") c.append(el("span", { class: "pport in", title: "input: wire a source or transform here" }));
      if (kind === "source" || kind === "transform") c.append(el("span", { class: "pport out", title: "output: drag to a node's input" }));
      return c;
    }

    // summary is the line of a node's config a card shows: its connection,
    // then what it reads or writes — or its code's first real line.
    function summary(n) {
      const c = n.cfg || {};
      const what = c.table || c.query || c.sql || c.rules || c.keep || c.name || c.rows ||
        (c.code ? c.code.split("\n").find((l) => l.trim() && !l.trim().startsWith("//")) || "" : "");
      return [c.conn, String(what || "").replace(/\s+/g, " ").trim()].filter(Boolean).join(" · ");
    }

    // drawEdges draws f's wires into svg: a Bezier from each output port to
    // the input it feeds, under a wider invisible stroke that takes clicks.
    function drawEdges(e, f, svg, pos) {
      const NS = "http://www.w3.org/2000/svg";
      svg.replaceChildren();
      for (const [a, b] of f.edges) {
        const pa = pos[a], pb = pos[b];
        if (!pa || !pb) continue;
        const d = curve(pa[0] + CARD_W, pa[1] + CARD_H / 2, pb[0], pb[1] + CARD_H / 2);
        const sel = e.sel && e.sel.kind === "edge" && e.sel.frag === f.name && e.sel.from === a && e.sel.to === b;
        const vis = document.createElementNS(NS, "path");
        vis.setAttribute("d", d);
        vis.setAttribute("class", "edge" + (sel ? " sel" : ""));
        const hit = document.createElementNS(NS, "path");
        hit.setAttribute("d", d);
        hit.setAttribute("class", "ehit");
        hit.dataset.from = a;
        hit.dataset.to = b;
        svg.append(vis, hit);
      }
    }
    function curve(x1, y1, x2, y2) {
      const dx = Math.max(40, Math.abs(x2 - x1) / 2);
      return "M" + x1 + "," + y1 + " C" + (x1 + dx) + "," + y1 + " " + (x2 - dx) + "," + y2 + " " + x2 + "," + y2;
    }

    function fit() {
      if (!shown || !dom.geo) return;
      stage.fit(dom.geo.w, dom.geo.h, 12);
    }

    // ── the inspector ────────────────────────────────────────────────────
    // renderInspector draws the selection's form: a node's fields (from its
    // plugin's), a fragment's settings, a wire, or — with nothing selected
    // — the pipeline's name, description and parameters. Edits apply as
    // they are typed; renames on Enter or leaving the box (a half-typed id
    // is not a rename).
    function renderInspector() {
      const e = shown;
      if (!e || !dom) return;
      const box = dom.insp;
      // a field being typed in keeps its box: redrawing under the caret
      // would lose it (the canvas redraws; this pane waits for the blur)
      if (box.contains(document.activeElement) && box.dataset.sel === selKey(e)) { drawDiags(e); return; }
      box.dataset.sel = selKey(e);
      box.replaceChildren();
      if (!e.spec) { box.append(el("p", "pnote", "Fix the JSON to see the inspector.")); return; }
      const s = e.sel;
      const f = s && fragOf(e, s.frag);
      if (s && s.kind === "node" && f && nodeOf(f, s.id)) inspectNode(e, f, nodeOf(f, s.id), box);
      else if (s && s.kind === "edge" && f) inspectEdge(e, f, s, box);
      else if (s && s.kind === "frag" && f) inspectFragment(e, f, box);
      else inspectPipeline(e, box);
      box.append(el("div", { class: "idiags", "data-diags": "1" }));
      drawDiags(e);
    }
    const selKey = (e) => (e.sel ? [e.sel.kind, e.sel.frag, e.sel.id, e.sel.from, e.sel.to].join("|") : "");

    // drawDiags lists the selection's findings at the inspector's foot.
    function drawDiags(e) {
      const box = dom.insp.querySelector("[data-diags]");
      if (!box) return;
      const da = diagsAt(e), s = e.sel;
      let ds = da.top;
      if (s && s.kind === "node") ds = da.node.get(s.frag + "/" + s.id) || [];
      else if (s && s.kind !== "node") ds = da.frag.get(s.frag) || [];
      box.replaceChildren(...ds.map((d) => el("div", "idiag " + d.severity, (d.severity === "error" ? "✗ " : "⚠ ") +
        (d.where && d.where.includes(".") ? d.where.split(".").slice(1).join(".") + ": " : "") + d.msg)));
    }

    function row(label, input, doc, type) {
      return el("label", "ifield", el("span", "iname", label, type ? el("span", "itype", type) : null), input,
        doc ? el("span", "idoc", doc) : null);
    }

    function inspectPipeline(e, box) {
      const sp = e.spec;
      box.append(el("div", "ihead", el("span", "ig", "⛓"), el("b", null, "pipeline"), el("span", "ik", e.name)));
      const name = el("input", { value: sp.name || "", spellcheck: "false" });
      name.addEventListener("input", () => { sp.name = name.value.trim(); changed(e); });
      const desc = el("textarea", { rows: "2", spellcheck: "true" });
      desc.value = sp.desc || "";
      desc.addEventListener("input", () => { sp.desc = desc.value; changed(e); });
      box.append(row("name", name, "What runs and logs call it — usually the file's name without .json"),
        row("desc", desc, "One line on what it does: the browser lists it"));
      box.append(el("div", "isub", "parameters", el("span", "idoc", " — ${name} in any field; a run asks for those without a default")));
      const params = sp.params || (sp.params = {});
      for (const k of Object.keys(params).sort()) {
        const p = params[k] || {};
        const nm = el("input", { value: k, class: "pk", spellcheck: "false", "aria-label": "parameter name" });
        const dv = el("input", { value: p.default || "", class: "pv", placeholder: "default", spellcheck: "false" });
        const dc = el("input", { value: p.doc || "", class: "pd", placeholder: "what it is", spellcheck: "true" });
        const x = el("button", { type: "button", class: "linkish", title: "Remove the parameter" }, "✕");
        nm.addEventListener("change", () => {
          const to = nm.value.trim();
          if (to === k) return;
          if (!validName(to) || params[to]) { nm.value = k; host.status("not a free parameter name: " + to, "warn"); return; }
          params[to] = params[k];
          delete params[k];
          changed(e);
        });
        dv.addEventListener("input", () => { params[k] = Object.assign({}, params[k], { default: dv.value }); changed(e); });
        dc.addEventListener("input", () => { params[k] = Object.assign({}, params[k], { doc: dc.value }); changed(e); });
        x.addEventListener("click", () => { delete params[k]; changed(e); renderInspector(); });
        box.append(el("div", "iparam", nm, dv, dc, x));
      }
      const add = el("button", { type: "button" }, "+ parameter");
      add.addEventListener("click", () => {
        params[freeID("param", Object.keys(params))] = { default: "" };
        changed(e);
        box.dataset.sel = "";
        renderInspector();
      });
      box.append(el("div", "iacts", add));
      box.append(el("p", "idoc", "Select a node or a lane to edit it. Drag plugins from the left onto a lane; " +
        "wire a card's output ● to another's input ●."));
    }

    function inspectFragment(e, f, box) {
      box.append(el("div", "ihead", el("span", "ig", "▤"), el("b", null, "fragment"), el("span", "ik", f.name)));
      const name = el("input", { value: f.name, spellcheck: "false" });
      const rename = () => {
        const why = renameFragment(e, f, name.value.trim());
        if (why) { host.status(why, "warn"); name.value = f.name; }
      };
      name.addEventListener("change", rename);
      name.addEventListener("keydown", (ev) => { if (ev.key === "Enter") { ev.preventDefault(); name.blur(); } });
      const batch = el("input", { value: f.batch ? String(f.batch) : "", placeholder: "1000", inputmode: "numeric" });
      batch.addEventListener("input", () => {
        const n = parseInt(batch.value, 10);
        if (n > 0) f.batch = n; else delete f.batch;
        changed(e);
      });
      const onErr = el("select", null, el("option", { value: "" }, "stop the pipeline"), el("option", { value: "continue" }, "log it and go on"));
      onErr.value = f.on_error === "continue" ? "continue" : "";
      onErr.addEventListener("change", () => { if (onErr.value) f.on_error = onErr.value; else delete f.on_error; changed(e); });
      box.append(row("name", name, "Later fragments read its values as ${frag." + f.name + ".rows}"),
        row("batch", batch, "Rows per batch: what a source yields per step, and a transform sees per call"),
        row("on error", onErr, "What a failure here does to the run"));
      const b = (label, act) => { const x = el("button", { type: "button" }, label); x.addEventListener("click", act); return x; };
      box.append(el("div", "iacts",
        b("◎ Preview it", () => preview(e.name, f.name)),
        b("▶ Run it", () => run(e.name, f.name)),
        b("Delete fragment", () => { removeFragment(e, f); })));
      box.append(el("p", "idoc", "A fragment is one batch loop: one source, transforms along the branches, sinks at the ends — " +
        "or a single action. Its sinks commit at its end, all or nothing; the next fragment starts after."));
    }

    function inspectEdge(e, f, s, box) {
      box.append(el("div", "ihead", el("span", "ig", "→"), el("b", null, "wire"), el("span", "ik", f.name)));
      box.append(el("p", null, "Rows flow from ", el("b", null, s.from), " into ", el("b", null, s.to), ". Each batch " +
        s.from + " hands on goes to every node it is wired to — a copy each, so one branch's changes never reach another."));
      const x = el("button", { type: "button" }, "Remove the wire (Delete)");
      x.addEventListener("click", () => removeEdge(e, f, s.from, s.to));
      box.append(el("div", "iacts", x));
    }

    function inspectNode(e, f, n, box) {
      const p = reg && reg.byName.get(n.plugin);
      const kind = p ? p.kind : "";
      box.append(el("div", "ihead", el("span", "ig", GLYPH[kind] || "?"), el("b", null, n.plugin), el("span", "ik", kind || "unknown plugin")));
      if (p && p.doc) box.append(el("details", "idocs", el("summary", null, p.label || "about"), el("p", null, p.doc)));
      const id = el("input", { value: n.id, spellcheck: "false" });
      id.addEventListener("change", () => {
        const why = renameNode(e, f, n.id, id.value.trim());
        if (why) { host.status(why, "warn"); id.value = n.id; }
      });
      id.addEventListener("keydown", (ev) => { if (ev.key === "Enter") { ev.preventDefault(); id.blur(); } });
      box.append(row("id", id, "Its name in the fragment: wires, the log and the record use it"));
      const broke = !p && reg && reg.problems.find((x) => x.name === n.plugin);
      if (broke) {
        box.append(el("p", "idiag error", n.plugin + " did not load from " + broke.file.split("/").pop() + ": " + broke.error +
          " — Ctrl+O → Plugins opens the file."));
      } else if (!p) {
        box.append(el("p", "idiag error", "No plugin " + n.plugin + " — `dbc plugins` lists them."));
      } else {
        for (const fd of p.fields) box.append(field(e, f, n, fd));
        const known = new Set(p.fields.map((x) => x.name));
        for (const k of Object.keys(n.cfg)) {
          if (known.has(k)) continue;
          const x = el("button", { type: "button", class: "linkish" }, "remove");
          x.addEventListener("click", () => { delete n.cfg[k]; changed(e); box.dataset.sel = ""; renderInspector(); });
          box.append(el("div", "idiag error", "“" + k + "” is not a field of " + n.plugin + " ", x));
        }
      }
      const b = (label, title, act) => { const x = el("button", { type: "button", title }, label); x.addEventListener("click", act); return x; };
      box.append(el("div", "iacts",
        b("Duplicate", "Ctrl+D", () => duplicateNode(e, f, n.id)),
        b("Delete node", "Delete", () => removeNode(e, f, n.id))));
    }

    // field draws one plugin field as its type wants:
    //   string, duration, int  a line          bool  a checkbox
    //   enum                   a picker        text  a few lines
    //   conn                   a line offering the connections
    //   columns                a line offering the columns previews saw
    //   sql, go                a code box (Tab indents)
    // Every value stays a string, as the spec holds it; ${…} works anywhere.
    function field(e, f, n, fd) {
      const cur = n.cfg[fd.name];
      const set = (v) => {
        if (v === "" || v === undefined) delete n.cfg[fd.name];
        else n.cfg[fd.name] = v;
        changed(e);
      };
      const label = fd.name + (fd.required ? " *" : "");
      const doc = fd.doc + (fd.default ? " (default " + fd.default + ")" : "");
      let input;
      switch (fd.type) {
        case "bool": {
          const truthy = (v) => /^(true|yes|on|1)$/i.test(String(v || "").trim());
          input = el("input", { type: "checkbox" });
          input.checked = truthy(cur !== undefined ? cur : fd.default);
          // unchecked writes "false" only when the default is true; else the
          // key goes, as a file written by hand would leave it
          input.addEventListener("change", () => set(input.checked ? "true" : truthy(fd.default) ? "false" : ""));
          return el("label", "ifield check", input, el("span", "iname", label), el("span", "idoc", doc));
        }
        case "enum": {
          input = el("select", null, el("option", { value: "" }, fd.default ? "default (" + fd.default + ")" : "—"),
            ...(fd.enum || []).map((v) => el("option", { value: v }, v)));
          input.value = cur || "";
          input.addEventListener("change", () => set(input.value));
          break;
        }
        case "text":
        case "sql":
        case "go": {
          input = el("textarea", { class: fd.type === "text" ? "" : "code", spellcheck: "false",
            rows: String(fd.type === "go" ? 12 : fd.type === "sql" ? 5 : 3), placeholder: fd.default || "" });
          input.value = cur || "";
          input.addEventListener("input", () => set(input.value));
          if (fd.type !== "text") {
            input.addEventListener("keydown", (ev) => {
              if (ev.key !== "Tab" || ev.ctrlKey || ev.metaKey || ev.altKey) return;
              ev.preventDefault();
              input.setRangeText(fd.type === "go" ? "\t" : "    ", input.selectionStart, input.selectionEnd, "end");
              set(input.value);
            });
          }
          break;
        }
        default: {
          input = el("input", { value: cur || "", placeholder: fd.default || "", spellcheck: "false",
            inputmode: fd.type === "int" ? "numeric" : undefined });
          const offer = fd.type === "conn" ? reg.conns : fd.type === "columns" ? Object.values(e.cols).flat() : null;
          if (offer && offer.length) {
            const listID = "pl-" + fd.name + "-" + Math.random().toString(36).slice(2, 7);
            input.setAttribute("list", listID);
            const dl = el("datalist", { id: listID }, ...[...new Set(offer)].map((v) => el("option", { value: v })));
            input.addEventListener("input", () => set(input.value));
            return el("label", "ifield", el("span", "iname", label, el("span", "itype", fd.type)), input, dl, el("span", "idoc", doc));
          }
          input.addEventListener("input", () => set(input.value));
        }
      }
      return row(label, input, doc, fd.type);
    }

    // useConn is a click on a connection in the sidebar while a pipeline
    // tab is on screen: into the selected node's connection field.
    function useConn(name) {
      const e = shown;
      const s = e && e.sel;
      const f = s && s.kind === "node" && fragOf(e, s.frag);
      const n = f && nodeOf(f, s.id);
      const p = n && reg && reg.byName.get(n.plugin);
      const fd = p && p.fields.find((x) => x.type === "conn");
      if (!fd) {
        host.status("select a node with a connection field, then click a connection to put it there", "");
        return;
      }
      n.cfg[fd.name] = name;
      changed(e);
      dom.insp.dataset.sel = "";
      renderInspector();
      host.status(n.id + "." + fd.name + " = " + name, "");
    }

    // ── the pointer ──────────────────────────────────────────────────────
    let drag = null;

    // laneAt is the lane under a client point, with the point in its body's
    // coordinates — or null.
    function laneAt(cx, cy) {
      const hit = document.elementFromPoint(cx, cy);
      const lane = hit && hit.closest(".plane");
      if (!lane || !dom.layer.contains(lane)) return null;
      const body = lane.querySelector(".lbody"), r = body.getBoundingClientRect(), k = stage.view.k;
      return { lane, frag: lane.dataset.frag, x: (cx - r.left) / k, y: (cy - r.top) / k, inBody: cy >= r.top };
    }

    function bindPointer() {
      const cv = dom.canvas;
      cv.addEventListener("pointerdown", (ev) => {
        if (ev.button !== 0 || !shown || !shown.spec) return;
        const e = shown;
        if (ev.target.closest("button")) return; // a lane's buttons: their click
        const port = ev.target.closest(".pport.out");
        const cardEl = ev.target.closest(".pcard");
        const laneEl = ev.target.closest(".plane");
        if (port && cardEl && laneEl) {
          const f = fragOf(e, laneEl.dataset.frag);
          const from = cardEl.dataset.id, p = posOf(f, from);
          const svg = laneEl.querySelector(".ledges");
          const wire = document.createElementNS("http://www.w3.org/2000/svg", "path");
          wire.setAttribute("class", "edge live");
          svg.append(wire);
          drag = { type: "wire", f, from, x0: p[0] + CARD_W, y0: p[1] + CARD_H / 2, wire, body: laneEl.querySelector(".lbody") };
          cv.setPointerCapture(ev.pointerId);
          ev.preventDefault();
          return;
        }
        if (cardEl && laneEl) {
          const f = fragOf(e, laneEl.dataset.frag), id = cardEl.dataset.id;
          const p = posOf(f, id);
          drag = { type: "node", f, id, el: cardEl, x0: p[0], y0: p[1], cx: ev.clientX, cy: ev.clientY, moved: false,
            svg: laneEl.querySelector(".ledges") };
          cv.setPointerCapture(ev.pointerId);
          cv.focus({ preventScroll: true });
          ev.preventDefault();
          return;
        }
        const hit = ev.target.closest(".ehit");
        if (hit && laneEl) {
          e.sel = { kind: "edge", frag: laneEl.dataset.frag, from: hit.dataset.from, to: hit.dataset.to };
          renderCanvas();
          renderInspector();
          cv.focus({ preventScroll: true });
          return;
        }
        if (ev.target.closest(".lhead") && laneEl) {
          e.sel = { kind: "frag", frag: laneEl.dataset.frag };
          renderCanvas();
          renderInspector();
          cv.focus({ preventScroll: true });
          return;
        }
        drag = { type: "pan", cx: ev.clientX, cy: ev.clientY, tx: stage.view.tx, ty: stage.view.ty, moved: false,
          add: !!ev.target.closest("[data-add]"), lane: laneEl ? laneEl.dataset.frag : "" };
        cv.setPointerCapture(ev.pointerId);
        cv.classList.add("panning");
        cv.focus({ preventScroll: true });
      });
      cv.addEventListener("pointermove", (ev) => {
        if (!drag) return;
        const k = stage.view.k;
        if (drag.type === "pan") {
          const dx = ev.clientX - drag.cx, dy = ev.clientY - drag.cy;
          if (Math.abs(dx) + Math.abs(dy) > 3) drag.moved = true;
          stage.set({ tx: drag.tx + dx, ty: drag.ty + dy });
        } else if (drag.type === "node") {
          const dx = (ev.clientX - drag.cx) / k, dy = (ev.clientY - drag.cy) / k;
          if (Math.abs(dx) + Math.abs(dy) > 3) drag.moved = true;
          if (!drag.moved) return;
          const x = Math.max(0, drag.x0 + dx), y = Math.max(0, drag.y0 + dy);
          drag.el.style.left = x + "px";
          drag.el.style.top = y + "px";
          drag.at = [x, y];
          const L = dom.geo.lanes.find((l) => l.f === drag.f);
          if (L) { L.pos[drag.id] = [x, y]; drawEdges(shown, drag.f, drag.svg, L.pos); }
        } else if (drag.type === "wire") {
          const r = drag.body.getBoundingClientRect();
          const x = (ev.clientX - r.left) / k, y = (ev.clientY - r.top) / k;
          drag.wire.setAttribute("d", curve(drag.x0, drag.y0, x, y));
          // the card it would land on lights up
          const hit = document.elementFromPoint(ev.clientX, ev.clientY);
          const target = hit && hit.closest(".pcard");
          dom.layer.querySelectorAll(".pcard.drop").forEach((c) => { if (c !== target) c.classList.remove("drop"); });
          if (target && target.dataset.id !== drag.from) target.classList.add("drop");
        }
      });
      const end = (ev) => {
        if (!drag) return;
        const d = drag;
        drag = null;
        cv.classList.remove("panning");
        const e = shown;
        if (!e) return;
        if (d.type === "pan") {
          if (d.moved) return;
          // a click: on the "+ fragment" strip adds one; on a lane's empty
          // body selects the lane; on the background, the pipeline
          if (d.add) { addFragment(e); return; }
          e.sel = d.lane ? { kind: "frag", frag: d.lane } : null;
          renderCanvas();
          renderInspector();
        } else if (d.type === "node") {
          if (!d.moved) {
            e.sel = { kind: "node", frag: d.f.name, id: d.id };
            renderCanvas();
            renderInspector();
            return;
          }
          d.f.ui[d.id] = [snap(d.at[0]), snap(d.at[1])];
          e.sel = { kind: "node", frag: d.f.name, id: d.id };
          changed(e);
        } else if (d.type === "wire") {
          d.wire.remove();
          dom.layer.querySelectorAll(".pcard.drop").forEach((c) => c.classList.remove("drop"));
          const hit = ev && document.elementFromPoint(ev.clientX, ev.clientY);
          const target = hit && hit.closest(".pcard");
          const laneEl = hit && hit.closest(".plane");
          if (!target || !laneEl) return;
          if (laneEl.dataset.frag !== d.f.name) {
            host.status("a wire stays inside its fragment — rows pass to the next fragment through the database", "warn");
            return;
          }
          const why = connect(e, d.f, d.from, target.dataset.id);
          if (why) host.status(why, "warn");
        }
      };
      cv.addEventListener("pointerup", end);
      cv.addEventListener("pointercancel", () => { if (drag && drag.wire) drag.wire.remove(); drag = null; cv.classList.remove("panning"); });
      cv.addEventListener("dblclick", (ev) => {
        // a double-click on a card puts the caret in its first field after
        // the id — what the node is for, usually its connection or query
        if (!ev.target.closest(".pcard")) return;
        const fields = dom.insp.querySelectorAll(".ifield input, .ifield textarea, .ifield select");
        const f = fields[1] || fields[0];
        if (f) f.focus();
      });

      // a lane's buttons
      cv.addEventListener("click", (ev) => {
        const b = ev.target.closest("button[data-lact]");
        const laneEl = b && b.closest(".plane");
        if (!b || !laneEl || !shown) return;
        const e = shown, f = fragOf(e, laneEl.dataset.frag);
        if (!f) return;
        if (b.dataset.lact === "preview") preview(e.name, f.name);
        else if (b.dataset.lact === "run") run(e.name, f.name);
        else {
          const r = b.getBoundingClientRect(), i = e.spec.fragments.indexOf(f);
          dbc.menu.open(r.left, r.bottom + 2, [
            { head: f.name },
            { label: "Preview this fragment", act: () => preview(e.name, f.name) },
            { label: "Save and run this fragment", act: () => run(e.name, f.name) },
            { head: "" },
            { label: "Move up", why: i > 0 ? "" : "it is the first", act: () => moveFragment(e, f, -1) },
            { label: "Move down", why: i < e.spec.fragments.length - 1 ? "" : "it is the last", act: () => moveFragment(e, f, 1) },
            { label: "New fragment above", act: () => addFragment(e, i) },
            { label: "New fragment below", act: () => addFragment(e, i + 1) },
            { head: "" },
            { label: "Delete fragment", act: () => removeFragment(e, f) },
          ]);
        }
      });

      // the palette: press, drag onto the canvas, release
      dom.palList.addEventListener("pointerdown", (ev) => {
        const item = ev.target.closest(".pitem[data-plugin]");
        if (!item || ev.button !== 0 || !shown || !shown.spec || !reg) return;
        ev.preventDefault();
        const p = reg.byName.get(item.dataset.plugin);
        const ghost = el("div", "pghost k-" + p.kind, el("span", "cg", GLYPH[p.kind]), " " + p.name);
        const start = { x: ev.clientX, y: ev.clientY };
        let moved = false;
        const move = (m) => {
          if (!moved && Math.abs(m.clientX - start.x) + Math.abs(m.clientY - start.y) < 4) return;
          if (!moved) { moved = true; document.body.append(ghost); }
          ghost.style.left = m.clientX + 8 + "px";
          ghost.style.top = m.clientY + 8 + "px";
          const L = laneAt(m.clientX, m.clientY);
          dom.layer.querySelectorAll(".plane.drop").forEach((x) => { if (!L || x !== L.lane) x.classList.remove("drop"); });
          if (L) L.lane.classList.add("drop");
        };
        const up = (u) => {
          document.removeEventListener("pointermove", move, true);
          document.removeEventListener("pointerup", up, true);
          ghost.remove();
          dom.layer.querySelectorAll(".plane.drop").forEach((x) => x.classList.remove("drop"));
          const e = shown;
          if (!e || !e.spec) return;
          if (!moved) { dropAtSelection(e, p); return; }
          const r = dom.canvas.getBoundingClientRect();
          if (u.clientX < r.left || u.clientX > r.right || u.clientY < r.top || u.clientY > r.bottom) return; // dropped outside
          const L = laneAt(u.clientX, u.clientY);
          if (L && fragOf(e, L.frag)) {
            addNode(e, fragOf(e, L.frag), p, L.x - CARD_W / 2, Math.max(0, (L.inBody ? L.y : 0) - CARD_H / 2));
          } else {
            const f = addFragment(e, undefined, true);
            addNode(e, f, p, PAD, PAD);
          }
          dom.canvas.focus({ preventScroll: true });
        };
        document.addEventListener("pointermove", move, true);
        document.addEventListener("pointerup", up, true);
      });
    }

    // dropAtSelection is a palette click without a drag: the node joins
    // the selected fragment (the last one when none is), right of the last
    // card — and is wired from the selected node when it can be.
    function dropAtSelection(e, p) {
      const s = e.sel;
      let f = s && fragOf(e, s.frag);
      if (!f) f = e.spec.fragments[e.spec.fragments.length - 1] || addFragment(e, undefined, true);
      let x = PAD, y = PAD;
      for (const n of f.nodes) { const q = posOf(f, n.id); if (q[0] + CARD_W + 56 > x) { x = q[0] + CARD_W + 56; y = q[1]; } }
      const from = s && s.kind === "node" && s.frag === f.name ? s.id : "";
      const id = addNode(e, f, p, x, y);
      if (from) {
        const why = connect(e, f, from, id);
        if (!why) e.sel = { kind: "node", frag: f.name, id };
        renderCanvas();
        renderInspector();
      }
    }

    function bindKeys() {
      dom.canvas.addEventListener("keydown", (ev) => {
        const e = shown;
        if (!e || !e.spec) return;
        const s = e.sel, f = s && fragOf(e, s.frag);
        if ((ev.key === "Delete" || ev.key === "Backspace") && f) {
          ev.preventDefault();
          if (s.kind === "node") removeNode(e, f, s.id);
          else if (s.kind === "edge") removeEdge(e, f, s.from, s.to);
          else if (s.kind === "frag" && !f.nodes.length) removeFragment(e, f);
          else if (s.kind === "frag") host.status("a fragment with nodes is deleted from its ⋯ menu", "");
        } else if ((ev.ctrlKey || ev.metaKey) && ev.key.toLowerCase() === "d" && s && s.kind === "node" && f) {
          ev.preventDefault();
          duplicateNode(e, f, s.id);
        } else if (ev.key === "Escape" && s) {
          e.sel = null;
          renderCanvas();
          renderInspector();
        } else if (ev.key === "f" && !ev.ctrlKey && !ev.metaKey && !ev.altKey) {
          fit();
        }
      });
    }

    // ── the JSON view ────────────────────────────────────────────────────
    // toggleJSON swaps the canvas for the editor showing the same text,
    // and back — which needs the text to parse: the canvas draws a spec.
    function toggleJSON(name) {
      const e = files.get(name);
      if (!e) return;
      if (e.json) {
        syncFromEditor(e);
        if (e.parseErr) { host.status("the JSON does not parse — fix it to go back to the canvas: " + e.parseErr, "warn"); return; }
        e.json = false;
        host.setJSON(false);
        render();
        return;
      }
      e.json = true;
      dbc.editor.useDoc(docKey(name), e.text, JSON_LANG);
      dbc.editor.replaceDoc(docKey(name), e.text);
      e.parseErr = "";
      host.setJSON(true);
      check(name);
      render();
      dbc.editor.focus();
    }

    // ── runs ─────────────────────────────────────────────────────────────
    const PIPE_LOG = "\u0001pipeline:";
    const logKey = (name) => PIPE_LOG + name;

    // askParams resolves to the params for a run of e, asking for those the
    // spec gives no default; null when cancelled. With all defaulted it
    // asks nothing (a run is one keystroke).
    function askParams(e, always) {
      const ps = (e.spec && e.spec.params) || {};
      const names = Object.keys(ps).sort();
      const last = lastParams.get(e.name) || {};
      if (!names.length || (!always && names.every((k) => (ps[k] || {}).default || last[k]))) {
        return Promise.resolve(Object.assign({}, last));
      }
      return new Promise((resolve) => {
        let done = false;
        const finish = (v) => { if (!done) { done = true; resolve(v); } };
        const inputs = names.map((k) => el("input", { value: last[k] !== undefined ? last[k] : (ps[k] || {}).default || "",
          "data-k": k, spellcheck: "false", placeholder: (ps[k] || {}).default ? "default " + ps[k].default : "required" }));
        const go = el("button", { type: "button", class: "primary" }, "Run");
        const no = el("button", { type: "button" }, "Cancel");
        const submit = () => {
          const out = {};
          for (const i of inputs) if (i.value !== "" && i.value !== (ps[i.dataset.k] || {}).default) out[i.dataset.k] = i.value;
          lastParams.set(e.name, out);
          finish(out);
          dbc.modal.close();
        };
        go.addEventListener("click", submit);
        no.addEventListener("click", () => dbc.modal.close());
        for (const i of inputs) i.addEventListener("keydown", (ev) => { if (ev.key === "Enter") { ev.preventDefault(); submit(); } });
        dbc.modal.open({ title: "Parameters · " + e.name, focus: inputs[0], onClose: () => finish(null),
          body: el("div", "confirm pparams", ...names.map((k, i) => row(k, inputs[i], (ps[k] || {}).doc || ""))),
          foot: el("div", "mfoot", go, no) });
      });
    }

    // preview runs the text on screen — saved or not — as a preview of
    // fragment ("" for all), into this tab's grid.
    async function preview(name, fragment) {
      const e = files.get(name), t = host.tabOf(name);
      if (!e || !t || !t.ws) return;
      if (e.json) syncFromEditor(e);
      const params = await askParams(e, false);
      if (params === null) return;
      try {
        await api("POST", "/api/v1/pipeline-preview", { ws: t.ws, name, text: e.text, fragment, rows, params });
      } catch (err) {
        host.status(err.message, err.status === 409 ? "warn" : "err");
        log(err.status === 409 ? "warn" : "err", "preview: " + err.message, logKey(name));
      }
    }

    // run saves, then runs the file — fragment alone when given; ask puts
    // up the parameters dialog even when every one has a default (Run with
    // parameters…).
    async function run(name, fragment, ask) {
      const e = files.get(name), t = host.tabOf(name);
      if (!e || !t || !t.ws) return;
      if (!(await save(name))) {
        host.status(name + " was not run — it is not saved (see the log)", "warn");
        return;
      }
      const params = await askParams(e, !!ask);
      if (params === null) return;
      try {
        await api("POST", "/api/v1/pipeline-run", { ws: t.ws, name, fragment: fragment || "", params });
      } catch (err) {
        host.status(err.message, err.status === 409 ? "warn" : "err");
        log(err.status === 409 ? "warn" : "err", "run: " + err.message, logKey(name));
      }
    }

    // liveOf is the run tab t (by its workspace) has going, or null.
    function liveOf(ws) {
      for (const r of runs.values()) if (r.origin === ws && !r.ended) return r;
      return null;
    }

    // stop stops the run the tab started.
    async function stop(t) {
      const r = t && liveOf(t.ws);
      if (!r) { host.status("nothing of this tab's is running", ""); return; }
      try { await api("POST", "/api/v1/runs/" + encodeURIComponent(r.id) + "/cancel"); } catch (err) { log("err", err.message, logKey(r.source)); }
    }

    // track records a run header (job.run, job.done, GET /api/v1/runs).
    // newest: it is the newest run of its pipeline for certain (a job.run:
    // events come in the order they happen); otherwise the later start
    // wins. Not the id: ids sort by start only to the second, so two runs
    // in one second could compare either way.
    function track(h, newest) {
      let r = runs.get(h.id);
      if (!r) {
        r = { id: h.id, frags: new Map() };
        runs.set(h.id, r);
      }
      Object.assign(r, { name: h.name, source: h.source || (h.name ? h.name + ".json" : ""), origin: h.origin || "",
        preview: h.preview || 0, status: h.status, started: Date.parse(h.started), error: h.error || "",
        ended: h.status !== "running" });
      for (const f of ((h.pipelines || [])[0] || {}).fragments || []) r.frags.set(f.name, f);
      const prev = runs.get(latest.get(r.source));
      if (newest || !prev || prev === r || prev.started <= r.started) latest.set(r.source, r.id);
      // keep the page's memory bounded: the newest 60
      if (runs.size > 60) runs.delete(runs.keys().next().value);
      return r;
    }

    // summaryOf is a finished run in a line, as RunStats.String says it.
    function summaryOf(r, h) {
      const st = ((h.pipelines || [])[0]) || {};
      const fr = st.fragments || [];
      const rowsN = fr.reduce((n, f) => n + (f.rows || 0), 0);
      const took = Date.parse(h.ended) - Date.parse(h.started);
      return (r.preview ? "preview of " : "pipeline ") + r.name + ": " + dbc.plural(fr.length, "fragment") + ", " +
        rowsN + " rows in " + fmtDur(took) + " (" + h.status + ")" + (h.error ? " — " + h.error : "");
    }

    // JOB RUNS (a DAG of pipelines: web/jobs.go) come on the same job.*
    // events, but they are the job tab's (jobs.js) and the Runs view's
    // (runs.js), not a pipeline tab's: a job called "nightly" must not
    // light up nightly.json's canvas, and a job step's lines (named by
    // the step) must not land in a pipeline's log. So they are only
    // recognised here, and passed over. jobRuns: the job runs going now.
    const jobRuns = new Set();
    function onJobEvent(type, d) {
      if (type === "job.notice") return true; // the scheduler's word: jobs.js logs it
      const head = (type === "job.run" || type === "job.done") ? d.run : null;
      if (head && head.kind === "job") jobRuns.add(head.id);
      const id = head ? head.id : d.run;
      if (!jobRuns.has(id)) return false;
      if (type === "job.done") jobRuns.delete(id);
      return true;
    }

    // pluginsChanged is the "plugins" event: the server reloaded the
    // user's plugin files. The registry is read again, and what was drawn
    // from it — the palette, the cards, the inspector's form, the check's
    // marks — follows.
    async function pluginsChanged() {
      regP = null;
      try { reg = await registry(); } catch (err) { log("err", "plugins: " + err.message); return; }
      if (!dom || !shown) return;
      renderPalette();
      render();
      check(shown.name, false);
    }

    // onEvent is a window-level "pipelines" or "job.*" event.
    async function onEvent(type, d) {
      if (type === "pipelines") { onStore(d); return; }
      if (onJobEvent(type, d)) return;
      if (type === "job.run") {
        const r = track(d.run, true);
        host.runState(r, true);
        if (shown && shown.name === r.source) renderCanvas();
        log("accent", (r.preview ? "◎ preview of " : "▶ ") + r.name + (r.fragment ? " · " + r.fragment : "") + " started", logKey(r.source));
      } else if (type === "job.progress") {
        const r = runs.get(d.run);
        if (!r) return;
        r.frags.set(d.fragment.name, d.fragment);
        if (shown && shown.name === r.source && !drag) renderCanvas();
        host.progress(r, d.fragment);
      } else if (type === "job.line") {
        const r = runs.get(d.run);
        log(d.level === "err" ? "err" : "info", d.text, logKey(r ? r.source : d.name + ".json"));
      } else if (type === "job.preview") {
        const r = runs.get(d.run);
        const e = r && files.get(r.source);
        if (e) {
          const frag = d.title.replace(/^preview /, "").split("/")[0];
          e.cols[frag] = [...new Set([...(e.cols[frag] || []), ...(d.cols || [])])];
        }
      } else if (type === "job.done") {
        // the log has the run's end already — the runner's summary, or the
        // engine's line for a failure or a stop — so only the status bar
        // and the tab's marks take it from here
        const r = track(d.run);
        const line = summaryOf(r, d.run);
        host.runState(r, false, line);
        if (shown && shown.name === r.source) renderCanvas();
      }
    }

    // sync is a page that loaded (or reconnected) mid-run catching up:
    // the runs going now and lately, from the engine.
    async function sync() {
      let got;
      try { got = await api("GET", "/api/v1/runs"); } catch (_) { return; }
      const pipe = (h) => h.kind !== "job";
      for (const h of got.running || []) if (!pipe(h)) jobRuns.add(h.id);
      for (const h of [...(got.recent || [])].reverse().filter(pipe)) track(h);
      for (const h of (got.running || []).filter(pipe)) host.runState(track(h), true);
      if (shown) renderCanvas();
    }

    // onStore is the "pipelines" event: a save, rename, trash or restore,
    // in any window (this one's too: they are idempotent).
    async function onStore(d) {
      const e = files.get(d.name);
      if (d.op === "saved") {
        if (d.win === host.win() || !e || isDirty(e) || d.rev === e.rev) return;
        try {
          const f = await api("GET", path(d.name));
          if (!isDirty(e)) adopt(e, f.text, f.rev);
        } catch (_) { /* the next save meets the conflict */ }
      } else if (d.op === "renamed") {
        renamed(d.name, d.to);
      } else if (d.op === "trashed") {
        if (!e || !e.rev) return;
        e.rev = "";
        e.saved = "";
        changedView(e);
        log("warn", d.name + " was moved to the trash — its tab keeps the text; Ctrl+S saves it again", logKey(d.name));
      }
    }

    function renamed(from, to) {
      const e = files.get(from);
      if (e && !files.has(to)) {
        files.delete(from);
        e.name = to;
        files.set(to, e);
        dbc.editor.renameDoc(docKey(from), docKey(to));
        const d = readDraft(from);
        writeDraft(from, null);
        if (d) writeDraft(to, d);
      }
      if (latest.has(from)) { latest.set(to, latest.get(from)); latest.delete(from); }
      for (const r of runs.values()) if (r.source === from) r.source = to;
      dbc.moveLog(logKey(from), logKey(to));
      host.renamed(from, to);
      if (shown === e) render();
    }

    // ── actions on a pipeline, from the browser or a tab's menu ─────────
    async function list() {
      return api("GET", "/api/v1/pipelines");
    }

    // make writes text as a new pipeline, asking for its name, then opens
    // it. A name taken (or refused) asks again.
    async function make(suggest, text, what) {
      let taken = [];
      try { taken = (await list()).pipelines.map((p) => p.name); } catch (_) { /* the server will say */ }
      let name = pipeName(suggest), i = 2;
      while (taken.includes(name)) name = stemOf(pipeName(suggest)) + "-" + i++ + ".json";
      let hint = "Letters, digits, '.', '-' and '_', ending in .json.";
      for (;;) {
        name = pipeName(await dbc.scripts.ask({ title: what, hint, value: name, ok: "Create" }));
        if (!name) return null;
        // the spec's own name follows the file's, as dbc expects them to agree
        const p = parseSpec(text);
        const body = p.spec ? specText(Object.assign(p.spec, { name: stemOf(name) })) : text;
        try {
          await api("PUT", path(name) + host.winQuery(), { text: body, base: "" });
          break;
        } catch (err) {
          if (err.status !== 409 && err.status !== 400) { log("err", what + ": " + err.message); return null; }
          hint = err.message;
        }
      }
      log("ok", "created " + name + " — drag plugins onto a lane, ◎ previews, Ctrl+Enter saves and runs", logKey(name));
      await edit(name);
      return name;
    }

    // newPipeline starts one on the tab's connection: a source into a
    // preview, the smallest pipeline that shows something.
    function newPipeline() {
      const conn = host.conn() || "demo-sqlite";
      const spec = { name: "pipeline", fragments: [{ name: "load", nodes: [
        { id: "src", plugin: "sql.read", cfg: { conn, query: "SELECT 1 AS one" } },
        { id: "peek", plugin: "preview", cfg: { rows: "50" } }],
      edges: [["src", "peek"]], ui: { src: [PAD, PAD], peek: [PAD + CARD_W + 80, PAD] } }] };
      return make("pipeline.json", specText(spec), "New pipeline");
    }

    async function copyExample(name) {
      let r;
      try { r = await api("GET", "/api/v1/pipeline-examples/" + encodeURIComponent(name)); } catch (err) { log("err", err.message); return null; }
      return make(name, r.text, "Copy the example " + name);
    }

    async function duplicate(name) {
      const e = files.get(name);
      let text = e ? e.text : null;
      if (text === null) {
        try { text = (await api("GET", path(name))).text; } catch (err) { log("err", err.message); return; }
      }
      await make(name, text, "Duplicate " + name);
    }

    async function edit(name) {
      try { await load(name); } catch (err) { log("err", "could not open " + name + ": " + err.message); return; }
      host.open(name);
    }

    async function rename(name) {
      const to = pipeName(await dbc.scripts.ask({ title: "Rename " + name, value: name, ok: "Rename",
        hint: "Letters, digits, '.', '-' and '_', ending in .json." }));
      if (!to || to === name) return;
      const e = files.get(name);
      if (e && isDirty(e)) { if (!(await save(name))) return; }
      try {
        await api("POST", path(name) + "/rename" + host.winQuery(), { to });
      } catch (err) { log("err", "rename: " + err.message); return; }
      renamed(name, to);
      // the spec's name follows when it was the file's
      const f = files.get(to);
      if (f && f.spec && f.spec.name === stemOf(name)) {
        f.spec.name = stemOf(to);
        changed(f);
        await save(to);
      }
      log("ok", "renamed " + name + " to " + to, logKey(to));
    }

    async function trash(name) {
      let r;
      try { r = await api("DELETE", path(name) + host.winQuery()); } catch (err) { log("err", "trash: " + err.message); return; }
      const e = files.get(name);
      if (e) { e.rev = ""; e.saved = ""; changedView(e); }
      log("info", "moved " + name + " to the trash (" + r.id + ") — Ctrl+O → Trash restores it", logKey(name));
    }

    async function restore(t) {
      try {
        const r = await api("POST", "/api/v1/pipeline-trash/" + encodeURIComponent(t.id) + "/restore" + host.winQuery(), {});
        log("ok", "restored " + r.name);
        await edit(r.name);
      } catch (err) {
        if (err.status !== 409) { log("err", "restore: " + err.message); return; }
        const to = pipeName(await dbc.scripts.ask({ title: "Restore " + t.name + " as", value: t.name, ok: "Restore", hint: err.message }));
        if (!to) return;
        try {
          const r = await api("POST", "/api/v1/pipeline-trash/" + encodeURIComponent(t.id) + "/restore" + host.winQuery(), { to });
          await edit(r.name);
        } catch (e2) { log("err", "restore: " + e2.message); }
      }
    }

    async function copyPath(name) {
      try {
        const l = await list();
        dbc.clip.copyText(l.dir + "/" + name, "the pipeline's path");
      } catch (err) { log("err", err.message); }
    }

    // exportGo saves, then asks the server for the builder form and makes
    // it a new script in a script tab: the same pipeline as Go, to change
    // where the canvas cannot (a Go func as a node, a loop over tables).
    async function exportGo(name) {
      if (!(await save(name))) return;
      let r;
      try { r = await api("GET", "/api/v1/pipeline-export/" + encodeURIComponent(name)); } catch (err) { log("err", "export: " + err.message, logKey(name)); return; }
      await host.scripts().make(stemOf(name).replace(/[^A-Za-z0-9_.\-]/g, "_") + ".go", r.text, "Export " + name + " as Go");
    }

    // items is the menu of things to do with a pipeline (its tab's
    // right-click, the browser's ⋯, the canvas's ⋯).
    function items(name) {
      return [
        { label: "Open in a tab", act: () => edit(name) },
        { label: "Save and run", key: "Ctrl+Enter", act: async () => { await edit(name); run(name, ""); } },
        { label: "Run with parameters…", act: async () => { await edit(name); run(name, "", true); } },
        { label: "Preview", act: async () => { await edit(name); preview(name, ""); } },
        { label: "Export as Go…", act: () => exportGo(name) },
        { label: "Duplicate…", act: () => duplicate(name) },
        { label: "Rename…", act: () => rename(name) },
        { label: "Move to the trash", act: () => trash(name) },
        { label: "Copy path", act: () => copyPath(name) },
      ];
    }

    return {
      load, show, hide, save, edited, check, flush, forget, onEvent, pluginsChanged, sync, preview, run, stop, liveOf,
      rename, duplicate, trash, restore, copyPath, exportGo, items, list, edit, newPipeline, copyExample, useConn,
      toggleJSON, logKey,
      entry: (name) => files.get(name) || null,
      dirty: (name) => isDirty(files.get(name)),
      diags: (name) => (files.get(name) || {}).diags || [],
      json: (name) => !!(files.get(name) || {}).json,
    };
  }

  dbc.pipelines = { create, specText, parseSpec };
})();
