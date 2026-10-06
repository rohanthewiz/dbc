# Scripts get a home: scripts_dir resolution, `dbc scripts`, and the revamp plan

Session: `514b47d3-3bcb-4667-a477-9abe1abf255f`

## Ask

1. Set up a script to copy `myschema.mytable` from the `ProdDr` connection to
   the same table on "Dev".
2. "Apparently the app is looking for scripts at `~/scripts`. It looks like
   the whole scripts UI needs a revamp. And I should be able to edit / create
   scripts from the app." Plan it under `ai_docs/plans/`.
3. "Go with your recommendations, start phase 1."

## The copy script

- Wrote `~/.config/dbc/scripts/copy_mytable.go` (outside the repo: it names
  production connections). `s.Copy(from, to, table, CopyOpts{Create: true,
  Truncate: replace, ProgressEvery: 100_000})`: Postgres to Postgres with no
  Transform, so it streams COPY to COPY in one destination transaction. It
  counts rows on prod first, which also checks the table exists, and counts
  dev afterwards, noting a mismatch.
- There is no connection named `Dev`. The connections are `ProdDr`, `Pilot`
  and `edp-dev-allia`, so `to` is set to `edp-dev-allia` and the user was
  asked to confirm it. Not run against either real connection (it writes to
  dev).
- Test-ran a copy with the names swapped for the demos. bytdb → sqlite
  passed twice (the re-run replaced rows, no duplicates). sqlite → bytdb
  failed on the `adopted` bool column: raised as N-131.
- The one-off alternative given:
  `dbc copy --from ProdDr --to edp-dev-allia --create --truncate myschema.mytable`.
- `~/.config/dbc/connections.toml` holds DSN passwords in plain text. The
  user was advised to switch to `${VAR}` references, which the file
  supports.

## Diagnosis: why dbc.app looked in ~/scripts

- `config.LoadDemo` defaulted `ScriptsDir` to `"scripts"`, and the TUI
  (`tui/modals.go` `openScripts`) and the web (`web/scripts.go`) globbed it
  as written, so it was relative to the process's cwd.
- `macapp/DbcApp.swift:222` starts the helper with
  `currentDirectoryURL = Paths.home`, which gives `~/scripts`. A terminal
  dbc used `./scripts` of whatever directory it was in, which was right
  only in a checkout.

## The plan: `ai_docs/plans/scripts-revamp.md`

Recommendations, all accepted:

- Default dir `~/.config/dbc/scripts`; a relative `scripts_dir` is relative
  to the config file (as `tls_*` paths already are).
- Plain `.go` files, not bytdb, because a script must stay runnable by
  `dbc script` from cron and editable in any editor. They reuse the
  consoles' revision/conflict machinery.
- Explicit save, and Run saves first (autosave could hand a cron job a
  half-edited file).
- Script tabs in dbc web: Monaco Go, `script.Check` markers (yaegi
  `CompilePath` without `Execute`, a static `Run` signature check, and a
  `m[k], _ =` lint), `sdb` API and connection-name completion.
- The TUI edits through `$EDITOR`. Delete moves to `.trash/`. The repo
  samples are embedded as read-only Examples.
- Six phases. Corrected mid-session: script runs are *not* recorded in
  `history.jsonl` today (`workspace/run.go` gives a script's `RunDone` only
  a log note), so that went to Later.

## Phase 1, done

- `config/scripts.go`:
  - `ResolveScriptsDir(raw, base)`: blank means `DefaultScriptsDir()`
    (`~/.config/dbc/scripts`, or the legacy `scripts` with no home dir).
    `${VAR}`s are expanded (an unset one warns), `~` against `$HOME`, a
    relative path joined to the config's dir, then cleaned and made
    absolute.
  - `(*Config).FindScript(arg)`: an existing file wins (old behavior).
    Otherwise a bare name is looked up as `ScriptsDir/arg` or
    `ScriptsDir/arg.go`, and the error names the dir.
  - `legacyScriptsWarning`: warns only when `./scripts/*.go` exists, is not
    the resolved dir, and the resolved dir has no scripts.
  - `TildePath` for display.
- `config/config.go`: there is no `"scripts"` default any more.
  `resolveScripts(cfgDir)` runs in `LoadDemo` for the demo fallback (base
  "") and for a config file (its dir), so `Config.ScriptsDir` is always
  absolute after load.
- `userdata/scripts.go` (pulled forward from phase 2): `ListScripts(dir)`
  (top-level `*.go`, no dotfiles or dirs, a missing dir is none) and
  `ScriptDesc` (the first comment before `package`, from a
  `PackageClauseOnly` parse, cut at the first ". " and at 100 runes).
- TUI: `openScripts` uses `ListScripts`. The title is
  `Scripts in ~/.config/dbc/scripts · Enter or click runs`, and the modal
  widens to fit it.
- Web: `/api/v1/scripts` adds `short` (the `~` form) and fails on an
  unreadable dir. The `app.js` picker title names the dir.
- CLI:
  - `dbc script <file.go|NAME>` goes through `FindScript`.
  - New `scriptscmd.go` adds `dbc scripts`: a `model.Result` (name,
    modified, size, description; typed Raw for JSON), so every `-t` and
    `-o` works. The dir goes to stderr. Only the config is loaded, no
    `db.Manager`.
  - `cats-plugin.toml` completions gain `scripts` (enforced by
    `TestCatsManifest_CompletionsCoverCLI`).
- Docs: README (config block, Scripting in Go, headless examples, Scripts
  headless), `dbc.example.toml` (`scripts_dir` commented out with the new
  meaning), the `.claude/skills/dbc` SKILL.md and scripting.md, and the
  plan's Outcome note.
- Also committed: the user's `.gitignore` change (`.ced/`).

## Verification

- `go build ./... && go vet ./... && go test ./...`: all pass.
- New tests: `TestResolveScriptsDir`, `TestLoadScriptsDir`,
  `TestFindScript`, `TestLegacyScriptsWarning`, `TestTildePath` (config);
  `TestListScripts`, `TestScriptDesc` (userdata); `TestScriptsListJSON`
  (root).
- Fresh binary, isolated HOME: `dbc scripts`, `dbc script loop_params` by
  name, an unknown name (exit 2, names the dir), `-t json`, and the legacy
  warning. `dbc web` (isolated, Bearer) `/api/v1/scripts` returns `dir`,
  `short` and the list. `dbc scripts` run from `~` against the real config
  lists `copy_mytable.go` from `~/.config/dbc/scripts` (read-only, no
  connection).
- Not run in a browser: the web picker's title change.

## Next

Closed: None. Declined: None. Raised: N-130, N-131.
Deferred: None. Promoted: None.
Updated: None. Full list: `ai_docs/todo/next-list.md`.
