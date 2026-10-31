# sqlsplit reads strings as the engine does (N-238)

Session: `0aceab31-c462-4f8f-bffa-6707e9771eb4`

## Ask

Next-list item N-238, pasted from the cats-todo backlog. sqlsplit's
scanner honored `\'` in every dialect (`skipQuoted`, MySQL's rule). So on
Postgres, SQLite and bytdb any string literal ending in a backslash ran on
past its quote. Split then merged that statement with the next, and the
editor colored what followed as a string. The designer still writes such
literals: an equality value ending in `\` (`x = 'a\'`), and on SQLite an
ends-with value ending in one. Typed SQL can hold one anywhere. The fix
asked for was a dialect-aware scan, with the driver passed to Split and
Lex by their callers.

Then `/sw`.

## What changed

- **`sqlsplit/dialect.go`** (new): `Dialect` and `DialectOf(driver)`.

  | dialect      | a `\` escapes in                       |
  | ------------ | -------------------------------------- |
  | Generic (0)  | every `'…'` — the old rule             |
  | Postgres     | an `E'…'` only (E standing as a token) |
  | bytdb        | as Postgres                            |
  | MySQL        | every `'…'`                            |
  | SQLite       | nowhere                                |

  `DialectOf` reads config names, the canonical driver names and code
  fence tags (`psql`, `plpgsql` …); anything else is Generic.
  `skipQuoted` takes the dialect. `Split`, `Lex`, `HasKeyword`, `Verbs`,
  `Folds`, `Format` and `FormatSpan` are now its methods. The
  package-level functions stay as Generic's, so a caller with no engine to
  name reads SQL exactly as before.
- **Why Generic is the old rule, not the strict one:** on the other
  engine's SQL it misreads only a string ending in a backslash. The
  strict rule would misread every escaped quote in a MySQL string
  (`'it\'s'`), which is far commoner.
- **Callers pass their engine:**
  - `workspace`: `Pick`, `PickAll` and `StmtRange` take a dialect, and
    `Workspace.Dialect()` is the active connection's. `RunEditor`,
    `ExplainEditor` and `ChatContext` use it.
  - TUI: the marker, run menu and Run (`tui/run.go`); the editor's
    coloring (`editor.setDialect`, called per frame, which drops the
    highlight cached against the edit version); ⌥⇧F; F12/F2; the chat's
    code blocks and the routine viewer.
  - dbc web: `/stmt`, `/folds`, `/format` and `/picks` read a `driver`
    field the page now sends (`dbc.state.driver`). Run, design import
    and symbol lookup use the tab's workspace.
  - Headless: `-f` and `explain` (`connDialect`); `runs --sql` (bytdb).
    `explain` now loads the config before counting statements.
  - migrate: `migrate.Dialect` carries its scan, and `Load`/`ParseFile`
    take one. `openMigrator` resolves the dialect before reading files
    (a config lookup, not a connect).
  - Pipelines: `sql.exec` (the conn's `etl.Engine`, through `ETLConn`)
    and `sql.write`'s setup (the sink's engine). `etl.Engine.Dialect()`
    is new.
  - `sdb` DDL logging, `etl.Copy`'s Query check, `designer.Import`, and
    `sqlcomplete` (Complete, Rename; `Resolve` now takes a dialect).
  - codehl: `LexDialect(tag, src, conn)` reads a SQL block as the engine
    its fence tag names, else as the connection's. `Lex` is the Generic
    form. `hl.js` mirrors it: `dbc.hl(tag, code, driver)`, and chat,
    routine DDL and the designer's SQL pass the driver.
- **Found beyond the item:** db's RETURNING check used the same scanner.
  On Postgres and bytdb, `INSERT … VALUES ('a\') RETURNING id` ran as an
  Exec and dropped its rows. `isQuery` and `isRead` now take the
  connection's dialect in `run`, `Session.Run` and explain. `hasParams`
  reads as Postgres, its only caller's engine.
- **Left on Generic, on purpose:** `txControl`, `db.IsRead` and
  `ChangesRows`, db's and explain's `sqlWords`,
  `explain.skipLeadingComments`, and sqlcomplete's `quotedName`. Each
  reads keywords or comments ahead of any string, or is best-effort
  (table mentions, alias mapping, a row-count hint).

## Tests

- **`sqlsplit/dialect_test.go`** (new) covers, per engine: split, Lex
  agreeing with Split, RETURNING and Verbs, folds and format. It also
  covers `E'…'` and `e'…'`, a word ending in e (`some'a\'`), SQLite
  having no E strings, backslashes in quoted names, `DialectOf`, and the
  package-level functions staying Generic.
- **`TestPickDialect`, `TestRunEditorDialect`** (`workspace`): on real
  SQLite, the caret after `'a\'` runs the second statement alone, and
  Run all runs both.
- **`TestBackslashValuesSplit`** (designer): the item's own literals.
  `= 'a\'` per engine (`'a\\'` on MySQL), and SQLite's ends-with with and
  without `ESCAPE '!'`, each split from a following statement. Import on
  Postgres reports two statements at line 2. `TestLikeEscapeScans` now
  splits by the engine's dialect and by Generic.
- **`TestSplitsAsTheEngine`** (migrate): a migration seeding `'C:\'`,
  then a CREATE, applies on SQLite and bytdb.
- **`TestBytdbReturning`**: the backslash value comes back as a row.
  Mutation check: with `run` reverted to Generic, it fails (`exec=true`).
- **`TestCompleteAfterBackslashString`**, **`TestHighlightFollowsDialect`**
  (TUI editor) and **`TestLexDialect`** (codehl).
- **Web e2e `backslashSplit`** (new step). On SQLite the marker sits on
  line 2, the menu says "Run statement 2/2", and Run runs it. With the
  driver dropped from `/stmt`'s request, it times out waiting for the
  marker. The whole web suite and the TUI e2e suite pass.
- **`hl.js`** checked under node with a DOM stub: string spans per
  driver and fence tag.
- **Real binary**, fresh build against the demos. `-f` of
  `SELECT 'a\' AS v; SELECT 2 AS n` now gives both results on SQLite and
  bytdb. HEAD's binary returned only `n` on SQLite (one merged
  statement, the driver keeping the last result) and a syntax error on
  bytdb. `explain` of it now says it holds 2 statements; before, it gave
  a SQLite plan-parse error.
- `go build ./... && go vet ./... && go test ./...` pass, with `CATS_*`
  stripped.

## Left open

MySQL's `"…"` is a string under the default `sql_mode`, with backslash
escapes. The scanner reads every `"…"` as a quoted name, so on MySQL
`SELECT "a\"; b"` splits inside the string. Fixing it needs to know
whether `ANSI_QUOTES` is on, which the text cannot say. Raised as N-241.

`sqlsplit/folds.go` is not gofmt-clean, as it already wasn't at HEAD.
gofmt would rewrite its doc comment's diagram, so it was left alone.

## Commits

- The work, this doc and the next-list edits, together.

## Next

Closed: N-238. Declined: None. Raised: N-241.
Deferred: None. Promoted: None. Moved: None. Updated: None.
Full list: `ai_docs/todo/next-list.md`.
