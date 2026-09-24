package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

// These tests drive the whole Model through the harness — keys, clicks,
// drags — against a seeded SQLite demo, and assert on state and on the frame.

func TestCtrlRRunsAndShowsTheResult(t *testing.T) {
	m := newTestModel(t)
	key(t, m, "ctrl+r")
	if m.lastRes == nil || len(m.lastRes.Rows) != 8 {
		t.Fatalf("result = %+v", m.lastRes)
	}
	c := frame(m)
	findText(t, c, "Whiskers")
	findText(t, c, "Results · 8 rows")
	if !strings.Contains(c.Line(c.H-1), "8 rows in") {
		t.Errorf("status bar = %q", c.Line(c.H-1))
	}
}

func TestRunButtonRunsToo(t *testing.T) {
	m := newTestModel(t)
	x, y := findText(t, frame(m), "▶ Run")
	click(t, m, x, y)
	if m.lastRes == nil {
		t.Fatal("clicking ▶ Run should run the statement")
	}
}

// The statement under the caret runs, and the gutter marks it.
func TestStatementUnderCaret(t *testing.T) {
	m := newTestModel(t)
	m.editor.SetText("SELECT 1 AS a;\nSELECT 2 AS b;")
	m.editor.move(pos{1, 3}, false)
	c := frame(m)
	if !strings.Contains(c.Line(3), "▎") || strings.Contains(c.Line(2), "▎") {
		t.Errorf("the gutter should mark line 2 only:\n%s\n%s", c.Line(2), c.Line(3))
	}
	findText(t, c, "Query · ^R runs statement 2/2")
	key(t, m, "ctrl+r")
	if m.lastRes.Columns[0] != "b" {
		t.Errorf("ran %v, want the second statement", m.lastRes.Columns)
	}
}

// BEGIN, work, and COMMIT across three runs land on one pinned connection.
func TestSessionCarriesAcrossRuns(t *testing.T) {
	m := newTestModel(t)
	for _, sql := range []string{"CREATE TEMP TABLE scratch (x INT)", "INSERT INTO scratch VALUES (7)", "SELECT x FROM scratch"} {
		m.editor.SetText(sql)
		key(t, m, "ctrl+r")
		if m.lastErr != "" {
			t.Fatalf("%s: %s", sql, m.lastErr)
		}
	}
	if m.lastRes.Rows[0][0] != "7" {
		t.Errorf("temp table did not survive between runs: %v", m.lastRes.Rows)
	}
}

func TestErrorsAreLoggedAndOfferedToTheAssistant(t *testing.T) {
	m := newTestModel(t)
	m.editor.SetText("SELEC nonsense")
	key(t, m, "ctrl+r")
	if m.lastErr == "" || !strings.Contains(logText(m), "syntax error") {
		t.Errorf("lastErr = %q\nlog: %s", m.lastErr, logText(m))
	}
	if !strings.Contains(frame(m).Line(39), "ask the assistant why") {
		t.Errorf("status should point at the assistant: %q", frame(m).Line(39))
	}
}

// Click a cell, drag to another: a range selection, visible in the frame.
func TestClickAndDragSelectsARange(t *testing.T) {
	m := newTestModel(t)
	key(t, m, "ctrl+r")
	c := frame(m)
	x0, y0 := findText(t, c, "Oliver")
	x1, y1 := findText(t, c, "Siamese")
	drive(t, m, tea.MouseClickMsg{X: x0, Y: y0, Button: tea.MouseLeft})
	drive(t, m, tea.MouseMotionMsg{X: x1, Y: y1 + 1, Button: tea.MouseLeft})
	drive(t, m, tea.MouseReleaseMsg{X: x1, Y: y1 + 1, Button: tea.MouseLeft})
	if m.focus != focusGrid {
		t.Error("clicking the grid should give it the keyboard")
	}
	r0, c0, r1, c1 := m.grid.bounds()
	if r0 != 0 || c0 != 1 || r1 != 2 || c1 != 2 {
		t.Errorf("selection = rows %d–%d cols %d–%d", r0, r1, c0, c1)
	}
	findText(t, frame(m), "3×2 selected")
}

// Right-click in the grid → "Markdown table" copies as one. With a single
// cell under the cursor there is no range, so the menu's formats apply to
// the whole result.
func TestRightClickCopyAsMarkdown(t *testing.T) {
	m := newTestModel(t)
	key(t, m, "ctrl+r")
	c := frame(m)
	x, y := findText(t, c, "Oliver")
	rightClick(t, m, x, y)
	if m.menu == nil {
		t.Fatal("right-click should open a menu")
	}
	mx, my := findText(t, frame(m), "Markdown table")
	click(t, m, mx, my)
	if m.menu != nil {
		t.Error("picking a row closes the menu")
	}
	got := lastClip(t)
	if !strings.HasPrefix(got.Text, "| id | name |") || strings.Count(got.Text, "\n") != 10 {
		t.Errorf("clipboard = %q", got.Text)
	}
	if !strings.Contains(logText(m), "copied the result (8 rows) as Markdown") {
		t.Errorf("log: %s", logText(m))
	}
}

// The ⧉ Copy dropdown's HTML row puts a real table on the clipboard.
func TestCopyWholeResultAsHTMLTable(t *testing.T) {
	m := newTestModel(t)
	key(t, m, "ctrl+r")
	x, y := findText(t, frame(m), "⧉ Copy")
	click(t, m, x, y)
	mx, my := findText(t, frame(m), "Table for Teams")
	click(t, m, mx, my)
	got := lastClip(t)
	if !strings.Contains(got.HTML, "<table style=") || strings.Count(got.HTML, "<tr>") != 9 {
		t.Errorf("HTML flavor = %.200s", got.HTML)
	}
	if !strings.Contains(logText(m), "paste into Teams") {
		t.Errorf("the log should say it landed as a table: %s", logText(m))
	}
}

func TestCopyKeys(t *testing.T) {
	m := newTestModel(t)
	key(t, m, "ctrl+r")
	m.focus = focusGrid
	key(t, m, "right")
	key(t, m, "y")
	if got := lastClip(t).Text; got != "Oliver" {
		t.Errorf("y copied %q", got)
	}
	key(t, m, "shift+Y")
	if got := lastClip(t).Text; got != "4\tOliver\tTabby\t1\t0" {
		t.Errorf("Y copied %q", got)
	}
}

func TestHeaderClickSorts(t *testing.T) {
	m := newTestModel(t)
	key(t, m, "ctrl+r")
	x, y := findText(t, frame(m), "name")
	y = m.grid.head.Y
	click(t, m, x+1, y)
	if v, _, _ := m.grid.value(0, 1); v != "Bella" {
		t.Errorf("first after sorting by name = %q", v)
	}
	findText(t, frame(m), "sorted by name asc")
}

// Double-click a cell → the inspector; y copies; Esc closes.
func TestDoubleClickInspects(t *testing.T) {
	m := newTestModel(t)
	key(t, m, "ctrl+r")
	x, y := findText(t, frame(m), "Oliver")
	click(t, m, x, y)
	click(t, m, x, y)
	if _, ok := m.modal.(*inspectModal); !ok {
		t.Fatalf("modal = %T", m.modal)
	}
	findText(t, frame(m), "name · row 1")
	key(t, m, "y")
	if lastClip(t).Text != "Oliver" {
		t.Errorf("inspector copy = %q", lastClip(t).Text)
	}
	key(t, m, "esc")
	if m.modal != nil {
		t.Error("Esc closes the inspector")
	}
}

func TestInspectorFormatsJSON(t *testing.T) {
	m := newTestModel(t)
	m.editor.SetText(`SELECT '{"a":1,"b":[2,3]}' AS doc`)
	key(t, m, "ctrl+r")
	m.focus = focusGrid
	key(t, m, "enter")
	c := frame(m)
	findText(t, c, "JSON, formatted")
	findText(t, c, `"a": 1,`)
}

func TestWheelScrollsThePaneUnderThePointer(t *testing.T) {
	m := newTestModel(t)
	m.editor.SetText("SELECT value FROM json_each('[" + strings.Repeat("1,", 99) + "1]')")
	key(t, m, "ctrl+r")
	frame(m)
	r := m.lay.results
	drive(t, m, tea.MouseWheelMsg{X: r.X + 10, Y: r.Y + 5, Button: tea.MouseWheelDown})
	if m.grid.top != 3 {
		t.Errorf("grid top = %d after one notch", m.grid.top)
	}
	if m.focus != focusEditor {
		t.Error("the wheel scrolls without taking focus")
	}
}

// Dragging the border between editor and results resizes both.
func TestDragSplitterResizes(t *testing.T) {
	m := newTestModel(t)
	frame(m)
	before := m.lay.editor.H
	sp := m.lay.splitEd
	drive(t, m, tea.MouseClickMsg{X: sp.X + 5, Y: sp.Y, Button: tea.MouseLeft})
	drive(t, m, tea.MouseMotionMsg{X: sp.X + 5, Y: sp.Y + 6, Button: tea.MouseLeft})
	drive(t, m, tea.MouseReleaseMsg{X: sp.X + 5, Y: sp.Y + 6, Button: tea.MouseLeft})
	frame(m)
	if m.lay.editor.H != before+6 {
		t.Errorf("editor height %d → %d, want +6", before, m.lay.editor.H)
	}

	side := m.lay.splitSide
	drive(t, m, tea.MouseClickMsg{X: side.X, Y: side.Y + 3, Button: tea.MouseLeft})
	drive(t, m, tea.MouseMotionMsg{X: side.X + 8, Y: side.Y + 3, Button: tea.MouseLeft})
	drive(t, m, tea.MouseReleaseMsg{X: side.X + 8, Y: side.Y + 3, Button: tea.MouseLeft})
	frame(m)
	if m.lay.conns.W != side.X+9 {
		t.Errorf("sidebar width = %d, want %d", m.lay.conns.W, side.X+9)
	}
}

// The tables sidebar fills on connect; a double-click previews the table.
func TestTablesSidebarPreview(t *testing.T) {
	m := newTestModel(t)
	frame(m)
	x, y := m.lay.tables.X+3, m.lay.tables.Y+1
	if !strings.Contains(frame(m).Line(y), "│ cats") {
		t.Fatalf("tables list row = %q", frame(m).Line(y))
	}
	click(t, m, x, y)
	click(t, m, x, y)
	if m.lastRes == nil || !strings.Contains(m.lastRes.Query, "FROM cats LIMIT 100") {
		t.Fatalf("preview did not run: %+v", m.lastRes)
	}
	if m.editor.Text() == m.lastRes.Query {
		t.Error("a preview must not overwrite the editor")
	}
}

// History: Ctrl+P lists past statements; Enter inserts, never runs.
func TestHistoryRecallInserts(t *testing.T) {
	m := newTestModel(t)
	key(t, m, "ctrl+r")
	m.editor.SetText("")
	key(t, m, "ctrl+p")
	if _, ok := m.modal.(*historyModal); !ok {
		t.Fatalf("modal = %T", m.modal)
	}
	typeText(t, m, "breed")
	key(t, m, "enter")
	if !strings.Contains(m.editor.Text(), "SELECT id, name, breed") {
		t.Errorf("editor = %q", m.editor.Text())
	}
	if m.modal != nil {
		t.Error("Enter should close history")
	}
}

func TestExportToFile(t *testing.T) {
	m := newTestModel(t)
	key(t, m, "ctrl+r")
	key(t, m, "ctrl+e")
	path := filepath.Join(t.TempDir(), "out.csv")
	typeText(t, m, path) // typing on the format row starts the path
	key(t, m, "enter")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("export: %v\nlog: %s", err, logText(m))
	}
	if !strings.HasPrefix(string(b), "id,name,breed,age,adopted\n") {
		t.Errorf("file = %q", b)
	}
}

// Keyboard follows the mouse, and Tab cycles the panes.
func TestFocusFollowsClicksAndTab(t *testing.T) {
	m := newTestModel(t)
	frame(m)
	l := m.lay.logR
	click(t, m, l.X+5, l.Y+2)
	if m.focus != focusLog {
		t.Errorf("focus = %d after clicking the log", m.focus)
	}
	key(t, m, "tab")
	if m.focus != focusChat && m.focus != focusEditor {
		t.Errorf("Tab from the log with the assistant closed should wrap to the editor, got %d", m.focus)
	}
}

// Ctrl+C cancels a run when busy and quits when idle.
func TestCtrlCQuitsWhenIdle(t *testing.T) {
	m := newTestModel(t)
	_, cmd := m.Update(keyMsg("ctrl+c"))
	if cmd == nil || !m.quit {
		t.Error("idle Ctrl+C should quit")
	}
}

// A menu row that cannot run says why instead of silently doing nothing.
func TestDisabledMenuRowsExplain(t *testing.T) {
	m := newTestModel(t)
	frame(m)
	r := m.lay.results
	rightClick(t, m, r.X+10, r.Y+5)
	x, y := findText(t, frame(m), "Markdown table")
	click(t, m, x, y)
	if !strings.Contains(logText(m), "nothing to copy yet") {
		t.Errorf("log: %s", logText(m))
	}
	if len(clipLog) != 0 {
		t.Error("nothing should have been copied")
	}
}

// With a range selected, the menu's first formats copy just the range.
func TestRightClickCopyRangeAsCSV(t *testing.T) {
	m := newTestModel(t)
	key(t, m, "ctrl+r")
	m.focus = focusGrid
	key(t, m, "right")
	key(t, m, "shift+down")
	key(t, m, "shift+right")
	x, y := m.grid.cursorScreen()
	rightClick(t, m, x, y)
	findText(t, frame(m), "copy selection as")
	mx, my := findText(t, frame(m), "CSV")
	click(t, m, mx, my)
	if got := lastClip(t).Text; got != "name,breed\nOliver,Tabby\nLuna,Siamese\n" {
		t.Errorf("clipboard = %q", got)
	}
}

// A script's s.Show results reach the grid and its s.Print lines the log,
// both mid-run through m.send.
func TestScriptRunsFromThePicker(t *testing.T) {
	m := newTestModel(t)
	m.cfg.ScriptsDir = "../scripts"
	key(t, m, "ctrl+o")
	sm, ok := m.modal.(*scriptsModal)
	if !ok {
		t.Fatalf("modal = %T; log: %s", m.modal, logText(m))
	}
	for i, f := range sm.files {
		if strings.HasSuffix(f, "loop_params.go") {
			sm.lst.cur = i
		}
	}
	key(t, m, "enter")
	if m.busy {
		t.Fatal("the script should have finished")
	}
	if !strings.Contains(logText(m), "script loop_params.go completed") {
		t.Errorf("log: %s", logText(m))
	}
	if m.lastRes == nil {
		t.Error("the script's shown results should reach the grid")
	}
}

// Drag a header border to widen a column; double-click it to fit.
func TestDragHeaderBorderResizes(t *testing.T) {
	m := newTestModel(t)
	key(t, m, "ctrl+r")
	c := frame(m)
	_, hy := findText(t, c, "│ name")
	line := c.Line(hy)
	bx := cellIndex(line, "│ breed") // the border right of name
	before := m.grid.colWidth(1)

	drive(t, m, tea.MouseMotionMsg{X: bx, Y: hy})
	if m.grid.hoverB != 1 {
		t.Errorf("hovering the border should highlight it, hoverB = %d", m.grid.hoverB)
	}
	drive(t, m, tea.MouseClickMsg{X: bx, Y: hy, Button: tea.MouseLeft})
	drive(t, m, tea.MouseMotionMsg{X: bx + 6, Y: hy, Button: tea.MouseLeft})
	drive(t, m, tea.MouseReleaseMsg{X: bx + 6, Y: hy, Button: tea.MouseLeft})
	if got := m.grid.colWidth(1); got != before+6 {
		t.Errorf("width = %d, want %d", got, before+6)
	}
	if m.grid.sortCol != -1 {
		t.Error("a border press must not sort")
	}
	// still hovered (no motion since the drop), so drawn heavy
	if got := cellIndex(frame(m).Line(hy), "┃ breed"); got != bx+6 {
		t.Errorf("the border should be drawn where it was dropped: %d, want %d", got, bx+6)
	}

	// double-click fits it back to its content
	now = func() time.Time { return time.Unix(100, 0) }
	t.Cleanup(func() { now = time.Now })
	click(t, m, bx+6, hy)
	click(t, m, bx+6, hy)
	if got := m.grid.colWidth(1); got != m.grid.contentWidth(1) {
		t.Errorf("double-click should fit: %d, want %d", got, m.grid.contentWidth(1))
	}
}

// Right-click a header → Hide column; the copy leaves it out, and the menu
// then offers it back by name.
func TestRightClickHeaderHidesColumn(t *testing.T) {
	m := newTestModel(t)
	key(t, m, "ctrl+r")
	x, y := findText(t, frame(m), "│ breed") // the header, not the query text
	rightClick(t, m, x+2, y)
	mx, my := findText(t, frame(m), "Hide column breed")
	click(t, m, mx, my)
	if m.grid.Cols() != 4 || strings.Contains(frame(m).Line(y), "breed") {
		t.Fatalf("breed should be hidden: cols %v", m.grid.cols)
	}
	if !strings.Contains(logText(m), "hid breed") {
		t.Errorf("log: %s", logText(m))
	}

	key(t, m, "Y") // the cursor's row, as text
	if got := lastClip(t).Text; strings.Contains(got, "Tabby") {
		t.Errorf("a row copy should leave the hidden column out: %q", got)
	}

	x, y = findText(t, frame(m), "Oliver")
	rightClick(t, m, x, y)
	mx, my = findText(t, frame(m), "Show breed")
	click(t, m, mx, my)
	if m.grid.Cols() != 5 {
		t.Errorf("Show breed should bring it back: %v", m.grid.cols)
	}
}

// - hides the cursor's column, + shows everything; the last column stays.
func TestHideKeys(t *testing.T) {
	m := newTestModel(t)
	key(t, m, "ctrl+r")
	m.focus = focusGrid
	for range 4 {
		typeText(t, m, "-")
	}
	if m.grid.Cols() != 1 {
		t.Fatalf("cols = %v", m.grid.cols)
	}
	typeText(t, m, "-")
	if m.grid.Cols() != 1 || !strings.Contains(logText(m), "can't hide every column") {
		t.Errorf("the last column should stay, and say why: %s", logText(m))
	}
	typeText(t, m, "+")
	if m.grid.Cols() != 5 || !strings.Contains(logText(m), "showing 4 hidden columns again") {
		t.Errorf("cols %v log %s", m.grid.cols, logText(m))
	}
}
