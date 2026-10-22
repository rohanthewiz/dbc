# dbc web: "New query console" on a connection's right-click menu

Session: `01327515-d4e8-4bd7-96d1-d18b18cfc597`

## Ask

"How do I add a new query console on a new connection? I would like to
right-click on a conn and choose 'New Query console'."

There was no such menu row. Alt+N (`newConsole`) only adds a console to
the tab on screen, and a plain new tab (Alt+T, **+**) opens on the current
tab's connection. So this session added the row.

## Done

- `web/static/js/conns.js`: the connection list's right-click menu has a
  **New query console** row under Connect/Disconnect. It is on every row,
  the active one too (a second console beside the first). It opens a new
  tab, so the tab on screen keeps its connection and any open transaction.
- `web/static/js/app.js`:
  - `newTab(g, opts)`: `opts.conn` overrides the connection the tab
    starts on (otherwise the group's or the tab on screen's, as before).
    `opts.fresh` sets a transient `t.fresh`, which is not saved
    (`tabBody` lists its fields).
  - `followConsole`: a tab with `t.fresh` skips the usual "first console
    no tab here shows" reuse and takes `nextConsoleName`. The flag is
    spent on that first pick, so later connects follow the usual rule.
    Without it, "new" would reopen a leftover console with text in it.
    With consoles off (no `ConsolesDir`), the flag is never read and
    harmless.
  - `dbc.cmd.newConsoleTab(name)` = `newTab(null, {conn: name, fresh: true})`.
  - Group joining is unchanged from a plain new tab: it joins the tab on
    screen's ad-hoc group, or the connection group that claims its
    connection.

## Tests

- New e2e step "new query console from a connection"
  (`web/e2e/newconsole_test.go`), after "consoles per database". It checks:
  - the row is on the active row (lite);
  - picking it on lite2 opens a new tab on lite2 whose console is in the
    editor, empty, and not one of lite2's console files;
  - after the new tab is closed, the first tab is still on lite.
- Before this step runs, "consoles per database" leaves lite2's `console`
  holding text with no tab showing it. That is the case `fresh` has to
  skip. With `fresh: false` the step fails ("timed out waiting for a new
  console of lite2"), so it does catch the case. Restored.
- The first full-suite run failed in the new step's tail: the full suite
  has several tabs open by then, so closing the new tab brings on a
  neighbour, not the tab it came from. The step now waits for the new
  tab's key to leave the strip, then clicks the first tab back.
- Full `DBC_E2E=1` suite passes (Postgres and Docker steps skipped: no
  `DBC_LIVE_PG_DSN`, Docker not running). `go test ./...` in `web` passes.

## Next

Closed: None. Declined: None. Raised: N-222.
Deferred: None. Promoted: None. Moved: None.
Updated: None. Full list: `ai_docs/todo/next-list.md`.
