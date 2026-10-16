# dbc web: the active grouped tab tinted in its group's colour; the job canvas redrawn on ⊞ Design

Session: `62924e14-c724-4cef-a211-1c2b25675253`

## Ask

1. From the cats-todo backlog: "On Query tabs, instead of just a bold
   foreground on active tabs, also make the background-color a slightly
   desaturated version of the Tab Group's color."
2. Fix the failing e2e step "job tabs and the runs view", which turned up
   while checking (1).

## Findings

- **Active tabs were never bold.** `.qtab.on` only switched `--muted` text
  to `--fg` and the fill to `--bg`. The font weight was left alone.
- **Group hues** are `--g0 … --g5` in `app.css`, a set for each theme;
  `.g0 … .g5` set `--gc` on a chip or tab.
- **Contrast of the tint**: the hue mixed 70% with `#808080`, then that
  over `--bg` at 32% (dark) / 22% (light). With `--fg` text, the contrast is
  6.1–6.8:1 on the dark slate (`#1f2420`) and 10.4–10.8:1 on the light paper
  (`#f6f8f6`) across all six hues.
- **The job-tabs failure was an app bug, not test flakiness.** It also failed
  on unmodified `main`, in about two of five runs. A probe showed the runs
  kit's record for the run read `succeeded` on every step while the cards
  read `s-queued`. The canvas was stale:
  - `jobs.js onEvent` redraws the canvas only when the tab shows its design
    face.
  - `run()` switches the tab to its runs face as soon as `POST …/run`
    answers.
  - When that answer beat the job's first event (the e2e job takes ~13 ms),
    every event afterwards skipped the hidden canvas.
  - `setFace(e, "design")` only unhid it, so the cards kept the `job.run`
    snapshot (○ queued).
  - A person running a job and going back to ⊞ Design could see the same
    stale states.
- The server's event order is fine. `hub.broadcast` sends synchronously,
  `Engine.emit` holds `emitMu`, and `RunDone` carries the final record with
  every pipeline's status.

## Changes

- `web/static/css/app.css`: `.qtab.grp.on` fills with
  `color-mix(in srgb, color-mix(in srgb, var(--gc) 70%, #808080) 32%, var(--bg))`,
  using 22% under `:root[data-theme="light"]`. Its specificity beats
  `.qtab.on` and `.qtab:hover`, so a hovered active tab keeps the tint.
  Ungrouped tabs keep `--bg`, and the 2px underline sits on the tint.
- `web/e2e/web_test.go` "tab groups": the computed backgrounds of the
  grouped tabs give `Query 1:plain,Query 2*:tint,Renamed tab:plain`. The
  tint must differ from `body`'s background and from transparent.
- `web/static/js/jobs.js setFace`: going back to the design face calls
  `renderCanvas()`, so the cards are drawn from the run as the runs kit has
  it now. A comment explains the race.

Commits: `5a086c1` (tint + e2e check, pushed); the jobs.js fix with this doc.

## Verification

- Full e2e suite with the tint: the "tab groups" step passed. The suite
  stopped at the job-tabs step, which failed the same way on a stash of
  `main`.
- Job steps (`DBC_E2E_STEPS=job`): eight of eight runs passed after the fix,
  and two of five failed before it. Probes in `app.js`/`jobs_test.go`
  were temporary and are removed.
- Full e2e suite after the fix: two consecutive passes (~57 s each).

## Next

Closed: None. Declined: None. Raised: None.
Deferred: None. Promoted: None. Moved: None.
Updated: None. Full list: `ai_docs/todo/next-list.md`.
