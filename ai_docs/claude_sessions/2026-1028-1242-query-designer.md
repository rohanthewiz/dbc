# Query designer — a design tab that writes, checks and explains SQL (dbc web)

Session: `f49e3fbb-db37-40e2-b9d1-2a8ebd75f2a4`

## Ask

From the user's cats-todo backlog: "Let's create a graphical query designer
that will keep a close eye on efficiency, performance and correctness.
Additionally provide good guidance and explanation along the way. Think
outside of the box and create something revolutionary."

No choices were asked up front. Decisions made unasked, and reported at the
end:
- **dbc web only.** A canvas does not fit the TUI. All the rules live in a
  pure Go package the TUI could reuse (N-230).
- **A new tab kind,** the design tab, saved with the tab in `web.bytdb`
  rather than as named design files (N-232).
- **One way:** a design writes SQL, and SQL is not read back into a design
  (N-231). **⇪ Query** copies the SQL into a plain query tab.
- **Alt+D** opens one.

Then `/sw`.

## What was there

- **`erd.Schema`**: tables, columns, primary, unique and foreign keys, and
  views (`readDiagram(Views:true)`).
- **Row estimates** per engine, and **Explain** with insights
  (`explain.Plan`). Plan nodes carry the alias, so a plan can be mapped
  back onto the tables.
- **Nothing read indexes.**
- **The pipeline canvas** (`dbc.stage`) for panning, zooming and cards.
- **Saved tabs** in `web.bytdb` (`web/store.go`), migrated with
  `ALTER TABLE … ADD COLUMN IF NOT EXISTS`.

## Design

A design tab is a query tab whose editor area is covered by a canvas
(`#qd`). The tab's editor document always holds the generated SQL, so
▶ Run, Ctrl+Enter, Explain (Ctrl+X), history and the assistant work on it
unchanged. The design is saved as JSON in a new `tabs.design` column.

**`db/indexes.go`** (new):
- `IndexesQuery(driver, tables)`:
  - **Postgres:** `pg_index` with `unnest(indkey::int2[]) WITH ORDINALITY`.
    It skips invalid indexes and stops at `indnkeyatts`, so INCLUDE
    columns are not key columns.
  - **MySQL:** `information_schema.statistics`.
  - **SQLite:** `pragma_index_list` and `pragma_index_info`.
  - **bytdb:** none. `Manager.Indexes` returns `ErrNoIndexes` there.
- `Manager.RowEstimates(ctx, name)` returns a map by table.

**`designer/`** (new, pure; it imports only `erd` and `explain`):
- **`model.go`:** a `Design` holds tables (with an alias and a position),
  joins, columns, filters, sorts, the main table, DISTINCT, LIMIT and
  OFFSET.
  - **Join kinds:** inner, left and full, plus three of its own:
    - **has any** writes `EXISTS`;
    - **has none** writes `NOT EXISTS`;
    - **summarize** writes a grouped derived table, left-joined.
  - **Filters:**
    - 18 ops, including year, on a day, last N days, contains, starts
      with and ends with;
    - OR groups, "or is empty";
    - "in the join" (for a left join's filter).
- **`catalog.go`:**
  - The catalog: the schema, indexes per table, row estimates, and whether
    the indexes are known.
  - "Big" is 10K rows and "huge" is 100K.
  - `leads()` asks whether an index starts with a column. It skips
    partial indexes and non-btree/hash ones.
  - Type families, so a join's types can be compared.
  - The dialects: identifier quoting with a reserved-word list, and
    literals (MySQL doubles backslashes).
- **`tree.go`:**
  - `normalize` fills in IDs and aliases. An alias is the table's
    initials, numbered when that is a reserved word (`orders_new` → `on2`).
  - `plan` lays the joins out as a tree, breadth first from the main
    table.
    - A join that closes a loop becomes an extra condition.
    - A table nothing reaches becomes a cross join, and is flagged.
    - Dangling joins and right joins are recorded.
    - Outer-joined tables are marked optional.
- **`fanout.go`:**
  - `multipliers`: which joins repeat a table's rows.
  - The cardinality of each join (N:1, 1:N, N:N, ?), from the keys.
- **`sql.go`,** the generator:
  - GROUP BY always lists the plain columns, and aggregate filters go to
    HAVING.
  - OR groups are parenthesized.
  - Values are checked against the column's type; a bad one is an issue,
    not SQL.
  - Year, on a day and last N days become ranges an index can use. Last N
    days uses each engine's date math, and a literal timestamp on bytdb.
  - LIKE escapes the value's own `%`, `_` and `\` with `ESCAPE '\'`.
  - A count of a table's rows is `count(DISTINCT pk)` where a join
    repeats them, and `count(pk)` where an outer join may leave none.
  - Summarize writes `LEFT JOIN (SELECT key, agg … GROUP BY key)`.
    - In a grouped outer query its results are aggregated again; an
      average becomes sum / NULLIF(count).
    - Its counts are COALESCEd to 0.
  - An OFFSET with no LIMIT is written `LIMIT -1` on SQLite and
    `LIMIT 18446744073709551615` on MySQL.
- **`lint.go`:** each rule has a *why* in plain words, and where it can,
  fixes. A fix is either a whole corrected design the page swaps in or SQL
  to copy.
  - **Correctness:**
    - **Broken designs:** missing tables, broken joins, joins with no
      condition, bad join columns, generator issues, a misplaced
      EXISTS/summary, a full join on MySQL.
    - **Cartesian products:** the fix is the shortest FK path, through
      bridge tables if needed.
    - **Suspect joins:** type mismatches, many-to-many joins, right joins,
      guessed joins.
    - **Repeated rows:**
      - fan-out inflating an aggregate (the fix is summarize);
      - chasm traps;
      - a fan-out nothing uses, and DISTINCT hiding one.
    - **Filters:** a WHERE filter undoing a left join, an anti-join
      written the hard way, `<>` dropping NULLs, `BETWEEN` a day on a
      timestamp.
    - **Counts and order:** a count of a nullable column, LIMIT without
      ORDER BY, an ORDER BY that is not unique.
  - **Performance:**
    - An unindexed filter, join or sort on a big table. Each comes with a
      `CREATE INDEX` to copy, never run. The join rule notes that
      Postgres does not index a foreign key's columns.
    - A leading wildcard, with a Postgres trigram fix.
    - A deep OFFSET, an unbounded result, an OR across columns, and
      selecting every column.
  - On an engine with no indexes or estimates, the index rules stay quiet
    rather than guess.
- **`story.go`:** the query read back in plain words, one line per step
  ("from here on there is a row per order", "tested with EXISTS: each
  customer once"). It is written from the same tree as the SQL.
- **`suggest.go`:**
  - `AddTable` joins a new table to each part of the design once, by
    foreign key.
    - The join kind is a left join when the key is nullable and followed
      from the child.
    - With no key it guesses by name, including the last word of a
      prefixed name (`qd_customers` → `customer_id`).
  - The related-tables list, and the FK-path search (4 joins deep) used by
    the cartesian fix.
- **`analyze.go`:** `Analyze(d, cat)` returns everything the page draws:
  - the design, normalized;
  - the SQL, whether it is valid, and the result's column names;
  - the lints and the story;
  - a view per card (role, rows, grain, columns with key and index marks);
  - a view per edge (cardinality, keyword, fan-out, text, other FKs to
    follow).
- **`plan.go`:** `PaintPlan` maps an Explain plan onto the tables: each
  table's access path (scan or index), estimated rows and loops, and the
  plan's insights.

**`workspace/design.go`** (new):
- `DesignCatalog` and `DesignPlan` run on the pool, outside the run slot,
  like Diagram. Designing never waits behind the user's query, nor reads
  inside their transaction.
- The catalog is cached per connection for a minute, under its own lock.
  Indexes are read per table as the design adds them.
  - A table whose indexes could not be read is not judged as having none:
    `IndexesKnown` is false.
  - `ForgetDesignCatalog` drops the cache, for ⟳ and after DDL.
- `DesignPlan` is EXPLAIN without ANALYZE, under an 8s budget.

## dbc web

- **`web/design.go`:**
  - `GET /api/v1/ws/:id/design/tables` (the palette; `?fresh=1`).
  - `POST …/design` analyzes a design.
  - `POST …/design/add` adds a table and says how it was joined.
  - `POST …/design/plan` re-generates the SQL from the design rather
    than taking SQL from the page. An `EXPLAIN ANALYZE …` handed to
    Explain would run, so the page cannot send one. An invalid design is a
    409.
- **`web/store.go`, `web/api.go`:** the `design` column, at most 1 MB. A
  save checks the JSON, and refuses a design together with a file.
- **`designer.js`** (new), `dbc.designer.create(hooks)`:
  - **The bar:**
    - health chips;
    - the plan's estimated rows, cost and full-scan count;
    - ↶ ↷ ⊡ ⟳ and ⇪ Query.
  - **The palette:** related tables first; click or drag to add.
  - **Cards:**
    - a role line and a plan badge;
    - column rows with a checkbox, key and index marks, and `?` for
      nullable.
  - **Edges:** SVG with a label button (`⋈ 1:N`). Stacked cards route side
    to side.
  - **Interactions:**
    - Drag a header to move a card. Drag a column onto another card's
      column to join; the join is oriented from the joined table.
    - Click a column for its menu.
    - Keys: Delete, Ctrl+Z, Ctrl+Shift+Z / Ctrl+Y, f, Esc.
  - **Side panes:**
    - **Query:** the join and table inspector, columns, filters, sort and
      rows.
    - **Guide:** the lints with their fixes, highlighting what they point
      at on hover, and the plan's insights.
    - **Story** and **SQL.**
  - **Internals:**
    - Focus is kept across redraws.
    - Stale responses are dropped.
    - A new card goes to a free spot, and the view fits a small design
      or pans to the new card.
- **`app.js`:**
  - **Opening one:**
    - the **◇ Design** button and Alt+D (page-wide and in Monaco);
    - the table menu's "Design a query from it";
    - the tab menu's "New design tab".
  - **The tab:**
    - The strip icon ◇.
    - Design tabs get no console.
    - Run is gated on a valid design.
    - The design saves debounced.
    - The catalog is forgotten when the connection changes.
  - The KEYS list.
- **`app.css`:** `.qd*` classes from the theme tokens, and the editor
  area's minimum height in design mode.
- **README:** "### The query designer", before "As a macOS app", plus the
  intro and the keys table.

## Found on the way

- **MySQL 8.4 parses a column's inline `REFERENCES t(id)` and ignores
  it,** so no foreign key exists. The live fixtures use table-level
  `FOREIGN KEY (…) REFERENCES …`, which Postgres takes too. (Noted in
  memory.)
- **Web e2e on a canvas:**
  - `clickAt`'s `scrollIntoView` scrolls even an `overflow: hidden` box,
    which shifted the stage under the pointer. designer.js now turns the
    canvas's scroll into a stage pan.
  - A card outside the viewport can sit under the palette, so the
    centre-point fallback clicked the palette and added a table twice.
- **After Alt+W the strip redraws only once the neighbouring tab has
  loaded,** so reading it straight after the active tab changes saw the
  old strip. That looked like Alt+W closing the wrong tab. The step now
  waits on the strip.
- **A step needing a query tab opens its own** (Alt+T, then the
  connection row), since the step before may leave a pipeline tab on
  screen. (Both e2e notes are in memory.)

## Tests

- **`designer/designer_test.go`:** 18 tests over a hand-built shop schema
  that includes a self-referencing table, a view, and a table named
  `order`. They cover:
  - generation, quoting and aliases;
  - GROUP BY and HAVING, OR groups and typed values;
  - ranges and LIKE escaping;
  - counts through joins, summarize (with its re-aggregation), EXISTS and
    NOT EXISTS;
  - each lint and its fix;
  - the story, AddTable and the FK-path fix.
- **`web/design_test.go`, `TestDesignRoutes`:** end to end on SQLite.
  - The palette and the FK add.
  - The fan-out lint and its summarize fix. Running the fix gives
    "Ana|120|120 Bo|20|".
  - The index mark and plan painting.
  - The 409, and the tab save with a broken design refused (400).
- **`workspace/live_design_test.go`, `TestLiveDesigner`:** on Postgres 17
  and MySQL 8.4, each designed statement is run and its rows checked:
  - summarized payments ("Ana|120|120|3 Bo|20|NULL|0");
  - EXISTS with its filter inside, and NOT EXISTS;
  - a left join with the year filter in the join;
  - an escaped LIKE;
  - count DISTINCT per country;
  - OFFSET with last N days;
  - DesignPlan painted onto the tables.
- **Web e2e step "query designer"** (`web/e2e/designer_test.go`):
  - Add tables, use the column menu, apply the guide's fix, and run with
    Ctrl+Enter ("Ana|120|120 Bo|20|NULL").
  - Drag a join and change it to a left join in the inspector.
  - Ctrl+Z, the story, a reload, then close both tabs.
- **Results:**
  - `go test ./...` passed, with `CATS_*` stripped.
  - The live suites for db, workspace, designer and web passed on both
    engines.
  - The full web e2e passed (68s).
  - gofmt and go vet are clean.

## Commits

- `9360daa` Query designer: a design tab that writes, checks and explains
  SQL (dbc web)
- This doc and the next-list edits.

## Next

Closed: None. Declined: None. Raised: N-230, N-231, N-232, N-233, N-234.
Deferred: None. Promoted: None. Moved: None.
Updated: None. Full list: `ai_docs/todo/next-list.md`.
