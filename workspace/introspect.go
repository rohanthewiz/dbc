package workspace

import (
	"github.com/rohanthewiz/dbc/config"
	"github.com/rohanthewiz/dbc/db"
)

// Levels of introspection: how much of a database the workspace reads
// UNASKED (config/introspect.go has the table of what each level reads,
// and why it is a setting). This file settles which level a read runs at;
// connect.go and complete.go do the reading.
//
// THE EFFECTIVE LEVEL is the configured one — the connection's introspect,
// else the global default — lowered from full to names once the
// connection's schema summary has run out its budget (db.Manager.SummarySlow):
//
//	configured     summary slow before?     read as
//	──────────     ────────────────────     ───────
//	off            (never asked)            off
//	names          (never asked)            names
//	full           no                       full
//	full           yes                      names   ← the adaptive default
//
// The lowering is what makes full a safe default: a database too big to
// count pays for finding that out once per run of dbc, not once per
// connect, refresh and relist — and nobody has to edit a config to stop
// it. It lowers the whole connection, completion included, rather than
// only the summary: a pg_class too big to count in five seconds is one
// whose every-schema read for completion is the next thing to time out.
//
// It lasts as long as the process and nothing retries it — not a Refresh
// either, which on such a database is pressed to see a new table, not to
// wait five seconds for counts that will not come. What it costs is the
// picker's counts and its "all schemas" entry; a restart, or an edit of
// what the connection dials (db.Manager.Drop), tries the summary again.
//
// OFF IS A STATE AS WELL AS A LEVEL. A connect at off lands with nothing
// read and nothing reading (Workspace.unread), which is neither "loading"
// nor "failed", and a UI says so (CatalogUnread). The user's Refresh is
// the one read that ends it; from then on the connection is read as names
// is, until the next connect:
//
//	connect (off) ─► unread: sidebar empty, completion = the dialect's words
//	      │
//	      └─ Refresh ─► read as names ─► listed; completion scoped
//	                    (a failed or canceled Refresh leaves it unread)

// level is an effective level of introspection, ordered from least to most
// so that "at most names" is a comparison.
type level int

const (
	levelOff level = iota
	levelNames
	levelFull
)

// levelOf is the effective level a read of conn runs at now (see above).
// It reads the config as it stands and the Manager's memory, taking only
// their own locks, so it may be called with or without w.mu held.
func levelOf(cfg *config.Config, mgr *db.Manager, conn string) level {
	switch cfg.IntrospectFor(conn) {
	case config.IntrospectOff:
		return levelOff
	case config.IntrospectNames:
		return levelNames
	}
	if mgr.SummarySlow(conn) {
		return levelNames
	}
	return levelFull
}

// CatalogUnread reports whether the workspace is on a connection whose
// catalog has not been read because its level of introspection is off
// (introspect = "off"), rather than because the read is on its way
// (CatalogLoading) or failed: Catalog, Databases and Schemas are empty by
// choice, and a Refresh reads them. A UI says so in its sidebar, so an
// empty list is read neither as an empty database nor as a failure.
func (w *Workspace) CatalogUnread() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.unread
}
