# Schema filter for `dbc web`'s tables list

Session: `56ef26e7-43c5-43b0-a15f-9ccb5a41dea4`

## Ask

"On the web app under tables I need to be able to filter by schema
otherwise very large dbs will overwhelm the tables list."

Follow-up, after a first cut with a native `<select>`: "The dropdown is not
scrolling and yes add the filter box so I can type the schema to select."

## What was built

A type-to-filter schema box under the sidebar's **Tables** heading. Only
the page changed; the server already sent each table's `schema`
(`web/hub.go` `tabRef`).

- **When it shows:** only when the connection's catalog spans more than one
  schema. A single-schema catalog (SQLite, bytdb, a Postgres with only
  `public`) has nothing to narrow, so the box is hidden.
- **The list:** focusing the box opens a list of the schemas, each with its
  table count. "all schemas (N)" leads it while nothing is typed.
- **Typing:** matching ignores case. Names starting with the text come
  first, then names containing it, each group by name (`sa` lists `sales`
  above `analytics_sandbox`). With text typed, "all schemas" is dropped so
  Enter takes the first match.
- **Keys:**
  - ↑/↓ wrap, PgUp/PgDn move 10. The highlight is kept in view.
  - Enter picks. A second Enter, with the list closed, selects the first
    table.
  - Esc drops the typing and keeps the pick; a second Esc leaves the box.
  - The search input's own × (the `search` event) shows every schema.
- **Mouse:** a click on a row picks. A mousedown on the list is
  `preventDefault`ed so the box keeps focus and blur doesn't close the list
  under the click. A click in the box after a pick reopens the list.
- **The tables list while narrowed:**
  - The heading reads `· 12 / 340`. The box gets an accent border (`.on`).
  - Rows drop the `schema.` prefix. `data-name` keeps the qualified name,
    which previews, inserts, copies and the ERD use.
- **Remembered per connection:** saved in the layout as
  `tableSchema.<conn>`. The value is `=<schema>`, or `""` for every schema;
  the `=` keeps a driver's empty schema apart from "all".
  - Loaded at boot, before the first list is drawn.
  - A schema the catalog no longer has falls back to every schema rather
    than an empty list.
  - A connection rename moves the pick and blanks the old key (the layout
    has no delete).
- **Row counts:** `showCounts` now also merges the counts into `allTables`
  (the whole catalog kept by `showTables`). Counts that land while a schema
  is filtered out are there when the filter changes.

### Why a combobox, not a `<select>`

The first cut used a native `<select>`. Its popup is drawn by the browser
or by the Mac app's WKWebView, not the page. The user found it didn't
scroll, and a select only jumps by typed prefix. The list is now page-drawn
(`ul#schema-list`, `role=listbox`, `aria-activedescendant`). It sits in the
column's flow rather than floating over it, so `.side-tables`' overflow
can't clip it. It scrolls on its own past `33vh`.

### Layout fixes

- **Pinned heading and filter:** `.side-tables` is now a flex column with
  `overflow: hidden`, and `#tables` is the scroller. The heading and the
  filter stay put while a long table list scrolls.
- **Connections squeezed to nothing (pre-existing):** `.side-tables` had
  `flex: 1 1 auto`, so its basis was its content's height. With ~300 tables
  it shrank `.side-conns` (`flex: 0 1 auto`) to 17 px, hiding the
  Connections list. It is now `flex: 1 1 0` and takes only the room
  Connections leaves (88 px for two connections in the check).

## Files

- `web/pages/workbench.go`: `div.tfilter#table-filter`, holding
  `input#table-schema` (type=search, role=combobox) and
  `ul#schema-list`.
- `web/static/js/app.js`:
  - `showTables` now just stores the catalog and calls `drawSchemaFilter`
    and `drawTables`.
  - The schema filter section: `drawSchemaFilter`, `showPick`,
    `chooseSchema`, `openSchemaList`, `closeSchemaList`, `markSchemaHi`, and
    the key/mouse handlers.
  - Boot reads `tableSchema.*` from the layout. `connRenamed` moves the
    pick.
- `web/static/css/app.css`: `.tfilter`, `.schema-list`, and the
  `.side-tables` column changes.
- `README.md`: a paragraph after the row-counts one.

## Verification

- `go test ./web/ ./` and `go vet ./web/...` are clean.
- **Browser:** a throwaway go-rod harness (scratchpad, not committed; see
  N-051). It ran a fresh build against a docker Postgres 17.
  - **First cut (3 schemas):** filtering, the pick kept after a reload,
    SQLite hiding the filter, the pick restored on switching back, and
    counts landing while filtered.
  - **Combobox (154 schemas, 307 tables):**
    - Focus opens 155 rows. 40 ↓ scrolls the list (`scrollTop` 607).
    - `sa` → `sales`, `analytics_sandbox`. Enter picks `sales` (`· 2 / 307`),
      and a second Enter focuses `sales.items`.
    - `s14` + a click on the third match picks `s142`, and a reload keeps it.
    - Emptying the box highlights "all schemas", and Enter restores all.
    - Scrolling `#tables` leaves the filter in place.
    - Esc Esc leaves the box. Connections keeps its height.
  - Screenshots looked right.
- **Not exercised:** a click on the search box's ×. Safari and the Mac app's
  WKWebView (added to N-064).

## Next

Closed: None. Declined: None. Raised: N-076, N-077.
Deferred: None. Promoted: None.
Updated: N-051, N-064. Full list: `ai_docs/todo/next-list.md`.
