# N-311: levels of introspection

Session: `c56feb96-acdd-45e3-93ba-1d9516845473`

## Ask

"Next list item N-311": the per-connection `introspect` setting proposed
at the end of the last session (`2026-1112-1538-lazy-catalog-after-connect`),
with a global default and an adaptive fallback. The item said the shape
was "awaiting the user's yes or their own levels"; being handed the item
as the task was taken as the yes, and the three levels were built as
proposed. Then `/sw`.

## What changed

**The setting (config)**

- `introspect = "full" | "names" | "off"` at the top of the config file
  is the default for every connection; `introspect` in a `[[connection]]`
  overrides it. `config/introspect.go` holds the levels, `ParseIntrospect`
  and `Config.IntrospectFor(name)`, which settles the level: the
  connection's own, else the global key, else `full`. A derived connection
  (`ProdDr/analytics`) reads at its base's.
- A value that is no level warns and falls back (the global key to
  `full`, a connection's to the default) rather than failing the load.
- `SavedConn.Introspect` (`omitempty`), so `connections.toml` leaves the
  key out for an entry that never chose a level.

| Level | The sidebar reads | Completion reads |
| --- | --- | --- |
| `full` (default) | databases, schemas with table counts, the open schema's tables | every schema |
| `names` | databases, schema names (`SchemaNames`, no pass over `pg_class`), the open schema's tables; no "all schemas" | the open schema's and the `search_path`'s (`SchemaIn`) |
| `off` | nothing until a Refresh, which reads as `names` | the dialect's words until that Refresh, then as `names` |

`names` differs from `full` only on Postgres: the other engines have no
schemas to count or to scope by.

**The reads (workspace)**

- `workspace/introspect.go`: `levelOf(cfg, mgr, conn)` is the effective
  level — the configured one, lowered from `full` to `names` once the
  Manager remembers the connection's schema summary as slow.
- `readCatalog` and `listSchemas` take the level: at `names` the schemas
  are listed by name and the summary is never tried, with nothing said.
- `off` is a state as well as a level. `landConnect` makes no `Load`; the
  workspace is `CatalogUnread()` (new), keeps the connect's pick in
  `loadPick`, and logs `catalog of <conn> not read (introspect = "off") —
  a refresh reads it`. While unread: a second click on the row does
  nothing, `PickSchema` is refused, no relist follows DDL, completion is
  ready with the dialect's words and `LoadCompletions` reads nothing. The
  user's Refresh ends it, reading as `names` for the pick the connect was
  asked for; a Refresh that read nothing at all (canceled, every list
  failed) leaves it unread.
- Completion: `LoadCompletions` and `ConnCompletions.load` start scoped
  below `full` on a navigable connection. `ConnCompletions` at `off` reads
  nothing and keeps no entry, so dbc web's pipeline inspector offers no
  tables or schema completions for such a connection.
- `resolvePick`'s note for a refused "all schemas" now says "are not
  counted" (at `names` nobody tried).

**The adaptive default (db.Manager)**

- `MarkSummarySlow` / `SummarySlow`: a summary that runs out its 5s
  budget is remembered for the connection, on the Manager so every
  workspace (every tab) shares it. The read that watched it time out logs
  the existing "counts unavailable" line plus one saying the connection
  is read by names from here on and that `introspect = "names"` skips the
  first wait too. A summary that fails fast for another reason is not
  remembered.
- The memory survives a `Disconnect` (which now closes the pools without
  going through `Drop`) and is forgotten by `Drop` — an edit of the
  connection's name, driver, DSN or TLS, or a removal — with whatever
  derived connections remembered.

**The forms and sidebars**

- `connedit.Form.Introspect`, validated in `Check`. Like `ai_rows` it is
  no part of how the pool dials, so a change to it alone goes through
  while the connection is in use and shows at the next refresh.
  `pgdocker` keeps it across a DSN update.
- TUI: an **Introspect** chip row (`default · full · names · off`); with
  the row focused and nothing else to say, the form's message line says
  what the level under the cursor reads. The Tables pane says `not read ·
  r refreshes`; `d` / `s` on an unread connection say why there is
  nothing to pick.
- dbc web: an **Introspect** select in the connection form (`connInfo`
  and the server-rendered list carry `introspect`); `sideState.Unread`,
  and the Tables list says "not read — refresh lists the tables", the
  word a link that refreshes.

**Docs**: README gains "Levels of introspection" under Configure
connections, with pointers from the sidebar, completion and both form
sections; `dbc.example.toml` documents the global key and a per-connection
example.

## Decisions made without the user

The item left two questions open. Both were settled by the session and
are recorded as N-314 so they can be flipped:

| Question | Chosen | Why | Cost |
| --- | --- | --- | --- |
| Does a user's Refresh try a remembered-slow summary again? | No | The item's aim was that no config edit is needed to stop paying the 5s, and refresh was one of the three places it was paid | One transient timeout on a small database loses its counts and "all schemas" until restart |
| Does the memory outlive the process? | No | Nothing is written down; a restart tries once more | The first connect of every run pays the 5s, unless `introspect = "names"` is set |

Also the session's: a remembered slow summary lowers the whole connection
to `names`, completion included, rather than the schema list alone.

## Verification

- `go test ./...` with `DBC_LIVE_PG_DSN` (Postgres 17 in Docker): all 33
  packages pass.
- `web/e2e` with `DBC_E2E=1` and the same DSN: 54 steps pass, including
  the new "introspect off: an unread sidebar" (add through the form with
  the select on `off`, the unread list, a run meanwhile, the refresh link
  listing the table, the edit form opening on `off`).
- New tests: `config/introspect_test.go`; `db` `TestSummarySlowMemory`;
  `workspace/introspect_test.go` (SQLite: level resolution, connect at
  off, refresh, the kept pick, completion, `ConnCompletions`); live
  Postgres `TestLiveWorkspaceIntrospectNames`, `…IntrospectOff`,
  `…SlowSummaryRemembered`; `connedit` `TestIntrospectRidesTheForm`; tui
  `TestConnFormIntrospectOff`; web `TestOffConnectionSidebarIsUnread` and
  a `TestConnEdit` case.
- Not done: a run against a database whose summary is really slow (the
  tests force the timeout with a 1ns budget). That is N-315.

## Notes

- A canceled Refresh of an unread connection used to "land" an empty
  catalog (the cached pool answers the dial, every list then fails on the
  dead context), which showed as a failed list rather than "not read".
  `landConnect` now leaves it unread when nothing at all was read; a
  Refresh that read the schemas but not the tables is still installed, so
  its picker is the way to a smaller read.
- The slow memory is read through `sctx.Err()` before the cancel:
  after it, `Err` says Canceled for a summary that returned in time.
- Pre-existing gofmt nits in `web/server_test.go` and `tui/app_test.go`
  were left alone again.

## Next

Closed: N-311. Declined: None. Raised: N-313 (the TUI pickers' wrong
reason while a catalog is loading), N-314 (the two adaptive-default calls
made without the user's word), N-315 (hand checks on a really slow
database, in Validate). Deferred: None. Promoted: None. Moved: None.
Updated: None. Full list: `ai_docs/todo/next-list.md`.
