# Postgres sidebar: complete catalog, then a database → schema → tables navigator

Session: `cfb293c7-ba05-46f1-952d-a517b23c0013`

## Ask

On a large Postgres database, (1) not every schema showed up in the sidebar
("perhaps list schemas first, not tables"), and (2) a schema the user knew
was there counted 0 tables. Then: "the connected host may have multiple
databases, so we should not try to load everything at once."

## Diagnosis

Reproduced locally (docker Postgres 17, non-owner role `reader`,
40 schemas × 40 tables, a schema with no table grants, an empty schema). The
user's prod connection was not touched.

1. **The sidebar's catalog was cut at `max_rows` (1000).** `workspace.Connect`
   ran `TablesQuery` through `Manager.RunContext`, which caps at `max_rows`.
   The catalog is ordered by schema, so the cut kept only the first few
   schemas alphabetically (25 of 40 in the repro), with no note. A catalog
   that timed out (10 s) was also dropped silently, leaving an empty sidebar.
2. **`information_schema.tables` hides relations without a privilege.** A
   schema whose tables belong to another role and were never granted listed
   0 tables. It is also slow on large catalogs (per-row privilege checks).
3. **One Postgres connection sees one database.** The schema "known to be
   there" may live in another database on the host. The DSN names one
   database, there is no `USE`, and every catalog is per database.

## What changed

### Pass 1: complete, privilege-independent catalog

- `TablesQuery` (Postgres) reads `pg_class` ⋈ `pg_namespace`. It covers
  relkinds r/p/v/m/f with information_schema's type names, and the user
  schemas filter (`pgUserSchemas`: not `information_schema`, not `pg\_%`).
  Matviews no longer need a `pg_matviews` UNION branch.
- `Manager.Catalog` reads the sidebar's catalog itself (`limitedRows`,
  shared with `stringRows`), bounded by `maxCatalogRows` (250k) rather than
  `max_rows`, and sets `Truncated` when cut. Ctrl+T still goes through
  `Run`, so `max_rows` still applies there.
- A connect now logs "tables list unavailable: …" or "tables list cut at N
  tables" instead of failing silently.

### Pass 2: lazy navigator (user picked: sidebar pickers, per-schema load, web first)

- **Derived per-database connections** (`config.DatabaseSep`, `DerivedName`).
  - `ConnByName("<base>/<database>")` returns the Postgres connection `base`
    with `Base`/`Database` set. Exact names match first, then the longest
    base wins. Nothing is stored; `Demo` is cleared.
  - `openPool` sets `pgx.ConnConfig.Database` after parsing, so it works for
    URL and keyword DSNs and after the TLS rewrite.
  - Every name-keyed thing (pools, row counts, sessions, history, tabs,
    layout keys) works unchanged.
  - `Manager.Drop(base)` also closes derived pools (`droppedWith`).
  - In web, `tabsOn` counts tabs on derived connections, and a rename
    retags their saved tabs (`derivedFrom`).
- **`db/navigate.go`**:
  - `Navigable(driver)` (Postgres only).
  - `DatabasesQuery`: `pg_database`, no templates, `datallowconn`,
    `has_database_privilege(oid,'CONNECT')`, plus a current flag.
  - `SchemaSummaryQuery`: every user schema, empty ones included, with its
    table count and whether it is `(current_schemas(false))[1]`.
  - `SchemaTablesQuery` / `pgTables(schema)`.
  - `Manager.Catalog(ctx, name, schema)`, `Databases`, `SchemaSummary`,
    `DefaultDatabase(cc)` (the DSN's effective database, via pgx).
  - `AllSchemasLimit = 5000`.
- **`TableIndex.SetSchemas`**:
  - `Display` qualifies names by the *database's* schema count, so previews
    work off `search_path`.
  - `Mentioned` also reports `schema.table` words in unloaded schemas. It
    only accepts real schema names, so an alias like `c.name` is not taken
    for a table. `Manager.Columns` verifies, and `AttachColumns` drops what
    it cannot find.
- **Workspace**:
  - `SchemaPick{Name, All}`, `ConnectPick`/`SwitchPick`.
  - `PickSchema` is its own cancelable Job, landing `*SchemaLoaded`. It
    shares `connGen` with connect and is refused while connecting.
  - `resolvePick` and `defaultSchema` choose the schema to open on: the
    `search_path` schema if it has tables, else the first schema with
    tables. An empty `public` on a shared server should not open as an
    empty list.
  - `Options.WholeCatalog` (TUI) asks for all schemas up to the limit.
  - `Diagram` defaults `erd.Selection.Schema` (new) to the listed schema.
  - The row-count cache merges a fresh counting instead of replacing it.
- **Web**:
  - `sideState` (embedded in `connEvent` and `wsState`) carries tables,
    `base`, databases (`dbRef.conn` maps the DSN's own database back to the
    base), schemas with counts, schema, `allowAll`, `navigable`.
  - New `POST /api/v1/ws/:id/schema`. Connect takes `schema`/`all` (the
    page's saved pick).
  - Page: a `combo()` factory replaces the schema-only combobox and builds
    the new **db** picker and the **schema** picker. The schema list
    optimistically shows "loading…".
  - `connItem()` maps a derived name to its base's row. `pickFor()` sends
    the saved pick on connect.
- **TUI**: names via `TableIndex.Display`; `WholeCatalog: true`.
- README: replaced the sidebar paragraph with the db/schema/tables
  description.

## Verification

- `go vet ./...`, gofmt clean, full `go test ./...` green, including all
  Postgres live tests against docker Postgres 17.
- New tests:
  - `TestTablesQueryPerDriver` (no `information_schema.tables` on Postgres).
  - `TestNavigatorQueriesPerDriver`, `TestTableIndexSetSchemas`,
    `TestConnByNameDerived`, `TestResolvePick`,
    `TestPickSchemaRefusedOffPostgres`, `TestConnectCatalogIgnoresMaxRows`.
  - Live: `TestLiveCatalogPostgres` (reader role: 12 tables despite
    `max_rows=10`, ungranted table listed, empty schema with 0, public as
    default) and `TestLiveDatabasesPostgres` (templates hidden, a derived
    connection lands on the other database, the base stays on its own).
- **Browser (go-rod, scratchpad):** fixture with databases dbc/analytics/
  postgres plus an unlisted `locked_db`, 1,601 tables. 16 checks:
  - Opens on `app_locked` (1 / 1601). The db list shows the three
    connectable databases.
  - Picking s07 lists 40 tables; the empty schema shows "no tables"; all
    schemas lists 1,601.
  - Switching to analytics gives `fixture/analytics`, the base row marked,
    and `public.events`. sales lists 2 tables, and the preview ran
    `SELECT * FROM sales.orders`.
  - A reload keeps the database and schema. Going back to `fixture`
    restores the s07 pick.
  - The screenshot matched the chosen mockup.
- **Not exercised:** Safari/WKWebView (N-064), TUI over the 5,000 limit,
  MySQL.

## Next

Closed: N-076, N-077. Declined: None. Raised: N-078, N-079, N-080, N-081, N-082.
Deferred: None. Promoted: None.
Updated: None. Full list: `ai_docs/todo/next-list.md`.
