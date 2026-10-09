package tui

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/rohanthewiz/serr"

	"github.com/rohanthewiz/dbc/jobs"
	"github.com/rohanthewiz/dbc/pipeline"
	"github.com/rohanthewiz/dbc/userdata"
)

// The Runs list (⌥J, ◷ Runs in the browser, a click on the status bar's
// indicator) and the run monitor — dbc web's Runs view (runs.js) without
// the drawing: the list of runs, and a run as a tree with drilldown.
//
//	⌥J ─► the Runs list: every run, newest first — this process's live ones
//	  │   and the records in runs_dir (dbc web's, a cron's `dbc job run`)
//	  └─Enter─► the run monitor ─Backspace─► the list again
//
//	╭ Run 20261009-020000-7f3a ───────────────────────────────────── ✕ ╮
//	│ job nightly · manual · started 02:00:00 · min_age=2      ■ Stop ^K │
//	│ ▾ ● running                       3.21s     8 rows                │
//	│   ▾ ✓ copy    copy-cats           22ms      8 rows                │
//	│     ▾ ✓ cats                      22ms      8 rows                │
//	│         src   sql.table   0 → 8   1 batch   2ms                   │
//	│         dst   sql.write   8 → 8   1 batch   14ms   571 rows/s     │
//	│   ▸ ● clean   clean-and-load      1.2s      5 rows   after copy   │
//	│   ▸ ○ report  cats-report                            after clean  │
//	│ ── log · clean ─────────────────────────────────────────────────── │
//	│ 02:00:01 ▶ clean: pipeline clean-and-load                         │
//	│ ↑↓ move · Enter fold · ^K stop · y id · Y as text · ⌫ list · Esc   │
//	╰────────────────────────────────────────────────────────────────────╯
//
// The tree is the record's: run → pipelines (a job's steps; one for a
// pipeline run) → fragments → nodes, with the state glyphs `dbc run show`
// prints. The row under the cursor narrows the log below it, as the web's
// run page does: the run shows every line, a step its own, a fragment (or
// a node) the step's lines logged while it ran or naming it; a row's error
// is pinned above the log in full.
//
// LIVE. The monitor keeps a copy of the record (Engine.Get) and applies
// the engine's events to it as they come (runsChanged): a log line is
// appended, a fragment's counters replaced, a state change re-reads the
// record. A run another process runs sends this engine nothing, so the
// 1s tick re-reads it every 2s while it is live (jobTick). Stop (^K)
// stops a run of this process; another's is refused with where it runs.

// runsListMax bounds the Runs list: the newest this many.
const runsListMax = 300

// runsModal is the Runs list.
type runsModal struct {
	modalBase
	// kind and name narrow the list to one job's or pipeline's runs (the
	// browser's h); "" lists every run
	kind, name string
	heads      []jobs.Summary // as last read, newest first
	note       string         // a refusal or a read error, said in the list
	// stamp is the runs directory's as last read (userdata.RunsStamp): the
	// tick re-reads the records only when it has moved
	stamp time.Time

	filter     *editor
	filtering  bool
	lst        *list
	filterRect Rect
}

// openRuns opens the Runs list, narrowed to kind/name when given.
func (m *Model) openRuns(kind, name string) tea.Cmd {
	rm := &runsModal{kind: kind, name: name, filter: newEditor(true), lst: newList()}
	rm.filter.placeholder = "/ filters by name, status, trigger, error"
	rm.reload(m)
	m.openModal(rm)
	return m.ensureJobTick()
}

// reload reads the runs again (live and on disk), keeping the cursor on
// the run it was on.
func (rm *runsModal) reload(m *Model) {
	rm.stamp = userdata.RunsStamp(m.runsDir)
	heads, err := m.jobs.History(userdata.RunFilter{Kind: rm.kind, Name: rm.name, Limit: runsListMax})
	if err != nil {
		rm.note = "could not read the run records: " + serr.StringFromErr(err)
	}
	rm.heads = heads
	rm.build(m)
}

// tick is the jobs' 1s refresh while the list is open. The elapsed times
// move every second, drawn from what the list holds; the records are read
// again only every other tick and only when the runs directory says one
// was written since (a run of another process starting, ending, or — every
// 2s while it runs — rewriting its record). Every listing reads every
// record whole, which the runs_keep prune bounds but does not make free,
// so an idle list does not do it every two seconds.
func (rm *runsModal) tick(m *Model, disk bool) {
	if disk && !userdata.RunsStamp(m.runsDir).Equal(rm.stamp) {
		rm.reload(m)
		return
	}
	rm.build(m)
}

// build lays the rows out for the filter, keeping the cursor's run.
func (rm *runsModal) build(m *Model) {
	keep := ""
	if h, ok := rm.current(); ok {
		keep = h.ID
	}
	q := strings.ToLower(strings.TrimSpace(rm.filter.Text()))
	now := time.Now()
	var items []listItem
	for _, h := range rm.heads {
		hay := strings.ToLower(strings.Join([]string{h.ID, h.Kind, h.Name, h.Status, h.Trigger, h.By, h.Error}, " "))
		if q != "" && !strings.Contains(hay, q) {
			continue
		}
		st := pipeline.Status(h.Status)
		mark := m.runStyle(st)
		desc := fmt.Sprintf("%-8s  %-8s  %s", h.Kind, h.Trigger, when(h.Started, now))
		if h.Error != "" && st != pipeline.Succeeded {
			desc += " · " + h.Error
		}
		sub := took(h.Started, h.Ended, now)
		if h.Rows > 0 || st == pipeline.Succeeded {
			sub += " · " + plural(int(h.Rows), "row")
		}
		items = append(items, listItem{label: h.Name, mark: runGlyph(st), markSt: &mark, desc: desc, sub: sub, data: h})
	}
	w := 0
	for _, it := range items {
		w = max(w, width(it.label))
	}
	rm.lst.descCol = min(w, 28) + 2
	rm.lst.set(items)
	for i, it := range rm.lst.items {
		if h, ok := it.data.(jobs.Summary); ok && h.ID == keep {
			rm.lst.cur = i
			rm.lst.ensureVisible()
		}
	}
}

// when is a run's start for a list: the time today, else the date too.
func when(t, now time.Time) string {
	t = t.Local()
	if y, m, d := t.Date(); y == now.Year() && m == now.Month() && d == now.Day() {
		return t.Format("15:04:05")
	}
	return t.Format("Jan 2 15:04")
}

// took is a run's (or a part's) time: so far while it runs, "" before.
func took(from, to, now time.Time) string {
	if from.IsZero() {
		return ""
	}
	if to.IsZero() {
		return shortDur(now.Sub(from))
	}
	return spanText(to.Sub(from))
}

// spanText is a finished span: to the millisecond under a minute ("<1ms"
// under one, rather than a "0s" that reads as "did not run"), to the
// second above.
func spanText(d time.Duration) string {
	switch {
	case d < time.Millisecond:
		return "<1ms"
	case d < time.Minute:
		return d.Round(time.Millisecond).String()
	}
	return shortDur(d)
}

// current is the run under the cursor.
func (rm *runsModal) current() (jobs.Summary, bool) {
	it, ok := rm.lst.current()
	if !ok {
		return jobs.Summary{}, false
	}
	h, ok := it.data.(jobs.Summary)
	return h, ok
}

func (rm *runsModal) title() string {
	t := "Runs"
	if rm.name != "" {
		t += " · " + rm.kind + " " + rm.name
	}
	live := 0
	for _, h := range rm.heads {
		if h.Status == string(pipeline.Running) || h.Status == string(pipeline.Queued) {
			live++
		}
	}
	if live > 0 {
		t += fmt.Sprintf(" · %d running", live)
	}
	return t
}

func (rm *runsModal) size(w, h int) (int, int) {
	return max(min(w-4, 120), min(72, w-4)), max(12, min(len(rm.lst.items)+6, h*3/4))
}

func (rm *runsModal) footer() string {
	if rm.filtering {
		return "typing filters · ↑↓ move · Enter opens · Esc/Tab: back to the list"
	}
	f := "Enter opens · ^K stops · y copies the id · / filter · Esc close"
	if rm.name != "" {
		f = "Enter opens · ^K stops · y copies the id · a all runs · / filter · Esc close"
	}
	return f
}

func (rm *runsModal) draw(m *Model, s Surface) *caret {
	bg := m.st.panel
	s.Put(1, 0, "⌕", onBg(m.st.accent, bg))
	field := s.Sub(Rect{3, 0, s.W() - 4, 1})
	rm.filterRect = field.Rect()
	cx, cy, ok := rm.filter.Draw(field, m.st, pick(rm.filtering, m.st.raised, bg), [2]int{}, true)
	listH := s.H() - 3
	if rm.note != "" {
		s.Put(1, s.H()-2, truncate(rm.note, s.W()-2), onBg(m.st.warn, bg))
		listH--
	}
	empty := "no runs yet — Ctrl+J picks a pipeline or job, Enter runs it"
	if rm.name != "" {
		empty = "no runs of the " + rm.kind + " " + rm.name + " yet — a lists every run"
	}
	if strings.TrimSpace(rm.filter.Text()) != "" {
		empty = "no run matches"
	}
	rm.lst.draw(s.Sub(Rect{0, 2, s.W(), listH}), m.st, bg, true, empty)
	s.Put(1, s.H()-1, truncate(rm.footer(), s.W()-2), onBg(m.st.muted, bg))
	if ok && rm.filtering {
		return &caret{cx, cy}
	}
	return nil
}

func (rm *runsModal) key(m *Model, k tea.KeyPressMsg) (tea.Cmd, bool) {
	if rm.filtering {
		switch k.String() {
		case "esc", "tab", "shift+tab":
			rm.filtering = false
			return nil, false
		case "enter":
			rm.filtering = false
			return rm.open(m), false
		case "up", "down", "pgup", "pgdown", "ctrl+p", "ctrl+n":
			rm.lst.key(k)
			return nil, false
		}
		before := rm.filter.Text()
		rm.filter.HandleKey(k)
		if rm.filter.Text() != before {
			rm.lst.cur = 0
			rm.build(m)
		}
		return nil, false
	}
	switch k.String() {
	case "esc":
		return nil, true
	case "/":
		rm.filtering = true
	case "enter":
		return rm.open(m), false
	case "ctrl+k":
		if h, ok := rm.current(); ok {
			rm.note = m.stopRun(h.ID, h.Status)
		}
	case "y":
		if h, ok := rm.current(); ok {
			return m.copyString(h.ID, "the run's id"), false
		}
	case "a":
		if rm.name != "" {
			rm.kind, rm.name = "", ""
			rm.reload(m)
		}
	default:
		rm.lst.key(k)
	}
	return nil, false
}

// open opens the monitor on the run under the cursor; Backspace there
// comes back to this list.
func (rm *runsModal) open(m *Model) tea.Cmd {
	h, ok := rm.current()
	if !ok {
		return nil
	}
	return m.openMonitor(h.ID, rm)
}

func (rm *runsModal) click(m *Model, x, y, clicks int, shift bool) (tea.Cmd, bool) {
	if m.modalClose().Contains(x, y) {
		return nil, true
	}
	if rm.filterRect.Contains(x, y) {
		rm.filtering = true
		rm.filter.Click(x, y, clicks, shift)
		return nil, false
	}
	if i := rm.lst.indexAt(x, y); i >= 0 {
		rm.lst.cur, rm.filtering = i, false
		if clicks >= 2 {
			return rm.open(m), false
		}
	}
	return nil, false
}

func (rm *runsModal) paste(m *Model, s string) {
	rm.filtering = true
	rm.filter.Insert(s)
	rm.lst.cur = 0
	rm.build(m)
}

func (rm *runsModal) hover(m *Model, x, y int)     { rm.lst.hover = rm.lst.indexAt(x, y) }
func (rm *runsModal) wheel(m *Model, x, y, dy int) { rm.lst.scroll(dy) }

// stopRun stops run id (status as the caller last saw it) and returns what
// to say: stopping, already ended, or — for a run another process runs —
// where to stop it. Said in the log too, which outlives the dialog.
func (m *Model) stopRun(id, status string) string {
	if status != string(pipeline.Running) && status != string(pipeline.Queued) {
		return "that run has ended (" + status + ") — nothing to stop"
	}
	err := m.jobs.Cancel(id)
	switch {
	case errors.Is(err, jobs.ErrElsewhere):
		msg := strings.TrimPrefix(err.Error(), jobs.ErrElsewhere.Error()+": ")
		m.log(logWarn, msg)
		return msg
	case err != nil:
		msg := "could not stop run " + id + ": " + serr.StringFromErr(err)
		m.log(logErr, msg)
		return msg
	}
	msg := "stopping run " + id + " — every fragment in flight rolls back"
	m.log(logInfo, msg)
	return msg
}

// ---------------------------------------------------------------------------
// The run monitor
// ---------------------------------------------------------------------------

// monRow is one row of the monitor's tree: the run itself, or one of its
// pipelines, fragments or nodes, by index into the record.
type monRow struct {
	depth      int // 0 the run, 1 a pipeline, 2 a fragment, 3 a node
	pi, fi, ni int
}

// key names a row across re-reads, for the cursor and the folds.
func (r monRow) key(run *jobs.Run) string {
	switch r.depth {
	case 1:
		return "p:" + run.Pipelines[r.pi].ID
	case 2:
		p := run.Pipelines[r.pi]
		return "f:" + p.ID + "/" + p.Fragments[r.fi].Name
	case 3:
		p := run.Pipelines[r.pi]
		f := p.Fragments[r.fi]
		return "n:" + p.ID + "/" + f.Name + "/" + f.Nodes[r.ni].ID
	}
	return "run"
}

// runMonitor is one run as a tree, live.
type runMonitor struct {
	modalBase
	id    string
	run   jobs.Run
	back  *runsModal // where Backspace goes; nil when opened from elsewhere
	note  string     // what a Stop or a copy said
	rows  []monRow
	cur   int
	top   int
	fold  map[string]bool // folded rows, by key
	logUp int             // lines scrolled back from the newest (0: follow)

	treeView, logView, stopBtn Rect
	hoverStop                  bool
}

// openMonitor opens run id's monitor. back, when set, is the Runs list it
// was opened from.
func (m *Model) openMonitor(id string, back *runsModal) tea.Cmd {
	mon := &runMonitor{id: id, back: back, fold: map[string]bool{}}
	if !mon.reload(m) {
		m.logf(logWarn, "no run %s — not running, and no record of it", id)
		return nil
	}
	m.openModal(mon)
	return m.ensureJobTick()
}

// reload reads the record again, keeping the cursor on its row.
func (mon *runMonitor) reload(m *Model) bool {
	r, ok := m.jobs.Get(mon.id)
	if !ok {
		return false
	}
	keep := ""
	if mon.cur < len(mon.rows) {
		keep = mon.rows[mon.cur].key(&mon.run)
	}
	mon.run = r
	mon.build(keep)
	return true
}

// liveHere reports whether the run is going in this process (its Stop
// works, and its events keep the copy fresh).
func (m *Model) liveHere(id string) bool {
	return slices.ContainsFunc(m.liveRuns, func(r jobs.Run) bool { return r.ID == id })
}

// refreshElsewhere re-reads a live run another process runs: nothing else
// would bring its progress here. (Called every 2s by jobTick.)
func (mon *runMonitor) refreshElsewhere(m *Model) {
	if mon.live() && !m.liveHere(mon.id) {
		mon.reload(m)
	}
}

func (mon *runMonitor) live() bool {
	return mon.run.Status == pipeline.Running || mon.run.Status == pipeline.Queued
}

// apply brings one of the run's events into the copy without re-reading
// the whole record for each line or counter: a line is appended, a
// fragment's counters replaced; a change of state re-reads it.
func (mon *runMonitor) apply(m *Model, ev jobs.Event) {
	switch e := ev.(type) {
	case *jobs.Logged:
		mon.run.Log = append(mon.run.Log, e.Line)
		if over := len(mon.run.Log) - jobs.MaxLogLines; over > 0 {
			mon.run.Log = mon.run.Log[over:]
		}
	case *jobs.Progress:
		for i := range mon.run.Pipelines {
			p := &mon.run.Pipelines[i]
			if p.ID != e.Pipeline {
				continue
			}
			if j := slices.IndexFunc(p.Fragments, func(f pipeline.FragmentStats) bool { return f.Name == e.Fragment.Name }); j >= 0 {
				p.Fragments[j] = e.Fragment
			}
		}
	default:
		mon.reload(m)
	}
}

// build flattens the record into rows, leaving out what folded rows hold,
// and puts the cursor back on keep.
func (mon *runMonitor) build(keep string) {
	r := &mon.run
	rows := []monRow{{depth: 0}}
	if !mon.fold["run"] {
		for pi, p := range r.Pipelines {
			pr := monRow{depth: 1, pi: pi}
			rows = append(rows, pr)
			if mon.fold[pr.key(r)] {
				continue
			}
			for fi, f := range p.Fragments {
				fr := monRow{depth: 2, pi: pi, fi: fi}
				rows = append(rows, fr)
				if mon.fold[fr.key(r)] {
					continue
				}
				for ni := range f.Nodes {
					rows = append(rows, monRow{depth: 3, pi: pi, fi: fi, ni: ni})
				}
			}
		}
	}
	mon.rows = rows
	mon.cur = min(mon.cur, len(rows)-1)
	for i, row := range rows {
		if row.key(r) == keep {
			mon.cur = i
		}
	}
}

// hasKids reports whether a row can fold.
func (mon *runMonitor) hasKids(row monRow) bool {
	r := &mon.run
	switch row.depth {
	case 0:
		return len(r.Pipelines) > 0
	case 1:
		return len(r.Pipelines[row.pi].Fragments) > 0
	case 2:
		return len(r.Pipelines[row.pi].Fragments[row.fi].Nodes) > 0
	}
	return false
}

// setFold folds (or unfolds) the row under the cursor.
func (mon *runMonitor) setFold(folded bool) {
	if mon.cur >= len(mon.rows) || !mon.hasKids(mon.rows[mon.cur]) {
		return
	}
	k := mon.rows[mon.cur].key(&mon.run)
	if folded {
		mon.fold[k] = true
	} else {
		delete(mon.fold, k)
	}
	mon.build(k)
}

func (mon *runMonitor) title() string { return "Run " + mon.id }

// size fits the run: the header, every tree row, the rule, ten lines of
// log and the footer — between 16 rows and four fifths of the screen, past
// which the tree scrolls. Rows come and go only by folding, so a live run
// does not make the dialog jump as its log grows.
func (mon *runMonitor) size(w, h int) (int, int) {
	want := 1 + len(mon.rows) + 1 + 10 + 1 + 2 // + the frame
	if mon.note != "" {
		want++
	}
	return max(min(w-4, 124), min(72, w-4)), max(16, min(want, h*4/5))
}

// selected is what the row under the cursor is: its pipeline and fragment
// (nil when the row is above them), for the log's narrowing and the
// pinned error.
func (mon *runMonitor) selected() (*jobs.PipelineRun, *pipeline.FragmentStats, *pipeline.NodeStats) {
	if mon.cur >= len(mon.rows) {
		return nil, nil, nil
	}
	row := mon.rows[mon.cur]
	if row.depth == 0 {
		return nil, nil, nil
	}
	p := &mon.run.Pipelines[row.pi]
	if row.depth == 1 {
		return p, nil, nil
	}
	f := &p.Fragments[row.fi]
	if row.depth == 2 {
		return p, f, nil
	}
	return p, f, &f.Nodes[row.ni]
}

// logLines is the run's log narrowed to the selection (see the top).
func (mon *runMonitor) logLines() ([]jobs.Line, string) {
	p, f, _ := mon.selected()
	if p == nil {
		return mon.run.Log, "the run"
	}
	var out []jobs.Line
	for _, l := range mon.run.Log {
		if l.Pipeline != p.ID {
			continue
		}
		if f != nil && !strings.Contains(l.Text, f.Name) {
			end := f.Ended
			if end.IsZero() {
				end = time.Now()
			}
			if f.Started.IsZero() || l.At.Before(f.Started) || l.At.After(end) {
				continue
			}
		}
		out = append(out, l)
	}
	if f != nil {
		return out, "fragment " + f.Name
	}
	return out, p.ID
}

// rowError is the error of the row under the cursor, for the pin above the
// log; the run's own when the run row is.
func (mon *runMonitor) rowError() string {
	p, f, n := mon.selected()
	switch {
	case n != nil:
		return n.Error
	case f != nil:
		return f.Error
	case p != nil:
		return p.Error
	}
	return mon.run.Error
}

func (mon *runMonitor) draw(m *Model, s Surface) *caret {
	bg := m.st.panel
	r := &mon.run
	now := time.Now()
	muted := onBg(m.st.muted, bg)

	// the header: what the run is, and its Stop
	head := []string{r.Kind + " " + r.Name}
	trig := r.Trigger
	if r.By != "" {
		trig += " (" + r.By + ")"
	}
	head = append(head, trig, "started "+when(r.Started, now))
	for _, k := range sortedParamKeys(r.Params) {
		head = append(head, k+"="+r.Params[k])
	}
	mon.stopBtn = Rect{}
	right := s.W() - 1
	if mon.live() {
		label := " ■ Stop ^K "
		mon.stopBtn = chip(s, s.W()-1-width(label), 0, label, pick(mon.hoverStop, m.st.buttonHover, m.st.buttonStop))
		right = mon.stopBtn.X - s.Rect().X - 1
	}
	s.Put(1, 0, truncate(strings.Join(head, " · "), right-1), onBg(m.st.base, bg))

	// the layout below the header: the tree, a rule naming what the log
	// shows, the pinned error, the log, the note and the footer
	footer, noteH := 1, 0
	if mon.note != "" {
		noteH = 1
	}
	errText := mon.rowError()
	errLines := []string{}
	if errText != "" {
		for _, l := range strings.Split(errText, "\n") {
			errLines = append(errLines, wrap(l, s.W()-2)...)
		}
		errLines = errLines[:min(len(errLines), 3)]
	}
	avail := s.H() - 1 - footer - noteH - 1 // header, footer, note, the rule
	treeH := max(3, min(len(mon.rows), avail*3/5))
	logH := max(1, avail-treeH-len(errLines))

	tree := s.Sub(Rect{0, 1, s.W(), treeH})
	mon.treeView = tree.Rect()
	mon.drawTree(m, tree, now)

	ruleY := 1 + treeH
	lines, what := mon.logLines()
	rule := "── log · " + what + " "
	s.Put(1, ruleY, rule+strings.Repeat("─", max(0, s.W()-2-width(rule))), muted)
	y := ruleY + 1
	for _, l := range errLines {
		s.Put(1, y, l, onBg(m.st.err, bg))
		y++
	}
	logS := s.Sub(Rect{1, y, s.W() - 2, logH})
	mon.logView = logS.Rect()
	mon.drawLog(m, logS, lines)

	if mon.note != "" {
		s.Put(1, s.H()-2, truncate(mon.note, s.W()-2), onBg(m.st.warn, bg))
	}
	foot := "↑↓ move · Enter fold · ^K stop · y id · Y as text · Esc close"
	if mon.back != nil {
		foot = "↑↓ move · Enter fold · ^K stop · y id · Y as text · ⌫ the runs · Esc close"
	}
	s.Put(1, s.H()-1, truncate(foot, s.W()-2), muted)
	return nil
}

// drawTree draws the rows: a fold mark and a state glyph, then columns
// that line up across the levels — what it is, its time, its rows, and
// what more there is to say (after, direct COPY, node counters, an error).
func (mon *runMonitor) drawTree(m *Model, s Surface, now time.Time) {
	bg := m.st.panel
	r := &mon.run
	s.Fill(bg)
	if mon.cur < mon.top {
		mon.top = mon.cur
	} else if mon.cur >= mon.top+s.H() {
		mon.top = mon.cur - s.H() + 1
	}
	mon.top = max(0, min(mon.top, max(len(mon.rows)-s.H(), 0)))

	// The name column ends where the widest name ends, at whatever level:
	// a row's name starts after its indent (two cells a level), its fold
	// mark and its glyph (two cells each), so every row's time and rows
	// line up in the columns after it. Half the width at most: the facts
	// on the right matter more than a long name's tail.
	nameEnd := 16
	for _, row := range mon.rows {
		nameEnd = max(nameEnd, 1+row.depth*2+4+width(mon.rowName(row)))
	}
	nameEnd = min(nameEnd, s.W()/2)

	for y := 0; y < s.H(); y++ {
		i := mon.top + y
		if i >= len(mon.rows) {
			break
		}
		row := mon.rows[i]
		rs := bg
		if i == mon.cur {
			rs = m.st.sel
		}
		line := s.Sub(Rect{0, y, s.W(), 1})
		line.Fill(rs)
		status, tm, rows, more := mon.rowFacts(row, now)
		fold := " "
		if mon.hasKids(row) {
			fold = pick(mon.fold[row.key(r)], "▸", "▾")
		}
		x := 1 + row.depth*2
		x = line.Put(x, 0, fold+" ", rs.WithFg(m.st.muted.Fg))
		if row.depth == 3 {
			x = line.Put(x, 0, "  ", rs) // nodes have no state of their own
		} else {
			x = line.Put(x, 0, runGlyph(status)+" ", rs.WithFg(m.runStyle(status).Fg))
		}
		line.Put(x, 0, truncate(mon.rowName(row), max(1, nameEnd-x)), rs)
		col := nameEnd + 1
		line.Put(col, 0, fmt.Sprintf("%9s", tm), rs.WithFg(m.st.muted.Fg))
		if rows != "" {
			line.Put(col+10, 0, fmt.Sprintf("%11s", rows), rs)
		}
		if more != "" {
			st := rs.WithFg(m.st.muted.Fg)
			if strings.HasPrefix(more, "✗ ") {
				st = rs.WithFg(m.st.err.Fg)
			}
			line.Put(col+23, 0, truncate(more, max(0, s.W()-col-24)), st)
		}
	}
	if len(mon.rows) > s.H() {
		drawVBar(s.Sub(Rect{s.W() - 1, 0, 1, s.H()}), m.st, mon.top, s.H(), len(mon.rows))
	}
}

// rowName is a row's first column: the run's state, a step's id and
// pipeline, a fragment's name, a node's id and plugin.
func (mon *runMonitor) rowName(row monRow) string {
	r := &mon.run
	switch row.depth {
	case 0:
		return string(r.Status)
	case 1:
		p := r.Pipelines[row.pi]
		if r.Kind == jobs.KindJob && p.Pipeline != "" && p.Pipeline != p.ID {
			return p.ID + "  " + p.Pipeline
		}
		return p.ID
	case 2:
		return r.Pipelines[row.pi].Fragments[row.fi].Name
	}
	n := r.Pipelines[row.pi].Fragments[row.fi].Nodes[row.ni]
	return n.ID + "  " + n.Plugin
}

// rowFacts is what the columns say of a row: its state, its time (so far,
// while it runs), its rows — a node's in → out — and the rest: a step's
// afters, a fragment's direct COPY (and its nodes' counters while it is
// folded), a node's batches and speed, or the row's error.
func (mon *runMonitor) rowFacts(row monRow, now time.Time) (status pipeline.Status, tm, rows, more string) {
	r := &mon.run
	switch row.depth {
	case 0:
		var n int64
		for _, p := range r.Pipelines {
			n += p.Rows()
		}
		if r.Error != "" && r.Status != pipeline.Succeeded {
			more = "✗ " + firstLine(r.Error)
		}
		return r.Status, took(r.Started, r.Ended, now), plural(int(n), "row"), more
	case 1:
		p := r.Pipelines[row.pi]
		if len(p.After) > 0 {
			more = "after " + strings.Join(p.After, ", ")
		}
		if p.Error != "" {
			more = "✗ " + firstLine(p.Error)
		}
		if p.Status == pipeline.Queued || p.Status == pipeline.Skipped {
			return p.Status, "", "", more
		}
		return p.Status, took(p.Started, p.Ended, now), plural(int(p.Rows()), "row"), more
	case 2:
		f := r.Pipelines[row.pi].Fragments[row.fi]
		if f.Direct {
			more = "direct COPY"
		}
		if mon.fold["f:"+r.Pipelines[row.pi].ID+"/"+f.Name] {
			// folded: its nodes' counters on its own line
			var ns []string
			for _, n := range f.Nodes {
				ns = append(ns, fmt.Sprintf("%s %d→%d", n.ID, n.In, n.Out))
			}
			more = strings.TrimSpace(more + "  " + strings.Join(ns, " · "))
		}
		if f.Error != "" {
			more = "✗ " + firstLine(f.Error)
		}
		if f.Status == pipeline.Queued || f.Status == pipeline.Skipped {
			return f.Status, "", "", more
		}
		return f.Status, took(f.Started, f.Ended, now), plural(int(f.Rows), "row"), more
	}
	n := r.Pipelines[row.pi].Fragments[row.fi].Nodes[row.ni]
	more = plural(int(n.Batches), "batch")
	// a speed over less than a tenth of a second is mostly the clock's
	// grain: left out, rather than a six-figure rows/s from 8 rows
	if secs := n.Elapsed.Seconds(); n.Out > 0 && n.Elapsed >= 100*time.Millisecond {
		more += fmt.Sprintf(" · %.0f rows/s", float64(n.Out)/secs)
	}
	if n.Error != "" {
		more = "✗ " + firstLine(n.Error)
	}
	return "", spanText(n.Elapsed), fmt.Sprintf("%d → %d", n.In, n.Out), more
}

// firstLine is an error's first line, for a row; the pin shows it whole.
func firstLine(s string) string {
	l, _, _ := strings.Cut(s, "\n")
	return l
}

// drawLog draws the newest lines that fit, or older ones scrolled back to.
func (mon *runMonitor) drawLog(m *Model, s Surface, lines []jobs.Line) {
	bg := m.st.panel
	s.Fill(bg)
	if len(lines) == 0 {
		s.Put(0, 0, "nothing logged here yet", onBg(m.st.muted, bg))
		return
	}
	// lines wrap at the width; the window is counted in visual rows from
	// the newest, so a long line does not push the newest out of view
	type vis struct {
		text string
		st   Style
	}
	var rows []vis
	for _, l := range lines {
		st := onBg(m.st.base, bg)
		switch l.Level {
		case "err":
			st = onBg(m.st.err, bg)
		case "warn":
			st = onBg(m.st.warn, bg)
		}
		for i, part := range wrap(l.At.Local().Format("15:04:05")+" "+l.Text, s.W()) {
			if i > 0 {
				part = "         " + part
			}
			rows = append(rows, vis{part, st})
		}
	}
	mon.logUp = max(0, min(mon.logUp, len(rows)-s.H()))
	end := len(rows) - mon.logUp
	start := max(0, end-s.H())
	for y, v := range rows[start:end] {
		s.Put(0, y, truncate(v.text, s.W()), v.st)
	}
}

// sortedParamKeys is a run's params in name order.
func sortedParamKeys(ps map[string]string) []string {
	keys := make([]string, 0, len(ps))
	for k := range ps {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

func (mon *runMonitor) key(m *Model, k tea.KeyPressMsg) (tea.Cmd, bool) {
	switch k.String() {
	case "esc", "q":
		return nil, true
	case "backspace":
		if mon.back != nil {
			mon.back.reload(m)
			m.openModal(mon.back)
		}
		return nil, false
	case "up", "k":
		mon.cur = max(0, mon.cur-1)
	case "down", "j":
		mon.cur = min(len(mon.rows)-1, mon.cur+1)
	case "pgup":
		mon.cur = max(0, mon.cur-max(mon.treeView.H-1, 1))
	case "pgdown":
		mon.cur = min(len(mon.rows)-1, mon.cur+max(mon.treeView.H-1, 1))
	case "home", "g":
		mon.cur = 0
	case "end", "G":
		mon.cur = len(mon.rows) - 1
	case "enter", "space":
		if mon.cur < len(mon.rows) {
			mon.setFold(!mon.fold[mon.rows[mon.cur].key(&mon.run)])
		}
	case "left", "h":
		mon.foldOrParent()
	case "right", "l":
		mon.setFold(false)
	case "ctrl+k":
		mon.note = m.stopRun(mon.id, string(mon.run.Status))
	case "y":
		mon.note = "copied the run's id"
		return m.copyString(mon.id, "the run's id"), false
	case "Y":
		mon.note = "copied the run as text"
		return m.copyString(mon.run.Tree(), "the run as text"), false
	}
	mon.logUp = 0 // a move shows the newest lines of what is selected
	return nil, false
}

// foldOrParent is ←: fold the row, or when it is folded (or a leaf) go to
// the row above it in the tree.
func (mon *runMonitor) foldOrParent() {
	if mon.cur >= len(mon.rows) {
		return
	}
	row := mon.rows[mon.cur]
	if mon.hasKids(row) && !mon.fold[row.key(&mon.run)] {
		mon.setFold(true)
		return
	}
	for i := mon.cur - 1; i >= 0; i-- {
		if mon.rows[i].depth < row.depth {
			mon.cur = i
			return
		}
	}
}

func (mon *runMonitor) click(m *Model, x, y, clicks int, shift bool) (tea.Cmd, bool) {
	switch {
	case m.modalClose().Contains(x, y):
		return nil, true
	case mon.stopBtn.Contains(x, y):
		mon.note = m.stopRun(mon.id, string(mon.run.Status))
		return nil, false
	case mon.treeView.Contains(x, y):
		i := mon.top + y - mon.treeView.Y
		if i >= len(mon.rows) {
			return nil, false
		}
		mon.cur, mon.logUp = i, 0
		row := mon.rows[i]
		// the fold mark, or a double-click anywhere on the row, folds
		foldX := mon.treeView.X + 1 + row.depth*2
		if clicks >= 2 || x == foldX {
			mon.setFold(!mon.fold[row.key(&mon.run)])
		}
	}
	return nil, false
}

func (mon *runMonitor) hover(m *Model, x, y int) { mon.hoverStop = mon.stopBtn.Contains(x, y) }

func (mon *runMonitor) wheel(m *Model, x, y, dy int) {
	if mon.logView.Contains(x, y) {
		mon.logUp = max(0, mon.logUp-dy) // the wheel down goes towards the newest
		return
	}
	mon.top = max(0, mon.top+dy)
	mon.cur = max(mon.top, min(mon.cur, mon.top+mon.treeView.H-1, len(mon.rows)-1))
}

// runsChanged brings an engine event to whichever view of runs is open:
// the monitor applies its own run's, the Runs list re-reads on a start,
// a change of state or an end, and the browser redraws its running marks.
func (m *Model) runsChanged(ev jobs.Event) {
	switch md := m.modal.(type) {
	case *runMonitor:
		if ev.RunID() == md.id {
			md.apply(m, ev)
		}
	case *runsModal:
		switch ev.(type) {
		case *jobs.RunStarted, *jobs.State, *jobs.RunDone:
			md.reload(m)
		}
	case *pipesModal:
		switch ev.(type) {
		case *jobs.RunStarted, *jobs.State, *jobs.RunDone:
			md.build(m)
		}
	}
}
