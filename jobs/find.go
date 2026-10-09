package jobs

import (
	"errors"
	"io/fs"
	"slices"
	"strings"

	"github.com/rohanthewiz/serr"

	"github.com/rohanthewiz/dbc/config"
	"github.com/rohanthewiz/dbc/pipeline"
	"github.com/rohanthewiz/dbc/scripts"
	"github.com/rohanthewiz/dbc/userdata"
)

// Names to specs. A job's step names its pipeline, and a trigger names its
// job, by the name in the directory — never a path: the user's file first,
// then the built-in example of that name, as `dbc pipeline run NAME` finds
// one. ".json" may be left off.

// specName is name with .json on.
func specName(name string) string {
	name = strings.TrimSpace(name)
	if !strings.HasSuffix(name, ".json") {
		name += ".json"
	}
	return name
}

// PipelineFinder resolves pipeline names in dir, then the examples.
func PipelineFinder(dir string) func(name string) (*pipeline.Spec, error) {
	return func(name string) (*pipeline.Spec, error) {
		file := specName(name)
		text, _, err := userdata.ReadPipeline(dir, file)
		if errors.Is(err, fs.ErrNotExist) || dir == "" {
			ex, ok := scripts.PipelineByName(file)
			if !ok {
				return nil, serr.New("no such pipeline (not in pipelines_dir, not an example)", "pipeline", name)
			}
			text, err = ex.Text, nil
		}
		if err != nil {
			return nil, err
		}
		spec, err := pipeline.Parse(text)
		if err != nil {
			return nil, serr.Wrap(err, "pipeline", name)
		}
		return spec, nil
	}
}

// LoadJob resolves a job name in dir, then the examples, and parses it. It
// returns the job and its file name ("nightly.json").
func LoadJob(dir, name string) (*Spec, string, error) {
	file := specName(name)
	text, _, err := userdata.ReadJob(dir, file)
	if errors.Is(err, fs.ErrNotExist) || dir == "" {
		ex, ok := scripts.JobByName(file)
		if !ok {
			return nil, file, serr.New("no such job (not in jobs_dir, not an example)", "job", name)
		}
		text, err = ex.Text, nil
	}
	if err != nil {
		return nil, file, err
	}
	spec, err := ParseJob(text)
	if err != nil {
		return nil, file, serr.Wrap(err, "job", name)
	}
	return spec, file, nil
}

// CheckConns is the connection names a pipeline's conn fields may use, for
// pipeline.CheckOptions.Conns: every configured connection, and any name
// the spec uses that resolves another way — a "<conn>/<database>" onto
// another database of a configured server (config.ConnByName) — so the
// check does not call a working name unknown. dbc web's check and the
// TUI's (after $EDITOR) share it.
func CheckConns(cfg *config.Config, spec *pipeline.Spec) []string {
	var names []string
	for _, c := range cfg.Conns() {
		names = append(names, c.Name)
	}
	for _, f := range spec.Fragments {
		for _, n := range f.Nodes {
			p, ok := pipeline.Lookup(n.Plugin)
			if !ok {
				continue
			}
			for k, v := range n.Cfg {
				fd, ok := p.Field(k)
				v = strings.TrimSpace(v)
				if !ok || fd.Type != pipeline.FieldConn || v == "" || slices.Contains(names, v) {
					continue
				}
				if _, found := cfg.ConnByName(v); found {
					names = append(names, v)
				}
			}
		}
	}
	return names
}
