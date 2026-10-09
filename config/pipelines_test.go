package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// FindPipeline: a file here, then pipelines_dir with .json optional,
// then a built-in example; and a relative pipelines_dir resolves against
// the config file.
func TestFindPipeline(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{PipelinesDir: dir}
	if err := os.WriteFile(filepath.Join(dir, "mine.json"), []byte(`{"name":"mine","fragments":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, arg := range []string{"mine", "mine.json"} {
		ref, err := cfg.FindPipeline(arg)
		if err != nil || ref.Path != filepath.Join(dir, "mine.json") {
			t.Errorf("%s: %+v %v", arg, ref, err)
		}
	}
	ref, err := cfg.FindPipeline("copy-cats")
	if err != nil || ref.Example == nil || ref.Label() != "example:copy-cats.json" {
		t.Errorf("example: %+v %v", ref, err)
	}
	if text, err := ref.Source(); err != nil || !strings.Contains(text, `"copy-cats"`) {
		t.Errorf("example source: %v", err)
	}
	if _, err = cfg.FindPipeline("nope"); err == nil || !strings.Contains(err.Error(), "dbc pipelines lists them") {
		t.Errorf("missing: %v", err)
	}
	got, _ := ResolvePipelinesDir("pipes", "/etc/dbc")
	if got != "/etc/dbc/pipes" {
		t.Errorf("relative: %s", got)
	}
	if got, _ = ResolvePipelinesDir("", ""); !strings.HasSuffix(got, filepath.Join(".config", "dbc", "pipelines")) {
		t.Errorf("default: %s", got)
	}
}
