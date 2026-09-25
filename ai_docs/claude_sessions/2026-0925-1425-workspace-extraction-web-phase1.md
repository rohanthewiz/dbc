# dbc web, Phase 1: the `workspace` extraction

Session: bd499dac-fe3d-401e-b1f2-47e65e28b8b8
Date: 2026-09-25

## Ask

"Start on the plan at `ai_docs/plans/web-ui.md`" — the `dbc web` browser UI.
Phase 1 of that plan: pull the TUI's application logic out of `tui.Model` into
a UI-agnostic `workspace` package, changing no behavior, so the terminal and
the browser share one set of rules. Mid-session, two more asks: make the web's
explain carry **all** the features of the existing HTML plan page, and run the
live Postgres/MySQL tests (recipe supplied, and noted).

The plan's other forks (loopback + per-launch secret, SSE + POST, Monaco,
bytdb for web-only state, port 8450) only matter from Phase 2 on and were not
revisited; Phase 1 follows the plan's recommended "extract a workspace
package".

## What was built

### `workspace/` (new)

| File | Holds |
|---|---|
| `workspace.go` | `Workspace` (cfg, mgr, history; `mu` for bookkeeping, `sessMu` for the pinned session), `New`, readers (`Active`, `Catalog`, `TableIndex`, `Busy`, `RunTag`, `Ticking`, `RunningStatus`, `LastStmt`, `LastErr`, `LastResult`, `Plan`, `Session`, `Connecting`), `Stop`, `Close`; the package doc with the Job diagram and the lock-order rule |
| `events.go` | `Note`/`Level` (the log words), `Refusal`/`Reason` (Busy → 409, NoConnection / Nothing / Invalid → 400), `Job`, `Start`, the events (`Connected`, `RunDone`, `ExplainDone`, `ScriptShow`, `ScriptPrint`, `SessionReleased`), `ResultStatus`, `Preview` |
| `pick.go` | `Editor{Text, Caret, Selection}`, `Pick` (Ctrl+R), `PickAll` (Ctrl+Shift+R), `StmtRange` (gutter marker) |
| `connect.go` | `Switch`, `Connect` (supersede by generation, cancelable), `landConnect`, `CancelConnect`, `releaseJob` |
| `run.go` | `RunEditor`, `RunStmts` (record then run — recorded even when refused), `ListTables`, `Run`, `RunScript`, the run slot, `landRun` (incl. `explain.Detect` of a typed EXPLAIN → `RunDone.Plan`), `failedLocked`, `Cancel` |
| `explain.go` | `ExplainEditor`, `Explain`, the analyze warning, `landExplain`, the plan-for-chat cache |
| `session.go` | `onSession` (retry once / drop / lost via `Session.Classify`), `runOnSession` |
| `chat.go` | `GridView`, `ChatContext` (the data rule, hidden columns, sort order), `MaxSchemaTables` |
| `workspace_test.go` | 17 tests: picking, landing, busy refusal (and history recorded anyway), refusals, straggler drop, stop-at-first-failure, cancel mid-run, typed EXPLAIN → plan, explain landing, session across runs, dead stateless session retried, dead stateful session lost, switch releases the old session, cancel connect, newer connect supersedes, chat context, script events through the sink (`testdata/show_two.go`) |

### The one design change from the plan

The plan drafted `Events() <-chan Event`. Built instead: a start method does
the immediate bookkeeping and returns `Start{Tag, Gen, Notes, Job}`; the
**Job** is the blocking half, run by the caller, and it **lands its own
outcome** under `w.mu` before returning the event. The TUI wraps a Job in a
`tea.Cmd` and routes the event (`*workspace.RunDone`, …) in `Update`; the web
will run Jobs on goroutines and write events to SSE. Mid-run script output goes
to `Options.Sink`. Reason: Bubble Tea is already an event loop, and the TUI's
harness drives it synchronously — a channel owned by the workspace would have
made every TUI test racy. Recorded in the plan.

Lock rule: `mu` is short-held; `sessMu` is held for a statement's duration;
never take `sessMu` under `mu`. The release of an old session on a switch is a
separate Job (`Connected.Release`) because it waits on `sessMu`.

### TUI

- `tui/run.go` rewritten as glue: `job()` (Job → tea.Cmd), `editorState()`,
  `note`/`notes`/`refused`, `startRun`, and thin `connectCmd`, `setActive`,
  `runQuery`, `runAll`, `listTables`, `runScript`, `tick`, `runDone`,
  `showResult`, `cancelRun`.
- `tui/explain.go`: `explainQuery`/`explainStmt`/`explainDone` delegate;
  `maybePlan`, `planForChat` and `onSession` moved out.
- `tui/app.go`: the run/connect/session/last* fields replaced by
  `ws *workspace.Workspace`; routes the workspace's events; `shutdown` is
  `ws.Stop` … `ws.Close`.
- `tui/chat.go`: `chatContext` passes the grid's view to `ws.ChatContext`.
- Field reads in `cats.go`, `layout.go`, `menu.go`, `modals.go`,
  `sidebar.go`, `chatarchive.go` became accessor calls; the table preview goes
  through `ws.RunStmts`; `planView`'s chat cache moved to the workspace.

### Other

- `userdata.History` gained a mutex (several web workspaces will share one).

## Verification

- `go vet ./...`, `gofmt -l .` clean.
- `go test ./...` green; `go test -race ./tui ./workspace` green;
  `go test -count=5 ./workspace` green.
- TUI test assertions all unchanged. Reads of moved fields became accessors
  (`m.lastRes` → `m.ws.LastResult()`); the two tests that poked the session
  directly moved to `workspace` (tui count 113 → 111).
- Live tests with the user's docker recipe (Postgres 17, MySQL 8.4):
  `go test -count=1 ./db -run Live -v` — 13/13 pass. Recipe saved to memory
  and to the plan's Phase 1 outcome. Readiness wait that works in this
  environment: loop on `pg_isready` / `mysql -e 'SELECT 1'` via `docker exec`,
  pausing with `perl -e 'select(undef,undef,undef,1)'` (ready in ~4 s).
- Checked: the on-screen plan is only ever set where the workspace sets its
  plan, so the assistant's plan context is unchanged by the move.

## Plan doc updates (`ai_docs/plans/web-ui.md`)

- Phase 1 marked done, with its outcome and the live-test recipe.
- The `workspace` API section rewritten to what was built.
- **Phase 4 rewritten as an explicit parity checklist with the HTML plan
  page** (header, metrics 1–4, graph pan/zoom/fit/fold/path, keyboard walk,
  flame with zoom and breadcrumbs, full step detail, insights with Copy and
  click-to-select, boot on the worst finding, `#flame`, theme), the page's
  script to be lifted into one `plan.js` shared by the standalone page and the
  web so they cannot drift, plus the workbench extras (re-explain with
  before/after comparison, ⤓ insert, ask the assistant, open standalone).

## Next

Closed: None. Declined: None. Raised: N-045, N-046, N-047, N-048.
Deferred: None. Promoted: None.
Updated: None. Full list: `ai_docs/todo/next-list.md`.
