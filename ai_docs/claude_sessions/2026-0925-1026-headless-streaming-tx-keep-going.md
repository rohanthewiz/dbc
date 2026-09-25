# Headless streaming, --tx and --keep-going

Session: 7d2d9a7d-fa2d-4f7b-83fc-11b46aab7ca5
Date: 2026-09-25

## Ask

Two next-list items, each pasted as-is:

- **N-003**: "Headless multi-statement output is collected and rendered at the
  end, not streamed per statement, so a long `text` run shows nothing until it
  finishes. … `-o` and the single-document HTML/JSON formats want the
  collected shape, so streaming would be `text`-only."
- **N-004**: "Headless: no `-tx` flag to wrap the whole buffer in one
  transaction, and no continue-on-error mode. Neither exists today."

N-003 was committed on its own (`0f5e7a5`) before N-004 started.

## N-003: streaming per statement

### Decisions

- **Streams in every block format, not only `text`.** For `text`, `markdown`,
  `csv` and `tsv`, the multi-result document is just the per-statement blocks
  joined by a blank line, so writing them one at a time gives the same bytes.
  Only HTML (a `<html>` shell) and JSON (one array) wrap every result and have
  to wait.
- **Still collected:** HTML, JSON, `-o` (one file, and a "wrote N rows"
  total), and a single statement (it renders bare, with no banner).
  `streamsResults(f, stmts)` decides.
- **One rendering primitive.** New `export.RenderBlock(r, f, i, n)` renders
  one block, and `RenderAll` is built from it, joined by `export.BlockSep`.
  `export.Streamable(f)` names the block formats. The unused
  `joinBlocks`/`position` helpers went with the refactor.
- **`runStatements` gained a per-result callback.** `blockStream`
  (`main.go`) writes a block, then its truncation note (`noteTruncated`,
  factored out of `warnTruncated`) on stderr, so the note lands next to its
  result. A render or write failure is kept on `blockStream.err`, so the
  caller reports "render failed" rather than "query failed".
- `os.Stdout` is unbuffered in Go, so each block shows as soon as it's
  written.

### Verified

Through the binary on `--demo sqlite`, with a 5M-row recursive CTE as
statement 2 and output lines timestamped: block 1 printed at 0.90s, block 2
and 3 at 2.57s (the CTE took 1.66s). `-t json` printed everything at the end.

## N-004: --tx and --keep-going

### Decisions

- **`--tx`** (`-tx` works too, since cli v3 takes one dash) runs dbc's own
  `BEGIN` through the session, the statements, then `COMMIT`. On the first
  failure, a render failure or a Ctrl+C, `endTx` runs `ROLLBACK` instead,
  under `context.WithoutCancel` plus a 10s timeout, because Ctrl+C has already
  killed the run's context. It then prints "note: transaction rolled back" on
  stderr. If the ROLLBACK itself fails, it's logged, and the connection is
  discarded, which rolls back on every server. A failed COMMIT exits 1 as
  "commit failed". The transaction is settled before any `fail`/`canceled`,
  since those `os.Exit` and skip the deferred `sess.Close`.
- **Results are still shown in a rolled-back run.** They're what the
  statements saw, and the stderr note says none of it was kept. This keeps
  the existing "output first" rule.
- **`-k`/`--keep-going`**: `runHooks{keepGoing, onFail}`. Each failure is
  logged as it happens (`reportFailed` → `logger.LogErr`, the same shape
  `fail` uses, with `statement=2/3`) and the run goes on. It exits 1 at the
  end with "N of M statements failed". It still stops on `db.ErrCanceled`
  and on `sess.Classify(err) != FaultNone` (a dead connection: every later
  statement would fail, or run without the earlier session state).
- **Refused as usage errors (exit 2) by `checkRunFlags`, before
  connecting:**
  - `--tx` with `-k`: a transaction is all or nothing, and Postgres rejects
    every statement after an error inside one anyway.
  - `--tx` over a buffer with its own transaction control. `txControl`
    checks the leading keyword: BEGIN, START … TRANSACTION, COMMIT, END,
    ROLLBACK (but not ROLLBACK TO), ABORT, PREPARE … TRANSACTION. A second
    BEGIN is an error on SQLite, only a warning on Postgres, and an implicit
    COMMIT on MySQL. SAVEPOINT and RELEASE are allowed.
  - Either flag on `script`/`migrate`: `refuseFile` became
    `refuseQueryFlags`.
- **MySQL's implicit commit before DDL** can't be read off the SQL reliably,
  so it's documented in the README rather than detected.
- **Real statement positions everywhere.** `runStatements` now returns a
  `runOutcome{results, at, failed}`. New `export.RenderRun(rs, at, total, f)`
  (with `RenderAll` = `RenderRun(rs, Seq(n), n, f)`) numbers text/markdown
  banners and HTML headings by statement, and chooses the document's shape by
  `total`, not `len(rs)`. `emitRun`/`warnTruncated` take `at`/`total` too.
  This fixed two older quirks of a failed run:
  - collected banners said `2/2` while the error said `statement 3/5`;
  - `-t json` for a multi-statement run that failed after one success
    printed a bare row array instead of the envelope array.
  Streamed and collected output now match byte for byte in every case,
  including a gap left by `-k`.

### Verified

Through the binary with `--driver sqlite --dsn file:…` and
`--driver bytdb --dsn …` (scratch files, so the real demo cache was never
written):

- a `--tx` run whose statement 2 failed printed statement 1's result and the
  rolled-back note, exited 1, and a later count showed 0 rows;
- a good `--tx` run kept both rows;
- `-k` printed `-- 1/3`, the error for 2/3, then `-- 3/3`, and exited 1 with
  "1 of 3 statements failed";
- `--tx -k`, `--tx` over `BEGIN; … COMMIT`, and `--tx script x.go` each
  exited 2 with their message.

## Tests

- `export`: `TestRenderBlockJoinsToRenderAll`,
  `TestRenderBlockRefusesDocumentFormats`,
  `TestRenderRunKeepsStatementPositions`, `TestRenderRunShapeFollowsTotal`,
  `TestRenderRunNeedsAPositionPerResult`.
- `main`: `TestBlockStreamMatchesCollected` (all four block formats),
  `TestBlockStreamStopsAtFailure`, `TestBlockStreamRenderFailureStopsRun`,
  `TestBlockStreamNotesTruncation`, `TestStreamsResults`,
  `TestRunStatementsKeepGoing`, `TestRunStatementsKeepGoingStopsOnCancel`,
  `TestBlockStreamMatchesCollectedWithGap`, `TestTxControl`,
  `TestCheckRunFlags`, `TestEndTx` (commit, and rollback under a canceled
  context). `TestWarnTruncated` gained a gap case (`statement 4/5`).
  `setBoolFlag` helper added next to `setFlag`.

`go test ./...` passes (CATS_* stripped); `gofmt -l .` is clean.

## Docs

README: flags table (`--tx`, `-k`); the multi-statement section now
describes streaming, `-k`, and `--tx` (including the MySQL DDL caveat); the
scripts section says `--tx`/`-k` don't apply there.

## Next

Closed: N-003, N-004. Declined: None. Raised: N-040. Deferred: None.
Promoted: None. Updated: None. Full list: `ai_docs/todo/next-list.md`.
