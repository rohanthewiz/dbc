# Idle connection-group chips: a group whose last tab left stays in sight

Session: `18551a40-b1ae-48d7-aa4b-714e5f0f537a`

## Ask

From the cats-todo backlog, with a screenshot of dbc web (groups ProdDr and
edpdeval on the strip; connections ProdDr, Pilot, edp-dev-allia in the
sidebar): "When I connect to Dev, my pilot group disappears".

## Diagnosis

- A click on a sidebar connection switches the tab on screen to it
  (`app.js` `connect` → POST `/connect`).
- `Pilot` is a CONNECTION group — a rule, "every tab on Pilot". Its only tab
  (Query 2) was switched to `edp-dev-allia`, so it joined `edpdeval` and
  Pilot was left with no tab.
- The strip drew a chip only in front of a group's first tab
  (`tabgroups.js` `chip`), so a memberless connection group vanished.
- Nothing was lost: a copy of `~/.config/dbc/web.bytdb` (read with
  `dbc --driver bytdb --dsn <copy>`) still had
  `{"name":"Pilot","color":1,"conn":"Pilot"}` in the layout's `groups`, and
  the group still claimed the next tab on Pilot — just with nothing on
  screen to say so. Connection groups are kept on purpose (`forgetTab`,
  `dropEmpty`, `restore` all keep them); only the drawing was missing.

## Change

- `web/static/js/tabgroups.js`: `idle()` — connection groups no tab here
  belongs to, in creation order; exported. Design note added: an idle group
  keeps a hollow chip at the strip's end; click opens a tab on its
  connection, right-click is its menu (Ungroup for one no longer wanted). An
  ad-hoc group never idles (`dropEmpty` deletes it). The group menu's
  "Collapse group" says why (no open tabs) for an idle group, unless it is
  already folded, so that state can still be undone.
- `web/static/js/app.js` `renderTabs`: after the tabs, before `+`, a
  `.qchip.idle` button per idle group (tooltip: `describe` + what a click
  does). The strip's click handler opens `newTab(g)` for an idle chip
  instead of folding; the existing `contextmenu` path (`chipGroup` by
  `data-group`) already opens its menu.
- `web/static/css/app.css`: `.qchip.idle` — no fill, text and a 1px inset
  ring in the group's colour.
- Placement at the end (not where the tab used to be) keeps the grouped
  tabs' order and Alt+1…9 untouched, and needs no stored position.

## Tests

- `web/e2e/web_test.go` `tabGroups`, after the reload: make a connection
  group `lite` on the last Query 1, connect it to lite2 →
  `…,Query 1,[lite]`; click the idle chip → Query 3 on lite in the group;
  close Query 3 → idle again; Ungroup from its menu; Query 1 back to lite
  so the rest of the step is unchanged.
- First run hung 10 minutes: rod's
  `p.MustElement("#conns .conn-item[…]").MustClick()` right after a
  `waitConnected` waits forever on a handle the sidebar's redraw detached.
  Switched the two new sidebar clicks to the step's coordinate `at()`
  helper; ran with `-timeout 150s`.
- `cd web/e2e && DBC_E2E=1 go test -count=1 -timeout 150s -v .` — PASS
  (22 s; "tab groups" 2.3 s; Postgres schema picker skipped without
  `DBC_LIVE_PG_DSN`). No Go changes, so no unit-test run needed.
- Memory `web-e2e-harness` gained the MustClick-hang pitfall.

## Notes

- The running `dbc.app` is the installed build; the fix shows after a
  rebuild/reinstall (static assets are embedded).

## Next

Closed: None. Declined: None. Raised: None.
Deferred: None. Promoted: None.
Updated: N-116, N-120. Full list: `ai_docs/todo/next-list.md`.
