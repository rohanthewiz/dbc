# Headless script streaming

Session: 7a323613-cb82-4757-954c-56bbb7d4d38c
Date: 2026-09-25

## Ask

One next-list item, pasted as-is:

- **N-040**: "A headless `dbc script` still collects what it `s.Show`s and
  renders it at the end, while its `s.Print` lines stream to stdout in
  `text` — so the log runs ahead of the results it describes. Streaming them
  (N-003's `blockStream`) needs a banner that does not know the total, since
  a script's result count is not known up front (`#2` instead of `2/5`)."

## Design

- **When a script streams** — `scriptStreams(f)`: stdout (no `-o`) and a
  block format (`export.Streamable`: text, markdown, csv, tsv). Unlike
  `streamsResults` there is no count condition: a script's count is unknown
  until it returns.
- **Open-ended banners** — `export.RenderBlock(r, f, i, n)` treats `n <= 0`
  as an open-ended total and writes `#i` instead of `i/n`. The new
  `export.Pos(i, n)` formats both and is used by banners, notes and errors.
- **A lone result keeps its banner.** A stream has to write block 1 before
  it knows whether block 2 will come. The alternative was to hold the first
  block until a second arrived or the script ended, which keeps one-`Show`
  output bare. It was rejected because it brings back the exact bug for
  result #1 (a script that shows, then logs through a long loop). Cost: a
  one-`Show` script's `text`/`markdown` output now starts with a `#1`
  banner. CSV/TSV have no banners, so they are unchanged.
- **`-o` matches the stream** — the new `export.RenderOpen(rs, f)` is the
  open-ended blocks joined by `BlockSep`. `emitScript` uses it for block
  formats, so `dbc script x.go > f` and `-o f` hold the same results (the
  stream has the `s.Print` lines between blocks in `text`). HTML and JSON
  still collect through `RenderAll`/`emit`, since they are one document and
  their count is known by then.
- **`blockStream` for scripts** — `total: 0` means open-ended. `where(i)`
  names a block `statement 2/5` or `result #2` for the serr field and the
  truncation note. The new `show(r, stop)` fits sdb's `show` callback, which
  has no error to return. A failed render or write is kept on `b.err`, the
  script's context is canceled once (`stop`), and later results are dropped.
  `runScriptHeadless` checks `stream.err` before the script's own error, so
  the exit reads "render failed", not "script canceled".
- `noteTruncated` now takes the whole label (`"statement 2/5"`,
  `"result #2"`, or `""`) instead of a bare position it prefixed with
  "statement". `emitRun`'s stdout/file tail became `writeOut`, shared with
  `emitScript`.

## Tests

- `export`: `TestRenderBlockOpenEnded` (`#2` banners in text/markdown, CSV
  unchanged), `TestRenderOpen` (joined blocks, a lone result keeps `#1`,
  refuses empty/HTML/JSON), `TestPos`.
- `main`: `TestScriptHeadlessStreamsInOrder` (captures stdout through a pipe
  and checks log 1 < `#1` < log 2 < `#2`; this fails on the old
  collect-at-end code), `TestScriptHeadlessOutfileMatchesStream` (with the
  log lines removed and durations normalized, `-o` equals the stream),
  `TestBlockStreamOpenEnded` (matches `RenderOpen`, note says
  `result #2`, a render failure stops the script once and names
  `result=#1`), `TestScriptStreams`. New helper `captureStdout`.
- Tests ran with `CATS_*` stripped. All packages pass, and vet and gofmt are
  clean.
- Through the binary on a scratch SQLite file, with a script that sleeps 1s
  after each `Show`: text output showed `#1` at :11 right after its log line
  and `#2` at :12. Markdown, CSV (log on stderr), `-o` and JSON all looked
  right.

## Docs

README "Scripts headless" now describes streaming, `#n` banners (a lone
result included), and `-o` matching the stream.

## Next

Closed: N-040. Declined: None. Raised: None.
Deferred: None. Promoted: None.
Updated: None. Full list: `ai_docs/todo/next-list.md`.
