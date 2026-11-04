// dbc web — live snippets: the list the editor's Tab consults (editor.js
// "live snippets"), and the snippets dialog (Alt+S, the editor menu's
// "Insert snippet…"): pick one to insert, and keep the list — new, edit,
// delete. The store is the TUI's (web/snippets.go), so a snippet saved in
// either expands in both.
//
// The list is fetched once at load and again on every "snippets" event
// (app.js): a save or delete in any window, which is how each window's Tab
// learns a new name.
//
// A snippet is SQL or Go (lang): it expands only in a document of its
// language — a query tab, a script tab (editor.js snippetLang). The dialog
// lists every snippet, as it is where they are all kept, but the editor's
// own language first; the others are dimmed and tagged, and a pick of one
// is refused with where it does expand.
(function () {
  "use strict";

  const dbc = window.dbc;
  const el = dbc.el;

  let list = [];        // [{name, desc, body, lang, selection, err}], by name
  let names = new Map(); // name → lang, of the snippets Tab may expand: those that parse
  let redraw = null;     // the open dialog's redraw, while it is open

  async function refresh() {
    try {
      const got = await dbc.api("GET", "/api/v1/snippets");
      list = got.snippets || [];
    } catch (err) {
      dbc.log("err", "snippets: " + err.message);
      return;
    }
    names = new Map(list.filter((s) => !s.err).map((s) => [s.name, s.lang || "sql"]));
    if (dbc.editor.snippetsChanged) dbc.editor.snippetsChanged();
    if (redraw) redraw();
  }

  // rank orders a filter's matches as the TUI's dialog does: an exact or
  // prefix name match first, then a name containing it, then a
  // description; -1 is no match.
  function rank(s, q) {
    if (!q) return 0;
    const n = s.name.toLowerCase();
    if (n === q) return 0;
    if (n.startsWith(q)) return 1;
    if (n.includes(q)) return 2;
    if ((s.desc || "").toLowerCase().includes(q)) return 3;
    return -1;
  }

  const langLabel = (l) => (l === "go" ? "Go" : "SQL");
  const langOf = (s) => s.lang || "sql";
  // where a snippet of language l is typed, for the messages
  const whereOf = (l) => (l === "go" ? "a script tab" : "a query tab");

  // open shows the dialog, the cursor on the snippet called sel when given
  // (after a save). A selection in the editor as it opens is what a pick
  // wraps ($SELECTION$), and the title says so.
  function open(sel) {
    const wraps = !!dbc.editor.selection();
    // the document's language: its snippets are listed first and are the
    // ones a pick inserts; "" (a pipeline's JSON) inserts none
    const lang = dbc.editor.snippetLang ? dbc.editor.snippetLang() : "sql";
    const input = el("input", { type: "search", class: "hfilter", placeholder: "type to filter by name or description",
      "aria-label": "Filter snippets", spellcheck: "false", autocomplete: "off" });
    const ul = el("ul", { class: "hlist snlist", role: "listbox" });
    const preview = el("pre", "ival snprev");
    const count = el("span", "hint");
    const add = el("button", { type: "button" }, "+ New");
    let shown = [], cur = 0;

    function draw() {
      const q = input.value.trim().toLowerCase();
      shown = [];
      // the filter's four ranks over this document's language, then over
      // the rest: another language's row is there to be kept, not picked,
      // so it should not stand between the filter and the pick
      for (const mine of [true, false]) {
        for (let r = 0; r <= 3; r++) for (const s of list) if (rank(s, q) === r && (langOf(s) === lang) === mine) shown.push(s);
      }
      if (sel) {
        const i = shown.findIndex((s) => s.name === sel);
        if (i >= 0) cur = i;
        sel = "";
      }
      cur = Math.max(0, Math.min(cur, shown.length - 1));
      ul.replaceChildren();
      shown.forEach((s, i) => {
        const other = langOf(s) !== lang;
        ul.append(el("li", { class: (i === cur ? "cur" : "") + (other ? " snother" : ""), role: "option", "data-i": String(i),
          title: other ? langLabel(langOf(s)) + " snippet — expands in " + whereOf(langOf(s)) : s.err || s.desc || s.name },
          el("span", "snmark", s.err ? "⚠" : s.selection ? "⇲" : ""),
          el("span", "hconn", s.name),
          el("span", "hsql", s.err ? "does not parse: " + s.err : s.desc || ""),
          other ? el("span", "snlang", langLabel(langOf(s))) : null));
      });
      const s = shown[cur];
      preview.textContent = s ? s.body : "";
      preview.hidden = !s;
      count.textContent = shown.length
        ? "↑↓ pick · Enter inserts · Alt+N new · Alt+E edit · Alt+D delete · ⇲ uses the selection · Esc closes"
        : "no snippet matches — Alt+N makes one";
      const c = ul.children[cur];
      if (c) c.scrollIntoView({ block: "nearest" });
    }
    function insert(i) {
      const s = shown[i];
      if (!s) return;
      if (s.err) { dbc.log("warn", "snippet " + s.name + " does not parse: " + s.err + " — Alt+E fixes it"); return; }
      if (langOf(s) !== lang) {
        dbc.log("warn", "snippet " + s.name + " is " + langLabel(langOf(s)) + " — it expands in " + whereOf(langOf(s)) +
          (lang ? ", not in this " + langLabel(lang) + " document" : ""));
        return;
      }
      redraw = null;
      dbc.modal.close();
      dbc.editor.insertSnippet(s.name);
    }
    // ^N after a search that found nothing makes the snippet searched for
    const newName = () => (/^[A-Za-z0-9_]{1,40}$/.test(input.value.trim()) ? input.value.trim() : "");
    input.addEventListener("input", () => { cur = 0; draw(); });
    ul.addEventListener("click", (e) => { const li = e.target.closest("li[data-i]"); if (li) { cur = +li.dataset.i; draw(); input.focus(); } });
    ul.addEventListener("dblclick", (e) => { const li = e.target.closest("li[data-i]"); if (li) insert(+li.dataset.i); });
    add.addEventListener("click", () => form("", newName(), lang));

    redraw = draw;
    dbc.modal.open({
      title: (wraps ? "Snippets · a pick wraps the selection" : "Snippets") + (lang === "go" ? " · Go" : ""),
      cls: "wide", focus: input,
      body: el("div", "history", input, ul, preview), foot: el("div", "mfoot", add, count),
      onKey: (e) => {
        if (e.key === "ArrowDown") { cur = Math.min(cur + 1, shown.length - 1); draw(); return true; }
        if (e.key === "ArrowUp") { cur = Math.max(cur - 1, 0); draw(); return true; }
        if (e.key === "Enter") { insert(cur); return true; }
        // Alt and e.code, not Ctrl: the browser keeps Ctrl+N (a window),
        // and on a Mac Alt+N types "˜"
        if (e.altKey && !e.ctrlKey && !e.metaKey) {
          if (e.code === "KeyN") { form("", newName(), lang); return true; }
          if (e.code === "KeyE" && shown[cur]) { form(shown[cur].name); return true; }
          if (e.code === "KeyD" && shown[cur]) { confirmDelete(shown[cur].name); return true; }
        }
        return false;
      },
      onClose: () => { redraw = null; dbc.editor.focus(); },
    });
    draw();
  }

  // confirmDelete asks before deleting: a delete cannot be undone.
  function confirmDelete(name) {
    redraw = null;
    const yes = el("button", { type: "button", class: "primary" }, "Delete " + name);
    const no = el("button", { type: "button" }, "Keep it");
    yes.addEventListener("click", async () => {
      try {
        await dbc.api("DELETE", "/api/v1/snippets/" + encodeURIComponent(name));
        dbc.log("ok", "deleted snippet " + name);
      } catch (err) {
        dbc.log("err", "snippets: " + err.message);
      }
      await refresh();
      open();
    });
    no.addEventListener("click", () => open(name));
    dbc.modal.open({
      title: "Delete snippet " + name + "?", focus: no,
      body: el("div", "confirm", el("p", null, "The snippet is deleted for every dbc on this machine — the TUI's too.")),
      foot: el("div", "mfoot", yes, no),
    });
  }

  // form edits the snippet called old, or makes a new one (named suggest,
  // in language lang — the document's, so ^N in a script tab makes a Go
  // snippet). Save and Esc go back to the dialog; a refusal (a bad name, a
  // name taken, a template that does not parse) stays with the reason.
  function form(old, suggest, lang) {
    redraw = null;
    const s = old ? list.find((x) => x.name === old) : null;
    const field = (label, ctl) => el("label", "ifield", el("span", "iname", label), ctl);
    const name = el("input", { type: "text", spellcheck: "false", autocomplete: "off", "aria-label": "Name" });
    const desc = el("input", { type: "text", spellcheck: "false", autocomplete: "off", "aria-label": "Description" });
    const body = el("textarea", { class: "code", rows: "12", spellcheck: "false", "aria-label": "Body" });
    // the language: a select of two, which also sets the body's placeholder
    // to what a snippet of it looks like
    const langSel = el("select", { "aria-label": "Language" },
      el("option", { value: "sql" }, "SQL — query tabs"),
      el("option", { value: "go" }, "Go — script tabs"));
    langSel.value = s ? langOf(s) : lang === "go" ? "go" : "sql";
    const placeholder = () => {
      body.placeholder = langSel.value === "go"
        ? "res, err := s.Query(\"$VAR1:conn$\", `$VAR2:SELECT 1$`)"
        : "SELECT $VAR2:*$ FROM $VAR1:table$";
    };
    placeholder();
    langSel.addEventListener("change", () => { placeholder(); err.textContent = ""; });
    name.value = s ? s.name : suggest || "";
    desc.value = s ? s.desc : "";
    body.value = s ? s.body : "";
    const err = el("div", "snerr");
    const save = el("button", { type: "button", class: "primary" }, "✓ Save");
    const cancel = el("button", { type: "button" }, "Cancel");

    async function doSave() {
      const req = { name: name.value.trim(), desc: desc.value, body: body.value, lang: langSel.value };
      try {
        if (old) await dbc.api("PUT", "/api/v1/snippets/" + encodeURIComponent(old), req);
        else await dbc.api("POST", "/api/v1/snippets", req);
      } catch (e) {
        err.textContent = e.message;
        return;
      }
      dbc.log("ok", (old ? "saved" : "made") + " " + langLabel(req.lang) + " snippet " + req.name + " — type " + req.name +
        " then Tab in " + whereOf(req.lang) + ", or pick it in Alt+S");
      await refresh();
      open(req.name);
    }
    save.addEventListener("click", doSave);
    cancel.addEventListener("click", () => open(old));
    for (const c of [name, desc, body]) c.addEventListener("input", () => { err.textContent = ""; });

    dbc.modal.open({
      title: old ? "Edit snippet " + old : "New snippet", cls: "wide", focus: old ? body : suggest ? desc : name,
      body: el("div", "snform",
        field("Name — typed before Tab: letters, digits and _", name),
        field("Description", desc),
        field("Language — the tabs it expands in", langSel),
        field("Body", body),
        el("p", "idoc", "$VAR1$ a stop — Tab visits them by number, and a number used twice mirrors · " +
          "$VAR1:default$ · $SELECTION$ the selected text · $END$ the caret at the end · $$ a $"),
        err),
      foot: el("div", "mfoot", save, cancel, el("span", "hint", "Ctrl+S save · Esc back")),
      onKey: (e) => {
        if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === "s") { doSave(); return true; }
        if (e.key === "Escape") { open(old); return true; }
        // Enter in a one-line field saves, as in the other dialogs; the
        // body takes it as a new line
        if (e.key === "Enter" && e.target !== body) { doSave(); return true; }
        return false;
      },
    });
  }

  dbc.snippets = {
    refresh,
    open,
    // has: n names a snippet Tab may expand in a document of language lang
    has: (n, lang) => names.get(n) === (lang || "sql"),
    // list: the snippets of language lang (the Go completion's rows)
    list: (lang) => list.filter((s) => langOf(s) === lang),
    onEvent: refresh,
  };
  refresh();
})();
