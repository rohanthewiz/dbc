# dbc web: Tab from the schema box lands on the tables, and typing finds one

Session: `9fdc5d52-a6c4-4fbd-8e9e-cd0c2d15f86e`

## Ask

From the user's cats-todo backlog:

> After db is selected and schema is selected with a TAB, the focus should
> land on the list of tables. There I should just be able to type to
> search / select the matching table

This is dbc web's sidebar: the db and schema boxes are the page's own
comboboxes (`combo()` in `web/static/js/app.js`), and only they take Tab as
"complete to the match". The TUI's pickers are modals with no Tab
behavior, and its `s` key already leaves focus on the tables.

## What it did before

- **Tab in the schema box** picked the typed-to schema but kept focus in
  the box. A second Tab moved on, but not to the tables: the rows are
  `tabindex=-1`, so focus skipped past the list.
- **Tab with nothing typed but the highlight arrowed** to another schema
  was left to the browser. The blur then put the box back on the old pick,
  so the arrowed-to schema was lost.
- **The tables list could not be searched.** Its rows had `Enter`, `c`
  (show columns), `e` (ERD) and the arrows.

## Design

The search is a third box, **table**, under db and schema (`#find-filter` /
`#table-find`), with the same `.tfilter` style. Typing straight on the
rows was rejected because `c` and `e` are row keys: no table starting
with either could be typed. Moving those keys would break the shortcuts
the web shares with the TUI.

```
[db box] ─Tab─► (connect lands) ─► [schema box] ─Tab/Enter─► [table box] ─type─► list narrowed, 1st selected
                                                                   │ ↑↓ PgUp/PgDn ─► selection moves, keyboard stays
                                                                   │ Enter ─► preview the selected
                                                                   │ Esc ─► clear find; again ─► leave box
                                                                   └ Tab ─► the selected row (c, e, Enter)
[a row] ─ letter/digit/_ $ . - (not c, e) ─► new find with it
        ─ / ─► the box, find kept
```

- **Matching** (`findTables`) is the pickers' rule: names that start with
  the text come first, then names that only contain it. It matches the
  unquoted `name`, not `qname`, so a quoted `"Orders"` still matches `ord`.
  Text containing a dot matches `schema.name`. Without a dot the schema is
  ignored; otherwise `ain` would match every table in SQLite's `main`.
- **The list is narrowed in place** (`drawTables` → `inPick` → `shown`).
  The heading's `· n / total` still counts the schema's tables; the find
  box's accent border (`.tfilter.on`) marks the find narrowing. When
  nothing matches, the list says "no table matches “x” — Esc clears".
- **Roving tabindex** (`selectRow`): the selected row, or else the first,
  is the list's one Tab stop. Tab from the box lands on it, and Shift+Tab
  goes back. `pickTable` is now `selectRow(li, true)`.
- **Combo Tab** (`combo()`): Tab picks when something is typed *or* the
  arrows moved the highlight (a new `moved` flag, reset in `open`). It then
  calls the new `o.advance()`. If the highlight is still on the current
  pick and nothing is typed, Tab is left to the browser, which now reaches
  the table box in DOM order.
  - **Schema combo**: `enter` and `advance` are both `focusTables`.
  - **DB combo**: if the pick is the database already on, `advance` moves
    on at once. Otherwise it records `awaiting`, and `dbPicker.landed()`
    (called at the end of `showSide`) moves on once that connection's
    sidebar is drawn, provided the keyboard is still in the db box. A
    failed connect drops the move. It doesn't move at once because, until
    the connect lands, the boxes below belong to the old database: the
    redraw would wipe what was typed there, or hide the box the keyboard
    is in.
- **Find text lifetime**:
  - `drawFindBox` clears the find on every sidebar redraw except while the
    box has focus. A schema picked with Tab puts the keyboard there before
    the tables land, so anything typed meanwhile applies when they arrive.
  - `chooseSchema` clears it whenever the pick changes.
- **Forwarding from rows** puts the typed character in by hand after
  `focus()`, rather than relying on the keypress following the focus
  change. That behavior is up to the browser, and WKWebView (`dbc.app`)
  may differ. The box's focus listener `select()`s, and setting `value`
  afterwards replaces the selection with the caret at the end.
- **Not "search"**: the box listens to `input` only. A search box also
  fires `search` on Enter, and that redraw would reset the selection to
  the first row just as Enter previews another.

## Bug the e2e caught

The first run of the new Postgres check failed on "arrow to e2e_b + Tab".
The find `al`, typed earlier against e2e_a, survived: the box had focus
when e2e_b's tables landed, so `drawFindBox` kept it, and beta was
filtered out. The fix is in `chooseSchema`: another schema means another
list, so the find is dropped there (`if (name !== schemaPick())`).

## Files

- `web/static/js/app.js`:
  - the `els` refs and the sidebar diagram
  - `showSide` calls `drawFindBox` and `dbPicker.landed()`
  - `drawTables` splits into `inPick` and `shown`, and sets the first row
    as the Tab stop (selected while a find is on)
  - the new "finding a table" section (`findTables`, `drawFindBox`,
    `focusTables`, `refind`, the box's listeners)
  - combo Tab and advance; the db combo's `moveOn`, `chosen`, `awaiting`
    and `landed`
  - `selectRow`, and the rows' `/` key and character forwarding
  - the F1 keys list
- `web/pages/workbench.go`: the `find-filter` row, its doc comment, the
  diagram, and the db/schema box titles (which now mention Tab).
- `README.md`: the Tab behavior in the schema-filter paragraph, a paragraph
  on the table box, and the db bullet.
- `web/e2e/web_test.go`:
  - **new step "find a table by typing"** (SQLite, runs without Postgres):
    `z` on a row starts a find with no match, Esc restores, `ats` finds
    cats, Enter previews it (3 rows), Tab reaches the row (tabIndex 0),
    `/` returns to the box, and Esc twice leaves it.
  - **`pgSchemaPicker`**: Tab lands in the table box with e2e_a.alpha
    selected; `al` + Enter previews it (0 rows); arrow + Tab picks e2e_b;
    Tab in the db box to `postgres` lands in the table box (one schema),
    and back to `dbc` lands in the schema box.

## Verification

- `go test ./...` passed (CATS_* stripped).
- `cd web/e2e && DBC_E2E=1 DBC_LIVE_PG_DSN=… go test -count=1 -v .` passed
  all 25 steps against a throwaway `postgres:17` container (port 55432,
  stopped afterwards).
- Untested: WKWebView (`dbc.app`). The checks were added to N-064.

## Notes

- Another session committed `c36bc07` (sqlsplit trailing comment) while
  this one ran. This session touched only the four files above, plus this
  doc and the next list.
- The cats-todo item `dad39a2add25c234` was already marked done when it
  was dropped into the session.

## Next

Closed: None. Declined: None. Raised: N-141.
Deferred: None. Promoted: None.
Updated: N-064. Full list: `ai_docs/todo/next-list.md`.
