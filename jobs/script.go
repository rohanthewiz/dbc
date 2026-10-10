package jobs

import (
	"context"
	"fmt"
	"time"

	"github.com/rohanthewiz/serr"

	"github.com/rohanthewiz/dbc/config"
	"github.com/rohanthewiz/dbc/pipeline"
	"github.com/rohanthewiz/dbc/sdb"
)

// s.RunJob's two runners (see sdb/jobs.go):
//
//	the host's engine   Engine.ScriptRunner, handed to a session with
//	                    WithJobs — dbc web's script tabs and the TUI's
//	                    scripts: the job runs on the process's engine,
//	                    beside its other runs, under its overlap rules, and
//	                    its lines go where every run's lines go
//	a private engine    DefaultJobRunner, set here for every host that set
//	                    none — headless `dbc script`: an engine over the
//	                    session's connections and paths for the length of
//	                    the call, whose lines are the script's own Print
//	                    lines, each with its step

func init() {
	sdb.DefaultJobRunner = runOnOwnEngine
}

// ScriptRunner is the engine as a session's job runner.
func (e *Engine) ScriptRunner() sdb.JobRunner {
	return func(s *sdb.S, name string, params sdb.Params) (*sdb.JobRun, error) {
		return e.runForScript(s, name, params)
	}
}

// runOnOwnEngine runs a job for a session with no engine of its host's.
func runOnOwnEngine(s *sdb.S, name string, params sdb.Params) (*sdb.JobRun, error) {
	p := s.Paths()
	cfg := &config.Config{ScriptsDir: p.ScriptsDir, PipelinesDir: p.PipelinesDir, JobsDir: p.JobsDir, RunsDir: p.RunsDir,
		FilesDir: p.FilesDir}
	e := New(cfg, s.Manager(), Options{RunsDir: p.RunsDir, Sink: func(ev Event) {
		if l, ok := ev.(*Logged); ok {
			s.Print("%s", scriptLine(name, l))
		}
	}})
	defer e.Close(30 * time.Second)
	return e.runForScript(s, name, params)
}

// scriptLine is a job's log line as a script's Print shows it: "[step] …".
func scriptLine(job string, l *Logged) string {
	if l.Pipeline == "" {
		return l.Line.Text
	}
	return "[" + l.Pipeline + "] " + l.Line.Text
}

// runForScript starts the job named name, waits for it under the session's
// context — Stop cancels the job — and hands its outcome back as a JobRun.
func (e *Engine) runForScript(s *sdb.S, name string, params map[string]string) (*sdb.JobRun, error) {
	spec, file, err := LoadJob(e.cfg.JobsDir, name)
	if err != nil {
		return nil, err
	}
	head, err := e.StartJob(JobRequest{Spec: spec, Params: params, Trigger: TriggerScript, Source: file})
	if err != nil {
		return nil, err
	}
	ctx := s.Ctx()
	stopped := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = e.Cancel(head.ID)
		case <-stopped:
		}
	}()
	fin, err := e.Wait(context.Background(), head.ID)
	close(stopped)
	if err != nil {
		return nil, err
	}
	out := &sdb.JobRun{ID: fin.ID, Job: fin.Name, Status: string(fin.Status), Error: fin.Error,
		Started: fin.Started, Ended: fin.Ended}
	for _, p := range fin.Pipelines {
		out.Pipelines = append(out.Pipelines, sdb.JobPipeline{ID: p.ID, RunStats: p.RunStats})
	}
	switch fin.Status {
	case pipeline.Succeeded:
		return out, nil
	case pipeline.Canceled:
		// a Stop: the error says canceled, as a stopped query's does, so
		// sdb.IsCanceled tells it from a failure
		return out, serr.Wrap(context.Canceled, "job", fin.Name, "run", fin.ID)
	}
	return out, serr.New(fmt.Sprintf("job %s %s: %s", fin.Name, fin.Status, fin.Error), "run", fin.ID)
}
