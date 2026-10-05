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

// A console swapped out and back keeps its caret, scroll and undo history
// (N-103) — and a file changed meanwhile comes back as one undoable edit
// over the text that was left, not as a fresh buffer with no history.
func TestConsoleKeepsCaretAndUndoAcrossSwaps(t *testing.T) {
	m, _ := consoleModel(t)
	drive(t, m, nil, m.setActive("a"))
	m.editor.Insert("SELECT 1;\nSELECT 2;\nSELECT 3;")
	m.editor.move(pos{1, 4}, false)
	m.editor.top = 1
	left := m.console

	drive(t, m, nil, m.setActive("b"))
	if m.editor.cur != (pos{}) {
		t.Fatalf("b's console opened with the caret at %v", m.editor.cur)
	}
	drive(t, m, nil, m.setActive("a"))
	if m.editor.cur != (pos{1, 4}) || m.editor.top != 1 {
		t.Errorf("back on a: caret %v top %d, want {1 4} and 1", m.editor.cur, m.editor.top)
	}
	if !m.editor.Undo() || m.editor.Text() != "" {
		t.Errorf("back on a, undo gave %q — the history did not survive the swap", m.editor.Text())
	}
	m.editor.Redo()

	// another writer saves over a's console while b is open
	drive(t, m, nil, m.setActive("b"))
	if err := userdata.SaveConsole(left, "SELECT 'web'"); err != nil {
		t.Fatal(err)
	}
	drive(t, m, nil, m.setActive("a"))
	if got := m.editor.Text(); got != "SELECT 'web'" {
		t.Fatalf("back on a after a save elsewhere, editor = %q", got)
	}
	if m.editor.cur != (pos{0, 4}) {
		t.Errorf("caret %v, want {0 4}: the old caret clamped to the new text", m.editor.cur)
	}
	if !m.editor.Undo() || m.editor.Text() != "SELECT 1;\nSELECT 2;\nSELECT 3;" {
		t.Errorf("undo after the outside save gave %q, want the text that was left", m.editor.Text())
	}
}

// pickMenu runs the open menu's row whose label holds label.
func pickMenu(t *testing.T, m *Model, label string) {
	t.Helper()
	if m.menu == nil {
		t.Fatalf("no menu open to pick %q from", label)
	}
	for i, it := range m.menu.items {
		if !it.head && strings.Contains(it.label, label) {
			drive(t, m, nil, m.menuPick(i))
			return
		}
	}
	t.Fatalf("no menu row %q", label)
}

// Renaming the editor's console (N-102) moves its file, keeps the editor's
// text and undo, and refuses a bad or taken name in the prompt, which stays
// open to fix it.
func TestRenameConsole(t *testing.T) {
	m, _ := consoleModel(t)
	drive(t, m, nil, m.setActive("a"))
	m.editor.SetText("SELECT 'first'")
	key(t, m, "alt+n")
	m.editor.SetText("SELECT 'second'")
	old := m.console

	m.openEditorMenu(m.lay.editor.X+2, m.lay.editor.Y+1)
	pickMenu(t, m, "Rename console")
	p, ok := m.modal.(*promptModal)
	if !ok {
		t.Fatalf("modal = %T, want the rename prompt", m.modal)
	}
	if p.field.Text() != "console-2" {
		t.Errorf("prompt prefilled %q", p.field.Text())
	}

	typeText(t, m, "console") // replaces the selected name: taken
	key(t, m, "enter")
	if m.modal == nil || !strings.Contains(p.errMsg, "already has a console named console") {
		t.Fatalf("a taken name: modal %T, error %q", m.modal, p.errMsg)
	}
	p.field.SetText("../escape")
	key(t, m, "enter")
	if m.modal == nil || !strings.Contains(p.errMsg, "up to 64") {
		t.Fatalf("a bad name: modal %T, error %q", m.modal, p.errMsg)
	}
	p.field.SetText("tuning")
	key(t, m, "enter")
	if m.modal != nil {
		t.Fatalf("a good name left the prompt open: %q", p.errMsg)
	}

	if m.consoleName != "tuning" || m.editor.Text() != "SELECT 'second'" {
		t.Fatalf("after rename: console %q, editor %q", m.consoleName, m.editor.Text())
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("the old file is still there: %v", err)
	}
	if text, _ := userdata.LoadConsole(m.console); text != "SELECT 'second'" {
		t.Errorf("the renamed file holds %q — the latest text should go with it", text)
	}
	// a switch away and back reopens it under its new name
	drive(t, m, nil, m.setActive("b"))
	drive(t, m, nil, m.setActive("a"))
	if m.consoleName != "tuning" || m.editor.Text() != "SELECT 'second'" {
		t.Errorf("back on a: console %q, editor %q", m.consoleName, m.editor.Text())
	}
}

// Deleting the editor's console (N-102) asks first, removes the file, and
// moves the editor to the database's next console — or, the last one gone,
// a fresh empty default with no file — without writing the deleted one back.
func TestDeleteConsole(t *testing.T) {
	m, _ := consoleModel(t)
	drive(t, m, nil, m.setActive("a"))
	m.editor.SetText("SELECT 'first'")
	key(t, m, "alt+n")
	m.editor.SetText("SELECT 'second'")
	doomed := m.console

	m.openEditorMenu(m.lay.editor.X+2, m.lay.editor.Y+1)
	pickMenu(t, m, "Delete console")
	pickMenu(t, m, "Keep it")
	if m.consoleName != "console-2" {
		t.Fatalf("Keep it deleted it anyway: console %q", m.consoleName)
	}

	m.openEditorMenu(m.lay.editor.X+2, m.lay.editor.Y+1)
	pickMenu(t, m, "Delete console")
	pickMenu(t, m, "✕ Delete")
	if m.consoleName != "console" || m.editor.Text() != "SELECT 'first'" {
		t.Fatalf("after delete: console %q, editor %q", m.consoleName, m.editor.Text())
	}
	if _, err := os.Stat(doomed); !os.IsNotExist(err) {
		t.Errorf("the deleted console's file is back or still there: %v", err)
	}

	// the last one: the editor lands on a fresh default console, no file
	first := m.console
	m.confirmDeleteConsole()
	pickMenu(t, m, "✕ Delete")
	if m.consoleName != userdata.DefaultConsole || m.editor.Text() != "" {
		t.Fatalf("after the last delete: console %q, editor %q", m.consoleName, m.editor.Text())
	}
	if err := m.saveConsole(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(first); !os.IsNotExist(err) {
		t.Errorf("an empty fresh console got a file: %v", err)
	}
	// and a switch away does not resurrect the deleted console-2 either
	drive(t, m, nil, m.setActive("b"))
	if _, err := os.Stat(doomed); !os.IsNotExist(err) {
		t.Errorf("switching away wrote the deleted console back: %v", err)
	}
}

// History is scoped to the database (N-101): Ctrl+P opens on what ran on
// the editor's database, Tab widens it to every database and back, and a
// database with no history yet opens on all of them.
func TestHistoryScopedToTheDatabase(t *testing.T) {
	m, _ := consoleModel(t)
	drive(t, m, nil, m.setActive("a"))
	m.editor.SetText("SELECT 'on a'")
	key(t, m, "ctrl+r")
	drive(t, m, nil, m.setActive("b"))
	m.editor.SetText("SELECT 'on b'")
	key(t, m, "ctrl+r")

	drive(t, m, nil, m.setActive("a-too")) // the same database as a
	key(t, m, "ctrl+p")
	h, ok := m.modal.(*historyModal)
	if !ok {
		t.Fatalf("modal = %T", m.modal)
	}
	if !h.scoped || len(h.shown) != 1 || h.shown[0].SQL != "SELECT 'on a'" {
		t.Fatalf("scoped history = %v (scoped %v), want only a's statement", h.shown, h.scoped)
	}
	if !strings.Contains(h.title(), "a.db") {
		t.Errorf("the title does not name the database: %q", h.title())
	}
	// the scope chip is drawn beside the filter, and a click on it flips
	// the scope just as Tab does
	x, y := findText(t, frame(m), "○ all")
	click(t, m, x, y)
	if h.scoped {
		t.Fatal("a click on the scope chip did not widen the history")
	}
	key(t, m, "tab")
	key(t, m, "tab")
	if h.scoped || len(h.shown) < 2 {
		t.Fatalf("after Tab: scoped %v, %d entries — want every database", h.scoped, len(h.shown))
	}
	typeText(t, m, "on b")
	if len(h.shown) != 1 {
		t.Errorf("the filter over all databases kept %d", len(h.shown))
	}
	key(t, m, "tab")
	if !h.scoped || len(h.shown) != 0 {
		t.Errorf("back on a, 'on b' should match nothing: %v", h.shown)
	}
	key(t, m, "esc")

	// the demo database has run nothing: its picker opens on everything
	drive(t, m, nil, m.setActive(config.DemoSQLite))
	key(t, m, "ctrl+p")
	if h := m.modal.(*historyModal); h.scoped || len(h.shown) < 2 {
		t.Errorf("a database with no history opened scoped %v with %d entries", h.scoped, len(h.shown))
	}
}
