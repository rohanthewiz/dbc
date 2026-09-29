# dbc.app — `dbc web` as a macOS app

Session: `51ee546c-baba-40f5-96c0-9eb87d0eb77d`

## Ask

Next-list item N-053, dropped in from the user's cats-todo backlog: a macOS
app wrapper for `dbc web`, the way gonotes has one. It was the optional last
item of the web plan's Phase 6. It had been left out because `dbc web` and
the cats "dbc — web" action already launch the server, and a `.app` would be
a second packaging path to keep building and signing. `/api/v1/health` was
already there for a wrapper to poll.

## What was built

Starting point: gonotes' `mac-install.sh`, which embeds a Swift/WebKit
`AppDelegate` in a heredoc, polls `/api/v1/health`, and adopts an
already-running server when the health check passes.

### `mac-install.sh` (repo root)

- **Source.** Run as a file inside a dbc checkout (its `go.mod` names
  `github.com/rohanthewiz/dbc`), it builds that checkout as it is,
  uncommitted changes included, and leaves its git state alone. Piped from
  curl, it clones `main` into `~/.dbc-src`, or fetches and runs
  `git reset --hard` there. `DBC_APP_SRC` overrides both.
- **Go.** The `go` on PATH if it is at least the version in `go.mod`'s `go`
  line. Otherwise a private copy in `~/.local/go`, downloaded once and
  reused.
- **Build.** `CGO_ENABLED=0 go build -trimpath -ldflags "-s -w"`, the same as
  goreleaser. Everything is built into a `mktemp` stage, and the bundle is
  moved into place only at the end, so a failed build leaves the installed
  app untouched.
- **Bundle.** `Contents/MacOS/dbc-app` is the Swift shell and
  `Contents/Helpers/dbc` is the Go binary. It is in Helpers rather than
  Resources because codesign treats Helpers as nested code.
  `Contents/Resources/dbc.icns` is the icon.
  - `CFBundleShortVersionString` is the output of `dbc version`.
  - `CFBundleVersion` is the short commit hash, with `+dirty` when the tree
    has uncommitted changes.
- **Signing.** Ad hoc: the helper first, then the bundle, then
  `codesign --verify --strict`. A failure is only a warning.
- **Registration.** `lsregister -f` makes Finder, Spotlight and the Dock
  pick up the new icon and version.
- **Running app.** If the app is running during an install, the script warns
  that the running copy keeps the old build.
- Env overrides use the `DBC_APP_*` prefix, because dbc itself already reads
  `DBC_DEMO`, `DBC_WEB_SECRET` and others.

It is local-build-only by design. The app is never a release artifact, so
there is nothing to notarize and no second pipeline, which answers the
original objection.

### `macapp/DbcApp.swift`: the shell

The Swift source is a real file, not a heredoc like gonotes', so it can be
reviewed and edited. The file's header has a flow diagram. In short:

1. Read the login shell's environment: `$SHELL -i -l -c "printf <marker>;
   /usr/bin/env -0"`, parsed after the marker.
   - It reads the output as it arrives, so a large environment can't fill
     the pipe and block the shell.
   - It waits for the shell's exit, not for EOF: a daemon the shell starts
     can hold stdout open.
   - It gives up after 10 seconds. On failure it falls back to the app's own
     environment plus `/opt/homebrew/bin` and `/usr/local/bin`.
   - **Why:** an app started from Finder gets launchd's bare PATH. The
     assistant's agents are npm binaries that need node, a DSN may say
     `${PGPASS}`, and agents read keys such as `ANTHROPIC_API_KEY`.
   - Terminal-session variables (`PWD`, `TERM`, `SHLVL`, …) and `CATS_*` are
     removed from the merged result.
2. Pick a free port by binding `127.0.0.1:0`, and generate a 32-character
   secret with `SystemRandomNumberGenerator`.
3. Spawn `dbc web --no-open --exit-on-eof --listen 127.0.0.1:<port>`:
   - the secret goes in `DBC_WEB_SECRET`, not argv, so `ps` does not show it
   - the working directory is `~`, so `~/dbc.toml` and then
     `~/.config/dbc/config.toml` are the config
   - stdin is a pipe whose write end the app holds
   - stdout and stderr go to `~/Library/Logs/dbc/dbc-web.log`, created fresh
     each launch with mode `0600` because it contains the login link; the
     previous log is kept as `dbc-web.log.1`
4. Poll health for up to 30 seconds, stopping early if the process exits.
   Then load `/login?s=<secret>`.

**Why the app never adopts an already-running server** (gonotes does): the
app can't know that server's secret, so it couldn't sign in. A separate
server also leaves a terminal `dbc web` on 8450 working beside the app. A
new port on every launch loses nothing, because the page keeps only
sessionStorage.

The window side:

- **Data store.** The website data store is non-persistent. The cookie is per
  launch, and tabs and layout are stored server-side in `web.bytdb`.
- **One class for every window.** `BrowserWindow` is an `NSWindowController`
  that is also the navigation, UI and download delegate. Being the window's
  controller puts it in the responder chain, so the View menu's zoom actions
  reach the key window's page.
- **Downloads.**
  - `<a download>` anchors (`shouldPerformDownload`) are downloaded.
  - So are responses marked `Content-Disposition: attachment` or with a MIME
    type the view can't show.
  - Each goes to `~/Downloads`, numbered `-2`, `-3`, … when the name is
    taken, and the Dock's Downloads stack bounces when it finishes.
- **`window.open`.**
  - A URL on the server opens in a second window that shares the
    configuration, so it has the session cookie. This is for the plan's
    standalone page.
  - Anything else opens in the default browser, as do links to other sites
    in the main frame.
- `alert()` and `confirm()` are shown as sheets.
- If the page's web process dies, the view reloads.
- The web view is inspectable (macOS 13.3+).
- The web view stays hidden behind a "starting dbc web…" label on the
  theme's background until the first `didFinish`, so no white flash.
- Closing the workbench window quits the app, so a plan window can't be left
  as the only window with no way back.
- **Menus.**
  - The app, Edit, View and Window menus.
  - Key equivalents avoid the page's own shortcuts (⌘R, K, P, E, B, I, O),
    so Reload Page has none.
  - View ▸ Open in Browser signs the default browser in with this launch's
    secret.
  - View ▸ Show Log opens the log.
- **Quit.** It returns `.terminateLater`, sends SIGTERM, and replies once the
  server exits, or after 8 seconds at most. That way, reopening the app right
  away finds `web.bytdb` free.
- **Unexpected exit.** If the server exits on its own, or never becomes
  healthy, one alert with Quit or Show Log is shown, then the app quits.

### `macapp/MakeIcon.swift`

Draws the iconset in AppKit using `theme.Bg` and `theme.Accent` (the
favicon's colors). It is a dark squircle with a green database cylinder
(a lid, a body and two bands), and "dbc" in monospace below it at 64px and
larger. It needs a window-server connection, so the installer treats a
failure as non-fatal.

### Go: hidden `dbc web --exit-on-eof` (`webcmd.go`)

`exitOnEOF(os.Stdin, interruptSelf)` copies stdin to `io.Discard` until it
ends, then sends the process SIGTERM. rweb already handles SIGTERM like
Ctrl+C, so there is a single shutdown path. The kernel closes the app's end
of the pipe however the app exits (Quit, a crash, Force Quit, `kill -9`), so
the server is never orphaned holding `web.bytdb` and its database sessions.
The flag is `Hidden`, so it is not in `--help`.

`TestCatsManifest_CompletionsCoverCLI` now skips flags whose
`cli.VisibleFlag.IsVisible()` is false, so the completions don't offer what
`--help` hides. New `webcmd_test.go` tests that the wait survives writes
while the pipe is open, and that EOF or a read error calls `stop`.

### README

A new "As a macOS app" subsection at the end of the `dbc web` section
covers:

- installing, and what the installer needs
- the app's own port and secret
- the login-shell environment and the home working directory
- the single-holder caveat for `web.bytdb`
- downloads, windows and the log
- how Quit and a crash shut the server down

## Verification

- `go vet ./...`, `go test ./...` and `swiftc -typecheck` all clean.
- `dbc web --exit-on-eof` run directly with a stdin pipe: health returned 200,
  and after the pipe closed it exited with status 0 in 0.02s, logging
  "stopping: canceling runs and releasing sessions…".
- Installed into the scratchpad (`DBC_APP_DIR=…`) and launched with `open`:
  - The server ran with the expected argv, and stdin was a PIPE (`lsof`).
  - Health reported `workspaces: 0`, then `1`: the page signed in and its
    JavaScript opened a workspace.
  - The log was `0600`, and rotation to `.1` worked on relaunch.
  - The server's `PATH` was the full login-shell PATH, and
    `copilot-language-server` resolved. `claude-code-acp` and `gemini` are
    not installed on this machine.
  - The first run leaked `TERM` and `PWD`: `open` from a terminal passes the
    terminal's environment to the app. Fixed by filtering the merged
    environment. The recheck found no `TERM`, `PWD` or `CATS_*`.
  - Quit (AppleScript): the server logged its shutdown, and both processes
    were gone.
  - `kill -9` of the app: the server saw EOF and shut down cleanly.
- `dbc web --help` does not list `--exit-on-eof`.
- Rendered the icon at 256 and 32 pixels and checked both by eye.
- A screenshot of the window failed (`screencapture -l`: no screen-recording
  permission), so nothing that needs clicking in the window was exercised.
  That is N-064.

## Gotchas

- `open App.app` run from a terminal hands the app that terminal's
  environment (`PWD`, `TERM`, …). Filter the merged environment, not only
  the shell overlay.
- `swiftc` 6.4 compiles the file in Swift 5 language mode with no warnings.
  `-swift-version 6` would need `@MainActor` annotations.
- The test runs created `~/Library/Logs/dbc/`, the app's real log location.
  They also left a test `dbc.app` in the session scratchpad, registered with
  LaunchServices.

## Next

Closed: N-053. Declined: None. Raised: N-064.
Deferred: None. Promoted: None.
Updated: None. Full list: `ai_docs/todo/next-list.md`.
