package web

import (
	"strings"
	"testing"

	"github.com/rohanthewiz/dbc/workspace"
)

// TestRunSafe is N-260's backstop: a panic in a goroutine dbc web starts
// is recovered and becomes an error, with after told, rather than ending
// the process.
func TestRunSafe(t *testing.T) {
	var got error
	runSafe(nil, "a test job", func() { panic("boom") }, func(err error) { got = err })
	if got == nil || !strings.Contains(got.Error(), "internal error in a test job: boom") {
		t.Fatalf("after got %v", got)
	}
	// a panic in after itself is recovered too
	runSafe(nil, "a test job", func() { panic("boom") }, func(error) { panic("again") })
}

// TestLaunchRecoversAPanickingRun: a run's Job that panics — no known one
// does — is recovered, said in the tab's log, and abandoned: the page gets
// the run's failure and the tab runs again, where it would have stayed
// busy for good (or dbc web died).
func TestLaunchRecoversAPanickingRun(t *testing.T) {
	e := newTestEnv(t)
	id, s := e.connected()
	tb, err := e.srv.hub.get(id)
	if err != nil {
		t.Fatal(err)
	}
	st, err := tb.ws.Run([]string{"SELECT 1"}, "run")
	if err != nil {
		t.Fatal(err)
	}
	st.Job = func() workspace.Event { panic("boom") }
	e.srv.launch(tb, st)
	ev, logs := s.await(t, "run")
	if r := decodeData[runEvent](t, testEnvelope{Data: ev.Data}); r.OK {
		t.Errorf("the abandoned run reads ok: %+v", r)
	}
	if !strings.Contains(strings.Join(logs, "\n"), "internal error in run: boom") {
		t.Errorf("no word in the tab's log: %q", logs)
	}
	if tb.ws.Busy() {
		t.Fatal("the tab is still busy after the abandoned run")
	}
	e.api("POST", "/api/v1/ws/"+id+"/run", runBody("SELECT 2", 0, false), 200)
	if ev, _ = s.await(t, "run"); !decodeData[runEvent](t, testEnvelope{Data: ev.Data}).OK {
		t.Error("the next run failed")
	}
}
