# TUI: a references list for Shift+F12

Session: `047ee6a8-6174-4fdf-a8a1-f5234249dff6`

## Ask

1. From the cats-todo backlog: N-118. Add a references list for ⇧F12 in the
   TUI, like Monaco's peek. Before this, the uses were underlined, listed by
   line in the log, and ⇧F12 again stepped through them. The ask was a small
   modal list (line + text, Enter jumps).
2. `/sess-wrap`.

## Design

- **Where it sits.** A centred modal would cover the editor, which is the
  thing you want to see. The list places itself over the results pane, right
  under the editor and as wide as it, so the editor stays in view. If the
  results pane is too short (under 5 rows), it falls back to the centre.
- **Preview in the real editor.** Monaco's peek embeds a second editor, and
  a terminal has no room for one. Moving the list cursor selects that use in
  the editor and scrolls to it. It is a selection rather than a caret
  because the editor's caret is not drawn while a modal holds the keyboard.
- **Enter vs Esc.** Enter or a click puts the caret at the use's start with
  no selection left (the old stepping's landing spot), so the next typed key
  can't replace the name. Esc or ✕ means "never mind": it restores caret,
  selection, goal column and scroll exactly as they were (`caretView`).
- **Stepping kept.** ⇧F12 inside the list moves to the next use, wrapping
  round. ⇧F12 with the list closed and the marks still live reopens the list
  one use further on, so pressing ⇧F12 repeatedly still steps through the
  uses. A name used only where it is declared gets the marks and the log
  line, but no list.
- **The marks outlive the list.** Closing the list doesn't clear the
  underlines; Esc in the editor, or any edit, still does.

## What changed

**tui**
- `usages.go` (new) holds the list:
  - `usesModal`. It uses `list` only for cursor, scroll, hover and
    hit-testing, and draws its own rows, since a `listItem` can't underline
    part of its label.
  - Each row shows the line number (muted), the line with leading blanks
    trimmed in the editor's syntax colours (when `hl` is current) with the
    use underlined and bold, and "declared" on the declaration. When a line
    is too long, the row starts 8 runes before the use behind `…`.
  - `caretView` with `saveCaret` / `restoreCaret`. It is a light cousin of
    `editorView`, which also carries text and undo stacks.
  - A `stale` guard on the editor version: if the text ever changed under
    the list, the next key closes it, and `draw` skips rows whose columns no
    longer fit.
- `modals.go`: the new `placedModal` interface (`place(m) (Rect, bool)`) is
  checked first in `modalRect`.
- `symbol.go`:
  - `showUsages` opens the list: on the use under the caret the first time
    (`useAt`), and on the next one when the marks are already up
    (`nextUse`).
  - The `Model.nextUse` method was replaced by pure index helpers, since the
    list does the stepping now.
  - The log line and the header diagram were updated.
- `help.go`: the ⇧F12 row now reads "highlight its uses and list them; Enter
  goes to one". The first, longer wording wrapped onto two lines in the keys
  dialog and pushed "Query tabs" off its first page, which broke the F1 e2e
  step. The description column is about 57 cells at 140×45.

**Docs**: the README's "Go to definition, usages and rename" section and
the keys table describe the list.

## Tests

`tui/symbol_test.go`:
- `TestShiftF12MarksUsesAndStepsThroughThem` (reworked): checks that the
  list opens on the caret's use. It then covers ⇧F12 + Enter landing on the
  next use with no selection, reopening one use on, wrapping, Esc in the
  list keeping the marks, and an edit clearing them.
- `TestUsesListPreviewsAndEscGoesBack`: the preview selects the declaration,
  and Esc restores the exact `caretView`.
- `TestUsesListRowsAndClick`: the list sits at `results.Y` and `editor.X`,
  the declaration row has its line number and "declared", and a click goes
  there.
- `TestShiftF12OneUseOpensNoList`: a CTE declared and never used gets marks
  but no modal.

`tui/e2e`: a new step, "Shift+F12 lists the uses; Enter goes to one",
runs in the real binary in a pty. charmbracelet/x/vt has no encoding for
shifted function keys (its `key.go` knows bare F1–F12 only), so the new
`term.raw` writes xterm's `ESC[24;2~` straight to the pty.

A frame dump during the work confirmed the layout: the editor above, with
the previewed use selected, and the list over the results pane.

`go test ./tui/... ./web/...` passes with `CATS_*` stripped, and
`DBC_TUI_E2E=1 go test ./tui/e2e` passes all 15 steps.

## Next

Closed: N-118. Declined: None. Raised: None. Deferred: None. Promoted: None.
Moved: None. Updated: None. Full list: `ai_docs/todo/next-list.md`.
