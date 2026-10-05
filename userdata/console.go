package userdata

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/rohanthewiz/serr"
)

// Consoles: running SQL files kept per database, as many as the user wants
// for each, rather than one editor buffer for everything.
//
// A single buffer.sql meant the queries written against one database sat in
// the editor while connected to another, and were lost the moment the
// scratchpad was cleared for the next. A console is that scratchpad, kept
// per database — what a connection lands on (db.Target), not its name — and
// a database may have several (one per line of work, or one per query tab
// in dbc web). They are namespaced host first, then database, then name,
// as plain .sql files:
//
//	~/.config/dbc/consoles/
//	    localhost_5432/                  ◄─ the HOST (and port)
//	        app/                         ◄─ the DATABASE on it
//	            console.sql              ◄─ a console: its NAME is "console"
//	            console-2.sql
//	            reports.sql              ◄─ renamed by the user
//	        analytics/
//	            console.sql
//	    db.example.com_5432/
//	        app/                         ◄─ another server's "app": its own
//	            console.sql
//	    local/                           ◄─ SQLite and bytdb: no server
//	        scratch.db-0b5e9f12/         ◄─ a file, by base name + path hash
//	            console.sql
//
// The directory names are the host and database as written wherever that is
// safe and unambiguous ("host:port" only loses its colon), so the files can
// be found by hand. A name that had to be changed to make it a file name
// (a socket path, an IPv6 address, a database with a space or a slash, an
// embedded database's full path) gets a hash of the original appended, so two
// names that fold to the same characters stay two directories.
//
// The first dbc with consoles kept one file per database, flat in the
// consoles directory; ListConsoles moves such a file in as DefaultConsole
// the first time it looks at that database.
//
// buffer.sql is not deleted: the TUI seeds the first console it opens from
// it, once, so an upgrade keeps the scratchpad.

// DefaultConsole is the name of a database's first console.
const DefaultConsole = "console"

// consoleNameRe is what a console name (and each directory level) may be:
// it is a file name, so nothing that could climb out of the directory, hide
// (a leading dot) or trip up another filesystem.
var consoleNameRe = regexp.MustCompile(`^[A-Za-z0-9_-][A-Za-z0-9._-]{0,63}$`)

// ValidConsoleName reports whether name may name a console, or be one level
// of a ConsoleDB: all three come from the browser as path segments and
// become file names.
func ValidConsoleName(name string) bool {
	return consoleNameRe.MatchString(name) && !strings.HasSuffix(name, ".")
}

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

// ConsoleDB names one database's directory of consoles: Host, then
// Database, each one directory level (see the layout above). Both are
// directory names, not the host and database as the driver knows them —
// ConsoleDBOf makes one from those.
type ConsoleDB struct {
	Host     string `json:"host"`
	Database string `json:"database"`
}

// hostUnknown is the Host level of a connection whose DSN could not be read
// (db.ConsoleTarget gives it host ""), whose consoles are kept by
// connection name. A real host always carries a port, so no host's
// directory is ever named this.
const hostUnknown = "unknown"

// plainHostRe is a host that needs no hash: a name or IPv4 address, with
// an optional port. Only the colon changes on the way to a file name.
var plainHostRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]*(:[0-9]+)?$`)

// ConsoleDBOf is the directory of database on host. host may be "" (a
// connection whose DSN could not be read — see db.ConsoleTarget).
func ConsoleDBOf(host, database string) ConsoleDB {
	d := ConsoleDB{Host: hostUnknown}
	switch {
	case host == "":
	case plainHostRe.MatchString(host):
		d.Host = strings.Replace(host, ":", "_", 1)
	default:
		d.Host = consoleSlug(host) + "-" + shortHash(host)
	}
	if ValidConsoleName(database) && filepath.Base(database) == database {
		d.Database = database
	} else {
		// SQLite and bytdb databases are absolute file paths; the base
		// name is the readable part, the hash keeps two scratch.db's apart
		d.Database = consoleSlug(filepath.Base(database)) + "-" + shortHash(database)
	}
	return d
}

// Valid reports whether both levels are names ConsoleDBOf could have made
// — the check on a ConsoleDB that came from the browser.
func (d ConsoleDB) Valid() bool {
	return ValidConsoleName(d.Host) && ValidConsoleName(d.Database)
}

// dir is d's directory under root.
func (d ConsoleDB) dir(root string) string { return filepath.Join(root, d.Host, d.Database) }

// shortHash is 8 hex digits of s's SHA-256: enough to keep apart the few
// names that slug alike, short enough to leave a path readable.
func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:4])
}

// consoleSlug keeps the characters every filesystem takes, turning the rest
// into '_', and bounds the length: the hash, not this, is what is unique.
func consoleSlug(s string) string {
	const maxSlug = 40
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
	if out := strings.Trim(b.String(), "."); out != "" { // no hidden files, no ".."
		return out
	}
	return "_"
}

// ConsolePath is the file of console name of database d under root: ""
// when root is "" (persistence off) or any level is not a valid name.
func ConsolePath(root string, d ConsoleDB, name string) string {
	if root == "" || !d.Valid() || !ValidConsoleName(name) {
		return ""
	}
	return filepath.Join(d.dir(root), name+".sql")
}

// ListConsoles is the names of database d's consoles under root, in the
// order they are offered: DefaultConsole first, then "console-2",
// "console-3", … by number, then any other name alphabetically. A database
// with none lists none: a console only gets a file once it holds something
// (SaveConsole).
//
// flat is the database's file in the first consoles layout (one file per
// database: FlatConsoleFile), "" when there is none to look for; such a file
// is moved in as DefaultConsole here.
func ListConsoles(root string, d ConsoleDB, flat string) []string {
	if root == "" || !d.Valid() {
		return nil
	}
	adoptFlat(root, d, flat)
	ents, err := os.ReadDir(d.dir(root))
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range ents {
		n, ok := strings.CutSuffix(e.Name(), ".sql")
		if ok && e.Type().IsRegular() && ValidConsoleName(n) {
			names = append(names, n)
		}
	}
	slices.SortFunc(names, compareConsoles)
	return names
}

// compareConsoles orders console names: see ListConsoles.
func compareConsoles(a, b string) int {
	na, nb := consoleNumber(a), consoleNumber(b)
	switch {
	case na > 0 && nb > 0:
		return na - nb
	case na > 0:
		return -1
	case nb > 0:
		return 1
	}
	return strings.Compare(a, b)
}

// consoleNumber is 1 for DefaultConsole, n for "console-n" (n ≥ 2), and 0
// for any other name.
func consoleNumber(name string) int {
	if name == DefaultConsole {
		return 1
	}
	s, ok := strings.CutPrefix(name, DefaultConsole+"-")
	if !ok {
		return 0
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 2 || strconv.Itoa(n) != s {
		return 0
	}
	return n
}

// NextConsoleName is the name a new console gets, given the names already
// taken: DefaultConsole if free, else the lowest free "console-n".
func NextConsoleName(taken []string) string {
	if !slices.Contains(taken, DefaultConsole) {
		return DefaultConsole
	}
	for n := 2; ; n++ {
		name := DefaultConsole + "-" + strconv.Itoa(n)
		if !slices.Contains(taken, name) {
			return name
		}
	}
}

// FlatConsoleFile is where the first consoles layout kept database on
// host's one console, directly under root — for ListConsoles to move in.
func FlatConsoleFile(root, host, database string) string {
	if root == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(host + "\x00" + database))
	return filepath.Join(root, consoleSlug(host)+"--"+consoleSlug(filepath.Base(database))+
		"-"+hex.EncodeToString(sum[:4])+".sql")
}

// adoptFlat moves a first-layout console file into d's directory as
// DefaultConsole — unless that console already exists, when the old file is
// left alone rather than overwrite either.
func adoptFlat(root string, d ConsoleDB, flat string) {
	if flat == "" {
		return
	}
	if _, err := os.Stat(flat); err != nil {
		return
	}
	dst := ConsolePath(root, d, DefaultConsole)
	if _, err := os.Stat(dst); err == nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return
	}
	_ = os.Rename(flat, dst)
}

// ConsoleRev is the revision of a console's text: a hash, so any writer —
// the TUI, another dbc web window, an editor outside dbc — changing the file
// changes it, with no counter to keep in step. A console with no file is
// revision "".
func ConsoleRev(text string, exists bool) string {
	if !exists {
		return ""
	}
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:8])
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
// database, or opening a console and leaving it empty, must not leave a
// file behind.
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

// ErrConsoleExists is a rename onto a name the database already has.
var ErrConsoleExists = errors.New("a console by that name already exists")

// RenameConsole renames a console of database d. A console that was never
// written has no file, and renaming it is only a change of name: nothing to
// move, and success.
func RenameConsole(root string, d ConsoleDB, from, to string) error {
	src, dst := ConsolePath(root, d, from), ConsolePath(root, d, to)
	if src == "" || dst == "" {
		return serr.New("not a console name", "from", from, "to", to)
	}
	if _, err := os.Stat(dst); err == nil {
		return serr.Wrap(ErrConsoleExists, "name", to)
	}
	if err := os.Rename(src, dst); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return serr.Wrap(err, "from", from, "to", to)
	}
	return nil
}

// DeleteConsole removes a console of database d. One with no file is
// already gone, which is success.
func DeleteConsole(root string, d ConsoleDB, name string) error {
	path := ConsolePath(root, d, name)
	if path == "" {
		return serr.New("not a console name", "name", name)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return serr.Wrap(err, "name", name)
	}
	return nil
}
