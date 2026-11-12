package config

import (
	"fmt"
	"slices"
	"strings"
)

// Introspection: how much of a database dbc reads UNASKED — on a connect,
// a refresh, the relist after a run's DDL, and for the editor's completion
// — as opposed to what the user asks for by name (a schema pick, a diagram,
// the row counts and routines switches, which stay their own switches).
//
// It is a setting because the right amount is the database's, not dbc's:
// a scratch database is read whole in a blink, and that is worth it for a
// sidebar and completions that know everything; a production catalog of
// millions of pg_class rows spends seconds — and its DBA's goodwill — on a
// count nobody asked for, every time a tab connects.
//
//	level   the sidebar reads                       completion reads
//	─────   ─────────────────────────────────────   ──────────────────────────
//	full    databases, schemas WITH table counts,   every schema's tables and
//	        the open schema's tables                columns
//	names   databases, schema NAMES (no counts,     the open schema's and the
//	        so no "all schemas"), the open          search_path's only
//	        schema's tables
//	off     nothing until a Refresh, which then     nothing until that Refresh
//	        reads as names does                     (the dialect's words); then
//	                                                as names does
//
// names differs from full only where there are schemas to count and to
// scope by (Postgres); on the other engines the two read the same.
//
// The key is written globally (introspect = "names", the default for every
// connection) and per connection (in a [[connection]] table, or the
// connection forms), the connection's own winning: IntrospectFor.
const (
	IntrospectFull  = "full"
	IntrospectNames = "names"
	IntrospectOff   = "off"
)

// IntrospectLevels lists the levels from the most read to the least, for
// messages and the connection forms.
var IntrospectLevels = []string{IntrospectFull, IntrospectNames, IntrospectOff}

// ParseIntrospect normalizes an introspect value as written: trimmed, any
// case. "" is valid and stays "" — "not set", which a connection reads as
// "the global default" and the global key as IntrospectFull. ok is false
// for anything that is not a level, and the value then comes back as typed
// (trimmed), for the caller's message.
func ParseIntrospect(s string) (level string, ok bool) {
	v := strings.ToLower(strings.TrimSpace(s))
	if v == "" || slices.Contains(IntrospectLevels, v) {
		return v, true
	}
	return strings.TrimSpace(s), false
}

// checkIntrospect normalizes the global introspect key and each
// connection's. A value that is no level falls back — the global key to
// full, a connection's to the global default — with a warning, rather than
// failing the load, as a misspelled plan_theme does: the setting decides
// how much is read, never whether a connection works, and the level it
// falls back to is the one dbc always read at.
//
// It runs over whatever Connections holds when called (Load's, before the
// saved ones are merged: MergeSaved checks its own).
func (c *Config) checkIntrospect() {
	lvl, ok := ParseIntrospect(c.Introspect)
	if !ok {
		c.Warnings = append(c.Warnings, fmt.Sprintf(
			"introspect = %q is not a level (use %s); using %s",
			lvl, quotedLevels(), IntrospectFull))
		lvl = ""
	}
	if lvl == "" {
		lvl = IntrospectFull
	}
	c.Introspect = lvl
	for i := range c.Connections {
		cn := &c.Connections[i]
		l, ok := ParseIntrospect(cn.Introspect)
		if !ok {
			c.Warnings = append(c.Warnings, fmt.Sprintf(
				"connection %q: introspect = %q is not a level (use %s); using the default (%s)",
				cn.Name, l, quotedLevels(), c.Introspect))
			l = ""
		}
		cn.Introspect = l
	}
}

// quotedLevels is the levels as a message lists them: "full", "names" or "off".
func quotedLevels() string {
	q := make([]string, len(IntrospectLevels))
	for i, l := range IntrospectLevels {
		q[i] = `"` + l + `"`
	}
	return strings.Join(q[:len(q)-1], ", ") + " or " + q[len(q)-1]
}

// IntrospectDefault is the level a connection that sets none reads at: the
// global introspect key, or IntrospectFull for a Config that never went
// through Load (a test's, an ad-hoc one) and so left it empty.
func (c *Config) IntrospectDefault() string {
	if c == nil {
		return IntrospectFull
	}
	if lvl, ok := ParseIntrospect(c.Introspect); ok && lvl != "" {
		return lvl
	}
	return IntrospectFull
}

// IntrospectFor is the level the connection named reads at: its own
// introspect when it sets one, else the global default. A connection
// derived onto another of its server's databases ("<conn>/<database>",
// ConnByName) reads at its base's — the copy carries the field — which is
// what a user who set it on a production server means. An unknown name
// gets the default.
//
// It is read from the config as it stands, so a level changed in a
// connection form takes effect at the connection's next read, with no
// reconnect.
func (c *Config) IntrospectFor(name string) string {
	if c == nil {
		return IntrospectFull
	}
	if cn, ok := c.ConnByName(name); ok {
		if lvl, ok := ParseIntrospect(cn.Introspect); ok && lvl != "" {
			return lvl
		}
	}
	return c.IntrospectDefault()
}
