# X11 clipboard owner: rich copy serves text too

Session: 45891005-9cd0-4a71-b511-bcdd02e309fd
Date: 2026-09-25

## Ask

Next-list item **N-030**: Linux rich copy offered text/html only (wl-copy and
xclip set one type), so a terminal paste right after an HTML copy got nothing
or markup. First re-check the premise, then (on the user's call) build the fix.

## Premise check

Context that decides it: HTML is the only format that makes a rich copy, and
its plain-text flavor *is* the markup (`export.ClipContent`, Text == HTML). A
terminal paste is meant to get markup, as on macOS and Windows.

- **Wayland: no gap.** `wl-copy --type text/html` also offers `text/plain`,
  `text/plain;charset=utf-8`, `TEXT`, `STRING`, `UTF8_STRING` with the same
  bytes for any `text/*` type (`wl-copy.c`, `mime_type_is_text`). The old
  comment's "some compositors convert" was really wl-copy, every time.
- **X11: real gap.** xclip lists only `TARGETS` and `text/html`; GTK/Qt
  clients read TARGETS first and paste nothing.
- **No cheap fix.** xclip's `-alt-text` (adds a STRING target) landed on its
  master in 2023; the last release, 0.13 (2016), is what distros ship.

Committed as `81e3e0b`: corrected comments in `clip/rich_unix.go` and
`clip/clip.go`, and narrowed N-030 to X11.

## Change

- **`clip/x11owner.go` (new).** dbc owns the X11 CLIPBOARD selection itself
  via `github.com/jezek/xgb` (pure Go; CGO stays off):
  - `writeX11` re-execs `os.Executable()` with a sentinel argument plus
    `DBC_CLIP_X11_HELPER=1`, payload JSON on stdin, `Setsid` so the helper
    survives dbc exiting, the terminal closing, or Ctrl+C. It waits up to 3s
    for `ok` on the helper's stdout, then reaps it in a goroutine.
  - The helper is caught by an `init()` in `clip` (arg **and** env required),
    so any binary linking `clip` — dbc and test binaries — can serve; no
    change to `main.go`. A helper refuses to launch another.
  - Owner: hidden InputOnly window, real server timestamp (PropertyNotify
    trick, ICCCM §2.1), `SetSelectionOwner` confirmed by
    `GetSelectionOwner`. Serves TARGETS, TIMESTAMP, text/html, UTF8_STRING,
    text/plain;charset=utf-8, text/plain, TEXT (as UTF8_STRING) and STRING
    (real Latin-1, `?` for the rest). MULTIPLE is refused.
  - INCR for values over one request (xgb writes a 16-bit request length, so
    chunks are `MaximumRequestLength` bytes, ~64 KiB, as xclip does). BadWindow
    drops a vanished requestor's transfers; transfers idle 30s are dropped.
  - Exits on SelectionClear once in-flight transfers finish.
- **`clip/rich_unix.go`.** Wayland + wl-copy unchanged; with `$DISPLAY` it now
  tries `writeX11` first and falls back to xclip (old HTML-only behavior).
  On Wayland without wl-copy, XWayland bridges the X11 owner.
- **`clip/live_linux_test.go` (new, opt-in `DBC_CLIP_LIVE=1`).** Reads every
  target back with `xclip -o`, a ~1.5 MB INCR copy, and checks no helper
  survives another client copying.
- README's Linux requirement is now "an X11 display, or `wl-copy` on
  Wayland"; package doc table updated.

## Verification

- `GOOS={linux,freebsd,darwin,windows} go vet ./clip`, `go build ./...`.
- `go test ./...` green on macOS and in a `golang:1.26` container.
- Docker + Xvfb: the live test passes. A headless `dbc script` doing
  `s.Export(r, "html", "")` exited 0; its helper stayed up (own session,
  parent 1), TARGETS listed all 8 targets, a `UTF8_STRING` read returned the
  HTML source, and the helper exited after `xclip -i` took the clipboard.
- Not yet tried on a real X11 desktop (raised as N-044).

## Next

Closed: N-030. Declined: None. Raised: N-044.
Deferred: None. Promoted: None.
Updated: None. Full list: `ai_docs/todo/next-list.md`.
