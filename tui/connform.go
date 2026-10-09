package tui

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/rohanthewiz/serr"

	"github.com/rohanthewiz/dbc/config"
	"github.com/rohanthewiz/dbc/connedit"
	"github.com/rohanthewiz/dbc/db"
	"github.com/rohanthewiz/dbc/userdata"
)

// The connection form: add, test, edit and remove the connections kept in
// ~/.config/dbc/connections.toml — dbc web's "+ beside Connections" form,
// in the terminal. What is valid, how a DSN is built from fields, what an
// empty field keeps on an edit, and the config-then-file order of a save
// are package connedit's, shared with dbc web, so the two forms agree on
// every rule; this file is only the drawing and the keys.
//
//	Connections pane ── a ──────────────────► Add a connection
//	                 ── + ──────────────────► menu: Add a connection… ·
//	                                          Postgres in Docker… (pgdocker.go)
//	                 ── e, or right-click ──► Edit… (connections added here)
//	                 ── right-click ────────► Remove… ─► confirm menu
//
//	form ── Test (^T) ─► tea.Cmd: connedit.Test ─► connTestMsg ─► the form's
//	                     message line (a test answered after another
//	                     started, or after a save, is dropped by its seq)
//	     ── Save (Enter) ─► connedit.Add  ─► list refreshed, connect to it
//	                      ─► connedit.Edit ─► list refreshed; a rename
//	                                          moves the schema picks too
//
// "In use" is the TUI's one connection: the active one (or one of its other
// databases), or the one still dialing. An edit that would change what it
// dials, or a removal, is refused while it is — disconnecting first is one
// key (x) — as dbc web refuses while a query tab is on it. ai_rows alone
// may change at any time: the assistant reads it afresh for each question.
//
// A Save runs in Update rather than as a command: it is a few milliseconds
// of file write under a lock, and doing it inline lets the form stay open
// with the refusal on screen, or close and connect, in the same frame.

// cfCtl is one control of the form, in focus order. It doubles as the row
// kind when drawing: cfHost's row also holds cfPort, and the hint rows
// (after cfCancel) hold no control at all.
type cfCtl int

const (
	cfName cfCtl = iota
	cfDriver
	cfMode
	cfHost
	cfPort
	cfUser
	cfPassword
	cfDatabase
	cfFile
	cfOptions
	cfDSN
	cfTLS
	cfTLSCA
	cfTLSCert
	cfTLSKey
	cfTLSKeyPass
	cfAIRows
	cfTest
	cfSave
	cfCancel
	cfDSNHint // rows below: words, not controls
	cfTLSHint
	cfNone cfCtl = -1
)

// cfDrivers are the driver chips; each is also the name saved.
var cfDrivers = []string{"postgres", "mysql", "sqlite", "bytdb"}

// cfTLSModes are the TLS chips: "" leaves TLS to the DSN, as in dbc web.
var cfTLSModes = append([]string{""}, config.TLSModes...)

// cfDSNExamples is what the DSN field shows while empty, per driver — the
// same examples dbc web's form gives.
var cfDSNExamples = map[string]string{
	"postgres": "postgres://user:${PGPASS}@localhost:5432/mydb?sslmode=disable",
	"mysql":    "user:${MYSQL_PASS}@tcp(localhost:3306)/mydb?parseTime=true",
	"sqlite":   "file:scratch.db",
	"bytdb":    "notes.bytdb",
}

// cfHints are the fields' placeholders per driver: the port the server
// listens on, and an example of its options or file.
var cfHints = map[string]struct{ port, file, options string }{
	"postgres": {port: "5432", options: "application_name=dbc connect_timeout=10"},
	"mysql":    {port: "3306", options: "parseTime=true loc=Local"},
	"sqlite":   {file: "scratch.db", options: "mode=memory cache=shared"},
	"bytdb":    {file: "notes.bytdb"},
}

// cfLabelW is the width of the label column.
const cfLabelW = 14

// connFormModal is the add/edit form.
type connFormModal struct {
	modalBase

	from   string // the connection being edited; "" when adding
	driver string // as it will be saved: a chip's name, or an edited entry's own spelling
	asDSN  bool   // "Enter as" DSN text rather than fields
	tls    string // a cfTLSModes entry
	aiRows bool

	fields map[cfCtl]*editor // every text control, made once

	// Editing: hasPassword says the stored DSN has a password the form was
	// not given (it is never shown); an empty password field then keeps it.
	// initial is the field values as filled, and initDriver the driver: a
	// form whose fields are untouched sends an empty DSN — "keep the stored
	// one" — rather than fields rebuilt from the split, which could respell
	// a DSN the user never meant to change.
	hasPassword bool
	initial     map[cfCtl]string
	initDriver  string

	focus   cfCtl
	hovered cfCtl

	msg     string // the form's own line: a test's verdict, a refusal
	msgKind logKind
	seq     int // the latest test's number; older answers are dropped

	// hit targets, as last drawn
	rects   map[cfCtl]Rect // text fields, the checkbox, the buttons
	drvR    []Rect
	modeR   [2]Rect
	tlsR    []Rect
	content int // rows the last layout needed, for size
}

// connTestMsg is a test's answer, for the form that asked (seq).
type connTestMsg struct {
	seq int
	res connedit.TestResult
	err error
}

func newConnForm() *connFormModal {
	cf := &connFormModal{driver: "postgres", fields: map[cfCtl]*editor{}, hovered: cfNone, rects: map[cfCtl]Rect{}}
	for _, c := range []cfCtl{cfName, cfHost, cfPort, cfUser, cfPassword, cfDatabase, cfFile, cfOptions,
		cfDSN, cfTLSCA, cfTLSCert, cfTLSKey, cfTLSKeyPass} {
		cf.fields[c] = newEditor(true)
	}
	cf.fields[cfName].placeholder = "prod-reports"
	cf.fields[cfHost].placeholder = "localhost"
	cf.fields[cfPassword].mask = '•'
	cf.fields[cfTLSCA].placeholder = "~/certs/ca.pem — blank: the system's trusted CAs"
	cf.fields[cfTLSCert].placeholder = "only if the server asks for a client certificate"
	cf.fields[cfTLSKey].placeholder = "the client certificate's private key"
	cf.fields[cfTLSKeyPass].placeholder = "${VAR} holding an encrypted key's passphrase"
	cf.place()
	return cf
}

// connEditor is the shared add/edit/remove logic over the TUI's config,
// saved-connections file and pools.
func (m *Model) connEditor() *connedit.Editor {
	return &connedit.Editor{Cfg: m.cfg, Saved: m.saved, Mgr: m.mgr, App: "dbc"}
}

// openConnForm opens the form: empty to add (name ""), or filled from the
// saved entry of the connection named. The config file's connections are
// refused with the reason in the log, the way dbc web's menu greys them.
func (m *Model) openConnForm(name string) {
	cf := newConnForm()
	if name != "" {
		cn, ok := m.cfg.ConnByName(name)
		if !ok || cn.Base != "" {
			m.logf(logWarn, "no connection named %q to edit", name)
			return
		}
		if !cn.Web {
			m.log(logWarn, m.connEditor().FileRefusal(name, "change").Error())
			return
		}
		sc, found, err := m.saved.Get(name)
		if err != nil {
			m.logf(logErr, "could not read the saved connections: %s", serr.StringFromErr(err))
			return
		}
		if !found {
			m.logf(logWarn, "%q is no longer in %s — removed by another dbc, or by hand", name, m.saved.Path())
			return
		}
		cf.fill(sc)
	}
	m.openModal(cf)
}

// fill loads a saved entry into the form for an edit. The DSN never comes
// back as text — the DSN field starts empty, meaning "keep it" — but its
// parts do, all except a password that is not a ${VAR} reference: that is
// the secret itself, and an empty field keeps it. A DSN the fields cannot
// hold (several hosts, a unix socket) opens on the DSN text and says why.
func (cf *connFormModal) fill(sc config.SavedConn) {
	cf.from, cf.driver, cf.initDriver, cf.aiRows = sc.Name, sc.Driver, sc.Driver, sc.AIRows
	cf.set(cfName, sc.Name)
	// the TLS settings as saved — ${VAR}s and relative paths as typed — so
	// saving does not pin them to today's expansion
	cf.tls = sc.TLS
	cf.set(cfTLSCA, sc.TLSCA)
	cf.set(cfTLSCert, sc.TLSCert)
	cf.set(cfTLSKey, sc.TLSKey)
	cf.set(cfTLSKeyPass, sc.TLSKeyPassword)
	p, err := db.SplitDSN(sc.Driver, sc.DSN)
	if err != nil {
		cf.asDSN = true
		cf.say(logInfo, "This DSN is edited as text: "+serr.UserMsgFromErr(err, err.Error()))
	} else {
		cf.hasPassword = p.Password != "" && !db.IsEnvRef(p.Password)
		if cf.hasPassword {
			p.Password = ""
		}
		for c, v := range map[cfCtl]string{cfHost: p.Host, cfPort: p.Port, cfUser: p.User, cfPassword: p.Password,
			cfDatabase: p.Database, cfFile: p.File, cfOptions: p.Options} {
			cf.set(c, v)
		}
	}
	cf.initial = map[cfCtl]string{}
	for _, c := range []cfCtl{cfHost, cfPort, cfUser, cfPassword, cfDatabase, cfFile, cfOptions} {
		cf.initial[c] = cf.fields[c].Text()
	}
	cf.place()
}

// set fills field c with v, the caret at its end — where typing to amend a
// filled-in value (a name, a host) expects to start.
func (cf *connFormModal) set(c cfCtl, v string) {
	e := cf.fields[c]
	e.SetText(v)
	e.move(e.posAt(len(e.Text())), false)
}

// server reports whether the driver is a server engine (postgres, mysql):
// those have host fields and TLS; the embedded ones a file.
func (cf *connFormModal) server() bool {
	d, _ := db.Driver(cf.driver)
	return d == "pgx" || d == "mysql"
}

// canonical is the driver's chip name ("pg" → "postgres"), for the hints.
func (cf *connFormModal) canonical() string {
	switch d, _ := db.Driver(cf.driver); d {
	case "pgx":
		return "postgres"
	case "mysql", "sqlite":
		return d
	case "":
		return cf.driver
	}
	return "bytdb"
}

// tlsFiles reports whether the TLS file fields show: a mode that reads them.
func (cf *connFormModal) tlsFiles() bool {
	return cf.server() && cf.tls != "" && cf.tls != config.TLSDisable
}

// rows is the form's rows top to bottom, as row kinds (see cfCtl). Which
// show follows dbc web's form: fields or the DSN by "Enter as", host
// fields for a server and a file for an embedded engine, TLS for a server,
// its file fields once a mode reads them.
func (cf *connFormModal) rows() []cfCtl {
	out := []cfCtl{cfName, cfDriver, cfMode}
	switch {
	case cf.asDSN:
		out = append(out, cfDSN, cfDSNHint)
	case cf.server():
		out = append(out, cfHost, cfUser, cfPassword, cfDatabase)
	default:
		out = append(out, cfFile)
	}
	if !cf.asDSN && cf.canonical() != "bytdb" {
		out = append(out, cfOptions)
	}
	if cf.server() {
		out = append(out, cfTLS)
		if cf.tlsFiles() {
			out = append(out, cfTLSCA, cfTLSCert, cfTLSKey, cfTLSKeyPass, cfTLSHint)
		}
	}
	return append(out, cfAIRows)
}

// order is the focus order: the rows' controls, then the buttons.
func (cf *connFormModal) order() []cfCtl {
	var out []cfCtl
	for _, r := range cf.rows() {
		switch r {
		case cfDSNHint, cfTLSHint:
		case cfHost:
			out = append(out, cfHost, cfPort)
		default:
			out = append(out, r)
		}
	}
	return append(out, cfTest, cfSave, cfCancel)
}

// place moves the focus onto a control that shows, after a change hid the
// one it was on (a driver without TLS, say), and sets the placeholders that
// depend on the driver and on what an edit keeps.
func (cf *connFormModal) place() {
	ord := cf.order()
	if !slices.Contains(ord, cf.focus) {
		cf.focus = cfMode // the row whose change hid it, or near it
		if !slices.Contains(ord, cf.focus) {
			cf.focus = ord[0]
		}
	}
	h := cfHints[cf.canonical()]
	cf.fields[cfPort].placeholder = h.port
	cf.fields[cfFile].placeholder = h.file
	cf.fields[cfOptions].placeholder = h.options
	cf.fields[cfPassword].placeholder = "or ${VAR} from the environment"
	if cf.hasPassword {
		cf.fields[cfPassword].placeholder = "unchanged — type to replace it"
	}
	cf.fields[cfDSN].placeholder = cfDSNExamples[cf.canonical()]
	if cf.from != "" && cf.driver == cf.initDriver {
		cf.fields[cfDSN].placeholder = "unchanged — type a DSN to replace it"
	}
}

// step moves the focus d controls along, wrapping.
func (cf *connFormModal) step(d int) {
	ord := cf.order()
	i := slices.Index(ord, cf.focus)
	cf.focus = ord[((i+d)%len(ord)+len(ord))%len(ord)]
}

// dirty reports whether the field-mode values differ from what an edit
// filled in (always, when adding).
func (cf *connFormModal) dirty() bool {
	if cf.from == "" || cf.initial == nil || cf.driver != cf.initDriver {
		return true
	}
	for c, v := range cf.initial {
		if cf.fields[c].Text() != v {
			return true
		}
	}
	return false
}

// form is what the form says, as connedit takes it — dbc web's body():
// fields as parts (or nothing, on an untouched edit, to keep the DSN), the
// DSN text otherwise, and TLS only for a server engine.
func (cf *connFormModal) form() connedit.Form {
	f := connedit.Form{Name: cf.fields[cfName].Text(), Driver: cf.driver, AIRows: cf.aiRows}
	if cf.asDSN {
		f.DSN = cf.fields[cfDSN].Text()
	} else if cf.dirty() {
		t := func(c cfCtl) string { return cf.fields[c].Text() }
		p := db.DSNParts{Options: t(cfOptions)}
		if cf.server() {
			p.Host, p.Port, p.User, p.Password, p.Database = t(cfHost), t(cfPort), t(cfUser), t(cfPassword), t(cfDatabase)
		} else {
			p.File = t(cfFile)
		}
		f.Parts = &p
		f.KeepPassword = cf.hasPassword && p.Password == ""
	}
	if cf.server() && cf.tls != "" {
		f.TLS = cf.tls
		if cf.tls != config.TLSDisable {
			f.TLSCA, f.TLSCert, f.TLSKey = cf.fields[cfTLSCA].Text(), cf.fields[cfTLSCert].Text(), cf.fields[cfTLSKey].Text()
			f.TLSKeyPassword = cf.fields[cfTLSKeyPass].Text()
		}
	}
	return f
}

// say sets the form's message line.
func (cf *connFormModal) say(kind logKind, text string) { cf.msg, cf.msgKind = text, kind }

// ---------------------------------------------------------------------------
// The modal interface
// ---------------------------------------------------------------------------

func (cf *connFormModal) title() string {
	if cf.from != "" {
		return "Edit " + cf.from
	}
	return "Add a connection"
}

func (cf *connFormModal) size(w, h int) (int, int) {
	fw := min(max(w*3/4, 70), 90)
	rows := len(cf.rows()) + 6 // a gap, the message (up to 3 lines), the buttons
	if slices.Contains(cf.rows(), cfTLS) {
		// the TLS chips wrap onto more lines in a narrow form; the inner
		// width is the form's as modalRect will clamp it (to the screen,
		// less a cell each side), less the frame
		_, lines := chipRows(cfTLSLabels(), cfLabelW, min(fw, w-2)-2)
		rows += lines - 1
	}
	return fw, rows + 2
}

// cfTLSLabels are the TLS chips' labels, as drawn.
func cfTLSLabels() []string {
	out := make([]string, len(cfTLSModes))
	for i, n := range cfTLSModes {
		out[i] = " " + orDefault(n, "from DSN") + " "
	}
	return out
}

// chipRows lays out a row of chips labelled labels, starting at column x0
// of a surface inner cells wide, one cell apart. A chip that would end in
// the surface's last column or past it starts a new line, back at x0 — so
// a row too long for a narrow terminal (the TLS modes' six chips need 79
// cells, a form on an 80-column screen has 68) wraps rather than being
// cut off, and every chip stays whole and clickable. It returns each
// chip's column and line (0-based), and how many lines the row takes.
//
//	TLS           [ from DSN ] [ disable ] [ prefer ] [ require ] [ verify-ca ]
//	              [ verify-full ]
func chipRows(labels []string, x0, inner int) (pos [][2]int, lines int) {
	x, line := x0, 0
	for _, l := range labels {
		w := width(l)
		// never wrap the first chip of a line: one wider than the whole
		// row is cut, as before, rather than looping on empty lines
		if x > x0 && x+w > inner-1 {
			x, line = x0, line+1
		}
		pos = append(pos, [2]int{x, line})
		x += w + 1
	}
	return pos, line + 1
}

func (cf *connFormModal) draw(m *Model, s Surface) *caret {
	bg := m.st.panel
	label := onBg(m.st.muted, bg)
	var car *caret
	clear(cf.rects)

	field := func(c cfCtl, x, y, w int) {
		sub := s.Sub(Rect{x, y, w, 1})
		cf.rects[c] = sub.Rect()
		focused := cf.focus == c
		cx, cy, ok := cf.fields[c].Draw(sub, m.st, m.st.raised, [2]int{}, focused)
		if ok && focused {
			car = &caret{cx, cy}
		}
	}
	// chips draws a row of chips from row y, wrapped as chipRows lays
	// them out, and returns their rects and how many lines they took (the
	// caller moves y down by the extra ones)
	chips := func(y int, names []string, cur string, on bool, show func(string) string) ([]Rect, int) {
		labels := make([]string, len(names))
		for i, n := range names {
			labels[i] = " " + show(n) + " "
		}
		pos, lines := chipRows(labels, cfLabelW, s.W())
		var out []Rect
		for i, n := range names {
			st := m.st.button
			if n == cur {
				st = pick(on, m.st.sel, m.st.buttonHover)
			}
			out = append(out, chip(s, pos[i][0], y+pos[i][1], labels[i], st))
		}
		return out, lines
	}
	same := func(n string) string { return n }
	fw := s.W() - cfLabelW - 1 // a text field's width

	y := 0
	for _, r := range cf.rows() {
		switch r {
		case cfName:
			s.Put(1, y, "Name", label)
			field(cfName, cfLabelW, y, min(fw, 40))
		case cfDriver:
			s.Put(1, y, "Driver", label)
			cf.drvR, _ = chips(y, cfDrivers, cf.canonical(), cf.focus == cfDriver, same) // 35 cells: never wraps
		case cfMode:
			s.Put(1, y, "Enter as", label)
			cur := pick(cf.asDSN, "DSN", "Fields")
			rs, _ := chips(y, []string{"Fields", "DSN"}, cur, cf.focus == cfMode, same)
			cf.modeR = [2]Rect{rs[0], rs[1]}
		case cfHost:
			s.Put(1, y, "Host", label)
			hw := max(fw-16, 10)
			field(cfHost, cfLabelW, y, hw)
			s.Put(cfLabelW+hw+2, y, "Port", label)
			field(cfPort, cfLabelW+hw+7, y, max(fw-hw-7, 4))
		case cfUser:
			s.Put(1, y, "User", label)
			field(cfUser, cfLabelW, y, fw)
		case cfPassword:
			s.Put(1, y, "Password", label)
			field(cfPassword, cfLabelW, y, fw)
		case cfDatabase:
			s.Put(1, y, "Database", label)
			field(cfDatabase, cfLabelW, y, fw)
		case cfFile:
			s.Put(1, y, "File", label)
			field(cfFile, cfLabelW, y, fw)
		case cfOptions:
			s.Put(1, y, "Options", label)
			field(cfOptions, cfLabelW, y, fw)
		case cfDSN:
			s.Put(1, y, "DSN", label)
			field(cfDSN, cfLabelW, y, fw)
		case cfDSNHint:
			s.Put(cfLabelW, y, truncate("${VAR} is read from dbc's environment — keeps a password out of the file", fw), label.Italic())
		case cfTLS:
			s.Put(1, y, "TLS", label)
			var lines int
			cf.tlsR, lines = chips(y, cfTLSModes, cf.tls, cf.focus == cfTLS, func(n string) string {
				return orDefault(n, "from DSN")
			})
			y += lines - 1 // size counted the extra lines (chipRows)
		case cfTLSCA:
			s.Put(1, y, "CA file", label)
			field(cfTLSCA, cfLabelW, y, fw)
		case cfTLSCert:
			s.Put(1, y, "Client cert", label)
			field(cfTLSCert, cfLabelW, y, fw)
		case cfTLSKey:
			s.Put(1, y, "Client key", label)
			field(cfTLSKey, cfLabelW, y, fw)
		case cfTLSKeyPass:
			s.Put(1, y, "Key password", label)
			field(cfTLSKeyPass, cfLabelW, y, fw)
		case cfTLSHint:
			s.Put(cfLabelW, y, truncate("PEM files; ~ and ${VAR} work; relative to ~/.config/dbc", fw), label.Italic())
		case cfAIRows:
			box := pick(cf.aiRows, "[✓]", "[ ]")
			st := pick(cf.focus == cfAIRows, m.st.sel, onBg(m.st.base, bg))
			cf.rects[cfAIRows] = chip(s, cfLabelW, y, box+" Let the assistant see result rows (ai_rows)", st)
		}
		y++
	}
	cf.content = y

	// the message: a test's verdict or a refusal, wrapped onto up to three
	// lines above the buttons
	if cf.msg != "" {
		st := m.st.muted
		switch cf.msgKind {
		case logOk:
			st = m.st.ok
		case logWarn:
			st = m.st.warn
		case logErr:
			st = m.st.err
		}
		var lines []string
		for _, l := range strings.Split(cf.msg, "\n") {
			lines = append(lines, wrap(l, s.W()-2)...)
		}
		top := s.H() - 4
		for i := 0; i < 3 && i < len(lines); i++ {
			s.Put(1, top+i, lines[i], onBg(st, bg))
		}
	}

	by := s.H() - 1
	btn := func(c cfCtl, x int, text string, rest Style) Rect {
		st := rest
		switch {
		case cf.focus == c:
			st = m.st.sel
		case cf.hovered == c:
			st = m.st.buttonHover
		}
		r := chip(s, x, by, text, st)
		cf.rects[c] = r
		return r
	}
	r := btn(cfTest, 1, " ⏻ Test connection ", m.st.button)
	r = btn(cfSave, r.X-s.Rect().X+r.W+2, " ✓ Save ", m.st.buttonHot)
	r = btn(cfCancel, r.X-s.Rect().X+r.W+2, " Cancel ", m.st.button)
	if hint := "Enter saves · ^T tests · Esc cancels"; r.X-s.Rect().X+r.W+2+width(hint) < s.W() {
		s.PutRight(s.W()-1, by, hint, label)
	}
	return car
}

func (cf *connFormModal) key(m *Model, k tea.KeyPressMsg) (tea.Cmd, bool) {
	s := k.String()
	switch s {
	case "esc":
		return nil, true
	case "tab", "down":
		cf.step(1)
		return nil, false
	case "shift+tab", "up":
		cf.step(-1)
		return nil, false
	case "ctrl+t":
		return cf.test(m), false
	case "enter":
		switch cf.focus {
		case cfTest:
			return cf.test(m), false
		case cfCancel:
			return nil, true
		}
		return cf.save(m)
	}
	switch cf.focus {
	case cfDriver:
		switch s {
		case "left", "right", "space":
			i := slices.Index(cfDrivers, cf.canonical())
			cf.setDriver(cfDrivers[(i+pick(s == "left", len(cfDrivers)-1, 1))%len(cfDrivers)])
		}
	case cfMode:
		if s == "left" || s == "right" || s == "space" {
			cf.asDSN = !cf.asDSN
			cf.place()
		}
	case cfTLS:
		if s == "left" || s == "right" || s == "space" {
			i := slices.Index(cfTLSModes, cf.tls)
			cf.tls = cfTLSModes[(i+pick(s == "left", len(cfTLSModes)-1, 1))%len(cfTLSModes)]
			cf.place()
		}
	case cfAIRows:
		if s == "space" {
			cf.aiRows = !cf.aiRows
		}
	case cfTest, cfSave, cfCancel:
		if s == "space" {
			return cf.key(m, tea.KeyPressMsg{Code: tea.KeyEnter})
		}
	default:
		if e := cf.fields[cf.focus]; e != nil {
			e.HandleKey(k)
		}
	}
	return nil, false
}

// setDriver picks a driver chip. The fields stay as typed: a host typed for
// postgres is still the host when the user meant mysql.
func (cf *connFormModal) setDriver(d string) {
	cf.driver = d
	cf.place()
}

func (cf *connFormModal) click(m *Model, x, y, clicks int, shift bool) (tea.Cmd, bool) {
	if m.modalClose().Contains(x, y) {
		return nil, true
	}
	for i, r := range cf.drvR {
		if r.Contains(x, y) {
			cf.focus = cfDriver
			cf.setDriver(cfDrivers[i])
			return nil, false
		}
	}
	for i, r := range cf.modeR {
		if r.Contains(x, y) {
			cf.focus, cf.asDSN = cfMode, i == 1
			cf.place()
			return nil, false
		}
	}
	for i, r := range cf.tlsR {
		if r.Contains(x, y) && cf.server() {
			cf.focus, cf.tls = cfTLS, cfTLSModes[i]
			cf.place()
			return nil, false
		}
	}
	for c, r := range cf.rects {
		if !r.Contains(x, y) {
			continue
		}
		switch c {
		case cfTest:
			return cf.test(m), false
		case cfSave:
			return cf.save(m)
		case cfCancel:
			return nil, true
		case cfAIRows:
			cf.focus, cf.aiRows = cfAIRows, !cf.aiRows
		default:
			cf.focus = c
			cf.fields[c].Click(x, y, clicks, shift)
		}
		return nil, false
	}
	return nil, false
}

func (cf *connFormModal) drag(m *Model, x, y int) {
	if e := cf.fields[cf.focus]; e != nil {
		e.Drag(x, y)
	}
}

func (cf *connFormModal) hover(m *Model, x, y int) {
	cf.hovered = cfNone
	for _, c := range []cfCtl{cfTest, cfSave, cfCancel} {
		if cf.rects[c].Contains(x, y) {
			cf.hovered = c
		}
	}
}

func (cf *connFormModal) paste(m *Model, s string) {
	if e := cf.fields[cf.focus]; e != nil {
		e.Insert(s)
	}
}

// ---------------------------------------------------------------------------
// Test, save, remove
// ---------------------------------------------------------------------------

// test probes the form's connection off the UI goroutine: a dial can take
// connect_timeout. Each test is numbered, and only the latest one's answer
// is shown (connTestDone), so testing again after an edit never ends on
// the old DSN's verdict.
func (cf *connFormModal) test(m *Model) tea.Cmd {
	cf.seq = m.nextConnTestSeq()
	seq, f, from, ed := cf.seq, cf.form(), cf.from, m.connEditor()
	cf.say(logInfo, "connecting…")
	return func() tea.Msg {
		res, err := ed.Test(context.Background(), f, from)
		return connTestMsg{seq: seq, res: res, err: err}
	}
}

// nextConnTestSeq numbers a test (or a save) across every form this run,
// not per form: a form closed with a test still out, and a new form whose
// own first test got the same per-form number, would otherwise take the
// old form's verdict for its own.
func (m *Model) nextConnTestSeq() int {
	m.connTestSeq++
	return m.connTestSeq
}

// connTestDone shows a test's verdict in the form that asked, if it is still
// open and has not asked again (or saved) since.
func (m *Model) connTestDone(msg connTestMsg) tea.Cmd {
	cf, ok := m.modal.(*connFormModal)
	if !ok || msg.seq != cf.seq {
		return nil
	}
	warns := ""
	if len(msg.res.Warnings) > 0 {
		warns = "\n" + strings.Join(msg.res.Warnings, "\n")
	}
	switch r := msg.res; {
	case msg.err != nil:
		cf.say(logErr, connErrText(msg.err)) // the form itself cannot work
	case r.ProbeErr != nil:
		cf.say(logErr, "✗ "+r.ProbeMsg()+warns)
	case r.Note != "":
		cf.say(logWarn, "✓ "+r.Note+warns)
	default:
		cf.say(pick(warns != "", logWarn, logOk), fmt.Sprintf("✓ connected in %d ms", r.Took.Milliseconds())+warns)
	}
	return nil
}

// connErrText is the words for a connedit error: a refusal's own message,
// or a file failure with its context.
func connErrText(err error) string {
	var ce *connedit.Error
	if errors.As(err, &ce) {
		return ce.Msg
	}
	return serr.StringFromErr(err)
}

// save adds or edits the connection. A refusal stays in the form, for the
// user to fix; success closes it — and an added connection is connected to
// at once, as dbc web switches its tab to one.
func (cf *connFormModal) save(m *Model) (tea.Cmd, bool) {
	cf.seq = m.nextConnTestSeq() // a test still out must not overwrite what the save says
	f, ed := cf.form(), m.connEditor()
	to := strings.TrimSpace(f.Name)
	if cf.from == "" {
		warns, err := ed.Add(f)
		if err != nil {
			cf.say(logErr, connErrText(err))
			return nil, false
		}
		m.refreshConns()
		m.logf(logOk, "added connection %s — saved in %s", to, orDefault(m.saved.Path(), "memory"))
		for _, w := range warns {
			m.log(logWarn, w)
		}
		return m.setActive(to), true
	}
	res, err := ed.Edit(cf.from, f, m.connInUse(true))
	if err != nil {
		cf.say(logErr, connErrText(err))
		return nil, false
	}
	if res.Renamed {
		m.connRenamed(cf.from, to)
		m.logf(logOk, "renamed connection %s to %s", cf.from, to)
	} else {
		m.logf(logOk, "updated connection %s", cf.from)
	}
	m.refreshConns()
	for _, w := range res.Warnings {
		m.log(logWarn, w)
	}
	return nil, true
}

// connInUse is the TUI's in-use rule for connedit, dbc web's "a query tab
// is on it": a tab on the connection — or on one of its other databases,
// whose pool shares its host and credentials — or still dialing it.
// forEdit adds that ai_rows alone may still change.
func (m *Model) connInUse(forEdit bool) func(string) error {
	return func(name string) error {
		on := func(c string) bool { return c != "" && (c == name || m.baseOf(c) == name) }
		var user *queryTab
		for i, t := range m.tabs {
			target, ws := m.tabTarget(i)
			if on(target) {
				user = t
				break
			}
			if ws == nil || t.lazy != "" {
				continue
			}
			if connecting, dialing := ws.Connecting(); dialing && on(connecting) {
				user = t
				break
			}
		}
		if user == nil {
			return nil
		}
		msg := fmt.Sprintf("dbc is on %q — disconnect (x in Connections) or switch to another connection first", name)
		if len(m.tabs) > 1 {
			msg = fmt.Sprintf("tab %s is on %q — disconnect it (x in Connections) or switch it to another connection first", user.title, name)
		}
		if forEdit {
			msg += " (the assistant's row access alone can change while it is in use)"
		}
		return &connedit.Error{Kind: connedit.Conflict, Msg: msg}
	}
}

// connRenamed moves the schema picks of a renamed connection — its own and
// its other databases' ("<old>/analytics" → "<new>/analytics") — in memory
// and in the picks file, so the renamed connection reopens where it was;
// its log and the tabs' result sets on it follow the same way.
// A configured connection that merely has such a name is not derived from
// it and stays.
func (m *Model) connRenamed(from, to string) {
	rename := func(conn string) (string, bool) {
		if conn == from {
			return to, true
		}
		rest, ok := strings.CutPrefix(conn, from+config.DatabaseSep)
		if !ok || rest == "" {
			return "", false
		}
		// not derived from from when a configured connection has that very
		// name, or a longer configured name is its base — config.ConnByName
		// resolves a derived name by the longest base ("a/x/db" is "a/x"'s
		// database, not "a"'s)
		for _, c := range m.cfg.Conns() {
			if c.Name == conn || (len(c.Name) > len(from) && config.SupportsDatabases(c.Driver) &&
				strings.HasPrefix(conn, c.Name+config.DatabaseSep)) {
				return "", false
			}
		}
		return config.DerivedName(to, rest), true
	}
	// a restored tab not yet looked at keeps its connection by name
	// (tabs.go): it follows the rename, or its first look would fail
	for _, t := range m.tabs {
		if t.lazy == "" {
			continue
		}
		if nto, ok := rename(t.lazy); ok {
			t.lazy = nto
		}
	}
	for conn, p := range m.schemaPicks {
		if nto, ok := rename(conn); ok {
			delete(m.schemaPicks, conn)
			m.schemaPicks[nto] = p
		}
	}
	// its log, and every tab's results on it (logs.go), go along too
	m.renameConnViews(rename)
	// and a connection group follows its connection, or it would sit idle
	// on a name nothing is on any more (tabgroups.go)
	m.renameGroupConns(rename)
	if err := userdata.MovePicks(m.picksFile, rename); err != nil {
		m.logf(logWarn, "saved schema picks on %q were not moved to %q, and it will open on its default schema: %s",
			from, to, serr.StringFromErr(err))
	}
}

// removeConn asks, then forgets a connection added here (the Remove… row of
// the connections menu). The menu row has already said why when it cannot.
func (m *Model) removeConn(name string, x, y int) tea.Cmd {
	m.openMenu(x, y, []menuItem{
		heading("remove " + name + "? it is deleted from connections.toml"),
		{label: "Keep it", act: func(m *Model) tea.Cmd { return nil }},
		{label: "✕ Remove " + name, act: func(m *Model) tea.Cmd {
			removed, err := m.connEditor().Delete(name, m.connInUse(false))
			if removed {
				m.connRemovedFromTabs(name)
				m.dropConnViews(name) // its log and every tab's results on it (logs.go)
				m.refreshConns()
				m.logf(logOk, "removed connection %s", name)
			}
			if err != nil {
				m.log(logErr, connErrText(err))
			}
			return nil
		}},
	})
	return nil
}

// connRemovedFromTabs points a restored tab not yet looked at, saved on
// the removed connection (or one of its other databases), at the default
// connection instead — its first look would otherwise fail on a name the
// config no longer has.
func (m *Model) connRemovedFromTabs(name string) {
	for _, t := range m.tabs {
		if t.lazy == "" {
			continue
		}
		if _, ok := m.cfg.ConnByName(t.lazy); !ok {
			t.lazy = m.cfg.DefaultConnection
			if _, ok := m.cfg.ConnByName(t.lazy); !ok {
				t.lazy = ""
				if conns := m.cfg.Conns(); len(conns) > 0 {
					t.lazy = conns[0].Name
				}
			}
		}
	}
}

// connMenuItems are the connections menu's rows for the connection target
// (the row right-clicked; "" when none): edit and remove, live only for one
// added here, then add — by the form, or by starting Postgres in Docker
// (pgdocker.go), which adds the connection itself.
func (m *Model) connMenuItems(target string, x, y int) []menuItem {
	items := []menuItem{heading("connections")}
	if target != "" {
		editWhy, removeWhy := "", ""
		if cn, ok := m.cfg.ConnByName(target); ok && !cn.Web {
			editWhy = m.connEditor().FileRefusal(target, "change").Error()
			removeWhy = m.connEditor().FileRefusal(target, "remove").Error()
		} else if err := m.connInUse(false)(target); err != nil {
			removeWhy = err.Error()
		}
		items = append(items,
			menuItem{label: "Edit " + target + "…", key: "e", why: editWhy,
				act: func(m *Model) tea.Cmd { m.openConnForm(target); return nil }},
			menuItem{label: "Remove " + target + "…", why: removeWhy,
				act: func(m *Model) tea.Cmd { return m.removeConn(target, x, y) }})
		// a Postgres in Docker connection's container (pgdocker.go)
		if it, ok := m.pgDockerStopItem(target); ok {
			items = append(items, it)
		}
	}
	return append(items, m.connAddItems(x, y)...)
}

// connAddItems are the two ways to add a connection: the form, and
// Postgres in Docker, which adds its own. The connections menu ends with
// them, and + in the Connections pane opens them alone — dbc web's + has
// the same two rows. The form leads, so + then Enter is still the form.
func (m *Model) connAddItems(x, y int) []menuItem {
	return []menuItem{
		{label: "+ Add a connection…", key: "a", act: func(m *Model) tea.Cmd { m.openConnForm(""); return nil }},
		m.pgDockerItem(x, y),
	}
}

// connKey is the Connections pane's own letter keys: a add (the form), +
// the ways to add (the form, or Postgres in Docker), e edit the row under
// the cursor. It reports whether it used k.
func (m *Model) connKey(k tea.KeyPressMsg) bool {
	switch k.String() {
	case "a":
		m.openConnForm("")
	case "+":
		// under the pane's top row, as a dropdown from its title
		m.openMenu(m.conns.view.X+1, m.conns.view.Y, append([]menuItem{heading("add a connection")}, m.connAddItems(m.conns.view.X+1, m.conns.view.Y)...))
	case "e":
		if it, ok := m.conns.current(); ok {
			m.openConnForm(it.data.(string))
		}
	default:
		return false
	}
	return true
}
