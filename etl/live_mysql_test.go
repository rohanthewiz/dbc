package etl

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
)

// Live MySQL tests: opt-in, like live_test.go's Postgres ones. They need
// DBC_LIVE_MYSQL_DSN and copy between two databases on that server — the
// DSN's own and "<db>_etl2", created here when missing — so source and
// destination are two connections, as they would be between two servers.
//
//	DBC_LIVE_MYSQL_DSN='root:pw@tcp(127.0.0.1:3306)/dbc' \
//	go test ./etl -run LiveMySQL -v
//
// MySQL has no schemas below a database, so both sides work in tables named
// etl_live_*, dropped first and on cleanup. The cross-engine test also needs
// DBC_LIVE_PG_DSN.
//
// What MySQL changes about a copy, and what each test pins down:
//
//	CREATE TABLE commits implicitly ─► Copy runs it before the load's
//	    transaction: a failed copy leaves the new table behind, empty
//	    (TestLiveMySQLFailureRollsBack)
//	TRUNCATE commits implicitly too ─► Truncate is a DELETE inside the load's
//	    transaction, so a failed reload keeps the old rows (same test)
//	LONGTEXT/LONGBLOB cannot be a key ─► a created text or bytes key column
//	    is VARCHAR(255)/VARBINARY(255) (TestLiveMySQLCopyCreates)
//	the text protocol returns most values as []byte ─► the Reader turns them
//	    into strings unless the column is a binary type, so a VARCHAR lands in
//	    Postgres as text and a BLOB as the same bytes (TestLiveMySQLToPostgres)

// liveMySQLTables are the tables the tests make, on either side.
var liveMySQLTables = []string{"etl_live_sink", "etl_live_sink_pt", "etl_live_fresh", "etl_live_pg"}

// liveMySQL returns the source and destination connections, or a skip. The
// source is opened as the DSN gives it; parseTime is what a test adds when
// it wants the driver to decode DATETIMEs as time.Time instead.
func liveMySQL(t *testing.T) (src, dst Conn) {
	t.Helper()
	dsn := os.Getenv("DBC_LIVE_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set DBC_LIVE_MYSQL_DSN to run against a live MySQL")
	}
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil || cfg.DBName == "" {
		t.Fatalf("DBC_LIVE_MYSQL_DSN must name a database: %v", err)
	}
	src = myOpen(t, "src", dsn)
	db2 := cfg.DBName + "_etl2"
	mustExec(t, src, "CREATE DATABASE IF NOT EXISTS "+MySQL.QuoteIdent(db2))
	cfg.DBName = db2
	dst = myOpen(t, "dst", cfg.FormatDSN())
	for _, c := range []Conn{src, dst} {
		dropMySQLTables(t, c)
		t.Cleanup(func() { dropMySQLTables(t, c) })
	}
	return src, dst
}

func dropMySQLTables(t *testing.T, c Conn) {
	t.Helper()
	for _, tb := range liveMySQLTables {
		if _, err := c.DB.Exec("DROP TABLE IF EXISTS " + tb); err != nil {
			t.Fatalf("%s: drop %s: %v", c.Name, tb, err)
		}
	}
}

func myOpen(t *testing.T, name, dsn string) Conn {
	t.Helper()
	dbh, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dbh.Close() })
	return Conn{Name: name, DB: dbh, Engine: MySQL}
}

// withParseTime reopens c's database with parseTime=true, the option dbc's
// own connection form suggests, under which DATETIME and DATE arrive as
// time.Time rather than as text.
func withParseTime(t *testing.T, c Conn) Conn {
	t.Helper()
	cfg, err := mysql.ParseDSN(os.Getenv("DBC_LIVE_MYSQL_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.ParseTime = true
	return myOpen(t, c.Name+"-pt", cfg.FormatDSN())
}

// seedMySQLSink makes a table with what trips a MySQL copy up: a composite
// key of a VARCHAR and a VARBINARY (the created key columns need a length),
// text with escapes and non-ASCII, a DECIMAL, a BLOB holding a NUL, a
// fractional DATETIME, a BOOLEAN (TINYINT(1)), an unsigned BIGINT at the
// signed maximum, a DOUBLE, JSON, and an all-NULL row.
func seedMySQLSink(t *testing.T, c Conn) {
	t.Helper()
	mustExec(t, c, `CREATE TABLE etl_live_sink (
			code VARCHAR(40) NOT NULL,
			tag VARBINARY(16) NOT NULL,
			name VARCHAR(80) NOT NULL,
			note TEXT,
			amount DECIMAL(12,2),
			data LONGBLOB,
			at DATETIME(6),
			day DATE,
			flag BOOLEAN,
			n BIGINT UNSIGNED,
			f DOUBLE,
			meta JSON,
			PRIMARY KEY (code, tag)
		)`,
		`INSERT INTO etl_live_sink VALUES
		 ('plain', x'01', 'plain', 'hello', 12.34, x'deadbeef', '2024-01-02 03:04:05.123456', '2024-01-02', true, 42, 1.5, '{"k": [1, 2]}'),
		 ('escapes', x'02', 'escapes', 'tab\there\nnewline\\backslash ünï', -0.01, x'00', '1999-12-31 23:59:59', '1999-12-31', false, 9223372036854775807, -2.25, '"a\\tb"'),
		 ('nulls', x'03', 'nulls', NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL)`)
}

// mySinkValues reads etl_live_sink-shaped table back in a form that compares
// across the created table's wider types: DECIMAL(65,30) cast back to two
// places, JSON and LONGTEXT both normalized through CAST(… AS JSON), bytes
// as hex.
func mySinkValues(t *testing.T, c Conn, table string) [][]string {
	t.Helper()
	return dump(t, c, `SELECT code, HEX(tag), name, note, CAST(amount AS DECIMAL(12,2)), HEX(data),
		CAST(at AS DATETIME(6)), day, flag + 0, n, f, CAST(CAST(meta AS JSON) AS CHAR)
		FROM `+table+` ORDER BY code`)
}

// myColumnTypes is each column's declared type and the key's columns.
func myColumnTypes(t *testing.T, c Conn, table string) (types [][]string, key []string) {
	t.Helper()
	types = dump(t, c, `SELECT column_name, column_type FROM information_schema.columns
		WHERE table_schema = DATABASE() AND table_name = '`+table+`' ORDER BY ordinal_position`)
	for _, r := range dump(t, c, `SELECT column_name FROM information_schema.key_column_usage
		WHERE table_schema = DATABASE() AND table_name = '`+table+`' AND constraint_name = 'PRIMARY'
		ORDER BY ordinal_position`) {
		key = append(key, r[0])
	}
	return types, key
}

// MySQL → MySQL with Create, by the row path (the only one MySQL has). The
// key's VARCHAR and VARBINARY become VARCHAR(255) and VARBINARY(255), since
// MySQL cannot key a LONGTEXT/LONGBLOB; every value survives; and it does so
// whether the source's driver hands DATETIMEs over as text or as time.Time.
func TestLiveMySQLCopyCreates(t *testing.T) {
	src, dst := liveMySQL(t)
	seedMySQLSink(t, src)
	want := mySinkValues(t, src, "etl_live_sink")

	st, err := Copy(context.Background(), src, "etl_live_sink", dst, CopyOptions{Create: true})
	if err != nil {
		t.Fatal(err)
	}
	if st.Direct || st.Rows != 3 {
		t.Errorf("stats = %+v, want the row path, 3 rows", st)
	}
	if got := mySinkValues(t, dst, "etl_live_sink"); !reflect.DeepEqual(got, want) {
		t.Errorf("rows differ\n got %q\nwant %q", got, want)
	}
	types, key := myColumnTypes(t, dst, "etl_live_sink")
	wantTypes := [][]string{
		{"code", "varchar(255)"}, {"tag", "varbinary(255)"}, {"name", "longtext"}, {"note", "longtext"},
		{"amount", "decimal(65,30)"}, {"data", "longblob"}, {"at", "datetime(6)"}, {"day", "date"},
		// BOOLEAN is TINYINT(1), reported as TINYINT: an integer, so BIGINT.
		// The unsigned BIGINT becomes a signed one; the seeded maximum fits.
		{"flag", "bigint"}, {"n", "bigint"}, {"f", "double"}, {"meta", "longtext"},
	}
	if !reflect.DeepEqual(types, wantTypes) {
		t.Errorf("column types\n got %q\nwant %q", types, wantTypes)
	}
	if !reflect.DeepEqual(key, []string{"code", "tag"}) {
		t.Errorf("primary key = %q, want [code tag]", key)
	}

	// The same copy from a parseTime=true connection: DATETIME and DATE
	// arrive as time.Time and are written back as such.
	st, err = Copy(context.Background(), withParseTime(t, src), "etl_live_sink", dst,
		CopyOptions{To: "etl_live_sink_pt", Create: true})
	if err != nil || st.Rows != 3 {
		t.Fatalf("parseTime copy: %+v, %v", st, err)
	}
	if got := mySinkValues(t, dst, "etl_live_sink_pt"); !reflect.DeepEqual(got, want) {
		t.Errorf("parseTime rows differ\n got %q\nwant %q", got, want)
	}
}

// A failed load leaves MySQL's destination rows as they were. Truncate is a
// DELETE in the load's transaction, so it is rolled back with the rows
// already inserted (a TRUNCATE would have committed on its own). A created
// table is the one exception the package documents: the CREATE commits
// implicitly, so it runs first, on its own, and a failed copy leaves it
// behind — empty, never partial.
func TestLiveMySQLFailureRollsBack(t *testing.T) {
	src, dst := liveMySQL(t)
	seedMySQLSink(t, src)
	mustExec(t, dst, `CREATE TABLE etl_live_sink (code VARCHAR(40) NOT NULL, tag VARBINARY(16) NOT NULL,
			name VARCHAR(80) NOT NULL, note TEXT, amount DECIMAL(12,2), data LONGBLOB, at DATETIME(6),
			day DATE, flag BOOLEAN, n BIGINT UNSIGNED, f DOUBLE, meta JSON, PRIMARY KEY (code, tag))`,
		`INSERT INTO etl_live_sink (code, tag, name) VALUES ('keep', x'ff', 'keep me')`)

	// failAt fails the copy on its third row; with one row per INSERT the
	// first two have already reached the server inside the transaction.
	failAt := func() func([]any) ([]any, error) {
		n := 0
		return func(r []any) ([]any, error) {
			if n++; n == 3 {
				return nil, errors.New("boom")
			}
			return r, nil
		}
	}
	keep := [][]string{{"keep", "keep me"}}

	_, err := Copy(context.Background(), src, "etl_live_sink", dst,
		CopyOptions{Truncate: true, BatchSize: 1, Transform: failAt()})
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v, want the transform's", err)
	}
	if got := dump(t, dst, "SELECT code, name FROM etl_live_sink ORDER BY code"); !reflect.DeepEqual(got, keep) {
		t.Errorf("destination after a failed reload = %q, want only %q", got, keep)
	}

	_, err = Copy(context.Background(), src, "etl_live_sink", dst,
		CopyOptions{To: "etl_live_fresh", Create: true, BatchSize: 1, Transform: failAt()})
	if err == nil {
		t.Fatal("failing copy into a created table succeeded")
	}
	if got := dump(t, dst, "SELECT count(*) FROM etl_live_fresh"); got[0][0] != "0" {
		t.Errorf("created table after a failed copy holds %s rows, want 0 (left behind, empty)", got[0][0])
	}
	assertNoOpenMySQLTx(t, dst)

	// And a reload that succeeds replaces the old rows.
	st, err := Copy(context.Background(), src, "etl_live_sink", dst, CopyOptions{Truncate: true})
	if err != nil || st.Rows != 3 {
		t.Fatalf("reload: %+v, %v", st, err)
	}
	if got, want := mySinkValues(t, dst, "etl_live_sink"), mySinkValues(t, src, "etl_live_sink"); !reflect.DeepEqual(got, want) {
		t.Errorf("reloaded rows\n got %q\nwant %q", got, want)
	}
}

// MySQL → Postgres and Postgres → MySQL. Out of MySQL, the text protocol's
// []byte values are read as strings by column type: VARCHAR, TEXT, DECIMAL,
// DATETIME and JSON load into Postgres as their values, while the
// VARBINARY and BLOB load as the same bytes. Into MySQL, Postgres types it
// has no match for (arrays, ranges, jsonb) land as their text forms.
func TestLiveMySQLToPostgres(t *testing.T) {
	src, _ := liveMySQL(t)
	pgSrc, pgDst := livePG(t)
	seedMySQLSink(t, src)
	ctx := context.Background()

	st, err := Copy(ctx, src, "etl_live_sink", pgDst, CopyOptions{To: "etl_live.my", Create: true})
	if err != nil || st.Rows != 3 {
		t.Fatalf("mysql → pg: %+v, %v", st, err)
	}
	got := dump(t, pgDst, `SELECT code, encode(tag, 'hex'), name, note, amount::text, encode(data, 'hex'),
		at::text, day::text, flag::text, n::text, f::text, meta::jsonb::text FROM etl_live.my ORDER BY code`)
	want := [][]string{
		{"escapes", "02", "escapes", "tab\there\nnewline\\backslash ünï", "-0.01", "00",
			"1999-12-31 23:59:59", "1999-12-31", "0", "9223372036854775807", "-2.25", `"a\tb"`},
		{"nulls", "03", "nulls", "NULL", "NULL", "NULL", "NULL", "NULL", "NULL", "NULL", "NULL", "NULL"},
		{"plain", "01", "plain", "hello", "12.34", "deadbeef",
			"2024-01-02 03:04:05.123456", "2024-01-02", "1", "42", "1.5", `{"k": [1, 2]}`},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("mysql → pg rows\n got %q\nwant %q", got, want)
	}
	types := columnTypes(t, pgDst, "etl_live.my")
	wantTypes := map[string]string{"code": "text", "tag": "bytea", "name": "text", "amount": "numeric",
		"data": "bytea", "at": "timestamp without time zone", "day": "date", "flag": "bigint",
		"f": "double precision", "meta": "text"}
	for _, r := range types {
		if w, ok := wantTypes[r[0]]; ok && r[1] != w {
			t.Errorf("pg column %s is %s, want %s", r[0], r[1], w)
		}
	}
	if got := dump(t, pgDst, `SELECT count(*) FROM pg_index WHERE indrelid = 'etl_live.my'::regclass AND indisprimary`); got[0][0] != "1" {
		t.Error("primary key not carried over from MySQL")
	}

	// Back the other way, from the Postgres kitchen sink. Its 'infinity'
	// timestamptz has no DATETIME to become: MySQL refuses the value, and
	// the load is rolled back (the created table stays, empty). That is
	// left to the caller on purpose — mapping infinity to 9999-12-31 or to
	// NULL is a choice about the data, made with Where or a Transform.
	seedKitchenSink(t, pgSrc)
	_, err = Copy(ctx, pgSrc, "etl_live.sink", src, CopyOptions{To: "etl_live_pg", Create: true})
	if err == nil || !strings.Contains(err.Error(), "Incorrect datetime value: 'infinity'") {
		t.Fatalf("pg → mysql with an infinity timestamp: err = %v, want MySQL's refusal", err)
	}
	if got := dump(t, src, "SELECT count(*) FROM etl_live_pg"); got[0][0] != "0" {
		t.Errorf("refused load left %s rows", got[0][0])
	}
	st, err = Copy(ctx, pgSrc, "etl_live.sink", src, CopyOptions{To: "etl_live_pg", Create: true,
		Where: "at IS DISTINCT FROM 'infinity'"})
	if err != nil || st.Rows != 3 {
		t.Fatalf("pg → mysql: %+v, %v", st, err)
	}
	// at is compared in UTC: the driver writes a time.Time in its loc
	// (UTC by default), and DATETIME keeps no zone.
	got = dump(t, src, `SELECT id, name, note, CAST(amount AS DECIMAL(12,2)), tags, HEX(`+"`blob`"+`),
		CAST(at AS DATETIME(6)), day, flag + 0, span FROM etl_live_pg ORDER BY id`)
	want = dump(t, pgSrc, `SELECT id::text, name, note, amount::text, tags::text, upper(encode(blob, 'hex')),
		to_char(at AT TIME ZONE 'UTC', 'YYYY-MM-DD HH24:MI:SS.US'), day::text, flag::int::text, span::text
		FROM etl_live.sink WHERE at IS DISTINCT FROM 'infinity' ORDER BY id`)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("pg → mysql rows\n got %q\nwant %q", got, want)
	}
}

// assertNoOpenMySQLTx checks that nothing this test did is still holding a
// transaction open on c's server — a leaked Writer would.
func assertNoOpenMySQLTx(t *testing.T, c Conn) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		n := dump(t, c, `SELECT count(*) FROM information_schema.innodb_trx
			WHERE trx_mysql_thread_id <> CONNECTION_ID()`)[0][0]
		if n == "0" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s sessions still in a transaction", n)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
