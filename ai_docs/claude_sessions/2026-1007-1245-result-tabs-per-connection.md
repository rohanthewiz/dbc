# Result tabs and a log per connection, and sharing a result with the assistant

Session: `3ec45ac2-8cde-4376-8b03-c2feb79b6fe7`

## Ask

1. "Each connection should have its own result set. When I change conns
   show me the result set for that connection. Each result set should hold
   up to (configurable) 10 tabs. Also maintain a message console per conn.
   For each console give me the ability to clear or copy the message
   buffer."
2. Dropped in mid-session from the cats-todo backlog: "We also have to
   have a mechanism for sharing a particular result set with the AI if
   result set sharing is enabled on that conn."
3. "when done, do a /sess-wrap".

## Decisions

The user's answers to the questions asked up front:

| question | answer |
| --- | --- |
| Which UI | both the TUI and dbc web |
| Who owns a result set when two query tabs share a connection | each query tab, per connection (like its session) |
| How runs fill the result tabs | a run reuses the current tab; a pinned tab makes it open a new one |
| A shared tab: replaced by a rerun, or kept? | kept, like a pinned tab ("keep it — shared tab stays like pinned") |

Judgement calls:

- **"Message console" is called the log.** "Console" already means the
  per-database SQL buffers (`consoles/`).
- **"Result set sharing enabled on that conn" is the existing `ai_rows`.**
  That is the setting that lets the assistant see result rows.
- **The run's target tab is picked when the run starts, not when it lands.**
  Browsing other result tabs during a slow query then never gets the
  browsed one overwritten.
- **A run that needs a new tab when every tab is kept is refused up front.**
  "Kept" means pinned or shared, with the set at the cap. Refusing costs a
  click; dropping a pinned result would break the pin. A pin made mid-run
  can still leave nowhere to land; the set then goes one over the cap
  rather than lose a result.
- **The share key is `S` in both UIs.** In dbc web `s` was already the
  grid's sort key. The TUI also takes `s`.
- **The no-connection log is a holding log.** Lines written while on no
  connection move into the next connection's log, so none is stranded.
- **A run or explain that lands after a switch writes to its own
  connection's log.** The log on screen gets one pointer line ("query on X
  finished — its result and log lines are X's"), and the status bar
  changes too (it showed the last "running" tick). I aligned both UIs on
  this after the forks merged: the TUI had logged on screen, while dbc
  web's explain would have loaded the on-screen connection's plan.

## How it ran

- **Core myself first:** config, `workspace`, `ai`.
- **Two forks in the same working tree**, on disjoint directories, with no
  worktree isolation: the UIs needed the uncommitted core. Fork A did
  `tui/`, fork B did `web/`. Each tested only its own packages while the
  other was mid-edit.
- **The sharing request arrived mid-run.** The core half was done here and
  sent to both forks with SendMessage.
- **Docs and the reconciliation after the merge were mine.** That covered
  the `S` key, the off-screen log routing and the README.

## What changed

### Config

`result_tabs` (default 10, `config.DefaultResultTabs`; held to 1…50 with a
warning, `checkResultTabs`). `Config.ResultTabLimit()` also covers a
`Config` built in code. Added to `dbc.example.toml` and the README's config
block.

### Workspace (`workspace/results.go`, new)

- **Per-connection sets.** `sets map[conn]*resultSet` replaces `lastRes`,
  `lastStmt`, `lastErr`, `lastScript`, `plan`, `scriptRes` and `scriptCut`.
  A set holds result tabs, the cursor, the last statement, error and script,
  the plan, and the shared tab. Every "last" accessor reads the active
  connection's set. `LastResultSeq()` gives each result a seq that is
  stable while it stays the same result.
- **Rules.**
  - `targetLocked` picks the tab at run start.
  - `placeLocked` replaces the target, or appends a tab, dropping the
    oldest one that is not kept.
  - `showLocked` puts all of a script's `s.Show` results in one tab
    (`shows`, at most `MaxScriptResults`).
  - `landRun` and `landExplain` write into the run's own connection's set
    (`RunDone.Conn`), even after a switch.
- **API.** `ResultTabs`, `ShowResultTab`, `PinResultTab`, `CloseResultTab`,
  `CloseUnpinnedResultTabs`, `ShareResultTab`, `CanShareResults`,
  `RenameResults`, `DropResults`, `ResultTabLimit`. `ScriptShow` and
  `ScriptPrint` gained `Conn`.
- **Sharing.**
  - One shared tab per set, refused without `ai_rows`; unsharing always
    works.
  - `ChatContext` and `ScriptChatContext` take `views ...GridView`. With a
    share, the shared tab's result goes in place of the caret statement's
    (`attachShared`), beside that statement's own error.
  - Views are matched by `GridView.Result`, so the shared tab's hidden
    columns stay out (`applyView`).

### ai (`ai/prompt.go`)

`Context.Shared`, `SharedLabel` and `SharedFrom`. `Build` frames a shared
result with "The user shared a result with you (result 2), from a different
statement: …". It sends the shared result even beside an error, and the
note reads `shared result 2: 10 of 25 rows`. With `ai_rows` off, only the
column names go and the "rows not sent" hint stays.

### TUI (fork A)

- **`tui/resulttabs.go` (new).** `syncResults` is idempotent; it is called
  after events and once per frame. Grids are parked per connection and
  tab id, plan views per connection.
- **Strip.** Drawn on the results pane's bottom border, with `⚑` for
  pinned and `✦` for shared, falling back to a compact `‹ k/N ›`.
- **Clicks and menus.** A click shows a tab. A right-click gives a menu
  with pin, share, close and close unpinned; the grid's menu has the same
  rows.
- **Keys.** `{` `}` `P` `S`/`s` `x` in the grid and the plan view.
- **`tui/logs.go` (new).** A log per connection; m.log writes to the one on
  screen. The holding log is folded into the next connection's. The title
  reads `Log · <conn>`, with `⧉ copy` / `✕ clear` on it, and `y` / `x`
  work with the log focused.
- **Connection form.** A rename or remove moves or drops results and logs,
  derived `<conn>/<db>` names included.
- **Off-screen runs (my reconciliation).** `landedHere`, `notesTo` and
  `logPlan(conn, …)` send an off-screen run's or explain's lines to its own
  log.

### dbc web (fork B)

- **Seq.** The grid's seq is the workspace's (`LastResultSeq`), and
  `rvSeq` is gone. A stale seq on a copy or export still gets a 409.
- **Plans.** Kept per connection (a `tab.ps` map).
- **`web/resulttabs.go` (new).** `resultTabsState` (resultTabs, resultTab,
  resultTabMax, canShare) rides on wsState, run, result and conn events.
- **Endpoint.** `POST /api/v1/ws/:id/result-tab {id, op}`, for show, pin,
  unpin, close, close-unpinned, share and unshare.
- **Logs.** Log lines carry `conn`.
- **Rename/remove.** Each tab tracks the connections it has visited
  (`tab.visited`), so `moveResults` can rename or drop them.
- **Chat.** The page sends the shared tab's kept view (`chatReq.Shared`),
  and `sharedView` rebuilds it as a second `GridView`.
- **Page.**
  - Strip `#rstrip`: number, `⚑`, `✦`, title, `×`, and an n/max count.
  - Grid snapshots are kept per seq, capped at 64.
  - Keys `{` `}` `P` `S` `x`.
  - A per-connection log in `core.js`, with a "Log · conn" header and
    ⧉ Copy / ✕ Clear. A script tab has its own log.
- **Behaviour change.** Disconnect sends its `conn` event before its notes,
  so "disconnected from X" goes to the holding log.
- **Off-screen runs (my reconciliation).** `offScreen()` in `app.js`; an
  explain landing on another connection no longer loads the on-screen
  plan.

### Docs

- **README.**
  - New section "Result tabs and the log, per connection".
  - A "Sharing a result" paragraph under the AI assistant.
  - Rows in the TUI's mouse and keys tables and in dbc web's keys table.
  - Script results "in the one result tab the run lands in".
- **`.claude/skills/dbc/scripting.md`.** The stale "the TUI keeps only the
  last" is gone.

## Verification

- **Merged tree:**
  - `gofmt` clean; `go build ./...`, `go vet ./...` and `go test ./...` all
    pass.
  - TUI e2e (`DBC_TUI_E2E=1`): pass, 9s. Its connection-switch step now
    checks that lite2's results are empty, the `Log · lite2` title, and
    lite's result, strip label, sort and log coming back.
  - Web e2e (`DBC_E2E=1`): pass, 39s, with a new step "result tabs and logs
    per connection".
- **Fork B's own runs:**
  - Its first web e2e run failed in the script-tabs step. `waitResult`
    assumed seqs only grow, and now waits for a different seq.
  - The next 3 runs passed.
- **New tests:**
  - `workspace/results_test.go`: 9 tests, including `TestShareResultTab`.
  - `ai` `TestSharedResult`.
  - `config` `TestLoadResultTabs`.
  - `tui/resulttabs_test.go`: 14 tests.
  - `web/resulttabs_test.go`.
- **Not driven end to end:** an actual share. Neither e2e harness has an
  `ai_rows` connection, so only the disabled share is checked there
  (N-145).

## Next

Closed: None. Declined: None. Raised: N-145, N-146, N-147, N-148.
Deferred: None. Promoted: None.
Updated: N-115. Full list: `ai_docs/todo/next-list.md`.
