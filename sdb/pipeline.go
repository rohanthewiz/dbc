package sdb

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/rohanthewiz/dbc/pipeline"
	"github.com/rohanthewiz/dbc/scripts"
	"github.com/rohanthewiz/serr"
)

// The pipeline half of the script API (package pipeline, which this file
// hands to scripts under the sdb name): rows in batches from a source,
// through transforms, into sinks — a fragment — and fragments in order.
// A script can run a saved pipeline by name, or build one in Go, with its
// own funcs as nodes:
//
//	st, err := s.RunPipelineNamed("orders-nightly", sdb.PipelineOpts{Params: sdb.Params{"days": "7"}})
//
//	p := sdb.NewPipeline("adhoc")
//	f := p.Fragment("orders").Batch(2000)
//	f.Node("sql.read", sdb.Cfg{"conn": "prod", "query": "SELECT id, email FROM users"})
//	f.ThenFunc(func(b *sdb.Batch) (*sdb.Batch, error) {
//		c := b.Col("email")
//		for i := range b.Rows {
//			if e, ok := b.Rows[i][c].(string); ok { b.Rows[i][c] = strings.ToLower(e) }
//		}
//		return b, nil
//	})
//	f.Then("sql.write", sdb.Cfg{"conn": "local", "table": "users_copy", "create": "true", "truncate": "true"})
//	st, err := s.RunPipeline(p, sdb.PipelineOpts{})
//
// S satisfies pipeline.Host, so a Go node's e.S is this session: e.S.Query,
// e.S.Print and the rest work inside a node as they do in a script.

var _ pipeline.Host = (*S)(nil)

// Batch is the rows between two nodes of a fragment: columns and rows,
// with helpers to find, add, drop and filter.
type Batch = pipeline.Batch

// Col is one column of a Batch: its name and the source's type name.
type Col = pipeline.Col

// Env is what a Go node runs in: the session (S), the parameters, the
// values earlier fragments published, and a logger.
type Env = pipeline.Env

// Cfg is a node's settings, every value a string: sdb.Cfg{"conn": "prod"}.
type Cfg = pipeline.Config

// Params are a run's parameter overrides: sdb.Params{"days": "7"}.
type Params = map[string]string

// Pipeline is a pipeline being built in Go (sdb.NewPipeline); Fragment
// is one of its fragments being filled.
type Pipeline = pipeline.Builder
type Fragment = pipeline.FragmentBuilder

// PipelineSpec is a pipeline as data — what a pipeline file holds.
type PipelineSpec = pipeline.Spec

// PipelineOpts shape a run: Params, a Log line sink, Progress, one
// Fragment only, or PreviewRows for a preview that writes nothing.
type PipelineOpts = pipeline.Options

// RunStats is a pipeline's run: each fragment's status, rows and node
// counters, and the outcome. Its String is a one-line summary.
type RunStats = pipeline.RunStats

// FragmentStats and NodeStats are the parts of RunStats.
type FragmentStats = pipeline.FragmentStats
type NodeStats = pipeline.NodeStats

// Paths are the directories a session resolves names in: a script's
// (script.run), a pipeline's (RunPipelineNamed), a job's (RunJob) — and
// where a job run there leaves its record. The host sets them.
type Paths struct {
	ScriptsDir   string
	PipelinesDir string
	JobsDir      string
	RunsDir      string
}

// NewBatch makes a batch of cols with rows; ColsOf pairs names with the
// source's type names ("" for unknown). For a go.source's first batch.
func NewBatch(cols []Col, rows [][]any) *Batch { return pipeline.NewBatch(cols, rows) }
func ColsOf(names, dbTypes []string) []Col     { return pipeline.ColsOf(names, dbTypes) }

// NewPipeline starts a pipeline called name, to fill with Fragment and run
// with s.RunPipeline.
func NewPipeline(name string) *Pipeline { return pipeline.New(name) }

// ParsePipeline reads a pipeline from its JSON text.
func ParsePipeline(text string) (*PipelineSpec, error) { return pipeline.Parse(text) }

// WithPaths tells the session where scripts and pipelines live, for
// running them by name. The host sets it; a script does not.
func (s *S) WithPaths(p Paths) *S {
	s.paths = p
	return s
}

// Paths is where the session resolves script and pipeline names.
func (s *S) Paths() Paths { return s.paths }

// RunPipeline runs a pipeline built with NewPipeline. Every fragment runs
// in order, each sink committing at its fragment's end; the stats come
// back complete even on error. Stop (Ctrl+K) cancels the run as it does a
// query.
func (s *S) RunPipeline(p *Pipeline, opt PipelineOpts) (*RunStats, error) {
	return s.RunPipelineSpec(p.Spec(), opt)
}

// RunPipelineSpec runs a pipeline given as data (ParsePipeline, or a
// literal).
func (s *S) RunPipelineSpec(spec *PipelineSpec, opt PipelineOpts) (*RunStats, error) {
	return pipeline.Run(s.Ctx(), s, spec, opt)
}

// RunPipelineNamed runs a saved pipeline: a file path, a name in the
// pipelines directory (.json optional), or a built-in example.
func (s *S) RunPipelineNamed(name string, opt PipelineOpts) (*RunStats, error) {
	spec, err := s.LoadPipeline(name)
	if err != nil {
		return nil, err
	}
	return s.RunPipelineSpec(spec, opt)
}

// LoadPipeline finds a pipeline the way RunPipelineNamed does and parses it.
func (s *S) LoadPipeline(name string) (*PipelineSpec, error) {
	text, err := s.findPipeline(name)
	if err != nil {
		return nil, err
	}
	return pipeline.Parse(text)
}

// findPipeline resolves a pipeline name to its text: an existing file
// path first, then the pipelines directory, then the examples.
func (s *S) findPipeline(arg string) (string, error) {
	if st, err := os.Stat(arg); err == nil && !st.IsDir() {
		bs, err := os.ReadFile(arg)
		return string(bs), err
	}
	if !strings.ContainsAny(arg, `/\`) {
		names := []string{arg}
		if !strings.HasSuffix(arg, ".json") {
			names = append(names, arg+".json")
		}
		if s.paths.PipelinesDir != "" {
			for _, n := range names {
				if bs, err := os.ReadFile(filepath.Join(s.paths.PipelinesDir, n)); err == nil {
					return string(bs), nil
				}
			}
		}
		for _, n := range names {
			if ex, ok := scripts.PipelineByName(n); ok {
				return ex.Text, nil
			}
		}
	}
	return "", serr.New("no such pipeline", "name", arg, "dir", s.paths.PipelinesDir)
}
