# Failed connects keep a tab in its group; a connection click never types into a console

Session: `6945d79a-7739-4981-8121-577e9487b048`

## Ask

From the cats-todo backlog:

1. "Take a deep look at the way tab grouping works. When opening a group
   (esp for a postgres in docker connection) orphan tabs appear. This
   _could_ be due to docker engine not fully running on the machine.
   Anyway check all the things around groups and perhaps lock down some of
   these features with tests."
2. "Also there was a point where simply clicking on a connection caused
   text to be generated into the current console. Double check all the
   things around this also and lock down with tests."
3. `/sess-wrap`.

## Findings

### Orphan tabs (both UIs)

- **The main cause: a tab's FIRST connect failing.** `workspace.New` names
  the config's default as active, so a UI can draw it before any connect,
  and it keeps naming it when that connect fails. A Postgres in Docker that
  is down, or still starting, fails at once or after a hang.
  - Both UIs then read the tab as on the default. It left its connection
    group, beside it as an orphan or into the default's group.
  - It was saved on the default, so the next start kept it out of the
    group.
  - In dbc web the failed `"conn"` event also carried the default's sidebar
    and console. `followConsole` swapped the default database's console into
    the tab, so another database's SQL appeared in its editor.
  - In the TUI this hit:
    - new tabs (`newTabIn`, the idle chip, "New tab in");
    - restored tabs on their first look, and the tab on screen at startup
      (`Init`);
    - a member looked at through a folded group's menu, which popped out
      beside the chip.
- **dbc web, mid-dial.** A tab brought back on screen while its first
  connect was still dialing went through `applyState`, which took
  `st.active` (the default) as the tab's connection. The same orphan, for as
  long as Docker made it wait.
- **dbc web, a closed tab coming back.** `reallyClose` brings the neighbour
  on with `activate`, and `activate` saved the tab it left: the closed one.
  - That `PUT /api/v1/tabs/<key>` raced the close's `DELETE`. Landing
    second, it put the tab back on the server, and the next boot claimed it.
  - Its ad-hoc group membership was already dropped (`forgetTab`), so it
    came back outside its group.
  - `showConsole` could do the same after its awaits (a slow connect's
    console swap).
  - This failed the existing e2e "tab groups" step once during the session
    (`Query 4` back after the reload).
- **dbc web, a group whose connection was removed.** "New tab in" or the
  idle chip opened a tab that `activate` connected elsewhere, outside the
  group. The TUI already refused this; the web now does too.
- **Related, "reads as on the default":**
  - dbc web's `tab.on` / `tabsOn` counted a never-connected tab as on the
    default. That gave the default's row an in-use mark and kept it from an
    edit or a removal.
  - `Refresh` on such a workspace quietly connected the default.
  - The TUI's sidebar marked the default ●.
  - The TUI's log for such a tab was the holding log, which `syncLog` folded
    into whichever connection's log was shown next, another tab's included.

### A click on a connection typing into a console (dbc web)

- Reproduced. A script tab is `state.tab` from the moment it is clicked,
  but `activate` awaits the file read before `useDoc`. Meanwhile the editor
  still shows the tab left, a console.
- A click on a connection then ran the script tab's
  `dbc.editor.insert(JSON.stringify(name))` into that console. The e2e probe
  on the old page got `"\"lite2\"SELECT 'typed on lite'"`.
- Pipeline and job tabs in JSON view had the same window.
- The TUI never types on a connection click. It switches, and the console
  follows the database. A test now holds it to that.

## Changes

### workspace

- `Workspace.Connected()` (`landed`): a connect has landed and no
  `Disconnect` followed. Set in `landConnect`, cleared in `Disconnect`.
- `Refresh` refuses (`NoConnection`) on a workspace that never connected.

### web (server)

- `connEvent.Connected` and `wsState.Connected` are both `ws.Connected()`.
  `wsState.connected` used to mean "catalog loaded"; its only reader,
  `activate`, wants "on a connection".
- `sidebar` gives a console and a sidebar (Base, Navigable…) only when
  connected.
- `tab.on` and `tabsOn` count Active only when connected.
- `tab.notes` goes unstamped for a never-connected workspace, to the log on
  screen.

### web (page, `app.js`)

- `firstFailed(d)` / `onNowhere`: on a failed first connect the tab keeps
  `t.conn` (its group, saved place and console), and the page shows "not
  connected — the connect to X failed". `onBackground` does the same.
- `applyState`: an unconnected workspace keeps `t.conn`, and shows
  "connecting to X…" while dialing.
- `activate` doesn't save a tab that is no longer in `tabs`. `showConsole`
  returns early for a tab closed during its awaits.
- `typeConn`: a connection click types only into the tab's own document
  (`dbc.editor.docKey() === docOf(t)`), else says "still opening".
- `newTabConn()` is shared by `newTab`, `markNew` (the +) and
  `openNewMenu`, so the + names the group Alt+T really uses.
- `newTab(g)` refuses a connection group whose connection is gone.

### TUI

- `queryTab.aim`: the connection an unconnected tab was last sent to.
  - Set by `aimAt` in `connectCmd` and `setActive` before the job runs, so
    there is no window where the tab reads as on the default.
  - `tabAim` (`tabTarget` or the aim) feeds `tabConn` (groups),
    `savedTabs` and `newTabConn`.
  - In-use checks still use `tabTarget`, so a failed tab never blocks
    Remove or Stop container.
- `unconnected` is now set for every tab opened (`newTabIn`) or restored
  (`restoreTabs`), until its first connect lands.
- `arrive` retries the aim on each look, as a restored tab connects on its
  first.
- `refreshCatalog` on an unconnected tab retries its aim.
- Disconnect clears the aim. A rename moves it, and a removal clears it.
- `refreshConns`: no ● for an unconnected tab. `newTabIn` now refreshes the
  list, as `arrive` does.
- `logKey`: an unconnected tab shows its aim's log, so "connect failed"
  stays with that connection. `logTo` compares against `logKey()`.

### Docs

README: a sentence in each "Tab groups" section (TUI and web), and the
script tab's click-to-type bullet.

## Tests

Each new test was run against the old code and failed there, except the
two lock-downs of behaviour that was already right (a failed switch, the TUI
click).

- `workspace.TestConnectedOnlyOnceAConnectLanded`.
- `web.TestFailedFirstConnectIsOnNothing`: no console or sidebar, wsState,
  in use, the Refresh refusal unstamped, and a failed switch staying put.
- TUI (`tabgroups_test.go`, the `brokenGroupModel` helper over
  consoleModel's `broken` connection):
  - `TestTabGroupKeepsATabWhoseFirstConnectFailed`, including the gap
    between the job and Update, the ●, the editor, + and save;
  - `TestTabGroupKeepsABackgroundTabWhoseConnectFailed`: mid-dial
    away/back, the failure in the background, its lines kept out of another
    tab's log, the retry on look;
  - `TestTabGroupRestoredTabsWhoseConnectFails`;
  - `TestTabGroupKeepsATabWhoseSwitchFailed`;
  - `TestTabGroupFailedTabFollowsRenameAndRemove`;
  - `TestTabGroupFoldedMembersWhoseConnectFails`.
- `tui.TestClickingAConnectionNeverTypesIntoTheEditor`: single and double
  clicks by mouse, including on the failing connection.
- New e2e step "connection clicks and failed connects"
  (`web/e2e/connclicks_test.go`, `DBC_E2E_STEPS=clicks`):
  - console text across clicks, and the script-opening window (a delayed
    fetch);
  - a `blackhole` listener as Docker's half-up port, for the mid-dial
    away/back, then the failure;
  - an immediate failure in the group, + marked, saved, through a reload;
  - `closeOnScreen` (no PUT after the close, gone from the server);
  - a removed connection's idle chip refusing;
  - cleanup.
- e2e harness `clickAt`: once the strip overflows, headless Chrome's
  overlay scrollbar hit-tests over the lower half of every tab and chip, so
  a centre click hit nothing. The helper now picks a point that
  `elementFromPoint` maps to the target. This is why the step passed alone
  but failed in the full suite.

## Verification

- `gofmt`, `go vet ./...` clean.
- `go test ./...`: all pass except `tutorial` (see N-211, pre-existing:
  `tutorial/workspace/dbc.toml` is gitignored and was never committed).
- `DBC_TUI_E2E=1 go test ./tui/e2e` passes.
- `DBC_E2E=1 go test ./web/e2e` passes in full, twice. Docker and the live
  Postgres steps skipped as usual.

## Next

Closed: None. Declined: None. Raised: N-211, N-212, N-213.
Deferred: None. Promoted: None. Moved: None.
Updated: None. Full list: `ai_docs/todo/next-list.md`.
