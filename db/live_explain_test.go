package db

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rohanthewiz/serr"

	"github.com/rohanthewiz/dbc/explain"
)

// Live EXPLAIN tests: the JSON and tree formats, analyze, and above all the
// promise that analyzing a write changes nothing — including inside a
// transaction the user has open. Opt-in, like the other live tests (see
// live_test.go for the DSN variables).

func livePGExplainSetup(t *testing.T) *Manager {
	mgr := liveMgr(t, "DBC_LIVE_PG_DSN", "postgres")
	liveExec(t, mgr,
		"DROP SCHEMA IF EXISTS dbc_explain CASCADE",
		"CREATE SCHEMA dbc_explain",
		"CREATE TABLE dbc_explain.orders (id int PRIMARY KEY, user_id int, total numeric, status text)",
		"INSERT INTO dbc_explain.orders SELECT g, 1 + g % 5000, g % 300, CASE WHEN g % 4 = 0 THEN 'paid' ELSE 'new' END FROM generate_series(1, 50000) g",
		"ANALYZE dbc_explain.orders",
	)
	t.Cleanup(func() { _, _ = mgr.Run("live", "DROP SCHEMA IF EXISTS dbc_explain CASCADE") })
	return mgr
}

func TestLiveExplainPostgres(t *testing.T) {
	mgr := livePGExplainSetup(t)
	ctx := context.Background()

	p, err := mgr.Explain(ctx, "live", "SELECT * FROM dbc_explain.orders WHERE user_id = 42", ExplainOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if p.Analyzed || p.Metric != explain.MetricCost || !strings.Contains(p.Command, "FORMAT JSON") {
		t.Errorf("estimate: analyzed=%v metric=%s command=%q", p.Analyzed, p.Metric, p.Command)
	}

	p, err = mgr.Explain(ctx, "live", "SELECT * FROM dbc_explain.orders WHERE user_id = 42", ExplainOptions{Analyze: true})
	if err != nil {
		t.Fatal(err)
	}
	if !p.Analyzed || p.ExecutionMs <= 0 || p.Metric != explain.MetricTime {
		t.Errorf("analyze: analyzed=%v exec=%v metric=%s", p.Analyzed, p.ExecutionMs, p.Metric)
	}
	if !strings.Contains(insightTitles(p), "keeps 10 of 50,000 rows") {
		t.Errorf("want the missing-index finding:\n%s", insightTitles(p))
	}
	found := false
	for _, in := range p.Insights {
		if in.SQL == "CREATE INDEX idx_orders_user_id ON dbc_explain.orders (user_id);" {
			found = true
		}
	}
	if !found {
		t.Errorf("want a CREATE INDEX suggestion on dbc_explain.orders:\n%s", p.Text(explain.TextOptions{Insights: true}))
	}

	// $1 plans generically on Postgres 16+
	p, err = mgr.Explain(ctx, "live", "SELECT * FROM dbc_explain.orders WHERE user_id = $1", ExplainOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Notes) == 0 || !strings.Contains(p.Notes[0], "generic plan") {
		t.Errorf("notes = %v", p.Notes)
	}
}

// Analyzing a write outside any transaction: BEGIN … ROLLBACK around it.
func TestLiveExplainPostgresWriteRolledBack(t *testing.T) {
	mgr := livePGExplainSetup(t)
	ctx := context.Background()
	p, err := mgr.Explain(ctx, "live", "UPDATE dbc_explain.orders SET total = -1 WHERE id <= 100", ExplainOptions{Analyze: true})
	if err != nil {
		t.Fatal(err)
	}
	if !p.Analyzed || p.Root.Kind != explain.KindModify {
		t.Errorf("want an analyzed Update:\n%s", p.Text(explain.TextOptions{}))
	}
	if len(p.Notes) == 0 || !strings.Contains(p.Notes[0], "rolled back") {
		t.Errorf("notes = %v", p.Notes)
	}
	res, err := mgr.Run("live", "SELECT count(*) FROM dbc_explain.orders WHERE total = -1")
	if err != nil {
		t.Fatal(err)
	}
	if res.Rows[0][0] != "0" {
		t.Fatalf("the analyzed UPDATE was kept: %s rows now have total = -1", res.Rows[0][0])
	}
}

// Analyzing a write inside the user's own open transaction: a savepoint, so
// the user's uncommitted work survives and only the analyzed write is undone.
func TestLiveExplainPostgresWriteInUserTx(t *testing.T) {
	mgr := livePGExplainSetup(t)
	ctx := context.Background()
	sess, err := mgr.Session(ctx, "live")
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	run := func(q string) string {
		t.Helper()
		res, err := sess.Run(ctx, q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return res.Rows[0][0]
	}
	run("BEGIN")
	run("INSERT INTO dbc_explain.orders VALUES (999999, 1, 5, 'mine')")

	p, err := sess.Explain(ctx, "UPDATE dbc_explain.orders SET status = 'x'", ExplainOptions{Analyze: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(p.Notes, " "), "savepoint") {
		t.Errorf("notes = %v", p.Notes)
	}
	if got := run("SELECT count(*) FROM dbc_explain.orders WHERE status = 'x'"); got != "0" {
		t.Errorf("the analyzed UPDATE survived the savepoint: %s rows", got)
	}
	if got := run("SELECT status FROM dbc_explain.orders WHERE id = 999999"); got != "mine" {
		t.Errorf("the user's own uncommitted insert was lost: status %q", got)
	}
	run("ROLLBACK")

	// in a FAILED transaction nothing can run; say so rather than try
	run("BEGIN")
	if _, err := sess.Run(ctx, "SELECT 1/0"); err == nil {
		t.Fatal("expected division by zero")
	}
	if _, err := sess.Explain(ctx, "UPDATE dbc_explain.orders SET status = 'y'", ExplainOptions{Analyze: true}); err == nil ||
		!strings.Contains(err.Error(), "failed") {
		t.Errorf("want a refusal in a failed transaction, got %v", err)
	}
	run("ROLLBACK")
}

// A plain read is analyzed inside the rollback too (N-272): its text cannot
// show a write made by a function it calls, so SELECT f() that updates
// rows changes none of them. And a read that takes a session advisory lock
// — which no rollback releases — leaves the session stateful, as running
// it would.
func TestLiveExplainPostgresReadThatWrites(t *testing.T) {
	mgr := livePGExplainSetup(t)
	liveExec(t, mgr, `CREATE FUNCTION dbc_explain.zero() RETURNS int LANGUAGE sql AS $$
		UPDATE dbc_explain.orders SET total = -1 WHERE id <= 100 RETURNING 1 $$`)
	ctx := context.Background()
	sess, err := mgr.Session(ctx, "live")
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	p, err := sess.Explain(ctx, "SELECT dbc_explain.zero()", ExplainOptions{Analyze: true})
	if err != nil {
		t.Fatal(err)
	}
	if !p.Analyzed {
		t.Errorf("not analyzed:\n%s", p.Text(explain.TextOptions{}))
	}
	if strings.Contains(strings.Join(p.Notes, " "), "rolled back") {
		t.Errorf("a plain read's analyze talks of a rollback: %v", p.Notes)
	}
	if n := liveVal(t, onPool(mgr), "SELECT count(*) FROM dbc_explain.orders WHERE total = -1"); n != "0" {
		t.Fatalf("the analyzed SELECT's function kept its UPDATE: %s rows", n)
	}
	if sess.Stateful() {
		t.Error("stateful after analyzing a plain read")
	}

	if _, err = sess.Explain(ctx, "SELECT pg_advisory_lock(272)", ExplainOptions{Analyze: true}); err != nil {
		t.Fatal(err)
	}
	if n := liveVal(t, onPool(mgr), "SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND objid = 272 AND granted"); n != "1" {
		t.Fatalf("advisory locks held after the analyze: %s, want the session's 1 (the rollback does not release it)", n)
	}
	if !sess.Stateful() {
		t.Error("not stateful while holding the advisory lock the analyze took")
	}
}

func TestLiveExplainMySQL(t *testing.T) {
	mgr := liveMgr(t, "DBC_LIVE_MYSQL_DSN", "mysql")
	liveExec(t, mgr,
		"DROP TABLE IF EXISTS dbc_live_explain",
		"CREATE TABLE dbc_live_explain (id int PRIMARY KEY, user_id int, total int)",
		"INSERT /*+ SET_VAR(cte_max_recursion_depth = 30000) */ INTO dbc_live_explain WITH RECURSIVE g(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM g WHERE n < 20000) "+
			"SELECT n, 1 + n % 2000, n % 300 FROM g",
		"ANALYZE TABLE dbc_live_explain",
	)
	t.Cleanup(func() { _, _ = mgr.Run("live", "DROP TABLE IF EXISTS dbc_live_explain") })
	ctx := context.Background()

	p, err := mgr.Explain(ctx, "live", "SELECT * FROM dbc_live_explain WHERE user_id = 42", ExplainOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p.Command, "FORMAT=TREE") || p.Analyzed {
		t.Errorf("command=%q analyzed=%v", p.Command, p.Analyzed)
	}

	p, err = mgr.Explain(ctx, "live", "SELECT * FROM dbc_live_explain WHERE user_id = 42", ExplainOptions{Analyze: true})
	if err != nil {
		t.Fatal(err)
	}
	if !p.Analyzed || !strings.Contains(insightTitles(p), "keeps 10 of 20,000 rows") {
		t.Errorf("analyzed=%v\n%s", p.Analyzed, p.Text(explain.TextOptions{Insights: true}))
	}

	// a single-table UPDATE: the tree format cannot describe it, the table
	// format can; and analyze is refused, so nothing changes
	p, err = mgr.Explain(ctx, "live", "UPDATE dbc_live_explain SET total = -1", ExplainOptions{Analyze: true})
	if err != nil {
		t.Fatal(err)
	}
	if p.Analyzed || len(p.Notes) == 0 || !strings.Contains(p.Notes[0], "not analyzed") {
		t.Errorf("analyzed=%v notes=%v", p.Analyzed, p.Notes)
	}
	res, err := mgr.Run("live", "SELECT count(*) FROM dbc_live_explain WHERE total = -1")
	if err != nil {
		t.Fatal(err)
	}
	if res.Rows[0][0] != "0" {
		t.Fatalf("explaining the UPDATE ran it")
	}

	// a read that takes a named lock (N-272): analyze is refused as for a
	// write, so the lock is not taken behind the user's back
	p, err = mgr.Explain(ctx, "live", "SELECT GET_LOCK('dbc_272', 0)", ExplainOptions{Analyze: true})
	if err != nil {
		t.Fatal(err)
	}
	if p.Analyzed || len(p.Notes) == 0 || !strings.Contains(p.Notes[0], "not analyzed") || !strings.Contains(p.Notes[0], "lock") {
		t.Errorf("analyzed=%v notes=%v", p.Analyzed, p.Notes)
	}
	if res, err = mgr.Run("live", "SELECT IS_USED_LOCK('dbc_272') IS NULL"); err != nil {
		t.Fatal(err)
	}
	if res.Rows[0][0] != "1" {
		t.Fatalf("explaining GET_LOCK took the lock")
	}
}

// Stopping an EXPLAIN ANALYZE of a write (N-271). The write runs inside
// dbc's own BEGIN … ROLLBACK, or a savepoint in the user's transaction, and
// the stop must leave nothing of it behind while saying the right thing
// about the session:
//
//	                   cancel request lands           grace runs out (0 here):
//	                   (the normal case)              the connection is closed
//	no transaction     stopped; session kept          stopped; session dropped
//	                                                  quietly (it held nothing)
//	user's open tx     stopped; the savepoint undone, stopped; session lost —
//	                   the user's transaction intact  the user's tx went with it
//
// Before N-271 every stop took the right-hand column, and reported it as a
// failed rollback: "the analyzed write may not have been undone", the
// session lost — though the server rolls back an open transaction when its
// connection ends, so nothing could have been kept.
//
// A plain read is analyzed inside the same rollback since N-272, so its stop
// takes the left-hand column too: inside the user's transaction the
// savepoint closes what the stop aborted, where a read analyzed bare left
// the transaction aborted (ErrTxAborted).
func TestLiveExplainPostgresStopWrite(t *testing.T) {
	const slowWrite = "UPDATE dbc_explain.orders SET total = -1 WHERE id <= 100 AND (SELECT true FROM pg_sleep(60))"
	const slowRead = "SELECT pg_sleep(60)"
	cases := []struct {
		name   string
		grace  time.Duration
		userTx bool
		fault  Fault
		stmt   string // slowWrite when ""
	}{
		{"kept, no transaction", pgCancelGrace, false, FaultNone, ""},
		{"kept, user transaction", pgCancelGrace, true, FaultNone, ""},
		{"closed, no transaction", 0, false, FaultDrop, ""},
		{"closed, user transaction", 0, true, FaultLost, ""},
		{"kept, user transaction, a read", pgCancelGrace, true, FaultNone, slowRead},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			old := pgCancelGrace
			pgCancelGrace = c.grace
			defer func() { pgCancelGrace = old }()
			mgr := livePGExplainSetup(t) // its pool's connections take this grace
			obs := liveMgr(t, "DBC_LIVE_PG_DSN", "postgres")
			ctx := context.Background()
			s, err := mgr.Session(ctx, "live")
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			id := liveVal(t, onSession(s), "SELECT pg_backend_pid()")
			if c.userTx {
				liveVal(t, onSession(s), "SELECT 1") // keep id's read out of the tx
				for _, q := range []string{"BEGIN", "INSERT INTO dbc_explain.orders VALUES (999999, 1, 5, 'mine')"} {
					if _, err = s.Run(ctx, q); err != nil {
						t.Fatalf("%s: %v", q, err)
					}
				}
			}

			sctx, stop := context.WithCancel(ctx)
			done := make(chan error, 1)
			stmt := c.stmt
			if stmt == "" {
				stmt = slowWrite
			}
			go func() {
				_, err := s.Explain(sctx, stmt, ExplainOptions{Analyze: true})
				done <- err
			}()
			waitBusy(t, obs, liveEngines[0], id, "1", 5*time.Second)
			stop()
			select {
			case err = <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("the stopped analyze never returned")
			}
			if !errors.Is(err, ErrCanceled) {
				t.Fatalf("err = %v, want ErrCanceled", err)
			}
			if s := serr.StringFromErr(err); strings.Contains(s, "may not have been undone") {
				t.Errorf("reported a write that cannot have been kept: %s", s)
			}
			if errors.Is(err, ErrTxAborted) {
				t.Errorf("the rollback should have closed what the stop aborted: %v", err)
			}
			if got := s.Classify(err); got != c.fault {
				t.Errorf("Classify = %d, want %d", got, c.fault)
			}
			waitBusy(t, obs, liveEngines[0], id, "0", 3*time.Second)
			if n := liveVal(t, onPool(obs), "SELECT count(*) FROM dbc_explain.orders WHERE total = -1"); n != "0" {
				t.Errorf("the stopped write was kept: %s rows", n)
			}
			if c.fault == FaultNone && c.userTx {
				// the user's transaction is as it was: open, not aborted,
				// their insert still in it
				if st := s.pgTxStatus(); st != 'T' {
					t.Errorf("tx status = %q, want T", st)
				}
				if got := liveVal(t, onSession(s), "SELECT status FROM dbc_explain.orders WHERE id = 999999"); got != "mine" {
					t.Errorf("the user's uncommitted insert: %q", got)
				}
			}
		})
	}
}
