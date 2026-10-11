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

// Schema is not row data: it goes without ai_rows, one line per table, and
// the note names the tables.
func TestSchemaGoesWithoutAIRows(t *testing.T) {
	p := Build("who owns whom?", Context{Conn: "prod", Driver: "postgres", Query: "SELECT * FROM cats",
		Tables: []Table{
			{Name: "cats", Columns: []Column{{"id", "integer"}, {"owner_id", "integer"}}},
			{Name: "old_cats", View: true, Columns: []Column{{"id", "integer"}, {"expr", ""}}},
		}}, false)
	for _, want := range []string{
		"from the database's catalog",
		"- cats: id integer, owner_id integer\n",
		"- old_cats (view): id integer, expr\n",
	} {
		if !strings.Contains(p.Text, want) {
			t.Errorf("prompt is missing %q:\n%s", want, p.Text)
		}
	}
	if p.Note != "sent: schema of cats, old_cats, query" {
		t.Errorf("note = %q", p.Note)
	}
}

// A table with no columns (not yet looked up, or not describable) is named
// in the note — the chip's forecast — but adds nothing to the text.
func TestSchemaWithoutColumnsIsOnlyNoted(t *testing.T) {
	p := Build("q", Context{Tables: []Table{{Name: "cats"}}}, false)
	if strings.Contains(p.Text, "catalog") {
		t.Errorf("no columns means no schema text:\n%s", p.Text)
	}
	if p.Note != "sent: schema of cats" {
		t.Errorf("note = %q", p.Note)
	}
}

func TestSchemaIsCapped(t *testing.T) {
	cols := make([]Column, maxSchemaColumns+5)
	for i := range cols {
		cols[i] = Column{Name: "c", Type: "int"}
	}
	var tables []Table
	for _, n := range []string{"a", "b", "c", "d"} {
		tables = append(tables, Table{Name: n, Columns: cols})
	}
	p := Build("q", Context{Tables: tables}, false)
	if !strings.Contains(p.Text, ", … and 5 more\n") {
		t.Errorf("a wide table should be cut with a count:\n%.300s", p.Text)
	}
	if p.Note != "sent: schema of 4 tables" {
		t.Errorf("many tables are counted, not listed: %q", p.Note)
	}
}

// Hidden columns: their values stay out of the rows, their names are said,
// and the note counts them.
func TestHiddenColumnsAreLeftOut(t *testing.T) {
	cols := []string{"id", "email", "name", "ssn"}
	rows := [][]string{{"1", "a@x", "Ann", "111"}, {"2", "b@x", "Bob", "222"}}
	p := Build("q", Context{Conn: "c", Columns: cols, Rows: rows, Hidden: []int{1, 3},
		SendRows: true, MaxRows: 10}, false)
	if strings.Contains(p.Text, "a@x") || strings.Contains(p.Text, "111") {
		t.Errorf("hidden values leaked:\n%s", p.Text)
	}
	for _, want := range []string{"| id | name |", "| 1 | Ann |",
		"The user hid these columns in the grid, so they are left out above: email, ssn."} {
		if !strings.Contains(p.Text, want) {
			t.Errorf("text is missing %q:\n%s", want, p.Text)
		}
	}
	if p.Note != "sent: 2 of 2 rows (2 columns hidden)" {
		t.Errorf("note = %q", p.Note)
	}

	// without ai_rows only names go, so the note need not mention hiding,
	// but the model still hears which columns the user hid
	p = Build("q", Context{Conn: "c", Columns: cols, Rows: rows, Hidden: []int{1}, MaxRows: 10}, false)
	if !strings.Contains(p.Text, "columns: id, name, ssn (2 rows") ||
		!strings.Contains(p.Text, "left out above: email.") {
		t.Errorf("text:\n%s", p.Text)
	}
	if strings.Contains(p.Note, "hidden") {
		t.Errorf("note = %q", p.Note)
	}
}

// Nonsense in Hidden is ignored, and hiding everything is read as hiding
// nothing rather than sending an empty result.
func TestHiddenColumnsEdgeCases(t *testing.T) {
	cols, rows := result(1)
	p := Build("q", Context{Columns: cols, Rows: rows, Hidden: []int{-1, 5, 1, 1},
		SendRows: true, MaxRows: 10}, false)
	if !strings.Contains(p.Text, "| id |\n") || !strings.Contains(p.Note, "(1 column hidden)") {
		t.Errorf("text:\n%s\nnote: %q", p.Text, p.Note)
	}
	p = Build("q", Context{Columns: cols, Rows: rows, Hidden: []int{0, 1},
		SendRows: true, MaxRows: 10}, false)
	if !strings.Contains(p.Text, "| id | name |") || strings.Contains(p.Text, "hid") {
		t.Errorf("all hidden:\n%s", p.Text)
	}
}

// After a header sort the rows go in the grid's order, the model is told
// the order is the grid's, and the note says so beside any hidden columns.
func TestRowsFollowTheGridSort(t *testing.T) {
	cols := []string{"name", "age"}
	rows := [][]string{{"Ann", "3"}, {"Bob", "9"}, {"Cy", "5"}}
	p := Build("q", Context{Columns: cols, Rows: rows, Order: []int{1, 2},
		SortedBy: "age", SortDesc: true, Hidden: []int{0}, SendRows: true, MaxRows: 2}, false)
	if !strings.Contains(p.Text, "| 9 |\n| 5 |\n") {
		t.Errorf("rows should be in the grid's order:\n%s", p.Text)
	}
	if !strings.Contains(p.Text, "sorted the result in the grid by age, descending (NULLs last), "+
		"so the rows below are in that order, not the query's.") {
		t.Errorf("text:\n%s", p.Text)
	}
	if p.Note != "sent: 2 of 3 rows (sorted by age desc, 1 column hidden)" {
		t.Errorf("note = %q", p.Note)
	}

	// no rows sent, no order to explain
	p = Build("q", Context{Conn: "c", Columns: cols, Rows: rows, Order: []int{1},
		SortedBy: "age", MaxRows: 2}, false)
	if strings.Contains(p.Text, "sorted") || strings.Contains(p.Note, "sorted") {
		t.Errorf("sort mentioned without rows:\n%s\nnote: %q", p.Text, p.Note)
	}
}

// A malformed Order drops rows rather than panicking or mixing in rows in
// result order, and the counts follow what was actually sent.
func TestMalformedOrderOnlyShrinks(t *testing.T) {
	cols, rows := result(5)
	p := Build("q", Context{Columns: cols, Rows: rows, Order: []int{9, 4, -1},
		SortedBy: "id", SendRows: true, MaxRows: 3}, false)
	if strings.Count(p.Text, "| cat |") != 1 || !strings.Contains(p.Text, "| 4 | cat |") {
		t.Errorf("text:\n%s", p.Text)
	}
	if !strings.HasPrefix(p.Note, "sent: 1 of 5 rows") {
		t.Errorf("note = %q", p.Note)
	}
}

// A plan goes with the question on any connection — it is the database's
// shape, like the schema — fenced, and named in the note.
func TestBuildIncludesPlan(t *testing.T) {
	ctx := Context{Conn: "pg", Driver: "postgres", Query: "SELECT * FROM orders WHERE user_id = 42",
		Plan: "Plan · postgres · analyzed\n\nSeq Scan · orders  (user_id = 42)"}
	p := Build("why is this slow?", ctx, false)
	if !strings.Contains(p.Text, "Its query plan") || !strings.Contains(p.Text, "Seq Scan · orders") {
		t.Errorf("prompt lacks the plan:\n%s", p.Text)
	}
	if !strings.Contains(p.Note, "plan") {
		t.Errorf("note = %q", p.Note)
	}
	if strings.Contains(Build("q", Context{Query: "SELECT 1"}, false).Note, "plan") {
		t.Error("no plan, no mention")
	}
}

// A script tab's question: the source is fenced as Go and called the
// script, never "The SQL in question"; the connections and (when the
// caller set it) the sdb API go ahead of it, each named in the note. The
// API is the caller's to send once — without it, only the script goes.
func TestScriptQuestion(t *testing.T) {
	src := "package main\n\nfunc Run(s *sdb.S) error { return nil }"
	ctx := Context{Script: "nightly.go", Query: src, ScriptConns: []string{"prod (postgres)", "lite (sqlite)"},
		ScriptAPI: "func (s *S) Query(conn, query string, args ...any) (*Result, error)"}
	p := Build("why?", ctx, true)
	for _, want := range []string{
		"The Go script in question (nightly.go):\n```go\n" + src + "\n```",
		"```go block", // the script preamble asks for Go back
		"The sdb API, by signature:\n```\nfunc (s *S) Query(",
		"Connections configured in dbc (a script names them as conn, src and dst): prod (postgres), lite (sqlite).",
	} {
		if !strings.Contains(p.Text, want) {
			t.Errorf("missing %q in:\n%s", want, p.Text)
		}
	}
	if strings.Contains(p.Text, "The SQL in question") {
		t.Errorf("a script was framed as SQL:\n%s", p.Text)
	}
	if p.Note != "sent: sdb API, connection names, script" {
		t.Errorf("note = %q", p.Note)
	}

	ctx.ScriptAPI = ""
	if p = Build("and?", ctx, false); strings.Contains(p.Text, "sdb API") || p.Note != "sent: connection names, script" {
		t.Errorf("without the API: note %q, text:\n%s", p.Note, p.Text)
	}
	// the API never goes with a SQL question, even if set
	if p = Build("q", Context{Query: "SELECT 1", ScriptAPI: "x"}, false); strings.Contains(p.Text, "sdb API") {
		t.Errorf("the API went with SQL:\n%s", p.Text)
	}
}

// A result the user shared on purpose is framed as one — which, and from
// which statement — goes even beside the query's own error, and is named
// in the note; the data rule still decides its rows.
func TestSharedResult(t *testing.T) {
	cols, rows := result(25)
	p := Build("compare", Context{Conn: "dev", Query: "SELEC 1", Err: "syntax error",
		Columns: cols, Rows: rows, SendRows: true, MaxRows: 10,
		Shared: true, SharedLabel: "result 2", SharedFrom: "SELECT * FROM cats"}, false)
	if !strings.Contains(p.Text, "The user shared a result with you (result 2), from a different statement:\n```sql\nSELECT * FROM cats\n```") {
		t.Errorf("framing:\n%s", p.Text)
	}
	if got := strings.Count(p.Text, "| cat |"); got != 10 {
		t.Errorf("sent %d rows beside the error, want 10", got)
	}
	if p.Note != "sent: query, error, shared result 2: 10 of 25 rows" {
		t.Errorf("note = %q", p.Note)
	}

	// the query's own result, shared: no "different statement"
	p = Build("q", Context{Conn: "dev", Query: "SELECT 1", Columns: cols, Rows: rows[:2], SendRows: true, MaxRows: 10,
		Shared: true, SharedLabel: "result 1"}, false)
	if !strings.Contains(p.Text, "The user shared a result with you (result 1).\nIts complete result (2 rows)") {
		t.Errorf("own result:\n%s", p.Text)
	}

	// ai_rows turned off after sharing: names only, and the note says why
	p = Build("q", Context{Conn: "prod", Columns: cols, Rows: rows, MaxRows: 10,
		Shared: true, SharedLabel: "result 3"}, false)
	if strings.Contains(p.Text, "| cat |") || p.Note != `sent: shared result 3: column names · rows not sent — set ai_rows = true on connection "prod" to include them` {
		t.Errorf("withheld: note %q\n%s", p.Note, p.Text)
	}
}

// A pipeline or job tab's question is framed as the spec it is (N-178):
// fenced as JSON and named, with the pipeline preamble asking for JSON
// back, and the connections a node names; never as SQL.
func TestPipelineQuestion(t *testing.T) {
	spec := `{"name": "orders", "fragments": []}`
	ctx := Context{Pipeline: "orders.json", PipelineKind: "pipeline", Query: spec, Err: "fragment load: no such table",
		ScriptConns: []string{"prod (postgres)"}, ScriptAPI: "sql.read — Reads rows. [conn*, query*]"}
	p := Build("why did it fail?", ctx, true)
	for _, want := range []string{
		"The dbc pipeline in question (orders.json):\n```json\n" + spec + "\n```",
		"editing a dbc pipeline or job: a JSON spec", // the pipeline preamble, not the script's
		"The sdb API, by signature:\n```\nsql.read",
		"Connections configured in dbc (a node names one in its conn field): prod (postgres).",
		"Running it failed with:\n```\nfragment load: no such table",
	} {
		if !strings.Contains(p.Text, want) {
			t.Errorf("missing %q in:\n%s", want, p.Text)
		}
	}
	if strings.Contains(p.Text, "The SQL in question") || strings.Contains(p.Text, "a Go program run by") {
		t.Errorf("a pipeline was framed as SQL or a script:\n%s", p.Text)
	}
	if p.Note != "sent: sdb API, connection names, pipeline, error" {
		t.Errorf("note = %q", p.Note)
	}
	ctx.PipelineKind, ctx.Pipeline, ctx.ScriptAPI = "job", "nightly.json", ""
	if p = Build("q", ctx, false); !strings.Contains(p.Text, "The dbc job in question (nightly.json):\n```json\n") ||
		p.Note != "sent: connection names, job, error" {
		t.Errorf("a job: note %q, text:\n%s", p.Note, p.Text)
	}
}
