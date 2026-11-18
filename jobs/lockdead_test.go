package jobs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rohanthewiz/btypedb"

	"github.com/rohanthewiz/dbc/pipeline"
	"github.com/rohanthewiz/dbc/userdata"
)

// A running record whose run lock is free has lost its writer, whatever
// its heartbeat says (N-319): read as interrupted at once, not after
// StaleAfter. A record whose lock is held, or that never had a lock file,
// is judged by its heartbeat as before.
func TestLockSaysDead(t *testing.T) {
	je := newJobEngine(t, func(o *Options) { o.RunsDir = t.TempDir() })
	runs := je.opt.RunsDir
	rec := Run{ID: "20261118-020000-abcd", Kind: KindJob, Name: "rec", Trigger: TriggerSchedule,
		Status: pipeline.Running, Started: time.Now()}
	bs, _ := json.Marshal(rec)
	if err := userdata.SaveRun(runs, KindJob, "rec", rec.ID, bs); err != nil {
		t.Fatal(err)
	}
	status := func() string {
		hs, err := je.History(userdata.RunFilter{Kind: KindJob, Name: "rec"})
		if err != nil || len(hs) != 1 {
			t.Fatalf("history = %+v %v", hs, err)
		}
		return hs[0].Status
	}

	// a fresh record and no lock file: the heartbeat says running
	if got := status(); got != string(pipeline.Running) {
		t.Fatalf("no lock file: %s, want running", got)
	}

	// its lock, held by "another process" (a second open of the file)
	lock := filepath.Join(runs, heldDir, lockJob+"rec")
	if err := os.MkdirAll(filepath.Dir(lock), 0o700); err != nil {
		t.Fatal(err)
	}
	c, err := btypedb.AcquireLock(lock)
	if err != nil {
		t.Fatal(err)
	}
	if got := status(); got != string(pipeline.Running) {
		t.Errorf("lock held: %s, want running", got)
	}
	if n, err := je.Recover(); n != 0 || err != nil {
		t.Errorf("recover with the lock held = %d %v", n, err)
	}

	// the holder dies: the OS drops its lock, and the record is dead now,
	// not StaleAfter later
	_ = c.Close()
	if got := status(); got != string(pipeline.Interrupted) {
		t.Errorf("lock free: %s, want interrupted", got)
	}
	if n, err := je.Recover(); n != 1 || err != nil {
		t.Errorf("recover = %d %v, want the record rewritten", n, err)
	}
}

// A start that meets a lock held for an instant — a liveness probe's —
// tries again rather than reading it as another holder (N-319).
func TestStartOutlastsAProbe(t *testing.T) {
	je := newJobEngine(t, func(o *Options) { o.RunsDir = t.TempDir() })
	dir := filepath.Join(je.opt.RunsDir, heldDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	c, err := btypedb.AcquireLock(filepath.Join(dir, lockJob+"probe"))
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(heldRetryGap / 2)
		_ = c.Close()
	}()
	je.Engine.mu.Lock()
	busy, _, warn := je.holdFileLocked(lockJob, "probe", "run-1")
	je.Engine.mu.Unlock()
	if busy || warn != nil {
		t.Errorf("a start refused for a probe's instant: busy %v, warn %v", busy, warn)
	}
}
