package db

import (
	"context"
	"database/sql"
	"maps"
	"strconv"
	"strings"
	"sync"
	"time"

	bytdbdrv "github.com/rohanthewiz/bytdb/stdlib"
	"github.com/rohanthewiz/serr"

	"github.com/rohanthewiz/dbc/sqlsplit"
)

// Row counts for the sidebar's tables list: "cats (1,234)".
//
// They arrive AFTER the list, as a separate job, because counting is the one
// catalog chore whose cost grows with the data rather than with the schema:
// a count(*) reads every row (or every index entry) of the table. The list
// must never wait for it.
//
// # Exact where cheap, estimated where not
//
// Postgres and MySQL keep a row estimate per table in their catalogs, free to
// read in one query. That estimate decides how each table is counted:
//
//	estimate query (pg_class.reltuples, information_schema.tables.table_rows)
//	      │
//	      ├─ ≥ exactCountLimit ─────────────────► keep the estimate  "~1.2M"
//	      │
//	      └─ below it, or unknown (never analyzed)
//	             │
//	             └─ SELECT count(*) under countTimeout
//	                   ├─ answered ──────────────► exact             "1,234"
//	                   └─ timed out / failed ────► the estimate if any, else no count
//
// SQLite and bytdb keep no estimate, so every table is counted exactly; both
// are local, and a count there costs a file scan, not a network round trip.
// A count that fails (no SELECT privilege, say) is left out rather than
// failing the rest: the sidebar then shows that table without a number.
//
// Views are never counted. A view's count(*) runs the view's whole query,
// which can be arbitrarily expensive, and "rows in a view" is rarely what a
// glance at the sidebar is after.
//
// # Caching
//
// The counts are cached per connection for rowCountTTL, in the Manager — so
// every workspace on it (dbc web's tabs share one Manager) and every quick
// switch away and back reuse one counting. Concurrent askers for the same
// connection wait for the one counting in flight rather than starting their
// own, the same single-flight shape as the Manager's opens.

const (
	// rowCountTTL is how long a connection's counts are served from the
	// cache. Counts drift as soon as anyone writes, so this is a bound on
	// staleness, not a promise of accuracy — a couple of minutes keeps a
	// switch away and back instant without leaving yesterday's numbers up.
	rowCountTTL = 2 * time.Minute

	// exactCountLimit is the estimate at and above which a table is not
	// counted exactly. A million rows is where a count(*) stops being a
	// blink on an ordinary server; above it the estimate (a few percent off
	// after an ANALYZE) says what a sidebar needs to say.
	exactCountLimit = 1_000_000

	// countTimeout bounds one table's count(*); countBudget bounds the
	// whole counting, so a catalog of thousands of tables still settles.
	countTimeout = 3 * time.Second
	countBudget  = 30 * time.Second

	// networkCountWorkers is how many counts run at once on a networked
	// server, where each count is mostly a round trip. The embedded
	// engines count one at a time: their work is local IO, and a shared
	// in-memory SQLite database's pool has one connection to spare
	// (memSQLiteMaxOpen) that the counting must not hog.
	networkCountWorkers = 4
)

// RowCount is a table's number of rows. Estimate reports a number taken from
// the database's statistics rather than counted.
type RowCount struct {
	N        int64
	Estimate bool
}

// Short is the count as the sidebar prints it beside a name: the exact
// number with separators while it is short ("1,234", "98,765"), compacted
// beyond that ("123K", "1.2M"), and marked "~" when estimated.
func (c RowCount) Short() string {
	s := groupDigits(c.N)
	if c.N >= 100_000 {
		s = compactCount(c.N)
	}
	if c.Estimate {
		s = "~" + s
	}
	return s
}

// Sentence is the count in words, for a tooltip: "cats with 1 row",
// "cats with 1,234 rows", "logs with about 12,345,678 rows (estimated)".
func (c RowCount) Sentence(name string) string {
	unit := "rows"
	if c.N == 1 {
		unit = "row"
	}
	about, est := "", ""
	if c.Estimate {
		about, est = "about ", " (estimated from the database's statistics)"
	}
	return name + " with " + about + groupDigits(c.N) + " " + unit + est
}

// groupDigits writes n with comma thousands separators.
func groupDigits(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// compactCount writes n as K, M or B with at most one decimal, dropping a
// ".0": 123456 → "123K", 1234567 → "1.2M", 2000000000 → "2B".
func compactCount(n int64) string {
	units := []struct {
		div    float64
		suffix string
	}{{1e9, "B"}, {1e6, "M"}, {1e3, "K"}}
	for _, u := range units {
		if float64(n) >= u.div {
			v := float64(n) / u.div
			prec := 1
			if v >= 100 {
				prec = 0 // "123K", not "123.5K": the width is the point
			}
			s := strconv.FormatFloat(v, 'f', prec, 64)
			return strings.TrimSuffix(s, ".0") + u.suffix
		}
	}
	return strconv.FormatInt(n, 10)
}

// RowEstimatesQuery returns the statement that reads a driver's per-table
// row estimates as (schema, name, estimate), or "" for a driver that keeps
// none. An estimate the database does not have yet comes back negative or
// NULL, and is read as unknown.
func RowEstimatesQuery(driver string) (string, error) {
	drv, err := driverFor(driver)
	if err != nil {
		return "", err
	}
	switch drv {
	case "pgx":
		// reltuples is the planner's estimate, refreshed by VACUUM, ANALYZE
		// and autovacuum. It is -1 for a table never analyzed (Postgres 14
		// and later; 0 before that, which the exact count then corrects).
		// 'm' is a materialized view, listed by TablesQuery but a view to
		// TableRefs, so never asked for.
		//
		// 'p' is a partitioned parent. It holds no rows of its own, and
		// autovacuum never analyzes it, so its own reltuples is usually -1:
		// read as is, the parent would be counted exactly, and its
		// count(*) reads every partition — on a big partitioned table, the
		// very table that most needs the estimate, that hits countTimeout
		// and shows no number at all. So a parent's estimate is the sum of
		// its leaf partitions' (the parts CTE walks pg_inherits down
		// through sub-partitioned levels; an intermediate 'p' level is
		// skipped like the root, for the same reason):
		//
		//	events (p, -1) ─┬─ events_2025 (r, 4.1M)
		//	                ├─ events_2026 (p, -1) ─┬─ events_2026_h1 (r, 2.0M)
		//	                │                       └─ events_2026_h2 (r, -1)
		//	                └─ events_default (r, 0)
		//	   ─► events ≈ 4.1M + 2.0M + 0 = 6.1M (the -1 leaf adds nothing)
		//
		// A leaf never analyzed (-1) is left out of the sum rather than
		// making the whole estimate unknown. The usual such leaf is a
		// partition created ahead of its data (next month's), which is
		// empty, so the sum is right; at worst it is a lower bound, and a
		// table at least that big is still one not worth counting. Only
		// when no leaf has an estimate does the parent fall back to its
		// own reltuples (set by a manual ANALYZE of the parent, else -1:
		// unknown, so counted exactly — a never-analyzed partitioned table
		// is a small or new one).
		//
		// The sum is taken in float8: reltuples is a float4, whose 24-bit
		// mantissa would round a sum of millions to the nearest few rows
		// and of billions to the nearest hundreds.
		return `WITH RECURSIVE parts AS (
  SELECT c.oid AS root, c.oid AS rel
  FROM pg_catalog.pg_class c
  WHERE c.relkind = 'p'
  UNION ALL
  SELECT p.root, i.inhrelid
  FROM parts p
  JOIN pg_catalog.pg_inherits i ON i.inhparent = p.rel
), leaves AS (
  SELECT p.root, sum(l.reltuples::float8) FILTER (WHERE l.reltuples >= 0) AS n
  FROM parts p
  JOIN pg_catalog.pg_class l ON l.oid = p.rel
  WHERE l.relkind <> 'p'
  GROUP BY p.root
)
SELECT n.nspname, c.relname, COALESCE(lv.n, c.reltuples)::bigint
FROM pg_catalog.pg_class c
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
LEFT JOIN leaves lv ON lv.root = c.oid
WHERE c.relkind IN ('r', 'p')
  AND n.nspname NOT IN ('pg_catalog', 'information_schema')`, nil
	case "mysql":
		// table_rows is exact for MyISAM and a sampled estimate for InnoDB
		// (it can be tens of percent off), so it only ever stands in for a
		// table too big to count. NULL for a view.
		return `SELECT table_schema, table_name, table_rows
FROM information_schema.tables
WHERE table_schema = DATABASE() AND table_type = 'BASE TABLE'`, nil
	case "sqlite", bytdbdrv.DriverName:
		return "", nil
	}
	return "", serr.New("no row estimates for this driver", "driver", driver)
}

// CountQuery returns the count(*) of one table, its name quoted for the
// driver: backticks on MySQL, standard double quotes elsewhere. The name
// comes from the catalog, but a table may be called anything, quotes
// included, so it is always quoted rather than trusted.
func CountQuery(driver string, t TableRef) (string, error) {
	drv, err := driverFor(driver)
	if err != nil {
		return "", err
	}
	q := func(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
	if drv == "mysql" {
		q = func(s string) string { return "`" + strings.ReplaceAll(s, "`", "``") + "`" }
	}
	name := q(t.Name)
	if t.Schema != "" {
		name = q(t.Schema) + "." + name
	}
	return "SELECT count(*) FROM " + name, nil
}

// rowCountCache is a connection's last counting.
type rowCountCache struct {
	at     time.Time             // when the counting started
	asked  map[TableRef]struct{} // the tables it was asked for
	counts map[TableRef]RowCount // what it found; a subset of asked
}

// covers reports whether this counting was asked for every one of tables —
// a table created since is not in it, and the next ask counts afresh.
// asked, not counts, is the test: a view or a failed count is absent from
// counts on purpose, and must not force a recount on every ask.
func (c *rowCountCache) covers(tables []TableRef) bool {
	for _, t := range tables {
		if _, ok := c.asked[t]; !ok {
			return false
		}
	}
	return true
}

// rowCounter is the Manager's row-count cache and its single-flight state.
// It has its own lock: a counting can take seconds, and must never hold up
// the Manager's opens.
type rowCounter struct {
	mu       sync.Mutex
	cache    map[string]*rowCountCache // by connection name
	inflight map[string]chan struct{}  // closed when that connection's counting ends
	// forgotAt is when each connection's counts were last forgotten. A
	// counting that STARTED before then is not fresh, even if it ends
	// after: it may have read the rows before the write that made the
	// forget necessary. Without it, a counting another workspace had in
	// flight across a forget would land and be served as current.
	forgotAt map[string]time.Time
}

// forget drops a connection's cached counts: the connection was dropped or
// edited, and may now point at another database, or a statement on it
// changed rows (ForgetRowCounts).
func (rc *rowCounter) forget(name string) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	delete(rc.cache, name)
	if rc.forgotAt == nil {
		rc.forgotAt = map[string]time.Time{}
	}
	rc.forgotAt[name] = time.Now()
}

// freshLocked reports whether c, name's cached counting, may still be
// served: started within rowCountTTL, and not before the last forget. The
// caller holds mu.
func (rc *rowCounter) freshLocked(name string, c *rowCountCache) bool {
	return c != nil && time.Since(c.at) < rowCountTTL && !c.at.Before(rc.forgotAt[name])
}

// ForgetRowCounts drops the named connection's cached row counts, so the
// next RowCounts counts afresh. The workspace calls it after a run that
// may have changed rows (ChangesRows): the cache would otherwise serve the
// numbers from before it for up to rowCountTTL.
func (m *Manager) ForgetRowCounts(name string) { m.rows.forget(name) }

// sessionVerbs are statements that are not plain reads (IsRead) yet change
// no table's rows: they set up the session or a transaction. ChangesRows
// leaves them out so a SET or a BEGIN does not recount a sidebar. COMMIT
// is not among them — it is what makes a transaction's writes visible to
// the pool the counts are read through — and nor is ROLLBACK, which can
// undo writes a counting inside the transaction's lifetime could not see
// anyway, but costs one recount to be sure of.
var sessionVerbs = map[string]bool{
	"set": true, "reset": true, "begin": true, "start": true, "savepoint": true,
	"release": true, "use": true, "discard": true, "listen": true, "unlisten": true,
	"notify": true, "prepare": true, "deallocate": true, "declare": true, "fetch": true,
	"move": true, "close": true, "lock": true, "unlock": true, "pragma": true,
}

// ChangesRows reports whether stmt may have changed the row count of a
// table: it is not a plain read (IsRead), nor a statement that only sets up
// the session (sessionVerbs). It errs toward yes — an UPDATE changes no
// count, a CREATE INDEX none either — since being wrong that way costs one
// recount, and the other way a stale number in the sidebar.
func ChangesRows(stmt string) bool {
	return !isRead(stmt) && !sessionVerbs[sqlsplit.Verbs(stmt).Main]
}

// RowCounts returns the row count of each of tables on the named
// connection, from the cache when a counting of them started less than
// rowCountTTL ago. A table missing from the result has no count: a view,
// or a table whose count failed and has no estimate to fall back on.
//
// The returned map is shared with the cache and other callers: read it,
// never write to it.
//
// A counting in flight for the same connection is waited for rather than
// duplicated. If it fails, or turns out not to cover these tables, the
// waiter counts for itself.
//
//	ask ──► cache fresh & covers? ── yes ──► cached counts
//	          │ no
//	          ├─ counting in flight? ── yes ──► wait, then ask again
//	          │ no
//	          └─ mark in flight ─► count ─► cache (on success) ─► unmark
func (m *Manager) RowCounts(ctx context.Context, name string, tables []TableRef) (map[TableRef]RowCount, error) {
	rc := &m.rows
	for {
		rc.mu.Lock()
		if c := rc.cache[name]; rc.freshLocked(name, c) && c.covers(tables) {
			rc.mu.Unlock()
			return c.counts, nil
		}
		if done, busy := rc.inflight[name]; busy {
			rc.mu.Unlock()
			select {
			case <-done:
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		done := make(chan struct{})
		if rc.inflight == nil {
			rc.inflight, rc.cache = map[string]chan struct{}{}, map[string]*rowCountCache{}
		}
		rc.inflight[name] = done
		rc.mu.Unlock()

		start := time.Now()
		counts, err := m.countRows(ctx, name, tables)

		rc.mu.Lock()
		delete(rc.inflight, name)
		// A counting that started before a forget is this caller's answer
		// but nobody else's: cached, it could replace a fresher one.
		if err == nil && !start.Before(rc.forgotAt[name]) {
			c := &rowCountCache{at: start, asked: make(map[TableRef]struct{}, len(tables)), counts: counts}
			// A sidebar loaded a schema at a time asks for one schema's
			// tables per counting, so a counting still fresh is merged
			// into rather than replaced: going back to the schema before
			// is then served from the cache, not counted again. The
			// merged cache keeps the OLDER start, as rowCountTTL bounds
			// how stale any number in it may be. The maps are new ones,
			// since the old are shared with earlier callers.
			if old := rc.cache[name]; rc.freshLocked(name, old) {
				c.at = old.at
				c.counts = make(map[TableRef]RowCount, len(old.counts)+len(counts))
				for t := range old.asked {
					c.asked[t] = struct{}{}
				}
				maps.Copy(c.counts, old.counts)
				maps.Copy(c.counts, counts)
			}
			for _, t := range tables {
				c.asked[t] = struct{}{}
			}
			rc.cache[name] = c
			counts = c.counts
		}
		rc.mu.Unlock()
		close(done)
		return counts, err
	}
}

// countRows does one counting; see the top of this file for the rules. It
// runs on the pool — never the pinned session, so it cannot land inside a
// transaction the user has open, nor wait behind their statement.
//
// Only the caller's ctx failing fails it: a counting cut short by
// countBudget returns what it has (and is cached like a whole one, the
// missing tables simply showing no number), but one the caller canceled is
// an error, so a half-done counting is not cached as if it were the answer.
func (m *Manager) countRows(ctx context.Context, name string, tables []TableRef) (map[TableRef]RowCount, error) {
	cc, ok := m.cfg.ConnByName(name)
	if !ok {
		return nil, serr.New("unknown connection", "name", name)
	}
	drv, err := driverFor(cc.Driver)
	if err != nil {
		return nil, err
	}
	dbh, err := m.DBContext(ctx, name)
	if err != nil {
		return nil, err
	}
	bctx, cancel := context.WithTimeout(ctx, countBudget)
	defer cancel()

	// The estimates, when the driver keeps any. Failing to read them only
	// means counting everything exactly, so the error is dropped.
	type key struct{ schema, name string }
	est := map[key]int64{}
	if q, _ := RowEstimatesQuery(cc.Driver); q != "" {
		if rows, err := stringRows(bctx, dbh, q); err == nil {
			for _, r := range rows {
				if len(r) < 3 {
					continue
				}
				if n, err := strconv.ParseInt(r[2], 10, 64); err == nil && n >= 0 {
					est[key{r[0], r[1]}] = n
				}
			}
		}
	}

	out := make(map[TableRef]RowCount, len(tables))
	var todo []TableRef
	for _, t := range tables {
		if t.View {
			continue
		}
		if n, ok := est[key{t.Schema, t.Name}]; ok && n >= exactCountLimit {
			out[t] = RowCount{N: n, Estimate: true}
			continue
		}
		todo = append(todo, t)
	}

	// Count the rest on a few workers, each result into its own slot so
	// the workers share nothing but the index they pull from.
	workers := 1
	if drv == "pgx" || drv == "mysql" {
		workers = networkCountWorkers
	}
	// q is what the counts run on: the pool, or for SQLite one connection
	// set to read uncommitted rows.
	//
	// WHY, FOR SQLITE. A shared-cache database (every in-memory one, the
	// demo included: "file:x?mode=memory&cache=shared") locks per table,
	// and a table a session has written inside an open transaction is
	// write-locked until it ends. Another connection's read of it then
	// waits — in modernc's driver, past its context, for as long as the
	// transaction stays open. A recount after the user's BEGIN; DELETE
	// (workspace.recountLocked) would hang there. read_uncommitted is
	// SQLite's way for a shared-cache reader to skip table read locks; it
	// does nothing outside shared-cache mode, where a file database's
	// readers already go on reading while a writer holds its RESERVED
	// lock. The count may then include the open transaction's rows, which
	// is what that user's own session sees anyway.
	var q interface {
		QueryRowContext(context.Context, string, ...any) *sql.Row
	} = dbh
	if drv == "sqlite" {
		c, err := dbh.Conn(bctx)
		if err != nil {
			return nil, wrapRunErr(ctx, err, name, "op", "count rows")
		}
		defer func() {
			// Back to the default before the connection returns to the
			// pool, where a user's script may pick it up. Background, so
			// a canceled counting still resets it.
			_, _ = c.ExecContext(context.Background(), `PRAGMA read_uncommitted = 0`)
			_ = c.Close()
		}()
		if _, err = c.ExecContext(bctx, `PRAGMA read_uncommitted = 1`); err != nil {
			return nil, wrapRunErr(ctx, err, name, "op", "count rows")
		}
		q = c
	}
	exact := make([]int64, len(todo))
	got := make([]bool, len(todo))
	var next sync.Mutex
	i := 0
	var wg sync.WaitGroup
	for range min(workers, len(todo)) {
		wg.Go(func() {
			for {
				next.Lock()
				j := i
				i++
				next.Unlock()
				if j >= len(todo) || bctx.Err() != nil {
					return
				}
				stmt, err := CountQuery(cc.Driver, todo[j])
				if err != nil {
					continue
				}
				tctx, tcancel := context.WithTimeout(bctx, countTimeout)
				err = q.QueryRowContext(tctx, stmt).Scan(&exact[j])
				tcancel()
				got[j] = err == nil
			}
		})
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for j, t := range todo {
		if got[j] {
			out[t] = RowCount{N: exact[j]}
		} else if n, ok := est[key{t.Schema, t.Name}]; ok {
			out[t] = RowCount{N: n, Estimate: true}
		}
	}
	return out, nil
}
