package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/rohanthewiz/dbc/scripts"
)

// Where pipelines live: pipelines_dir, resolved exactly as scripts_dir is
// (scripts.go) — absent, ~/.config/dbc/pipelines beside the scripts;
// relative, against the config file's directory.

// DefaultPipelinesDir is where pipelines live when the config does not
// say.
func DefaultPipelinesDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "pipelines"
	}
	return filepath.Join(home, ".config", "dbc", "pipelines")
}

// ResolvePipelinesDir turns pipelines_dir as written into the directory
// dbc reads, by ResolveScriptsDir's rules.
func ResolvePipelinesDir(raw, base string) (string, []string) {
	if strings.TrimSpace(raw) == "" {
		return absOr(DefaultPipelinesDir()), nil
	}
	return resolvePath("pipelines_dir", raw, base)
}

// PipelineRef is what the argument of `dbc pipeline run ARG` names: a
// file on disk, or one of the samples built into the binary.
type PipelineRef struct {
	Path    string
	Example *scripts.Pipeline
}

// Label names the pipeline in messages: the file's path, or
// "example:NAME.json".
func (r PipelineRef) Label() string {
	if r.Example != nil {
		return "example:" + r.Example.Name
	}
	return r.Path
}

// Source is the pipeline's text.
func (r PipelineRef) Source() (string, error) {
	if r.Example != nil {
		return r.Example.Text, nil
	}
	bs, err := os.ReadFile(r.Path)
	return string(bs), err
}

// FindPipeline resolves ARG as FindScript does: an existing file path,
// then PipelinesDir/ARG and ARG.json, then the built-in example.
func (c *Config) FindPipeline(arg string) (PipelineRef, error) {
	if st, err := os.Stat(arg); err == nil && !st.IsDir() {
		return PipelineRef{Path: arg}, nil
	}
	if !strings.ContainsRune(arg, filepath.Separator) && !strings.ContainsRune(arg, '/') {
		names := []string{arg}
		if !strings.HasSuffix(arg, ".json") {
			names = append(names, arg+".json")
		}
		for _, name := range names {
			p := filepath.Join(c.PipelinesDir, name)
			if st, err := os.Stat(p); err == nil && !st.IsDir() {
				return PipelineRef{Path: p}, nil
			}
		}
		for _, name := range names {
			if ex, ok := scripts.PipelineByName(name); ok {
				return PipelineRef{Example: &ex}, nil
			}
		}
		return PipelineRef{}, fmt.Errorf("no pipeline %q: not a file here, not in %s, and not a built-in example (dbc pipelines lists them)",
			arg, c.PipelinesDir)
	}
	return PipelineRef{}, fmt.Errorf("no pipeline file %q", arg)
}
