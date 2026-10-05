package userdata

import (
	"os"
	"path/filepath"
	"testing"
)

// A layout round-trips through its file; a missing or garbled file is the
// zero Layout (the defaults), and no path keeps it off disk.
func TestLayoutRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "tui-layout.json")
	if got := LoadLayout(path); got != (Layout{}) {
		t.Fatalf("missing file: %+v", got)
	}
	want := Layout{SideW: 30, ChatW: 50, LogH: 9, EdFrac: 0.42, SideHidden: true}
	if err := SaveLayout(path, want); err != nil {
		t.Fatal(err)
	}
	if got := LoadLayout(path); got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := LoadLayout(path); got != (Layout{}) {
		t.Fatalf("garbled file: %+v", got)
	}
	if err := SaveLayout("", want); err != nil {
		t.Fatalf("no path: %v", err)
	}
}
