package userdata

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Consoles are namespaced host ─► database ─► name, with readable directory
// names wherever the original is already a safe one.
func TestConsoleDBOf(t *testing.T) {
	cases := []struct {
		host, database string
		want           ConsoleDB
	}{
		{"localhost:5432", "app", ConsoleDB{"localhost_5432", "app"}},
		{"db.example.com:3306", "shop", ConsoleDB{"db.example.com_3306", "shop"}},
		{"", "odd", ConsoleDB{"unknown", "odd"}},
		{"local", "scratch.db", ConsoleDB{"local", "scratch.db"}},
	}
	for _, c := range cases {
		if got := ConsoleDBOf(c.host, c.database); got != c.want {
			t.Errorf("ConsoleDBOf(%q, %q) = %+v, want %+v", c.host, c.database, got, c.want)
		}
	}

	// names that are not file names keep a readable part and gain a hash
	sock := ConsoleDBOf("/tmp:5432", "my db")
	if !sock.Valid() || !strings.HasPrefix(sock.Host, "_tmp_5432-") || !strings.HasPrefix(sock.Database, "my_db-") {
		t.Errorf("socket host / spaced database = %+v", sock)
	}
	// an embedded database is its path: two scratch.db's are two directories
	a, b := ConsoleDBOf("local", "/p1/scratch.db"), ConsoleDBOf("local", "/p2/scratch.db")
	if a == b || !strings.HasPrefix(a.Database, "scratch.db-") {
		t.Errorf("embedded databases %+v and %+v", a, b)
	}
	// the same server's databases share a host directory; another server
	// with a database of the same name does not share its consoles
	if ConsoleDBOf("h:1", "app").Host != ConsoleDBOf("h:1", "other").Host ||
		ConsoleDBOf("h:1", "app") == ConsoleDBOf("h:2", "app") {
		t.Error("host namespacing broken")
	}
	// a literal underscore cannot impersonate a port
	if ConsoleDBOf("a_5432", "x").Host == ConsoleDBOf("a:5432", "x").Host {
		t.Error("a_5432 and a:5432 share a directory")
	}
}

func TestConsolePathRejectsEscapes(t *testing.T) {
	root := t.TempDir()
	for _, bad := range []struct {
		d    ConsoleDB
		name string
	}{
		{ConsoleDB{"..", "app"}, "console"},
		{ConsoleDB{"h", "../../etc"}, "console"},
		{ConsoleDB{"h", "app"}, "../x"},
		{ConsoleDB{"h", "app"}, ".hidden"},
		{ConsoleDB{"h", "app"}, ""},
	} {
		if p := ConsolePath(root, bad.d, bad.name); p != "" {
			t.Errorf("ConsolePath(%+v, %q) = %q, want refused", bad.d, bad.name, p)
		}
	}
	if ConsolePath("", ConsoleDB{"h", "app"}, "console") != "" {
		t.Error("no root must mean no console file")
	}
	want := filepath.Join(root, "h", "app", "console.sql")
	if got := ConsolePath(root, ConsoleDB{"h", "app"}, "console"); got != want {
		t.Errorf("ConsolePath = %q, want %q", got, want)
	}
}

func TestConsoleRoundTrip(t *testing.T) {
	path := ConsolePath(filepath.Join(t.TempDir(), "consoles"), ConsoleDBOf("localhost:5432", "app"), DefaultConsole)

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

func TestListRenameDeleteConsoles(t *testing.T) {
	root := t.TempDir()
	d := ConsoleDBOf("localhost:5432", "app")
	for _, n := range []string{"reports", "console-10", "console", "console-2", "adhoc"} {
		if err := SaveConsole(ConsolePath(root, d, n), "SELECT '"+n+"'"); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"console", "console-2", "console-10", "adhoc", "reports"}
	if got := ListConsoles(root, d, ""); !slices.Equal(got, want) {
		t.Errorf("ListConsoles = %v, want %v", got, want)
	}
	if got := NextConsoleName(want); got != "console-3" {
		t.Errorf("NextConsoleName = %q, want console-3", got)
	}
	if got := NextConsoleName(nil); got != DefaultConsole {
		t.Errorf("NextConsoleName(none) = %q", got)
	}

	if err := RenameConsole(root, d, "adhoc", "reports"); err == nil {
		t.Error("a rename onto an existing console was allowed")
	}
	if err := RenameConsole(root, d, "adhoc", "scratch"); err != nil {
		t.Fatal(err)
	}
	if text, _ := LoadConsole(ConsolePath(root, d, "scratch")); text != "SELECT 'adhoc'" {
		t.Errorf("renamed console holds %q", text)
	}
	// a console never written renames as a name only
	if err := RenameConsole(root, d, "console-9", "later"); err != nil {
		t.Errorf("renaming a fileless console: %v", err)
	}
	if err := DeleteConsole(root, d, "scratch"); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(ListConsoles(root, d, ""), "scratch") {
		t.Error("deleted console still listed")
	}
	if err := DeleteConsole(root, d, "scratch"); err != nil {
		t.Errorf("deleting a gone console: %v", err)
	}
}

// The first consoles layout kept one flat file per database; listing the
// database moves it in as its first console, never over one that exists.
func TestFlatConsoleAdopted(t *testing.T) {
	root := t.TempDir()
	d := ConsoleDBOf("localhost:5432", "app")
	flat := FlatConsoleFile(root, "localhost:5432", "app")
	if err := os.WriteFile(flat, []byte("SELECT 'flat'"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := ListConsoles(root, d, flat); !slices.Equal(got, []string{DefaultConsole}) {
		t.Fatalf("after adopting, ListConsoles = %v", got)
	}
	if text, _ := LoadConsole(ConsolePath(root, d, DefaultConsole)); text != "SELECT 'flat'" {
		t.Errorf("adopted console holds %q", text)
	}
	if _, err := os.Stat(flat); !os.IsNotExist(err) {
		t.Error("the flat file was not moved")
	}
}

func TestConsoleRev(t *testing.T) {
	if ConsoleRev("x", false) != "" {
		t.Error("a console with no file must be revision \"\"")
	}
	if ConsoleRev("", true) == "" || ConsoleRev("a", true) == ConsoleRev("b", true) {
		t.Error("revisions must tell texts apart, empty included")
	}
}
