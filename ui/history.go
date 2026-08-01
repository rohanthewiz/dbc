package ui

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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

// histEntry is one statement the user ran — one line of the history file.
type histEntry struct {
	At   time.Time `json:"at"`
	Conn string    `json:"conn"`
	SQL  string    `json:"sql"`
}

// history is the statements the user has run, oldest first, backed by a
// JSON-lines file: one object per line carries the multi-line SQL that a
// plain-text history would have to escape, and appending one entry costs one
// write rather than a rewrite of the file.
type history struct {
	path    string
	entries []histEntry
}

// historyFile is where the history persists, beside the editor buffer. It
// returns "" when no home directory is known, and history lives only for the
// session.
func historyFile() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "dbc", "history.jsonl")
}

// loadHistory reads the history file, skipping lines it cannot parse — a
// half-written last line from a killed process must not cost the whole
// history. An over-long file is trimmed back to histMax here, which is the
// only point at which the file is rewritten rather than appended to.
func loadHistory(path string) *history {
	h := &history{path: path}
	if path == "" {
		return h
	}
	f, err := os.Open(path)
	if err != nil {
		return h
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), histMaxLine)
	for sc.Scan() {
		var e histEntry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			continue
		}
		if strings.TrimSpace(e.SQL) == "" {
			continue
		}
		h.entries = append(h.entries, e)
	}
	if len(h.entries) > histMax {
		h.entries = h.entries[len(h.entries)-histMax:]
		h.rewrite()
	}
	return h
}

// add records a statement, unless it repeats the one just recorded — running
// the same query twice while watching a table change should not bury the rest
// of the history. The entry is kept in memory whether or not the file write
// works, so a read-only config directory costs persistence, not the session.
func (h *history) add(conn, sql string, at time.Time) error {
	sql = strings.TrimSpace(sql)
	if sql == "" {
		return nil
	}
	if n := len(h.entries); n > 0 && h.entries[n-1].SQL == sql {
		return nil
	}
	e := histEntry{At: at, Conn: conn, SQL: sql}
	h.entries = append(h.entries, e)
	return h.appendLine(e)
}

// appendLine writes one entry to the file, creating it user-only: a query
// history accumulates real values as surely as the scratchpad does.
func (h *history) appendLine(e histEntry) error {
	if h.path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(h.path), 0o755); err != nil {
		return serr.Wrap(err, "path", h.path)
	}
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

// rewrite replaces the file with what is in memory — the trim at load time.
func (h *history) rewrite() {
	if h.path == "" {
		return
	}
	var sb strings.Builder
	for _, e := range h.entries {
		bs, err := json.Marshal(e)
		if err != nil {
			continue
		}
		sb.Write(bs)
		sb.WriteByte('\n')
	}
	_ = os.WriteFile(h.path, []byte(sb.String()), 0o600)
}

// recent returns the entries newest first — the order they are picked in.
func (h *history) recent() []histEntry {
	out := make([]histEntry, len(h.entries))
	for i, e := range h.entries {
		out[len(h.entries)-1-i] = e
	}
	return out
}

// matchHistory keeps the entries whose SQL or connection name holds q,
// case-insensitively. An empty query keeps everything.
func matchHistory(entries []histEntry, q string) []histEntry {
	q = strings.ToLower(strings.TrimSpace(q))
	if q == "" {
		return entries
	}
	out := make([]histEntry, 0, len(entries))
	for _, e := range entries {
		if strings.Contains(strings.ToLower(e.SQL), q) ||
			strings.Contains(strings.ToLower(e.Conn), q) {
			out = append(out, e)
		}
	}
	return out
}
