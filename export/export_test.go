package export

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/rohanthewiz/dbc/model"
)

func query(conn, sql string, cols []string, rows [][]string) *model.Result {
	raw := make([][]any, len(rows))
	for i, row := range rows {
		raw[i] = make([]any, len(row))
		for j, v := range row {
			raw[i][j] = v
		}
	}
	return &model.Result{
		Conn: conn, Query: sql, Columns: cols, Rows: rows, Raw: raw,
		Duration: 2 * time.Millisecond,
	}
}

func exec(conn, sql string, affected int64) *model.Result {
	return &model.Result{
		Conn: conn, Query: sql, IsExec: true, Affected: affected,
		Columns: []string{"rows_affected"}, Rows: [][]string{{"1"}},
		Raw: [][]any{{affected}}, Duration: time.Millisecond,
	}
}

// A lone result must render exactly as it always did — the multi-statement
// path is not allowed to change one-statement output.
func TestRenderAllSingleMatchesRender(t *testing.T) {
	r := query("demo", "SELECT id FROM cats", []string{"id"}, [][]string{{"1"}})
	for _, f := range Names() {
		f := Format(f)
		want, err := Render(r, f)
		if err != nil {
			t.Fatalf("Render(%s): %v", f, err)
		}
		got, err := RenderAll([]*model.Result{r}, f)
		if err != nil {
			t.Fatalf("RenderAll(%s): %v", f, err)
		}
		if got != want {
			t.Errorf("%s: RenderAll of one result differs from Render:\n%s\n---\n%s", f, got, want)
		}
	}
}

func TestRenderAllEmpty(t *testing.T) {
	if _, err := RenderAll(nil, Text); err == nil {
		t.Fatal("expected an error for no results")
	}
}

func TestRenderAllTextBanners(t *testing.T) {
	out, err := RenderAll([]*model.Result{
		query("demo", "SELECT id FROM cats", []string{"id"}, [][]string{{"1"}}),
		exec("demo", "DELETE FROM cats", 3),
	}, Text)
	if err != nil {
		t.Fatalf("RenderAll: %v", err)
	}
	for _, want := range []string{
		"-- 1/2 │ demo │ 1 rows in 2ms",
		"-- SELECT id FROM cats",
		"-- 2/2 │ demo │ 3 rows affected in 1ms",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("text output missing %q:\n%s", want, out)
		}
	}
}

// The banner is flattened to one line so a multi-line statement cannot smear
// the table below it.
func TestTextBannerFlattensStatement(t *testing.T) {
	out, err := RenderAll([]*model.Result{
		query("demo", "SELECT id\n  FROM cats\n  WHERE age > 3", []string{"id"}, [][]string{{"1"}}),
		query("demo", "SELECT 2", []string{"n"}, [][]string{{"2"}}),
	}, Text)
	if err != nil {
		t.Fatalf("RenderAll: %v", err)
	}
	if !strings.Contains(out, "-- SELECT id FROM cats WHERE age > 3\n") {
		t.Errorf("statement not flattened into one banner line:\n%s", out)
	}
}

// CSV stays parseable: no banners, one header row per block, blocks split by
// a blank line.
func TestRenderAllCSVBlocks(t *testing.T) {
	out, err := RenderAll([]*model.Result{
		query("demo", "SELECT id FROM cats", []string{"id"}, [][]string{{"1"}, {"2"}}),
		query("demo", "SELECT name FROM cats", []string{"name"}, [][]string{{"Luna"}}),
	}, CSV)
	if err != nil {
		t.Fatalf("RenderAll: %v", err)
	}
	want := "id\n1\n2\n\nname\nLuna\n"
	if out != want {
		t.Errorf("csv output = %q, want %q", out, want)
	}
}

func TestRenderAllJSONEnvelopes(t *testing.T) {
	out, err := RenderAll([]*model.Result{
		query("demo", "SELECT id FROM cats", []string{"id"}, [][]string{{"1"}}),
		exec("demo", "DELETE FROM cats", 3),
	}, JSON)
	if err != nil {
		t.Fatalf("RenderAll: %v", err)
	}
	var got []map[string]any
	if err = json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out)
	}
	if len(got) != 2 {
		t.Fatalf("got %d envelopes, want 2", len(got))
	}
	if got[0]["statement"] != "SELECT id FROM cats" || got[0]["conn"] != "demo" {
		t.Errorf("first envelope lost its statement or conn: %v", got[0])
	}
	rows, ok := got[0]["rows"].([]any)
	if !ok || len(rows) != 1 {
		t.Fatalf("first envelope rows = %v, want one row", got[0]["rows"])
	}
	if row := rows[0].(map[string]any); row["id"] != "1" {
		t.Errorf("row = %v, want id 1", row)
	}
	// an exec reports what it affected, not a synthetic one-cell table
	if got[1]["rows_affected"] != float64(3) {
		t.Errorf("second envelope rows_affected = %v, want 3", got[1]["rows_affected"])
	}
	if _, ok := got[1]["rows"]; ok {
		t.Errorf("exec envelope should carry no rows: %v", got[1])
	}
}

func TestRenderAllHTMLSections(t *testing.T) {
	out, err := RenderAll([]*model.Result{
		query("demo", "SELECT id FROM cats", []string{"id"}, [][]string{{"1"}}),
		query("demo", "SELECT name FROM cats", []string{"name"}, [][]string{{"Luna"}}),
	}, HTML)
	if err != nil {
		t.Fatalf("RenderAll: %v", err)
	}
	if n := strings.Count(out, "<html>"); n != 1 {
		t.Errorf("got %d <html> elements, want one document", n)
	}
	for _, want := range []string{"Statement 1 of 2", "Statement 2 of 2", "<th>name</th>"} {
		if !strings.Contains(out, want) {
			t.Errorf("html missing %q", want)
		}
	}
}

func TestRenderAllMarkdownBanners(t *testing.T) {
	out, err := RenderAll([]*model.Result{
		query("demo", "SELECT id FROM cats", []string{"id"}, [][]string{{"1"}}),
		query("demo", "SELECT name FROM cats", []string{"name"}, [][]string{{"Luna"}}),
	}, Markdown)
	if err != nil {
		t.Fatalf("RenderAll: %v", err)
	}
	for _, want := range []string{"**1/2** · `demo`", "```sql\nSELECT id FROM cats\n```", "**2/2**"} {
		if !strings.Contains(out, want) {
			t.Errorf("markdown missing %q:\n%s", want, out)
		}
	}
}

func TestRenderAllUnknownFormat(t *testing.T) {
	rs := []*model.Result{
		query("demo", "SELECT 1", []string{"n"}, [][]string{{"1"}}),
		query("demo", "SELECT 2", []string{"n"}, [][]string{{"2"}}),
	}
	if _, err := RenderAll(rs, Format("yaml")); err == nil {
		t.Fatal("expected an error for an unknown format")
	}
}
