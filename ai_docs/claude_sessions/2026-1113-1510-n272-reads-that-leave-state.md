# N-272: a read that leaves state makes the session stateful

Session: `e752c44d-60e7-4faa-926f-41044a5d7a36`

## Ask

N-272 from the next-list (RPU plan, phase 2), then `/sw`. The item:
`Session.stateful` went by the verb, so a "read" that sets state counted as
a plain one — `SELECT set_config(…, false)`, `SELECT pg_advisory_lock()`,
MySQL `SELECT GET_LOCK()` and `@v := …`, SQLite `PRAGMA foreign_keys = ON`
(`pragma` is in `queryVerbs`). After an idle cut `ready()` reports
`ErrBadConn`, `Classify` gives FaultRetry, and the statement reruns on a
fresh session without the state, the case `ErrSessionLost` exists to stop.
Also `SELECT … INTO newtable` (a read, so no recount), and EXPLAIN ANALYZE
of such a "read" possibly running outside the rollback wrapper on
Postgres (suspected). Fix as stated: a SELECT calling a known
state-setting function, and a PRAGMA with `=`, are stateful; always wrap
an analyze on Postgres.

Commit: `18d775e`.

## What was built

**`db/effect.go`** (new) — `effectOf(stmt, dialect)` returns what a
statement may leave behind: `effectNone` (a plain read), `effectSession`
(a setting, a lock, a variable) or `effectAny` (anything: a write, DDL, a
transaction, a SET). The verb still decides most statements: a non-read
verb, or a WITH with a write CTE, is `effectAny`. For a read verb:

- `EXPLAIN` without ANALYZE → none; with ANALYZE → `effectOf(inner)`
  (`explain.Strip` from `Verbs().MainAt`, so a MySQL `#` comment is
  already behind). Added beyond the item: `EXPLAIN ANALYZE DELETE …` typed
  into a Ctrl+R run counted as a read.
- `PRAGMA` → session when it has `=`, or `name(value)` for a pragma not in
  `pragmaReads` (`table_info`, `table_xinfo`, `table_list`, `index_info`,
  `index_xinfo`, `index_list`, `foreign_key_list`, `foreign_key_check`,
  `integrity_check`, `quick_check`). `PRAGMA foreign_keys(1)` is the
  setter's second spelling.
- `SHOW`, `DESCRIBE`, `DESC` → none (`DESCRIBE INSERT INTO …` only
  explains).
- SELECT / VALUES / TABLE: `INTO` in the main statement (past the CTE
  list) → any; a call to a `sessionFuncs` function anywhere in the
  statement → session; on MySQL only, a `:=` → session.

`sessionFuncs`: `set_config`, `setseed`, `pg_advisory_lock[_shared]`,
`pg_try_advisory_lock[_shared]`, `dblink_connect[_u]`, `get_lock`. One set
for every engine; a same-named user function elsewhere only errs toward
stateful. Left out on purpose (the comment says why): the
transaction-scoped advisory locks (a BEGIN already made the session
stateful; outside a transaction they end with the statement), the unlock
functions, and `nextval` (a lost currval fails loudly, and a "session
state" badge after every `SELECT nextval(…)` would cry wolf).

**`sqlsplit/calls.go`** (new) — `Dialect.Calls(sql)`: the lowercased
names of the functions called (a word followed by `(`, blanks and comments
between allowed; a qualified call by its last part; a word after `:`, `@`
or `$` passed over). `Dialect.HasOp(sql, op)`: an operator in code. Both
skip comments, strings, quoted identifiers and dollar bodies the way the
dialect reads them, through a shared `codeEnd`. They are lexical and
over-inclusive: a keyword before a paren (`IN (`) is listed too, which only
matters when the name is looked up.

**`db/manager.go`** — `isRead` is `effectOf == effectNone`.
`IsRead(stmt, dialect)` now takes the connection's dialect; its one caller
is `workspace.analyzeWarning`. The verb alone no longer decides, and the
`:=` rule and a call after a `'a\'` string depend on the engine's reading.

**`db/rowcount.go`** — `ChangesRows` recounts on `effectAny` only (and
still not after `sessionVerbs`): `SELECT … INTO` and an analyzed DELETE
recount, `SELECT set_config(…)` and `pg_advisory_lock` do not.

**`db/explain.go`** —
- Every Postgres analyze runs inside `rolledBack` (BEGIN…ROLLBACK, or the
  savepoint in an open transaction), a plain read's too: a SELECT can write
  through a function it calls, and no reading of the text shows that. The
  rollback note is only for non-reads.
- `rolledBack`'s refusals now say "statement" rather than "write" ("ROLLBACK
  before analyzing a statement").
- After a successful Postgres analyze of an `effectSession` statement the
  session is marked stateful. A session advisory lock outlives the
  rollback. A rolled-back `set_config` errs toward stateful.
- MySQL, SQLite and bytdb refuse to analyze anything but a plain read. For
  `effectSession` the note reads "not analyzed: that would run a statement
  that leaves a lock or a setting on the session for real on mysql …".

## Found on the way

- **The suspected explain hole was real, and wider than the item said.**
  On the old code, analyzing `SELECT dbc_explain.zero()`, a SQL function
  that UPDATEs 100 rows, kept the UPDATE: a read ran bare. Lexical
  detection could never catch this, which is why the fix wraps every
  Postgres analyze rather than widening the classification.
- **A stopped analyze of a read inside the user's transaction aborted
  that transaction** (ErrTxAborted, status `E`, 25P02 after). Running
  inside the savepoint now restores it. This was confirmed as a failing case
  on the old code.
- **An advisory lock survives the analyze's rollback.** `pg_locks` showed
  it granted after `EXPLAIN ANALYZE SELECT pg_advisory_lock(272)`. Hence the
  stateful mark.
- **`ChangesCatalog` misses `SELECT … INTO`.** It goes by the first
  keyword, so the sidebar and completion don't show the new table until a
  refresh. Out of scope here; raised as N-320.
- Not raised: `workspace.analyzeWarning` on Postgres still says "analyzing
  a write: … so no rows change" for a state-setting read. That is true
  about rows, and the advisory-lock case is rare.

## Tests

- `sqlsplit/calls_test.go`: `TestCalls`, `TestHasOp`. The cases cover
  qualified names, comments between name and paren, strings, dollar
  bodies, quoted names, `@`/`:` prefixes, and Postgres vs MySQL reading of
  `'a\'`.
- `db/effect_test.go`: `TestEffectOf` (~45 cases across Postgres, MySQL and
  SQLite: plain reads, session effects, any), `TestSessionStatefulPragma`
  (SQLite: `PRAGMA foreign_keys` and `PRAGMA table_info(t)` stay stateless;
  `PRAGMA foreign_keys = ON` makes the session stateful).
  `TestChangesRows` gained the N-272 cases.
- Live, `workspace/live_test.go` `TestLiveWorkspaceSessionLostToRead`.
  Postgres: `set_config`, `pg_advisory_lock`, `SELECT … INTO TEMP`. MySQL:
  `GET_LOCK`, `@v :=`, `SELECT … INTO @v`. Each runs the read, checks the
  session is stateful, kills the backend, and expects `ErrSessionLost` on
  the next run. **All six fail with effectOf forced to the old verb
  rule** ("stateful false").
- Live, `db/live_explain_test.go`:
  `TestLiveExplainPostgresReadThatWrites` checks that the function's
  UPDATE is not kept, that there is no rollback note, and that a lock
  taken by an analyze leaves the session stateful. A new case "kept, user
  transaction, a read" in `TestLiveExplainPostgresStopWrite`.
  `TestLiveExplainMySQL` checks that `GET_LOCK` is refused and that
  `IS_USED_LOCK` is NULL afterwards. **The first two fail with the old
  `opt.Analyze && !read` condition**: 100 rows kept; ErrTxAborted with
  status `E`.

Runs (postgres:17, mysql:8.4): `go test -count=1 ./...` with both live
DSNs, all 33 packages green; `go test -race ./db ./workspace ./sqlsplit`
green.

Real binary (fresh build, scratch HOME, the Postgres container): a table
of 50 rows and `n272_zero()`, which UPDATEs them all. `dbc explain -a
"SELECT n272_zero()"` was analyzed (1 row, 367 µs), and `count(*) WHERE
total = -1` read 0 afterwards. `dbc explain -a -t json "UPDATE …"` still
carries the rollback note.

## Docs

- README:
  - Editor sessions: the reads that count as leaving state (and so get
    "session lost").
  - Explain: the analyze table's Postgres read cell (inside the same
    rolled-back transaction), plus a paragraph saying that a query taking a
    lock or setting a variable counts as a write there, and that a
    Postgres advisory lock outlives the rollback.
  - Stopping a long query: a stopped read's analyze also leaves the user's
    transaction as it was.
- `ai_docs/todo/next-list.md`: N-272 to Closed; N-320 raised.

## Next

Closed: N-272. Declined: None. Raised: N-320. Deferred: None. Promoted:
None. Moved: None. Updated: None. Full list: `ai_docs/todo/next-list.md`.
