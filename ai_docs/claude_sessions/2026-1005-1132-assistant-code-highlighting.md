# Syntax highlighting in the assistant's code blocks

Session: `d9eac285-8899-49ac-a642-672843eafcdb`

## Ask

From the cats-todo backlog: "Add syntax highlighting to sql, Go, python, and
JS code blocks in the AI assistant's response". Then, after asking which
other languages would help: "yes, add JSON and shell".

Both assistant panes draw answers themselves: the TUI in `tui/chat.go`
(`agentRows`), the web pane in the browser (`web/static/js/chat.js`
`renderAnswer` → `codeBlock`). Both got highlighting.

## Design

- **A hand-written lexer, not chroma.** Answers are short, are redrawn every
  frame while they stream, and only a few languages matter. A table-driven
  scanner (comments, strings, numbers, keyword list) has no dependency and
  degrades gracefully: an unknown construct is uncolored, never an error.
- **SQL reuses the editor's scanner** (`sqlsplit.Lex`), so a statement looks
  the same in an answer as it does after ⤓ insert.
- **Same colors as the editor**: TUI `syn*` styles laid on the code block's
  background; web CSS classes using the Monaco theme's variables (keyword
  `--accent` bold, string/number `--warn`, comment `--muted` italic,
  quoted identifier underlined, param `--err`).
- **Two copies of the lexer** (Go for the TUI, JS for the page). Monaco was
  ruled out: the vendored copy carries only the SQL grammars, and
  `colorize()` is async, which would fight the redraw on every streamed
  chunk. Keyword lists are kept equal by a test; scanning rules by hand.
  Cross-checked with node on 30 shared inputs: identical output.
- **Untagged fences are SQL** (the same call `isSQLLang`/`SQL_LANGS` make
  for ⤓ insert), except an untagged block whose first non-blank byte is `{`
  or `[`: that is drawn as JSON, since as SQL its `"keys"` would be drawn as
  quoted identifiers. Only colors change; ⤓ insert is still offered by tag.

## Changes

- **`codehl/`** (new package)
  - `codehl.go`: `Lang(tag)`, `Lex(tag, src) []Span`, `Keywords(tag)`.
    Kinds: Keyword, String, Number, Comment, Ident, Param. One `spec`
    scanner serves Go, Python, JS and JSON: Go raw strings, Python
    triple quotes and `r""`/`b""`/`f""` prefixes, JS templates (whole
    template one string), member access (`obj.default`) not a keyword,
    `...this` still one. Single-line strings stop at the newline when
    unterminated, so a misread quote (a JS regex literal) colors one line.
  - `shell.go`: own scanner. Words run to whitespace/operators
    (`--if-exists` is not `if`), `#` comments only at a word start,
    `'…'`/`"…"` span lines but an unclosed one ends at its line (the
    "doesn't" in console output), `$NAME`/`${…}`/`$?` as Param, heredoc
    bodies (`psql <<'SQL'`) as strings, `<<<` and `$((1<<2))` not heredocs,
    numbers left plain.
  - Tags: SQL dialect names; `go`/`golang`; `python`/`py`/`python3`/`py3`;
    `js`/`javascript`/`jsx`/`mjs`/`cjs`/`node`;
    `json`/`jsonc`/`json5`/`jsonl`/`ndjson`;
    `sh`/`bash`/`shell`/`zsh`/`console`/`shell-session`/`shellsession`/`terminal`.
- **`sqlsplit/lex.go`**: exported `Keywords()` (sorted) so codehl and,
  through the test, `hl.js` share the editor's SQL vocabulary.
- **`tui/chat.go`**: `agentRows` lexes the whole block (multi-line strings
  and comments keep their color), then colors each wrapped part by byte
  offset (`wrap` only splits, so parts concatenate back exactly). New
  `codeKinds` (per-byte kind table), `codeSegs`, `codeStyle`.
- **`web/static/js/hl.js`** (new): `dbc.hl(tag, code)` → DocumentFragment
  of text nodes and `<span class="hl-k|s|n|c|i|p">`; never innerHTML.
  Loaded before `chat.js` (`web/pages/workbench.go` scripts), listed in
  `core.js`'s module map; `chat.js` `codeBlock` uses it.
- **`web/static/css/app.css`**: `.cblock .hl-*` rules.

## Tests

- `codehl`: `TestLex` (every language, untagged JSON sniff, shell edge
  cases), `TestLexSpansAreWellFormed` (rune boundaries, no overlap),
  `TestLang`.
- `tui`: `TestAgentCodeIsHighlighted` (Go keywords, a raw string across
  two lines, comment, plain text on the code background, unknown language
  plain).
- `web`: `TestHLKeywordsMatchCodehl` reads `hl.js`'s backquoted keyword
  lists and compares each with `codehl.Keywords`.
- `web/e2e`: new step "code blocks highlighted" builds a block through
  `dbc.hl` in the live page and checks the computed colors against the
  theme variables. `DBC_E2E=1` suite passes (~15 s).
- `go vet ./...` and full `go test ./...`: all pass.

## Known limits

- JS regex literals aren't recognized: a quote inside one (`/it's/`)
  opens a string, limited to that line.
- Lexical only: a shell `in` or SQL keyword used as a plain word is colored.

## Next

Closed: None. Declined: None. Raised: N-107, N-108.
Deferred: None. Promoted: None.
Updated: None. Full list: `ai_docs/todo/next-list.md`.
