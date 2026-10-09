package tui

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/rohanthewiz/serr"

	"github.com/rohanthewiz/dbc/pgdump"
)

// "Dump database…" in the connections menu: a Postgres connection dumped
// with pg_dump, the way `dbc dump` does it. The checks, the connection
// handed to pg_dump, the pg_dump picked and the words logged are package
// pgdump's, shared with the CLI and dbc web; this file is the form and the
// keys.
//
//	connections menu ─► Dump database… ─► the form (format, where, what)
//	Dump (Enter) ─► Form.Options: refused? say it in the form
//	             ─► tea.Cmd: pgdump.Prepare (asks the server its version,
//	                finds pg_dump) ─► dumpPreparedMsg
//	                   ├ error ─► said in the form, which stays open
//	                   └ ok    ─► form closes; tea.Cmd: Run.Report, each
//	                              line to the log by m.send ─► dumpDoneMsg
//	connections menu ─► ■ Stop dump of X ─► cancel: pg_dump is interrupted,
//	                                       and its partial output removed
//
// Prepare runs off the UI goroutine because it connects (the server's
// version picks the pg_dump), which can take connect_timeout; until it
// answers the form says "checking…" and stays usable. Answers are numbered
// (seq), so one that comes back after the form was closed, or after a
// second press, is dropped.
//
// One dump at a time: dumps are long and heavy on the server, and a second
// would mostly be a slip of the hand. While one runs, the menu row says so
// and offers Stop instead.

// dfCtl is one control of the dump form, in focus order.
type dfCtl int

const (
	dfFormat dfCtl = iota
	dfOut
	dfJobs
	dfContent
	dfSchemas
	dfTables
	dfExclTables
	dfExclData
	dfNoOwner
	dfNoPrivileges
	dfInserts
	dfCreate
	dfClean
	dfExtra
	dfDump
	dfCancel
	dfNone dfCtl = -1
)

// dumpFormatInfo is a format chip, with what the format writes.
type dumpFormatInfo struct {
	f    pgdump.Format
	what string
}

// dfFormats are the format chips.
var dfFormats = []dumpFormatInfo{
	{pgdump.Plain, "one SQL file, for psql"},
	{pgdump.Custom, "one archive, for pg_restore"},
	{pgdump.Directory, "a file per table, for pg_restore; dumps tables in parallel"},
	{pgdump.Tar, "one tar archive, for pg_restore"},
	{pgdump.Split, "SQL, a file per table, and a restore.sql for psql"},
}

// dfContents are the "What" chips: pgdump.Form.Content values and labels.
var dfContents = [][2]string{{"", "everything"}, {"schema", "schema only"}, {"data", "data only"}}

// dfLabelW is the width of the label column.
const dfLabelW = 15

// dumpForm is the dump dialog for one connection.
type dumpForm struct {
	modalBase
	conn    string
	format  pgdump.Format
	content string
	checks  map[dfCtl]bool    // the checkboxes
	fields  map[dfCtl]*editor // the text controls
	focus   dfCtl
	hovered dfCtl

	msg     string
	msgKind logKind
	seq     int  // the latest Dump press; older answers are dropped
	busy    bool // a Prepare is out

	rects    map[dfCtl]Rect
	formatR  []Rect
	contentR []Rect
}

// dumpPreparedMsg is Prepare's answer to the press numbered seq.
type dumpPreparedMsg struct {
	seq int
	run *pgdump.Run
	err error
}

// dumpLineMsg is a line of a running dump, for the log.
type dumpLineMsg struct{ level, text string }

// dumpDoneMsg is a dump's end; Report has logged how it went.
type dumpDoneMsg struct{ conn string }

// dumpItems are the connections menu's rows for target: Dump database…,
// dimmed with the reason where it cannot run, and Stop while a dump is out.
func (m *Model) dumpItems(target string) []menuItem {
	why := ""
	if cn, ok := m.cfg.ConnByName(target); !ok || !pgdump.IsPostgres(cn.Driver) {
		why = "pg_dump dumps Postgres databases only"
	} else if m.dumpBusy != "" {
		why = "a dump of " + m.dumpBusy + " is still running — one at a time"
	}
	items := []menuItem{{label: "⇩ Dump database…", why: why,
		act: func(m *Model) tea.Cmd { m.openDumpForm(target); return nil }}}
	if m.dumpBusy != "" {
		items = append(items, menuItem{label: "■ Stop dump of " + m.dumpBusy,
			act: func(m *Model) tea.Cmd { m.stopDump(); return nil }})
	}
	return items
}

// openDumpForm opens the form for conn, suggesting a file in Downloads (or
// in the directory the last dump went to) named for the connection.
func (m *Model) openDumpForm(conn string) {
	df := &dumpForm{conn: conn, format: pgdump.Plain, checks: map[dfCtl]bool{}, fields: map[dfCtl]*editor{},
		hovered: dfNone, rects: map[dfCtl]Rect{}}
	for _, c := range []dfCtl{dfOut, dfJobs, dfSchemas, dfTables, dfExclTables, dfExclData, dfExtra} {
		df.fields[c] = newEditor(true)
	}
	df.fields[dfJobs].placeholder = "1 — tables dumped at once"
	df.fields[dfSchemas].placeholder = "all — or patterns: public, sales*"
	df.fields[dfTables].placeholder = "all — or patterns: orders, sales.*"
	df.fields[dfExclTables].placeholder = "none"
	df.fields[dfExclData].placeholder = "none — tables kept without their rows"
	df.fields[dfExtra].placeholder = "other pg_dump options, e.g. --no-comments"
	out := pgdump.DefaultOut(conn, df.format, time.Now())
	if m.dumpLastDir != "" {
		out = m.dumpLastDir + out[strings.LastIndexAny(out, `/\`):]
	}
	df.fields[dfOut].SetText(out)
	df.fields[dfOut].move(df.fields[dfOut].posAt(len(out)), false)
	df.focus = dfOut
	m.openModal(df)
}

// archive reports whether the format is one pg_restore reads, which takes
// the owner, create and clean choices itself: the form hides those.
func (df *dumpForm) archive() bool { return df.format.Archive() }

// rows is the form's controls top to bottom; the checkboxes share rows
// (see draw), and the buttons are on the last line.
func (df *dumpForm) order() []dfCtl {
	out := []dfCtl{dfFormat, dfOut}
	if df.format.ToDir() {
		out = append(out, dfJobs)
	}
	out = append(out, dfContent, dfSchemas, dfTables, dfExclTables, dfExclData)
	if !df.archive() {
		out = append(out, dfNoOwner)
	}
	out = append(out, dfNoPrivileges, dfInserts)
	if !df.archive() {
		out = append(out, dfCreate, dfClean)
	}
	return append(out, dfExtra, dfDump, dfCancel)
}

// step moves the focus d controls along, wrapping.
func (df *dumpForm) step(d int) {
	ord := df.order()
	i := slices.Index(ord, df.focus)
	df.focus = ord[((i+d)%len(ord)+len(ord))%len(ord)]
}

// setFormat picks a format, moving a suggested file name to the new
// format's suffix (pgdump.SwapSuffix: a name typed by hand stays).
func (df *dumpForm) setFormat(f pgdump.Format) {
	out := df.fields[dfOut]
	out.SetText(pgdump.SwapSuffix(out.Text(), df.format, f))
	out.move(out.posAt(len(out.Text())), false)
	df.format = f
	if !slices.Contains(df.order(), df.focus) {
		df.focus = dfFormat
	}
}

// form is what the form says, as pgdump takes it.
func (df *dumpForm) form() pgdump.Form {
	t := func(c dfCtl) string { return df.fields[c].Text() }
	f := pgdump.Form{Format: string(df.format), Out: t(dfOut), Content: df.content,
		Schemas: t(dfSchemas), Tables: t(dfTables), ExcludeTables: t(dfExclTables), ExcludeData: t(dfExclData),
		NoOwner: df.checks[dfNoOwner], NoPrivileges: df.checks[dfNoPrivileges], Inserts: df.checks[dfInserts],
		Create: df.checks[dfCreate], Clean: df.checks[dfClean], Extra: t(dfExtra)}
	if df.format.ToDir() {
		f.Jobs = t(dfJobs)
	}
	return f
}

func (df *dumpForm) say(kind logKind, text string) { df.msg, df.msgKind = text, kind }

func (df *dumpForm) title() string { return "Dump " + df.conn }

func (df *dumpForm) size(w, h int) (int, int) {
	// format + its description, the destination, What, four filters and
	// More options; then Jobs and the checkbox lines when shown; then a
	// gap, three message lines and the buttons; and the frame
	rows := 9 + len(df.checkRows()) + 5 + 2
	if df.format.ToDir() {
		rows++
	}
	return min(max(w*3/4, 72), 96), rows
}

// checkRows are the checkbox lines, and the boxes on each.
func (df *dumpForm) checkRows() [][]dfCtl {
	var rows [][]dfCtl
	if df.archive() {
		rows = append(rows, []dfCtl{dfNoPrivileges, dfInserts})
	} else {
		rows = append(rows, []dfCtl{dfNoOwner, dfNoPrivileges, dfInserts}, []dfCtl{dfCreate, dfClean})
	}
	return rows
}

var dfCheckLabels = map[dfCtl]string{
	dfNoOwner:      "no owners",
	dfNoPrivileges: "no grants",
	dfInserts:      "INSERTs, not COPY",
	dfCreate:       "CREATE DATABASE first",
	dfClean:        "DROP before creating",
}

func (df *dumpForm) draw(m *Model, s Surface) *caret {
	bg := m.st.panel
	label := onBg(m.st.muted, bg)
	var car *caret
	clear(df.rects)
	fw := s.W() - dfLabelW - 1

	field := func(c dfCtl, y int, name string) {
		s.Put(1, y, name, label)
		sub := s.Sub(Rect{dfLabelW, y, fw, 1})
		df.rects[c] = sub.Rect()
		focused := df.focus == c
		cx, cy, ok := df.fields[c].Draw(sub, m.st, m.st.raised, [2]int{}, focused)
		if ok && focused {
			car = &caret{cx, cy}
		}
	}
	chips := func(y int, labels []string, cur int, on bool) []Rect {
		var out []Rect
		x := dfLabelW
		for i, l := range labels {
			st := m.st.button
			if i == cur {
				st = pick(on, m.st.sel, m.st.buttonHover)
			}
			r := chip(s, x, y, " "+l+" ", st)
			out = append(out, r)
			x += r.W + 1
		}
		return out
	}

	y := 0
	s.Put(1, y, "Format", label)
	var names []string
	cur := 0
	for i, f := range dfFormats {
		names = append(names, string(f.f))
		if f.f == df.format {
			cur = i
		}
	}
	df.formatR = chips(y, names, cur, df.focus == dfFormat)
	y++
	s.Put(dfLabelW, y, truncate(dfFormats[cur].what, fw), label.Italic())
	y++
	field(dfOut, y, pick(df.format.ToDir(), "To directory", "To file"))
	y++
	if df.format.ToDir() {
		field(dfJobs, y, "Jobs")
		y++
	}
	s.Put(1, y, "What", label)
	var cl []string
	cc := 0
	for i, c := range dfContents {
		cl = append(cl, c[1])
		if c[0] == df.content {
			cc = i
		}
	}
	df.contentR = chips(y, cl, cc, df.focus == dfContent)
	y++
	field(dfSchemas, y, "Schemas")
	y++
	field(dfTables, y, "Tables")
	y++
	field(dfExclTables, y, "Skip tables")
	y++
	field(dfExclData, y, "Skip data of")
	y++
	for i, row := range df.checkRows() {
		if i == 0 {
			s.Put(1, y, "Options", label)
		}
		x := dfLabelW
		for _, c := range row {
			box := pick(df.checks[c], "[✓] ", "[ ] ")
			st := pick(df.focus == c, m.st.sel, onBg(m.st.base, bg))
			r := chip(s, x, y, box+dfCheckLabels[c], st)
			df.rects[c] = r
			x += r.W + 3
		}
		y++
	}
	field(dfExtra, y, "More options")

	if df.msg != "" {
		st := m.st.muted
		switch df.msgKind {
		case logOk:
			st = m.st.ok
		case logWarn:
			st = m.st.warn
		case logErr:
			st = m.st.err
		}
		var lines []string
		for _, l := range strings.Split(df.msg, "\n") {
			lines = append(lines, wrap(l, s.W()-2)...)
		}
		top := s.H() - 4
		for i := 0; i < 3 && i < len(lines); i++ {
			s.Put(1, top+i, lines[i], onBg(st, bg))
		}
	}

	by := s.H() - 1
	btn := func(c dfCtl, x int, text string, rest Style) Rect {
		st := rest
		switch {
		case df.focus == c:
			st = m.st.sel
		case df.hovered == c:
			st = m.st.buttonHover
		}
		r := chip(s, x, by, text, st)
		df.rects[c] = r
		return r
	}
	r := btn(dfDump, 1, pick(df.busy, " checking… ", " ⇩ Dump "), m.st.buttonHot)
	r = btn(dfCancel, r.X-s.Rect().X+r.W+2, " Cancel ", m.st.button)
	if hint := "Enter dumps · Space ticks · Esc cancels"; r.X-s.Rect().X+r.W+2+width(hint) < s.W() {
		s.PutRight(s.W()-1, by, hint, label)
	}
	return car
}

func (df *dumpForm) key(m *Model, k tea.KeyPressMsg) (tea.Cmd, bool) {
	s := k.String()
	switch s {
	case "esc":
		return nil, true
	case "tab", "down":
		df.step(1)
		return nil, false
	case "shift+tab", "up":
		df.step(-1)
		return nil, false
	case "enter":
		if df.focus == dfCancel {
			return nil, true
		}
		return df.submit(m), false
	}
	switch df.focus {
	case dfFormat:
		if s == "left" || s == "right" || s == "space" {
			i := slices.IndexFunc(dfFormats, func(f dumpFormatInfo) bool { return f.f == df.format })
			df.setFormat(dfFormats[(i+pick(s == "left", len(dfFormats)-1, 1))%len(dfFormats)].f)
		}
	case dfContent:
		if s == "left" || s == "right" || s == "space" {
			i := slices.IndexFunc(dfContents, func(c [2]string) bool { return c[0] == df.content })
			df.content = dfContents[(i+pick(s == "left", len(dfContents)-1, 1))%len(dfContents)][0]
		}
	case dfNoOwner, dfNoPrivileges, dfInserts, dfCreate, dfClean:
		if s == "space" {
			df.checks[df.focus] = !df.checks[df.focus]
		}
	case dfDump, dfCancel:
		if s == "space" {
			return df.key(m, tea.KeyPressMsg{Code: tea.KeyEnter})
		}
	default:
		if e := df.fields[df.focus]; e != nil {
			e.HandleKey(k)
		}
	}
	return nil, false
}

func (df *dumpForm) click(m *Model, x, y, clicks int, shift bool) (tea.Cmd, bool) {
	if m.modalClose().Contains(x, y) {
		return nil, true
	}
	for i, r := range df.formatR {
		if r.Contains(x, y) {
			df.setFormat(dfFormats[i].f)
			df.focus = dfFormat
			return nil, false
		}
	}
	for i, r := range df.contentR {
		if r.Contains(x, y) {
			df.focus, df.content = dfContent, dfContents[i][0]
			return nil, false
		}
	}
	for c, r := range df.rects {
		if !r.Contains(x, y) {
			continue
		}
		switch c {
		case dfDump:
			return df.submit(m), false
		case dfCancel:
			return nil, true
		case dfNoOwner, dfNoPrivileges, dfInserts, dfCreate, dfClean:
			df.focus = c
			df.checks[c] = !df.checks[c]
		default:
			df.focus = c
			df.fields[c].Click(x, y, clicks, shift)
		}
		return nil, false
	}
	return nil, false
}

func (df *dumpForm) drag(m *Model, x, y int) {
	if e := df.fields[df.focus]; e != nil {
		e.Drag(x, y)
	}
}

func (df *dumpForm) hover(m *Model, x, y int) {
	df.hovered = dfNone
	for _, c := range []dfCtl{dfDump, dfCancel} {
		if df.rects[c].Contains(x, y) {
			df.hovered = c
		}
	}
}

func (df *dumpForm) paste(m *Model, s string) {
	if e := df.fields[df.focus]; e != nil {
		e.Insert(s)
	}
}

// submit checks the form and, if it reads, asks pgdump.Prepare off the UI
// goroutine. A refusal stays in the form, for the user to fix.
func (df *dumpForm) submit(m *Model) tea.Cmd {
	if m.dumpBusy != "" {
		df.say(logWarn, "a dump of "+m.dumpBusy+" is still running — one at a time")
		return nil
	}
	opts, err := df.form().Options()
	if err != nil {
		df.say(logErr, serr.UserMsgFromErr(err, err.Error()))
		return nil
	}
	m.dumpSeq++
	df.seq, df.busy = m.dumpSeq, true
	df.say(logInfo, "checking the server's version and finding pg_dump…")
	seq, cfg, mgr, conn := df.seq, m.cfg, m.mgr, df.conn
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		run, err := pgdump.Prepare(ctx, cfg, mgr, conn, opts)
		return dumpPreparedMsg{seq: seq, run: run, err: err}
	}
}

// dumpPrepared starts the dump Prepare readied, or says in the form why it
// cannot. An answer for a form since closed, or pressed again, is dropped.
func (m *Model) dumpPrepared(msg dumpPreparedMsg) tea.Cmd {
	df, ok := m.modal.(*dumpForm)
	if !ok || msg.seq != df.seq {
		return nil
	}
	df.busy = false
	if msg.err != nil {
		df.say(logErr, serr.UserMsgFromErr(msg.err, msg.err.Error()))
		return nil
	}
	if m.dumpBusy != "" { // one started from elsewhere meanwhile
		df.say(logWarn, "a dump of "+m.dumpBusy+" is still running — one at a time")
		return nil
	}
	m.modal = nil
	run := msg.run
	if run.Opts.Out != "" {
		m.dumpLastDir = run.Opts.Out[:strings.LastIndexAny(run.Opts.Out, `/\`)]
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	m.dumpBusy, m.dumpCancel, m.dumpWait = run.Name, cancel, done
	send := m.send
	return func() tea.Msg {
		defer close(done)
		_ = run.Report(ctx, func(level, text string) {
			if send != nil {
				send(dumpLineMsg{level: level, text: text})
			}
		})
		cancel()
		return dumpDoneMsg{conn: run.Name}
	}
}

// dumpLine logs a line of the running dump on the log on screen: the user
// started it from here, and is looking at it.
func (m *Model) dumpLine(msg dumpLineMsg) {
	kind := logInfo
	switch msg.level {
	case "ok":
		kind = logOk
	case "warn":
		kind = logWarn
	case "err":
		kind = logErr
	}
	m.log(kind, msg.text)
}

// dumpDone clears the running dump; Report has logged its outcome.
func (m *Model) dumpDone(dumpDoneMsg) {
	m.dumpBusy, m.dumpCancel = "", nil
}

// stopDumpForQuit stops a dump still running as the TUI quits: pg_dump is
// a child process, and would otherwise go on writing after dbc has gone.
// It waits (a while) for the dump to end, so its partial output is
// removed rather than left looking like a dump; stderr says so, as the
// log is no longer on screen.
func (m *Model) stopDumpForQuit() {
	if m.dumpCancel == nil {
		return
	}
	m.dumpCancel()
	select {
	case <-m.dumpWait:
	case <-time.After(15 * time.Second):
	}
	fmt.Fprintf(os.Stderr, "the dump of %s was still running, and was stopped — nothing of it was kept\n", m.dumpBusy)
}

// stopDump interrupts the running dump; its end comes as dumpDoneMsg.
func (m *Model) stopDump() {
	if m.dumpCancel != nil {
		m.logf(logInfo, "stopping the dump of %s…", m.dumpBusy)
		m.dumpCancel()
	}
}
