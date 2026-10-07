# Scripts revamp phase 5: the TUI browser and `$EDITOR`

Session: `a9036bb3-86e4-426e-a307-a2766a31c773`

## Ask

`/sess-load`, then "Do phase 5" of `ai_docs/plans/scripts-revamp.md`: the
TUI's scripts browser, editing through `$EDITOR`, checked with the
`tui/e2e` harness with `EDITOR` set to a script that edits the file.

## The browser (`tui/scripts.go`, new)

It replaces the run-only `scriptsModal` that was in `tui/modals.go`. It
keeps the type name, so `openScripts(sel)` now takes the script to select.

- **Rows.** Sections are drawn with the list widget:
  - Scripts. With none, a note naming the dir and the five templates.
  - Examples ("read-only — Enter makes your own copy").
  - Trash, folded. `t` or a click on its heading unfolds it.
  - Each row has a muted description in one column (`descCol`), and on
    the right an age (`ago`: now, 5m, 3h, 2d, Jan 2), "example", or
    "Enter restores".
  - The title is `Scripts · <~-shortened resolved dir>`.
- **Keys are bare letters.** The plan had these; the web had to use chords
  instead. The list has the keyboard:

  | key | does |
  | --- | --- |
  | Enter | run |
  | `e` | edit |
  | `n` / Alt+N | template menu |
  | `d` | duplicate |
  | `r` / F2 | rename |
  | Del / `x` | trash, no question asked |
  | `y` | copy path |
  | `t` | trash fold |
  | `/` | the filter |
  | Esc | close |

  - The filter is a mode. In it, typing narrows the list, ↑↓ still move,
    Enter does the row's action, and Esc or Tab goes back to the list.
  - Why not the web's chords: ⇧Enter is plain Enter outside the kitty
    protocol, and Ctrl+Delete varies between terminals.
- **Each row kind has one Enter.**
  - A script runs.
  - An example is copied into your scripts.
  - A template starts a new script.
  - A trashed script is restored. When its name has been taken since, a
    prompt offers `name-2.go`.
- **Mouse.** A double-click edits (on other rows it does their action). A
  right-click opens the row's menu (Run, Edit in `<editor>`, Duplicate…,
  Rename…, Move to the trash, Copy path). The `+ New` chip opens the
  template menu.
- **Making a script.** New, duplicate and copy-an-example go through
  `makeScript`:
  1. A name prompt, offered as `freeName` with the stem selected.
  2. `SaveScript(base "")`.
  3. Then edit.

  A taken or invalid name is shown under the field, and the prompt stays
  open. Templates are filled with `config.ConnOrder(m.ws.Active())`.
- **Editing** (`editScript`):
  - It uses `tea.ExecProcess` (through the `execEditor` var, which tests
    stub) with `$VISUAL`, else `$EDITOR`, else `vi`.
  - The command is split on spaces, so `code -w` works.
  - A missing binary is logged ("no editor … on PATH — set $VISUAL or
    $EDITOR"), and nothing is suspended.
- **When the editor exits** (`scriptEdited`):
  - It re-reads the file, logs saved or unchanged, and runs
    `script.Check`.
  - Each diag goes to the log as `~/…/name.go:L:C: msg`, at error or warn
    level, capped at 20.
  - The browser reopens on the script, so Enter runs it.
- **Actions only for your own scripts.** Rename, trash and copy path on an
  example, template or trash row say why in the log ("only a script of
  yours can be renamed — this row is a built-in example…").

## Plumbing

- **`tui/list.go`.** `listItem.head` marks rows the cursor steps over
  (`settle`, `firstPick`). They are drawn muted and bold and never hover.
  It also gained `desc` and `list.descCol`.
- **`tui/prompt.go`.** It gained `acceptCmd` (an accept that returns a
  command) and `cancel` (run on Esc, Cancel or ✕). The browser's prompts
  use `cancel` to reopen the browser.
- **`tui/app.go` key, `tui/mouse.go` click.** A modal that reports
  `closed` is dropped only if it is still `m.modal`. Before this, a prompt
  that reopened the browser had the browser cleared right after.
- **`config.ConnOrder(first)`.** It returns first (if known), the
  default, then the rest. `web/scripts.go`'s `templateConns` now calls it.
- **Docs.**
  - `tui/help.go` gained a "Scripts browser (^O)" group and a new ^O line.
  - README's Ctrl+O key row.
  - The plan: header "Phases 1–5 are done" and a phase 5 Outcome.
  - `tui/e2e/doc.go` explains the `$EDITOR` stand-in.

## Verification

- `go build ./... && go vet ./... && go test ./...` all pass, and gofmt is
  clean. `tui/e2e` vets.
- **New unit tests** (`tui/scripts_test.go`):
  - The browser: `TestScriptsBrowserEmptyDir`, `…NewFromTemplate` (the
    created file names `demo-sqlite`, and the check's `6:9: undefined`
    line is logged), `…NamePrompt`, `…Runs`, `…Filter`,
    `…RenameTrashRestore`, `…RenameCancel`, `…CopiesAnExample`,
    `…ScriptOnlyActions`, `…NoEditor`, `…Mouse`.
  - Helpers: `TestEditorLine`, `TestAgo`, `TestFreeName`,
    `TestListSkipsHeadings`.
  - `TestScriptRunsFromThePicker` was updated to use `selectName`.
- **`tui/e2e`.**
  - `writeEditor` makes `$HOME/edit.sh`, which copies `next-edit.go` over
    `$1`. `environ` drops the developer's VISUAL/EDITOR and points EDITOR
    at the stand-in.
  - The new step:
    1. Ctrl+O on an empty dir.
    2. `n` → Query and show → name `nightly`.
    3. The editor writes a broken script, and the log shows
       `nightly.go:6:9: undefined: undefinedThing`.
    4. `e`, and the editor writes a good script that checks clean.
    5. Enter runs it ("3 cats", grid `│ cat` / `│ Leo`).
    6. Del → `t` → `G` → Enter restores it.
  - The real binary's suspend and resume through `tea.ExecProcess` worked
    in the pty.
  - **The first step was stale on a clean HEAD.** It waited for row counts,
    which have been opt-in since N-124. It was confirmed failing in a clean
    worktree of 6c67fdf. It now does Ctrl+L, Tab, `#`, then clicks back
    into the editor.
  - The full suite passed 3 of 3 runs.
- I looked at frames of the browser (scripts, examples, open trash), the
  template menu over it, and the footer. The footer was shortened to fit
  at 120 columns.
- **Not done:**
  - A real editor (vim, nvim, `code -w`) in real terminals and in a cats
    pane. Added to N-115.
  - The README body and the skill. That is phase 6.

## Next

Closed: None. Declined: None. Raised: N-136.
Deferred: None. Promoted: None.
Updated: N-130, N-132, N-115. Full list: `ai_docs/todo/next-list.md`.
