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
  // The plain textarea has no such state to keep, so it just swaps text.
  const docs = new Map(); // tab key → {model, view}
  let docKey = "";

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
      if (ed) monaco.editor.setModelLanguage(ed.getModel(), lang);
    },
    setHeight(px) {
      wrap.style.height = px + "px";
    },
    height: () => wrap.getBoundingClientRect().height,
    isMonaco: () => !!ed,
    element: wrap,
    // useDoc shows query tab key's document, creating it with text the
    // first time. It fires no change: switching tabs edits nothing.
    useDoc(key, text) {
      if (key === docKey) return;
      if (!ed) {
        ta.value = text;
        docKey = key;
        return;
      }
      const cur = docs.get(docKey);
      if (cur) cur.view = ed.saveViewState();
      let d = docs.get(key);
      if (!d) {
        d = { model: monaco.editor.createModel(text, lang), view: null };
        docs.set(key, d);
      }
      docKey = key;
      monaco.editor.setModelLanguage(d.model, lang);
      ed.setModel(d.model);
      if (d.view) ed.restoreViewState(d.view);
      ta.value = ed.getValue();
      scheduleMark();
    },
    // dropDoc forgets a closed tab's document.
    dropDoc(key) {
      const d = docs.get(key);
      if (d && key !== docKey) { d.model.dispose(); docs.delete(key); }
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
    bind(0, C.KeyX, explain(false), "!editorHasSelection");
    bind(K.Shift, C.KeyX, explain(true));
    ed.addCommand(K.Alt | C.KeyX, explain(true)); // the TUI's Alt+X
    bind(0, C.KeyI, () => dbc.cmd.assistant());
    // query tabs (Alt: the browser keeps Ctrl+T/W/1…9) and the key list —
    // Monaco's own, or it would type "†" for Alt+T on a Mac and open its
    // command palette on F1 (still in its right-click menu)
    ed.addCommand(K.Alt | C.KeyT, () => dbc.cmd.newTab());
    ed.addCommand(K.Alt | C.KeyW, () => dbc.cmd.closeTab());
    for (let n = 1; n <= 9; n++) ed.addCommand(K.Alt | C["Digit" + n], () => dbc.cmd.pickTab(n - 1));
    ed.addCommand(C.F1, () => dbc.cmd.help());
    bind(0, C.KeyO, () => dbc.cmd.scripts());
    // the TUI's editor menu row, in Monaco's own right-click menu
    ed.addAction({
      id: "dbc.ask", label: "✦ Ask the assistant about this query", contextMenuGroupId: "navigation",
      contextMenuOrder: 0, run: () => dbc.cmd.askAbout("Explain this query."),
    });
  }

  function start() {
    defineTheme();
    ed = monaco.editor.create(host, {
      // The model is made here, not by the editor from a value: a model
      // the editor made itself is disposed when it switches to another
      // (see useDoc), which would lose the first query tab's document —
      // undo history, cursor and all — on the first tab switch.
      model: monaco.editor.createModel(ta.value, lang),
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
      // SQL is typed by someone who knows what they want; the word-based
      // popup that fires on every letter is noise here. Ctrl+Space asks.
      quickSuggestions: false,
      suggestOnTriggerCharacters: false,
      // Off, and not for looks: Monaco's word highlighter (the same word
      // marked elsewhere as the caret rests) disposes a pending delay when
      // the editor switches models — every query tab switch — and rejects
      // a promise nobody holds, an "Uncaught (in promise) Canceled" in the
      // console each time. The TUI has no such highlight to match anyway.
      occurrencesHighlight: "off",
      contextmenu: true,
    });
    decos = ed.createDecorationsCollection();
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
  }

  load().then(start).catch((err) => {
    console.error("Monaco did not load; the plain editor stays:", err);
    dbc.log("warn", "the rich editor did not load — using the plain one (" + (err.message || err) + ")");
  });
})();
