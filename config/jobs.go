package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/rohanthewiz/dbc/scripts"
)

// Where jobs and run records live: jobs_dir and runs_dir, resolved exactly
// as pipelines_dir is (pipelines.go) — absent, beside the scripts in
// ~/.config/dbc; relative, against the config file's directory.

// DefaultRunsKeep is how many records of each job and pipeline are kept
// when runs_keep does not say.
const DefaultRunsKeep = 200

func defaultDir(name string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return name
	}
	return filepath.Join(home, ".config", "dbc", name)
}

// DefaultJobsDir is ~/.config/dbc/jobs; DefaultRunsDir ~/.config/dbc/runs.
func DefaultJobsDir() string { return defaultDir("jobs") }
func DefaultRunsDir() string { return defaultDir("runs") }

// ResolveJobsDir turns jobs_dir as written into the directory dbc reads.
func ResolveJobsDir(raw, base string) (string, []string) {
	if strings.TrimSpace(raw) == "" {
		return absOr(DefaultJobsDir()), nil
	}
	return resolvePath("jobs_dir", raw, base)
}

// ResolveRunsDir turns runs_dir as written into the directory dbc writes.
func ResolveRunsDir(raw, base string) (string, []string) {
	if strings.TrimSpace(raw) == "" {
		return absOr(DefaultRunsDir()), nil
	}
	return resolvePath("runs_dir", raw, base)
}

// JobRef is what the argument of `dbc job run ARG` names: a file on disk,
// or one of the samples built into the binary.
type JobRef struct {
	Path    string
	Example *scripts.Job
}

// Label names the job in messages: the file's path, or "example:NAME.json".
func (r JobRef) Label() string {
	if r.Example != nil {
		return "example:" + r.Example.Name
	}
	return r.Path
}

// FileName is the job's file name ("nightly.json"), however it was found.
func (r JobRef) FileName() string {
	if r.Example != nil {
		return r.Example.Name
	}
	return filepath.Base(r.Path)
}

// Source is the job's text.
func (r JobRef) Source() (string, error) {
	if r.Example != nil {
		return r.Example.Text, nil
	}
	bs, err := os.ReadFile(r.Path)
	return string(bs), err
}

// FindJob resolves ARG as FindPipeline does: an existing file path, then
// JobsDir/ARG and ARG.json, then the built-in example.
func (c *Config) FindJob(arg string) (JobRef, error) {
	if st, err := os.Stat(arg); err == nil && !st.IsDir() {
		return JobRef{Path: arg}, nil
	}
	if !strings.ContainsRune(arg, filepath.Separator) && !strings.ContainsRune(arg, '/') {
		names := []string{arg}
		if !strings.HasSuffix(arg, ".json") {
			names = append(names, arg+".json")
		}
		for _, name := range names {
			p := filepath.Join(c.JobsDir, name)
			if st, err := os.Stat(p); err == nil && !st.IsDir() {
				return JobRef{Path: p}, nil
			}
		}
		for _, name := range names {
			if ex, ok := scripts.JobByName(name); ok {
				return JobRef{Example: &ex}, nil
			}
		}
		return JobRef{}, fmt.Errorf("no job %q: not a file here, not in %s, and not a built-in example (dbc jobs lists them)",
			arg, c.JobsDir)
	}
	return JobRef{}, fmt.Errorf("no job file %q", arg)
}
