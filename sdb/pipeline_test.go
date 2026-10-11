package sdb

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/rohanthewiz/dbc/config"
	"github.com/rohanthewiz/dbc/db"
	"github.com/rohanthewiz/dbc/model"
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

// A script's pipeline run has a ${run.id} of its own, fresh each run and
// in the jobs engine's format, and ${run.trigger} "script" (N-206); a
// trigger the script passes is kept.
func TestRunPipelineRunValues(t *testing.T) {
	mgr := db.NewManager(&config.Config{MaxRows: 1000, Connections: []config.Connection{
		{Name: "a", Driver: "sqlite", DSN: filepath.Join(t.TempDir(), "a.db")},
	}})
	t.Cleanup(mgr.Close)
	var shown []*model.Result
	s := New(mgr, func(r *model.Result) { shown = append(shown, r) }, func(string) {})
	spec, err := pipeline.Parse(`{"name": "rv", "fragments": [{"name": "f", "nodes": [
	  {"id": "src", "plugin": "sql.read", "cfg": {"conn": "a", "query": "SELECT '${run.id}' AS id, '${run.trigger}' AS trig"}},
	  {"id": "show", "plugin": "preview", "cfg": {}}
	], "edges": [["src", "show"]]}]}`)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := s.RunPipelineSpec(spec, PipelineOpts{}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.RunPipelineSpec(spec, PipelineOpts{Run: map[string]string{"trigger": "nightly", "id": "mine"}}); err != nil {
		t.Fatal(err)
	}
	if len(shown) != 3 {
		t.Fatalf("%d results shown, want 3", len(shown))
	}
	idRe := regexp.MustCompile(`^\d{8}-\d{6}-[0-9a-f]{4}$`)
	a, b, c := shown[0].Rows[0], shown[1].Rows[0], shown[2].Rows[0]
	if !idRe.MatchString(a[0]) || !idRe.MatchString(b[0]) || a[0] == b[0] {
		t.Errorf("run ids %q and %q: want two distinct ids like 20261009-020000-7f3a", a[0], b[0])
	}
	if a[1] != "script" || b[1] != "script" {
		t.Errorf("triggers %q, %q; want script", a[1], b[1])
	}
	if c[0] != "mine" || c[1] != "nightly" {
		t.Errorf("given run values came back as id %q, trigger %q", c[0], c[1])
	}
}
