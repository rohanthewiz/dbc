# README multi-statement example, and the schema lookup on live servers

Session: 24c65680-adca-41ec-9c01-9538519673da
Date: 2026-09-25

## Ask

Two next-list items, pasted one after the other:

- **N-041**: regenerate the README's `### Multi-statement runs` sample from
  the binary. It showed `│ demo │` banners, but with no config the active
  demo is `demo-bytdb`.
- **N-032**: run the assistant's schema lookup (`db.ColumnsQuery`) against a
  live Postgres and MySQL. Neither query had met a real server.

## N-041 — README example

The item understated it: the example did not run at all on the default
demo. On `demo-bytdb` the INSERT failed with `primary key column may not be
NULL`, because the example left out `id` and bytdb does not fill in an
integer primary key the way SQLite does. The old sample output was
also wrong on its own terms: 3 breed rows where the seed has 5 (no Bengal,
no Sphynx).

- The INSERT now gives `id` (`VALUES (9, 'Zed', 'Tabby', 4)`), which runs on
  both demos. The demo re-seeds on every open (DELETE + insert), so the
  example gives the same result on every run.
- `ORDER BY n DESC, breed`: Maine Coon and Siamese tie at 2, so the order
  was otherwise up to the engine.
- The sample output is the binary's real output. It keeps the banner's `…`
  on the shortened SQL echo.

Run with `HOME` pointed at a scratch dir and `CATS_*` stripped, so the user's
cached `demo.bytdb` and the live cats pane were untouched. Both
`demo-bytdb` and `-c demo-sqlite` exit 0 with identical rows. Commit
`75b0bda`.

## N-032 — schema lookup on live servers

Throwaway Docker containers (`postgres:17` → 17.11, `mysql:8.4` → 8.4.11,
`lower_case_table_names=0`), on random localhost ports, stopped and
removed afterwards. **No change to the lookup was needed.**

`db/live_test.go` (new) makes the check repeatable. Like
`clip/live_darwin_test.go`, it is opt-in: each test skips unless its DSN is
set (`DBC_LIVE_PG_DSN`, `DBC_LIVE_MYSQL_DSN`). It walks the assistant's path
rather than calling `ColumnsQuery` alone: `TablesQuery` → `TableRefs` →
`NewTableIndex(...).Mentioned(sql)` → `Manager.Columns`, keyed by
`TableIndex.Display`. It compares each table's `name type` list exactly.

- Postgres (`dbc_live`, `dbc_live2` schemas): `character varying(80)`,
  `numeric(5,2)`, `text[]`, an enum reported as `dbc_live.mood` (qualified
  because the schema is off the search_path), `timestamp with time zone`.
  A dropped column is absent. A view and a partitioned parent (relkind `p`)
  describe correctly. `"MixedCase"` with a `"Weird Col"` column works.
  `dbc_live.cats` and `dbc_live2.cats` stay separate.
- MySQL (`dbc_live_*` tables): `int unsigned`, `varchar(80)`,
  `enum('calm','feisty')`, `decimal(5,2)`, `datetime(3)`, `json`, a view, and
  a mixed-case table name.

The tests drop their own objects before and after, so an interrupted run
does not break the next one. Commit `1d29b38`.

**Found on the way (raised as N-042):** Postgres materialized views are not
in `information_schema.tables`. Confirmed on the live server: 0 rows there, 1
in `pg_class`. So the sidebar and the assistant never list them, even though
`ColumnsQuery` already accepts relkind `'m'`.

## A flaky test from the previous session

The full suite failed on `ai`: `TestSignInAnswersConfigurationRequests`
failed in about half of runs (9 of 20 passed). The cause is in the test,
not the product. The fake was already signed in, and `beginSignIn` closes the
connection as soon as `signIn` says `AlreadySignedIn`. Meanwhile the client
answers the server's `workspace/configuration` request on its own goroutine
(`rpc.go` `serveRequest`). When the close won, the answer was never written.
Nothing needs that answer once the account is known to be signed in, so the
product is correct. The test now uses a device flow that stays open until
it calls `Close`, with a comment saying why. It passed 100 of 100 runs and
`go test -race ./ai` is clean. Commit `e0b5366`.

## Verification

- `go test ./... -count=1` passes with `CATS_*` stripped. `go vet ./ai ./db`
  is clean.
- The live tests pass against both servers, and skip without their DSNs.
- The editor reported `CanSignIn` / `ErrAuthRequired` as undefined in
  `signin_test.go`. Its index was out of date: both exist (`ai/agents.go:49`,
  `ai/chat.go:251`), and the package builds and vets.

## Next

Closed: N-041, N-032. Declined: None. Raised: N-042.
Deferred: None. Promoted: None.
Updated: None. Full list: `ai_docs/todo/next-list.md`.
