package script

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// header is the top of a well-formed script, for the cases below to append to.
const header = "package main\n\nimport \"github.com/rohanthewiz/dbc/sdb\"\n\n"

func TestCheck(t *testing.T) {
	for _, c := range []struct {
		name, src string
		want      []string // Diag.String() of each, in order; nil is clean
	}{
		{"clean", header + "func Run(s *sdb.S) error {\n\ts.Print(\"hi\")\n\treturn nil\n}\n", nil},
		{"named import and an unnamed parameter",
			"package main\n\nimport db \"github.com/rohanthewiz/dbc/sdb\"\n\nfunc Run(*db.S) error { return nil }\n", nil},
		{"dot import",
			"package main\n\nimport . \"github.com/rohanthewiz/dbc/sdb\"\n\nfunc Run(s *S) error { return nil }\n", nil},
		{"build tag and comment before package",
			"// Does a thing.\n//go:build ignore\n\n" + header + "func Run(s *sdb.S) error { return nil }\n", nil},

		// 1. syntax: go/parser's exact positions, and nothing else reported
		{"syntax error", header + "func Run(s *sdb.S) error {\n\treturn nil\n", []string{"6:13: expected '}', found 'EOF'"}},

		// 2. signature
		{"not package main", "package scripts\n\nfunc Run() error { return nil }\n",
			[]string{"1:9: package scripts: a script is package main"}},
		{"no Run", header + "func Main(s *sdb.S) error { return nil }\n",
			[]string{"1:1: no func Run(s *sdb.S) error: it is what dbc calls to run the script"}},
		{"Run takes the wrong type", header + "func Run(s sdb.S) error { return nil }\n",
			[]string{"5:6: Run must be func Run(s *sdb.S) error, not func Run(s sdb.S) error"}},
		{"Run returns nothing", header + "func Run(s *sdb.S) {}\n",
			[]string{"5:6: Run must be func Run(s *sdb.S) error, not func Run(s *sdb.S)"}},
		{"Run as a method does not count", header + "type T struct{}\n\nfunc (T) Run(s *sdb.S) error { return nil }\n",
			[]string{"1:1: no func Run(s *sdb.S) error: it is what dbc calls to run the script"}},

		// 3. the yaegi#1655 lint: a warning, and the script still compiles
		{"map comma-ok", header + "func Run(s *sdb.S) error {\n\tm := map[string]string{}\n\tvar v any = \"x\"\n" +
			"\tm[\"k\"], _ = v.(string)\n\tok := false\n\tm[\"j\"], ok = m[\"k\"]\n\t_ = ok\n\treturn nil\n}\n",
			[]string{
				"8:2: warning: the script interpreter drops a two-value assignment into a map element (yaegi#1655): assign to a variable, then m[k] = v. (A slice element is fine.)",
				"10:2: warning: the script interpreter drops a two-value assignment into a map element (yaegi#1655): assign to a variable, then m[k] = v. (A slice element is fine.)",
			}},
		{"a variable on the left is fine", header + "func Run(s *sdb.S) error {\n\tvar v any = 1\n\tn, ok := v.(int)\n\t_, _ = n, ok\n\treturn nil\n}\n", nil},

		// 4. yaegi's compile pass, with its position
		{"undefined method", header + "func Run(s *sdb.S) error {\n\ts.Nope()\n\treturn nil\n}\n",
			[]string{"6:2: undefined selector: Nope"}},
		{"type mismatch", header + "func Run(s *sdb.S) error {\n\tvar n int = \"x\"\n\t_ = n\n\treturn nil\n}\n",
			[]string{"6:14: cannot convert \"x\" to int"}},
		{"import a script cannot have", "package main\n\nimport (\n\t\"github.com/foo/bar\"\n\n\t\"github.com/rohanthewiz/dbc/sdb\"\n)\n\n" +
			"func Run(s *sdb.S) error { return bar.Err }\n",
			[]string{`4:2: cannot import "github.com/foo/bar": a script can import the standard library and github.com/rohanthewiz/dbc/sdb`}},
		{"a signature error and a compile error are both reported", header + "func Run(s *sdb.S) {\n\ts.Nope()\n}\n",
			[]string{"5:6: Run must be func Run(s *sdb.S) error, not func Run(s *sdb.S)", "6:2: undefined selector: Nope"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			var got []string
			for _, d := range Check("s.go", c.src) {
				got = append(got, d.String())
			}
			if strings.Join(got, "\n") != strings.Join(c.want, "\n") {
				t.Errorf("Check\n got %q\nwant %q", got, c.want)
			}
		})
	}
}

// Check must never run any of the script: not Run, not init(), not a
// package-level initializer. Each of these would leave a file behind.
func TestCheckNeverExecutes(t *testing.T) {
	dir := t.TempDir()
	mark := func(n string) string { return strings.ReplaceAll(filepath.Join(dir, n), `\`, `\\`) }
	src := "package main\n\nimport (\n\t\"os\"\n\n\t\"github.com/rohanthewiz/dbc/sdb\"\n)\n\n" +
		"var _ = os.WriteFile(\"" + mark("var") + "\", nil, 0o644)\n\n" +
		"func init() { os.WriteFile(\"" + mark("init") + "\", nil, 0o644) }\n\n" +
		"func main() { os.WriteFile(\"" + mark("main") + "\", nil, 0o644) }\n\n" +
		"func Run(s *sdb.S) error { return os.WriteFile(\"" + mark("run") + "\", nil, 0o644) }\n"
	if d := Check("s.go", src); d != nil {
		t.Fatalf("Check = %v, want clean", d)
	}
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		t.Errorf("Check ran the script: %s was written", e.Name())
	}
}

// A check runs on idle in the editor, so it has to be quick: a fresh
// interpreter (the stdlib symbol load) and a compile.
func TestCheckIsQuick(t *testing.T) {
	src := header + "func Run(s *sdb.S) error {\n\tr, err := s.Query(\"a\", \"SELECT 1\")\n\tif err != nil {\n\t\treturn err\n\t}\n\ts.Show(r)\n\treturn nil\n}\n"
	Check("s.go", src) // the first pays for one-time package init
	const n = 5
	start := time.Now()
	for range n {
		if d := Check("s.go", src); d != nil {
			t.Fatal(d)
		}
	}
	per := time.Since(start) / n
	t.Logf("Check: %v per call", per)
	if per > 500*time.Millisecond { // generous: a loaded CI box, -race
		t.Errorf("Check took %v per call", per)
	}
}

func TestYaegiDiag(t *testing.T) {
	for msg, want := range map[string]string{
		"6:2: undefined selector: Nope": "6:2: undefined selector: Nope",
		"_.go:3:4: undefined: x":        "3:4: undefined: x",
		"/a/b.go:3:4: multi\nline":      "3:4: multi\nline",
		"something without a position":  "1:0: something without a position",
		`2:8: import "x/y" error: unable to find source related to: "x/y". Either the GOPATH…`: `2:8: cannot import "x/y": a script can import the standard library and github.com/rohanthewiz/dbc/sdb`,
	} {
		if got := yaegiDiag(msg).String(); got != want {
			t.Errorf("yaegiDiag(%q) = %q, want %q", msg, got, want)
		}
	}
}
