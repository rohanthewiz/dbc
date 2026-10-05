# Caret, scroll and undo per console (N-103)

Session: `15ffc543-d3c3-4296-aff5-f7e23699a060`

## Ask

From the cats-todo backlog, next-list item N-103: remember the caret and
scroll per console across switches. The TUI's `SetText` put the caret at the
top on every swap, and `dbc web` dropped a console's document (and its undo
history) once no tab of the window showed it.

## TUI: park and restore the editor's view

`tui/editor.go`:

- `editorView` is everything `SetText` throws away besides the text: caret,
  anchor, selection flag, `top`/`left`/`goal`, and the undo/redo stacks, plus
  the text they belong to.
- `View()` captures it. The undo stacks are handed over, not copied. The
  editor gets another buffer right after, and no edit writes into a snapshot
  (`swap` moves them whole), so nothing changes them while they are parked.
- `Restore(v, text)` rebuilds the lines from `v.text`, clamps the caret,
  anchor and `top`, and restores the stacks. If the file's text (normalized)
  differs from `v.text`, because another writer saved meanwhile, it applies
  the new text as ONE undo step with `Replace`. Then it puts the caret back on
  its row and column, clamped, rather than at the end where `Replace` leaves
  it. This follows `dbc web`'s rule: Ctrl+Z brings back the version you left.

`tui/console.go`, `openInEditor`: before `setConsole`, the console being
left is parked in `Model.consoleViews` (a map keyed by console file path,
for this run only). After loading the new console's file, a parked view is
restored (and removed from the map). Otherwise it falls back to `SetText` as
before. Re-picking the console that is already open from the editor menu now
keeps the caret too (it used to reset).

Test: `TestConsoleKeepsCaretAndUndoAcrossSwaps`. It covers the caret and
`top` surviving a switch from a to b and back, and undo still reaching the
empty buffer. It then saves over a's file while b is open, and checks that
switching back shows the new text with the old caret clamped, and that undo
returns the text that was left.

## dbc web: keep console documents for the page's life

`web/static/js/app.js`: a new `leaveDoc(k)` replaces the two places that
dropped a document once no tab showed it (`showConsole` leaving the old
console, `reallyClose`). It saves the console, then drops the document only
when it is not a console's (`!cons.has(k)`), which means a tab's own
pre-console buffer. A console's Monaco model stays, so `useDoc` restores its
view state (caret, scroll) and the model keeps its undo history.

Why a hidden document stays correct: `onConsoleSaved` → `adopt` →
`replaceDoc` already works on any document, shown or not. A save from it that
meets a newer file goes through the existing conflict path. The cost is one
model per console opened, which is small.

Deleted consoles: `onConsolesChanged` used to drop a deleted console's
document only after moving the tabs that showed it. A kept document that no
tab shows would never have been dropped, so it is now dropped straight away
when no tab shows it.

`web/static/js/editor.js`: the comments on documents and `dropDoc` were
updated to describe this.

e2e (`web/e2e/web_test.go`, `consolesPerDatabase`): after switching from
lite to lite2 and back, the step waits for the caret to sit after
`SELECT 'on lite'` and for `model.canUndo()`. I checked that it fails when
`leaveDoc` always drops (it timed out at "lite's caret and undo kept"), then
put the fix back.

## Verification

- `go test ./...` passed (CATS_* stripped).
- `cd web/e2e && DBC_E2E=1 go test -count=1 .` passed (~15 s).

README: both console sections now say a console comes back with its caret,
scroll and undo history, and that the TUI applies a file changed meanwhile
as one undoable edit.

## Next

Closed: N-103. Declined: None. Raised: None.
Deferred: None. Promoted: None.
Updated: None. Full list: `ai_docs/todo/next-list.md`.
