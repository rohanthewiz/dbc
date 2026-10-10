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

// Locate matches a where's names against the ones the text declares: a
// dotted node id is found whole (N-197), alone in its fragment or beside a
// node whose id is its first part; an invalid id or fragment name, which
// Check names by its place, is found at that place, duplicates included;
// and a fragment called like the pipeline is found in "fragments", not on
// the pipeline's own "name".
func TestLocateNames(t *testing.T) {
	text := `{
  "name": "w",
  "fragments": [
    {
      "name": "w",
      "nodes": [
        { "id": "my", "plugin": "preview", "cfg": { "query": "my's" } },
        { "id": "my.src", "plugin": "sql.read", "cfg": { "conn": "lite", "query": "select ${nope}" } },
        { "id": "a b", "plugin": "sql.read", "cfg": { "conn": "lite", "query": "x" } },
        { "id": "a b", "plugin": "sql.read", "cfg": { "conn": "lite", "query": "y" } }
      ]
    },
    {
      "name": "",
      "nodes": [ { "id": "src", "plugin": "sql.read", "cfg": { "conn": "lite", "query": "z" } } ],
      "edges": [ ["src", "gone"] ]
    },
    {
      "name": "solo.f",
      "nodes": [ { "id": "my.src", "plugin": "sql.read", "cfg": { "conn": "lite", "query": "alone" } } ]
    }
  ]
}
`
	lines := strings.Split(text, "\n")
	for _, c := range []struct{ where, line, at string }{
		{"name", `"name": "w",`, `"name"`},
		{"w", `      "name": "w",`, `"name": "w"`},
		{"w/my", `"query": "my's"`, `"id": "my"`},
		{"w/my.query", `"query": "my's"`, `"query"`},
		{"w/my.src", `${nope}`, `"id": "my.src"`},
		{"w/my.src.query", `${nope}`, `"query"`},
		{"w/nodes[2]", `"query": "x"`, `"id": "a b"`},
		{"w/nodes[3].query", `"query": "y"`, `"query"`},
		{"fragments[1]", `"name": ""`, `"name": ""`},
		{"fragments[1]/src.query", `"query": "z"`, `"query"`},
		{"fragments[1]:edge src→gone", `["src", "gone"]`, `["src", "gone"]`},
		{"solo.f", `"name": "solo.f"`, `"name"`},
		{"solo.f/my.src.query", `"query": "alone"`, `"query"`},
	} {
		line, col := Locate(text, c.where)
		if line == 0 || !strings.Contains(lines[line-1], c.line) || !strings.HasPrefix(lines[line-1][col-1:], c.at) {
			got := ""
			if line > 0 {
				got = lines[line-1]
			}
			t.Errorf("Locate(%q) = %d:%d (%q), want at %s on the line with %s", c.where, line, col, got, c.at, c.line)
		}
	}
	// a where naming nothing the text has is placed as near as it can be:
	// an unknown node at its fragment, an index past the end not at all
	if line, _ := Locate(text, "w/gone.query"); !strings.Contains(lines[line-1], `"name": "w"`) || line == 2 {
		t.Errorf("Locate(w/gone.query) = line %d, want the fragment's name", line)
	}
	if line, col := Locate(text, "fragments[9]"); line != 0 || col != 0 {
		t.Errorf("Locate(fragments[9]) = %d:%d, want no place", line, col)
	}
}
