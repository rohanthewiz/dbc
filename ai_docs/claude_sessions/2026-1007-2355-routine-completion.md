# Autocomplete for stored functions and procedures

Session: `f019975a-d6d4-414f-b839-086c81af8a1e`

## Asks

1. From the cats-todo backlog: "Autocomplete on schema functions and
   procedures too. Cache if necessary."
2. `/sess-wrap`: save this doc, commit all, push.

## What was built

Completion now suggests the database's own functions and procedures as
well as the dialect's built-ins. They are read in the same load as the
schema and kept in the same cache (`workspace.complState.routines`), so
they share its generation. A connect, a disconnect or a DDL run
(`CREATE FUNCTION` included) drops both together.

| Where | Offered | Inserted as |
| --- | --- | --- |
| `SELECT ▮`, `WHERE ▮`, `= ▮` | functions, aggregates, window functions (rank +3, beside the built-ins) | `ref(▮)`, or `ref()▮` when no overload takes an argument |
| `FROM ▮` / `JOIN ▮` | functions that return rows (`SETOF …`, `TABLE(…)`), rank 3 | `ref(▮)` |
| `CALL ▮` (first word only) | procedures | `ref(▮)` |
| `DROP`/`ALTER`/`CREATE [OR] REPLACE`/`ON` + `FUNCTION`/`PROCEDURE`/`ROUTINE ▮`, `DROP FUNCTION IF EXISTS ▮` | by keyword | the name, no parens |
| `EXECUTE FUNCTION ▮` / `EXECUTE PROCEDURE ▮` | trigger functions first, then other functions | `ref()` |
| `schema.▮` | that schema's routines, of the kind the word before the qualifier asks for | the bare name |

- **Overloads** are one suggestion. The detail is the first signature plus
  "+N overloads", and the doc lists every signature.
- **Qualifying.** The bare-or-qualified rule is the tables' rule, applied
  over the routines' schemas (`routineBare`). Built-ins in `pg_catalog`
  are not checked for shadowing.
- **Dedup with built-ins.** User routines are added after the built-ins.
  A name that is both is offered once, as the built-in, since `add`
  dedupes on Kind+Insert.

### Catalog read (`db/routines.go`)

- **Postgres.** `pg_proc` with `prokind` (Postgres 11+),
  `pg_get_function_arguments` and `pg_get_function_result`. It skips
  `information_schema` and every `pg\_%` schema, and narrows to the scope
  schemas on a scoped load.
- **MySQL.** `information_schema.routines`, with the parameters joined in
  through `GROUP_CONCAT`. The parameter mode is shown for procedures only.
  MySQL 8.4 reports `IN` for function parameters too, which the live test
  caught.
- **SQLite and bytdb.** An empty query and no error. bytdb serves no
  `pg_proc`.
- **`BuildRoutines` filtering.** Drops routines that return `internal`,
  `cstring` or a `*_handler` type, and routines that take an `internal` or
  `cstring` argument (`hasServerArg`). These are done in Go, because a
  cast to a handler type the server lacks would fail the whole query.
  Functions returning `trigger` or `event_trigger` become
  `RoutineTrigger`. With pg_trgm installed on postgres:17, 31 functions in
  public came down to 16, with the `gtrgm_*` internals gone.
- **Best effort.** Routines are read only after the schema read succeeds,
  so a down server does not wait out a second timeout. A failure leaves no
  routines and is not reported.

## Changes

- `model/routine.go` (new). `Routine` and `RoutineKind`
  (function/procedure/aggregate/window/trigger). They live in `model` so
  that `db` and `sqlcomplete` need not import each other.
- `db/routines.go` (new). `RoutinesQuery`, `routinesQuery(scope)`,
  `BuildRoutines`, `hasServerArg` and `Manager.Routines`.
- `sqlcomplete/routines.go` (new):
  - `rgroup` (overload groups) and `indexRoutines`.
  - The filters: `callable`, `procedures`, `rowSources`, `anyRoutine`,
    `notProcedures` and `triggersFirst`.
  - `routineSuggestions`, `routineDetail`/`routineDoc`, and
    `routineRef`/`routineBare`.
  - `routineAfter` and `routineLeads` (a column named `call` or `function`
    is not taken for the keyword), and `qualifiedRoutines`.
- `sqlcomplete/complete.go`. New `KindProcedure`, `Request.Routines` and
  `completer.manySchemas`. Hooks in `suggest` (the qualified branch and a
  routine-name check before the switch), `tables` and `expression`.
- `sqlcomplete/vocab.go`. `CALL` added to the Postgres and MySQL
  statement starts.
- `workspace/complete.go`. A `routines` field in the cache, `readRoutines`
  with its own `CompletionTimeout`, and a "THE ROUTINES" section in the
  doc comment.
- `tui/complete.go`. A procedure's glyph is `λ`.
- `web/static/js/editor.js`. `procedure` maps to Monaco's `K.Method`.
- `README.md`. The Completion section: new table rows and a paragraph on
  routines.

## Tests

- `sqlcomplete/routines_test.go`. Eight tests: expression, search path,
  CALL (and `SELECT call ▮` offering none), FROM, DDL contexts including
  both EXECUTE forms, qualified, no routines, and one schema (MySQL shape).
- `db/routines_test.go`. `TestBuildRoutines`, `TestHasServerArg` and
  `TestRoutinesQuery`.
- `workspace/live_routines_test.go`. `TestLiveWorkspaceCompletionRoutines`
  (Postgres: overloads, a schema off the path, `schema.`, CALL, FROM,
  EXECUTE FUNCTION, and a drop and reload after `CREATE FUNCTION`) and
  `TestLiveWorkspaceCompletionRoutinesMySQL`. Both pass on postgres:17 and
  mysql:8.4 (throwaway containers from the live-db recipe, stopped
  afterwards).
- A full `go test ./...` with both live DSNs and `CATS_*` removed. All
  pass except `TestLiveWorkspaceNotices`, which also fails on a clean HEAD
  worktree. It was broken by N-155's write lines and is raised as N-156.
- Not run: the real binary, or a browser (N-157).

## Next

Closed: None. Declined: None. Raised: N-156, N-157. Deferred: None.
Promoted: None. Moved: None. Updated: None. Full list:
`ai_docs/todo/next-list.md`.
