# Session: multi-statement headless execution

Session id: `1a4872d0-250e-43f4-97f0-6a94acd71c40`
Date: 2026-07-28
Follows: `2026-0728-2000-stmt-under-cursor-and-query-cancel.md` (picked up its
first "next idea" — headless was still running the whole string as one
statement)

## Ask

> now add multi-statement headless execution

## What was built

### The run loop (`main.go`)

`dbc "INSERT …; SELECT …"` splits the SQL argument with `sqlsplit.Split` — the
same scanner the editor uses, so semicolons inside strings, comments, and `$$`
bodies still don't split — and runs the statements in order.

- `runStatements(ctx, sess, stmts) ([]*model.Result, error)` was extracted so
  it is testable from `package main`: it stops at the first failure and returns
  **both** the results that completed and the error, so the caller can still
  render the work that got done. A failure in a multi-statement run is tagged
  `statement=2/3` via `serr.Wrap` (single statements stay untagged — no
  pointless `1/1`).
- Output happens before the error is reported: a failing statement 3 does not
  invalidate results 1 and 2. Exit stays 1 for a failure, 130 for Ctrl+C
  (`errors.Is(err, db.ErrCanceled)` still works through the extra wrap).
- Empty/comment-only SQL exits 2 with `nothing to run — the SQL holds no
  statement`.
- `emit()` centralizes stdout vs `-o file`; the file message becomes
  `wrote 2 results (3 rows) to x.html` when there is more than one.

### Session pinning (`db/manager.go`) — the non-obvious part

A first pass ran each statement through `Manager.RunContext`, which takes
whatever pooled connection is free. That silently breaks the thing people
actually type into a multi-statement buffer: `BEGIN` lands on connection A and
the `UPDATE` on connection B, so the "transaction" isn't one. Same for `SET`,
`PRAGMA`, and temp tables.

- Extracted the statement body into
  `(m *Manager) run(ctx, ex execQuerier, name, stmt, args…)`, where
  `execQuerier` is the `ExecContext`/`QueryContext` pair that both `*sql.DB`
  and `*sql.Conn` satisfy.
- New `db.Session` wraps a `*sql.Conn` pinned from the pool:
  `Manager.Session(ctx, name)`, `(*Session).Run(ctx, stmt, args…)`, `Close()`.
- Headless multi-statement runs open one session for the whole buffer.
  `RunContext` is unchanged for everyone else (TUI, scripts), and its doc now
  says out loud that it takes any free connection.

### Multi-result rendering (`export/export.go`)

New `RenderAll(rs []*model.Result, f Format)`. Each format keeps its own idiom
rather than being force-fit into one:

| Format | Shape |
| --- | --- |
| `text`, `markdown` | banner per statement — `-- 2/3 │ demo │ 3 rows in 150µs` + the statement, above its table |
| `csv`, `tsv` | blocks separated by a blank line, each with its own header row — a banner would break parsers |
| `html` | one document, `<h2>Statement 2 of 3</h2>` section per result |
| `json` | array of envelopes `{statement, conn, duration, columns, rows}` — or `rows_affected` for a non-query |

- **A single result renders byte-identical to before, in every format** —
  `RenderAll` delegates to `Render` for `len(rs) == 1`, and a test pins that
  down across `Names()`. Bare-concatenating row arrays for JSON was rejected
  because it loses which rows came from which statement.
- Supporting refactors: `htmlDoc` is now `htmlDocAll([]*Result)` with the
  heading switching to "Query Result" for one result (uses `element.ForEach2`,
  which v0.6.0 has); `jsonArr` and `jsonAll` share `rowMaps`; `summary(r)`
  ("3 rows in 150µs" / "1 rows affected in 50µs" / "(truncated)") feeds both
  the banners and the HTML meta line.
- Durations round to 10µs, matching the status bar, so a fast statement reads
  as `150µs` instead of `0s`. (Slight change to the HTML meta line: it was
  `connection: demo · 3 rows · 2ms`, now `connection: demo · 3 rows in 2ms`.)
- `preview()` flattens a statement to one clipped line for the banner, so a
  multi-line statement can't smear the table under it.

## Verification

- New `export/export_test.go`: single-result identity vs `Render` for every
  format, text banners incl. flattening, exact CSV block layout, JSON envelope
  shape (exec carries `rows_affected` and *no* rows), HTML one-document +
  section headings, markdown banners, empty and unknown-format errors.
- New `main_test.go` (package `main`, in-memory SQLite seeded per test):
  ordering with a later `SELECT` seeing an earlier `INSERT`, `BEGIN; INSERT;
  ROLLBACK; SELECT` proving the statements share a session, stop-at-first-error
  keeping the earlier result, and a pre-canceled context yielding
  `db.ErrCanceled` tagged `1/2`.
- Manual runs against the demo DB in all six formats, `-o` to file, error
  midway (results printed, then `statement=2/3`, exit 1), and the rollback
  case (count 9 inside the txn, 8 after).
- Full suite, `go vet`, `gofmt` clean.

## Gotchas worth remembering

- `serr` fields are **not** in `err.Error()` — assert on them with
  `errors.As(err, &se)` where `se` is `*serr.SErr` (pointer: `serr.Wrap`
  returns `*SErr`, so a value target never matches), then `se.FieldsMap()`.
- `*sql.DB` and `*sql.Conn` share enough method surface that a two-method
  interface lets one runner serve both — no duplication needed for pinning.
- modernc sqlite returns a stale `changes()` for `RowsAffected` on `BEGIN`
  (showed as "8 rows affected" in the demo DB). Postgres/MySQL report 0. Left
  alone — it's pre-existing driver behavior, not multi-statement logic.

## Loose ends / next ideas

- Output is collected then written, not streamed per statement — `-o` and the
  single-document HTML/JSON formats want it that way, but a long `text` run
  shows nothing until it finishes.
- No `-tx` flag wrapping the whole buffer in a transaction, and no
  continue-on-error mode; both are small if wanted.
- SQL still comes only from the argument — no `dbc -f file.sql` or stdin.
- The TUI still runs one statement at a time by design; a "run all" key could
  reuse `runStatements` if that changes.
- Earlier ideas still open: per-connection query history, result-cell
  inspection popup.
