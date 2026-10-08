package script

import (
	"strings"
	"testing"
)

// runBody wraps a Run body in the script header, so a case reads as the
// lines it is about. Its first body line is line 6.
func runBody(body string) string {
	return header + "func Run(s *sdb.S) error {\n" + body + "\treturn nil\n}\n"
}

// cut takes the ▮ out of src and says where it was: the caret.
func cut(t *testing.T, src string) (string, int) {
	t.Helper()
	i := strings.Index(src, "▮")
	if i < 0 {
		t.Fatalf("no ▮ in %q", src)
	}
	return src[:i] + src[i+len("▮"):], i
}

func TestRename(t *testing.T) {
	for _, c := range []struct {
		name, src, to, want string
	}{
		{"a shadowed err: the outer one's own uses only",
			runBody("\t_, ▮err := s.Query(\"dev\", \"x\")\n\tif err := s.Exec(\"dev\", \"y\"); err != nil {\n\t\treturn err\n\t}\n\ts.Print(err)\n"),
			"qerr",
			runBody("\t_, qerr := s.Query(\"dev\", \"x\")\n\tif err := s.Exec(\"dev\", \"y\"); err != nil {\n\t\treturn err\n\t}\n\ts.Print(qerr)\n")},
		{"the shadowing err, from a use",
			runBody("\t_, err := s.Query(\"dev\", \"x\")\n\tif err := s.Exec(\"dev\", \"y\"); ▮err != nil {\n\t\treturn err\n\t}\n\ts.Print(err)\n"),
			"xerr",
			runBody("\t_, err := s.Query(\"dev\", \"x\")\n\tif xerr := s.Exec(\"dev\", \"y\"); xerr != nil {\n\t\treturn xerr\n\t}\n\ts.Print(err)\n")},
		{"a field: declaration, selector and literal key",
			header + "type total struct{ ▮Sum int }\n\nfunc Run(s *sdb.S) error {\n\tt := total{Sum: 1}\n\tt.Sum++\n\treturn nil\n}\n",
			"Count",
			header + "type total struct{ Count int }\n\nfunc Run(s *sdb.S) error {\n\tt := total{Count: 1}\n\tt.Count++\n\treturn nil\n}\n"},
		{"a method",
			header + "type t struct{}\n\nfunc (t) ▮hi() string { return \"hi\" }\n\nfunc Run(s *sdb.S) error {\n\ts.Print(t{}.hi())\n\treturn nil\n}\n",
			"hello",
			header + "type t struct{}\n\nfunc (t) hello() string { return \"hi\" }\n\nfunc Run(s *sdb.S) error {\n\ts.Print(t{}.hello())\n\treturn nil\n}\n"},
		{"a type switch's x, every clause",
			runBody("\tvar v any = 1\n\tswitch x := v.(type) {\n\tcase int:\n\t\ts.Print(▮x)\n\tcase string:\n\t\ts.Print(x)\n\t}\n"),
			"val",
			runBody("\tvar v any = 1\n\tswitch val := v.(type) {\n\tcase int:\n\t\ts.Print(val)\n\tcase string:\n\t\ts.Print(val)\n\t}\n")},
		{"a label",
			runBody("▮outer:\n\tfor range 3 {\n\t\tbreak outer\n\t}\n"),
			"loop",
			runBody("loop:\n\tfor range 3 {\n\t\tbreak loop\n\t}\n")},
		{"an unnamed import gets a name",
			runBody("\ts.Print(▮sdb.Version)\n"),
			"db",
			"package main\n\nimport db \"github.com/rohanthewiz/dbc/sdb\"\n\nfunc Run(s *db.S) error {\n\ts.Print(db.Version)\n\treturn nil\n}\n"},
		{"a named import keeps its path",
			"package main\n\nimport (\n\tf \"fmt\"\n\n\t\"github.com/rohanthewiz/dbc/sdb\"\n)\n\nfunc Run(s *sdb.S) error {\n\ts.Print(▮f.Sprint(1))\n\treturn nil\n}\n",
			"fm",
			"package main\n\nimport (\n\tfm \"fmt\"\n\n\t\"github.com/rohanthewiz/dbc/sdb\"\n)\n\nfunc Run(s *sdb.S) error {\n\ts.Print(fm.Sprint(1))\n\treturn nil\n}\n"},
		{"the caret just past the name, the new name trimmed",
			runBody("\tn▮ := 1\n\ts.Print(n)\n"),
			"  count ",
			runBody("\tcount := 1\n\ts.Print(count)\n")},
		// a name declared in an outer block only, never used under the
		// renamed one, may be shadowed by it
		{"shadowing an outer name nothing below uses",
			runBody("\ti := 0\n\ts.Print(i)\n\tif true {\n\t\t▮n := 1\n\t\ts.Print(n)\n\t}\n"),
			"i",
			runBody("\ti := 0\n\ts.Print(i)\n\tif true {\n\t\ti := 1\n\t\ts.Print(i)\n\t}\n")},
		// a local named like an imported member's key stays apart from it
		{"beside an imported type's literal key",
			runBody("\t▮n := 1\n\ts.Copy(sdb.CopyOpts{Batch: n})\n"),
			"Batch",
			runBody("\tBatch := 1\n\ts.Copy(sdb.CopyOpts{Batch: Batch})\n")},
	} {
		t.Run(c.name, func(t *testing.T) {
			src, caret := cut(t, c.src)
			edits, err := Rename(src, caret, c.to)
			if err != nil {
				t.Fatalf("Rename: %v", err)
			}
			if got := applyEdits(src, edits); got != c.want {
				t.Errorf("got\n%s\nwant\n%s", got, c.want)
			}
		})
	}
}

// The name it already has is nothing to do, not an error.
// A well-known method name with another signature, or on an interface,
// satisfies nothing outside the script: it renames like any method.
func TestRenameKeptMethodNot(t *testing.T) {
	for _, c := range []struct{ name, src, to string }{
		{"String with a parameter",
			header + "type t int\n\nfunc (t) ▮String(w int) string { return \"\" }\n\nfunc Run(s *sdb.S) error {\n\ts.Print(t(1).String(2))\n\treturn nil\n}\n",
			"Pad"},
		{"Len returning int64",
			header + "type t []int\n\nfunc (x t) ▮Len() int64 { return 0 }\n\nfunc Run(s *sdb.S) error {\n\treturn nil\n}\n",
			"Size"},
		{"String with the script's own string type",
			header + "type string int\ntype t int\n\nfunc (t) ▮String() string { return 0 }\n\nfunc Run(s *sdb.S) error {\n\treturn nil\n}\n",
			"Text"},
		{"an interface's own String",
			header + "type named interface{ ▮String() string }\n\nfunc Run(s *sdb.S) error {\n\tvar n named\n\t_ = n.String()\n\treturn nil\n}\n",
			"Name"},
		{"a method renamed to String with another signature",
			header + "type t int\n\nfunc (t) ▮str() int { return 0 }\n\nfunc Run(s *sdb.S) error {\n\ts.Print(t(1).str())\n\treturn nil\n}\n",
			"String"},
	} {
		t.Run(c.name, func(t *testing.T) {
			src, caret := cut(t, c.src)
			if sym := Resolve(src, caret); sym.Fixed != "" {
				t.Fatalf("fixed = %q, want none", sym.Fixed)
			}
			if edits, err := Rename(src, caret, c.to); err != nil || len(edits) == 0 {
				t.Errorf("Rename = %v, %v; want edits", edits, err)
			}
		})
	}
}

func TestRenameSameName(t *testing.T) {
	src, caret := cut(t, runBody("\t▮n := 1\n\ts.Print(n)\n"))
	if edits, err := Rename(src, caret, "n"); err != nil || len(edits) != 0 {
		t.Errorf("Rename to n = %v, %v; want no edits", edits, err)
	}
}

func TestRenameRefused(t *testing.T) {
	for _, c := range []struct {
		name, src, to, want string
	}{
		{"nothing there", runBody("\t▮return nil\n"), "x", "nothing to rename here"},
		{"an imported member", runBody("\ts.▮Query(\"dev\", \"x\")\n"), "Ask", "Query belongs to an imported package"},
		{"a builtin", runBody("\ts.Print(▮len(\"x\"))\n"), "size", "len is predeclared by Go"},
		{"Run", header + "func ▮Run(s *sdb.S) error {\n\treturn nil\n}\n", "Main", "Run is the script's entry point"},
		{"an embedded field",
			header + "type base struct{ N int }\ntype t struct{ ▮base }\n\nfunc Run(s *sdb.S) error {\n\treturn nil\n}\n",
			"core", "base is an embedded field"},
		{"an embedded type",
			header + "type ▮base struct{ N int }\ntype t struct{ base }\n\nfunc Run(s *sdb.S) error {\n\treturn nil\n}\n",
			"core", "base is embedded in a struct on line 6"},

		{"empty", runBody("\t▮n := 1\n\ts.Print(n)\n"), " ", "the new name is empty"},
		{"blank", runBody("\t▮n := 1\n\ts.Print(n)\n"), "_", "_ is the blank identifier"},
		{"a keyword", runBody("\t▮n := 1\n\ts.Print(n)\n"), "range", "range is a Go keyword"},
		{"not a name", runBody("\t▮n := 1\n\ts.Print(n)\n"), "n-1", `"n-1" is not a Go name`},

		// Scope.Lookup at the declaration
		{"taken in the same scope", runBody("\t▮n, m := 1, 2\n\ts.Print(n, m)\n"), "m",
			"m is already declared in this scope, on line 6"},
		{"a label taken", runBody("a:\n\tfor range 1 {\n\t\tbreak a\n\t}\n▮b:\n\tfor range 1 {\n\t\tbreak b\n\t}\n"), "a",
			"a is already declared in this scope, on line 6"},
		{"a package-level name an import has",
			header + "func ▮helper() {}\n\nfunc Run(s *sdb.S) error {\n\thelper()\n\treturn nil\n}\n", "sdb",
			"sdb is already the name of an import, on line 3"},
		{"an import a package-level name has",
			header + "func helper() {}\n\nfunc Run(s *▮sdb.S) error {\n\thelper()\n\treturn nil\n}\n", "helper",
			"helper is already declared in the script, on line 5"},
		// LookupParent at a use: a block between it and the declaration
		// declares the new name
		{"a use the new name would capture",
			runBody("\t▮n := 1\n\tfor i := 0; i < 3; i++ {\n\t\ts.Print(n)\n\t}\n"), "i",
			"the n on line 8 would then mean the i declared on line 7"},

		// the recheck: what no scope check sees
		{"an outer name's use the renamed one would capture",
			runBody("\tx := 1\n\tif true {\n\t\t▮y := 2\n\t\ts.Print(x, y)\n\t}\n"), "x",
			"renaming y to x would change what x on line 9 means: the one declared on line 8, not the one declared on line 6"},
		{"a field taken", header + "type p struct{ ▮A, B int }\n\nfunc Run(s *sdb.S) error {\n\treturn nil\n}\n", "B",
			"renaming A to B would not compile: B redeclared"},
		{"a method named like a field",
			header + "type t struct{ N int }\n\nfunc (t) ▮m() {}\n\nfunc Run(s *sdb.S) error {\n\treturn nil\n}\n", "N",
			"renaming m to N would not compile: field and method with the same name N"},
		{"a builtin's use captured",
			runBody("\t▮n := \"abc\"\n\ts.Print(len(n))\n"), "len",
			"renaming n to len would change what len on line 7 means"},

		// methods code outside the script calls by name (rename_iface.go)
		{"String() away from fmt.Stringer",
			header + "type cents int\n\nfunc (c cents) ▮String() string { return \"$\" }\n\nfunc Run(s *sdb.S) error {\n\ts.Print(\"%v\", cents(1))\n\treturn nil\n}\n",
			"Text", "String() string satisfies fmt.Stringer"},
		{"Error() away, never used as an error",
			header + "type oops struct{}\n\nfunc (*oops) ▮Error() string { return \"oops\" }\n\nfunc Run(s *sdb.S) error {\n\treturn nil\n}\n",
			"Msg", "Error() string satisfies error"},
		{"Format under a renamed fmt import",
			"package main\n\nimport (\n\tf \"fmt\"\n\n\t\"github.com/rohanthewiz/dbc/sdb\"\n)\n\ntype t int\n\nfunc (t) ▮Format(st f.State, verb int32) {}\n\nfunc Run(s *sdb.S) error {\n\treturn nil\n}\n",
			"F", "Format(fmt.State, rune) satisfies fmt.Formatter"},
		{"Value for driver.Valuer",
			"package main\n\nimport (\n\t\"database/sql/driver\"\n\n\t\"github.com/rohanthewiz/dbc/sdb\"\n)\n\ntype t int\n\nfunc (t) ▮Value() (driver.Value, error) { return nil, nil }\n\nfunc Run(s *sdb.S) error {\n\treturn nil\n}\n",
			"V", "satisfies driver.Valuer"},
		{"Less, its parameters grouped",
			header + "type by []int\n\nfunc (b by) ▮Less(i, j int) bool { return b[i] < b[j] }\n\nfunc Run(s *sdb.S) error {\n\treturn nil\n}\n",
			"Lt", "Less(int, int) bool satisfies sort.Interface"},
		{"a method renamed to String() string",
			header + "type cents int\n\nfunc (c cents) ▮str() string { return \"$\" }\n\nfunc Run(s *sdb.S) error {\n\ts.Print(cents(1).str())\n\treturn nil\n}\n",
			"String", "as String() string, str would satisfy fmt.Stringer"},
	} {
		t.Run(c.name, func(t *testing.T) {
			src, caret := cut(t, c.src)
			_, err := Rename(src, caret, c.to)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %v, want %q", err, c.want)
			}
		})
	}
}

// Resolve marks what Rename would refuse outright, so the rename box can
// say so instead of opening.
func TestResolveFixed(t *testing.T) {
	for mark, want := range map[string]string{
		"s.Query(\"dev\"": "imported package", "len(": "predeclared", "Run(": "entry point",
	} {
		at := caretAt(t, symbolSrc, mark, 1, 0)
		if mark == "s.Query(\"dev\"" {
			at += 2
		}
		if sym := Resolve(symbolSrc, at); !strings.Contains(sym.Fixed, want) {
			t.Errorf("%s: fixed = %q, want %q", mark, sym.Fixed, want)
		}
	}
	if sym := Resolve(symbolSrc, caretAt(t, symbolSrc, "add(&t", 1, 0)); sym.Fixed != "" {
		t.Errorf("a helper func: fixed = %q, want none", sym.Fixed)
	}
}

func TestBackMapper(t *testing.T) {
	// "ab cd ef": cd → wxyz, and an insertion before ef
	edits := []Edit{{From: 6, To: 6, Text: "q "}, {From: 3, To: 5, Text: "wxyz"}}
	src := "ab cd ef"
	got := applyEdits(src, edits)
	if got != "ab wxyz q ef" {
		t.Fatalf("applyEdits = %q", got)
	}
	back := backMapper(edits)
	for newOff, want := range map[int]int{0: 0, 3: 3, 6: 3, 7: 5, 8: 6, 9: 6, 10: 6, 11: 7} {
		if got := back(newOff); got != want {
			t.Errorf("back(%d) = %d, want %d", newOff, got, want)
		}
	}
}
