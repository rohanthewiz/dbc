package workspace

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rohanthewiz/dbc/db"
)

// A script that does not heed Stop is let go a while after it (N-310):
// the run lands as stopped, with a note that says the script runs on, the
// tab is free for the next run, and what the script still shows is
// dropped. It used to hold the tab busy until the tab closed.
func TestScriptIgnoringStopIsAbandoned(t *testing.T) {
	prev := scriptAbandonAfter
	scriptAbandonAfter = 300 * time.Millisecond
	t.Cleanup(func() { scriptAbandonAfter = prev })

	w := newTestWorkspace(t)
	path := filepath.Join(t.TempDir(), "stubborn.go")
	src := `//go:build ignore

package main

import (
	"time"

	"github.com/rohanthewiz/dbc/sdb"
)

// Run sleeps through a Stop: it never asks s.Ctx().
func Run(s *sdb.S) error {
	time.Sleep(3 * time.Second)
	r, err := s.Query("demo-sqlite", "SELECT 'late' AS x")
	if err == nil {
		s.Show(r)
	}
	return nil
}
`
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := w.RunScript(path)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan *RunDone, 1)
	go func() { done <- st.Job().(*RunDone) }()
	time.Sleep(100 * time.Millisecond)
	w.Cancel()

	var ev *RunDone
	select {
	case ev = <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the stopped script's run did not land: the tab is still busy")
	}
	if !errors.Is(ev.Err, db.ErrCanceled) {
		t.Errorf("err = %v, want a stop", ev.Err)
	}
	var notes []string
	for _, n := range ev.Notes {
		notes = append(notes, n.Text)
	}
	if all := strings.Join(notes, "\n"); !strings.Contains(all, "not heeding the stop") || !strings.Contains(all, "stopped after") {
		t.Errorf("notes:\n%s", all)
	}
	// the tab takes the next run at once
	if got := run(t, w, "SELECT 1 AS one"); got.Err != nil {
		t.Fatalf("the next run: %v", got.Err)
	}
	// and the script's late show, once it wakes, lands nowhere
	time.Sleep(3500 * time.Millisecond)
	if r := w.LastResult(); r == nil || r.Columns[0] != "one" {
		t.Errorf("the abandoned script's show landed: %+v", r)
	}
}
