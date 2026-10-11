// dbc web — the Runs view: every run of a job or a pipeline — this page's,
// another window's, a scheduled one, a cron's `dbc job run` — listed, and
// one opened as a RUN PAGE that drills down from the job's DAG to one
// pipeline's fragments to one fragment's nodes, live while it runs.
//
//	Alt+R · ◷ Runs           the Runs dialog: the list, a run page in its place
//	a job tab's ◷ Runs       the same view inside the tab, on that job's runs
//	                         (and where the tab lands when it starts a run)
//
//	┌ Runs ──────────────────────────────────────────────────────────────── ✕ ┐
//	│ [every job and pipeline ▾] [any status ▾] [7 days ▾]      ● 1 running ⟳ │
//	│ ● nightly    job   schedule  02:00:00   3m 12s  copy ✓ clean ● breeds ● │
//	│ ✓ nightly    job   manual    yesterday  4m 01s  1,204 rows              │
//	│ ✗ nightly    job   manual    Oct 7      0m 40s  breeds: duplicate key … │
//	│ ✓ copy-cats  pipe… cli       Oct 7      0m 12s  8 rows                  │
//	└──────────────────────────────────────────────────────────────────────────┘
//
//	the run page:
//	‹ Runs  ✓ nightly · job · manual · 12:38:00 · 26ms · 8 rows    ■ Stop ⧉ id ↗
//	┌ the DAG (a job's): a card per step, coloured by state, the critical ─────┐
//	│ path drawn bold; a click opens the step                                  │
//	├ ⛓ clean · clean-and-load ✓ 12ms — its fragments on the run's time axis ──┤
//	│   load   ▕      ███████████▏                 8 rows · 3ms                │
//	├ ▤ load — its nodes ──────────────────────────────────────────────────────┤
//	│   node    plugin      in    out   batches   rows/s   time                │
//	│   src     sql.read     0      8         1        —   1ms                 │
//	├ log · clean/load ────────────────────────────────────────────────────────┤
//	│   12:38:00 fragment load: sql.read → go.transform → sql.write            │
//	└──────────────────────────────────────────────────────────────────────────┘
//
// WHERE THE DATA COMES FROM. The list is GET /api/v1/runs with a filter:
// the records in runs_dir (every process's) and the engine's live runs. A
// run page is GET /api/v1/runs/:id — the record with its log, and for a
// job the layout of its steps as it ran (the server lays the DAG out:
// jobs.Run.Layout). A live run is then kept current from the job.* window
// events (job.run, job.state, job.progress, job.line, job.done), applied
// to the same record; a reload, or a page opened mid-run, catches up by
// fetching the record again. A run another process runs (a cron's `dbc job
// run`) sends no events here: its page re-reads the record, which that
// process rewrites every 2 s, while it says running.
//
// THE DRILLDOWN is one selection, {pipeline, fragment}: a DAG card picks
// the pipeline, a timeline row picks the fragment, and the log below shows
// the lines of what is picked — the run's, the pipeline's (a line carries
// its step), or the fragment's (the pipeline's lines that name it, or
// were logged while it ran: its nodes' s.Print and DDL lines carry no
// name). Nothing picked on a job opens on the step that failed, else the
// one running; a pipeline run is its one pipeline.
(function () {
  "use strict";

  const dbc = window.dbc;
  const { api, log, el } = dbc;

  const STATE = { queued: "○", running: "●", succeeded: "✓", failed: "✗", canceled: "■", skipped: "↷", interrupted: "⚠" };
  const LIVE = (s) => s === "running" || s === "queued";
  const NS = "http://www.w3.org/2000/svg";

  // fmtRows is a counter as a card shows it: 1,234 · 12.3k · 4.5M.
  function fmtRows(n) {
    n = n || 0;
    if (n < 10000) return n.toLocaleString("en-US");
    if (n < 1e6) return (n / 1e3).toFixed(n < 1e5 ? 1 : 0) + "k";
    return (n / 1e6).toFixed(1) + "M";
  }
  const fmtDur = (ms) => (ms < 0 ? "" : ms < 1000 ? Math.round(ms) + "ms" : ms < 60e3 ? (ms / 1000).toFixed(1) + "s" :
    Math.floor(ms / 60e3) + "m" + String(Math.round((ms % 60e3) / 1000)).padStart(2, "0") + "s");
  // a Go zero time ("0001-01-01T00:00:00Z") is "not yet"
  const tOf = (s) => { const t = Date.parse(s || ""); return t > 0 ? t : 0; };
  // tookOf is how long something with started/ended took, or has so far
  const tookOf = (x) => { const a = tOf(x.started), b = tOf(x.ended); return a ? (b || Date.now()) - a : -1; };
  // when is a start as a list shows it: the time today, else the day
  function when(s) {
    const t = tOf(s);
    if (!t) return "";
    const d = new Date(t), now = new Date();
    if (d.toDateString() === now.toDateString()) return d.toTimeString().slice(0, 8);
    const y = new Date(now.getTime() - 864e5);
    if (d.toDateString() === y.toDateString()) return "yesterday " + d.toTimeString().slice(0, 5);
    return d.toLocaleDateString("en-US", { month: "short", day: "numeric" }) + " " + d.toTimeString().slice(0, 5);
  }
  const rowsOf = (p) => (p.fragments || []).reduce((n, f) => n + (f.rows || 0), 0);
  const runRows = (r) => (r.pipelines || []).reduce((n, p) => n + rowsOf(p), 0);

  // ── the DAG, shared with the jobs tab ──────────────────────────────────
  // drawDag fills layer with a job's steps: an SVG of edges under a card
  // per step, at the server's layout ({nodes: {id: [x, y]}, w, h, card:
  // [w, h]}). steps is [{id, after}] in the job's order; card(step) builds
  // a card's element (drawDag positions it); opt.edgeClass(from, to) names
  // an edge's class, and opt.hit adds a wide transparent stroke per edge
  // that takes clicks (data-from, data-to). A step the layout does not
  // have yet — one added a moment ago, before the server answered — goes
  // right of the rest, so it is visible until the next layout lands.
  //
  // An edge leaves its upstream card's right side and enters the
  // downstream one's left, as a horizontal-tangent Bezier: the DAG reads
  // left to right as the pipeline canvas does.
  function drawDag(layer, steps, layout, card, opt) {
    opt = opt || {};
    const L = layout || { nodes: {}, w: 0, h: 0, card: [196, 74] };
    const cw = (L.card || [196, 74])[0], ch = (L.card || [196, 74])[1];
    const pos = {};
    let w = L.w || 0, h = L.h || 0, spare = 0;
    for (const s of steps) {
      const p = L.nodes && L.nodes[s.id];
      if (p) { pos[s.id] = p; continue; }
      pos[s.id] = [Math.max(w, 20) + 10, 20 + spare * (ch + 22)];
      spare++;
    }
    for (const s of steps) {
      w = Math.max(w, pos[s.id][0] + cw + 20);
      h = Math.max(h, pos[s.id][1] + ch + 20);
    }
    const svg = document.createElementNS(NS, "svg");
    svg.setAttribute("class", "dedges");
    svg.setAttribute("width", String(w));
    svg.setAttribute("height", String(h));
    const ids = new Set(steps.map((s) => s.id));
    for (const s of steps) {
      for (const a of s.after || []) {
        if (!ids.has(a) || a === s.id) continue;
        const pa = pos[a], pb = pos[s.id];
        const d = curve(pa[0] + cw, pa[1] + ch / 2, pb[0], pb[1] + ch / 2);
        const vis = document.createElementNS(NS, "path");
        vis.setAttribute("d", d);
        vis.setAttribute("class", "edge " + (opt.edgeClass ? opt.edgeClass(a, s.id) : ""));
        svg.append(vis);
        if (opt.hit) {
          const hit = document.createElementNS(NS, "path");
          hit.setAttribute("d", d);
          hit.setAttribute("class", "ehit");
          hit.dataset.from = a;
          hit.dataset.to = s.id;
          svg.append(hit);
        }
      }
    }
    layer.replaceChildren(svg);
    for (const s of steps) {
      const c = card(s);
      c.style.left = pos[s.id][0] + "px";
      c.style.top = pos[s.id][1] + "px";
      c.style.width = cw + "px";
      c.style.height = ch + "px";
      layer.append(c);
    }
    layer.style.width = w + "px";
    layer.style.height = h + "px";
    return { w, h, pos, card: [cw, ch] };
  }
  function curve(x1, y1, x2, y2) {
    const dx = Math.max(36, Math.abs(x2 - x1) / 2);
    return "M" + x1 + "," + y1 + " C" + (x1 + dx) + "," + y1 + " " + (x2 - dx) + "," + y2 + " " + x2 + "," + y2;
  }

  // critical is a job run's critical path: the chain of steps that set
  // when it ended. From the step that ended last (or runs now), back
  // through the upstream that ended last — the one it waited for — to the
  // root. Returns the steps and the edges ("from>to") on it.
  function critical(r) {
    const ps = r.pipelines || [];
    const byID = new Map(ps.map((p) => [p.id, p]));
    const end = (p) => (p ? tOf(p.ended) || (p.status === "running" ? Date.now() : 0) : 0);
    let cur = null;
    for (const p of ps) if (end(p) && (!cur || end(p) > end(cur))) cur = p;
    const steps = new Set(), edges = new Set();
    while (cur && !steps.has(cur.id)) {
      steps.add(cur.id);
      let up = null;
      for (const a of cur.after || []) { const q = byID.get(a); if (q && end(q) && (!up || end(q) > end(up))) up = q; }
      if (up) edges.add(up.id + ">" + cur.id);
      cur = up;
    }
    return { steps, edges };
  }

  // logLines is the run's log for the selection: every line, the
  // pipeline's, or the fragment's (see THE DRILLDOWN above).
  function logLines(r, sel) {
    const lines = r.log || [];
    if (!sel.pipeline) return lines;
    const p = (r.pipelines || []).find((x) => x.id === sel.pipeline);
    const mine = lines.filter((l) => (l.pipeline || "") === sel.pipeline ||
      // a bare pipeline run's lines carry its one PipelineRun's id
      ((r.pipelines || []).length === 1 && !l.pipeline));
    if (!sel.fragment || !p) return mine;
    const f = (p.fragments || []).find((x) => x.name === sel.fragment);
    const a = f ? tOf(f.started) : 0, b = f ? tOf(f.ended) || Date.now() : 0;
    const named = (t) => t.startsWith("fragment " + sel.fragment + " ") || t.startsWith("fragment " + sel.fragment + ":") ||
      t.startsWith("[" + sel.fragment + "/");
    return mine.filter((l) => named(l.text) || (a && tOf(l.at) >= a && tOf(l.at) <= b + 5));
  }

  // ── the kit: what this page knows of runs, kept from events ───────────
  function create(host) {
    // records: run id → the run as this page knows it: the header from
    // job.run (pipelines and fragments listed), moved on by job.state and
    // job.progress, ended by job.done; with its log and layout once a run
    // page fetched it (full). Only runs seen live, or opened, are here.
    const records = new Map();
    const views = new Set(); // mounted views, redrawn on every change
    // local: the runs this server's engine runs or ran — the ones whose
    // events reach this page. A run page of any other (a cron's `dbc job
    // run`) reads its record again while it runs instead (follow).
    const local = new Set();

    function remember(r) {
      let have = records.get(r.id);
      if (!have) {
        have = { log: [], full: false };
        records.set(r.id, have);
      }
      const keep = { log: have.log, layout: have.layout, full: have.full };
      Object.assign(have, r, keep);
      if (r.log) have.log = r.log;
      if (r.layout) have.layout = r.layout;
      // bounded: the newest 200 runs are plenty for a page
      if (records.size > 200) {
        for (const [id, x] of records) { if (!LIVE(x.status)) { records.delete(id); break; } }
      }
      return have;
    }

    // fetchRun reads a run's record whole (its log, a job's layout). Lines
    // that arrive while it is on the way are kept aside and added after,
    // unless the record already had them (the same step and text among
    // its last lines): an event can race the read either way.
    const pending = new Map(); // run id → lines that came while fetching
    async function fetchRun(id) {
      pending.set(id, []);
      let r;
      try { r = await api("GET", "/api/v1/runs/" + encodeURIComponent(id)); } finally {
        const extra = pending.get(id) || [];
        pending.delete(id);
        if (r) {
          r.log = r.log || [];
          const tail = r.log.slice(-60).map((l) => (l.pipeline || "") + "\u0000" + l.text);
          for (const l of extra) if (!tail.includes((l.pipeline || "") + "\u0000" + l.text)) r.log.push(l);
        }
      }
      const have = remember(r);
      have.full = true;
      return have;
    }

    let frame = 0;
    function changed() {
      if (frame) return;
      frame = requestAnimationFrame(() => { frame = 0; for (const v of views) v.soft(); });
    }

    // onEvent takes every job.* window event (app.js forwards them).
    function onEvent(type, d) {
      if (type === "job.run" || type === "job.done") {
        local.add(d.run.id);
        const r = remember(d.run);
        if (type === "job.done") {
          for (const v of views) v.ended(r);
        } else {
          for (const v of views) v.started(r);
        }
      } else if (type === "job.state") {
        const r = records.get(d.run);
        if (!r) return;
        if (!d.pipeline) {
          r.status = d.status;
        } else {
          const p = (r.pipelines || []).find((x) => x.id === d.pipeline);
          if (!p) return;
          p.status = d.status;
          if (d.error) p.error = d.error;
          if (d.status === "running" && !tOf(p.started)) p.started = new Date().toISOString();
          if (!LIVE(d.status) && !tOf(p.ended)) p.ended = new Date().toISOString();
        }
      } else if (type === "job.progress") {
        const r = records.get(d.run);
        if (!r) return;
        const p = (r.pipelines || []).find((x) => x.id === d.name) || (r.pipelines || [])[0];
        if (!p) return;
        p.fragments = p.fragments || [];
        const i = p.fragments.findIndex((f) => f.name === d.fragment.name);
        if (i >= 0) p.fragments[i] = d.fragment; else p.fragments.push(d.fragment);
        if (!p.status || p.status === "queued") p.status = "running";
        if (!tOf(p.started) && tOf(d.fragment.started)) p.started = d.fragment.started;
      } else if (type === "job.line") {
        const line = { at: new Date().toISOString(), level: d.level, pipeline: d.name || "", text: d.text };
        // a bare pipeline run's lines carry the pipeline's name, its one
        // PipelineRun's id; a job run's lines carry the step's ("" for the
        // job's own lines)
        const p = pending.get(d.run);
        if (p) p.push(line);
        const r = records.get(d.run);
        if (!r || !r.full) return;
        r.log.push(line);
        if (r.log.length > 2000) r.log.splice(0, r.log.length - 2000);
      } else return;
      changed();
    }

    // sync is a page that loaded (or came back) mid-run: the runs going now.
    async function sync() {
      let got;
      try { got = await api("GET", "/api/v1/runs"); } catch (_) { return; }
      for (const h of (got.running || []).concat(got.recent || [])) local.add(h.id);
      for (const h of got.running || []) if (!h.preview) remember(h);
      changed();
    }

    // ── a view: the list, or one run's page ─────────────────────────────
    // mount draws a Runs view into box. opts.fixed ({kind, name}) keeps it
    // on one job's runs (a job tab's); opts.close is the dialog's close,
    // for actions that leave it.
    function mount(box, opts) {
      opts = opts || {};
      const v = {
        mode: "list", id: "", sel: { pipeline: "", fragment: "" }, auto: true, reveal: "",
        filter: { what: opts.fixed ? opts.fixed.kind + ":" + opts.fixed.name : "", status: "", since: "168h" },
        rows: [], cur: 0, loading: false, error: "", seq: 0, jobs: [],
      };
      const root = el("div", { class: "rv", tabindex: "-1" });
      box.replaceChildren(root);
      let tick = 0, poll = 0;

      // ── the list ──
      async function list() {
        const n = ++v.seq;
        v.loading = true;
        const q = new URLSearchParams({ history: "1", limit: "200" });
        const [kind, name] = v.filter.what.split(":");
        if (kind) q.set("kind", kind);
        if (name) q.set("name", name);
        if (v.filter.status) q.set("status", v.filter.status);
        if (v.filter.since) q.set("since", v.filter.since);
        let got;
        try {
          got = await api("GET", "/api/v1/runs?" + q.toString());
          v.error = "";
        } catch (err) {
          v.error = err.message;
        }
        if (n !== v.seq) return;
        v.loading = false;
        if (got) {
          v.rows = got.runs || [];
          for (const h of (got.running || []).concat(got.recent || [])) local.add(h.id);
          for (const h of got.running || []) if (!h.preview) remember(h);
        }
        if (!opts.fixed && !v.jobs.length) {
          try {
            const j = await api("GET", "/api/v1/jobs");
            v.jobs = [...new Set([...j.jobs, ...j.examples].map((x) => x.name.replace(/\.json$/, "")))].sort();
          } catch (_) { /* the filter offers what the runs name */ }
        }
        v.redraw();
      }

      // rowsShown is the list: the history, with what this page knows live
      // laid over it (a live record is newer than the listing's copy)
      function rowsShown() {
        const [kind, name] = v.filter.what.split(":");
        const out = v.rows.map((h) => (records.has(h.id) ? Object.assign({}, h, records.get(h.id)) : h));
        for (const r of records.values()) {
          if (!LIVE(r.status) || r.preview || out.some((h) => h.id === r.id)) continue;
          if ((kind && r.kind !== kind) || (name && r.name !== name) || (v.filter.status && r.status !== v.filter.status)) continue;
          out.push(r);
        }
        out.sort((a, b) => tOf(b.started) - tOf(a.started) || String(b.id).localeCompare(String(a.id)));
        return out;
      }

      function drawList() {
        const rows = rowsShown();
        v.cur = Math.min(v.cur, Math.max(0, rows.length - 1));
        const sel = (attrs, options, val, set) => {
          const s = el("select", attrs, ...options.map(([k, label]) => el("option", { value: k }, label)));
          s.value = val;
          s.addEventListener("change", () => { set(s.value); v.cur = 0; list(); });
          return s;
        };
        const names = new Set(v.jobs.map((n) => "job:" + n));
        for (const h of v.rows) names.add(h.kind + ":" + h.name);
        const what = opts.fixed ? null : sel({ "aria-label": "Which runs" },
          [["", "every job and pipeline"], ["job", "every job"], ["pipeline", "every pipeline"]]
            .concat([...names].sort().map((k) => [k, k.replace(":", " · ")])),
          v.filter.what, (x) => { v.filter.what = x; });
        const status = sel({ "aria-label": "Status" }, [["", "any status"], ["running", "running"], ["succeeded", "succeeded"],
          ["failed", "failed"], ["canceled", "canceled"], ["interrupted", "interrupted"]], v.filter.status, (x) => { v.filter.status = x; });
        const since = sel({ "aria-label": "Since" }, [["24h", "24 hours"], ["168h", "7 days"], ["720h", "30 days"], ["", "all time"]],
          v.filter.since, (x) => { v.filter.since = x; });
        const live = rows.filter((r) => LIVE(r.status)).length;
        const refresh = el("button", { type: "button", title: "List again", "data-act": "refresh" }, "⟳");
        const bar = el("div", "rvbar", what, status, since, el("span", "spacer"),
          live ? el("span", "rvlive", "● " + live + " running") : null,
          v.loading ? el("span", "hint", "loading…") : null, refresh);
        const ul = el("div", { class: "rvlist", role: "listbox", "aria-label": "Runs" });
        rows.forEach((r, i) => {
          const took = tookOf(r);
          const what2 = LIVE(r.status) && (r.pipelines || []).length > 1
            ? el("span", "rvsteps", ...(r.pipelines || []).map((p) => el("span", "s-" + (p.status || "queued"), p.id + " " + (STATE[p.status] || "○") + " ")))
            : el("span", r.error ? "rverr" : "rvrows", r.error ? r.error : fmtRows(r.pipelines ? runRows(r) : r.rows) + " rows");
          ul.append(el("div", { class: "rvrow" + (i === v.cur ? " cur" : ""), role: "option", "data-id": r.id, "data-i": String(i),
            title: "run " + r.id + (r.by ? " — " + r.by : "") + (r.error ? "\n" + r.error : "") },
          el("span", "rvst s-" + r.status, STATE[r.status] || "?"),
          el("span", "rvname", r.name), el("span", "rvkind", r.kind), el("span", "rvtrig", r.trigger),
          el("span", "rvwhen", when(r.started)), el("span", "rvtook", took >= 0 ? fmtDur(took) : ""), what2));
        });
        if (!rows.length) {
          ul.append(el("div", "pnote", v.error ? "could not list the runs: " + v.error : v.loading ? "" :
            opts.fixed ? "no runs of " + opts.fixed.name + " yet — ▶ Run starts one" :
              "no runs match — a pipeline or job run from a tab, the CLI or the scheduler is listed here"));
        }
        root.replaceChildren(bar, ul);
        const c = ul.querySelector(".rvrow.cur");
        if (c) c.scrollIntoView({ block: "nearest" });
      }

      // ── one run's page ──
      async function open(id, keepSel) {
        v.mode = "run";
        v.id = id;
        if (!keepSel) { v.sel = { pipeline: "", fragment: "" }; v.auto = true; }
        v.error = "";
        v.redraw();
        try {
          await fetchRun(id);
        } catch (err) {
          v.error = err.message;
        }
        if (v.mode === "run" && v.id === id) v.redraw();
        follow();
      }

      // follow re-reads a run that runs in another process (no events
      // come for it) every 2 s while it says it runs — that process
      // rewrites its record as often
      function follow() {
        clearTimeout(poll);
        const r = records.get(v.id);
        if (!r || !LIVE(r.status) || local.has(r.id)) return;
        poll = setTimeout(async () => {
          if (v.mode !== "run" || !root.isConnected) return;
          try { await fetchRun(v.id); } catch (_) { /* gone: keep what is shown */ }
          v.redraw();
          follow();
        }, 2000);
      }

      // pickDefault opens a job run on the step that failed, else one
      // running; a fragment likewise. Only until the user picks.
      function pickDefault(r) {
        if (!v.auto) return;
        const ps = r.pipelines || [];
        if (ps.length === 1) v.sel.pipeline = ps[0].id;
        else {
          const p = ps.find((x) => x.status === "failed") || ps.find((x) => x.status === "running");
          v.sel.pipeline = p ? p.id : "";
        }
        const p = ps.find((x) => x.id === v.sel.pipeline);
        const fs = (p && p.fragments) || [];
        const f = fs.find((x) => x.status === "failed") || fs.find((x) => x.status === "running");
        v.sel.fragment = f ? f.name : "";
      }

      function drawRun() {
        const r = records.get(v.id);
        const back = el("button", { type: "button", class: "rvback", title: "Back to the list (Backspace)", "data-act": "back" }, "‹ Runs");
        if (!r || !r.full) {
          root.replaceChildren(el("div", "rvbar", back), el("div", "pnote", v.error ? "could not read run " + v.id + ": " + v.error : "loading run " + v.id + "…"));
          return;
        }
        pickDefault(r);
        const took = tookOf(r);
        const head = el("div", "rvhead",
          el("span", "rvst s-" + r.status, (STATE[r.status] || "?") + " " + r.status),
          el("b", "rvname", r.name), el("span", "rvkind", r.kind),
          el("span", "rvmeta", [r.trigger + (r.by ? " (" + r.by + ")" : ""), when(r.started), took >= 0 ? fmtDur(took) : "",
            fmtRows(runRows(r)) + " rows", r.fragment ? "fragment " + r.fragment + " only" : ""].filter(Boolean).join(" · ")));
        const acts = el("span", "rvacts",
          LIVE(r.status) ? el("button", { type: "button", "data-act": "stop", title: "Stop the run: every fragment in flight rolls back" }, "■ Stop") : null,
          el("button", { type: "button", "data-act": "copyid", title: "Copy the run's id (dbc run show " + r.id + ")" }, "⧉ " + r.id),
          host.canOpen(r) ? el("button", { type: "button", "data-act": "openfile", title: "Open the " + r.kind + " in a tab" },
            "↗ " + (r.kind === "job" ? "job" : "pipeline")) : null);
        const bar = el("div", "rvbar", back, head, el("span", "spacer"), acts);
        const parts = [bar];
        if (r.error) parts.push(el("div", "rverrline", "✗ " + r.error));
        if (r.params && Object.keys(r.params).length) {
          parts.push(el("div", "rvparams", "params: " + Object.keys(r.params).sort().map((k) => k + "=" + r.params[k]).join(" · ")));
        }
        if (r.kind === "job") parts.push(drawRunDag(r));
        const p = (r.pipelines || []).find((x) => x.id === v.sel.pipeline);
        if (p) parts.push(drawPipeline(r, p));
        const f = p && (p.fragments || []).find((x) => x.name === v.sel.fragment);
        if (f) parts.push(drawNodes(r, p, f));
        parts.push(drawLog(r));
        // a redraw (every event, every second while it runs) keeps where
        // the reader was: the page's scroll, and the log's — which follows
        // new lines only while it is scrolled to its end
        const was = root.querySelector(".rvscroll"), wasLog = root.querySelector(".rvlog");
        const top = was ? was.scrollTop : 0;
        const stick = !wasLog || wasLog.dataset.stick !== "0", logTop = wasLog ? wasLog.scrollTop : 0;
        const scroll = el("div", "rvscroll", ...parts.slice(1));
        root.replaceChildren(parts[0], scroll);
        scroll.scrollTop = top;
        const lg = scroll.querySelector(".rvlog");
        if (lg) {
          lg.dataset.stick = stick ? "1" : "0";
          lg.scrollTop = stick ? lg.scrollHeight : logTop;
        }
        // what a click just opened is brought into view: in a job tab the
        // page sits in the editor's pane, and a step's fragments open
        // below the DAG, out of sight. By hand rather than scrollIntoView,
        // which would also scroll the workbench's own clipped boxes.
        const sec = v.reveal && scroll.querySelector("." + v.reveal);
        v.reveal = "";
        if (sec) {
          const a = sec.getBoundingClientRect(), b = scroll.getBoundingClientRect();
          if (a.top < b.top || a.top > b.bottom - 60) scroll.scrollTop += a.top - b.top - 6;
        }
      }

      function drawRunDag(r) {
        const crit = critical(r);
        const layer = el("div", "dlayer");
        drawDag(layer, (r.pipelines || []).map((p) => ({ id: p.id, after: p.after || [] })), r.layout, (s) => {
          const p = r.pipelines.find((x) => x.id === s.id);
          const st = p.status || "queued", took = tookOf(p);
          return el("div", { class: "dcard s-" + st + (crit.steps.has(p.id) ? " crit" : "") + (v.sel.pipeline === p.id ? " sel" : ""),
            "data-step": p.id, title: p.id + " — pipeline " + (p.pipeline || "") + " — " + st + (p.error ? "\n" + p.error : "") },
          el("div", "ctop", el("span", "dst s-" + st, STATE[st] || "○"), el("b", "cid", p.id),
            el("span", "ctime", took >= 0 && st !== "queued" && st !== "skipped" ? fmtDur(took) : "")),
          el("div", "cp", "⛓ " + (p.pipeline || "")),
          el("div", p.error ? "cerr" : "cnum", p.error ? p.error : st === "queued" || st === "skipped" ? st :
            fmtRows(rowsOf(p)) + " rows" + ((p.fragments || []).length ? " · " + dbc.plural(p.fragments.length, "fragment") : "")));
        }, { edgeClass: (a, b) => (crit.edges.has(a + ">" + b) ? "crit" : "") + " to-" + (((r.pipelines || []).find((x) => x.id === b) || {}).status || "") });
        return el("section", "rvdag", el("div", "rvsub", "the job · " + dbc.plural((r.pipelines || []).length, "pipeline") +
          " · the critical path is drawn bold — click a step to open it"), el("div", "dwrap", layer));
      }

      // drawPipeline is the step's fragments on the run's time axis: bars
      // from the run's start to its end (or now), so the wait before the
      // step and the order of its fragments both show.
      function drawPipeline(r, p) {
        const t0 = tOf(r.started), t1 = Math.max(tOf(r.ended) || Date.now(), t0 + 1);
        const span = t1 - t0;
        const pct = (t) => Math.min(100, Math.max(0, ((t - t0) / span) * 100));
        const rows = (p.fragments || []).map((f) => {
          const a = tOf(f.started), b = tOf(f.ended) || (f.status === "running" ? Date.now() : 0);
          const bar = a ? el("span", { class: "tbar s-" + f.status, style: "left:" + pct(a) + "%;width:max(2px," + (pct(b || a) - pct(a)) + "%)" }) : null;
          return el("div", { class: "trow" + (v.sel.fragment === f.name ? " sel" : ""), "data-frag": f.name, role: "button", tabindex: "-1",
            title: f.name + " — " + f.status + (f.error ? "\n" + f.error : "") },
          el("span", "tname", el("span", "dst s-" + f.status, STATE[f.status] || "○"), " " + f.name),
          el("span", "ttrack", bar),
          el("span", f.error ? "terr" : "tmeta", f.error ? f.error : f.status === "queued" || f.status === "skipped" ? f.status :
            fmtRows(f.rows) + " rows · " + fmtDur(tookOf(f)) + (f.direct ? " · direct COPY" : "")));
        });
        const took = tookOf(p);
        const title = r.kind === "job" ? "⛓ " + p.id + " · " + (p.pipeline || "") : "⛓ " + (p.pipeline || p.id);
        const ax = el("div", "taxis", el("span"), el("span", "tax", el("span", null, when(r.started)), el("span", null, "+" + fmtDur(span))), el("span"));
        return el("section", "rvpipe",
          el("div", "rvsub", el("span", "dst s-" + (p.status || "queued"), STATE[p.status] || "○"), " " + title + " · " + (p.status || "queued") +
            (took >= 0 && tOf(p.started) ? " · " + fmtDur(took) : "") +
            (p.params && Object.keys(p.params).length ? " · " + Object.keys(p.params).sort().map((k) => k + "=" + p.params[k]).join(" ") : "") +
            " — click a fragment for its nodes"),
          p.error ? el("div", "rverrline", "✗ " + p.error) : null,
          rows.length ? el("div", "tline", ...rows, ax) : el("div", "pnote", p.status === "skipped" ? "skipped — a step it waits for did not succeed" : "not started"));
      }

      function drawNodes(r, p, f) {
        const head = el("tr", null, ...["node", "plugin", "in", "out", "batches", "rows/s", "time"].map((h) => el("th", null, h)));
        const body = [];
        for (const n of f.nodes || []) {
          const secs = (n.elapsed || 0) / 1e9; // a Go duration: nanoseconds
          const rate = secs > 0 ? fmtRows(Math.round((n.out || n.in || 0) / secs)) : "—";
          body.push(el("tr", { class: n.error ? "bad" : "", "data-node": n.id },
            el("td", "nid", n.id), el("td", "nplug", n.plugin), el("td", "num", fmtRows(n.in)), el("td", "num", fmtRows(n.out)),
            el("td", "num", String(n.batches || 0)), el("td", "num", rate), el("td", "num", fmtDur((n.elapsed || 0) / 1e6))));
          if (n.error) body.push(el("tr", "nerr", el("td", { colspan: "7" }, "✗ " + n.id + ": " + n.error)));
        }
        const preview = (f.nodes || []).some((n) => n.plugin === "preview") ? previewAct(r, p, f) : null;
        return el("section", "rvfrag",
          el("div", "rvsub", el("span", "dst s-" + f.status, STATE[f.status] || "○"), " ▤ " + f.name + " · " + f.status +
            (f.direct ? " · ran as one direct Postgres COPY" : "") + " · " + fmtRows(f.rows) + " rows", preview),
          f.error ? el("div", "rverrline", "✗ " + f.error) : null,
          (f.nodes || []).length ? el("table", "ntable", el("thead", null, head), el("tbody", null, ...body)) :
            el("div", "pnote", "no node has reported yet"),
          f.vars && Object.keys(f.vars).length ? el("div", "rvparams", "values: " +
            Object.keys(f.vars).sort().map((k) => "${frag." + f.name + "." + k + "} = " + f.vars[k]).join(" · ")) : null,
          ...(p.shown || []).filter((x) => x.fragment === f.name).map(shownTable));
      }

      // shownTable is a result the fragment showed (a preview sink's rows,
      // a go node's s.Show) as the run's record kept it: its first rows
      // (jobs.MaxShownRows), N-185. They are what the starting tab's grid
      // got — here for any run, a scheduled one that had no tab included.
      function shownTable(x) {
        const rows = x.rows || [], cols = x.columns || [];
        const what = rows.length < x.total ? "the first " + rows.length + " of " + fmtRows(x.total) + " rows" : fmtRows(rows.length) + " rows";
        return el("div", "rvshown",
          el("div", "rvsub", "◎ " + x.title + " · " + what + (x.truncated ? " (the source sent more)" : "")),
          el("div", "rvshownbox", el("table", "ntable stable",
            el("thead", null, el("tr", null, ...cols.map((c) => el("th", null, c)))),
            el("tbody", null, ...rows.map((r) => el("tr", null, ...r.map((v) => el("td", null, v))))))));
      }

      // previewAct is what a fragment with a preview sink offers: its rows
      // went to the grid of the tab that started the run, when that tab is
      // in this window; otherwise (a scheduled run) nowhere, and a fresh
      // preview of the fragment shows today's.
      function previewAct(r, p, f) {
        const tab = host.tabOfOrigin(r.origin);
        if (tab) {
          return el("button", { type: "button", class: "rvpv", "data-act": "origin", title: "Its preview rows are in " + tab.title + "'s grid" },
            "◎ Open preview");
        }
        return el("button", { type: "button", class: "rvpv", "data-act": "repreview", "data-pipe": p.pipeline || p.id, "data-frag": f.name,
          title: "This run's preview rows went to no tab here — preview the fragment again, now, into a pipeline tab" }, "◎ Preview again");
      }

      function drawLog(r) {
        const lines = logLines(r, v.sel);
        const where = [v.sel.pipeline && (r.pipelines || []).length > 1 ? v.sel.pipeline : "", v.sel.fragment].filter(Boolean).join("/");
        const box = el("div", { class: "rvlog", "data-stick": "1" }, ...lines.map((l) => el("div", "rvline",
          el("span", "t", new Date(tOf(l.at) || Date.now()).toTimeString().slice(0, 8)),
          el("span", l.level === "err" ? "err" : "info", (l.pipeline && !v.sel.pipeline ? "[" + l.pipeline + "] " : "") + l.text))));
        if (!lines.length) box.append(el("div", "pnote", "no lines"));
        box.addEventListener("scroll", () => { box.dataset.stick = box.scrollTop + box.clientHeight >= box.scrollHeight - 4 ? "1" : "0"; });
        return el("section", "rvlogs", el("div", "rvsub", "log" + (where ? " · " + where : " · the whole run") +
          (v.sel.pipeline ? " " : ""), v.sel.pipeline ? el("button", { type: "button", class: "linkish", "data-act": "all" }, "show the whole run's") : null), box);
      }

      // ── input ──
      root.addEventListener("click", async (ev) => {
        const b = ev.target.closest("button[data-act]");
        if (b) {
          const r = records.get(v.id);
          const act = b.dataset.act;
          if (act === "refresh") list();
          else if (act === "back") back();
          else if (act === "all") { v.sel = { pipeline: "", fragment: "" }; v.auto = false; v.redraw(); }
          else if (act === "stop" && r) {
            try { await api("POST", "/api/v1/runs/" + encodeURIComponent(r.id) + "/cancel"); } catch (err) { log(err.status === 409 ? "warn" : "err", err.message); }
          } else if (act === "copyid" && r) dbc.clip.copyText(r.id, "the run's id");
          else if (act === "openfile" && r) { if (opts.close) opts.close(); host.openFile(r); }
          else if (act === "origin" && r) { if (opts.close) opts.close(); host.showOrigin(r.origin); }
          else if (act === "repreview") { if (opts.close) opts.close(); host.preview(b.dataset.pipe, b.dataset.frag); }
          return;
        }
        const row = ev.target.closest(".rvrow");
        if (row && v.mode === "list") { v.cur = Number(row.dataset.i); open(row.dataset.id); return; }
        const card = ev.target.closest(".dcard[data-step]");
        if (card) {
          v.auto = false;
          v.sel = { pipeline: v.sel.pipeline === card.dataset.step ? "" : card.dataset.step, fragment: "" };
          v.reveal = v.sel.pipeline ? "rvpipe" : "";
          v.redraw();
          return;
        }
        const tr = ev.target.closest(".trow[data-frag]");
        if (tr) {
          v.auto = false;
          v.sel.fragment = v.sel.fragment === tr.dataset.frag ? "" : tr.dataset.frag;
          v.reveal = v.sel.fragment ? "rvfrag" : "";
          v.redraw();
        }
      });
      root.addEventListener("keydown", (ev) => {
        if (ev.ctrlKey || ev.metaKey || ev.altKey || /^(INPUT|SELECT|TEXTAREA)$/.test(ev.target.tagName)) return;
        if (v.mode === "list") {
          const n = rowsShown().length;
          if (ev.key === "ArrowDown") { v.cur = Math.min(v.cur + 1, n - 1); drawList(); ev.preventDefault(); }
          else if (ev.key === "ArrowUp") { v.cur = Math.max(v.cur - 1, 0); drawList(); ev.preventDefault(); }
          else if (ev.key === "Enter") { const r = rowsShown()[v.cur]; if (r) open(r.id); ev.preventDefault(); }
        } else if (ev.key === "Backspace") { back(); ev.preventDefault(); }
      });

      function back() {
        v.mode = "list";
        clearTimeout(poll);
        list();
        v.redraw();
        root.focus({ preventScroll: true });
      }

      // durations of what runs tick by the second
      tick = setInterval(() => {
        if (!root.isConnected) { destroy(); return; }
        const r = v.mode === "run" ? records.get(v.id) : null;
        if ((r && LIVE(r.status)) || (v.mode === "list" && rowsShown().some((x) => LIVE(x.status)))) v.soft();
      }, 1000);

      function destroy() {
        clearInterval(tick);
        clearTimeout(poll);
        views.delete(v);
      }

      v.redraw = () => {
        if (!root.isConnected) return;
        if (v.mode === "list") drawList(); else drawRun();
      };
      // soft is a redraw nobody asked for (an event, the second's tick): it
      // waits while a filter's picker has the focus, which a redraw would
      // replace — and close under the user's pointer
      v.soft = () => {
        const a = document.activeElement;
        if (a && a.tagName === "SELECT" && root.contains(a)) return;
        v.redraw();
      };
      // a run started: listed at once; ended: the list reads the records
      // again (its rows came from them), a page shows the final record
      v.started = (r) => { if (v.mode === "list" && matches(r)) v.soft(); };
      v.ended = (r) => {
        if (v.mode === "list" && matches(r)) list();
        else if (v.mode === "run" && v.id === r.id) fetchRun(r.id).then(() => v.redraw(), () => {});
      };
      const matches = (r) => {
        const [kind, name] = v.filter.what.split(":");
        return !r.preview && (!kind || r.kind === kind) && (!name || r.name === name);
      };
      views.add(v);
      list();
      return {
        open, back, destroy,
        focus: () => root.focus({ preventScroll: true }),
        refresh: () => (v.mode === "list" ? list() : open(v.id, true)),
        get mode() { return v.mode; },
      };
    }

    // open is Alt+R / ◷ Runs: the view in a dialog, on a run when given.
    function open(id) {
      const box = el("div", "rvbox");
      let view = null;
      dbc.modal.open({
        title: "Runs", cls: "runs", body: box,
        onClose: () => { if (view) view.destroy(); host.focus(); },
        onKey: (e) => {
          // Backspace goes back to the list from a run page, not out
          if (e.key === "Backspace" && view && view.mode === "run" && !/^(INPUT|SELECT|TEXTAREA)$/.test(e.target.tagName)) {
            view.back();
            return true;
          }
          return false;
        },
      });
      view = mount(box, { close: () => dbc.modal.close() });
      view.focus();
      if (id) view.open(id);
    }

    return { onEvent, sync, mount, open, load: fetchRun, record: (id) => records.get(id) || null };
  }

  dbc.runs = { create, drawDag, critical, STATE, fmtRows, fmtDur, tOf, when };
})();
