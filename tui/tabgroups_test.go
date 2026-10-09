package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/rohanthewiz/dbc/config"
	"github.com/rohanthewiz/dbc/userdata"
)

// pickRow runs the open menu's row whose label holds label, by moving the
// cursor there and pressing Enter — through Update, as a real pick is, so
// the pass that keeps groups together (arrangeTabs) runs after it.
func pickRow(t *testing.T, m *Model, label string) {
	t.Helper()
	if m.menu == nil {
		t.Fatalf("no menu open to pick %q from", label)
	}
	for i, it := range m.menu.items {
		if !it.head && strings.Contains(it.label, label) {
			m.menu.cur = i
			key(t, m, "enter")
			return
		}
	}
	t.Fatalf("no menu row %q", label)
}

// titles is the strip's tabs in order, by title.
func titles(m *Model) string {
	var out []string
	for _, t := range m.tabs {
		out = append(out, t.title)
	}
	return strings.Join(out, ",")
}

// stripLine is the editor's top border, where the strip is drawn.
func stripLine(m *Model) string { return frame(m).Line(m.lay.editor.Y) }

// groupedModel is consoleModel with three tabs: Query 1 on a, Query 2 on
// the default, Query 3 on the default, Query 3 on screen.
func groupedModel(t *testing.T) *Model {
	t.Helper()
	m, _ := consoleModel(t)
	drive(t, m, nil, m.setActive("a"))
	key(t, m, "alt+t")
	drive(t, m, nil, m.setActive(config.DemoSQLite))
	key(t, m, "alt+t")
	if titles(m) != "Query 1,Query 2,Query 3" || m.curTab != 2 {
		t.Fatalf("setup: tabs %s, cur %d", titles(m), m.curTab)
	}
	return m
}

// A connection group is a rule: made from a tab's menu, it claims every tab
// on its connection, and a tab that switches onto that connection later
// joins — moved in beside the group on the same message, so ⌥N and the
// strip agree.
func TestTabGroupConnectionRule(t *testing.T) {
	m := groupedModel(t)
	key(t, m, "alt+1")
	key(t, m, "alt+g")
	pickRow(t, m, "Add to group…")
	pickRow(t, m, "New connection group: a")
	if _, ok := m.modal.(*promptModal); !ok {
		t.Fatalf("no name prompt (modal %T)", m.modal)
	}
	key(t, m, "enter") // the seed, "a", as offered
	if len(m.groups) != 1 || m.groups[0].name != "a" || m.groups[0].conn != "a" {
		t.Fatalf("groups = %+v", m.groups)
	}
	if line := stripLine(m); !strings.Contains(line, " a ─ Query 1") {
		t.Fatalf("strip = %q, want the chip in front of Query 1", line)
	}

	key(t, m, "alt+3")
	drive(t, m, nil, m.setActive("a"))
	if titles(m) != "Query 1,Query 3,Query 2" || m.active().title != "Query 3" {
		t.Fatalf("after Query 3 moved onto a: tabs %s, on screen %s", titles(m), m.active().title)
	}
	if m.groupOf(m.active()) != m.groups[0] {
		t.Fatal("Query 3 on a is not in the connection group")
	}

	// taken out by hand, it stays out though the rule would claim it
	key(t, m, "alt+g")
	pickRow(t, m, "Remove from group a")
	if m.groupOf(m.active()) != nil || !m.groups[0].exclude[m.active().key] {
		t.Fatal("removed from a connection group, but the rule still claims it")
	}
	// and back in through Add to group
	key(t, m, "alt+g")
	pickRow(t, m, "Add to group…")
	pickRow(t, m, "a  · every tab on a")
	if m.groupOf(m.active()) != m.groups[0] {
		t.Fatal("added back to a, but not in it")
	}
}

// An ad-hoc group is a hand-picked set: a tab opened from one of its tabs
// joins it, the strip gathers its members, and it goes away with its last
// tab.
func TestTabGroupAdhoc(t *testing.T) {
	m := groupedModel(t)
	key(t, m, "alt+1")
	key(t, m, "alt+g")
	pickRow(t, m, "Add to group…")
	pickRow(t, m, "New ad-hoc group…")
	typeText(t, m, "wip")
	key(t, m, "enter")
	g := m.groupByName("wip", nil)
	if g == nil || g.isConn() || !g.members[m.tabs[0].key] {
		t.Fatalf("groups = %+v", m.groups)
	}

	key(t, m, "alt+t") // from Query 1, in wip: Query 4 joins wip
	if m.groupOf(m.active()) != g {
		t.Fatal("a tab opened from an ad-hoc group's tab did not join it")
	}
	if titles(m) != "Query 1,Query 4,Query 2,Query 3" {
		t.Fatalf("tabs %s, want Query 4 gathered beside Query 1", titles(m))
	}
	if line := stripLine(m); !strings.Contains(line, " wip ─ Query 1 ─ Query 4") {
		t.Fatalf("strip = %q", line)
	}
	if lbl := m.st.base.Bg; m.lay.tabChips[0].kind != stripGroup || frame(m).StyleAt(m.lay.tabChips[0].r.X+1, m.lay.tabChips[0].r.Y).Fg != lbl {
		t.Error("the group's chip is not drawn filled in its colour with the background as its text")
	}

	key(t, m, "alt+w") // Query 4
	key(t, m, "alt+1")
	key(t, m, "alt+w") // Query 1, its last member
	if len(m.groups) != 0 {
		t.Fatalf("an ad-hoc group outlived its last tab: %+v", m.groups)
	}
}

// Folding a group hides its tabs behind the chip, which counts them — but
// never the tab on screen: ⌥N onto a folded member shows that one tab.
func TestTabGroupCollapse(t *testing.T) {
	m := groupedModel(t)
	key(t, m, "alt+2")
	g := m.newGroup("demo", config.DemoSQLite)
	key(t, m, "alt+1") // Query 2 and Query 3 are demo's; Query 1 is on screen
	if titles(m) != "Query 1,Query 2,Query 3" || m.groupOf(m.tabs[2]) != g {
		t.Fatalf("tabs %s", titles(m))
	}
	x, y := findText(t, frame(m), " demo ")
	click(t, m, x+1, y)
	if !g.collapsed {
		t.Fatal("a click on the chip did not fold the group")
	}
	line := stripLine(m)
	if !strings.Contains(line, " demo +2 ") || strings.Contains(line, "Query 2") || strings.Contains(line, "Query 3") {
		t.Fatalf("folded strip = %q, want the chip counting 2 and no members", line)
	}

	key(t, m, "alt+3")
	line = stripLine(m)
	if !strings.Contains(line, " demo +1 ") || !strings.Contains(line, "Query 3") || strings.Contains(line, "Query 2") {
		t.Fatalf("on a folded member, strip = %q", line)
	}

	// the group's menu reaches a folded member without unfolding
	x, y = findText(t, frame(m), " demo +1 ")
	rightClick(t, m, x+1, y)
	pickRow(t, m, "  Query 2")
	if m.active().title != "Query 2" || !g.collapsed {
		t.Fatalf("menu row: on %s, folded %v", m.active().title, g.collapsed)
	}
	pickGroupMenu(t, m, g, "Expand group")
	if g.collapsed {
		t.Fatal("Expand group left it folded")
	}
}

// pickGroupMenu opens g's menu and runs the row holding label.
func pickGroupMenu(t *testing.T, m *Model, g *tabGroup, label string) {
	t.Helper()
	m.openGroupMenu(g, 2, 2)
	pickRow(t, m, label)
}

// A connection group whose last tab moves away keeps a hollow chip after
// the tabs; a click on it opens a tab on its connection, in the group.
func TestTabGroupIdleChip(t *testing.T) {
	m := groupedModel(t)
	g := m.newGroup("lite", "a")
	key(t, m, "alt+1")
	drive(t, m, nil, m.setActive("b"))
	if line := stripLine(m); !strings.Contains(line, "[lite]") {
		t.Fatalf("strip = %q, want the idle chip", line)
	}
	if len(m.idleGroups()) != 1 {
		t.Fatalf("idle groups = %d", len(m.idleGroups()))
	}
	x, y := findText(t, frame(m), "[lite]")
	click(t, m, x+1, y)
	if len(m.tabs) != 4 || m.ws.Active() != "a" || m.groupOf(m.active()) != g {
		t.Fatalf("after the idle chip: %d tabs, on %q, group %v", len(m.tabs), m.ws.Active(), m.groupOf(m.active()))
	}
	if strings.Contains(stripLine(m), "[lite]") {
		t.Error("the chip stayed hollow with a tab in its group")
	}

	// a connection group whose connection is gone opens nothing
	g.conn = "gone"
	n := len(m.tabs)
	drive(t, m, nil, m.newTabIn(g))
	if len(m.tabs) != n || !strings.Contains(logText(m), "no longer a connection") {
		t.Fatalf("a tab opened for a removed connection: %d tabs; log:\n%s", len(m.tabs), logText(m))
	}
}

// The + says where a new tab lands: in the landing group's colour, with a
// hover hint naming it; its right-click opens a tab in any group, on that
// group's connection.
func TestTabGroupPlus(t *testing.T) {
	m := groupedModel(t)
	g := m.newGroup("lite", "a")
	key(t, m, "alt+2") // on the default: + lands in no group
	if m.landing() != nil {
		t.Fatalf("landing = %v on an ungrouped connection", m.landing().name)
	}
	key(t, m, "alt+1") // on a: + lands in lite
	if m.landing() != g {
		t.Fatal("+ from a tab on a does not land in lite")
	}
	x, y := findText(t, frame(m), " + ")
	if st := frame(m).StyleAt(x+1, y); st.Fg != m.groupColor(g) {
		t.Errorf("+ is not in lite's colour: %+v", st)
	}
	drive(t, m, tea.MouseMotionMsg{X: x + 1, Y: y})
	if line := stripLine(m); !strings.Contains(line, "new tab in lite on a") {
		t.Errorf("hovering +: strip = %q", line)
	}

	key(t, m, "alt+2") // back on the default, ask for lite by hand
	x, y = findText(t, frame(m), " + ")
	rightClick(t, m, x+1, y)
	pickRow(t, m, "lite")
	if m.ws.Active() != "a" || m.groupOf(m.active()) != g {
		t.Fatalf("new tab in lite: on %q, group %v", m.ws.Active(), m.groupOf(m.active()))
	}
}

// Deepest wins: a tab on prod/analytics belongs to the prod/analytics group,
// not prod's; adding it to prod by hand excludes it from the deeper one so
// the pick holds.
func TestTabGroupDeepestWins(t *testing.T) {
	m, _ := consoleModel(t)
	m.cfg.Connections = append(m.cfg.Connections,
		config.Connection{Name: "pg", Driver: "postgres", DSN: "postgres://localhost/x"})
	m.restoreTabs([]userdata.LayoutTab{{Title: "one"}, {Title: "two"}}, 0)
	m.tabs[1].lazy = "pg/analytics"
	pg, an := m.newGroup("pg", "pg"), m.newGroup("an", "pg/analytics")
	if m.groupOf(m.tabs[1]) != an {
		t.Fatalf("a tab on pg/analytics resolved to %v", m.groupOf(m.tabs[1]))
	}
	m.addToGroup(m.tabs[1], pg)
	if m.groupOf(m.tabs[1]) != pg || !an.exclude[m.tabs[1].key] {
		t.Fatal("adding to the shallower group did not win over the deeper one")
	}
}

// Groups are saved with the tabs — members by their place in the saved
// strip — and come back on the restored tabs; a renamed connection takes
// its group along.
func TestTabGroupsRestore(t *testing.T) {
	m := groupedModel(t)
	key(t, m, "alt+2")
	wip := m.newGroup("wip", "")
	m.addToGroup(m.active(), wip)
	lite := m.newGroup("lite", "a")
	lite.collapsed = true
	saved, groups := m.savedTabs(), m.savedGroups()
	if len(groups) != 2 || groups[0].Members[0] != 1 {
		t.Fatalf("saved groups = %+v", groups)
	}

	m2, _ := consoleModel(t)
	m2.cfg.Connections = m.cfg.Connections
	m2.restoreTabs(saved, 0)
	m2.restoreGroups(groups)
	if len(m2.groups) != 2 {
		t.Fatalf("restored groups = %+v", m2.groups)
	}
	w2, l2 := m2.groupByName("wip", nil), m2.groupByName("lite", nil)
	if m2.groupOf(m2.tabs[1]) != w2 || m2.groupOf(m2.tabs[0]) != l2 || !l2.collapsed || l2.color != lite.color {
		t.Fatalf("restored: tab 1 in %v, tab 0 in %v", m2.groupOf(m2.tabs[1]), m2.groupOf(m2.tabs[0]))
	}

	// a saved ad-hoc group none of whose tabs came back is dropped, and a
	// name the prompt would refuse is skipped
	m2.restoreGroups([]userdata.LayoutTabGroup{{Name: "gone", Members: []int{7}}, {Name: "bad name", Conn: "a"}})
	if len(m2.groups) != 0 {
		t.Fatalf("restored %+v", m2.groups)
	}

	m.connRenamed("a", "a2")
	if lite.conn != "a2" {
		t.Errorf("after renaming a, lite is on %q", lite.conn)
	}
}

// Names are letters and digits, at most eight, unique case-insensitively;
// the prompt says why and keeps what was typed.
func TestTabGroupNames(t *testing.T) {
	m := groupedModel(t)
	m.newGroup("wip", "")
	for _, c := range []struct{ name, why string }{
		{"", "needs a name"}, {"toolongname", "at most 8"}, {"a-b", "letters and digits"}, {"WIP", "already a group named wip"},
	} {
		err := m.groupNameProblem(c.name, nil)
		if err == nil || !strings.Contains(err.Error(), c.why) {
			t.Errorf("%q: %v, want %q", c.name, err, c.why)
		}
	}
	if m.uniqueGroupName("wip") != "wip2" || groupNameFrom("lite-2/reporting") != "lite2rep" {
		t.Errorf("unique %q, from %q", m.uniqueGroupName("wip"), groupNameFrom("lite-2/reporting"))
	}

	m.promptGroupName(m.active(), nil, "")
	typeText(t, m, "toolongname")
	key(t, m, "enter")
	p, ok := m.modal.(*promptModal)
	if !ok || !strings.Contains(p.errMsg, "at most 8") || p.field.Text() != "toolongname" {
		t.Fatalf("a refused name: modal %T", m.modal)
	}
}

// Close group's tabs closes its tabs, but not one whose session may hold a
// transaction, and the tab that was on screen comes back.
func TestTabGroupCloseTabs(t *testing.T) {
	m := groupedModel(t)
	g := m.newGroup("demo", config.DemoSQLite) // Query 2 and Query 3
	m.editor.SetText("BEGIN")                  // Query 3, on screen
	key(t, m, "ctrl+r")
	key(t, m, "alt+1")
	pickGroupMenu(t, m, g, "Close group's tabs")
	if titles(m) != "Query 1,Query 3" || m.active().title != "Query 1" {
		t.Fatalf("tabs %s, on screen %s", titles(m), m.active().title)
	}
	if !strings.Contains(logText(m), "kept 1 tab") {
		t.Errorf("log:\n%s", logText(m))
	}
}
