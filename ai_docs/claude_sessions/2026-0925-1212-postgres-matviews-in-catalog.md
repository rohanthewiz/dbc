# Postgres materialized views in the catalog

Session: cc51b036-e79e-414a-8dd8-d91c4d3fc7aa
Date: 2026-09-25

## Ask

Next-list item **N-042**: Postgres materialized views never reached the
sidebar or the assistant. `db.TablesQuery` read only
`information_schema.tables`, which leaves them out (the SQL standard has no
materialized views). `db.ColumnsQuery` already accepted relkind `'m'`, so only
the listing was missing.

## Change

`db/catalog.go` — `TablesQuery`:
- **Postgres** now adds rows from `pg_catalog.pg_matviews` with `UNION ALL`,
  typed `'MATERIALIZED VIEW'`. `TableRefs` and the sidebar (`tui/sidebar.go`)
  both treat any type containing `VIEW` as a view, so neither changed.
- **Same privilege rule for both halves.** `information_schema.tables` shows
  only relations the user holds some privilege on, but `pg_matviews` shows
  all of them. The matview rows are filtered with
  `has_table_privilege(quote_ident(schemaname) || '.' || quote_ident(matviewname), 'SELECT')`.
  The names are quoted because that function parses its text argument as SQL.
- **bytdb gets its own case.** It had shared the Postgres query, but bytdb
  v0.16.0 has no `pg_matviews` (checked in the module source), so the union
  would fail there. bytdb keeps the plain `information_schema.tables` query.

## Tests

- `TestTablesQueryPerDriver`: the Postgres aliases must mention
  `pg_matviews`; bytdb's query must not.
- `TestLiveColumnsPostgres` (opt-in, `DBC_LIVE_PG_DSN`): creates
  `dbc_live.cat_names` as a materialized view, and checks the assistant's
  lookup finds it with `id integer`, `name character varying(80)`. It also
  checks that `TableRefs` marks it as a view.
- Ran against a throwaway `postgres:17` container (random localhost port,
  removed afterwards): passes. Full `go test ./...` passes.
- Checked by hand in psql on the same container (not kept as a test):
  - the old query did not list matviews; the new one does
  - a role with no SELECT on `public.secret` does not see it, but does see a
    matview it was granted
  - a matview named `"We ird"."m'v.X"` gets through `has_table_privilege`
    and is listed

## Next

Closed: N-042. Declined: None. Raised: None.
Deferred: None. Promoted: None.
Updated: None. Full list: `ai_docs/todo/next-list.md`.
