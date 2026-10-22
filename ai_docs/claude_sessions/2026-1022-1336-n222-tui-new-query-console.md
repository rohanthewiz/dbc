# TUI: "New query console" on a connection's right-click menu (N-222)

Session: `01327515-d4e8-4bd7-96d1-d18b18cfc597` (continues
`2026-1022-1322-new-query-console-from-conn`)

## Ask

1. Cut a tag-only patch release v0.13.2 after the web feature.
2. "do N-222 in the TUI too": add the web's connection-menu **New query
   console** to the TUI.

## Release v0.13.2 (`38058e7`)

Tag-only patch, as for v0.13.1: version bumped in `version/version.go` and
`cats-plugin.toml` (`TestVersion` passes), committed as
`Release v0.13.2 [skip ci]`, annotated tag `v0.13.2`. Pushed main and the
tag, not `release`, so there are no goreleaser archives. Changes since
v0.13.1: the web's New query console row, and smaller gutter fold arrows.

## Done: N-222

- `tui/connform.go` `connMenuItems`: on a right-clicked row,
  **New query console on <conn>** leads the connection's rows (before
  Edit/Remove). It is on every row, the one the tab is on too. Its `why`
  is `tabsFullWhy()`, so with 9 tabs it is dimmed and says why.
- `tui/tabs.go`:
  - The body of `newTabIn(g)` moved to `openNewTab(g, onConn, fresh)`;
    `newTabIn` wraps it, so ⌥T, group tabs and the + chip are unchanged.
  - `newConsoleTab(conn)` = `openNewTab(nil, conn, true)`. A connection
    removed while the menu was open is refused with a log line rather than
    opening a tab that connects to nothing.
  - `tabsFullWhy()` is the 9-tab refusal text, shared by the log and the
    menu row.
  - `openTabConsole(conn, fresh)`: with `fresh` it skips the "first console
    no other tab shows" reuse and takes `NextConsoleName` over the
    database's files plus the other tabs' consoles (which may have no file
    yet).
  - The console is set before the connect, so the connect's landing
    (`switchConsole`) finds the editor already on that database and swaps
    nothing. The fresh pick needs no flag carried to the landing, unlike
    the web's `t.fresh`.
  - Group joining is a plain new tab's (`joinNewTab`), as in dbc web.
- `README.md`: the TUI's connections-menu table row, the TUI consoles
  paragraph, and the web consoles paragraph now mention the row. The web
  side had been missing it since v0.13.2.

## Tests

- `tui/tabs_test.go` `TestNewQueryConsoleFromConnMenu`. Setup: b gets a
  console holding "SELECT 'left on b'" that no tab shows (the one a plain
  ⌥T would reuse). It right-clicks b's row in the Connections pane, picks
  the row, and checks:
  - a second tab, on screen, on b;
  - a new, empty console of b's database, not b's old one;
  - the first tab is still on the demo connection;
  - with 9 tabs, the row's why reads "tabs is the most".
- With `fresh` off it fails ("new tab's console \"console\" … editor
  \"SELECT 'left on b'\""), so it does test the new-console behaviour.
  Restored.
- `go test ./...` passes; the TUI e2e suite (`DBC_TUI_E2E=1`) passes.

## Next

Closed: N-222. Declined: None. Raised: None.
Deferred: None. Promoted: None. Moved: None.
Updated: None. Full list: `ai_docs/todo/next-list.md`.
