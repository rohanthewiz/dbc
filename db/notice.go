package db

import (
	"strconv"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5/pgconn"
	pgxstdlib "github.com/jackc/pgx/v5/stdlib"
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
// A notice on a connection no Session holds — the pooled RunContext path
// (catalog queries, a script's Query) — finds no buffer and is dropped, as
// every notice was before. Those runs have no log line to put it on.
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
