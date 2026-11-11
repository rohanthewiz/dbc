package jobs

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rohanthewiz/dbc/pipeline"
)

// stubborn is a pipeline whose one node ignores its stop: it returns only
// when release is closed, whatever its context says — a retry loop that
// swallows the cancel's error, a driver call that never returns. started
// gets a value once it is running.
func stubborn(name string, started chan<- struct{}, release <-chan struct{}) *pipeline.Spec {
	p := pipeline.New(name)
	p.Fragment("stuck").Func(func(*pipeline.Env) (*pipeline.Batch, error) {
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
		return nil, nil
	})
	return p.Spec()
}

// A run whose node ignores Cancel is abandoned AbandonAfter later (N-268):
// its record ends failed and says why, Wait returns, and the pipeline is
// free to run again. Before, the run never ended: "already running" until
// the process exited. When the stuck node does return, nothing about the
// finished run changes, and no event follows its RunDone.
func TestEngineAbandonsARunThatIgnoresItsStop(t *testing.T) {
	te := newTestEngine(t)
	te.opt.AbandonAfter = 150 * time.Millisecond
	started, release := make(chan struct{}, 1), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	head, err := te.StartPipeline(Request{Spec: stubborn("stuck", started, release), Origin: "tab1"})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	if err = te.Cancel(head.ID); err != nil {
		t.Fatal(err)
	}
	fin := te.wait(t, head.ID) // 10 s at most: the old engine would hang here
	if fin.Status != pipeline.Failed || !strings.Contains(fin.Error, "abandoned") || !strings.Contains(fin.Error, "canceled") {
		t.Fatalf("final = %s %q, want failed, abandoned after a cancel", fin.Status, fin.Error)
	}
	if p := fin.Pipelines[0]; p.Status != pipeline.Failed || p.Fragments[0].Status != pipeline.Failed {
		t.Errorf("pipeline %s, fragment %s: want both failed", p.Status, p.Fragments[0].Status)
	}
	if last := fin.Log[len(fin.Log)-1]; last.Level != "err" || !strings.HasPrefix(last.Text, "abandoned") {
		t.Errorf("last line = %+v", last)
	}

	// let go of everything: the same pipeline, and the same origin
	again, err := te.StartPipeline(Request{Spec: example(t, "cats-report.json"), Origin: "tab1"})
	if err != nil {
		t.Fatalf("after the abandon, a new run from the tab: %v", err)
	}
	te.wait(t, again.ID)
	re := stubborn("stuck", nil, release)
	if _, err = te.StartPipeline(Request{Spec: re}); err != nil {
		t.Fatalf("after the abandon, the same pipeline: %v", err)
	}

	// the stuck node returns at last: its run stays as it was ended
	evs := len(te.events(head.ID))
	close(release)
	time.Sleep(200 * time.Millisecond)
	if r, _ := te.Get(head.ID); r.Status != pipeline.Failed || !strings.Contains(r.Error, "abandoned") {
		t.Errorf("after the node returned: %s %q", r.Status, r.Error)
	}
	if n := len(te.events(head.ID)); n != evs {
		t.Errorf("%d events after the run's RunDone, want none", n-evs)
	}
	if _, ok := te.events(head.ID)[evs-1].(*RunDone); !ok {
		t.Errorf("the run's last event is %T, want RunDone", te.events(head.ID)[evs-1])
	}
}

// A job whose step ignores its stop is abandoned once its policy timeout
// has ended the walk and AbandonAfter has passed: the timeout finishes the
// run, as the policy promises, and its overlap slot is free (a second run
// of the job starts, where "skip" would have refused it).
func TestJobTimeoutAbandonsAStuckStep(t *testing.T) {
	je := newJobEngine(t, func(o *Options) { o.AbandonAfter = 100 * time.Millisecond })
	release := make(chan struct{})
	defer close(release)
	je.act("hang", func(*pipeline.Env) error { <-release; return nil })
	spec := job(t, `{"name": "t", "pipelines": [{"id": "s", "pipeline": "hang"}],
	  "policy": {"timeout": "50ms", "overlap": "skip"}}`)
	head, err := je.StartJob(JobRequest{Spec: spec})
	if err != nil {
		t.Fatal(err)
	}
	fin := je.wait(t, head.ID)
	if fin.Status != pipeline.Failed || !strings.Contains(fin.Error, "abandoned") || !strings.Contains(fin.Error, "timed out") {
		t.Fatalf("final = %s %q, want failed: timed out, then abandoned", fin.Status, fin.Error)
	}
	if fin.Pipelines[0].Status != pipeline.Failed {
		t.Errorf("step s = %s, want failed", fin.Pipelines[0].Status)
	}
	if _, err = je.StartJob(JobRequest{Spec: spec}); err != nil {
		t.Errorf("a second run under overlap skip: %v, want it started (the first is over)", err)
	}
}

// AbandonAfter's zero is the default, and a negative one never abandons:
// such a run waits on its node as before.
func TestAbandonAfterSettings(t *testing.T) {
	if e := New(newTestEngine(t).cfg, nil, Options{}); e.opt.AbandonAfter != DefaultAbandonAfter {
		t.Errorf("default = %s, want %s", e.opt.AbandonAfter, DefaultAbandonAfter)
	}
	te := newTestEngine(t)
	te.opt.AbandonAfter = -1
	started, release := make(chan struct{}, 1), make(chan struct{})
	head, err := te.StartPipeline(Request{Spec: stubborn("never", started, release)})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	_ = te.Cancel(head.ID)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, err = te.Wait(ctx, head.ID); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("with abandoning off, Wait = %v, want it still waiting", err)
	}
	close(release)
	if fin := te.wait(t, head.ID); strings.Contains(fin.Error, "abandoned") {
		t.Errorf("once the node returned: %s %q, want it ended by the node, not abandoned", fin.Status, fin.Error)
	}
}
