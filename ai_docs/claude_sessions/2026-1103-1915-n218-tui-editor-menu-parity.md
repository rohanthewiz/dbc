# N-218: TUI editor menu parity with dbc web (Stop, Paste, Redo)

Session: `40e2ec30-e2df-41e3-9243-6227b92b4475`

## Ask

From the cats-todo backlog, next-list item N-218: dbc web's editor menu
(`app.js editorMenu`) has three rows the TUI's (`tui/menu.go
openEditorMenu`) lacks: **■ Stop** (dim with nothing running), **Paste**
and **Redo**. Its Undo/Redo also dim when there is nothing to undo. Port
them to the TUI.

## What changed

- **■ Stop** (^K), after Run all as in dbc web's query tab, runs
  `cancelRun`. It is dim with "nothing is running or connecting" unless
  `ws.Busy()` or `ws.Connecting()` is true, the two things
  `Workspace.Cancel` stops. So the row is live in more cases than the
  toolbar's Stop button, which follows `Busy` alone.
- **Redo** (⌥Z, the key the TUI already had) follows Undo. Both dim on an
  empty stack: new `editor.CanUndo` / `CanRedo`.
- **Paste** has no terminal paste key behind it (a terminal paste arrives
  as `tea.PasteMsg`), so the row reads the clipboard itself:
  - new `clip.Read`, atotto's `ReadAll` (already a dependency), behind a
    `readText` var for tests;
  - `tui/paste.go`: `menuPaste` reads on a command goroutine and returns
    `clipReadMsg{ed, text, err}`. On error (SSH, a container) it logs that
    it is asking the terminal, then sends `tea.ReadClipboard` (OSC 52). Many
    terminals refuse that read by never answering, which is why it logs
    first: otherwise the click would look dropped (menu grammar);
  - the text goes to the editor the menu was opened on (`clipReadMsg.ed`,
    and `Model.pasteInto` across the OSC 52 round trip), not whichever is
    current when it lands. An unasked-for `tea.ClipboardMsg` is ignored. An
    empty clipboard is logged.
- The `Model.pasteInto` field sits in its own block, so gofmt does not
  realign the neighbouring fields.
- README's TUI mouse table row for the editor lists the new rows.

## Files

- `clip/clip.go`: `Read`, `readText`.
- `tui/paste.go` (new): the Paste flow, with a diagram in the doc comment.
- `tui/menu.go`: the four rows, plus the why's for Stop, Undo and Redo.
- `tui/editor.go`: `CanUndo`, `CanRedo`.
- `tui/app.go`: `pasteInto`, routes for `clipReadMsg` and
  `tea.ClipboardMsg`.
- `tui/harness_test.go`: `clipReader` is stubbed to fail, so no test reads
  the real clipboard.
- `README.md`, `ai_docs/todo/next-list.md`.

## Tests

- `tui/paste_test.go`:
  - `TestEditorMenuDimsIdleRows`: the why's, Paste always live, and a dim
    Stop logs its why;
  - `TestEditorMenuUndoRedo`;
  - `TestEditorMenuPasteLocal`: replaces the selection, and an empty
    clipboard is logged;
  - `TestEditorMenuPasteViaTerminal`: the reply lands in the asking tab
    after a tab switch, and a stray reply is ignored.
- `clip.TestRead`.
- `go test ./...` green (CATS_* stripped).
- Not checked in a real terminal; raised as N-246.

## Next

Closed: N-218. Declined: None. Raised: N-246. Deferred: None.
Promoted: None. Moved: None. Updated: None.
Full list: `ai_docs/todo/next-list.md`.
