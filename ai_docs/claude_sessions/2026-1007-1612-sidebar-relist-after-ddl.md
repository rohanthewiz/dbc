# N-149: the sidebar relists after a run that changes the catalog

Session: `7df06715-8637-4f4f-ab7b-8f946d000775`

## Asks

1. N-149 from the cats-todo backlog. After a CREATE / DROP / ALTER / RENAME
   run in dbc, the completion cache was dropped but the sidebar's table list
   waited for the connections menu's Refresh. The item suggested starting
   the same catalog re-read (`connectLocked(…, refresh)`) after a DDL run or
   a script. It added that a script that ran DDL can now be told apart, and
   that `sqlsplit.IsDDL` and `workspace.ddlVerbs` answer different questions,
   so check which one this needs before sharing one.
2. "merge these changes into local main".
3. `/sess-wrap sidebar-relist-after-ddl`.

## Decisions

Judgement calls (no questions were asked):

- **The classifier is "catalog-changing", not `IsDDL`.** The relist needs
  to know whether a statement can change what a catalog read lists. TRUNCATE,
  GRANT and REVOKE don't change it. SQLite's ATTACH and DETACH add or remove
  a whole schema. That is `workspace.ddlVerbs`, which moved to
  `sqlsplit.ChangesCatalog` beside `IsDDL`. A comment table shows where the
  two differ. Completion's cache drop and the relist share it, and `IsDDL`
  (the DDL log) is unchanged.
- **A relist is a Refresh in all but its words.** It is the same
  `connectLocked` re-read with the same pick and session (no Release, so an
  open transaction stays open), and the Manager's row counts are dropped.
  The new `connectKind` (`kindConnect` / `kindRefresh` / `kindRelist`)
  replaces the `refresh bool`. A relist logs "relisted <conn> after the DDL:
  N tables", or "relist of … failed / canceled" and "not relisted: the
  tables listed before stay". It never touches the status bar: the user asked
  for the run, and the bar keeps the run's summary.
- **The relist replaces the recount.** Its landing counts the list it read,
  so `RunDone.Counts` is nil whenever `RunDone.Relist` is set. A recount
  would count the old list, only to be canceled. `Wrote` still drives other
  tabs' recounts in dbc web.
- **Only statements the run reached count**, as with `wrote`. A DROP before
  the failing statement relists. A CREATE after it doesn't, because it never
  ran. Completion's drop still looks at every statement, as before.
- **A script relists only if it ran catalog DDL on its own connection.**
  `sdb.S` notes each catalog-changing statement in `logDDL`, whether the log
  is on or off. Both ways a statement runs come through it: `run`, and the
  etl Trace of a Copy's or Writer's setup. It is noted before the statement
  runs, so a failed one counts too (one extra relist rather than a stale
  list). `S.CatalogChanged(conn)` is host-only (added to `sdbapi.hostOnly`).
  Completion's drop stays "any script", because a drop costs nothing until
  the next completion and the raw `DB()` handle is out of S's sight.
- **Found on the way: DDL inside a transaction.** The relist reads through
  the pool, so on Postgres or SQLite a `BEGIN; CREATE TABLE …` on the pinned
  session is invisible until COMMIT. A COMMIT is not DDL, so the table would
  never appear. Now a run whose DDL may still be open marks the connection
  (`ddlSinceCommit`), and the COMMIT or END run after it relists again.
  Transactions aren't tracked lexically, so an auto-committed CREATE also
  costs one extra relist at the next COMMIT on that connection. ROLLBACK
  doesn't relist (the pool saw the pre-transaction state) and keeps the
  mark, because ROLLBACK TO SAVEPOINT leaves the transaction open.
- **Found on the way: a re-read during a schema pick undid the pick.**
  `refreshPickLocked` re-read `w.schema`, which only changes when a pick
  lands. Now `PickSchema` remembers its pick (`schemaPick`), and a re-read
  started while it loads asks for that pick. This fixes the manual Refresh
  too.
- **No relist when:** the run's connection is no longer active, the sidebar
  has no list (no catalog query, or a failed connect that Refresh covers),
  or a connect is in flight (its read replaces the list anyway).
- **The TUI starts a background tab's relist at once.** Until a relist
  lands, the workspace is mid-connect (`Connecting`), which refuses Refresh
  and schema picks. Left in the background tab's pending queue, it would
  have stayed that way until the user came back. `routeTab` starts it the
  way it starts a switch's Release, and its `*Connected` is queued behind
  the `RunDone`.
- **dbc web's page keeps the run's status.** A status-less "conn" event set
  "ready on …". `Connected.Relisted`, sent as `relisted` on the wire, makes
  the page leave the status alone.
- **Other tabs on the same connection keep their list** until their own
  Refresh, as the item accepted.
- **Merge into local main.** Main had gained N-147 (a result tab per
  statement). I committed on the branch, merged main into it, resolved the
  conflicts there, re-ran everything, then fast-forwarded main. The
  conflicts: `landRun` now takes both `eff runEffects` and main's
  `rows []landing, over int`; one test call; and both items' Open and Closed
  entries in next-list.

## Changes

- `sqlsplit/sqlsplit.go`: `catalogVerbs` and `ChangesCatalog`, documented
  against `ddlVerbs` / `IsDDL`.
- `workspace/complete.go`: `ddlVerbs` removed. `dropCompletionsAfterRunLocked`
  uses `slices.ContainsFunc(ev.Stmts, sqlsplit.ChangesCatalog)` and explains
  why scripts still always drop.
- `sdb/sdb.go`: `catMu` / `catalog` on S, `noteCatalog`, `CatalogChanged`.
  `logDDL` notes catalog changes whether the log is on or off.
- `sdb/sdbapi/sdbapi.go`: `CatalogChanged` added to `hostOnly`.
- `workspace/events.go`: `RunDone.Relist` and `Connected.Relisted`.
- `workspace/workspace.go`: the `schemaPick` and `ddlSinceCommit` fields.
- `workspace/connect.go`: the `connectKind` type. `landConnect` words and
  status are per kind. `refreshPickLocked` honours a pick in flight, and
  `PickSchema` records it.
- `workspace/run.go`: `runEffects` (wrote / catalog / open / committed,
  filled by `see` for each reached statement) and `commitVerbs`. `landRun`
  decides the relist before the recount. New `relistAfterRunLocked`, with a
  diagram of the transaction case, and `relistLocked`. `RunScript` passes
  `s.CatalogChanged(conn)`.
- `tui/run.go`: `runDone` starts `ev.Relist` alongside `ev.Counts`.
  `tui/tabs.go`: `routeTab` starts a background tab's relist at once.
- `web/hub.go`: `deliver` runs `ev.Relist`, and `connEvent.Relisted` is set
  from the event. `web/static/js/app.js`: a `relisted` "conn" keeps the
  status.

## Tests

- `sqlsplit`: `TestChangesCatalog`, which includes where it differs from
  `IsDDL`.
- `workspace`:
  - `TestDDLRunRelistsTheSidebar`: Relist instead of Counts; mid-connect
    until it lands; the new table listed; words, level, no status;
    `Relisted`; counts on the landing; session kept. Also a SELECT doesn't
    relist, a DROP before a failure does, and a CREATE after a failure
    doesn't.
  - `TestCommitRelistsAfterDDL`: a step table over BEGIN / CREATE / INSERT /
    COMMIT, COMMIT twice, in-run commits, END, ROLLBACK, and another
    connection's mark.
  - `TestRelistGates`.
  - `TestRefreshPickHonoursAPickInFlight`.
  - `TestScriptRelistsAfterItsDDL`: a reading script recounts only, and a
    CREATE script relists and lists the table.
  - `TestRefreshWithoutTablesKeepsTheList` also covers `kindRelist`.
- `tui`:
  - `TestDDLRunRelistsTheSidebar` (Ctrl+R): the table listed, cursor kept,
    the run's status kept, the log line, not left mid-connect.
  - `TestTabBackgroundDDLRelistsAtOnce`: checked to fail with the `routeTab`
    start commented out.
- `web`:
  - `TestDDLRunRelistsTheSidebar`: a "conn" with `relisted`, no status, the
    table listed, the log line.
  - `TestRefreshRereadsTheSidebar` now creates its table through the pool,
    because a DDL run in the tab would relist on its own.
- Full `go test ./...` passes with `CATS_*` removed from the environment,
  before and after the merge. gofmt and vet are clean.
  `DBC_E2E=1 go test ./...` in `web/e2e` passes, before and after the merge.
  Not run by hand: the TUI or dbc web against a live Postgres transaction.
  The transaction rule is covered at unit level only, since shared-cache
  SQLite locks the catalog read during an open DDL transaction.

## Next

Closed: N-149. Declined: None. Raised: None. Deferred: None.
Promoted: None. Moved: None. Updated: None. Full list:
`ai_docs/todo/next-list.md`.
