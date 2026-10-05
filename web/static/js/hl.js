// dbc web — syntax highlighting for the code blocks in the assistant's
// answers: SQL, Go, Python, JavaScript, JSON and shell.
//
//   dbc.hl(lang, code) → DocumentFragment of text nodes and <span class="hl-k">…
//
// THIS IS codehl/codehl.go (and shell.go) IN JAVASCRIPT, rule for rule: the TUI and the
// page draw the same answer, so they should color it the same. The keyword
// lists below are checked against Go's by web/hl_test.go (keep them as one
// backquoted string per language — the test reads them by that shape); the
// scanning rules are kept in step by hand. Monaco is not used: the page's
// copy carries only the SQL grammars, and colorize() is async, which would
// fight the answer's redraw on every streamed chunk.
//
// Everything is built from text nodes, never innerHTML, so model output
// cannot inject markup.
(function () {
  "use strict";

  const dbc = (window.dbc = window.dbc || {});

  const KEYWORDS = {
    // sqlsplit/lex.go keywords — the editor's SQL vocabulary
    sql: `
      select from where and or not in is null like ilike between exists
      insert into values update set delete returning
      create alter drop table view index sequence schema database if
      primary key foreign references unique check default constraint
      join inner left right full outer cross on using natural
      group by order having limit offset fetch first next rows only
      union all intersect except distinct as asc desc nulls last
      case when then else end cast
      begin commit rollback transaction savepoint release
      with recursive over partition window
      grant revoke explain analyze vacuum pragma show describe
      true false`,
    go: `
      break case chan const continue default defer else fallthrough for
      func go goto if import interface map package range return select
      struct switch type var
      true false nil iota`,
    python: `
      False None True and as assert async await break class continue
      def del elif else except finally for from global if import in is
      lambda nonlocal not or pass raise return try while with yield`,
    js: `
      async await break case catch class const continue debugger default
      delete do else export extends finally for function if import in
      instanceof let new return super switch this throw try typeof var
      void while with yield
      true false null undefined NaN Infinity`,
    json: `true false null`,
    // reserved words plus the declaration/control builtins (codehl/shell.go)
    shell: `
      if then else elif fi for while until do done case esac in function
      export local readonly declare unset return exit break continue`,
  };
  const kw = {};
  for (const k in KEYWORDS) kw[k] = new Set(KEYWORDS[k].trim().split(/\s+/));

  // lang maps a fence tag to its lexer, as codehl.Lang: untagged is SQL (the
  // same call SQL_LANGS makes for ⤓ insert in chat.js) — but see dbc.hl for
  // untagged JSON.
  function lang(tag) {
    switch ((tag || "").trim().toLowerCase()) {
      case "": case "sql": case "postgres": case "postgresql": case "psql":
      case "mysql": case "sqlite": case "pgsql": case "plpgsql":
        return "sql";
      case "go": case "golang": return "go";
      case "python": case "py": case "python3": case "py3": return "python";
      case "js": case "javascript": case "jsx": case "mjs": case "cjs": case "node": return "js";
      case "json": case "jsonc": case "json5": case "jsonl": case "ndjson": return "json";
      case "sh": case "bash": case "shell": case "zsh": case "console":
      case "shell-session": case "shellsession": case "terminal":
        return "shell";
    }
    return "";
  }

  // the C-family languages as data, as codehl's specs
  const SPECS = {
    go: { line: "//", block: true, multi: "`", multiEsc: false },
    python: { line: "#", triple: true, prefixes: true },
    js: { line: "//", block: true, multi: "`", multiEsc: true, dollar: true },
    // JSON is JS's literal syntax; comments are for JSONC
    json: { line: "//", block: true },
  };

  const isDigit = (c) => c >= "0" && c <= "9";
  const isAlpha = (c) => (c >= "a" && c <= "z") || (c >= "A" && c <= "Z");

  // scanLine: '…' or "…" with escapes, to past the close — or to the end of
  // the line (newline excluded) when unterminated, so a misread quote colors
  // one line rather than the rest of the block.
  function scanLine(s, i) {
    const q = s[i];
    let j = i + 1;
    while (j < s.length) {
      const c = s[j];
      if (c === "\\") { j += 2; continue; }
      if (c === q) return j + 1;
      if (c === "\n") return j;
      j++;
    }
    return s.length;
  }

  // scanUntil: a string or comment that may span lines, to past close (or
  // the end of s); esc honors backslash escapes.
  function scanUntil(s, from, close, esc) {
    let j = from;
    while (j < s.length) {
      if (esc && s[j] === "\\") { j += 2; continue; }
      if (s.startsWith(close, j)) return j + close.length;
      j++;
    }
    return s.length;
  }

  // scanNumber is loose — digits, letters, _ and dots, plus a sign after a
  // non-hex exponent — since this colors, it does not parse.
  function scanNumber(s, i) {
    const hex = /^0[xX]/.test(s.slice(i, i + 2));
    let j = i;
    while (j < s.length) {
      const c = s[j];
      if (!(isDigit(c) || isAlpha(c) || c === "_" || c === ".")) break;
      j++;
      if (!hex && (c === "e" || c === "E") && (s[j] === "+" || s[j] === "-")) j++;
    }
    return j;
  }

  // lexC is codehl's spec.lex: one pass, every branch consumes, anything
  // unrecognized stays text. Spans are [start, end, kind] with kind one of
  // k(eyword) s(tring) n(umber) c(omment).
  function lexC(sp, words, s) {
    const out = [];
    const emit = (a, b, k) => { if (b > a) out.push([a, b, k]); };
    // non-ASCII counts as a word character, as in Go (there per byte, here
    // per UTF-16 unit — either way a character is never split)
    const isWord = (c) => c === "_" || c > "\x7f" || isAlpha(c) || (sp.dollar && c === "$");
    const wordBefore = (i) => i > 0 && (isWord(s[i - 1]) || isDigit(s[i - 1]));
    const n = s.length;
    let i = 0;
    while (i < n) {
      const c = s[i];
      if (s.startsWith(sp.line, i)) {
        let j = s.indexOf("\n", i);
        if (j < 0) j = n;
        emit(i, j, "c"); i = j;
      } else if (sp.block && s.startsWith("/*", i)) {
        const j = scanUntil(s, i + 2, "*/", false);
        emit(i, j, "c"); i = j;
      } else if (sp.triple && (s.startsWith('"""', i) || s.startsWith("'''", i))) {
        const j = scanUntil(s, i + 3, s.slice(i, i + 3), true);
        emit(i, j, "s"); i = j;
      } else if (c === '"' || c === "'") {
        const j = scanLine(s, i);
        emit(i, j, "s"); i = j;
      } else if (sp.multi && c === sp.multi) {
        const j = scanUntil(s, i + 1, sp.multi, sp.multiEsc);
        emit(i, j, "s"); i = j;
      } else if ((isDigit(c) || (c === "." && isDigit(s[i + 1] || ""))) && !wordBefore(i)) {
        const j = scanNumber(s, i);
        emit(i, j, "n"); i = j;
      } else if (isWord(c)) {
        let j = i;
        while (j < n && (isWord(s[j]) || isDigit(s[j]))) j++;
        const w = s.slice(i, j);
        // Python's r"", b"", f"", rb"" … : the prefix is part of the string
        if (sp.prefixes && (s[j] === '"' || s[j] === "'") && /^(r|u|b|f|br|rb|fr|rf)$/i.test(w)) {
          const k = (s.startsWith('"""', j) || s.startsWith("'''", j))
            ? scanUntil(s, j + 3, s.slice(j, j + 3), true)
            : scanLine(s, j);
          emit(i, k, "s"); i = k;
          continue;
        }
        // obj.default is a property, not a keyword; ...this is an expression
        const member = i > 0 && s[i - 1] === "." && s.slice(i - 3, i) !== "...";
        if (words.has(w) && !member) emit(i, j, "k");
        i = j;
      } else {
        i++;
      }
    }
    return out;
  }

  // lexSQL follows sqlsplit.Lex: -- and nested /* */ comments, '…' strings
  // (doubled '' and MySQL's backslash escapes), "…"/`…` quoted identifiers
  // (i), $tag$ bodies, and $1 ? :name parameters (p) — but not ::casts.
  // Words are ASCII only there, so they are here.
  function lexSQL(s) {
    const out = [];
    const emit = (a, b, k) => { if (b > a) out.push([a, b, k]); };
    const isWord = (c) => c !== undefined && (c === "_" || isAlpha(c) || isDigit(c));
    const quoted = (i) => { // '' (or "", ``) inside is the quote doubled
      const q = s[i];
      let j = i + 1;
      while (j < s.length) {
        if (s[j] === "\\" && q === "'" && j + 1 < s.length) { j += 2; continue; }
        if (s[j] === q) { if (s[j + 1] === q) { j += 2; continue; } return j + 1; }
        j++;
      }
      return s.length;
    };
    const blockComment = (i) => { // PostgreSQL nests them
      let depth = 0;
      while (i < s.length) {
        if (s.startsWith("/*", i)) { depth++; i += 2; }
        else if (s.startsWith("*/", i)) { i += 2; if (--depth === 0) return i; }
        else i++;
      }
      return s.length;
    };
    const n = s.length;
    let i = 0;
    while (i < n) {
      const c = s[i];
      if (s.startsWith("--", i)) {
        let j = s.indexOf("\n", i);
        if (j < 0) j = n;
        emit(i, j, "c"); i = j;
      } else if (s.startsWith("/*", i)) {
        const j = blockComment(i);
        emit(i, j, "c"); i = j;
      } else if (c === "'") {
        const j = quoted(i); emit(i, j, "s"); i = j;
      } else if (c === '"' || c === "`") {
        const j = quoted(i); emit(i, j, "i"); i = j;
      } else if (c === "$") {
        const tag = s.slice(i).match(/^\$(?![0-9])\w*\$/);
        if (tag) {
          const j = scanUntil(s, i + tag[0].length, tag[0], false);
          emit(i, j, "s"); i = j;
          continue;
        }
        let j = i + 1;
        while (j < n && isDigit(s[j])) j++;
        emit(i, j, "p"); i = j;
      } else if (c === "?") {
        emit(i, i + 1, "p"); i++;
      } else if (c === ":" && isWord(s[i + 1]) && !isDigit(s[i + 1]) && s[i - 1] !== ":") {
        let j = i + 1;
        while (j < n && isWord(s[j])) j++;
        emit(i, j, "p"); i = j;
      } else if (isDigit(c) && !isWord(s[i - 1])) {
        let j = i;
        while (j < n && (isDigit(s[j]) || s[j] === "." || s[j] === "e" || s[j] === "E")) j++;
        emit(i, j, "n"); i = j;
      } else if (isWord(c)) {
        let j = i;
        while (j < n && isWord(s[j])) j++;
        if (kw.sql.has(s.slice(i, j).toLowerCase())) emit(i, j, "k");
        i = j;
      } else {
        i++;
      }
    }
    return out;
  }

  // lexShell is codehl's: words run to whitespace or an operator (so
  // --if-exists is one word), # comments only at a word start, '…' without
  // and "…" with escapes — both may span lines, but an unclosed one ends at
  // its own line — $variables as p, heredoc bodies as strings, and no
  // numbers (in shell they are arguments).
  function lexShell(s) {
    const out = [];
    const emit = (a, b, k) => { if (b > a) out.push([a, b, k]); };
    const isName = (c) => c !== undefined && (c === "_" || isAlpha(c) || isDigit(c));
    const isWord = (c) => c !== undefined && (isName(c) || c > "\x7f" || "-./:=+@%,~^".includes(c));
    const wordStart = (i) => i === 0 || " \t\r\n;|&()".includes(s[i - 1]);
    const quote = (i) => {
      const q = s[i];
      for (let j = i + 1; j < s.length; j++) {
        if (s[j] === "\\" && q === '"') j++;
        else if (s[j] === q) return j + 1;
      }
      const nl = s.indexOf("\n", i);
      return nl < 0 ? s.length : nl;
    };
    const variable = (i) => { // end of $NAME, ${…} or $? …, or i for none
      const c = s[i + 1];
      if (c === "{") {
        for (let j = i + 2; j < s.length; j++) {
          if (s[j] === "}") return j + 1;
          if (s[j] === "\n") return j;
        }
        return s.length;
      }
      if (c !== undefined && (c === "_" || isAlpha(c))) {
        let j = i + 2;
        while (isName(s[j])) j++;
        return j;
      }
      if (c !== undefined && (isDigit(c) || "?#@!$*-".includes(c))) return i + 2;
      return i;
    };
    const heredocEnd = (from, term) => { // its terminator line, blanks ignored
      for (let at = from; at < s.length;) {
        let end = s.indexOf("\n", at);
        if (end < 0) end = s.length;
        if (s.slice(at, end).trim() === term) return end;
        at = end + 1;
      }
      return s.length;
    };
    const n = s.length;
    let heredoc = "", i = 0;
    while (i < n) {
      const c = s[i];
      if (c === "\n" && heredoc) {
        const j = heredocEnd(i + 1, heredoc);
        emit(i + 1, j, "s");
        i = j; heredoc = "";
      } else if (c === "#" && wordStart(i)) {
        let j = s.indexOf("\n", i);
        if (j < 0) j = n;
        emit(i, j, "c"); i = j;
      } else if (c === "'" || c === '"') {
        const j = quote(i);
        emit(i, j, "s"); i = j;
      } else if (c === "$") {
        const j = variable(i);
        emit(i, j, "p"); i = Math.max(j, i + 1);
      } else if (s.startsWith("<<<", i)) {
        i += 3; // a here-string: its word is an argument
      } else if (s.startsWith("<<", i)) {
        // <<[-] [quote]NAME[quote]; NAME starts like a name, so the 1<<2
        // of $((…)) is a shift
        const m = s.slice(i + 2).match(/^-?[ \t]*(?:'([A-Za-z_]\w*)'|"([A-Za-z_]\w*)"|([A-Za-z_]\w*))/);
        if (m) {
          const word = m[1] || m[2] || m[3];
          const end = i + 2 + m[0].length;
          emit(end - word.length - (m[3] ? 0 : 2), end, "s"); // the word, quotes and all
          heredoc = word;
          i = end;
        } else {
          i += 2;
        }
      } else if (isWord(c)) {
        let j = i;
        while (j < n && isWord(s[j])) j++;
        if (kw.shell.has(s.slice(i, j))) emit(i, j, "k");
        i = j;
      } else {
        i++;
      }
    }
    return out;
  }

  // dbc.hl draws code as text nodes with <span class="hl-…"> around each
  // classified run; an unhighlighted language comes back as one text node.
  dbc.hl = function (tag, code) {
    const frag = document.createDocumentFragment();
    let l = lang(tag);
    // untagged and opening with { or [ is JSON, not SQL (codehl.Lex): only
    // the colors change, ⤓ insert is still offered by tag
    if (l === "sql" && !(tag || "").trim() && /^\s*[{[]/.test(code)) l = "json";
    const spans = l === "sql" ? lexSQL(code) : l === "shell" ? lexShell(code)
      : SPECS[l] ? lexC(SPECS[l], kw[l], code) : [];
    let at = 0;
    for (const [a, b, k] of spans) {
      if (a > at) frag.append(code.slice(at, a));
      const sp = document.createElement("span");
      sp.className = "hl-" + k;
      sp.textContent = code.slice(a, b);
      frag.append(sp);
      at = b;
    }
    if (at < code.length) frag.append(code.slice(at));
    return frag;
  };
})();
