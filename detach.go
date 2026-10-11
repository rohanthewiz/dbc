package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/rohanthewiz/serr"

	"github.com/rohanthewiz/dbc/config"
	"github.com/rohanthewiz/dbc/export"
	"github.com/rohanthewiz/dbc/jobs"
	"github.com/rohanthewiz/dbc/userdata"
)

// `dbc job run --wait=false` (N-181): start the run and return its id at
// once, for an outside trigger (a CI step, another scheduler) that then
// polls `dbc run show ID` or stops it with `dbc run cancel ID`.
//
// A run lives in the engine of the process that runs it, so the command
// cannot just return: the run would die with it. It starts a copy of
// itself instead, detached — its own session (Setsid; on Windows a new
// process group, no console), its output to nowhere (the record keeps the
// log) — and passes the run's id down, picked here so it can be printed
// before the run exists:
//
//	dbc job run --wait=false nightly
//	  │ checks the job (as a run would, so a broken one fails here, loudly)
//	  │ id := jobs.NewRunID()
//	  ├──► dbc job run nightly   env DBC_RUN_ID=id, detached ──► runs it, writes the record
//	  │ waits for the record (or for the child to exit without one)
//	  └─ prints the id, exits 0; the child runs on
//
// The child is an ordinary `dbc job run` with the id given: Signalable, so
// `dbc run cancel` and dbc web's ■ Stop interrupt it (jobs/signal.go).

// detachedRunEnv carries the parent's run id to the detached child; set,
// it makes `dbc job run` run in the foreground under that id whatever
// --wait says.
const detachedRunEnv = "DBC_RUN_ID"

// detachStart is how long the parent waits for the child's record.
const detachStart = 30 * time.Second

// startDetached starts job ref (checked) in a detached child and reports
// its run id, or fails saying why it did not start.
func startDetached(ctx context.Context, cfg *config.Config, name string, f export.Format) error {
	if cfg.RunsDir == "" {
		usage("--wait=false needs runs_dir: the run's record is how it is followed")
	}
	exe, err := os.Executable()
	if err != nil {
		fail(serr.Wrap(err), "could not find dbc's own binary to start the run with")
	}
	id := jobs.NewRunID()
	null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		fail(serr.Wrap(err), "could not open "+os.DevNull)
	}
	defer null.Close()
	child := exec.Command(exe, os.Args[1:]...)
	child.Env = append(os.Environ(), detachedRunEnv+"="+id)
	child.Stdin, child.Stdout, child.Stderr = null, null, null
	child.SysProcAttr = detachAttr()
	if err := child.Start(); err != nil {
		fail(serr.Wrap(err), "could not start the run")
	}
	exited := make(chan error, 1)
	go func() { exited <- child.Wait() }()

	deadline := time.After(detachStart)
	for {
		if _, _, err := userdata.ReadRun(cfg.RunsDir, id); err == nil {
			break
		}
		select {
		case err := <-exited:
			code := 0
			if ee, ok := err.(*exec.ExitError); ok {
				code = ee.ExitCode()
			}
			fmt.Fprintf(os.Stderr, "the run did not start (dbc job run exited %d) — run it with --wait to see why\n", code)
			os.Exit(1)
		case <-deadline:
			fmt.Fprintf(os.Stderr, "the run has not started after %s — dbc runs --job %s shows it if it does\n", detachStart, name)
			os.Exit(1)
		case <-ctx.Done():
			return nil
		case <-time.After(50 * time.Millisecond):
		}
	}
	// the child is on its own now: this process neither waits for it nor
	// takes it down when it exits
	_ = child.Process.Release()
	if f == export.JSON {
		writeRunJSON(map[string]string{"id": id, "kind": jobs.KindJob, "name": name, "status": "running"}, "the run")
		return nil
	}
	fmt.Printf("started run %s (job %s) — dbc run show %s · dbc run cancel %s\n", id, name, id, id)
	return nil
}
