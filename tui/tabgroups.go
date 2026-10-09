package tui

import (
	"fmt"
	"slices"
	"strings"
	"unicode"

	tea "charm.land/bubbletea/v2"

	"github.com/rohanthewiz/dbc/theme"
	"github.com/rohanthewiz/dbc/userdata"
)

// Tab groups: a short named chip in the query-tab strip with its member
// tabs gathered behind it and underlined in the group's colour, and
// foldable to the chip alone — dbc web's groups (web/static/js/tabgroups.js)
// on the TUI's strip, which in turn are ced's (internal/app/tabgroups.go).
//
//	╭─ prod ─Query 1─Query 3─  wip +2 ─Query 4─ Query 2 ─[lite]─ + ───╮
//	   └chip┴── underlined ──┘ └ folded: 2      ungrouped  └ idle: a
//	                             members hidden               connection
//	                                                          group, no tab
//
// The model is the web's, carried over decision for decision; what is said
// here is only where the TUI differs or where the reason is not obvious
// from the code. The web module's header has the full set.
//
//   - Two kinds, one struct. A CONNECTION group is a rule: every tab on its
//     connection — or on one of its other databases ("prod/analytics") —
//     belongs, including a tab that switches onto it later. An AD-HOC group
//     is a hand-picked set. conn tells them apart ("" = ad-hoc).
//   - One group per tab, resolved, never stored on the tab (groupOf):
//     ad-hoc membership first (the explicit gesture), then the DEEPEST
//     connection group that has not excluded the tab.
//   - Membership is by tab KEY, not *queryTab: keys are what tabMsg already
//     uses for a tab's identity, and a closed tab's key is never reused, so
//     a stale key can match nothing. On disk the key becomes the tab's index
//     in the saved strip (userdata.LayoutTabGroup), since keys are minted
//     fresh each run.
//   - Groups are contiguous because m.tabs is REORDERED (arrangeTabs, after
//     every Update), not because the strip draws in another order — so ⌥1…9,
//     the saved order and what is drawn always agree. A tab whose connect
//     lands on a grouped connection moves in beside its group on that same
//     message.
//   - A connection group with no tab keeps a hollow chip, [name], after the
//     tabs: it is a rule waiting for a tab, and switching its last tab away
//     must not hide that it still claims the next one. A click on it opens a
//     tab on its connection; right-click (or ⌥G) reaches Ungroup.
//   - Collapse hides members, never the active tab: ⌥N or a menu row onto a
//     member of a folded group shows that one tab, so the editor never
//     shows text without its tab in the strip.
//   - Where a new tab lands is never a guess: the strip's + wears the colour
//     of the group ⌥T would put the tab in, and hovering it says so (the
//     terminal's stand-in for the web's tooltip, drawn after the strip);
//     right-click on + — or a group's "New tab in <name>" — opens a tab in
//     any group, on that group's connection (connFor).
//   - Every mouse gesture has a keyboard door: ⌥G opens the groups menu (the
//     tab on screen's add / remove rows, then every group's own menu). A
//     right-click is not always delivered (macOS Terminal under tmux), and
//     the strip is not drawn with a single tab and no groups at all.
//   - Names are letters and digits, unique case-insensitively, at most
//     eight — the web's limit rather than ced's four: the TUI's strip holds
//     at most nine tabs (maxTabs), against ced's every open file.

// groupNameMax is the longest group name, in runes (see the header).
const groupNameMax = 8

// tabGroup is one live group.
type tabGroup struct {
	name string
	// conn is a connection group's connection; "" for an ad-hoc group.
	conn string
	// members is an ad-hoc group's membership, by tab key. Unused by
	// connection groups, whose membership is computed.
	members map[int]bool
	// exclude lists tabs taken out of a CONNECTION group by hand — the rule
	// would otherwise pull them straight back in on the next message.
	exclude   map[int]bool
	collapsed bool
	// color indexes groupHues; assigned at creation so it stays put while
	// other groups come and go.
	color int
}

func (g *tabGroup) isConn() bool { return g.conn != "" }

// groupHues are the six colours groups cycle through, per background: the
// palette has only three hues (accent, warn, err) — too few to tell groups
// apart — so these are dbc web's --g0…--g5 (app.css), light hues for a dark
// background and deep ones for a light, each readable with the editor's
// background as the chip's text.
var groupHues = [2][6]string{
	{"#6ea8fe", "#63c174", "#e0b44c", "#ef7f7c", "#b48ef0", "#4fc1c9"}, // dark background
	{"#2f6fd0", "#2e8a58", "#9a6400", "#c53d3d", "#7a4fc4", "#13808a"}, // light background
}

// groupColor is g's colour under the palette in use. Read at paint time,
// never cached, so a cats theme change restyles the chips with everything
// else.
func (m *Model) groupColor(g *tabGroup) Color {
	set := 0
	if lightBg(m.st.pal.Bg) {
		set = 1
	}
	n := len(groupHues[set])
	return hex(groupHues[set][((g.color%n)+n)%n])
}

// lightBg reports whether a palette background is a light one, by its
// perceived brightness (Rec. 601 weights). An unparseable value counts as
// dark, the TUI's own default.
func lightBg(bg string) bool {
	r, g, b, ok := theme.ParseHex(bg)
	if !ok {
		return false
	}
	return 299*int(r)+587*int(g)+114*int(b) > 128*1000
}

// nextGroupColor is the first colour no live group uses, so the first six
// groups are all distinct; after that the cycle repeats.
func (m *Model) nextGroupColor() int {
	used := map[int]bool{}
	for _, g := range m.groups {
		used[g.color] = true
	}
	for c := range len(groupHues[0]) {
		if !used[c] {
			return c
		}
	}
	return len(m.groups) % len(groupHues[0])
}

// groupLive reports whether g is still one of the groups — a menu row
// captured g, and it may have been ungrouped (or emptied) while the menu
// was open.
func (m *Model) groupLive(g *tabGroup) bool { return g != nil && slices.Contains(m.groups, g) }

// ---------------------------------------------------------------------------
// Membership
// ---------------------------------------------------------------------------

// tabIndex is t's place in the strip, -1 once it is closed.
func (m *Model) tabIndex(t *queryTab) int { return slices.Index(m.tabs, t) }

// tabConn is the connection t is on for grouping: the one a connect in
// flight is dialing, else tabTarget's (a restored tab's lazy one, none for
// a tab never connected). The connect in flight comes first so a tab joins
// the group it is headed for at once — a tab just opened into a connection
// group would otherwise read as on the workspace's default until its
// connect lands, and arrangeTabs would move it out and back.
func (m *Model) tabConn(t *queryTab) string {
	i := m.tabIndex(t)
	if i < 0 {
		return ""
	}
	conn, ws := m.tabTarget(i)
	if ws != nil && t.lazy == "" {
		if c, ok := ws.Connecting(); ok && c != "" {
			return c
		}
	}
	return conn
}

// connUnder reports whether conn is base or one of base's other databases
// ("prod/analytics" for "prod"), the way tabOn reckons a tab on a pool.
func (m *Model) connUnder(conn, base string) bool {
	return conn != "" && (conn == base || m.baseOf(conn) == base)
}

// groupOf is the group t belongs to, or nil.
func (m *Model) groupOf(t *queryTab) *tabGroup {
	if t == nil || len(m.groups) == 0 {
		return nil
	}
	return m.groupFor(t.key, m.tabConn(t))
}

// groupFor resolves a tab by its key and connection: ad-hoc membership
// first, then the deepest connection group claiming conn that has not
// excluded key. It is groupOf without a tab, so a tab not yet made (the +
// chip's landing) can be asked about with key 0, which no tab has.
func (m *Model) groupFor(key int, conn string) *tabGroup {
	for _, g := range m.groups {
		if !g.isConn() && g.members[key] {
			return g
		}
	}
	if conn == "" {
		return nil
	}
	var best *tabGroup
	for _, g := range m.groups {
		if !g.isConn() || g.exclude[key] || !m.connUnder(conn, g.conn) {
			continue
		}
		// deepest wins: "prod/analytics" is the more specific claim
		if best == nil || len(g.conn) > len(best.conn) {
			best = g
		}
	}
	return best
}

// groupMembers is g's open tabs, in strip order.
func (m *Model) groupMembers(g *tabGroup) []*queryTab {
	var out []*queryTab
	for _, t := range m.tabs {
		if m.groupOf(t) == g {
			out = append(out, t)
		}
	}
	return out
}

// arrangeTabs reorders m.tabs so each group's members sit together at its
// first member's place; ungrouped tabs keep their places relative to each
// other. Stable and idempotent, and a no-op without groups, so Update runs
// it after every message (see the header). The tab on screen is followed by
// pointer: its fields are the Model's, and only its index moves.
//
//	before   A(g)  B  C(g)  D(h)  E(g)
//	after    A(g) C(g) E(g)  B   D(h)      g gathered at A's place
func (m *Model) arrangeTabs() {
	if len(m.groups) == 0 || len(m.tabs) < 2 {
		return
	}
	cur := m.active()
	of := make([]*tabGroup, len(m.tabs))
	for i, t := range m.tabs {
		of[i] = m.groupOf(t)
	}
	out := make([]*queryTab, 0, len(m.tabs))
	placed := map[*tabGroup]bool{}
	for i, t := range m.tabs {
		g := of[i]
		if g == nil {
			out = append(out, t)
			continue
		}
		if placed[g] {
			continue
		}
		placed[g] = true
		for j := i; j < len(m.tabs); j++ {
			if of[j] == g {
				out = append(out, m.tabs[j])
			}
		}
	}
	m.tabs = out
	m.curTab = m.tabIndex(cur)
}

// forgetTab drops a closing tab from every group, and an ad-hoc group with
// it when that was its last member — an empty ad-hoc group has no chip to
// draw and nothing to manage. Connection groups stay: they are rules, and
// the next tab on the connection joins.
func (m *Model) forgetTab(t *queryTab) {
	for _, g := range m.groups {
		delete(g.members, t.key)
		delete(g.exclude, t.key)
	}
	m.dropEmptyGroups()
}

// dropEmptyGroups removes ad-hoc groups left with no members.
func (m *Model) dropEmptyGroups() {
	m.groups = slices.DeleteFunc(m.groups, func(g *tabGroup) bool { return !g.isConn() && len(g.members) == 0 })
}

// newTabConn is the connection a plain new tab (⌥T, a click on +) starts on:
// the tab on screen's, as newTab has always taken it.
func (m *Model) newTabConn() string { return m.ws.Active() }

// landing is the group a plain new tab joins: the tab on screen's ad-hoc
// group (joinNewTab), else whichever connection group claims the
// connection it opens on. The probe is key 0 — the new tab's key is fresh,
// so no exclusion can name it, even when the tab on screen was itself taken
// out of that connection group.
func (m *Model) landing() *tabGroup {
	if g := m.groupOf(m.active()); g != nil && !g.isConn() {
		return g
	}
	return m.groupFor(0, m.newTabConn())
}

// connFor is the connection a tab opened INTO g starts on: a connection
// group's own; for an ad-hoc group, the tab on screen's when it is a
// member, else its last member's (the newest place the group was working);
// fallback when the group has no tab here to ask.
func (m *Model) connFor(g *tabGroup, fallback string) string {
	if g.isConn() {
		return g.conn
	}
	ms := m.groupMembers(g)
	if slices.Contains(ms, m.active()) {
		if c := m.tabConn(m.active()); c != "" {
			return c
		}
		return fallback
	}
	if len(ms) > 0 {
		if c := m.tabConn(ms[len(ms)-1]); c != "" {
			return c
		}
	}
	return fallback
}

// orderedGroups is the groups in strip order — chip by chip, left to right
// — then the idle ones, so a menu listing them reads like the strip.
func (m *Model) orderedGroups() []*tabGroup {
	var out []*tabGroup
	for _, t := range m.tabs {
		if g := m.groupOf(t); g != nil && !slices.Contains(out, g) {
			out = append(out, g)
		}
	}
	for _, g := range m.groups {
		if !slices.Contains(out, g) {
			out = append(out, g)
		}
	}
	return out
}

// idleGroups is the connection groups no open tab belongs to, in the order
// they were made. Their chips go after the tabs: an idle group has no
// member to sit in front of, and a fixed place at the end leaves the
// grouped tabs' order — ⌥1…9 — untouched.
func (m *Model) idleGroups() []*tabGroup {
	used := map[*tabGroup]bool{}
	for _, t := range m.tabs {
		used[m.groupOf(t)] = true
	}
	var out []*tabGroup
	for _, g := range m.groups {
		if g.isConn() && !used[g] {
			out = append(out, g)
		}
	}
	return out
}

// groupHiddenCount is how many of g's tabs its fold hides: all but the
// active tab.
func (m *Model) groupHiddenCount(g *tabGroup) int {
	n := 0
	for i, t := range m.tabs {
		if i != m.curTab && m.groupOf(t) == g {
			n++
		}
	}
	return n
}

// tabHidden reports whether tab i is folded away: in a collapsed group and
// not the tab on screen.
func (m *Model) tabHidden(i int) bool {
	if i == m.curTab || len(m.groups) == 0 {
		return false
	}
	g := m.groupOf(m.tabs[i])
	return g != nil && g.collapsed
}

// groupChipText is the chip drawn in front of tab i — the first of its
// group — and the group, or ok false when tab i carries none. Folded, the
// chip counts what it hides: the count is what tells a folded group apart
// from a group of one.
func (m *Model) groupChipText(i int) (string, *tabGroup, bool) {
	g := m.groupOf(m.tabs[i])
	if g == nil || (i > 0 && m.groupOf(m.tabs[i-1]) == g) {
		return "", nil, false
	}
	if g.collapsed {
		if n := m.groupHiddenCount(g); n > 0 {
			return fmt.Sprintf(" %s +%d ", g.name, n), g, true
		}
	}
	return " " + g.name + " ", g, true
}

// describeGroup is a group's one-line summary for menu heads and the
// strip's hover hint.
func (m *Model) describeGroup(g *tabGroup) string {
	n := plural(len(m.groupMembers(g)), "tab")
	if g.isConn() {
		return "every tab on " + g.conn + " · " + n
	}
	return "ad-hoc · " + n
}

// ---------------------------------------------------------------------------
// Names
// ---------------------------------------------------------------------------

// groupByName finds a group by name, case-insensitively, skipping except.
func (m *Model) groupByName(name string, except *tabGroup) *tabGroup {
	for _, g := range m.groups {
		if g != except && strings.EqualFold(g.name, name) {
			return g
		}
	}
	return nil
}

// groupNameProblem is why name can't be used (except by the group being
// renamed, which may keep its own), or nil when it can.
func (m *Model) groupNameProblem(name string, except *tabGroup) error {
	runes := []rune(name)
	switch {
	case len(runes) == 0:
		return fmt.Errorf("a group needs a name")
	case len(runes) > groupNameMax:
		return fmt.Errorf("group names are at most %d letters (%s has %d)", groupNameMax, name, len(runes))
	}
	for _, r := range runes {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			return fmt.Errorf("group names are letters and digits only")
		}
	}
	if o := m.groupByName(name, except); o != nil {
		return fmt.Errorf("there is already a group named %s", o.name)
	}
	return nil
}

// groupNameFrom suggests a name from s: its first letters and digits, so
// "reporting" → "reportin" and "lite-2" → "lite2".
func groupNameFrom(s string) string {
	var b []rune
	for _, r := range s {
		if len(b) == groupNameMax {
			break
		}
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b = append(b, r)
		}
	}
	return string(b)
}

// uniqueGroupName is base, or base cut short plus a digit when base is
// taken ("grp" → "grp2"), so the name prompt's seed is usable as offered.
func (m *Model) uniqueGroupName(base string) string {
	if base == "" || m.groupByName(base, nil) == nil {
		return base
	}
	stem := []rune(base)
	if len(stem) > groupNameMax-1 {
		stem = stem[:groupNameMax-1]
	}
	for d := 2; d <= 9; d++ {
		if cand := fmt.Sprintf("%s%d", string(stem), d); m.groupByName(cand, nil) == nil {
			return cand
		}
	}
	return base
}

// ---------------------------------------------------------------------------
// Mutations
// ---------------------------------------------------------------------------

// newGroup appends a group with the next free colour; conn is "" for an
// ad-hoc group.
func (m *Model) newGroup(name, conn string) *tabGroup {
	g := &tabGroup{name: name, conn: conn, members: map[int]bool{}, exclude: map[int]bool{}, color: m.nextGroupColor()}
	m.groups = append(m.groups, g)
	return g
}

// addToGroup makes g the group t resolves to. t leaves any ad-hoc group
// first (one group per tab). For a connection group, t is let back in if it
// was excluded, and every DEEPER connection group that would claim it
// excludes it, so the pick actually wins; shallower ones already lose to g
// on depth, and an exclusion there would outlive g.
func (m *Model) addToGroup(t *queryTab, g *tabGroup) {
	if t == nil || !m.groupLive(g) {
		return
	}
	for _, o := range m.groups {
		if !o.isConn() && o != g {
			delete(o.members, t.key)
		}
	}
	if g.isConn() {
		delete(g.exclude, t.key)
		conn := m.tabConn(t)
		for _, o := range m.groups {
			if o != g && o.isConn() && len(o.conn) > len(g.conn) && m.connUnder(conn, o.conn) {
				o.exclude[t.key] = true
			}
		}
	} else {
		g.members[t.key] = true
	}
	m.dropEmptyGroups()
}

// removeFromGroup takes t out of the group it shows in: an ad-hoc
// membership is dropped, a connection rule gets an exclusion. A shallower
// connection group that also holds t then takes it — the gesture was
// about the visible group.
func (m *Model) removeFromGroup(t *queryTab) {
	g := m.groupOf(t)
	if g == nil {
		return
	}
	if g.isConn() {
		g.exclude[t.key] = true
	} else {
		delete(g.members, t.key)
	}
	m.dropEmptyGroups()
	m.logf(logInfo, "removed %s from group %s", t.title, g.name)
}

// toggleGroup folds or unfolds g.
func (m *Model) toggleGroup(g *tabGroup) {
	if m.groupLive(g) {
		g.collapsed = !g.collapsed
	}
}

// ungroup deletes g; its tabs stay open, where they are.
func (m *Model) ungroup(g *tabGroup) {
	if !m.groupLive(g) {
		return
	}
	m.groups = slices.DeleteFunc(m.groups, func(x *tabGroup) bool { return x == g })
	m.logf(logInfo, "ungrouped %s", g.name)
}

// makeGroupAdhoc turns a connection group into an ad-hoc one holding its
// current members, so tabs on other connections can join and tabs that
// switch onto the connection later no longer do.
func (m *Model) makeGroupAdhoc(g *tabGroup) {
	if !m.groupLive(g) || !g.isConn() {
		return
	}
	ms := m.groupMembers(g)
	g.conn = ""
	g.exclude = map[int]bool{}
	for _, t := range ms {
		g.members[t.key] = true
	}
	m.dropEmptyGroups()
	m.logf(logInfo, "%s is now an ad-hoc group (%s)", g.name, plural(len(ms), "tab"))
}

// tabStateful reports whether tab i's session may hold a transaction.
func (m *Model) tabStateful(i int) bool {
	ws := m.tabs[i].ws
	if i == m.curTab {
		ws = m.ws
	}
	if ws == nil {
		return false
	}
	_, stateful := ws.Session()
	return stateful
}

// closeGroupTabs closes g's tabs — but not one whose session may hold a
// transaction (one pick must not roll back work the per-tab close would
// have asked about), and never the last tab. The tabs are captured first
// and found again per close, since each close shifts the indices after it.
// closeTab brings each tab on screen to close it; the tab that was on
// screen before comes back after, when it was not one of them.
func (m *Model) closeGroupTabs(g *tabGroup) tea.Cmd {
	if !m.groupLive(g) {
		return nil
	}
	was := m.active()
	ms := m.groupMembers(g)
	// the tab on screen last: closing it activates a neighbour, which
	// should not be one about to close too
	slices.SortStableFunc(ms, func(a, b *queryTab) int {
		switch {
		case a == was && b != was:
			return 1
		case b == was && a != was:
			return -1
		}
		return 0
	})
	var cmds []tea.Cmd
	kept := 0
	for _, t := range ms {
		i := m.tabIndex(t)
		if i < 0 {
			continue
		}
		if len(m.tabs) == 1 || m.tabStateful(i) {
			kept++
			continue
		}
		cmds = append(cmds, m.closeTab(i, true))
	}
	if i := m.tabIndex(was); i >= 0 && i != m.curTab {
		cmds = append(cmds, m.activate(i))
	}
	if kept > 0 {
		m.logf(logWarn, "kept %s — its session may hold a transaction, or it is the last tab", plural(kept, "tab"))
	}
	return tea.Batch(cmds...)
}

// renameGroupConns moves connection groups after a connection rename;
// rename is connRenamed's mapping, which carries "<old>/<db>" too.
func (m *Model) renameGroupConns(rename func(string) (string, bool)) {
	for _, g := range m.groups {
		if !g.isConn() {
			continue
		}
		if to, ok := rename(g.conn); ok {
			g.conn = to
		}
	}
}

// joinNewTab puts a tab just opened from from into from's ad-hoc group, as a
// browser's tab group takes a tab opened from one of its own. A connection
// group needs no help: the new tab is on the same connection.
func (m *Model) joinNewTab(from, t *queryTab) {
	if g := m.groupOf(from); g != nil && !g.isConn() {
		g.members[t.key] = true
	}
}

// ---------------------------------------------------------------------------
// Menus and the name prompt
// ---------------------------------------------------------------------------

// groupMenuItems are the group menu's rows, a fixed list so the hand learns
// the positions: rows that don't apply say why rather than vanish (the
// menus' rule). The member tabs follow as rows of their own — the way into
// a folded group without unfolding it.
func (m *Model) groupMenuItems(g *tabGroup) []menuItem {
	ms := m.groupMembers(g)
	live := func(f func(m *Model) tea.Cmd) func(*Model) tea.Cmd {
		return func(m *Model) tea.Cmd {
			if !m.groupLive(g) {
				return nil
			}
			return f(m)
		}
	}
	none := ""
	if len(ms) == 0 {
		none = g.name + " has no open tabs"
	}
	fold, foldWhy := "Collapse group", none
	if g.collapsed {
		// a fold an idle group still carries can always be undone, so the
		// row never strands that state
		fold, foldWhy = "Expand group", ""
	}
	adhocWhy := ""
	if !g.isConn() {
		adhocWhy = g.name + " is already ad-hoc"
	}
	items := []menuItem{
		heading(g.name + " — " + m.describeGroup(g)),
		{label: "New tab in " + g.name, act: live(func(m *Model) tea.Cmd { return m.newTabIn(g) })},
		{label: fold, why: foldWhy, act: live(func(m *Model) tea.Cmd { m.toggleGroup(g); return nil })},
		{label: "Rename group…", act: live(func(m *Model) tea.Cmd { m.promptGroupName(nil, g, ""); return nil })},
		{label: "Make ad-hoc group", why: adhocWhy, act: live(func(m *Model) tea.Cmd { m.makeGroupAdhoc(g); return nil })},
		{label: "Close group's tabs", why: none, act: live(func(m *Model) tea.Cmd { return m.closeGroupTabs(g) })},
		{label: "Ungroup", act: live(func(m *Model) tea.Cmd { m.ungroup(g); return nil })},
	}
	if len(ms) > 0 {
		items = append(items, heading("tabs"))
		for _, t := range ms {
			label := "  " + t.title
			if t == m.active() {
				label = "● " + t.title
			}
			items = append(items, menuItem{label: label, act: func(m *Model) tea.Cmd {
				if i := m.tabIndex(t); i >= 0 {
					return m.activate(i)
				}
				return nil
			}})
		}
	}
	return items
}

// openGroupMenu opens g's menu at (x, y).
func (m *Model) openGroupMenu(g *tabGroup, x, y int) {
	if m.groupLive(g) {
		m.openMenu(x, y, m.groupMenuItems(g))
	}
}

// tabGroupItems are a tab's group rows, for its right-click menu and ⌥G:
// add, and — when it is in a group — remove and the group's own menu.
func (m *Model) tabGroupItems(t *queryTab, x, y int) []menuItem {
	items := []menuItem{
		heading("group"),
		{label: "Add to group…", act: func(m *Model) tea.Cmd { m.openAddMenu(t, x, y); return nil }},
	}
	if g := m.groupOf(t); g != nil {
		items = append(items,
			menuItem{label: "Remove from group " + g.name, act: func(m *Model) tea.Cmd {
				if m.tabIndex(t) >= 0 {
					m.removeFromGroup(t)
				}
				return nil
			}},
			menuItem{label: "Group " + g.name + "…", act: func(m *Model) tea.Cmd { m.openGroupMenu(g, x, y); return nil }},
		)
	}
	return items
}

// openAddMenu lists the groups t could join, then the two ways to start one:
// an ad-hoc group, or a connection group for t's connection (offered when
// the tab is on one and no group is that connection's already).
func (m *Model) openAddMenu(t *queryTab, x, y int) {
	if m.tabIndex(t) < 0 {
		return
	}
	cur := m.groupOf(t)
	conn := m.tabConn(t)
	head := "add " + t.title + " to"
	if cur != nil {
		head += " (now in " + cur.name + ")"
	}
	items := []menuItem{heading(head)}
	for _, g := range m.orderedGroups() {
		if g == cur {
			continue
		}
		// a connection group can only take a tab on its connection
		if g.isConn() && !m.connUnder(conn, g.conn) {
			continue
		}
		items = append(items, menuItem{label: g.name + "  · " + m.describeGroup(g), act: func(m *Model) tea.Cmd {
			if m.tabIndex(t) < 0 || !m.groupLive(g) {
				return nil
			}
			m.addToGroup(t, g)
			m.logf(logInfo, "added %s to %s", t.title, g.name)
			return nil
		}})
	}
	items = append(items, menuItem{label: "New ad-hoc group…", act: func(m *Model) tea.Cmd {
		m.promptGroupName(t, nil, "")
		return nil
	}})
	taken := slices.ContainsFunc(m.groups, func(g *tabGroup) bool { return g.conn == conn })
	if conn != "" && !taken {
		items = append(items, menuItem{label: "New connection group: " + conn, act: func(m *Model) tea.Cmd {
			m.promptGroupName(t, nil, conn)
			return nil
		}})
	}
	m.openMenu(x, y, items)
}

// openNewTabMenu is the + chip's right-click menu: a new tab in any group,
// listed as the strip shows them, the one a plain click would pick marked
// ●; then the plain click itself, for when no group is wanted to steer it.
func (m *Model) openNewTabMenu(x, y int) {
	land := m.landing()
	items := []menuItem{heading("new query tab in")}
	for _, g := range m.orderedGroups() {
		label := "  " + g.name
		if g == land {
			label = "● " + g.name
		}
		items = append(items, menuItem{label: label + "  · " + m.describeGroup(g), act: func(m *Model) tea.Cmd {
			if !m.groupLive(g) {
				return nil
			}
			return m.newTabIn(g)
		}})
	}
	if len(m.groups) == 0 {
		items = append(items, menuItem{label: "no groups yet", why: "no groups yet — right-click a tab (or ⌥G) → Add to group… starts one"})
	}
	beside := "Beside " + m.active().title
	if land != nil {
		beside += " (" + land.name + ")"
	}
	items = append(items, heading(""), menuItem{label: beside, key: "⌥T", act: func(m *Model) tea.Cmd { return m.newTab() }})
	m.openMenu(x, y, items)
}

// openGroupsMenu (⌥G) is the keyboard door to every group gesture: the tab
// on screen's group rows, then each group's own menu, in strip order.
func (m *Model) openGroupsMenu() {
	x, y := m.lay.editor.X+2, m.lay.editor.Y+1
	items := m.tabGroupItems(m.active(), x, y)
	items[0] = heading("tab groups · " + m.active().title)
	if len(m.groups) > 0 {
		items = append(items, heading("groups"))
		for _, g := range m.orderedGroups() {
			items = append(items, menuItem{label: g.name + "…  · " + m.describeGroup(g), act: func(m *Model) tea.Cmd {
				m.openGroupMenu(g, x, y)
				return nil
			}})
		}
	}
	m.openMenu(x, y, items)
}

// promptGroupName asks for a name: a new group's (t starts it; conn is ""
// for an ad-hoc group) or, with g, a rename. A refused name is said under
// the field and what was typed stays, so a nine-letter attempt costs one
// Backspace rather than starting over.
func (m *Model) promptGroupName(t *queryTab, g *tabGroup, conn string) {
	if g != nil {
		m.openPrompt("Rename group "+g.name, m.describeGroup(g), "Rename", g.name,
			func(m *Model, text string) error {
				name := strings.TrimSpace(text)
				if !m.groupLive(g) {
					return nil
				}
				if err := m.groupNameProblem(name, g); err != nil {
					return err
				}
				g.name = name
				return nil
			})
		return
	}
	title, hint, seed := "New tab group", fmt.Sprintf("ad-hoc — %s starts it · up to %d letters or digits", t.title, groupNameMax), m.uniqueGroupName("grp")
	if conn != "" {
		title = "New connection group"
		hint = fmt.Sprintf("every tab on %s joins it · up to %d letters or digits", conn, groupNameMax)
		// the database's own name ("prod/analytics" → "analytic")
		seed = m.uniqueGroupName(groupNameFrom(conn[strings.LastIndex(conn, "/")+1:]))
	}
	m.openPrompt(title, hint, "Create", seed, func(m *Model, text string) error {
		name := strings.TrimSpace(text)
		if err := m.groupNameProblem(name, nil); err != nil {
			return err
		}
		if m.tabIndex(t) < 0 {
			return nil // closed while the prompt was up
		}
		ng := m.newGroup(name, conn)
		m.addToGroup(t, ng)
		if conn != "" {
			m.logf(logInfo, "group %s: %s on %s", name, plural(len(m.groupMembers(ng)), "tab"), conn)
		} else {
			m.logf(logInfo, "group %s: added %s", name, t.title)
		}
		return nil
	})
}

// ---------------------------------------------------------------------------
// Saving and restoring
// ---------------------------------------------------------------------------

// savedGroups is the groups as the layout file keeps them, with members by
// their index in savedTabs (see userdata.LayoutTabGroup).
func (m *Model) savedGroups() []userdata.LayoutTabGroup {
	at := map[int]int{} // key → index
	for i, t := range m.tabs {
		at[t.key] = i
	}
	idx := func(keys map[int]bool) []int {
		var out []int
		for k := range keys {
			if i, ok := at[k]; ok {
				out = append(out, i)
			}
		}
		slices.Sort(out) // a stable file, whatever the map's order
		return out
	}
	out := make([]userdata.LayoutTabGroup, 0, len(m.groups))
	for _, g := range m.groups {
		out = append(out, userdata.LayoutTabGroup{Name: g.name, Conn: g.conn, Members: idx(g.members),
			Exclude: idx(g.exclude), Collapsed: g.collapsed, Color: g.color})
	}
	return out
}

// restoreGroups rebuilds the groups the last run saved, against the tabs
// restoreTabs rebuilt: an index with no tab is dropped, and an ad-hoc group
// none of whose tabs came back with it. A name the prompt would refuse
// (a hand-edited file) is skipped — groups are a convenience, never a
// reason for the TUI not to start.
func (m *Model) restoreGroups(saved []userdata.LayoutTabGroup) {
	m.groups = nil
	for _, sg := range saved {
		if m.groupNameProblem(sg.Name, nil) != nil {
			continue
		}
		g := &tabGroup{name: sg.Name, conn: sg.Conn, members: map[int]bool{}, exclude: map[int]bool{},
			collapsed: sg.Collapsed, color: sg.Color}
		keys := func(ix []int, into map[int]bool) {
			for _, i := range ix {
				if i >= 0 && i < len(m.tabs) {
					into[m.tabs[i].key] = true
				}
			}
		}
		keys(sg.Members, g.members)
		keys(sg.Exclude, g.exclude)
		if !g.isConn() && len(g.members) == 0 {
			continue
		}
		m.groups = append(m.groups, g)
	}
	m.arrangeTabs()
}
