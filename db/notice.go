package db

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"strconv"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5/pgconn"
	pgxstdlib "github.com/jackc/pgx/v5/stdlib"

	"github.com/rohanthewiz/dbc/model"
)

// Server notices: Postgres's RAISE NOTICE / WARNING / INFO / LOG / DEBUG,
// and the server's own ("table … does not exist, skipping").
//
// pgx hands a notice to the connection config's OnNotice callback, on the
// goroutine reading the reply, with the *pgconn.PgConn it arrived on and
// nothing else — no context, no statement. So the notice is routed by its
// connection: a pinned Session registers a buffer for its PgConn when it is
// opened, and every Postgres pool's OnNotice (openPool) is dispatchNotice,
// which looks that buffer up.
//
//	Manager.Session ─► noticeSinks[pgconn] = buf
//	Session.Run     ─► buf.reset ─► statement runs ─► server sends N notices
//	                                     │
//	                    pgconn reader ───┴─► dispatchNotice ─► buf.add (×N)
//	Session.Notices ─► buf.take  (what the caller logs)
//	Session.Close   ─► delete(noticeSinks, pgconn)
//
// RunNotices does the same for one pooled statement (a script's Query and
// Exec), on a connection checked out for it. A notice on a connection
// neither holds — the plain RunContext path (catalog queries, row counts)
// — finds no buffer and is dropped: those are dbc's own statements, with
// no log line to put it on.
//
// MySQL's warnings are a different mechanism (SHOW WARNINGS, a separate
// round trip) and SQLite has none, so only Postgres sessions get a buffer.

// Notice is one message the server sent while a statement ran.
type Notice struct {
	Severity string // NOTICE, WARNING, INFO, LOG, DEBUG (unlocalized when the server says)
	Message  string
	Detail   string
	Hint     string
}

// String is the notice as one log line, psql-style: "NOTICE: users : 42",
// with the detail and hint, when the server sent them, on the same line so
// a log that draws one entry per line keeps them together.
func (n Notice) String() string {
	var b strings.Builder
	b.WriteString(n.Severity)
	b.WriteString(": ")
	b.WriteString(n.Message)
	if n.Detail != "" {
		b.WriteString(" — DETAIL: ")
		b.WriteString(n.Detail)
	}
	if n.Hint != "" {
		b.WriteString(" — HINT: ")
		b.WriteString(n.Hint)
	}
	return b.String()
}

// maxNotices caps what one statement's buffer keeps. A loop that raises a
// notice per row can send millions; past the cap they are counted, not
// kept, so a runaway RAISE costs memory for a counter, not for every line,
// and the log is not flooded past reading.
const maxNotices = 1000

// noticeBuf collects the notices of the statement running on one
// connection. add runs on pgconn's reading goroutine, which is the one
// running the statement for a stdlib connection, but the mutex keeps that
// an implementation detail rather than a requirement.
type noticeBuf struct {
	mu      sync.Mutex
	notices []Notice
	dropped int // notices past maxNotices since the last reset
}

func (b *noticeBuf) add(n Notice) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.notices) >= maxNotices {
		b.dropped++
		return
	}
	b.notices = append(b.notices, n)
}

func (b *noticeBuf) reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.notices, b.dropped = nil, 0
}

// take returns the buffered notices and empties the buffer. When some were
// dropped at the cap, a last synthetic notice says how many, so the log
// does not read as though the stream ended there.
func (b *noticeBuf) take() []Notice {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := b.notices
	if b.dropped > 0 {
		out = append(out, Notice{Severity: "NOTICE",
			Message: "… " + strconv.Itoa(b.dropped) + " more notices not shown"})
	}
	b.notices, b.dropped = nil, 0
	return out
}

// noticeSinks maps a *pgconn.PgConn to the *noticeBuf of the Session
// holding it. A package-level map rather than a Manager field: OnNotice is
// fixed in the pool's config when the pool opens, and a PgConn pointer is
// unique process-wide, so one map serves every Manager without the
// callback having to capture one.
var noticeSinks sync.Map

// dispatchNotice is every Postgres pool's OnNotice (see openPool).
func dispatchNotice(pc *pgconn.PgConn, n *pgconn.Notice) {
	v, ok := noticeSinks.Load(pc)
	if !ok {
		return
	}
	sev := n.SeverityUnlocalized // stable "WARNING", even on a server set to another lc_messages
	if sev == "" {
		sev = n.Severity
	}
	v.(*noticeBuf).add(Notice{Severity: sev, Message: n.Message, Detail: n.Detail, Hint: n.Hint})
}

// attachNotices registers a buffer for the Postgres connection behind raw
// (a *sql.Conn's Raw) and returns it with its key, or nils when the
// connection is not a pgx one.
func attachNotices(raw func(func(any) error) error) (*noticeBuf, *pgconn.PgConn) {
	var pc *pgconn.PgConn
	_ = raw(func(dc any) error {
		if c, ok := dc.(*pgxstdlib.Conn); ok {
			pc = c.Conn().PgConn()
		}
		return nil
	})
	if pc == nil {
		return nil, nil
	}
	buf := &noticeBuf{}
	noticeSinks.Store(pc, buf)
	return buf, pc
}

// RunNotices is RunContext that also returns the server notices the
// statement raised, in the order sent, success or failure. Session.Run has
// its session's buffer for that. A pooled run has none, since a statement
// on *sql.DB runs on whichever connection database/sql picks and that
// connection is never known here. So on Postgres the statement runs on a
// connection checked out for it alone, with a buffer registered for that
// connection's lifetime in the run:
//
//	dbh.Conn ─► attachNotices ─► run ─► take ─► unregister ─► Conn.Close (back to the pool)
//
// The connection goes back to the pool, not discarded as a Session's is:
// nothing here could have left session state on it that a plain pooled
// run would not have left too.
//
// The checkout costs nothing extra: *sql.DB's ExecContext/QueryContext
// check out a connection the same way, and the checkout path is where
// pgShouldPing's dead-connection check runs. The one thing database/sql's
// pooled path adds is a retry when the driver says driver.ErrBadConn
// (the statement never reached the server). A *sql.Conn hands that error
// back instead, so it is retried here, once, on a fresh checkout.
//
// The other engines have no notices and run on the pool as RunContext
// does, and nil notices come back.
func (m *Manager) RunNotices(ctx context.Context, name, stmt string, args ...any) (*model.Result, []Notice, error) {
	dbh, err := m.DBContext(ctx, name)
	if err != nil {
		return nil, nil, err
	}
	cc, _ := m.cfg.ConnByName(name)
	if drv, _ := driverFor(cc.Driver); drv != "pgx" {
		res, err := m.run(ctx, dbh, name, stmt, args...)
		return res, nil, err
	}
	for retried := false; ; retried = true {
		res, notices, err := m.runNoticed(ctx, dbh, name, stmt, args...)
		if err != nil && !retried && errors.Is(err, driver.ErrBadConn) {
			continue
		}
		return res, notices, err
	}
}

// runNoticed is one attempt of RunNotices on a Postgres pool.
func (m *Manager) runNoticed(ctx context.Context, dbh *sql.DB, name, stmt string, args ...any) (*model.Result, []Notice, error) {
	c, err := dbh.Conn(ctx)
	if err != nil {
		return nil, nil, wrapRunErr(ctx, err, name, "op", "conn")
	}
	defer c.Close()
	buf, key := attachNotices(c.Raw)
	if key != nil {
		defer noticeSinks.Delete(key)
	}
	res, err := m.run(ctx, c, name, stmt, args...)
	var notices []Notice
	if buf != nil {
		notices = buf.take()
	}
	return res, notices, err
}
