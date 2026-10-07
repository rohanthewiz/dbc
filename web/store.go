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
// shared with the TUI, so a query run in either shows up in both — and
// neither are the connections added in the browser, which every dbc reads
// from connections.toml (config.SavedStore). Its conns table is where an
// older dbc web kept those; dbc web moves them out at startup (see
// SavedConn).
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
	conns  map[string]SavedConn
}

// Tab is one query tab as the browser left it.
type Tab struct {
	ID      string    `json:"id"`
	Title   string    `json:"title"`
	Conn    string    `json:"conn"`   // the connection it was on; "" before any
	Buffer  string    `json:"buffer"` // the editor's text
	Updated time.Time `json:"updated"`
	// Console is the name of the console of Conn's database the tab shows
	// (consoles.go); "" before it has one, or with consoles off. Its text
	// is the console's file; Buffer keeps a copy, so a dbc from before
	// consoles still opens the tab as it was.
	Console string `json:"console"`
	// Script makes it a script tab: the name of the Go script in
	// scripts_dir it edits (scripts.go). Its text is the script's file,
	// saved only when asked (Ctrl+S, or Run); Buffer stays "" and Conn is
	// only where the tab was when it opened. "" for a query tab.
	Script string `json:"script"`
}

// SavedConn is a row of the conns table, where dbc web kept connections
// added in the browser until they moved to connections.toml, which the TUI
// and headless runs can read beside a running dbc web (this file is locked
// by it). The table is now only read, once, by Server.moveStoreConns, which
// copies each row to config.SavedStore and deletes it here; the table stays
// in the schema so an older file opens, and so that read has something to
// query.
//
// DSN is as typed — ${VAR} references unexpanded — which is how it moves.
type SavedConn struct {
	Name   string
	Driver string
	DSN    string
	AIRows bool // config.Connection.AIRows: the assistant may see result rows
	Added  time.Time
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
	// a tab's console name (Tab.Console). A later column, so an ALTER;
	// bytdb v0.21.0+ takes ADD COLUMN IF NOT EXISTS, which keeps it
	// idempotent with the rest. The DEFAULT backfills the rows a store
	// from before the column already has, which NOT NULL alone would
	// refuse. Before v0.21.0 the name had a table of its own,
	// tab_consoles — see migrateTabConsoles.
	`ALTER TABLE tabs ADD COLUMN IF NOT EXISTS console TEXT NOT NULL DEFAULT ''`,
	// a script tab's script name (Tab.Script), added the same way
	`ALTER TABLE tabs ADD COLUMN IF NOT EXISTS script TEXT NOT NULL DEFAULT ''`,
	`CREATE TABLE IF NOT EXISTS conns (
		name    TEXT PRIMARY KEY,
		driver  TEXT NOT NULL,
		dsn     TEXT NOT NULL,
		ai_rows BOOLEAN NOT NULL,
		added   TIMESTAMPTZ NOT NULL
	)`,
}

// OpenStore opens (creating it if needed) the store at path. path == ""
// returns a memory-only store and no error. On a failure — the file locked
// by another dbc web, an unwritable directory — it returns a memory-only
// store AND the error, so the caller can warn and carry on.
func OpenStore(path string) (*Store, error) {
	st := &Store{path: path, tabs: map[string]Tab{}, layout: map[string]string{},
		conns: map[string]SavedConn{}}
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
	if err = migrateTabConsoles(ctx, dbh); err != nil {
		_ = dbh.Close()
		return st, serr.Wrap(err, "path", path)
	}
	st.db = dbh
	return st, nil
}

// migrateTabConsoles folds the tab_consoles table, where a store from before
// tabs.console kept each tab's console name, into that column, then drops
// it. A store that never had the table, or has already been migrated, costs
// one catalog read.
//
// bytdb runs no DDL inside a transaction, so this is two steps: the copy, as
// one transaction, then the DROP. A crash between them leaves the table for
// the next open to copy again, which is harmless: the values are the same
// ones. An older dbc web run against the store in the meantime recreates the
// table (its schema has it) and writes the names there, not to the column it
// does not know; copying them over the column is then the right thing too,
// since they are the newer ones.
func migrateTabConsoles(ctx context.Context, dbh *sql.DB) error {
	// information_schema rather than probing with a SELECT, so "no such
	// table" never has to be told apart from a real failure by its text
	var n int
	if err := dbh.QueryRowContext(ctx, `SELECT count(*) FROM information_schema.tables
		WHERE table_name = 'tab_consoles'`).Scan(&n); err != nil {
		return serr.Wrap(err, "op", "find tab_consoles")
	}
	if n == 0 {
		return nil
	}
	tx, err := dbh.BeginTx(ctx, nil)
	if err != nil {
		return serr.Wrap(err, "op", "migrate tab consoles")
	}
	defer func() { _ = tx.Rollback() }() // a no-op after Commit
	// Read every row before the first UPDATE: the store runs on one
	// connection (see OpenStore), so open rows and a write cannot overlap.
	rows, err := tx.QueryContext(ctx, `SELECT id, console FROM tab_consoles`)
	if err != nil {
		return serr.Wrap(err, "op", "read tab_consoles")
	}
	byID := map[string]string{}
	for rows.Next() {
		var id, c string
		if err = rows.Scan(&id, &c); err != nil {
			_ = rows.Close()
			return serr.Wrap(err, "op", "scan tab_consoles")
		}
		byID[id] = c
	}
	_ = rows.Close()
	if err = rows.Err(); err != nil {
		return serr.Wrap(err, "op", "read tab_consoles")
	}
	// A row whose tab is gone (an older dbc deleted the two separately)
	// matches nothing and drops with the table.
	for id, c := range byID {
		if _, err = tx.ExecContext(ctx, `UPDATE tabs SET console = $1 WHERE id = $2`, c, id); err != nil {
			return serr.Wrap(err, "op", "copy tab console", "tab", id)
		}
	}
	if err = tx.Commit(); err != nil {
		return serr.Wrap(err, "op", "migrate tab consoles")
	}
	_, err = dbh.ExecContext(ctx, `DROP TABLE IF EXISTS tab_consoles`)
	return wrap(err, "op", "drop tab_consoles")
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
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, title, conn, buffer, updated, console, script FROM tabs ORDER BY id`)
	if err != nil {
		return nil, serr.Wrap(err, "op", "list tabs")
	}
	defer rows.Close()
	var out []Tab
	for rows.Next() {
		var t Tab
		if err = rows.Scan(&t.ID, &t.Title, &t.Conn, &t.Buffer, &t.Updated, &t.Console, &t.Script); err != nil {
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
	// one row now (see storeSchema), so one statement: the transaction the
	// two-table write needed is gone with tab_consoles
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO tabs (id, title, conn, buffer, updated, console, script) VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (id) DO UPDATE SET
			title = EXCLUDED.title, conn = EXCLUDED.conn,
			buffer = EXCLUDED.buffer, updated = EXCLUDED.updated,
			console = EXCLUDED.console, script = EXCLUDED.script`,
		t.ID, t.Title, t.Conn, t.Buffer, t.Updated, t.Console, t.Script)
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

// MoveLayout renames layout keys: each from key's value is stored under its
// to key, replacing whatever was there, and the from key is deleted. A from
// key with no saved value is skipped, so a missing one leaves its to key
// alone. It is how a connection rename carries the "tableSchema.<conn>"
// schema picks along (handleConnEdit) — including picks saved by a window
// that has since closed, which no page is left to move.
//
// The moves are applied as one simultaneous rename: every from value is
// read, then every from key deleted, then every to key written. A rename
// onto a name under the old one ("a" → "a/b") makes chains — "a" → "a/b"
// and "a/b" → "a/b/b" in one call — that applied one by one in map order
// could read a value another move had just written.
//
// One transaction for the lot, so a rename's picks move together or not at
// all. A delete, not a blank: the page's own move (connRenamed) can only
// blank, as SetLayout has no delete, but "" is itself a pick ("every
// schema") and would apply to a later connection given the old name.
func (s *Store) MoveLayout(moves map[string]string) error {
	if s.db == nil {
		s.mu.Lock()
		defer s.mu.Unlock()
		vals := map[string]string{}
		for from, to := range moves {
			if v, ok := s.layout[from]; ok {
				vals[to] = v
			}
		}
		for from := range moves {
			delete(s.layout, from)
		}
		maps.Copy(s.layout, vals)
		return nil
	}
	ctx, cancel := opCtx()
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return serr.Wrap(err, "op", "move layout")
	}
	defer func() { _ = tx.Rollback() }() // a no-op after Commit
	vals := map[string]string{}
	for from, to := range moves {
		var v string
		err = tx.QueryRowContext(ctx, `SELECT value FROM layout WHERE key = $1`, from).Scan(&v)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return serr.Wrap(err, "op", "move layout", "key", from)
		}
		vals[to] = v
	}
	for from := range moves {
		if _, err = tx.ExecContext(ctx, `DELETE FROM layout WHERE key = $1`, from); err != nil {
			return serr.Wrap(err, "op", "move layout", "key", from)
		}
	}
	for to, v := range vals {
		if _, err = tx.ExecContext(ctx, `
			INSERT INTO layout (key, value) VALUES ($1, $2)
			ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`, to, v); err != nil {
			return serr.Wrap(err, "op", "move layout", "key", to)
		}
	}
	return wrap(tx.Commit(), "op", "move layout")
}

// Conns lists the conns table's rows (see SavedConn), oldest first — the
// order they were added, which moveStoreConns keeps in the file.
func (s *Store) Conns() ([]SavedConn, error) {
	if s.db == nil {
		s.mu.Lock()
		defer s.mu.Unlock()
		out := make([]SavedConn, 0, len(s.conns))
		for _, c := range s.conns {
			out = append(out, c)
		}
		slices.SortFunc(out, func(a, b SavedConn) int { return a.Added.Compare(b.Added) })
		return out, nil
	}
	ctx, cancel := opCtx()
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `SELECT name, driver, dsn, ai_rows, added FROM conns ORDER BY added, name`)
	if err != nil {
		return nil, serr.Wrap(err, "op", "list conns")
	}
	defer rows.Close()
	var out []SavedConn
	for rows.Next() {
		var c SavedConn
		if err = rows.Scan(&c.Name, &c.Driver, &c.DSN, &c.AIRows, &c.Added); err != nil {
			return nil, serr.Wrap(err, "op", "scan conn")
		}
		out = append(out, c)
	}
	return out, wrap(rows.Err(), "op", "list conns")
}

// SaveConn adds a row to the conns table. Nothing in dbc web writes the
// table any more (see SavedConn); this is how the tests make a store as an
// older dbc web left it. It does not replace a row by the same name.
func (s *Store) SaveConn(c SavedConn) error {
	if c.Added.IsZero() {
		c.Added = time.Now().UTC()
	}
	if s.db == nil {
		s.mu.Lock()
		defer s.mu.Unlock()
		if _, dup := s.conns[c.Name]; dup {
			return serr.New("a saved connection by that name already exists", "name", c.Name)
		}
		s.conns[c.Name] = c
		return nil
	}
	ctx, cancel := opCtx()
	defer cancel()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO conns (name, driver, dsn, ai_rows, added) VALUES ($1, $2, $3, $4, $5)`,
		c.Name, c.Driver, c.DSN, c.AIRows, c.Added)
	return wrap(err, "op", "save conn", "name", c.Name)
}

// RetagTabs moves the saved query tabs on connection from to connection to
// — a rename of a connection added in the browser (handleConnEdit). A saved
// tab's conn is the connection it reconnects to when shown again; left on a
// name that no longer exists it would fall back to whatever the window has
// active, and the user would find a tab quietly on another database.
// (History and saved assistant chats keep the old name: they record what
// happened, under the name it had then.)
func (s *Store) RetagTabs(from, to string) error {
	if s.db == nil {
		s.mu.Lock()
		defer s.mu.Unlock()
		for id, t := range s.tabs {
			if t.Conn == from {
				t.Conn = to
				s.tabs[id] = t
			}
		}
		return nil
	}
	ctx, cancel := opCtx()
	defer cancel()
	_, err := s.db.ExecContext(ctx, `UPDATE tabs SET conn = $1 WHERE conn = $2`, to, from)
	return wrap(err, "op", "retag tabs", "from", from, "to", to)
}

// DeleteConn removes a conns table row, once moveStoreConns has moved it.
// A missing one is success.
func (s *Store) DeleteConn(name string) error {
	if s.db == nil {
		s.mu.Lock()
		defer s.mu.Unlock()
		delete(s.conns, name)
		return nil
	}
	ctx, cancel := opCtx()
	defer cancel()
	_, err := s.db.ExecContext(ctx, `DELETE FROM conns WHERE name = $1`, name)
	return wrap(err, "op", "delete conn", "name", name)
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
