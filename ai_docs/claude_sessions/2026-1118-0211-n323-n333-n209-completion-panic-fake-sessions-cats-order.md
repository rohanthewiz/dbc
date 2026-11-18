# N-323, N-333, N-209: a panicking completion read, the fake's sessions, cats reports in seq order

Session: `cbde2eba-f688-4850-8468-0b28475b5b48`

## Ask

`/work-next-list`, the eighth batch. The triage is in
`2026-1118-0042-n321-n286-n288-n291-plain-errors-value-text-status-error`.

## N-323: a panic in a completion schema read is its error (`8cdfaab`)

**The problem.** `Workspace.LoadCompletions` and `ConnCompletions.load`
closed their loading channel, and stored the outcome, only on a normal
return. A panic between left the cache "loading" for good.

**The fix.** Both loaders now read through `readCompletionsSafely`:
`readCompletions`, then `readRoutines`, with a deferred recover.

- A panic becomes the read's error: `internal error reading the schema for
  completion: <value>`, with `conn` and `stack` as serr fields.
- Each loader's normal path then still stores the outcome and closes its
  channel.
- `readCompletionsFn` is the test seam.

Test: `TestCompletionReadPanics`. A second `LoadCompletions` and a second
`ConnCompletions.Tables` both return rather than wait.

## N-333: each fake chat session keeps its own state (`3471953`)

`aitest.Start` now makes a `session` of its own: pipe, write lock, cancel
channel, model and effort.

- The serve goroutine and its turns keep that session.
- `Fake.cur` points at the newest one, for `Effort()`.

**New test: `ai.TestOneFakeManyChats`.** Six chats start at once on one
Fake; each must get its own echo back.

- It passes under `-race`.
- To see the old behaviour, it was run in a scratch `git worktree` at
  HEAD with the old fake: 5 DATA RACEs, then a hang until its 90 s
  timeout, the answers going to other chats' pipes.
- The web chat tests didn't show the race in 20 runs on either version;
  the bug only appears when a chat restarts mid-turn.

**A slip with git stash.** A first attempt stashed `fake.go` and ran the
old version in a background shell that hung. The stash, which held the
change, had to be popped by hand after stopping that shell. The worktree
approach that followed leaves the working tree alone.

## N-209: the cats hook test reads reports in seq order (`9574162`)

**The cause.** The 5 s window was never the problem. `ReportState`
sends each report from its own goroutine, so they can arrive out of
order. The failure's "idle, idle, working" was the run's "idle"
arriving before its "working". In arrival order, no wait was long enough.

**The fix.** The cats server orders reports by their `seq` token, so
`sockRecorder.states` now sorts by `params.seq`.

- The recorder decodes with `UseNumber`. A seq is a clock-seeded uint64
  (~1.7e18), and as a float64 consecutive values compare equal.
- `TestCatsAgentPickerStagesTheStatement` compares the pane as
  `json.Number("4")` to match.

**Checks.** The cats tests passed 10 runs and the transitions test 20.
Two full `go test ./...` runs, whole log kept, passed.

## Notes

- Every full-suite run in this batch kept its whole log in the
  scratchpad (`full4.log` … `full7.log`); all passed.

## Next

Closed: N-323, N-333, N-209. Declined: None. Raised: None. Deferred:
None. Promoted: None. Moved: None. Updated: None. Full list:
`ai_docs/todo/next-list.md`.
