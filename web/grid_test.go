package web

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
)

// The results grid's server half: the browser equivalents of the TUI's grid
// tests (tui/grid_test.go, tui/app_test.go). The view — sort, hidden
// columns, a range — is what the page would send; these check that the
// server sorts, pages, projects and renders exactly what the TUI's grid
// would have copied.

// runAndWait runs one statement on a connected tab and waits for it.
func (e *testEnv) runAndWait(id string, s *stream, sql string) runEvent {
	e.t.Helper()
	e.api("POST", "/api/v1/ws/"+id+"/run", runBody(sql, 0, false), 200)
	ev, logs := s.await(e.t, "run")
	run := decodeData[runEvent](e.t, testEnvelope{Data: ev.Data})
	if !run.OK {
		e.t.Fatalf("run %q failed: %+v (logs %q)", sql, run, logs)
	}
	return run
}

func (e *testEnv) page(id, query string) resultPage {
	e.t.Helper()
	return decodeData[resultPage](e.t, e.api("GET", "/api/v1/ws/"+id+"/result?"+query, "", 200))
}

// col reads one column of a page as text, NULL as "∅".
func col(pg resultPage, c int) string {
	var out []string
	for _, row := range pg.Cells {
		if row[c] == nil {
			out = append(out, "∅")
		} else {
			out = append(out, *row[c])
		}
	}
	return strings.Join(out, ",")
}

// petsSQL has a numeric column, a text column with mixed case, and NULLs.
const petsSQL = `SELECT id, name, CASE WHEN id IN (2, 5) THEN NULL ELSE age END AS age FROM cats WHERE id IN (1, 2, 3, 5, 8) ORDER BY id`

// Sorting is the TUI's: numbers as numbers, NULLs last both ways, and a
// third state back to the result's order; the page fetch carries the sort.
func TestGridSortsOnTheServer(t *testing.T) {
	e := newTestEnv(t)
	id, s := e.connected()
	e.runAndWait(id, s, petsSQL)

	pg := e.page(id, "from=0&n=50")
	if pg.Sort != -1 || col(pg, 1) != "Whiskers,Luna,Bella,Leo,Simba" {
		t.Fatalf("result order = %s (sort %d)", col(pg, 1), pg.Sort)
	}
	if !pg.Numeric[0] || pg.Numeric[1] || !pg.Numeric[2] {
		t.Errorf("numeric = %v", pg.Numeric)
	}
	if got := col(e.page(id, "sort=2"), 2); got != "3,5,6,∅,∅" {
		t.Errorf("age asc = %s, want NULLs last", got)
	}
	if got := col(e.page(id, "sort=2&desc=1"), 2); got != "6,5,3,∅,∅" {
		t.Errorf("age desc = %s, want NULLs last", got)
	}
	if got := col(e.page(id, "sort=1&desc=1"), 1); got != "Whiskers,Simba,Luna,Leo,Bella" {
		t.Errorf("name desc = %s", got)
	}
	if got := e.page(id, "sort=99"); got.Sort != -1 {
		t.Errorf("an unknown column sorts nothing, got sort %d", got.Sort)
	}
}

// Pages are windows of display rows, and max_display_rows caps them — it
// bounds drawing, not what a whole-result copy takes.
func TestGridPagesAndDisplayCap(t *testing.T) {
	e := newTestEnv(t)
	e.srv.cfg.MaxDisplayRows = 5
	id, s := e.connected()
	run := e.runAndWait(id, s, "SELECT value FROM json_each('[1,2,3,4,5,6,7,8,9,10,11,12]')")
	if !strings.Contains(run.Status, "(showing 5)") {
		t.Errorf("status = %q", run.Status)
	}
	pg := e.page(id, "from=3&n=10")
	if pg.Total != 5 || pg.Rows != 12 || pg.From != 3 || col(pg, 0) != "4,5" {
		t.Fatalf("page = total %d rows %d from %d cells %s", pg.Total, pg.Rows, pg.From, col(pg, 0))
	}
	if !pg.Numeric[0] {
		t.Error("value should be numeric")
	}
	env := e.api("POST", "/api/v1/ws/"+id+"/copy",
		`{"seq":`+itoa(pg.Seq)+`,"sort":0,"desc":true,"cols":[0],"format":"csv"}`, 200)
	out := decodeData[copyOut](t, env)
	if !strings.HasPrefix(out.Text, "value\n12\n11\n") || strings.Count(out.Text, "\n") != 13 {
		t.Errorf("a whole copy takes every row, sorted: %q", out.Text)
	}
	if out.What != "the result (12 rows) as CSV" {
		t.Errorf("what = %q", out.What)
	}
}

// A range copy is a real Result in display order, Raw carried along (NULL
// stays NULL), and it leaves out a column the grid hides — the page just
// does not list it.
func TestGridCopyRangeAndHidden(t *testing.T) {
	e := newTestEnv(t)
	id, s := e.connected()
	e.runAndWait(id, s, petsSQL)
	seq := itoa(e.page(id, "").Seq)

	// sorted by name: Bella, Leo, Luna, Simba, Whiskers; rows 1–2, cols name+age
	out := decodeData[copyOut](t, e.api("POST", "/api/v1/ws/"+id+"/copy",
		`{"seq":`+seq+`,"sort":1,"cols":[1,2],"rows":[1,2],"format":"csv"}`, 200))
	if out.Text != "name,age\nLeo,NULL\nLuna,NULL\n" || out.What != "2×2 cells as CSV" {
		t.Errorf("range = %q (%s)", out.Text, out.What)
	}

	// plain: one value alone; a row tab-separated without a header
	out = decodeData[copyOut](t, e.api("POST", "/api/v1/ws/"+id+"/copy",
		`{"seq":`+seq+`,"sort":1,"cols":[1],"rows":[0,0],"format":"plain"}`, 200))
	if out.Text != "Bella" || out.What != "name of row 1" {
		t.Errorf("cell = %q (%s)", out.Text, out.What)
	}
	out = decodeData[copyOut](t, e.api("POST", "/api/v1/ws/"+id+"/copy",
		`{"seq":`+seq+`,"sort":-1,"cols":[0,2],"rows":[0,0],"row":true,"hidden":1}`, 200))
	if out.Text != "1\t3" || out.What != "row 1" {
		t.Errorf("row with name hidden = %q (%s)", out.Text, out.What)
	}

	// the whole result with a column hidden says so
	out = decodeData[copyOut](t, e.api("POST", "/api/v1/ws/"+id+"/copy",
		`{"seq":`+seq+`,"sort":-1,"cols":[0,2],"hidden":1,"format":"markdown"}`, 200))
	if !strings.HasPrefix(out.Text, "| id | age |") || strings.Contains(out.Text, "Whiskers") {
		t.Errorf("markdown = %q", out.Text)
	}
	if out.What != "the result (5 rows, 1 column hidden) as Markdown" {
		t.Errorf("what = %q", out.What)
	}
}

// HTML is the copy that means something else on a clipboard: a rich flavor
// holding an inline-styled table, for Teams.
func TestGridCopyHTMLHasTheRichFlavor(t *testing.T) {
	e := newTestEnv(t)
	id, s := e.connected()
	e.runAndWait(id, s, "SELECT id, name FROM cats ORDER BY id")
	seq := itoa(e.page(id, "").Seq)
	out := decodeData[copyOut](t, e.api("POST", "/api/v1/ws/"+id+"/copy",
		`{"seq":`+seq+`,"sort":-1,"cols":[0,1],"format":"html"}`, 200))
	if !strings.Contains(out.HTML, "<table style=") || strings.Count(out.HTML, "<tr>") != 9 {
		t.Errorf("html = %.200s", out.HTML)
	}
	if out.What != "the result (8 rows) as a table" {
		t.Errorf("what = %q", out.What)
	}
	for _, bad := range []string{
		`{"seq":` + seq + `,"cols":[0],"format":"nope"}`, // unknown format
		`{"seq":` + seq + `,"cols":[7],"format":"csv"}`,  // no such column
		`{"seq":` + seq + `,"cols":[],"format":"csv"}`,   // nothing to copy
		`{"seq":` + seq + `,"cols":[0],"rows":[5,99]}`,   // rows past the end
		`{"seq":` + seq + `,"cols":[0],"rows":[3,1]}`,    // backwards
	} {
		e.api("POST", "/api/v1/ws/"+id+"/copy", bad, 400)
	}
}

// A copy made against a result that has since been replaced is refused:
// the clipboard must never get a different result than the one on screen.
func TestGridCopyOfAStaleResultIs409(t *testing.T) {
	e := newTestEnv(t)
	id, s := e.connected()
	e.runAndWait(id, s, "SELECT 1 AS a")
	old := e.page(id, "").Seq
	e.runAndWait(id, s, "SELECT 2 AS a")
	if e.page(id, "").Seq == old {
		t.Fatal("a new result should have a new seq")
	}
	env := e.api("POST", "/api/v1/ws/"+id+"/copy", `{"seq":`+itoa(old)+`,"cols":[0],"format":"plain"}`, 409)
	if !strings.Contains(env.Error, "result changed") {
		t.Errorf("error = %q", env.Error)
	}
	// and nothing to copy before any run
	id2, _ := e.connected()
	e.api("POST", "/api/v1/ws/"+id2+"/copy", `{"seq":1,"cols":[0]}`, 400)
}

// Export is a download of the whole view, named after the connection.
func TestGridExportDownloads(t *testing.T) {
	e := newTestEnv(t)
	id, s := e.connected()
	e.runAndWait(id, s, "SELECT id, name, breed FROM cats ORDER BY id")
	seq := itoa(e.page(id, "").Seq)
	res := e.req("GET", "/api/v1/ws/"+id+"/export?format=csv&seq="+seq+"&sort=1&desc=0&cols=1,0", "", nil)
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("export = %d: %s", res.StatusCode, b)
	}
	cd := res.Header.Get("Content-Disposition")
	if !strings.HasPrefix(cd, `attachment; filename="demo-sqlite-`) || !strings.HasSuffix(cd, `.csv"`) {
		t.Errorf("Content-Disposition = %q", cd)
	}
	if !strings.HasPrefix(res.Header.Get("Content-Type"), "text/csv") {
		t.Errorf("Content-Type = %q", res.Header.Get("Content-Type"))
	}
	if !strings.HasPrefix(string(b), "name,id\nBella,3\n") || strings.Contains(string(b), "Tabby") {
		t.Errorf("file = %q, want name then id, sorted by name, breed left out", b)
	}
	for _, f := range []string{"tsv", "markdown", "html", "json", "text"} {
		res := e.req("GET", "/api/v1/ws/"+id+"/export?format="+f+"&seq="+seq+"&cols=0", "", nil)
		res.Body.Close()
		if res.StatusCode != 200 || !strings.Contains(res.Header.Get("Content-Disposition"), "attachment") {
			t.Errorf("export %s = %d %q", f, res.StatusCode, res.Header.Get("Content-Disposition"))
		}
	}
	e.api("GET", "/api/v1/ws/"+id+"/export?format=xls&seq="+seq+"&cols=0", "", 400)
}

// History lists what ran (shared with the TUI's file), newest first and
// filtered; a table preview runs without the editor, and is recorded.
func TestHistoryAndPreview(t *testing.T) {
	e := newTestEnv(t)
	id, s := e.connected()
	e.runAndWait(id, s, "SELECT id, name, breed FROM cats")
	e.runAndWait(id, s, "SELECT count(*) FROM cats")

	type entry struct{ Conn, SQL string }
	all := decodeData[[]entry](t, e.api("GET", "/api/v1/history", "", 200))
	if len(all) < 2 || all[0].SQL != "SELECT count(*) FROM cats" || all[0].Conn != "demo-sqlite" {
		t.Fatalf("history = %+v", all)
	}
	got := decodeData[[]entry](t, e.api("GET", "/api/v1/history?q=BREED", "", 200))
	if len(got) != 1 || !strings.Contains(got[0].SQL, "breed") {
		t.Errorf("filtered = %+v", got)
	}

	env := e.api("POST", "/api/v1/ws/"+id+"/preview", `{"name":"cats"}`, 200)
	if tag := decodeData[map[string]string](t, env)["tag"]; tag != "preview cats" {
		t.Errorf("tag = %q", tag)
	}
	s.await(t, "run")
	if pg := e.page(id, ""); pg.Total != 8 || len(pg.Columns) != 5 {
		t.Errorf("preview = %d rows, %v", pg.Total, pg.Columns)
	}
	if h := decodeData[[]entry](t, e.api("GET", "/api/v1/history", "", 200)); h[0].SQL != "SELECT * FROM cats LIMIT 100" {
		t.Errorf("the preview should be recorded: %+v", h[0])
	}
	e.api("POST", "/api/v1/ws/"+id+"/preview", `{"name":"cats; DROP TABLE cats"}`, 400)
}

// The editor's marker asks for the statement under the caret, in UTF-16.
func TestStmtRange(t *testing.T) {
	e := newTestEnv(t)
	buf := "SELECT 'é😀' AS a;\nSELECT 2 AS b;"
	body, _ := json.Marshal(stmtReq{Buffer: buf, Caret: 22})
	r := decodeData[[2]int](t, e.api("POST", "/api/v1/stmt", string(body), 200))
	// 'é' is one unit and the emoji two (4 bytes): the second statement
	// starts at unit 19, past the newline, and ends before its ";" at 32 —
	// in bytes it would be 22 and 35
	if r != [2]int{19, 32} {
		t.Errorf("range = %v, want [19 32]", r)
	}
	body, _ = json.Marshal(stmtReq{Buffer: "SELECT 1", Caret: 3})
	if r := decodeData[[2]int](t, e.api("POST", "/api/v1/stmt", string(body), 200)); r != [2]int{} {
		t.Errorf("one statement = %v, want no marker", r)
	}
}

// A multi-statement run says which statement it is on, on the stream.
func TestMultiStatementProgress(t *testing.T) {
	e := newTestEnv(t)
	id, s := e.connected()
	slow := "WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n) SELECT count(*) FROM n"
	e.api("POST", "/api/v1/ws/"+id+"/run", runBody("SELECT 1; "+slow+"; SELECT 3", 0, true), 200)
	for {
		ev, _ := s.await(t, "tick")
		var b busyEvent
		_ = json.Unmarshal(ev.Data, &b)
		if strings.HasPrefix(b.Status, "all 3 statements · 2/3 ") {
			break
		}
	}
	e.api("POST", "/api/v1/ws/"+id+"/cancel", "", 200)
	ev, logs := s.await(t, "run")
	if run := decodeData[runEvent](t, testEnvelope{Data: ev.Data}); !run.Stopped {
		t.Fatalf("run = %+v (logs %q)", run, logs)
	}
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}
