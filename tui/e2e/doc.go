// Package e2e drives dbc's TUI in a pseudo-terminal: the built binary,
// real keys and SGR mouse events, and every check read off the screen a VT
// emulator draws from its output. The tests in tui/ stop at the Model and
// its rendered frames; this is what sees what a terminal would show —
// that the binary starts, draws, reads its keys and mouse, and quits.
//
// It is opt-in, like web/e2e, and skipped unless asked for:
//
//	cd tui/e2e
//	DBC_TUI_E2E=1 go test -count=1 -v .
//
// Everything is self-contained: the test builds dbc, writes a config with
// two file-backed SQLite connections under a temporary HOME, seeds them
// with the headless `dbc -c conn "SQL"` mode, and runs the TUI at 140×45.
//
// What it cannot see: how a particular terminal renders the glyphs and
// colors (iTerm2, Terminal.app, kitty, Ghostty), and real mouse hardware's
// wheel and drag streams. The emulator is charmbracelet/x/vt, the same
// family of code the TUI's own renderer comes from, so a glyph-width
// disagreement between the two would not show here either.
//
// WHY A NESTED MODULE. creack/pty and charmbracelet/x/vt are test tooling
// for one opt-in suite; as a module of its own they never enter dbc's
// go.mod, and `go test ./...` at the repository root does not descend into
// it. The test drives the built binary as a user would and imports none of
// dbc's packages, so it needs no replace directive.
package e2e
