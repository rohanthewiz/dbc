package export

import (
	"reflect"
	"strings"
	"testing"
)

// Transpose swaps rows and columns: a line per source column, led by its
// name, a column per source row headed by its display number — counting
// from first, so a range from the middle of a grid keeps its numbers.
func TestTransposeSwapsRowsAndColumns(t *testing.T) {
	tr := Transpose(mixed(), 4)
	if !tr.Transposed || tr.Conn != "demo" || tr.Query != "SELECT …" {
		t.Errorf("transposed result should keep its source's identity: %+v", tr)
	}
	if want := []string{"column", "row 4", "row 5", "row 6"}; !reflect.DeepEqual(tr.Columns, want) {
		t.Errorf("columns = %q, want %q", tr.Columns, want)
	}
	want := [][]string{
		{"id", "1", "2", "3"},
		{"name", "NULL", "Café", "Tom"},
		{"note", "<b>bold</b>", "NULL", "line one\nline two"},
	}
	if !reflect.DeepEqual(tr.Rows, want) {
		t.Errorf("rows = %q, want %q", tr.Rows, want)
	}
	// Raw turns with Rows: the real NULL is still nil, the string "NULL" a
	// string, and a number still a number for JSON
	if tr.Raw[2][2] != nil || tr.Raw[1][1] != "NULL" || tr.Raw[0][1] != int64(1) {
		t.Errorf("raw = %#v", tr.Raw)
	}
}

// One record — the common case — is "column | value", not "column | row 1".
func TestTransposeOneRowIsAValueColumn(t *testing.T) {
	r := mixed()
	r.Rows, r.Raw = r.Rows[:1], r.Raw[:1]
	tr := Transpose(r, 7)
	if want := []string{"column", "value"}; !reflect.DeepEqual(tr.Columns, want) {
		t.Errorf("columns = %q, want %q", tr.Columns, want)
	}
	csv, err := Render(tr, CSV)
	if err != nil {
		t.Fatal(err)
	}
	if csv != "column,value\nid,1\nname,NULL\nnote,<b>bold</b>\n" {
		t.Errorf("csv = %q", csv)
	}
}

// The pasted table draws the names as row headers in the header's colors,
// still escaped, and a real NULL in its muted italic.
func TestHTMLFragmentTransposedHasRowHeaders(t *testing.T) {
	out := HTMLFragment(Transpose(mixed(), 1))
	if n := strings.Count(out, `<th scope="row"`); n != 3 {
		t.Errorf("want 3 row headers (one per source column), got %d:\n%s", n, out)
	}
	if !strings.Contains(out, `background:`+fragHeadBg+`;color:`+fragHeadFg) ||
		!strings.Contains(out, `font-weight:600">note</th>`) {
		t.Errorf("row headers should wear the header's colors:\n%s", out)
	}
	if strings.Count(out, `font-style:italic">NULL</td>`) != 1 {
		t.Errorf("exactly one real NULL, drawn as such:\n%s", out)
	}
	if !strings.Contains(out, "&lt;b&gt;bold&lt;/b&gt;") || !strings.Contains(out, "line one<br>line two") {
		t.Errorf("values should be escaped and keep their line breaks:\n%s", out)
	}
	// upright, nothing is a row header
	if strings.Contains(HTMLFragment(mixed()), `scope="row"`) {
		t.Error("an upright fragment has no row headers")
	}
}

// The HTML page marks the names as row headers too.
func TestHTMLDocTransposedHasRowHeaders(t *testing.T) {
	out, err := Render(Transpose(mixed(), 1), HTML)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "3 rows in 0s, transposed") {
		t.Errorf("the meta line should count the statement's rows, not the lines: %s", out)
	}
	for _, name := range []string{"id", "name", "note"} {
		if !strings.Contains(out, `<th scope="row">`+name+`</th>`) {
			t.Errorf("page should head the %s line: %s", name, out)
		}
	}
}

// The plain copy turns too, values only: one line per column.
func TestPlainCellsTransposed(t *testing.T) {
	if got := PlainCellsTransposed(mixed()); got != "1\t2\t3\nNULL\tCafé\tTom\n<b>bold</b>\tNULL\tline one\nline two" {
		t.Errorf("plain = %q", got)
	}
	r := mixed()
	r.Rows, r.Columns = [][]string{{"x"}}, []string{"a"}
	if got := PlainCellsTransposed(r); got != "x" {
		t.Errorf("one value alone = %q", got)
	}
}
