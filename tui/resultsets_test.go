package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// at is the switcher's current result (ScriptResults' at).
func setAt(m *Model) int {
	_, at, _ := m.ws.ScriptResults()
	return at
}

// A script that shows three results gets "Result 1 · 2 · 3" on the results
// title, on its last; [ and ] step through them, a click on a number jumps,
// and a run of the editor's own query takes the switcher away.
func TestScriptResultSwitcher(t *testing.T) {
	m := newTestModel(t)
	drive(t, m, nil, m.runScript("../scripts/loop_params.go"))
	if n, at, _ := m.ws.ScriptResults(); n != 3 || at != 2 {
		t.Fatalf("ScriptResults n=%d at=%d, want 3 and 2; log: %s", n, at, logText(m))
	}
	_, ty := findText(t, frame(m), "Result 1 · 2 · 3")
	if ty != m.lay.results.Y {
		t.Errorf("the switcher should sit on the results title (y %d), not y %d", m.lay.results.Y, ty)
	}
	// the current one is not a chip; the other two are
	if got := len(m.lay.setChips); got != 2 {
		t.Errorf("chips = %d, want 2", got)
	}

	// [ in the grid steps back one, and the grid follows the workspace
	m.focus = focusGrid
	last := m.ws.LastResult()
	key(t, m, "[")
	if setAt(m) != 1 {
		t.Fatalf("[ should show result 2, at = %d; log: %s", setAt(m), logText(m))
	}
	if r := m.ws.LastResult(); r == last || m.grid.Rows() != len(r.Rows) {
		t.Errorf("the grid should be on result 2 (%d rows), has %d", len(r.Rows), m.grid.Rows())
	}

	// a click on the 1
	x, y := findText(t, frame(m), "Result 1 ·")
	click(t, m, x+width("Result 1")-1, y)
	if setAt(m) != 0 {
		t.Fatalf("a click on 1 should show result 1, at = %d", setAt(m))
	}
	// [ at the first stops there rather than wrapping
	key(t, m, "[")
	if setAt(m) != 0 {
		t.Errorf("[ on the first should stay, at = %d", setAt(m))
	}
	key(t, m, "]")
	key(t, m, "]")
	key(t, m, "]")
	if setAt(m) != 2 {
		t.Errorf("] three times from the first should end on the last, at = %d", setAt(m))
	}

	// the editor's own run lands a result of its own: no switcher
	key(t, m, "ctrl+r")
	if strings.Contains(frame(m).Text(), "Result 1 · 2") || len(m.lay.setChips) != 0 {
		t.Error("a run of the query should take the switcher away")
	}
	key(t, m, "[")
	if !strings.Contains(logText(m), "no script results to step through") {
		t.Errorf("[ with nothing to step through should say so; log: %s", logText(m))
	}
}

// Twelve results in a narrow results pane: the numbered form does not fit,
// so the compact "Result ‹ 12/12 ›" steps instead, and the title is cut to
// leave it room.
func TestScriptResultSwitcherCompact(t *testing.T) {
	m := newTestModel(t)
	path := filepath.Join(t.TempDir(), "twelve.go")
	src := `package main

import "github.com/rohanthewiz/dbc/sdb"

func Run(s *sdb.S) error {
	for i := 1; i <= 12; i++ {
		r, err := s.Query("demo-sqlite", "SELECT ? AS n", i)
		if err != nil {
			return err
		}
		s.Show(r)
	}
	return nil
}
`
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	drive(t, m, tea.WindowSizeMsg{Width: 80, Height: 30})
	drive(t, m, nil, m.runScript(path))
	if n, _, _ := m.ws.ScriptResults(); n != 12 {
		t.Fatalf("n = %d, want 12; log: %s", n, logText(m))
	}
	c := frame(m)
	x, y := findText(t, c, "Result ‹ 12/12 ›")
	if y != m.lay.results.Y {
		t.Errorf("compact switcher at y %d, want the title row %d", y, m.lay.results.Y)
	}
	// it ends one cell left of the ╮, and the title before it was cut
	if end := x + width("Result ‹ 12/12 › "); end != m.lay.results.X+m.lay.results.W-1 {
		t.Errorf("the switcher should end at the corner: %d, want %d", end, m.lay.results.X+m.lay.results.W-1)
	}
	if line := c.Line(y); !strings.Contains(line, "…") {
		t.Errorf("the title should be cut to leave room: %q", line)
	}
	// only ‹ is live on the last
	if len(m.lay.setChips) != 1 {
		t.Fatalf("chips = %d, want 1 (‹)", len(m.lay.setChips))
	}
	click(t, m, x+width("Result ‹")-1, y)
	if setAt(m) != 10 {
		t.Fatalf("‹ should step back one, at = %d", setAt(m))
	}
	if r := m.ws.LastResult(); fmt.Sprint(r.Rows[0][0]) != "11" {
		t.Errorf("result 11 holds %v, want 11", r.Rows[0][0])
	}
	x, y = findText(t, frame(m), "Result ‹ 11/12 ›")
	click(t, m, x+width("Result ‹ 11/12 ›")-1, y)
	if setAt(m) != 11 {
		t.Errorf("› should step forward one, at = %d", setAt(m))
	}
}

// With a plan open, the switcher and the Results │ ◈ Plan tabs share the
// title: the tabs keep clear of the switcher, and a click on a number goes
// back to the grid.
func TestScriptResultSwitcherBesideThePlan(t *testing.T) {
	m := newTestModel(t)
	key(t, m, "ctrl+x") // a plan of the editor's query
	if m.planv.plan == nil {
		t.Fatalf("no plan; log: %s", logText(m))
	}
	drive(t, m, nil, m.runScript("../scripts/loop_params.go"))
	m.resTab = tabPlan
	c := frame(m)
	x, y := findText(t, c, "Result 1 · 2 · 3")
	px, _ := findText(t, c, "◈ Plan")
	if px >= x {
		t.Errorf("the Plan tab (x %d) should be left of the switcher (x %d)", px, x)
	}
	click(t, m, x+width("Result 1")-1, y)
	if setAt(m) != 0 || m.resTab != tabResults {
		t.Errorf("a click on 1 should show result 1 on the grid: at %d, tab %v", setAt(m), m.resTab)
	}
}
