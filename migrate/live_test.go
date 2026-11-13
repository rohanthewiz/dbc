package migrate

// Live tests for the run lock (N-270) and MySQL's partly applied report.
// They skip unless a DSN is set; with throwaway containers:
//
//	DBC_LIVE_PG_DSN='postgres://postgres:pw@127.0.0.1:55432/dbc?sslmode=disable' \
//	DBC_LIVE_MYSQL_DSN='root:pw@tcp(127.0.0.1:53306)/dbc' \
//	go test -count=1 ./migrate -run Live -v
//
// They drop and re-create goose_db_version and their own tables in the
// DSN's database, so point them at a scratch one.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/rohanthewiz/serr"
)

type liveEngine struct {
	name, env, driver string
	d                 Dialect
	sleep             string // a statement that takes a few hundred ms
}

var liveEngines = []liveEngine{
	{name: "postgres", env: "DBC_LIVE_PG_DSN", driver: "pgx", d: Postgres, sleep: "SELECT pg_sleep(0.3)"},
	{name: "mysql", env: "DBC_LIVE_MYSQL_DSN", driver: "mysql", d: MySQL, sleep: "DO SLEEP(0.3)"},
}

// liveOpen opens a handle on the engine's live database, or skips. Each call
// is its own pool, as a separate process's would be.
func liveOpen(t *testing.T, e liveEngine) *sql.DB {
	t.Helper()
	dsn := os.Getenv(e.env)
	if dsn == "" {
		t.Skipf("set %s to run against a live %s", e.env, e.name)
	}
	dbh, err := sql.Open(e.driver, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dbh.Close() })
	if err = dbh.Ping(); err != nil {
		t.Fatalf("%s: %v", e.name, err)
	}
	return dbh
}

// liveReset drops the version table and the named tables, now and after the
// test, so each test starts from an unmigrated database.
func liveReset(t *testing.T, dbh *sql.DB, tables ...string) {
	t.Helper()
	drop := func() {
		for _, tb := range append([]string{VersionTable}, tables...) {
			if _, err := dbh.Exec("DROP TABLE IF EXISTS " + tb); err != nil {
				t.Fatalf("drop %s: %v", tb, err)
			}
		}
	}
	drop()
	t.Cleanup(drop)
}

func eachLive(t *testing.T, fn func(t *testing.T, e liveEngine)) {
	for _, e := range liveEngines {
		t.Run(e.name, func(t *testing.T) { fn(t, e) })
	}
}

// TestLiveConcurrentRunsApplyOnce: several `up`s at once, each on its own
// pool as separate deploys would be. All succeed, the data migration lands
// once, each version has one row, and the runs that had to wait said who
// they waited for.
func TestLiveConcurrentRunsApplyOnce(t *testing.T) {
	withFastPoll(t)
	eachLive(t, func(t *testing.T, e liveEngine) {
		ctx := context.Background()
		check := liveOpen(t, e)
		liveReset(t, check, "live_counter")
		dir := writeMigrations(t, map[string]string{
			"10_counter.sql": "-- +goose Up\nCREATE TABLE live_counter (n int);\n-- +goose Down\nDROP TABLE live_counter;\n",
			// slow enough that every runner is waiting while it runs
			"20_bump.sql": "-- +goose Up\nINSERT INTO live_counter (n) VALUES (20);\n" + e.sleep + ";\n" +
				"-- +goose Down\nDELETE FROM live_counter WHERE n = 20;\n",
		})

		const runners = 6
		var (
			wg      sync.WaitGroup
			mu      sync.Mutex
			waiting []string
			applied int
		)
		start := make(chan struct{})
		errs := make([]error, runners)
		for i := 0; i < runners; i++ {
			m, err := New(liveOpen(t, e), e.d, dir)
			if err != nil {
				t.Fatal(err)
			}
			m.Log = func(s string) {
				mu.Lock()
				defer mu.Unlock()
				if strings.HasPrefix(s, "waiting for the migration lock") {
					waiting = append(waiting, s)
				}
			}
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				done, err := m.Up(ctx)
				errs[i] = err
				mu.Lock()
				applied += len(done)
				mu.Unlock()
			}(i)
		}
		close(start)
		wg.Wait()

		for i, err := range errs {
			if err != nil {
				t.Errorf("runner %d: %s", i, serr.StringFromErr(err))
			}
		}
		if applied != 2 {
			t.Errorf("runners applied %d migrations between them, want 2", applied)
		}
		if n := countRows(t, check, "SELECT count(*) FROM live_counter"); n != 1 {
			t.Errorf("live_counter holds %d rows, want 1", n)
		}
		for _, v := range []int{0, 10, 20} {
			q := fmt.Sprintf("SELECT count(*) FROM %s WHERE version_id = %d", VersionTable, v)
			if n := countRows(t, check, q); n != 1 {
				t.Errorf("version %d has %d rows, want 1", v, n)
			}
		}
		// the holder query works: at least one waiter named a session
		named := false
		for _, w := range waiting {
			named = named || strings.Contains(w, "(pid ") || strings.Contains(w, "(connection ")
		}
		if !named {
			t.Errorf("no waiter named the lock's holder: %q", waiting)
		}
	})
}

// TestLiveLockWaitsForHolder holds the lock by hand from another session —
// on Postgres by goose's key, so a goose run and a dbc run exclude each
// other — and checks that a run waits, names that session, gives up with
// its context, and goes through once the lock is free.
func TestLiveLockWaitsForHolder(t *testing.T) {
	withFastPoll(t)
	eachLive(t, func(t *testing.T, e liveEngine) {
		ctx := context.Background()
		dbh := liveOpen(t, e)
		liveReset(t, dbh, "a")
		dir := writeMigrations(t, map[string]string{"10_a.sql": migA})

		holder, err := liveOpen(t, e).Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer holder.Close()
		var who string
		switch e.name {
		case "postgres":
			if _, err = holder.ExecContext(ctx, "SELECT pg_advisory_lock(5887940537704921958)"); err != nil {
				t.Fatal(err)
			}
			var pid int
			_ = holder.QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&pid)
			who = fmt.Sprintf("(pid %d)", pid)
		case "mysql":
			var got int
			if err = holder.QueryRowContext(ctx, "SELECT GET_LOCK("+myLockName+", 0)").Scan(&got); err != nil || got != 1 {
				t.Fatalf("GET_LOCK: %d %v", got, err)
			}
			var id int
			_ = holder.QueryRowContext(ctx, "SELECT CONNECTION_ID()").Scan(&id)
			who = fmt.Sprintf("(connection %d)", id)
		}

		m, _ := New(dbh, e.d, dir)
		var log []string
		m.Log = func(s string) { log = append(log, s) }
		wctx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
		_, err = m.Up(wctx)
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want the deadline while the lock is held", err)
		}
		if len(log) != 1 || log[0] != "waiting for the migration lock: another run holds it "+who {
			t.Errorf("log = %q, want the holder %s named", log, who)
		}
		if tableExists(t, dbh, "a") {
			t.Error("the run went ahead without the lock")
		}

		switch e.name {
		case "postgres":
			_, err = holder.ExecContext(ctx, "SELECT pg_advisory_unlock(5887940537704921958)")
		case "mysql":
			_, err = holder.ExecContext(ctx, "SELECT RELEASE_LOCK("+myLockName+")")
		}
		if err != nil {
			t.Fatal(err)
		}
		if done, err := m.Up(ctx); err != nil || len(done) != 1 {
			t.Fatalf("up once free: %v %+v", err, done)
		}
		// and the run released it: the holder can take it again at once
		var free bool
		switch e.name {
		case "postgres":
			_ = holder.QueryRowContext(ctx, "SELECT pg_try_advisory_lock(5887940537704921958)").Scan(&free)
			_, _ = holder.ExecContext(ctx, "SELECT pg_advisory_unlock(5887940537704921958)")
		case "mysql":
			var got int
			_ = holder.QueryRowContext(ctx, "SELECT GET_LOCK("+myLockName+", 0)").Scan(&got)
			free = got == 1
			_, _ = holder.ExecContext(ctx, "SELECT RELEASE_LOCK("+myLockName+")")
		}
		if !free {
			t.Error("the run kept the lock after it ended")
		}
	})
}

// TestLivePostgresConcurrentIndexWhileWaiting is why the Postgres lock is
// polled: a run's CREATE INDEX CONCURRENTLY waits for every older snapshot,
// and a second run blocked inside pg_advisory_lock would hold one for as
// long as it waits — each waiting for the other until Postgres failed one
// of them. Polling holds no snapshot between tries, so both runs finish.
func TestLivePostgresConcurrentIndexWhileWaiting(t *testing.T) {
	withFastPoll(t)
	e := liveEngines[0]
	ctx := context.Background()
	check := liveOpen(t, e)
	liveReset(t, check, "live_ci")
	dir := writeMigrations(t, map[string]string{
		// a write and a pause: the second run starts waiting while this
		// transaction is open, so its snapshot predates the index build
		"10_ci.sql": "-- +goose Up\nCREATE TABLE live_ci (id int);\nINSERT INTO live_ci VALUES (1);\nSELECT pg_sleep(0.5);\n" +
			"-- +goose Down\nDROP TABLE live_ci;\n",
		"20_ci_idx.sql": "-- +goose NO TRANSACTION\n-- +goose Up\nCREATE INDEX CONCURRENTLY live_ci_id ON live_ci (id);\n" +
			"-- +goose Down\nDROP INDEX CONCURRENTLY live_ci_id;\n",
	})

	first, _ := New(liveOpen(t, e), e.d, dir)
	second, _ := New(liveOpen(t, e), e.d, dir)
	started := make(chan struct{})
	first.Log = func(string) {}
	var once sync.Once
	second.Log = func(s string) {
		if strings.HasPrefix(s, "waiting") {
			once.Do(func() { close(started) })
		}
	}

	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	var errFirst, errSecond error
	wg.Add(2)
	go func() { defer wg.Done(); _, errFirst = first.Up(rctx) }()
	// start the second once the first holds the lock, so it waits through
	// migration 10's open transaction (only these runs take advisory locks)
	go func() {
		defer wg.Done()
		for rctx.Err() == nil {
			var n int
			if check.QueryRowContext(rctx, "SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND granted").
				Scan(&n) == nil && n > 0 {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		_, errSecond = second.Up(rctx)
	}()
	select {
	case <-started:
	case <-rctx.Done():
		t.Fatal("the second run never waited for the lock")
	}
	wg.Wait()
	if errFirst != nil {
		t.Errorf("first run: %s", serr.StringFromErr(errFirst))
	}
	if errSecond != nil {
		t.Errorf("second run: %s", serr.StringFromErr(errSecond))
	}
	if n := countRows(t, check, "SELECT count(*) FROM pg_indexes WHERE indexname = 'live_ci_id'"); n != 1 {
		t.Errorf("index live_ci_id: %d, want 1", n)
	}
}

// TestLivePartlyApplied runs failing migrations on both engines. Postgres
// rolls everything back, so nothing is reported kept; MySQL commits DDL as
// it runs, and the error says which statements stayed — checked against
// what actually stayed.
func TestLivePartlyApplied(t *testing.T) {
	cases := []struct {
		name, up string
		row      int // the row the migration inserts, to look for after
		kept     int // statements MySQL keeps (Postgres always 0)
	}{
		{"ddl then failing dml",
			"CREATE TABLE live_pa_b (id int);\nINSERT INTO live_pa VALUES (1);\nINSERT INTO live_pa_nope VALUES (1);\n", 1, 2},
		{"dml then ddl failing to run",
			"INSERT INTO live_pa VALUES (2);\nALTER TABLE live_pa ADD COLUMN id int;\n", 2, 1},
		{"dml then ddl syntax error",
			"INSERT INTO live_pa VALUES (3);\nALTER TABLE live_pa ADD COLUMN (;\n", 3, 0},
		{"temporary table does not commit",
			"INSERT INTO live_pa VALUES (4);\nCREATE TEMPORARY TABLE live_pa_tmp (id int);\nINSERT INTO live_pa_nope VALUES (1);\n", 4, 0},
	}
	eachLive(t, func(t *testing.T, e liveEngine) {
		ctx := context.Background()
		dbh := liveOpen(t, e)
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				liveReset(t, dbh, "live_pa", "live_pa_b")
				if _, err := dbh.Exec("CREATE TABLE live_pa (id int)"); err != nil {
					t.Fatal(err)
				}
				dir := writeMigrations(t, map[string]string{
					"10_pa.sql": "-- +goose Up\n" + c.up + "-- +goose Down\nSELECT 1;\n",
				})
				m, _ := New(dbh, e.d, dir)
				var log []string
				m.Log = func(s string) { log = append(log, s) }
				if _, err := m.Up(ctx); err == nil {
					t.Fatal("expected the migration to fail")
				} else {
					want := c.kept
					if e.name == "postgres" {
						want = 0
					}
					s := serr.StringFromErr(err)
					switch {
					case want == 0 && strings.Contains(s, "partly_applied"):
						t.Errorf("reported partly applied: %s", s)
					case want == 1 && !strings.Contains(s, "statement 1 of"):
						t.Errorf("want statement 1 kept: %s", s)
					case want > 1 && !strings.Contains(s, fmt.Sprintf("statements 1-%d of", want)):
						t.Errorf("want statements 1-%d kept: %s", want, s)
					}
					if (want > 0) != (len(log) == 1 && strings.HasPrefix(log[0], "PARTIAL 10_pa.sql: ")) {
						t.Errorf("log = %q", log)
					}
					// what stayed is what the report said: the row is there
					// exactly when its INSERT was among the kept statements
					got := countRows(t, dbh, fmt.Sprintf("SELECT count(*) FROM live_pa WHERE id = %d", c.row))
					if (got == 1) != (want > 0) {
						t.Errorf("row %d present=%v, but kept=%d", c.row, got == 1, want)
					}
				}
				if v, _ := m.Version(ctx); v != 0 {
					t.Errorf("version = %d, want 0: a failed migration is not recorded", v)
				}
			})
		}
	})
}
