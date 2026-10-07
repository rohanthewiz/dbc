# Scripts: a home of their own, and an editor in the app

Raised 2026-10-06. The ask: dbc.app looks for scripts in `~/scripts`; the
scripts UI needs a revamp; scripts should be creatable and editable from the
app, not only from an outside editor.

This is a plan. **Phases 1–3 are done** (2026-10-06); phases 4–6 are
not started. The decisions table was accepted as recommended.

## The one-paragraph version

Scripts get a fixed, predictable home — `~/.config/dbc/scripts`, beside the
consoles and the history — instead of a directory relative to wherever dbc
happened to start. A relative `scripts_dir` is taken relative to the config
file that sets it, as `tls_*` paths already are. In `dbc web` (and so
dbc.app), a script opens in a **script tab**: Monaco in Go mode, with
completion for the `sdb` API and connection names, a Check that compiles the
script without running it and marks the errors in place, Save, and Run. The
Ctrl+O picker becomes a scripts browser: filter, descriptions, new from a
template, duplicate, rename, delete, and the built-in examples. The TUI gets
the same browser, editing through `$EDITOR`. Headless, `dbc script NAME`
finds a script by name, and `dbc scripts` lists them and says where they
live.

## Why it is broken today

```
config/config.go:267   ScriptsDir: "scripts"        ← default, relative
tui/modals.go:467      filepath.Glob(ScriptsDir/*.go)   ← relative to cwd
web/scripts.go:23      filepath.Glob(ScriptsDir/*.go)   ← relative to cwd
macapp/DbcApp.swift:222  p.currentDirectoryURL = Paths.home
```

`scripts_dir` is used as-is, so a relative value resolves against the
process's working directory. dbc.app starts its helper in `$HOME`, so it
looks in `~/scripts`. A terminal dbc looks in `./scripts` of whatever
directory the shell is in. That happens to be right only in a dbc checkout,
where the repo's samples live in `scripts/`. Everywhere else the directory is
missing, and the only feedback is a log line naming a relative path.

The rest of the UI matches that: a modal of file names (TUI and web alike),
Enter runs, and that is all. You cannot see what a script does before
running it, make one, or change one without leaving dbc and finding the
directory yourself.

## Goals

- **One place for scripts, the same for every way dbc starts**: dbc.app, the
  TUI from any directory, `dbc web`, and headless.
- **Create, edit, check, save and run a script without leaving dbc.app**,
  with an editor that knows Go and the `sdb` API.
- **Errors before a run.** A compile error or a wrong `Run` signature shows
  on its line, not as a stack of `serr` fields in the log after Run.
- **The samples come with the binary**, so a new install has working
  examples to start from without a checkout.
- **Nothing lost.** Saves never silently overwrite a change made elsewhere
  (another window, the TUI, vim), and a delete can be undone.

## Non-goals

- A Go IDE. No gopls, no type checking beyond what yaegi's compile pass
  catches, and no debugger or breakpoints. A script is a page of Go, not a
  project.
- Third-party imports in scripts. yaegi gets stdlib + `sdb`, as today.
- Script parameters prompted at run time (`s.Param("table", …)` → a dialog).
  Worth doing, but it is a change to the script API, not to where scripts
  live or how they are edited. It is under *Later*.
- Heavier auth for `dbc web`. Saving a script and running it is code
  execution as the user. That is no different in kind from what the page can
  already do (run any SQL with the config's credentials), and the same
  boundary covers it: loopback, per-launch secret, SameSite=Strict cookie,
  Host and Origin checks. Nothing is added (see the `dbc web` auth decision
  in `web-ui.md`).

## Decisions to take to the user

The recommendation is first in each row.

| Fork | Recommended | Alternatives |
|---|---|---|
| Default scripts directory | **`~/.config/dbc/scripts`**, beside `consoles/`, `history.jsonl` and `connections.toml` | `~/dbc/scripts` or `~/Documents/dbc` (easier to find in Finder, but splits dbc's state over two trees) |
| A relative `scripts_dir` | **Relative to the config file that sets it** (as `tls_*` already are) | Relative to cwd (today's behavior, which is the bug) |
| Storage | **Plain `.go` files on disk** | bytdb (see *Storage* for why not here) |
| Save model | **Explicit save (Ctrl+S); Run saves first; unsaved changes are kept in the tab** | Autosave as consoles do |
| Where a script is edited in the web | **A script tab: a query tab whose editor is Go** | A separate editor modal (cramped; loses the grid and log beside it) |
| Editing in the TUI | **`$EDITOR` (TUI suspends, resumes, re-checks)** | A Go-mode TUI editor tab (more work; see *Later*) |
| Delete | **Move to `<dir>/.trash/` (listed and restorable)** | Hard delete |
| The repo's samples | **Embedded in the binary as read-only Examples; Duplicate makes an editable copy** | Copied into the scripts dir on first start (clutters the dir and goes stale on upgrade) |

### Storage: why files and not bytdb

bytdb is the default for persistence, but scripts stay files on purpose:

- A script must stay runnable as `dbc script path.go` from cron and the
  shell, and editable in vim, VS Code or anything else. A file is the only
  form every one of those reads.
- Consoles already made this choice for the same reason (`userdata/console.go`).
  Scripts reuse that machinery: revision hashes, conflict on save, and
  validated names that cannot climb out of the directory.
- What is *about* a script, rather than the script itself (last run, last
  status, duration), is history, not a file. Script runs are not recorded
  in `history.jsonl` today (`workspace/run.go` lands a `RunDone.Script` with
  a log note only). Recording them is under *Later*.

## Design

### 1. Where scripts live

```
scripts_dir in config?
 ├─ no ───────────────► ~/.config/dbc/scripts      (no home dir → scripts off, as consoles)
 └─ yes
     ├─ starts with ~/ ► expanded against $HOME
     ├─ absolute ─────► as is
     └─ relative ─────► joined to the config file's directory
```

- One resolver, `config.(*Config).ScriptsPath()` (or resolved once in
  `LoadDemo` next to the `tls_*` paths, which already compute `cfgDir`). Every
  reader uses the result: TUI, web, headless.
- The demo fallback (no config at all) uses the default home.
- **Compatibility.** A checkout with `./dbc.toml` and `scripts_dir = "scripts"`
  resolves to `./scripts` as before, because the config file is in cwd.
  Someone who relied on the cwd default with no config now gets
  `~/.config/dbc/scripts`. The repo's own samples are still offered, as
  Examples (§4).
- The directory is created on the first save, not at startup, so a user who
  never makes a script never gets an empty directory.
- `dbc.example.toml` and the README's config block lose the `scripts_dir =
  "scripts"` line, or show it commented out with the new meaning.

### 2. The script store (`userdata/scripts.go`)

This mirrors `userdata/console.go`. Shared pieces (the rev hash,
write-temp-and-rename) move to a small common helper rather than being
copied.

| Func | Does |
|---|---|
| `ListScripts(dir) ([]ScriptInfo, error)` | `*.go` at the top level, sorted. `ScriptInfo{Name, Desc, Size, Mod, Rev}`. Dot-dirs (`.trash`) skipped |
| `ReadScript(dir, name) (text, rev, error)` | |
| `SaveScript(dir, name, text, base) (rev, conflict, error)` | Writes when the file's rev == base. `base == ""` means create, and refuses when the file exists |
| `RenameScript(dir, from, to)` | Refuses when `to` exists |
| `TrashScript(dir, name)` / `RestoreScript` | `.trash/<name>.<unix>.go`; the trash keeps the last 50 |
| `ValidScriptName(name)` | The console-name rule plus a required `.go` suffix |

**Desc** is the script's first comment line, so a script describes itself and
there is no sidecar file. Comments before `//go:build` count, which is how
every sample already starts:

```go
// Copy myschema.mytable from ProdDr to the dev connection.   ← Desc
//go:build ignore
```

### 3. Checking without running (`script.Check`)

yaegi v0.16.1 (`interp/program.go`) has `Compile(src)` and `CompilePath`,
which parse and compile without executing. `Check(name, src) []Diag` does
three passes:

1. **Parse** with `go/parser`, which gives good positions for syntax errors.
2. **Signature**: from the AST, verify there is a `func Run(s *sdb.S) error`
   in `package main`. This is static on purpose. `Eval("main.Run")` would
   first need `Execute`, which runs package-level initializers and `init()`,
   and a check must never run the script.
3. **Compile** with the same `interp.Options` and `Use(...)` exports as `Run`,
   factored into one `newInterp()` so Check and Run cannot disagree about
   what a script may import. yaegi's errors carry `file:line:col:`, which is
   parsed into a `Diag`.

Plus one **lint**, because it has cost real time: any two-value assignment
whose left side is an index expression (`m[k], _ = …`) is a warning. yaegi
silently drops that write (traefik/yaegi#1655; see the README note). Without
type information it cannot tell a map from a slice. It flags both and says
so, and slices are rare on that side.

```go
type Diag struct {
	Line, Col int
	Severity  string // "error" | "warning"
	Msg       string
}
```

`Run` itself is unchanged apart from sharing `newInterp()`.

### 4. Examples

`scripts/` (the repo's samples) gets an `embed.go` (`package scripts`,
`//go:embed *.go` minus itself). The sample files keep `//go:build ignore`,
so the two packages never meet. Examples are read-only, listed after the
user's scripts, and **Duplicate** copies one into the scripts dir, where it
opens. New-script templates come from the same embed: a blank skeleton, plus
"query & show", "loop over params", "copy a table" and "export a report".
The copy-table template has its `from`/`to` constants filled with the first
two connection names.

### 5. `dbc web` API

The rule in `web/scripts.go` stays: **the page names a script, never a
path.** Every `:name` is checked with `ValidScriptName` and joined to the
resolved dir.

| Route | |
|---|---|
| `GET /api/v1/scripts` | `{dir, scripts: [ScriptInfo], examples: [{name, desc}], trash: n}` (extends today's `{dir, scripts}`) |
| `GET /api/v1/scripts/:name` | `{text, rev}` |
| `PUT /api/v1/scripts/:name` | `{text, base}` → `{rev}`, or `{conflict, text, rev}` (the console protocol) |
| `POST /api/v1/scripts/:name/rename` | `{to}` |
| `DELETE /api/v1/scripts/:name` | to trash; `POST /api/v1/scripts/trash/:id/restore` brings it back |
| `POST /api/v1/scripts/check` | `{name, text}` → `{diags}`. Checks **unsaved** text, so markers follow typing |
| `GET /api/v1/scripts/examples/:name` | `{text}` |
| `GET /api/v1/scripts/api` | the `sdb` surface for completion (§6) |
| `POST /api/v1/ws/:id/script` | unchanged: `{name}` runs the saved file |

Other windows hear about a save, rename or delete over the existing hub
stream (a `scripts` event, as `console` is today), so an open list or tab
refreshes.

### 6. `dbc web` UI

**The scripts browser** (Ctrl+O, ▷ Scripts) replaces the picker:

```
┌ Scripts ─ ~/.config/dbc/scripts ⧉ ──────────────────── [+ New] ┐
│ ⌕ filter                                                         │
│ copy_mytable.go   Copy myschema.mytable from ProdDr to dev   2h  │  ▶ ✎ ⋯
│ nightly_report.go Export yesterday's orders as HTML          3d  │
│ ─ Examples ───────────────────────────────────────────────────── │
│ copy_table.go     Copy a Postgres table from one connection…     │  ⧉ Duplicate
│ loop_params.go    …                                              │
│ ─ Trash (2) ▸ ────────────────────────────────────────────────── │
└ Enter run · E edit · N new · F2 rename · Del trash · Esc close ──┘
```

- Enter runs, as Ctrl+O does today, so existing muscle memory still works.
  E or ✎ opens the script in a script tab. ⋯ has Duplicate, Rename,
  Trash and Copy path.
- The header shows the **resolved absolute dir**, with a copy button. With
  that on screen, this bug could not have hidden.
- An empty dir shows the templates instead of a warning in the log.

**Script tabs.** A tab has a kind: `query` (today) or `script`. A script tab:

```
topbar:  ▶ Run (Ctrl+Enter)  ✓ Check  💾 Save (Ctrl+S)  ■ Stop   copy_mytable.go ●
editor:  Monaco, language "go", the script's text
results: the grid, for s.Show; the log, for s.Print — as script runs already do
```

- The tab is still a workspace (`/api/v1/ws/:id`), so Run, Stop, the run
  slot and the stream are the ones scripts use today. The connection chip is
  hidden, because a script names its own connections.
- **Run** saves first if dirty, then runs the file, so what ran is what is
  on disk, in history, and what `dbc script` would run. A save conflict stops
  the run and shows the conflict.
- **Check** runs on demand and on idle (debounced, ~600 ms after typing),
  posting the unsaved text. Diags become Monaco markers. Run is not blocked
  by warnings, only by nothing: a compile error still surfaces from Run as
  today.
- **Dirty state**: the `●` on the tab and in its title. The unsaved text is
  kept in the browser (localStorage, keyed by script name and base rev) so
  a reload does not lose it. Closing a dirty tab asks first.
- **Conflict**: as consoles. The file's text loads as an undoable edit, and
  Ctrl+Z gets yours back to save over it.
- **Several `s.Show`s.** Today each one replaces the grid (`lastRes`), so a
  script that shows three results leaves only the last. A script tab keeps
  them all: a small "Result 1 · 2 · 3" switcher on the results bar,
  capped at 20 per run.
- **Monaco Go.** `scripts/vendor_monaco.sh` adds `basic-languages/go`
  (tokenizer only, a few KB). It loads only when a Go model is created, so
  query tabs pay nothing.
- **Completion and hover** from `/api/v1/scripts/api`: the methods of
  `*sdb.S`, `sdb.Result` fields, and `CopyOpts`/`WriteOpts` fields, with
  signatures and the doc comments. It is generated once at build time from
  the `sdb` package with `go/doc` and embedded, so it cannot drift from the
  code. Inside the first string argument of `Query`/`Exec`/`Copy`/`Reader`/
  `Writer`/`DB`/`Explain`, completion offers connection names.
- The **Connections** sidebar stays. In a script tab, a click on a
  connection inserts its quoted name at the caret instead of switching.

### 7. TUI

- Same resolver, so the TUI from any directory finds the same scripts.
- Ctrl+O becomes the same browser, drawn with the existing `list` widget.
  It has a filter line, a Desc column, the resolved dir in the title, and
  Examples and Trash sections. Keys match the web: Enter run, `e` edit, `n`
  new (name prompt, then template), `d` duplicate, `r` rename, Del trash.
- **Edit** suspends the TUI with `tea.ExecProcess($VISUAL || $EDITOR ||
  "vi", path)`. On return, the TUI runs `script.Check` and logs any diags as
  `file:line: msg`.
- New-from-template writes the file, then opens it the same way.

### 8. Headless

- `dbc script NAME`: an argument with no `/` is looked up in the scripts dir,
  with `.go` optional (`dbc script copy_mytable`). A path works as today.
- `dbc scripts` lists name, desc and modified time, plus the resolved dir on
  stderr. `-t json` is for tooling.
- `dbc script --check NAME|PATH` prints diags as `file:line:col: msg` and
  exits 1 on an error. It is useful in CI for a scripts repo, and it is the
  same `script.Check`.

## Phases

Each phase ships alone and leaves main releasable.

1. **Where scripts live.** The resolver, its use in TUI/web/headless, the
   dir in the picker title and in the empty-dir message, `dbc script NAME`,
   `dbc scripts`, and README + example config. *This alone fixes the
   dbc.app bug.* Tests: resolver table (unset, `~`, absolute, relative with
   `./dbc.toml`, relative with `~/.config/dbc/config.toml`, demo fallback,
   no home).

   **Outcome (2026-10-06).** `config/scripts.go` (`ResolveScriptsDir`,
   `FindScript`, `TildePath`, the legacy `./scripts` warning), resolved once
   in `LoadDemo` so `Config.ScriptsDir` is always absolute.
   `userdata/scripts.go` has `ListScripts` and `ScriptDesc`, pulled forward
   from phase 2 because `dbc scripts` shows descriptions. The TUI and web
   pickers name the directory in their titles, and `scriptscmd.go` adds
   `dbc scripts`. Also `cats-plugin.toml` completions. With no home
   directory, the default falls back to the old cwd-relative `scripts`
   rather than turning scripts off.
2. **Store, check, examples.** `userdata/scripts.go`, the shared rev helper
   factored out of consoles, `script.Check` with `newInterp()`, the embedded
   examples and templates, and `dbc script --check`. Tests: name validation
   (traversal, dotfiles, missing `.go`), save conflict / create-refuses-
   existing, trash and restore, Check on a syntax error, missing `Run`,
   wrong signature, compile error with position, and the map-comma-ok lint.
   Check must never execute: a script whose `init()` writes a file is
   checked and the file must not appear.

   **Outcome (2026-10-06).**
   - Store (`userdata/scripts.go`): `ValidScriptName`, `ReadScript`,
     `SaveScript` (rev check; `base ""` creates and returns
     `ErrScriptExists` over a file; a deleted file is a conflict with rev
     ""), `RenameScript` (a case-only rename is allowed), `TrashScript`,
     `ListTrash` and `RestoreScript(dir, id, to)`.
     - `to` restores under another name when the old one is taken.
     - Trash IDs are `<stem>.<unix ms>.go`, with the newest 50 kept.
     - Saves are atomic, keep an existing file's mode, and write through
       a symlink.
     - `ScriptInfo` gained `Rev`. `ListScripts` skips a Go file of another
       package (the checkout's `scripts/embed.go`).
   - Shared helpers: `userdata/files.go` has `textRev` (`ConsoleRev` now
     calls it) and `writeAtomic` (schema picks use it).
   - `script/check.go`: `Check(name, src) []Diag`, sharing `newInterp()`
     with `Run`. yaegi's `Compile` reports `line:col: msg`, stops at its
     first error, and runs neither `init()` nor var initializers.
     - One Check costs about 2 ms, so there is no caching.
     - An import a script cannot have gets a plain message (no GOPATH
       advice).
     - Unused variables and imports pass. yaegi is not gc.
   - `scripts/embed.go` (package `scripts`) embeds the samples. They are
     named in the directive, because `*.go` would also embed the package's
     tests; `TestSamplesAllEmbedded` guards the list.
     - Templates are in `scripts/templates/`: blank, query, loop, copy,
       export. `"{{conn}}"`/`"{{conn2}}"` are filled with `strconv.Quote`d
       names by `Fill`.
     - Every example and filled template passes Check. blank, query, loop
       and export run on SQLite (`script/templates_test.go`).
   - The legacy `./scripts` warning ignores exact copies of the samples, so
     a checkout no longer warns.
   - `dbc script --check NAME|PATH…` (`scriptcheck.go`): compiler lines on
     stdout, `-t json` for one array, exit 1 on an error. Warnings alone
     exit 0.
3. **Web API.** The routes in §5, the `scripts` hub event, and the `sdb` API
   JSON generator. Tests in `web/` beside `consoles_test.go`, including a
   `:name` of `../x.go` and of an example name.

   **Outcome (2026-10-06).**
   - Routes (`web/scripts.go`). The ones that are not about one script moved
     off `/api/v1/scripts/` to siblings: `script-check`, `script-api`,
     `script-examples/:name`, `script-templates/:name` and
     `script-trash/:id/restore`. rweb's radix router does not backtrack.
     With a literal like `examples/` beside `:name`, a script whose name
     shares its first letters (`export_report.go`, `trash.go`) 404'd.
     `TestScriptNamesBesideRouteWords` holds the line.
   - `GET /api/v1/scripts` now returns `{dir, short, scripts: [ScriptInfo],
     examples, templates, trash: [TrashInfo]}`, so trash is the list, not a
     count. The page's Ctrl+O picker reads `.name` and shows the desc.
   - The save is the console protocol: `PUT {text, base}` returns `{rev}` or
     `{conflict, text, rev}`, and a deleted file is a conflict with rev "".
     A create (`base ""`) over a taken name is a 409. The size cap is
     `userdata.MaxScriptBytes`, now exported.
   - `DELETE` returns the trash `{id}`, so the page can offer Undo. Restore
     takes an optional `{to}`, and a taken name is a 409.
   - Beyond the plan, `GET /api/v1/script-templates/:name?conn=` returns a
     filled template. The connection order is the tab's connection, then
     the default, then the rest. New and Duplicate are the page PUTting text
     with `base ""`; there is no server-side "create from" route.
   - Every save, rename, trash or restore broadcasts a window-level
     `scripts` event: `{op, name, to?, rev?, id?, win}`. It carries no text,
     because a window with the script open fetches it.
   - `sdb/sdbapi` builds the `sdb` API from source with `go/doc`: S's
     methods (less `New`, `WithContext` and `Release`), package funcs, and
     the aliased types' fields and methods, one level deep. Each func has
     its params and its `connArgs` (string params named conn, src or dst).
     It is embedded as `api.json`, and `go generate ./sdb/sdbapi` rebuilds
     it. `TestAPIUpToDate` fails on drift.
4. **Web UI.** The browser, script tabs, Monaco Go, markers, completion,
   the multi-result switcher, and dirty/conflict handling. Checked with the
   go-rod e2e (`web/e2e`, `DBC_E2E=1`): new from template → edit → see a
   marker → fix → save → run → result in the grid → rename → trash →
   restore.
5. **TUI.** The browser and `$EDITOR`. Checked with the TUI e2e harness
   (`tui/e2e`), with `EDITOR` set to a script that edits the file.
6. **Docs and the skill.** The README scripting section rewritten around the
   browser, and `.claude/skills/dbc` (`SKILL.md`, `scripting.md`) updated for
   the new dir, `dbc scripts`, and `--check`.

## Risks

- **yaegi compile errors are not always well-positioned.** Some come back
  without a line, or with the line of a different statement. Mitigation: the
  `go/parser` pass catches syntax errors first with exact positions; a yaegi
  error without a position becomes a diag on line 1 with the message intact.
- **Compile cost on idle checks.** A fresh interpreter plus stdlib symbols
  per check. Measure it in phase 2. If it is over ~100 ms, cache the
  configured interpreter's symbol load and debounce harder; parse-only
  checks stay instant.
- **Changing the default dir moves people's scripts.** Only someone with no
  config who kept scripts in `./scripts` of some directory is affected. The
  dir is shown everywhere and `dbc scripts` prints it; a one-time log line
  when `./scripts/*.go` exists in cwd but is not the resolved dir says
  where scripts are now read from.

## Later

- Script parameters: `s.Param(name, default)`, with a run dialog in the web
  and TUI, and `--param k=v` headless.
- Sub-folders in the scripts dir, shown as groups.
- A Go-mode editor tab in the TUI (the existing editor widget, with a Go
  tokenizer and `script.Check` markers in the gutter).
- Scheduling a script from the app (cron-like, while dbc web runs).
- Record script runs in `history.jsonl` (name, status, duration), and show
  each script's last run in the browser.
