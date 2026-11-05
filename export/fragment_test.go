package export

import (
	"strings"
	"testing"

	"github.com/rohanthewiz/dbc/model"
)

// mixed is a result with a number column, a text column holding the string
// "NULL", a real NULL, markup that must be escaped, and a multi-line value.
func mixed() *model.Result {
	return &model.Result{
		Conn: "demo", Query: "SELECT …",
		Columns: []string{"id", "name", "note"},
		Rows: [][]string{
			{"1", "NULL", "<b>bold</b>"},
			{"2", "Café", "NULL"},
			{"3", "Tom", "line one\nline two"},
		},
		Raw: [][]any{
			{int64(1), "NULL", "<b>bold</b>"},
			{int64(2), "Café", nil},
			{int64(3), "Tom", "line one\nline two"},
		},
	}
}

// Paste targets strip <style>, so everything the table looks like must ride
// on the elements themselves.
func TestHTMLFragmentIsSelfStyled(t *testing.T) {
	out := HTMLFragment(mixed())
	for _, banned := range []string{"<style", "<head", "<html", "<body", "class="} {
		if strings.Contains(out, banned) {
			t.Errorf("fragment must not rely on %q — chat apps drop it:\n%s", banned, out)
		}
	}
	if !strings.HasPrefix(out, `<meta charset="utf-8"><table style="`) {
		t.Errorf("fragment should open with the charset hint and a styled table: %.80s", out)
	}
	// No cell may set a surface: Teams keeps a pasted background but
	// rewrites the text color to its theme's, so in dark-theme Teams any
	// light fill became white text on white. The only text color allowed is
	// the mid-gray of a real NULL, which reads in either theme.
	for _, cell := range strings.Split(out, "<t")[1:] {
		if strings.HasPrefix(cell, "able") || strings.HasPrefix(cell, "head") ||
			strings.HasPrefix(cell, "body") || strings.HasPrefix(cell, "r>") {
			continue
		}
		if strings.Contains(cell, "background") {
			t.Errorf("a cell sets a background, which Teams' dark theme turns unreadable: <t%.120s", cell)
		}
		if strings.Contains(cell, "color:") && !strings.Contains(cell, ">NULL</td>") {
			t.Errorf("a cell sets a text color: <t%.120s", cell)
		}
		if !strings.Contains(cell, "border:1px solid "+fragBorder) {
			t.Errorf("a cell is missing its border: <t%.120s", cell)
		}
	}
	if strings.Contains(out, "font-family") || strings.Contains(out, "font-size") {
		t.Error("the table should take the message's own font")
	}
}

func TestHTMLFragmentCells(t *testing.T) {
	out := HTMLFragment(mixed())
	cases := []struct{ name, want string }{
		{"markup is escaped", "&lt;b&gt;bold&lt;/b&gt;"},
		{"non-ASCII survives", "Café"},
		{"a newline becomes a line break", "line one<br>line two"},
		{"a real NULL is muted and italic", "text-align:left;vertical-align:top;color:" + fragNullFg + ";font-style:italic\">NULL</td>"},
	}
	for _, c := range cases {
		if !strings.Contains(out, c.want) {
			t.Errorf("%s: missing %q", c.name, c.want)
		}
	}
	// the string "NULL" in the name column is data, drawn like any other value
	if !strings.Contains(out, "text-align:left;vertical-align:top\">NULL</td>") {
		t.Error(`the string "NULL" must not be styled as a real NULL`)
	}
	if strings.Contains(out, "<b>bold</b>") {
		t.Error("unescaped markup would render as bold in the chat app")
	}
}

// Numbers right-align like a spreadsheet; the header cell of a numeric column
// aligns with its values.
func TestHTMLFragmentAlignsNumbers(t *testing.T) {
	got := NumericColumns(mixed())
	want := []bool{true, false, false}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("column %d numeric = %v, want %v", i, got[i], want[i])
		}
	}
	out := HTMLFragment(mixed())
	if !strings.Contains(out, "text-align:right;vertical-align:top;font-weight:600\">id</th>") {
		t.Error("the id header should right-align with its numbers")
	}

	// a column of only NULLs is not evidence of anything
	allNull := &model.Result{Columns: []string{"x"}, Rows: [][]string{{"NULL"}}, Raw: [][]any{{nil}}}
	if NumericColumns(allNull)[0] {
		t.Error("an all-NULL column should stay left-aligned")
	}
}

func TestClipContent(t *testing.T) {
	r := mixed()
	html, err := ClipContent(r, HTML)
	if err != nil {
		t.Fatal(err)
	}
	if html.HTML == "" || html.HTML != HTMLFragment(r) {
		t.Error("an HTML copy must carry the fragment as its rich flavor")
	}
	// the plain flavor is what shows when the rich one is lost: a readable
	// table, never the markup
	if html.Text != TextTable(r) || strings.Contains(html.Text, "<td") {
		t.Errorf("an HTML copy's plain flavor should be the aligned table, got:\n%s", html.Text)
	}
	for _, f := range []Format{CSV, Markdown, TSV, JSON, Text} {
		c, err := ClipContent(r, f)
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		want, _ := Render(r, f)
		if c.HTML != "" || c.Text != want {
			t.Errorf("%s: a plain format is its rendering and nothing else", f)
		}
	}
	if _, err := ClipContent(nil, CSV); err == nil {
		t.Error("nothing to copy should be an error")
	}
}
