package userdata

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

// A Go file of another package beside the scripts (the repo's
// scripts/embed.go, when scripts_dir is a checkout's ./scripts) is not a
// script; one whose header does not parse yet is, since it is being written.
func TestListScriptsSkipsOtherPackages(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"embed.go": "// Package scripts embeds the samples.\npackage scripts\n",
		"draft.go": "// Being written.\npackag main\n",
		"real.go":  "package main\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := ListScripts(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, in := range got {
		names = append(names, in.Name)
		if in.Rev == "" {
			t.Errorf("%s has no rev", in.Name)
		}
	}
	if want := "draft.go real.go"; strings.Join(names, " ") != want {
		t.Errorf("names = %v, want %s", names, want)
	}
}

func TestValidScriptName(t *testing.T) {
	for name, want := range map[string]bool{
		"copy.go": true, "copy_my-table.v2.go": true, "A1.go": true,
		"copy":                          false, // no .go
		"copy.sql":                      false,
		".go":                           false, // no stem
		".hidden.go":                    false,
		"a..go":                         false, // stem ends in a dot
		"../x.go":                       false,
		"sub/x.go":                      false,
		`sub\x.go`:                      false,
		"x y.go":                        false,
		"":                              false,
		"..":                            false,
		"x.go/":                         false,
		"é.go":                          false,
		strings.Repeat("a", 64) + ".go": true,
		strings.Repeat("a", 65) + ".go": false,
	} {
		if got := ValidScriptName(name); got != want {
			t.Errorf("ValidScriptName(%q) = %v, want %v", name, got, want)
		}
	}
	// every function refuses a bad name before touching the disk
	dir := t.TempDir()
	if _, _, err := SaveScript(dir, "../x.go", "package main\n", ""); !errors.Is(err, ErrBadScriptName) {
		t.Errorf("SaveScript(../x.go) = %v, want ErrBadScriptName", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "x.go")); err == nil {
		t.Error("SaveScript wrote outside the directory")
	}
	if _, _, err := ReadScript(dir, "../x.go"); !errors.Is(err, ErrBadScriptName) {
		t.Errorf("ReadScript(../x.go) = %v", err)
	}
	if err := RenameScript(dir, "a.go", "../b.go"); !errors.Is(err, ErrBadScriptName) {
		t.Errorf("RenameScript(→ ../b.go) = %v", err)
	}
	if _, err := TrashScript(dir, ".x.go"); !errors.Is(err, ErrBadScriptName) {
		t.Errorf("TrashScript(.x.go) = %v", err)
	}
	for _, id := range []string{"../x.123.go", "x.go", "x.abc.go", ".x.123.go", "a/x.123.go"} {
		if _, err := RestoreScript(dir, id, ""); err == nil {
			t.Errorf("RestoreScript(%q) accepted", id)
		}
	}
}

// The save protocol: create, create refused over an existing file, an
// update from the right base, a conflict from a stale one (a change made
// elsewhere, as by vim), and a conflict when the file was deleted.
func TestSaveScript(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "scripts") // created by the first save
	rev1, conflict, err := SaveScript(dir, "a.go", "package main // v1\n", "")
	if err != nil || conflict || rev1 == "" {
		t.Fatalf("create = %q, %v, %v", rev1, conflict, err)
	}
	if st, err := os.Stat(filepath.Join(dir, "a.go")); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("new script mode = %v, %v; want 0600", st.Mode().Perm(), err)
	}
	if _, _, err = SaveScript(dir, "a.go", "package main // other\n", ""); !errors.Is(err, ErrScriptExists) {
		t.Fatalf("create over existing = %v, want ErrScriptExists", err)
	}
	if text, rev, _ := ReadScript(dir, "a.go"); text != "package main // v1\n" || rev != rev1 {
		t.Fatalf("after refused create: %q %q", text, rev)
	}

	// the user made it world-readable; a save keeps that
	if err = os.Chmod(filepath.Join(dir, "a.go"), 0o644); err != nil {
		t.Fatal(err)
	}
	rev2, conflict, err := SaveScript(dir, "a.go", "package main // v2\n", rev1)
	if err != nil || conflict || rev2 == rev1 {
		t.Fatalf("update = %q, %v, %v", rev2, conflict, err)
	}
	if st, _ := os.Stat(filepath.Join(dir, "a.go")); st.Mode().Perm() != 0o644 {
		t.Errorf("mode after save = %v, want 0644 kept", st.Mode().Perm())
	}

	// changed outside dbc: a save from rev2 is a conflict and writes nothing
	if err = os.WriteFile(filepath.Join(dir, "a.go"), []byte("package main // vim\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cur, conflict, err := SaveScript(dir, "a.go", "package main // v3\n", rev2)
	if err != nil || !conflict {
		t.Fatalf("stale save = %q, %v, %v; want a conflict", cur, conflict, err)
	}
	if text, rev, _ := ReadScript(dir, "a.go"); text != "package main // vim\n" || rev != cur {
		t.Errorf("after conflict: %q rev %q (conflict said %q)", text, rev, cur)
	}

	// deleted elsewhere: a conflict with rev ""; saving with base "" puts it back
	if err = os.Remove(filepath.Join(dir, "a.go")); err != nil {
		t.Fatal(err)
	}
	if cur, conflict, err = SaveScript(dir, "a.go", "x", cur); err != nil || !conflict || cur != "" {
		t.Errorf("save over deleted = %q, %v, %v; want conflict, rev \"\"", cur, conflict, err)
	}
	if _, _, err = ReadScript(dir, "a.go"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("ReadScript(missing) = %v, want ErrNotExist", err)
	}

	if _, _, err = SaveScript(dir, "big.go", strings.Repeat("x", maxScriptBytes+1), ""); err == nil {
		t.Error("an oversized script was saved")
	}

	// no temp files left behind
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("temp file left: %s", e.Name())
		}
	}
}

// A symlinked script is saved through to its target: the link stays.
func TestSaveScriptThroughSymlink(t *testing.T) {
	dir, elsewhere := t.TempDir(), t.TempDir()
	target := filepath.Join(elsewhere, "shared.go")
	if err := os.WriteFile(target, []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "shared.go")); err != nil {
		t.Skip("no symlinks here:", err)
	}
	_, rev, err := ReadScript(dir, "shared.go")
	if err != nil {
		t.Fatal(err)
	}
	if _, conflict, err := SaveScript(dir, "shared.go", "package main // new\n", rev); err != nil || conflict {
		t.Fatalf("save = %v, %v", conflict, err)
	}
	if st, err := os.Lstat(filepath.Join(dir, "shared.go")); err != nil || st.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the link was replaced by a file")
	}
	if bs, _ := os.ReadFile(target); string(bs) != "package main // new\n" {
		t.Errorf("target = %q", bs)
	}
}

func TestRenameScript(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"a.go", "b.go"} {
		if _, _, err := SaveScript(dir, n, "package main // "+n+"\n", ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := RenameScript(dir, "a.go", "b.go"); !errors.Is(err, ErrScriptExists) {
		t.Fatalf("rename onto b.go = %v, want ErrScriptExists", err)
	}
	if err := RenameScript(dir, "missing.go", "c.go"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("rename of a missing script = %v", err)
	}
	if err := RenameScript(dir, "a.go", "c.go"); err != nil {
		t.Fatal(err)
	}
	if text, _, err := ReadScript(dir, "c.go"); err != nil || text != "package main // a.go\n" {
		t.Errorf("c.go = %q, %v", text, err)
	}
	// a change of case only: on a case-insensitive filesystem "C.go" is
	// c.go itself, which must not count as taken
	if err := RenameScript(dir, "c.go", "C.go"); err != nil {
		t.Errorf("case-only rename = %v", err)
	}
	if text, _, err := ReadScript(dir, "C.go"); err != nil || text != "package main // a.go\n" {
		t.Errorf("C.go = %q, %v", text, err)
	}
}

func TestTrashAndRestore(t *testing.T) {
	dir := t.TempDir()
	save := func(name, text string) {
		t.Helper()
		if _, _, err := SaveScript(dir, name, text, ""); err != nil {
			t.Fatal(err)
		}
	}
	save("a.go", "package main // first\n")
	id1, err := TrashScript(dir, "a.go")
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := ListScripts(dir); len(got) != 0 {
		t.Errorf("trashed script still listed: %v", got)
	}
	// a second a.go, trashed in the same millisecond, keeps its own ID
	save("a.go", "package main // second\n")
	id2, err := TrashScript(dir, "a.go")
	if err != nil || id2 == id1 {
		t.Fatalf("second trash = %q, %v (first %q)", id2, err, id1)
	}
	tr := ListTrash(dir)
	if len(tr) != 2 || tr[0].ID != id2 || tr[0].Name != "a.go" || tr[1].ID != id1 {
		t.Fatalf("trash = %+v, want %s then %s", tr, id2, id1)
	}

	// restore under the old name; then the other can only come back as another name
	if name, err := RestoreScript(dir, id1, ""); err != nil || name != "a.go" {
		t.Fatalf("restore = %q, %v", name, err)
	}
	if _, err := RestoreScript(dir, id2, ""); !errors.Is(err, ErrScriptExists) {
		t.Fatalf("restore over a.go = %v, want ErrScriptExists", err)
	}
	if name, err := RestoreScript(dir, id2, "a2.go"); err != nil || name != "a2.go" {
		t.Fatalf("restore as a2.go = %q, %v", name, err)
	}
	if text, _, _ := ReadScript(dir, "a.go"); text != "package main // first\n" {
		t.Errorf("a.go = %q", text)
	}
	if text, _, _ := ReadScript(dir, "a2.go"); text != "package main // second\n" {
		t.Errorf("a2.go = %q", text)
	}
	if len(ListTrash(dir)) != 0 {
		t.Errorf("trash not empty: %v", ListTrash(dir))
	}
	if _, err := RestoreScript(dir, id1, ""); err == nil {
		t.Error("restored the same trash ID twice")
	}
}

// The trash keeps the newest trashMax and deletes the rest.
func TestTrashKeepsTheNewest(t *testing.T) {
	dir := t.TempDir()
	td := filepath.Join(dir, trashDir)
	if err := os.MkdirAll(td, 0o755); err != nil {
		t.Fatal(err)
	}
	// trashMax old entries, a minute apart, oldest first
	base := time.Now().Add(-time.Hour)
	for i := range trashMax {
		id := fmt.Sprintf("old%d.%d.go", i, base.Add(time.Duration(i)*time.Minute).UnixMilli())
		if err := os.WriteFile(filepath.Join(td, id), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := SaveScript(dir, "new.go", "package main\n", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := TrashScript(dir, "new.go"); err != nil {
		t.Fatal(err)
	}
	tr := ListTrash(dir)
	if len(tr) != trashMax || tr[0].Name != "new.go" || tr[len(tr)-1].Name != "old1.go" {
		t.Errorf("trash has %d, first %s, last %s; want %d, new.go … old1.go (old0 pruned)",
			len(tr), tr[0].Name, tr[len(tr)-1].Name, trashMax)
	}
}
