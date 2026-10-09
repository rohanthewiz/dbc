package etl

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"

	pgxstdlib "github.com/jackc/pgx/v5/stdlib"
	"github.com/rohanthewiz/serr"
)

// WriteOptions shape a Writer's load.
type WriteOptions struct {
	// Setup statements run first, inside the load's transaction — a CREATE
	// TABLE, an ALTER, a DELETE of the slice being reloaded. If the load
	// fails they are rolled back with it (except where the engine commits
	// DDL on its own: MySQL).
	Setup []string
	// Truncate empties the table after Setup and before the first row, in
	// the same transaction: a reload either replaces the old rows or leaves
	// them alone, never a half-empty table.
	Truncate bool
	// BatchSize is rows per INSERT statement on engines loaded by INSERT
	// (everything but Postgres). 0 means 500. It is lowered automatically so
	// one statement stays under the engines' bind-parameter limits.
	BatchSize int
	// Upsert, when set, names the columns a row is matched on: a row whose
	// values in them match an existing row's replaces that row's other
	// columns, any other row is inserted, and when one load carries a key
	// twice the last row wins. The columns must be among the Writer's and
	// carry a unique constraint (a primary key, a unique index) on the
	// table; without one the server's own error says so. Postgres and
	// SQLite only — see upsertEngine.
	Upsert []string
}

const (
	defaultBatchSize = 500
	// maxBindParams keeps one INSERT under SQLite's 32766-variable limit
	// (MySQL's is 65535); with 10 columns a batch is capped at 3000 rows.
	maxBindParams = 30000
	// pgFlushAt is how many encoded bytes a Postgres Writer buffers before
	// handing them to the COPY stream: big enough to amortize the pipe
	// handoff, small enough to keep memory flat.
	pgFlushAt = 64 << 10
	// endTxTimeout bounds the COMMIT/ROLLBACK at the end of a load. They run
	// on a context of their own: a ROLLBACK after the user pressed Stop must
	// still reach the server rather than fail on the canceled run context.
	endTxTimeout = 30 * time.Second
)

// errAborted is what a Postgres COPY sees when its Writer is aborted.
var errAborted = errors.New("etl: load aborted")

// Writer loads rows into one table inside one transaction. Rows become
// visible to others only when Close commits; Abort (or any failed Write)
// rolls everything back.
//
//	w, err := NewWriter(ctx, dst, "users", []string{"id", "email"}, WriteOptions{Truncate: true})
//	defer w.Abort() // no-op once Close has committed
//	for … { if err := w.Write(row); err != nil { return err } }
//	n, err := w.Close()
//
// A Writer is not safe for concurrent use.
type Writer struct {
	ctx   context.Context
	conn  string
	table string
	cols  []string
	impl  loader
	n     int64
	err   error // first failure; the Writer is unusable after it
	ended bool  // committed or rolled back
}

// loader is one engine strategy behind a Writer.
type loader interface {
	write(row []any) error
	commit() (int64, error)
	abort()
}

// NewWriter opens the load's transaction on c, runs opt.Setup and the
// optional truncate, and is then ready for rows in the order of cols.
func NewWriter(ctx context.Context, c Conn, table string, cols []string, opt WriteOptions) (*Writer, error) {
	if len(cols) == 0 {
		return nil, serr.New("etl: a Writer needs at least one column", "table", table)
	}
	if len(opt.Upsert) > 0 {
		if err := upsertEngine(c.Engine); err != nil {
			return nil, serr.Wrap(err, "conn", c.Name, "table", table)
		}
		for _, k := range opt.Upsert {
			if !slices.Contains(cols, k) {
				return nil, serr.New("etl: an upsert key column is not among the columns", "table", table,
					"column", k, "columns", strings.Join(cols, ", "))
			}
		}
	}
	setup := opt.Setup
	if opt.Truncate {
		setup = append(append([]string(nil), setup...), c.Engine.truncateStmt(table))
	}
	var (
		impl loader
		err  error
	)
	if c.Engine == Postgres {
		impl, err = newPGCopy(ctx, c, table, cols, setup, opt.Upsert)
	} else {
		impl, err = newInserter(ctx, c, table, cols, setup, opt.BatchSize, opt.Upsert)
	}
	if err != nil {
		return nil, serr.Wrap(canceled(ctx, err), "conn", c.Name, "table", table)
	}
	return &Writer{ctx: ctx, conn: c.Name, table: table, cols: cols, impl: impl}, nil
}

// Columns returns the column list rows are written in.
func (w *Writer) Columns() []string { return w.cols }

// Count is the number of rows accepted by Write so far.
func (w *Writer) Count() int64 { return w.n }

// Write queues one row; its values line up with Columns. A failure aborts
// the whole load, and every later Write or Close returns the same error.
func (w *Writer) Write(row []any) error {
	if w.err != nil {
		return w.err
	}
	if w.ended {
		return serr.New("etl: Write after Close", "table", w.table)
	}
	if len(row) != len(w.cols) {
		return w.fail(serr.New("etl: row has the wrong number of values",
			"table", w.table, "want", itoa(int64(len(w.cols))), "got", itoa(int64(len(row)))))
	}
	if err := w.impl.write(row); err != nil {
		return w.fail(serr.Wrap(err, "conn", w.conn, "table", w.table, "row", itoa(w.n+1)))
	}
	w.n++
	return nil
}

// Close sends any buffered rows and commits, returning the rows loaded.
// After a failed Write it rolls back instead and returns that failure.
func (w *Writer) Close() (int64, error) {
	if w.err != nil {
		return 0, w.err
	}
	if w.ended {
		return w.n, nil
	}
	w.ended = true
	n, err := w.impl.commit()
	if err != nil {
		w.err = serr.Wrap(canceled(w.ctx, err), "conn", w.conn, "table", w.table, "op", "commit")
		return 0, w.err
	}
	return n, nil
}

// Abort rolls the load back. It is a no-op after Close or a previous Abort,
// so `defer w.Abort()` is the idiom that guarantees an early return or a
// panic never leaves a transaction open.
func (w *Writer) Abort() error {
	if !w.ended {
		w.ended = true
		w.impl.abort()
	}
	return nil
}

func (w *Writer) fail(err error) error {
	err = canceled(w.ctx, err)
	w.err = err
	if !w.ended {
		w.ended = true
		w.impl.abort()
	}
	return err
}

// ─── Postgres: COPY … FROM STDIN ────────────────────────────────────────────
//
// The rows reach the server through an io.Pipe. Write encodes into a buffer
// and, every pgFlushAt bytes, writes it into the pipe; a goroutine holds
// the connection in pgconn.CopyFrom, which reads the other end.
//
//	Write ─► buf ─(64 KiB)─► pw ══ io.Pipe ══ pr ─► pgconn.CopyFrom ─► server
//	                                               (goroutine, inside conn.Raw)
//	Close: flush, pw.Close() ─► CopyFrom sees EOF, returns the row count ─► COMMIT
//	Abort: pw.CloseWithError ─► CopyFrom sends CopyFail ─► ROLLBACK
//
// A server-side failure (a constraint, an unparsable value) ends CopyFrom
// early; the goroutine then closes pr with that error, so the next pipe
// write fails at once rather than blocking forever on a reader that is gone.
//
// The transaction is BEGIN/COMMIT on a dedicated *sql.Conn rather than
// sql.Tx, because the COPY must run on the very connection the transaction
// is on, and the only way to reach pgconn is conn.Raw — which sql.Tx does
// not offer.

type pgCopy struct {
	ctx   context.Context
	conn  *sql.Conn
	pw    *io.PipeWriter
	buf   []byte
	bytea []bool
	done  chan copyResult
	res   *copyResult
	// merge is an upsert's last step, run after the COPY into the staging
	// table and before COMMIT (see pgUpsert); "" for a plain load
	merge string
	trace func(string)
}

type copyResult struct {
	n   int64
	err error
}

func newPGCopy(ctx context.Context, c Conn, table string, cols []string, setup []string, upsert []string) (*pgCopy, error) {
	conn, err := c.DB.Conn(ctx)
	if err != nil {
		return nil, serr.Wrap(err, "op", "checkout")
	}
	if _, err = conn.ExecContext(ctx, "BEGIN"); err != nil {
		discard(conn)
		return nil, serr.Wrap(err, "op", "begin")
	}
	for _, s := range setup {
		c.trace(s)
		if _, err = conn.ExecContext(ctx, s); err != nil {
			endTx(conn, "ROLLBACK")
			return nil, serr.Wrap(err, "op", "setup", "stmt", clip(s))
		}
	}
	// bytea is read off the target even for an upsert: the staging table
	// is made from it, with the same column types
	bytea, err := pgByteaCols(ctx, conn, Postgres.QuoteTable(table), cols)
	if err != nil {
		endTx(conn, "ROLLBACK")
		return nil, err
	}
	into, merge := Postgres.QuoteTable(table), ""
	if len(upsert) > 0 {
		stage, m := pgUpsert(table, cols, upsert)
		c.trace(stage)
		if _, err = conn.ExecContext(ctx, stage); err != nil {
			endTx(conn, "ROLLBACK")
			return nil, serr.Wrap(err, "op", "upsert staging table", "stmt", clip(stage))
		}
		into, merge = pgUpsertStage, m
	}

	pr, pw := io.Pipe()
	w := &pgCopy{ctx: ctx, conn: conn, pw: pw, bytea: bytea, done: make(chan copyResult, 1),
		buf: make([]byte, 0, pgFlushAt+4096), merge: merge, trace: c.trace}
	stmt := "COPY " + into + " (" + Postgres.quoteCols(cols) + ") FROM STDIN"
	go func() {
		var n int64
		err := conn.Raw(func(dc any) error {
			pc, ok := dc.(*pgxstdlib.Conn)
			if !ok {
				return serr.New("etl: not a pgx connection")
			}
			tag, err := pc.Conn().PgConn().CopyFrom(ctx, pr, stmt)
			n = tag.RowsAffected()
			return err
		})
		// Close the read end whatever happened, so a Write still in flight
		// (or a later one) fails at once instead of blocking on a reader
		// that is gone: with the COPY's error when it failed, ErrClosedPipe
		// when it finished.
		if err != nil {
			pr.CloseWithError(err)
		} else {
			pr.CloseWithError(io.ErrClosedPipe)
		}
		w.done <- copyResult{n: n, err: err}
	}()
	return w, nil
}

func (w *pgCopy) write(row []any) error {
	w.buf = appendPGRow(w.buf, row, w.bytea)
	if len(w.buf) >= pgFlushAt {
		return w.flush()
	}
	return nil
}

func (w *pgCopy) flush() error {
	if len(w.buf) == 0 {
		return nil
	}
	_, err := w.pw.Write(w.buf)
	w.buf = w.buf[:0]
	if err != nil {
		// The COPY ended early; its own error says why, the pipe's does not.
		if r := w.wait(); r.err != nil {
			return r.err
		}
		return err
	}
	return nil
}

// writeRaw takes bytes that are already COPY text rows — another server's
// COPY TO STDOUT — into the stream. They are buffered like encoded rows,
// not written through: pgconn's CopyTo hands over one row per Write, and
// CopyFrom sends whatever one pipe Read returns as its own CopyData message
// and socket write, so passing them straight on cost a syscall per row and
// made the "fast" path slower than decoding. p is copied, so the caller may
// reuse it.
func (w *pgCopy) writeRaw(p []byte) error {
	w.buf = append(w.buf, p...)
	if len(w.buf) >= pgFlushAt {
		return w.flush()
	}
	return nil
}

// wait returns the COPY goroutine's result, receiving it only once.
func (w *pgCopy) wait() copyResult {
	if w.res == nil {
		r := <-w.done
		w.res = &r
	}
	return *w.res
}

func (w *pgCopy) commit() (int64, error) {
	if err := w.flush(); err != nil {
		w.pw.CloseWithError(err)
		w.wait()
		endTx(w.conn, "ROLLBACK")
		return 0, err
	}
	_ = w.pw.Close()
	r := w.wait()
	if r.err != nil {
		endTx(w.conn, "ROLLBACK")
		return 0, r.err
	}
	if w.merge != "" {
		// the rows are all in the staging table; the merge moves them into
		// the target in one statement, inside the load's transaction, so a
		// failure here (no unique constraint on the key, a NOT NULL column
		// the rows leave out) rolls the whole load back
		w.trace(w.merge)
		if _, err := w.conn.ExecContext(w.ctx, w.merge); err != nil {
			endTx(w.conn, "ROLLBACK")
			return 0, serr.Wrap(canceled(w.ctx, err), "op", "upsert", "stmt", clip(w.merge))
		}
	}
	if err := endTx(w.conn, "COMMIT"); err != nil {
		return 0, err
	}
	return r.n, nil
}

// ─── upsert ─────────────────────────────────────────────────────────────────
//
// An upsert matches rows on the key columns the caller names, updating
// the other columns of a row that is there and inserting one that is not.
// Each engine says it its own way:
//
//	Postgres  COPY ─► a staging table (a temp copy of the target's columns,
//	          no constraints) ─► at commit, one INSERT … SELECT … ON CONFLICT
//	          (key) DO UPDATE into the target, then COMMIT
//	SQLite    every batched INSERT … VALUES ends ON CONFLICT (key) DO UPDATE
//
// Why Postgres stages: COPY has no ON CONFLICT, and giving up COPY for
// batched INSERTs would give up the speed a load into Postgres is for.
// The staging table keeps COPY's throughput; the merge is one set-based
// statement on the server.

// pgUpsertStage is the staging table's name. There is one per load's
// connection at a time, and it lives only as long as the load's
// transaction (ON COMMIT DROP; a ROLLBACK undoes its CREATE), so a fixed
// name is safe even on a pooled connection the next load reuses.
const pgUpsertStage = `"_dbc_upsert"`

// pgUpsert is the staging table's CREATE and the merge that moves its
// rows into table at commit.
//
// The merge takes ONE row per key, the last to arrive: DISTINCT ON (key)
// keeps the first row of each key group in the ORDER BY, and ordering by
// ctid DESC puts the last-arrived first — a fresh table filled by one
// COPY, with no updates or deletes, stores its rows in arrival order, so
// a later row has a greater ctid. That does two things: the last row for
// a key wins, as it does on SQLite (whose INSERT applies rows in order),
// and the INSERT never meets one key twice, which Postgres refuses ("ON
// CONFLICT DO UPDATE command cannot affect row a second time").
func pgUpsert(table string, cols, key []string) (stage, merge string) {
	qt, qc, qk := Postgres.QuoteTable(table), Postgres.quoteCols(cols), Postgres.quoteCols(key)
	// AS SELECT … WITH NO DATA copies the columns' exact types and nothing
	// else — no NOT NULL, no unique index, no default — so the staging
	// table takes duplicate keys, and the target's own constraints judge
	// the rows at the merge
	stage = "CREATE TEMP TABLE " + pgUpsertStage + " ON COMMIT DROP AS SELECT " + qc + " FROM " + qt + " WITH NO DATA"
	merge = "INSERT INTO " + qt + " (" + qc + ") SELECT DISTINCT ON (" + qk + ") " + qc +
		" FROM " + pgUpsertStage + " ORDER BY " + qk + ", ctid DESC " + upsertClause(Postgres, cols, key)
	return stage, merge
}

// upsertClause is the ON CONFLICT tail of an upsert's INSERT: every
// column that is not a key set from the incoming row (excluded.c), or DO
// NOTHING when every column is a key — there is nothing to update, and an
// existing row is left as it is. Postgres and SQLite spell it alike.
func upsertClause(e Engine, cols, key []string) string {
	var set []string
	for _, c := range cols {
		if !slices.Contains(key, c) {
			q := e.QuoteIdent(c)
			set = append(set, q+" = excluded."+q)
		}
	}
	out := "ON CONFLICT (" + e.quoteCols(key) + ") "
	if len(set) == 0 {
		return out + "DO NOTHING"
	}
	return out + "DO UPDATE SET " + strings.Join(set, ", ")
}

// upsertEngine refuses an upsert where the Writer has no way to say one.
// MySQL's INSERT … ON DUPLICATE KEY UPDATE is left out on purpose: it
// fires on a clash with ANY unique key of the table, not the columns
// named, so a load "matched on email" could silently update the row whose
// id clashed instead. bytdb has no conflict clause.
func upsertEngine(e Engine) error {
	if e == Postgres || e == SQLite {
		return nil
	}
	return serr.New("etl: upsert is for Postgres and SQLite", "engine", e.String())
}

func (w *pgCopy) abort() {
	w.pw.CloseWithError(errAborted)
	w.wait()
	endTx(w.conn, "ROLLBACK")
}

// pgByteaCols reports, per column, whether the destination column is bytea —
// where a []byte value must go in as hex rather than as text. It reads the
// catalog inside the load's transaction, so it sees a table Setup just
// created; a missing table fails here with Postgres's own message, before
// any row is sent.
func pgByteaCols(ctx context.Context, conn *sql.Conn, qtable string, cols []string) ([]bool, error) {
	rows, err := conn.QueryContext(ctx, `SELECT a.attname, a.atttypid = 'bytea'::regtype
		FROM pg_attribute a WHERE a.attrelid = $1::regclass AND a.attnum > 0 AND NOT a.attisdropped`, qtable)
	if err != nil {
		return nil, serr.Wrap(err, "op", "read destination columns")
	}
	defer rows.Close()
	isBytea := map[string]bool{}
	for rows.Next() {
		var name string
		var b bool
		if err = rows.Scan(&name, &b); err != nil {
			return nil, serr.Wrap(err, "op", "read destination columns")
		}
		isBytea[name] = b
	}
	if err = rows.Err(); err != nil {
		return nil, serr.Wrap(err, "op", "read destination columns")
	}
	out := make([]bool, len(cols))
	for i, c := range cols {
		out[i] = isBytea[c]
	}
	return out, nil
}

// endTx runs COMMIT or ROLLBACK on a load's connection and releases it.
// When that statement fails the connection's transaction state is unknown,
// so it is discarded rather than pooled — the server ends a dropped
// session's transaction itself.
func endTx(conn *sql.Conn, stmt string) error {
	ctx, cancel := context.WithTimeout(context.Background(), endTxTimeout)
	defer cancel()
	if _, err := conn.ExecContext(ctx, stmt); err != nil {
		discard(conn)
		return serr.Wrap(err, "op", strings.ToLower(stmt))
	}
	return conn.Close()
}

// discard closes a checked-out connection instead of returning it to the
// pool: database/sql's one way to do that is a Raw callback that returns
// driver.ErrBadConn (see db.Session.Close).
func discard(conn *sql.Conn) {
	_ = conn.Raw(func(any) error { return driver.ErrBadConn })
}

// ─── MySQL, SQLite, bytdb: batched multi-row INSERT ─────────────────────────
//
// Rows are buffered until a batch is full, then sent as one
// INSERT … VALUES (…), (…), … with bind parameters — one round trip and one
// parse per batch instead of per row. The full-batch statement is prepared
// once and reused; only the final, shorter batch is built ad hoc.

type inserter struct {
	ctx     context.Context
	tx      *sql.Tx
	e       Engine
	prefix  string // INSERT INTO t (cols) VALUES
	suffix  string // " ON CONFLICT …" for an upsert; "" for a plain load
	ncols   int
	batch   int
	args    []any
	pending int
	full    *sql.Stmt
	n       int64
}

func newInserter(ctx context.Context, c Conn, table string, cols []string, setup []string, batch int, upsert []string) (*inserter, error) {
	if batch <= 0 {
		batch = defaultBatchSize
	}
	if lim := maxBindParams / len(cols); batch > lim {
		batch = max(lim, 1)
	}
	tx, err := c.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, serr.Wrap(err, "op", "begin")
	}
	for _, s := range setup {
		c.trace(s)
		if _, err = tx.ExecContext(ctx, s); err != nil {
			_ = tx.Rollback()
			return nil, serr.Wrap(err, "op", "setup", "stmt", clip(s))
		}
	}
	w := &inserter{ctx: ctx, tx: tx, e: c.Engine, ncols: len(cols), batch: batch,
		prefix: "INSERT INTO " + c.Engine.QuoteTable(table) + " (" + c.Engine.quoteCols(cols) + ") VALUES ",
		args:   make([]any, 0, batch*len(cols))}
	if len(upsert) > 0 {
		// the rows of one INSERT are applied in order, and batches go in
		// order, so a key repeated within a batch or across batches ends
		// with its last row's values — what Postgres's merge does too
		w.suffix = " " + upsertClause(c.Engine, cols, upsert)
	}
	return w, nil
}

// insertSQL is the statement for a batch of rows: one (?, ?, …) group per row,
// numbered $1… on the engines that number their parameters, and an
// upsert's ON CONFLICT clause after them.
func (w *inserter) insertSQL(rows int) string {
	var b strings.Builder
	b.Grow(len(w.prefix) + rows*w.ncols*5 + len(w.suffix))
	b.WriteString(w.prefix)
	p := 1
	for r := range rows {
		if r > 0 {
			b.WriteString(", ")
		}
		b.WriteByte('(')
		for c := range w.ncols {
			if c > 0 {
				b.WriteString(", ")
			}
			b.WriteString(w.e.placeholder(p))
			p++
		}
		b.WriteByte(')')
	}
	b.WriteString(w.suffix)
	return b.String()
}

func (w *inserter) write(row []any) error {
	w.args = append(w.args, row...)
	w.pending++
	if w.pending == w.batch {
		return w.flush()
	}
	return nil
}

func (w *inserter) flush() error {
	if w.pending == 0 {
		return nil
	}
	var err error
	if w.pending == w.batch {
		if w.full == nil {
			if w.full, err = w.tx.PrepareContext(w.ctx, w.insertSQL(w.batch)); err != nil {
				return serr.Wrap(err, "op", "prepare insert")
			}
		}
		_, err = w.full.ExecContext(w.ctx, w.args...)
	} else {
		_, err = w.tx.ExecContext(w.ctx, w.insertSQL(w.pending), w.args...)
	}
	if err != nil {
		return serr.Wrap(err, "op", "insert", "batch_rows", itoa(int64(w.pending)))
	}
	w.n += int64(w.pending)
	w.args = w.args[:0]
	w.pending = 0
	return nil
}

func (w *inserter) commit() (int64, error) {
	if err := w.flush(); err != nil {
		w.abort()
		return 0, err
	}
	if w.full != nil {
		_ = w.full.Close()
	}
	if err := w.tx.Commit(); err != nil {
		return 0, serr.Wrap(err, "op", "commit")
	}
	return w.n, nil
}

func (w *inserter) abort() {
	if w.full != nil {
		_ = w.full.Close()
	}
	_ = w.tx.Rollback()
}

// ─── small helpers ──────────────────────────────────────────────────────────

// canceled makes a failure caused by ctx say so. When a run is stopped the
// driver rarely reports context.Canceled itself: pgconn, for one, closes the
// socket under a COPY, and the error that surfaces is "use of closed network
// connection" from whichever side noticed first. Wrapping with ctx.Err()
// lets callers test errors.Is(err, context.Canceled) — sdb.IsCanceled does —
// and keeps the driver's words for the log.
func canceled(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if cerr := ctx.Err(); cerr != nil && !errors.Is(err, cerr) {
		return fmt.Errorf("%w: %w", cerr, err)
	}
	return err
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// clip shortens a statement for an error message.
func clip(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > 120 {
		return string(r[:120]) + "…"
	}
	return s
}
