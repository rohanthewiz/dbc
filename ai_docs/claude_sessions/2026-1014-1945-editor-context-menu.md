# dbc web: a right-click menu on the query console, Run first

Session: `ab978b8a-7a55-4957-9003-652b1032b794`

## Ask

From the cats-todo backlog: "On a query console give me a context menu. To
begin with, give me an item to run the current query. If there any other
useful options add them."

## Findings

- **The TUI already had this menu.** `tui/menu.go openEditorMenu` offers Run,
  Run all, Explain and explain analyze, the clipboard rows, history, suggest,
  ask, the symbol rows (`symbolMenuItems`) and the consoles
  (`consoleMenuItems`). The gap was in dbc web.
- **dbc web used Monaco's built-in menu.** It had Monaco's defaults plus two
  dbc actions (`dbc.ask`/`dbc.askScript`, `dbc.goToUsages`), and no Run.
  Monaco's menu has two limits. It cannot name what Run would run, and it
  hides a row whose precondition fails, where the project's menu grammar
  (`ui.js`, cats-todo's) dims the row and says why.
- **Monaco with `contextmenu: false`.** Checked in the vendored
  `editor.main.js` (`ContextMenuController._onContextMenu`): it still focuses
  the editor and moves the caret to the click unless the click is inside the
  selection. It does **not** `preventDefault` the browser's menu. Its
  Shift+F10 action (`showContextMenu`) does nothing.
- **What Run runs is the server's call.** `workspace.Pick` and `PickAll`
  name it ("statement 2/4", "selection (3 statements)", "all 4 statements").
  `/api/v1/stmt` only returns a range, so the menu needed a new endpoint.
  Copying the splitter into JavaScript is what handleStmt's comment warns
  against.

## Changes

- **`web/grid.go` `handlePicks`** (`POST /api/v1/picks`, route in
  `web/server.go`). It takes the run request's shape (buffer, UTF-16 caret,
  selection) and answers `picksResp{tag, n, allTag, all, same}`. It needs no
  tab and no connection. A buffer over `maxBuffer` answers "nothing", as
  `/stmt` does.
- **`web/static/js/editor.js`**
  - The main editor now sets `contextmenu: false`.
  - New `bindMenu`, wired from `start()`, handles a right-click through
    `ed.onContextMenu`. It skips content and overlay widgets (the find box
    and the suggestions keep the browser's menu), re-applies Monaco's caret
    rule and `preventDefault`s. Shift+F10 and the ContextMenu key open the
    menu at the caret.
  - New API: `onMenu(fn)` and `trigger(id)`; `cut()`, which writes through
    `dbc.clip.copy` and deletes the captured range only once the write
    succeeds; `paste()` (`navigator.clipboard.readText`, logging "paste with
    Ctrl+V" when refused); `undoable()`.
  - The `dbc.ask`/`goToUsages` actions are kept as commands. Their comment
    now says the menu rows live in `editorMenu`.
  - The mini editors (the pipeline inspector) keep Monaco's menu.
- **`web/static/js/app.js` `editorMenu`** is registered with
  `dbc.editor.onMenu`.
  - It fetches the picks and `refreshNames(t.cdb)` in parallel before
    opening. A newer right-click, or a switch to another tab, drops a menu
    still waiting (`menuSeq`).
  - **Query tab:** ▶ Run `<tag>` · ▶ Run `<allTag>`. Run all is dim for an
    empty buffer, for a single statement, or when the selection already
    covers every statement, as in the TUI. Then ■ Stop (dim when `#stop` is
    disabled) and ◈ Explain / explain analyze. Next come Cut, Copy (dim with
    no selection), Paste, Select all, and Undo and Redo (dim when there is
    nothing to undo or redo). Then History…, Suggest… and ✦ Ask; Go to
    definition, Go to usages and Rename… (Monaco reports "nothing found" in
    place itself); Command palette…; and `consoleItems(t)`.
  - **Script tab:** Save and run, Stop, Save, Check, the edit rows, Suggest,
    Ask about this script, the symbol rows and the palette.
  - **Pipeline and job tabs (JSON):** Save and run, Stop (never dim: `#stop`
    does not track their runs), Save, Check, the edit rows and the palette.
  - The F1 key list gains a row: "right-click · Shift+F10".
- **README:** a row in the dbc web keys table describes the menu.

## Verification

- `go test ./web ./workspace` pass. The new `TestPicks` covers a UTF-16
  caret past an emoji, a selection, a selection of the whole buffer
  (`same`), a single statement, and a comment-only buffer.
- New e2e step "editor menu" (`web/e2e/editormenu_test.go`, registered after
  "run a query"):
  - A right-click on line 2 moves the caret there. The rows read "▶ Run
    statement 2/2", "▶ Run all 2 statements"; Cut and Copy are dim; History,
    Go to definition and + New console are present. Monaco's own menu never
    opens.
  - Picking Run returns the count's 1 row.
  - With one statement, Run all is dim, and picking it logs "the buffer
    holds one statement".
  - A right-click inside a selection keeps it ("Run selection"). Copy puts
    "SELECT 1" on the clipboard, and Paste inserts the clipboard at the
    caret.
  - Shift+F10 opens the menu, and Esc returns focus to the editor.
- The full e2e suite passes (`DBC_E2E=1 go test -C web/e2e .`, ~57 s).
  `gofmt` and `go vet ./web` are clean.

## Next

Closed: None. Declined: None. Raised: N-217, N-218.
Deferred: None. Promoted: None. Moved: None.
Updated: None. Full list: `ai_docs/todo/next-list.md`.
