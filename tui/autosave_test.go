package tui

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rohanthewiz/dbc/userdata"
)

// consoleText is what console file f holds.
func consoleText(t *testing.T, f string) string {
	t.Helper()
	text, _ := userdata.LoadConsole(f)
	return text
}

// TestAutosaveOnPause is N-254: typing is saved once it pauses, not only
// on ^Q, a console swap or a tab opened or closed. (The harness's pause is
// a millisecond, and it runs the timer as it comes.)
func TestAutosaveOnPause(t *testing.T) {
	m, _ := consoleModel(t)
	m.focus = focusEditor
	m.editor.SetText("")
	typeText(t, m, "SELECT 'typed'")
	if got := consoleText(t, m.console); got != "SELECT 'typed'" {
		t.Errorf("after typing, the console file holds %q", got)
	}
	if m.autosave.armed || !m.autosave.first.IsZero() {
		t.Errorf("the timer is still armed after the save: %+v", m.autosave)
	}
}

// TestAutosaveWaitsForAPause: the timer firing while the typing goes on
// waits out the rest of a pause rather than saving under the fingers —
// but steady typing is saved once autosaveMax has passed since the first
// unsaved edit.
func TestAutosaveWaitsForAPause(t *testing.T) {
	m, _ := consoleModel(t)
	defer func(idle, most time.Duration) { autosaveIdle, autosaveMax = idle, most }(autosaveIdle, autosaveMax)
	autosaveIdle, autosaveMax = time.Hour, 2*time.Hour
	m.editor.SetText("SELECT 'still typing'")
	now := time.Now()
	m.autosave.first, m.autosave.last, m.autosave.armed = now, now, true

	if cmd := m.autosaveDue(); cmd == nil || !m.autosave.armed {
		t.Fatal("fired mid-typing: no re-arm")
	}
	if got := consoleText(t, m.console); got == "SELECT 'still typing'" {
		t.Fatal("saved mid-typing")
	}
	// typing for longer than autosaveMax: saved even without a pause
	m.autosave.first = now.Add(-3 * time.Hour)
	if cmd := m.autosaveDue(); cmd != nil {
		t.Error("re-armed past autosaveMax")
	}
	if got := consoleText(t, m.console); got != "SELECT 'still typing'" {
		t.Errorf("past autosaveMax the console file holds %q", got)
	}
}

// TestAutosaveSavesAParkedTab: a tab left with unsaved text is saved from
// its own fields when the timer fires, and the tab on screen keeps its
// completion popup — saving through forEachTab would load each tab and
// close it.
func TestAutosaveSavesAParkedTab(t *testing.T) {
	m, _ := consoleModel(t)
	drive(t, m, nil, m.setActive("a"))
	m.editor.SetText("SELECT 'left behind'") // no Update, so no save yet
	left := m.console
	key(t, m, "alt+t")
	if m.curTab != 1 {
		t.Fatalf("no second tab: on %d", m.curTab)
	}
	m.focus = focusEditor
	m.editor.SetText("SELECT * FROM ")
	m.editor.cur = pos{0, len("SELECT * FROM ")}
	key(t, m, "ctrl+space")
	if m.compl == nil {
		t.Fatal("no popup to keep")
	}
	m.autosave.first = time.Now().Add(-time.Hour)
	m.autosaveDue()
	if got := consoleText(t, left); got != "SELECT 'left behind'" {
		t.Errorf("the parked tab's console holds %q", got)
	}
	if m.compl == nil {
		t.Error("the autosave closed the popup on screen")
	}
}

// TestHangupSavesThenQuits: SIGHUP (the terminal gone) saves every tab and
// the layout, then quits as ^Q would.
func TestHangupSavesThenQuits(t *testing.T) {
	m, _ := consoleModel(t)
	m.layoutFile = filepath.Join(t.TempDir(), "layout.json")
	m.editor.SetText("SELECT 'before the hangup'")
	// Update, not drive: the tea.Quit it answers with would loop the
	// harness (as app_test's quit tests do)
	if _, cmd := m.Update(hangupMsg{}); cmd == nil {
		t.Error("SIGHUP returned no command (tea.Quit)")
	}
	if got := consoleText(t, m.console); got != "SELECT 'before the hangup'" {
		t.Errorf("after SIGHUP the console file holds %q", got)
	}
	if _, err := os.Stat(m.layoutFile); err != nil {
		t.Errorf("no layout saved: %v", err)
	}
	if !m.quit {
		t.Error("SIGHUP did not quit")
	}
}

// TestNoAutosaveWithoutPersistence: under NoPersist (the tests' default)
// no timer is armed at all.
func TestNoAutosaveWithoutPersistence(t *testing.T) {
	m := newTestModel(t)
	m.focus = focusEditor
	typeText(t, m, "x")
	if m.autosave.armed || !m.autosave.first.IsZero() {
		t.Errorf("autosave ran with persistence off: %+v", m.autosave)
	}
}
