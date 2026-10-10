package sdb

import (
	"os"
	"path/filepath"
	"testing"
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
