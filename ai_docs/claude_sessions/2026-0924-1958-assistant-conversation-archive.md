# Assistant conversations are kept: saved, offered back, reopened, deleted

Session: b36dd1e7-968d-46d7-b4e1-ba4538ba3fdb
Date: 2026-09-24

## Ask

1. Next-list item N-027, pasted as-is: "Keep assistant conversations: save
   each to `~/.config/dbc/chats/` and offer recent ones, as ced's
   `internal/chatstore` does (stdlib-only, portable). Today ⟲ new and
   quitting discard the transcript."
2. "Add a delete to the UI" — after the first part shipped with deletion
   only by `rm`.

## Decisions

- **The store is a port of ced's `internal/chatstore`, living in
  `userdata`.** That package already holds the history and the editor
  buffer ("state dbc keeps between sessions"), so conversations go beside
  them rather than in a new package. The shape is ced's: one JSON file per
  conversation, temp file + rename, newest-first listing that skips
  unreadable files, a cap of 30 trimmed from the oldest. Differences: a
  conversation records the **connection** it was about (ced records a
  project root), the roles are dbc's six as strings (`user`, `agent`,
  `tool`, `note`, `info`, `error`), and errors are wrapped with `serr` like
  the rest of `userdata`. Files are `0600`, dir `0700`: a conversation
  carries queries and, with `ai_rows`, result rows.
- **Saved after every answer**, and on agent exit, ⟲ new, and quit (in
  `shutdown`, before the agent is closed, so a partial answer is kept). A
  crash costs at most the answer in flight. A pane where nothing was asked
  saves nothing.
- **Offered in the empty pane.** The five most recent (other than the live
  one) are clickable rows above the usual hint, with `all N recent…` and a
  right-click **Recent conversations…** list for the rest. The list is read
  when the pane opens or a conversation is put away — never from the draw.
  A row's detail is time · message count · connection (the connection only
  when it isn't the active one); on a narrow pane it sheds the count, then
  the connection, then the time, so the title keeps ≥12 columns.
- **Reopening is a transcript, not a resumed session, and says so.** It
  goes through `newChat` (saves the live one, fresh ACP session), loads the
  messages, keeps saving to the **same file**, and adds an info line that
  the assistant has no memory of it. ACP `session/load` was not used: it is
  optional and replays the history as updates that would duplicate the
  transcript.
- **Delete is the one gesture that destroys, so it is never one keypress.**
  In the list, `d` (or Delete/Backspace) arms the row — marked `✕`, its
  detail replaced by "delete? press d again — any other key keeps it" — and
  a second press deletes. Any other key, moving, or Esc disarms; Esc closes
  the list only when nothing is armed. From the mouse, right-click a row (in
  the list or the empty pane) → **Delete conversation**; the menu pick is
  the deliberate second step. The live conversation is never offered (its
  next save would write it back) — raised as N-038.
- A save failure is logged **once** (deduped by message), not after every
  answer.

## What changed

### `userdata/chats.go` (new)

`Chat`, `ChatMsg`, `ChatMeta`, `ChatsDir`, `NewChatID` (nanosecond tail so
back-to-back ids cannot collide), `ChatTitle` (first line of the first
question, ≤72 runes), `SaveChat`, `ListChats`, `LoadChat` (refuses ids with
a path in them), `RemoveChat`, `pruneChats`. `MaxChats = 30`.
`userdata/history.go`'s package doc mentions conversations.

### `tui/chatarchive.go` (new)

Role mapping, `worthSaving`, `chatSave` (returns whether it saved),
`chatLoadRecent`, `chatOpenSaved`, `restoreNote`, `chatWhen`, row
title/detail helpers, `otherRecent`; `chatDelete`, `openSavedChatMenu`,
`chatRightClick`; `recentChatsModal` with the two-press delete and a row
right-click menu. `chatNow` is a var for tests.

### `tui/chat.go`, `tui/app.go`, `tui/mouse.go`

- `chatPane` gained `dir`, `archiveID`, `archiveStart`, `recent`,
  `saveErr`; new target kinds `targetOpenSaved`, `targetAllSaved`.
- `newChat` saves first (and logs it), resets the archive identity, and
  reloads the recent list. Saves on `EventTurnDone` and `EventExit`.
- Opening the pane empty reloads the recent list (`chatOpened`).
- `drawRecent` draws the empty-pane rows and registers their targets only
  for rows that are on screen.
- The transcript menu gained **Recent conversations…**; a right-click on a
  saved row opens that row's Open/Delete menu instead.
- `New` sets `chat.dir = userdata.ChatsDir()` unless `NoPersist`;
  `shutdown` saves before closing the agent.

### Tests, docs

- `userdata/chats_test.go`: round trip and same-id replace, file mode, empty
  no-ops, newest-first listing that skips bad/non-JSON files, missing dir,
  pruning to the cap, path-id refusal, id from filename, titles, id order.
- `tui/chat_test.go`: keep → ⟲ new → offered → reopen into the same file
  with a fresh session; save on quit mid-answer; no save for an empty pane;
  the list via `all N recent…` and the menu; two-press delete (disarm on
  move and on Esc, closes when empty); delete from the pane-row and
  list-row menus.
- Checked rendered frames at the default and minimum (30) pane widths.
- README: "Conversations are kept" paragraph in the assistant section,
  including how to delete; the persistence line mentions conversations.

`go vet` and `go test -race ./...` pass.

## Next

Closed: N-027. Declined: None. Raised: N-038.
Deferred: None. Promoted: None.
Updated: None. Full list: `ai_docs/todo/next-list.md`.
