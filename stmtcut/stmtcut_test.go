package stmtcut

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A fake driver whose rows stand in for a server still sending: Close
// "drains" until the test's drained channel closes, which a stop does
// (as the server's error ends a real drain), or at once when the test
// gives it no channel (the rest was already in).

type fakeDriver struct{}

// drains is the channel the next query's Close waits on; nil: no wait.
var (
	drainMu sync.Mutex
	drains  chan struct{}
)

func (fakeDriver) Open(string) (driver.Conn, error) { return fakeConn{}, nil }

type fakeConn struct{}

func (fakeConn) Prepare(string) (driver.Stmt, error) { return nil, driver.ErrSkip }
func (fakeConn) Close() error                        { return nil }
func (fakeConn) Begin() (driver.Tx, error)           { return nil, driver.ErrSkip }
func (fakeConn) QueryContext(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
	drainMu.Lock()
	defer drainMu.Unlock()
	return &fakeRows{drained: drains}, nil
}

type fakeRows struct {
	drained chan struct{}
	n       int
}

func (*fakeRows) Columns() []string { return []string{"n"} }
func (r *fakeRows) Next(dest []driver.Value) error {
	r.n++
	dest[0] = int64(r.n) // an endless result
	return nil
}
func (r *fakeRows) Close() error {
	if r.drained != nil {
		<-r.drained
	}
	return nil
}

func init() { sql.Register("stmtcut-fake", fakeDriver{}) }

// openRows runs a query on the fake driver, its Close waiting on drained,
// and reads one row of it, as a reader that stopped early would have.
func openRows(t *testing.T, ctx context.Context, drained chan struct{}) *sql.Rows {
	t.Helper()
	db, err := sql.Open("stmtcut-fake", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	drainMu.Lock()
	drains = drained
	drainMu.Unlock()
	rows, err := db.QueryContext(ctx, "SELECT")
	if err != nil {
		t.Fatal(err)
	}
	if !rows.Next() {
		t.Fatal("no first row")
	}
	return rows
}

func withGrace(t *testing.T, d time.Duration) {
	old := Grace
	Grace = d
	t.Cleanup(func() { Grace = old })
}

// A rest that arrives within Grace is never stopped.
func TestCloseRestArrivesWithinGrace(t *testing.T) {
	withGrace(t, time.Hour)
	ctx, p := Watch(context.Background())
	defer p.Release()
	var stops atomic.Int32
	p.SetKill(func() { stops.Add(1) })
	rows := openRows(t, ctx, nil)
	if p.Close(rows, true) {
		t.Error("Close reported a stop")
	}
	if stops.Load() != 0 {
		t.Error("a stop was sent for a rest that was already in")
	}
}

// A rest still arriving after Grace is stopped, and Close returns only once
// the stop has: a KILL in flight must not reach the connection's next
// statement.
func TestCloseStopsAndWaitsForTheStop(t *testing.T) {
	withGrace(t, 20*time.Millisecond)
	ctx, p := Watch(context.Background())
	defer p.Release()
	drained := make(chan struct{})
	var stops atomic.Int32
	var finished atomic.Bool
	p.SetKill(func() {
		stops.Add(1)
		close(drained) // the server ends the statement: the drain stops
		time.Sleep(30 * time.Millisecond)
		finished.Store(true) // the KILL's own round trip completes
	})
	rows := openRows(t, ctx, drained)
	start := time.Now()
	if !p.Close(rows, true) {
		t.Error("Close reported no stop")
	}
	if took := time.Since(start); took < 20*time.Millisecond {
		t.Errorf("stopped after %s, before Grace", took)
	}
	if stops.Load() != 1 {
		t.Errorf("%d stops, want 1", stops.Load())
	}
	if !finished.Load() {
		t.Error("Close returned before the stop finished")
	}
}

// A Postgres stop cancels the statement's own context — the one Watch
// returned — and not the caller's.
func TestCloseCancelsTheStatementContext(t *testing.T) {
	withGrace(t, 0)
	parent := t.Context()
	ctx, p := Watch(parent)
	defer p.Release()
	p.SetCancels(false)
	drained := make(chan struct{})
	// what pgx does on a cancel: a cancel request, then the drain ends
	go func() {
		<-ctx.Done()
		close(drained)
	}()
	rows := openRows(t, ctx, drained)
	if !p.Close(rows, true) {
		t.Error("Close reported no stop")
	}
	if ctx.Err() == nil {
		t.Error("the statement's context was not canceled")
	}
	if parent.Err() != nil {
		t.Error("the caller's context was canceled")
	}
}

// What a stop would be, by what the driver side said.
func TestStopper(t *testing.T) {
	kill := func() {}
	for _, c := range []struct {
		name          string
		kill          bool
		cancels, inTx bool
		keepTx        bool
		want          string // "kill", "cancel", ""
	}{
		{name: "nothing said: read to the end", want: ""},
		{name: "MySQL", kill: true, want: "kill"},
		{name: "MySQL in a transaction", kill: true, inTx: true, cancels: true, keepTx: true, want: "kill"},
		{name: "Postgres", cancels: true, keepTx: true, want: "cancel"},
		{name: "Postgres in the user's transaction", cancels: true, inTx: true, keepTx: true, want: ""},
		{name: "Postgres in the holder's own transaction", cancels: true, inTx: true, want: "cancel"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, p := Watch(context.Background())
			defer p.Release()
			if c.cancels {
				p.SetCancels(c.inTx)
			}
			if c.kill {
				p.SetKill(kill)
			}
			got := ""
			switch f := p.stopper(c.keepTx); {
			case f == nil:
			case c.kill:
				got = "kill" // only SetKill's func is ever set alongside
			default:
				got = "cancel"
			}
			if got != c.want {
				t.Errorf("stop = %q, want %q", got, c.want)
			}
		})
	}
}

// With no way to stop the statement, Close just closes: the rest is read,
// as before.
func TestCloseWithoutStopper(t *testing.T) {
	withGrace(t, 0)
	ctx, p := Watch(context.Background())
	defer p.Release()
	rows := openRows(t, ctx, nil)
	if p.Close(rows, true) {
		t.Error("Close reported a stop it had no way to send")
	}
	if err := rows.Err(); err != nil && err != io.EOF {
		t.Error(err)
	}
}

func TestFromAndRelease(t *testing.T) {
	if From(context.Background()) != nil {
		t.Error("From found a Point in a bare context")
	}
	ctx, p := Watch(context.Background())
	if From(ctx) != p {
		t.Error("From did not find the Point Watch made")
	}
	p.Release()
	p.Release() // twice is fine
	if ctx.Err() == nil {
		t.Error("Release left the statement's context running")
	}
}
