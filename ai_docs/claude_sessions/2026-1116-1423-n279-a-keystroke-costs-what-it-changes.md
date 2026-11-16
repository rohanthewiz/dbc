# N-279: a keystroke costs the rows it changes

Session: `3a5ced77-859f-425c-b2f1-a7dc2d5bd323`

## Ask

"continue with N-279" (RPU plan, phase 3), then `/sw`.

N-279 said each keystroke cost the whole buffer:

- `highlight` re-lexed everything and made a `Style` per rune.
- `checkpoint` copied the buffer for undo. `undoCap` counted 200 snapshots
  with no byte limit, and parked consoles kept theirs.
- Completion called `Text()`/`Caret()` and a synchronous `ws.Complete` per
  letter.

The suggested fix was a per-line lex cache (or highlighting only the visible
rows) and a byte-capped undo.

Commit: `67716dc`.

## Measured first

New benchmarks in `tui/frame_test.go`. Each types a letter mid-buffer into a
3.5 MB migration, opening the completion popup, then draws the 200×50 frame.
The "before" numbers were run on HEAD in a throwaway worktree, with the
benchmarks copied in.

| benchmark | before | after |
|---|---|---|
| `BenchmarkKeyBigBuffer` (a letter merged into the open step) | 111 ms, 139 MB | 1.7 ms, 9.8 MB |
| `BenchmarkKeyBigBufferNewStep` (a letter that begins a step) | 114 ms, 153 MB | 1.7 ms, 9.8 MB |
| `sqlsplit` `BenchmarkSplitBig` vs `BenchmarkSplittingEdit` (3.5 MB) | ~18 ms whole | ~0.6 ms, 4.2 MB |
| TUI e2e: 60 letters in one burst into a pasted 3.5 MB buffer | 6.2 s | 0.1 s |

The profile before the fix, per keystroke:

- highlight: ~40%
- two whole splits (title, completion's `stmtWindow`): ~25%
- the text join: ~11%

After N-278 these all ran once per edit, not per frame, but each one was
still the whole buffer.

What is left is O(statements) copying at memmove speed:

- the split's chunk and statement lists (~4 MB)
- the patched text (3.5 MB)
- the rows' outer slice (~1.5 MB)

## What was built

### The invariant everything rests on

`editor.lines`: a row is never changed in place once it is in `lines`. Every
edit puts a new slice where a row was, and only the outer slice is rewritten.
This was already true (`Insert` copies head and tail, and `Backspace` and
`Delete` use a three-index append). It is now documented and relied on:

- `snap()`: `lines` copied once per edit version (the outer slice only),
  shared by every cache and by the open undo step.
- `rowsDiff(old, now)`: how many rows at the head and tail are the same. It
  compares by identity first (a pointer compare) and falls back to the
  runes. A rename or a snippet's mirrors set the buffer afresh, and most of it
  is unchanged.

### Text (`editorBuf.patch`)

- The unchanged head and tail are copied over as bytes; only the middle rows
  are encoded.
- `starts` is edited in place with `slices.Replace`, and the tail is moved by
  delta.
- The arithmetic works in `text+"\n"` terms (`rowStart`), where every row is
  followed by its newline.
- `byteEdit(p, s, oldLen)` turns a row diff into the byte edit
  `[at, oldEnd) → [at, newEnd)` that sqlsplit takes.

### Split (`sqlsplit/resplit.go`, `sqlsplit.go`)

- `scan` is now `scanFrom(sql, d, from, delim, tab, rejoin)`. A chunk's start
  is a clean break: the only state there is the terminator.
- `chunk.delim` is the terminator in force at the chunk's start, kept as an
  index into a per-split `delimTable`.
  - As a string it made each chunk 16 bytes bigger, and a whole `Split` 8%
    slower and 18% bigger (17.7 → 19.1 ms, 21.3 → 25.2 MB).
  - As a `uint32` it fits in the padding after the bools: back to 21.3 MB.
  - An experiment with a 1-byte field showed the size was the whole cost,
    not the refactor's closures.
- `Splitting{d, n, chunks, delims, stmts}`. `Edit(sql, at, oldEnd, newEnd)`
  works like this:
  - It resumes one chunk before the chunk the edit is in. The previous cut's
    `remarkEnd` reads ahead to the first code after its `;`, which can lie
    inside the edit.
  - It rejoins at a new chunk start `x > newEnd` where an old chunk starts at
    `x−delta` under the same terminator.
  - Lookbehind at a chunk start is at most one byte: `x−1` is the
    terminator's last byte, or `x` is a newline. The rules that look back are
    `qualified`, `backslashEscapes`, `dollarTag` and `atLineStart`.
  - A first draft also required a newline past the edit. Its mutant survived
    because the rule was unnecessary, and it would have blocked rejoining in
    a one-line dump, so it was dropped.
  - It keeps the chunks before the resume point, moves those after the rejoin
    by delta, and re-slices every `Stmt.Text` from the new buffer, so no old
    buffer stays alive.
  - A new terminator is appended to a clipped copy of the table, so two
    edits of one `Splitting` cannot write into each other's.
  - An input that doesn't add up (lengths, bounds) falls back to a whole
    split.
- `Dialect.LexRange(sql, from, to)`: `Lex` from a point the whole scan stops
  at (0, a token's start, or a line start outside any token). It reads
  lookbehind from the whole `sql`, so it decides as `Lex` does.
- `stmtsOf` was factored out of `Split`.

### Editor split and completion

- `editor.Stmts(d)` keeps a `*sqlsplit.Splitting` and calls `Edit` with
  `byteEdit`'s range.
- `sqlcomplete.Request.Split`: the caller's split. `stmtWindow` takes stmts.
  `newResolver` (F12 and others, not per key) still splits.
- `workspace.CompleteIn(Editor)`, with `Complete(buffer, caret)` delegating
  to it. The TUI passes `m.editor.Stmts`; web is unchanged.
- `complAutoHere` and `complInWord` read `lineBeforeCaret()`. Nothing they
  look for crosses a line break.
- The editor and completion use the same dialect (`DialectOf` of the active
  connection's driver), so the one-dialect split cache does not ping-pong.

### Highlight (`tui/highlight.go`, new)

- `hlRow{in, lexed, toks}`:
  - `in` is where the lexer stands at the row's start: between tokens, or
    inside a token begun `back` rows up at byte `col`.
  - `toks` are the row's tokens as rune columns, holding kinds, not styles.
- `hlCache{rows, ln, done}`: rows `[0, done)` are certain, and `ln[done].in`
  is certain too.
- `highlight(upto)`: a new snapshot triggers `rowsDiff` and then `shift`;
  then it lexes down to `upto`. Draw asks for its last drawn row, and
  `rowStyles` (the usages list) for its row.
- `shift`:
  - Drops rows from `q = p−1`. A row's tokens can read its newline, and the
    edit may add or remove the last row's newline.
  - Keeps `q`'s `in`, and keeps the tail rows tentatively, moved in place
    with `slices.Replace`. A new slice per edit was 4 MB a key.
  - The dropped range can be empty when lines are added at the very top.
- `lex`:
  - Resumes at the row start or at the start of the token it is inside of.
  - Works in doubling batches (64, 128, …), so a far-down request stops at a
    confirmation.
- `spread`:
  - Hands tokens out to rows (`colCursor`: byte → rune column, immediate on
    ASCII rows) and computes each next row's `in`.
  - A tentative row reached between tokens, with the `in` it was lexed from,
    confirms itself and the run of lexed rows after it.
  - Inside a token there is no confirmation: two tokens begun at the same
    place can differ past an edit (a nested comment's depth).
- **The end-of-buffer rule** (found by the random test):
  - A token cut off by the buffer's end, ending at `len(text)` on the newline
    before an empty last row, is still open at that row's start.
  - No finished token ends on a newline; a line comment's token stops before
    it.
  - Before the fix, `'a⏎BEGIN;⏎` left the empty last row "between tokens",
    and text typed there was lexed as code.
- Styles are looked up as each rune is drawn, so `hlPal` is gone; a theme
  change needs nothing dropped. `setDialect` resets the cache.
- `hasIn` was in a first draft. Its mutant survived, showing it was
  redundant: a lexed row always has its `in`, and a confirmed run never ends
  at a blank row. It was removed.

### Undo

- `undoStep{at, old, n, cur, size, base}`:
  - The open step holds `base`, the snapshot from before it.
  - `closeStep` diffs `base` against `lines` and keeps a clone of only the
    replaced range. A sub-slice would keep the whole snapshot alive.
- It closes on the next step, `swap` (Undo/Redo) and `View` (parking a
  console).
- `swap` splices `old` back into a new outer slice and pushes the displaced
  rows as the inverse step.
- Caps: `undoCap = 200` steps and `undoBytesCap = 32 MB`
  (`stepSize ≈ 4·runes + 24·rows + 64`).
  - `trimUndo` runs after every push, so the stack never sits at 201.
  - The newest step is kept whatever its size.
  - Dropped slots are zeroed so the backing array lets their rows go.
- `setUndo` recounts bytes when `Restore` puts stacks back.
- A merge now needs an open step. Typing right after `SetText` within the
  coalescing window used to merge into nothing and could not be undone.

## Tests

- `TestEditorCachesFollowEveryEdit` (`tui/framecache_test.go`):
  - Now 60 seeds × 300 steps over Postgres, MySQL, SQLite and Generic.
  - New edit pieces open or close a token alone (`/*`, `*/`, `'`, `$$`,
    `--`, a trigger body).
  - Checks the split in the editor's own dialect, so the incremental path is
    what runs.
  - Highlight oracle (`checkHighlight`): after random partial lexes, every
    certain row against a whole lex (`refKinds`).
  - Undo oracle (`undoOracle`): each undo or redo must give back the text
    recorded when its step opened, which an in-place row change would
    corrupt. It took three tries to get the oracle's own bookkeeping right:
    - a new step typed after an undo moves the stacks like a redo
    - `SetText` empties both stacks
    - the byte cap can drop the oldest step on an undo too
  - Stress-run at 3,000 seeds.
- `TestByteEditDescribesTheEdit`: `old[:at] + new[at:newEnd] + old[oldEnd:]
  == new`, over several edits between reads. `Splitting.Edit` falls back
  silently on bad ends, so without this a wrong end would only cost speed.
- `tui/undo_test.go`:
  - `TestUndoStepsHoldTheRowsTheyChanged`: 200 kept steps under 1 MB on
    3.5 MB.
  - `TestUndoByteCapDropsTheOldest`
  - `TestUndoKeepsANewestStepPastTheCap`
  - `TestTypingAfterSetTextCanBeUndone`
  - `TestViewClosesTheOpenStep`
- `sqlsplit/resplit_test.go`:
  - `TestSplittingEditMatchesSplit`: 60 seeds × 300 edits, every dialect,
    DELIMITER, bodies, BOM, mid-token. Chunks are compared with the
    terminator spelled out.
  - `TestSplittingEditOfAnotherBuffer`
  - `TestSplittingEditSlicesTheNewBuffer` (`unsafe.StringData` bounds)
  - `TestSplittingEditsBranch`: a table with room to spare, so a shared
    append would collide.
  - `FuzzSplitEdit`: 90 s, then 60 s after the table change, clean.
    `FuzzSplit` 30 s, clean.
- Mutations: breaking each rule fails a test.
  - Split: resume one chunk back, the terminator check, the re-slice, the
    moved tail, the clipped table.
  - Highlight: the back-off row, the kept `in`, between-tokens-only
    confirmation, the end-of-buffer rule.
  - Editor: the patch's moved starts, `byteEdit`'s ends and clamp, an
    in-place Backspace, both caps, bytes counted, `View` closing the step,
    closing on a new step, merge-only-when-open.
- TUI e2e (`tui/e2e/tui_test.go`), before "Ctrl+Q quits": "typing into a
  3.5 MB buffer keeps up with the keys".
  - A new tab and one bracketed paste of the migration, then 60 `q`s in one
    `raw` write.
  - Bound 2 s. 104 ms now; on HEAD's binary (worktree) 6.2 s, so the step
    fails there.
- Full `./...`, `-race` on tui, workspace, sqlsplit, sqlcomplete and web, and
  the TUI e2e: all green.

## Found on the way

- The Write tool turned `\uFEFF` in a Go string into a raw BOM again (known
  pitfall). Fixed with python.
- The language server kept reporting stale errors from the files mid-edit
  (`hlRow redeclared`, `stmtsOf undefined`). `go build` and `go vet` were the
  truth throughout.
- Pre-existing `gofmt -l` hits: `tui/app_test.go` and `sqlsplit/folds.go`
  (the latter also on HEAD). Left alone.

## Next

Closed: N-279. Declined: None. Raised: N-327. Deferred: None. Promoted: None.
Moved: None. Updated: N-302 (`FuzzSplitEdit`), N-303 (the keystroke and
split benchmarks, without budgets). Full list: `ai_docs/todo/next-list.md`.
