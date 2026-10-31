package workspace

import (
	"fmt"
	"strings"

	"github.com/rohanthewiz/dbc/sqlsplit"
)

// Editor is what a UI's SQL editor holds at the moment of a request: the
// buffer, the caret as a byte offset into it, and the selected text ("" when
// nothing is selected). The UI sends the caret and the workspace picks the
// statement, so "the statement under the caret" means the same thing in
// every UI.
type Editor struct {
	Text      string
	Caret     int
	Selection string
}

// Pick is what Ctrl+R executes: the selection's statements when there is a
// selection, otherwise the statement under the caret. tag names the run for
// the status bar and log: "query" when the buffer holds one statement,
// "statement 2/4" when the caret picked one of several, "selection" or
// "selection (3 statements)".
//
// d is the engine the statements will run on (Workspace.Dialect), which
// decides where a string ends: 'a\' closes at its quote on Postgres and
// does not on MySQL, and the statement after it is a statement of its own
// only where it closes. Pick, PickAll and StmtRange all take it, so what a
// UI marks and labels is what the run takes.
func Pick(ed Editor, d sqlsplit.Dialect) (stmts []string, tag string) {
	if strings.TrimSpace(ed.Selection) != "" {
		for _, p := range d.Split(ed.Selection) {
			stmts = append(stmts, p.Text)
		}
		switch len(stmts) {
		case 0:
			return nil, ""
		case 1:
			return stmts, "selection"
		}
		return stmts, fmt.Sprintf("selection (%d statements)", len(stmts))
	}
	all := d.Split(ed.Text)
	if len(all) == 0 {
		return nil, ""
	}
	i := sqlsplit.IndexAt(all, ed.Caret)
	if len(all) == 1 {
		return []string{all[i].Text}, "query"
	}
	return []string{all[i].Text}, fmt.Sprintf("statement %d/%d", i+1, len(all))
}

// PickAll is what Ctrl+Shift+R executes: every statement in the buffer,
// whatever the caret or selection says. A one-statement buffer is tagged
// "query", as Pick tags it, since "all" of one says nothing more. d is the
// engine's reading of strings, as for Pick.
func PickAll(text string, d sqlsplit.Dialect) (stmts []string, tag string) {
	for _, p := range d.Split(text) {
		stmts = append(stmts, p.Text)
	}
	switch len(stmts) {
	case 0:
		return nil, ""
	case 1:
		return stmts, "query"
	}
	return stmts, fmt.Sprintf("all %d statements", len(stmts))
}

// StmtRange is the byte range of the statement under the caret, for an
// editor's gutter marker — zero when the buffer holds fewer than two
// statements, since marking the only one says nothing.
//
// The range starts at the statement's first code (CodeStart), not at the
// comments heading it: a separator such as `---` between two statements is
// left unmarked, so it reads as the gap it is meant to be. Which statement
// the caret picks is unchanged, so a caret on such a comment still marks
// (and Ctrl+R still runs) the statement below, with the marker starting
// under the caret — as a caret on a blank line between statements does.
// d is the engine's reading of strings, as for Pick.
func StmtRange(text string, caret int, d sqlsplit.Dialect) [2]int {
	all := d.Split(text)
	if len(all) < 2 {
		return [2]int{}
	}
	i := sqlsplit.IndexAt(all, caret)
	return [2]int{all[i].CodeStart, all[i].End}
}
