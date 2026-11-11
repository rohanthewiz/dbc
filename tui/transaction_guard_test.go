package tui

import (
	"strings"
	"testing"

	"github.com/rohanthewiz/dbc/config"
)

// openTx leaves the tab's session in a transaction that has deleted rows.
func openTx(t *testing.T, m *Model) {
	t.Helper()
	m.focus = focusEditor
	m.editor.SetText("BEGIN")
	key(t, m, "ctrl+r")
	m.editor.SetText("DELETE FROM cats")
	key(t, m, "ctrl+r")
	if _, stateful := m.ws.Session(); !stateful {
		t.Fatal("the session holds no state after BEGIN")
	}
}

// TestQuitAsksAboutAnOpenTransaction is N-257: Ctrl+C on an idle editor —
// psql's way to clear a line — quit at once and rolled back every tab's
// transaction. Now it asks; Enter keeps working; ^Q (or ^C) again quits.
func TestQuitAsksAboutAnOpenTransaction(t *testing.T) {
	m := newTestModel(t)
	openTx(t, m)

	key(t, m, "ctrl+c")
	if m.quit || m.menu == nil || !strings.Contains(m.menu.items[0].label, "may hold a transaction — quitting rolls it back") {
		t.Fatalf("idle ^C over a transaction: quit=%v menu=%+v", m.quit, m.menu)
	}
	key(t, m, "enter") // the cursor opens on Keep working
	if m.quit || m.menu != nil {
		t.Fatalf("Keep working: quit=%v menu=%v", m.quit, m.menu)
	}

	// ^Q asks too, and a second ^Q means it (Update, not drive: the
	// tea.Quit it answers with would loop the harness)
	m.Update(keyMsg("ctrl+q"))
	if m.quit || m.menu == nil {
		t.Fatalf("^Q over a transaction: quit=%v menu=%v", m.quit, m.menu)
	}
	if _, cmd := m.Update(keyMsg("ctrl+q")); cmd == nil || !m.quit {
		t.Error("^Q again did not quit")
	}
}

// TestQuitWithoutATransactionAsksNothing: a session that holds nothing
// quits at once, as before.
func TestQuitWithoutATransactionAsksNothing(t *testing.T) {
	m := newTestModel(t)
	m.editor.SetText("SELECT 1")
	key(t, m, "ctrl+r")
	if _, cmd := m.Update(keyMsg("ctrl+q")); cmd == nil || !m.quit || m.menu != nil {
		t.Errorf("^Q with no transaction: quit=%v menu=%v", m.quit, m.menu)
	}
}

// TestSwitchAsksAboutAnOpenTransaction is N-257's other half: a click on
// another connection switched at once, the old session's transaction
// rolled back with only a note after. Now it asks, staying by default.
func TestSwitchAsksAboutAnOpenTransaction(t *testing.T) {
	m, _ := consoleModel(t)
	openTx(t, m)

	drive(t, m, nil, m.setActive("a"))
	if m.menu == nil || !strings.Contains(m.menu.items[0].label, "switching rolls it back") {
		t.Fatalf("no confirm before the switch: %+v", m.menu)
	}
	key(t, m, "enter") // Stay on the demo
	if m.ws.Active() != config.DemoSQLite {
		t.Fatalf("Stay switched to %q", m.ws.Active())
	}
	if _, stateful := m.ws.Session(); !stateful {
		t.Fatal("Stay lost the transaction")
	}

	drive(t, m, nil, m.setActive("a"))
	key(t, m, "down")
	key(t, m, "enter")
	if m.ws.Active() != "a" {
		t.Fatalf("Switch left the tab on %q", m.ws.Active())
	}
	// the switch released the session, which rolled the DELETE back
	if res, err := m.mgr.Run(config.DemoSQLite, "SELECT count(*) FROM cats"); err != nil || res.Rows[0][0] == "0" {
		t.Errorf("the DELETE survived the switch: %v %v", res, err)
	}
	// on a session that holds nothing, a switch asks nothing
	drive(t, m, nil, m.setActive(config.DemoSQLite))
	if m.menu != nil || m.ws.Active() != config.DemoSQLite {
		t.Errorf("a switch from a clean session asked: menu=%v active=%q", m.menu, m.ws.Active())
	}
}
