package userdata

import (
	"os"
	"path/filepath"
	"testing"
)

// TestPicksRoundTrip saves picks one connection at a time and reads them
// back: each save keeps the others, including one written to the file by
// someone else (another dbc) after this one loaded it.
func TestPicksRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cfg", "schema-picks.json")
	if got := LoadPicks(path); len(got) != 0 {
		t.Fatalf("missing file loaded %v", got)
	}
	if err := SavePick(path, "pg", SchemaPick{Name: "billing"}); err != nil {
		t.Fatal(err)
	}
	if err := SavePick(path, "pg/analytics", SchemaPick{All: true}); err != nil {
		t.Fatal(err)
	}
	if err := SavePick(path, "pg", SchemaPick{Name: "public"}); err != nil {
		t.Fatal(err)
	}
	got := LoadPicks(path)
	want := map[string]SchemaPick{"pg": {Name: "public"}, "pg/analytics": {All: true}}
	if len(got) != len(want) {
		t.Fatalf("loaded %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %+v, want %+v", k, got[k], v)
		}
	}
	// no temp files left beside it
	ents, _ := os.ReadDir(filepath.Dir(path))
	if len(ents) != 1 {
		t.Errorf("dir holds %d entries, want just the file", len(ents))
	}
}

// TestPicksBadOrDisabled: no path persists nothing, and a corrupt file is
// no picks rather than an error — the cost is a default schema.
func TestPicksBadOrDisabled(t *testing.T) {
	if err := SavePick("", "pg", SchemaPick{Name: "x"}); err != nil {
		t.Fatal(err)
	}
	if got := LoadPicks(""); len(got) != 0 {
		t.Fatalf("no path loaded %v", got)
	}
	path := filepath.Join(t.TempDir(), "schema-picks.json")
	for _, junk := range []string{"{not json", "null"} {
		if err := os.WriteFile(path, []byte(junk), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := LoadPicks(path); got == nil || len(got) != 0 {
			t.Fatalf("%q loaded %v", junk, got)
		}
	}
	// and a save over a corrupt file starts it afresh
	if err := SavePick(path, "pg", SchemaPick{Name: "x"}); err != nil {
		t.Fatal(err)
	}
	if got := LoadPicks(path); got["pg"].Name != "x" {
		t.Fatalf("after save: %v", got)
	}
}
