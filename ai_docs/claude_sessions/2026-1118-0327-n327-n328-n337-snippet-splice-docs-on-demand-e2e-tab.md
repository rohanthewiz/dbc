# N-327, N-328, N-337: snippet stops splice their rows, completion docs on demand, the e2e tab assumption

Session: `cbde2eba-f688-4850-8468-0b28475b5b48`

## Ask

`/work-next-list`, the thirteenth batch. The triage is in
`2026-1118-0042-n321-n286-n288-n291-plain-errors-value-text-status-error`.

## N-327: a key in a mirrored snippet stop (`f8a04fd`)

**Before.** Each key re-laid out the whole buffer and set it afresh with
`splitLines`. Every row became a new slice, so the text cache, the undo
diff and the highlighter all compared rows rune by rune.

**Now.**

- `spliceMirrors` builds the stretch from the first mirror to the last,
  re-splits only those rows, and splices them into `lines`.
- `relay` (was `rebuild`) counts span offsets, and builds the text only
  for the undo-shape test.
- `mirrorsDiffer` compares the mirrors' values instead of whole texts.

**Measured.** New `BenchmarkKeySnippetStop` (`cnt` expanded mid-way
through 3.5 MB): 11.6 ms, 41 MB and 69k allocs a key became about
2.0 ms, 19 MB and 3.3k. Plain typing is 1.25 ms. What's left is a second
text patch for the mirrors' version.

**Tests.** `TestSnippetMirrorsSplice` covers a letter, a multi-line paste
and undo. Rows outside the snippet keep their slice, which fails on the
old code. The TUI pty e2e snippet step passes.

## N-328: completion docs on demand (`0199a3b`)

**sqlcomplete.**

- A kept item's catalog doc is a `heldDoc` on the result's `docBook`.
- `Item.Documentation()` builds it once, on first read, and copies of
  the item share it. The relationship index is made by the first read.
- `DocPending` says a doc is held.
- `BenchmarkCompleteBig/from-empty`: 2.8 → 2.3 ms, 177 KB of docs → 0,
  27k → 15k allocs.

**TUI.** The popup reads only the focused row's doc.

**dbc web.**

- An answer carries the first 20 items' docs and marks the rest
  `docLater`.
- Monaco's `resolveCompletionItem` sends `docOf {index, label, kind}` on
  the same request. The server re-runs it and reads the item, by index
  or else by label and kind; `conn-complete` does the same.
- The request in flight is kept on the row, and a fetched doc is kept
  even if Monaco cancelled.

**Tests.**

- `TestDocsHeldBack`, with a `-race` read.
- `TestCompleteDocsOnDemand`.
- A new web e2e step, "completion docs on demand": zz25's DDL appears in
  the details pane. The details pane is an overlay outside
  `.suggest-widget`, and the DDL is schema-qualified (`main.zz25 (`).
  The step fails with the old `editor.js`.

## N-337: the stale-completion e2e step's tab (`3238920`)

The failure kept coming back: 4 of 5 full runs failed once the new steps
were added. Logging the active tab after every step showed why. From the
query designer step on, `p` sat on `e2e_pipe.go`: that step closes its
own query tab, and its neighbour, the pipeline step's Go script, became
active. The stale-completion step then typed SQL into a Go editor.

The fix is in the test: the step now opens a query tab of its own on
`lite`, as `queryDesigner` does. Two full web e2e runs passed after it.
Nothing showed the page switching tabs by itself.

## Checks

For all three: the full `go test ./...`, `go vet`, and staticcheck
2026.2.2 are clean, and `-race` passes on sqlcomplete and web. The TUI
e2e and two full web e2e runs pass.

## Next

Closed: N-327, N-328, N-337. Declined: None. Raised: None. Deferred:
None. Promoted: None. Moved: None. Updated: None. Full list:
`ai_docs/todo/next-list.md`.
