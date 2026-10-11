// dbc web — job tabs: a job (a DAG of pipelines, package jobs) drawn and
// edited as a canvas — a card per step, laid out by the server, wired by
// dragging from one card's output to another — with an inspector for the
// job's triggers and policy and each step's pipeline and params, and its
// runs: started here, watched on the cards, and drilled into in the tab's
// Runs face (runs.js).
//
// app.js owns the tabs; this module owns what a job tab edits and draws,
// as pipelines.js does for a pipeline tab, and shares its shapes: the
// same #jobp box over the editor (class .pipe), the same palette, canvas
// and inspector styles, the same JSON view.
//
//	┌ #jobp ─────────────────────────────────────────────────────────────────┐
//	│ ⧉ nightly.json ●  · The demo pipelines as a DAG    ⊞ Design ◷ Runs ⊡ { } ⋯ │
//	├ pipelines ┬ canvas (stage.js: drag the background, wheel, Ctrl+wheel) ┬ inspector ─┤
//	│ ⌕ filter  │ ┌ copy ──────┐      ┌ clean ─────┐      ┌ report ───┐     │ job        │
//	│ YOURS     │ │ ✓ copy   ◉ ●┼──┬──►●✓ clean     ●┼──┬──►●✓ report  │     │ triggers   │
//	│ orders    │ │ ⛓ copy-cats │  │   │ ⛓ clean-and…│  │   │ ⛓ cats-re…│     │  0 2 * * * │
//	│ EXAMPLES  │ └─────────────┘  │   └─────────────┘  │   └──────────┘     │   next: …  │
//	│ copy-cats │                  │   ┌ breeds ────┐   │                    │ policy     │
//	│ …         │                  └──►●✓ breeds    ●┼──┘                    │ ▶ Run      │
//	└───────────┴───────────────────────────────────────────────────────────┴────────────┘
//	  ◷ Runs swaps the canvas for this job's runs: the list, a run page
//
// THE MODEL is the spec (jobs.Spec as JSON). Every canvas edit changes the
// parsed spec and writes its text again as the Go side writes it (Spec.JSON:
// two-space indent, struct order, omitempty, arrays of scalars on one
// line), so a file saved from here diffs like one saved by dbc; the JSON
// view (the editor, in the canvas's place) edits the same text.
//
// THE LAYOUT IS THE SERVER'S. A job has no positions in its file: the cards
// are placed by package dag (ranked left to right by how far downstream a
// step is, ordered to uncross the edges), the same algorithm the ERD uses,
// and POST /api/v1/job-check returns it with the diags — so a structural
// edit (a step added, a wire drawn) asks for the check at once, and the
// cards move to where the layout puts them when it lands. A card cannot be
// dragged to a place of its own; a reader finds a step where the
// dependencies put it.
//
// THE FILE is a job in jobs_dir, saved only when asked (Ctrl+S, ⤓ Save,
// ▶ Run, which saves first: what runs is the file, as cron and the
// scheduler would run it), against the revision it was read at; unsaved
// text survives a reload in localStorage, per window (drafts.js,
// dbc.job.draft.<owner>:<name>), as a pipeline's does.
//
// RUNS are the server's engine's. ▶ Run saves and starts one with this tab
// as its origin (its Stop, its busy mark, the grid any preview sink's rows
// land in) and turns the tab to its Runs face, on the new run's page. The
// cards show the newest run's state per step — this page's, a scheduled
// one's, another window's — as the job.* events move it.
//
// THE POINTER (pointer events, as on the pipeline canvas):
//
//	press on…               drag does                  release does
//	a palette pipeline      a ghost follows            on a card: a step waiting for that card;
//	                                                   elsewhere on the canvas: a step (after the
//	                                                   selected one, or the first step: the root);
//	                                                   no drag: after the selected step
//	a card's output ●       a wire follows             on another card: it now waits for this one
//	a card                  —                          selects it
//	a wire                  —                          selects it (Delete removes the dependency)
//	the background          pans                       no drag: selects the job
(function () {
  "use strict";

  const dbc = window.dbc;
  const { api, log, el } = dbc;

  // NAME is pipeline.ValidName: a step id, a job or parameter name.
  const NAME = /^[A-Za-z0-9_][A-Za-z0-9_.\-]{0,99}$/;
  const validName = (s) => NAME.test(s) && !s.endsWith(".");
  const jobName = (s) => (s && !/\.json$/i.test(s) ? s + ".json" : s);
  const stemOf = (name) => String(name || "").replace(/\.json$/i, "");
  const JSON_LANG = "dbcjson";
  const STATE = () => dbc.runs.STATE;
  const LIVE = (s) => s === "running" || s === "queued";

  // ── the spec as text ───────────────────────────────────────────────────
  // jobText writes a job as jobs.Spec.JSON does: the struct's fields in
  // order, omitempty ones left out when empty, map keys sorted, two-space
  // indent, then every array of scalars (an after, the schedule) on one
  // line — pipeline.CompactArrays' pattern.
  const SCALARS = /\[\s*((?:"[^"\\\n]*"|-?[0-9.eE+\-]+)(?:,\s*(?:"[^"\\\n]*"|-?[0-9.eE+\-]+))*)\s*\]/g;
  function sorted(obj, f) {
    const out = {};
    for (const k of Object.keys(obj || {}).sort()) out[k] = f ? f(obj[k]) : obj[k];
    return out;
  }
  // keepRest copies the keys of from that the job canvas has no field for into o,
  // after o's own and in the file's order. A canvas edit writes the text
  // again from what the canvas knows, so without this an unknown key — a
  // typo such as "on_eror" — was dropped by the first edit, silently.
  // Kept, the check goes on naming it ("unknown field") until it is put
  // right in the JSON view; refusing canvas edits instead would stop all
  // work over one typo.
  function keepRest(o, from, known) {
    if (from && typeof from === "object" && !Array.isArray(from)) {
      for (const k of Object.keys(from)) if (!known.includes(k)) o[k] = from[k];
    }
    return o;
  }
  function jobText(spec) {
    const o = { name: spec.name || "" };
    if (spec.desc) o.desc = spec.desc;
    if (spec.root) o.root = spec.root;
    if (spec.params && Object.keys(spec.params).length) {
      o.params = sorted(spec.params, (p) => keepRest(p && p.doc ? { default: p.default || "", doc: p.doc } : { default: (p && p.default) || "" },
        p, ["default", "doc"]));
    }
    o.pipelines = (spec.pipelines || []).map((st) => {
      const m = { id: st.id || "", pipeline: st.pipeline || "" };
      if (st.after && st.after.length) m.after = st.after.slice();
      if (st.params && Object.keys(st.params).length) m.params = sorted(st.params);
      return keepRest(m, st, ["id", "pipeline", "after", "params"]);
    });
    const t = spec.triggers || {}, tr = {};
    if (t.schedule && t.schedule.length) tr.schedule = t.schedule.slice();
    if (t.tz) tr.tz = t.tz;
    if (t.catch_up) tr.catch_up = true;
    if (t.webhook) tr.webhook = true;
    o.triggers = keepRest(tr, t, ["schedule", "tz", "catch_up", "webhook"]);
    const p = spec.policy || {}, po = {};
    if (p.on_failure) po.on_failure = p.on_failure;
    if (p.max_parallel) po.max_parallel = p.max_parallel;
    if (p.overlap) po.overlap = p.overlap;
    if (p.timeout) po.timeout = p.timeout;
    o.policy = keepRest(po, p, ["on_failure", "max_parallel", "overlap", "timeout"]);
    keepRest(o, spec, ["name", "desc", "root", "params", "pipelines", "triggers", "policy"]);
    return JSON.stringify(o, null, 2).replace(SCALARS, (m) =>
      "[" + m.slice(1, -1).split(",").map((s) => s.trim()).join(", ") + "]") + "\n";
  }

  // parseJob reads text into a spec the canvas can draw — every list and
  // map there, even where the file left it out — or says why it cannot.
  function parseJob(text) {
    let s;
    try { s = JSON.parse(text); } catch (e) { return { err: e.message }; }
    if (!s || typeof s !== "object" || Array.isArray(s)) return { err: "a job is a JSON object" };
    if (!Array.isArray(s.pipelines)) s.pipelines = [];
    for (const st of s.pipelines) {
      if (!st || typeof st !== "object") return { err: "a step is an object" };
      if (!Array.isArray(st.after)) st.after = [];
      if (!st.params || typeof st.params !== "object") st.params = {};
    }
    if (!s.params || typeof s.params !== "object") s.params = {};
    if (!s.triggers || typeof s.triggers !== "object") s.triggers = {};
    if (!Array.isArray(s.triggers.schedule)) s.triggers.schedule = [];
    if (!s.policy || typeof s.policy !== "object") s.policy = {};
    return { spec: s };
  }

  function freeID(base, taken) {
    let n = base, i = 2;
    while (taken.includes(n)) n = base + i++;
    return n;
  }

  // a cron fire as the inspector shows it: "Thu 02:00", with the date when
  // it is not this week
  function fireText(iso) {
    const d = new Date(iso);
    const soon = d.getTime() - Date.now() < 6 * 864e5;
    return d.toLocaleDateString("en-US", soon ? { weekday: "short" } : { weekday: "short", month: "short", day: "numeric" }) +
      " " + d.toTimeString().slice(0, 5);
  }

  // drafts: unsaved text per job, per window, in this browser (drafts.js:
  // a closed window's is adopted by the next to open the file, a live
  // window's left alone). Storage may refuse (a private window); a draft is
  // only a safety net, so a failure costs nothing but the net.
  const drafts = dbc.drafts.kit("dbc.job.draft.", log);

  function create(host) {
    // files: job file → what this window has of it:
    //   rev, saved, text, spec, parseErr   as a pipeline tab's (pipelines.js)
    //   diags, fires   the last check's findings, and each cron line's next fires
    //   layout         the last check's layout ({nodes, w, h, card})
    //   json           the JSON view is on
    //   face           "design" (the canvas) or "runs" (this job's runs)
    //   sel            {kind: "step"|"edge", id, from, to} or null (the job)
    //   view           the canvas's pan and zoom
    //   run            the run whose states the cards show (its id)
    const files = new Map();
    const path = (name) => "/api/v1/jobs/" + encodeURIComponent(name);
    const docKey = (name) => "j:" + name;
    const isDirty = (e) => !!e && e.text !== e.saved;
    const JOB_LOG = "\u0001job:";
    const logKey = (name) => JOB_LOG + name;

    let shown = null, dom = null, stage = null, runsView = null;
    let pipes = null;      // the pipelines the palette offers: {mine: [{name, desc}], examples: […]}
    let paletteQ = "";
    const lastParams = new Map();
    const pipeParams = new Map(); // pipeline name → a promise of its params ({name: {default, doc}})

    // ── the file ─────────────────────────────────────────────────────────
    async function load(name) {
      const f = await api("GET", path(name));
      let e = files.get(name);
      if (e) {
        if (!isDirty(e)) adopt(e, f.text, f.rev);
        return e;
      }
      e = { name, rev: f.rev, saved: f.text, text: f.text, spec: null, diags: [], fires: [], layout: null, json: false,
        face: "design", sel: null, view: null, run: "", runLooked: false };
      e.spec = parseJob(f.text).spec || null;
      const { d, liveN } = drafts.adopt(name);
      if (!d && liveN) {
        log("info", name + " has unsaved edits in another dbc web window — this tab shows the saved file; " +
          "save there first to see them here", logKey(name));
      }
      if (d && d.text !== f.text) {
        e.text = d.text;
        e.rev = d.base || "";
        e.spec = parseJob(d.text).spec || null;
        log("info", name + ": unsaved changes from before were put back — Ctrl+S saves them", logKey(name));
      } else if (d) drafts.write(name, null);
      files.set(name, e);
      check(name);
      return e;
    }

    function adopt(e, text, rev) {
      Object.assign(e, { text, saved: text, rev, spec: parseJob(text).spec || null, parseErr: "" });
      drafts.write(e.name, null);
      dbc.editor.replaceDoc(docKey(e.name), text);
      changedView(e);
      check(e.name);
    }

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
          try {
            const f = await api("GET", path(name));
            return conflicted(e, text, f.text, f.rev);
          } catch (_) { /* fall through */ }
        }
        log("err", name + " was not saved: " + err.message, logKey(name));
        return false;
      }
      if (r.conflict && !r.rev) {
        e.rev = "";
        log("warn", name + " is gone from the jobs directory — Ctrl+S writes it again", logKey(name));
        return save(name);
      }
      if (r.conflict) return conflicted(e, text, r.text, r.rev);
      Object.assign(e, { rev: r.rev, saved: text });
      drafts.write(name, null);
      changedView(e);
      return true;
    }

    // conflicted: the file changed elsewhere since this tab read it — keep
    // this tab's (written over it) or load the file's.
    function conflicted(e, mine, theirs, rev) {
      return new Promise((resolve) => {
        const keep = el("button", { type: "button", class: "primary" }, "Save mine over it");
        const take = el("button", { type: "button" }, "Load the file's");
        const no = el("button", { type: "button" }, "Cancel");
        let done = false;
        const finish = (v) => { if (!done) { done = true; resolve(v); } };
        keep.addEventListener("click", async () => { dbc.modal.close(); e.rev = rev; finish(await save(e.name)); });
        take.addEventListener("click", () => {
          dbc.modal.close();
          adopt(e, theirs, rev);
          log("info", e.name + ": the file's version is loaded", logKey(e.name));
          finish(false);
        });
        no.addEventListener("click", () => dbc.modal.close());
        dbc.modal.open({
          title: e.name + " changed on disk", focus: no, onClose: () => finish(false),
          body: el("div", "confirm", el("p", null, e.name + " was saved elsewhere — another window, an editor — " +
            "since this tab read it. Saving this tab's version replaces that one.")),
          foot: el("div", "mfoot", keep, take, no),
        });
      });
    }

    // changed ends every canvas edit: the text written again, the draft
    // kept, the check asked — at once for a change of shape (the layout
    // comes with it), after a pause for typing.
    function changed(e, shape) {
      e.text = jobText(e.spec);
      dbc.editor.replaceDoc(docKey(e.name), e.text);
      storeDraft(e);
      scheduleCheck(e.name, shape ? 0 : 500);
      changedView(e);
    }

    function edited(name) {
      const e = files.get(name);
      if (!e || !e.json) return;
      syncFromEditor(e);
      storeDraft(e);
      scheduleCheck(name, 500);
      host.changed(name);
      renderBar();
    }

    function syncFromEditor(e) {
      const t = dbc.editor.docText(docKey(e.name));
      if (t === null || t === undefined) return;
      e.text = t;
      const p = parseJob(t);
      if (p.spec) e.spec = p.spec;
      e.parseErr = p.err || "";
    }

    // storeDraft writes e's draft a beat after its last edit (or drops it
    // once nothing is unsaved). Each file has its own timer: one shared
    // timer let an edit to a second file within the beat cancel the first
    // file's write.
    const draftOf = (e) => (isDirty(e) ? { base: e.rev, text: e.text, at: Date.now() } : null);
    function storeDraft(e) {
      clearTimeout(e.draftTimer);
      e.draftTimer = setTimeout(() => { e.draftTimer = 0; drafts.write(e.name, draftOf(e)); }, 300);
    }
    // flush writes every pending draft now (a tab switch, the page going)
    function flush() {
      for (const e of files.values()) {
        clearTimeout(e.draftTimer);
        e.draftTimer = 0;
        drafts.write(e.name, draftOf(e));
      }
    }

    function forget(name, discard) {
      const e = files.get(name);
      if (!e) return;
      clearTimeout(e.draftTimer);
      if (discard || !isDirty(e)) drafts.write(name, null);
      else drafts.write(name, draftOf(e));
      files.delete(name);
      dbc.editor.dropDoc(docKey(name));
      if (shown === e) shown = null;
    }

    function changedView(e) {
      host.changed(e.name);
      if (shown === e) render();
    }

    // ── the check (and the layout) ───────────────────────────────────────
    const checkTimers = new Map(), checkSeq = new Map();
    function scheduleCheck(name, ms) {
      clearTimeout(checkTimers.get(name));
      checkTimers.set(name, setTimeout(() => check(name), ms));
    }
    async function check(name, loud) {
      const e = files.get(name);
      if (!e) return [];
      const n = (checkSeq.get(name) || 0) + 1;
      checkSeq.set(name, n);
      let r;
      try { r = await api("POST", "/api/v1/job-check", { text: e.text }); } catch (err) {
        if (loud) log("err", "check: " + err.message, logKey(name));
        return e.diags;
      }
      if (checkSeq.get(name) !== n || files.get(name) !== e) return e.diags;
      e.diags = r.diags || [];
      e.fires = r.fires || [];
      if (r.layout) e.layout = r.layout;
      // the JSON view's markers: a diag names where it is ("clean.after"),
      // and the server places that on the text (jobs.Locate, which matches
      // it against the step ids the text declares); one it could not place
      // (line 0) is in the inspector's list only
      dbc.editor.setMarkers(docKey(name), e.diags.filter((d) => d.line > 0).map((d) => ({
        line: d.line, col: d.col, severity: d.severity, msg: (d.where ? d.where + ": " : "") + d.msg })));
      if (loud) {
        if (!e.diags.length) log("ok", "✓ " + name + " checks out", logKey(name));
        for (const d of e.diags) log(d.severity === "error" ? "err" : "warn", (d.where ? d.where + ": " : "") + d.msg, logKey(name));
      }
      host.changed(name);
      if (shown === e) { renderBar(); renderCanvas(); drawFires(); drawDiags(e); if (!dom.insp.contains(document.activeElement)) renderInspector(); }
      return e.diags;
    }

    // diagsAt sorts the check's findings: a step's — CheckJob names it by
    // its id, or "pipelines[i]" when the id is invalid, then "after",
    // "params" or "params.<p>" — keyed by the step's id with what follows
    // as the label, and the job's own (the rest). An id may hold dots, so
    // the step is the longest id that is the whole where or is followed by
    // a step's key, as jobs.Locate (stepAt) matches it: "nightly.copy.after"
    // is nightly.copy's even beside a step nightly, and "triggers.tz" is
    // the job's even beside a step called triggers.
    function diagsAt(e) {
      const steps = (e.spec && e.spec.pipelines) || [];
      const out = { step: new Map(), top: [] };
      const stepKey = (r) => r === "after" || r === "params" || r.startsWith("params.");
      for (const d of e.diags || []) {
        const w = String(d.where || "");
        let st = null, rest = "";
        const m = /^pipelines\[(\d+)\](?:\.(.*))?$/.exec(w);
        if (m && steps[Number(m[1])]) {
          st = steps[Number(m[1])];
          rest = m[2] || "";
        } else {
          // an id is a string when the file has one; a step without one is
          // only ever named by its index
          for (const x of steps) {
            if (typeof x.id !== "string" || !x.id || (st && x.id.length <= st.id.length) || !w.startsWith(x.id)) continue;
            const r = w.slice(x.id.length);
            if (r === "" || (r[0] === "." && stepKey(r.slice(1)))) { st = x; rest = r.slice(1); }
          }
        }
        if (st) {
          if (!out.step.has(st.id)) out.step.set(st.id, []);
          out.step.get(st.id).push({ ...d, label: rest });
        } else {
          const dot = w.indexOf(".");
          out.top.push({ ...d, label: dot < 0 ? "" : w.slice(dot + 1) });
        }
      }
      return out;
    }

    // ── model edits ──────────────────────────────────────────────────────
    const stepOf = (e, id) => e.spec.pipelines.find((s) => s.id === id) || null;
    const downOf = (e, id) => e.spec.pipelines.filter((s) => s.after.includes(id)).map((s) => s.id);
    // reaches: from ⇝ to along the dependencies (to runs after from)
    function reaches(e, from, to) {
      const seen = new Set(), todo = [from];
      while (todo.length) {
        const x = todo.pop();
        if (x === to) return true;
        if (seen.has(x)) continue;
        seen.add(x);
        todo.push(...downOf(e, x));
      }
      return false;
    }

    // addStep adds a step running pipeline, waiting for after (an id, or
    // ""), selected. Its id is the pipeline's name, made free.
    function addStep(e, pipeline, after) {
      const base = stemOf(pipeline).replace(/[^A-Za-z0-9_.\-]/g, "_") || "step";
      const id = freeID(base, e.spec.pipelines.map((s) => s.id));
      e.spec.pipelines.push({ id, pipeline: stemOf(pipeline), after: after ? [after] : [], params: {} });
      e.sel = { kind: "step", id };
      changed(e, true);
      return id;
    }

    // connect makes to wait for from — refused when it would close a loop
    // (from already waits for to, however far back), with what to say.
    function connect(e, from, to) {
      const b = stepOf(e, to);
      if (!b || !stepOf(e, from) || from === to) return "";
      if (b.after.includes(from)) return "";
      if (reaches(e, to, from)) return "that would make a loop: " + from + " already waits for " + to;
      b.after.push(from);
      if (e.spec.root === to) delete e.spec.root; // it waits now: it is not the root
      e.sel = { kind: "edge", from, to };
      changed(e, true);
      return "";
    }

    function removeEdge(e, from, to) {
      const b = stepOf(e, to);
      if (!b) return;
      b.after = b.after.filter((a) => a !== from);
      e.sel = { kind: "step", id: to };
      changed(e, true);
    }

    function removeStep(e, id) {
      e.spec.pipelines = e.spec.pipelines.filter((s) => s.id !== id);
      for (const s of e.spec.pipelines) s.after = s.after.filter((a) => a !== id);
      if (e.spec.root === id) delete e.spec.root;
      e.sel = null;
      changed(e, true);
    }

    function duplicateStep(e, id) {
      const s = stepOf(e, id);
      if (!s) return;
      const nid = freeID(s.id, e.spec.pipelines.map((x) => x.id));
      e.spec.pipelines.push({ id: nid, pipeline: s.pipeline, after: s.after.slice(), params: Object.assign({}, s.params) });
      e.sel = { kind: "step", id: nid };
      changed(e, true);
    }

    // renameStep renames a step everywhere its id is: the step, the afters
    // that name it, the root, the selection.
    function renameStep(e, id, to) {
      if (to === id) return "";
      if (!validName(to)) return "not a step id: letters, digits, . _ -";
      if (stepOf(e, to)) return "the job already has a step " + to;
      stepOf(e, id).id = to;
      for (const s of e.spec.pipelines) s.after = s.after.map((a) => (a === id ? to : a));
      if (e.spec.root === id) e.spec.root = to;
      e.sel = { kind: "step", id: to };
      changed(e, true);
      return "";
    }

    // ── rendering ────────────────────────────────────────────────────────
    function mount() {
      if (dom) return;
      const root = document.getElementById("jobp");
      const name = el("span", "pname"), meta = el("span", "pmeta");
      const b = (label, title, act) => el("button", { type: "button", title, "data-act": act }, label);
      const design = b("⊞ Design", "The job's DAG: steps, dependencies, triggers, policy", "design");
      const runsB = b("◷ Runs", "This job's runs: the list, and each run drilled into (Alt+R: every run)", "runs");
      const bar = el("div", "pbar", name, meta, el("span", "spacer"), el("span", "jfaces", design, runsB),
        b("⊡ Fit", "Fit the canvas to the pane (f)", "fit"),
        b("{ } JSON", "Edit the job as JSON (the canvas and the file are the same text)", "json"),
        b("⋯", "More: run with parameters, rename, duplicate, trash, copy path", "more"));
      const filter = el("input", { type: "search", class: "pfilter", placeholder: "filter pipelines…",
        "aria-label": "Filter pipelines", spellcheck: "false", autocomplete: "off" });
      const palList = el("div", "plist");
      const palette = el("aside", { class: "ppal", "aria-label": "Pipelines" }, filter, palList);
      const layer = el("div", "pstage jstage");
      const canvas = el("div", { class: "pcanvas jcanvas", tabindex: "0", "aria-label":
        "Job canvas — drag pipelines in, wire output ● to a card, Delete removes, Ctrl+D duplicates, f fits" }, layer);
      const insp = el("aside", { class: "pinsp", "aria-label": "Inspector" });
      // the inspector's drag edge, as a pipeline tab's: one width for both
      // (stage.js dbc.inspector)
      const edge = el("div", { class: "pisplit", role: "separator", "aria-orientation": "vertical",
        title: "Drag to resize the inspector (double-click to reset)" });
      dbc.inspector.split(edge, insp);
      const body = el("div", "pbody", palette, canvas, edge, insp);
      const runsBox = el("div", "jruns");
      runsBox.hidden = true;
      root.replaceChildren(bar, body, runsBox);
      dom = { root, name, meta, bar, filter, palList, layer, canvas, insp, body, runsBox, design, runsB };
      stage = dbc.stage(canvas, layer, { min: 0.25, max: 2, onChange: (v) => { if (shown) shown.view = { tx: v.tx, ty: v.ty, k: v.k }; } });
      bar.addEventListener("click", (ev) => {
        const a = ev.target.closest("button[data-act]");
        if (!a || !shown) return;
        const act = a.dataset.act;
        if (act === "design" || act === "runs") setFace(shown, act);
        else if (act === "fit") fit();
        else if (act === "json") toggleJSON(shown.name);
        else if (act === "more") {
          const r = a.getBoundingClientRect();
          dbc.menu.open(r.left, r.bottom + 2, items(shown.name));
        }
      });
      filter.addEventListener("input", () => { paletteQ = filter.value.trim().toLowerCase(); renderPalette(); });
      bindPointer();
      bindKeys();
    }

    async function show(name) {
      mount();
      let e = files.get(name);
      if (!e) e = await load(name);
      shown = e;
      dom.root.hidden = false;
      dbc.editor.useDoc(docKey(name), e.text, JSON_LANG);
      dbc.editor.replaceDoc(docKey(name), e.text);
      host.setJSON(e.json);
      // the palette is read again whenever a job tab comes on screen: a
      // pipeline made since — here, in an editor, by `dbc` — is offered
      loadPipes(true);
      lastRun(e);
      setFace(e, e.face, true);
      render();
      if (e.view) stage.set(e.view);
      else { stage.set({ tx: 12, ty: 10, k: 1 }); fitIfBig(); }
    }

    function hide() {
      if (shown && shown.json) syncFromEditor(shown);
      shown = null;
      if (runsView) { runsView.destroy(); runsView = null; }
      if (dom) dom.root.hidden = true;
      flush();
    }

    // setFace shows the canvas (design) or this job's runs, which a run
    // started here lands on (openRun).
    function setFace(e, face, quiet) {
      e.face = face;
      if (shown !== e || !dom) return;
      const runs = face === "runs";
      dom.body.hidden = runs;
      dom.runsBox.hidden = !runs;
      dom.design.classList.toggle("on", !runs);
      dom.runsB.classList.toggle("on", runs);
      if (runs && !runsView) {
        runsView = host.runs().mount(dom.runsBox, { fixed: { kind: "job", name: jobOf(e) } });
      } else if (!runs && runsView) {
        runsView.destroy();
        runsView = null;
        dom.runsBox.replaceChildren();
      }
      if (!quiet) (runs ? runsView.focus() : dom.canvas.focus({ preventScroll: true }));
    }

    // jobOf is the job's name as its runs carry it: the spec's, else the
    // file's stem.
    const jobOf = (e) => (e.spec && e.spec.name) || stemOf(e.name);

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
      dom.name.textContent = "⧉ " + e.name + (isDirty(e) ? " ●" : "");
      dom.name.title = isDirty(e) ? "unsaved changes — Ctrl+S saves (▶ Run saves first)" : "saved";
      dom.meta.replaceChildren();
      if (e.spec && e.spec.desc) dom.meta.append(el("span", "pdesc", e.spec.desc));
      if (errs) dom.meta.append(el("span", "perr", "· " + dbc.plural(errs, "error")));
      if (warns) dom.meta.append(el("span", "pwarn", "· " + dbc.plural(warns, "warning")));
      const jb = dom.bar.querySelector('[data-act="json"]');
      jb.classList.toggle("on", !!e.json);
      jb.textContent = e.json ? "⊞ Canvas" : "{ } JSON";
    }

    // ── the palette: the pipelines a step can run ────────────────────────
    async function loadPipes(force) {
      if (pipes && !force) return;
      try {
        const l = await api("GET", "/api/v1/pipelines");
        pipes = {
          mine: l.pipelines.map((p) => ({ name: stemOf(p.name), desc: p.desc || "" })),
          examples: l.examples.map((p) => ({ name: stemOf(p.name), desc: p.desc || "" })),
        };
      } catch (err) { pipes = { mine: [], examples: [], err: err.message }; }
      renderPalette();
    }

    function renderPalette() {
      if (!dom) return;
      dom.palList.replaceChildren();
      if (!pipes) { dom.palList.append(el("div", "pnote", "loading pipelines…")); return; }
      if (pipes.err) { dom.palList.append(el("div", "pnote", "pipelines: " + pipes.err)); return; }
      const hit = (p) => !paletteQ || p.name.toLowerCase().includes(paletteQ) || p.desc.toLowerCase().includes(paletteQ);
      const sect = (title, list) => {
        const ps = list.filter(hit);
        if (!ps.length) return;
        dom.palList.append(el("div", "phead", title));
        for (const p of ps) {
          dom.palList.append(el("div", { class: "pitem jpitem", "data-pipe": p.name, title: p.desc || p.name, role: "button", tabindex: "-1" },
            el("span", "pg", "⛓"), el("span", "pn", p.name)));
        }
      };
      sect("Your pipelines", pipes.mine);
      sect("Examples", pipes.examples);
      if (!dom.palList.children.length) dom.palList.append(el("div", "pnote", paletteQ ? "no pipeline matches" : "no pipelines yet — Ctrl+O → + New ▾"));
    }

    // ── the canvas ───────────────────────────────────────────────────────
    // runOf is the run the cards show: this job's newest that this page
    // knows (e.run), from the runs kit's records.
    const runOf = (e) => (e.run ? host.runs().record(e.run) : null);

    function renderCanvas() {
      const e = shown;
      if (!e || !dom) return;
      if (!e.spec || (e.parseErr && e.json)) {
        dom.layer.replaceChildren(el("div", "pempty", e.parseErr ? "The JSON does not parse: " + e.parseErr : "Not a job."));
        return;
      }
      if (!e.spec.pipelines.length) {
        dom.layer.replaceChildren(el("div", "jhint", "Drag a pipeline from the left onto the canvas: it is the job's first step, " +
          "its root. Drop the next ones on a card to make them wait for it."));
        dom.geo = { w: 420, h: 80 };
        return;
      }
      const da = diagsAt(e), run = runOf(e);
      const root = rootOf(e);
      dom.geo = dbc.runs.drawDag(dom.layer, e.spec.pipelines, e.layout, (s) => card(e, s, da.step.get(s.id), run, root), {
        hit: true,
        edgeClass: (a, b) => (e.sel && e.sel.kind === "edge" && e.sel.from === a && e.sel.to === b ? "sel" : ""),
      });
    }

    // rootOf is the root as the spec has it: root, else the one step with
    // no after (as jobs.Spec.RootID).
    function rootOf(e) {
      if (e.spec.root) return e.spec.root;
      const roots = e.spec.pipelines.filter((s) => !s.after.length);
      return roots.length === 1 ? roots[0].id : "";
    }

    // card is one step: its id (◉ on the root), its pipeline, its params,
    // and its state in the newest run; ports for wiring.
    function card(e, s, diags, run, root) {
      const sel = e.sel && e.sel.kind === "step" && e.sel.id === s.id;
      const errs = (diags || []).filter((d) => d.severity === "error");
      const pr = run && (run.pipelines || []).find((p) => p.id === s.id);
      const st = pr ? pr.status || "queued" : "";
      const params = Object.keys(s.params || {}).sort().map((k) => k + "=" + s.params[k]).join(" ");
      const took = pr ? dbc.runs.tOf(pr.started) && ((dbc.runs.tOf(pr.ended) || Date.now()) - dbc.runs.tOf(pr.started)) : 0;
      const c = el("div", { class: "dcard jcard" + (sel ? " sel" : "") + (errs.length ? " bad" : diags && diags.length ? " warn" : "") +
          (st ? " s-" + st : ""), "data-step": s.id,
        title: (diags || []).map((d) => d.msg).join("\n") || (s.id + " runs pipeline " + s.pipeline + (s.after.length ? ", after " + s.after.join(", ") : ", first")) },
      el("div", "ctop", st ? el("span", "dst s-" + st, STATE()[st] || "○") : null, el("b", "cid", s.id),
        s.id === root ? el("span", { class: "croot", title: "the root: the step that starts the job" }, "◉") : null,
        diags && diags.length ? el("span", "cd", "⚠" + diags.length) : null),
      el("div", "cp", "⛓ " + (s.pipeline || "—")),
      el("div", "csum", params || (s.after.length ? "after " + s.after.join(", ") : "the root")),
      el("div", pr && pr.error ? "cerr" : "cnum", !pr ? "" : pr.error ? pr.error : st === "queued" || st === "skipped" ? st :
        dbc.runs.fmtRows((pr.fragments || []).reduce((n, f) => n + (f.rows || 0), 0)) + " rows" + (took > 0 ? " · " + dbc.runs.fmtDur(took) : "")));
      c.append(el("span", { class: "pport in", title: "input: drop a wire here — this step waits for that one" }));
      c.append(el("span", { class: "pport out", title: "output: drag to a step that waits for this one" }));
      return c;
    }

    function fit() {
      if (!shown || !dom.geo) return;
      stage.fit(dom.geo.w, dom.geo.h, 12);
    }
    // the first look: full size, fitted down (never below 75%) when bigger
    function fitIfBig() {
      const g = dom.geo, W = dom.canvas.clientWidth, H = dom.canvas.clientHeight;
      if (g && W && H && (g.w + 24 > W || g.h + 20 > H)) {
        stage.fit(g.w, g.h, 12);
        if (stage.view.k < 0.75) stage.set({ tx: 12, ty: 10, k: 0.75 });
      }
    }

    // ── the inspector ────────────────────────────────────────────────────
    function renderInspector() {
      const e = shown;
      if (!e || !dom) return;
      const box = dom.insp;
      if (box.contains(document.activeElement) && box.dataset.sel === selKey(e)) { drawDiags(e); drawFires(); return; }
      box.dataset.sel = selKey(e);
      box.replaceChildren();
      if (!e.spec) { box.append(el("p", "pnote", "Fix the JSON to see the inspector.")); return; }
      const s = e.sel;
      if (s && s.kind === "step" && stepOf(e, s.id)) inspectStep(e, stepOf(e, s.id), box);
      else if (s && s.kind === "edge" && stepOf(e, s.to)) inspectEdge(e, s, box);
      else inspectJob(e, box);
      box.append(el("div", { class: "idiags", "data-diags": "1" }));
      drawDiags(e);
      drawFires();
    }
    const selKey = (e) => (e.sel ? [e.sel.kind, e.sel.id, e.sel.from, e.sel.to].join("|") : "");

    function drawDiags(e) {
      const box = dom.insp.querySelector("[data-diags]");
      if (!box) return;
      const da = diagsAt(e), s = e.sel;
      const ds = s && s.kind === "step" ? da.step.get(s.id) || [] : s && s.kind === "edge" ? da.step.get(s.to) || [] : da.top;
      box.replaceChildren(...ds.map((d) => el("div", "idiag " + d.severity, (d.severity === "error" ? "✗ " : "⚠ ") +
        (d.label ? d.label + ": " : "") + d.msg)));
    }

    // drawFires writes each cron line's next fires under it (from the
    // last check), in place: the line being typed in keeps its caret.
    function drawFires() {
      const e = shown;
      if (!e || !dom) return;
      for (const f of dom.insp.querySelectorAll("[data-fires]")) {
        const cf = (e.fires || [])[Number(f.dataset.fires)];
        f.className = "jfires" + (cf && cf.error ? " bad" : "");
        f.textContent = !cf ? "…" : cf.error ? cf.error : cf.next && cf.next.length ? "next: " + cf.next.map(fireText).join(" · ") : "never fires";
      }
    }

    function row(label, input, doc) {
      return el("label", "ifield", el("span", "iname", label), input, doc ? el("span", "idoc", doc) : null);
    }
    const btn = (label, title, act) => { const x = el("button", { type: "button", title }, label); x.addEventListener("click", act); return x; };

    function inspectJob(e, box) {
      const sp = e.spec;
      box.append(el("div", "ihead", el("span", "ig", "⧉"), el("b", null, "job"), el("span", "ik", e.name)));
      const name = el("input", { value: sp.name || "", spellcheck: "false" });
      name.addEventListener("input", () => { sp.name = name.value.trim(); changed(e); });
      const desc = el("textarea", { rows: "2", spellcheck: "true" });
      desc.value = sp.desc || "";
      desc.addEventListener("input", () => { sp.desc = desc.value; changed(e); });
      box.append(row("name", name, "What runs and logs call it — usually the file's name without .json"),
        row("desc", desc, "One line on what it does"));
      box.append(el("div", "iacts",
        btn("▶ Run", "Save, then run the job (Ctrl+Enter)", () => run(e.name)),
        btn("Run with parameters…", "Ask for every parameter first", () => run(e.name, true)),
        btn("◷ Runs", "This job's runs", () => setFace(e, "runs"))));

      // parameters: as a pipeline's (pipelines.js inspectPipeline)
      box.append(el("div", "isub", "parameters", el("span", "idoc", " — ${name} in a step's params; a run asks for those without a default")));
      const params = sp.params;
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
        x.addEventListener("click", () => { delete params[k]; changed(e); box.dataset.sel = ""; renderInspector(); });
        box.append(el("div", "iparam", nm, dv, dc, x));
      }
      box.append(el("div", "iacts", btn("+ parameter", "", () => {
        params[freeID("param", Object.keys(params))] = { default: "" };
        changed(e);
        box.dataset.sel = "";
        renderInspector();
      })));

      // triggers: the cron lines with their next fires, the zone, catch-up,
      // the webhook
      const t = sp.triggers;
      box.append(el("div", "isub", "schedule", el("span", "idoc", " — fired by dbc web while it runs (min hour day month weekday)")));
      t.schedule.forEach((expr, i) => {
        const inp = el("input", { value: expr, class: "jcron", spellcheck: "false", placeholder: "0 2 * * *", "aria-label": "cron line " + (i + 1) });
        inp.addEventListener("input", () => { t.schedule[i] = inp.value; changed(e); });
        const x = el("button", { type: "button", class: "linkish", title: "Remove the line" }, "✕");
        x.addEventListener("click", () => { t.schedule.splice(i, 1); changed(e); box.dataset.sel = ""; renderInspector(); });
        box.append(el("div", "jcronrow", inp, x), el("div", { class: "jfires", "data-fires": String(i) }, "…"));
      });
      box.append(el("div", "iacts", btn("+ schedule line", "A cron line: 0 2 * * * is 02:00 daily, */15 * * * * every quarter hour", () => {
        t.schedule.push("0 2 * * *");
        changed(e);
        box.dataset.sel = "";
        renderInspector();
      })));
      const tz = el("input", { value: t.tz || "", spellcheck: "false", placeholder: "local (" + (Intl.DateTimeFormat().resolvedOptions().timeZone || "") + ")" });
      tz.addEventListener("input", () => { if (tz.value.trim()) t.tz = tz.value.trim(); else delete t.tz; changed(e); });
      box.append(row("time zone", tz, "The IANA zone the schedule reads its clock in (Europe/Paris); empty: dbc web's machine's"));
      const cu = el("input", { type: "checkbox" });
      cu.checked = !!t.catch_up;
      cu.addEventListener("change", () => { if (cu.checked) t.catch_up = true; else delete t.catch_up; changed(e); });
      box.append(el("label", "ifield check", cu, el("span", "iname", "catch up"),
        el("span", "idoc", "When dbc web starts after a fire it missed, run once for the latest one")));
      const wh = el("input", { type: "checkbox" });
      wh.checked = !!t.webhook;
      wh.addEventListener("change", () => { if (wh.checked) t.webhook = true; else delete t.webhook; changed(e); box.dataset.sel = ""; renderInspector(); });
      box.append(el("label", "ifield check", wh, el("span", "iname", "webhook"),
        el("span", "idoc", "Let a caller with dbc web's secret start it: POST …/run with Authorization: Bearer")));
      if (t.webhook) {
        const url = location.origin + "/api/v1/jobs/" + encodeURIComponent(e.name) + "/run";
        const u = el("input", { value: url, readonly: "readonly", class: "jurl", "aria-label": "Webhook URL" });
        box.append(el("div", "jcronrow", u, btn("⧉ curl", "Copy a curl command that starts the job", () => dbc.clip.copyText(
          "curl -X POST -H \"Authorization: Bearer $DBC_WEB_SECRET\" -d '{\"params\":{}}' " + url, "the webhook's curl command"))));
        box.append(el("p", "idoc", "The secret is dbc web's launch secret (dbc web --secret, or $DBC_WEB_SECRET); " +
          "a random one each launch unless given."));
      }

      // the policy
      const po = sp.policy;
      box.append(el("div", "isub", "policy"));
      const sel = (val, opts, set) => {
        const s = el("select", null, ...opts.map(([v, l]) => el("option", { value: v }, l)));
        s.value = val || "";
        s.addEventListener("change", () => { set(s.value); changed(e); });
        return s;
      };
      box.append(row("on failure", sel(po.on_failure, [["", "finish the other branches (default)"], ["stop", "stop the job at once"]],
        (v) => { if (v) po.on_failure = v; else delete po.on_failure; }),
      "A failed step's downstream is skipped either way"));
      const mp = el("input", { value: po.max_parallel ? String(po.max_parallel) : "", placeholder: "2", inputmode: "numeric" });
      mp.addEventListener("input", () => { const n = parseInt(mp.value, 10); if (n > 0) po.max_parallel = n; else delete po.max_parallel; changed(e); });
      box.append(row("max parallel", mp, "Pipelines at once — each running fragment holds a reader and a writer connection"));
      box.append(row("overlap", sel(po.overlap, [["", "skip a start while it runs (default)"], ["queue", "queue one start behind it"]],
        (v) => { if (v) po.overlap = v; else delete po.overlap; }), "A start (a fire, the webhook, Run) while a run of it is going"));
      const to = el("input", { value: po.timeout || "", placeholder: "none (2h, 45m)", spellcheck: "false" });
      to.addEventListener("input", () => { if (to.value.trim()) po.timeout = to.value.trim(); else delete po.timeout; changed(e); });
      box.append(row("timeout", to, "Cancel the run after this long"));
      box.append(el("p", "idoc", "Select a step to edit it. Drag pipelines from the left onto the canvas — onto a card to make " +
        "the new step wait for it; wire a card's output ● to another card to add a dependency."));
    }

    function inspectStep(e, s, box) {
      box.append(el("div", "ihead", el("span", "ig", "⛓"), el("b", null, s.id), el("span", "ik", s.id === rootOf(e) ? "the root" : "step")));
      const id = el("input", { value: s.id, spellcheck: "false" });
      id.addEventListener("change", () => { const why = renameStep(e, s.id, id.value.trim()); if (why) { host.status(why, "warn"); id.value = s.id; } });
      id.addEventListener("keydown", (ev) => { if (ev.key === "Enter") { ev.preventDefault(); id.blur(); } });
      box.append(row("id", id, "Its name in the job: afters, the log and the run record use it"));
      const pl = el("input", { value: s.pipeline || "", spellcheck: "false" });
      const listID = "jp-" + Math.random().toString(36).slice(2, 7);
      pl.setAttribute("list", listID);
      const dl = el("datalist", { id: listID }, ...(pipes ? pipes.mine.concat(pipes.examples) : []).map((p) => el("option", { value: p.name })));
      pl.addEventListener("input", () => { s.pipeline = pl.value.trim(); changed(e); });
      pl.addEventListener("change", () => { box.dataset.sel = ""; renderInspector(); });
      box.append(el("label", "ifield", el("span", "iname", "pipeline"), pl, dl,
        el("span", "idoc", "A pipeline in your pipelines directory, or an example — by name")));
      box.append(el("div", "iacts", btn("↗ Open the pipeline", "Open it in a pipeline tab", () => host.openPipeline(s.pipeline))));

      // after: every other step, ticked when this one waits for it; one
      // that waits for this step (however far down) cannot be ticked
      box.append(el("div", "isub", "after", el("span", "idoc", " — the steps it waits for; none: the root")));
      for (const o of e.spec.pipelines) {
        if (o.id === s.id) continue;
        const cb = el("input", { type: "checkbox" });
        cb.checked = s.after.includes(o.id);
        const loop = !cb.checked && reaches(e, s.id, o.id);
        cb.disabled = loop;
        cb.addEventListener("change", () => {
          if (cb.checked) connect(e, o.id, s.id); else removeEdge(e, o.id, s.id);
          e.sel = { kind: "step", id: s.id };
          box.dataset.sel = "";
          renderInspector();
        });
        box.append(el("label", { class: "ifield check jafter", title: loop ? o.id + " waits for " + s.id + ": that would be a loop" : "" },
          cb, el("span", "iname", o.id), el("span", "idoc", "⛓ " + o.pipeline + (loop ? " — waits for this one" : ""))));
      }

      // params: the pipeline's own, with its defaults as placeholders
      box.append(el("div", "isub", "params", el("span", "idoc", " — the pipeline's; ${param} for the job's, ${run.date} … for the run's")));
      const pbox = el("div", "jparams");
      box.append(pbox);
      paramsOf(s.pipeline).then((pp) => {
        if (!shown || shown !== e || box.dataset.sel !== selKey(e)) return;
        const keys = [...new Set(Object.keys(pp || {}).concat(Object.keys(s.params)))].sort();
        if (!keys.length) pbox.append(el("p", "idoc", pp ? "the pipeline has no parameters" : "the pipeline was not found"));
        for (const k of keys) {
          const def = pp && pp[k];
          const inp = el("input", { value: s.params[k] !== undefined ? s.params[k] : "", spellcheck: "false",
            placeholder: def ? (def.default ? "default " + def.default : "required") : "not a parameter of " + s.pipeline });
          inp.addEventListener("input", () => { if (inp.value === "") delete s.params[k]; else s.params[k] = inp.value; changed(e); });
          pbox.append(row(k + (def && !def.default ? " *" : ""), inp, def ? def.doc || "" : "the pipeline has no such parameter"));
        }
      });
      box.append(el("div", "iacts",
        btn("Duplicate", "Ctrl+D", () => duplicateStep(e, s.id)),
        btn("Delete step", "Delete", () => removeStep(e, s.id))));
    }

    // paramsOf reads a pipeline's params (its file, else the example), once.
    function paramsOf(name) {
      const n = jobName(stemOf(name));
      if (!n) return Promise.resolve(null);
      if (!pipeParams.has(n)) {
        pipeParams.set(n, api("GET", "/api/v1/pipelines/" + encodeURIComponent(n))
          .catch(() => api("GET", "/api/v1/pipeline-examples/" + encodeURIComponent(n)))
          .then((f) => { try { return JSON.parse(f.text).params || {}; } catch (_) { return {}; } })
          .catch(() => null));
      }
      return pipeParams.get(n);
    }

    function inspectEdge(e, s, box) {
      box.append(el("div", "ihead", el("span", "ig", "→"), el("b", null, "dependency")));
      box.append(el("p", null, el("b", null, s.to), " waits for ", el("b", null, s.from), ": it starts once " + s.from +
        " has succeeded (and every other step it waits for). If " + s.from + " fails, " + s.to + " is skipped."));
      box.append(el("div", "iacts", btn("Remove the dependency (Delete)", "", () => removeEdge(e, s.from, s.to))));
    }

    // ── the pointer ──────────────────────────────────────────────────────
    let drag = null;
    function bindPointer() {
      const cv = dom.canvas;
      cv.addEventListener("pointerdown", (ev) => {
        if (ev.button !== 0 || !shown || !shown.spec) return;
        const e = shown;
        const port = ev.target.closest(".pport.out");
        const cardEl = ev.target.closest(".dcard");
        if (port && cardEl) {
          const svg = dom.layer.querySelector("svg.dedges");
          const wire = document.createElementNS("http://www.w3.org/2000/svg", "path");
          wire.setAttribute("class", "edge live");
          if (svg) svg.append(wire);
          const p = dom.geo.pos[cardEl.dataset.step];
          drag = { type: "wire", from: cardEl.dataset.step, x0: p[0] + dom.geo.card[0], y0: p[1] + dom.geo.card[1] / 2, wire };
          cv.setPointerCapture(ev.pointerId);
          ev.preventDefault();
          return;
        }
        if (cardEl) {
          e.sel = { kind: "step", id: cardEl.dataset.step };
          renderCanvas();
          renderInspector();
          cv.focus({ preventScroll: true });
          return;
        }
        const hit = ev.target.closest(".ehit");
        if (hit) {
          e.sel = { kind: "edge", from: hit.dataset.from, to: hit.dataset.to };
          renderCanvas();
          renderInspector();
          cv.focus({ preventScroll: true });
          return;
        }
        drag = { type: "pan", cx: ev.clientX, cy: ev.clientY, tx: stage.view.tx, ty: stage.view.ty, moved: false };
        cv.setPointerCapture(ev.pointerId);
        cv.classList.add("panning");
        cv.focus({ preventScroll: true });
      });
      cv.addEventListener("pointermove", (ev) => {
        if (!drag) return;
        if (drag.type === "pan") {
          const dx = ev.clientX - drag.cx, dy = ev.clientY - drag.cy;
          if (Math.abs(dx) + Math.abs(dy) > 3) drag.moved = true;
          stage.set({ tx: drag.tx + dx, ty: drag.ty + dy });
        } else if (drag.type === "wire") {
          const p = stage.toStage(ev.clientX, ev.clientY);
          drag.wire.setAttribute("d", "M" + drag.x0 + "," + drag.y0 + " C" + (drag.x0 + 40) + "," + drag.y0 + " " + (p.x - 40) + "," + p.y + " " + p.x + "," + p.y);
          const target = document.elementFromPoint(ev.clientX, ev.clientY);
          const c = target && target.closest(".dcard");
          dom.layer.querySelectorAll(".dcard.drop").forEach((x) => { if (x !== c) x.classList.remove("drop"); });
          if (c && c.dataset.step !== drag.from) c.classList.add("drop");
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
          e.sel = null;
          renderCanvas();
          renderInspector();
        } else if (d.type === "wire") {
          d.wire.remove();
          dom.layer.querySelectorAll(".dcard.drop").forEach((x) => x.classList.remove("drop"));
          const hit = ev && document.elementFromPoint(ev.clientX, ev.clientY);
          const c = hit && hit.closest(".dcard");
          if (!c || c.dataset.step === d.from) return;
          const why = connect(e, d.from, c.dataset.step);
          if (why) host.status(why, "warn");
        }
      };
      cv.addEventListener("pointerup", end);
      cv.addEventListener("pointercancel", () => { if (drag && drag.wire) drag.wire.remove(); drag = null; cv.classList.remove("panning"); });
      cv.addEventListener("dblclick", (ev) => {
        // a double-click on a card puts the caret in its pipeline field
        if (!ev.target.closest(".dcard")) return;
        const f = dom.insp.querySelectorAll(".ifield input")[1];
        if (f) f.focus();
      });

      // the palette: press, drag onto the canvas (or a card), release
      dom.palList.addEventListener("pointerdown", (ev) => {
        const item = ev.target.closest(".pitem");
        if (!item || ev.button !== 0 || !shown || !shown.spec) return;
        ev.preventDefault();
        const name = item.dataset.pipe;
        const ghost = el("div", "pghost", "⛓ " + name);
        const start = { x: ev.clientX, y: ev.clientY };
        let moved = false;
        const over = (x, y) => { const h = document.elementFromPoint(x, y); return h && h.closest(".jcanvas .dcard"); };
        const move = (m) => {
          if (!moved && Math.abs(m.clientX - start.x) + Math.abs(m.clientY - start.y) < 4) return;
          if (!moved) { moved = true; document.body.append(ghost); }
          ghost.style.left = m.clientX + 8 + "px";
          ghost.style.top = m.clientY + 8 + "px";
          const c = over(m.clientX, m.clientY);
          dom.layer.querySelectorAll(".dcard.drop").forEach((x) => { if (x !== c) x.classList.remove("drop"); });
          if (c) c.classList.add("drop");
        };
        const up = (u) => {
          document.removeEventListener("pointermove", move, true);
          document.removeEventListener("pointerup", up, true);
          ghost.remove();
          dom.layer.querySelectorAll(".dcard.drop").forEach((x) => x.classList.remove("drop"));
          const e = shown;
          if (!e || !e.spec) return;
          const selStep = e.sel && e.sel.kind === "step" ? e.sel.id : "";
          if (!moved) { addStep(e, name, selStep); return; }
          const r = dom.canvas.getBoundingClientRect();
          if (u.clientX < r.left || u.clientX > r.right || u.clientY < r.top || u.clientY > r.bottom) return;
          const c = over(u.clientX, u.clientY);
          addStep(e, name, c ? c.dataset.step : selStep);
          dom.canvas.focus({ preventScroll: true });
        };
        document.addEventListener("pointermove", move, true);
        document.addEventListener("pointerup", up, true);
      });
    }

    function bindKeys() {
      dom.canvas.addEventListener("keydown", (ev) => {
        const e = shown;
        if (!e || !e.spec) return;
        const s = e.sel;
        if ((ev.key === "Delete" || ev.key === "Backspace") && s) {
          ev.preventDefault();
          if (s.kind === "step") removeStep(e, s.id);
          else if (s.kind === "edge") removeEdge(e, s.from, s.to);
        } else if ((ev.ctrlKey || ev.metaKey) && ev.key.toLowerCase() === "d" && s && s.kind === "step") {
          ev.preventDefault();
          duplicateStep(e, s.id);
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
      if (e.face === "runs") setFace(e, "design", true);
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
    // askParams: the job's params without a default (or all of them when
    // asked), or none — a run is one keystroke when every one has one.
    function askParams(e, always) {
      const ps = (e.spec && e.spec.params) || {};
      const names = Object.keys(ps).sort();
      const last = lastParams.get(e.name) || {};
      if (!names.length || (!always && names.every((k) => (ps[k] || {}).default || last[k]))) return Promise.resolve(Object.assign({}, last));
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

    // run saves, then starts the job with this tab as its origin, and
    // lands the tab on the run's page.
    async function run(name, ask) {
      const e = files.get(name), t = host.tabOf(name);
      if (!e || !t || !t.ws) return;
      if (!(await save(name))) {
        host.status(name + " was not run — it is not saved (see the log)", "warn");
        return;
      }
      const params = await askParams(e, !!ask);
      if (params === null) return;
      let r;
      try {
        r = await api("POST", path(name) + "/run", { ws: t.ws, params });
      } catch (err) {
        host.status(err.message, err.status === 409 ? "warn" : "err");
        log(err.status === 409 ? "warn" : "err", "run: " + err.message, logKey(name));
        return;
      }
      e.run = r.run.id;
      if (shown === e) {
        setFace(e, "runs", true);
        if (runsView) runsView.open(r.run.id);
      } else e.face = "runs";
    }

    // stop stops the run this tab started.
    async function stop(t) {
      const r = t && liveOf(t.ws);
      if (!r) { host.status("nothing of this tab's is running", ""); return; }
      try { await api("POST", "/api/v1/runs/" + encodeURIComponent(r.id) + "/cancel"); } catch (err) { log("err", err.message, logKey(t.job)); }
    }

    // live: run id → {id, origin, source, name} of the job runs going now
    // (job.run → job.done): a tab's busy mark and its Stop.
    const live = new Map();
    function liveOf(ws) {
      for (const r of live.values()) if (r.origin === ws) return r;
      return null;
    }

    // lastRun finds the newest run of the job, once per file shown, so
    // the cards show its states from the first look.
    async function lastRun(e) {
      if (e.runLooked) return;
      e.runLooked = true;
      try {
        const got = await api("GET", "/api/v1/runs?" + new URLSearchParams({ kind: "job", name: jobOf(e), limit: "1" }).toString());
        const h = (got.runs || [])[0];
        if (!h || e.run) return;
        await host.runs().load(h.id);
        if (!e.run) e.run = h.id;
        if (shown === e) renderCanvas();
      } catch (_) { /* no history: cards without states */ }
    }

    // fileOf is the job file a run is of: the file it was started from,
    // else the job's name.
    const fileOf = (h) => h.source || jobName(h.name);

    // onEvent takes the job.* window events for job runs (pipeline runs are
    // pipelines.js's) and the "jobs" store event. The runs kit keeps the
    // records; this moves the tab marks, the cards, and the log.
    function onEvent(type, d) {
      if (type === "jobs") { onStore(d); return; }
      if (type === "job.notice") {
        log(d.level === "err" ? "err" : d.level === "warn" ? "warn" : "info", d.text, d.job ? logKey(jobName(d.job)) : undefined);
        if (d.job && (!host.onScreen(jobName(d.job)))) log(d.level === "err" ? "err" : d.level === "warn" ? "warn" : "info", d.text);
        return;
      }
      const head = (type === "job.run" || type === "job.done") ? d.run : null;
      if (head && head.kind !== "job") return;
      const id = head ? head.id : d.run;
      if (type === "job.run") {
        live.set(id, { id, origin: head.origin || "", source: fileOf(head), name: head.name });
        const file = fileOf(head);
        for (const e of files.values()) if (e.name === file || jobOf(e) === head.name) e.run = id;
        const say = (head.status === "queued" ? "⏸ job " + head.name + " queued behind its last run" : "▶ job " + head.name + " started") +
          " (" + head.trigger + (head.by ? ": " + head.by : "") + ") — run " + id;
        log("accent", say, logKey(file));
        if (!host.onScreen(file)) log("accent", say);
        host.runState(live.get(id), true);
      } else if (type === "job.line") {
        const r = live.get(id);
        if (!r) return;
        log(d.level === "err" ? "err" : "info", (d.name ? "[" + d.name + "] " : "") + d.text, logKey(r.source));
      } else if (type === "job.done") {
        const r = live.get(id) || { id, origin: head.origin || "", source: fileOf(head), name: head.name };
        live.delete(id);
        const took = dbc.runs.tOf(head.ended) - dbc.runs.tOf(head.started);
        const line = "job " + head.name + " " + head.status + " in " + dbc.runs.fmtDur(took) + (head.error ? " — " + head.error : "") +
          " (dbc run show " + id + ")";
        const lvl = head.status === "succeeded" ? "ok" : head.status === "canceled" ? "warn" : "err";
        log(lvl, line, logKey(r.source));
        if (!host.onScreen(r.source)) log(lvl, line);
        host.runState(Object.assign(r, { status: head.status }), false, line);
      } else if (type !== "job.state" && type !== "job.progress") return;
      // the cards of the job on screen follow its run
      if (shown && shown.run === id && shown.face === "design" && !drag) renderCanvas();
    }

    // sync is a page that loaded (or came back) mid-run: the job runs
    // going now, for the tabs' busy marks.
    async function sync() {
      let got;
      try { got = await api("GET", "/api/v1/runs"); } catch (_) { return; }
      live.clear();
      for (const h of got.running || []) {
        if (h.kind !== "job") continue;
        live.set(h.id, { id: h.id, origin: h.origin || "", source: fileOf(h), name: h.name });
        for (const e of files.values()) if (e.name === fileOf(h)) e.run = h.id;
        host.runState(live.get(h.id), true);
      }
      if (shown) renderCanvas();
    }

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

    // a "pipelines" event: a pipeline saved or renamed — the palette and
    // the params read from them are stale
    function pipelinesChanged() {
      pipeParams.clear();
      if (pipes) loadPipes(true);
    }

    function renamed(from, to) {
      const e = files.get(from);
      if (e && !files.has(to)) {
        files.delete(from);
        e.name = to;
        files.set(to, e);
        dbc.editor.renameDoc(docKey(from), docKey(to));
      }
      // this window's draft follows the file, and so do closed windows'
      // (a live window's moves when it gets this same event)
      drafts.rename(from, to);
      for (const r of live.values()) if (r.source === from) r.source = to;
      dbc.moveLog(logKey(from), logKey(to));
      host.renamed(from, to);
      if (shown === e) render();
    }

    // ── actions on a job, from the browser or a tab's menu ───────────────
    const list = () => api("GET", "/api/v1/jobs");

    async function make(suggest, text, what) {
      let taken = [];
      try { taken = (await list()).jobs.map((p) => p.name); } catch (_) { /* the server will say */ }
      let name = jobName(suggest), i = 2;
      while (taken.includes(name)) name = stemOf(jobName(suggest)) + "-" + i++ + ".json";
      let hint = "Letters, digits, '.', '-' and '_', ending in .json.";
      for (;;) {
        name = jobName(await dbc.scripts.ask({ title: what, hint, value: name, ok: "Create" }));
        if (!name) return null;
        const p = parseJob(text);
        const body = p.spec ? jobText(Object.assign(p.spec, { name: stemOf(name) })) : text;
        try {
          await api("PUT", path(name) + host.winQuery(), { text: body, base: "" });
          break;
        } catch (err) {
          if (err.status !== 409 && err.status !== 400) { log("err", what + ": " + err.message); return null; }
          hint = err.message;
        }
      }
      log("ok", "created " + name + " — drag pipelines onto the canvas, wire them, Ctrl+Enter saves and runs", logKey(name));
      await edit(name);
      return name;
    }

    // newJob starts an empty one: the canvas says what to drag.
    function newJob() {
      return make("job.json", jobText({ name: "job", pipelines: [], triggers: {}, policy: {} }), "New job");
    }

    async function copyExample(name) {
      let r;
      try { r = await api("GET", "/api/v1/job-examples/" + encodeURIComponent(name)); } catch (err) { log("err", err.message); return null; }
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
      const to = jobName(await dbc.scripts.ask({ title: "Rename " + name, value: name, ok: "Rename",
        hint: "Letters, digits, '.', '-' and '_', ending in .json." }));
      if (!to || to === name) return;
      const e = files.get(name);
      if (e && isDirty(e)) { if (!(await save(name))) return; }
      try {
        await api("POST", path(name) + "/rename" + host.winQuery(), { to });
      } catch (err) { log("err", "rename: " + err.message); return; }
      renamed(name, to);
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
        const r = await api("POST", "/api/v1/job-trash/" + encodeURIComponent(t.id) + "/restore" + host.winQuery(), {});
        log("ok", "restored " + r.name);
        await edit(r.name);
      } catch (err) {
        if (err.status !== 409) { log("err", "restore: " + err.message); return; }
        const to = jobName(await dbc.scripts.ask({ title: "Restore " + t.name + " as", value: t.name, ok: "Restore", hint: err.message }));
        if (!to) return;
        try {
          const r = await api("POST", "/api/v1/job-trash/" + encodeURIComponent(t.id) + "/restore" + host.winQuery(), { to });
          await edit(r.name);
        } catch (e2) { log("err", "restore: " + e2.message); }
      }
    }

    async function copyPath(name) {
      try {
        const l = await list();
        dbc.clip.copyText(l.dir + "/" + name, "the job's path");
      } catch (err) { log("err", err.message); }
    }

    function items(name) {
      return [
        { label: "Open in a tab", act: () => edit(name) },
        { label: "Save and run", key: "Ctrl+Enter", act: async () => { await edit(name); run(name); } },
        { label: "Run with parameters…", act: async () => { await edit(name); run(name, true); } },
        { label: "Its runs", act: async () => { await edit(name); const e = files.get(name); if (e) setFace(e, "runs"); } },
        { label: "Duplicate…", act: () => duplicate(name) },
        { label: "Rename…", act: () => rename(name) },
        { label: "Move to the trash", act: () => trash(name) },
        { label: "Copy path", act: () => copyPath(name) },
      ];
    }

    return {
      load, show, hide, save, edited, check, flush, forget, onEvent, sync, run, stop, liveOf,
      rename, duplicate, trash, restore, copyPath, items, list, edit, newJob, copyExample, toggleJSON, logKey,
      pipelinesChanged, setFace: (name, face) => { const e = files.get(name); if (e) setFace(e, face); },
      entry: (name) => files.get(name) || null,
      dirty: (name) => isDirty(files.get(name)),
      // the run the canvas shows, for the assistant
      lastRun: (name) => (files.get(name) || {}).run || "",
      diags: (name) => (files.get(name) || {}).diags || [],
      json: (name) => !!(files.get(name) || {}).json,
      face: (name) => (files.get(name) || {}).face || "design",
    };
  }

  dbc.jobs = { create, jobText, parseJob };
})();
