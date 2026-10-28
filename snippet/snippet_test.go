package snippet

import (
	"reflect"
	"strings"
	"testing"
)

func mustParse(t *testing.T, body string) Template {
	t.Helper()
	tp, err := Parse(body)
	if err != nil {
		t.Fatalf("Parse(%q): %v", body, err)
	}
	return tp
}

// spanText lists what each stop's spans hold, so a test reads as text.
func spanText(x Expansion) map[int][]string {
	out := map[int][]string{}
	for _, st := range x.Stops {
		for _, sp := range st.Spans {
			out[st.N] = append(out[st.N], x.Text[sp.From:sp.To])
		}
	}
	return out
}

func TestExpandStopsMirrorsAndOrder(t *testing.T) {
	x := Expand(mustParse(t, "SELECT $VAR2:col$, count(*)\nFROM $VAR1$\nGROUP BY $VAR2$"), Context{})
	if want := "SELECT col, count(*)\nFROM \nGROUP BY col"; x.Text != want {
		t.Fatalf("text = %q, want %q", x.Text, want)
	}
	// visited by number, not by place: VAR1 (FROM) before VAR2
	if len(x.Stops) != 2 || x.Stops[0].N != 1 || x.Stops[1].N != 2 {
		t.Fatalf("stops = %+v", x.Stops)
	}
	if got := spanText(x); !reflect.DeepEqual(got, map[int][]string{1: {""}, 2: {"col", "col"}}) {
		t.Fatalf("spans = %v", got)
	}
	if x.Stops[0].Spans[0].From != strings.Index(x.Text, "FROM ")+5 {
		t.Fatalf("VAR1 at %d", x.Stops[0].Spans[0].From)
	}
	if x.End != len(x.Text) {
		t.Fatalf("end = %d, want the end (%d)", x.End, len(x.Text))
	}
}

func TestExpandEndAndDollars(t *testing.T) {
	x := Expand(mustParse(t, "SELECT $$1, $$$$, $1, $body$ $END$;"), Context{})
	if want := "SELECT $1, $$, $1, $body$ ;"; x.Text != want {
		t.Fatalf("text = %q, want %q", x.Text, want)
	}
	if x.End != strings.Index(x.Text, ";") {
		t.Fatalf("end = %d", x.End)
	}
	// "$VAR$" and "$VARIANT" are not stops: a dollar-quote tag may be named so
	x = Expand(mustParse(t, "$VAR$ $VARIANT"), Context{})
	if x.Text != "$VAR$ $VARIANT" || len(x.Stops) != 0 {
		t.Fatalf("got %q %+v", x.Text, x.Stops)
	}
	// a default holding a "$"
	x = Expand(mustParse(t, "$VAR1:$$1$"), Context{})
	if x.Text != "$1" {
		t.Fatalf("default with $$: %q", x.Text)
	}
}

func TestParseErrors(t *testing.T) {
	for body, want := range map[string]string{
		"$VAR1":            "not closed",
		"$VAR1 x":          "not closed",
		"$VAR1:abc":        "not closed",
		"$VAR0$":           "numbered from 1",
		"$END$ $END$":      "more than once",
		"$VAR1:a\nb$":      "one line",
		"$VAR1000$":        "numbered from 1",
		"x $VAR2:ok$ $VAR": "", // "$VAR" with no digit is text
	} {
		_, err := Parse(body)
		switch {
		case want == "" && err != nil:
			t.Errorf("Parse(%q): %v", body, err)
		case want != "" && (err == nil || !strings.Contains(err.Error(), want)):
			t.Errorf("Parse(%q) = %v, want an error with %q", body, err, want)
		}
	}
}

func TestExpandIndent(t *testing.T) {
	x := Expand(mustParse(t, "CASE\n    WHEN $VAR1$ THEN $VAR2$\nEND"), Context{Indent: "    "})
	if want := "CASE\n        WHEN  THEN \n    END"; x.Text != want {
		t.Fatalf("text = %q, want %q", x.Text, want)
	}
	if got := spanText(x); len(got[1]) != 1 || x.Stops[0].Spans[0].From != strings.Index(x.Text, "WHEN ")+5 {
		t.Fatalf("stop offsets off after indenting: %+v", x.Stops)
	}
}

func TestExpandSelection(t *testing.T) {
	body := "WITH $VAR1:q$ AS (\n    $SELECTION$\n)\nSELECT * FROM $VAR1$"
	tp := mustParse(t, body)
	if !tp.UsesSelection() {
		t.Fatal("UsesSelection = false")
	}

	// whole lines from the indentation, relative indent kept, the
	// trailing newline kept outside the snippet with End before it
	x := Expand(tp, Context{Selection: "    SELECT a\n      FROM t\n    WHERE x\n", SelPrefix: ""})
	want := "WITH q AS (\n    SELECT a\n      FROM t\n    WHERE x\n)\nSELECT * FROM q\n"
	if x.Text != want {
		t.Fatalf("text =\n%s\nwant\n%s", x.Text, want)
	}
	if x.End != len(x.Text)-1 {
		t.Fatalf("end = %d, want before the kept newline (%d)", x.End, len(x.Text)-1)
	}

	// started mid-line: the first line as it is, later lines by their own
	// common indent
	x = Expand(tp, Context{Selection: "SELECT a\n        FROM t", SelPrefix: "x = (    "})
	if want := "WITH q AS (\n    SELECT a\n    FROM t\n)\nSELECT * FROM q"; x.Text != want {
		t.Fatalf("mid-line: %q", x.Text)
	}

	// a first line indented past the rest keeps its extra indent
	x = Expand(mustParse(t, "($SELECTION$)"), Context{Selection: "      a\n    b", SelPrefix: ""})
	if x.Text != "(  a\nb)" {
		t.Fatalf("extra first-line indent: %q", x.Text)
	}

	// no selection: "" and a single line as it is
	if x = Expand(tp, Context{}); !strings.Contains(x.Text, "(\n    \n)") {
		t.Fatalf("empty selection: %q", x.Text)
	}
	if x = Expand(mustParse(t, "[$SELECTION$]"), Context{Selection: "  a b  "}); x.Text != "[  a b  ]" {
		t.Fatalf("single line: %q", x.Text)
	}
	// blank lines in a selection stay empty
	if x = Expand(mustParse(t, "  $SELECTION$"), Context{Selection: "a\n\nb"}); x.Text != "  a\n\n  b" {
		t.Fatalf("blank line: %q", x.Text)
	}
}

func TestMonaco(t *testing.T) {
	for _, c := range []struct{ body, want string }{
		{"SELECT $VAR2:*$ FROM $VAR1$ WHERE $VAR2$ > 0$END$;", "SELECT ${2:*} FROM ${1} WHERE ${2:*} > 0$0;"},
		{"$END$$VAR1:x$", "$0${1:x}"},
		{"cost: $$5 {a} \\n $VAR1:}$", `cost: \$5 {a\} \\n ${1:\}}$0`},
		{"plain", "plain$0"},
	} {
		if got := Expand(mustParse(t, c.body), Context{}).Monaco(); got != c.want {
			t.Errorf("Monaco(%q) = %q, want %q", c.body, got, c.want)
		}
	}
	// a selection's own "$" is escaped too
	x := Expand(mustParse(t, "f($SELECTION$)"), Context{Selection: "$1"})
	if got := x.Monaco(); got != `f(\$1)$0` {
		t.Errorf("selection: %q", got)
	}
}

func TestValidName(t *testing.T) {
	for _, ok := range []string{"sel", "cte_1", "X"} {
		if err := ValidName(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "a-b", "a.b", "a b", strings.Repeat("x", NameMax+1)} {
		if ValidName(bad) == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
