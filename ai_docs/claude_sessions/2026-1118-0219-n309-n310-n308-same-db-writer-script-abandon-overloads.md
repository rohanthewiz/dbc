# N-309, N-310, N-308: same-database script loads, abandoning a stuck script, routine overloads in order

Session: `cbde2eba-f688-4850-8468-0b28475b5b48`

## Ask

`/work-next-list`, the ninth batch. The triage is in
`2026-1118-0042-n321-n286-n288-n291-plain-errors-value-text-status-error`.

## N-308 (updated): routine overloads in a fixed order (`2d9d4df`)

The Postgres routines query (`db/routines.go`) ordered by schema and name
only, so overloads came back in storage order. It now orders by
`p.pronargs`, then the argument text: completion's first form is the one
with the fewest arguments, on every server.

`TestLiveWorkspaceCompletionRoutines` passes 6 runs on postgres:17. It
passed before the change too; it depended on luck.

What remains in N-308 is unexplained: the `tui` "pipeline slow" failure,
the web e2e console-switch one, and `TestLiveDesigner` under parallel live
packages.

## N-309: a script's Writer under the same-database rules (`7863ecb`)

**Reproduced first.** New live `sdb.TestLiveReaderWriterSameTable` opens
`s.Reader` on a table, then `s.Writer(…, Truncate: true)` on that table.
On the old code the Writer's TRUNCATE waited until the test's 20 s
context ended ("context deadline exceeded"): the read held ACCESS SHARE
until its rows ran out, and the loop that would end it never started.

**The fix.** `s.Writer` asks `heldOn`:

- Is a Reader this script opened still holding a transaction?
  `etl.ReadHeld` answers: not closed, not run out, or held for a load.
- Is that transaction on the Writer's Postgres database?
  `etl.SameDatabase` answers: the same pool, or two names for one
  database.

If both hold, the Writer opens through `etl.NewSameDatabaseWriter`. Its
truncate is a DELETE, and `fromQuery` is set when such a Reader's query
may write. `sdb` keeps each Reader's connection alongside it
(`readerConns`).

**Why `ReadHeld` is a function.** It was two Reader methods at first.
Reader is a script type, so the methods went into
`sdb/sdbapi/api.json`, and the assistant's summary went 63 bytes over
its 14 KB budget (`TestSummary`). A package function in `etl` isn't
listed.

The test passes with the Writer on the Reader's pool and on a second
name for the same database.

## N-310: a script that ignores Stop frees its tab (`3970e79`)

The item offered two options; this takes the abandon one.
`RunScript` runs `Run` through `scriptStopped`, on an inner goroutine.

- Once the run's context ends, the script has `scriptAbandonAfter`
  (1 min, matching the engine's `AbandonAfter`) to return.
- Past that the run lands as stopped (`db.ErrCanceled`), with a Warn
  note: the script runs on, and its connection, locks and transactions
  stay busy until it returns or dbc exits.
- An `abandoned` flag drops the script's late shows and prints.

The other option, `ExecuteWithContext`, races yaegi's done channel
under the running frames.

Test: `TestScriptIgnoringStopIsAbandoned`.

- A script that sleeps 3 s without checking `s.Ctx()` is stopped, with
  the window set to 300 ms.
- The run lands as stopped, with the note.
- The next run goes through at once.
- The script's late show lands nowhere.

The README's `s.Ctx()` row says what happens.

## Next

Closed: N-309, N-310. Declined: None. Raised: None. Deferred: None.
Promoted: None. Moved: None. Updated: N-308. Full list:
`ai_docs/todo/next-list.md`.
