package sdb

import (
	"sync"

	"github.com/rohanthewiz/dbc/etl"
	"github.com/rohanthewiz/serr"
)

// The ETL half of the script API: move rows between connections, possibly
// on different engines. Three levels, from most to least packaged:
//
//	s.Copy(src, dst, table, opts)          a whole table (or query) over, in one call
//	s.Reader(conn, query, args...)  ┐      stream rows out, reshape them in Go,
//	s.Writer(conn, table, cols, opts) ┘    load them in: joins across servers,
//	                                       fan-out, lookups, anything Copy can't say
//
// Unlike s.Query, a Reader is not capped at max_rows and keeps values typed
// (int64, time.Time, []byte …) rather than rendered for display, so nothing
// is lost or truncated on the way through. See package etl for the engine
// strategies (direct Postgres COPY, batched INSERT) and the transaction
// guarantees.

// CopyOpts shapes s.Copy: destination name, a Where or a whole Query as the
// source, a column subset, Create/Truncate of the destination, a per-row
// Transform. The zero value copies the whole table into an existing table
// of the same name. Set ProgressEvery to have the script output report the
// running row count.
type CopyOpts = etl.CopyOptions

// CopyStats reports a finished copy; its String() is a one-line summary.
type CopyStats = etl.CopyStats

// Reader streams a query's rows: for rd.Next() { row := rd.Row() }, then
// rd.Err(). Close it (defer rd.Close()) if you stop before the end.
type Reader = etl.Reader

// Writer loads rows into one table in one transaction: w.Write(row) per
// row, then w.Close() to commit. defer w.Abort() rolls back on any early
// return; it is a no-op after Close.
type Writer = etl.Writer

// WriteOpts shapes s.Writer: Setup statements and Truncate run first, in
// the load's transaction; BatchSize tunes INSERT batching off Postgres.
type WriteOpts = etl.WriteOptions

// openSet is every Reader and Writer a run opened, so the host can release
// what the script forgot. A script may use goroutines, hence the lock.
type openSet struct {
	mu      sync.Mutex
	readers []*etl.Reader
	writers []*etl.Writer
}

// etlConn resolves a connection name to the etl package's view of it: the
// pool, opened on first use under the run's context, and its SQL dialect.
// Its Trace feeds the DDL log (LogDDL), so the CREATE TABLE a Copy makes or
// a Writer's Setup runs is logged as a script's own Exec of it would be.
func (s *S) etlConn(name string) (etl.Conn, error) {
	drv, err := s.mgr.DriverOf(name)
	if err != nil {
		return etl.Conn{}, err
	}
	e, err := etl.EngineOf(drv)
	if err != nil {
		return etl.Conn{}, serr.Wrap(err, "conn", name)
	}
	dbh, err := s.mgr.DBContext(s.Ctx(), name)
	if err != nil {
		return etl.Conn{}, err
	}
	trace := func(stmt string) { s.logDDL(name, stmt) }
	return etl.Conn{Name: name, DB: dbh, Engine: e, Trace: trace}, nil
}

// Copy copies table from connection src into connection dst:
//
//	st, err := s.Copy("prod", "local", "public.orders", sdb.CopyOpts{
//		Create: true, Truncate: true, Where: "created_at >= now() - interval '7 days'",
//	})
//	s.Print("%s", st) // copied 48213 rows prod:public.orders → local:public.orders in 1.4s (direct COPY)
//
// Postgres to Postgres without a Transform or Args streams COPY to COPY and
// never decodes a row; every other pairing goes row by row. Either way the
// destination is loaded in one transaction (with its Create and Truncate),
// so a failed or stopped copy leaves it as it was.
func (s *S) Copy(src, dst, table string, opt CopyOpts) (CopyStats, error) {
	from, err := s.etlConn(src)
	if err != nil {
		return CopyStats{}, err
	}
	to, err := s.etlConn(dst)
	if err != nil {
		return CopyStats{}, err
	}
	// ProgressEvery alone means "tell me how it's going": report to the
	// script output, where Print lines go, rather than make every script
	// write the same callback.
	if opt.Progress == nil && opt.ProgressEvery > 0 {
		opt.Progress = func(n int64) { s.Print("  %s → %s: %d rows", src, dst, n) }
	}
	return etl.Copy(s.Ctx(), from, table, to, opt)
}

// Reader runs query on conn and streams its rows. Use the placeholder style
// of the connection's driver ($1 postgres/bytdb, ? mysql/sqlite).
func (s *S) Reader(conn, query string, args ...any) (*Reader, error) {
	c, err := s.etlConn(conn)
	if err != nil {
		return nil, err
	}
	rd, err := etl.Read(s.Ctx(), c, query, args...)
	if err != nil {
		return nil, err
	}
	s.open.mu.Lock()
	s.open.readers = append(s.open.readers, rd)
	s.open.mu.Unlock()
	return rd, nil
}

// Writer starts a load into table on conn; rows passed to Write line up
// with cols. Nothing is visible to other sessions until Close commits.
func (s *S) Writer(conn, table string, cols []string, opt WriteOpts) (*Writer, error) {
	c, err := s.etlConn(conn)
	if err != nil {
		return nil, err
	}
	w, err := etl.NewWriter(s.Ctx(), c, table, cols, opt)
	if err != nil {
		return nil, err
	}
	s.open.mu.Lock()
	s.open.writers = append(s.open.writers, w)
	s.open.mu.Unlock()
	return w, nil
}

// Release closes every Reader and rolls back every Writer the script left
// open. The host calls it when the script's Run returns (or panics): a
// forgotten Writer would otherwise hold a connection and an open
// transaction — and its locks — for the rest of the process. It rolls back
// rather than commits, because only an explicit Close says the load is
// complete. Both are no-ops on what the script already closed.
func (s *S) Release() {
	s.open.mu.Lock()
	readers, writers := s.open.readers, s.open.writers
	s.open.readers, s.open.writers = nil, nil
	s.open.mu.Unlock()
	for _, w := range writers {
		_ = w.Abort()
	}
	for _, rd := range readers {
		_ = rd.Close()
	}
}
