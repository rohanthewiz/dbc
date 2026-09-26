package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/rohanthewiz/btypedb"
	"github.com/rohanthewiz/serr"
)

// Saved connections: the ones added in dbc web's browser UI, kept in a file
// of their own, ~/.config/dbc/connections.toml, so that every way of running
// dbc — the TUI, a headless run, script, migrate, explain, and dbc web
// itself — sees them.
//
// WHY A FILE OF ITS OWN. The config file is the user's: the TOML encoder
// would drop its comments and layout, so dbc never writes it back. This one
// is dbc's, written whole every time, so nothing in it is lost that dbc did
// not put there (the header says comments are not kept).
//
// WHY NOT web.bytdb, WHERE THEY WERE. A running dbc web holds that file's
// lock for as long as it runs, so the TUI could not open it beside it. A
// plain file needs no lock to read.
//
// WHY TOML AND NOT A bytdb FILE OF ITS OWN. Every dbc start reads this file,
// and a bytdb one would mean opening an engine (and its exclusive lock) for
// each — contending with a dbc web in the middle of a write. It is also a
// list of a few entries a person may want to read, fix or copy into
// config.toml, which is what TOML is for.
//
// Reading and writing:
//
//	readers (every start) ──► read the file; no lock
//	                           the rename below means a reader sees the old
//	                           file or the new one, never half of one
//
//	writers (dbc web)     ──► AcquireLock(file) — the sidecar connections.toml.lock
//	                           read ─► change ─► write a temp file ─► fsync ─► rename over
//	                           release
//
// The lock is only for writers: a read-modify-write by two dbc webs at once
// would otherwise lose one's change. It is btypedb's sidecar lock (flock on
// Unix, a share mode on Windows), which the OS drops if the holder dies, so
// a crash never leaves the file stuck. It is held for a few milliseconds, so
// a writer that finds it taken waits briefly (lockWait) rather than failing.
//
// PASSWORDS. A DSN is stored as typed: a ${VAR} reference stays unexpanded
// and is expanded by each process as it merges the entry (ExpandDSN), so a
// password kept in the environment never lands in the file. One typed inline
// is stored as typed, in a file created 0600 in the 0700 ~/.config/dbc — as
// safe as config.toml, which holds DSNs the same way.

// SavedConn is one connection in the saved-connections file.
type SavedConn struct {
	Name   string `toml:"name"`
	Driver string `toml:"driver"`
	DSN    string `toml:"dsn"` // as typed: ${VAR}s unexpanded
	// AIRows is Connection.AIRows: the assistant may see result rows.
	AIRows bool `toml:"ai_rows"`
	// Added orders the sidebar in dbc web (after the config file's). It is
	// kept across an edit so an edited entry does not move.
	Added time.Time `toml:"added"`
}

// savedDoc is the file's shape: [[connection]] tables, like config.toml's,
// so an entry can be copied from one file to the other as it is.
type savedDoc struct {
	Connections []SavedConn `toml:"connection"`
}

// savedHeader starts every write. The file is rewritten whole, so it says
// what that costs a hand edit.
const savedHeader = `# Connections added in dbc web. Every dbc (the TUI, headless runs, dbc web)
# reads them after config.toml's; a name config.toml also defines is skipped.
# dbc rewrites this file whole when the browser adds, edits or removes one:
# hand edits to entries are kept, comments are not. A DSN may reference
# ${VAR}s, expanded from the environment of each dbc that reads it.

`

// SavedFile is where saved connections live, beside config.toml; "" when no
// home directory is known, and they then last only as long as a dbc web.
func SavedFile() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "dbc", "connections.toml")
}

// ReadSaved reads the saved connections at path, in file order. A missing
// file, or path == "", is none and no error: most users never add one.
func ReadSaved(path string) ([]SavedConn, error) {
	if path == "" {
		return nil, nil
	}
	bs, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, serr.Wrap(err, "path", path)
	}
	var doc savedDoc
	if err = toml.Unmarshal(bs, &doc); err != nil {
		return nil, serr.Wrap(err, "path", path)
	}
	return doc.Connections, nil
}

// LoadSaved merges the saved connections at path into c, after what c
// already holds, recording anything skipped in c.Warnings. A file that
// cannot be read or parsed is a warning too, not a failure: the config
// file's connections still work, and refusing to start over a stray
// character in a file dbc wrote would be out of proportion. See MergeSaved
// for knownDriver.
//
// Call it once c is complete (after Load, the demos and any ad-hoc
// connection), before anything else can see c.
func (c *Config) LoadSaved(path string, knownDriver func(string) bool) {
	saved, err := ReadSaved(path)
	if err != nil {
		c.Warnings = append(c.Warnings, fmt.Sprintf(
			"connections added in dbc web could not be read, so they are not listed: %v", err))
		return
	}
	c.Warnings = append(c.Warnings, c.MergeSaved(saved, knownDriver)...)
}

// MergeSaved adds saved connections to c, marked Web, and returns warnings
// for the ones it skips; it does not add them to c.Warnings (dbc web also
// merges the entries it moves from its old store, and logs them itself).
//
// Skipped, and left in the file for the user to sort out:
//   - an entry with no name or no driver (a hand edit gone wrong);
//   - a name c already has — the config file's word wins, it being the
//     user's deliberate one where this is only what was once typed in a
//     form; so does the first of two saved entries by one name;
//   - a driver knownDriver rejects. config cannot know the drivers itself
//     (it is a leaf; db registers them), so the caller passes db's check.
//     nil accepts every driver, and an unknown one fails at connect.
func (c *Config) MergeSaved(saved []SavedConn, knownDriver func(string) bool) []string {
	var warns []string
	for _, sc := range saved {
		switch {
		case strings.TrimSpace(sc.Name) == "" || strings.TrimSpace(sc.Driver) == "":
			warns = append(warns, fmt.Sprintf(
				"saved connection %q skipped: it needs both a name and a driver", sc.Name))
			continue
		case knownDriver != nil && !knownDriver(sc.Driver):
			warns = append(warns, fmt.Sprintf(
				"saved connection %q skipped: unknown driver %q", sc.Name, sc.Driver))
			continue
		}
		dsn, w := ExpandDSN(sc.Name, sc.DSN)
		if err := c.AddConn(Connection{Name: sc.Name, Driver: sc.Driver, DSN: dsn,
			AIRows: sc.AIRows, Web: true}); err != nil {
			warns = append(warns, fmt.Sprintf(
				"saved connection %q skipped: a connection by that name is already defined "+
					"(rename one of them, in the config file or in dbc web)", sc.Name))
			continue
		}
		// only a merged entry's unset ${VAR}s are worth a word
		warns = append(warns, w...)
	}
	return warns
}

// lockWait bounds how long a writer waits for another's lock. A write
// holds it for milliseconds, so running out means something is wrong (a
// process wedged mid-write) and saying so beats hanging the request.
const lockWait = 3 * time.Second

// SavedStore is the writer of the saved-connections file, for dbc web: each
// change is a locked read-modify-write of the whole file, so it always
// starts from what is on disk — another dbc web's change, or a hand edit,
// included — rather than from what this process read at startup.
//
// With path == "" (no home directory) it keeps the list in memory, lasting
// as long as the process, and callers do not branch on which kind they have.
type SavedStore struct {
	path string

	mu  sync.Mutex  // orders this process's writers; the file lock orders processes
	mem []SavedConn // path == "" only
}

// OpenSaved returns the store for path. Nothing is read or created until
// the first call.
func OpenSaved(path string) *SavedStore { return &SavedStore{path: path} }

// Persistent reports whether changes reach a file.
func (s *SavedStore) Persistent() bool { return s.path != "" }

// Path is the file; "" for a memory-only store.
func (s *SavedStore) Path() string { return s.path }

// List returns the saved connections, in file order.
func (s *SavedStore) List() ([]SavedConn, error) {
	if s.path == "" {
		s.mu.Lock()
		defer s.mu.Unlock()
		return slices.Clone(s.mem), nil
	}
	return ReadSaved(s.path)
}

// Get finds one saved connection by name.
func (s *SavedStore) Get(name string) (SavedConn, bool, error) {
	all, err := s.List()
	if err != nil {
		return SavedConn{}, false, err
	}
	for _, sc := range all {
		if sc.Name == name {
			return sc, true, nil
		}
	}
	return SavedConn{}, false, nil
}

// Add appends a connection. A name the file already has is ErrConnExists —
// another dbc web may have added it since this one started — rather than a
// replacement. A zero Added is now.
func (s *SavedStore) Add(c SavedConn) error {
	if c.Added.IsZero() {
		c.Added = time.Now()
	}
	// whole seconds: the file is for reading, and nothing orders finer
	c.Added = c.Added.UTC().Truncate(time.Second)
	return s.edit(func(all []SavedConn) ([]SavedConn, error) {
		if slices.ContainsFunc(all, func(x SavedConn) bool { return x.Name == c.Name }) {
			return nil, serr.Wrap(ErrConnExists, "name", c.Name)
		}
		return append(all, c), nil
	})
}

// Update replaces the connection named old with c, in place (the file's
// order is the sidebar's), keeping old's Added. c may carry a new name;
// one another entry has is ErrConnExists. A missing old is
// ErrConnNotFound: it was removed meanwhile, and writing c would bring it
// back.
func (s *SavedStore) Update(old string, c SavedConn) error {
	return s.edit(func(all []SavedConn) ([]SavedConn, error) {
		at := -1
		for i, x := range all {
			switch {
			case x.Name == old:
				at = i
			case x.Name == c.Name:
				return nil, serr.Wrap(ErrConnExists, "name", c.Name)
			}
		}
		if at < 0 {
			return nil, serr.Wrap(ErrConnNotFound, "name", old)
		}
		c.Added = all[at].Added
		all[at] = c
		return all, nil
	})
}

// Delete removes a connection. A missing one is success: the caller wanted
// it gone.
func (s *SavedStore) Delete(name string) error {
	return s.edit(func(all []SavedConn) ([]SavedConn, error) {
		return slices.DeleteFunc(all, func(x SavedConn) bool { return x.Name == name }), nil
	})
}

// edit runs one read-modify-write. change gets a list it may modify and
// returns the list to write; an error from it writes nothing.
//
// A file that cannot be parsed fails the edit rather than being replaced:
// it is most likely a hand edit with a typo, and overwriting it would throw
// away every entry in it.
func (s *SavedStore) edit(change func([]SavedConn) ([]SavedConn, error)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.path == "" {
		next, err := change(slices.Clone(s.mem))
		if err != nil {
			return err
		}
		s.mem = next
		return nil
	}

	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return serr.Wrap(err, "path", s.path, "op", "mkdir")
	}
	lock, err := lockSaved(s.path)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }()

	all, err := ReadSaved(s.path)
	if err != nil {
		return serr.Wrap(err, "op", "read saved connections before a change")
	}
	next, err := change(all)
	if err != nil {
		return err
	}
	return writeSaved(s.path, next)
}

// lockSaved takes the writers' lock on path, waiting up to lockWait for
// another writer to finish. btypedb.AcquireLock does not wait itself (it is
// made for failing fast on a database left open elsewhere), hence the poll;
// at 10ms a waiting writer is late by at most that much.
func lockSaved(path string) (io.Closer, error) {
	deadline := time.Now().Add(lockWait)
	for {
		lock, err := btypedb.AcquireLock(path)
		if err == nil {
			return lock, nil
		}
		if !errors.Is(err, btypedb.ErrLocked) || time.Now().After(deadline) {
			return nil, serr.Wrap(err, "op", "lock saved connections")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// writeSaved replaces the file with list, atomically: a temp file in the
// same directory (so the rename stays on one filesystem, where it is
// atomic), synced, then renamed over the old one. A reader without the lock
// thus sees the whole old file or the whole new one. os.CreateTemp makes it
// 0600, which the rename carries over: the file can hold passwords.
func writeSaved(path string, list []SavedConn) error {
	var buf bytes.Buffer
	buf.WriteString(savedHeader)
	if len(list) > 0 {
		if err := toml.NewEncoder(&buf).Encode(savedDoc{Connections: list}); err != nil {
			return serr.Wrap(err, "op", "encode saved connections")
		}
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return serr.Wrap(err, "path", path, "op", "create temp")
	}
	// removes the temp file on any failure below; after the rename it names
	// nothing, and the error is ignored
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err = tmp.Write(buf.Bytes()); err != nil {
		_ = tmp.Close()
		return serr.Wrap(err, "path", tmp.Name(), "op", "write")
	}
	if err = tmp.Sync(); err != nil {
		_ = tmp.Close()
		return serr.Wrap(err, "path", tmp.Name(), "op", "sync")
	}
	if err = tmp.Close(); err != nil {
		return serr.Wrap(err, "path", tmp.Name(), "op", "close")
	}
	if err = os.Rename(tmp.Name(), path); err != nil {
		return serr.Wrap(err, "path", path, "op", "rename")
	}
	return nil
}
