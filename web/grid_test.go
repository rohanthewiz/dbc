package web

import (
	"encoding/json"
	"io"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/rohanthewiz/dbc/config"
	"github.com/rohanthewiz/dbc/export"
	"github.com/rohanthewiz/dbc/userdata"
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

// A numeric column is sized from every row — "1000" is the 1,000th — while
// a text column is still sized from the first widthSample rows.
func TestGridSizesNumericColumnsFromEveryRow(t *testing.T) {
	e := newTestEnv(t)
	id, s := e.connected()
	e.runAndWait(id, s, `WITH RECURSIVE n(v) AS (SELECT 1 UNION ALL SELECT v + 1 FROM n WHERE v < 1000)
SELECT v, CASE WHEN v <= 500 THEN 'x' ELSE 'a much longer text value' END AS t FROM n`)
	pg := e.page(id, "from=0&n=10")
	if pg.Rows != 1000 || !pg.Numeric[0] || pg.Numeric[1] {
		t.Fatalf("rows %d numeric %v", pg.Rows, pg.Numeric)
	}
	if pg.Widths[0] != 4 || pg.Content[0] != 4 {
		t.Errorf("numeric width = %d/%d, want 4", pg.Widths[0], pg.Content[0])
	}
	if pg.Widths[1] != 3 {
		t.Errorf("text width = %d, want 3 (header + arrow; sampled rows are \"x\")", pg.Widths[1])
	}
}

// longSQL has two long multi-line text values (100,000 and 300,000 runes,
// "é" and a newline repeated) around a NULL, and a short column: the N-277
// scenario of a column of large documents.
const longSQL = `SELECT id,
  CASE id WHEN 2 THEN NULL ELSE replace(hex(zeroblob(50000 * id)), '00', 'é' || char(10)) END AS doc,
  'short' AS s
FROM (SELECT 1 AS id UNION ALL SELECT 2 UNION ALL SELECT 3)`

// A page carries only the first cellClip runes of a long value, and lists
// the cells it cut; the widths are what they were, and the page stays small
// however large the values (N-277).
func TestGridPageClipsLongValues(t *testing.T) {
	e := newTestEnv(t)
	id, s := e.connected()
	e.runAndWait(id, s, longSQL)

	res := e.req("GET", "/api/v1/ws/"+id+"/result?from=0&n=50", "", nil)
	raw, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if len(raw) > 8<<10 {
		t.Errorf("the page is %d bytes; ~450 KB of values should have been clipped to a few KB", len(raw))
	}
	var env testEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	pg := decodeData[resultPage](t, env)
	full := strings.Repeat("é\n", 50000)
	if got := *pg.Cells[0][1]; utf8.RuneCountInString(got) != cellClip || !strings.HasPrefix(full, got) {
		t.Errorf("row 1's doc is %d runes, want its first %d", utf8.RuneCountInString(got), cellClip)
	}
	if pg.Cells[1][1] != nil || *pg.Cells[0][2] != "short" {
		t.Errorf("NULL and a short value come through as they are: %v %q", pg.Cells[1][1], *pg.Cells[0][2])
	}
	if want := [][2]int{{0, 1}, {2, 1}}; !slices.Equal(pg.Cut, want) {
		t.Errorf("cut = %v, want %v (the two docs, not the NULL or the short column)", pg.Cut, want)
	}
	if pg.Widths[1] != maxColWidth || pg.Content[1] != cellClip {
		t.Errorf("doc width = %d/%d, want the cap %d and content %d", pg.Widths[1], pg.Content[1], maxColWidth, cellClip)
	}
	// a page starting further down lists its cuts relative to its own rows
	if pg2 := e.page(id, "from=2&n=5"); !slices.Equal(pg2.Cut, [][2]int{{0, 1}}) {
		t.Errorf("cut from row 2 = %v, want [[0 1]]", pg2.Cut)
	}
}

// Inspect asks for a cut value whole: GET …/cell names it as the page sees
// it (a display row under a sort, a result column) and the seq it was drawn
// from. A copy never saw the clip.
func TestGridCellIsTheWholeValue(t *testing.T) {
	e := newTestEnv(t)
	id, s := e.connected()
	e.runAndWait(id, s, longSQL)
	seq := itoa(e.page(id, "n=1").Seq)
	cell := func(query string, want int) cellValue {
		t.Helper()
		env := e.api("GET", "/api/v1/ws/"+id+"/cell?seq="+seq+"&"+query, "", want)
		if want != 200 {
			return cellValue{}
		}
		return decodeData[cellValue](t, env)
	}

	if v := cell("row=0&col=1", 200); v.Value == nil || *v.Value != strings.Repeat("é\n", 50000) {
		t.Errorf("row 1's doc is not whole (%d bytes)", len(deref(v.Value)))
	}
	// sorted by id descending, display row 0 is id 3
	if v := cell("sort=0&desc=1&row=0&col=1", 200); v.Value == nil || *v.Value != strings.Repeat("é\n", 150000) {
		t.Errorf("sorted row 0's doc is not id 3's (%d bytes)", len(deref(v.Value)))
	}
	if v := cell("row=1&col=1", 200); v.Value != nil {
		t.Errorf("NULL = %q, want null", *v.Value)
	}
	cell("row=3&col=1", 400)
	cell("row=0&col=3", 400)
	cell("row=-1&col=0", 400)

	env := e.api("POST", "/api/v1/ws/"+id+"/copy", `{"seq":`+seq+`,"cols":[1],"rows":[0,0],"format":"plain"}`, 200)
	if out := decodeData[copyOut](t, env); out.Text != strings.Repeat("é\n", 50000) {
		t.Errorf("copying the cell took %d bytes, want the whole value", len(out.Text))
	}

	// a result replaced since the page was drawn is refused, as a copy is
	e.runAndWait(id, s, "SELECT 1 AS a")
	if env := e.api("GET", "/api/v1/ws/"+id+"/cell?seq="+seq+"&row=0&col=0", "", 409); !strings.Contains(env.Error, "result changed") {
		t.Errorf("error = %q", env.Error)
	}
	id2, _ := e.connected()
	e.api("GET", "/api/v1/ws/"+id2+"/cell?seq=1&row=0&col=0", "", 400)
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// clipRunes cuts on rune boundaries and reads no further than it must.
func TestClipRunes(t *testing.T) {
	for _, c := range []struct {
		in   string
		n    int
		want string
		cut  bool
	}{
		{"abc", 5, "abc", false},
		{"abc", 3, "abc", false},
		{"abcd", 3, "abc", true},
		{"ééé", 3, "ééé", false}, // 6 bytes, 3 runes: not cut
		{"éééé", 3, "ééé", true},
		{"", 0, "", false},
		{"a", 0, "", true},
	} {
		if got, cut := clipRunes(c.in, c.n); got != c.want || cut != c.cut {
			t.Errorf("clipRunes(%q, %d) = %q, %v; want %q, %v", c.in, c.n, got, cut, c.want, c.cut)
		}
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

// A range copy is a real Result in display order, Raw carried along (a NULL
// stays a NULL: an empty CSV field since N-288, not the text "NULL"), and
// it leaves out a column the grid hides — the page just does not list it.
func TestGridCopyRangeAndHidden(t *testing.T) {
	e := newTestEnv(t)
	id, s := e.connected()
	e.runAndWait(id, s, petsSQL)
	seq := itoa(e.page(id, "").Seq)

	// sorted by name: Bella, Leo, Luna, Simba, Whiskers; rows 1–2, cols name+age
	out := decodeData[copyOut](t, e.api("POST", "/api/v1/ws/"+id+"/copy",
		`{"seq":`+seq+`,"sort":1,"cols":[1,2],"rows":[1,2],"format":"csv"}`, 200))
	if out.Text != "name,age\nLeo,\nLuna,\n" || out.What != "2×2 cells as CSV" {
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
	// the plain flavor — what pastes when the rich one is lost — is the
	// aligned table, not the markup
	if strings.Contains(out.Text, "<td") || !strings.HasPrefix(out.Text, "id") || !strings.Contains(out.Text, "name") {
		t.Errorf("text = %.200s", out.Text)
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

// N-334: the config's csv_bom and csv_formula_guard reach a download, and
// not a copy: the copy is going into the user's own sheet, where a BOM is
// a stray character.
func TestGridExportCSVOptions(t *testing.T) {
	e := newTestEnv(t, func(c *config.Config, _ *Options) { c.CSVBOM, c.CSVFormulaGuard = true, true })
	id, s := e.connected()
	e.runAndWait(id, s, "SELECT '=1+1' AS f, -5 AS n")
	seq := itoa(e.page(id, "").Seq)
	res := e.req("GET", "/api/v1/ws/"+id+"/export?format=csv&seq="+seq+"&cols=0,1", "", nil)
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 || string(b) != export.BOM+"f,n\n'=1+1,-5\n" {
		t.Fatalf("download = %d %q, want a BOM, the formula guarded, -5 as it is", res.StatusCode, b)
	}
	out := decodeData[copyOut](t, e.api("POST", "/api/v1/ws/"+id+"/copy", `{"seq":`+seq+`,"cols":[0,1],"format":"csv"}`, 200))
	if strings.Contains(out.Text, export.BOM) || strings.Contains(out.Text, "'=1+1") {
		t.Errorf("a copy took the export options: %q", out.Text)
	}
}

// A transposed copy or export turns the piece the grid named — the rows
// and columns are chosen upright, then the result is put on its side — and
// says so in the log's words.
func TestGridCopyAndExportTransposed(t *testing.T) {
	e := newTestEnv(t)
	id, s := e.connected()
	e.runAndWait(id, s, petsSQL)
	seq := itoa(e.page(id, "").Seq)

	// rows 2–3 (Luna, Bella), id and name: headed by their grid numbers
	out := decodeData[copyOut](t, e.api("POST", "/api/v1/ws/"+id+"/copy",
		`{"seq":`+seq+`,"sort":-1,"cols":[0,1],"rows":[1,2],"format":"csv","transpose":true}`, 200))
	if out.Text != "column,row 2,row 3\nid,2,3\nname,Luna,Bella\n" || out.What != "2×2 cells as CSV, transposed" {
		t.Errorf("csv = %q (%s)", out.Text, out.What)
	}

	// the plain copy turns too, without names
	out = decodeData[copyOut](t, e.api("POST", "/api/v1/ws/"+id+"/copy",
		`{"seq":`+seq+`,"sort":-1,"cols":[0,1],"rows":[1,2],"format":"plain","transpose":true}`, 200))
	if out.Text != "2\t3\nLuna\tBella" || out.What != "2×2 cells, transposed" {
		t.Errorf("plain = %q (%s)", out.Text, out.What)
	}

	// one record for Teams: "column | value", names as row headers, its
	// NULL age still a NULL
	out = decodeData[copyOut](t, e.api("POST", "/api/v1/ws/"+id+"/copy",
		`{"seq":`+seq+`,"sort":-1,"cols":[0,1,2],"rows":[1,1],"row":true,"format":"html","transpose":true}`, 200))
	if !strings.Contains(out.HTML, `>value</th>`) || strings.Count(out.HTML, `<th scope="row"`) != 3 ||
		!strings.Contains(out.HTML, `font-style:italic">NULL</td>`) {
		t.Errorf("html = %s", out.HTML)
	}
	if out.What != "row 2 as a table, transposed" {
		t.Errorf("what = %q", out.What)
	}

	// the export: the whole view on its side
	res := e.req("GET", "/api/v1/ws/"+id+"/export?format=html&seq="+seq+"&sort=-1&cols=1,0&t=1", "", nil)
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("export = %d: %s", res.StatusCode, b)
	}
	page := string(b)
	if !strings.Contains(page, `<th scope="row">name</th><td>Whiskers</td><td>Luna</td>`) ||
		!strings.Contains(page, `<th>row 5</th>`) || strings.Contains(page, `<th scope="row">age`) ||
		!strings.Contains(page, "5 rows in") {
		t.Errorf("transposed page = %s", page)
	}
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

// "Show columns" lists a sidebar table's information_schema.columns rows in
// the grid and records the statement; a name not in the catalog is refused.
func TestShowColumns(t *testing.T) {
	e := newTestEnv(t)
	id, s := e.connected()

	env := e.api("POST", "/api/v1/ws/"+id+"/columns", `{"name":"cats"}`, 200)
	if tag := decodeData[map[string]string](t, env)["tag"]; tag != "columns cats" {
		t.Errorf("tag = %q", tag)
	}
	s.await(t, "run")
	pg := e.page(id, "")
	if pg.Total != 5 || len(pg.Columns) != 6 || pg.Columns[1] != "column_name" {
		t.Fatalf("columns = %d rows, %+v", pg.Total, pg.Columns)
	}
	if got := col(pg, 1); got != "id,name,breed,age,adopted" {
		t.Errorf("column_name = %s", got)
	}
	type entry struct{ SQL string }
	if h := decodeData[[]entry](t, e.api("GET", "/api/v1/history", "", 200)); !strings.Contains(h[0].SQL, "pragma_table_info('cats')") {
		t.Errorf("the columns query should be recorded: %+v", h[0])
	}
	e.api("POST", "/api/v1/ws/"+id+"/columns", `{"name":"cats'); DROP TABLE cats; --"}`, 400)
	e.api("POST", "/api/v1/ws/"+id+"/columns", `{"name":"nope"}`, 400)
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

// The editor's folds come back in UTF-16 too: the statement's code and
// the CTE body, past a non-BMP character that would skew byte offsets.
func TestFolds(t *testing.T) {
	e := newTestEnv(t)
	buf := "SELECT '😀';\nWITH r AS (\n  SELECT 1\n)\nSELECT * FROM r;"
	body, _ := json.Marshal(stmtReq{Buffer: buf})
	got := decodeData[[][2]int](t, e.api("POST", "/api/v1/folds", string(body), 200))
	// the emoji is two units (4 bytes): "WITH" is at unit 13, not byte 15;
	// the statement ends at its "r" (unit 52), the CTE fold at the "\n"
	// before ")" (unit 35)
	want := [][2]int{{13, 52}, {13, 35}}
	if !slices.Equal(got, want) {
		t.Errorf("folds = %v, want %v", got, want)
	}
	if got := decodeData[[][2]int](t, e.api("POST", "/api/v1/folds", `{"buffer":"SELECT 1"}`, 200)); len(got) != 0 {
		t.Errorf("one line = %v, want no folds", got)
	}
}

// Format lays out the buffer as the TUI's ⌥⇧F does, keeping the blanks
// around a selection, and says when there was nothing to change.
func TestFormat(t *testing.T) {
	e := newTestEnv(t)
	ask := func(buf string) formatResp {
		t.Helper()
		body, _ := json.Marshal(formatReq{Buffer: buf})
		return decodeData[formatResp](t, e.api("POST", "/api/v1/format", string(body), 200))
	}
	if r := ask("select a,b from t where x=1\n"); r != (formatResp{Text: "SELECT a, b\nFROM t\nWHERE x = 1\n", Changed: true}) {
		t.Errorf("format = %+v", r)
	}
	if r := ask("SELECT 1"); r.Changed || r.Text != "SELECT 1" {
		t.Errorf("already formatted = %+v", r)
	}

	// format_keywords = "keep": the server's config decides the case, so
	// lower-case SQL stays lower-case with no word from the page
	keep := newTestEnv(t, func(c *config.Config, _ *Options) { c.FormatKeywords = "keep" })
	askKeep := func(buf string) formatResp {
		t.Helper()
		body, _ := json.Marshal(formatReq{Buffer: buf})
		return decodeData[formatResp](t, keep.api("POST", "/api/v1/format", string(body), 200))
	}
	if r := askKeep("select a,b From t where x=1\n"); r != (formatResp{Text: "select a, b\nFrom t\nwhere x = 1\n", Changed: true}) {
		t.Errorf("keep format = %+v", r)
	}
	if r := askKeep("select 1"); r.Changed || r.Text != "select 1" {
		t.Errorf("keep, already formatted = %+v", r)
	}
}

// The editor's right-click menu asks what Run and Run all would execute,
// so its labels and dim rows match the run the click starts.
func TestPicks(t *testing.T) {
	e := newTestEnv(t)
	ask := func(buf string, caret int, sel string) picksResp {
		t.Helper()
		body, _ := json.Marshal(runReq{Buffer: buf, Caret: caret, Selection: sel})
		return decodeData[picksResp](t, e.api("POST", "/api/v1/picks", string(body), 200))
	}
	// the caret's offset is UTF-16: past the emoji's two units it is in
	// the second statement, which in bytes it would not yet be
	buf := "SELECT '😀' AS a;\nSELECT 2 AS b;\nSELECT 3"
	if r := ask(buf, 18, ""); r != (picksResp{Tag: "statement 2/3", N: 1, AllTag: "all 3 statements", All: 3}) {
		t.Errorf("caret pick = %+v", r)
	}
	if r := ask(buf, 0, "SELECT 1; SELECT 2"); r.Tag != "selection (2 statements)" || r.N != 2 || r.Same {
		t.Errorf("selection pick = %+v", r)
	}
	// a selection of every statement: Run all would add nothing
	if r := ask(buf, 0, buf); !r.Same || r.N != 3 {
		t.Errorf("whole-buffer selection = %+v, want same", r)
	}
	if r := ask("SELECT 1", 3, ""); r != (picksResp{Tag: "query", N: 1, AllTag: "query", All: 1, Same: true}) {
		t.Errorf("one statement = %+v", r)
	}
	if r := ask("  -- nothing\n", 0, ""); r != (picksResp{}) {
		t.Errorf("empty buffer = %+v, want nothing to run", r)
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

// A query tab's history is scoped to its database (N-101): scope=auto opens
// on the tab's database when it has entries, else on all of them; db and
// all are asked for outright; the filter applies within the scope.
func TestTabHistoryScopedToTheDatabase(t *testing.T) {
	e := newTestEnv(t)
	id, s := e.connected()
	type answer struct {
		Entries []struct{ Conn, SQL, DB string }
		Scope   string
		DB      string
	}
	get := func(q string) answer {
		t.Helper()
		return decodeData[answer](t, e.api("GET", "/api/v1/ws/"+id+"/history"+q, "", 200))
	}

	// another database's statement, and one from before databases were
	// recorded: this tab's database has nothing yet, so auto is "all"
	e.srv.opt.History.AddEntry(userdata.Entry{At: time.Now(), Conn: "elsewhere", SQL: "SELECT 'there'", DB: "h_1/other"})
	e.srv.opt.History.AddEntry(userdata.Entry{At: time.Now(), Conn: "demo-sqlite", SQL: "SELECT 'old'"})
	if a := get("?scope=auto"); a.Scope != "all" || len(a.Entries) != 2 {
		t.Fatalf("no history here yet: %+v", a)
	}

	e.runAndWait(id, s, "SELECT count(*) FROM cats")
	a := get("?scope=auto")
	if a.Scope != "db" || a.DB != "demo-sqlite" || len(a.Entries) != 1 || a.Entries[0].SQL != "SELECT count(*) FROM cats" {
		t.Fatalf("scoped = %+v", a)
	}
	if a := get("?scope=all"); a.Scope != "all" || len(a.Entries) != 3 {
		t.Errorf("all = %+v", a)
	}
	if a := get("?scope=db&q=there"); a.Scope != "db" || len(a.Entries) != 0 {
		t.Errorf("another database's statement matched in scope: %+v", a)
	}
	if a := get("?scope=all&q=there"); len(a.Entries) != 1 {
		t.Errorf("filter over all = %+v", a)
	}
	// the unscoped listing is as it was: a plain array of everything
	if all := decodeData[[]userdata.Entry](t, e.api("GET", "/api/v1/history", "", 200)); len(all) != 3 {
		t.Errorf("/api/v1/history = %+v", all)
	}
	e.api("GET", "/api/v1/ws/nope/history", "", 404)
}
