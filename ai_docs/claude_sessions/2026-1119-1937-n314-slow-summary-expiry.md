# N-314: a slow schema summary is remembered for 15 minutes, not the whole run

Session: `919a0bf6-8e3f-46ea-8c60-5ea20d62a8af`

## Ask

"Next list item N-314". N-311's adaptive default remembered a timed-out
schema summary for the rest of the process, and a Refresh never retried
it. Both were the session's calls, not the user's. The item offered three
fixes: let a Refresh retry, expire the memory after some minutes, or drop
it on a Disconnect. The user was asked and chose **expiry**.

## What changed (`0191e1f`)

- **The memory lapses.** `db.Manager.slowSummary` is now
  `map[string]time.Time`. `SummarySlow` reports true only within
  `slowSummaryTTL` (15 min, a var so a test could shorten it) of the
  mark, and deletes a lapsed entry. `MarkSummarySlow` on a second timeout
  restarts the window. `db.SlowSummaryTTL()` exposes it for the note.
- **What stays.** Within the window a Refresh does not retry, and a
  Disconnect keeps the memory. `Drop` (an edit of what the connection
  dials) and a restart end it early.
- **Why 15 minutes, not `complRetry`'s 5.** A lapse on a big catalog is a
  5 s wait the user sits through (a connect or a Refresh). A retried
  completion load runs behind the editor.
- **The note** reads "… is read by schema names for the next 15 minutes …"
  (was "from here on"). Comments in `workspace/introspect.go` and
  `workspace/connect.go` updated, and `db/navigate.go` has a timeline.
- **Docs:** README "`full` adapts" and the `introspect` comment in
  `dbc.example.toml`.

## Tests

- New `db.TestSummarySlowLapses`: an entry is kept inside the window, and
  is lapsed and removed past it. A second mark restarts the window.
  Timestamps are backdated, so the test does not sleep.
- `TestLiveWorkspaceSlowSummaryRemembered` and its neighbours check the
  new note text.
- The full suite is green with `DBC_LIVE_PG_DSN` against a throwaway
  Postgres 17.

## Notes

- The first commit's message had no blank line after the subject. It was
  amended before any push.

## Next

Closed: N-314. Declined: None. Raised: None. Deferred: None.
Promoted: None. Moved: None. Updated: None.
Full list: `ai_docs/todo/next-list.md`.
