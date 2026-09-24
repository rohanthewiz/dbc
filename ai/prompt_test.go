package ai

import (
	"strings"
	"testing"
)

func result(n int) ([]string, [][]string) {
	cols := []string{"id", "name"}
	rows := make([][]string, n)
	for i := range rows {
		rows[i] = []string{string(rune('0' + i%10)), "cat"}
	}
	return cols, rows
}

// The data rule: rows stay home unless the connection opted in, and the note
// names the setting that would send them.
func TestRowsAreWithheldByDefault(t *testing.T) {
	cols, rows := result(3)
	p := Build("why?", Context{Conn: "prod", Driver: "postgres", Query: "SELECT * FROM t",
		Columns: cols, Rows: rows, MaxRows: 10}, false)
	if strings.Contains(p.Text, "| cat |") {
		t.Errorf("row values leaked without ai_rows:\n%s", p.Text)
	}
	if !strings.Contains(p.Text, "columns: id, name") {
		t.Error("column names are schema and should still go")
	}
	if !strings.Contains(p.Note, `set ai_rows = true on connection "prod"`) {
		t.Errorf("note should say how to opt in: %q", p.Note)
	}
}

func TestRowsAreCappedWhenAllowed(t *testing.T) {
	cols, rows := result(25)
	p := Build("summarize", Context{Conn: "dev", Driver: "sqlite", Query: "SELECT 1",
		Columns: cols, Rows: rows, SendRows: true, MaxRows: 10}, false)
	if got := strings.Count(p.Text, "| cat |"); got != 10 {
		t.Errorf("sent %d rows, want 10", got)
	}
	if !strings.Contains(p.Text, "The first 10 of its 25 result rows") {
		t.Error("the model must know it sees a slice, not the table")
	}
	if p.Note != "sent: query, 10 of 25 rows" {
		t.Errorf("note = %q", p.Note)
	}
}

func TestSmallCompleteResultSaysSo(t *testing.T) {
	cols, rows := result(2)
	p := Build("q", Context{Columns: cols, Rows: rows, SendRows: true, MaxRows: 10}, false)
	if !strings.Contains(p.Text, "Its complete result (2 rows)") {
		t.Errorf("text:\n%s", p.Text)
	}
	// a truncated fetch is never "complete", even when every fetched row fits
	p = Build("q", Context{Columns: cols, Rows: rows, Truncated: true, SendRows: true, MaxRows: 10}, false)
	if !strings.Contains(p.Text, "first 2 of its 2+ result rows") {
		t.Errorf("truncated text:\n%s", p.Text)
	}
}

func TestZeroContextRowsSendsNone(t *testing.T) {
	cols, rows := result(5)
	p := Build("q", Context{Conn: "c", Columns: cols, Rows: rows, SendRows: true, MaxRows: 0}, false)
	if strings.Contains(p.Text, "| cat |") {
		t.Error("ai_context_rows = 0 means no rows")
	}
	if strings.Contains(p.Note, "ai_rows") {
		t.Error("the connection did opt in, so the hint would be wrong")
	}
}

// An error is what the user is asking about; the stale result from an earlier
// run is not attached next to it.
func TestErrorReplacesTheResult(t *testing.T) {
	cols, rows := result(3)
	p := Build("fix it", Context{Query: "SELEC 1", Err: "syntax error at SELEC",
		Columns: cols, Rows: rows, SendRows: true, MaxRows: 10}, false)
	if !strings.Contains(p.Text, "failed with:\n```\nsyntax error at SELEC") {
		t.Errorf("text:\n%s", p.Text)
	}
	if strings.Contains(p.Text, "| cat |") {
		t.Error("rows of an earlier result must not ride along with an error")
	}
	if p.Note != "sent: query, error" {
		t.Errorf("note = %q", p.Note)
	}
}

func TestPreambleOnlyOnTheFirstTurn(t *testing.T) {
	if p := Build("q", Context{}, true); !strings.HasPrefix(p.Text, preamble) {
		t.Error("the first turn frames the conversation")
	}
	if p := Build("q", Context{}, false); strings.Contains(p.Text, "SQL assistant inside dbc") {
		t.Error("later turns rely on the session's history")
	}
	if p := Build("  what now?  ", Context{}, false); p.Text != "Question: what now?" || p.Note != "sent: question only" {
		t.Errorf("bare question = %q / %q", p.Text, p.Note)
	}
}

func TestCellsAreEscapedAndCapped(t *testing.T) {
	long := strings.Repeat("x", 500)
	p := Build("q", Context{Columns: []string{"a"}, Rows: [][]string{{"p|q\nr"}, {long}},
		SendRows: true, MaxRows: 10}, false)
	if !strings.Contains(p.Text, `| p\|q r |`) {
		t.Errorf("pipes and newlines must not break the table:\n%s", p.Text)
	}
	if strings.Contains(p.Text, long) || !strings.Contains(p.Text, strings.Repeat("x", maxCellRunes-1)+"…") {
		t.Error("a huge cell should be capped")
	}
}

func TestBytdbIsDescribedAsPostgresDialect(t *testing.T) {
	p := Build("q", Context{Conn: "demo-bytdb", Driver: "bytdb"}, false)
	if !strings.Contains(p.Text, "PostgreSQL dialect") {
		t.Errorf("text:\n%s", p.Text)
	}
}
