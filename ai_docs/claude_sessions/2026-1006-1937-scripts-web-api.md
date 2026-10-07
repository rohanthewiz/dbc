# Scripts revamp phase 3: the web API (plus bytdb v0.21.2)

Session: `b9d64dc9-c2da-497d-8e17-8af6fa87c155`

## Ask

`/sess-load`, then "start on phase 3" of `ai_docs/plans/scripts-revamp.md`
(the web API). Interrupted early with: bump the bytdb pin to `v0.21.2`, then
continue.

## bytdb v0.21.2

- `go get github.com/rohanthewiz/bytdb@v0.21.2` and `go mod tidy`. Only
  the root `go.mod` pins bytdb; the e2e modules don't.
- v0.21.2's change is in `dml.go`: `coerce` takes an integer 0/1 for a
  bool column, and any other integer is an error. That is the upstream fix
  for N-131.
- `etl/copy_test.go` `TestCopyBoolToBytdbCreates` copies a SQLite BOOLEAN
  (1, 0, NULL) into a created bytdb table, then filters `WHERE adopted`. It
  passes on v0.21.2 and fails on v0.21.1 with "value does not fit column
  type". N-131 is closed.

## Routes (`web/scripts.go`, registered in `web/server.go`)

| Route | Does |
| --- | --- |
| `GET /api/v1/scripts` | `{dir, short, scripts: [ScriptInfo], examples, templates, trash: [TrashInfo]}`, each `[]` when empty |
| `GET /api/v1/scripts/:name` | `{text, rev}`; missing → 404 |
| `PUT /api/v1/scripts/:name?win=` | `{text, base}` → `{rev}` or `{conflict, text, rev}` |
| `POST /api/v1/scripts/:name/rename?win=` | `{to}` → `{name}`; taken → 409 |
| `DELETE /api/v1/scripts/:name?win=` | to `.trash` → `{id}` |
| `POST /api/v1/script-trash/:id/restore?win=` | `{to?}` → `{name}`; taken → 409, unknown id → 404 |
| `POST /api/v1/script-check` | `{name, text}` → `{diags}`, for unsaved text |
| `GET /api/v1/script-examples/:name` | `{name, desc, text}` |
| `GET /api/v1/script-templates/:name?conn=` | `{text}`, filled with connection names |
| `GET /api/v1/script-api` | the embedded sdb API JSON |

- Every `:name` is checked with `ValidScriptName` (400) before the store
  is called. `scriptErr` maps `fs.ErrNotExist` to 404, `ErrScriptExists`
  to 409 and `ErrBadScriptName` to 400.
- **Why `script-check` and not `scripts/check`, as the plan had it.**
  rweb's radix router doesn't backtrack. A GET of `trash.go` beside a
  literal `templates/` child (they share the "t") came back "no such
  endpoint". Any name sharing a literal's first letters (`export_report.go`
  vs `examples/`) would have broken too. So only `:name` sits under
  `/api/v1/scripts/`. `TestScriptNamesBesideRouteWords` covers `check.go`,
  `api.go`, `trash.go`, `examples.go`, `templates.go`, `export_report.go`,
  `script-check.go` and `rename.go`. Raised as N-133.
- A save follows the console protocol, with the store's own lock
  (`userdata.SaveScript`):
  - `base ""` over an existing file → 409.
  - A file deleted since its base was read → a conflict with rev "" and
    no text.
  - The size cap is `userdata.MaxScriptBytes`, renamed from
    `maxScriptBytes`.
- Templates go beyond the plan. The connection order is `?conn=` when it
  is a known connection, then the default, then the rest, each once
  (`templateConns`). New and Duplicate are the page PUTting text with
  `base ""`; there is no server-side create-from route.
- The window-level `scripts` event (`hub.broadcast`) is
  `{op: saved|renamed|trashed|restored, name, to?, rev?, id?, win}`. It
  carries no text, since a script can be 1 MiB and only a window with it
  open needs it.
- `POST /api/v1/ws/:id/script` (run) is unchanged.
- `web/static/js/app.js`: the Ctrl+O picker reads `sc.name` and shows
  `name — desc`, with the desc as a tooltip. That is the only page change;
  phase 4 replaces the picker.

## `sdb/sdbapi`

- `Build(root)` parses `sdb` with `go/parser` and `go/doc`. It skips
  `_test.go` and `//go:build ignore` files.
- An alias of another package in the module (`CopyOpts = etl.CopyOptions`)
  is followed through the declaring package's imports. One level only.
- Output: `API{Package, Doc, Funcs, Types}`.
  - A Type is `{Name, Of, Kind, Doc, Fields, Methods}`.
  - A Func is `{Name, Sig, Doc, Params, ConnArgs}`. `ConnArgs` holds the
    indices of string params named conn, src or dst, so `Copy` is `[0,1]`.
- `hostOnly` (`New`, `WithContext`, `Release`) is left off S, because a
  script must not call them. `TestHostOnlyExist` keeps the list real.
- `Encode` writes indented JSON plus a newline. `gen.go` (`//go:build
  ignore`, `//go:generate go run gen.go`) writes `api.json` (about 34 KB),
  which is embedded and served by `JSON()`.
- `TestAPIUpToDate` rebuilds the JSON and compares it to the embedded file.
  It says to run `go generate ./sdb/sdbapi`. `TestAPIShape` checks what
  the editor relies on.

## Docs

- The plan's header now reads "Phases 1–3 are done", and phase 3 has an
  Outcome. It records the route move, the list shape, the template route
  and the event.
- `next-list.md`: N-130 updated, N-131 closed, N-133 raised.

## Verification

- `go build ./... && go vet ./... && go test ./...`: all pass. gofmt is
  clean, and `node --check web/static/js/app.js` passes.
- New tests:
  - `web/scripts_test.go`: `TestScriptSaveRevisions`,
    `TestScriptNamesChecked`, `TestScriptNamesBesideRouteWords`,
    `TestScriptRenameTrashRestore`, `TestScriptCheckRoute` (check never
    runs `init()` or creates the dir), `TestScriptExamplesAndTemplates`
    (every filled template passes Check), `TestTemplateConns`,
    `TestScriptAPIRoute`.
  - `sdb/sdbapi`: `TestAPIUpToDate`, `TestAPIShape`, `TestHostOnlyExist`.
  - `TestScriptListAndRun` was updated for `[]ScriptInfo`.
- The real binary ran with an isolated HOME, CATS_* unset and `--secret`,
  driven with curl:
  - The list came back empty, with examples and templates.
  - A filled `query` template PUT as `export_report.go` created the dir,
    and the read returned it.
  - Check flagged `func Run()` with the want/got message.
  - `script-api` served the JSON.
  - DELETE moved the file into `.trash/` (0600).
- Not done: driving the updated Ctrl+O picker in a browser. Phase 4's
  go-rod steps will cover it.

## Next

Closed: N-131. Declined: None. Raised: N-133.
Deferred: None. Promoted: None.
Updated: N-130. Full list: `ai_docs/todo/next-list.md`.
