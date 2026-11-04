package userdata

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"slices"
	"testing"

	bytdbdrv "github.com/rohanthewiz/bytdb/stdlib"

	"github.com/rohanthewiz/dbc/snippet"
)

func snippetNames(t *testing.T, s *Snippets) []string {
	t.Helper()
	list, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var out []string
	for _, sn := range list {
		out = append(out, sn.Name)
	}
	return out
}

func TestStarterSnippetsParse(t *testing.T) {
	for _, sn := range StarterSnippets {
		if err := snippet.ValidName(sn.Name); err != nil {
			t.Errorf("%s: %v", sn.Name, err)
		}
		if _, err := snippet.Parse(sn.Body); err != nil {
			t.Errorf("%s: %v", sn.Name, err)
		}
	}
}

// The file store and the memory store behave the same: seeded once, save,
// rename, refusals, delete.
func TestSnippetsStore(t *testing.T) {
	for _, path := range []string{"", filepath.Join(t.TempDir(), "snippets.bytdb")} {
		s := OpenSnippets(path)
		starters := len(StarterSnippets)
		if got := snippetNames(t, s); len(got) != starters || !slices.IsSorted(got) {
			t.Fatalf("%q: seeded = %v", path, got)
		}
		if err := s.Save("", Snippet{Name: "mine", Desc: "two\n  lines", Body: "SELECT $VAR1$"}); err != nil {
			t.Fatalf("%q: save: %v", path, err)
		}
		sn, ok, err := s.Get("mine")
		if err != nil || !ok || sn.Desc != "two lines" || sn.Updated.IsZero() {
			t.Fatalf("%q: get = %+v %v %v", path, sn, ok, err)
		}
		// refusals: a taken name, a broken template, a bad name, a gone one
		if err := s.Save("", Snippet{Name: "sel", Body: "x"}); !errors.Is(err, ErrSnippetExists) {
			t.Errorf("%q: duplicate: %v", path, err)
		}
		if err := s.Save("", Snippet{Name: "bad", Body: "$VAR1"}); err == nil {
			t.Errorf("%q: broken template saved", path)
		}
		if err := s.Save("", Snippet{Name: "a-b", Body: "x"}); err == nil {
			t.Errorf("%q: bad name saved", path)
		}
		if err := s.Save("nope", Snippet{Name: "nope", Body: "x"}); !errors.Is(err, ErrNoSnippet) {
			t.Errorf("%q: edit of a missing snippet: %v", path, err)
		}
		// rename onto a free name, then onto a taken one
		if err := s.Save("mine", Snippet{Name: "ours", Body: "SELECT 2"}); err != nil {
			t.Fatalf("%q: rename: %v", path, err)
		}
		if err := s.Save("ours", Snippet{Name: "cnt", Body: "x"}); !errors.Is(err, ErrSnippetExists) {
			t.Errorf("%q: rename onto a taken name: %v", path, err)
		}
		if err := s.Delete("sel"); err != nil {
			t.Fatalf("%q: delete: %v", path, err)
		}
		if err := s.Delete("sel"); !errors.Is(err, ErrNoSnippet) {
			t.Errorf("%q: second delete: %v", path, err)
		}
		got := snippetNames(t, s)
		if slices.Contains(got, "sel") || slices.Contains(got, "mine") || !slices.Contains(got, "ours") {
			t.Fatalf("%q: after edits = %v", path, got)
		}
		if path == "" {
			continue
		}
		// a second handle — another process — sees the file as it is, and
		// the deleted starter is not seeded back
		if got2 := snippetNames(t, OpenSnippets(path)); !slices.Equal(got, got2) {
			t.Fatalf("reopened = %v, want %v", got2, got)
		}
	}
}

// One handle's cache notices another handle's write (the TUI seeing a
// snippet saved in dbc web), and a held file is waited for, not refused.
func TestSnippetsAcrossHandles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snippets.bytdb")
	a, b := OpenSnippets(path), OpenSnippets(path)
	snippetNames(t, a)
	if err := b.Save("", Snippet{Name: "fromb", Body: "SELECT 1"}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := a.Get("fromb"); err != nil || !ok {
		t.Fatalf("a did not see b's save: %v %v", ok, err)
	}

	// hold the file briefly, as the other process would mid-operation
	held, err := sql.Open(bytdbdrv.DriverName, path)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- a.Save("", Snippet{Name: "waited", Body: "SELECT 1"}) }()
	_ = held.Ping()
	if err := held.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("save while held: %v", err)
	}
	if _, ok, _ := b.Get("waited"); !ok {
		t.Fatal("b did not see the save made after the lock was let go")
	}
}

// Languages: "" saves as SQL, an unknown one is refused, GetFor and
// ListFor see only their language's snippets, and an edit can move a
// snippet from one language to the other.
func TestSnippetsLang(t *testing.T) {
	for _, path := range []string{"", filepath.Join(t.TempDir(), "snippets.bytdb")} {
		s := OpenSnippets(path)
		if err := s.Save("", Snippet{Name: "plain", Body: "SELECT 1"}); err != nil {
			t.Fatalf("%q: save: %v", path, err)
		}
		if sn, _, _ := s.Get("plain"); sn.Lang != LangSQL {
			t.Errorf("%q: no lang saved as %q, want sql", path, sn.Lang)
		}
		if err := s.Save("", Snippet{Name: "x", Lang: "rust", Body: "x"}); err == nil {
			t.Errorf("%q: unknown language saved", path)
		}
		if err := s.Save("", Snippet{Name: "gofn", Lang: "Go", Body: "func $VAR1$() {}"}); err != nil {
			t.Fatalf("%q: save go: %v", path, err)
		}
		if _, ok, _ := s.GetFor("gofn", LangSQL); ok {
			t.Errorf("%q: a Go snippet found for SQL", path)
		}
		if sn, ok, _ := s.GetFor("gofn", LangGo); !ok || sn.Lang != LangGo {
			t.Errorf("%q: GetFor go = %+v %v", path, sn, ok)
		}
		if _, ok, _ := s.GetFor("sel", LangGo); ok {
			t.Errorf("%q: a SQL starter found for Go", path)
		}
		goList, err := s.ListFor(LangGo)
		if err != nil {
			t.Fatal(err)
		}
		for _, sn := range goList {
			if sn.Lang != LangGo {
				t.Errorf("%q: ListFor(go) has %s (%s)", path, sn.Name, sn.Lang)
			}
		}
		if !slices.ContainsFunc(goList, func(sn Snippet) bool { return sn.Name == "iferr" }) {
			t.Errorf("%q: the Go starters are not seeded: %v", path, goList)
		}
		// a name is unique across languages
		if err := s.Save("", Snippet{Name: "sel", Lang: LangGo, Body: "x"}); !errors.Is(err, ErrSnippetExists) {
			t.Errorf("%q: Go snippet onto a SQL name: %v", path, err)
		}
		// an edit moves it to the other language
		if err := s.Save("plain", Snippet{Name: "plain", Lang: LangGo, Body: "x := 1"}); err != nil {
			t.Fatal(err)
		}
		if _, ok, _ := s.GetFor("plain", LangGo); !ok {
			t.Errorf("%q: the language change did not stick", path)
		}
	}
}

func TestStarterSnippetsLangs(t *testing.T) {
	for _, sn := range StarterSnippets {
		if l, err := NormLang(sn.Lang); err != nil || l != sn.Lang || sn.Lang == "" {
			t.Errorf("%s: starter lang %q", sn.Name, sn.Lang)
		}
	}
}

// A file made before languages — the old table, seeded with the SQL
// starters and a snippet of the user's — gains the column (its rows
// SQL), and the Go starters once, without the SQL ones coming back or a
// user snippet that took a Go starter's name being overwritten.
func TestSnippetsOldFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snippets.bytdb")
	old, err := sql.Open(bytdbdrv.DriverName, path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`CREATE TABLE snippets (name TEXT PRIMARY KEY, descr TEXT NOT NULL, body TEXT NOT NULL, updated TIMESTAMPTZ NOT NULL)`,
		`CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		`INSERT INTO meta (key, value) VALUES ('seeded', '1')`,
		`INSERT INTO snippets VALUES ('mine', '', 'SELECT 1', now())`,
		`INSERT INTO snippets VALUES ('iferr', 'mine too', 'SELECT 2', now())`,
	} {
		if _, err := old.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	s := OpenSnippets(path)
	got := snippetNames(t, s)
	if slices.Contains(got, "sel") {
		t.Errorf("the SQL starters came back: %v", got)
	}
	if !slices.Contains(got, "qry") || !slices.Contains(got, "mine") {
		t.Errorf("after upgrade = %v", got)
	}
	if sn, _, _ := s.Get("mine"); sn.Lang != LangSQL {
		t.Errorf("an old row reads as %q, want sql", sn.Lang)
	}
	if sn, _, _ := s.Get("iferr"); sn.Lang != LangSQL || sn.Body != "SELECT 2" {
		t.Errorf("the user's iferr was replaced by the starter: %+v", sn)
	}
	// a second open seeds nothing more: delete a Go starter, reopen
	if err := s.Delete("qry"); err != nil {
		t.Fatal(err)
	}
	if got := snippetNames(t, OpenSnippets(path)); slices.Contains(got, "qry") {
		t.Errorf("a deleted Go starter came back: %v", got)
	}
}

// Opening the file runs the schema (CREATE/ALTER IF NOT EXISTS) every time;
// that must not rewrite it, or the TUI and dbc web would each see the
// other's every open as a change and re-read the file on every Tab.
func TestSnippetsOpenLeavesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snippets.bytdb")
	s := OpenSnippets(path)
	snippetNames(t, s) // creates and seeds
	before, err := stampOf(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.withDB(func(context.Context, *sql.DB) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if after, _ := stampOf(path); after != before {
		t.Errorf("an open with nothing to do changed the file: %+v → %+v", before, after)
	}
}
