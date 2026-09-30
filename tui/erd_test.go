package tui

import (
	"image/png"
	"os"
	"strings"
	"testing"

	"github.com/rohanthewiz/dbc/erd"
)

// e on a table saves the diagram around it as a PNG and opens it; the
// table menu copies the whole connection's Mermaid source.
func TestTablesSidebarDiagram(t *testing.T) {
	m := newTestModel(t)
	dir := t.TempDir()
	prev := erdDir
	erdDir = func() string { return dir }
	t.Cleanup(func() { erdDir = prev })

	frame(m)
	click(t, m, m.lay.tables.X+3, m.lay.tables.Y+1) // selects cats, focuses the list
	before := m.editor.Text()
	key(t, m, "e")
	if len(openLog) != 1 || !strings.HasPrefix(openLog[0], "file://"+dir) || !strings.HasSuffix(openLog[0], ".png") {
		t.Fatalf("opened %v\n%s", openLog, logText(m))
	}
	f, err := os.Open(strings.TrimPrefix(openLog[0], "file://"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = png.Decode(f)
	f.Close()
	if err != nil {
		t.Errorf("the saved diagram is not a PNG: %v", err)
	}
	if !strings.Contains(logText(m), "saved the diagram (dbc · demo") {
		t.Errorf("log:\n%s", logText(m))
	}
	if m.editor.Text() != before || m.ws.Busy() {
		t.Error("a diagram must not touch the editor or take the run slot")
	}

	// the whole connection's source, through the menu's action
	clipLog = nil
	drive(t, m, nil, m.diagram(erd.Selection{}, true))
	if len(clipLog) != 1 || !strings.HasPrefix(clipLog[0].Text, "erDiagram\n") || !strings.Contains(clipLog[0].Text, "cats {") {
		t.Errorf("clipboard = %+v", clipLog)
	}
}
