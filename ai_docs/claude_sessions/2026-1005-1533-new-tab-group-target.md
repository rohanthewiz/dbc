# dbc web: show where a new tab lands, and open one in any group

Session: `67fb581b-c74b-445e-82a6-6245e0669fb6`

## Ask

From the cats-todo backlog, with a screenshot of a strip holding two groups
(`ProdDr`, `Pilot`) and the + after them: "When adding a new tab it is not
clear under which connection/db group this tab is being added for. I may
want to add a new tab to the leftmost db group." Then: cut a minor release.

## What was done

A plain new tab (+ or Alt+T) opens on the active connection, beside the tab
on screen. It joins that tab's ad-hoc group, or whichever connection group
claims the connection. Nothing on screen said which group that was, and the
only way to add a tab to another group was to switch to a tab in it first.

- **The + is marked** (`app.js` `markNew`, run at the end of every
  `renderTabs`). It takes the landing group's `gN` colour and the same
  underline its tabs have (`.qnew.grp` in `app.css`). Its tooltip names the
  group and the connection, e.g. "New query tab in group Pilot on pilot
  (Alt+T) · right-click picks another group". It goes back to plain grey
  when no group would take the tab.
- **Right-click on +** (`openNewMenu`) lists the groups in strip order, left
  to right (`groups.ordered()`), with ● on the one a plain click would use.
  The last row is the plain click itself ("Beside <tab> (<group>)", Alt+T).
- **The group menu** (right-click a chip) has a new row, "New tab in <name>".
- **`newTab(g)`**: a tab opened into a group starts on that group's
  connection (`groups.connFor`). For a connection group that is its own
  connection. For an ad-hoc group it is the connection of the tab on screen
  if that tab is a member, otherwise the last member's. The new tab goes
  after the group's last tab, and `groups.place` (that is, `add`) puts it in
  the group, so a deeper connection group cannot take it. Alt+T and a plain
  click on + behave exactly as before.
- **`groups.landing(fromTab, conn)`** answers "which group would a plain new
  tab join". It checks the tab on screen's ad-hoc group first, then probes
  the connection groups with a keyless tab. A fresh key is in no group's
  exclusion list, so the answer holds even when the tab on screen was itself
  taken out of that connection group.
- The F1 key list has a new line for "right-click +". The design note at the
  top of `tabgroups.js` explains the choice.

## Verification

- `web/e2e` tabGroups step, extended (`DBC_E2E=1 go test -count=1 .`, CATS_*
  stripped):
  - the + is marked for wip;
  - right-click + → lite2 puts Query 3 in the lite2 group and connects it
    to lite2, and the + switches to lite2;
  - the wip chip's "New tab in wip", used from a lite2 tab, puts Query 4
    after Query 2 on lite;
  - the two tabs are then closed, so the rest of the step runs unchanged.
- Results: the whole e2e suite passes, and so does `go test ./web/`.

## Release

The version was bumped by hand to 0.8.0 (`version/version.go` and
`cats-plugin.toml`), then `release` was fast-forwarded to main and pushed.
The release workflow takes a hand-edited version as it is, tags `v0.8.0`
and runs goreleaser.

## Next

Closed: None. Declined: None. Raised: None.
Deferred: None. Promoted: None.
Updated: N-116. Full list: `ai_docs/todo/next-list.md`.
