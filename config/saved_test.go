package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rohanthewiz/btypedb"
)

func savedNames(list []SavedConn) string {
	n := make([]string, len(list))
	for i, c := range list {
		n[i] = c.Name
	}
	return strings.Join(n, ",")
}

func TestSavedRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cfg", "connections.toml")

	// no file yet: none, and no error
	if got, err := ReadSaved(path); err != nil || got != nil {
		t.Fatalf("missing file = %v, %v", got, err)
	}

	st := OpenSaved(path)
	added := time.Date(2026, 9, 25, 18, 12, 0, 0, time.UTC)
	must(t, st.Add(SavedConn{Name: "stg", Driver: "postgres", DSN: "postgres://app:${PGPASS}@stg/app", Added: added}))
	must(t, st.Add(SavedConn{Name: "local", Driver: "sqlite", DSN: "file:x.db", AIRows: true}))

	got, err := ReadSaved(path)
	if err != nil {
		t.Fatal(err)
	}
	if savedNames(got) != "stg,local" || got[0].DSN != "postgres://app:${PGPASS}@stg/app" ||
		!got[0].Added.Equal(added) || !got[1].AIRows || got[1].Added.IsZero() {
		t.Fatalf("read back %+v", got)
	}

	// the file can hold passwords: user-only, in a user-only directory
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if perm := fi.Mode().Perm(); perm != 0o600 {
			t.Errorf("file mode %o, want 600", perm)
		}
		di, _ := os.Stat(filepath.Dir(path))
		if perm := di.Mode().Perm(); perm != 0o700 {
			t.Errorf("dir mode %o, want 700", perm)
		}
	}
	// readable, and copyable into config.toml as is: [[connection]] tables,
	// the ${VAR} unexpanded, under the header
	bs, _ := os.ReadFile(path)
	text := string(bs)
	for _, want := range []string{"# Connections added in dbc web", "[[connection]]", `${PGPASS}`} {
		if !strings.Contains(text, want) {
			t.Errorf("file lacks %q:\n%s", want, text)
		}
	}
	// no temp file is left behind
	if ents, _ := os.ReadDir(filepath.Dir(path)); len(ents) != 2 { // the file and its .lock
		t.Errorf("directory holds %d entries, want the file and its lock", len(ents))
	}
}

func TestSavedEdits(t *testing.T) {
	st := OpenSaved(filepath.Join(t.TempDir(), "connections.toml"))
	for _, n := range []string{"a", "b", "c"} {
		must(t, st.Add(SavedConn{Name: n, Driver: "sqlite", DSN: "file:" + n}))
	}
	if err := st.Add(SavedConn{Name: "b", Driver: "sqlite"}); !errors.Is(err, ErrConnExists) {
		t.Fatalf("duplicate add = %v", err)
	}

	before, _, _ := st.Get("b")
	// a rename keeps the place and Added, whatever Added the caller sends
	must(t, st.Update("b", SavedConn{Name: "b2", Driver: "mysql", DSN: "u@/d", AIRows: true}))
	all, _ := st.List()
	if savedNames(all) != "a,b2,c" || !all[1].Added.Equal(before.Added) || all[1].Driver != "mysql" || !all[1].AIRows {
		t.Fatalf("after update: %+v", all)
	}
	if err := st.Update("b2", SavedConn{Name: "c"}); !errors.Is(err, ErrConnExists) {
		t.Fatalf("rename onto another = %v", err)
	}
	if err := st.Update("gone", SavedConn{Name: "x"}); !errors.Is(err, ErrConnNotFound) {
		t.Fatalf("update of a missing one = %v", err)
	}

	must(t, st.Delete("a"))
	must(t, st.Delete("a")) // already gone: still success
	if all, _ = st.List(); savedNames(all) != "b2,c" {
		t.Fatalf("after delete: %s", savedNames(all))
	}
	// the last one out leaves a file with only the header, which reads as none
	must(t, st.Delete("b2"))
	must(t, st.Delete("c"))
	if all, err := st.List(); err != nil || len(all) != 0 {
		t.Fatalf("emptied: %+v, %v", all, err)
	}
}

// A file that does not parse — a hand edit with a typo — is not replaced by
// a write: that would throw away every entry in it.
func TestSavedBrokenFileNotOverwritten(t *testing.T) {
	path := filepath.Join(t.TempDir(), "connections.toml")
	broken := "[[connection]]\nname = \"a\"\ndriver = sqlite\n" // unquoted value
	must(t, os.WriteFile(path, []byte(broken), 0o600))

	if err := OpenSaved(path).Add(SavedConn{Name: "b", Driver: "sqlite"}); err == nil {
		t.Fatal("an add over a broken file succeeded")
	}
	if bs, _ := os.ReadFile(path); string(bs) != broken {
		t.Fatalf("the broken file was changed:\n%s", bs)
	}

	// a reader gets a warning, not a failure
	c := &Config{Connections: []Connection{{Name: "file", Driver: "sqlite"}}}
	c.LoadSaved(path, nil)
	if len(c.Connections) != 1 || len(c.Warnings) != 1 || !strings.Contains(c.Warnings[0], "could not be read") {
		t.Fatalf("after a broken file: %+v, %q", c.Connections, c.Warnings)
	}
}

// Without a home directory the list lives in memory, and behaves the same.
func TestSavedMemoryOnly(t *testing.T) {
	st := OpenSaved("")
	if st.Persistent() {
		t.Fatal("a pathless store says it persists")
	}
	must(t, st.Add(SavedConn{Name: "a", Driver: "sqlite"}))
	if err := st.Add(SavedConn{Name: "a", Driver: "sqlite"}); !errors.Is(err, ErrConnExists) {
		t.Fatalf("duplicate = %v", err)
	}
	must(t, st.Update("a", SavedConn{Name: "b", Driver: "sqlite"}))
	if all, _ := st.List(); savedNames(all) != "b" {
		t.Fatalf("memory list = %s", savedNames(all))
	}
	// the list handed out is a copy
	all, _ := st.List()
	all[0].Name = "changed"
	if _, ok, _ := st.Get("b"); !ok {
		t.Fatal("a caller's change reached the store")
	}
}

func TestLoadSaved(t *testing.T) {
	path := filepath.Join(t.TempDir(), "connections.toml")
	st := OpenSaved(path)
	t.Setenv("DBC_TEST_SAVED", "expanded")
	for _, sc := range []SavedConn{
		{Name: "web1", Driver: "sqlite", DSN: "file:${DBC_TEST_SAVED}", AIRows: true},
		{Name: "file", Driver: "sqlite", DSN: "file:clash"}, // the config file's name
		{Name: "odd", Driver: "oracle", DSN: "x"},
		{Name: "web2", Driver: "sqlite", DSN: "file:${DBC_TEST_SURELY_UNSET_SAVED}"},
	} {
		must(t, st.Add(sc))
	}
	// a hand edit: a second entry by one name, and one with no driver
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	must(t, err)
	_, err = f.WriteString("\n[[connection]]\nname = \"web1\"\ndriver = \"sqlite\"\ndsn = \"file:second\"\n" +
		"\n[[connection]]\nname = \"nodriver\"\ndsn = \"file:y\"\n")
	must(t, err)
	must(t, f.Close())

	c := &Config{Connections: []Connection{{Name: "file", Driver: "sqlite", DSN: "file:config"}}}
	c.LoadSaved(path, func(d string) bool { return d == "sqlite" })

	if got := fmt.Sprint(names(c.Connections)); got != "[file web1 web2]" {
		t.Fatalf("merged = %s", got)
	}
	if cn, _ := c.ConnByName("file"); cn.Web || cn.DSN != "file:config" {
		t.Fatalf("the config file's connection was displaced: %+v", cn)
	}
	if cn, _ := c.ConnByName("web1"); !cn.Web || !cn.AIRows || cn.DSN != "file:expanded" {
		t.Fatalf("web1 = %+v", cn)
	}
	w := strings.Join(c.Warnings, "\n")
	for _, want := range []string{
		`"file" skipped: a connection by that name is already defined`,
		`"odd" skipped: unknown driver "oracle"`,
		`"web1" skipped: a connection by that name`, // the second of two
		`"nodriver" skipped: it needs both a name and a driver`,
		`$DBC_TEST_SURELY_UNSET_SAVED`,
	} {
		if !strings.Contains(w, want) {
			t.Errorf("warnings lack %q:\n%s", want, w)
		}
	}
	// a skipped entry's unset ${VAR} is not worth a word: nothing uses it
	if strings.Count(w, "unset env var") != 1 {
		t.Errorf("unset-var warnings:\n%s", w)
	}

	// no file at all: nothing merged, nothing said
	c = &Config{Connections: []Connection{{Name: "file"}}}
	c.LoadSaved(filepath.Join(t.TempDir(), "none.toml"), nil)
	c.LoadSaved("", nil)
	if len(c.Connections) != 1 || len(c.Warnings) != 0 {
		t.Fatalf("no file: %+v, %q", c.Connections, c.Warnings)
	}
}

// With no config file (the built-in demos), saved connections still merge.
func TestLoadSavedOnDemo(t *testing.T) {
	isolateDemo(t) // no ./dbc.toml or ~/.config/dbc/config.toml
	path := filepath.Join(t.TempDir(), "connections.toml")
	must(t, OpenSaved(path).Add(SavedConn{Name: "web1", Driver: "sqlite", DSN: "file:w"}))

	c, err := LoadDemo("", DemoEngineSQLite)
	must(t, err)
	c.LoadSaved(path, nil)
	if got := fmt.Sprint(names(c.Connections)); got != "[demo-sqlite demo-bytdb web1]" || c.DefaultConnection != DemoSQLite {
		t.Fatalf("demo + saved = %s (default %q)", got, c.DefaultConnection)
	}
}

// Writers in separate stores over one path — two dbc webs, as far as the
// lock can tell (flock is per open file, not per process) — do not lose
// each other's adds.
func TestSavedConcurrentWriters(t *testing.T) {
	path := filepath.Join(t.TempDir(), "connections.toml")
	const n = 16
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		wg.Go(func() {
			errs <- OpenSaved(path).Add(SavedConn{Name: fmt.Sprintf("c%02d", i), Driver: "sqlite"})
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		must(t, err)
	}
	all, err := ReadSaved(path)
	must(t, err)
	if len(all) != n {
		t.Fatalf("%d entries after %d concurrent adds: %s", len(all), n, savedNames(all))
	}
}

// A writer finding the lock held waits for it rather than failing.
func TestSavedWaitsForLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "connections.toml")
	lock, err := btypedb.AcquireLock(path)
	must(t, err)
	done := make(chan error, 1)
	go func() { done <- OpenSaved(path).Add(SavedConn{Name: "a", Driver: "sqlite"}) }()

	select {
	case err = <-done:
		t.Fatalf("the add did not wait for the lock: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	must(t, lock.Close())
	select {
	case err = <-done:
		must(t, err)
	case <-time.After(lockWait):
		t.Fatal("the add did not finish once the lock was free")
	}
}

func names(cs []Connection) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.Name
	}
	return out
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
