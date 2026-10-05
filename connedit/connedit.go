// Package connedit adds, tests, edits and removes the connections a user
// creates from inside dbc — dbc web's connection form and the TUI's — as
// opposed to the config file's, which are the file's to change.
//
// It is the UI-free half of both forms. The web turns its answers into HTTP
// statuses (web/conns.go) and the TUI into lines in the form and the log
// (tui/connform.go); what counts as a valid form, how a DSN is built from
// fields, what is kept when an edit leaves a field empty, and the order in
// which the running config and the saved-connections file are changed live
// here once, so the two UIs cannot drift apart on any of it.
//
// It sits above both config and db (config must not import db: db opens
// what config describes), which is why it is a package of its own rather
// than part of either.
//
// Where a connection lives:
//
//	                 ┌── config file ([[connection]]) ──┐
//	startup:  cfg ◄──┤                                  ├── file's names win a clash
//	(any dbc)        └── connections.toml ──────────────┘
//	                        ▲
//	                        └── Editor.Add / Edit / Delete (dbc web, the TUI)
//
// Every change goes config first, file second:
//
//	Add ──► cfg.AddConn ──── name taken? ──► Conflict
//	         │
//	         └► saved.Add ── fails? ───────► cfg.RemoveConn (undo), error
//
//	Edit ─► web-added? ─ no ─► Invalid ("edit the file")
//	         │ FormDSN, Check, expand
//	         │ reconnects (name, driver, DSN or TLS changed)?
//	         │     └► inUse(name) refuses? ─► that refusal
//	         ├► cfg.ReplaceConn ─ clash / gone ─► Conflict / NotFound
//	         ├► saved.Update ── fails? ─► cfg.ReplaceConn back, error
//	         └► reconnects? mgr.Drop(old name)
//
//	Delete ► web-added? ─ no ─► Invalid
//	         inUse(name) refuses? ─► that refusal
//	         cfg.RemoveConn, mgr.Drop, saved.Delete (a failure here is
//	         reported, but the removal stands for this run)
//
// The config is changed first because it is where a duplicate name is caught
// atomically — two windows (or a window and a TUI) saving one name race
// there, under the config's lock, not in the file. A file that then fails
// to write is undone in the config, so the two never disagree about what
// exists for longer than one call.
//
// "In use" is the caller's to judge: dbc web counts the query tabs on a
// connection across every window, the TUI has one — its active connection.
// Either way a connection something is on cannot be renamed, given a new
// DSN, driver or TLS, or removed, since that would pull the pool out from
// under a session and whatever transaction it holds. Its ai_rows alone can
// change at any time: the assistant reads it afresh for every question.
package connedit

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/rohanthewiz/serr"

	"github.com/rohanthewiz/dbc/config"
	"github.com/rohanthewiz/dbc/db"
)

// MaxName bounds a connection name. It is shown in the sidebar, the topbar
// and every history row: a name that long is a paste gone wrong.
const MaxName = 64

// TestTimeoutCap bounds a test when connect_timeout is 0 ("no limit"): a
// real connect may then wait on the OS, but a button press should answer
// while the user is still looking at it.
const TestTimeoutCap = 30 * time.Second

// Kind says what sort of refusal an Error is, which each UI maps to its own
// words — dbc web to an HTTP status.
type Kind int

const (
	// Invalid is a form that could never work as given (400 in the web).
	Invalid Kind = iota + 1
	// NotFound is a connection that is not there (404).
	NotFound
	// Conflict is a change that raced another, or a name already taken (409).
	Conflict
)

// Error is a refusal the user can act on. Its message is written for the
// user, not the log. Any other error from this package is a failure to read
// or write the saved-connections file (500 in the web).
type Error struct {
	Kind Kind
	Msg  string
}

func (e *Error) Error() string { return e.Msg }

func refuse(k Kind, format string, args ...any) error {
	return &Error{Kind: k, Msg: fmt.Sprintf(format, args...)}
}

// userMsg is the words to show for err: serr's user message when it has
// one, else the error itself.
func userMsg(err error) string { return serr.UserMsgFromErr(err, err.Error()) }

// Form is the add- or edit-connection form, as either UI collects it.
type Form struct {
	Name   string `json:"name"`
	Driver string `json:"driver"`
	DSN    string `json:"dsn"`
	AIRows bool   `json:"ai_rows"`
	// TLS as typed: ${VAR}s unexpanded and a relative path relative, as the
	// saved file keeps them (config.SavedConn); TLSFor resolves them.
	config.TLSOpts

	// Parts, when set, is the DSN as fields; it replaces DSN (see FormDSN).
	Parts *db.DSNParts `json:"parts,omitempty"`
	// KeepPassword: editing in fields, the password field was left empty
	// over a stored password the form was not given — keep that one.
	KeepPassword bool `json:"keep_password,omitempty"`
}

// Editor changes the connections a user added, in the running config and
// in the saved-connections file together.
type Editor struct {
	Cfg   *config.Config
	Saved *config.SavedStore
	// Mgr's pool for a connection is dropped when the connection changes
	// what it dials or goes away, so the next connect opens a fresh one
	// from the new entry. Nil drops nothing (tests).
	Mgr *db.Manager
	// App names this program in messages: "dbc web", "dbc". A clash in the
	// file means another such program added the name since this one read
	// it ("added by another dbc web").
	App string
}

// FormDSN settles f.DSN, whichever way the form gave it: built from fields,
// or kept from the stored connection from ("" when adding).
func (e *Editor) FormDSN(f *Form, from string) error {
	if f.Parts != nil {
		return e.partsDSN(f, from)
	}
	if from == "" {
		return nil
	}
	return e.keepDSN(f, from)
}

// partsDSN writes f.Parts into f.DSN. With KeepPassword, the password is the
// one in from's stored DSN — taken apart with the driver it was stored
// under, so switching drivers (a postgres server moving to a mysql one, say)
// still keeps it. BuildDSN's refusals name the field to fix.
func (e *Editor) partsDSN(f *Form, from string) error {
	p := *f.Parts
	if f.KeepPassword && p.Password == "" && from != "" {
		sc, found, err := e.Saved.Get(from)
		if err != nil {
			return err
		}
		if !found {
			return refuse(NotFound, "no saved connection named %q", from)
		}
		old, err := db.SplitDSN(sc.Driver, sc.DSN)
		if err != nil {
			return refuse(Invalid, "%q's stored DSN cannot be taken apart to keep its password — type the password, or edit the DSN as text", from)
		}
		p.Password = old.Password
	}
	dsn, err := db.BuildDSN(strings.TrimSpace(f.Driver), p)
	if err != nil {
		return refuse(Invalid, "%s", userMsg(err))
	}
	f.DSN = dsn
	return nil
}

// keepDSN fills an empty DSN in f with the one stored for the saved
// connection from — the edit form's "leave the DSN unchanged", which exists
// because neither form is ever given the stored DSN to send back. It is the
// DSN as typed, ${VAR}s and all, so a kept DSN expands as it always did.
//
// A kept DSN goes only with its own driver: a postgres URL handed to the
// mysql driver cannot work, so changing the driver asks for a DSN to match.
// A DSN that is typed is left alone for Check to judge as any other.
func (e *Editor) keepDSN(f *Form, from string) error {
	if strings.TrimSpace(f.DSN) != "" {
		return nil
	}
	sc, found, err := e.Saved.Get(from)
	if err != nil {
		return err
	}
	if !found {
		return refuse(NotFound, "no saved connection named %q", from)
	}
	if drv := strings.TrimSpace(f.Driver); drv != sc.Driver {
		return refuse(Invalid, "%q's DSN is a %s one — type a %s DSN to change the driver", from, sc.Driver, drv)
	}
	f.DSN = sc.DSN
	return nil
}

// Check trims the form and turns away what could never work. needName: a
// test may be run before a name is typed; a save may not.
func (f *Form) Check(needName bool) error {
	f.Name, f.Driver, f.DSN = strings.TrimSpace(f.Name), strings.TrimSpace(f.Driver), strings.TrimSpace(f.DSN)
	if needName || f.Name != "" {
		switch {
		case f.Name == "":
			return refuse(Invalid, "name the connection")
		case utf8.RuneCountInString(f.Name) > MaxName:
			return refuse(Invalid, "a connection name is at most %d characters", MaxName)
		case strings.ContainsFunc(f.Name, unicode.IsControl):
			return refuse(Invalid, "a connection name cannot hold control characters")
		}
	}
	if _, err := db.Driver(f.Driver); err != nil {
		return refuse(Invalid, "pick a driver: postgres, mysql, sqlite or bytdb")
	}
	if f.DSN == "" {
		return refuse(Invalid, "the DSN is empty")
	}
	if err := f.TLSOpts.Check(); err != nil {
		return refuse(Invalid, "%s", userMsg(err))
	}
	// Both forms hide the TLS fields for the embedded engines, so this is a
	// stale form or a hand-made request; refused here rather than saved and
	// then failing every connect (db.tlsOpen refuses it too).
	if drv, _ := db.Driver(f.Driver); f.TLSOpts.Set() && drv != "pgx" && drv != "mysql" {
		return refuse(Invalid, "TLS settings are for postgres and mysql — %s opens a local file", f.Driver)
	}
	return nil
}

// TLSFor is f's TLS settings as a connect uses them: paths resolved the way
// config.MergeSaved resolves a saved entry's, so what is tested, what runs
// now and what the next start merges are the same files.
func (f *Form) TLSFor(name string) (config.TLSOpts, []string) {
	return config.ExpandTLS(name, f.TLSOpts, config.SavedDir())
}

// TestResult is the answer to "does this connect?". A connect that fails is
// not an error of the test — the test worked, and this is its answer — so it
// is ProbeErr here, not Test's error.
type TestResult struct {
	OK       bool
	Took     time.Duration // open + ping, when it connected
	Note     string        // a qualified yes ("the file does not exist yet …")
	ProbeErr error         // the driver's refusal; nil when it connected
	Warnings []string      // unset ${VAR}s and the like
}

// Test opens the form's DSN, pings it within connect_timeout (capped at
// TestTimeoutCap when that is "no limit") and closes it again: nothing is
// saved, and no pool is left behind for a connection that may never be
// added. from is the connection being edited ("" when adding), whose stored
// DSN an empty one means (see FormDSN). The error is the form's — one that
// could never work; a connect that fails is a TestResult with ProbeErr set.
func (e *Editor) Test(ctx context.Context, f Form, from string) (TestResult, error) {
	if err := e.FormDSN(&f, from); err != nil {
		return TestResult{}, err
	}
	if err := f.Check(false); err != nil {
		return TestResult{}, err
	}
	name := f.Name
	if name == "" {
		name = "(new connection)"
	}
	dsn, warns := config.ExpandDSN(name, f.Driver, f.DSN)
	tlsOpts, tw := f.TLSFor(name)
	warns = append(warns, tw...)
	timeout := e.Cfg.ConnectTimeout
	if timeout <= 0 {
		timeout = TestTimeoutCap
	}
	res, err := db.Probe(ctx, config.Connection{Name: name, Driver: f.Driver, DSN: dsn, TLSOpts: tlsOpts}, timeout)
	out := TestResult{OK: res.OK, Took: res.Took, Note: res.Note, ProbeErr: err, Warnings: warns}
	if err != nil {
		out.OK, out.Took, out.Note = false, 0, ""
	}
	return out, nil
}

// ProbeMsg is the words for a failed test's ProbeErr.
func (r TestResult) ProbeMsg() string {
	if r.ProbeErr == nil {
		return ""
	}
	return userMsg(r.ProbeErr)
}

// Add adds a connection to the running config and keeps it in the
// saved-connections file. It does not connect: each UI switches to it right
// after, where the connect's outcome shows the way every connect's does.
func (e *Editor) Add(f Form) (warns []string, err error) {
	if err = e.FormDSN(&f, ""); err != nil {
		return nil, err
	}
	if err = f.Check(true); err != nil {
		return nil, err
	}
	dsn, warns := config.ExpandDSN(f.Name, f.Driver, f.DSN)
	tlsOpts, tw := f.TLSFor(f.Name)
	warns = append(warns, tw...)
	cn := config.Connection{Name: f.Name, Driver: f.Driver, DSN: dsn, TLSOpts: tlsOpts, AIRows: f.AIRows, Web: true}
	if err = e.Cfg.AddConn(cn); err != nil {
		if errors.Is(err, config.ErrConnExists) {
			return nil, refuse(Conflict, "a connection named %q already exists", f.Name)
		}
		return nil, err
	}
	// the DSN as typed, ${VAR}s and all: see config.SavedConn
	if err = e.Saved.Add(config.SavedConn{Name: f.Name, Driver: f.Driver, DSN: f.DSN,
		TLSOpts: f.TLSOpts, AIRows: f.AIRows}); err != nil {
		e.Cfg.RemoveConn(f.Name)
		if errors.Is(err, config.ErrConnExists) {
			return nil, refuse(Conflict, "a connection named %q was added by another %s — "+
				"restart this one to see it", f.Name, e.App)
		}
		return nil, serr.Wrap(err, "op", "add conn")
	}
	if w := e.UnsavedWarning("connection"); w != "" {
		warns = append(warns, w)
	}
	return warns, nil
}

// EditResult says what an edit changed, for the caller's follow-ups: a
// rename moves what the UI keeps under the old name (saved tabs, schema
// picks, tab groups).
type EditResult struct {
	Renamed    bool
	Reconnects bool // name, driver, DSN or TLS changed: the old pool was dropped
	Warnings   []string
}

// Edit changes a connection added from inside dbc — its name, driver, DSN,
// TLS or ai_rows — in place. An empty DSN keeps the stored one (FormDSN).
//
// Anything but ai_rows changes what the connection connects to, or what it
// is called, and whatever is on it holds a session opened under the old
// name and DSN; inUse (nil: nothing ever is) is asked then, and its error
// refuses the edit. An ai_rows change alone goes through regardless.
func (e *Editor) Edit(name string, f Form, inUse func(name string) error) (EditResult, error) {
	cur, known := e.Cfg.ConnByName(name)
	if !known {
		return EditResult{}, refuse(NotFound, "no connection named %q", name)
	}
	if !cur.Web {
		return EditResult{}, e.FileRefusal(name, "change")
	}
	if err := e.FormDSN(&f, name); err != nil {
		return EditResult{}, err
	}
	if err := f.Check(true); err != nil {
		return EditResult{}, err
	}
	dsn, warns := config.ExpandDSN(f.Name, f.Driver, f.DSN)
	tlsOpts, tw := f.TLSFor(f.Name)
	warns = append(warns, tw...)
	next := config.Connection{Name: f.Name, Driver: f.Driver, DSN: dsn, TLSOpts: tlsOpts, AIRows: f.AIRows, Web: true}

	// Compared expanded, as the pool would see it: a DSN retyped to the same
	// text, or kept, is no change; a ${VAR} whose value moved since startup
	// is one, and the pool should pick it up. TLS settings are part of how
	// the pool dials, so a change to them reconnects too.
	res := EditResult{Renamed: f.Name != name}
	res.Reconnects = res.Renamed || f.Driver != cur.Driver || dsn != cur.DSN || tlsOpts != cur.TLSOpts
	if res.Reconnects && inUse != nil {
		if err := inUse(name); err != nil {
			return EditResult{}, err
		}
	}

	if err := e.Cfg.ReplaceConn(name, next); err != nil {
		switch {
		case errors.Is(err, config.ErrConnExists):
			return EditResult{}, refuse(Conflict, "a connection named %q already exists", f.Name)
		case errors.Is(err, config.ErrConnNotFound):
			return EditResult{}, refuse(NotFound, "no connection named %q", name)
		}
		return EditResult{}, err
	}
	// the DSN as typed (or as stored, when kept): see config.SavedConn
	if err := e.Saved.Update(name, config.SavedConn{Name: f.Name, Driver: f.Driver, DSN: f.DSN,
		TLSOpts: f.TLSOpts, AIRows: f.AIRows}); err != nil {
		// Put the config back as it was. That fails only if another window
		// added a connection under the old name in the moment since it was
		// given up; the edit then stands for this run and the file keeps
		// the old entry, which the next start skips as a clash and says so.
		_ = e.Cfg.ReplaceConn(f.Name, cur)
		switch {
		case errors.Is(err, config.ErrConnExists):
			return EditResult{}, refuse(Conflict, "a connection named %q was added by another %s — "+
				"restart this one to see it", f.Name, e.App)
		case errors.Is(err, config.ErrConnNotFound):
			return EditResult{}, refuse(NotFound, "%q is no longer in %s — removed by another %s, "+
				"or by hand", name, e.Saved.Path(), e.App)
		}
		return EditResult{}, serr.Wrap(err, "op", "edit conn")
	}
	if res.Reconnects && e.Mgr != nil {
		e.Mgr.Drop(name)
	}
	if w := e.UnsavedWarning("change"); w != "" {
		warns = append(warns, w)
	}
	res.Warnings = warns
	return res, nil
}

// Delete forgets a connection added from inside dbc. The config file's are
// refused, as is one inUse (nil: nothing ever is) says something is on.
//
// removed reports whether the running config lost it: a file that then
// fails to write is an error, but the connection stays gone for this run —
// putting it back would only hand the user a connection they just asked to
// be rid of — and comes back at the next start, which the error says.
//
// The in-use check and the removal are not one atomic step: something that
// picks the connection in between gets a connect that fails ("unknown
// connection"), or a pool the Drop then closes. Either shows as an error
// there and breaks nothing else.
func (e *Editor) Delete(name string, inUse func(name string) error) (removed bool, err error) {
	cn, known := e.Cfg.ConnByName(name)
	if !known {
		return false, refuse(NotFound, "no connection named %q", name)
	}
	if !cn.Web {
		return false, e.FileRefusal(name, "remove")
	}
	if inUse != nil {
		if err = inUse(name); err != nil {
			return false, err
		}
	}
	e.Cfg.RemoveConn(name)
	if e.Mgr != nil {
		e.Mgr.Drop(name)
	}
	if err = e.Saved.Delete(name); err != nil {
		return true, serr.Wrap(err, "op", "delete conn")
	}
	return true, nil
}

// FileRefusal is the answer to changing or removing a connection that is
// not dbc's to change: the config file's, or a built-in demo. verb is what
// was asked ("change", "remove").
func (e *Editor) FileRefusal(name, verb string) error {
	if e.Cfg.Demo {
		return refuse(Invalid, "%q is a built-in demo connection", name)
	}
	where := "the config file"
	if e.Cfg.Path != "" {
		where = e.Cfg.Path
	}
	return refuse(Invalid, "%q is defined in %s — edit the file to %s it", name, where, verb)
}

// UnsavedWarning is the note for a connection change that no file keeps —
// there is no home directory to keep one in: it holds only until this
// program stops. what is the thing that lasts that long.
func (e *Editor) UnsavedWarning(what string) string {
	if e.Saved.Persistent() {
		return ""
	}
	return "connections are not being saved (no home directory to keep them in), " +
		"so this " + what + " lasts only until " + e.App + " stops"
}
