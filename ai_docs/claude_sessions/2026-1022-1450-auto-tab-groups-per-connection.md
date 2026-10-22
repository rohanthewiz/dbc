# Automatic tab groups per connection (TUI and dbc web)

Session: `bb03ff8a-7056-47ce-b8ae-a81ed313be24`

## Ask

"Automatically group tabs per connection. Automatically create an
abbreviated name for the group. Allow the user to rename the group."

Both strips already had hand-made connection groups and *Rename group…*
(`tui/tabgroups.go`, `web/static/js/tabgroups.js`). This session adds an
automatic rule on top of them, the same in both.

## Design

| Question | Decision |
|---|---|
| When do groups appear | Once the strip holds 2+ tabs. One tab gets no chip, so the TUI strip stays hidden as before |
| Which connection | The tab's **base** connection: `prod/analytics` joins `prod`'s group |
| Name | `abbrevConnName` / `abbrevConn` (below), then made unique with a digit |
| Lifetime | An `auto` group goes when no tab here is on its connection, so connections passed through don't pile up as idle chips |
| Rename | Clears `auto`: the group is the user's and stays, idle chip and all |
| Ungroup / Make ad-hoc on a connection group | The connection goes into a saved "no auto" set; only a connection group made by hand for it clears it |
| Remove from group | An exclusion. The group stays alive (a tab on its connection in no group still counts), so it is not rebuilt around the tab |

**Abbreviation.** Split the name at anything not a letter or digit, at
lower→upper case changes, and at letter↔digit boundaries, then lowercase.
Names of up to 5 letters and digits stay whole (`prod`, `pg16`, `web-1` →
`web1`). Longer ones take the first word's first 3 letters plus each later
word's initial, keep digit runs whole, and are cut to 8. Examples:
`reporting-mysql` → `repm`, `local-pg` → `locp`, `postgres16` → `pos16`,
`postgres in docker` → `posid`, `myAppDB` → `myad`, `demo-sqlite` →
`dems`. A name with no letters or digits gives `conn`. The 3-letter prefix
was chosen over 4 to avoid unhappy prefixes of common database words.

## Changes

- `tui/tabgroups.go`:
  - `tabGroup.auto`; `autoGroupTabs` (drop dead auto groups, then create
    missing ones), run from `Update` before `arrangeTabs` and at the end of
    `restoreGroups`.
  - Helpers `connClaimed`, `autoDeclined`, `declineAutoGroup`,
    `allowAutoGroup`, `abbrevConnName`, `connNameWords`,
    `savedNoAutoGroup`.
  - `ungroup` and `makeGroupAdhoc` decline the connection. A rename clears
    `auto`. A hand-made connection group re-allows its connection.
    `renameGroupConns` also moves declined names.
- `tui/app.go`: `Model.noAutoGroup`; the Update hook.
- `tui/layout.go`, `userdata/layout.go`: `LayoutTabGroup.Auto` and
  `Layout.NoAutoGroup` (`no_auto_group`) in `tui-layout.json`.
- `web/static/js/tabgroups.js`:
  - The same model: `g.auto`, `off`, `auto()`, `abbrevConn`, and
    `encodeOff`.
  - `restore(value, offValue)`.
  - `ungroup`/`makeAdhoc`/rename/new-group hooks as in the TUI.
- `web/static/js/app.js`:
  - `connBase(conn)`: base found via the Connections list rows.
  - Host `base`; `renderTabs` runs `groups.auto()` before `arrange` and
    saves on change.
  - `saveGroups` writes `groups` and `groupsOff`; boot restores both.
- `web/groups.go`:
  - `tabGroupRec.Auto`, so the cross-window merge keeps the flag.
  - `groupsOffKey`.
  - `moveGroupConns` now also moves `groupsOff` names on a connection
    rename (one rename helper for both).
- README: TUI "Tab groups" section and the web paragraph. TUI help (`?`)
  has an "automatic groups" row.

## Tests

- `tui/tabgroups_test.go`:
  - New: `TestAbbrevConnName` (14 vectors), `TestTabGroupAuto`,
    `TestTabGroupAutoRemoveAndUngroup`, `TestTabGroupAutoRename`,
    `TestTabGroupAutoBaseAndRestore`.
  - The existing hand-made-group tests call `manualGroups` (decline every
    connection) so they still start from no groups.
  - Mutation check: with the Update hook commented out, the auto tests
    fail.
- `web/conns_test.go` `TestConnEdit`: a connection rename moves
  `groupsOff` entries.
- JS parity: a scratch node script ran the same 14 vectors and the auto
  pass against `tabgroups.js`. Not committed.
- Web e2e step "tab groups":
  - Starts from `[lite],Query 1,[lite2],Renamed tab`.
  - Renames `lite2` → `two`, which is saved without `auto`.
  - Ungroups both, and `groupsOff` is saved as `["lite","lite2"]`.
  - The rest of the step is unchanged.
  - The later steps run with lite/lite2 declined.
- Results:
  - `go test ./...` is green.
  - Full web e2e (`DBC_E2E=1`) is green, 62 s; the steps before "tab
    groups" ran with auto groups on.
  - TUI e2e (`DBC_TUI_E2E=1`) is green.

## Next

Closed: None. Declined: None. Raised: N-223.
Deferred: None. Promoted: None. Moved: None.
Updated: N-109. Full list: `ai_docs/todo/next-list.md`.
