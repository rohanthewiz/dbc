# Script tabs: Go to Definition, Go to Usages, and inline errors like ced

Session: `fdf0935a-2959-447a-92d3-499ca994c30e`

## Ask

1. Dropped in from the cats-todo backlog:
   - in the scripts editor add "Go to Definition" and "Go to Usages";
   - make Go errors more obvious, as ced (`~/projs/go/ced`) shows them, from
     a screenshot: a red dot in the gutter, a box on the error's spot, and
     `× missing ',' before newline in argument list` after the line.
2. `/sw` (sess-wrap).

## Decisions

- **Web only.** The "scripts editor" is dbc web's Monaco script tab. The TUI
  edits scripts in `$EDITOR` (`tui/scripts.go`), so it has no editor to add
  this to.
- **go/types, not a text search.** Scripts shadow freely (`err` in every
  block), so `script.Resolve` runs the type checker over the editor's text
  and binds each identifier to its real object. That also covers selectors
  and composite-literal keys of types the script declares.
- **Fake imports.** Every import becomes an empty, complete
  `types.Package` named by Go's convention (`pkgName`: the last path
  element, skipping `/vN` and trimming `.vN`). A real importer would need the
  stdlib's source or export data on the user's machine, and sdb cannot be
  loaded from inside the binary at all. Errors are swallowed
  (`Config.Error`). So only what the script declares resolves, which is all
  a jump could reach anyway.
- **Imported members match by shape.** `s.Query` and `sdb.Copy` stay
  unbound, so they have no definition, but their usages are listed: every
  selector of the same name from the same variable or package object.
  `kind: "member"`, `def: null`.
- **Type switch.** `switch x := v.(type)` gives each clause its own implicit
  object. These, and the switch's own `x` ident, are folded onto one
  representative (`typeSwitchAliases`).
- **Route outside any workspace.** `POST /api/v1/script-symbol {text,
  caret}` sits beside `script-check`, because a script belongs to no tab's
  connection. It is not under `/api/v1/scripts/`, which rweb's radix router
  keeps for `:name` (see `web/scripts.go`). Offsets are UTF-16 both ways,
  reusing `byteOffset` and `utf16Counter`.
- **Our own "Go to Usages" action.** Monaco's built-in Go to References
  (Shift+F12) with exactly two references (the declaration and one use)
  jumps between them instead of listing them, and from the use it does
  nothing. Found in the e2e. Script tabs bind Shift+F12, and a right-click
  menu item labelled "Go to Usages" (the user's wording), to
  `editor.action.referenceSearch.trigger`, which always opens the list. The
  SQL editor has the same quirk; raised as N-165 rather than changed here.
- **Every diagnosed line shows its message**, not just the caret's line as
  ced does. ced's rule is meant for gopls's dozens of findings. A script
  check reports at most ten parse errors, or one yaegi error plus lints, and
  the point is to see an error without moving the caret. The worst diag on a
  line wins, with "(+n more)" after it. The message is cut at 160
  characters; the hover has all of it.
- **End-of-line errors box the last character.** go/parser puts "missing
  ','" at the newline. A mark there was drawn after the injected note, with
  the squiggle under the note's tail. ced boxes the last character in this
  case, and so does dbc now (`diagRange`).
- **Decorations per model** (`model.deltaDecorations`, ids kept on the doc
  entry), so they stay with each script tab's document across tab
  switches. They are replaced by the next check, about 600 ms after typing
  stops.

## Changes

- `script/symbol.go` (new): `Resolve(src, caret) Symbol{Kind, Name, At,
  Def *Span, Uses}` with `identAt` (a caret just past a name counts as on
  it), `typeSwitchAliases`, `selectorUses`, `kindOf`, `declSpan` (an
  unnamed import is declared by its path literal, which joins the uses),
  `fakeImporter` and `pkgName`. Parses with
  `AllErrors|SkipObjectResolution`. A broken script still resolves what
  parsed. A panic gives an empty symbol.
- `web/scripts.go`: `handleScriptSymbol` and the route table comment.
  `web/server.go`: `r.Post("/api/v1/script-symbol", …)`.
- `web/static/js/scripts.js`: definition and reference providers for
  `"go"` inside `register(monaco)`, and a header section on them.
- `web/static/js/editor.js`:
  - `setMarkers` now also sets `diagDecos` (gutter dot via
    `linesDecorationsClassName`, `after:` injected note on a whole-line range
    with AlwaysGrows stickiness, `diag-at` box per diag);
  - the marker range is factored into `diagRange`;
  - new `dbc.goToUsages` action (precondition `dbcScript`).
- `web/static/css/app.css`: `.diag-dot`, `.diag-at`, `.diag-note` (err and
  warn).
- `web/static/js/app.js`: help lists F12 and Shift+F12 under Script tabs, and
  ✓ Check's line mentions the inline message.

## Tests

- `script/symbol_test.go`: `TestResolve` covers the outer and shadowing
  `err`, a helper func, a field by selector and by literal key, a type, the
  type-switch var from a clause and from the switch, a label, a caret after
  a name, an sdb method (no def, uses from the same `s`), the package (def is
  the path literal) and a builtin. Also `TestResolveNothing`,
  `TestResolveBrokenScript` and `TestPkgName`.
- `web/scripts_test.go` `TestScriptSymbol`: UTF-16 in and out, with a cat
  emoji (two UTF-16 units) before the name.
- e2e `scriptTabs` (`web/e2e/scripts_test.go`): the error's dot, box and
  end-of-line message, then F12 to the outer `n` and Shift+F12 listing
  exactly its two rows, not the `if`'s `n`.
- `go test ./...` is green (CATS_* stripped). `DBC_E2E=1 go test` in
  `web/e2e` is green.
- A headless screenshot of `s.Print("Hello, World!"` matched ced's look.
  Dark theme only (N-166).
- Pitfalls found on the way are recorded in the e2e memory. Monaco draws an
  injected text's spaces as U+00A0, so test it with `\s`, not
  `includes("a b")`. Also the two-reference Go to References quirk above.

## Next

Closed: None. Declined: None. Raised: N-164, N-165, N-166.
Deferred: None. Promoted: None. Moved: None.
Updated: None. Full list: `ai_docs/todo/next-list.md`.
