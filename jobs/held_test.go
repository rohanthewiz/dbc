package jobs

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rohanthewiz/btypedb"

	"github.com/rohanthewiz/dbc/pipeline"
)

// The tests here stand two or three engines over one runs dir for as many
// processes. A flock belongs to the open file, not the process, so two
// engines in one process exclude each other exactly as two processes do;
// only the words differ ("elsewhere in this process", the PID being ours).

// heldEngines is n engines over one runs dir, each with pipeline "slow":
// it says it started on started, then waits for its stop.
func heldEngines(t *testing.T, n int, started chan struct{}) (string, []*jobEngine) {
	t.Helper()
	runs := t.TempDir()
	var out []*jobEngine
	for range n {
		je := newJobEngine(t, func(o *Options) { o.RunsDir = runs })
		je.act("slow", func(e *pipeline.Env) error {
			started <- struct{}{}
			<-e.Ctx.Done()
			return e.Ctx.Err()
		})
		out = append(out, je)
	}
	return runs, out
}

// fastPoll makes a queued run try for its job's lock every few
// milliseconds, for the length of the test.
func fastPoll(t *testing.T) {
	old := heldPoll
	heldPoll = 5 * time.Millisecond
	t.Cleanup(func() { heldPoll = old })
}

// eventually fails the test if cond is not true within five seconds.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !cond(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// logSays reports whether run id's log, as its events sent it, has a line
// containing part.
func (je *jobEngine) logSays(id, part string) bool {
	for _, ev := range je.events(id) {
		if l, ok := ev.(*Logged); ok && strings.Contains(l.Line.Text, part) {
			return true
		}
	}
	return false
}

// One real run of a pipeline at a time holds across processes (N-269):
// alone or as a job's step, whichever process asks, and the refusal names
// the run holding it. A preview is not held. The lock is free again once
// the run ends, and its file stays.
func TestPipelineHeldAcrossEngines(t *testing.T) {
	started := make(chan struct{}, 4)
	runs, es := heldEngines(t, 2, started)
	a, b := es[0], es[1]

	head, err := a.StartPipeline(Request{Spec: a.pipes["slow"]})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	_, err = b.StartPipeline(Request{Spec: b.pipes["slow"]})
	if !errors.Is(err, ErrBusy) || BusyRun(err) != head.ID ||
		!strings.Contains(err.Error(), "slow is already running elsewhere in this process (pid "+strconv.Itoa(os.Getpid())+", run "+head.ID+")") {
		t.Errorf("the pipeline in another engine: %v (run %q)", err, BusyRun(err))
	}
	// (a preview skips an action fragment, so it ends at once, by itself)
	pv, err := b.StartPipeline(Request{Spec: b.pipes["slow"], PreviewRows: 5})
	if err != nil {
		t.Fatalf("a preview beside another engine's run: %v", err)
	}
	b.wait(t, pv.ID)

	// as b's job step, beside a's run of it alone: the step fails, saying why
	oneStep := job(t, `{"name": "o", "pipelines": [{"id": "s", "pipeline": "slow"}]}`)
	jr, err := b.StartJob(JobRequest{Spec: oneStep})
	if err != nil {
		t.Fatal(err)
	}
	if fin := b.wait(t, jr.ID); fin.Status != pipeline.Failed ||
		!strings.Contains(fin.Pipelines[0].Error, "slow is already running elsewhere in this process") {
		t.Errorf("the step beside another engine's run: %s %q", fin.Status, fin.Pipelines[0].Error)
	}

	_ = a.Cancel(head.ID)
	a.wait(t, head.ID)
	// free again: b's job runs it now, and a's run of it alone is refused,
	// naming the job's step
	jr, err = b.StartJob(JobRequest{Spec: oneStep})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	_, err = a.StartPipeline(Request{Spec: a.pipes["slow"]})
	if !errors.Is(err, ErrBusy) || BusyRun(err) != jr.ID || !strings.Contains(err.Error(), "running as step s of job o elsewhere in this process") {
		t.Errorf("the pipeline beside another engine's job: %v (run %q)", err, BusyRun(err))
	}
	_ = b.Cancel(jr.ID)
	b.wait(t, jr.ID)

	// never removed: unlinking a lock file races the next flock of it
	for _, f := range []string{lockPipeline + "slow.lock", lockJob + "o.lock"} {
		if _, err := os.Stat(filepath.Join(runs, heldDir, f)); err != nil {
			t.Errorf("lock file %s: %v", f, err)
		}
	}
}

// A job's overlap policy holds across processes (N-269). Under "skip" a
// run while another engine's goes is refused, naming it. Under "queue" it
// waits for that run to end, and "one waits at most" holds across them
// too: a third run is refused, from any engine, the first's included.
func TestJobOverlapAcrossEngines(t *testing.T) {
	fastPoll(t)
	started := make(chan struct{}, 4)
	_, es := heldEngines(t, 3, started)
	a, b, c := es[0], es[1], es[2]
	one := `{"name": "o", "pipelines": [{"id": "s", "pipeline": "slow"}], "policy": {"overlap": "%s"}}`
	skip, queue := job(t, fmt.Sprintf(one, "skip")), job(t, fmt.Sprintf(one, "queue"))

	first, err := a.StartJob(JobRequest{Spec: skip})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	_, err = b.StartJob(JobRequest{Spec: skip})
	if !errors.Is(err, ErrBusy) || BusyRun(err) != first.ID ||
		!strings.Contains(err.Error(), "job o is already running elsewhere in this process (pid ") ||
		!strings.Contains(err.Error(), "overlap: skip") {
		t.Errorf("skip: %v (run %q)", err, BusyRun(err))
	}

	second, err := b.StartJob(JobRequest{Spec: queue})
	if err != nil || second.Status != pipeline.Queued {
		t.Fatalf("queue: %s %v", second.Status, err)
	}
	// its record is written, and its log says what it waits for
	eventually(t, "the queued run to say it waits", func() bool {
		return b.logSays(second.ID, "waiting for the run before it to end, elsewhere in this process")
	})
	for name, je := range map[string]*jobEngine{"another engine": c, "the running run's own engine": a} {
		_, err = je.StartJob(JobRequest{Spec: queue})
		if !errors.Is(err, ErrBusy) || BusyRun(err) != second.ID ||
			!strings.Contains(err.Error(), "already waiting for the one before it, elsewhere in this process") {
			t.Errorf("a second waiter from %s: %v (run %q)", name, err, BusyRun(err))
		}
	}
	select {
	case <-started:
		t.Fatal("the queued run started while the first ran")
	case <-time.After(50 * time.Millisecond):
	}

	_ = a.Cancel(first.ID)
	a.wait(t, first.ID)
	<-started // the queued run took the lock and runs
	if r, _ := b.Get(second.ID); r.Status != pipeline.Running {
		t.Errorf("second = %s", r.Status)
	}
	// running, it has given up its place in line: one may wait behind it
	third, err := c.StartJob(JobRequest{Spec: queue})
	if err != nil || third.Status != pipeline.Queued {
		t.Fatalf("a waiter behind the second: %s %v", third.Status, err)
	}
	_ = c.Cancel(third.ID)
	if fin := c.wait(t, third.ID); fin.Status != pipeline.Canceled || !strings.Contains(fin.Error, "while it waited") {
		t.Errorf("third = %s %q", fin.Status, fin.Error)
	}
	_ = b.Cancel(second.ID)
	b.wait(t, second.ID)
}

// A lock held by something with no record to name it — a process that
// has not written its record yet, or one keeping none — is refused with
// its PID alone. A run queued behind it starts once it is let go, and one
// canceled while it waits gives up its place in line.
func TestHeldByAHolderWithNoRecord(t *testing.T) {
	fastPoll(t)
	started := make(chan struct{}, 4)
	runs, es := heldEngines(t, 1, started)
	a := es[0]
	if err := os.MkdirAll(filepath.Join(runs, heldDir), 0o700); err != nil {
		t.Fatal(err)
	}
	take := func(name string) io.Closer {
		t.Helper()
		l, err := btypedb.AcquireLock(filepath.Join(runs, heldDir, name))
		if err != nil {
			t.Fatal(err)
		}
		return l
	}
	pid := "(pid " + strconv.Itoa(os.Getpid()) + ")"

	pl := take(lockPipeline + "slow")
	_, err := a.StartPipeline(Request{Spec: a.pipes["slow"]})
	if !errors.Is(err, ErrBusy) || BusyRun(err) != "" || !strings.Contains(err.Error(), "slow is already running elsewhere in this process "+pid) {
		t.Errorf("pipeline: %v", err)
	}
	_ = pl.Close()

	one := `{"name": "o", "pipelines": [{"id": "s", "pipeline": "slow"}], "policy": {"overlap": "%s"}}`
	jl := take(lockJob + "o")
	_, err = a.StartJob(JobRequest{Spec: job(t, fmt.Sprintf(one, "skip"))})
	if !errors.Is(err, ErrBusy) || !strings.Contains(err.Error(), "job o is already running elsewhere in this process (pid "+
		strconv.Itoa(os.Getpid())+"; overlap: skip)") {
		t.Errorf("job, skip: %v", err)
	}
	queue := job(t, fmt.Sprintf(one, "queue"))
	waiter, err := a.StartJob(JobRequest{Spec: queue})
	if err != nil || waiter.Status != pipeline.Queued {
		t.Fatalf("job, queue: %s %v", waiter.Status, err)
	}
	eventually(t, "the waiter to say it waits", func() bool { return a.logSays(waiter.ID, "waiting for the run before it to end") })
	_ = a.Cancel(waiter.ID)
	if fin := a.wait(t, waiter.ID); fin.Status != pipeline.Canceled {
		t.Errorf("a waiter canceled = %s", fin.Status)
	}
	// its place in line is free: another may wait, and runs once the
	// holder lets go
	waiter, err = a.StartJob(JobRequest{Spec: queue})
	if err != nil || waiter.Status != pipeline.Queued {
		t.Fatalf("job, queue again: %s %v", waiter.Status, err)
	}
	_ = jl.Close()
	<-started
	_ = a.Cancel(waiter.ID)
	a.wait(t, waiter.ID)
}
