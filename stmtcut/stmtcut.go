// Package stmtcut ends a statement on the server once its reader has every
// row it wants, so that closing its rows does not read the rest first
// (N-276).
//
// Closing database/sql rows part way through a result is not cheap on a
// server engine: the drivers finish the statement before the connection can
// take another, by reading and dropping every row still to come. pgx's
// ResultReader.Close reads until the command concludes; go-sql-driver/mysql's
// Rows.Close skips rows to the end. A run cut at max_rows over a 100M-row
// table kept its 1,000 rows and stayed "running" while the server shipped
// the rest — measured on a local Postgres 17 and MySQL 8.4: 1.9 s and 3.4 s
// for 1,000 rows of a 3M-row result, growing with the result. So did a
// pipeline Preview of 50 rows, a script that stopped reading early, and a
// catalog read cut at its bound.
//
// How a statement is stopped depends on the engine, and the driver side
// (package db, which opens every pool) says which applies to the connection
// the statement went out on, through the Point its context carries:
//
//	engine    stop                                          the connection
//	MySQL     KILL QUERY <its id>, over a new connection    kept, transaction too
//	          (SetKill, from db's MySQL connector)          (only the statement ends)
//	Postgres  cancel the statement's own context; db's      kept; but a transaction it
//	          pgx handler sends a cancel request            ran in is aborted, so a
//	          (SetCancels)                                  holder may refuse (keepTx)
//	others    nothing: SQLite and bytdb stop reading        —
//	          without draining
//
// Why not a plain context cancel on MySQL: the driver stops watching the
// context as soon as Rows.Close begins, and before that a cancel makes it
// close the connection — a pinned session's SETs and transaction with it.
// A KILL QUERY ends only the statement, and its connection reads the
// server's "interrupted" error and carries on.
//
// The stop waits Grace first. Most results cut at a cap end soon after it,
// and then the rest arrives faster than a stop could be sent: a Postgres
// cancel costs a second connection and pgx's 100 ms hold on the next
// statement, a KILL a second connection. Only a result still arriving after
// Grace is stopped:
//
//	Close(rows) ──► timer(Grace) ────────────────────────► Stop ──► KILL QUERY / cancel
//	          └──► rows.Close() reads what is left:            │
//	                 the rest, when it ends within Grace;      │
//	                 else up to the server's error ◄───────────┘
//	          then waits for a Stop that began, so a KILL still
//	          in flight cannot land on the connection's next statement
package stmtcut

import (
	"context"
	"database/sql"
	"sync"
	"time"
)

// Grace is how long a result left part way may go on arriving before its
// statement is stopped. A variable for tests, which set 0 to stop at once.
var Grace = 100 * time.Millisecond

// key is the context key a Point travels under.
type key struct{}

// Point is one statement's handle for being ended early. The holder makes
// it with Watch and runs the statement under the context Watch returns; the
// driver side finds it there (From) when the statement goes out and records
// how to stop it (SetKill, SetCancels).
type Point struct {
	cancel context.CancelFunc // the statement's own context

	mu      sync.Mutex
	kill    func() // ends the statement on the server, keeping its connection
	cancels bool   // canceling the context stops the statement and keeps the connection
	inTx    bool   // the statement went out inside a transaction (with cancels)
}

// Watch returns a context for one statement — a child of ctx carrying a
// fresh Point — and the Point. Run only that statement under it, and
// Release the Point once the rows are closed.
func Watch(ctx context.Context) (context.Context, *Point) {
	ctx, cancel := context.WithCancel(ctx)
	p := &Point{cancel: cancel}
	return context.WithValue(ctx, key{}, p), p
}

// From returns the Point ctx carries, or nil. The driver side calls it as a
// statement goes out.
func From(ctx context.Context) *Point {
	p, _ := ctx.Value(key{}).(*Point)
	return p
}

// SetKill records how to end the statement on the server while keeping
// its connection: MySQL's KILL QUERY on the connection's id. It takes
// precedence over SetCancels.
func (p *Point) SetKill(kill func()) {
	p.mu.Lock()
	p.kill = kill
	p.mu.Unlock()
}

// SetCancels records that canceling the statement's context stops it on
// the server (Postgres), and whether the statement went out inside a
// transaction, which such a stop aborts. db's pgx tracer calls it, for a
// pool whose cancel handler also keeps the connection. A holder may call it
// itself, before the statement goes out, when it knows its engine stops a
// statement on a cancel and can spare the connection if that costs it:
// etl's Reader does, for the transaction it owns.
func (p *Point) SetCancels(inTx bool) {
	p.mu.Lock()
	p.cancels, p.inTx = true, inTx
	p.mu.Unlock()
}

// stopper returns what Stop would do, or nil when it would do nothing.
// keepTx refuses a stop that would abort the transaction the statement
// runs in: a user's open BEGIN must not end because dbc stopped reading.
func (p *Point) stopper(keepTx bool) func() {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case p.kill != nil:
		return p.kill
	case p.cancels && !(p.inTx && keepTx):
		return p.cancel
	}
	return nil
}

// Close closes rows that their reader left part way, stopping the statement
// on the server if the rest is still arriving after Grace (see the package
// comment). It reports whether a stop was sent. rows.Close's error is not
// returned: once stopped, the statement's error is the stop's own; a holder
// that wants to know whether its caller canceled too asks its own context.
//
// keepTx: see stopper. With it, a Postgres statement inside a transaction
// is read to the end, as before.
func (p *Point) Close(rows *sql.Rows, keepTx bool) (stopped bool) {
	stop := p.stopper(keepTx)
	if stop == nil {
		_ = rows.Close()
		return false
	}
	fired := make(chan struct{})
	t := time.AfterFunc(Grace, func() {
		defer close(fired)
		stop()
	})
	_ = rows.Close()
	if t.Stop() {
		return false // the rest arrived within Grace; nothing was sent
	}
	// The timer fired: wait for its stop to finish. For MySQL that is the
	// KILL QUERY's round trip; until it returns, the KILL could still reach
	// the server after this connection's next statement has started.
	<-fired
	return true
}

// Release frees the statement's context. Call it once the rows are closed;
// it is safe to call more than once.
func (p *Point) Release() { p.cancel() }
