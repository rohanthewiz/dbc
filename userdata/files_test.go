package userdata

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestSaveBufferIsAtomic is N-255: a console (SaveConsole goes through
// SaveBuffer) is replaced through a temp file renamed over it, not written
// in place, where a crash or a full disk mid-write truncated the user's
// SQL. The rename shows as a new file under the old name; nothing of the
// temp file is left; the file stays user-only.
func TestSaveBufferIsAtomic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "console.sql")
	if err := SaveBuffer(path, "SELECT 1"); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = SaveBuffer(path, "SELECT 2"); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(before, after) {
		t.Error("the console was rewritten in place, not replaced")
	}
	if got, _ := LoadConsole(path); got != "SELECT 2" {
		t.Errorf("console holds %q", got)
	}
	if runtime.GOOS != "windows" && after.Mode().Perm() != 0o600 {
		t.Errorf("mode %v, want 0600", after.Mode().Perm())
	}
	ents, _ := os.ReadDir(filepath.Dir(path))
	if len(ents) != 1 {
		t.Errorf("left behind: %v", ents)
	}
}

// TestWriteAtomicKeepsASymlink: a console or script that is a symlink
// (into a repository, say) stays one; the file it points at gets the text.
func TestWriteAtomicKeepsASymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "repo", "scratch.sql")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "console.sql")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := SaveBuffer(link, "new"); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the link was replaced by a file (%v)", err)
	}
	if bs, _ := os.ReadFile(target); string(bs) != "new" {
		t.Errorf("target holds %q", bs)
	}
}

// TestSweepTemps: a temp file a crash left between create and rename is
// removed on the process's first write into its directory — an old one,
// not one that may be another process's write in flight.
func TestSweepTemps(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, ".console.sql.tmp-123")
	fresh := filepath.Join(dir, ".console.sql.tmp-456")
	other := filepath.Join(dir, ".hidden")
	for _, p := range []string{stale, fresh, other} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-2 * staleTemp)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(other, old, old); err != nil {
		t.Fatal(err)
	}
	if err := SaveBuffer(filepath.Join(dir, "console.sql"), "SELECT 1"); err != nil {
		t.Fatal(err)
	}
	for p, want := range map[string]bool{stale: false, fresh: true, other: true} {
		if _, err := os.Stat(p); (err == nil) != want {
			t.Errorf("%s: exists %v, want %v", filepath.Base(p), err == nil, want)
		}
	}
}

// TestHistoryTrimKeepsAnotherProcessAppend is N-255's race: the trim reads
// the file again under its lock, so an entry another process appended
// after this one loaded (a dbc web recording a query while a TUI starts)
// survives the rewrite rather than landing in the file it replaced.
func TestHistoryTrimKeepsAnotherProcessAppend(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.jsonl")
	h := &History{path: path}
	at := time.Now()
	for i := range histMax + 20 {
		if err := h.Add("demo", "SELECT "+strconv.Itoa(i), at); err != nil {
			t.Fatal(err)
		}
	}
	// this process's view, loaded before the other's append
	loaded, _ := readHistory(path)
	mine := &History{path: path, entries: loaded}
	other := &History{path: path}
	if err := other.Add("demo", "SELECT 'from the other process'", at); err != nil {
		t.Fatal(err)
	}
	mine.trim()
	bs, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(bs)), "\n")
	if len(lines) != histMax || !strings.Contains(lines[len(lines)-1], "from the other process") {
		t.Errorf("after the trim: %d lines, last %q", len(lines), lines[len(lines)-1])
	}
	if got := mine.Recent()[0].SQL; got != "SELECT 'from the other process'" {
		t.Errorf("the trimmed history's newest entry is %q", got)
	}
}
