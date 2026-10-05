package userdata

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Consoles: one running SQL file per database, rather than one editor
// buffer for everything.
//
// A single buffer.sql meant the queries written against one database sat in
// the editor while connected to another, and were lost the moment the
// scratchpad was cleared for the next. A console is that scratchpad, kept per
// (host, database) — what a connection lands on (db.Target), not its name —
// in a directory of plain .sql files:
//
//	~/.config/dbc/consoles/
//	    localhost_5432--app-3f9c1a2b.sql
//	    db.example.com_5432--analytics-77d0e4c1.sql
//	    local--scratch.db-0b5e9f12.sql
//
// The name is readable, so the files can be found and opened by hand, and
// ends in a hash of the exact (host, database) pair, because making the
// readable part safe for a filename folds characters together (":" and "/"
// both become "_") and could otherwise put two databases in one file.
//
// buffer.sql is not deleted: it seeds the first console opened when that
// console has no file yet (see the TUI), so an upgrade keeps the scratchpad.

// ConsolesDir is where the consoles live, beside the history and the legacy
// buffer. It returns "" when no home directory is known, and consoles then
// do not persist.
func ConsolesDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "dbc", "consoles")
}

// ConsoleFile is the console of database on host, in dir. It returns "" when
// dir is "" (persistence off). host may be "" (a connection whose DSN could
// not be read, kept by name — see db.ConsoleTarget).
func ConsoleFile(dir, host, database string) string {
	if dir == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(host + "\x00" + database))
	// SQLite and bytdb databases are absolute file paths; the base name is
	// the readable part, the hash keeps two scratch.db's apart.
	readable := consoleSlug(host) + "--" + consoleSlug(filepath.Base(database))
	return filepath.Join(dir, readable+"-"+hex.EncodeToString(sum[:4])+".sql")
}

// consoleSlug keeps the characters every filesystem takes, turning the rest
// into '_', and bounds the length: the hash, not this, is what is unique.
func consoleSlug(s string) string {
	const maxSlug = 48
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
		if b.Len() >= maxSlug {
			break
		}
	}
	return strings.Trim(b.String(), ".") // no hidden files, no ".."
}

// LoadConsole returns a console's text and whether it has a file at all. The
// difference matters: a console emptied on purpose stays empty, where one
// never written may be seeded (from the legacy buffer.sql).
func LoadConsole(path string) (text string, exists bool) {
	if path == "" {
		return "", false
	}
	bs, err := os.ReadFile(path)
	if err != nil {
		return "", !errors.Is(err, fs.ErrNotExist)
	}
	return string(bs), true
}

// SaveConsole writes a console, user-only like the buffer it replaces. A
// console with nothing in it and no file yet is not written: looking at a
// database must not leave an empty file behind for it.
func SaveConsole(path, text string) error {
	if path == "" {
		return nil
	}
	if text == "" {
		if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
			return nil
		}
	}
	return SaveBuffer(path, text)
}
