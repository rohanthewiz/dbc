package web

import (
	"context"
	"database/sql"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/rohanthewiz/bytdb"
	bytdbdrv "github.com/rohanthewiz/bytdb/stdlib"
	"github.com/rohanthewiz/serr"
)

// Store keeps what only the browser UI has: its query tabs (the editor
// buffer and the connection each one was on) and its layout (pane sizes).
// History and assistant conversations are not here — they stay in userdata,
// shared with the TUI, so a query run in either shows up in both.
//
// It is a bytdb file, ~/.config/dbc/web.bytdb. Two processes writing one
// bytdb WAL would corrupt it, so only one may hold it. bytdb (v0.18.0+) sees
// to that itself: opening the file takes an exclusive lock on its sidecar,
// web.bytdb.lock, held until the last handle closes, and a second opener
// gets bytdb.ErrLocked. The store once took that same sidecar lock itself
// (bytdb v0.16.0 had none); it must not now — the lock is not reentrant, so
// the store's lock would make bytdb refuse the store's own open. A second
// `dbc web` finds the file held and, rather than refuse to start, gets a
// memory-only Store (see OpenStore) and says so. The same fallback covers a
// machine with no home directory.
//
// A memory-only Store has db == nil and keeps everything in the maps below,
// so callers never branch on which kind they hold.
type Store struct {
	db   *sql.DB // nil: memory only; holds bytdb's file lock while open
	path string

	mu     sync.Mutex
	tabs   map[string]Tab
	layout map[string]string
}

// Tab is one query tab as the browser left it.
type Tab struct {
	ID      string    `json:"id"`
	Title   string    `json:"title"`
	Conn    string    `json:"conn"`   // the connection it was on; "" before any
	Buffer  string    `json:"buffer"` // the editor's text
	Updated time.Time `json:"updated"`
}

// StateFile is where the store lives, beside the history and the TUI's
// editor buffer; "" when no home directory is known.
func StateFile() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "dbc", "web.bytdb")
}

// storeSchema is created on every open. IF NOT EXISTS keeps it idempotent;
// a later column is an ALTER here, run the same way.
var storeSchema = []string{
	`CREATE TABLE IF NOT EXISTS tabs (
		id      TEXT PRIMARY KEY,
		title   TEXT NOT NULL,
		conn    TEXT NOT NULL,
		buffer  TEXT NOT NULL,
		updated TIMESTAMPTZ NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS layout (
		key   TEXT PRIMARY KEY,
		value TEXT NOT NULL
	)`,
}

// OpenStore opens (creating it if needed) the store at path. path == ""
// returns a memory-only store and no error. On a failure — the file locked
// by another dbc web, an unwritable directory — it returns a memory-only
// store AND the error, so the caller can warn and carry on.
func OpenStore(path string) (*Store, error) {
	st := &Store{path: path, tabs: map[string]Tab{}, layout: map[string]string{}}
	if path == "" {
		return st, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return st, serr.Wrap(err, "path", path, "op", "mkdir")
	}
	// The bytdb driver opens the engine — and takes its file lock — in
	// sql.Open itself (its OpenConnector), not lazily on the first query, so
	// a held file shows up here.
	dbh, err := sql.Open(bytdbdrv.DriverName, path)
	if err != nil {
		if errors.Is(err, bytdb.ErrLocked) {
			return st, serr.Wrap(errLocked, "path", path)
		}
		return st, serr.Wrap(err, "path", path)
	}
	// one connection: the store's writes are a few small ones per edit
	// pause, and a single handle keeps them trivially ordered
	dbh.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, ddl := range storeSchema {
		if _, err = dbh.ExecContext(ctx, ddl); err != nil {
			_ = dbh.Close()
			return st, serr.Wrap(err, "path", path, "op", "schema")
		}
	}
	st.db = dbh
	return st, nil
}

// Persistent reports whether the store writes to disk.
func (s *Store) Persistent() bool { return s.db != nil }

// Path is the store's file; "" for a memory-only store.
func (s *Store) Path() string {
	if s.db == nil {
		return ""
	}
	return s.path
}

// errLocked is another process holding the store — bytdb.ErrLocked, in words
// that name the likely culprit for the warning webcmd prints.
var errLocked = errors.New("the store is in use by another dbc web")

// Close closes the file, and with it bytdb lets go of the lock. A memory-only
// store has nothing to close.
func (s *Store) Close() error {
	if s.db == nil {
		return nil
	}
	return s.db.Close()
}

// Tabs lists the saved tabs, oldest id first.
func (s *Store) Tabs() ([]Tab, error) {
	if s.db == nil {
		s.mu.Lock()
		defer s.mu.Unlock()
		out := make([]Tab, 0, len(s.tabs))
		for _, t := range s.tabs {
			out = append(out, t)
		}
		slices.SortFunc(out, func(a, b Tab) int { return strings.Compare(a.ID, b.ID) })
		return out, nil
	}
	ctx, cancel := opCtx()
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `SELECT id, title, conn, buffer, updated FROM tabs ORDER BY id`)
	if err != nil {
		return nil, serr.Wrap(err, "op", "list tabs")
	}
	defer rows.Close()
	var out []Tab
	for rows.Next() {
		var t Tab
		if err = rows.Scan(&t.ID, &t.Title, &t.Conn, &t.Buffer, &t.Updated); err != nil {
			return nil, serr.Wrap(err, "op", "scan tab")
		}
		out = append(out, t)
	}
	return out, wrap(rows.Err(), "op", "list tabs")
}

// SaveTab writes a tab, replacing any saved under the same id.
func (s *Store) SaveTab(t Tab) error {
	if t.Updated.IsZero() {
		t.Updated = time.Now().UTC()
	}
	if s.db == nil {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.tabs[t.ID] = t
		return nil
	}
	ctx, cancel := opCtx()
	defer cancel()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO tabs (id, title, conn, buffer, updated) VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (id) DO UPDATE SET
			title = EXCLUDED.title, conn = EXCLUDED.conn,
			buffer = EXCLUDED.buffer, updated = EXCLUDED.updated`,
		t.ID, t.Title, t.Conn, t.Buffer, t.Updated)
	return wrap(err, "op", "save tab", "tab", t.ID)
}

// DeleteTab forgets a saved tab. A missing one is success: the caller
// wanted it gone.
func (s *Store) DeleteTab(id string) error {
	if s.db == nil {
		s.mu.Lock()
		defer s.mu.Unlock()
		delete(s.tabs, id)
		return nil
	}
	ctx, cancel := opCtx()
	defer cancel()
	_, err := s.db.ExecContext(ctx, `DELETE FROM tabs WHERE id = $1`, id)
	return wrap(err, "op", "delete tab", "tab", id)
}

// Layout returns every saved layout value (pane sizes and the like), keyed
// by the name the page gave it. The values are opaque here: the page owns
// their meaning. (The one exception is above the store: a write of "tabs"
// or "plans", which list saved-tab keys, is merged with the keys other
// windows hold before it gets here — see mergeTabKeys in claims.go.)
func (s *Store) Layout() (map[string]string, error) {
	out := map[string]string{}
	if s.db == nil {
		s.mu.Lock()
		defer s.mu.Unlock()
		maps.Copy(out, s.layout)
		return out, nil
	}
	ctx, cancel := opCtx()
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `SELECT key, value FROM layout`)
	if err != nil {
		return nil, serr.Wrap(err, "op", "read layout")
	}
	defer rows.Close()
	for rows.Next() {
		var k, v string
		if err = rows.Scan(&k, &v); err != nil {
			return nil, serr.Wrap(err, "op", "scan layout")
		}
		out[k] = v
	}
	return out, wrap(rows.Err(), "op", "read layout")
}

// SetLayout merges values into the layout: each key given is replaced, the
// rest are kept.
func (s *Store) SetLayout(values map[string]string) error {
	if s.db == nil {
		s.mu.Lock()
		defer s.mu.Unlock()
		maps.Copy(s.layout, values)
		return nil
	}
	ctx, cancel := opCtx()
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return serr.Wrap(err, "op", "save layout")
	}
	defer func() { _ = tx.Rollback() }() // a no-op after Commit
	for k, v := range values {
		if _, err = tx.ExecContext(ctx, `
			INSERT INTO layout (key, value) VALUES ($1, $2)
			ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`, k, v); err != nil {
			return serr.Wrap(err, "op", "save layout", "key", k)
		}
	}
	return wrap(tx.Commit(), "op", "save layout")
}

// opCtx bounds one store operation. The store is a local file, so this is a
// backstop against a wedged disk, not a limit anything should reach.
func opCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 5*time.Second)
}

// wrap is serr.Wrap for an error that may be nil — serr.Wrap prints a
// complaint to stdout when handed one.
func wrap(err error, fields ...string) error {
	if err == nil {
		return nil
	}
	return serr.Wrap(err, fields...)
}
