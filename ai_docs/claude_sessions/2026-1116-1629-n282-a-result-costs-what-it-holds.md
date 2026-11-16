# N-282: a result costs what it holds; an export streams

Session: `249c3841-255d-493f-bf01-27dfda5528fc`

## Ask

"do N-282" (RPU plan, phase 3), then `/sw`.

N-282 said a result is held twice and an export about four times:

- `readSet` built `Rows` and `Raw` from a value database/sql had already
  copied. MySQL's `[]byte` was cloned by Scan, then turned into a string
  once for each. Every number went through `fmt.Sprint`.
- A headless or `-o` export added the rendered string and a `[]byte` copy
  of it for `os.WriteFile`.
- In the TUI, `export.ToFile`, `renderCopy` and the sort (two `ToLower` per
  comparison) ran inside `Update`, so the screen froze once `max_rows` was
  raised.

The suggested fix: convert once and share the string, with strconv fast
paths; a streaming row writer for a single-statement csv/tsv/JSON-lines
export (N-267's `--max-rows` needed it); and run the TUI's work as a
`tea.Cmd` with an "exporting…" status.

Commit: `02c3aa0`.

## Measured first

Every "before" is HEAD (`dbb53ad`). Benchmarks were added for each part,
and a HEAD worktree in the scratchpad gave the "before" numbers for the
export benchmark and the real binary.

| what | before | after |
|---|---|---|
| fetch, MySQL-shaped, 10k × 14 (`BenchmarkFetch`) | 19 ms, 740k allocs, 20.8 MB | 10.5 ms, 360k allocs, 13.5 MB |
| fetch, typed (pgx/SQLite shape) | 14 ms, 296k allocs | 8 ms, 200k allocs |
| export to file, 100k rows, CSV (`BenchmarkExport`) | 39.8 MB allocated | 71 KB |
| … text | 67.8 MB, 64 ms | 71 KB, 30 ms |
| … Markdown | 49 MB | 71 KB |
| … JSON | 164 MB | 37 MB |
| sort, 100k rows, text (`BenchmarkSortRows`) | 389 ms | 30 ms |
| sort, 1M rows, text | 6.1 s, 53M allocs | 0.74 s, 1M allocs |
| sort, 1M rows, numeric | 555 ms | 218 ms |

In the fetch, the driver's own boxing of values is 120k of those
allocations (`BenchmarkFetchDriver`), so dbc's part went from 5.2 to 2.0
allocations per non-NULL cell.

Real binary, a 1M-row × 6-column SQLite table, `--max-rows 0`, `CATS_*`
stripped, scratch HOME:

| format | RSS before → after | time | output |
|---|---|---|---|
| csv (streamed) | 1,158 → 42 MB | 3.6 → 2.6 s | identical |
| csv `-o` | — → 45 MB | | identical to HEAD's stdout |
| json | 1,869 → 704 MB | 4.2 → 2.6 s | identical |
| text | 1,398 → 528 MB | 2.3 → 1.2 s | identical |

## What was built

### The fetch loop (`db/cell.go`, `db/stmtkind.go` `readSet`)

- **A Scanner destination.** `cell` implements `sql.Scanner`.
  database/sql's `convertAssign` clones a `[]byte` only for the destination
  types it knows (`*any`, `*[]byte`), so a Scanner gets the driver's value
  as it is. `cell.Scan` converts it before returning, as the Scanner
  contract asks for driver memory.
- **One conversion.** `cellValue` returns both the display text and the
  raw value:
  - a `[]byte` becomes one string, used for both
  - a string reuses the driver's own interface box
  - `int64`, `float64`, `bool`, `int32`, `uint64` and `float32` go through
    strconv. `TestCellValueMatchesSprint` holds it to the old `fmt.Sprint`
    over the edges and 2,000 floats by their bits.
  - a `time.Time` is formatted once (RFC3339) and the text used both ways
- **Slabs.** The rows' `[]string` and `[]any` are cut from chunks
  (`rowSlab`) that start at 8 rows and double, up to 16k cells. Each row is
  capped at its length, so an append to one cannot write over the next. A
  row with no columns stays an empty, not nil, slice.
- `rawVal` became `cellValue`'s second result. `renderVal` (EXPLAIN's rows)
  calls `cellValue`.

### Streaming rows out of db

- `db.StreamRows(ctx, sink)` puts a `RowSink` (`Set(cols)`, `Row(disp,
  raw)`) in the context. `readSet` then hands each row to the sink with one
  pair of reused slices, and the Result keeps only its columns and
  `Truncated`.
- `Set` is called for a set with columns or rows. The empty, column-less
  first set that tells runMaybe "it wrote" never reaches the sink, so an
  INSERT still comes back as its rows-affected Result.
- A sink error is marked `sinkErr`. `readSets` (`sinkStop`) stops a plain
  read on the server through its stmtcut Point instead of letting
  `rows.Close` drain the rest. A full disk should not wait out a large
  table.

### Export to an `io.Writer` (`export/export.go`)

- `Write`, `WriteRun` and `WriteBlock` render into a writer.
  `Render`, `RenderRun` and `RenderBlock` are those into a strings.Builder,
  so the bytes cannot drift.
- `bufio.Writer` keeps its first error, so the renderers skip per-write
  checks and `Flush` reports it.
- **JSON**, one row at a time: the one map is cleared and refilled, and an
  Encoder with indent `"  ", "  "` writes each row. Indent puts the prefix
  after each newline and none before the first byte, so `"  " + row`
  matches `MarshalIndent` of the whole array exactly. `jsonAll` (several
  results) is still built whole, because its envelopes' sorted keys put
  `rows` between `duration` and `statement`. It is now encoded straight to
  the writer.
- **Text table**: it used to hold a clipped copy of every cell, made
  through a `[]rune` of each. It now measures the widths and then writes,
  clipping each value twice. `textClip` has a no-allocation path for a
  value with no line break, valid UTF-8 and at most 60 runes. Anything else
  goes the old `[]rune` way, which also turns invalid bytes into U+FFFD, so
  the text stays the same.
- **HTML** is still built whole by the element builder and written once.
- Watch-out: `serr.Wrap(nil)` prints "SErr: Not wrapping a nil error" to
  **stdout**. Every wrap here sits behind `err != nil`, or a headless
  export would have carried that line.
- **`export.File`** (`Create`, `Commit`, `Abort`) and `WriteFile`:
  - writes `.<name>.<random>.partial` beside the path, then fsyncs, closes,
    sets the mode and renames
  - keeps the old file's mode, or uses 0644 for a new file (CreateTemp's
    0600 would have been a change)
  - resolves symlinks, so a link stays a link
  - is what `ToFile`, every headless `-o` and `writeOutTo` (scripts,
    pipelines) now use, so a failed export leaves the old file
- `TestWriteMatchesRender` keeps the old renderers in the test file and
  compares byte for byte over awkward results: NULLs, line breaks, pipes,
  quotes, long and wide values, invalid UTF-8, HTML characters, nested
  JSON, duplicate columns, no rows, no columns, short Raw rows, rows only,
  an exec result, 3,000 rows, and a multi-result run.
- Other tests: `TestWriteJSONRefusesNaN`, `TestWriteReportsWriterFailure`
  (every format), and `TestWriteFile` (a failure keeps the old file and
  leaves no `.partial`; the mode is kept; a symlink is kept; a missing
  directory fails).

### Headless (`main.go`)

- **`headlessOut`**: a 64 KB bufio over an `errWriter`, over stdout or the
  `-o` `export.File`. The `errWriter` keeps the destination's own error, so
  a failure is reported as "write failed" rather than "render failed".
- **`emitRun`** now writes as it renders. The truncation notes now follow
  the document on stderr: a note printed first scrolls away above a long
  output, and a streamed note can only come after.
- **`rowStreams`**: a lone CSV or TSV statement streams. A MySQL CALL's
  several sets are blocks a `BlockSep` apart, the same as the collected
  document, whatever the sets turn out to be.
  - JSON is not streamed: one set is a bare array and several are an
    array of envelopes, and which it is is known only after the first
    set's rows have gone.
  - Markdown and text need every row first, for a banner's count or a
    column's width.
- **`rowStream`** is the RowSink. Its `csv.Writer` takes `o.w` as its
  buffer: `bufio.NewWriterSize` returns a big-enough `*bufio.Writer` as it
  is. It counts rows per set for `noteTruncatedAt` and the "wrote N rows"
  line.
- Only the statement runs under the sink context. BEGIN and COMMIT keep
  the plain context.
- **Failures**:
  - A statement that fails part way, or a Ctrl+C: the rows read are
    flushed to stdout, the `-o` twin is dropped, and the exit is non-zero.
  - A render or write failure: the buffer is dropped (`failed`).
- **Tests**:
  - `TestRowStreamMatchesCollected` (CSV and TSV against `export.Render`
    of a collected Run, plus the notes of a cut result)
  - `TestRowStreamFailures`, as child processes: `abs()` of the smallest
    integer fails at row 500 with `op=iterate`, after rows had streamed,
    and the old `-o` file is kept; a read-only stdout exits 1 with "write
    failed"
- Real binary also checked: a capped TSV with its note, an un-streamed
  write result, a failing statement keeping `-o`'s old file, and no
  `.partial` left behind.

### Sort (`workspace/view.go`)

- `SortRows` makes each row's key once: the lower-cased text, or a float
  (`toFloat` now switches on the drivers' types before reflecting). It
  sorts with `slices.SortFunc` (pdqsort), with the row index as the last
  tie-break. A total order has one sorted arrangement, so the result is
  the old stable sort's exactly.
- The comparator's helpers (`nulls`, `keyed`) are built once. A closure
  per comparison would have allocated.
- `TestSortRowsMatchesOld` compares against the old algorithm, kept in the
  test, on 40 random results, every column, both directions: ties, NULLs,
  NaN, `int32`/`float32`, ß/SS/é, and Raw rows that stop short.

### TUI

- **Off-loop sort** (`tui/grid.go`):
  - Past `asyncSortRows` (50,000 rows, about 15 ms of text sort),
    `applySort` bumps `sortGen`, sets `sorting` and leaves a `sortCmd`. The
    rows stay as they are, and the strip (both orientations) says
    "sorting by … …".
  - A grid can't return a command from a header click or from
    `syncResults`, so `Update`'s tail starts it (`gridCmds`). A grid
    parked in the same message leaves its command in `m.parkedSorts`.
  - `sortDoneMsg` goes to the grid that asked, shown or parked.
    `sorted()` applies it only while its `gen` is current. `setResult`
    bumps the gen, so an answer for old rows never lands on new ones.
  - Back to the result's order, or a small result, still sorts in the
    click.
- **Copies and exports**:
  - A `piece` takes the rectangle: the rows and columns are copied (a sort
    on the loop rewrites `order` in place), plus `what`, `flip` and `first`
    (which replaced `selFirst`). The result is never changed once shown;
    sdb.Show copies it.
  - `renderCopyCmd` and the transposed plain copy build, turn, render and
    write on the command goroutine. A render failure comes back as
    `clipDoneMsg.failed`, logged as "copy failed".
  - The export dialog opens its `export.File` in the click, so a bad path
    still keeps the dialog open. `exportCmd` does the rest and reports
    with `exportDoneMsg`. Past 50,000 rows it logs "exporting … …" first.
- **Tests**:
  - `TestBigSortRunsOffTheLoop`: the answer lands; a superseded or
    new-result answer is dropped; a small sort runs in the click
  - `TestBigSortThroughUpdate`: a header click on 50,010 rows returns a
    command; the strip shows "sorting by …" and then "sorted by …"
  - `TestExportRunsAsACommand`: an unwritable path keeps the dialog; the
    file is not written until the command runs
- **TUI e2e** (`tui/e2e`): the config gets `max_rows = 300000`, and a new
  step "a 300,000-row result sorts and exports without freezing the screen
  (N-282)":
  - The strip answered the click in 32 ms and the rows moved in 115 ms.
    The export wrote 300,001 lines in sorted order.
  - The same step run on HEAD's code answered in 1.185 s and failed.
  - The first draft's labels (`A1`, `b2`: short, half lower-case) sorted
    in 0.1 s even on HEAD, so that step passed there. The step now uses
    `Customer <scrambled n> of Region <m>`, the costly shape: every value
    mixed-case, with a long shared prefix.

## Checks

- `go test -race ./...`: 34 packages ok. The `main` package was re-run
  after the last change to `failed`.
- The TUI e2e suite is green.
- `gofmt` and `go vet` are clean on every changed file. `tui/app_test.go`
  was already unformatted at HEAD and is left alone.

## Not done

- dbc web's export (`web/grid.go` `handleExport`) still renders a string. A
  render error has to be known before the headers go out. It belongs with
  N-284.
- A run of several statements still holds each result whole, and so do
  scripts.
- HTML is built whole.
- No JSON-lines format: raised as N-332.

## Next

Closed: N-282. Declined: None. Raised: N-332. Deferred: None.
Promoted: None. Moved: None. Updated: N-303. Full list:
`ai_docs/todo/next-list.md`.
