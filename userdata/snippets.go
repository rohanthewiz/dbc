package userdata

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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
//
// LANGUAGES. A snippet is SQL or Go (Lang). It expands only in a document
// of its language: the TUI's editor and dbc web's query tabs are SQL, dbc
// web's script tabs Go — so a Go "iferr" never fires in a query, nor a SQL
// "sel" in a script. Names stay unique across both languages: the name is
// the snippet's key everywhere (the store's primary key, the web routes,
// a rename), and a name meaning two things depending on the tab would make
// "edit sel" or "delete sel" ambiguous in the dialogs, which list both.

// Snippet is one saved snippet.
type Snippet struct {
	Name string `json:"name"` // what is typed before Tab; snippet.ValidName
	Desc string `json:"desc"` // one line, shown in the pickers
	Body string `json:"body"` // the template (package snippet)
	// Lang is the language of the documents it expands in: LangSQL or
	// LangGo. "" on a value being saved means SQL (NormLang), so a caller
	// that predates languages still saves what it always did.
	Lang    string    `json:"lang"`
	Updated time.Time `json:"updated"`
}

// The languages a snippet can be for.
const (
	LangSQL = "sql" // the TUI's editor, dbc web's query tabs
	LangGo  = "go"  // dbc web's script tabs (sdb.S scripts)
)

// SnippetLangs are the languages in the order the forms offer them.
var SnippetLangs = []string{LangSQL, LangGo}

// NormLang is lang as the store keeps it: lower case, "" read as SQL (a
// snippet saved before languages existed, or by a caller that does not
// say). Any other name is refused rather than stored, since a snippet in
// a language no document has would never expand anywhere.
func NormLang(lang string) (string, error) {
	switch l := strings.ToLower(strings.TrimSpace(lang)); l {
	case "", LangSQL:
		return LangSQL, nil
	case LangGo:
		return LangGo, nil
	default:
		return "", fmt.Errorf("a snippet is for sql or go, not %q", lang)
	}
}

// LangLabel is a language as the pickers name it.
func LangLabel(lang string) string {
	if lang == LangGo {
		return "Go"
	}
	return "SQL"
}

// For reports whether sn expands in a document of language lang.
func (sn Snippet) For(lang string) bool {
	l, _ := NormLang(sn.Lang)
	want, _ := NormLang(lang)
	return l == want
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

// StarterSnippets are written into a new store once per language, so the
// feature shows what it does before anyone has written a snippet: stops
// visited out of place order (sel), a mirrored stop (cnt), $SELECTION$
// (cte, countof, tx) and $END$ (upd, qry). They are ordinary rows after
// that — edited or deleted like any other, and not brought back (the
// seeded flags, not the rows, say the store was seeded).
//
// Each language has its own flag (seedKey) so a store made before Go
// snippets existed — seeded with the SQL starters only — still gets the
// Go ones, once, the first time a dbc that knows Go opens it.
//
// The Go bodies indent with a tab, as gofmt and the script tabs do.
var StarterSnippets = []Snippet{
	{Name: "sel", Lang: LangSQL, Desc: "select from a table",
		Body: "SELECT $VAR2:*$\nFROM $VAR1:table$\nWHERE $VAR3:true$\nLIMIT $VAR4:100$"},
	{Name: "cnt", Lang: LangSQL, Desc: "count rows per value",
		Body: "SELECT $VAR2:column$, count(*) AS n\nFROM $VAR1:table$\nGROUP BY $VAR2$\nORDER BY n DESC"},
	{Name: "cte", Lang: LangSQL, Desc: "wrap the selection in a CTE",
		Body: "WITH $VAR1:q$ AS (\n    $SELECTION$\n)\nSELECT *\nFROM $VAR1$$END$"},
	{Name: "countof", Lang: LangSQL, Desc: "count the rows the selection returns",
		Body: "SELECT count(*)\nFROM (\n    $SELECTION$\n) AS $VAR1:t$"},
	{Name: "upd", Lang: LangSQL, Desc: "update rows inside a transaction",
		Body: "BEGIN;\nUPDATE $VAR1:table$\nSET $VAR2:column$ = $VAR3:value$\nWHERE $VAR4:id = 0$;\n-- check the count, then COMMIT; or ROLLBACK;\n$END$"},
	{Name: "iferr", Lang: LangGo, Desc: "return a non-nil error",
		Body: "if $VAR1:err$ != nil {\n\treturn $VAR1$\n}\n$END$"},
	{Name: "qry", Lang: LangGo, Desc: "query a connection and print each row",
		Body: "res, err := s.Query(\"$VAR1:conn$\", `$VAR2:SELECT 1$`)\nif err != nil {\n\treturn err\n}\nfor _, row := range res.Rows {\n\ts.Print(\"%v\", row)\n}\n$END$"},
	{Name: "exec", Lang: LangGo, Desc: "run a statement and print the rows it changed",
		Body: "n, err := s.Exec(\"$VAR1:conn$\", `$VAR2:UPDATE t SET x = 1$`)\nif err != nil {\n\treturn err\n}\ns.Print(\"%d rows\", n)\n$END$"},
	{Name: "each", Lang: LangGo, Desc: "run the selection once per connection",
		Body: "for _, $VAR1:conn$ := range s.Conns() {\n\t$SELECTION$$END$\n}"},
}

// seedKey is the meta row that marks lang's starters as written. SQL
// keeps the key it had before languages, "seeded", so a store seeded by
// an older dbc is not seeded with the SQL starters a second time.
func seedKey(lang string) string {
	if lang == LangSQL {
		return "seeded"
	}
	return "seeded_" + lang
}

// snippetSchema is run on every open: IF NOT EXISTS keeps it idempotent.
// meta holds the seeded flags; a later setting would go there too.
//
// lang came after the table did: a new file gets it from the CREATE, and
// a file made before it from the ALTER, which backfills the rows already
// there as SQL — all a snippet could be then. An older dbc that writes to
// a file with the column inserts without it and gets the same default,
// and its upsert does not name lang, so its edit of a Go snippet keeps
// the snippet Go.
var snippetSchema = []string{
	`CREATE TABLE IF NOT EXISTS snippets (
		name    TEXT PRIMARY KEY,
		descr   TEXT NOT NULL,
		body    TEXT NOT NULL,
		updated TIMESTAMPTZ NOT NULL,
		lang    TEXT NOT NULL DEFAULT 'sql'
	)`,
	`ALTER TABLE snippets ADD COLUMN IF NOT EXISTS lang TEXT NOT NULL DEFAULT 'sql'`,
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

// GetFor is Get for a document of language lang: a snippet of another
// language is not found, so its name typed in this document is just a
// word — Tab does what it did before snippets.
func (s *Snippets) GetFor(name, lang string) (Snippet, bool, error) {
	sn, found, err := s.Get(name)
	if err != nil || !found || !sn.For(lang) {
		return Snippet{}, false, err
	}
	return sn, true, nil
}

// ListFor is List narrowed to the snippets of language lang.
func (s *Snippets) ListFor(lang string) ([]Snippet, error) {
	list, err := s.List()
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(list, func(sn Snippet) bool { return !sn.For(lang) }), nil
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
	lang, err := NormLang(sn.Lang)
	if err != nil {
		return err
	}
	sn.Lang = lang
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
	err = s.withDB(func(ctx context.Context, dbh *sql.DB) error {
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
		if _, err := tx.ExecContext(ctx, `INSERT INTO snippets (name, descr, body, updated, lang) VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (name) DO UPDATE SET descr = excluded.descr, body = excluded.body,
				updated = excluded.updated, lang = excluded.lang`,
			sn.Name, sn.Desc, sn.Body, sn.Updated, sn.Lang); err != nil {
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

// seedSnippets writes the starters of each language whose store was never
// seeded with them (seedKey), and marks it seeded in the same transaction,
// so a crash between the two cannot seed twice — nor bring back a starter
// deleted since. ON CONFLICT DO NOTHING keeps a snippet the user already
// made under a starter's name (a Go starter arriving in an older store).
func seedSnippets(ctx context.Context, dbh *sql.DB) error {
	for _, lang := range SnippetLangs {
		if err := seedLang(ctx, dbh, lang); err != nil {
			return err
		}
	}
	return nil
}

func seedLang(ctx context.Context, dbh *sql.DB, lang string) error {
	key := seedKey(lang)
	var n int
	if err := dbh.QueryRowContext(ctx, `SELECT count(*) FROM meta WHERE key = $1`, key).Scan(&n); err != nil || n > 0 {
		return err
	}
	tx, err := dbh.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().UTC().Truncate(time.Microsecond)
	for _, sn := range StarterSnippets {
		if sn.Lang != lang {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO snippets (name, descr, body, updated, lang) VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (name) DO NOTHING`, sn.Name, sn.Desc, sn.Body, now, sn.Lang); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO meta (key, value) VALUES ($1, '1')`, key); err != nil {
		return err
	}
	return tx.Commit()
}

func querySnippets(ctx context.Context, dbh *sql.DB) ([]Snippet, error) {
	rows, err := dbh.QueryContext(ctx, `SELECT name, descr, body, updated, lang FROM snippets ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Snippet
	for rows.Next() {
		var sn Snippet
		var lang sql.NullString // NOT NULL in the schema; read defensively all the same
		if err := rows.Scan(&sn.Name, &sn.Desc, &sn.Body, &sn.Updated, &lang); err != nil {
			return nil, err
		}
		// a language this dbc does not know (written by a newer one) is
		// kept as it is: it is listed, and expands nowhere here
		sn.Lang = lang.String
		if l, err := NormLang(lang.String); err == nil {
			sn.Lang = l
		}
		out = append(out, sn)
	}
	return out, rows.Err()
}

func sortSnippets(list []Snippet) {
	slices.SortFunc(list, func(a, b Snippet) int { return strings.Compare(a.Name, b.Name) })
}
