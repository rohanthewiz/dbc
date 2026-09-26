// dbc web — the Plan tab: explain from the editor, and the plan view.
//
// The view itself is plan.js — the standalone plan page's own script,
// loaded here as a file — mounted into #plan. This module is the glue the
// workbench adds around it, the TUI's Plan-tab actions:
//
//   Ctrl+X / Ctrl+Shift+X (the editor), ◈ Explain   explain / analyze the caret's statement
//   e / a                                         explain the plan's statement again / analyze it
//   y / Y                                         copy the plan as text / the engine's own output
//   m                                             copy the plan as a Mermaid chart
//   b                                             open it as the standalone page (a tab of its own)
//   s, ⤓ Save                                     save or share it: the page, a PDF, a JPEG or PNG,
//                                                 the Mermaid source
//   p                                             back to the results (and, from the grid, to the plan)
//   ⤓ Insert, beside a finding's Copy             put its SQL at the end of the editor — never run it
//
// A new plan replaces the view (destroy, mount); one of the same statement
// as the last carries the before/after comparison the server worked out.
(function () {
  "use strict";

  const dbc = window.dbc;
  const { api, log, setStatus, el } = dbc;
  const $ = (id) => document.getElementById(id);
  const host = $("plan");
  const panes = { results: $("rpane-results"), plan: $("rpane-plan") };
  const tabs = $("rtabs");
  const results = $("results");

  let view = null;   // the mounted DbcPlan view
  let planSeq = 0;   // the plan on show (the server's number)
  let again = false; // the plan has its own statement to explain again

  // ── tabs ───────────────────────────────────────────────────────────────
  // showTab switches the results pane between the grid and the plan, and
  // tells the workbench which is up (onPlanPane) so the query tab can
  // remember it across a reload. quiet skips that: reset flips to the
  // grid while another query tab loads, which is not the user leaving
  // that tab's plan.
  function showTab(name, quiet) {
    for (const b of tabs.querySelectorAll(".rtab")) b.classList.toggle("on", b.dataset.rtab === name);
    panes.results.hidden = name !== "results";
    panes.plan.hidden = name !== "plan";
    results.classList.toggle("plan-on", name === "plan");
    if (name === "plan") {
      host.focus({ preventScroll: true });
      if (view) view.fit();
    } else {
      dbc.grid.focus();
    }
    if (!quiet && dbc.cmd.onPlanPane) dbc.cmd.onPlanPane(planVisible());
  }
  const planVisible = () => !panes.plan.hidden && !!view;

  tabs.addEventListener("click", (e) => {
    const b = e.target.closest(".rtab");
    if (b) showTab(b.dataset.rtab);
  });

  // ── explaining ─────────────────────────────────────────────────────────
  async function explain(analyze, fromPlan) {
    const body = Object.assign(dbc.cmd.editorState(), { analyze, again: !!fromPlan });
    try {
      await api("POST", dbc.wsPath("/explain"), body);
    } catch (e) {
      // the server logged the refusal's words already
      setStatus(e.message, e.status === 409 ? "warn" : "err");
    }
  }

  // explainAgain is e / a on the plan: its own statement when it has one;
  // a plan detected in a result has none, so the editor's is explained.
  const explainAgain = (analyze) => explain(analyze, again);

  // ── the view ───────────────────────────────────────────────────────────
  async function load(open) {
    let p;
    const ws = dbc.state.ws;
    try {
      p = await api("GET", dbc.wsPath("/plan"));
    } catch (e) {
      log("err", "could not load the plan: " + e.message);
      return;
    }
    if (!p || ws !== dbc.state.ws) return; // nothing, or the user switched query tabs meanwhile
    if (p.seq !== planSeq || !view) {
      if (view) view.destroy();
      // a new view starts in the workbench's light or dark; its own ◐
      // still flips it alone
      host.dataset.theme = document.documentElement.dataset.theme || "dark";
      planSeq = p.seq;
      again = p.again;
      view = DbcPlan.mount(host, p.doc, {
        embedded: true,
        keys: keysForView,
        copy: (text, what) => dbc.clip.copyText(text, what),
        onInsert: insert,
        compare: p.compare ? { text: p.compare, good: p.better } : null,
        actions: [
          { label: "◈ Again", title: "Explain it again (e) — after an index, say; the comparison shows the change", act: () => explainAgain(false) },
          { label: "▶ Analyze", title: "Explain analyze: runs the statement to time each step (a)", act: () => explainAgain(true) },
          { label: "⧉ Text", title: "Copy the plan as text, findings included (y)", act: () => copyPlan("text") },
          { label: "⧉ Engine", title: "Copy the engine's own output (Y)", act: () => copyPlan("raw") },
          { label: "↗ Page", title: "Open as the standalone page, to keep or send (b)", act: openPage },
          { label: "⤓ Save ▾", id: "save", title: "Save or share the plan: the page, a PDF, a picture, a Mermaid chart (s)", act: saveMenu },
          { label: "✦ Ask", title: "Ask the assistant where the time goes and what to change", act: askPlan },
        ],
        onAsk: (title) => dbc.cmd.askAbout("In this plan, what is the \"" + title + "\" step doing, and is it a problem?"),
      });
    }
    $("plan-tab").hidden = false;
    if (open) showTab("plan");
  }

  // keysForView says whether a key belongs to the plan view: only while it
  // is on screen, with nothing modal open, and the keyboard not in the
  // editor or the sidebar.
  function keysForView() {
    if (!planVisible() || dbc.modal.isOpen() || dbc.menu.isOpen()) return false;
    const a = document.activeElement;
    return !a || a === document.body || panes.plan.contains(a);
  }

  // insert puts a finding's suggested SQL at the end of the editor, on a
  // line of its own. Never runs it: a CREATE INDEX on a production table
  // is a decision, and the editor is where it gets read first.
  function insert(sql) {
    dbc.editor.appendStatement(sql);
    log("ok", "inserted at the end of the editor — review it, then Ctrl+Enter runs it; Ctrl+X re-explains the query to compare");
  }

  async function copyPlan(what) {
    const metric = view ? view.state.metric : "";
    const p = api("GET", dbc.wsPath("/plan/text?what=" + what + "&metric=" + encodeURIComponent(metric)));
    dbc.clip.copy(p, false).then(async () => log("ok", "copied " + (await p).what),
      (e) => log("err", "copy failed: " + e.message));
  }

  // askPlan drafts the TUI's plan question in the assistant's composer —
  // drafted, not sent, so the user can add to it. The plan itself goes
  // with the question (workspace.ChatContext attaches it when the editor's
  // statement is the plan's).
  function askPlan() {
    dbc.cmd.askAbout("Explain this query plan: where does the time go, and what would you change?");
  }

  function openPage() {
    window.open(dbc.wsPath("/plan.html"), "_blank", "noopener");
    log("ok", "opened the plan as a page of its own — save it from there, or ⤓ Save");
  }

  // saveMenu is ⤓ Save: every form the plan can leave in. The page is the
  // interactive one; the PDF and the pictures are the graph and findings
  // drawn by the server (explain.Picture) for places a page will not go —
  // a chat, a ticket, a slide; Mermaid is for a pull request or a wiki,
  // which draw it. The pictures are drawn as the view is showing: sized
  // by its metric, in its light or dark.
  function saveMenu(button) {
    const b = button || host.querySelector('[data-act="save"]');
    if (!b) return;
    dbc.menu.at(b, [
      { head: "save as" },
      { label: "Page — interactive (.html)", act: savePage },
      { label: "PDF — graph and findings", act: () => download("pdf") },
      { label: "JPEG picture", act: () => download("jpg") },
      { label: "PNG picture — lossless", act: () => download("png") },
      { label: "Mermaid chart (.mmd)", act: () => download("mmd") },
      { head: "" },
      { label: "⧉ Copy as Mermaid", key: "m", act: () => copyPlan("mermaid") },
    ]);
  }

  // download fetches one of the plan's files as an attachment, drawn with
  // the view's metric and theme.
  function download(ext) {
    const metric = view ? view.state.metric : "";
    const q = "?download=1&metric=" + encodeURIComponent(metric) + "&theme=" + encodeURIComponent(host.dataset.theme || "dark");
    const a = el("a", { href: dbc.wsPath("/plan." + ext + q), download: "" });
    document.body.append(a);
    a.click();
    a.remove();
    log("ok", "downloading the plan as ." + ext);
  }

  function savePage() {
    const a = el("a", { href: dbc.wsPath("/plan.html?download=1"), download: "" });
    document.body.append(a);
    a.click();
    a.remove();
  }

  // The workbench's keys for the plan, beside the view's own (arrows,
  // Enter, f, g, 1–4, Esc — plan.js).
  document.addEventListener("keydown", (e) => {
    if (e.defaultPrevented || e.ctrlKey || e.metaKey || e.altKey || !keysForView()) return;
    const acts = { e: () => explainAgain(false), a: () => explainAgain(true), y: () => copyPlan("text"),
      Y: () => copyPlan("raw"), m: () => copyPlan("mermaid"), b: openPage, s: () => saveMenu(null),
      p: () => showTab("results") };
    const f = acts[e.key];
    if (f) { e.preventDefault(); f(); }
  });

  $("explain-btn").addEventListener("click", (e) => explain(e.shiftKey, false));

  // reset drops the view — another query tab is coming on screen, whose
  // plan (if it has one) onState loads. Plan seqs are per query tab, so a
  // kept view could be mistaken for the next tab's plan of the same number.
  function reset() {
    if (view) view.destroy();
    view = null;
    planSeq = 0;
    again = false;
    host.replaceChildren();
    $("plan-tab").hidden = true;
    showTab("results", true);
  }

  Object.assign(dbc.cmd, {
    resetPlan: reset,
    planOpen: () => planVisible(),
    // the workbench switched light/dark: the plan follows
    planTheme: (t) => { if (view) host.dataset.theme = t; },
    explain: (analyze) => explain(analyze, false),
    hasPlan: () => !!view,
    showPlan: () => { if (view) showTab("plan"); else log("warn", "no plan yet — Ctrl+X explains the statement under the caret"); },
    showResults: () => showTab("results"),
    // "explain" on the stream: an explain landed
    onExplain(d) {
      if (!d.stateful) $("stateful").hidden = true;
      else $("stateful").hidden = false;
      if (d.ok && d.hasPlan) {
        setStatus(d.status);
        load(true);
      } else {
        setStatus(d.status || "explain failed", d.stopped ? "warn" : "err");
      }
    },
    // a run whose result is itself a plan (an EXPLAIN typed and run)
    onRunPlan(d) { if (d.hasPlan) load(true); },
    // a reattach or a query tab coming on screen: its workspace may hold
    // a plan — shown again if it was on screen when the tab was left
    onState(st) { if (st.hasPlan) load(!!st.openPlan); },
  });
})();
