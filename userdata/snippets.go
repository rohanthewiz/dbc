package userdata

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/rohanthewiz/bytdb"
	bytdbdrv "github.com/rohanthewiz/bytdb/stdlib"
	"github.com/rohanthewiz/serr"

	"github.com/rohanthewiz/dbc/snippet"
)

// Snippets: the live snippets both editors expand (package snippet has the
// template syntax), kept in a bytdb file, ~/.config/dbc/snippets.bytdb.
//
// WHY OPEN PER OPERATION. bytdb takes an exclusive lock on its file for as
// long as it is open (see web.Store), and the TUI and dbc web may both be
// running, each wanting the same snippets — so neither may hold the file.
// Every read and write opens it, does its work and closes it, which costs a
// WAL replay of a few kilobytes; one that finds the file held by the other
// process waits a moment and tries again (withDB). Between operations the
// list is cached against the file's size and modification time, so the
// editors' Tab — which asks "is the word before the caret a snippet?" on
// every press — opens the file only after something changed it.
//
//	TUI ──┐                    ┌── open · read/write · close ──┐
//	      ├─ Snippets.List ────┤                               ├── snippets.bytdb
//	web ──┘   (cached on stat) └── ErrLocked: wait, retry ─────┘     (+ .lock)
//
// A Snippets made with path "" keeps everything in memory, seeded the same
// way: tests, and a machine with no home directory.

// Snippet is one saved snippet.
type Snippet struct {
	Name    string    `json:"name"` // what is typed before Tab; snippet.ValidName
	Desc    string    `json:"desc"` // one line, shown in the pickers
	Body    string    `json:"body"` // the template (package snippet)
	Updated time.Time `json:"updated"`
}

// SnippetsFile is where the snippets live, beside the history and the
// connections; "" when no home directory is known.
func SnippetsFile() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "dbc", "snippets.bytdb")
}

// StarterSnippets are written into a new store once, so the feature shows
// what it does before anyone has written a snippet: stops visited out of
// place order (sel), a mirrored stop (cnt), $SELECTION$ (cte, countof) and
// $END$ (upd). They are ordinary rows after that — edited or deleted like
// any other, and not brought back (the seeded flag, not the rows, says the
// store was seeded).
var StarterSnippets = []Snippet{
	{Name: "sel", Desc: "select from a table",
		Body: "SELECT $VAR2:*$\nFROM $VAR1:table$\nWHERE $VAR3:true$\nLIMIT $VAR4:100$"},
	{Name: "cnt", Desc: "count rows per value",
		Body: "SELECT $VAR2:column$, count(*) AS n\nFROM $VAR1:table$\nGROUP BY $VAR2$\nORDER BY n DESC"},
	{Name: "cte", Desc: "wrap the selection in a CTE",
		Body: "WITH $VAR1:q$ AS (\n    $SELECTION$\n)\nSELECT *\nFROM $VAR1$$END$"},
	{Name: "countof", Desc: "count the rows the selection returns",
		Body: "SELECT count(*)\nFROM (\n    $SELECTION$\n) AS $VAR1:t$"},
	{Name: "upd", Desc: "update rows inside a transaction",
		Body: "BEGIN;\nUPDATE $VAR1:table$\nSET $VAR2:column$ = $VAR3:value$\nWHERE $VAR4:id = 0$;\n-- check the count, then COMMIT; or ROLLBACK;\n$END$"},
}

// snippetSchema is run on every open: IF NOT EXISTS keeps it idempotent.
// meta holds the seeded flag; a later setting would go there too.
var snippetSchema = []string{
	`CREATE TABLE IF NOT EXISTS snippets (
		name    TEXT PRIMARY KEY,
		descr   TEXT NOT NULL,
		body    TEXT NOT NULL,
		updated TIMESTAMPTZ NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS meta (
		key   TEXT PRIMARY KEY,
		value TEXT NOT NULL
	)`,
}

// lockWait is how long an operation keeps retrying a file the other
// process holds. Its operations are a few milliseconds each, so a second
// is a long queue; past it the caller reports the store as busy.
const lockWait = time.Second

// ErrSnippetExists refuses a save that would overwrite another snippet
// (a new one, or a rename, onto a name in use).
var ErrSnippetExists = errors.New("a snippet by that name already exists")

// ErrNoSnippet is a delete or rename of a name not in the store.
var ErrNoSnippet = errors.New("no such snippet")

// ErrSnippetsBusy is an operation that waited lockWait for the other
// process to let go of the file and gave up: worth saying as "try again",
// not as a failure.
var ErrSnippetsBusy = errors.New("the snippets file is busy (another dbc is using it) — try again")

// Snippets is the store. Safe for concurrent use: dbc web's handlers and
// its Tab lookups share one.
type Snippets struct {
	path string

	mu    sync.Mutex
	list  []Snippet // the cache (memory mode: the store itself), by name
	stamp fileStamp // the file as list was read from it
	read  bool      // list has been read at least once
}

// fileStamp is what a stat says about the file, enough to tell that some
// process wrote it since.
type fileStamp struct {
	size int64
	mod  time.Time
}

// OpenSnippets returns the store at path ("" for memory only). It does no
// IO: the file is read on the first List.
func OpenSnippets(path string) *Snippets {
	s := &Snippets{path: path}
	if path == "" {
		s.list = slices.Clone(StarterSnippets)
		sortSnippets(s.list)
		s.read = true
	}
	return s
}

// Path is the store's file, "" in memory.
func (s *Snippets) Path() string { return s.path }

// List returns every snippet, by name. It reads the file only when it
// changed since the last read (see the package comment above).
func (s *Snippets) List() ([]Snippet, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.refresh(); err != nil {
		return nil, err
	}
	return slices.Clone(s.list), nil
}

// Get returns the snippet called name — a Tab's lookup.
func (s *Snippets) Get(name string) (Snippet, bool, error) {
	list, err := s.List()
	if err != nil {
		return Snippet{}, false, err
	}
	i := slices.IndexFunc(list, func(sn Snippet) bool { return sn.Name == name })
	if i < 0 {
		return Snippet{}, false, nil
	}
	return list[i], true, nil
}

// refresh re-reads the file when its stamp moved. Called with mu held.
func (s *Snippets) refresh() error {
	if s.path == "" {
		return nil
	}
	st, err := stampOf(s.path)
	if err != nil {
		return err
	}
	if s.read && st == s.stamp && st != (fileStamp{}) {
		return nil
	}
	var list []Snippet
	err = s.withDB(func(ctx context.Context, dbh *sql.DB) error {
		var err error
		list, err = querySnippets(ctx, dbh)
		return err
	})
	if err != nil {
		return err
	}
	s.list, s.read = list, true
	// Stamped after the read, not before: opening may itself rewrite the
	// file (the schema, the seed), and a stamp from before that would make
	// the next List read it all again for nothing.
	s.stamp, _ = stampOf(s.path)
	return nil
}

// stampOf stats path; a file not there yet is the zero stamp.
func stampOf(path string) (fileStamp, error) {
	fi, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return fileStamp{}, nil
	}
	if err != nil {
		return fileStamp{}, serr.Wrap(err, "path", path)
	}
	return fileStamp{fi.Size(), fi.ModTime()}, nil
}

// Save writes sn, as a new snippet when old is "", else over the snippet
// called old — renaming it when sn.Name differs. The name and the body are
// checked first (snippet.ValidName, snippet.Parse), so a broken template
// is refused with the reason rather than saved to fail on every Tab.
func (s *Snippets) Save(old string, sn Snippet) error {
	sn.Name = strings.TrimSpace(sn.Name)
	sn.Desc = strings.Join(strings.Fields(sn.Desc), " ") // one line, as the pickers show it
	sn.Body = strings.ReplaceAll(sn.Body, "\r\n", "\n")
	if err := snippet.ValidName(sn.Name); err != nil {
		return err
	}
	if strings.TrimSpace(sn.Body) == "" {
		return errors.New("the snippet's body is empty")
	}
	if _, err := snippet.Parse(sn.Body); err != nil {
		return err
	}
	sn.Updated = time.Now().UTC().Truncate(time.Microsecond)

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.path == "" {
		return s.saveMem(old, sn)
	}
	err := s.withDB(func(ctx context.Context, dbh *sql.DB) error {
		tx, err := dbh.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		if err := checkSave(ctx, tx, old, sn.Name); err != nil {
			return err
		}
		if old != "" && old != sn.Name {
			if _, err := tx.ExecContext(ctx, `DELETE FROM snippets WHERE name = $1`, old); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO snippets (name, descr, body, updated) VALUES ($1, $2, $3, $4)
			ON CONFLICT (name) DO UPDATE SET descr = excluded.descr, body = excluded.body, updated = excluded.updated`,
			sn.Name, sn.Desc, sn.Body, sn.Updated); err != nil {
			return err
		}
		return tx.Commit()
	})
	s.read = false // the next List reads what is there now, whatever happened
	return err
}

// checkSave refuses a save onto a name in use (other than old itself),
// and an edit of a snippet that is gone (deleted elsewhere meanwhile).
func checkSave(ctx context.Context, tx *sql.Tx, old, name string) error {
	exists := func(n string) (bool, error) {
		var c int
		err := tx.QueryRowContext(ctx, `SELECT count(*) FROM snippets WHERE name = $1`, n).Scan(&c)
		return c > 0, err
	}
	if old != "" {
		ok, err := exists(old)
		if err != nil {
			return err
		}
		if !ok {
			return serr.Wrap(ErrNoSnippet, "name", old)
		}
	}
	if name != old {
		taken, err := exists(name)
		if err != nil {
			return err
		}
		if taken {
			return serr.Wrap(ErrSnippetExists, "name", name)
		}
	}
	return nil
}

// saveMem is Save in memory, by the same rules.
func (s *Snippets) saveMem(old string, sn Snippet) error {
	at := func(n string) int { return slices.IndexFunc(s.list, func(x Snippet) bool { return x.Name == n }) }
	if old != "" && at(old) < 0 {
		return serr.Wrap(ErrNoSnippet, "name", old)
	}
	if sn.Name != old && at(sn.Name) >= 0 {
		return serr.Wrap(ErrSnippetExists, "name", sn.Name)
	}
	if old != "" {
		s.list = slices.Delete(s.list, at(old), at(old)+1)
	}
	s.list = append(s.list, sn)
	sortSnippets(s.list)
	return nil
}

// Delete removes the snippet called name.
func (s *Snippets) Delete(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.path == "" {
		i := slices.IndexFunc(s.list, func(x Snippet) bool { return x.Name == name })
		if i < 0 {
			return serr.Wrap(ErrNoSnippet, "name", name)
		}
		s.list = slices.Delete(s.list, i, i+1)
		return nil
	}
	err := s.withDB(func(ctx context.Context, dbh *sql.DB) error {
		res, err := dbh.ExecContext(ctx, `DELETE FROM snippets WHERE name = $1`, name)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return serr.Wrap(ErrNoSnippet, "name", name)
		}
		return nil
	})
	s.read = false
	return err
}

// withDB opens the file, readies it (schema, first-time seed), runs fn and
// closes it, retrying for up to lockWait while the other process holds it.
func (s *Snippets) withDB(fn func(ctx context.Context, dbh *sql.DB) error) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return serr.Wrap(err, "path", s.path, "op", "mkdir")
	}
	var dbh *sql.DB
	var err error
	deadline := time.Now().Add(lockWait)
	for wait := 5 * time.Millisecond; ; wait = min(wait*2, 100*time.Millisecond) {
		// the driver opens the engine, and takes the lock, in sql.Open
		dbh, err = sql.Open(bytdbdrv.DriverName, s.path)
		if !errors.Is(err, bytdb.ErrLocked) || time.Now().After(deadline) {
			break
		}
		time.Sleep(wait)
	}
	if errors.Is(err, bytdb.ErrLocked) {
		return serr.Wrap(ErrSnippetsBusy, "path", s.path)
	}
	if err != nil {
		return serr.Wrap(err, "path", s.path)
	}
	defer func() { _ = dbh.Close() }()
	dbh.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, ddl := range snippetSchema {
		if _, err := dbh.ExecContext(ctx, ddl); err != nil {
			return serr.Wrap(err, "path", s.path, "op", "schema")
		}
	}
	if err := seedSnippets(ctx, dbh); err != nil {
		return serr.Wrap(err, "path", s.path, "op", "seed")
	}
	if err := fn(ctx, dbh); err != nil {
		return serr.Wrap(err, "path", s.path)
	}
	return nil
}

// seedSnippets writes StarterSnippets into a store that was never seeded,
// and marks it seeded in the same transaction, so a crash between the two
// cannot seed twice — nor bring back a starter deleted since.
func seedSnippets(ctx context.Context, dbh *sql.DB) error {
	var n int
	if err := dbh.QueryRowContext(ctx, `SELECT count(*) FROM meta WHERE key = 'seeded'`).Scan(&n); err != nil || n > 0 {
		return err
	}
	tx, err := dbh.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().UTC().Truncate(time.Microsecond)
	for _, sn := range StarterSnippets {
		if _, err := tx.ExecContext(ctx, `INSERT INTO snippets (name, descr, body, updated) VALUES ($1, $2, $3, $4)
			ON CONFLICT (name) DO NOTHING`, sn.Name, sn.Desc, sn.Body, now); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO meta (key, value) VALUES ('seeded', '1')`); err != nil {
		return err
	}
	return tx.Commit()
}

func querySnippets(ctx context.Context, dbh *sql.DB) ([]Snippet, error) {
	rows, err := dbh.QueryContext(ctx, `SELECT name, descr, body, updated FROM snippets ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Snippet
	for rows.Next() {
		var sn Snippet
		if err := rows.Scan(&sn.Name, &sn.Desc, &sn.Body, &sn.Updated); err != nil {
			return nil, err
		}
		out = append(out, sn)
	}
	return out, rows.Err()
}

func sortSnippets(list []Snippet) {
	slices.SortFunc(list, func(a, b Snippet) int { return strings.Compare(a.Name, b.Name) })
}
