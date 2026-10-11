//go:build unix

package jobs

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/rohanthewiz/dbc/pipeline"
	"github.com/rohanthewiz/dbc/userdata"
)

// A run another process is running is stopped by interrupting that
// process when its record names it on this machine (N-183); a record that
// names none, another machine, or this very process is refused as before.
// The "process" here is a child that only waits for its interrupt.
func TestCancelSignalsTheRunsProcess(t *testing.T) {
	runs := t.TempDir()
	je := newJobEngine(t, func(o *Options) { o.RunsDir = runs })
	child := exec.Command("sleep", "60")
	if err := child.Start(); err != nil {
		t.Skip("no sleep to stand in for a run's process:", err)
	}
	done := make(chan error, 1)
	go func() { done <- child.Wait() }()
	t.Cleanup(func() { _ = child.Process.Kill() })

	plant := func(id string, pid int, host string) {
		t.Helper()
		r := Run{ID: id, Kind: KindJob, Name: "elsewhere", Trigger: TriggerCLI, Status: pipeline.Running,
			Started: time.Now(), PID: pid, Host: host}
		bs, _ := json.Marshal(r)
		if err := userdata.SaveRun(runs, KindJob, "elsewhere", id, bs); err != nil {
			t.Fatal(err)
		}
	}
	plant("20261010-020000-0001", 0, "")
	plant("20261010-020000-0002", child.Process.Pid, "some-other-machine")
	plant("20261010-020000-0003", os.Getpid(), hostname())
	for _, id := range []string{"20261010-020000-0001", "20261010-020000-0002", "20261010-020000-0003"} {
		if err := je.Cancel(id); !errors.Is(err, ErrElsewhere) {
			t.Errorf("cancel %s = %v, want ErrElsewhere", id, err)
		}
	}
	select {
	case err := <-done:
		t.Fatalf("the child was signalled for a record that does not name it here: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	plant("20261010-020000-0004", child.Process.Pid, hostname())
	if err := je.Cancel("20261010-020000-0004"); err != nil {
		t.Fatalf("cancel = %v", err)
	}
	select {
	case err := <-done:
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.Sys().(syscall.WaitStatus).Signal() != syscall.SIGINT {
			t.Errorf("the child ended %v, want by SIGINT", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the child was not interrupted")
	}
}
