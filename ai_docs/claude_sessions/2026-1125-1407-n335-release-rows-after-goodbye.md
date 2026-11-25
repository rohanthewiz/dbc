# N-335: dbc web drops a closed browser tab's result rows at the idle release

Session: `d544f295-7934-4af7-a8e8-acf111b66da2`

## Ask

"Next list item N-335". A dbc web window with no stream kept every query
tab's result sets until it was forgotten, a day later (`forgetAfter`).
That is up to `result_tabs` × `max_rows` rows per connection a tab visited,
and cells have no size cap. N-284 had proposed dropping them at the idle
release (`conn_idle_timeout`, 1 h). That was left for the user, because a
laptop asleep over lunch would come back to empty result tabs. The item
offered three choices: drop at the release, drop past some size, or keep
the day. Then `/sw`.

## What was found before asking

- **The page already says goodbye when it closes.** On pagehide it sends
  `POST /win/:id/release`, which sets `handedOff` (`web/claims.go`). A
  laptop that sleeps, or a tab Chrome discards, sends nothing. That makes a
  fourth option possible: drop the rows only for a window that said goodbye.
- **A reload says goodbye too**, but its stream is back within seconds. On
  a bfcache return, `pageshow` with `persisted` reclaims the tabs.
- **The reaper's notes never reached a real page.** `sayReleased` was
  called only from `handleEvents` (`/ws/:id/events`, used by scripts and
  the Go tests). The page attaches to `/win/:id/events`, so N-261's "your
  session was released" note never got to it.
- **The grid holds a second copy of the rows.** Each tab's grid cache
  (`tab.rv`) keeps the result on screen, so dropping the workspace's
  copy alone would free nothing for that result.

## The user's decision

Asked with four options: drop if the page said goodbye (recommended), drop
every idle window, drop past a size, keep the day. The user picked **drop
if it said goodbye**.

```
                          goodbye?   at releaseAfter          at forgetAfter
browser tab closed          yes      session + rows go        window goes
reload / bfcache return     yes      — (stream back in secs)
laptop asleep, tab dropped  no       session goes, rows kept  window goes
by Chrome, network gone
```

## What changed (commit `8214352`)

**The server (`web/`)**

- `window.goodbye`, guarded by `hub.mu`:
  - set by `hub.release`, the page's goodbye;
  - cleared by `hub.attached`, called from both stream handlers;
  - also cleared by the reaper's tick while a stream is attached, which
    covers a goodbye that lands after a reload's new stream.
- It is kept apart from `handedOff`. The claims clear that one on a boot
  claim and read it for something else (telling a reload from a copied tab).
- `reap` collects `idleRelease{t, rows: w.goodbye}`. `releaseIdle(t, rows)`
  first closes the session as before. With `rows` set it then calls
  `ws.ReleaseResults()`, empties `t.rv`, and stores a note in `t.dropped`
  when there were rows to drop. A retried pass (another tab of the window
  was busy) finds none and keeps the first note.
- `sayReleased` logs both notes. `handleWindowEvents` now calls it, which
  closes the N-261 gap.
- `resultTabRef.Released` is sent with the strip.

**The workspace (`workspace/`)**

- `ReleaseResults() int` sets `res` and `shows` to nil on every tab of
  every set, marks the tab `released`, and adds the dropped shows to `cut`.
  Title, name, statement, pin, share, group and seq stay.
- `placeRunLocked` and `showIntoLocked` clear `released` when they fill a
  tab. A rerun's `rerunOf` is the released seq.
- `LastResultRerun` returns 0 for a released tab, as it does for no result.
- `sharedRows()` is `sharedTab()` but nil while released. The three chat
  context builders use it, so an empty share doesn't take the place of the
  question's own result.
- `ShowScriptResult` on a released tab refuses with "run the script again".

**The page (`web/static`, `web/pages`)**

- A released tab is italic (`.rt.rel`). Its tooltip and hover card say
  "released".
- `releasedNote(d)` gives the grid's text for a released tab.
  `grid.clear(note)` shows it in place of the run hint. It is called from
  `showResultOf`, `applyState` and `applyScriptState`.
- `#grid-msg` has `tabindex -1` (no focus ring), so a click on the message
  puts `r` and `{ }` in reach. That covers an exec's "n rows affected" too.

**README**: the browser workbench section says when sessions and result
rows are released.

## Verification

- `web.TestIdleReleaseDropsRowsAfterGoodbye`, in order:
  - run a query, pin it, fetch a page (so the grid cache holds the rows);
  - goodbye, detach, reap past the release;
  - checks: rows and cache gone; the tab kept, pinned, released, with its
    statement; `/result` answers no data;
  - attach `/win/:id/events` and hear the note; the goodbye is cleared;
  - the rerun refills the same tab, pinned, with `rerunOf` = the old seq.
- `web.TestIdleReleaseKeepsRowsWithoutGoodbye` has two cases. In both the
  rows survive the release:
  - the stream is just lost;
  - goodbye, a reload's stream, then a detach.
- `workspace.TestReleaseResults`: the count, a second release finding 0,
  the tabs kept, `LastResultSeq` 0, the share withheld then sent again
  after its rerun, and script shows (n 0, cut 2; the next show is numbered
  after them and clears the mark).
- A throwaway headless-Chrome run, deleted after, set
  `conn_idle_timeout = "1s"`:
  - run a query, navigate the tab to about:blank (pagehide → goodbye), wait
    out the reaper's 1-minute tick (135 s in all);
  - navigate back in the same tab, so sessionStorage still names the window;
  - seen: the italic tab, the "Released — …" note, the log line;
  - a click on the note, then `r`: 3 rows back.
  - It turned up "1 result tab keep their statements", now worded by count.
- `go test ./...` green. `go test -race` on the idle tests. The full web
  e2e (`DBC_E2E=1`) passes. `go vet` and staticcheck 2026.2.2 on `web` and
  `workspace`, darwin and `GOOS=linux`: clean.

## Notes

- One near-impossible case is not handled. If the release ran without a
  goodbye (the session went, the rows stayed) and a goodbye arrives later
  with no stream in between, the rows wait for the forget. A page able to
  send a goodbye would have reattached first.
- The grid's kept views (`t.rviews`) live only in the page's memory. A
  reopened page's rerun starts unsorted, as any new result does.
- Plans, and each set's last statement and error, are not dropped. They
  are small and read as text.

## Next

Closed: N-335. Declined: None. Raised: None. Deferred: None.
Promoted: None. Moved: None. Updated: None.
Full list: `ai_docs/todo/next-list.md`.
