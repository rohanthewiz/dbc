package userdata

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/rohanthewiz/serr"
)

// SchemaPick is the schema the terminal UI's Tables pane was last on for one
// connection: Name, or every schema (All). It mirrors workspace.SchemaPick
// rather than importing it, so this package stays below workspace.
//
// dbc web keeps its picks in its own store's layout ("tableSchema.<conn>");
// the TUI has no store, so its picks get a file of their own here.
type SchemaPick struct {
	Name string `json:"name,omitempty"`
	All  bool   `json:"all,omitempty"`
}

// PicksFile is where the TUI's schema picks persist, beside the buffer.
// It returns "" when no home directory is known, and picks last only for
// the run.
func PicksFile() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "dbc", "schema-picks.json")
}

// LoadPicks returns the saved picks, keyed by connection name ("<conn>" or
// a derived "<conn>/<database>"). A missing or unreadable file is no picks:
// losing them only means a connection opens on its default schema.
func LoadPicks(path string) map[string]SchemaPick {
	out := map[string]SchemaPick{}
	if path == "" {
		return out
	}
	bs, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	_ = json.Unmarshal(bs, &out)
	if out == nil { // the file held "null"
		out = map[string]SchemaPick{}
	}
	return out
}

// SavePick records one connection's pick in the file, keeping every other
// connection's.
//
// It re-reads the file rather than writing the caller's map: two dbc TUIs
// can be open at once, each picking on its own connections, and writing a
// whole map loaded at startup would undo the other's picks since then. The
// write goes to a temp file renamed over the old one, so a crash mid-write
// leaves the old picks rather than a truncated file. (Two saves in the same
// instant can still lose one of them; a pick is a click, and the cost is a
// connection opening on its default schema once.)
//
// The file is never pruned: an entry is a few bytes, and a connection that
// is gone from one config may be in another (-config).
func SavePick(path, conn string, pick SchemaPick) error {
	if path == "" {
		return nil
	}
	all := LoadPicks(path)
	all[conn] = pick
	return writePicks(path, all)
}

// MovePicks renames the picks rename maps to a new name (ok true), keeping
// the rest — a connection renamed in the TUI's form takes its pick, and
// those of its other databases ("<old>/analytics"), with it. Like SavePick
// it re-reads the file, so another TUI's picks since startup are kept.
func MovePicks(path string, rename func(conn string) (to string, ok bool)) error {
	if path == "" {
		return nil
	}
	all := LoadPicks(path)
	moved := false
	for conn, p := range all {
		if to, ok := rename(conn); ok && to != conn {
			delete(all, conn)
			all[to] = p
			moved = true
		}
	}
	if !moved {
		return nil
	}
	return writePicks(path, all)
}

// writePicks replaces the file with all, through a temp file renamed over
// it (see SavePick).
func writePicks(path string, all map[string]SchemaPick) error {
	bs, err := json.MarshalIndent(all, "", "  ")
	if err != nil {
		return serr.Wrap(err, "op", "encode schema picks")
	}
	// user-only, as the file always was (it came from CreateTemp)
	if err = writeAtomic(path, append(bs, '\n'), 0o600); err != nil {
		return serr.Wrap(err, "op", "save schema picks", "path", path)
	}
	return nil
}
