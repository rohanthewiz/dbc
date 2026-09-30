# Entity-relationship diagrams as PNG, JPEG and Mermaid

Session: `18f854e0-f81a-48dc-b4ed-01bb97160696`
Date: 2026-09-29 → 30

## Ask

From the user's cats-todo backlog: "I would like to be able to create an ERD
diagram for my database. Exportable as JPEG, PNG, or Mermaid diagram."
Then: wrap the session with `/sw` when done.

## Approach

It follows the explain plan's sharing work (`2026-0925-1902`). The pictures
are drawn in Go, so the shell, the TUI and `dbc web` share one tested
renderer and no browser is needed. Mermaid is generated as text.

```
db.Manager.Schema ─► *erd.Schema ─┬─ Mermaid()        erDiagram source
 (3 catalog queries,  Select(...)  ├─ PNG / JPEG(opt)  erd.Picture
  uncapped)           narrows it   └─ FileName / WriteFile
```

### Reading the schema (`db/schema.go`)

- Three queries per engine, run on the pool through `stringRows`, **not**
  `Run`: the `max_rows` cap would silently cut a big catalog. There is a
  250k-row guard (`maxSchemaRows`) instead.
  - `TablesQuery` gives the sidebar's list, so the labels match.
  - `SchemaColumnsQuery` gives schema · table · column · type · nullable ·
    position.
  - `SchemaKeysQuery` gives one row per key column: schema, table,
    constraint, kind `p|u|f`, position, column, ref_schema, ref_table,
    ref_column.
- **Postgres/bytdb** read `pg_constraint` with `conkey::text` / `confkey`
  as `{1,2}` attnum arrays. `expandPgKeys` maps those to column names in
  Go, using the column query's attnums. A key naming an unknown attnum is
  skipped whole. bytdb has no `referential_constraints`, and probing showed
  its `pg_constraint` (v0.18.0) is complete: p/u/f, composite keys,
  self-references, `confrelid`.
- **MySQL** joins `key_column_usage` to `table_constraints`.
- **SQLite** reads `pragma_foreign_key_list`, `pragma_table_info` (pk
  position) and `pragma_index_list`/`_info` for unique indexes. A
  `REFERENCES parent` with no column reports a NULL `to`, which
  `BuildSchema` resolves to the parent's PK.
- `BuildSchema` is pure and tested without a database. It falls back to
  the table name alone when MySQL's `table_schema` case differs. It drops
  foreign keys to tables outside the catalog.

### The model (`erd/erd.go`)

- `Schema{Conn, Driver, Tables, Rels}`, where `Table` has
  `Label, View, Cols, PK, Uniques` and `Rel` has
  `Child, Parent, ChildCols, ParentCols`.
- A `Rel` derives its notation:
  - `Optional` (any key column nullable) → the parent end is zero-or-one.
  - `OneToOne` (key cols = the child's PK or a unique key, as a set) → the
    child end is zero-or-one.
  - `Identifying` (all key cols in the child's PK) → Mermaid `--` vs `..`.
- `MarkKeys` sets the column flags. A PK column is forced NOT NULL, since
  SQLite reports an INTEGER PRIMARY KEY as nullable.
- `Find` uses the TableIndex.Lookup rule (exact case wins, ambiguity
  refused). `Around(names, depth)` is an undirected BFS; negative depth is
  unlimited. `Select(Selection{Tables, Depth, Views})`: views are off by
  default, but a view named explicitly is kept.

### Mermaid (`erd/mermaid.go`)

- Relationships are written parent-first: `owners |o..o{ cats : "owner_id"`.
- An entity id is the label when it is a plain word. Otherwise it is
  `tN["label"]` (aliases need Mermaid ≥ 10.5), and ids never collide with a
  real table's name.
- Attribute words keep `[A-Za-z0-9_()[\]-]`; other runs become `_`, and the
  original goes in the comment (`numeric(10_2) c "numeric(10,2)"`). A
  name's own underscores are kept.
- A test regex checks that every output line is a statement shape the
  grammar takes.

### The picture (`erd/layout.go`, `erd/picture.go`, `erd/paint.go`)

- **Layout**: connected groups (union-find), largest first. Each group is a
  lite Sugiyama:
  1. Longest-path ranks with parents on the left. Cycles are cut at DFS
     back edges.
  2. Each root is pulled right to one rank before its nearest child, so a
     lookup table doesn't stretch across the picture.
  3. Eight barycenter sweeps order each rank.
  4. Ranks taller than √(group area)·0.9 wrap into several columns.
  5. Columns are centred vertically.

  Tables with no relationships at all are masonry-packed under a "TABLES
  WITHOUT RELATIONSHIPS" caption. Past 40 columns a box keeps its key
  columns, fills with others, and ends with "… n more columns". Names get
  the slack; types are right-aligned.
- **Drawing**: title plus a legend of the markers and badges. Lines are
  drawn first, then boxes, so a long line passes under a box rather than
  through its text.
  - A line runs from the child's first key-column row to the parent's
    referenced row.
  - Sides depend on position. When the boxes overlap in x (same column or a
    self-reference) the line loops out on the right.
  - Each end has a 26px straight stub, and the middle is a horizontal-
    tangent cubic.
  - The markers are IE crow's feet: `||`, `|o`, and `>o` with its ring.
  - Badges: PK/PF gold (warn), FK accent, UK muted.
- **Painter**: its own copy of the explain painter's primitives (N-069 says
  why), plus a general `polyline`.
  - One same-winding quad per segment, so a U-turn self-loop can't cancel
    itself out.
  - Interior joints are extended half a width.
  - Strokes are rasterized in chunks of 8 segments. One bounding box per
    line made a 400-table star take 5.5s; chunked it takes 1.2s at the
    36M-pixel cap.
- The pixel caps (36M, 20k a side) match explain. The +1 in the scale
  formula keeps `ceil()` from rounding past the cap.

### Surfaces

| Where | What |
|---|---|
| `workspace/diagram.go` | `Diagram(ctx, sel)`: the active connection, `DiagramTimeout` 30s, on the pool, not the run slot. An unknown table is a `Refusal{Invalid}`. |
| CLI `erdcmd.go` | `dbc erd [-t mermaid\|markdown\|png\|jpeg] [-o f] [--table T]… [--depth N] [--views] [--open] [--theme light\|dark]`. `-t text` (the global default) means Mermaid. Markdown wraps it in a ```` ```mermaid ```` fence. Pictures to a terminal are refused, and `--theme` with a text format is refused. Checks run before the catalog is read. An unknown `--table` is exit 2. `--open` saves a PNG to `$TMPDIR/dbc-erd` and opens it. |
| Web `web/erd.go` | `GET …/erd` → `{title, tables, rels, text}`, asked first so refusals arrive as words. `GET …/erd.png\|.jpg\|.mmd` take `?table=&depth=&views=1&theme=light&download=1`, and a download is named `erd-<conn>-<stamp>.<ext>`. |
| Web page | **ERD** button in the Tables heading (`workbench.go`, `hrow`), a table's right-click ▸ **Diagram around it (ERD)**, and `e` on a selected table. `erdview.js`: a nearly full-window dialog with depth select, views toggle, Fit/100% (`f`), ⤓ PNG/JPEG/Mermaid and ⧉ Copy as Mermaid (`m`). The picture follows the page's theme. The Keys help gained rows for the tables list. |
| TUI `tui/erd.go` | `e` on the tables list, plus table menu items Diagram around it / Diagram all tables / Copy ERD as Mermaid. The work runs in a `tea.Cmd`, reports as an `erdMsg`, saves to `erdDir()` and opens the file. The palette is `plan_theme`. |
| Docs | README: an ERD section after the explain section, "Diagrams headless", and the tables rows of the mouse table. `plan_theme` docs in `dbc.example.toml` and `config.go` now cover ERD pictures. `cats-plugin.toml` completions add `erd`, `--table`, `--depth` and `--views`. |

## Verified

- New tests:
  - `erd/erd_test.go`: notation, MarkKeys, Find, Around, Select,
    WithoutViews, Mermaid (fixture and hostile names, with a line-shape
    regex), pictures in both palettes, no box overlaps (fixture, 45-star,
    12-chain), parents left, wrapping, scale-down at 400 tables, FileName.
  - `db/schema_test.go`: `petsDDL` (table-level FKs, since MySQL 8 ignores
    inline REFERENCES) on real bytdb and SQLite; SQLite's implicit
    REFERENCES, unique index and view; the max_rows bypass; Postgres-shaped
    rows (dropped-attnum gap, same name in two schemas, an outside
    reference, an unknown attnum); MySQL schema case; `pgArray`.
  - `workspace/diagram_test.go`: fresh read, around, refusal, works while
    the run slot is taken.
  - `web/erd_test.go`: JSON, three files with magic bytes and names, the
    light corner pixel, 400 refusals.
  - `tui/erd_test.go`: `e` saves and opens a real PNG without touching the
    editor or the run slot; the menu copies Mermaid.
- `go vet ./...`, gofmt and `go test ./...` (with `CATS_*` stripped) are
  green. The manifest completion test passes.
- CLI by hand on a scratch SQLite shop schema: mermaid, markdown at depth 0,
  `-o` png, an unknown table (exit 2), `--theme` on text (exit 2), a view by
  name piped as JPEG, a stray argument.
- By eye: the fixture in dark and light, the shop schema, and a 45-child
  star (wrapped into a 5×9 block).
- Chrome, against an isolated server (`HOME` pointed at the scratchpad so
  `~/.config/dbc` was untouched):
  - the ERD button opens the dialog, laid out correctly at 1536×895;
  - `f` gives actual size;
  - the right-click menu lists the item, and around `orders` gives 3 tables
    at depth 1, 1 at depth 0 and 4 at "all" with views;
  - `e` works;
  - in the light theme the picture's corner is `#f6f8f6`.
- **Not verified**: the Postgres/MySQL live tests, because Docker was down
  (N-062 updated). Web copy-to-clipboard stayed pending under automation;
  a `navigator.clipboard.readText()` probe then hung the tab on a
  permission prompt, and the test tabs were closed (N-067).

## Notes

- zsh doesn't word-split `$VAR`, so the CLI checks ran from a bash script.
- The scratch config needed `[[connection]]`, not `[[connections]]`.
- Known picture limits, raised as follow-ups: long lines pass under boxes
  (N-065), and markers overlap at a shared parent column (N-066).

## Next

Closed: None. Declined: None. Raised: N-065, N-066, N-067, N-068, N-069.
Deferred: None. Promoted: None.
Updated: N-062. Full list: `ai_docs/todo/next-list.md`.
