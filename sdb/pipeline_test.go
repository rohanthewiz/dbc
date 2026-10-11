package sdb

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/rohanthewiz/dbc/pipeline"
)

// s.Path is Env.Path's rule over the session's Paths.FilesDir: a relative
// path joined to it, ~ expanded, an absolute one as written; with no
// files_dir, a relative path stays relative (the working directory).
func TestPath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	files := filepath.Join(t.TempDir(), "files")
	abs := filepath.Join(t.TempDir(), "x.csv")
	s := New(nil, nil, nil).WithPaths(Paths{FilesDir: files})
	bare := New(nil, nil, nil)
	for _, c := range []struct {
		s        *S
		in, want string
	}{
		{s, "out.csv", filepath.Join(files, "out.csv")},
		{s, "exports/out.csv", filepath.Join(files, "exports", "out.csv")},
		{s, abs, abs},
		{s, "~/x.csv", filepath.Join(home, "x.csv")},
		{s, "", ""}, // not files_dir itself: an empty path stays a mistake to report
		{bare, "out.csv", "out.csv"},
		{bare, "~/x.csv", filepath.Join(home, "x.csv")},
	} {
		if got := c.s.Path(c.in); got != c.want {
			t.Errorf("Path(%q) with files_dir %q = %q; want %q", c.in, c.s.paths.FilesDir, got, c.want)
		}
	}
}

// ForFiles is the session with another files_dir and nothing else of its
// own: Path resolves there, while what the view records — DDL for
// CatalogChanged, the Readers and Writers for Release — is the session's.
// The session itself comes back when the directory is already its own.
func TestForFiles(t *testing.T) {
	files, run := t.TempDir(), t.TempDir()
	s := New(nil, nil, nil).WithPaths(Paths{FilesDir: files, ScriptsDir: "scripts"})
	if s.ForFiles(files) != pipeline.Host(s) {
		t.Error("ForFiles of the session's own files_dir is not the session")
	}
	v, ok := s.ForFiles(run).(*S)
	if !ok || v == s {
		t.Fatalf("ForFiles(run) = %T, want a view", s.ForFiles(run))
	}
	if got, want := v.Path("a.csv"), filepath.Join(run, "a.csv"); got != want {
		t.Errorf("the view's Path = %q, want %q", got, want)
	}
	if got, want := s.Path("a.csv"), filepath.Join(files, "a.csv"); got != want {
		t.Errorf("the session's Path = %q, want %q (unchanged)", got, want)
	}
	if v.Paths().ScriptsDir != "scripts" {
		t.Errorf("the view's other paths: %+v", v.Paths())
	}
	v.noteCatalog("a")
	if !s.CatalogChanged("a") {
		t.Error("DDL through the view is not on the session's record")
	}
}
