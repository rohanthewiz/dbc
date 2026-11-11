// Package userdata is the state dbc keeps for the user between sessions:
// the query history, the editor's per-database SQL consoles (and the single
// editor buffer they replaced), the assistant's conversations and the
// terminal UI's schema picks, all under ~/.config/dbc.
//
// It started life inside the former tview UI (package ui, since removed),
// and kept that UI's on-disk format — same paths, same JSON-lines shape — so
// history and the scratchpad written by older dbc builds still load.
package userdata

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/rohanthewiz/serr"
)

const (
	// histMax bounds the history file. It is read whole at startup and shown
	// in one list, so keeping everything ever run is neither useful nor free;
	// the oldest entries fall off the front.
	histMax = 500

	// histMaxLine caps how much of one entry we will read back. A generated
	// INSERT can be enormous, and a scanner that gives up on a long line would
	// silently drop everything after it.
	histMaxLine = 1 << 20
)

// Entry is one statement the user ran — one line of the history file.
type Entry struct {
	At   time.Time `json:"at"`
	Conn string    `json:"conn"`
	SQL  string    `json:"sql"`
	// DB is the database the statement ran on, as ConsoleDB.Key — the same
	// host ─► database identity the consoles are kept by, so "this
	// database's history" and "this database's consoles" agree, and two
	// connection names onto one database share both. It is a key rather
	// than the connection's name because a name is only a label (renaming
	// "prod" must not orphan its history) and one server holds many
	// databases.
	//
	// omitempty leaves entries written before the field existed as they
	// were, and an older dbc reading a new line ignores the unknown field.
	// "" means "database unknown": such an entry shows only under "all
	// databases", never under a database it might not belong to.
	DB string `json:"db,omitempty"`
}

// History is the statements the user has run, oldest first, backed by a
// JSON-lines file: one object per line carries the multi-line SQL that a
// plain-text history would have to escape, and appending one entry costs one
// write rather than a rewrite of the file.
//
// It is safe for concurrent use. The TUI touches it from one goroutine, but
// `dbc web` gives every browser tab its own workspace over ONE shared
// History (so a query run in any tab, or in the terminal's history file,
// is recallable from all of them), and those tabs record from their own
// request goroutines. mu also serializes the file appends, so two tabs
// recording at once cannot interleave half-lines.
type History struct {
	mu      sync.Mutex
	path    string
	entries []Entry
}

// HistoryFile is where the history persists, beside the editor buffer. It
// returns "" when no home directory is known, and history lives only for the
// session.
func HistoryFile() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "dbc", "history.jsonl")
}

// LoadHistory reads the history file, skipping lines it cannot parse — a
// half-written last line from a killed process must not cost the whole
// history. An over-long file is trimmed back to histMax here, which is the
// only point at which the file is rewritten rather than appended to.
func LoadHistory(path string) *History {
	h := &History{path: path}
	if path == "" {
		return h
	}
	h.entries = readHistory(path)
	if len(h.entries) > histMax {
		h.trim()
	}
	return h
}

// readHistory reads the entries of the history file at path, skipping lines
// that do not parse or hold no SQL.
func readHistory(path string) []Entry {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	var out []Entry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), histMaxLine)
	for sc.Scan() {
		var e Entry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			continue
		}
		if strings.TrimSpace(e.SQL) == "" {
			continue
		}
		out = append(out, e)
	}
	return out
}

// Add records a statement, unless it repeats the one just recorded — running
// the same query twice while watching a table change should not bury the rest
// of the history. The entry is kept in memory whether or not the file write
// works, so a read-only config directory costs persistence, not the session.
func (h *History) Add(conn, sql string, at time.Time) error {
	return h.AddEntry(Entry{At: at, Conn: conn, SQL: sql})
}

// AddEntry is Add with the whole entry given — the database (Entry.DB)
// included, which the workspace knows and Add's callers need not.
//
// The repeat check compares the database too: the same query run on one
// database and then on another is two entries, or the second database's
// scoped history would be missing a statement that ran on it.
func (h *History) AddEntry(e Entry) error {
	e.SQL = strings.TrimSpace(e.SQL)
	if e.SQL == "" {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if n := len(h.entries); n > 0 && h.entries[n-1].SQL == e.SQL && h.entries[n-1].DB == e.DB {
		return nil
	}
	h.entries = append(h.entries, e)
	return h.appendLine(e)
}

// appendLine writes one entry to the file, creating it user-only: a query
// history accumulates real values as surely as the scratchpad does.
//
// The append holds the file's lock (lockFile), which another process's
// trim holds too: without it, a dbc web appending while a TUI starts up and
// trims would write into the file the trim is about to rename away, and
// the entry would be gone.
func (h *History) appendLine(e Entry) error {
	if h.path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(h.path), 0o755); err != nil {
		return serr.Wrap(err, "path", h.path)
	}
	lock, err := lockFile(h.path)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }()
	f, err := os.OpenFile(h.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return serr.Wrap(err, "path", h.path)
	}
	defer f.Close()

	bs, err := json.Marshal(e)
	if err != nil {
		return serr.Wrap(err)
	}
	if _, err = f.Write(append(bs, '\n')); err != nil {
		return serr.Wrap(err, "path", h.path)
	}
	return nil
}

// trim cuts the file back to its newest histMax entries — at load time,
// the one point the file is rewritten rather than appended to — and the
// entries in memory with it.
//
// It holds the file's lock and reads the file again under it, so an entry
// another process appended since this one's load is kept rather than
// overwritten, and the rewrite is atomic (writeAtomic): it used to write
// the file in place, unlocked, so a crash mid-write truncated the history
// and a running dbc web's append could land in the old file and be lost
// (N-255). If the lock or the write fails, the file is left long; the
// next start trims it.
func (h *History) trim() {
	if h.path == "" {
		return
	}
	keep := func(es []Entry) []Entry {
		if len(es) > histMax {
			return es[len(es)-histMax:]
		}
		return es
	}
	h.entries = keep(h.entries)
	lock, err := lockFile(h.path)
	if err != nil {
		return
	}
	defer func() { _ = lock.Close() }()
	h.entries = keep(readHistory(h.path))
	var sb strings.Builder
	for _, e := range h.entries {
		bs, err := json.Marshal(e)
		if err != nil {
			continue
		}
		sb.Write(bs)
		sb.WriteByte('\n')
	}
	_ = writeAtomic(h.path, []byte(sb.String()), 0o600)
}

// Recent returns the entries newest first — the order they are picked in.
func (h *History) Recent() []Entry {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]Entry, len(h.entries))
	for i, e := range h.entries {
		out[len(h.entries)-1-i] = e
	}
	return out
}

// MatchHistory keeps the entries whose SQL or connection name holds q,
// case-insensitively. An empty query keeps everything.
func MatchHistory(entries []Entry, q string) []Entry {
	q = strings.ToLower(strings.TrimSpace(q))
	if q == "" {
		return entries
	}
	out := make([]Entry, 0, len(entries))
	for _, e := range entries {
		if strings.Contains(strings.ToLower(e.SQL), q) ||
			strings.Contains(strings.ToLower(e.Conn), q) {
			out = append(out, e)
		}
	}
	return out
}

// HistoryIn keeps the entries recorded on database key (Entry.DB). An empty
// key keeps everything: with no database to scope to (no connection,
// consoles off), "this database" can only mean all of them. Entries with no
// DB — written before the field existed — are in no database's scope; they
// show under "all".
func HistoryIn(entries []Entry, key string) []Entry {
	if key == "" {
		return entries
	}
	out := make([]Entry, 0, len(entries))
	for _, e := range entries {
		if e.DB == key {
			out = append(out, e)
		}
	}
	return out
}
