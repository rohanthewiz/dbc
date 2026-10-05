package userdata

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/rohanthewiz/serr"
)

// Layout is the terminal UI's pane geometry as the user left it: the
// sizes dragged at the pane borders and whether the sidebar was folded away
// (^B). dbc web keeps the same things in its store's layout; the TUI has no
// store, so, like the schema picks, they get a small file of their own.
//
// Zero means "never set": New keeps its default for a zero field, so a file
// from an older dbc (or one missing a field) costs only that field.
type Layout struct {
	SideW      int     `json:"side_w,omitempty"`
	ChatW      int     `json:"chat_w,omitempty"`
	LogH       int     `json:"log_h,omitempty"`
	EdFrac     float64 `json:"ed_frac,omitempty"`
	SideHidden bool    `json:"side_hidden,omitempty"`

	// Tabs are the query tabs, in strip order, and ActiveTab the index of
	// the one on screen. Empty from an older dbc: one tab, as before.
	Tabs      []LayoutTab `json:"tabs,omitempty"`
	ActiveTab int         `json:"active_tab,omitempty"`
}

// LayoutTab is one saved query tab: what it is called, the connection it
// was on, and the console it showed (a name within that connection's
// database's consoles). The console's text is in its file, not here.
type LayoutTab struct {
	Title   string `json:"title"`
	Conn    string `json:"conn,omitempty"`
	Console string `json:"console,omitempty"`
}

// LayoutFile is where the TUI's layout persists, beside the schema picks.
// It returns "" when no home directory is known, and the layout lasts only
// for the run.
func LayoutFile() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "dbc", "tui-layout.json")
}

// LoadLayout returns the saved layout. A missing or unreadable file is the
// zero Layout: the defaults, which is all losing it costs.
func LoadLayout(path string) Layout {
	var l Layout
	if path == "" {
		return l
	}
	if bs, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(bs, &l)
	}
	return l
}

// SaveLayout writes the layout, through a temp file renamed over the old
// one so a crash mid-write leaves the previous layout rather than a
// truncated file. Two TUIs quitting at once: the last one's layout wins,
// which is the one the user saw last.
func SaveLayout(path string, l Layout) error {
	if path == "" {
		return nil
	}
	bs, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return serr.Wrap(err, "op", "encode layout")
	}
	if err = os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return serr.Wrap(err, "op", "save layout")
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tui-layout-*")
	if err != nil {
		return serr.Wrap(err, "op", "save layout")
	}
	_, err = tmp.Write(append(bs, '\n'))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
		return serr.Wrap(err, "op", "save layout", "path", path)
	}
	return nil
}
