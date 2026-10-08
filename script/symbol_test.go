package script

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// symbolSrc exercises everything Resolve tells apart: a shadowed err, a
// helper func, a script-declared type's field reached by a selector and by
// a composite-literal key, a type switch, a label, sdb members, a builtin.
const symbolSrc = `package main

import (
	"fmt"

	"github.com/rohanthewiz/dbc/sdb"
)

type total struct {
	Sum int
}

func add(t *total, n int) { t.Sum += n }

func Run(s *sdb.S) error {
	res, err := s.Query("dev", "SELECT 1")
	if err != nil {
		return err
	}
	t := total{Sum: 0}
	for i := 0; i < len(res.Rows); i++ {
		add(&t, i)
	}
	if err := s.Exec("dev", "SELECT 2"); err != nil {
		return fmt.Errorf("exec: %w", err)
	}
	var v any = t.Sum
	switch x := v.(type) {
	case int:
		s.Print(x)
	case string:
		s.Print(x + "!")
	}
outer:
	for range 3 {
		break outer
	}
	_, _ = s.Query("prod", "SELECT 3")
	return nil
}
`

// caretAt is the byte offset of the nth (1-based) occurrence of mark in
// src, plus delta — a caret placed by the text around it.
func caretAt(t *testing.T, src, mark string, nth, delta int) int {
	t.Helper()
	from := 0
	for range nth {
		i := strings.Index(src[from:], mark)
		if i < 0 {
			t.Fatalf("%q: occurrence %d not in src", mark, nth)
		}
		from += i + 1
	}
	return from - 1 + delta
}

// lineCol renders a span as "line:col" (1-based), the form the cases below
// expect, so a failure reads like a compiler position.
func lineCol(src string, sp Span) string {
	line := strings.Count(src[:sp.From], "\n") + 1
	col := sp.From - strings.LastIndex(src[:sp.From], "\n")
	return fmt.Sprintf("%d:%d", line, col)
}

func TestResolve(t *testing.T) {
	for _, c := range []struct {
		name       string
		mark       string // the caret is at this text…
		nth, delta int    // …its nth occurrence, plus delta bytes
		kind, def  string // def "" means none in the script
		uses       []string
		symbolName string
	}{
		// the outer err: its := and its two uses, not the if's own err
		{"outer err", "err != nil", 1, 0, "var", "16:7", []string{"16:7", "17:5", "18:10"}, "err"},
		// the shadowing err in the if: its own three, none of the outer's
		{"shadowing err", "err := s.Exec", 1, 0, "var", "24:5", []string{"24:5", "24:39", "25:33"}, "err"},
		// a call jumps to the func, and the func lists its call
		{"helper func", "add(&t", 1, 0, "func", "13:6", []string{"13:6", "22:3"}, "add"},
		// a field by selector, by key, and declared in the struct
		{"field via selector", "t.Sum +=", 1, 2, "field", "10:2", []string{"10:2", "13:31", "20:13", "27:16"}, "Sum"},
		{"field via literal key", "Sum: 0", 1, 0, "field", "10:2", []string{"10:2", "13:31", "20:13", "27:16"}, "Sum"},
		{"type", "*total", 1, 1, "type", "9:6", []string{"9:6", "13:13", "20:7"}, "total"},
		// a type switch's x: every clause's x is the switch's
		{"type switch var in a clause", "x + \"!\"", 1, 0, "var", "28:9", []string{"28:9", "30:11", "32:11"}, "x"},
		{"type switch var at the switch", "x := v", 1, 0, "var", "28:9", []string{"28:9", "30:11", "32:11"}, "x"},
		{"label", "break outer", 1, 6, "label", "34:1", []string{"34:1", "36:9"}, "outer"},
		// the caret just past a name is on it
		{"caret after the name", "res.Rows", 1, 3, "var", "16:2", []string{"16:2", "21:22"}, "res"},
		// an sdb member: no declaration here, uses by the same s
		{"sdb method", "s.Query(\"dev\"", 1, 2, "member", "", []string{"16:16", "38:11"}, "Query"},
		// a package: declared by its path literal (an unnamed import)
		{"package", "sdb.S", 1, 0, "package", "6:2", []string{"6:2", "15:13"}, "sdb"},
		{"builtin", "len(", 1, 0, "builtin", "", []string{"21:18"}, "len"},
	} {
		t.Run(c.name, func(t *testing.T) {
			sym := Resolve(symbolSrc, caretAt(t, symbolSrc, c.mark, c.nth, c.delta))
			if sym.Kind != c.kind || sym.Name != c.symbolName {
				t.Fatalf("kind, name = %q, %q; want %q, %q", sym.Kind, sym.Name, c.kind, c.symbolName)
			}
			def := ""
			if sym.Def != nil {
				def = lineCol(symbolSrc, *sym.Def)
			}
			if def != c.def {
				t.Errorf("def = %q, want %q", def, c.def)
			}
			var uses []string
			for _, u := range sym.Uses {
				uses = append(uses, lineCol(symbolSrc, u))
			}
			if !slices.Equal(uses, c.uses) {
				t.Errorf("uses = %v, want %v", uses, c.uses)
			}
		})
	}
}

func TestResolveNothing(t *testing.T) {
	for _, c := range []struct{ name, src, mark string }{
		{"keyword", symbolSrc, "return nil"},
		{"string", symbolSrc, "SELECT 1"},
		{"blank", symbolSrc, "_, _"},
		{"empty", "", ""},
		{"not go", "SELECT * FROM cats", "cats"},
	} {
		t.Run(c.name, func(t *testing.T) {
			at := 0
			if c.mark != "" {
				at = caretAt(t, c.src, c.mark, 1, 1)
			}
			if sym := Resolve(c.src, at); sym.Kind != "" || sym.Uses == nil {
				t.Errorf("Resolve = %+v, want no kind and an empty (non-nil) uses", sym)
			}
		})
	}
}

// A script with a syntax error still resolves what the parser could read:
// F12 must not go dead the moment a brace is missing.
func TestResolveBrokenScript(t *testing.T) {
	src := header + "func Run(s *sdb.S) error {\n\tn := 1\n\ts.Print(n,\n\treturn nil\n"
	sym := Resolve(src, caretAt(t, src, "n,", 1, 0))
	if sym.Kind != "var" || sym.Def == nil || lineCol(src, *sym.Def) != "6:2" {
		t.Fatalf("Resolve = %+v, want var n declared at 6:2", sym)
	}
}

func TestPkgName(t *testing.T) {
	for p, want := range map[string]string{
		"fmt": "fmt", "math/rand": "rand", "github.com/rohanthewiz/dbc/sdb": "sdb",
		"github.com/x/y/v2": "y", "gopkg.in/yaml.v3": "yaml", "v2": "v2",
	} {
		if got := pkgName(p); got != want {
			t.Errorf("pkgName(%q) = %q, want %q", p, got, want)
		}
	}
}
