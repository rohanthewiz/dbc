package userdata

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestListScripts(t *testing.T) {
	if got, err := ListScripts(filepath.Join(t.TempDir(), "missing")); err != nil || got != nil {
		t.Fatalf("missing dir = %v, %v; want none and no error", got, err)
	}

	dir := t.TempDir()
	files := map[string]string{
		"b_copy.go":  "// Copy a table. Then more detail.\n//go:build ignore\n\npackage main\n",
		"a_plain.go": "//go:build ignore\n\npackage main\n\nfunc Run() {}\n",
		// a syntax error below the header still lists, with its description
		"c_broken.go": "// Half written.\npackage main\n\nfunc Run( {\n",
		"notes.txt":   "not a script",
		".hidden.go":  "// hidden\npackage main\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "dir.go"), 0o755); err != nil { // a directory named like a script
		t.Fatal(err)
	}

	got, err := ListScripts(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names, descs []string
	for _, in := range got {
		names = append(names, in.Name)
		descs = append(descs, in.Desc)
	}
	if want := "a_plain.go b_copy.go c_broken.go"; strings.Join(names, " ") != want {
		t.Errorf("names = %v, want %s", names, want)
	}
	if want := []string{"", "Copy a table.", "Half written."}; strings.Join(descs, "|") != strings.Join(want, "|") {
		t.Errorf("descs = %q, want %q", descs, want)
	}
}

func TestScriptDesc(t *testing.T) {
	long := strings.Repeat("word ", 40)
	for _, c := range []struct{ src, want string }{
		// the repo samples' shape: comment, then the build tag, then package
		{"// Sample dbc script: copy a table from one\n// connection to another. More.\n//go:build ignore\n\npackage main\n",
			"Sample dbc script: copy a table from one connection to another."},
		// a dot inside a word is not a sentence end
		{"// Uses sdb.S to copy v1.2 rows\npackage main\n", "Uses sdb.S to copy v1.2 rows"},
		// a comment below the package clause is not a description
		{"package main\n\n// Run does things.\nfunc Run() {}\n", ""},
		{"/* Block style. Works too. */\npackage main\n", "Block style."},
		{"// " + long + "\npackage main\n", strings.TrimRight(long[:descMax-1], " ") + "…"},
	} {
		p := filepath.Join(t.TempDir(), "s.go")
		if err := os.WriteFile(p, []byte(c.src), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := ScriptDesc(p); got != c.want {
			t.Errorf("ScriptDesc(%q)\n got %q\nwant %q", c.src, got, c.want)
		}
	}
}
