# Rename the SQLite demo to demo-sqlite

Session: f91a296e-443d-4c09-ba78-e454f42b5501
Date: 2026-09-25

## Ask

One next-list item, pasted as-is:

- **N-009**: "The shipped scripts hard-code the connection name `demo` —
  `scripts/export_report.go`, `scripts/loop_params.go` and
  `testdata/show_two.go` — which is why the SQLite demo keeps the name `demo`
  instead of a symmetric `demo-sqlite`. Only matters if that rename is wanted."

## Decision

The item was contingent on a product call, so the user was asked first:
keep `demo` and close, rename, or rename with a `demo` alias. They chose a
plain **rename, no alias**.

Premise check before asking: the rename alone does not make the scripts
engine-neutral. Their `?` placeholders are SQLite's (bytdb takes `$1`), so
they stay tied to the SQLite demo under any name.

## Changes

- `config/config.go`: `DemoSQLite = "demo-sqlite"`. The comment that
  justified keeping `demo` now explains the rename and the placeholder point.
  Everything that goes through the constant (`demoFallback`, db and config
  demo tests) follows for free.
- Scripts: `scripts/export_report.go`, `scripts/loop_params.go`,
  `testdata/show_two.go` query `"demo-sqlite"`.
- Test harnesses that run shipped scripts name their private SQLite
  connection `config.DemoSQLite`: `newTestManager`/`newTestSession`
  (`main_test.go`, runs `testdata/show_two.go`) and `newTestModelInHost`
  (`tui/harness_test.go`, `TestScriptRunsFromThePicker` runs
  `scripts/loop_params.go`). Knock-on literals updated in
  `tui/chat_test.go`, `tui/session_test.go` and the `│ demo-sqlite │` banner
  checks in `TestScriptHeadlessStreamsInOrder`. Tests using `"demo"` only as
  a label on synthetic results (export, userdata, grid) left alone.
- `config/demo_test.go` comment, `main.go` package doc.
- README: demo table, `--demo sqlite` / `-c demo-sqlite` text, both script
  examples.

Not handled: saved history entries and conversations still carry `demo` as
their connection; it is display-only (history list label, chat row detail),
so nothing breaks.

## Verification

`go vet ./...`, `gofmt -l .` clean; `go test ./...` passes (CATS_* unset).
Through the binary with an empty scratch HOME (no config):
`dbc script scripts/loop_params.go` banners `-- #1 │ demo-sqlite │ 8 rows`,
and `dbc -c demo "SELECT 1"` fails with "unknown connection" — the accepted
break.

## Next

Closed: N-009. Declined: None. Raised: N-041.
Deferred: None. Promoted: None.
Updated: None. Full list: `ai_docs/todo/next-list.md`.
