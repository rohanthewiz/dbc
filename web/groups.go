package web

import (
	"encoding/json"
	"slices"
	"strings"

	"github.com/rohanthewiz/dbc/config"
)

// Tab groups: a short named chip in the query-tab strip with its member
// tabs gathered behind it, underlined in the group's colour, and foldable to
// the chip alone — ced's tab groups (internal/app/tabgroups.go), on the
// browser's strip. The page owns them (static/js/tabgroups.js); the server
// keeps them in the layout and only does what the page cannot: merge one
// window's write with the groups other windows hold, and follow a renamed
// connection.
//
//	[≡ prod ][Query 1 ×][Query 3 ×] [wip +2][Query 4 ×] [+]
//	 └chip──┴── underlined ────────┘ └ collapsed: 2 members hidden
//
// Two kinds, one record, as in ced:
//
//   - a CONNECTION group is a rule — every tab on its connection, or on one
//     of that connection's other databases ("prod/analytics", the derived
//     connections the sidebar's database picker opens), belongs, including
//     tabs that switch to it later. It is ced's folder group with the
//     connection for the folder: where a tab "lives" in dbc. Exclude lists
//     the tabs taken out by hand, which the rule would otherwise pull
//     straight back in.
//   - an AD-HOC group is a hand-picked set of tabs (Members).
//
// Membership is by saved-tab key — the page's tab objects do not survive a
// reload, the keys do — and is never stored on the tab row: a connection
// group's membership is computed, and one place to read groups from keeps
// the strip and the store from disagreeing.
//
// Why the layout and not a table: the layout is already where the strip's
// order ("tabs") and the plan flags ("plans") live, written by the same
// saveLayout, and the groups are one small value rewritten whole, so a
// closed tab or an ungrouped group simply drops out of the next write (the
// layout has no delete).
//
// THE MERGE. Each window knows only the tabs it claimed (claims.go), so its
// write of "groups" would drop the members other windows hold — the same
// problem mergeTabKeys solves for "tabs". mergeTabGroups puts back the old
// value's member and exclude keys another live window holds: into the
// writer's group of the same name when it has one, else as the old group
// with only those keys. Keys held by nobody go with the writer's word, so a
// closed tab cannot linger. A connection group none of whose tabs another
// window holds is the writer's to delete — the honest limit of a merge by
// keys: a group made in another window after this one booted, and holding
// no tab of that window's, is dropped by this window's next write.

// groupsKey is the layout key the page keeps its groups under (app.js).
const groupsKey = "groups"

// tabGroupRec is one group as the layout stores it; the page's encode and
// decode (tabgroups.js) are the other side of this shape.
type tabGroupRec struct {
	Name string `json:"name"`
	// Conn is a connection group's connection; "" for an ad-hoc group.
	Conn      string   `json:"conn,omitempty"`
	Members   []string `json:"members,omitempty"` // ad-hoc: saved-tab keys
	Exclude   []string `json:"exclude,omitempty"` // connection: keys taken out
	Collapsed bool     `json:"collapsed,omitempty"`
	// Color indexes the page's group hues (--g0 … in app.css); it is the
	// page's, kept so a group keeps its colour across reloads.
	Color int `json:"color"`
}

// parseGroups reads a "groups" value. "" is no groups; anything that is not
// the page's shape is an error the callers fall back from.
func parseGroups(v string) ([]tabGroupRec, error) {
	if strings.TrimSpace(v) == "" {
		return nil, nil
	}
	var gs []tabGroupRec
	if err := json.Unmarshal([]byte(v), &gs); err != nil {
		return nil, err
	}
	return gs, nil
}

func encodeGroups(gs []tabGroupRec) string {
	if len(gs) == 0 {
		return "[]"
	}
	b, err := json.Marshal(gs)
	if err != nil { // plain strings, ints and bools: cannot fail
		return "[]"
	}
	return string(b)
}

// mergeTabGroups is a layout write's "groups" value with the member and
// exclude keys another live window holds put back (see THE MERGE above).
// A value that does not parse — given or old — is passed through as given:
// the page's word stands rather than the write failing over a value only
// the page reads.
func mergeTabGroups(given, old string, elsewhere map[string]bool) string {
	if len(elsewhere) == 0 {
		return given
	}
	ng, err := parseGroups(given)
	if err != nil {
		return given
	}
	og, err := parseGroups(old)
	if err != nil || len(og) == 0 {
		return given
	}
	held := func(keys []string) []string {
		var out []string
		for _, k := range keys {
			if elsewhere[k] {
				out = append(out, k)
			}
		}
		return out
	}
	changed := false
	for _, o := range og {
		members, exclude := held(o.Members), held(o.Exclude)
		if len(members) == 0 && len(exclude) == 0 {
			continue
		}
		changed = true
		// names are unique case-insensitively (the page refuses a clash),
		// so a name finds at most one group
		i := slices.IndexFunc(ng, func(g tabGroupRec) bool { return strings.EqualFold(g.Name, o.Name) })
		if i < 0 {
			o.Members, o.Exclude = members, exclude
			ng = append(ng, o)
			continue
		}
		// One group per tab: a key the writer placed in another group of
		// its own is not one of its tabs (it is held elsewhere), so this
		// only fills in what the writer could not see.
		for _, k := range members {
			if !slices.Contains(ng[i].Members, k) {
				ng[i].Members = append(ng[i].Members, k)
			}
		}
		for _, k := range exclude {
			if !slices.Contains(ng[i].Exclude, k) {
				ng[i].Exclude = append(ng[i].Exclude, k)
			}
		}
	}
	if !changed {
		return given
	}
	return encodeGroups(ng)
}

// moveGroupConns moves the connection groups on connection from — and on
// its other databases ("<from>/analytics") — to to's names, as
// moveSchemaPicks moves the schema picks: the page moves the groups its
// window holds (connRenamed), but a window reads the layout only at boot,
// so groups saved by a window that has since closed are in no window to be
// moved. Both landing is harmless: they write the same names.
func (s *Server) moveGroupConns(from, to string) error {
	saved, err := s.store.Layout()
	if err != nil {
		return err
	}
	gs, err := parseGroups(saved[groupsKey])
	if err != nil || len(gs) == 0 {
		return nil // nothing the page would read back as groups
	}
	moved := false
	for i, g := range gs {
		switch {
		case g.Conn == "":
		case g.Conn == from:
			gs[i].Conn, moved = to, true
		case derivedFrom(s.cfg, g.Conn, from):
			gs[i].Conn = config.DerivedName(to, g.Conn[len(from)+len(config.DatabaseSep):])
			moved = true
		}
	}
	if !moved {
		return nil
	}
	return s.store.SetLayout(map[string]string{groupsKey: encodeGroups(gs)})
}
