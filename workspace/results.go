package workspace

import (
	"slices"
	"strings"

	"github.com/rohanthewiz/dbc/db"
	"github.com/rohanthewiz/dbc/explain"
	"github.com/rohanthewiz/dbc/model"
)

// Result sets: what the results pane shows, kept PER CONNECTION.
//
// A workspace (one query tab) keeps a result set for each connection it has
// been on. Switching the tab to another connection does not carry the old
// connection's result along, nor throw it away: the pane shows the new
// connection's set, and switching back shows the old one again as it was.
// "The last result", "the last error", "the last statement" and "the plan"
// are all the active connection's — so the grid, copies, exports and the
// assistant all talk about the connection the tab is on.
//
//	Workspace (active = "pg")
//	  sets["pg"]   ─► resultSet{ tabs: [#3 ⚑ orders, #7 cats], cur: 1, plan, last* }
//	  sets["lite"] ─► resultSet{ tabs: [#5 select 1], cur: 0, … }
//	                     LastResult() = sets[active].tabs[cur].res
//
// A set is a strip of RESULT TABS, at most Config.ResultTabLimit of them.
// A run REPLACES the result in the tab that was current when it started,
// unless that tab is PINNED, in which case it opens a new tab — the
// DataGrip/DBeaver model, where the common loop (edit the WHERE, run again)
// does not pile up tabs, and a result worth keeping is one click from kept:
//
//	run starts ──► target = the current tab, or nil when it is pinned (or
//	               the set is empty)
//	run lands  ──► target still in the set and unpinned? replace its result
//	               otherwise a new tab at the end; at the cap, the oldest
//	               unpinned tab is dropped first
//	               either way the landed tab becomes the current one
//
// The target is fixed at the START, not at landing: a slow query must not
// overwrite whichever tab the user happens to be browsing when it ends.
// A run that would need a new tab while every tab is pinned and the set is
// full is refused before it starts (targetLocked) — dropping a pinned result
// to make room would break the one promise a pin makes, and refusing up
// front costs nothing but a click, where a result thrown away at landing
// may have cost minutes. (Pinning the last unpinned tab while a run is in
// flight can still leave it nowhere to land; the set then goes one over
// the cap rather than lose either result.)
//
// A tab can be RERUN (RerunResultTab): its own statement runs again and
// the fresh result goes back in that same tab, pinned or shared or not,
// with its pin, its share and its title left as they were. The pin keeps a
// result from being replaced by OTHER runs; rerunning the tab's own
// statement is the user asking for that result to be brought up to date —
// what DataGrip's refresh on a pinned tab does — and opening a new tab for
// it would leave a stale copy behind and lose the pin's place in the
// strip:
//
//	before    [#1 ⚑ orders (yesterday)] [#4 cats]     #4 current
//	rerun #1  ─► runs #1's statement ─► target #1 even though pinned
//	after     [#1 ⚑ orders (now)] [#4 cats]           #1 current, still pinned
//
// A rerun runs on the tab's connection's session like any run (one at a
// time, the same session state), and a failed one leaves the tab's old
// result in place, as a failed run always does. A tab of a script's
// shows has no statement to rerun; a tab of a multi-statement run's group
// reruns its own statement alone and, as a single statement run on it
// would, leaves the group. A rerun's result notes the seq of the one it
// replaced (rerunOf), so a UI can keep the grid's sort across the refresh
// — and only across a refresh: any other run into the tab starts in
// result order.
//
// One tab per set may be SHARED with the assistant (ShareResultTab, on a
// connection with ai_rows only): its result goes with every question asked
// on that connection until it is unshared or closed — see ChatContext. A
// shared tab keeps its result as a pinned one does: a run does not replace
// it, and room is not made by dropping it.
//
// A run of SEVERAL statements (run all, a selection of several) opens a
// tab per statement that returns rows, as DBeaver and DataGrip do — a
// write's "n affected" gets none, or a script of INSERTs would bury the
// strip; when no statement returns rows, the last one's result lands
// alone, as a single statement's would. The tabs of one run are a GROUP
// (resultTab.run), and "a run replaces its tab" becomes "a run replaces its
// group": rerunning the buffer refills the same tabs in order rather than
// piling up a strip's worth each time:
//
//	before   [#1 ⚑ users] [#4 a] [#5 b] [#6 c]     #4–#6 one run's group, #5 current
//	run all  (2 SELECTs and an UPDATE) ─► target #5 ─► group #4 #5 #6
//	after    [#1 ⚑ users] [#4 a'] [#5 b']          #4, #5 refilled in order; #6
//	                                               had no result to take and is
//	                                               closed; #5 (the last) current
//
// More results than the group has tabs open new ones right after the
// group's last, so a run's tabs stay side by side. A pinned or shared tab
// leaves the group as far as a rerun is concerned (it keeps its result).
// A single statement run on one tab of a group replaces just that tab —
// that is the edit-one-and-rerun loop — and takes it out of the group. A
// rerun of several statements replaces the group however many of them
// return rows this time (a failure part way included): fewer results than
// tabs closes the rest.
//
// The cap still holds: a new tab first drops the oldest unpinned tab of
// OTHER runs, then this run's own oldest, so a run with more results than
// result_tabs keeps its last ones (the last result is still the one on
// screen, as before tabs-per-statement) and says how many did not fit.
//
// A script run lands in one tab too: its first s.Show takes the target as a
// run's result would, and every later show of the same run joins that tab
// (shows, below), where the "Result 1 · 2 · 3" switcher steps through them.
//
// Every result put in a tab gets a SEQ, a number unique within the
// workspace that moves whenever a tab's result does. A UI keys what it
// keeps about a result (dbc web's grid view, a copy's "still the same
// result?" check) on it; unlike a pointer it survives a JSON round trip.

// resultTab is one tab of a result set.
type resultTab struct {
	id     int    // unique within the workspace, for the tab's whole life
	title  string // what produced it, one line: see resultTitle
	stmt   string // the statement whose result it is ("" for a script's)
	pinned bool
	res    *model.Result // what the tab shows
	seq    int           // res's number; see SEQ above
	// shows are the s.Show results of the script run that filled the tab,
	// oldest first, at most MaxScriptResults; cut counts the ones dropped
	// off the front to keep to that, so shows[i] is the script's
	// (cut+i+1)th. res is one of them. Empty for a run's own result.
	shows []shown
	cut   int
	// run is the run that filled the tab (its runGen): tabs sharing it are
	// one multi-statement run's group, which a rerun refills together
	run int
	// rerunOf is the seq of the result that res replaced when res is a
	// RERUN of this tab's own statement (RerunResultTab), else 0. A UI
	// carries its view of the old result — the grid's sort — over to the
	// new one only then: a refresh is the same question asked again, where
	// an edited statement run into the tab is a new one whose rows start in
	// result order. A seq, not a flag, so a UI can check the rerun replaced
	// the very result it was showing: one that missed a landing (an edited
	// run into the tab, then a rerun of that) would otherwise carry a sort
	// over from a different question.
	rerunOf int
}

// landing is one result a run puts in a tab: what placeRunLocked takes.
type landing struct {
	title, stmt string
	res         *model.Result
	n           int // the statement's number in the run, 1-based (0: not a statement's)
}

// shown is one of a script run's s.Show results, with its seq.
type shown struct {
	res *model.Result
	seq int
}

// resultSet is one connection's results as a query tab keeps them.
type resultSet struct {
	tabs []*resultTab
	cur  int // index of the tab on screen; -1 when there is none

	// the last run on this connection: its statement (or script) and what
	// it failed with — the assistant's "this query" and "✦ ask why"
	lastStmt   string
	lastScript string // the script's file name when the last run was one
	lastErr    string
	plan       *explain.Plan // the last plan: an explain's, or one detected in a result

	// shared is the tab the user shared with the assistant (ShareResultTab):
	// its result goes with every question asked on this connection until
	// it is unshared or closed. At most one per set — one result is what
	// "this result" in a question can mean. A tab closed or dropped from
	// the set is no longer shared (sharedTab checks it is still there).
	shared *resultTab
}

// sharedTab is the set's shared tab and its 0-based position, or nil.
func (s *resultSet) sharedTab() (*resultTab, int) {
	if s == nil || s.shared == nil {
		return nil, -1
	}
	if i := slices.Index(s.tabs, s.shared); i >= 0 {
		return s.shared, i
	}
	return nil, -1
}

// kept reports whether t keeps its result: a run does not replace it, and
// making room for a new tab does not drop it. A pinned tab, by definition;
// and the shared one, since the user shared that RESULT — a rerun swapping
// it for another under the share, or the cap dropping it, would change
// what the assistant is told without a word.
func (s *resultSet) kept(t *resultTab) bool { return t.pinned || t == s.shared }

// current is the tab on screen, or nil.
func (s *resultSet) current() *resultTab {
	if s == nil || s.cur < 0 || s.cur >= len(s.tabs) {
		return nil
	}
	return s.tabs[s.cur]
}

// setLocked is conn's result set, made on first use. The caller holds mu.
func (w *Workspace) setLocked(conn string) *resultSet {
	if w.sets == nil {
		w.sets = map[string]*resultSet{}
	}
	s := w.sets[conn]
	if s == nil {
		s = &resultSet{cur: -1}
		w.sets[conn] = s
	}
	return s
}

// activeSetLocked is the active connection's set, or nil before anything
// landed there. Read-only callers use it, so nothing is made for a lookup.
func (w *Workspace) activeSetLocked() *resultSet { return w.sets[w.active] }

// curLocked is the active connection's current result tab, or nil.
func (w *Workspace) curLocked() *resultTab { return w.activeSetLocked().current() }

// resultTitle names a result tab after what produced it. An editor run's
// tag ("query", "statement 2/4", "selection", "all 3 statements") says
// where the statement was picked, not what it was, so those tabs are named
// by the statement itself; the app's own runs ("preview cats", "columns
// cats", "list tables") have tags that already say it better than their
// generated SQL would.
func resultTitle(tag, stmt string) string {
	switch {
	case tag == "query", strings.HasPrefix(tag, "statement "),
		strings.HasPrefix(tag, "selection"), strings.HasPrefix(tag, "all "):
		return Preview(stmt)
	}
	return tag
}

// targetLocked picks the tab a run on conn will land in: the current tab
// when it is not kept (pinned or shared), else nil (a new one). It refuses
// when a new tab is needed and there is no room for one: every tab kept,
// at the cap.
func (w *Workspace) targetLocked(conn string) (*resultTab, error) {
	s := w.sets[conn]
	cur := s.current()
	if cur != nil && !s.kept(cur) {
		return cur, nil
	}
	if s == nil {
		return nil, nil
	}
	limit := w.cfg.ResultTabLimit()
	if len(s.tabs) < limit || slices.ContainsFunc(s.tabs, func(t *resultTab) bool { return !s.kept(t) }) {
		return nil, nil
	}
	return nil, refuse(Invalid, Warn,
		"every result tab on %s is pinned (or shared with the assistant) and there are %d of %d (result_tabs) — unpin or close one, then run again",
		conn, len(s.tabs), limit)
}

// placeLocked puts r in conn's result set — in target when that is still in
// the set and not kept (a pin or a share made since the run started), else
// in a new tab at the end — and makes that tab the current one. The caller
// holds mu.
func (w *Workspace) placeLocked(conn string, target *resultTab, title, stmt string, r *model.Result) *resultTab {
	t, _ := w.placeRunLocked(conn, target, w.runGen, false, []landing{{title: title, stmt: stmt, res: r}})
	return t
}

// placeRunLocked lands one run's results (at least one) in conn's result
// set, a tab each, and makes the last one's tab current; it returns that
// tab and how many of the run's own earlier tabs the cap made it drop. The
// caller holds mu.
//
// The tabs it may refill — the SLOTS — are target alone for a run of one
// statement, and target's whole group (the unkept tabs its run filled, in
// strip order) when group is set, for a run of several; none when target
// has gone or is now kept. It is the run's statements that decide, not how
// many results came back: rerunning the buffer after a statement stopped
// returning rows (or failed) still replaces the group, closing the tab the
// missing result had.
// Results past the slots open new tabs after the last tab filled; slots
// past the results are closed — they held the replaced run's results for
// statements this run did not return rows from. See the package comment
// above for the picture.
func (w *Workspace) placeRunLocked(conn string, target *resultTab, run int, group bool, ls []landing) (*resultTab, int) {
	s := w.setLocked(conn)
	var slots []*resultTab
	// a kept target is refilled only by a rerun of that very tab: the pin
	// (or share) keeps the tab's result from OTHER runs, while rerunning
	// its own statement is how the user asks for it fresh
	if target != nil && slices.Contains(s.tabs, target) && (!s.kept(target) || target == w.runAgain) {
		slots = []*resultTab{target}
		if group {
			slots = slices.DeleteFunc(slices.Clone(s.tabs), func(t *resultTab) bool {
				return t.run != target.run || s.kept(t)
			})
		}
	}
	var placed []*resultTab // this landing's tabs, in the order filled
	dropped := 0
	for i, l := range ls {
		w.resSeq++
		if i < len(slots) {
			t := slots[i]
			// read before the seq moves: the result this one replaces
			t.rerunOf = 0
			if t == w.runAgain {
				t.rerunOf = t.seq
			}
			t.title, t.stmt, t.res, t.seq, t.run = l.title, l.stmt, l.res, w.resSeq, run
			t.shows, t.cut = nil, 0
			placed = append(placed, t)
			continue
		}
		dropped += w.makeRoomLocked(s, placed)
		// right after the tab filled last, so one run's tabs sit side by
		// side; the first new tab of a landing goes at the end
		at := len(s.tabs)
		if n := len(placed); n > 0 {
			if j := slices.Index(s.tabs, placed[n-1]); j >= 0 {
				at = j + 1
			}
		}
		w.tabSeq++
		t := &resultTab{id: w.tabSeq, title: l.title, stmt: l.stmt, res: l.res, seq: w.resSeq, run: run}
		s.tabs = slices.Insert(s.tabs, at, t)
		placed = append(placed, t)
	}
	if len(slots) > len(ls) {
		gone := slots[len(ls):]
		s.tabs = slices.DeleteFunc(s.tabs, func(t *resultTab) bool { return slices.Contains(gone, t) })
	}
	// the last tab filled is never dropped: makeRoomLocked runs only
	// before a tab is added, and drops this landing's oldest first
	last := placed[len(placed)-1]
	s.cur = slices.Index(s.tabs, last)
	return last, dropped
}

// makeRoomLocked drops tabs until s has room for one more under the cap:
// the oldest unkept tab that is not one of ours (this landing's) first,
// then the oldest of ours. It returns how many of ours went. With nothing
// left to drop the set goes over the cap (see the package comment: only a
// pin or share made mid-run gets here, and then only for the first tab a
// landing adds — the next has that one to drop).
func (w *Workspace) makeRoomLocked(s *resultSet, ours []*resultTab) int {
	n := 0
	for len(s.tabs) >= w.cfg.ResultTabLimit() {
		i := slices.IndexFunc(s.tabs, func(t *resultTab) bool { return !s.kept(t) && !slices.Contains(ours, t) })
		if i < 0 {
			// ours are never kept: a landing holds mu from its first tab
			// to its last, so nothing pins one in between
			if i = slices.IndexFunc(s.tabs, func(t *resultTab) bool { return slices.Contains(ours, t) }); i < 0 {
				break
			}
			n++
		}
		s.tabs = slices.Delete(s.tabs, i, i+1)
	}
	return n
}

// showLocked lands one of a script run's s.Show results on conn: the run's
// first show is placed as a run's result would be (in target, or a new
// tab); later ones join the tab the first went to, as long as it is still
// in the set — closed meanwhile, the next show starts over in a new tab.
func (w *Workspace) showLocked(conn string, target *resultTab, title string, r *model.Result) {
	s := w.setLocked(conn)
	t := w.showTab
	i := slices.Index(s.tabs, t)
	if t == nil || i < 0 {
		t = w.placeLocked(conn, target, title, "", r)
		t.shows = []shown{{res: r, seq: t.seq}}
		w.showTab = t
		return
	}
	w.resSeq++
	t.shows = append(t.shows, shown{res: r, seq: w.resSeq})
	if len(t.shows) > MaxScriptResults {
		// drop the oldest; a fresh slice so the dropped result is not kept
		// alive by the backing array
		t.shows = slices.Clone(t.shows[1:])
		t.cut++
	}
	t.res, t.seq, t.rerunOf = r, w.resSeq, 0
	s.cur = i // a show moves the grid to it, as a landed run does
}

// ---------------------------------------------------------------------------
// Reading and changing the result tabs
// ---------------------------------------------------------------------------

// ResultTab describes one tab of the active connection's result set, for a
// UI's strip.
type ResultTab struct {
	ID     int    // stable for the tab's life: what ShowResultTab and the rest take
	Seq    int    // numbers the result on it; moves whenever that result does
	Title  string // what produced it, one line: a statement's preview, "preview cats", "script x.go"
	Stmt   string // the whole statement, for a tooltip ("" for a script's)
	Pinned bool
	// Result is the result the tab shows. Shared: read it, never write it.
	Result *model.Result
	Shows  int  // how many s.Show results of a script it holds (0 for a run's own)
	Shared bool // shared with the assistant (ShareResultTab)
	// Writes: rerunning the tab (RerunResultTab) would run a statement that
	// may change the database (db.ChangesRows) — an INSERT whose "n
	// affected" landed in the tab — so a UI asks before it does. False
	// for a tab with no statement, which cannot be rerun at all.
	Writes bool
	// RerunOf is the Seq of the result this one replaced when it is a rerun
	// of the tab's own statement (RerunResultTab), 0 for any other landing.
	// A UI keeps its grid's sort across a rerun on it — see resultTab.
	RerunOf int
}

// ResultTabs is the active connection's result set: its tabs in strip
// order, and the index of the one on screen (-1 when there is none).
func (w *Workspace) ResultTabs() (tabs []ResultTab, cur int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	s := w.activeSetLocked()
	if s == nil {
		return nil, -1
	}
	tabs = make([]ResultTab, len(s.tabs))
	shared, _ := s.sharedTab()
	for i, t := range s.tabs {
		tabs[i] = ResultTab{ID: t.id, Seq: t.seq, Title: t.title, Stmt: t.stmt, Pinned: t.pinned,
			Result: t.res, Shows: len(t.shows), Shared: t == shared,
			Writes: t.stmt != "" && db.ChangesRows(t.stmt), RerunOf: t.rerunOf}
	}
	return tabs, s.cur
}

// ResultTabLimit is how many tabs one connection's result set holds
// (result_tabs).
func (w *Workspace) ResultTabLimit() int { return w.cfg.ResultTabLimit() }

// tabLocked finds tab id in the active connection's set.
func (w *Workspace) tabLocked(id int) (*resultSet, int, error) {
	s := w.activeSetLocked()
	if s != nil {
		if i := slices.IndexFunc(s.tabs, func(t *resultTab) bool { return t.id == id }); i >= 0 {
			return s, i, nil
		}
	}
	return nil, -1, refuse(Invalid, Warn, "no result tab %d on %s — it was closed, or replaced", id, w.active)
}

// ShowResultTab puts result tab id on screen: its result becomes the last
// result, which is what the grid, copies, exports and the assistant read.
// Allowed mid-run — looking back at an earlier result while a slow query
// runs is what keeping them is for — since the run lands in the tab it
// picked when it started, not in whichever is on screen.
func (w *Workspace) ShowResultTab(id int) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	s, i, err := w.tabLocked(id)
	if err != nil {
		return err
	}
	s.cur = i
	return nil
}

// PinResultTab pins or unpins result tab id. A pinned tab keeps its
// result: the next run that would have replaced it opens a new tab. Only a
// rerun of the tab itself (RerunResultTab) refreshes it in place.
func (w *Workspace) PinResultTab(id int, pin bool) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	s, i, err := w.tabLocked(id)
	if err != nil {
		return err
	}
	s.tabs[i].pinned = pin
	return nil
}

// RerunResultTab runs result tab id's statement again, on the active
// connection, and lands the result back in that tab — even a pinned or
// shared one, which keeps its pin, share and title (see the package
// comment). It is refused, like any run, while a run is in flight, and
// for a tab that holds a script's shows, which has no statement: running
// the script again is how those are refreshed.
//
// The statement is not recorded in the history again: it got there when
// it was first run by hand (an app run's — a table preview's, "list
// tables" — never belonged there), and a refresh is not a new query to
// recall.
func (w *Workspace) RerunResultTab(id int) (Start, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	s, i, err := w.tabLocked(id)
	if err != nil {
		return Start{}, err
	}
	t := s.tabs[i]
	if t.stmt == "" {
		return Start{}, refuse(Invalid, Warn,
			"result %d holds a script's results, not a statement's — run the script again to refresh them", i+1)
	}
	return w.runLocked([]string{t.stmt}, "rerun "+t.title, t)
}

// CloseResultTab closes result tab id, pinned or not. When it was on
// screen, the tab to its right takes its place (its left, at the end of
// the strip), as a browser's tabs do; the last one closed leaves the pane
// empty. A run in flight that picked it lands in a new tab instead.
func (w *Workspace) CloseResultTab(id int) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	s, i, err := w.tabLocked(id)
	if err != nil {
		return err
	}
	s.tabs = slices.Delete(s.tabs, i, i+1)
	switch {
	case len(s.tabs) == 0:
		s.cur = -1
	case s.cur > i, s.cur == len(s.tabs):
		// a tab left of the current one went, or the current one was the
		// last: either way the index moves one left
		s.cur--
	}
	return nil
}

// CanShareResults reports whether the active connection lets the assistant
// see result rows (ai_rows), which is what sharing a result tab needs —
// for a UI to offer "Share with the assistant", or say why not. The
// config is read live: dbc web can change ai_rows on a connection at any
// time.
func (w *Workspace) CanShareResults() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	cc, ok := w.cfg.ConnByName(w.active)
	return ok && cc.AIRows
}

// ShareResultTab shares result tab id with the assistant, or stops sharing
// it. A shared tab's result goes with every question asked on its
// connection — in place of the result that would otherwise ride along
// with the statement under the caret — until it is unshared, closed, or
// another tab is shared (one at a time). The RESULT is what is shared: a
// shared tab keeps it as a pinned tab would (resultSet.kept), so the next
// run opens a new tab rather than change what the assistant sees.
//
// Sharing is refused on a connection without ai_rows: result rows are the
// database's contents, and sending them is that connection's setting to
// grant (package ai's DATA RULE), not a menu's. Unsharing always works. A
// share made while ai_rows was on, then turned off, sends column names
// only, and the note says the rows were withheld (ai.Build).
func (w *Workspace) ShareResultTab(id int, share bool) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	s, i, err := w.tabLocked(id)
	if err != nil {
		return err
	}
	if !share {
		if s.shared == s.tabs[i] {
			s.shared = nil
		}
		return nil
	}
	if cc, ok := w.cfg.ConnByName(w.active); !ok || !cc.AIRows {
		return refuse(Invalid, Warn,
			"%s does not share results with the assistant — set ai_rows = true on connection %q to share them",
			w.active, w.active)
	}
	s.shared = s.tabs[i]
	return nil
}

// CloseUnpinnedResultTabs closes every unpinned tab of the active
// connection's result set — except the one shared with the assistant,
// which the user is still using — and reports how many went. The current
// tab, if kept, stays current; otherwise the last kept tab comes on screen.
func (w *Workspace) CloseUnpinnedResultTabs() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	s := w.activeSetLocked()
	if s == nil {
		return 0
	}
	cur := s.current()
	n := len(s.tabs)
	s.tabs = slices.DeleteFunc(s.tabs, func(t *resultTab) bool { return !s.kept(t) })
	s.cur = slices.Index(s.tabs, cur)
	if s.cur < 0 {
		s.cur = len(s.tabs) - 1
	}
	return n - len(s.tabs)
}

// RenameResults moves conn from's result sets to to, after a connection
// rename, so a query tab that visited the old name finds its results under
// the new one. A set already under to (a name reused) is replaced.
func (w *Workspace) RenameResults(from, to string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if s, ok := w.sets[from]; ok && from != to {
		delete(w.sets, from)
		w.sets[to] = s
	}
}

// DropResults forgets conn's result set — after the connection is removed,
// so its results do not stay alive in every query tab that visited it.
func (w *Workspace) DropResults(conn string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.sets, conn)
}
