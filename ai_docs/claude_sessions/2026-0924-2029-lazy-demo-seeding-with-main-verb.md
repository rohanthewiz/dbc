# Lazy demo seeding, and WITH writes judged by their main verb

Session: fb3470ca-a440-4a75-8542-06ec4eb41cf2
Date: 2026-09-24

## Ask

Two next-list items, each pasted as-is:

- **N-010**: "With no config, every headless invocation seeds both demos
  (`db.SeedDemos` in `setup`, `main.go:288`), so a bare `dbc "SELECT 1"`
  opens the bytdb file. Lazy seeding on first use of a connection is the fix
  if it ever matters."
- **N-039**: "A `WITH … INSERT/UPDATE/DELETE` without `RETURNING` runs as a
  Query, since `with` is on the read-verb list (`queryVerbs`,
  `db/manager.go`): it shows an empty result instead of rows affected, and a
  `Session` does not mark itself stateful after it (`isRead`). Telling it
  apart means finding the verb after the CTE list, not just the first
  keyword."

## N-010: lazy demo seeding

### Found on the way: seeding deleted rows from the user's database

With no config, `addAdHocConn` appends the `--dsn` connection to
`cfg.Connections` before `setup` calls `SeedDemos`, and `SeedDemos` looped
over every connection. So `dbc --driver … --dsn … "SELECT 1"` ran
`CREATE TABLE IF NOT EXISTS cats` and `DELETE FROM cats` + the demo inserts
on the user's own database. The TUI with `--dsn` did the same.

Reproduced before fixing: a scratch SQLite file holding a `cats` table with
one row (`99|MyRealCat`) came out holding the eight demo cats. After the fix
the row survives.

### Decisions

- **Demos are marked per connection**: `config.Connection.Demo`
  (`toml:"-"`), set only by `demoFallback`. It can't be read from a file, so
  it can't be switched on for a real database. It's per connection, not
  `Config.Demo`, because a demo config can also carry the `--dsn`
  connection.
- **Seeding happens in `Manager.open`**, after the ping and the SQLite
  anchor, before the pool is cached. Callers waiting on the same open (the
  `opening` dedup) therefore see a seeded database. `open` runs once per name
  per Manager, so a demo is never reseeded under the user's edits
  mid-session. It runs under the caller's ctx, not the connect-timeout ctx,
  since `connect_timeout` bounds dialing. A seed failure closes the pool and
  anchor and fails the open.
- **`setup(demoOpen)`** says how much to open up front:
  - `demoAll`, for the TUI: `SeedDemos` opens every demo, prunes an unusable
    one and moves the default, as before. The TUI lists every connection, so
    it needs to know which ones work.
  - `demoForRun`, for a headless query or `migrate`: only when there's no
    `-c`/`--dsn`, `OpenDefaultDemo` opens the demos in config order (active
    first) and stops at the first that opens. The fallback survives, e.g.
    the bytdb file held by a running TUI hands over to the SQLite demo with a
    warning, but the other demo isn't touched when the active one works. A
    run that names a connection opens just that one, lazily. Falling back
    after an explicit `-c` was not wanted: it would run on a database the user
    didn't ask for.
  - `demoLazy`, for `dbc script`: scripts name their own connections, so
    nothing is opened up front.
- **`SeedDemos` and `OpenDefaultDemo` share `openDemos(firstOnly)`**, which
  opens only `Demo` connections and keeps everything else untouched. It fails
  only when no demo opened *and* nothing else is left (so a TUI on `--dsn`
  with both demos broken still starts).
- **`SeedDemo` stays an unconditional seed** of whatever it's given. The
  tests use it on their own private databases, and
  `TestSeedDemoIsRepeatable` relies on it seeding each time. On a flagged
  demo it just runs the idempotent script a second time.
- The setup failure message is now "could not open the demo connection"
  (was "could not seed demo data"); nothing matched on the old text.

### Behavior now

- `dbc "SELECT …"` (default bytdb demo) opens and seeds bytdb only.
- `dbc --demo sqlite "…"` and `dbc -c demo "…"` never create or open
  `demo.bytdb`.
- `dbc script testdata/show_two.go` seeds `demo` on first use, and bytdb is
  not touched.
- The premise "a bare `dbc "SELECT 1"` opens the bytdb file" still holds,
  because bytdb is the default demo. What changed is that the demo not in use
  is left alone.

### Tests (`db/demo_test.go`)

- `demoCfg` marks both connections `Demo` and gives the SQLite demo a
  per-test in-memory name (`memName`), since a shared name could leak rows
  between tests while an earlier Manager is open. `opened` peeks at
  `m.conns`.
- `TestDemoSeedsOnFirstUse`: querying the SQLite demo with nothing called up
  front returns the 8 cats, and the bytdb file doesn't exist.
- `TestDemoSeedsOncePerOpen`: an insert after first use survives the next
  statement.
- `TestDemoSeedingLeavesUserConnectionAlone`: the `--dsn` connection isn't
  opened by `SeedDemos` and its `cats` row survives.
- `TestOpenDefaultDemoOpensOnlyTheActiveDemo` and
  `TestOpenDefaultDemoFallsBack`.
- Verified end to end with the built binary under a scratch `HOME` (CATS_*
  stripped): the `--dsn` repro, `--demo sqlite`, `-c demo`, the default run
  and `-c demo-bytdb`.

## N-039: WITH writes

### Decisions

- **`sqlsplit.Verbs(sql) StmtVerbs`** returns `Main` (the verb after the
  CTE list for a WITH, else the leading keyword), `MainAt` (its byte offset)
  and `CTEs` (each CTE body's leading keyword). It's lexical, like the rest
  of the package. It walks a WITH at paren depth 0 using the package scanner
  (strings, quoted identifiers, comments and dollar bodies are skipped). The
  first depth-0 word in `withMainVerbs` (select, insert, update, delete,
  merge, values, table) is the main statement. None of those is a word the
  CTE list uses (`AS`, `RECURSIVE`, `[NOT] MATERIALIZED`, Postgres
  `SEARCH … SET` / `CYCLE … USING`), and as reserved words none can be an
  unquoted name.
  - A `(` at depth 0 after `as`/`materialized` is a CTE body; its first
    keyword is recorded.
  - A `(` at depth 0 right after a `)` is a parenthesized main statement.
    `Verbs` recurses into it, which also handles a WITH nested there.
  - If no main verb is found (malformed SQL), `Main` stays `with`, which
    keeps the old Query path.
- **`isQuery`** goes by `Main`, and looks for `RETURNING` only in
  `stmt[MainAt:]`. So `WITH d AS (DELETE … RETURNING id) INSERT INTO log
  SELECT id FROM d` is an Exec: the CTE's rows go to the statement, not the
  caller.
- **`isRead`** is false when `Main` is not a read verb, or when any CTE body
  is a write (`returningVerbs`). That catches Postgres's
  `WITH d AS (DELETE … RETURNING *) SELECT count(*) FROM d`, which returns
  rows but deleted them, so the session is marked stateful.
- `"with"` stays in `queryVerbs`, only as that fallback. `FirstKeyword` is
  now `wordAt(sql, keywordAt(sql, 0))`, the two helpers `Verbs` uses, which
  behaves the same (its tests pass unchanged).

### Tests

- `sqlsplit.TestVerbs`: 19 cases, including CTE lists with `RECURSIVE`,
  `MATERIALIZED`, column lists, `MERGE`, `CYCLE … SET`, writing CTEs,
  verb-like words inside strings, quoted names, comments and dollar bodies,
  and parenthesized and nested mains. It also checks that `MainAt` points at
  the verb.
- `db.TestIsQueryIsReadWith`: the decision table.
- `db.TestSessionWithWrite`: SQLite. `WITH … UPDATE` reports 2 rows affected
  and marks the session stateful; `WITH … DELETE … RETURNING` returns its
  row.
- Through the binary on `--demo sqlite`: the same two statements give
  `2 rows affected` and the returned names.
- **bytdb rejects a write after a CTE list**: `syntax error … got 'update',
  want SELECT`. dbc now classifies such a statement correctly, but bytdb's
  parser can't run it. That's a bytdb gap, not a dbc one, so it wasn't
  raised here.

`go test ./...` passes (CATS_* stripped); `gofmt -l .` is clean.

## Next

Closed: N-010, N-039. Declined: None. Raised: None. Deferred: None.
Promoted: None. Updated: None. Full list: `ai_docs/todo/next-list.md`.
