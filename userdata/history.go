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
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/rohanthewiz/serr"
)

const (
	// histMax bounds the history file. It is read whole at startup and shown
	// in one list, so keeping everything ever run is neither useful nor free;
	// the oldest entries fall off the front.
	histMax = 500

	// histMaxLine caps how much of one line the loader will read back; a
	// longer line is skipped and the reading goes on past it (readHistory).
	// It used to cap a bufio.Scanner whose error nobody checked, so one
	// line over it — a 2 MB generated INSERT run once — stopped the load
	// there, and every entry after it never loaded again (N-256).
	histMaxLine = 1 << 20

	// histMaxSQL caps the SQL an entry keeps (cutSQL), well under
	// histMaxLine even after JSON's escaping, so a new entry is never one
	// the loader skips. History is for finding and re-running a statement;
	// the head of a statement this long is what tells it apart.
	histMaxSQL = 256 << 10
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
	entries, skipped := readHistory(path)
	h.entries = entries
	// past the cap, or holding a line too long to load: rewrite the file
	// as what loaded, so the next start reads neither
	if len(h.entries) > histMax || skipped {
		h.trim()
	}
	return h
}

// readHistory reads the entries of the history file at path, skipping lines
// that do not parse, hold no SQL, or run past histMaxLine; skipped reports
// whether it skipped one for its length, which the file is then rewritten
// without (LoadHistory).
func readHistory(path string) (out []Entry, skipped bool) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer f.Close()

	r := bufio.NewReaderSize(f, 64<<10)
	for {
		line, long, err := nextLine(r, histMaxLine)
		skipped = skipped || long
		if !long && len(line) > 0 {
			var e Entry
			if json.Unmarshal(line, &e) == nil && strings.TrimSpace(e.SQL) != "" {
				out = append(out, e)
			}
		}
		if err != nil {
			return out, skipped // io.EOF, or a read error: what was read stands
		}
	}
}

// nextLine reads r's next line, without its newline. A line longer than
// max is read through to its end but not kept: long is true and line nil,
// so the caller goes on with the line after it. err is io.EOF after the
// last line (which may lack a newline, a killed process's half line).
func nextLine(r *bufio.Reader, max int) (line []byte, long bool, err error) {
	for {
		frag, ferr := r.ReadSlice('\n')
		if !long {
			if len(line)+len(frag) > max+1 { // +1: the newline
				long, line = true, nil
			} else {
				line = append(line, frag...)
			}
		}
		if ferr == bufio.ErrBufferFull {
			continue // the line goes on
		}
		return bytes.TrimSuffix(line, []byte("\n")), long, ferr
	}
}

// cutSQL keeps the head of a statement longer than histMaxSQL, cut on a
// rune boundary, with a comment saying what was dropped, so the entry
// reads as cut rather than as the statement it was.
func cutSQL(sql string) string {
	if len(sql) <= histMaxSQL {
		return sql
	}
	n := histMaxSQL
	for n > 0 && !utf8.RuneStart(sql[n]) {
		n--
	}
	return sql[:n] + fmt.Sprintf("\n-- … cut here: the statement was %d bytes, and history keeps the first %d", len(sql), n)
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
	e.SQL = cutSQL(strings.TrimSpace(e.SQL))
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
	again, _ := readHistory(h.path)
	h.entries = keep(again)
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
