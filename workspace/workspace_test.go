package workspace

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rohanthewiz/dbc/config"
	"github.com/rohanthewiz/dbc/db"
	"github.com/rohanthewiz/dbc/model"
)

// These tests exercise the workspace's rules directly, with no UI: start a
// request, run its Job as a UI would, and assert on the event and the state
// it landed. The database is a fresh, seeded in-memory SQLite per test.

var dbSeq atomic.Int64

const demo = "demo-sqlite"

func memDSN() string {
	return fmt.Sprintf("file:wstest%d?mode=memory&cache=shared", dbSeq.Add(1))
}

// newTestWorkspace builds a workspace over a seeded demo database and
// connects to it, as a UI's startup would.
func newTestWorkspace(t *testing.T) *Workspace {
	t.Helper()
	cfg := &config.Config{
		MaxRows: 1000, MaxDisplayRows: 2000, AIContextRows: config.DefaultAIContextRows,
		DefaultConnection: demo,
		Connections:       []config.Connection{{Name: demo, Driver: "sqlite", DSN: memDSN()}},
	}
	mgr := db.NewManager(cfg)
	t.Cleanup(mgr.Close)
	if err := db.SeedDemo(mgr, demo); err != nil {
		t.Fatalf("seed: %v", err)
	}
	w := New(cfg, mgr, nil, Options{})
	t.Cleanup(w.Close)
	ev := w.Connect(w.Active()).Job().(*Connected)
	if ev.Err != nil || w.Catalog() == nil {
		t.Fatalf("connect: %v", ev.Err)
	}
	return w
}

// run starts stmts and runs the Job to completion.
func run(t *testing.T, w *Workspace, stmts ...string) *RunDone {
	t.Helper()
	st, err := w.RunStmts(stmts, "query")
	if err != nil {
		t.Fatalf("run %v: %v", stmts, err)
	}
	return st.Job().(*RunDone)
}

func refusal(t *testing.T, err error, want Reason) {
	t.Helper()
	var r *Refusal
	if !errors.As(err, &r) || r.Reason != want {
		t.Fatalf("err = %v, want a refusal with reason %d", err, want)
	}
}

// ---------------------------------------------------------------------------
// Picking statements
// ---------------------------------------------------------------------------

func TestPick(t *testing.T) {
	text := "SELECT 1 AS a;\nSELECT 2 AS b;\nSELECT 3 AS c;"
	for _, tc := range []struct {
		name string
		ed   Editor
		want []string
		tag  string
	}{
		{"caret in the second", Editor{Text: text, Caret: 17}, []string{"SELECT 2 AS b"}, "statement 2/3"},
		{"one statement", Editor{Text: "SELECT 1"}, []string{"SELECT 1"}, "query"},
		{"selection of two", Editor{Text: text, Selection: "SELECT 1 AS a;\nSELECT 2 AS b;"},
			[]string{"SELECT 1 AS a", "SELECT 2 AS b"}, "selection (2 statements)"},
		{"blank selection falls back to the caret", Editor{Text: text, Caret: 0, Selection: "  "},
			[]string{"SELECT 1 AS a"}, "statement 1/3"},
		{"empty", Editor{Text: "  \n"}, nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, tag := Pick(tc.ed)
			if strings.Join(got, "|") != strings.Join(tc.want, "|") || tag != tc.tag {
				t.Errorf("Pick = %q, %q; want %q, %q", got, tag, tc.want, tc.tag)
			}
		})
	}
	if all, tag := PickAll(text); len(all) != 3 || tag != "all 3 statements" {
		t.Errorf("PickAll = %q, %q", all, tag)
	}
	if r := StmtRange(text, 17); r[0] != 15 || r[1] <= r[0] {
		t.Errorf("StmtRange = %v, want the second statement's range", r)
	}
	if r := StmtRange("SELECT 1", 3); r != [2]int{} {
		t.Errorf("StmtRange of one statement = %v, want none", r)
	}
}

// ---------------------------------------------------------------------------
// The run slot
// ---------------------------------------------------------------------------

// A run lands its outcome before its Job returns: the slot is free and the
// last statement and result are set, whether or not a UI draws the event.
func TestRunLandsItsOutcome(t *testing.T) {
	w := newTestWorkspace(t)
	st, err := w.RunEditor(Editor{Text: "SELECT id, name FROM cats ORDER BY id"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !w.Busy() || w.RunTag() != "query" {
		t.Fatalf("busy = %v, tag = %q while the job is pending", w.Busy(), w.RunTag())
	}
	if len(st.Notes) != 1 || !strings.Contains(st.Notes[0].Text, "running query on demo-sqlite") {
		t.Errorf("notes = %+v", st.Notes)
	}
	ev := st.Job().(*RunDone)
	if ev.Err != nil || ev.Stale || ev.Result == nil || len(ev.Result.Rows) != 8 {
		t.Fatalf("event = %+v", ev)
	}
	if w.Busy() {
		t.Error("the slot is still held after the job returned")
	}
	if w.LastResult() != ev.Result || w.LastStmt() != "SELECT id, name FROM cats ORDER BY id" || w.LastErr() != "" {
		t.Errorf("last* = %q / %q / %p", w.LastStmt(), w.LastErr(), w.LastResult())
	}
	if _, ok := w.Ticking(st.Gen); ok {
		t.Error("Ticking should report the run over")
	}
}

// ONE RUN AT A TIME: a second request is refused in words while the first
// is in flight — but still recorded in the history.
func TestBusyRefusal(t *testing.T) {
	w := newTestWorkspace(t)
	first, err := w.RunStmts([]string{"SELECT 1"}, "query")
	if err != nil {
		t.Fatal(err)
	}
	for name, try := range map[string]func() (Start, error){
		"run":     func() (Start, error) { return w.RunStmts([]string{"SELECT 2"}, "query") },
		"explain": func() (Start, error) { return w.Explain("SELECT 3", "", false) },
		"tables":  w.ListTables,
	} {
		st, err := try()
		refusal(t, err, Busy)
		if st.Job != nil || !strings.Contains(err.Error(), "busy — query is still running (Ctrl+K stops it)") {
			t.Errorf("%s: job = %v, err = %v", name, st.Job != nil, err)
		}
	}
	if got := w.History().Recent(); len(got) == 0 || got[0].SQL != "SELECT 2" {
		t.Errorf("a refused run should still be recorded; history = %+v", got)
	}
	first.Job()
	if _, err := w.RunStmts([]string{"SELECT 4"}, "query"); err != nil {
		t.Errorf("after the first run landed: %v", err)
	}
}

func TestRefusals(t *testing.T) {
	w := newTestWorkspace(t)
	_, err := w.RunEditor(Editor{Text: "  "}, false)
	refusal(t, err, Nothing)
	_, err = w.ExplainEditor(Editor{Text: "SELECT 1; SELECT 2", Selection: "SELECT 1; SELECT 2"}, false)
	refusal(t, err, Invalid)

	w.mu.Lock()
	w.active = ""
	w.mu.Unlock()
	_, err = w.Run([]string{"SELECT 1"}, "query")
	refusal(t, err, NoConnection)
	_, err = w.ListTables()
	refusal(t, err, NoConnection)
}

// An outcome from a run already written off lands nothing.
func TestStragglerIsDropped(t *testing.T) {
	w := newTestWorkspace(t)
	run(t, w, "SELECT 1 AS kept")
	kept := w.LastResult()

	// a run whose generation is no longer current — the state a
	// written-off run's late outcome finds
	w.mu.Lock()
	w.busy, w.runGen = true, 7
	w.mu.Unlock()
	ev := &RunDone{Tag: "old", Stmts: []string{"SELECT 2"}, Result: &model.Result{Columns: []string{"late"}}}
	w.landRun(ev, 6)
	if !ev.Stale {
		t.Error("the straggler was not marked stale")
	}
	if w.LastResult() != kept || w.LastStmt() != "SELECT 1 AS kept" || !w.Busy() {
		t.Errorf("the straggler landed: last = %q, busy = %v", w.LastStmt(), w.Busy())
	}
}

// Multi-statement runs stop at the first failure, naming it, and the error
// is remembered for the assistant.
func TestRunStopsAtTheFirstFailure(t *testing.T) {
	w := newTestWorkspace(t)
	ev := run(t, w, "SELECT 1", "SELEC nonsense", "CREATE TEMP TABLE never (x INT)")
	if ev.Err == nil || !strings.Contains(w.LastErr(), "2/3") || !strings.HasPrefix(ev.Status, "error after") {
		t.Fatalf("err = %v, lastErr = %q, status = %q", ev.Err, w.LastErr(), ev.Status)
	}
	if ev.Result != nil {
		t.Error("a failed run should publish no result")
	}
	ev = run(t, w, "SELECT count(*) FROM temp.sqlite_master WHERE name = 'never'")
	if ev.Result.Rows[0][0] != "0" {
		t.Error("the statement after the failure ran")
	}
	if w.LastErr() != "" {
		t.Errorf("a successful run should clear lastErr: %q", w.LastErr())
	}
}

// CANCEL REACHES THE SERVER: a stopped run lands as stopped, not failed.
func TestCancelStopsTheRun(t *testing.T) {
	w := newTestWorkspace(t)
	slow := "WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n) SELECT count(*) FROM n"
	st, err := w.RunStmts([]string{slow}, "query")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan *RunDone, 1)
	go func() { done <- st.Job().(*RunDone) }()
	time.Sleep(100 * time.Millisecond) // let the statement reach the engine

	n, status := w.Cancel()
	if !strings.Contains(n.Text, "stopping query") || status != "stopping query…" {
		t.Errorf("cancel = %+v, %q", n, status)
	}
	select {
	case ev := <-done:
		if !errors.Is(ev.Err, db.ErrCanceled) || !strings.HasPrefix(ev.Status, "stopped after") {
			t.Errorf("err = %v, status = %q", ev.Err, ev.Status)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the canceled run never landed")
	}
	if w.LastErr() != "" || w.Busy() {
		t.Errorf("a stop is not a failure: lastErr = %q, busy = %v", w.LastErr(), w.Busy())
	}
	if n, _ := w.Cancel(); n.Text != "nothing is running" {
		t.Errorf("idle cancel = %q", n.Text)
	}
}

// A result that is itself a plan is handed over as one.
func TestTypedExplainBecomesThePlan(t *testing.T) {
	w := newTestWorkspace(t)
	ev := run(t, w, "EXPLAIN QUERY PLAN SELECT * FROM cats WHERE age > 3")
	if ev.Plan == nil || w.Plan() != ev.Plan {
		t.Fatalf("plan = %v, want the result recognized as a plan", ev.Plan)
	}
}

func TestExplainLandsAPlan(t *testing.T) {
	w := newTestWorkspace(t)
	run(t, w, "SELEC oops")
	if w.LastErr() == "" {
		t.Fatal("setup: the run should have failed")
	}
	st, err := w.Explain("SELEC oops", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if ev := st.Job().(*ExplainDone); ev.Err == nil {
		t.Fatal("explaining a syntax error should fail")
	}

	run(t, w, "SELECT name FROM cats")
	st, err = w.Explain("SELECT name FROM cats", "statement 1/2", false)
	if err != nil {
		t.Fatal(err)
	}
	if st.Tag != "explain (statement 1/2)" {
		t.Errorf("tag = %q", st.Tag)
	}
	ev := st.Job().(*ExplainDone)
	if ev.Err != nil || ev.Plan == nil || w.Plan() != ev.Plan {
		t.Fatalf("event = %+v", ev)
	}
	// a plan is not a result: the last statement stays the last RUN one
	if w.LastStmt() != "SELECT name FROM cats" {
		t.Errorf("lastStmt = %q", w.LastStmt())
	}
}

// ---------------------------------------------------------------------------
// The pinned session
// ---------------------------------------------------------------------------

// BEGIN, work and COMMIT across runs land on one pinned connection.
func TestSessionCarriesAcrossRuns(t *testing.T) {
	w := newTestWorkspace(t)
	run(t, w, "CREATE TEMP TABLE scratch (x INT)")
	run(t, w, "INSERT INTO scratch VALUES (7)")
	if ev := run(t, w, "SELECT x FROM scratch"); ev.Err != nil || ev.Result.Rows[0][0] != "7" {
		t.Fatalf("temp table did not survive between runs: %+v", ev)
	}
	if conn, stateful := w.Session(); conn != demo || !stateful {
		t.Errorf("session = %q, stateful %v", conn, stateful)
	}
}

// killSession closes the pinned session's connection behind the workspace's
// back, the way a server-side idle timeout would: w.sess still points at
// it, and the next statement on it fails as a bad connection.
func killSession(t *testing.T, w *Workspace) {
	t.Helper()
	if w.sess == nil {
		t.Fatal("no session to kill")
	}
	if err := w.sess.Close(); err != nil {
		t.Fatalf("kill: %v", err)
	}
}

// A dead session that only ever ran queries is replaced and the statement
// retried once: nothing was lost, so the user need not hear about it.
func TestDeadStatelessSessionIsReplaced(t *testing.T) {
	w := newTestWorkspace(t)
	ctx := context.Background()
	if _, err := w.runOnSession(ctx, demo, "SELECT 1"); err != nil {
		t.Fatalf("select: %v", err)
	}
	killSession(t, w)
	if _, err := w.runOnSession(ctx, demo, "SELECT 2"); err != nil {
		t.Fatalf("retry on a fresh session failed: %v", err)
	}
}

// A dead session that held state is not: replaying the statement on a fresh
// session would run it outside the transaction the user thinks is open.
func TestDeadStatefulSessionFailsLoudly(t *testing.T) {
	w := newTestWorkspace(t)
	ctx := context.Background()
	if _, err := w.runOnSession(ctx, demo, "BEGIN"); err != nil {
		t.Fatalf("begin: %v", err)
	}
	killSession(t, w)

	_, err := w.runOnSession(ctx, demo, "COMMIT")
	if !errors.Is(err, db.ErrSessionLost) {
		t.Fatalf("err = %v, want ErrSessionLost", err)
	}
	if w.sess != nil {
		t.Error("the dead session is still pinned")
	}
	// the next run starts clean
	if _, err = w.runOnSession(ctx, demo, "SELECT 1"); err != nil {
		t.Fatalf("run after a lost session: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Connecting
// ---------------------------------------------------------------------------

func addConn(w *Workspace, c config.Connection) {
	w.cfg.Connections = append(w.cfg.Connections, c)
}

// Switching connections releases the old session — rolling back what it
// left open — through the event's Release job, and says so.
func TestSwitchReleasesTheOldSession(t *testing.T) {
	w := newTestWorkspace(t)
	addConn(w, config.Connection{Name: "other", Driver: "sqlite", DSN: memDSN()})
	run(t, w, "BEGIN")
	run(t, w, "DELETE FROM cats")

	if st := w.Switch(demo); st.Job != nil {
		t.Error("switching to the active, loaded connection should do nothing")
	}
	st := w.Switch("other")
	if len(st.Notes) != 1 || st.Notes[0].Text != "connecting to other…" {
		t.Errorf("notes = %+v", st.Notes)
	}
	ev := st.Job().(*Connected)
	if ev.Err != nil || !ev.Changed || w.Active() != "other" || ev.Release == nil {
		t.Fatalf("event = %+v, active = %q", ev, w.Active())
	}
	rel, _ := ev.Release().(*SessionReleased)
	if rel == nil || !rel.Stateful || len(rel.Notes) != 1 || !strings.Contains(rel.Notes[0].Text, "left demo-sqlite") {
		t.Fatalf("release = %+v", rel)
	}
	if conn, _ := w.Session(); conn != "" {
		t.Errorf("session on %q still pinned", conn)
	}
	res, err := w.mgr.Run(demo, "SELECT count(*) FROM cats")
	if err != nil || res.Rows[0][0] == "0" {
		t.Errorf("the DELETE survived the switch: %v %v", res, err)
	}
}

// blackholeDSN is a postgres DSN for a loopback listener that accepts and
// never answers, so a connect to it waits in the handshake until canceled.
func blackholeDSN(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	var held []net.Conn
	var mu sync.Mutex
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			held = append(held, c)
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		for _, c := range held {
			_ = c.Close()
		}
		mu.Unlock()
	})
	return "postgres://u:p@" + ln.Addr().String() + "/x?sslmode=disable"
}

func async(j Job) <-chan Event {
	ch := make(chan Event, 1)
	go func() { ch <- j() }()
	return ch
}

func await(t *testing.T, ch <-chan Event) Event {
	t.Helper()
	select {
	case ev := <-ch:
		return ev
	case <-time.After(5 * time.Second):
		t.Fatal("timed out")
		return nil
	}
}

// Cancel stops a connect still dialing, which lands as canceled.
func TestCancelConnect(t *testing.T) {
	w := newTestWorkspace(t)
	addConn(w, config.Connection{Name: "slow", Driver: "postgres", DSN: blackholeDSN(t)})
	done := async(w.Switch("slow").Job)
	if name, ok := w.Connecting(); !ok || name != "slow" {
		t.Fatalf("connecting = %q, %v", name, ok)
	}
	if n, _ := w.Cancel(); n.Text != "canceling connect to slow…" {
		t.Errorf("cancel = %q", n.Text)
	}
	ev := await(t, done).(*Connected)
	if !errors.Is(ev.Err, db.ErrCanceled) || ev.Stale || ev.Status != "connect canceled" {
		t.Errorf("event = %+v", ev)
	}
	if _, ok := w.Connecting(); ok || w.Active() != demo {
		t.Errorf("after the cancel: connecting %v, active %q", ok, w.Active())
	}
}

// A newer connect supersedes one still in flight: the older is canceled,
// and its outcome — landing last — is stale and switches nothing.
func TestNewerConnectSupersedesOlder(t *testing.T) {
	w := newTestWorkspace(t)
	addConn(w, config.Connection{Name: "slow", Driver: "postgres", DSN: blackholeDSN(t)})
	addConn(w, config.Connection{Name: "other", Driver: "sqlite", DSN: memDSN()})
	slow := async(w.Switch("slow").Job)
	if ev := w.Switch("other").Job().(*Connected); ev.Err != nil || w.Active() != "other" {
		t.Fatalf("other: %+v", ev)
	}
	ev := await(t, slow).(*Connected)
	if !ev.Stale || len(ev.Notes) != 0 || w.Active() != "other" {
		t.Errorf("superseded connect: %+v, active %q", ev, w.Active())
	}
}

// ---------------------------------------------------------------------------
// The assistant's context
// ---------------------------------------------------------------------------

// The last result goes along only for the statement that produced it, with
// the grid's hidden columns and sort applied only when the view is of that
// very result.
func TestChatContext(t *testing.T) {
	w := newTestWorkspace(t)
	w.cfg.Connections[0].AIRows = true
	stmt := "SELECT id, name, breed FROM cats ORDER BY id"
	res := run(t, w, stmt).Result

	view := GridView{Result: res, Hidden: []int{2}, SortCol: 1, SortDesc: true, Order: []int{3, 2, 1, 0}}
	ctx, refs := w.ChatContext("why?", Editor{Text: stmt}, view)
	if ctx.Query != stmt || len(ctx.Rows) != 8 || ctx.SortedBy != "name" || !ctx.SortDesc ||
		len(ctx.Hidden) != 1 || len(ctx.Order) != 4 {
		t.Errorf("ctx = %+v", ctx)
	}
	if len(refs) != 1 || len(ctx.Tables) != 1 {
		t.Errorf("the cats table should be named for its schema: %+v", refs)
	}
	view.Order[0] = 99
	if ctx.Order[0] == 99 {
		t.Error("Order must be copied: a grid re-sorts in place")
	}

	// a view of some other result lends nothing
	ctx, _ = w.ChatContext("", Editor{Text: stmt}, GridView{Result: &model.Result{}, SortCol: 1, Hidden: []int{0}})
	if ctx.Hidden != nil || ctx.SortedBy != "" {
		t.Errorf("a stale view leaked: %+v", ctx)
	}

	// a different statement under the caret: no rows, no error
	ctx, _ = w.ChatContext("", Editor{Text: "SELECT 1"}, view)
	if ctx.Query != "SELECT 1" || ctx.Rows != nil {
		t.Errorf("ctx = %+v", ctx)
	}

	// an empty editor: the last run stands in
	ctx, _ = w.ChatContext("", Editor{}, view)
	if ctx.Query != stmt || ctx.Rows == nil {
		t.Errorf("ctx = %+v", ctx)
	}
}

// A script's s.Print and s.Show reach the sink mid-run, in order, and each
// s.Show publishes its result; the Job lands the script as a run.
func TestScriptEventsReachTheSink(t *testing.T) {
	var mu sync.Mutex
	var got []Event
	w := newTestWorkspace(t)
	w.sink = func(e Event) { mu.Lock(); got = append(got, e); mu.Unlock() }

	st, err := w.RunScript("../testdata/show_two.go")
	if err != nil {
		t.Fatal(err)
	}
	if st.Tag != "script show_two.go" {
		t.Errorf("tag = %q", st.Tag)
	}
	ev := st.Job().(*RunDone)
	if !ev.Script || ev.Err != nil || w.Busy() {
		t.Fatalf("event = %+v", ev)
	}
	mu.Lock()
	defer mu.Unlock()
	var kinds []string
	for _, e := range got {
		switch e := e.(type) {
		case *ScriptPrint:
			kinds = append(kinds, "print:"+e.Text)
		case *ScriptShow:
			kinds = append(kinds, fmt.Sprintf("show:%d", len(e.Result.Rows)))
		}
	}
	if len(kinds) != 4 || !strings.HasPrefix(kinds[0], "print:breed Tabby") || !strings.HasPrefix(kinds[1], "show:") {
		t.Errorf("sink got %q", kinds)
	}
	if last, _ := got[3].(*ScriptShow); last == nil || w.LastResult() != last.Result {
		t.Error("the last s.Show should be the last result")
	}
}
