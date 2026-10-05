# Completion on a catalog too big to read whole

Session: `bd52bdb4-ccc4-4f16-91f5-c349f7569390`

## Ask

On a console against the ProdDr connection, completion logged:

```
completion is without the schema: the catalog is too big to read whole -
Error: location[db/manager.go:915 -> db/schema.go:482],
function[dbc/db.wrapRunErr -> dbc/db.stringRows], conn[ProdDr],
op[read the schema], rows[250000]
```

## Cause

Completion's cache is filled by `Manager.Schema`, which reads every user
schema's tables, columns, keys and partitions — four catalog queries, each
bounded by `maxSchemaRows` (250,000), past which `stringRows` fails rather
than return a partial (silently wrong) schema. The sidebar already loads a
big Postgres a schema at a time; completion still read the whole database.
A failed load is remembered for `complRetry` (5 min), so completion stayed
on the vocabulary alone.

ProdDr itself was not queried (production), so which query overflowed is
unconfirmed — see N-106.

## Fix 1: partitions left out in SQL (`db/schema.go`)

On Postgres every partition repeats its parent's columns; `BuildSchema`
dropped them (via `PartitionsQuery`) but only after reading them all — a
40-column table split by day over three years was 40,000 rows thrown away.
Now:

- `schemaColumnsQuery`: `AND NOT c.relispartition` (pgx only — bytdb shares
  the statement but its `pg_class` need not carry the column).
- `schemaKeysQuery`: `AND NOT c.relispartition AND fc.relispartition IS
  NOT TRUE` (keys cloned onto partitions, or referencing one; `IS NOT TRUE`
  keeps p/u rows whose `fc` is NULL).

Equivalent output: the existing live partition test
(`TestLiveSchemaPostgres`, sub-partitions across schemas, cloned keys)
passes unchanged.

## Fix 2: scoped fallback for completion

- `db`: `ErrCatalogTooBig` sentinel (`stringRows` wraps it, so `errors.Is`
  works through serr). `Manager.SchemaIn(ctx, conn, scope)` — Schema kept to
  the named schemas on a Navigable driver (Postgres); `Schema` is
  `SchemaIn(…, nil)`. Scoped variants `schemaColumnsQuery`,
  `schemaKeysQuery`, `partitionsQuery`, plus `pgTablesIn` / `pgSchemaIn`
  (`n.nspname IN (…)`, `FALSE` for an empty list) in `db/catalog.go`.
  The overflow error now names the query: `query[tables|columns|keys|partitions]`.
- `workspace/complete.go`: `readCompletions` reads the search path first,
  then the whole schema; on `ErrCatalogTooBig` (Navigable only) it marks the
  connection in `Workspace.complScoped` and reads `SchemaIn` over
  `complScope(focus, path)` — the sidebar's schema + the search path (public
  when unknown). Each read gets its own `CompletionTimeout`.
  `complState.scope` records the scope; `covers(focus)` makes `Complete`
  not ready (and `LoadCompletions` reload) when a pick moves the sidebar to
  a schema the scoped cache does not hold. Whole caches still survive picks.
- `sqlcomplete.bare`: one schema in the cache was always bare; now only
  when the server reported no path. A scoped cache holding just `sales`
  with path `public` inserts `sales.orders`.

## Tests

- `db`: `TestSchemaQueriesScoped`, `TestStringRowsTooBig` (SQLite recursive
  CTE at 250,000 and 250,001 rows).
- `workspace`: `TestCompletionScopedCache`, `TestComplScope`.
- `sqlcomplete`: `TestSearchPath` extended for the one-schema cases.
- Live (postgres:17 container): `TestLiveSchemaPostgres` extended with
  `SchemaIn` scoped to each schema; `TestLiveWorkspaceCompletionBigCatalog`
  (`DBC_LIVE_BIG=1`) now expects the 260,000-column catalog to load scoped
  (~330ms) with the sidebar on public, and the big schema's own scoped read
  to fail too big. Full `go test ./...` with the PG DSN: all pass.

Diagnostic for ProdDr (column rows on partitions vs. not):

```sql
SELECT c.relispartition, count(*) FROM pg_attribute a JOIN pg_class c ON c.oid = a.attrelid
WHERE a.attnum > 0 AND NOT a.attisdropped AND c.relkind IN ('r','v','m','p','f') GROUP BY 1;
```

## Next

Closed: None. Declined: None. Raised: N-105, N-106.
Deferred: None. Promoted: None.
Updated: None. Full list: `ai_docs/todo/next-list.md`.
