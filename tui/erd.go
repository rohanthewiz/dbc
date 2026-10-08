package tui

import (
	"context"
	"errors"

	tea "charm.land/bubbletea/v2"
	"github.com/rohanthewiz/serr"

	"github.com/rohanthewiz/dbc/erd"
	"github.com/rohanthewiz/dbc/theme"
	"github.com/rohanthewiz/dbc/workspace"
)

// The entity-relationship diagram from the TUI. A terminal cannot show the
// picture, so it is saved as a PNG and opened in the system's viewer, as a
// plan's PDF or JPEG is (planFile); the Mermaid source goes to the
// clipboard, for a pull request or a wiki. Three ways in:
//
//	tables list, e                  the table under the cursor and its neighbours
//	table menu  Diagram around it   the same
//	            Diagram all tables  the whole connection
//	            Copy ERD as Mermaid the whole connection's source
//
// Reading the schema is catalog queries on the pool (workspace.Diagram), so
// it runs as a command off the event loop and reports back in an erdMsg;
// it never takes the run slot, so a diagram can be drawn while a query runs.

// erdDir is where diagrams are saved; a var so tests can point it at a
// temp dir.
var erdDir = erd.Dir

// erdMsg is a diagram drawn (or not) off the event loop.
type erdMsg struct {
	path    string // the saved picture, when a picture was asked for
	mermaid string // the source, when Mermaid was asked for
	title   string
	err     error
}

// tableDiagram diagrams the table under the tables list's cursor with its
// neighbours one foreign-key hop out: what it references and what
// references it, which is what "how does this table fit in" asks.
func (m *Model) tableDiagram() tea.Cmd {
	if m.tableOnly("A diagram") {
		return nil
	}
	name, ok := m.currentTable()
	if !ok {
		return nil
	}
	return m.diagram(erd.Selection{Tables: []string{name}, Depth: 1}, false)
}

// diagram reads the schema, narrowed to sel, and either saves and opens the
// picture or fetches the Mermaid source for the clipboard.
func (m *Model) diagram(sel erd.Selection, mermaid bool) tea.Cmd {
	// config.Load has normalized plan_theme (the one setting for every
	// picture dbc draws), so ByName cannot fail; "" is dark
	pal, _ := theme.ByName(m.cfg.PlanTheme)
	ws := m.ws
	m.log(logInfo, "reading the schema for a diagram…")
	return func() tea.Msg {
		s, err := ws.Diagram(context.Background(), sel)
		if err != nil {
			return erdMsg{err: err}
		}
		if mermaid {
			return erdMsg{mermaid: s.Mermaid(), title: s.Title()}
		}
		data, err := s.PNG(erd.Options{Palette: pal})
		if err != nil {
			return erdMsg{err: err}
		}
		path, err := s.WriteFile(erdDir(), "png", data)
		return erdMsg{path: path, title: s.Title(), err: err}
	}
}

// erdDone reports a diagram: opened, copied, or why not. A refusal (no
// connection, a table gone since the sidebar was drawn) is a warning in its
// own words; anything else is an error.
func (m *Model) erdDone(msg erdMsg) tea.Cmd {
	if msg.err != nil {
		var r *workspace.Refusal
		if errors.As(msg.err, &r) {
			m.log(logWarn, r.Note.Text)
		} else {
			m.logf(logErr, "could not draw the diagram: %s", serr.StringFromErr(msg.err))
		}
		return nil
	}
	if msg.path == "" {
		return m.copyString(msg.mermaid, "the diagram as Mermaid")
	}
	openURL("file://" + msg.path)
	m.logf(logOk, "saved the diagram (%s) as %s — opened it", msg.title, msg.path)
	return nil
}
