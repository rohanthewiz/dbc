# UI revamp: Bubble Tea v2, mouse-first, AI chat, rich copy

Raised 2026-09-24. The ask, verbatim in spirit:

- The UI needs a major revamp, with **mouse support as a first-class
  citizen**.
- Take technology hints from `~/projs/go/cats-todo` (Bubble Tea v2 +
  lipgloss v2 + bubbles v2).
- **AI chat assistance, Copilot by default**, as in `~/projs/go/ced`. Send the
  query and the first 10 (configurable) rows of the result as context.
- **Copy results as a real HTML table** that pastes into Teams (and other
  chat apps) as a table. Also Markdown and CSV.

## Decisions taken to the user (2026-09-24)

| Fork | Chosen | Rejected |
|---|---|---|
| UI stack | **Rewrite on Bubble Tea v2** (cats-todo's stack) | Staying on tview and deepening mouse support; borrowing only cats-todo's ideas |
| AI transport | **ACP sidecar, as ced does**: `copilot-language-server --acp`, with Claude Code and Gemini as alternatives | Talking HTTP directly to api.githubcopilot.com |
| Result rows sent to the AI | **Off by default; a connection opts in** with `ai_rows = true` | On by default with a per-connection opt-out; asking on every send |

## What the research found

**ced does not call Copilot's API.** It runs GitHub's `copilot-language-server`
as a subprocess and speaks JSON-RPC to it. The binary owns OAuth, token
refresh, and the HTTP calls; credentials are shared with every other editor
the user has signed in to Copilot from. Chat runs a second copy with `--acp`,
the Agent Client Protocol: `initialize` → `session/new` (which returns the
model roster) → `session/prompt` (blocks for the whole turn) while
`session/update` notifications stream the answer; `session/cancel` stops it.
ced's transport (`internal/lsp/client.go` core, `acp.go`, `ndjson.go`, about
700 lines) and its chat archive (`internal/chatstore`) are stdlib-only and
port as-is. Everything drawn is tcell-specific and is rewritten.

**cats-todo's mouse handling is hand-made, and works well.** There is no
zone library and no layout engine:

- One description of a row's clickable spans is used both to draw the row
  and to hit-test it, so the two cannot disagree.
- Nothing above a hit-tested line may wrap. Lines truncate or shrink in tiers
  instead.
- Floating boxes (menus, cards) are composited over the finished frame with
  lipgloss v2's `Compositor`, never spliced into it.
- Mouse reporting is chosen per screen through `tea.View.MouseMode`.
- Tests check every row constant against the rendered frame.

It has **no wheel scrolling, no table component, and no editor with click
support** (it works around private parts of bubbles `textarea`). dbc needs
all three, so they are built here.

**dbc's HTML copy is not rich today.** `export.ToClipboard` and the TUI's
`clipWrite` put HTML *source* on the clipboard as plain text, so Teams pastes
markup. A chat app renders a table only when the clipboard also carries an
HTML flavor: `public.html` on macOS, `CF_HTML` on Windows, `text/html` on
X11 or Wayland. The file export's `<style>` block would be stripped too, so
the clipboard fragment needs inline styles. OSC 52, the fallback over SSH,
carries plain text only, so there a rich copy degrades to a plain-text table.

## Shape of the result

```
┌ Connections ──┐┌ Query ─────────────── ▶ Run  ■ Stop  ✦ Ask ─┐┌ Assistant · Copilot · gpt-4.1 ┐
│● demo-bytdb   ││ SELECT id, name FROM cats                   ││ ❯ why is this slow?           │
│  demo    sqlite││                                             ││ The planner …                 │
│               │└─────────────────────────────────────────────┘│ ```sql                        │
│ Tables        │┌ Results · 8 rows · 1.2ms ─── ⧉ Copy ▾ ─────┐│ SELECT …          ⤓ insert   │
│  cats         ││ id │ name     │ breed  │ age                ▲││ ```                           │
│  owners       ││  1 │ Whiskers │ Tabby  │   3                █││                               │
│               ││  2 │ …        │        │                    ▼││ ▤ query + 10 rows attached    │
└───────────────┘└─────────────────────────────────────────────┘│ ask about this result… ⏎      │
 Log (click to expand)                                          └────────────────────────────────┘
 demo-bytdb │ 8 rows in 1.2ms │ ^R run ^K stop ^E export ^J ask …
```

What the mouse does:

| Where | Gesture | Effect |
|---|---|---|
| Any pane | click | focus it (keyboard follows the mouse) |
| Connections | click / double-click | select / connect |
| Tables list | click / double-click | preview `SELECT *` / insert the name at the caret |
| Editor | click, drag, double-click | caret, selection, word |
| Editor | right-click | menu: run statement · run selection · copy · paste · ask AI |
| Results | click, drag, shift-click | cell, rectangular range, extend |
| Results | double-click | inspect the cell's full value in a popup (closes N-001) |
| Results | header click | sort by that column, client-side (again to reverse) |
| Results | right-click | copy menu: cell · row · selection · whole result × Table (HTML) / Markdown / CSV / TSV / JSON · ask AI about the selection |
| Results, log, chat, editor | wheel / shift-wheel | scroll vertically / horizontally |
| Scrollbars | click, drag | jump / drag the thumb |
| Pane borders | drag | resize the editor/results split and the chat width |
| Toolbar | click | Run, Stop, Copy ▾, Ask |
| Chat | click `⤓ insert` on a code block | put the SQL into the editor at the caret |
| Chat | click `⧉ copy` | copy one reply |

The keyboard keeps every current binding, so keyboard users lose nothing.

## Phases

Each phase is one commit or a short run of them, with tests, and leaves the
build green. Phases 0 and 1 do not depend on the UI stack, so the current
tview UI benefits from them immediately.

### Phase 0: rich copy (`clip` package + `export.HTMLFragment`)

- `export.HTMLFragment(r)` renders a bare `<table>` with **inline styles**
  in neutral colors that read on both light and dark chat themes. The
  existing `<style>` document stays for file export.
- `clip.Write(clip.Content{Text, HTML})` puts both flavors on the clipboard
  in one operation:
  - **macOS:** `osascript -l JavaScript` driving `NSPasteboard`. The payload
    goes in as JSON on stdin, so no size limit and no quoting.
  - **Windows:** user32 `CF_HTML` through `golang.org/x/sys/windows`, with the
    header offsets computed in Go.
  - **Linux:** `wl-copy --type text/html`, else `xclip -t text/html`.
  - With no HTML flavor available, plain text goes through the existing path.
- `export.ToClipboard` and the tview export modal use it. The text flavor of
  an HTML copy is the aligned text table, so pasting into a terminal still
  reads well.

### Phase 1: the `ai` package, UI-agnostic

- `ai/acp`: ced's JSON-RPC client core, ACP and ndjson framing, ported with
  a provenance note.
- `ai`: an agent registry (Copilot as default, Claude Code, Gemini) and a
  `Chat` that owns one ACP process. Its methods are `Start`, `Models`,
  `SetModel`, `Send`, `Cancel` and `Close`. Streaming, tool-call lines and turn
  completion arrive as values on a channel, so either UI can consume them.
- The client advertises no filesystem capability and **rejects every
  permission request**, because a database client has no business letting an
  agent write files. The rejection is logged in the transcript.
- `ai.Context{Driver, Conn, Query, Columns, Rows, Err}` renders into the
  prompt. The query and last error are always included. Rows are included
  only when the connection has `ai_rows = true`, capped at `ai_context_rows`
  (default 10; 0 means none). The transcript shows exactly what was attached.
- Config: `ai_agent` (default `copilot`), `ai_model`, `ai_context_rows`, and
  per-connection `ai_rows`.
- Tests use a fake ACP agent over `io.Pipe`, as ced's do. The real binary is
  never spawned in tests.

### Phase 2: the Bubble Tea v2 UI (`tui` package)

The new package lives alongside `ui` and is selected with `-ui`. The
default flips once it matches the old UI. `ui` is kept until the user says to
remove it.

- **2a: skeleton.** A root model, the pane layout, focus, the status bar and
  log, every current key, run/cancel/session pinning, history, buffer
  persistence, and `-demo`. Mouse mode is `AllMotion` on the main screen.
- **2b: grid.** A virtualized results table with cell and range selection,
  horizontal scrolling, wheel scrolling, a scrollbar, NULL styling,
  `max_display_rows`, header sorting and the inspect popup.
- **2c: editor.** A purpose-built SQL editor with no soft wrap, so a click
  maps to a caret with no guessing. It has horizontal scroll, selection,
  undo, clipboard and keyword highlighting from `sqlsplit`'s scanner.
  bubbles `textarea` is rejected because click mapping would mean copying
  its private wrap logic, as cats-todo had to.
- **2d: overlays.** Menus and modals composited with lipgloss: export,
  history, scripts, connections, right-click menus, the copy menu, the cell
  inspector.
- **2e: cats glue port.** Hooks, the host theme, ⌘ accelerators, ^G to a
  sibling agent, host identity, and OSC 52 via `tea.SetClipboard`. The
  `cats` package itself is untouched.
- **2f: chat pane.** Wires phase 1 into the new UI: streaming, the model
  picker, attachments, insert-into-editor, cancel, and archiving.

### Phase 3: switch over

Flip the default to `tui`, update README, the key list and
`dbc.example.toml`, and record a session doc. Retiring `ui` is a separate,
explicit decision.

## Risks and how they are handled

- **The rewrite is large** (about 2.6k lines of UI plus 1.4k of cats glue).
  Running old and new side by side keeps the tool usable throughout, and the
  engine packages (`db`, `export`, `script`, `sdb`, `migrate`, `cats`) are
  not touched.
- **Mouse reporting costs the terminal's own text selection.** Every pane
  that shows text therefore gets its own selection and copy: the grid, the
  editor, the chat and the log.
- **The Copilot binary may be missing.** Then the chat pane says how to
  install it (`npm i -g @github/copilot-language-server`) instead of failing
  silently, and everything else works without it.
- **Data leaving the machine.** Rows are opt-in per connection and are
  visible in the transcript before and after sending.

## Status (2026-09-24)

All phases landed in the session that planned them.

- **Phase 0**, `20be4e4`: `clip` + `export.HTMLFragment`.
- **Phase 1**, `71281f6`: the `ai` package. A live handshake with the real
  Copilot binary listed 25 models.
- **Phases 2 and 3**, the `tui` package, now the default UI.

Where the build departed from the plan, and why:

- **A cell canvas instead of lipgloss joins.** Every frame is drawn into a
  grid of cells and serialized once (`tui/canvas.go`). cats-todo's rule — one
  layout description both draws and hit-tests — becomes structural: widgets
  draw through clipped Surfaces, so a long value cannot shift a click target
  in another pane, and overlays need no compositor. lipgloss is not a
  dependency.
- **Two plan items were folded or cut.** Column-border resize went to the
  backlog (N-031). Chat archiving went too (N-027).
- **Shared state moved, the old UI untouched.** History and buffer
  persistence moved to `userdata/` for the new UI, with the same files on
  disk. The host-palette mapping went to `theme.FromHost`. The classic UI keeps
  its own copies until it is retired (N-025).
- **`ai.StartPipes` and `ai/aitest` were added**, so UI tests drive a
  scripted agent. The real one is never spawned by a test.

Lessons for next time:

- **Keep tests away from a live cats host.** A developer's terminal may itself
  be a cats pane (this session's was), so the tui test harness clears the
  `CATS_*` variables. Without that, tests report "dbc" states to the live pane
  and dial its socket.
- **pyte (used for pty screen checks) does not implement SU/SD** (`CSI n S`
  / `CSI n T`). Bubble Tea's renderer scrolls regions with them, so an
  unpatched pyte shows phantom boxes after a pane resize. The frame is
  correct, as a forced repaint confirms. The pty driver used in this session
  patches both.
