# Headless `dbc copy` (N-125)

Session: `3350db03-d99e-4fd2-bfd0-8fc2d4a6656b` (follows
`2026-1006-1346-etl-scripting-layer`)

## Ask

N-125 from the next list, sent from the cats-todo backlog: a headless
`dbc copy` over `etl.Copy` (`--from A --to B table`, `--create`,
`--truncate`, `--where`, `--to`), so a cron job or shell pipeline can copy a
table without writing a one-function script for `dbc script`.

Later: wrap the session, including the README changes another agent made in
the same working tree.

## Design

- **Through `sdb.S.Copy`, not `etl.Copy` directly.** The command is the
  one-call script it replaces, so it reuses `sdb`'s connection resolution
  (`DriverOf` + `DBContext`, derived `<conn>/<database>` names included) and
  its `ProgressEvery` printing. The two cannot drift apart.
- **`--to` was listed twice** in the item: once as the destination
  connection, once as `CopyOptions.To` (the destination table). One flag
  can't mean both, so `--to` names the connection and an optional second
  argument names the destination table:
  `dbc copy --from prod --to local public.orders archive.orders`.
- **Output:** `CopyStats.String()` on stdout. A running row count
  (`  src → dst: N rows`, every 100,000) goes to stderr only when stderr is
  a terminal, so a cron job's mail holds just the summary.
- **Exit status:** 0 copied, 1 failed, 2 bad usage, 130 Ctrl+C.
  An unknown connection name is checked against the config before anything
  opens (`checkCopyConns`), which makes it exit 2 instead of a "copy failed"
  1.
- **Refusals (exit 2), before any connection opens:**
  - a table copied onto itself: same connection and same name, compared as
    typed. With `--truncate` on Postgres this would hang, since the load's
    TRUNCATE lock blocks the source COPY. Without it, the copy can only
    duplicate rows or fail on the key.
  - the root flags a copy would otherwise ignore silently: `-c` (ambiguous
    between the two ends), `-f`, `--tx` (a copy already loads in one
    transaction), `-k`, a non-text `-t`, and `-o`.
- Connections open lazily (`setup(demoLazy)`), as `dbc script` does.

## What changed

- `copycmd.go` (new): `copyCommand`, its flags (`flagFrom`, `flagTo`,
  `flagCreate`, `flagTruncate`, `flagWhere`), `copyAction`, `copyReq`,
  `checkCopyFlags`, `copyRequest`, `checkCopyConns`, `runCopy`.
- `main.go`: registers `copyCommand()` and adds the doc-comment line.
- `cats-plugin.toml`: `copy` subcommand plus `--from --to --create --truncate
  --where` completions (`TestCatsManifest_CompletionsCoverCLI` requires them).
- `main_test.go`: `resetFlags` also resets the new flags, and now resets
  `flagTx` and `flagKeep` too.
- README:
  - a new "Copy headless" section (examples, flag table, output and exit
    codes), a line in the Headless mode examples, and a pointer from the ETL
    section.
  - **from another agent, in the same tree:** a paragraph under "ETL across
    connections" saying `s.Copy`'s arguments are connection names (not
    DSNs), and that `<conn>/<database>` names another database on the same
    server. `config.ConnByName` resolves those names, so `dbc copy` accepts
    them too. The copy flag table says so, and `TestCheckCopyConns` covers
    `pg/reporting`.

## Tests

- `copycmd_test.go`:
  - `TestCopyCLIParsing`: flags before and after the table.
  - `TestCopyRequest`: every refusal, a renamed destination on one
    connection, and flags reaching `CopyOpts`.
  - `TestCheckCopyFlags`, `TestCheckCopyConns`: the latter includes a
    derived database.
  - `TestRunCopy`, over two file SQLite databases: a missing destination
    fails, `--create`, a re-run refused by the key with the destination
    unchanged, `--truncate --where`, a renamed destination, and no progress
    for a small table.
  - `TestRunCopyCanceled`: reported as a cancellation.
- Full `go test ./...` passes.
- The built binary, with `CATS_*` stripped from its environment:
  - SQLite ↔ bytdb: every scenario above, plus every usage error and its
    exit code. Under a pty, progress lines appear at 100k and 200k of 250k
    rows; with stderr redirected, stderr is empty.
  - Postgres 17 (throwaway docker, databases `dbc` and `dbc2`): a 2M-row
    direct COPY with `--create` in 4.0 s, `--truncate --where` (180k rows),
    and a renamed `--create` whose types came over exactly (`bigint`,
    `date`, `text[]`, `jsonb`, `numeric(10,2)`). SIGINT 300 ms into a copy
    exits 130 and leaves no table and no idle-in-transaction session.

## Bugs found (pre-existing, in the ETL layer; not fixed here)

1. **Date into bytdb:** bytdb's driver binds every `time.Time` as
   `"2006-01-02 15:04:05…"`, and `bytdb.ParseDate` takes only
   `YYYY-MM-DD`, so any date column copied into bytdb fails (N-128).
2. **numeric into bytdb with Create:** `etl.Engine.columnType` gives bytdb
   the Postgres name `numeric`, which bytdb has no type for (N-129).

Both affect `s.Copy` in scripts the same way.

## Next

Closed: N-125. Declined: None. Raised: N-128, N-129.
Deferred: None. Promoted: None.
Updated: None. Full list: `ai_docs/todo/next-list.md`.
