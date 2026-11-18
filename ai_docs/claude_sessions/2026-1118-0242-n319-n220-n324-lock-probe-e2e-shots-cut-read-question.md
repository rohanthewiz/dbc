# N-319, N-220, N-324: run records by their lock, the e2e screenshot failure, a question on cut reads

Session: `cbde2eba-f688-4850-8468-0b28475b5b48`

## Ask

`/work-next-list`, the eleventh batch. The triage is in
`2026-1118-0042-n321-n286-n288-n291-plain-errors-value-text-status-error`.

## N-319: a run record whose lock is free reads as dead (`26f17df`)

**What changed.** `stale` and `liveElsewhere` now also ask
`lockSaysDead` (`jobs/held.go`).

- The probe applies to a running or queued record that isn't this
  engine's own live run.
- It takes the lock such a run holds and lets go at once:
  `job-<name>` or `pipeline-<name>` while running, `queue-<name>` while
  queued.
- If the lock is free, the writer is gone. The record then reads
  interrupted, and `Recover` rewrites it without waiting out StaleAfter.

**When the heartbeat still decides.** The probe says nothing when there
is no runs dir; when the `.lock` sidecar was never made (the probe never
creates one); when this engine holds the lock; or when the lock is held.

**A detail.** `btypedb.AcquireLock(path)` locks `path + ".lock"`. The
first version stat-ed the bare path, and its test caught that.

**The probe can't refuse a start.** `holdFileLocked` retries a lock it
found taken, 3 × 2 ms, before counting it as another holder's. A probe's
instant of holding therefore never refuses a start.

**Tests:**

- `TestLockSaysDead`, with real flocks: no lock file reads running; a
  held lock reads running; a released lock reads interrupted, and
  `Recover` rewrites it.
- `TestStartOutlastsAProbe`.

Not checked with a real `kill -9`.

## N-220: the pipeline e2e step under DBC_E2E_SHOTS (`ac9234d`)

The item's guess, that a screenshot stalls the animation frames, was
wrong.

**Finding the cause.**

- Skipping one shot at a time with a temporary `DBC_E2E_SKIPSHOT` hook
  showed that only `pipeline-lane-drag` mattered. It is taken while lane
  `b` is held near the canvas's bottom edge.
- Logging the stage's transform showed the cause. Holding `b` there
  auto-pans the view for as long as it is held, and the screenshot
  stretched the hold: ty ended at -21.95 against -4.51.
- The view keeps its pan when a new spec loads. `l00`'s header sat above
  the canvas's top, the press missed it, and no drag began (`lifted`
  false, the transform frozen).

**The fix.** After loading the twelve lanes, the test clicks ⊡ Fit and
waits for `l00`'s header to be inside the canvas.

**Checks.** The step passed twice with screenshots and once without, and
the full web e2e passed. The debug code and the skip hook were removed:
the file went back to its backup before the fix was applied. This was a
test fix only.

## N-324: left open with the question

Each fix changes behaviour, so the item now spells out the choice:

- **SAVEPOINT:** a read's error would no longer abort the user's
  transaction.
- **A cursor:** a fast-start plan, so a different plan and timing, and it
  only works for SELECT and VALUES.
- **Leave it:** slow on a huge table, but correct.

The question is for the user. A cursor looks the least surprising.

## Next

Closed: N-319, N-220. Declined: None. Raised: None. Deferred: None.
Promoted: None. Moved: None. Updated: N-324 (the question). Full list:
`ai_docs/todo/next-list.md`.
