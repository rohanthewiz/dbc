# dbc web: sidebar splitter and fold, and a splitter between every section

Session: `f7047dbb-53aa-4214-899e-c10d0f2c33cd`

## Ask

1. Give the web workbench's left sidebar (Connections) a splitter and a hide
   button, like the left aside in Cats (`../cats`).
2. (Added mid-session.) Put a draggable splitter between every pair of major
   sections.
3. Wrap the session (`/sess-wrap`).

## Reference: how Cats does it

The code is `../cats/cmd/catway/web/`: `js/38-sidebar.js`,
`css/03-splitter.css`, and `#sb-fold` in `css/04-brand.css`.

- A 5px gutter (`#splitter`) sits in a grid column of its own. Dragging it
  sets `--sidebar-w`, which is kept in localStorage and in config.json.
- Dragging past about 55% of the 150px minimum folds the column. The fold
  happens live during the drag, and dragging back out of that zone unfolds it.
- When folded, the gutter widens to 14px and shows a `›` tab
  (`body.sb-hidden #splitter::after`). Clicking the tab brings the column back.
- A `‹` button in the column's brand row folds it (one direction only).
- ⌘B / Ctrl+Alt+B toggles the fold. Double-clicking the gutter resets the
  width, with a guard so the reveal click is not read as a double-click.

## What dbc web does now

The gestures follow Cats. The persistence follows dbc's own convention:
layout keys in `web.bytdb` through `PUT /api/v1/layout`, the same place as
`chatWidth` and `chatOpen`. dbc does not use localStorage for this.

| Bar | Element | Sizes | Layout key |
|---|---|---|---|
| sidebar ‖ work | `#side-split` (new) | `--side-w` on `:root` | `sideWidth`, `sideHidden` |
| Connections ═ Tables | `#side-hsplit` (new) | `--conns-h` on `#side-conns` (+`.sized`) | `connsHeight` |
| editor ═ results | `#splitter` (existing) | `#editor-wrap` height | `editorHeight` |
| results ═ log | `#log-split` (new) | `--log-h` on `.work` | `logHeight` |
| work ‖ assistant | `#chat-split` (existing) | `--chat-w` on `.app` | `chatWidth` |

Every bar now resets to its default size on a double-click. It saves `""`
because the layout has no delete. The editor and chat bars did not have a
reset before this session.

### Sidebar edge and fold

- **Markup** (`web/pages/workbench.go`): `#side-split` is a sibling of the
  `<aside>`, not a child of it. It lives in the same named grid area (`side`),
  so the two overlap in one cell and the edge survives when the aside is
  `display:none`. The `‹` button (`#side-fold`) and `+` are grouped in
  `.hbtns` in the Connections heading row.
- **Server-rendered fold**: `Workbench.SideHidden` comes from
  `layout["sideHidden"] == "1"` in `handlePage`. It renders
  `class="app side-off"` and the matching tooltip (`sideSplitTitle`).
  Rendering it on the server is the same no-flash trick the theme uses.
  `syncSideTitle` in app.js must keep the same wording.
- **CSS** (`app.css`, section "the sidebar edge"):
  - Every grid template uses `var(--side-w, 230px)`.
  - While shown, the edge is a 6px strip straddling the border
    (`justify-self:end; margin-right:-3px; z-index:5`), mirroring `chat-split`.
  - `.app.side-off` sets `--side-w:14px` on `.app` itself. That beats the
    value app.js puts on `:root` without erasing it, so a reveal lands on the
    old width. The folded strip draws the `›` tab with `::after`.
  - At ≤700px the edge and the fold button are hidden and a saved fold is
    ignored, because the sidebar is stacked above the work column there.
- **JS** (`app.js`):
  - `setSideWidth` clamps to between 150 and `innerWidth - 400`.
  - During a drag the fold is a live preview below 82px, the same as Cats.
  - A press on the folded tab that never becomes a drag reveals the column.
  - A drag that ends folded puts `--side-w` back to its value from before the
    drag. Without that, the reveal would land on the narrow widths the drag
    passed through. This was a bug found in browser testing.
  - Ctrl+B / ⌘B toggles the fold. It is VS Code's side-bar key, and no dbc
    binding used it before.
  - Hiding moves focus that was inside the column to the editor.

### Connections | Tables

The sidebar is now a flex column: `#side-conns`, then `#side-hsplit`, then
`.side-tables`. Each half scrolls on its own, and the column no longer does.
By default Connections takes its natural height up to a 45% cap. A drag
replaces the cap with a fixed height. `connsHeight` saved while the column is
folded cannot be measured at boot, so it is held in `pendingConnsH` and
applied on the first reveal.

### Results | log

`#log-split` reuses the `.splitter` class. `.log` is now
`flex: 0 0 var(--log-h, 130px)`.
- `setLogHeight` keeps at least 40px and leaves the results pane 90px.
- `setEditorHeight`'s ceiling now subtracts the log's height.
- At boot the log height is applied before the editor height, because the
  editor's clamp measures against it.

The two row bars share one helper, `dragRows(bar, {start, set, dir, done,
reset})`. The existing editor splitter's handler was left as it was.

## Bug found on the way: Monaco swallowed presses on the row bars

This bug was already there before this session. A real mouse press on the
existing editor splitter, and on the new log bar, landed on Monaco's
`.lines-content`, so no drag began. That element is `position:absolute`,
`contain:strict`, transformed, and 2^24px square. On screen it is clipped by
`.overflow-guard`, but it still wins Chrome's hit test over an unpositioned
5px bar. `elementFromPoint` agreed. The fix is `.splitter { position:relative;
z-index:5 }`. The other z-indexes are menus 50, modal shade 40, and the
floating chat 30, so nothing else is covered.

## Verification

- `go vet ./...` and `go test ./...` pass.
- New test `TestSidebarFoldRendered` (`web/server_test.go`):
  - The page carries the edge, the fold button and both row bars.
  - `sideHidden=1` renders `class="app side-off"` and the reveal tooltip.
  - Clearing `sideHidden` removes `side-off`.
- Browser checks in Chrome against a built binary with `HOME` pointed at the
  scratchpad, so the user's real `web.bytdb` was not touched:
  - Real-mouse drags of all four new or fixed bars resize their panes and
    save `sideWidth`, `connsHeight`, `editorHeight` and `logHeight`.
  - Real clicks on `‹` fold the column, and a real click on the `›` tab
    reveals it at the saved width.
  - A drag into the fold zone folds the column. Ctrl+B toggles it. A reload
    while folded renders folded.
  - Revealing applies the pending Connections height.
  - Double-clicking each bar resets it and clears its key.
- The browser tool's screenshot frame is not CSS pixels. Here a click at
  screenshot (x, y) arrived at CSS (1.333x, 1.333y). A `mousemove` probe
  calibrated this, after the first drags missed.

## Files

- `web/pages/workbench.go`: `SideHidden`, `appClass`, `sideSplitTitle`, the
  sidebar split into `side-conns`, `side-hsplit` and `side-tables`
  (`connsList`), the `#side-split` and `#log-split` bars, the `‹` fold button,
  bar titles, and the layout diagram.
- `web/server.go`: `handlePage` passes `SideHidden`.
- `web/server_test.go`: `TestSidebarFoldRendered`.
- `web/static/css/app.css`: `--side-w` in the grid templates, the sidebar
  edge and fold, the sidebar halves and hsplit, `--log-h`, and `.splitter`
  lifted above Monaco.
- `web/static/js/app.js`: sidebar width, fold and reveal, `dragRows`, the log
  and Connections bars, the editor bar's double-click reset, Ctrl+B, boot
  restore, and the key help (`Sidebar`, `Splitters`).
- `web/static/js/chat.js`: double-click reset for the assistant's bar.
- `README.md`: a **Layout** paragraph in the `dbc web` section.

## Next

Closed: None. Declined: None. Raised: N-061.
Deferred: None. Promoted: None.
Updated: None. Full list: `ai_docs/todo/next-list.md`.
