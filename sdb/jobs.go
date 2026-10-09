package sdb

import (
	"time"

	"github.com/rohanthewiz/serr"

	"github.com/rohanthewiz/dbc/db"
)

// Jobs from a script: s.RunJob runs a job — a DAG of pipelines with one
// root, fan-out and fan-in (package jobs) — by name, and waits for it, so
// a job can be one step of a script:
//
//	run, err := s.RunJob("nightly", sdb.Params{"min_age": "3"})
//	if err != nil {
//		return err // the job failed: run says which pipeline, and why
//	}
//	for _, p := range run.Pipelines {
//		s.Print("%s: %s, %d rows", p.ID, p.Status, p.Rows())
//	}
//
// The job runs on the host's jobs engine when it has one (dbc web: the run
// shows among the others, and its overlap rules hold), else on an engine
// of the script's own for the length of the call. Its record is written
// like any run's, with trigger "script".
//
// Package sdb cannot import package jobs (jobs runs pipelines on sessions
// of this package), so the job runner is handed in: by the host per
// session (WithJobs), or once for all by package jobs (DefaultJobRunner).

// JobRun is a job's run as s.RunJob returns it: its id, its outcome, and
// each pipeline's stats in the job's order.
type JobRun struct {
	ID        string
	Job       string
	Status    string // succeeded, failed, canceled
	Error     string
	Started   time.Time
	Ended     time.Time
	Pipelines []JobPipeline
}

// JobPipeline is one pipeline of a job's run: its step id in the job and
// its stats (Status, Fragments, Rows(), …). A step skipped because one
// before it failed has status skipped.
type JobPipeline struct {
	ID string
	RunStats
}

// JobRunner runs a job for RunJob and waits for it. The host provides it.
type JobRunner func(s *S, name string, params Params) (*JobRun, error)

// DefaultJobRunner runs s.RunJob for a session whose host set none. Package
// jobs sets it when it is linked in (every dbc binary): a private engine
// over the session's connections and paths.
var DefaultJobRunner JobRunner

// WithJobs sets the runner s.RunJob uses: the host's engine. The host sets
// it; a script does not.
func (s *S) WithJobs(r JobRunner) *S {
	s.jobs = r
	return s
}

// Manager is the connections the session runs on. For the host's helpers
// (a job runner builds its sessions over it), not for scripts.
func (s *S) Manager() *db.Manager { return s.mgr }

// RunJob runs the job called name — a file in the jobs directory (.json
// may be left off) or a built-in example — with params overriding its
// own, and waits for it to end. Stop (Ctrl+K) cancels it as it does a
// query. The run comes back however it ended; the error is non-nil when
// it did not succeed.
func (s *S) RunJob(name string, params Params) (*JobRun, error) {
	r := s.jobs
	if r == nil {
		r = DefaultJobRunner
	}
	if r == nil {
		return nil, serr.New("this host cannot run jobs", "job", name)
	}
	return r(s, name, params)
}
