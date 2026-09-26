# Light plan pictures from the shell and the TUI (N-059)

Session: 334b9f82-ef3a-4d6e-9be1-e31f352f5014
Date: 2026-09-25

## Ask

Next-list item N-059. `dbc explain -t pdf|jpeg|png` and the TUI's Save as
PDF / JPEG always drew in the dark palette. Only dbc web followed the view's
light or dark. A light choice for the shell, as a flag or a config key, would
make printed PDFs friendlier.

This session added both. Then the user asked for three more steps: commit;
rebase onto the main repo's `main`, taking care with `next-list.md`
conflicts; and fast-forward `main`.

## Changes (`f5bfc11`)

| Piece | Change |
|---|---|
| `theme/palette.go` | `ByName(name)`: `""` or `"dark"` returns `Default()`, `"light"` returns `Light()`. It ignores case and surrounding space, and any other name is an error. Only the two built-ins can be named. A cats host palette (`FromHost`) belongs to the terminal dbc runs in, and a picture goes to people who aren't looking at that terminal. |
| `config/config.go` | `PlanTheme string` (`plan_theme`), defaulting to `"dark"`. `checkPlanTheme` in `Load` normalizes it to `"dark"` or `"light"`. An unknown value adds a warning and falls back to dark instead of failing the load, since it's only cosmetic. The field is a string, not a `theme.Palette`, so config stays a leaf package. |
| `explain.go` | New `--theme light\|dark` flag. `explainAction` checks it before the explain runs, as with the binary-on-terminal check, so an `--analyze` isn't spent on a mistake. A bad value is a usage error (exit 2). So is `--theme` with a format that has no palette (text, json, html, markdown, mermaid), because a flag that silently does nothing reads as a bug. `planPalette(cfg)` picks the flag first, then `plan_theme`, then dark. `renderPlan` now takes a `theme.Palette` and passes it to the three picture renderers. The doc comment and the command description mention the option. |
| `tui/explain.go` | `planFile` draws in `theme.ByName(m.cfg.PlanTheme)`. |
| `explain/picture.go` | The `PictureOptions.Palette` comment says who passes what. |
| `dbc.example.toml`, `README.md` | Document `plan_theme` and `--theme`, with a new example line: `-t pdf --theme light`. |
| `ai_docs/todo/next-list.md` | N-059 moved to Closed. |

dbc web is unchanged: its `plan.pdf`, `.jpg` and `.png` still follow `?theme=` from the view.
Mermaid ignores the palette. The page that renders a Mermaid chart applies its own theme.

## Verified

- New tests:
  - `theme`: `TestByName`.
  - `config`: `TestLoadPlanTheme` covers absent, `"Light"`, `"dark"`, and a typo that warns and falls back to dark.
  - `explain_test.go`: `TestExplainPictureTheme` checks the precedence (flag, then key, then dark). It also decodes a rendered PNG and checks that the corner pixel is exactly the palette's `Bg`, dark and light. `TestExplainCLIParsing` now parses `--theme light`.
  - `tui/explain_test.go`: `TestPlanFileLightTheme` saves a JPEG with `plan_theme` dark, then light, and checks the corner's brightness. The check is loose because JPEG is lossy.
- By hand with a built binary: dark, `--theme light`, and `--theme LIGHT` as a PDF all rendered correctly. A config with `plan_theme = "light"` drew light, and `--theme dark` overrode it to dark. I looked at each PNG. `--theme lite` was refused with exit 2, and `--theme light` with text output was refused.
- After the rebase, `go vet ./...` and `gofmt` are clean, and `go test ./...` is green with `CATS_*` stripped.

## Rebase and fast-forward

- `main` had gained N-057 (`403094c` and `2434591`: `connections.toml`) since the branch was cut from `321d815`.
- The only conflict was `next-list.md`:
  - **Open:** main removed N-057 right next to where the branch removed N-059, so neither entry was kept.
  - **Closed:** both sides added an entry at the top. N-059 now sits above N-057, since the list is newest first.
  - After the fix, `git diff main` on the file showed only the N-059 move.
- `config.go` and `README.md` merged automatically. The full test suite passed on the combined code.
- `main` in `~/projs/go/dbc` was fast-forwarded to `f5bfc11`. Nothing was pushed.

## Notes

- The first full `go test` run printed test-flag usage for `workspace`.
  The cause was building the `env -u CATS_…` list through `xargs`, not the
  tests. `env $(env | grep -o '^CATS_[A-Z_]*' | sed 's/^/-u /') go test ./...`
  works, and `workspace` passed `-count=3`.
- `dbc explain --open` and the TUI's `b` still write the interactive page,
  which has its own light toggle. `plan_theme` doesn't apply to it.

## Next

Closed: N-059. Declined: None. Raised: None.
Deferred: None. Promoted: None.
Updated: None. Full list: `ai_docs/todo/next-list.md`.
