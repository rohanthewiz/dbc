# UI revamp: mouse-first Bubble Tea UI, AI assistant, rich copy

Session: ea5a6528-1387-4788-8311-f01e045aebad
Date: 2026-09-24

## Ask

1. `/next-list seed` — build the living follow-up list from session history.
2. "The UI needs a major revamp": mouse support as a first-class citizen,
   technology hints from `~/projs/go/cats-todo`, AI chat assistance with
   Copilot by default as in `~/projs/go/ced` (send the query and the first 10,
   configurable, rows as context), and copying results as a nice HTML table
   into Teams, plus Markdown and CSV.

## Decisions taken to the user

| Fork | Chosen |
|---|---|
| UI stack | Rewrite on Bubble Tea v2 (cats-todo's stack), old tview UI kept as `-ui classic` |
| AI transport | ACP sidecar like ced: `copilot-language-server --acp` |
| Rows to the AI | **Off by default**, per-connection opt-in `ai_rows = true` |

## What happened

### Living list seeded

`ai_docs/todo/next-list.md` created from all ten session docs. Found lapsed
items (cell inspector, status-bar truncation, headless SQL-from-file, …),
and one premise wrong in the docs: editor-buffer persistence *was* done
(`5104d9e`) though `fable_recommends.md` still shows it unticked.

### Research (two Explore agents)

- **ced makes no Copilot HTTP calls.** It runs `copilot-language-server`
  and speaks JSON-RPC: LSP mode for sign-in, `--acp` (Agent Client Protocol)
  for chat. The binary owns tokens and refresh. ced's transport
  (`internal/lsp` client core + acp/ndjson) is stdlib-only and portable.
- **cats-todo** has hand-made hit-testing (one layout description draws and
  hit-tests, fixed row constants, lipgloss Compositor for overlays,
  per-screen MouseMode), no wheel, no table, no clickable editor.
- **dbc's HTML copy was never rich**: it put HTML *source* on the clipboard.

Plan written to `ai_docs/plans/ui-revamp.md` (phases 0–3, then a Status
section recording deviations and lessons).

### Commits (in order, not pushed until this wrap)

1. `20be4e4` **feat(clip)** — `clip` package writes text + HTML flavors in
   one operation: macOS `osascript -l JavaScript` → NSPasteboard (payload as
   JSON on stdin, no cgo), Windows user32 CF_HTML (offset envelope built in
   `clip/cfhtml.go`, copy via RtlMoveMemory to satisfy vet), Linux
   wl-copy/xclip `-t text/html`. `export.HTMLFragment`: inline styles,
   paired light colors (reads in light and dark Teams), numbers
   right-aligned, NULL muted italic. `export.ClipContent`/`ToClipboard` and
   the classic UI's export modal use it. Live round-trip test opt-in via
   `DBC_CLIP_LIVE=1`.
2. `71281f6` **feat(ai)** — `ai` package: ported ACP JSON-RPC client
   (`ai/rpc.go`), agent registry (Copilot default, Claude Code, Gemini),
   `Chat` with Start/Send/Cancel/SetModel/Close reporting Events on a
   channel. No fs capability; every permission request declined. `ai.Build`
   applies the data rule (query + error always; rows only with `ai_rows`,
   capped by `ai_context_rows`, default 10) and returns the transcript note.
   Config keys `ai_agent`, `ai_model`, `ai_context_rows`, per-connection
   `ai_rows`. Live handshake (`DBC_AI_LIVE=1`) listed 25 Copilot models.
3. `ecd044b` **feat(tui)** — the new UI, now the default:
   - `tui/canvas.go`: frames drawn into a cell canvas via clipped Surfaces;
     one `layout` value drives both drawing and hit-testing. Zero `Color`
     means terminal default (a set bit marks real colors).
   - `tui/editor.go`: no-wrap SQL editor (click → exactly one caret),
     highlighting from new `sqlsplit.Lex` (same scanner as the splitter),
     indent carry, word-grouped undo, multi-click selection.
   - `tui/grid.go`: virtualized grid, typed sort (NULLs last), range
     selection, copy as HTML/Markdown/CSV/TSV/JSON, inspector, scrollbar.
   - toolbar, sidebar (connections + tables, double-click previews), log
     that wraps at draw time, right-click menus with dim-but-explaining
     disabled rows, modals (export, history, scripts, inspector), draggable
     pane borders, wheel under the pointer.
   - `tui/chat.go`: assistant pane — lazy start, streaming, model/agent
     menu, context chip, ⤓ insert / ⧉ copy on code blocks, Ctrl+K stop.
   - `tui/cats.go`: cats glue ported (hooks incl. assistant-answering as
     working, host theme via new `theme.FromHost`, ⌘ keys, ^G picker,
     OSC 7 via `tea.Raw`, title via `View.WindowTitle`).
   - `userdata/`: history + buffer shared by both UIs (same files on disk).
   - `ai.StartPipes` + `ai/aitest` scripted agent for UI tests.
   - README rewritten for the new UI; `-ui` flag / `DBC_UI`.

## Verification

- Full suite green, `-race` clean; tui tests drive the model like a session
  (keys, clicks, drags) against seeded SQLite and a scripted agent.
- Real binary driven in a pty with pyte: run, drag-select, right-click
  menu, splitter drags, assistant pane (auth-required path shown correctly
  under a scratch HOME), double-click inspector.

## Gotchas worth remembering

- **This session ran inside a live cats pane.** Early tui test runs and pty
  launches inherited `CATS_*` and reported "dbc" states to that pane. Tests
  now clear the vars (`newTestModel`; `newTestModelInHost` for fake-host
  tests); the pty driver strips them. Saved to memory.
- **pyte lacks SU/SD** (`CSI n S`/`T`); Bubble Tea scrolls regions with
  them, so unpatched pyte shows phantom boxes after resizes. Not a dbc bug.
- Harness must render after every Update (hit-tests read the last layout)
  and must not re-arm the chat event pump on a directly-fed chat event.
- `strings.Index` on drawn lines gives bytes; box-drawing chars are 3 bytes,
  1 cell — convert with `width(line[:i])`.
- `^A` is now the assistant; editor select-all moved to `Alt+A`.

## Next

Closed: N-001, N-002, N-016, N-017, N-018, N-019, N-020, N-021, N-022,
N-023, N-024. Declined: N-012, N-013, N-014, N-015 (recorded at seeding).
Raised: N-021, N-022, N-023, N-024, N-025, N-026, N-027, N-028, N-029, N-030,
N-031. Deferred: None. Promoted: None. Updated: None.
Full list: `ai_docs/todo/next-list.md`.
