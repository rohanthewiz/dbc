# TUI Tables pane: `/` type-to-find

Session: `c931e0f3-128e-4775-8a3a-f842ee28b0a0`

## Ask

1. From the cats-todo backlog: N-141. The TUI's Tables pane had no
   type-to-find, while dbc web's sidebar has a table box under the
   db/schema pickers. The pane's letters are already keys
   (`j k g G c e d s f #`), so the request was a `/` that opens a filter line
   over the list, filtering the way the pickers' `pickModal` does.
2. `/sess-wrap`.

## What was built

`tui/tablefind.go` (new):

```
┌ Tables · 3 of 40 ─────┐
│ ◫ sales · 40        ▾ │   navigator rows, as before
│ ⌕ ord▏                │   the filter line, while open or holding text
│ orders                │
│ order_items           │   rows whose name holds "ord", any case
│ backorders            │
```

- **Opening the line:** `/` in the pane, or the menu's "Find a table…" /
  "Find a routine…" (`openTableFind`).
- **While typing:** letters are text, and `?` no longer opens help
  (`findEditing` gates both `app.go` paths). ↑↓, PgUp/PgDn and Ctrl+N move
  the cursor. Enter previews the row (a routine's DDL when routines are
  listed) and keeps the filter. Esc clears it.
- **Off the line:** `/` or a click on the line (`lay.findRow`) goes back to
  typing, and Esc clears a filter that is still applied.
- **Esc keeps the cursor** on the row it was on (`clearTableFind` →
  `cursorToKey`, by `itemKey`), so a find followed by Esc jumps to the table
  in the full list.
- **Paste** goes into the line, with newlines stripped.
- **Title:** `Tables · 3 of 40` and `Routines · 3 of 12`
  (`tablesTitleCount`). With nothing matching, the list says "no match".

## Decisions

- **The filter sits between the catalog and the list**, not inside `list`.
  `fillTables` and `fillRoutines` now call `setTableRows`, which keeps every
  row in `tableFind.all` and sets only the matches. A keystroke re-narrows
  without re-reading the catalog. Row counts landing and relists after DDL
  pass through the filter without special cases, and `relistTables` keeps
  the cursor on its row inside the filtered list.
- **Per tab, nil until first use.** `tfind` sits beside `tables` on
  `Model`/`queryTab` and is swapped in `park`/`load`. A tab that never uses
  find carries nothing.
- **Typing ends when focus leaves the pane**, settled in `drawTablesPane`
  (every frame passes there) instead of at each of the many places that set
  `m.focus`. Coming back to the pane finds its letters as keys again, with
  the filter still applied.
- **A new catalog drops the filter** (`refreshTables`: connect, database or
  schema pick, disconnect). Its text was a fragment of the old names. A line
  still being typed in stays open and empty. A relist of the same catalog
  keeps the filter.
- **Matching is substring, case-insensitive**, as `pickModal`'s.
- **"Diagram all tables"** checks the unfiltered total (`tableRowsTotal`),
  so a filter with no matches doesn't disable it.
- **Ctrl+P** stays the global history key even while typing in the line;
  ↑ or Ctrl+N move the cursor instead.

## Files

- `tui/tablefind.go` (new), `tui/tablefind_test.go` (new):
  `TestTablesFind`, `TestTablesFindEdges`, `TestTablesFindAcrossCatalogs`.
- `tui/app.go`: `tfind` field, key routing (`/`, Esc, editing), the `?`
  gate, paste.
- `tui/tabs.go`: `queryTab.tfind`, park/load.
- `tui/navigator.go`: `drawTablesPane` draws the line, returns its caret,
  and shows "no match".
- `tui/layout.go`: `lay.findRow`, the caret passed out of the Tables pane,
  and the title count.
- `tui/sidebar.go`: `setTableRows` in `fillTables`, and the filter dropped
  in `refreshTables`.
- `tui/routines.go`: `setTableRows` in `fillRoutines`, the routines title
  count, and the menu item.
- `tui/menu.go`: the menu item, and the unfiltered "no tables" check.
- `tui/mouse.go`: a click on the line resumes typing.
- `tui/help.go`: the `/` row.
- `README.md`: a paragraph after the terminal's navigator rows.

## Verification

- `go build ./...`, `go vet ./tui`, and the full `go test ./...` with
  `CATS_*` stripped: all pass.
- A frame rendered in a throwaway test showed the line and the
  `Tables · 2 of 4` title as drawn above. The real binary was not driven by
  hand.

## Next

Closed: N-141. Declined: None. Raised: None.
Deferred: None. Promoted: None. Moved: None.
Updated: None. Full list: `ai_docs/todo/next-list.md`.
