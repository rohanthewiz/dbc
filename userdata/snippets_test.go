package userdata

import (
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
