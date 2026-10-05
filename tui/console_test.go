package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rohanthewiz/dbc/config"
	"github.com/rohanthewiz/dbc/userdata"
)

// consoleModel is a test model with consoles on, in a directory of the
// test's own, and three file-backed SQLite connections beside the demo:
// "a" and "a-too" on one file, "b" on another.
func consoleModel(t *testing.T) (*Model, string) {
	t.Helper()
	m := newTestModel(t)
	files := t.TempDir()
	m.cfg.Connections = append(m.cfg.Connections,
		config.Connection{Name: "a", Driver: "sqlite", DSN: "file:" + filepath.Join(files, "a.db")},
		config.Connection{Name: "a-too", Driver: "sqlite", DSN: filepath.Join(files, "a.db")},
		config.Connection{Name: "b", Driver: "sqlite", DSN: filepath.Join(files, "b.db")},
		config.Connection{Name: "broken", Driver: "sqlite", DSN: "file:" + filepath.Join(files, "no", "such", "dir", "x.db") + "?mode=ro"},
	)
	m.consoleDir = filepath.Join(t.TempDir(), "consoles")
	if err := os.MkdirAll(m.consoleDir, 0o755); err != nil { // no legacy seeding here
		t.Fatal(err)
	}
	m.editor.SetText(m.openConsole(m.ws.Active(), ""))
	return m, m.consoleDir
}

// The editor follows the database: switching saves the console being left
// and opens the one arrived at, so each database keeps its own running SQL.
func TestConsoleFollowsTheDatabase(t *testing.T) {
	m, _ := consoleModel(t)
	demo := m.console

	m.editor.SetText("SELECT 'demo'")
	drive(t, m, nil, m.setActive("a"))
	if got := m.editor.Text(); got != "" {
		t.Fatalf("a's new console opened with %q, want empty", got)
	}
	if text, _ := userdata.LoadConsole(demo); text != "SELECT 'demo'" {
		t.Errorf("the demo console saved %q on the way out", text)
	}

	m.editor.SetText("SELECT 'a'")
	drive(t, m, nil, m.setActive("b"))
	m.editor.SetText("SELECT 'b'")

	drive(t, m, nil, m.setActive("a"))
	if got := m.editor.Text(); got != "SELECT 'a'" {
		t.Errorf("back on a, editor = %q", got)
	}
	drive(t, m, nil, m.setActive(config.DemoSQLite))
	if got := m.editor.Text(); got != "SELECT 'demo'" {
		t.Errorf("back on the demo, editor = %q", got)
	}
	if !strings.Contains(logText(m), "console: memory · "+config.DemoSQLite+" · console") {
		t.Errorf("the switch was not logged:\n%s", logText(m))
	}
}

// Two connections onto one database share its console: switching between
// them leaves the editor alone, unsaved edits and all.
func TestConsoleSharedByOneDatabase(t *testing.T) {
	m, _ := consoleModel(t)
	drive(t, m, nil, m.setActive("a"))
	m.editor.SetText("SELECT 'typed on a'")
	drive(t, m, nil, m.setActive("a-too"))
	if got := m.editor.Text(); got != "SELECT 'typed on a'" {
		t.Errorf("a-too (same file as a) swapped the editor to %q", got)
	}
}

// A connect that fails leaves the connection where it was, and the console
// stays with it.
func TestConsoleStaysOnAFailedConnect(t *testing.T) {
	m, _ := consoleModel(t)
	before := m.console
	m.editor.SetText("SELECT 'still here'")
	drive(t, m, nil, m.setActive("broken"))
	if m.ws.Active() == "broken" {
		t.Fatal("the broken connection connected; the test needs it to fail")
	}
	if m.console != before || m.editor.Text() != "SELECT 'still here'" {
		t.Errorf("a failed connect moved the console to %q, editor %q", m.console, m.editor.Text())
	}
}

// The first start with consoles seeds the opening console from the old
// single buffer.sql — once: after that, a console with no file opens empty.
func TestConsoleSeededFromLegacyBufferOnce(t *testing.T) {
	m, dir := consoleModel(t)
	if err := os.RemoveAll(dir); err != nil { // as before the first start with consoles
		t.Fatal(err)
	}
	legacy := filepath.Join(t.TempDir(), "buffer.sql")
	if err := os.WriteFile(legacy, []byte("SELECT 'from before'"), 0o600); err != nil {
		t.Fatal(err)
	}

	if got := m.openConsole("a", legacy); got != "SELECT 'from before'" {
		t.Errorf("first start opened %q, want the legacy buffer", got)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("the consoles directory was not made on seeding: %v", err)
	}
	if got := m.openConsole("b", legacy); got != "" {
		t.Errorf("a later start seeded again: %q", got)
	}
	// with persistence off there is no console, and the legacy buffer is
	// all there is
	m.consoleDir = ""
	if got := m.openConsole("b", legacy); got != "SELECT 'from before'" {
		t.Errorf("no consoles: opened %q, want the legacy buffer", got)
	}
}

// ⌥N opens a fresh console of the same database, ⌥C cycles through them,
// and a switch away and back reopens the console last open there.
func TestNewAndNextConsole(t *testing.T) {
	m, _ := consoleModel(t)
	drive(t, m, nil, m.setActive("a"))
	m.editor.SetText("SELECT 'first'")

	key(t, m, "alt+n")
	if m.consoleName != "console-2" || m.editor.Text() != "" {
		t.Fatalf("after ⌥N: console %q, editor %q", m.consoleName, m.editor.Text())
	}
	m.editor.SetText("SELECT 'second'")
	if !strings.Contains(m.editorTitle(), "console-2") {
		t.Errorf("the title does not name the console: %q", m.editorTitle())
	}

	key(t, m, "alt+c")
	if m.consoleName != "console" || m.editor.Text() != "SELECT 'first'" {
		t.Fatalf("after ⌥C: console %q, editor %q", m.consoleName, m.editor.Text())
	}
	key(t, m, "alt+c")
	if m.consoleName != "console-2" || m.editor.Text() != "SELECT 'second'" {
		t.Fatalf("⌥C did not wrap round: console %q, editor %q", m.consoleName, m.editor.Text())
	}

	drive(t, m, nil, m.setActive("b"))
	drive(t, m, nil, m.setActive("a"))
	if m.consoleName != "console-2" || m.editor.Text() != "SELECT 'second'" {
		t.Errorf("back on a: console %q, editor %q — want console-2, the one left open", m.consoleName, m.editor.Text())
	}
}

// A console the TUI only had open is not written back over what another
// writer (dbc web) saved to it meanwhile.
func TestConsoleUnchangedIsNotWrittenBack(t *testing.T) {
	m, _ := consoleModel(t)
	drive(t, m, nil, m.setActive("a"))
	m.editor.SetText("SELECT 'tui'")
	if err := m.saveConsole(); err != nil {
		t.Fatal(err)
	}
	// the browser saves the same console
	if err := userdata.SaveConsole(m.console, "SELECT 'web'"); err != nil {
		t.Fatal(err)
	}
	if err := m.saveConsole(); err != nil { // quitting, say
		t.Fatal(err)
	}
	if text, _ := userdata.LoadConsole(m.console); text != "SELECT 'web'" {
		t.Errorf("the TUI wrote its unchanged copy over the web's: %q", text)
	}
}
