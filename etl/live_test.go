package etl

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Live Postgres tests: opt-in, like db/live_*. They need DBC_LIVE_PG_DSN
// and copy between two databases on that server — the DSN's own and
// "<db>_etl2", created here when missing — so source and destination are
// genuinely two connections, as they would be between two servers.
//
//	DBC_LIVE_PG_DSN='postgres://postgres:pw@127.0.0.1:5432/dbc?sslmode=disable' \
//	go test ./etl -run Live -v
//
// Both sides work in an etl_live schema, dropped first and on cleanup.

// livePG returns the source and destination connections, or a skip.
func livePG(t *testing.T) (src, dst Conn) {
	t.Helper()
	dsn := os.Getenv("DBC_LIVE_PG_DSN")
	if dsn == "" {
		t.Skip("set DBC_LIVE_PG_DSN to run against a live Postgres")
	}
	u, err := url.Parse(dsn)
	if err != nil || u.Path == "" {
		t.Fatalf("DBC_LIVE_PG_DSN must be a postgres:// URL naming a database: %v", err)
	}
	src = pgOpen(t, "src", dsn)
	db2 := strings.TrimPrefix(u.Path, "/") + "_etl2"
	if _, err = src.DB.Exec("CREATE DATABASE " + Postgres.QuoteIdent(db2)); err != nil &&
		!strings.Contains(err.Error(), "already exists") {
		t.Fatal(err)
	}
	u.Path = "/" + db2
	dst = pgOpen(t, "dst", u.String())
	for _, c := range []Conn{src, dst} {
		mustExec(t, c, "DROP SCHEMA IF EXISTS etl_live CASCADE", "CREATE SCHEMA etl_live")
		t.Cleanup(func() { _, _ = c.DB.Exec("DROP SCHEMA IF EXISTS etl_live CASCADE") })
	}
	return src, dst
}

func pgOpen(t *testing.T, name, dsn string) Conn {
	t.Helper()
	dbh, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dbh.Close() })
	return Conn{Name: name, DB: dbh, Engine: Postgres}
}

// seedKitchenSink makes a table with the types that trip copies up: text
// needing COPY escapes, a literal \N string (not NULL), numeric precision,
// arrays, jsonb, bytea, timestamptz, ranges, and an all-NULL row.
func seedKitchenSink(t *testing.T, c Conn) {
	t.Helper()
	mustExec(t, c, `CREATE TABLE etl_live.sink (
			id serial PRIMARY KEY,
			name varchar(80) NOT NULL,
			note text,
			amount numeric(12,2),
			tags text[],
			meta jsonb,
			blob bytea,
			at timestamptz,
			day date,
			flag boolean,
			span int4range
		)`,
		`INSERT INTO etl_live.sink (name, note, amount, tags, meta, blob, at, day, flag, span) VALUES
		 ('plain', 'hello', 12.34, '{a,b}', '{"k": [1, 2]}', '\xdeadbeef', '2024-01-02 03:04:05.123456+00', '2024-01-02', true, '[1,10)'),
		 ('escapes', E'tab\there\nnewline\\backslash\rcr', -0.01, '{"with space","quo\"te"}', '{"t": "a\tb"}', '\x00', '1999-12-31 23:59:59-08', '1999-12-31', false, 'empty'),
		 ('literal \N', '\N', 0, '{}', 'null', '', 'infinity', '2000-02-29', NULL, NULL),
		 ('nulls', NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL)`)
}

// asText reads every column cast to text, in id order — the comparison
// that shows a copy lost nothing (an int4range, a jsonb, a bytea compare as
// their canonical text forms).
func asText(t *testing.T, c Conn, table string) [][]string {
	t.Helper()
	return dump(t, c, `SELECT id::text, name::text, note::text, amount::text, tags::text, meta::text,
		blob::text, at::text, day::text, flag::text, span::text FROM `+table+` ORDER BY id`)
}

func columnTypes(t *testing.T, c Conn, table string) [][]string {
	t.Helper()
	return dump(t, c, `SELECT attname::text, format_type(atttypid, atttypmod), attnotnull::text FROM pg_attribute
		WHERE attrelid = '`+table+`'::regclass AND attnum > 0 AND NOT attisdropped ORDER BY attnum`)
}

func TestLivePGDirectCopyCreates(t *testing.T) {
	src, dst := livePG(t)
	seedKitchenSink(t, src)

	st, err := Copy(context.Background(), src, "etl_live.sink", dst, CopyOptions{Create: true})
	if err != nil {
		t.Fatal(err)
	}
	if !st.Direct || st.Rows != 4 {
		t.Errorf("stats = %+v, want direct, 4 rows", st)
	}
	if got, want := asText(t, dst, "etl_live.sink"), asText(t, src, "etl_live.sink"); !reflect.DeepEqual(got, want) {
		t.Errorf("rows differ\n got %q\nwant %q", got, want)
	}
	// Exact column types and NOT NULLs, from the source catalog.
	want := columnTypes(t, src, "etl_live.sink")
	want[0][1] = "integer" // serial's default (its sequence) is not carried over; the type is
	if got := columnTypes(t, dst, "etl_live.sink"); !reflect.DeepEqual(got, want) {
		t.Errorf("column types\n got %q\nwant %q", got, want)
	}
	pk := dump(t, dst, `SELECT count(*) FROM pg_index WHERE indrelid = 'etl_live.sink'::regclass AND indisprimary`)
	if pk[0][0] != "1" {
		t.Error("primary key not created")
	}

	// A filtered, column-subset copy into a second, created table.
	st, err = Copy(context.Background(), src, "etl_live.sink", dst, CopyOptions{
		To: "etl_live.named", Create: true, Columns: []string{"name", "id"}, Where: "flag IS NOT NULL",
	})
	if err != nil || st.Rows != 2 {
		t.Fatalf("subset copy: %+v, %v", st, err)
	}
	if got := dump(t, dst, "SELECT id, name FROM etl_live.named ORDER BY id"); !reflect.DeepEqual(got,
		[][]string{{"1", "plain"}, {"2", "escapes"}}) {
		t.Errorf("subset rows = %q", got)
	}
}

// The row path through a Postgres destination: Transform forces it, so
// every value is decoded by pgx and re-encoded as COPY text by appendPGText.
// The result must be identical to the direct copy's.
func TestLivePGRowPathRoundTrips(t *testing.T) {
	src, dst := livePG(t)
	seedKitchenSink(t, src)
	calls := 0
	st, err := Copy(context.Background(), src, "etl_live.sink", dst, CopyOptions{
		Create:    true,
		Transform: func(row []any) ([]any, error) { calls++; return row, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if st.Direct || st.Rows != 4 || calls != 4 {
		t.Errorf("stats = %+v, calls = %d; want row path, 4 rows", st, calls)
	}
	if got, want := asText(t, dst, "etl_live.sink"), asText(t, src, "etl_live.sink"); !reflect.DeepEqual(got, want) {
		t.Errorf("rows differ\n got %q\nwant %q", got, want)
	}

	// Query + Args: also the row path (COPY cannot bind), with types from
	// the driver's names rather than the catalog.
	st, err = Copy(context.Background(), src, "", dst, CopyOptions{
		Query: "SELECT id, upper(name) AS name, tags, at FROM etl_live.sink WHERE id <= $1",
		Args:  []any{2}, To: "etl_live.q", Create: true,
	})
	if err != nil || st.Direct || st.Rows != 2 {
		t.Fatalf("query copy: %+v, %v", st, err)
	}
	got := dump(t, dst, "SELECT name, tags::text, pg_typeof(tags)::text, pg_typeof(at)::text FROM etl_live.q ORDER BY id")
	want := [][]string{{"PLAIN", "{a,b}", "text[]", "timestamp with time zone"},
		{"ESCAPES", `{"with space","quo\"te"}`, "text[]", "timestamp with time zone"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("query copy rows\n got %q\nwant %q", got, want)
	}
}

// A failed copy leaves the destination as it was, on both paths: the
// truncate is undone, and so is a create.
func TestLivePGFailureRollsBack(t *testing.T) {
	src, dst := livePG(t)
	seedKitchenSink(t, src)
	mustExec(t, dst, `CREATE TABLE etl_live.sink (id int PRIMARY KEY, name text NOT NULL, note text,
			amount numeric CHECK (amount > -0.001), tags text[], meta jsonb, blob bytea,
			at timestamptz, day date, flag boolean, span int4range)`,
		"INSERT INTO etl_live.sink (id, name) VALUES (99, 'keep me')")

	for _, direct := range []bool{true, false} {
		opt := CopyOptions{Truncate: true} // row 2's -0.01 fails the CHECK
		if !direct {
			opt.Transform = func(r []any) ([]any, error) { return r, nil }
		}
		st, err := Copy(context.Background(), src, "etl_live.sink", dst, opt)
		if err == nil || !strings.Contains(err.Error(), "check constraint") || st.Direct != direct {
			t.Fatalf("direct=%v: %+v, err = %v; want a check-constraint failure", direct, st, err)
		}
		if got := dump(t, dst, "SELECT id, name FROM etl_live.sink"); !reflect.DeepEqual(got, [][]string{{"99", "keep me"}}) {
			t.Errorf("direct=%v: destination after failure = %q", direct, got)
		}
	}

	// Create in the same transaction: a failed copy into a new table leaves
	// no table behind. 'escapes' violates the extra NOT NULL via the where.
	_, err := Copy(context.Background(), src, "", dst, CopyOptions{
		Query: "SELECT id, CASE WHEN id = 4 THEN 1/0 END AS boom FROM etl_live.sink", To: "etl_live.fresh", Create: true,
	})
	if err == nil {
		t.Fatal("copy with a failing source query succeeded")
	}
	if got := dump(t, dst, "SELECT to_regclass('etl_live.fresh') IS NULL"); got[0][0] != "true" {
		t.Error("a failed copy left its created table behind")
	}
	assertNoOpenTx(t, dst)
}

// Postgres → SQLite → Postgres: the cross-engine row path both ways, with
// Create's type mapping on each side.
func TestLivePGToSQLiteAndBack(t *testing.T) {
	src, pgDst := livePG(t)
	seedKitchenSink(t, src)
	lite := fileConn(t, SQLite, "mid.db")
	ctx := context.Background()

	if st, err := Copy(ctx, src, "etl_live.sink", lite, CopyOptions{To: "sink", Create: true}); err != nil || st.Rows != 4 {
		t.Fatalf("pg → sqlite: %+v, %v", st, err)
	}
	if st, err := Copy(ctx, lite, "sink", pgDst, CopyOptions{To: "etl_live.back", Create: true}); err != nil || st.Rows != 4 {
		t.Fatalf("sqlite → pg: %+v, %v", st, err)
	}
	// Columns SQLite stores natively come back exactly. amount is compared
	// as a float: SQLite's NUMERIC affinity stores "0.00" as the integer 0,
	// so its scale does not survive the trip — the value does.
	got := dump(t, pgDst, `SELECT id, name, note, amount::float8::text, encode(blob, 'hex'), flag::text FROM etl_live.back ORDER BY id`)
	want := dump(t, src, `SELECT id, name, note, amount::float8::text, encode(blob, 'hex'), flag::text FROM etl_live.sink ORDER BY id`)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round trip\n got %q\nwant %q", got, want)
	}
	if got := dump(t, pgDst, `SELECT count(*) FROM pg_index WHERE indrelid = 'etl_live.back'::regclass AND indisprimary`); got[0][0] != "1" {
		t.Error("primary key not carried over from SQLite")
	}
}

// Stopping a big direct copy part way: the error is the cancellation, the
// created table is gone, and no connection is left in a transaction.
func TestLivePGCancelDirect(t *testing.T) {
	src, dst := livePG(t)
	mustExec(t, src, `CREATE TABLE etl_live.big AS SELECT g AS id, md5(g::text) AS h FROM generate_series(1, 2000000) g`)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var seen int64
	st, err := Copy(ctx, src, "etl_live.big", dst, CopyOptions{
		Create: true, ProgressEvery: 50_000,
		Progress: func(n int64) {
			if seen == 0 {
				cancel()
			}
			seen = n
		},
	})
	if err == nil {
		t.Fatalf("canceled copy succeeded: %+v", st)
	}
	if !errors.Is(err, context.Canceled) && !strings.Contains(err.Error(), "cancel") {
		t.Errorf("err = %v, want a cancellation", err)
	}
	if seen == 0 || seen >= 2_000_000 {
		t.Errorf("progress stopped at %d", seen)
	}
	if got := dump(t, dst, "SELECT to_regclass('etl_live.big') IS NULL"); got[0][0] != "true" {
		t.Error("canceled copy left its table behind")
	}
	assertNoOpenTx(t, dst)

	// And to the end, for a speed sanity check in -v output.
	start := time.Now()
	st, err = Copy(context.Background(), src, "etl_live.big", dst, CopyOptions{Create: true})
	if err != nil || st.Rows != 2_000_000 {
		t.Fatalf("full copy: %+v, %v", st, err)
	}
	t.Logf("%s (%.0f rows/s)", st, float64(st.Rows)/time.Since(start).Seconds())
}

// assertNoOpenTx checks that nothing this test did is still holding a
// transaction open on c's database — a leaked Writer would.
func assertNoOpenTx(t *testing.T, c Conn) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		n := dump(t, c, `SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND pid <> pg_backend_pid() AND xact_start IS NOT NULL`)[0][0]
		if n == "0" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s sessions still in a transaction", n)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
