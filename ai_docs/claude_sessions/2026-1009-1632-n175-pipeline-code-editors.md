# N-175: small Monaco editors for the pipeline inspector's code fields

Session: `79e76aa6-6001-43a8-ba2b-43204e56d9bb`

## Ask

1. `/next-list` — review the living list (living-list mode, n = 15).
2. N-175, from the cats-todo backlog: the pipeline inspector's `sql` and
   `go` fields were plain code boxes (`pipelines.js` `field()`: a textarea,
   Tab indents). Use small Monaco editors — the script tab's Go completion
   and its markers — disposed when the selection moves.
3. `/sw n175-pipeline-code-editors`.

## The next-list review

- History clean: every ID that ever appeared in the file's git history is
  still in it (191), none duplicated, **Next ID** right; the last 15 session
  docs all use the `Closed: … Raised: …` summary and reconcile with the file.
- **N-170's premise was wrong.** It said this machine has only Homebrew's
  postgresql@16. It has libpq 17.5 (`/opt/homebrew/opt/libpq/bin`, first
  in `pgdump.probeDirs`) and no postgresql@16 keg, so PG17 dumps here; the
  gap is PG18, the newest `pgdocker.Versions` offers. Corrected in place.
- **N-156 confirmed still failing** on HEAD against a throwaway postgres:17
  container (stopped after): `TestLiveWorkspaceNotices` gets five notes —
  NOTICE, "statement 1/2: 0 affected — DO …", WARNING, "statement 2/2: …",
  the done note — where it wants three. Value raised medium → high: every
  live `./workspace` run is red on it.
- Every other Open and Validate premise held against the code. Roadmap
  candidates to promote (left to the user): N-143 (pipelines' `sql.write`
  is an `etl.Writer`, so a `go.transform` handing it slices/maps on
  MySQL/SQLite is a second route into the same refusal). N-161 not ready:
  Docker Hub has `19beta4`, no `19`.

## How N-175 was done

### `editor.js` — `mini(box, opts)`

A small Monaco on a model of its own, returned as
`{editor, setMarkers, focus, hasFocus, dispose}`, or `null` before Monaco
has loaded (the caller keeps its textarea then).

- **Height follows the text** between `minLines` and `maxLines`
  (`onDidContentSizeChange`), since a field is one of several in a
  scrolling pane; `alwaysConsumeMouseWheel: false` so the wheel at its ends
  still moves the inspector. `fixedOverflowWidgets` lets the suggest box
  and hovers escape the inspector's overflow clip.
- **Go gets everything registered for "go"** — scripts.js's sdb completion,
  hover, the symbol providers — because Monaco's providers are per
  language, not per editor. No new wiring was needed for that.
- **`setMarkers`** reuses the main editor's `diagRange` / `diagDecos`: the
  squiggles plus the gutter dot, box and end-of-line note.
- **`onChange` runs in a microtask**, after Monaco's change event: what it
  starts (`changed` → render) may redraw the inspector and dispose the very
  editor whose event is still being delivered.
- **Chords.** The workbench's `bindKeys` uses `addCommand`, and standalone
  Monaco has one keybinding service for every editor on the page, so those
  chords fire in a mini too: Ctrl+S saves and Ctrl+Enter runs, as from the
  canvas. But their when-clauses read context keys the main editor set, and
  in a mini `dbcScript` is unset, so `!dbcScript` let **Ctrl+X explain** the
  tab's (hidden JSON) query instead of cutting. Each mini sets
  `dbcScript = true`. Verified both ways in the e2e: with the key removed,
  explain fired twice (Ctrl+X and ⌘X); with it, zero.
- **`minis` (a WeakSet of their models)**: the workspace's SQL completion
  and symbol providers skip them. They complete and resolve against the
  tab's connection, and a pipeline tab has none (it would only log
  "completion is without the schema").
- `langOf(driver)` factored out of `setDriver`, shared with the inspector.

### `pipelines.js`

- `field()` hands `sql` and `go` to `codeField()`, which falls back to the
  old textarea when `mini` returns null. The row is a `<div>`, not `row()`'s
  `<label>`: a click in a label is forwarded to its control — Monaco's
  hidden textarea — which fights the editor's own mouse handling.
- SQL fields are coloured in the dialect of the node's `conn` field: the
  driver from the sidebar's `.conn-item[data-driver]` (`sqlLang`).
- `codeEds` holds the live editors of the current drawing; `dropCode()`
  disposes them before every inspector redraw and on `hide()`.
  `renderInspector`'s guard (focus inside the pane + same selection → only
  `drawDiags`) already covers Monaco, whose input is a textarea in the box,
  so typing keeps the same editor across checks.
- `drawDiags` now also marks each code field (`fieldMarks`). The check says
  where in three ways: a Go field's `code:L:C: …` (script's `checkSnippet`,
  already on snippet lines), `code: no func Apply…` (the field, no place),
  and a field's own diag `where: "frag/id.field"` (marked on the `${…}` it
  names when the text has it, else line 1). Matched on the whole
  `frag/id.field`, since a node id may hold dots — which exposed N-195.
- `dbc.editor.ready` in `mount()` redraws an inspector drawn before Monaco
  loaded (not under a caret).

### `scripts.js` — `varType` reads parameters

`[(,]\s*v\s+\*?sdb\.T` as a third form, so `b.` in
`func Apply(b *sdb.Batch)` / `e.` in `func Next(e *sdb.Env)` complete and
hover. Plugin-file tabs benefit too.

### `app.css`

`.ifield input/textarea/select` (and `:focus`, `resize`, `.code`) scoped to
direct children (`>`): the descendant rules would have given Monaco's hidden
input a border, padding and `width: 100%`. Every existing field's control is
a direct child. New `.ifield .icode` border/focus ring.

### Docs

README's pipeline-inspector paragraph. The plan's Phase 2 outcome ("code
boxes, not Monaco mini-editors") was left as the record of what Phase 2 did.

## Verification

- Web e2e, whole suite in headless Chrome: all steps pass (two
  Postgres-only steps skip without `DBC_LIVE_PG_DSN`; "postgres in docker
  dialog" logs Docker as not running and passes).
  - "pipeline tabs" now types its query through the mini (`codeEd(name)`,
    a page helper found by `monaco.editor.getEditors()` inside
    `.pinsp .ifield[data-field=name]`) and asserts its language is `sql`.
  - New `pipelineCodeField` (after ■ Stop, so the "Run saves first"
    assertion keeps its meaning): `varType` cases incl. a parameter; the go
    field opens as Go, one editor more than before; `e.` typed as keys
    offers `Ctx` and `Params`; a compile error marked on line 2 with its
    note, in the same editor still focused, and in the list; Ctrl+X / ⌘X
    do not explain; Ctrl+S from inside saves; selecting another card
    disposes the editor (count back to base).
  - Pitfall: `ed.trigger("…", "type", {text: "e."})` did not open the
    suggest widget; real key presses (`p.Keyboard.MustType`) do, as the
    script step does.
- `go test ./web/` passes; `node --check` on the three JS files.
- Screenshot `pipeline-code-field` (DBC_E2E_SHOTS) looked right: Go
  colouring, gutter dot, box on the undefined name, the note cut at the
  280px inspector's edge (N-194).

## Files

- `web/static/js/editor.js` — `mini`, `minis`, `langOf`, provider guards
- `web/static/js/pipelines.js` — `codeField`, `codeEds`/`dropCode`,
  `fieldMarks`, `sqlLang`, redraw on Monaco ready, dispose on hide
- `web/static/js/scripts.js` — `varType` parameters
- `web/static/css/app.css` — `.ifield >` scoping, `.icode`
- `web/e2e/pipelines_test.go` — query via the mini, `pipelineCodeField`,
  `defineCodeEd`
- `README.md` — the inspector paragraph
- `ai_docs/todo/next-list.md`

## Next

Closed: N-175. Declined: None. Raised: N-192, N-193, N-194, N-195.
Deferred: None. Promoted: None. Moved: None.
Updated: N-064, N-156 (value medium → high), N-170. Full list: `ai_docs/todo/next-list.md`.
