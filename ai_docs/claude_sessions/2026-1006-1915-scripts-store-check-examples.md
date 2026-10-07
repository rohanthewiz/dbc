# Scripts revamp phase 2: the store, `script.Check`, built-in examples, `dbc script --check`

Session: `4aaff8bf-1cd2-4789-bed4-ea31136e7a2b`

## Ask

`/sess-load`, then "start phase 2", meaning phase 2 of
`ai_docs/plans/scripts-revamp.md`: the store, the check, the examples.

## The store (`userdata/scripts.go`)

- `ValidScriptName`: the console-name rule for the stem, plus a required
  `.go`. It refuses `.go`, `.hidden.go`, `a..go`, `../x.go`, `sub/x.go` and
  stems over 64 runes. Every function takes a dir and a NAME, never a path.
- `ReadScript(dir, name) (text, rev, err)`. A missing file `errors.Is`
  `fs.ErrNotExist`.
- `SaveScript(dir, name, text, base) (rev, conflict, err)`:

  | base | file now | result |
  | --- | --- | --- |
  | `""` | missing | created, along with the dir |
  | `""` | exists | `ErrScriptExists` |
  | the file's rev | matches | written |
  | a rev | changed or deleted | conflict, with the current rev (`""` if gone) |

  - The write is atomic: a dot-named temp file renamed over the target.
  - An existing file keeps its mode, and a new one is 0600.
  - A symlinked script is written through to its target.
  - The cap is 1 MiB.
  - A process-wide mutex covers read, compare and write.
- `RenameScript`: refuses to replace another script, but allows a
  case-only rename on case-insensitive filesystems (`os.SameFile`).
- `TrashScript` / `ListTrash` / `RestoreScript(dir, id, to)`:
  - Trash IDs are `.trash/<stem>.<unix ms>.go`. On a collision the ms
    steps forward, and the newest 50 are kept.
  - `to == ""` restores under the old name, and `ErrScriptExists` is
    returned if that name is taken. This is a small extension of the plan,
    which had no `to`.
- `ListScripts`:
  - `ScriptInfo` gained `Rev`.
  - It skips a Go file whose header parses as a package other than `main`,
    so the checkout's own `scripts/embed.go` is not listed when
    `scripts_dir` is `./scripts`.
  - A header that does not parse still lists, since it is a script being
    written.
- `DescOf(src)`: the description from source, for embedded files.
  `ScriptDesc(path)` wraps it.
- `userdata/files.go` holds the shared helpers. `textRev` replaces the body
  of `ConsoleRev`, and `writeAtomic` replaces `writePicks`' own temp-file
  dance.

## `script.Check` (`script/check.go`)

- `newInterp()` was factored out of `Run` (`engine.go`), so Check and Run
  load the same stdlib and `sdb` exports.
- Four passes:
  1. `go/parser`. A syntax error returns only those diags.
  2. The signature, checked on the AST:
     - The file must be `package main`.
     - It needs a top-level `func Run(<x> *sdb.S) error`, where `sdb` may
       be renamed or dot-imported.
     - Missing → a diag on the package clause. Wrong → a diag on `Run`'s
       name, showing want and got (printed with `go/printer`).
  3. A lint: a two-value `=` whose left side has an `IndexExpr` is a
     warning (yaegi#1655). Without types, slices are flagged too, and the
     message says so.
  4. `newInterp().Compile(src)`, with no Execute.
- Probed facts about yaegi v0.16.1:
  - Errors look like `L:C: msg`, with no file prefix, and only the first
    error is reported.
  - `Compile` runs neither `init()` nor package-level `var` initializers.
  - An unused variable or import passes.
  - An unknown import's GOPATH advice is rewritten to "a script can import
    the standard library and …/sdb".
  - A panic inside yaegi becomes a diag.
  - A `nil` program (a legacy `// +build` line excluding the file) is an
    error diag.
- Cost: about 1.8 ms per Check, including a fresh interpreter, so no
  caching.
- `Diag{Line, Col, Severity, Msg}` and `HasError`. `String()` gives
  `L:C: [warning: ]msg`.

## Examples and templates (`scripts/`)

- `scripts/embed.go` is `package scripts`.
  - `//go:embed` names the five samples explicitly. `*.go` would also have
    embedded `embed_test.go` into the binary, which the first test run
    exposed. `TestSamplesAllEmbedded` fails when a `package main` file on
    disk is missing from the directive.
  - `Examples()` / `ExampleByName`. The desc drops the shared "Sample dbc
    script: " lead and is re-capitalised.
  - `IsExample(src)`: byte-equal to a sample after CRLF normalising.
- `scripts/templates/`: blank, query, loop, copy and export.
  - They say `"{{conn}}"`/`"{{conn2}}"` inside string literals, so the
    unfilled files are still valid Go.
  - `Fill(name, conns)` substitutes `strconv.Quote`d names, or the
    placeholders `my-connection`/`other-connection` when there are too few.
  - `Templates()` reads a table (name, title) plus each file's DescOf, and
    a test keeps the table and the dir in step.
  - The copy template's constants have their comments above them, so
    filling doesn't misalign gofmt columns.
- The scripts tests are an external package (`scripts_test`) with an
  `export_test.go`. An in-package test importing `script` was an import
  cycle (script → sdb → db → config → scripts), because config now imports
  scripts.
- Every example passes Check with no errors. Every filled template is
  fully clean. `script/templates_test.go` runs blank, query, loop and
  export on SQLite (export in a `t.Chdir` temp dir).

## Legacy `./scripts` warning

`config.legacyScriptsWarning` now counts only own scripts: `ListScripts` of
`./scripts`, minus `IsExample` copies. A checkout with no `dbc.toml` and an
empty `~/.config/dbc/scripts` no longer warns. One edited sample still
does. Test: `TestLegacyScriptsWarningSkipsSamples`.

## `dbc script --check` (`scriptcheck.go`)

- It takes one or more names or paths, all resolved with `FindScript`
  before any is checked. Only the config is loaded (`LoadDemo`); nothing
  connects.
- Output:
  - Compiler lines `path:L:C: msg` on stdout, and nothing when clean.
  - `-t json` prints one array of `{file,line,col,severity,msg}` (`[]`
    when clean).
- Exit codes: 1 on any error, 0 with only warnings, 2 for an unknown name.
  `-o`, `-c` and other `-t` values are refused (exit 2), as is the
  `--file/--tx/-k` family.
- `cats-plugin.toml` completions gain `--check`, which
  `TestCatsManifest_CompletionsCoverCLI` enforces.

## Docs

- README: a `--check` paragraph and the built-in examples under Scripting
  in Go, headless examples, and the Scripts headless rules.
- `.claude/skills/dbc`:
  - The SKILL.md command table.
  - scripting.md: `--check` lines, the lint noted on the yaegi pitfall,
    and a "compile pass is not gc" pitfall.
- The plan's phase 2 Outcome, with the header now "Phases 1 and 2 are
  done".
- `main.go`'s header usage line.

## Verification

- `go build ./... && go vet ./... && go test ./...`: all pass.
- New tests:
  - userdata: `TestListScriptsSkipsOtherPackages`, `TestValidScriptName`,
    `TestSaveScript`, `TestSaveScriptThroughSymlink`, `TestRenameScript`,
    `TestTrashAndRestore`, `TestTrashKeepsTheNewest`.
  - script: `TestCheck` (16 cases), `TestCheckNeverExecutes`,
    `TestCheckIsQuick`, `TestYaegiDiag`, `TestTemplatesRun`.
  - scripts: `TestSamplesAllEmbedded`, `TestExamples`, `TestIsExample`,
    `TestTemplates`, `TestFill`.
  - config: `TestLegacyScriptsWarningSkipsSamples`.
  - root: `TestCheckScripts`.
- The real binary with an isolated HOME and CATS_* unset:
  - `--check bad` by name gave a warning line plus an error line, exit 1.
  - `--check` over the checkout's `scripts/*.go`: the samples are clean.
    `embed.go` and `embed_test.go` are reported, correctly, since they
    are not scripts.
  - `-t json`, an unknown name (exit 2) and `-o` refused (exit 2) all
    behaved as expected.
  - `dbc scripts` run in the checkout with an empty dir no longer warns.
  - An unused variable and an unused import both pass `--check`, which
    the skill doc now says.

## Next

Closed: None. Declined: None. Raised: N-132.
Deferred: None. Promoted: None.
Updated: N-130. Full list: `ai_docs/todo/next-list.md`.
