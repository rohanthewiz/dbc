package pipeline

import (
	"strings"
	"testing"
)

// Locate and ParseErrorAt, moved here from dbc web so the TUI can place
// findings too: a node's field, an edge, a param, and the three kinds of
// parse error.
func TestLocate(t *testing.T) {
	text := `{
  "name": "orders",
  "params": { "days": { "default": "" } },
  "fragments": [
    {
      "name": "load",
      "nodes": [
        { "id": "src", "plugin": "sql.read", "cfg": { "conn": "pg", "query": "SELECT 1" } },
        { "id": "dst", "plugin": "sql.write", "cfg": { "conn": "pg", "table": "t" } }
      ],
      "edges": [ ["src", "dst"] ]
    }
  ]
}
`
	lines := strings.Split(text, "\n")
	for _, c := range []struct{ where, at string }{
		{"name", `"name": "orders"`},
		{"params.days", `"days"`},
		{"load", `"name": "load"`},
		{"load/dst", `"id": "dst"`},
		{"load/dst.table", `"table"`},
		{"load:edge src→dst", `["src", "dst"]`},
	} {
		line, col := Locate(text, c.where)
		if line == 0 || !strings.HasPrefix(lines[line-1][col-1:], c.at) {
			t.Errorf("Locate(%q) = %d:%d, want at %s", c.where, line, col, c.at)
		}
	}

	for _, c := range []struct {
		text string
		want [2]int
	}{
		// syntax: just past the bad character (json's Offset counts the
		// bytes read, the bad one included)
		{"{\n  \"name\": \"x\",\n  oops\n}", [2]int{3, 4}},
		{`{"name": 7}`, [2]int{1, 11}},                              // type: after the value
		{"{\n  \"name\": \"x\",\n  \"colour\": 1\n}", [2]int{3, 3}}, // unknown key: where it is written
	} {
		_, err := Parse(c.text)
		if err == nil {
			t.Fatalf("%q parsed", c.text)
		}
		if line, col := ParseErrorAt(c.text, err); line != c.want[0] || col != c.want[1] {
			t.Errorf("ParseErrorAt(%q) = %d:%d, want %d:%d (%v)", c.text, line, col, c.want[0], c.want[1], err)
		}
	}
}
