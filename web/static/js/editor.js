// dbc web — the SQL editor: a <textarea>, upgraded to Monaco once it loads.
//
// THE TEXTAREA IS THE SOURCE OF TRUTH (gonotes' pattern). The page works the
// moment it loads, with the plain textarea; Monaco is loaded right after,
// from the copy embedded in the binary (scripts/vendor_monaco.sh — never a
// CDN: dbc works offline and the CSP allows only this server), and takes
// over the same text. While Monaco is up, every edit is mirrored back into
// the textarea, so the tab autosave and anything else reading the textarea
// keep working, and if Monaco fails to load nothing is lost — the textarea
// just stays.
//
//   textarea.value ──(load)──► Monaco model ──onDidChangeModelContent──┐
//        ▲                                                             │
//        └──────────── mirrored, + an "input" event ◄──────────────────┘
//
// Offsets are UTF-16 code units everywhere here — JavaScript string
// indexes, which is also what Monaco's getOffsetAt returns — and the server
// converts them to bytes (web/api.go byteOffset).
//
// THE STATEMENT UNDER THE CARET is marked in the gutter, as the TUI marks
// it, so it is plain which statement Ctrl+Enter will run. The server finds
// it (POST /api/v1/stmt) with the very splitter a run uses, rather than a
// JavaScript copy of it that would disagree about some quote one day.
//
// COMPLETIONS come from the server for the same reason (POST
// /api/v1/ws/:id/complete, package sqlcomplete): the columns of the tables
// the statement names, join clauses from foreign keys, the tables after
// FROM, the dialect's keywords, functions and types — the rules the TUI's
// popup follows, computed once in Go from the connection's schema. Monaco
// asks as a word starts and after "." or "::", and filters what came back
// as the word grows; a list the server cut short (incomplete) is asked for
// again on each keystroke instead.
//
// GO TO DEFINITION, USAGES AND RENAME of a table alias, a CTE name or a
// column the statement names (a CTE's output column, a select alias) come
// from the server too (POST /api/v1/ws/:id/symbol and …/rename), from the
// scanner completion reads the statement with: F12, Shift+F12 and F2.
//
// SCRIPT TABS edit Go, not SQL (scripts.js). Their documents are opened
// with a FIXED language ("go"): the connection's dialect (setDriver) never
// touches them, the statement marker and the SQL providers leave them
// alone, and the chords that mean something only for SQL (Ctrl+X explain)
// step aside through the dbcScript context key, so Monaco's own Ctrl+X
// (cut the line) works there. Go completion, hover and the check's
// markers are scripts.js's, hung on the editor through ready() and
// setMarkers().
(function () {
  "use strict";

  const dbc = window.dbc;
  // Keep in sync with scripts/vendor_monaco.sh.
  const MONACO_VERSION = "0.52.2";
  const BASE = "/static/vendor/monaco";

  const wrap = document.getElementById("editor-wrap");
  const ta = document.getElementById("editor");
  const host = document.getElementById("monaco");

  let ed = null;         // the Monaco editor, once loaded
  let decos = null;      // its statement-marker decorations
  let lang = "sql";
  const changeFns = [];

  // ONE DOCUMENT PER QUERY TAB. With Monaco, each tab gets its own model,
  // so switching tabs keeps each one's undo history, cursor and scroll —
  // swapping the text in and out of one model would throw all three away.
  // With consoles the document is the console's (app.js docOf), and it
  // outlives the tabs showing it (app.js leaveDoc), so a console swapped
  // out and back keeps all three too. The plain textarea has no such state
  // to keep, so it just swaps text.
  const docs = new Map(); // tab key → {model, view}
  let docKey = "";
  // fixed: document key → its language, for a document that is not SQL (a
  // script tab's "go"). Kept apart from docs so the plain textarea, which
  // has no models, still knows what the document on screen is.
  const fixed = new Map();
  const readyFns = []; // ready(fn) callbacks waiting for Monaco
  let scriptCtx = null; // the dbcScript context key, once Monaco is up
  let warmed = "";   // the connection completions were last warmed for
  let lastNote = ""; // the last completion note logged, so it is logged once

  // ── the API the rest of the page uses ─────────────────────────────────
  const api = {
    text: () => (ed ? ed.getValue() : ta.value),
    setText(s) {
      if (ed) ed.setValue(s);
      else ta.value = s;
      ta.value = s;
    },
    // caret is the UTF-16 offset of the caret (the selection's active end)
    caret() {
      if (!ed) return ta.selectionStart;
      return ed.getModel().getOffsetAt(ed.getPosition());
    },
    selection() {
      if (!ed) return ta.value.slice(ta.selectionStart, ta.selectionEnd);
      const sel = ed.getSelection();
      return sel ? ed.getModel().getValueInRange(sel) : "";
    },
    // insert replaces the selection (or inserts at the caret) with s
    insert(s) {
      if (ed) {
        ed.executeEdits("dbc", [{ range: ed.getSelection(), text: s, forceMoveMarkers: true }]);
        ed.pushUndoStop();
        ed.focus();
        return;
      }
      ta.focus();
      ta.setRangeText(s, ta.selectionStart, ta.selectionEnd, "end");
      ta.dispatchEvent(new Event("input"));
    },
    // appendStatement puts sql on a line of its own after the buffer, so it
    // neither lands inside the statement being tuned nor runs with it by
    // accident — the TUI's rule for a finding's suggested SQL.
    appendStatement(sql) {
      const text = api.text();
      let sep = "";
      if (text.trim() !== "") {
        sep = text.endsWith("\n") ? "\n" : "\n\n";
        if (!text.trim().endsWith(";")) sep = ";" + sep;
      }
      const at = text.length;
      if (ed) {
        const m = ed.getModel(), pos = m.getPositionAt(at);
        ed.executeEdits("dbc", [{ range: new monaco.Range(pos.lineNumber, pos.column, pos.lineNumber, pos.column), text: sep + sql }]);
        ed.pushUndoStop();
        const end = m.getPositionAt(api.text().length);
        ed.setPosition(end);
        ed.revealPositionInCenter(end);
        ed.focus();
        return;
      }
      ta.focus();
      ta.setRangeText(sep + sql, at, at, "end");
      ta.dispatchEvent(new Event("input"));
    },
    focus: () => (ed ? ed.focus() : ta.focus()),
    hasFocus: () => (ed ? ed.hasTextFocus() : document.activeElement === ta),
    onChange: (fn) => changeFns.push(fn),
    // setDriver picks the SQL dialect Monaco colors: pgsql and mysql know
    // their own keywords and quoting; everything else is generic sql.
    setDriver(driver) {
      lang = /postgres|pgx|cockroach/.test(driver || "") ? "pgsql" : /mysql|maria/.test(driver || "") ? "mysql" : "sql";
      if (ed && !fixed.has(docKey)) monaco.editor.setModelLanguage(ed.getModel(), lang);
    },
    setHeight(px) {
      wrap.style.height = px + "px";
    },
    height: () => wrap.getBoundingClientRect().height,
    isMonaco: () => !!ed,
    element: wrap,
    // useDoc shows query tab key's document, creating it with text the
    // first time. It fires no change: switching tabs edits nothing.
    // language fixes the document's language ("go" for a script tab); left
    // out, the document is SQL in the connection's dialect.
    useDoc(key, text, language) {
      if (language) fixed.set(key, language);
      if (key === docKey) return;
      const go = fixed.get(key) === "go";
      ta.placeholder = go ? "func Run(s *sdb.S) error { … }" : "SELECT * FROM cats;";
      ta.setAttribute("aria-label", go ? "Go script editor" : "SQL editor");
      if (!ed) {
        ta.value = text;
        docKey = key;
        return;
      }
      const cur = docs.get(docKey);
      if (cur) cur.view = ed.saveViewState();
      let d = docs.get(key);
      if (!d) {
        d = { model: newModel(text, key), view: null };
        docs.set(key, d);
      }
      docKey = key;
      monaco.editor.setModelLanguage(d.model, fixed.get(key) || lang);
      ed.setModel(d.model);
      if (d.view) ed.restoreViewState(d.view);
      ta.value = ed.getValue();
      scriptCtx.set(go);
      scheduleMark();
    },
    // isScript: the document on screen is a script's (Go)
    isScript: () => fixed.get(docKey) === "go",
    // setMarkers puts a check's diagnostics (script.Diag: line, col,
    // severity, msg) on key's document as Monaco markers — squiggles, the
    // hover's message, F8 to walk them. A document not open (or the plain
    // textarea, which cannot draw them) is skipped; the next check after
    // it opens marks it.
    //
    // A squiggle alone is easy to miss, and its message waits behind a
    // hover. So, as ced draws a Go error, each diagnosed line also gets
    // decorations (diagDecos) that need no gesture:
    //
    //	 ● 	println("Hello, World!"[)]   × missing ',' before newline in argument list
    //	 │                         └┬┘   └── the message, after the line's end
    //	 └ a dot in the gutter      └ a box around where the error is
    //
    // Every diagnosed line shows its message, not just the caret's (ced's
    // rule, made for gopls's dozens of findings): a check here reports at
    // most ten syntax errors, or one compile error and a lint or two, so
    // the lines stay readable — and the point is to see an error without
    // first putting the caret on it.
    setMarkers(key, diags) {
      const d = docs.get(key);
      if (!ed || !d) return;
      const m = d.model, S = monaco.MarkerSeverity;
      const at = (diags || []).map((x) => ({ x, r: diagRange(m, x) }));
      monaco.editor.setModelMarkers(m, "dbc", at.map(({ x, r }) => ({
        severity: x.severity === "error" ? S.Error : S.Warning, message: x.msg, ...r,
      })));
      d.diagIds = m.deltaDecorations(d.diagIds || [], diagDecos(m, at));
    },
    // ready runs fn(monaco) once Monaco has loaded — at once if it has. A
    // page whose Monaco never loads never calls it (the textarea has no
    // completion to offer anyway).
    ready(fn) {
      if (ed) fn(window.monaco);
      else readyFns.push(fn);
    },
    // dropDoc forgets a document no tab will show again: a closed tab's
    // own, or a deleted console's. The one on screen is never dropped.
    dropDoc(key) {
      const d = docs.get(key);
      if (d && key !== docKey) { d.model.dispose(); docs.delete(key); }
      if (key !== docKey) fixed.delete(key);
    },
    // A document is a query tab's own, or — with consoles — a console's,
    // shared by every tab of the window showing that console (app.js
    // docOf). The three below let app.js reach one that is not on screen.
    //
    // docKey is the key of the document on screen.
    docKey: () => docKey,
    // docText is key's text: the editor's for the one on screen, the
    // model's for another; null for a document the textarea editor does
    // not keep (it has one text, swapped on useDoc) or never opened.
    docText(key) {
      if (key === docKey) return api.text();
      const d = docs.get(key);
      return d ? d.model.getValue() : null;
    },
    // replaceDoc puts text in key's document as ONE EDIT, so Ctrl+Z brings
    // back what it replaced: a console saved elsewhere lands this way, and
    // the user's own version is an undo away. A document not opened yet
    // is left alone; it will be opened with the new text. Firing the
    // change (on screen) lets the page save what the user undoes to.
    replaceDoc(key, text) {
      const d = docs.get(key);
      if (ed && d) {
        if (d.model.getValue() === text) return;
        d.model.pushEditOperations([], [{ range: d.model.getFullModelRange(), text }], () => null);
        d.model.pushStackElement();
        return;
      }
      if (key === docKey) { ta.value = text; }
    },
    // renameDoc moves key's document to a new key (a console renamed),
    // keeping its undo history, caret and scroll.
    renameDoc(from, to) {
      const d = docs.get(from);
      if (d && !docs.has(to)) { docs.delete(from); docs.set(to, d); }
      if (fixed.has(from) && !fixed.has(to)) { fixed.set(to, fixed.get(from)); fixed.delete(from); }
      if (docKey === from) docKey = to;
    },
    // warm asks for completions once when the active connection changes,
    // so its schema is read while the user is still looking at the page
    // rather than on their first keystroke. The answer is thrown away.
    warm(conn) {
      if (!conn || conn === warmed || !dbc.state.ws) return;
      warmed = conn;
      dbc.api("POST", dbc.wsPath("/complete"), { buffer: "", caret: 0 }).then(noteOnce, () => {});
    },
    // retheme re-reads the palette after the page's light/dark switch.
    retheme() {
      if (ed) { defineTheme(); monaco.editor.setTheme("dbc"); }
    },
  };
  dbc.editor = api;

  function changed() {
    for (const fn of changeFns) fn();
    scheduleMark();
  }

  // newModel makes key's Monaco model in its language. A Go document
  // indents with tabs, as gofmt does; SQL keeps the editor's four spaces.
  function newModel(text, key) {
    const m = monaco.editor.createModel(text, fixed.get(key) || lang);
    if (fixed.get(key) === "go") m.updateOptions({ insertSpaces: false, tabSize: 4 });
    return m;
  }
  ta.addEventListener("input", changed);
  ta.addEventListener("keyup", scheduleMark);
  ta.addEventListener("click", scheduleMark);

  // Tab indents in the textarea instead of leaving it (Monaco does its own).
  ta.addEventListener("keydown", (e) => {
    if (e.key !== "Tab" || e.ctrlKey || e.metaKey || e.altKey) return;
    e.preventDefault();
    ta.setRangeText("    ", ta.selectionStart, ta.selectionEnd, "end");
    changed();
  });

  // ── the statement marker ───────────────────────────────────────────────
  // Debounced: a caret held down on an arrow key asks once it rests. The
  // textarea has no gutter, so the marker is Monaco's alone.
  let markTimer = 0, markSeq = 0;
  function scheduleMark() {
    if (!ed) return;
    clearTimeout(markTimer);
    markTimer = setTimeout(mark, 150);
  }
  async function mark() {
    const seq = ++markSeq;
    // a script is Go: there is no statement to mark
    if (api.isScript()) { decos.set([]); return; }
    const text = ed.getValue();
    let r;
    try {
      r = await dbc.api("POST", "/api/v1/stmt", { buffer: text, caret: api.caret() });
    } catch (_) { return; }
    if (seq !== markSeq || !ed || ed.getValue() !== text) return; // stale
    const m = ed.getModel();
    const list = [];
    if (r[1] > r[0]) {
      // trim the blank lines a statement's range can start with, so the
      // bar sits beside the statement's own text
      let start = r[0];
      while (start < r[1] && /\s/.test(text[start])) start++;
      const a = m.getPositionAt(start), b = m.getPositionAt(Math.max(start, r[1] - 1));
      list.push({
        range: new monaco.Range(a.lineNumber, 1, b.lineNumber, 1),
        options: { isWholeLine: true, linesDecorationsClassName: "stmt-mark" },
      });
    }
    decos.set(list);
  }

  // diagRange is where a diag is marked, as Monaco range fields. Col 0 is
  // a message about the line as a whole (yaegi does not always say where);
  // a col marks the word that starts there, or one character when nothing
  // word-like does. A col at or past the line's end — go/parser puts "missing
  // ','" at the newline — marks the line's last character instead, as ced
  // does: a mark on nothing would be drawn after the end-of-line message.
  function diagRange(m, x) {
    const line = Math.min(Math.max(1, x.line || 1), m.getLineCount());
    let a = 1, b = m.getLineMaxColumn(line);
    if (x.col > 0) {
      a = Math.min(x.col, b);
      if (a === b && b > 1) a = b - 1;
      const w = m.getWordAtPosition({ lineNumber: line, column: a });
      b = w && w.startColumn === a ? w.endColumn : Math.min(a + 1, m.getLineMaxColumn(line));
    }
    return { startLineNumber: line, startColumn: a, endLineNumber: line, endColumn: Math.max(b, a + 1) };
  }

  // diagDecos is setMarkers' always-visible half: per diagnosed line, a
  // gutter dot and the worst diag's message after the line ("(+n more)"
  // when it has company — the hover and F8 have them all), and per diag a
  // box over its range. An error outranks a warning on the same line, and
  // colours the dot and the message.
  //
  // The message rides an "after" injected text on a range spanning the
  // whole line, with stickiness that grows as the user types at either
  // edge: typing at the end of the line pushes the note along rather than
  // splitting the new text from the old. The decorations are replaced by
  // the next check (~600 ms after typing stops), so a fixed line loses
  // its note almost as soon as it is fixed.
  const NOTE_MAX = 160; // a long yaegi message is cut; the hover has it whole
  function diagDecos(m, at) {
    const byLine = new Map();
    for (const it of at) {
      const l = it.r.startLineNumber;
      if (!byLine.has(l)) byLine.set(l, []);
      byLine.get(l).push(it.x);
    }
    const out = [];
    for (const [line, xs] of byLine) {
      const worst = xs.find((x) => x.severity === "error") || xs[0];
      const sev = worst.severity === "error" ? "err" : "warn";
      let msg = String(worst.msg || "").replace(/\s+/g, " ").trim();
      if (msg.length > NOTE_MAX) msg = msg.slice(0, NOTE_MAX - 1) + "…";
      if (xs.length > 1) msg += "  (+" + (xs.length - 1) + " more)";
      out.push({
        range: new monaco.Range(line, 1, line, m.getLineMaxColumn(line)),
        options: {
          linesDecorationsClassName: "diag-dot " + sev,
          stickiness: monaco.editor.TrackedRangeStickiness.AlwaysGrowsWhenTypingAtEdges,
          after: { content: "\u2003" + (sev === "err" ? "× " : "⚠ ") + msg, inlineClassName: "diag-note " + sev },
        },
      });
    }
    for (const { x, r } of at) {
      out.push({ range: new monaco.Range(r.startLineNumber, r.startColumn, r.endLineNumber, r.endColumn),
        options: { inlineClassName: "diag-at " + (x.severity === "error" ? "err" : "warn") } });
    }
    return out;
  }

  // ── completions ────────────────────────────────────────────────────────
  // noteOnce logs a completion answer's note — the schema could not be
  // read, so only the vocabulary is offered — once, not on every keystroke.
  function noteOnce(r) {
    if (r && r.note && r.note !== lastNote) {
      lastNote = r.note;
      dbc.log("warn", r.note);
    }
  }

  // snippet turns an item's insert text and cursor into a Monaco snippet
  // with $0 where the caret should land (between a function's
  // parentheses). Snippet syntax gives $, } and \ meaning, so they are
  // escaped in the text around it.
  const esc = (t) => t.replace(/[\\$}]/g, "\\$&");
  function snippet(it) {
    if (it.cursor < 0) return { text: it.insert, rules: undefined };
    return {
      text: esc(it.insert.slice(0, it.cursor)) + "$0" + esc(it.insert.slice(it.cursor)),
      rules: monaco.languages.CompletionItemInsertTextRule.InsertAsSnippet,
    };
  }

  // the server's kinds as Monaco's, for the icons
  function kindOf(k) {
    const K = monaco.languages.CompletionItemKind;
    return ({
      column: K.Field, table: K.Struct, view: K.Interface, schema: K.Module, alias: K.Variable,
      join: K.Reference, keyword: K.Keyword, function: K.Function, type: K.TypeParameter,
      procedure: K.Method, // a stored procedure, which CALL runs
    })[k] ?? K.Text;
  }

  // a table's or a column's documentation is DDL, shown as SQL; a
  // function's is a sentence, shown as text
  const SQL_DOC = new Set(["table", "view", "column", "join"]);

  function registerCompletion() {
    const provider = {
      triggerCharacters: [".", ":"],
      async provideCompletionItems(model, position, _ctx, token) {
        if (!dbc.state.ws) return { suggestions: [] };
        let r;
        try {
          r = await dbc.api("POST", dbc.wsPath("/complete"), {
            buffer: model.getValue(), caret: model.getOffsetAt(position),
          });
        } catch (_) {
          return { suggestions: [] }; // a lost server is the status bar's to report
        }
        if (token.isCancellationRequested) return { suggestions: [] };
        noteOnce(r);
        const a = model.getPositionAt(r.from), b = model.getPositionAt(r.to);
        const range = new monaco.Range(a.lineNumber, a.column, b.lineNumber, b.column);
        return {
          incomplete: !!r.incomplete,
          suggestions: (r.items || []).map((it) => {
            const sn = snippet(it);
            return {
              label: { label: it.label, description: it.detail || undefined },
              kind: kindOf(it.kind),
              detail: it.detail || undefined,
              documentation: !it.doc ? undefined
                : SQL_DOC.has(it.kind) ? { value: "```sql\n" + it.doc + "\n```" } : it.doc,
              insertText: sn.text,
              insertTextRules: sn.rules,
              filterText: it.filter || undefined,
              // the server's order is the context's: keep it
              sortText: it.sort,
              range,
            };
          }),
        };
      },
    };
    // a provider is per language, and setDriver moves models between these
    for (const l of ["sql", "pgsql", "mysql"]) monaco.languages.registerCompletionItemProvider(l, provider);
  }

  // ── go to definition, usages, rename ───────────────────────────────────
  // The server's resolver (POST …/symbol and …/rename, sqlcomplete's
  // resolve.go) knows a table's alias, a CTE's name, and the columns the
  // statement names itself (a CTE's or derived table's output columns, a
  // select alias in ORDER BY): where each is declared and every use of it
  // in the caret's statement, a subquery's own alias kept apart from the
  // outer one of the same name. Monaco's keys and right-click menu reach
  // it: F12 or Ctrl/⌘+click goes to the declaration, Shift+F12 lists the
  // uses, F2 renames. A catalog table, or a catalog column a CTE passes on,
  // is found but not renamed (renaming the text would only stop the query
  // finding it), and any other catalog column is not resolved at all; the
  // rename box says so rather than opening.
  async function symbolAt(model, position) {
    if (!dbc.state.ws) return null;
    try {
      const s = await dbc.api("POST", dbc.wsPath("/symbol"), {
        buffer: model.getValue(), caret: model.getOffsetAt(position),
      });
      return s && s.kind ? s : null;
    } catch (_) {
      return null; // a lost server is the status bar's to report
    }
  }

  // a {from, to} of UTF-16 offsets as a Monaco range
  function rangeOf(model, sp) {
    const a = model.getPositionAt(sp.from), b = model.getPositionAt(sp.to);
    return new monaco.Range(a.lineNumber, a.column, b.lineNumber, b.column);
  }

  function registerSymbols() {
    const definition = {
      async provideDefinition(model, position) {
        const s = await symbolAt(model, position);
        return s ? { uri: model.uri, range: rangeOf(model, s.def) } : null;
      },
    };
    const references = {
      async provideReferences(model, position, ctx) {
        const s = await symbolAt(model, position);
        if (!s) return [];
        return s.uses
          .filter((u) => ctx.includeDeclaration || u.from !== s.def.from)
          .map((u) => ({ uri: model.uri, range: rangeOf(model, u) }));
      },
    };
    const rename = {
      // where the rename box opens and what it holds — or why it does not
      // open, said beside the caret
      async resolveRenameLocation(model, position) {
        const s = await symbolAt(model, position);
        const here = new monaco.Range(position.lineNumber, position.column, position.lineNumber, position.column);
        if (!s) return { range: here, text: "", rejectReason: "Rename works on a table alias, a CTE name, or a column the query names itself." };
        if (s.fixed) return { range: here, text: "", rejectReason: s.fixed };
        return { range: rangeOf(model, s.at), text: s.name };
      },
      // The edits come from the server, which quotes the new name as the
      // connection's dialect needs. versionId makes Monaco refuse them if
      // the text changed while the request was out, rather than apply
      // offsets that no longer point at the name.
      async provideRenameEdits(model, position, newName) {
        const versionId = model.getVersionId();
        let r;
        try {
          r = await dbc.api("POST", dbc.wsPath("/rename"), {
            buffer: model.getValue(), caret: model.getOffsetAt(position), name: newName,
          });
        } catch (err) {
          return { edits: [], rejectReason: err.message };
        }
        return {
          edits: (r.edits || []).map((e) => ({
            resource: model.uri, versionId, textEdit: { range: rangeOf(model, e), text: e.text },
          })),
        };
      },
    };
    for (const l of ["sql", "pgsql", "mysql"]) {
      monaco.languages.registerDefinitionProvider(l, definition);
      monaco.languages.registerReferenceProvider(l, references);
      monaco.languages.registerRenameProvider(l, rename);
    }
  }

  // ── loading Monaco ─────────────────────────────────────────────────────
  // The AMD build: loader.js, then vs/editor/editor.main. Its web worker is
  // a same-origin file (static/js/monaco-worker.js) that points the worker
  // at the vendored copy — a data: URI shim, the usual trick, would need
  // worker-src data: in the CSP.
  function load() {
    return new Promise((resolve, reject) => {
      const ver = document.body.dataset.ver || MONACO_VERSION;
      window.MonacoEnvironment = { getWorkerUrl: () => "/static/js/monaco-worker.js?v=" + ver };
      const s = document.createElement("script");
      s.src = BASE + "/vs/loader.js?v=" + ver;
      s.onload = () => {
        // 'require' here is Monaco's AMD loader. urlArgs versions every
        // module URL, so the long cache on /static is safe across builds.
        window.require.config({ paths: { vs: BASE + "/vs" }, urlArgs: "v=" + ver });
        window.require(["vs/editor/editor.main"], () => resolve(), reject);
      };
      s.onerror = () => reject(new Error("could not load " + s.src));
      document.head.append(s);
    });
  }

  // cssVar reads a palette color from /theme.css, so Monaco wears the
  // TUI's colors like everything else on the page.
  const cssVar = (n) => getComputedStyle(document.documentElement).getPropertyValue("--" + n).trim();
  const hex = (n) => cssVar(n).replace("#", "");

  function defineTheme() {
    // the base follows the page's light/dark, so Monaco's own widgets
    // (the find box, hovers) match the palette laid over them
    const light = document.documentElement.dataset.theme === "light";
    monaco.editor.defineTheme("dbc", {
      base: light ? "vs" : "vs-dark", inherit: true,
      rules: [
        { token: "keyword", foreground: hex("accent"), fontStyle: "bold" },
        { token: "operator", foreground: hex("accent") },
        { token: "string", foreground: hex("warn") },
        { token: "number", foreground: hex("warn") },
        { token: "comment", foreground: hex("muted"), fontStyle: "italic" },
        { token: "predefined", foreground: hex("fg") },
      ],
      colors: {
        "editor.background": cssVar("bg"),
        "editor.foreground": cssVar("fg"),
        "editorLineNumber.foreground": cssVar("muted"),
        "editorLineNumber.activeForeground": cssVar("accent"),
        "editorCursor.foreground": cssVar("accent"),
        "editor.selectionBackground": cssVar("sel"),
        "editor.lineHighlightBackground": cssVar("panel"),
        "editorGutter.background": cssVar("bg"),
        "editorWidget.background": cssVar("panel2"),
        "editorWidget.border": cssVar("line"),
      },
    });
  }

  // bindKeys gives Monaco the workbench's chords. Monaco eats a key it has
  // a binding for (Ctrl+Enter inserts a line, Ctrl+K starts a chord), so
  // these must be Monaco commands, not a document listener.
  //
  // BOTH MODIFIERS ON A MAC. Monaco's CtrlCmd is ⌘ there, and the real
  // Ctrl keys carry macOS's emacs-style editing — Ctrl+P is cursor up,
  // Ctrl+K deletes to the end of the line, Ctrl+E jumps to it. Someone
  // coming from the TUI presses Ctrl, so each chord is bound to Ctrl
  // (WinCtrl, in Monaco's names) as well as ⌘. Elsewhere WinCtrl is the
  // Windows key, so it is left alone.
  //
  // THE ASSISTANT IS Ctrl+I, not the TUI's Ctrl+A: in a browser Ctrl+A
  // selects all, in the editor and out of it, and taking it would break
  // the one chord every text field shares. Ctrl+I is the assistant's key in
  // the editors that have one (VS Code's inline chat). Scripts keep Ctrl+O.
  //
  // Ctrl+X explains only with NOTHING selected: with a selection it is
  // cut, as everywhere else in a browser — the TUI's chord bent to fit,
  // not the browser's.
  const isMac = /Mac|iPhone|iPad/.test(navigator.platform || navigator.userAgent);
  function bindKeys() {
    const K = monaco.KeyMod, C = monaco.KeyCode;
    const bind = (mods, key, fn, when) => {
      ed.addCommand(K.CtrlCmd | mods | key, fn, when);
      if (isMac) ed.addCommand(K.WinCtrl | mods | key, fn, when);
    };
    const explain = (analyze) => () => dbc.cmd.explain && dbc.cmd.explain(analyze);
    bind(0, C.Enter, () => dbc.cmd.run(false));
    bind(K.Shift, C.Enter, () => dbc.cmd.run(true));
    bind(0, C.KeyR, () => dbc.cmd.run(false));
    bind(K.Shift, C.KeyR, () => dbc.cmd.run(true));
    bind(0, C.KeyK, () => dbc.cmd.stop());
    bind(0, C.KeyP, () => dbc.cmd.history());
    bind(0, C.KeyE, () => dbc.cmd.exportMenu());
    // explain is SQL's: in a script tab these step aside (dbcScript), and
    // Ctrl+X with nothing selected cuts the line as Monaco does by default
    bind(0, C.KeyX, explain(false), "!editorHasSelection && !dbcScript");
    bind(K.Shift, C.KeyX, explain(true), "!dbcScript");
    ed.addCommand(K.Alt | C.KeyX, explain(true), "!dbcScript"); // the TUI's Alt+X
    // save: a script tab's explicit save; a query tab's console saves
    // itself, and this only flushes it — either way the browser's "save
    // the page" dialog stays out of it
    bind(0, C.KeyS, () => dbc.cmd.save && dbc.cmd.save());
    bind(0, C.KeyI, () => dbc.cmd.assistant());
    // query tabs (Alt: the browser keeps Ctrl+T/W/1…9) and the key list —
    // Monaco's own, or it would type "†" for Alt+T on a Mac and open its
    // command palette on F1 (still in its right-click menu)
    ed.addCommand(K.Alt | C.KeyT, () => dbc.cmd.newTab());
    ed.addCommand(K.Alt | C.KeyW, () => dbc.cmd.closeTab());
    ed.addCommand(K.Alt | C.KeyN, () => dbc.cmd.newConsole());
    ed.addCommand(K.Alt | C.KeyC, () => dbc.cmd.nextConsole());
    for (let n = 1; n <= 9; n++) ed.addCommand(K.Alt | C["Digit" + n], () => dbc.cmd.pickTab(n - 1));
    ed.addCommand(C.F1, () => dbc.cmd.help());
    bind(0, C.KeyO, () => dbc.cmd.scripts());
    // the sidebar fold: on a Mac, Ctrl+B is otherwise Monaco's cursor-left
    // and never reaches the page's own Ctrl+B / ⌘B
    bind(0, C.KeyB, () => dbc.cmd.toggleSidebar && dbc.cmd.toggleSidebar());
    // the TUI's editor menu row, in Monaco's own right-click menu
    // Two actions, one per kind of tab: a menu item's precondition is its
    // "when", so each shows only where it applies, and a script tab's says
    // what the assistant is actually handed (the script, as Go — see
    // chat.js request) instead of calling a Go program "this query".
    ed.addAction({
      id: "dbc.ask", label: "✦ Ask the assistant about this query", contextMenuGroupId: "navigation",
      contextMenuOrder: 0, precondition: "!dbcScript", run: () => dbc.cmd.askAbout("Explain this query."),
    });
    ed.addAction({
      id: "dbc.askScript", label: "✦ Ask the assistant about this script", contextMenuGroupId: "navigation",
      contextMenuOrder: 0, precondition: "dbcScript", run: () => dbc.cmd.askAbout("Explain this script."),
    });
    // Go to Usages: Shift+F12 and the right-click menu, beside Monaco's own
    // Go to Definition, in every tab. It always opens the usages list (the
    // peek), where Monaco's Go to References — Shift+F12's default —
    // second-guesses a short answer: with just the declaration and one use
    // it jumps between them instead of listing, and from the use it does
    // nothing at all. A name used once is the commonest case in both
    // editors (a script's local, a query's alias: SELECT c.name FROM cats
    // c), so the list is asked for outright. The answer is the language's
    // reference provider — registerSymbols for SQL, scripts.js for Go.
    ed.addAction({
      id: "dbc.goToUsages", label: "Go to Usages", contextMenuGroupId: "navigation", contextMenuOrder: 1.5,
      keybindings: [K.Shift | C.F12], precondition: "editorHasReferenceProvider",
      run: () => ed.trigger("dbc", "editor.action.referenceSearch.trigger", {}),
    });
  }

  function start() {
    defineTheme();
    registerCompletion();
    registerSymbols();
    ed = monaco.editor.create(host, {
      // The model is made here, not by the editor from a value: a model
      // the editor made itself is disposed when it switches to another
      // (see useDoc), which would lose the first query tab's document —
      // undo history, cursor and all — on the first tab switch.
      model: newModel(ta.value, docKey),
      theme: "dbc",
      automaticLayout: true, // the splitter resizes the pane; Monaco follows
      minimap: { enabled: false },
      fontSize: 13,
      fontFamily: "ui-monospace, SFMono-Regular, Menlo, Consolas, monospace",
      lineNumbersMinChars: 3,
      scrollBeyondLastLine: false,
      renderLineHighlight: "line",
      tabSize: 4,
      wordWrap: "off",
      glyphMargin: false,
      folding: false,
      lineDecorationsWidth: 8,
      fixedOverflowWidgets: true,
      padding: { top: 8 },
      // Suggestions as you type, from the schema (see registerCompletion)
      // — but never Monaco's word-based ones: words already in the buffer,
      // offered on every letter, were the noise that once kept this popup
      // off altogether. Not in strings or comments. Enter accepts only a
      // pick that changes the text ("smart"), so Enter at the end of a
      // fully typed word still starts a new line. Ctrl+Space asks anywhere.
      quickSuggestions: { other: true, comments: false, strings: false },
      suggestOnTriggerCharacters: true,
      wordBasedSuggestions: "off",
      acceptSuggestionOnEnter: "smart",
      suggest: { showWords: false, showStatusBar: false, preview: false },
      // Off, and not for looks: Monaco's word highlighter (the same word
      // marked elsewhere as the caret rests) disposes a pending delay when
      // the editor switches models — every query tab switch — and rejects
      // a promise nobody holds, an "Uncaught (in promise) Canceled" in the
      // console each time. The TUI has no such highlight to match anyway.
      occurrencesHighlight: "off",
      contextmenu: true,
    });
    decos = ed.createDecorationsCollection();
    scriptCtx = ed.createContextKey("dbcScript", api.isScript());
    if (docKey) docs.set(docKey, { model: ed.getModel(), view: null });
    // keep the caret where it was in the textarea
    const pos = ed.getModel().getPositionAt(ta.selectionStart);
    ed.setPosition(pos);
    ed.onDidChangeModelContent(() => {
      ta.value = ed.getValue();
      changed();
    });
    ed.onDidChangeCursorPosition(scheduleMark);
    bindKeys();
    const hadFocus = document.activeElement === ta;
    wrap.classList.add("monaco-on");
    if (hadFocus) ed.focus();
    scheduleMark();
    for (const fn of readyFns.splice(0)) fn(window.monaco);
  }

  load().then(start).catch((err) => {
    console.error("Monaco did not load; the plain editor stays:", err);
    dbc.log("warn", "the rich editor did not load — using the plain one (" + (err.message || err) + ")");
  });
})();
