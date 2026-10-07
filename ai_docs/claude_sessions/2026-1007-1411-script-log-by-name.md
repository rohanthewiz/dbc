# A dbc web script's log is kept by the script's name

Session: `10144d4c-92cd-4106-b153-0382490f55bf`

## Ask

N-148, dropped from the cats-todo backlog:

"A dbc web script tab's log is keyed by the query tab, not the script: it
survives a rename, but opening the script again in a new tab (or after
closing its tab) starts an empty log. Key it by the script's name if script
logs turn out to be read across tabs. Logs are per page (lost on a reload)
in dbc web and per process in the TUI; neither is persisted."

Then "/sess-wrap".

## Decisions

No questions were asked. Judgement calls:

- **Done, not left contingent.** The item said "if script logs turn out to
  be read across tabs". Dropping it from the backlog was taken as that
  signal.
- **dbc web only.** The TUI has no script tabs; its logs are per connection
  and stay as they are.
- **A rename moves the log, whether or not a tab is open on the script.**
  The log belongs to the name. Left under the old name, it would be
  stranded, or read by a later script that took that name.
- **The move appends to the target; it never replaces it.**
  - A rename is applied twice in the window that made it: once from its
    response, once from its "scripts" event (`scripts.js` `renamed`). The
    second move finds nothing to move, and appending nothing leaves the
    log as the first move made it. Replacing would wipe it.
  - The target may already hold lines: a trashed script of that name
    whose tab is still open.
- **Trash keeps the log.** A trashed script's tab stays open (Ctrl+S saves
  it back), so its log stays too. Restore brings back the same name, and
  with it the same log.
- **Accepted cost.** A later script given a name used earlier in the page
  (after the first was trashed) reads on in the earlier one's lines. Every
  line has a timestamp, and ✕ clear empties the log.
- **Lines stay with the run.** Running script B from the browser while
  script tab A is on screen still writes to A's log, because the run lands
  in A's tab, results included.
- **Left alone: the holding-log fold.** Lines held while a tab is on no
  connection still fold into whichever log comes on screen next, a
  script's included, as before.

## What changed

- **`web/static/js/app.js`**
  - A script tab's log key is now `"\x01script:" + name`, from `logKeyOf`
    and the new `scriptLogKey`. It used to be `"\x01tab:" + t.key`.
  - `scriptKit.renamed` calls `dbc.moveLog`, and it does so even when no
    tab has the script open.
  - On a rename, the log header now updates for the script on screen
    (`showLogOf`). It used to keep reading `Log · <old name>`; this was a
    bug in the old code too.
  - A comment block with a diagram walks a script's log through open,
    close, reopen, rename and trash/restore.
- **`web/static/js/core.js`**
  - New `dbc.moveLog(from, to)`: it appends from's lines to to's and
    trims to `LOG_MAX`.
  - If either log is on screen, the screen follows the move.
  - The holding log `""` is never moved.
  - The diagram of the logs map gains the script entry.
- **`web/e2e/scripts_test.go`**: three new checks in the "script tabs" step.
  - After a rename, the header reads `· e2e_renamed.go`, and the log still
    holds the conflict line from before the rename, plus the "renamed …"
    line.
  - After the tab is closed, the query tab's log does not show the
    script's lines.
  - When the script is reopened in a new tab (the orphan-draft step), its
    log is back, including the "renamed …" and "moved … to the trash"
    lines.
- **README**: the log paragraph now says a script's log is kept by name
  and follows a rename. It also says logs last for the page in dbc web and
  for the process in the TUI, and neither is saved.

## Verification

- `node --check` passes on `app.js` and `core.js`.
- `go vet` in `web/e2e` passes, and `gofmt` is clean.
- `go test ./web/` passes.
- Web e2e (`DBC_E2E=1`, `CATS_*` stripped) passes in 43s.
- **Run against the old code.** With `app.js` and `core.js` stashed, the
  suite fails at the new rename check (`scripts_test.go:256`, "the log
  renamed with the script"), because the old header kept the old name.
  The reopen check comes after it, so the old code never reached it.

## Next

Closed: N-148. Declined: None. Raised: None.
Deferred: None. Promoted: None.
Updated: None. Full list: `ai_docs/todo/next-list.md`.
