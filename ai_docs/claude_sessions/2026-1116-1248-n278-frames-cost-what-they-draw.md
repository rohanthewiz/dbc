# N-278: a TUI frame costs what it draws

Session: `1989239b-fa89-4e54-a24a-2231cda1f592`

## Ask

"continue with N-278" (RPU plan, phase 3), then `/sw`.

Bubble Tea calls `View()` after every message, and the TUI asks for every
pointer motion (`MouseModeAllMotion`). So a frame's cost is paid on each
mouse move, each streamed chunk of an answer and each `s.Print`. The item
listed the parts of the frame that redid work the size of their whole
content:

- **Editor:** `Text()` about four times and two dialect `Split`s per frame;
  `offset()` made a string per line.
- **Log:** all 2,000 lines re-wrapped, every timestamp formatted.
- **Chat:** the transcript re-wrapped and every code block re-lexed.
- **Inspect:** the value split and wrapped twice.
- **Scripts:** each `s.Print` was a blocking `p.Send` plus a frame, and a
  background tab's pending list grew without bound.

Commits:

- `a5730ac`: N-326 (found on the way, its own commit).
- `e3ecbc1`: N-278, which also closes N-325.

## Measured first

New `tui/frame_test.go`: a pointer motion plus its 200×50 frame. The
"before" numbers were run on HEAD in a throwaway worktree, with the
benchmarks and the harness's `testing.TB` change copied in.

| benchmark | before | after |
|---|---|---|
| `BenchmarkFrameBigBuffer` (3.5 MB buffer, full log) | 96 ms, 70 MB | 0.27 ms, 0.42 MB |
| `BenchmarkFrameLog` (full log only) | 6.7 ms | 0.26 ms |
| `BenchmarkFrameLogLine` (a line lands, then the frame) | 6.3 ms | 0.27 ms |
| `BenchmarkFrameChat` (60 answers, each with a 30-line SQL block) | 8.1 ms | 0.41 ms |
| `BenchmarkFrameChatDelta` (a streamed chunk) | 8.1 ms | 0.95 ms |
| `BenchmarkFrameChatBigBuffer` (assistant over 3.5 MB) | 129 ms | 0.39 ms |
| `BenchmarkFrameInspect` (1 MB value) | 40 ms | 0.35 ms |
| `BenchmarkScriptPrints` (2,000 prints, real `tea.Program`, full log) | 4.4 s | 6.5 ms |
| TUI e2e, 10,000 prints in the real binary | 7.2 s | 42 ms |

A profile of the big-buffer frame after the fix shows only canvas drawing
and `Canvas.String`: nothing buffer-sized is left.

`BenchmarkFrameChatBigBuffer` was the case the item had not named. The
assistant's context chip calls `ws.ChatContext` on every frame. That goes
through `workspace.Pick`, which split the whole buffer again. That is why
the cache went into `workspace.Editor` rather than staying in the TUI.

## What was built

### Editor (`tui/editor.go`)

- `editorBuf`: the joined text and each line's byte start, kept per edit
  version.
  - `buffer()` builds it in one pass for the starts and one copy of the
    runes.
  - `offset` is `starts[row]` plus the row's head (`runesLen`, which
    counts an invalid rune as U+FFFD's three bytes, as `string([]rune)`
    does).
  - `posAt` is a binary search over the starts.
  - An offset inside a rune now goes to that rune's end. The old
    conversion counted stray bytes as runes and could put the column past
    the line.
- `editorSplit` / `Stmts(d)`: the split, kept per version and dialect.
- `checkpoint` bumps the version *before* the edit lands. This is safe
  only because nothing reads the buffer between the two, which the
  highlight cache already assumed. The comment on `buffer()` says so, and
  the random test holds every path to it.
- The highlight is now also keyed on the palette (`hlPal`). Before, a
  cats theme change left the editor in the old colors until the next
  keystroke.

### Workspace (`workspace/pick.go`)

- `Editor.Split func(d) []Stmt`: the UI's own split.
  - `Pick`, `RunEditor`'s run-all (`pickAll`) and so `ChatContext` use it.
  - nil splits `Text` as before. dbc web does not set it.
- `StmtRangeIn(all, caret)` sits beside `StmtRange`.

### TUI wiring (`tui/run.go`)

- `editorState` passes `m.editor.Stmts`.
- `currentStmtRange` uses `StmtRangeIn` over the kept split.
- `stmtsToRun` keeps a selection's pick in `selPick`, keyed on the editor,
  version, range and dialect. Hovering with a whole 3.5 MB buffer selected
  would otherwise split it on every motion.

### Log (`tui/logpane.go`)

- `rows`/`nrows`/`rowsW` cache the wrapped rows per width. `wrapped(w)`
  wraps only the lines added since the last frame.
- `trim` drops the oldest lines' rows, or all of them when more lines went
  than were ever drawn.
- `clear` resets the cache.
- New `take(from)`, used by `syncLog`'s holding-log fold.
- A guard resets the cache if `lines` ever shrinks behind its back.

### Chat (`tui/chat.go`)

- `laidMsg` per message holds the rows plus what they were laid out from:
  role, text, width, palette, dialect, `lead` (the blank before a
  question) and `chip` (the copy row of an answer that is not streaming).
- An unchanged text compares as the same string, which is a pointer
  check. A streamed chunk makes a new string, so only that answer is laid
  out again.
- `flat` reuses the backing array frame to frame.

### Inspect (`tui/modals.go`)

- Lines are kept per body width (`linesW`).
- The size is kept per screen size (`inspectSize`).

### Prints (`tui/prints.go`, new)

- `printQueue` (a mutex and a slice). The workspace sink sends
  `ScriptPrint` to `scriptPrinted` and everything else through `m.send`
  as before.
- `Update` calls `drainPrints` before routing any message, so a run's
  lines land above its "completed" note.
- `push` returns a `wakeKind`:
  - `wakePost` when the queue was empty: `m.post`, which is `sendLater`,
    a goroutine around `send`.
  - `wakeWait` at `printBacklog` (= `logMaxLines`) lines: a blocking
    `m.send`, which is the backpressure.
  - `wakeNone` otherwise.
- The first try sent the wake-up from the script's goroutine. Send
  returns only once the loop takes the message, which is after the frame
  it is drawing. Each drain then found one line: 372 ms and 545 MB for
  2,000 prints, about a frame per line still. Posting fixed it (6.5 ms).
- `Model.post` is a new field. The test harness sets it to its
  synchronous recorder, because a goroutine would race `drive`'s reading
  of `sent`.
- Background tabs (`routeTab`):
  - A `ScriptPrint` goes straight to its connection's log, not onto the
    pending list.
  - A `ScriptShow` right behind another of the same connection replaces
    it. Replaying each would only draw over the one before, since the
    results are already in the workspace's result set.
  - A nil show is not queued.
  - A closed tab's queued lines are dropped, as its other events are.

## N-326, found on the way

`TestEditorCachesFollowEveryEdit` panicked in `wordLeft` on its first run.
The bug:

- With the caret at the start of a line, `left1` lands on the line above
  at its length.
- The first loop then reads `lines[row][col]`, one past the end.
- Bubble Tea recovers the panic and exits, so Alt+Left, Ctrl+Left, Alt+B,
  Ctrl+W, Alt+Backspace or Ctrl+Backspace at a line start below a
  non-empty line ended dbc.

It reproduces on HEAD and has been there since `ecd044b`.

The fix: a column at the line's end counts as the newline, a non-word, so
the step crosses to the start of the last word above. It was committed
separately, before N-278. The staged tree was built from `HEAD:` plus the
one hunk (`git hash-object -w` + `update-index --cacheinfo`), then
checked out with `checkout-index --prefix` and tested on its own, the e2e
suite included.

## Tests

- `tui/framecache_test.go`:
  - `TestEditorCachesFollowEveryEdit`: 40 seeds × 300 random steps (keys,
    `Insert` pieces with CJK/emoji/ZWJ/CRLF/tab/`;` in strings,
    `Backspace`, `Delete`, undo/redo, `Replace`, `ApplyEdits`, `SetText`,
    View/Restore over changed text). It compares `Text`, `offset`,
    `posAt` (round trip, and against the old code on rune starts) and
    `Stmts` for Postgres and MySQL against copies of the uncached code.
  - `TestEditorPosAtMidRuneStaysOnTheLine`.
  - `TestEditorHighlightFollowsTheTheme`.
  - `TestLogWrapCacheMatchesAFullWrap`: 4,000 random steps of adds,
    multi-line adds, bursts past `logMaxLines`, width changes, `clear` and
    `take`.
  - `TestChatRowsCacheMatchesAFreshLayout`: chunks, a code block opening
    and closing, the stream's end, width, dialect, theme, more messages,
    a new conversation.
  - `TestInspectWrapsOncePerWidth`.
- `tui/prints_test.go`:
  - `TestScriptPrintsAreBatched`: 1,500 lines, one wake-up, drained by an
    unrelated key, in order.
  - `TestScriptPrintsBackpressure`.
  - `TestScriptPrintsLandBeforeItsEnd`: a real script of 2,300 lines, the
    log's last 2,000 consecutive and above "completed".
  - `TestBackgroundScriptPrintsSkipThePendingList`, including a closed
    tab.
  - `TestBackgroundScriptShowsKeepTheLast`.
  - `BenchmarkScriptPrints`: `runWatch` wraps the Model in a real
    `tea.Program` with `WithInput(nil)`, `WithOutput(io.Discard)` and
    `WithWindowSize`.
- `tui/frame_test.go`: the frame benchmarks (`bigSQL`, `fillLog`,
  `fillChat`, `frameModel`, `motion`).
- `tui/editor_test.go`: `TestEditorWordLeftCrossesALineBreak` (N-326).
- `tui/harness_test.go`: `newTestModel*` and `drive` take `testing.TB`,
  so benchmarks can build a model. `m.post` is set to the recorder.
- TUI e2e (`tui/e2e/tui_test.go`), two new steps before "Ctrl+Q quits":
  - "Alt+Left at a line start steps back over the line break": dbc exits
    on HEAD.
  - "a script printing 10,000 lines finishes in a moment": a 3 s bound;
    7.2 s on HEAD, 42 ms now.
  - New helper `env.writeScript`.
- Mutation check: breaking each cache key fails its test. Removing the
  text cache's version check, the split's dialect check, `trim`'s row drop,
  the chat key's `chip` or `pal`, or Inspect's width check each made a test
  fail. `take` not clearing the holding log survived, because `wrapped`'s
  shrink guard catches it.
- Full `./...` green, `-race` on tui/workspace/web green, TUI e2e green.

## Found on the way

- **A `tea.Program` whose output is no terminal reports 0×0 when it
  starts.** That replaced the `WindowSizeMsg` the benchmark had sent, so
  every frame was "window too small" and HEAD's 2,000 prints "took"
  12 ms. `tea.WithWindowSize` plus a check of `m.w`/`m.h` fixed the
  measurement. Saved to memory as a pitfall.
- `gofmt -l tui` still flags `tui/app_test.go`. This predates the session
  and was left alone.

## Next

Closed: N-278, N-325, N-326. Declined: None. Raised: N-326 (closed the same
session). Deferred: None. Promoted: None. Moved: None. Updated: N-303 (the
frame and print benchmarks now exist, without budgets). Full list:
`ai_docs/todo/next-list.md`.
