package etl

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

// More live Postgres tests (DBC_LIVE_PG_DSN, see live_test.go): the corners
// of a copy that the kitchen-sink tests there do not reach.
//
//	TestLivePGTypeMatrix         every built-in type family, edge values, on each path
//	TestLivePGSessionSettings    servers whose DateStyle, IntervalStyle and float
//	                             digits differ (pgPinOutput)
//	TestLivePGIdentifiers        names that only work quoted
//	TestLivePGQueryShapes        a Query with comments, a ";", a CTE, VALUES
//	TestLivePGDestinationShapes  generated/identity columns, FKs, partitions, views
//	TestLivePGFailuresMidStream  failing or canceled part way, fast and clean
//	TestLivePGProgress           the running count, on both paths
//	TestLivePGTransformValues    Go values a Transform hands back (slices, maps …)
//	TestLivePGSameDatabase       a table rewritten in place (loadOptions)
//	TestLivePGConcurrentCopies   copies in parallel over shared pools (-race)
//	TestLivePGMoveRows           a DELETE … RETURNING source, kept if the load fails
//	                             or, in one database, waits on it (lock_timeout)
//
// They take about 15s against a local server, most of it the 300k- and
// 1M-row tables that make "part way" and "in place" mean something.
//
// The three ways a Postgres-to-Postgres copy can run, and what each one
// exercises:
//
//	direct   COPY (SELECT …) TO STDOUT ─► COPY … FROM STDIN; the rows are
//	         the source server's own text output, never decoded
//	row      a Transform forces the row path: pgx decodes each value (binary
//	         for the intrinsic types, text for the rest) and appendPGText
//	         re-encodes it as COPY text
//	query    a Query with Args: also the row path, and Create then types
//	         the columns from the driver's names, not the source catalog

// matrixDDL is a table with a column of every built-in type family a copy
// can meet, plus a user enum, domain and composite type (which must also
// exist on the destination for an exact-type Create to work).
var matrixDDL = []string{
	`CREATE TYPE etl_live.mood AS ENUM ('sad', 'ok', 'happy')`,
	`CREATE DOMAIN etl_live.posint AS integer CHECK (VALUE > 0)`,
	`CREATE TYPE etl_live.pair AS (label text, at timestamptz)`,
	`CREATE TABLE etl_live.matrix (
		id int PRIMARY KEY,
		i2 smallint, i4 integer, i8 bigint,
		f4 real, f8 double precision,
		num numeric, num_s numeric(30,10),
		cash money,
		txt text, vc varchar(5), ch char(3),
		b boolean, by bytea,
		d date, tm time, tmz timetz, ts timestamp, tsz timestamptz, iv interval,
		u uuid, j json, jb jsonb, x xml,
		ip inet, net cidr, mac macaddr, bits bit(4), vbits varbit,
		tsv tsvector, pt point, bx box,
		r4 int4range, rts tstzrange, rd daterange,
		mood etl_live.mood, pos etl_live.posint, pr etl_live.pair,
		ia int[], ta2 text[][], na numeric[], ba bytea[], tsa timestamptz[], da date[],
		ja jsonb[], fa float8[], ma etl_live.mood[]
	)`,
}

// matrixRows: a typical row, a row of edge values, an all-NULL row, and a
// row big enough to cross the 64 KiB COPY buffer on its own.
var matrixRows = []string{
	`INSERT INTO etl_live.matrix VALUES (1,
		1, 2, 3, 1.5, 0.1, 1.23, 123.4567890123, 12.34,
		'hello', 'abcde', 'ab', true, '\x0102',
		'2024-01-02', '03:04:05.123456', '03:04:05+02', '2024-01-02 03:04:05.123456',
		'2024-01-02 03:04:05.123456+00', '1 year 2 mons 3 days 04:05:06.789',
		'a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11', '{"b": 1,  "a": [1, 2]}', '{"b": 1, "a": [1, 2]}',
		'<a x="1">t</a>', '192.168.0.1/24', '10.0.0.0/8', '08:00:2b:01:02:03', B'1010', B'10101',
		'a fat cat', '(1.5,2.5)', '((0,0),(1,1))',
		'[1,10)', '[2024-01-01 00:00+00,2024-02-01 00:00+00)', '[2024-01-01,2024-02-01)',
		'ok', 5, ROW('lbl', '2024-01-02 03:04:05+00'),
		'{1,2,NULL}', '{{a,b},{c,d}}', '{1.5,NaN}', '{"\\x00ff",NULL}',
		'{"2024-01-02 03:04:05+00"}', '{2024-01-02,2024-03-15}', ARRAY['{"k":1}'::jsonb],
		'{0.1,0.30000000000000004}', '{sad,happy}')`,
	`INSERT INTO etl_live.matrix VALUES (2,
		-32768, -2147483648, -9223372036854775808, 'NaN', '-Infinity', 'NaN', '-0.0000000001',
		-92233720368547758.08,
		E'tab\there\nnl\\bs\rcr \\. end\n\\.\nunicode é ✓ 🐈', 'ä✓🐈ab', 'x', false,
		decode((SELECT string_agg(lpad(to_hex(g), 2, '0'), '') FROM generate_series(0, 255) g), 'hex'),
		'0044-03-15 BC', '24:00:00', '23:59:59.999999-14', '-infinity', 'infinity',
		'-1 year +2 mons -3 days +04:05:06.789',
		'00000000-0000-0000-0000-000000000000', '[]', '"str\twith\\esc"',
		'plain content', '::1/128', '2001:db8::/32', 'ff:ff:ff:ff:ff:ff', B'0000', B'',
		'', '(-1e-300,1e300)', '((-1,-1),(1,1))',
		'empty', '(,)', '[,2024-01-01)',
		'happy', 2147483647, ROW(E'q"uote,\\ ( )', NULL),
		'{}', '{{"with space","quo\"te"},{"back\\\\slash","NULL"}}', '{}', '{}',
		'{infinity,-infinity}', '{}', '{}', '{Infinity,-Infinity,NaN,-0}', '{}')`,
	`INSERT INTO etl_live.matrix (id) VALUES (3)`,
	`INSERT INTO etl_live.matrix (id, f4, f8, num, num_s, txt, by, d, ts, tsz) VALUES (4,
		3.4028235e38, 1.7976931348623157e308, '-Infinity', 12345678901234567890.0123456789,
		repeat(E'line\n', 200000), decode(repeat('ab', 300000), 'hex'),
		'12345-06-07', '12345-06-07 01:02:03', '1999-12-31 23:59:59.999999-08')`,
}

// seedMatrix makes the matrix table on src and its types on every other
// connection given (a Create of an enum column needs the enum).
func seedMatrix(t *testing.T, src Conn, others ...Conn) {
	t.Helper()
	mustExec(t, src, matrixDDL...)
	mustExec(t, src, matrixRows...)
	for _, c := range others {
		mustExec(t, c, matrixDDL[:3]...)
	}
}

// textOf reads every column of table cast to text, ordered by its first
// column — the canonical text of each value, so equal output means the copy
// lost nothing.
func textOf(t *testing.T, c Conn, table string) [][]string {
	t.Helper()
	cols := dump(t, c, `SELECT quote_ident(attname) FROM pg_attribute WHERE attrelid = '`+table+
		`'::regclass AND attnum > 0 AND NOT attisdropped ORDER BY attnum`)
	exprs := make([]string, len(cols))
	for i, col := range cols {
		exprs[i] = col[0] + "::text"
	}
	return dump(t, c, "SELECT "+strings.Join(exprs, ", ")+" FROM "+table+" ORDER BY 1")
}

// diffRows reports the first cells that differ, rather than two walls of
// text with a megabyte of "line\n" in them.
func diffRows(t *testing.T, label string, got, want [][]string) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s: %d rows, want %d", label, len(got), len(want))
		return
	}
	n := 0
	for r := range want {
		for c := range want[r] {
			if c >= len(got[r]) || got[r][c] != want[r][c] {
				g := "<missing>"
				if c < len(got[r]) {
					g = clipTest(got[r][c])
				}
				t.Errorf("%s: row %d col %d: got %q, want %q", label, r, c, g, clipTest(want[r][c]))
				if n++; n > 12 {
					return
				}
			}
		}
	}
}

func clipTest(s string) string {
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}

func TestLivePGTypeMatrix(t *testing.T) {
	src, dst := livePG(t)
	seedMatrix(t, src, dst)
	want := textOf(t, src, "etl_live.matrix")
	identity := func(r []any) ([]any, error) { return r, nil }
	ctx := context.Background()

	cases := []struct {
		name   string
		opt    CopyOptions
		direct bool
	}{
		{"direct/create", CopyOptions{To: "etl_live.m_direct", Create: true}, true},
		{"row/create", CopyOptions{To: "etl_live.m_row", Create: true, Transform: identity}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st, err := Copy(ctx, src, "etl_live.matrix", dst, tc.opt)
			if err != nil {
				t.Fatal(err)
			}
			if st.Direct != tc.direct || st.Rows != 4 {
				t.Errorf("stats = %+v", st)
			}
			diffRows(t, tc.name, textOf(t, dst, tc.opt.To), want)
			if got, want := columnTypes(t, dst, tc.opt.To), columnTypes(t, src, "etl_live.matrix"); !reflect.DeepEqual(got, want) {
				t.Errorf("column types\n got %q\nwant %q", got, want)
			}
		})
	}

	// Into an existing table on each path: the same values, with no Create.
	for _, direct := range []bool{true, false} {
		name := map[bool]string{true: "direct/existing", false: "row/existing"}[direct]
		t.Run(name, func(t *testing.T) {
			mustExec(t, dst, strings.Replace(matrixDDL[3], "etl_live.matrix", "etl_live.m_exist", 1))
			opt := CopyOptions{To: "etl_live.m_exist", Truncate: true}
			if !direct {
				opt.Transform = identity
			}
			if _, err := Copy(ctx, src, "etl_live.matrix", dst, opt); err != nil {
				t.Fatal(err)
			}
			diffRows(t, name, textOf(t, dst, "etl_live.m_exist"), want)
			mustExec(t, dst, "DROP TABLE etl_live.m_exist")
		})
	}

	// A Query with Args: the row path, and Create types columns from the
	// driver's type names. Types pgx does not know (money, timetz, the enum,
	// the composite) must still make a valid table.
	t.Run("query/create", func(t *testing.T) {
		st, err := Copy(ctx, src, "", dst, CopyOptions{
			Query: "SELECT * FROM etl_live.matrix WHERE id > $1", Args: []any{0},
			To: "etl_live.m_query", Create: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		if st.Rows != 4 {
			t.Errorf("stats = %+v", st)
		}
		diffRows(t, "query/create", textOf(t, dst, "etl_live.m_query"), want)
		// Built-in types pgx reports by OID keep their type; user types,
		// which the destination may not have, become text.
		types := map[string]string{}
		for _, c := range columnTypes(t, dst, "etl_live.m_query") {
			types[c[0]] = c[1]
		}
		for col, typ := range map[string]string{"cash": "money", "tmz": "time with time zone",
			"bits": "bit varying", "mood": "text", "pr": "text", "ma": "text", "tsa": "timestamp with time zone[]"} {
			if types[col] != typ {
				t.Errorf("query/create: %s is %q, want %q", col, types[col], typ)
			}
		}
	})
}

// withParams returns dsn with runtime parameters added to its query string;
// pgx sends each one in the startup message, so the session starts with it
// set — as it would for a database or role configured that way
// (ALTER DATABASE … SET DateStyle …).
func withParams(t *testing.T, dsn string, params map[string]string) string {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	for k, v := range params {
		q.Set(k, v)
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// settingsTable holds the values whose text form depends on session
// settings: DateStyle (dates, also inside arrays and ranges), IntervalStyle,
// extra_float_digits (floats, also inside arrays), TimeZone, bytea_output.
var settingsTable = []string{
	`CREATE TABLE etl_live.settings (
		id int PRIMARY KEY, d date, ts timestamp, tsz timestamptz, iv interval,
		f4 real, f8 double precision, by bytea, da date[], fa float8[], tr tstzrange, dr daterange)`,
	`INSERT INTO etl_live.settings VALUES
		(1, '2024-01-02', '2024-01-02 03:04:05.5', '2024-01-02 03:04:05.123456+00',
		 '-1 year +2 mons -3 days +04:05:06.789', 0.1, 0.30000000000000004, '\x00ff5c',
		 '{2024-01-02,2024-03-15}', '{0.30000000000000004,1e-300}',
		 '[2024-01-02 03:04:05+00,2024-03-15 00:00:00+00)', '[2024-01-02,2024-03-15)'),
		(2, '2024-03-15', '1999-12-31 23:59:59.999999', '1850-01-01 00:00:00+00',
		 '1 day -01:00:00', 3.4028235e38, 2.2250738585072014e-308, '\x', '{}', '{}', 'empty', 'empty'),
		(3, '0044-03-15 BC', '2024-02-29 12:00:00', 'infinity', '-04:05:06', -0.0, -0.0, NULL,
		 NULL, NULL, NULL, NULL)`,
}

// TestLivePGSessionSettings copies between sessions whose output and input
// settings differ — one end European dates and SQL-standard intervals with
// short floats, the other the defaults — and checks every value arrives
// unchanged. Both ends are compared through plain sessions with identical
// settings, so equal text means equal values.
func TestLivePGSessionSettings(t *testing.T) {
	src0, dst0 := livePG(t) // plain sessions: setup and comparison
	mustExec(t, src0, settingsTable...)
	want := textOf(t, src0, "etl_live.settings")
	srcDSN := os.Getenv("DBC_LIVE_PG_DSN")
	u, _ := url.Parse(srcDSN)
	u.Path += "_etl2"
	dstDSN := u.String()

	odd := map[string]string{
		"datestyle": "SQL, DMY", "intervalstyle": "sql_standard", "extra_float_digits": "0",
		"timezone": "Asia/Kolkata", "bytea_output": "escape",
	}
	plain := map[string]string{"timezone": "America/New_York"}
	for _, dir := range []struct {
		name     string
		src, dst map[string]string
	}{
		{"odd source", odd, plain},
		{"odd destination", plain, odd},
	} {
		src := pgOpen(t, "src", withParams(t, srcDSN, dir.src))
		dst := pgOpen(t, "dst", withParams(t, dstDSN, dir.dst))
		for _, direct := range []bool{true, false} {
			name := dir.name + map[bool]string{true: "/direct", false: "/row"}[direct]
			t.Run(name, func(t *testing.T) {
				opt := CopyOptions{To: "etl_live.settings_copy", Create: true}
				if !direct {
					opt.Transform = func(r []any) ([]any, error) { return r, nil }
				}
				st, err := Copy(context.Background(), src, "etl_live.settings", dst, opt)
				if err != nil {
					t.Fatal(err)
				}
				if st.Direct != direct {
					t.Errorf("stats = %+v", st)
				}
				diffRows(t, name, textOf(t, dst0, "etl_live.settings_copy"), want)
				mustExec(t, dst0, "DROP TABLE etl_live.settings_copy")
			})
		}
	}
}

// TestLivePGIdentifiers: names that only work quoted — mixed case, spaces,
// an embedded quote, reserved words — as the table, its schema and its
// columns, on both paths, with Create, a Columns subset and a Where.
func TestLivePGIdentifiers(t *testing.T) {
	src, dst := livePG(t)
	for _, c := range []Conn{src, dst} {
		mustExec(t, c, `CREATE SCHEMA "etl_live Odd"`)
		t.Cleanup(func() { _, _ = c.DB.Exec(`DROP SCHEMA IF EXISTS "etl_live Odd" CASCADE`) })
	}
	mustExec(t, src, `CREATE TABLE "etl_live Odd"."Order ""Items""" (
			"Id" int, "select" text NOT NULL, "two words" numeric(5,1), "tab	in" text,
			PRIMARY KEY ("select", "Id"))`,
		`INSERT INTO "etl_live Odd"."Order ""Items""" VALUES (1, 'a', 1.5, 'x'), (2, 'b', NULL, 'y'), (3, 'c', 3, NULL)`)
	table := `"etl_live Odd"."Order ""Items"""`
	for _, direct := range []bool{true, false} {
		name := map[bool]string{true: "direct", false: "row"}[direct]
		t.Run(name, func(t *testing.T) {
			opt := CopyOptions{Create: true, Truncate: true}
			if !direct {
				opt.Transform = func(r []any) ([]any, error) { return r, nil }
			}
			st, err := Copy(context.Background(), src, table, dst, opt)
			if err != nil {
				t.Fatal(err)
			}
			if st.Rows != 3 {
				t.Errorf("stats = %+v", st)
			}
			if got, want := textOf(t, dst, table), textOf(t, src, table); !reflect.DeepEqual(got, want) {
				t.Errorf("rows\n got %q\nwant %q", got, want)
			}
			// the key, in key order ("select" first), not table order
			key := dump(t, dst, `SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conrelid = '`+
				table+`'::regclass AND contype = 'p'`)
			if len(key) != 1 || key[0][0] != `PRIMARY KEY ("select", "Id")` {
				t.Errorf("key = %q", key)
			}

			// a subset, into an unquoted name, filtered on a quoted column
			opt.To, opt.Columns, opt.Where = "etl_live.items_sub", []string{"two words", "Id"}, `"two words" IS NOT NULL`
			if st, err = Copy(context.Background(), src, table, dst, opt); err != nil || st.Rows != 2 {
				t.Fatalf("subset: %+v, %v", st, err)
			}
			if got := dump(t, dst, `SELECT "Id", "two words" FROM etl_live.items_sub ORDER BY 1`); !reflect.DeepEqual(got,
				[][]string{{"1", "1.5"}, {"3", "3.0"}}) {
				t.Errorf("subset rows = %q", got)
			}
			mustExec(t, dst, "DROP TABLE etl_live.items_sub")
		})
	}
}

// TestLivePGQueryShapes: a Query (or Where) the way people write them —
// with a trailing line comment, a trailing semicolon and comment, a CTE,
// VALUES — on the direct path (no Args) and the row path (Args).
func TestLivePGQueryShapes(t *testing.T) {
	src, dst := livePG(t)
	mustExec(t, src, `CREATE TABLE etl_live.q (id int PRIMARY KEY, name text)`,
		`INSERT INTO etl_live.q SELECT g, 'n' || g FROM generate_series(1, 5) g`)
	ctx := context.Background()
	queries := map[string]string{
		"trailing line comment": "SELECT id, name FROM etl_live.q WHERE id > 2 -- the newer ones",
		"semicolon and comment": "SELECT id, name FROM etl_live.q WHERE id > 2; -- done\n",
		"block comment":         "/* hi */ SELECT id, name FROM etl_live.q WHERE id > 2 /* bye */",
		"cte":                   "WITH x AS (SELECT * FROM etl_live.q) SELECT id, name FROM x WHERE id > 2",
		"values":                "VALUES (3, 'n3'), (4, 'n4'), (5, 'n5')",
		"order and limit":       "SELECT id, name FROM etl_live.q ORDER BY id DESC LIMIT 3",
	}
	n := 0
	for name, q := range queries {
		for _, direct := range []bool{true, false} {
			label := name + map[bool]string{true: "/direct", false: "/row"}[direct]
			// A table per case: VALUES names its columns column1, column2,
			// and pgx's statement cache would trip over a reused name whose
			// columns changed under it.
			n++
			out := "etl_live.q_out" + itoa(int64(n))
			t.Run(label, func(t *testing.T) {
				opt := CopyOptions{Query: q, To: out, Create: true}
				if !direct {
					opt.Transform = func(r []any) ([]any, error) { return r, nil }
				}
				st, err := Copy(ctx, src, "", dst, opt)
				if err != nil {
					t.Fatal(err)
				}
				if st.Rows != 3 || st.Direct != direct {
					t.Errorf("stats = %+v", st)
				}
				got := dump(t, dst, "SELECT * FROM "+out+" ORDER BY 1")
				if want := [][]string{{"3", "n3"}, {"4", "n4"}, {"5", "n5"}}; !reflect.DeepEqual(got, want) {
					t.Errorf("rows = %q", got)
				}
			})
		}
	}
	// A Where with a trailing comment, on the direct path.
	mustExec(t, dst, `CREATE TABLE etl_live.q (id int PRIMARY KEY, name text)`)
	st, err := Copy(ctx, src, "etl_live.q", dst, CopyOptions{Where: "id > 2 -- newer"})
	if err != nil || st.Rows != 3 {
		t.Errorf("where with comment: %+v, %v", st, err)
	}
	// Two statements: refused up front, by name, before anything runs.
	_, err = Copy(ctx, src, "", dst, CopyOptions{Query: "SELECT 1; SELECT 2", To: "etl_live.two"})
	if err == nil || !strings.Contains(err.Error(), "more than one statement") {
		t.Errorf("two statements: %v", err)
	}
	// A semicolon inside a string or a dollar quote is not a terminator.
	st, err = Copy(ctx, src, "", dst, CopyOptions{
		Query: "SELECT 1 AS id, 'a;b' AS s, $$c;d$$ AS t;", To: "etl_live.semi", Create: true,
	})
	if err != nil || st.Rows != 1 {
		t.Fatalf("quoted semicolons: %+v, %v", st, err)
	}
	if got := dump(t, dst, "SELECT s, t FROM etl_live.semi"); !reflect.DeepEqual(got, [][]string{{"a;b", "c;d"}}) {
		t.Errorf("quoted semicolons = %q", got)
	}
}

// TestLivePGDestinationShapes: destinations that are not a plain twin of
// the source table.
func TestLivePGDestinationShapes(t *testing.T) {
	src, dst := livePG(t)
	ctx := context.Background()
	mustExec(t, src, `CREATE TABLE etl_live.src (id int PRIMARY KEY, name text, qty int,
			total int GENERATED ALWAYS AS (qty * 2) STORED)`,
		`INSERT INTO etl_live.src (id, name, qty) VALUES (1, 'a', 1), (2, 'b', 2), (3, 'c', NULL)`)
	both := func(t *testing.T, f func(t *testing.T, opt CopyOptions)) {
		for _, direct := range []bool{true, false} {
			t.Run(map[bool]string{true: "direct", false: "row"}[direct], func(t *testing.T) {
				var opt CopyOptions
				if !direct {
					opt.Transform = func(r []any) ([]any, error) { return r, nil }
				}
				f(t, opt)
			})
		}
	}

	// Create from a source with a generated column: the values land in a
	// plain column.
	both(t, func(t *testing.T, opt CopyOptions) {
		opt.To, opt.Create = "etl_live.gen_out", true
		if _, err := Copy(ctx, src, "etl_live.src", dst, opt); err != nil {
			t.Fatal(err)
		}
		if got := dump(t, dst, "SELECT id, total FROM etl_live.gen_out ORDER BY id"); !reflect.DeepEqual(got,
			[][]string{{"1", "2"}, {"2", "4"}, {"3", "NULL"}}) {
			t.Errorf("rows = %q", got)
		}
		mustExec(t, dst, "DROP TABLE etl_live.gen_out")
	})

	// An existing destination with its columns in another order, an extra
	// column with a default, and an identity column fed from the source.
	mustExec(t, dst, `CREATE TABLE etl_live.reordered (
		note text DEFAULT 'dflt', qty int, id int GENERATED ALWAYS AS IDENTITY PRIMARY KEY, name text)`)
	both(t, func(t *testing.T, opt CopyOptions) {
		opt.To, opt.Truncate, opt.Columns = "etl_live.reordered", true, []string{"id", "name", "qty"}
		if _, err := Copy(ctx, src, "etl_live.src", dst, opt); err != nil {
			t.Fatal(err)
		}
		if got := dump(t, dst, "SELECT id, name, qty, note FROM etl_live.reordered ORDER BY id"); !reflect.DeepEqual(got,
			[][]string{{"1", "a", "1", "dflt"}, {"2", "b", "2", "dflt"}, {"3", "c", "NULL", "dflt"}}) {
			t.Errorf("rows = %q", got)
		}
	})

	// A destination with a generated column of the same name: Postgres
	// refuses to load it, and the copy says so and changes nothing.
	mustExec(t, dst, `CREATE TABLE etl_live.gen_dst (id int PRIMARY KEY, name text, qty int,
		total int GENERATED ALWAYS AS (qty * 3) STORED)`, `INSERT INTO etl_live.gen_dst (id) VALUES (99)`)
	both(t, func(t *testing.T, opt CopyOptions) {
		opt.To, opt.Truncate = "etl_live.gen_dst", true
		_, err := Copy(ctx, src, "etl_live.src", dst, opt)
		if err == nil || !strings.Contains(err.Error(), "generated column") {
			t.Fatalf("err = %v, want a generated-column refusal", err)
		}
		if got := dump(t, dst, "SELECT id FROM etl_live.gen_dst"); !reflect.DeepEqual(got, [][]string{{"99"}}) {
			t.Errorf("destination changed: %q", got)
		}
		// …and leaving it out works: the destination computes its own.
		opt.Columns = []string{"id", "name", "qty"}
		if _, err = Copy(ctx, src, "etl_live.src", dst, opt); err != nil {
			t.Fatal(err)
		}
		if got := dump(t, dst, "SELECT id, total FROM etl_live.gen_dst ORDER BY id"); !reflect.DeepEqual(got,
			[][]string{{"1", "3"}, {"2", "6"}, {"3", "NULL"}}) {
			t.Errorf("rows = %q", got)
		}
		mustExec(t, dst, "TRUNCATE etl_live.gen_dst", "INSERT INTO etl_live.gen_dst (id) VALUES (99)")
	})

	// Truncate of a table another table's foreign key points at: Postgres
	// refuses TRUNCATE without CASCADE, which Copy must not add (it would
	// empty the referencing table too). The refusal leaves both as they were.
	mustExec(t, dst, `CREATE TABLE etl_live.parent (id int PRIMARY KEY, name text, qty int, total int)`,
		`CREATE TABLE etl_live.child (pid int REFERENCES etl_live.parent)`,
		`INSERT INTO etl_live.parent VALUES (7, 'p', 0, 0)`, `INSERT INTO etl_live.child VALUES (7)`)
	both(t, func(t *testing.T, opt CopyOptions) {
		opt.To, opt.Truncate = "etl_live.parent", true
		_, err := Copy(ctx, src, "etl_live.src", dst, opt)
		if err == nil || !strings.Contains(err.Error(), "foreign key") {
			t.Fatalf("err = %v, want a foreign-key refusal", err)
		}
		if got := dump(t, dst, "SELECT (SELECT count(*) FROM etl_live.parent), (SELECT count(*) FROM etl_live.child)"); !reflect.DeepEqual(got,
			[][]string{{"1", "1"}}) {
			t.Errorf("parent, child counts = %q", got)
		}
	})

	// A partitioned destination: COPY routes each row to its partition.
	mustExec(t, dst, `CREATE TABLE etl_live.parted (id int, name text, qty int, total int) PARTITION BY RANGE (id)`,
		`CREATE TABLE etl_live.parted_lo PARTITION OF etl_live.parted FOR VALUES FROM (0) TO (2)`,
		`CREATE TABLE etl_live.parted_hi PARTITION OF etl_live.parted FOR VALUES FROM (2) TO (100)`)
	both(t, func(t *testing.T, opt CopyOptions) {
		opt.To, opt.Truncate = "etl_live.parted", true
		if _, err := Copy(ctx, src, "etl_live.src", dst, opt); err != nil {
			t.Fatal(err)
		}
		if got := dump(t, dst, "SELECT (SELECT count(*) FROM etl_live.parted_lo), (SELECT count(*) FROM etl_live.parted_hi)"); !reflect.DeepEqual(got,
			[][]string{{"1", "2"}}) {
			t.Errorf("lo, hi = %q", got)
		}
	})

	// Sources that are not plain tables: a view, a materialized view, a
	// partitioned parent. Create works from each (no key to carry).
	mustExec(t, src, `CREATE VIEW etl_live.v AS SELECT id, upper(name) AS name FROM etl_live.src`,
		`CREATE MATERIALIZED VIEW etl_live.mv AS SELECT id, name FROM etl_live.src`,
		`CREATE TABLE etl_live.sp (id int, name text) PARTITION BY LIST (id)`,
		`CREATE TABLE etl_live.sp_a PARTITION OF etl_live.sp FOR VALUES IN (1, 2)`,
		`CREATE TABLE etl_live.sp_b PARTITION OF etl_live.sp FOR VALUES IN (3)`,
		`INSERT INTO etl_live.sp VALUES (1, 'x'), (2, 'y'), (3, 'z')`)
	for _, from := range []string{"etl_live.v", "etl_live.mv", "etl_live.sp"} {
		both(t, func(t *testing.T, opt CopyOptions) {
			opt.To, opt.Create = "etl_live.from_rel", true
			st, err := Copy(ctx, src, from, dst, opt)
			if err != nil || st.Rows != 3 {
				t.Fatalf("%s: %+v, %v", from, st, err)
			}
			mustExec(t, dst, "DROP TABLE etl_live.from_rel")
		})
	}

	// Create into a schema the destination does not have: a clear failure.
	_, err := Copy(ctx, src, "etl_live.src", dst, CopyOptions{To: "nosuch.t", Create: true})
	if err == nil || !strings.Contains(err.Error(), `schema "nosuch" does not exist`) {
		t.Errorf("err = %v", err)
	}
	assertNoOpenTx(t, dst)
}

// assertSourceIdle checks that nothing a copy started is still running on
// c's database: no session but this one in a transaction, and none still
// executing a statement. A copy that failed at the destination must have
// stopped its source query, not left it streaming into a closed socket.
func assertSourceIdle(t *testing.T, c Conn) {
	t.Helper()
	assertNoOpenTx(t, c)
	deadline := time.Now().Add(5 * time.Second)
	for {
		n := dump(t, c, `SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND pid <> pg_backend_pid() AND state = 'active'`)[0][0]
		if n == "0" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s sessions still active", n)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestLivePGFailuresMidStream: a copy that fails part way — at the
// destination, at the source, or in a Transform — on a table big enough
// that "part way" means something. Each must fail fast, name the cause,
// leave the destination's old rows in place, and leave no session busy or
// in a transaction on either side.
func TestLivePGFailuresMidStream(t *testing.T) {
	src, dst := livePG(t)
	ctx := context.Background()
	const rows = 1_000_000
	mustExec(t, src, `CREATE TABLE etl_live.big AS SELECT g AS id, md5(g::text) AS h FROM generate_series(1, 1000000) g`)
	mustExec(t, dst, `CREATE TABLE etl_live.big (id int PRIMARY KEY CHECK (id < 2000), h text)`,
		`INSERT INTO etl_live.big VALUES (1, 'keep')`)
	identity := func(r []any) ([]any, error) { return r, nil }
	kept := func(t *testing.T) {
		t.Helper()
		if got := dump(t, dst, "SELECT id, h FROM etl_live.big"); !reflect.DeepEqual(got, [][]string{{"1", "keep"}}) {
			t.Errorf("destination after failure = %q", got)
		}
		assertSourceIdle(t, src)
		assertNoOpenTx(t, dst)
	}

	// The full copy, for the time a failure must beat (a failure that
	// drained the source first would take about as long).
	mustExec(t, dst, `CREATE TABLE etl_live.big_ok (id int, h text)`)
	start := time.Now()
	if _, err := Copy(ctx, src, "etl_live.big", dst, CopyOptions{To: "etl_live.big_ok", Transform: identity}); err != nil {
		t.Fatal(err)
	}
	full := time.Since(start)
	t.Logf("full row-path copy of %d rows: %s", rows, full)

	for _, direct := range []bool{true, false} {
		path := map[bool]string{true: "direct", false: "row"}[direct]
		opt := func(o CopyOptions) CopyOptions {
			o.Truncate = true
			if !direct && o.Transform == nil {
				o.Transform = identity
			}
			return o
		}
		t.Run(path+"/destination fails early", func(t *testing.T) {
			start := time.Now()
			_, err := Copy(ctx, src, "etl_live.big", dst, opt(CopyOptions{}))
			if err == nil || !strings.Contains(err.Error(), "check constraint") {
				t.Fatalf("err = %v", err)
			}
			if d := time.Since(start); d > full/2 {
				t.Errorf("failure took %s; the full copy takes %s — was the source drained?", d, full)
			}
			kept(t)
		})
		t.Run(path+"/source fails mid-read", func(t *testing.T) {
			_, err := Copy(ctx, src, "", dst, opt(CopyOptions{To: "etl_live.big",
				Query: "SELECT id, CASE WHEN id = 500000 THEN (1/0)::text ELSE 'x' END AS h FROM etl_live.big WHERE id = 1 OR id >= 400000"}))
			if err == nil || !strings.Contains(err.Error(), "division by zero") {
				t.Fatalf("err = %v", err)
			}
			kept(t)
		})
	}
	t.Run("row/transform fails", func(t *testing.T) {
		// Fail on the 700th row seen, not on id 700: a table this size is
		// read by a synchronized seq scan, which may start mid-table.
		n := 0
		_, err := Copy(ctx, src, "etl_live.big", dst, CopyOptions{Truncate: true, Transform: func(r []any) ([]any, error) {
			if n++; n == 700 {
				return nil, errors.New("no 700s")
			}
			return r, nil
		}})
		if err == nil || !strings.Contains(err.Error(), "no 700s") {
			t.Fatalf("err = %v", err)
		}
		kept(t)
	})
	t.Run("row/transform returns a short row", func(t *testing.T) {
		_, err := Copy(ctx, src, "etl_live.big", dst, CopyOptions{Truncate: true,
			Transform: func(r []any) ([]any, error) { return r[:1], nil }})
		if err == nil || !strings.Contains(err.Error(), "wrong number of values") {
			t.Fatalf("err = %v", err)
		}
		kept(t)
	})
	t.Run("row/canceled mid-way", func(t *testing.T) {
		cctx, cancel := context.WithCancel(ctx)
		defer cancel()
		_, err := Copy(cctx, src, "etl_live.big", dst, CopyOptions{Truncate: true, To: "etl_live.big",
			Where: "id < 1500", Transform: func(r []any) ([]any, error) {
				if r[0] == int64(1000) {
					cancel()
				}
				return r, nil
			}})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want a cancellation", err)
		}
		kept(t)
	})
	t.Run("canceled before it starts", func(t *testing.T) {
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		for _, direct := range []bool{true, false} {
			_, err := Copy(cctx, src, "etl_live.big", dst, opt2(direct, CopyOptions{To: "etl_live.never", Create: true}))
			if !errors.Is(err, context.Canceled) {
				t.Errorf("direct=%v: err = %v, want a cancellation", direct, err)
			}
		}
		if got := dump(t, dst, "SELECT to_regclass('etl_live.never') IS NULL"); got[0][0] != "true" {
			t.Error("a copy canceled before it started created its table")
		}
		kept(t)
	})
}

// opt2 is o set up for the direct path or (with an identity Transform) the
// row path.
func opt2(direct bool, o CopyOptions) CopyOptions {
	if !direct {
		o.Transform = func(r []any) ([]any, error) { return r, nil }
	}
	return o
}

// TestLivePGProgress: the running count a Progress callback sees, on both
// paths, for rows whose values hold newlines (escaped in COPY text, so the
// direct path's newline count is still one per row).
func TestLivePGProgress(t *testing.T) {
	src, dst := livePG(t)
	mustExec(t, src, `CREATE TABLE etl_live.p AS SELECT g AS id, E'a\nb\\nc\r' || g AS s FROM generate_series(1, 10500) g`)
	for _, direct := range []bool{true, false} {
		t.Run(map[bool]string{true: "direct", false: "row"}[direct], func(t *testing.T) {
			var seen []int64
			st, err := Copy(context.Background(), src, "etl_live.p", dst, opt2(direct, CopyOptions{
				To: "etl_live.p_" + map[bool]string{true: "d", false: "r"}[direct], Create: true,
				ProgressEvery: 1000, Progress: func(n int64) { seen = append(seen, n) },
			}))
			if err != nil {
				t.Fatal(err)
			}
			if st.Rows != 10500 {
				t.Errorf("rows = %d", st.Rows)
			}
			// One call per thousand crossed. The row path reports exact
			// multiples; the direct path counts whole buffers, so its
			// numbers land at or past each multiple.
			if len(seen) != 10 {
				t.Fatalf("progress calls = %v, want 10", seen)
			}
			for i, n := range seen {
				lo := int64(i+1) * 1000
				if n < lo || n >= lo+1000 || (!direct && n != lo) {
					t.Errorf("call %d reported %d", i, n)
				}
			}
		})
	}
}

// TestLivePGTransformValues: a Transform may hand back Go values of other
// types than it was given; each must load into its column the way the same
// value would as a literal.
func TestLivePGTransformValues(t *testing.T) {
	src, dst := livePG(t)
	mustExec(t, src, `CREATE TABLE etl_live.tv (id int PRIMARY KEY)`, `INSERT INTO etl_live.tv VALUES (1)`)
	mustExec(t, dst, `CREATE TABLE etl_live.tv (id int PRIMARY KEY, i int, n numeric, txt text, d date,
		ts timestamp, tsz timestamptz, tm time, by bytea, b boolean, j jsonb, iv interval, bc date, ta text[], ia int[])`)
	at := time.Date(2024, 5, 6, 7, 8, 9, 123456000, time.FixedZone("x", 2*3600))
	cols := []string{"id", "i", "n", "txt", "d", "ts", "tsz", "tm", "by", "b", "j", "iv", "bc", "ta", "ia"}
	_, err := Copy(context.Background(), src, "", dst, CopyOptions{
		Query: "SELECT id, NULL AS i, NULL AS n, NULL AS txt, NULL AS d, NULL AS ts, NULL AS tsz, NULL AS tm, " +
			"NULL AS by, NULL AS b, NULL AS j, NULL AS iv, NULL AS bc, NULL AS ta, NULL AS ia FROM etl_live.tv",
		To: "etl_live.tv",
		Transform: func(r []any) ([]any, error) {
			return []any{r[0], "42", 1.25, int64(7), at, at, at, at, []byte{0, 1, 0xff}, "true",
				[]byte(`{"a": [1, "x"]}`), 90*time.Minute + 1500*time.Millisecond,
				time.Date(-99, 12, 31, 0, 0, 0, 0, time.UTC), []string{"a b", `q"uote`, "back\\slash"}, []int64{1, 2, 3}}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := dump(t, dst, "SELECT "+strings.Join(func() []string {
		out := make([]string, len(cols))
		for i, c := range cols {
			out[i] = c + "::text"
		}
		return out
	}(), ", ")+" FROM etl_live.tv")
	want := [][]string{{"1", "42", "1.25", "7", "2024-05-06", "2024-05-06 07:08:09.123456",
		dump(t, dst, "SELECT '2024-05-06 07:08:09.123456+02'::timestamptz::text")[0][0], "07:08:09.123456",
		`\x0001ff`, "true", `{"a": [1, "x"]}`, "01:30:01.5", "0100-12-31 BC",
		`{"a b","q\"uote","back\\slash"}`, "{1,2,3}"}}
	diffRows(t, "transform values", got, want)
}

// TestLivePGSameDatabase: source and destination are one database —
// copying a table to another name, and rewriting a table in place: copied
// onto itself with Truncate and a Transform. A TRUNCATE there would wait on
// the copy's own read for good (see loadOptions); it must be a DELETE, and
// the rewrite must work, through one pool or two.
func TestLivePGSameDatabase(t *testing.T) {
	src, dst := livePG(t)
	mustExec(t, src, `CREATE TABLE etl_live.a (id int PRIMARY KEY, s text)`,
		`INSERT INTO etl_live.a SELECT g, 'r' || g FROM generate_series(1, 300000) g`)
	for _, direct := range []bool{true, false} {
		t.Run(map[bool]string{true: "direct", false: "row"}[direct], func(t *testing.T) {
			st, err := Copy(context.Background(), src, "etl_live.a", src, opt2(direct, CopyOptions{
				To: "etl_live.b", Create: true, Truncate: true,
			}))
			if err != nil || st.Rows != 300000 {
				t.Fatalf("%+v, %v", st, err)
			}
		})
	}
	// A second pool on the same database: what two connection names for
	// one server are.
	other := pgOpen(t, "src-again", os.Getenv("DBC_LIVE_PG_DSN"))
	if !samePGDatabase(context.Background(), src, other) {
		t.Fatal("two pools on one database not recognised as the same")
	}
	if samePGDatabase(context.Background(), src, dst) {
		t.Fatal("two databases on one server taken for the same")
	}

	rewrites := 0
	for _, pools := range []struct {
		name string
		to   Conn
	}{{"one pool", src}, {"two pools", other}} {
		for _, shape := range []string{"table", "query", "view"} {
			name := pools.name + "/" + shape
			t.Run(name, func(t *testing.T) {
				// A deadline turns the hang this guards against into a
				// failure rather than a test that never ends.
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cancel()
				rewrites++
				suffix := "-" + itoa(int64(rewrites))
				opt := CopyOptions{Truncate: true, Transform: func(r []any) ([]any, error) {
					r[1] = strings.SplitN(r[1].(string), "-", 2)[0] + suffix
					return r, nil
				}}
				from := "etl_live.a"
				switch shape {
				case "query":
					opt.Query, opt.To = "SELECT id, s FROM etl_live.a -- all of it", "etl_live.a"
					from = ""
				case "view":
					mustExec(t, src, "CREATE OR REPLACE VIEW etl_live.av AS SELECT id, s FROM etl_live.a")
					from, opt.To = "etl_live.av", "etl_live.a"
				}
				st, err := Copy(ctx, src, from, pools.to, opt)
				if err != nil || st.Rows != 300000 {
					t.Fatalf("%+v, %v", st, err)
				}
				got := dump(t, src, "SELECT count(*), count(*) FILTER (WHERE s = 'r' || id || '"+suffix+"') FROM etl_live.a")
				if !reflect.DeepEqual(got, [][]string{{"300000", "300000"}}) {
					t.Errorf("after the rewrite: rows, rewritten = %q", got)
				}
				assertNoOpenTx(t, src)
			})
		}
	}

	// The direct path onto itself has nothing to rewrite, but must not hang
	// either: it puts the same rows back.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if st, err := Copy(ctx, src, "etl_live.a", src, CopyOptions{Truncate: true}); err != nil || st.Rows != 300000 || !st.Direct {
		t.Fatalf("direct onto itself: %+v, %v", st, err)
	}
	assertNoOpenTx(t, src)
}

// TestLivePGConcurrentCopies: several copies at once over the same two
// pools, on both paths — as a script that fans out with goroutines would.
// Run with -race.
func TestLivePGConcurrentCopies(t *testing.T) {
	src, dst := livePG(t)
	mustExec(t, src, `CREATE TABLE etl_live.c AS SELECT g AS id, md5(g::text) AS h FROM generate_series(1, 50000) g`)
	errs := make(chan error, 8)
	for i := range 8 {
		go func() {
			_, err := Copy(context.Background(), src, "etl_live.c", dst, opt2(i%2 == 0, CopyOptions{
				To: "etl_live.c" + itoa(int64(i)), Create: true, Where: "id % 8 = " + itoa(int64(i)),
			}))
			errs <- err
		}()
	}
	for range 8 {
		if err := <-errs; err != nil {
			t.Error(err)
		}
	}
	for i := range 8 {
		if got := dump(t, dst, "SELECT count(*) FROM etl_live.c"+itoa(int64(i))); got[0][0] != "6250" {
			t.Errorf("c%d has %s rows", i, got[0][0])
		}
	}
	assertSourceIdle(t, src)
	assertNoOpenTx(t, dst)
}

// TestLivePGMoveRows: a Query that changes the source as it reads it — a
// DELETE … RETURNING, the "move rows" copy — on both paths. The direct
// path must be able to describe it without running it, and on either path
// the source's change must commit only once the load has: a load that
// fails, even as late as its commit, leaves the source rows where they
// were.
func TestLivePGMoveRows(t *testing.T) {
	src, dst := livePG(t)
	ctx := context.Background()
	n := 0
	// fresh makes a new five-row source table — a new name per case, since
	// pgx's statement cache trips over a name reused with other columns —
	// and the name its archive on the destination gets.
	fresh := func(t *testing.T) (from, to string) {
		t.Helper()
		n++
		from, to = "etl_live.m"+itoa(int64(n)), "etl_live.arch"+itoa(int64(n))
		mustExec(t, src, "CREATE TABLE "+from+" (id int PRIMARY KEY, name text)",
			"INSERT INTO "+from+" SELECT g, 'n' || g FROM generate_series(1, 5) g")
		return from, to
	}
	ids := func(t *testing.T, c Conn, table string) string {
		t.Helper()
		return dump(t, c, "SELECT coalesce(string_agg(id::text, ',' ORDER BY id), '') FROM "+table)[0][0]
	}
	idle := func(t *testing.T) {
		t.Helper()
		assertNoOpenTx(t, src)
		assertNoOpenTx(t, dst)
	}

	for _, direct := range []bool{true, false} {
		path := map[bool]string{true: "direct", false: "row"}[direct]
		moves := map[string]string{
			"delete returning": "DELETE FROM %s WHERE id <= 3 RETURNING id, name",
			"data-modifying with": "WITH d AS (DELETE FROM %s WHERE id <= 3 RETURNING id, name)\n" +
				"SELECT id, name FROM d -- moved",
		}
		for name, q := range moves {
			t.Run(path+"/"+name, func(t *testing.T) {
				from, to := fresh(t)
				st, err := Copy(ctx, src, "", dst, opt2(direct, CopyOptions{
					Query: fmt.Sprintf(q, from), To: to, Create: true,
				}))
				if err != nil || st.Rows != 3 || st.Direct != direct {
					t.Fatalf("%+v, %v", st, err)
				}
				if got := ids(t, src, from); got != "4,5" {
					t.Errorf("source keeps %q, want 4,5", got)
				}
				if got := ids(t, dst, to); got != "1,2,3" {
					t.Errorf("archive has %q, want 1,2,3", got)
				}
				idle(t)
			})
		}
		t.Run(path+"/load fails at commit", func(t *testing.T) {
			// id 2 is already archived: the key violation reaches the copy
			// when the load ends, after every source row has been read.
			from, to := fresh(t)
			mustExec(t, dst, "CREATE TABLE "+to+" (id int PRIMARY KEY, name text)",
				"INSERT INTO "+to+" VALUES (2, 'already here')")
			_, err := Copy(ctx, src, "", dst, opt2(direct, CopyOptions{
				Query: "DELETE FROM " + from + " RETURNING id, name", To: to,
			}))
			if err == nil || !strings.Contains(err.Error(), "duplicate key") {
				t.Fatalf("err = %v, want a duplicate key", err)
			}
			if got := ids(t, src, from); got != "1,2,3,4,5" {
				t.Errorf("source after a failed move = %q: rows lost", got)
			}
			if got := ids(t, dst, to); got != "2" {
				t.Errorf("archive after a failed move = %q", got)
			}
			idle(t)
		})
		t.Run(path+"/no RETURNING is refused before it runs", func(t *testing.T) {
			from, to := fresh(t)
			for _, q := range []string{
				"DELETE FROM " + from,
				"WITH x AS (SELECT 3 AS id) DELETE FROM " + from + " USING x WHERE " + from + ".id = x.id",
			} {
				_, err := Copy(ctx, src, "", dst, opt2(direct, CopyOptions{Query: q, To: to, Create: true}))
				if err == nil || !strings.Contains(err.Error(), "RETURNING") {
					t.Errorf("%s: err = %v, want a refusal naming RETURNING", q, err)
				}
			}
			if got := ids(t, src, from); got != "1,2,3,4,5" {
				t.Errorf("source after a refused copy = %q", got)
			}
			if got := dump(t, dst, "SELECT to_regclass('"+to+"') IS NULL"); got[0][0] != "true" {
				t.Error("a refused copy created its table")
			}
			idle(t)
		})
	}
	t.Run("direct/placeholders without Args", func(t *testing.T) {
		from, to := fresh(t)
		_, err := Copy(ctx, src, "", dst, CopyOptions{
			Query: "DELETE FROM " + from + " WHERE id = $1 RETURNING id, name", To: to, Create: true,
		})
		if err == nil || !strings.Contains(err.Error(), "Args") {
			t.Errorf("err = %v, want one naming Args", err)
		}
		if got := ids(t, src, from); got != "1,2,3,4,5" {
			t.Errorf("source after a refused copy = %q", got)
		}
		idle(t)
	})

	// Within one database the load can need the rows the Query has deleted
	// but not committed, and the Query commits only after the load: a cycle
	// through this process. The lock_timeout loadOptions sets ends it as an
	// error, and the read's rollback keeps the rows. The deadline is what
	// fails the test if the cycle hangs again.
	defer func(d time.Duration) { sameDBLockTimeout = d }(sameDBLockTimeout)
	sameDBLockTimeout = time.Second
	for _, direct := range []bool{true, false} {
		path := map[bool]string{true: "direct", false: "row"}[direct]
		cycles := map[string]func(from, to string) (string, CopyOptions){
			// The archive's key references the table being emptied: its
			// check waits on the deleted, uncommitted parent rows.
			"foreign key into the source": func(from, to string) (string, CopyOptions) {
				mustExec(t, src, "CREATE TABLE "+to+" (id int PRIMARY KEY REFERENCES "+from+", name text)")
				return to, CopyOptions{To: to}
			},
			// The rows go back into their own table: each key matches a row
			// deleted by the uncommitted read.
			"rows moved back": func(from, _ string) (string, CopyOptions) {
				return from, CopyOptions{To: from}
			},
			// The same with Truncate: the load's DELETE waits on them first.
			"rows moved back, truncating": func(from, _ string) (string, CopyOptions) {
				return from, CopyOptions{To: from, Truncate: true}
			},
		}
		for name, setup := range cycles {
			t.Run(path+"/same database/"+name, func(t *testing.T) {
				from, to := fresh(t)
				to, opt := setup(from, to)
				opt.Query = "DELETE FROM " + from + " WHERE id <= 3 RETURNING id, name"
				ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
				defer cancel()
				_, err := Copy(ctx, src, "", src, opt2(direct, opt))
				if err == nil || !strings.Contains(err.Error(), "lock timeout") ||
					!strings.Contains(err.Error(), "waited on each other") {
					t.Fatalf("err = %v, want a lock timeout with the hint", err)
				}
				if got := ids(t, src, from); got != "1,2,3,4,5" {
					t.Errorf("source after the timed-out move = %q: rows lost", got)
				}
				if to != from {
					if got := ids(t, src, to); got != "" {
						t.Errorf("archive after the timed-out move = %q", got)
					}
				}
				assertNoOpenTx(t, src)
			})
		}
	}
	// A setting already in force stands: the load's set_config is
	// conditional on lock_timeout being off.
	t.Run("same database/an existing lock_timeout stands", func(t *testing.T) {
		conn, err := src.DB.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		var got string
		mustExecConn(t, conn, "BEGIN", "SET LOCAL lock_timeout = '7s'", pgSameDBLockTimeout())
		if err = conn.QueryRowContext(ctx, "SHOW lock_timeout").Scan(&got); err != nil || got != "7s" {
			t.Errorf("lock_timeout = %q, %v; want the 7s already set", got, err)
		}
		mustExecConn(t, conn, "ROLLBACK", "BEGIN", pgSameDBLockTimeout())
		if err = conn.QueryRowContext(ctx, "SHOW lock_timeout").Scan(&got); err != nil || got != "1s" {
			t.Errorf("lock_timeout = %q, %v; want 1s when none was set", got, err)
		}
		mustExecConn(t, conn, "ROLLBACK")
	})
}

func mustExecConn(t *testing.T, c *sql.Conn, stmts ...string) {
	t.Helper()
	for _, s := range stmts {
		if _, err := c.ExecContext(context.Background(), s); err != nil {
			t.Fatal(s, err)
		}
	}
}
