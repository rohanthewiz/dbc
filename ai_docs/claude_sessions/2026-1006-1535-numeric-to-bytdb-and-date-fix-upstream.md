# Numeric into bytdb (N-129); date fix upstream in bytdb (N-128)

Session: `d422360d-20aa-45ed-88cc-067de08a117f` (follows
`2026-1006-1430-headless-dbc-copy`)

## Ask

1. "fix N-129 numeric mapping for bytdb": `Create` into bytdb failed on a
   numeric/decimal source column.
2. "Do N-128 on bytdb": date columns could not be copied into bytdb. Fixed
   in the bytdb repo (`../bytdb`), then wrapped and released there as
   v0.21.1 at the user's request.

## N-129: numeric into bytdb (`etl/engine.go`)

Two causes, both fixed:

1. **No bytdb type for `numeric`.** `Engine.columnType` gave bytdb the
   Postgres names, and bytdb has no decimal type. bytdb now has its own row,
   identical except numeric → `double precision`.
2. **SQLite's sized decimals missed the numeric family.** SQLite reports
   declared types verbatim (`DECIMAL(10,2)`, `NUMERIC(5)`), and `familyOf`
   matched only the bare names, so those columns became text. `familyOf`
   now also matches the `NUMERIC(` / `DECIMAL(` prefixes. Other engines
   benefit too: a SQLite → MySQL/SQLite/Postgres `Create` now gets a
   numeric column instead of text.

**Why float, not text.** Probed what a bytdb column accepts through the
driver:

| value as it arrives | bytdb `text` | bytdb `double precision` |
|---|---|---|
| decimal string (pgx, MySQL) | accepted | accepted (parsed) |
| int64 / float64 (SQLite) | rejected: "value does not fit column type" | accepted |

Float also keeps numeric ORDER BY and arithmetic. The cost is silent
rounding past ~15 significant digits (`123456789012345678901234.5678` →
`1.2345678901234569e+23`). bytdb's own docs already say `numeric` casts
land on float. The README's ETL `Create` paragraph documents the float
mapping and the workaround: create the table yourself with a text column.
That workaround works from Postgres and MySQL, not SQLite.

`timestamptz` stays in the bytdb row: bytdb parses it and folds it into
timestamp.

## N-128: dates into bytdb (fixed upstream)

- **Cause:** bytdb's driver binds every `time.Time` as UTC
  `'2006-01-02 15:04:05…'` text, and `bytdb.ParseDate` accepted only
  `YYYY-MM-DD`. pgx (Postgres) and modernc (SQLite) both read dates as
  `time.Time`, so every date column failed.
- **Fix, in bytdb:** `ParseDate` accepts any timestamp text and keeps the
  date as written, dropping the time and zone. A malformed time is still
  rejected. This matches Postgres 17, checked in a throwaway container:
  `'2024-01-02 23:30:00-05'::date` is 2024-01-02, and `'2024-01-02 25:00:00'`
  is an error. Recorded as bytdb N-026. Released as bytdb **v0.21.1** and
  **pgwire/v0.21.1**. bytdb session doc:
  `2026-1006-1520-date-input-accepts-timestamp-text`.
- **dbc side:** not bumped yet. With a throwaway `go.work` (in the session
  scratchpad, outside both repos) pointing dbc at the local bytdb, an etl
  SQLite→bytdb `Copy` of DATE columns worked. On v0.21.0 it fails as
  before. N-128 stays open until dbc moves to v0.21.1.

## Tests

- `TestCopyNumericToBytdbCreates` (`etl/copy_test.go`): SQLite `NUMERIC`,
  `DECIMAL(10,2)` and `numeric(5)` columns, `Create` into bytdb. Values
  match the source, and `ORDER BY` sorts as numbers. It fails on the old
  `engine.go` with "unknown column type".
- `TestFamilyOf`: `DECIMAL(10,2)`, `NUMERIC(5)` → numeric; `NUMERICAL`
  stays text.
- Full `go test ./...` passes (with `CATS_*` stripped). `gofmt` and
  `go vet ./etl` are clean.
- Not run: Postgres → bytdb for numerics. A probe showed that bytdb parses
  the decimal strings pgx yields into a float column.

## Next

Closed: N-129. Declined: None. Raised: None.
Deferred: None. Promoted: None.
Updated: N-128. Full list: `ai_docs/todo/next-list.md`.
