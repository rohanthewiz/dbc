package sqlsplit

import (
	"fmt"
	"strings"
	"testing"
)

// foldLines renders folds as "header-last" line pairs (1-based), the way
// an editor sees them, so a case reads against its SQL rather than offsets.
func foldLines(sql string, folds []Fold) string {
	line := func(i int) int { return strings.Count(sql[:i], "\n") + 1 }
	var out []string
	for _, f := range folds {
		out = append(out, fmt.Sprintf("%d-%d", line(f.Start), line(f.End)))
	}
	return strings.Join(out, " ")
}

func TestFolds(t *testing.T) {
	cases := []struct {
		name string
		sql  string
		want string
	}{
		{"empty", "", ""},
		{"one-line statements fold nothing", "SELECT 1;\nSELECT 2;", ""},
		{"a statement over lines", "SELECT a,\n  b\nFROM t;\nSELECT 1;", "1-3"},
		{"an unterminated last statement", "SELECT 1;\nSELECT a\nFROM t", "2-3"},
		// the header comments stay above the fold, as its label
		{"heading comments stay out", "-- totals\n-- by month\nSELECT a\nFROM t;", "3-4"},
		// the ")" on its own line stays visible: the CTE folds to "AS (⋯"
		{"a CTE body", "WITH r AS (\n  SELECT 1\n  FROM t\n)\nSELECT * FROM r;", "1-5 1-3"},
		{"a ) after code folds with its line", "SELECT * FROM (\n  SELECT 1\n  FROM t) x;", "1-3 1-3"},
		{"nested blocks", "SELECT * FROM (\n  SELECT * FROM (\n    SELECT 1\n  ) a\n) b;",
			"1-5 2-3 1-4"},
		{"a one-line block inside folds only its statement", "SELECT count(*)\nFROM t;", "1-2"},
		{"an empty block over two lines hides nothing", "SELECT f(\n)\n;", "1-2"},
		// brackets in strings, quoted names, comments and dollar bodies
		// are text: none of them opens a block
		{"brackets in opaque spans", "SELECT '(',\n  \"a(\", -- (\n  $$ ( $$\nFROM t;", "1-4"},
		// parens are matched per statement: one left open while typing
		// does not reach into the next
		{"an unclosed ( stays in its statement", "SELECT (\n1;\nSELECT 2\nFROM (\n  t\n);",
			"1-2 3-6 4-5"},
		{"an unmatched ) is skipped", "SELECT 1)\nFROM t;", "1-2"},
		{"CRLF line breaks", "WITH r AS (\r\n  SELECT 1\r\n)\r\nSELECT * FROM r;", "1-4 1-2"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := foldLines(c.sql, Folds(c.sql)); got != c.want {
				t.Errorf("folds = %q, want %q", got, c.want)
			}
		})
	}
}
