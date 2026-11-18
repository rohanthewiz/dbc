# N-166, N-300, N-307: light-theme notes, web a11y basics, bodies kept as written

Session: `cbde2eba-f688-4850-8468-0b28475b5b48`

## Ask

`/work-next-list`, the twelfth batch. The triage is in
`2026-1118-0042-n321-n286-n288-n291-plain-errors-value-text-status-error`.

## N-166: inline error notes on the light theme (checked, still open; `2479d03`)

A temporary probe in the web e2e "script tabs" step (not committed)
measured the notes and took screenshots.

- **Error note:** "× undefined: nope" is 5.28:1 on dark and 4.79:1 on
  light. It reads.
- **Warning note:** `--warn` on light was 3.41:1, which belongs to N-300
  and is fixed there.
- **A long message** is cut at 160 characters with an ellipsis; Monaco
  draws it in 50-character spans, which is cosmetic.

Still to look at: the pipeline inspector's narrow Go field. N-166 stays
in Validate.

## N-300: dbc web usability basics (`fca15d7`)

All six parts are done:

1. **Focus trap** (`ui.js` `trapTab`):
   - Tab and Shift+Tab wrap inside an open dialog, and focus outside it
     is brought back.
   - The wrap listens on the box in the bubble phase and skips a
     cancelled Tab, so Monaco or a code textarea keeps Tab for itself.
2. **Grid ARIA** (`grid.js`):
   - The grid is `role=grid`, with whole-result `aria-rowcount` and
     `aria-colcount`.
   - The rows and cells drawn carry their indices, `aria-selected` and
     `aria-sort`.
   - `aria-activedescendant` points at the cursor's cell. Its id is
     position-named (`gcur-<seq>-<row>-<col>`) so every move changes the
     attribute and a screen reader speaks it.
   - The transposed grid gets the same.
3. **Spoken status:**
   - `#announce` is a hidden `role=status` copy of the status line.
   - `setStatus(text, level, quiet)`: the per-tick and pipeline-progress
     writers pass `quiet`, so a run is spoken as its first and last lines.
     An error is always spoken.
4. **Contrast:**
   - Light `--accent` is now #277a51 and `--warn` #946219.
   - Each keeps its hue, darkened to `--muted`'s level: about 4.9:1 on
     the background and 4.6:1 on panel2.
   - Changed in `theme/palette.go` and `explain/assets/plan.css`.
5. **Topbar:** `.topbar` and `.actions` wrap at every width.
6. **Retried pages:**
   - A failed grid page backs off from 1 s, doubling to 16 s.
   - A timer redraws when the wait is up, so the view fills in by itself.
   - The outage is logged once, and its end once ("rows load again").

**New e2e step:** `web/e2e/a11y_test.go` checks every part. Run on the
old code, each part fails:

- Tab left the dialog at press 15.
- Light accent and warn measured 3.2–3.8:1.
- ◐ was off screen at 900 px; three buttons were off screen at 760 px.
- 26 fetches and 26 log lines in about 3 s, against 2 and 1 now.

**N-337 raised.** One full web e2e run failed "a stale completion
request is aborted": the active tab was the Go script `e2e_pipe.go`, so
no SQL completion was asked. The rerun passed.

## N-307: Format keeps a body statement as written (`2a5dac7`)

**What changed.** `keptBodies` uses the scan's chunks to find statements
that have a `;` before their terminator. `keptStatement` merges the
pieces `fmtStatements` cut such a statement into, and `fmtWriter.raw`
writes it as written. The comment heading it and the remark after its
`;` stay with it.

**Safeguards.**

- If a piece runs past the span (a bracket left open), the statement is
  laid out as before, so no token is dropped.
- A buffer whose only `;` is its last token skips the scan.
  BenchmarkFormatDeep stays within noise; it was 3–5% slower without
  this.

**Tests.** `TestBodyFormat` asserts the exact outputs for:

- a SQLite trigger;
- BEGIN ATOMIC;
- a rule's action list;
- a MySQL routine without DELIMITER;
- an unterminated trigger;

and checks that the open-bracket case keeps every word.

## Checks

For both N-300 and N-307: the full `go test ./...`, `go vet`, and
staticcheck 2026.2.2 are clean. The full web e2e passed on its rerun.

## Next

Closed: N-300, N-307. Declined: None. Raised: N-337. Deferred: None.
Promoted: None. Moved: None. Updated: N-166 (the light-theme results).
Full list: `ai_docs/todo/next-list.md`.
