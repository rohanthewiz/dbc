# Robustness items 6–9 from the review backlog

Session: df978c21-dac7-42ba-a08d-69b3d495f6f7

## What happened

Continued from the low-hanging-fruit session: implemented the four remaining
robustness items in `ai_docs/fable_recommends.md`, one commit per item (as
requested mid-session — that's a standing preference now). All tests pass,
`go vet` clean, plus a live smoke test of the headless behavior.

## Commits (in order)

1. `5ee18d4` — **Truncation warning in headless output** (item 6). New
   `warnTruncated(w, results)` in `main.go`, called from `emit`: every result
   with `Truncated` set gets a stderr note
   (`note: result truncated at N rows — raise max_rows in config for more`),
   prefixed `statement i/n:` in multi-statement runs. Data stream on stdout /
   `-o` stays clean. Test: `TestWarnTruncated`.
2. `1a4ec0c` — **Config validation at load** (item 7). `config.Load` expands
   DSN env vars via `os.Expand` + `os.LookupEnv` instead of `os.ExpandEnv`,
   collecting `cfg.Warnings` naming the connection and unset var. Warnings
   print to stderr in headless runs (`warnConfig` in main.go) and to the log
   pane in the TUI (`ui.Run`). Duplicate connection names and a
   `default_connection` naming no configured connection are now load errors.
   New `config/config_test.go` (4 tests).
3. `259224c` — **SQLite busy_timeout** (item 8). `sqliteDSN` appends
   `_pragma=busy_timeout(5000)` to every sqlite DSN that doesn't set one,
   applied in `Manager.DB`. Verified in the modernc.org/sqlite@v1.54.0
   source (conn.go `newConn`, sqlite.go `applyQueryParams`) that `_pragma`
   DSN params are applied per new connection, busy_timeout first, and that
   query params work with or without a `file:` prefix. New
   `db/manager_test.go`: `TestSqliteDSN` + `TestSqliteConcurrentWrites`
   (4 writers × 25 inserts on a file DB — fails "database is locked"
   without the pragma).
4. `bd8ed65` — **Duplicate columns survive JSON export** (item 9). New
   `jsonKeys` in `export.go` suffixes duplicate column names (`a`, `a_2`,
   `a_3`; collision-safe against a pre-existing `a_2` column). Used by
   `rowMaps` and by `jsonAll`'s `columns` field so the envelope matches the
   row-object keys. Tests: `TestJSONKeepsDuplicateColumns`, `TestJSONKeys`.
5. `ed0794d` — Doc + README updates: items 1–9 all marked
   `✅ *(done 2026-07-31)*` inline in `fable_recommends.md`; README notes the
   env-var warning, load-time validation, truncation note, and JSON
   duplicate-column suffixing.

## Design decisions

- Unset env var in a DSN is a **warning**, not an error — the connection may
  be one of several and unused; duplicates and a bad `default_connection`
  are config bugs and fail fast.
- busy_timeout via DSN pragma rather than `SetMaxOpenConns(1)` — a
  single-conn pool would re-create the pinned-Session-starves-the-pool
  deadlock that memory mode dodged by allowing 2 conns.
- `warnTruncated` fires for multi-statement runs too (banners already say
  "(truncated)" but csv/tsv have no banner).

## Smoke test

`dbc -config <max_rows=2, DSN with unset ${VAR}> -f csv "…5 rows…" > out.csv`
→ both the env-var warning and the truncation note on stderr, clean 2-row
CSV on stdout, exit 0.

## Still open (see ai_docs/fable_recommends.md)

All features: query history; headless script `-f`/`-o`; list-tables key;
NULL styling in results; row/cell copy; display cap decoupled from
`max_rows`.
