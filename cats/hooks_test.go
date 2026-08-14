package cats

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// waitForRequests polls the collector a fake socket fills, because reports
// are sent on their own goroutines and land after ReportState returns.
func waitForRequests(t *testing.T, got *recorder, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got.len() >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d hook requests, got %d", n, got.len())
}

func hookParamsOf(t *testing.T, req map[string]any) map[string]any {
	t.Helper()
	p, ok := req["params"].(map[string]any)
	if !ok {
		t.Fatalf("request carried no params: %v", req)
	}
	return p
}

// seqOf reads the idempotency token at full width. See serveOnce's UseNumber
// for why this cannot go through float64.
func seqOf(t *testing.T, req map[string]any) uint64 {
	t.Helper()
	num, ok := hookParamsOf(t, req)["seq"].(json.Number)
	if !ok {
		t.Fatalf("request carried no seq: %v", req)
	}
	n, err := strconv.ParseUint(num.String(), 10, 64)
	if err != nil {
		t.Fatalf("seq %q is not a uint64: %v", num, err)
	}
	return n
}

func TestReportStateSendsTheHookEnvelope(t *testing.T) {
	path := sockPath(t, "h.sock")
	got := serveOnce(t, path, nil) // fire and forget: nothing replies

	r := NewReporter(path, "w1:p3")
	r.ReportState(StateWorking, "query")
	waitForRequests(t, got, 1)

	req := got.at(t, 0)
	if req["method"] != "pane.report_agent" {
		t.Errorf("method = %v", req["method"])
	}
	p := hookParamsOf(t, req)
	if p["pane_id"] != "w1:p3" {
		t.Errorf("pane_id = %v — the hook API addresses the PUBLIC handle", p["pane_id"])
	}
	// An unreserved source is what gets real state authority; "cats:dbc"
	// would be treated as one of cats' own detection-driven integrations and
	// have its state ignored.
	if p["source"] != "dbc" || p["agent"] != "dbc" {
		t.Errorf("source/agent = %v/%v, want dbc/dbc", p["source"], p["agent"])
	}
	if p["state"] != StateWorking || p["custom_status"] != "query" {
		t.Errorf("state/status = %v/%v", p["state"], p["custom_status"])
	}
	if _, ok := p["seq"]; !ok {
		t.Error("a report must carry a seq or the server cannot drop stale ones")
	}
}

func TestReleaseWithdrawsTheClaim(t *testing.T) {
	path := sockPath(t, "h.sock")
	got := serveOnce(t, path, nil)

	r := NewReporter(path, "w1:p3")
	// Release blocks on the dial and write, unlike ReportState — that is what
	// lets it run on the way out, where a posted goroutine would be killed
	// before it dialed. What it does NOT wait for is the server's own
	// bookkeeping, so the arrival is still polled for.
	r.Release()
	waitForRequests(t, got, 1)

	if got.at(t, 0)["method"] != "pane.release_agent" {
		t.Errorf("method = %v", got.at(t, 0)["method"])
	}
	p := hookParamsOf(t, got.at(t, 0))
	if p["pane_id"] != "w1:p3" || p["source"] != "dbc" {
		t.Errorf("release did not name the pane and source: %v", p)
	}
}

// Reports go out on goroutines and can reach the socket out of order, so the
// number is stamped in call order and the server drops anything stale.
func TestReportSeqFollowsTransitionOrder(t *testing.T) {
	path := sockPath(t, "h.sock")
	got := serveOnce(t, path, nil)

	r := NewReporter(path, "w1:p3")
	r.ReportState(StateWorking, "query")
	r.ReportState(StateIdle, "")
	waitForRequests(t, got, 2)

	// Indexed by state rather than by arrival order — the point of the seq is
	// precisely that arrival order is not guaranteed.
	seqs := map[string]uint64{}
	for _, req := range got.all() {
		state, _ := hookParamsOf(t, req)["state"].(string)
		seqs[state] = seqOf(t, req)
	}
	if seqs[StateWorking] >= seqs[StateIdle] {
		t.Errorf("working(%d) should carry a lower seq than idle(%d) — the number "+
			"is stamped in transition order, not send order",
			seqs[StateWorking], seqs[StateIdle])
	}
}

// The server's high-water mark lives on the PANE and outlives this process,
// so a counter starting at 1 would have every report from a re-launched dbc
// silently dropped. The clock seed is what prevents that.
func TestReportSeqSurvivesARestart(t *testing.T) {
	path := sockPath(t, "h.sock")
	got := serveOnce(t, path, nil)

	first := NewReporter(path, "w1:p3")
	first.ReportState(StateWorking, "query")
	waitForRequests(t, got, 1)

	time.Sleep(2 * time.Millisecond) // the clock has to move between instances
	second := NewReporter(path, "w1:p3")
	second.ReportState(StateIdle, "")
	waitForRequests(t, got, 2)

	a, b := seqOf(t, got.at(t, 0)), seqOf(t, got.at(t, 1))
	if b <= a {
		t.Errorf("a restarted reporter must outrank the previous one: %d then %d", a, b)
	}
}

// A nil reporter is the Tier-0 form, so call sites need no availability check.
func TestNilReporterIsInert(t *testing.T) {
	if r := NewReporter("", "w1:p3"); r != nil {
		t.Error("no socket should produce no reporter")
	}
	if r := NewReporter("/tmp/x.sock", ""); r != nil {
		t.Error("no pane handle should produce no reporter")
	}
	var r *Reporter
	r.ReportState(StateIdle, "") // must not panic
	r.Release()
}

// A dead socket must cost nothing: no panic, no block, no error to handle.
func TestReportToADeadSocketIsSilent(t *testing.T) {
	r := NewReporter(sockPath(t, "gone.sock"), "w1:p3")
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.ReportState(StateWorking, "query")
		r.Release()
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("reporting to a dead socket blocked")
	}
}

func TestClampStatus(t *testing.T) {
	if got := clampStatus("  query  "); got != "query" {
		t.Errorf("clampStatus should trim, got %q", got)
	}
	// A connection name reaches this field, so an escape sequence in one must
	// not reach the host's UI.
	if got := clampStatus("run\x1b]0;pwned\x07"); strings.ContainsRune(got, 0x1b) {
		t.Errorf("control bytes survived: %q", got)
	}
	long := strings.Repeat("a", 100)
	if got := clampStatus(long); len(got) > customStatusMax {
		t.Errorf("status not clamped: %d bytes", len(got))
	}
	// The cap is in bytes, and cutting there naively would split a rune —
	// cats truncates byte-naively, so doing it correctly here is what keeps
	// the server's own cut from ever firing.
	multi := strings.Repeat("é", 40)
	got := clampStatus(multi)
	if len(got) > customStatusMax {
		t.Errorf("multibyte status not clamped: %d bytes", len(got))
	}
	if !utf8.ValidString(got) {
		t.Errorf("clamped status is not valid UTF-8: %q", got)
	}
}
