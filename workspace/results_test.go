package workspace

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/rohanthewiz/dbc/config"
)

// These pin down results.go's rules: a result set per connection, a run
// replacing the current tab unless it is pinned, the result_tabs cap, and
// the run's target fixed when it starts.

// otherConn adds an empty in-memory SQLite connection named "other".
func otherConn(w *Workspace) {
	addConn(w, config.Connection{Name: "other", Driver: "sqlite", DSN: memDSN()})
}

// switchTo connects w to name and fails the test unless it landed.
func switchTo(t *testing.T, w *Workspace, name string) {
	t.Helper()
	if ev := w.Switch(name).Job().(*Connected); ev.Err != nil || w.Active() != name {
		t.Fatalf("switch to %s: %+v", name, ev)
	}
}

// tabIDs lists the active set's tab ids, and the current one.
func tabIDs(w *Workspace) (ids []int, cur int) {
	tabs, at := w.ResultTabs()
	cur = -1
	for i, t := range tabs {
		ids = append(ids, t.ID)
		if i == at {
			cur = t.ID
		}
	}
	return ids, cur
}

// Each connection keeps its own results: switching shows the new
// connection's (nothing, the first time), and switching back shows the old
// one's again — the result, the last statement, the error and the plan.
func TestResultSetPerConnection(t *testing.T) {
	w := newTestWorkspace(t)
	otherConn(w)
	run(t, w, "SELECT name FROM cats")
	onDemo, demoSeq := w.LastResultSeq()
	if onDemo == nil || demoSeq == 0 {
		t.Fatal("no result on demo")
	}
	if st, err := w.Explain("SELECT name FROM cats", "", false); err != nil {
		t.Fatalf("explain refused: %v", err)
	} else if done := st.Job().(*ExplainDone); done.Err != nil {
		t.Fatal(done.Err)
	}
	if w.Plan() == nil {
		t.Fatal("no plan on demo")
	}

	switchTo(t, w, "other")
	if w.LastResult() != nil || w.LastStmt() != "" || w.Plan() != nil {
		t.Errorf("other shows demo's: result %v, stmt %q, plan %v", w.LastResult() != nil, w.LastStmt(), w.Plan() != nil)
	}
	if tabs, cur := w.ResultTabs(); len(tabs) != 0 || cur != -1 {
		t.Errorf("other's tabs = %+v, %d", tabs, cur)
	}
	if ev := run(t, w, "SELECT nope"); ev.Err == nil || w.LastErr() == "" {
		t.Fatalf("a failing run on other: %+v", ev)
	}
	run(t, w, "SELECT 2 AS two")
	onOther := w.LastResult()

	switchTo(t, w, demo)
	if r, seq := w.LastResultSeq(); r != onDemo || seq != demoSeq {
		t.Errorf("back on demo: result %p seq %d, want %p seq %d", r, seq, onDemo, demoSeq)
	}
	if w.LastStmt() != "SELECT name FROM cats" || w.LastErr() != "" || w.Plan() == nil {
		t.Errorf("back on demo: stmt %q, err %q, plan %v", w.LastStmt(), w.LastErr(), w.Plan() != nil)
	}
	switchTo(t, w, "other")
	if w.LastResult() != onOther || w.LastStmt() != "SELECT 2 AS two" {
		t.Errorf("back on other: stmt %q", w.LastStmt())
	}

	// disconnected, the pane is on no connection's set
	if _, st, err := w.Disconnect(); err != nil {
		t.Fatal(err)
	} else if st.Job != nil {
		st.Job()
	}
	if w.LastResult() != nil {
		t.Error("a disconnected tab still shows a result")
	}
}

// A run replaces the result in the current tab — same tab, a new seq — and
// a pinned tab makes the next run open a new one, leaving the pinned
// result where it was.
func TestRunReplacesUnlessPinned(t *testing.T) {
	w := newTestWorkspace(t)
	run(t, w, "SELECT 1 AS a")
	ids, cur := tabIDs(w)
	_, seq1 := w.LastResultSeq()
	run(t, w, "SELECT 2 AS a")
	ids2, cur2 := tabIDs(w)
	_, seq2 := w.LastResultSeq()
	if len(ids) != 1 || len(ids2) != 1 || cur2 != cur || seq2 == seq1 {
		t.Fatalf("rerun: tabs %v → %v, cur %d → %d, seq %d → %d", ids, ids2, cur, cur2, seq1, seq2)
	}
	if tabs, _ := w.ResultTabs(); tabs[0].Title != "SELECT 2 AS a" || tabs[0].Stmt != "SELECT 2 AS a" {
		t.Errorf("title = %q", tabs[0].Title)
	}

	if err := w.PinResultTab(cur, true); err != nil {
		t.Fatal(err)
	}
	pinned := w.LastResult()
	run(t, w, "SELECT 3 AS a")
	ids3, cur3 := tabIDs(w)
	if len(ids3) != 2 || cur3 == cur || ids3[0] != cur {
		t.Fatalf("after a pin: tabs %v, cur %d", ids3, cur3)
	}
	if err := w.ShowResultTab(cur); err != nil || w.LastResult() != pinned {
		t.Errorf("the pinned tab lost its result: %v", err)
	}
	if tabs, _ := w.ResultTabs(); !tabs[0].Pinned || tabs[1].Pinned {
		t.Errorf("pins = %v, %v", tabs[0].Pinned, tabs[1].Pinned)
	}

	// an app run is titled by its tag, not its generated SQL; it lands in
	// the unpinned current tab like any run
	_ = w.ShowResultTab(cur3)
	st, err := w.ShowColumns("cats")
	if err != nil {
		t.Fatal(err)
	}
	st.Job()
	if tabs, at := w.ResultTabs(); len(tabs) != 2 || tabs[at].ID != cur3 || tabs[at].Title != "columns cats" {
		t.Errorf("columns: %+v at %d", tabs, at)
	}
}

// The set holds result_tabs tabs. A run that needs a new tab drops the
// oldest unpinned one to make room; with every tab pinned it is refused
// before it starts, and a refusal costs nothing — the slot stays free.
func TestResultTabCap(t *testing.T) {
	w := newTestWorkspace(t)
	w.cfg.ResultTabs = 3
	for i := range 3 {
		run(t, w, "SELECT "+strconv.Itoa(i+1)+" AS n")
		_, cur := tabIDs(w)
		if err := w.PinResultTab(cur, true); err != nil {
			t.Fatal(err)
		}
	}
	ids, _ := tabIDs(w)
	if len(ids) != 3 {
		t.Fatalf("tabs = %v", ids)
	}
	_, err := w.RunStmts([]string{"SELECT 4"}, "query")
	refusal(t, err, Invalid)
	if !strings.Contains(err.Error(), "pinned") || w.Busy() {
		t.Errorf("refusal %q, busy %v", err, w.Busy())
	}
	if _, err := w.RunScript("../testdata/show_two.go"); err == nil {
		t.Error("a script with nowhere to show was not refused")
	}

	// unpin the first: the next run (the current tab is still pinned)
	// opens a new tab, dropping that one to stay at 3
	if err := w.PinResultTab(ids[0], false); err != nil {
		t.Fatal(err)
	}
	run(t, w, "SELECT 4 AS n")
	got, cur := tabIDs(w)
	if len(got) != 3 || got[0] != ids[1] || got[1] != ids[2] || cur != got[2] {
		t.Errorf("after the cap: tabs %v (cur %d), was %v", got, cur, ids)
	}
	if r := w.LastResult(); r == nil || r.Rows[0][0] != "4" {
		t.Errorf("current result = %v", r)
	}
}

// The tab a run lands in is the one current when it STARTED: browsing to
// another tab while a query runs must not get that tab overwritten.
func TestRunTargetFixedAtStart(t *testing.T) {
	w := newTestWorkspace(t)
	run(t, w, "SELECT 1 AS a")
	first, _ := tabIDs(w)
	if err := w.PinResultTab(first[0], true); err != nil {
		t.Fatal(err)
	}
	run(t, w, "SELECT 2 AS a") // a second tab, unpinned and current
	ids, second := tabIDs(w)

	st, err := w.RunStmts([]string{"SELECT 3 AS a"}, "query")
	if err != nil {
		t.Fatal(err)
	}
	// mid-run, look back at the pinned first tab
	if err := w.ShowResultTab(first[0]); err != nil {
		t.Fatalf("showing a tab mid-run: %v", err)
	}
	st.Job()
	got, cur := tabIDs(w)
	if len(got) != len(ids) || cur != second {
		t.Fatalf("landed in %d (tabs %v), want the run's own tab %d", cur, got, second)
	}
	if w.LastResult().Rows[0][0] != "3" {
		t.Errorf("current = %v", w.LastResult().Rows)
	}
	_ = w.ShowResultTab(first[0])
	if w.LastResult().Rows[0][0] != "1" {
		t.Errorf("the browsed tab was overwritten: %v", w.LastResult().Rows)
	}

	// the run's tab closed mid-run: it lands in a new one
	_ = w.ShowResultTab(second)
	st, err = w.RunStmts([]string{"SELECT 4 AS a"}, "query")
	if err != nil {
		t.Fatal(err)
	}
	if err := w.CloseResultTab(second); err != nil {
		t.Fatal(err)
	}
	st.Job()
	if got, cur = tabIDs(w); len(got) != 2 || cur == second || w.LastResult().Rows[0][0] != "4" {
		t.Errorf("after closing its tab: tabs %v, cur %d", got, cur)
	}
}

// A run that started on one connection lands in that connection's set,
// even when the tab switched away before it ended.
func TestRunLandsOnItsOwnConnection(t *testing.T) {
	w := newTestWorkspace(t)
	otherConn(w)
	st, err := w.RunStmts([]string{"SELECT 7 AS n"}, "query")
	if err != nil {
		t.Fatal(err)
	}
	switchTo(t, w, "other")
	ev := st.Job().(*RunDone)
	if ev.Conn != demo || ev.Result == nil {
		t.Fatalf("event = %+v", ev)
	}
	if w.LastResult() != nil {
		t.Error("demo's result landed on other's set")
	}
	switchTo(t, w, demo)
	if r := w.LastResult(); r == nil || r.Rows[0][0] != "7" {
		t.Errorf("demo's set = %v", r)
	}
}

// Closing the current tab shows its right-hand neighbour (its left, at the
// end); closing the last leaves the pane empty. Ids never repeat, and the
// seq of a result is stable while it stays the same result.
func TestCloseResultTabs(t *testing.T) {
	w := newTestWorkspace(t)
	for i := range 4 {
		run(t, w, "SELECT "+strconv.Itoa(i)+" AS n")
		_, cur := tabIDs(w)
		_ = w.PinResultTab(cur, true)
	}
	ids, _ := tabIDs(w)
	_ = w.ShowResultTab(ids[1])
	_, seq := w.LastResultSeq()
	_ = w.ShowResultTab(ids[2])
	_ = w.ShowResultTab(ids[1])
	if _, again := w.LastResultSeq(); again != seq {
		t.Errorf("seq moved on a revisit: %d → %d", seq, again)
	}

	if err := w.CloseResultTab(ids[1]); err != nil {
		t.Fatal(err)
	}
	if got, cur := tabIDs(w); len(got) != 3 || cur != ids[2] {
		t.Errorf("closed the current: tabs %v, cur %d, want %d", got, cur, ids[2])
	}
	_ = w.ShowResultTab(ids[3])
	_ = w.CloseResultTab(ids[3])
	if _, cur := tabIDs(w); cur != ids[2] {
		t.Errorf("closed the last: cur %d, want %d", cur, ids[2])
	}
	_ = w.CloseResultTab(ids[0]) // left of the current: it stays current
	if _, cur := tabIDs(w); cur != ids[2] {
		t.Errorf("closed one to the left: cur %d, want %d", cur, ids[2])
	}
	if err := w.CloseResultTab(ids[0]); err == nil {
		t.Error("a closed tab closed twice")
	}
	_ = w.CloseResultTab(ids[2])
	if tabs, cur := w.ResultTabs(); len(tabs) != 0 || cur != -1 || w.LastResult() != nil {
		t.Errorf("all closed: %d tabs, cur %d", len(tabs), cur)
	}
	run(t, w, "SELECT 9")
	if got, _ := tabIDs(w); got[0] <= ids[3] {
		t.Errorf("an id was reused: %v after %v", got, ids)
	}

	// Close unpinned: the pinned stay, the current moves to the last of them
	_ = w.PinResultTab(got(w), true)
	run(t, w, "SELECT 10")
	run(t, w, "SELECT 11") // replaces 10: still two tabs
	if n := w.CloseUnpinnedResultTabs(); n != 1 {
		t.Errorf("closed %d unpinned, want 1", n)
	}
	if tabs, cur := w.ResultTabs(); len(tabs) != 1 || cur != 0 || !tabs[0].Pinned {
		t.Errorf("after close unpinned: %+v, cur %d", tabs, cur)
	}
}

// got is the current tab's id.
func got(w *Workspace) int {
	_, cur := tabIDs(w)
	return cur
}

// A script's shows land in one tab — the current one, when unpinned — and
// its switcher is that tab's: another tab has none, and the script's tab
// keeps its shows while another is on screen.
func TestScriptShowsLandInOneTab(t *testing.T) {
	w := newTestWorkspace(t)
	run(t, w, "SELECT 1 AS a")
	_ = w.PinResultTab(got(w), true)
	pinned := got(w)

	path := filepath.Join(t.TempDir(), "three.go")
	src := `//go:build ignore

package main

import "github.com/rohanthewiz/dbc/sdb"

func Run(s *sdb.S) error {
	for i := 1; i <= 3; i++ {
		r, err := s.Query("demo-sqlite", "SELECT ? AS i", i)
		if err != nil {
			return err
		}
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
	if ev := st.Job().(*RunDone); ev.Err != nil {
		t.Fatal(ev.Err)
	}
	tabs, cur := w.ResultTabs()
	if len(tabs) != 2 || tabs[cur].ID == pinned || tabs[cur].Shows != 3 || tabs[cur].Title != "script three.go" {
		t.Fatalf("tabs = %+v, cur %d", tabs, cur)
	}
	script := tabs[cur].ID
	if n, at, _ := w.ScriptResults(); n != 3 || at != 2 {
		t.Errorf("ScriptResults = %d, %d", n, at)
	}
	_ = w.ShowScriptResult(0)
	_ = w.ShowResultTab(pinned)
	if n, _, _ := w.ScriptResults(); n != 0 {
		t.Errorf("the pinned run's tab has %d shows", n)
	}
	_ = w.ShowResultTab(script)
	if n, at, _ := w.ScriptResults(); n != 3 || at != 0 || w.LastResult().Rows[0][0] != "1" {
		t.Errorf("back on the script's tab: %d, %d", n, at)
	}

	// a run replaces the script's (unpinned) tab, shows and all
	run(t, w, "SELECT 5")
	if n, _, _ := w.ScriptResults(); n != 0 || got(w) != script {
		t.Errorf("after a run: %d shows, cur %d", n, got(w))
	}
}

// A renamed connection's results follow it; a removed one's are dropped.
func TestRenameAndDropResults(t *testing.T) {
	w := newTestWorkspace(t)
	otherConn(w)
	run(t, w, "SELECT 1")
	switchTo(t, w, "other")
	w.RenameResults(demo, "renamed")
	w.cfg.Connections[0].Name = "renamed"
	switchTo(t, w, "renamed")
	if w.LastResult() == nil {
		t.Fatal("the result did not follow the rename")
	}
	switchTo(t, w, "other")
	w.DropResults("renamed")
	switchTo(t, w, "renamed")
	if w.LastResult() != nil {
		t.Error("a dropped connection's result came back")
	}
}

// Sharing a result tab with the assistant: refused without ai_rows; with
// it, the shared tab's result goes with questions about any statement —
// beside that statement's own error — through the shared tab's own grid
// view, so its hidden columns stay out. The shared tab keeps its result as
// a pinned one does; closing it ends the share.
func TestShareResultTab(t *testing.T) {
	w := newTestWorkspace(t)
	shared := "SELECT id, name, breed FROM cats ORDER BY id"
	res := run(t, w, shared).Result
	id := got(w)

	if w.CanShareResults() {
		t.Fatal("demo has no ai_rows, yet sharing is offered")
	}
	err := w.ShareResultTab(id, true)
	refusal(t, err, Invalid)
	if !strings.Contains(err.Error(), "ai_rows = true") {
		t.Errorf("the refusal should name the setting: %q", err)
	}

	w.cfg.Connections[0].AIRows = true
	if !w.CanShareResults() || w.ShareResultTab(id, true) != nil {
		t.Fatal("sharing refused on an ai_rows connection")
	}
	if tabs, _ := w.ResultTabs(); !tabs[0].Shared {
		t.Error("the tab does not say it is shared")
	}

	// the next run does not replace the shared result: a new tab
	if ev := run(t, w, "SELEC oops"); ev.Err == nil {
		t.Fatal("the typo ran")
	}
	run(t, w, "SELECT 1 AS other")
	if ids, cur := tabIDs(w); len(ids) != 2 || cur == id {
		t.Fatalf("a run replaced the shared tab: %v, cur %d", ids, cur)
	}

	// asked about another statement: the shared rows go, through the
	// shared tab's view (the one on screen is another result's)
	onScreen := GridView{Result: w.LastResult(), SortCol: -1, Hidden: []int{0}}
	sharedView := GridView{Result: res, SortCol: -1, Hidden: []int{2}}
	ctx, refs := w.ChatContext("compare", Editor{Text: "SELECT 1 AS other"}, onScreen, sharedView)
	if !ctx.Shared || ctx.SharedLabel != "result 1" || ctx.SharedFrom != shared || len(ctx.Rows) != len(res.Rows) {
		t.Errorf("ctx = %+v", ctx)
	}
	if len(ctx.Hidden) != 1 || ctx.Hidden[0] != 2 {
		t.Errorf("hidden = %v, want the shared grid's [2]", ctx.Hidden)
	}
	if len(refs) != 1 {
		t.Errorf("the shared statement's table should be looked up: %v", refs)
	}
	// beside the error of the statement in question
	ctx, _ = w.ChatContext("", Editor{Text: "SELEC oops"}, onScreen)
	if ctx.Err != "" {
		t.Errorf("the error belongs to the run before last, not this question: %q", ctx.Err)
	}
	if ev := run(t, w, "SELEC oops"); ev.Err == nil {
		t.Fatal("the typo ran")
	}
	ctx, _ = w.ChatContext("", Editor{Text: "SELEC oops"})
	if ctx.Err == "" || !ctx.Shared || ctx.Rows == nil {
		t.Errorf("error and shared result should both go: %+v", ctx)
	}

	// a script tab's question gets it too
	ctx, _ = w.ScriptChatContext("", "x.go", "package main")
	if !ctx.Shared || ctx.Rows == nil {
		t.Errorf("script ctx = %+v", ctx)
	}

	// on another connection, its own set: nothing shared there
	otherConn(w)
	switchTo(t, w, "other")
	if ctx, _ = w.ChatContext("", Editor{Text: "SELECT 1"}); ctx.Shared {
		t.Error("demo's share went with a question on other")
	}
	switchTo(t, w, demo)

	// sharing another tab moves the share; closing it ends it
	second := got(w)
	_ = w.ShareResultTab(second, true)
	if tabs, _ := w.ResultTabs(); tabs[0].Shared || !tabs[1].Shared {
		t.Errorf("shares = %v, %v", tabs[0].Shared, tabs[1].Shared)
	}
	_ = w.CloseResultTab(second)
	if ctx, _ = w.ChatContext("", Editor{Text: "SELECT 1"}); ctx.Shared {
		t.Error("a closed tab is still shared")
	}
	// unsharing needs no ai_rows
	_ = w.ShareResultTab(id, true)
	w.cfg.Connections[0].AIRows = false
	if err := w.ShareResultTab(id, false); err != nil {
		t.Errorf("unshare: %v", err)
	}
}

// titles lists the active set's tab titles in strip order.
func titles(w *Workspace) []string {
	tabs, _ := w.ResultTabs()
	var out []string
	for _, t := range tabs {
		out = append(out, t.Title)
	}
	return out
}

// A run of several statements opens a tab per statement that returned
// rows — a write gets none — and the last one's is on screen. The log
// says how many tabs and whose result is showing.
func TestRunAllTabPerStatement(t *testing.T) {
	w := newTestWorkspace(t)
	ev := run(t, w, "SELECT 1 AS a", "CREATE TEMP TABLE tps (x INT)", "SELECT 2 AS b", "INSERT INTO tps VALUES (1)")
	if ev.Err != nil || ev.Tabs != 2 {
		t.Fatalf("run: err %v, tabs %d", ev.Err, ev.Tabs)
	}
	if got := titles(w); strings.Join(got, "|") != "SELECT 1 AS a|SELECT 2 AS b" {
		t.Errorf("titles = %q", got)
	}
	if r := w.LastResult(); r != ev.Result || r.Columns[0] != "b" {
		t.Errorf("on screen: %+v", r)
	}
	if n := ev.Notes[len(ev.Notes)-1]; !strings.HasSuffix(n.Text, " — 2 result tabs, showing statement 3's: 1 rows") {
		t.Errorf("done note = %q", n.Text)
	}
	// the last statement is still the INSERT, so the assistant is not
	// handed b's rows as the INSERT's; with the caret on b, they go
	text := "SELECT 1 AS a;\nSELECT 2 AS b;\nINSERT INTO tps VALUES (1);"
	if ctx, _ := w.ChatContext("", Editor{Text: text, Caret: len(text) - 2}); ctx.Query != "INSERT INTO tps VALUES (1)" || ctx.Columns != nil {
		t.Errorf("caret on the INSERT: query %q, columns %v", ctx.Query, ctx.Columns)
	}
	if ctx, _ := w.ChatContext("", Editor{Text: text, Caret: strings.Index(text, "AS b")}); len(ctx.Columns) != 1 || ctx.Columns[0] != "b" {
		t.Errorf("caret on b: columns %v", ctx.Columns)
	}
	// back on a's tab, with the caret on a: a's rows, though a is not the
	// last statement run
	ids, _ := tabIDs(w)
	_ = w.ShowResultTab(ids[0])
	if ctx, _ := w.ChatContext("", Editor{Text: text, Caret: 3}); len(ctx.Columns) != 1 || ctx.Columns[0] != "a" {
		t.Errorf("caret on a, its tab on screen: columns %v", ctx.Columns)
	}

	// only writes: the last statement's result lands alone, as before
	ev = run(t, w, "INSERT INTO tps VALUES (2)", "INSERT INTO tps VALUES (3), (4)")
	if ev.Tabs != 1 || ev.Result == nil || !ev.Result.IsExec || ev.Result.Affected != 2 {
		t.Errorf("writes only: tabs %d, result %+v", ev.Tabs, ev.Result)
	}
}

// A run of several statements logs each write's count, which has no tab
// of its own, folding them past writeLines; a single statement's count is
// left to the done note.
func TestRunAllLogsEachWrite(t *testing.T) {
	w := newTestWorkspace(t)
	// lines picks the per-statement write lines out of a run's notes
	lines := func(ev *RunDone) []string {
		var out []string
		for _, n := range ev.Notes {
			if strings.HasPrefix(n.Text, "statement ") || strings.HasPrefix(n.Text, "… ") {
				out = append(out, n.Text)
			}
		}
		return out
	}

	// the UPDATE's count, lost before behind the SELECT's tab
	run(t, w, "CREATE TEMP TABLE lw (x INT)", "INSERT INTO lw VALUES (1), (2), (3)")
	ev := run(t, w, "SELECT 1 AS a", "UPDATE lw SET x = x + 1 WHERE x > 1")
	if got := lines(ev); len(got) != 1 || got[0] != "statement 2/2: 2 affected — UPDATE lw SET x = x + 1 WHERE x > 1" {
		t.Errorf("select then update: %q", got)
	}

	// a single statement: the done note has its count
	if got := lines(run(t, w, "DELETE FROM lw WHERE x = 1")); len(got) != 0 {
		t.Errorf("single statement: %q", got)
	}

	// no line for BEGIN or COMMIT, "done" for DDL, the rest folded
	stmts := []string{"BEGIN", "CREATE TEMP TABLE lw2 (x INT)"}
	for i := range 8 {
		stmts = append(stmts, fmt.Sprintf("INSERT INTO lw2 VALUES (%d), (%d)", i, i))
	}
	stmts = append(stmts, "COMMIT")
	got := lines(run(t, w, stmts...))
	want := []string{
		"statement 2/11: done — CREATE TEMP TABLE lw2 (x INT)",
		"statement 3/11: 2 affected — INSERT INTO lw2 VALUES (0), (0)",
		"statement 4/11: 2 affected — INSERT INTO lw2 VALUES (1), (1)",
		"statement 5/11: 2 affected — INSERT INTO lw2 VALUES (2), (2)",
		"statement 6/11: 2 affected — INSERT INTO lw2 VALUES (3), (3)",
		"… 4 more writes, to statement 10: 8 affected in all",
	}
	if !slices.Equal(got, want) {
		t.Errorf("folded:\n got %q\nwant %q", got, want)
	}

	// a failed run logs the writes before the failure, ahead of the error
	ev = run(t, w, "INSERT INTO lw VALUES (9)", "SELECT * FROM no_such_table", "INSERT INTO lw VALUES (10)")
	if ev.Err == nil {
		t.Fatal("want the failure")
	}
	if got := lines(ev); len(got) != 1 || got[0] != "statement 1/3: 1 affected — INSERT INTO lw VALUES (9)" {
		t.Errorf("failed run: %q", got)
	}
	if n := ev.Notes[len(ev.Notes)-1]; n.Level != Err {
		t.Errorf("last note = %+v, want the error", n)
	}
}

// A rerun refills its group's tabs in order, closing the ones it has no
// result for; a pinned tab stays out of it, and a single statement run on
// one of the group's tabs replaces just that one.
func TestRunAllReplacesItsGroup(t *testing.T) {
	w := newTestWorkspace(t)
	run(t, w, "SELECT 0 AS kept")
	ids, cur := tabIDs(w)
	if err := w.PinResultTab(cur, true); err != nil {
		t.Fatal(err)
	}
	run(t, w, "SELECT 1 AS a", "SELECT 2 AS b", "SELECT 3 AS c")
	group, cur := tabIDs(w)
	if len(group) != 4 || group[0] != ids[0] || cur != group[3] {
		t.Fatalf("first run all: tabs %v cur %d", group, cur)
	}

	// rerun with the middle tab on screen: the whole group is refilled,
	// the same tabs, and the last is current again
	_ = w.ShowResultTab(group[2])
	run(t, w, "SELECT 1 AS a2", "SELECT 2 AS b2", "SELECT 3 AS c2")
	again, cur := tabIDs(w)
	if !slices.Equal(again, group) || cur != group[3] {
		t.Errorf("rerun: tabs %v cur %d, want %v", again, cur, group)
	}
	if got := titles(w); strings.Join(got, "|") != "SELECT 0 AS kept|SELECT 1 AS a2|SELECT 2 AS b2|SELECT 3 AS c2" {
		t.Errorf("titles = %q", got)
	}

	// one statement fewer: the group's third tab goes
	run(t, w, "SELECT 1 AS a3", "SELECT 2 AS b3")
	if got, cur := tabIDs(w); !slices.Equal(got, group[:3]) || cur != group[2] {
		t.Errorf("shorter rerun: tabs %v cur %d, want %v", got, cur, group[:3])
	}

	// a single statement on the group's first tab replaces only it …
	_ = w.ShowResultTab(group[1])
	run(t, w, "SELECT 9 AS solo")
	if got := titles(w); strings.Join(got, "|") != "SELECT 0 AS kept|SELECT 9 AS solo|SELECT 2 AS b3" {
		t.Errorf("solo: titles %q", got)
	}
	// … and takes it out of the group: run all from the group's other tab
	// refills that one and opens a new tab right after it, leaving solo
	_ = w.ShowResultTab(group[2])
	run(t, w, "SELECT 1 AS a4", "SELECT 2 AS b4")
	got, cur := tabIDs(w)
	if len(got) != 4 || got[2] != group[2] || cur != got[3] {
		t.Errorf("after solo: tabs %v cur %d", got, cur)
	}
	if t2 := titles(w); strings.Join(t2, "|") != "SELECT 0 AS kept|SELECT 9 AS solo|SELECT 1 AS a4|SELECT 2 AS b4" {
		t.Errorf("after solo: titles %q", t2)
	}
}

// A run's new tabs go right after its group's last, so they stay side by
// side when the group sits mid-strip.
func TestRunAllTabsStayTogether(t *testing.T) {
	w := newTestWorkspace(t)
	run(t, w, "SELECT 1 AS a", "SELECT 2 AS b")
	_, cur := tabIDs(w)
	_ = w.PinResultTab(cur, true) // b is kept: the run after opens a new tab
	run(t, w, "SELECT 3 AS later")
	ids, _ := tabIDs(w)
	_ = w.ShowResultTab(ids[0]) // back on a, the group's unpinned tab
	run(t, w, "SELECT 1 AS a2", "SELECT 2 AS x2", "SELECT 3 AS y2")
	if got := titles(w); strings.Join(got, "|") != "SELECT 1 AS a2|SELECT 2 AS x2|SELECT 3 AS y2|SELECT 2 AS b|SELECT 3 AS later" {
		t.Errorf("titles = %q", got)
	}
	if _, cur := tabIDs(w); cur == 0 {
		t.Error("no current tab")
	} else if r := w.LastResult(); r.Columns[0] != "y2" {
		t.Errorf("on screen: %v", r.Columns)
	}
}

// More results than result_tabs: other runs' unpinned tabs go first, then
// the run's own oldest, so the last ones show; the log says how many did
// not fit — whether the run let them go itself (past the cap outright) or
// the cap dropped them at landing (pinned tabs taking room).
func TestRunAllUnderTheCap(t *testing.T) {
	w := newTestWorkspace(t)
	w.cfg.ResultTabs = 3
	run(t, w, "SELECT 0 AS kept")
	_, cur := tabIDs(w)
	_ = w.PinResultTab(cur, true)
	run(t, w, "SELECT 9 AS old") // an unpinned tab of another run

	ev := run(t, w, "SELECT 1 AS a", "SELECT 2 AS b", "SELECT 3 AS c", "SELECT 4 AS d")
	if got := titles(w); strings.Join(got, "|") != "SELECT 0 AS kept|SELECT 3 AS c|SELECT 4 AS d" {
		t.Errorf("titles = %q", got)
	}
	if ev.Tabs != 2 || w.LastResult().Columns[0] != "d" {
		t.Errorf("tabs %d, on screen %v", ev.Tabs, w.LastResult().Columns)
	}
	var warned bool
	for _, n := range ev.Notes {
		warned = warned || (n.Level == Warn && strings.HasPrefix(n.Text, "2 of the run's 4 results did not fit") &&
			strings.Contains(n.Text, "showing the last 2 (result_tabs = 3)"))
	}
	if !warned {
		t.Errorf("notes = %+v", ev.Notes)
	}
}

// rerun reruns result tab id and runs its job, failing the test on a
// refusal.
func rerun(t *testing.T, w *Workspace, id int) *RunDone {
	t.Helper()
	st, err := w.RerunResultTab(id)
	if err != nil {
		t.Fatalf("rerun %d: %v", id, err)
	}
	return st.Job().(*RunDone)
}

// A rerun brings a pinned tab's result up to date IN that tab: same id,
// same place in the strip, still pinned, title kept, a new seq — and it
// comes on screen, whichever tab was. The other tabs are untouched.
func TestRerunPinnedTab(t *testing.T) {
	w := newTestWorkspace(t)
	run(t, w, "CREATE TABLE counted (x INTEGER)")
	run(t, w, "SELECT count(*) AS n FROM counted")
	pinned := got(w)
	before := w.LastResult().Rows[0][0]
	if err := w.PinResultTab(pinned, true); err != nil {
		t.Fatal(err)
	}
	_, seq := w.LastResultSeq()
	run(t, w, "INSERT INTO counted VALUES (1)") // a new tab: the current one is pinned
	ids, other := tabIDs(w)
	if other == pinned || len(ids) != 2 {
		t.Fatalf("the write landed in %d (tabs %v)", other, ids)
	}
	otherRes := w.LastResult()

	ev := rerun(t, w, pinned)
	if ev.Err != nil || !strings.HasPrefix(ev.Tag, "rerun ") {
		t.Fatalf("rerun: %+v", ev)
	}
	got2, cur := tabIDs(w)
	if !slices.Equal(got2, ids) || cur != pinned {
		t.Fatalf("after the rerun: tabs %v cur %d, want %v cur %d", got2, cur, ids, pinned)
	}
	r, seq2 := w.LastResultSeq()
	if seq2 == seq || r.Rows[0][0] == before {
		t.Errorf("not refreshed: seq %d → %d, n %v → %v", seq, seq2, before, r.Rows[0][0])
	}
	tabs, _ := w.ResultTabs()
	if !tabs[0].Pinned || tabs[0].Title != "SELECT count(*) AS n FROM counted" {
		t.Errorf("the rerun tab = %+v", tabs[0])
	}
	if tabs[1].Result != otherRes {
		t.Error("the rerun touched another tab")
	}
	// the write's tab may write again; the count's may not
	if tabs[0].Writes || !tabs[1].Writes {
		t.Errorf("Writes = %v, %v", tabs[0].Writes, tabs[1].Writes)
	}
}

// A rerun's result names the seq of the one it replaced (RerunOf), for a
// UI to keep its grid's sort across the refresh; any other run into the
// tab — the edited statement, the common loop — names none, nor does a
// rerun's result once another run has replaced it.
func TestRerunNotesTheResultItReplaced(t *testing.T) {
	w := newTestWorkspace(t)
	run(t, w, "SELECT 1 AS n")
	id := got(w)
	_, seq := w.LastResultSeq()
	if _, _, of := w.LastResultRerun(); of != 0 {
		t.Fatalf("a first run's RerunOf = %d, want 0", of)
	}

	if ev := rerun(t, w, id); ev.Err != nil {
		t.Fatal(ev.Err)
	}
	_, seq2, of := w.LastResultRerun()
	tabs, cur := w.ResultTabs()
	if of != seq || tabs[cur].RerunOf != seq || tabs[cur].Seq != seq2 {
		t.Errorf("after the rerun: RerunOf %d (tab %+v), want %d", of, tabs[cur], seq)
	}

	run(t, w, "SELECT 2 AS n") // the edit-and-run loop: same tab, a new question
	tabs, cur = w.ResultTabs()
	if got(w) != id || tabs[cur].RerunOf != 0 {
		t.Errorf("an edited run into tab %d: %+v, want RerunOf 0 in tab %d", got(w), tabs[cur], id)
	}
}

// A rerun needs no room: at the cap with every tab pinned — where a run is
// refused — rerunning one of them still lands, in place. A shared tab
// stays shared, and an app run's tab keeps its tag for a title.
func TestRerunKeptTabsAtTheCap(t *testing.T) {
	w := newTestWorkspace(t)
	w.cfg.ResultTabs = 2
	w.cfg.Connections[0].AIRows = true
	run(t, w, "SELECT 1 AS n")
	first := got(w)
	_ = w.PinResultTab(first, true)
	st, err := w.ShowColumns("cats")
	if err != nil {
		t.Fatal(err)
	}
	st.Job()
	cols := got(w)
	if err := w.ShareResultTab(cols, true); err != nil {
		t.Fatal(err)
	}
	if _, err := w.RunStmts([]string{"SELECT 2"}, "query"); err == nil {
		t.Fatal("a run with every tab kept at the cap was not refused")
	}

	rerun(t, w, cols)
	tabs, cur := w.ResultTabs()
	if len(tabs) != 2 || tabs[cur].ID != cols || !tabs[cur].Shared || tabs[cur].Title != "columns cats" {
		t.Errorf("after rerunning the shared tab: %+v at %d", tabs, cur)
	}
	rerun(t, w, first)
	if tabs, cur = w.ResultTabs(); len(tabs) != 2 || tabs[cur].ID != first || !tabs[cur].Pinned {
		t.Errorf("after rerunning the pinned tab: %+v at %d", tabs, cur)
	}
}

// A rerun that fails leaves the tab's old result, as any failed run does;
// a script's tab has no statement to rerun; and a rerun is a run — one at
// a time.
func TestRerunRefusalsAndFailure(t *testing.T) {
	w := newTestWorkspace(t)
	run(t, w, "CREATE TABLE gone (x INTEGER)")
	run(t, w, "INSERT INTO gone VALUES (7)")
	run(t, w, "SELECT x FROM gone")
	id := got(w)
	_ = w.PinResultTab(id, true)
	old := w.LastResult()
	run(t, w, "DROP TABLE gone")
	if ev := rerun(t, w, id); ev.Err == nil {
		t.Fatal("a rerun on a dropped table did not fail")
	}
	_ = w.ShowResultTab(id)
	if w.LastResult() != old {
		t.Error("the failed rerun replaced the tab's result")
	}

	st, err := w.RunScript("../testdata/show_two.go")
	if err != nil {
		t.Fatal(err)
	}
	st.Job()
	_, err = w.RerunResultTab(got(w))
	refusal(t, err, Invalid)
	if !strings.Contains(err.Error(), "script") {
		t.Errorf("refusal = %q", err)
	}

	st, err = w.RunStmts([]string{"SELECT 1"}, "query")
	if err != nil {
		t.Fatal(err)
	}
	_, err = w.RerunResultTab(id)
	refusal(t, err, Busy)
	st.Job()
	if _, err = w.RerunResultTab(12345); err == nil {
		t.Error("a rerun of no tab was not refused")
	}
}

// A rerun of one tab of a multi-statement run's group refills just that
// tab and takes it out of the group, as a single statement run on it does:
// the next run-all refills the rest of the group, not it.
func TestRerunGroupTab(t *testing.T) {
	w := newTestWorkspace(t)
	run(t, w, "SELECT 1 AS a", "SELECT 2 AS b")
	ids, _ := tabIDs(w)
	if len(ids) != 2 {
		t.Fatalf("tabs = %v", ids)
	}
	other := w.LastResult()
	rerun(t, w, ids[0])
	if now, cur := tabIDs(w); !slices.Equal(now, ids) || cur != ids[0] {
		t.Fatalf("after the rerun: %v cur %d", now, cur)
	}
	_ = w.ShowResultTab(ids[1])
	if w.LastResult() != other {
		t.Error("the rerun refilled the group's other tab")
	}
	if fmt.Sprint(titles(w)) != "[SELECT 1 AS a SELECT 2 AS b]" {
		t.Errorf("titles = %v", titles(w))
	}
}
