package userdata

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConsoleFileNames(t *testing.T) {
	dir := t.TempDir()
	if ConsoleFile("", "h", "d") != "" {
		t.Error("no directory must mean no console file")
	}

	pg := ConsoleFile(dir, "db.example.com:5432", "app")
	if filepath.Dir(pg) != dir || !strings.HasPrefix(filepath.Base(pg), "db.example.com_5432--app-") ||
		!strings.HasSuffix(pg, ".sql") {
		t.Errorf("unexpected console file %q", pg)
	}
	if ConsoleFile(dir, "db.example.com:5432", "app") != pg {
		t.Error("the same target must give the same file")
	}

	// the slug folds ':' and '/' into '_'; the hash must keep these apart
	if ConsoleFile(dir, "a:b", "x") == ConsoleFile(dir, "a/b", "x") {
		t.Error("targets that slug alike share a file")
	}
	// an embedded database is its path: two scratch.db's are two consoles
	a, b := ConsoleFile(dir, "local", "/p1/scratch.db"), ConsoleFile(dir, "local", "/p2/scratch.db")
	if a == b || !strings.HasPrefix(filepath.Base(a), "local--scratch.db-") {
		t.Errorf("embedded consoles %q and %q", a, b)
	}
	// nothing in the name may climb out of the directory or hide
	if f := ConsoleFile(dir, "..", "../../etc"); filepath.Dir(f) != dir || strings.HasPrefix(filepath.Base(f), ".") {
		t.Errorf("console file %q escapes or hides", f)
	}
}

func TestConsoleRoundTrip(t *testing.T) {
	path := ConsoleFile(filepath.Join(t.TempDir(), "consoles"), "localhost:5432", "app")

	if text, exists := LoadConsole(path); exists || text != "" {
		t.Fatalf("absent console loaded as %q, exists=%v", text, exists)
	}
	// an empty console with no file is not written
	if err := SaveConsole(path, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("an empty console left a file behind (stat err %v)", err)
	}

	if err := SaveConsole(path, "SELECT 1;"); err != nil {
		t.Fatal(err)
	}
	if text, exists := LoadConsole(path); !exists || text != "SELECT 1;" {
		t.Fatalf("loaded %q, exists=%v", text, exists)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("console mode %o, want 600", perm)
	}

	// emptied on purpose, it stays — empty, and still there
	if err := SaveConsole(path, ""); err != nil {
		t.Fatal(err)
	}
	if text, exists := LoadConsole(path); !exists || text != "" {
		t.Errorf("emptied console loaded as %q, exists=%v", text, exists)
	}
}
