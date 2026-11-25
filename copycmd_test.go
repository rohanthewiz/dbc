package main

import (
	"bytes"
	"context"
	"io"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/urfave/cli/v3"

	"github.com/rohanthewiz/dbc/config"
	"github.com/rohanthewiz/dbc/db"
	"github.com/rohanthewiz/dbc/sdb"
)

// TestCopyCLIParsing checks that copy's own flags land in their variables and
// the table names stay positional, with flags before or after them.
func TestCopyCLIParsing(t *testing.T) {
	cases := []struct {
		name string
		argv []string
		want []string // args
	}{
		{"flags first", []string{"copy", "--from", "a", "--to", "b", "--create", "--truncate",
			"--where", "id > 1", "public.orders", "archive.orders"}, []string{"public.orders", "archive.orders"}},
		{"flags after the table", []string{"copy", "public.orders", "--from", "a", "--to", "b", "--create",
			"--truncate", "--where", "id > 1"}, []string{"public.orders"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resetFlags(t)
			var args []string
			app := newCLI()
			for _, sub := range app.Commands {
				if sub.Name == "copy" {
					sub.Action = func(_ context.Context, cmd *cli.Command) error {
						args = cmd.Args().Slice()
						return nil
					}
				}
			}
			if err := app.Run(context.Background(), append([]string{"dbc"}, c.argv...)); err != nil {
				t.Fatalf("run: %v", err)
			}
			if !reflect.DeepEqual(args, c.want) {
				t.Errorf("args = %q, want %q", args, c.want)
			}
			if flagFrom != "a" || flagTo != "b" || !flagCreate || !flagTruncate || flagWhere != "id > 1" {
				t.Errorf("flags: from=%q to=%q create=%v truncate=%v where=%q",
					flagFrom, flagTo, flagCreate, flagTruncate, flagWhere)
			}
		})
	}
}

// TestCopyCLIParsingN336: --columns splits on commas and repeats, --batch
// is a number, and --query marks itself given even when empty, so an empty
// $Q in `--query "$Q"` is not taken for no --query at all.
func TestCopyCLIParsingN336(t *testing.T) {
	parse := func(t *testing.T, argv ...string) []string {
		t.Helper()
		resetFlags(t)
		var args []string
		app := newCLI()
		for _, sub := range app.Commands {
			if sub.Name == "copy" {
				sub.Action = func(_ context.Context, cmd *cli.Command) error {
					args = cmd.Args().Slice()
					return nil
				}
			}
		}
		if err := app.Run(context.Background(), append([]string{"dbc", "copy", "--from", "a", "--to", "b"}, argv...)); err != nil {
			t.Fatalf("run: %v", err)
		}
		return args
	}

	args := parse(t, "--columns", "id, name", "--columns", "email", "--batch", "50", "users")
	if want := []string{"id", " name", "email"}; !reflect.DeepEqual(flagColumns, want) {
		t.Errorf("--columns = %q, want %q (trimmed later, by copyColumns)", flagColumns, want)
	}
	if flagBatch != 50 || !reflect.DeepEqual(args, []string{"users"}) {
		t.Errorf("--batch = %d, args = %q", flagBatch, args)
	}
	if flagQuerySet {
		t.Error("--query marked given when it was not")
	}

	args = parse(t, "--query", "SELECT 1 AS n", "totals")
	if flagQuery != "SELECT 1 AS n" || !flagQuerySet || !reflect.DeepEqual(args, []string{"totals"}) {
		t.Errorf("--query = %q (set %v), args = %q", flagQuery, flagQuerySet, args)
	}
	parse(t, "--query", "", "totals")
	if !flagQuerySet {
		t.Error("an empty --query was not marked given")
	}
}

func TestCopyRequest(t *testing.T) {
	cases := []struct {
		name     string
		from, to string
		args     []string
		want     copyReq
		err      string // substring; "" = no error
	}{
		{name: "no --from", to: "b", args: []string{"t"}, err: "both --from and --to"},
		{name: "no --to", from: "a", args: []string{"t"}, err: "both --from and --to"},
		{name: "no table", from: "a", to: "b", err: "no table"},
		{name: "three arguments", from: "a", to: "b", args: []string{"t", "u", "v"}, err: "got 3 arguments"},
		{name: "blank table", from: "a", to: "b", args: []string{"  "}, err: "table name is empty"},
		{name: "blank destination", from: "a", to: "b", args: []string{"t", " "}, err: "destination table name is empty"},
		{name: "onto itself", from: "a", to: "a", args: []string{"t"}, err: "onto itself"},
		{name: "onto itself, named twice", from: "a", to: "a", args: []string{"t", "t"}, err: "onto itself"},
		{name: "same name across connections", from: "a", to: "b", args: []string{"t"},
			want: copyReq{from: "a", to: "b", table: "t"}},
		{name: "renamed on one connection", from: "a", to: "a", args: []string{"t", "t_bak"},
			want: copyReq{from: "a", to: "a", table: "t", dest: "t_bak", opts: sdb.CopyOpts{To: "t_bak"}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resetFlags(t)
			flagFrom, flagTo = c.from, c.to
			got, err := copyRequest(c.args)
			if c.err != "" {
				if err == nil || !strings.Contains(err.Error(), c.err) {
					t.Fatalf("err = %v, want one containing %q", err, c.err)
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("got  %+v\nwant %+v", got, c.want)
			}
		})
	}

	// the option flags carry through to CopyOpts
	resetFlags(t)
	flagFrom, flagTo, flagCreate, flagTruncate, flagWhere = "a", "b", true, true, "id > 1"
	got, err := copyRequest([]string{"t"})
	if err != nil {
		t.Fatal(err)
	}
	if want := (sdb.CopyOpts{Create: true, Truncate: true, Where: "id > 1"}); !reflect.DeepEqual(got.opts, want) {
		t.Errorf("opts = %+v, want %+v", got.opts, want)
	}
}

// TestCopyRequestN336 covers --columns, --query and --batch: what each
// becomes in CopyOpts, and the shapes refused before anything is opened.
func TestCopyRequestN336(t *testing.T) {
	type flags struct {
		where    string
		columns  []string
		query    string
		querySet bool
		batch    int
	}
	cases := []struct {
		name     string
		from, to string
		f        flags
		args     []string
		want     copyReq
		err      string
	}{
		{name: "columns, trimmed", from: "a", to: "b", f: flags{columns: []string{"id", " name ", "email"}},
			args: []string{"users"},
			want: copyReq{from: "a", to: "b", table: "users", opts: sdb.CopyOpts{Columns: []string{"id", "name", "email"}}}},
		{name: "an empty column", from: "a", to: "b", f: flags{columns: []string{"id", " "}}, args: []string{"users"},
			err: "empty name"},
		{name: "a column twice", from: "a", to: "b", f: flags{columns: []string{"id", "name", "id"}}, args: []string{"users"},
			err: "names id twice"},
		{name: "batch", from: "a", to: "b", f: flags{batch: 2000}, args: []string{"t"},
			want: copyReq{from: "a", to: "b", table: "t", opts: sdb.CopyOpts{BatchSize: 2000}}},
		{name: "negative batch", from: "a", to: "b", f: flags{batch: -1}, args: []string{"t"}, err: "above 0"},
		{name: "query", from: "a", to: "b", f: flags{query: " SELECT 1 AS n ", querySet: true}, args: []string{"totals"},
			want: copyReq{from: "a", to: "b", dest: "totals", opts: sdb.CopyOpts{Query: "SELECT 1 AS n", To: "totals"}}},
		// a query's rows onto its own connection are no self-copy
		{name: "query on one connection", from: "a", to: "a", f: flags{query: "SELECT * FROM t", querySet: true},
			args: []string{"t_summary"},
			want: copyReq{from: "a", to: "a", dest: "t_summary", opts: sdb.CopyOpts{Query: "SELECT * FROM t", To: "t_summary"}}},
		{name: "query and batch", from: "a", to: "b", f: flags{query: "SELECT 1", querySet: true, batch: 10},
			args: []string{"x"},
			want: copyReq{from: "a", to: "b", dest: "x", opts: sdb.CopyOpts{Query: "SELECT 1", To: "x", BatchSize: 10}}},
		{name: "empty query", from: "a", to: "b", f: flags{querySet: true}, args: []string{"t"}, err: "--query is empty"},
		{name: "blank query", from: "a", to: "b", f: flags{query: "  ", querySet: true}, args: []string{"t"},
			err: "--query is empty"},
		{name: "query without a destination", from: "a", to: "b", f: flags{query: "SELECT 1", querySet: true},
			err: "destination table's name"},
		{name: "query with two arguments", from: "a", to: "b", f: flags{query: "SELECT 1", querySet: true},
			args: []string{"src", "dst"}, err: "takes one: the destination table"},
		{name: "query with a blank destination", from: "a", to: "b", f: flags{query: "SELECT 1", querySet: true},
			args: []string{" "}, err: "destination table name is empty"},
		{name: "query and where", from: "a", to: "b", f: flags{query: "SELECT 1", querySet: true, where: "x > 1"},
			args: []string{"t"}, err: "replaces --where and --columns"},
		{name: "query and columns", from: "a", to: "b", f: flags{query: "SELECT 1", querySet: true, columns: []string{"x"}},
			args: []string{"t"}, err: "replaces --where and --columns"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resetFlags(t)
			flagFrom, flagTo = c.from, c.to
			flagWhere, flagColumns, flagQuery, flagQuerySet, flagBatch = c.f.where, c.f.columns, c.f.query, c.f.querySet, c.f.batch
			got, err := copyRequest(c.args)
			if c.err != "" {
				if err == nil || !strings.Contains(err.Error(), c.err) {
					t.Fatalf("err = %v, want one containing %q", err, c.err)
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("got  %+v\nwant %+v", got, c.want)
			}
		})
	}
}

// TestCheckCopyFlags: every root flag copy would ignore is refused, and the
// defaults (including an explicit -t text) are not.
func TestCheckCopyFlags(t *testing.T) {
	cases := []struct {
		name string
		set  func()
		err  string
	}{
		{"defaults", func() { flagFormat = "text" }, ""},
		{"-c", func() { flagConn = "pg" }, "not -c"},
		{"--file", func() { flagFile = "q.sql" }, "--file"},
		{"--tx", func() { flagTx = true }, "--tx"},
		{"--keep-going", func() { flagKeep = true }, "--keep-going"},
		{"--format", func() { flagFormat = "json" }, "--format"},
		{"--out", func() { flagOut = "x.txt" }, "--out"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resetFlags(t)
			c.set()
			err := checkCopyFlags()
			if c.err == "" {
				if err != nil {
					t.Fatalf("err = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Fatalf("err = %v, want one containing %q", err, c.err)
			}
		})
	}
}

func TestCheckCopyConns(t *testing.T) {
	cfg := &config.Config{Connections: []config.Connection{{Name: "a"}, {Name: "b"}, {Name: "pg", Driver: "postgres"}}}
	if err := checkCopyConns(cfg, copyReq{from: "a", to: "b"}); err != nil {
		t.Errorf("known names: %v", err)
	}
	// a database derived from a configured server ("pg/reporting") is a
	// name s.Copy accepts, so the command must too
	if err := checkCopyConns(cfg, copyReq{from: "pg/reporting", to: "b"}); err != nil {
		t.Errorf("derived database: %v", err)
	}
	if err := checkCopyConns(cfg, copyReq{from: "a", to: "nope"}); err == nil || !strings.Contains(err.Error(), `"nope"`) {
		t.Errorf("unknown --to: err = %v", err)
	}

	// --batch: refused into Postgres (a derived database of it too), which
	// loads by COPY; taken into anything else, and from Postgres
	cfg.Connections = append(cfg.Connections, config.Connection{Name: "lite", Driver: "sqlite"})
	batch := sdb.CopyOpts{BatchSize: 100}
	for _, to := range []string{"pg", "pg/reporting"} {
		if err := checkCopyConns(cfg, copyReq{from: "lite", to: to, opts: batch}); err == nil ||
			!strings.Contains(err.Error(), "--batch") {
			t.Errorf("--batch into %s: err = %v", to, err)
		}
	}
	if err := checkCopyConns(cfg, copyReq{from: "pg", to: "lite", opts: batch}); err != nil {
		t.Errorf("--batch into sqlite: %v", err)
	}
	if err := checkCopyConns(cfg, copyReq{from: "lite", to: "pg"}); err != nil {
		t.Errorf("into Postgres without --batch: %v", err)
	}
}

// newCopyManager is two file SQLite databases, src seeded with the demo's
// cats table and dst empty, so runCopy goes through real connections end to
// end (the cross-engine and Postgres paths are etl's own tests).
func newCopyManager(t *testing.T) *db.Manager {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{MaxRows: 1000, Connections: []config.Connection{
		{Name: "src", Driver: "sqlite", DSN: filepath.Join(dir, "src.db")},
		{Name: "dst", Driver: "sqlite", DSN: filepath.Join(dir, "dst.db")},
	}}
	mgr := db.NewManager(cfg)
	t.Cleanup(mgr.Close)
	if err := db.SeedDemo(mgr, "src"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return mgr
}

// count returns count(*) of table on conn, through the same pool runCopy used.
func count(t *testing.T, mgr *db.Manager, conn, table string) int {
	t.Helper()
	dbh, err := mgr.DB(conn)
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err = dbh.QueryRow("SELECT count(*) FROM " + table).Scan(&n); err != nil {
		t.Fatalf("count %s:%s: %v", conn, table, err)
	}
	return n
}

// TestRunCopy walks the shapes the command line can ask for, in the order a
// user would meet them: a missing destination, --create, a re-run that would
// duplicate the key, --truncate, --where, and a renamed destination.
func TestRunCopy(t *testing.T) {
	mgr := newCopyManager(t)
	ctx := context.Background()
	all := count(t, mgr, "src", "cats")
	if all == 0 {
		t.Fatal("the demo seeded no cats")
	}
	req := func(dest string, opts sdb.CopyOpts) copyReq {
		opts.To = dest
		return copyReq{from: "src", to: "dst", table: "cats", dest: dest, opts: opts}
	}

	// no --create: the destination must exist, and the failure says so
	if _, err := runCopy(ctx, mgr, req("", sdb.CopyOpts{}), nil); err == nil {
		t.Fatal("copy into a missing table succeeded")
	}

	st, err := runCopy(ctx, mgr, req("", sdb.CopyOpts{Create: true}), nil)
	if err != nil {
		t.Fatalf("--create: %v", err)
	}
	if st.Rows != int64(all) || count(t, mgr, "dst", "cats") != all {
		t.Fatalf("--create copied %d rows, dst has %d, want %d", st.Rows, count(t, mgr, "dst", "cats"), all)
	}
	if !strings.Contains(st.String(), "src:cats → dst:cats") {
		t.Errorf("summary = %q", st)
	}

	// again without --truncate: the carried-over key refuses the duplicates,
	// and the failed load leaves the table as it was
	if _, err = runCopy(ctx, mgr, req("", sdb.CopyOpts{}), nil); err == nil {
		t.Fatal("re-copy onto the same keys succeeded")
	}
	if n := count(t, mgr, "dst", "cats"); n != all {
		t.Fatalf("after the failed re-copy dst has %d rows, want %d", n, all)
	}

	// --truncate --where: replaced by the filtered rows, not added to
	want := count(t, mgr, "src", "cats WHERE id > 2")
	if want == 0 || want == all {
		t.Fatalf("filter selects %d of %d rows; the test needs a strict subset", want, all)
	}
	if st, err = runCopy(ctx, mgr, req("", sdb.CopyOpts{Truncate: true, Where: "id > 2"}), nil); err != nil {
		t.Fatalf("--truncate --where: %v", err)
	}
	if st.Rows != int64(want) || count(t, mgr, "dst", "cats") != want {
		t.Fatalf("--truncate --where copied %d, dst has %d, want %d", st.Rows, count(t, mgr, "dst", "cats"), want)
	}

	// a destination name of its own, and a progress writer that, for a table
	// this small, never fires
	var progress bytes.Buffer
	if st, err = runCopy(ctx, mgr, req("cats_copy", sdb.CopyOpts{Create: true}), &progress); err != nil {
		t.Fatalf("renamed: %v", err)
	}
	if count(t, mgr, "dst", "cats_copy") != all || !strings.Contains(st.String(), "dst:cats_copy") {
		t.Fatalf("renamed copy: %s, dst has %d rows", st, count(t, mgr, "dst", "cats_copy"))
	}
	if progress.Len() != 0 {
		t.Errorf("progress for %d rows: %q", all, progress.String())
	}
}

// columnsOf is table's column names on conn, in order.
func columnsOf(t *testing.T, mgr *db.Manager, conn, table string) []string {
	t.Helper()
	dbh, err := mgr.DB(conn)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := dbh.Query("SELECT * FROM " + table + " WHERE 1 = 0")
	if err != nil {
		t.Fatalf("columns of %s:%s: %v", conn, table, err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	return cols
}

// TestRunCopyN336 runs --columns, --query and --batch end to end, each
// through copyRequest as the command line would build it, so the flags are
// checked to reach etl and not only to parse.
func TestRunCopyN336(t *testing.T) {
	mgr := newCopyManager(t)
	ctx := context.Background()
	all := count(t, mgr, "src", "cats")
	build := func(t *testing.T, set func(), args ...string) copyReq {
		t.Helper()
		resetFlags(t)
		flagFrom, flagTo = "src", "dst"
		set()
		req, err := copyRequest(args)
		if err != nil {
			t.Fatal(err)
		}
		return req
	}

	t.Run("columns", func(t *testing.T) {
		req := build(t, func() { flagCreate, flagColumns = true, []string{"name", " id"} }, "cats", "cat_names")
		st, err := runCopy(ctx, mgr, req, nil)
		if err != nil {
			t.Fatal(err)
		}
		// made with only the named columns, in the order named
		if got := columnsOf(t, mgr, "dst", "cat_names"); !reflect.DeepEqual(got, []string{"name", "id"}) {
			t.Errorf("columns = %q", got)
		}
		if st.Rows != int64(all) || count(t, mgr, "dst", "cat_names") != all {
			t.Errorf("%s; dst has %d", st, count(t, mgr, "dst", "cat_names"))
		}
	})

	t.Run("query", func(t *testing.T) {
		req := build(t, func() {
			flagCreate, flagQuerySet = true, true
			flagQuery = "SELECT breed, count(*) AS n FROM cats GROUP BY breed"
		}, "breeds")
		st, err := runCopy(ctx, mgr, req, nil)
		if err != nil {
			t.Fatal(err)
		}
		want := count(t, mgr, "src", "(SELECT DISTINCT breed FROM cats)")
		if st.Rows != int64(want) || count(t, mgr, "dst", "breeds") != want {
			t.Errorf("%s; dst has %d, want %d", st, count(t, mgr, "dst", "breeds"), want)
		}
		if !strings.Contains(st.String(), "src:query → dst:breeds") {
			t.Errorf("summary = %q", st)
		}
		if got := columnsOf(t, mgr, "dst", "breeds"); !reflect.DeepEqual(got, []string{"breed", "n"}) {
			t.Errorf("columns = %q", got)
		}
	})

	t.Run("batch", func(t *testing.T) {
		// a batch of 1: one INSERT a row, and every row still lands
		req := build(t, func() { flagCreate, flagBatch = true, 1 }, "cats", "cats_one_by_one")
		if req.opts.BatchSize != 1 {
			t.Fatalf("BatchSize = %d", req.opts.BatchSize)
		}
		st, err := runCopy(ctx, mgr, req, nil)
		if err != nil {
			t.Fatal(err)
		}
		if st.Rows != int64(all) || count(t, mgr, "dst", "cats_one_by_one") != all {
			t.Errorf("%s; dst has %d", st, count(t, mgr, "dst", "cats_one_by_one"))
		}
	})
}

// TestRunCopyCanceled: a canceled copy is reported as a cancellation, so
// copyAction exits 130 rather than "copy failed".
func TestRunCopyCanceled(t *testing.T) {
	mgr := newCopyManager(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := runCopy(ctx, mgr, copyReq{from: "src", to: "dst", table: "cats", opts: sdb.CopyOpts{Create: true}}, nil)
	if err == nil || !sdb.IsCanceled(err) {
		t.Fatalf("err = %v, want a cancellation", err)
	}
}

// TestCopyRequestsTables: --tables is one copyReq a table, each with the
// options a single copy would have, and the shapes it cannot mean are
// refused before anything is opened.
func TestCopyRequestsTables(t *testing.T) {
	// no --tables: copyRequest's one request
	resetFlags(t)
	flagFrom, flagTo = "a", "b"
	reqs, err := copyRequests([]string{"t", "t2"})
	if err != nil || len(reqs) != 1 || reqs[0].dest != "t2" {
		t.Fatalf("one table: %+v %v", reqs, err)
	}

	resetFlags(t)
	flagFrom, flagTo, flagCreate, flagWhere = "a", "b", true, "id > 1"
	flagColumns, flagTables = []string{"id"}, []string{"cats", " owners ", "public.toys"}
	reqs, err = copyRequests(nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := tableNames(reqs); !reflect.DeepEqual(got, []string{"cats", "owners", "public.toys"}) {
		t.Errorf("tables = %q", got)
	}
	for _, r := range reqs {
		want := sdb.CopyOpts{Create: true, Where: "id > 1", Columns: []string{"id"}}
		if r.from != "a" || r.to != "b" || r.dest != "" || !reflect.DeepEqual(r.opts, want) {
			t.Errorf("%s: %+v", r.table, r)
		}
	}

	cases := []struct {
		name string
		set  func()
		args []string
		err  string
	}{
		{"and a table argument", func() {}, []string{"orders"}, "put it in --tables"},
		{"and --query", func() { flagQuery, flagQuerySet = "SELECT 1", true }, nil, "give one of them"},
		{"on one connection", func() { flagTo = "a" }, nil, "onto itself"},
		{"a table twice", func() { flagTables = []string{"cats", "owners", "cats"} }, nil, "names cats twice"},
		{"an empty name", func() { flagTables = []string{"cats", " "} }, nil, "empty name"},
		{"no --to", func() { flagTo = "" }, nil, "both --from and --to"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resetFlags(t)
			flagFrom, flagTo, flagTables = "a", "b", []string{"cats", "owners"}
			c.set()
			if _, err := copyRequests(c.args); err == nil || !strings.Contains(err.Error(), c.err) {
				t.Fatalf("err = %v, want one containing %q", err, c.err)
			}
		})
	}

	// -k is a copy flag once there are tables to go on to
	resetFlags(t)
	flagKeep, flagTables = true, []string{"cats", "owners"}
	if err := checkCopyFlags(); err != nil {
		t.Errorf("-k with --tables: %v", err)
	}
	flagTables = nil
	if err := checkCopyFlags(); err == nil {
		t.Error("-k with one table was not refused")
	}
}

// seedOwners adds a second table to the copy manager's src, so a copy of
// several tables has more than cats to go through.
func seedOwners(t *testing.T, mgr *db.Manager) {
	t.Helper()
	dbh, err := mgr.DB("src")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = dbh.Exec(`CREATE TABLE owners (id INTEGER PRIMARY KEY, name TEXT);
		INSERT INTO owners VALUES (1, 'ann'), (2, 'bo')`); err != nil {
		t.Fatal(err)
	}
}

// TestCopyMany walks --tables through a failure in the middle: without -k
// it stops there, with it it goes on; each table's summary line comes as
// it ends, and the total names what failed and what was not tried.
func TestCopyMany(t *testing.T) {
	mgr := newCopyManager(t)
	seedOwners(t, mgr)
	ctx := context.Background()
	cats := count(t, mgr, "src", "cats")
	reqs := func(opts sdb.CopyOpts) []copyReq {
		var out []copyReq
		for _, tb := range []string{"cats", "missing", "owners"} {
			out = append(out, copyReq{from: "src", to: "dst", table: tb, opts: opts})
		}
		return out
	}
	var failed []string
	onFail := func(r copyReq, err error) { failed = append(failed, r.table) }

	// stop at the first failure: cats stays copied, owners is not tried
	var out bytes.Buffer
	r := copyMany(ctx, mgr, reqs(sdb.CopyOpts{Create: true}), false, &out, nil, onFail)
	if r.done != 1 || r.rows != int64(cats) || !reflect.DeepEqual(r.failed, []string{"missing"}) ||
		!reflect.DeepEqual(r.notTried, []string{"owners"}) || r.firstErr == nil || r.canceled != "" {
		t.Errorf("without -k: %+v", r)
	}
	if lines := strings.Split(strings.TrimSpace(out.String()), "\n"); len(lines) != 1 ||
		!strings.Contains(lines[0], "src:cats → dst:cats") {
		t.Errorf("summary lines:\n%s", out.String())
	}
	if !reflect.DeepEqual(failed, []string{"missing"}) {
		t.Errorf("onFail saw %q", failed)
	}
	total := r.String()
	for _, want := range []string{"copied 1 of 3 tables, " + strconv.Itoa(cats) + " rows in ", "; failed: missing",
		"; not tried: owners"} {
		if !strings.Contains(total, want) {
			t.Errorf("total %q has no %q", total, want)
		}
	}
	if count(t, mgr, "dst", "cats") != cats {
		t.Error("cats was not kept after a later table failed")
	}

	// -k: past the failure to owners (--truncate, as cats is there now)
	out.Reset()
	failed = nil
	r = copyMany(ctx, mgr, reqs(sdb.CopyOpts{Create: true, Truncate: true}), true, &out, nil, onFail)
	if r.done != 2 || r.rows != int64(cats+2) || !reflect.DeepEqual(r.failed, []string{"missing"}) || len(r.notTried) != 0 {
		t.Errorf("with -k: %+v", r)
	}
	if !strings.Contains(r.String(), "copied 2 of 3 tables") || strings.Contains(r.String(), "not tried") {
		t.Errorf("total = %q", r.String())
	}
	if count(t, mgr, "dst", "owners") != 2 || count(t, mgr, "dst", "cats") != cats {
		t.Errorf("dst: %d owners, %d cats", count(t, mgr, "dst", "owners"), count(t, mgr, "dst", "cats"))
	}

	// all copied: nothing failed, nothing named
	r = copyMany(ctx, mgr, []copyReq{reqs(sdb.CopyOpts{Truncate: true})[0], reqs(sdb.CopyOpts{Truncate: true})[2]},
		false, io.Discard, nil, onFail)
	if r.done != 2 || len(r.failed) != 0 || strings.Contains(r.String(), ";") {
		t.Errorf("all copied: %+v %q", r, r.String())
	}

	// a cancel stops it at once, with or without -k: the table it stopped
	// rolled back, the rest not tried
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	r = copyMany(cctx, mgr, reqs(sdb.CopyOpts{Truncate: true}), true, io.Discard, nil, onFail)
	if r.canceled != "cats" || !reflect.DeepEqual(r.notTried, []string{"missing", "owners"}) || len(r.failed) != 0 {
		t.Errorf("canceled: %+v", r)
	}
	if !strings.Contains(r.String(), "; canceled: cats; not tried: missing, owners") {
		t.Errorf("canceled total = %q", r.String())
	}
	if count(t, mgr, "dst", "cats") != cats {
		t.Error("the canceled copy's truncate was not rolled back")
	}
}

// TestPlanMany: the dry run of several tables plans every one, whatever
// fails, and lists the problems in its total.
func TestPlanMany(t *testing.T) {
	mgr := newCopyManager(t)
	seedOwners(t, mgr)
	ctx := context.Background()
	reqs := []copyReq{
		{from: "src", to: "dst", table: "cats", opts: sdb.CopyOpts{Create: true}}, // missing, would be made
		{from: "src", to: "dst", table: "nope"},                                   // no such source: an error
		{from: "src", to: "dst", table: "owners"},                                 // missing, no --create: a problem
	}
	var out bytes.Buffer
	var errs []string
	r := planMany(ctx, mgr, reqs, &out, func(r copyReq, err error) { errs = append(errs, r.table) })
	if r.done != 1 || !reflect.DeepEqual(r.failed, []string{"nope", "owners"}) || !reflect.DeepEqual(errs, []string{"nope"}) {
		t.Errorf("plans: %+v, errors for %q", r, errs)
	}
	if n := strings.Count(out.String(), "dry run: "); n != 2 {
		t.Errorf("%d plans printed:\n%s", n, out.String())
	}
	if !strings.Contains(out.String(), "\n\ndry run: src:owners") {
		t.Errorf("plans not set apart by a blank line:\n%s", out.String())
	}
	if want := "dry run of 3 tables, nothing was written; problem: nope, owners"; r.String() != want {
		t.Errorf("total = %q, want %q", r.String(), want)
	}
	dbh, err := mgr.DB("dst")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dbh.Exec("SELECT 1 FROM cats"); err == nil {
		t.Error("the dry run created cats")
	}
}

// TestCopyMeterLine: the progress line with and without a total, its
// percentage and time left, the rate only from a tenth of a second, and a
// copy that has passed its count (rows arrived since) back to the plain
// count rather than "104%".
func TestCopyMeterLine(t *testing.T) {
	m := &copyMeter{label: "a:t → b:t", total: -1}
	for _, c := range []struct {
		total int64
		n     int64
		took  time.Duration
		want  string
	}{
		{-1, 100000, 50 * time.Millisecond, "  a:t → b:t: 100000 rows"},
		{-1, 100000, time.Second, "  a:t → b:t: 100000 rows, 100000 rows/s"},
		{400000, 100000, time.Second, "  a:t → b:t: 100000 of 400000 rows (25%), 100000 rows/s, ~3s left"},
		{400000, 100000, 10 * time.Millisecond, "  a:t → b:t: 100000 of 400000 rows (25%)"},
		{400000, 400000, 4 * time.Second, "  a:t → b:t: 400000 of 400000 rows (100%), 100000 rows/s"},
		{400000, 399000, 4 * time.Second, "  a:t → b:t: 399000 of 400000 rows (99%), 99750 rows/s"},
		{400000, 500000, 5 * time.Second, "  a:t → b:t: 500000 rows, 100000 rows/s"},
		{3000000, 100000, time.Second, "  a:t → b:t: 100000 of 3000000 rows (3%), 100000 rows/s, ~29s left"},
		{30000000, 100000, time.Second, "  a:t → b:t: 100000 of 30000000 rows (0%), 100000 rows/s, ~4m59s left"},
	} {
		m.total = c.total
		if got := m.line(c.n, c.took); got != c.want {
			t.Errorf("total %d, %d rows in %s:\n got  %q\n want %q", c.total, c.n, c.took, got, c.want)
		}
	}

	// the ends are named as the summary names them
	for _, c := range []struct {
		req  copyReq
		want string
	}{
		{copyReq{from: "a", to: "b", table: "t"}, "a:t → b:t"},
		{copyReq{from: "a", to: "b", table: "t", dest: "u"}, "a:t → b:u"},
		{copyReq{from: "a", to: "b", dest: "x", opts: sdb.CopyOpts{Query: "SELECT 1", To: "x"}}, "a:query → b:x"},
	} {
		if got := newCopyMeter(io.Discard, c.req).label; got != c.want {
			t.Errorf("label = %q, want %q", got, c.want)
		}
	}
}

// TestCopyMeterCounted: the count is said when progress lines will follow
// it, not for a table too small to get any, and nothing is printed once
// the copy has ended.
func TestCopyMeterCounted(t *testing.T) {
	var buf bytes.Buffer
	m := &copyMeter{w: &buf, label: "a:t → b:t", total: -1, start: time.Now()}
	m.counted(copyProgressEvery - 1)
	if buf.Len() != 0 || m.total != copyProgressEvery-1 {
		t.Errorf("a small count: %q, total %d", buf.String(), m.total)
	}
	m.counted(250000)
	if got := buf.String(); got != "  a:t → b:t: 250000 rows to copy\n" {
		t.Errorf("a big count: %q", got)
	}
	buf.Reset()
	m.done = true
	m.counted(300000)
	m.progress(100000)
	if buf.Len() != 0 || m.total != 250000 {
		t.Errorf("after the copy: %q, total %d", buf.String(), m.total)
	}
}

// TestCopyMeterCountBeside: the background count reaches the meter, and a
// query that writes is never run by it.
func TestCopyMeterCountBeside(t *testing.T) {
	mgr := newCopyManager(t)
	ctx := context.Background()
	cats := count(t, mgr, "src", "cats")
	s := sdb.New(mgr, nil, nil).WithContext(ctx)

	req := copyReq{from: "src", to: "dst", table: "cats", opts: sdb.CopyOpts{Where: "id > 2"}}
	m := newCopyMeter(io.Discard, req)
	stop := m.countBeside(ctx, s, req)
	want := int64(count(t, mgr, "src", "cats WHERE id > 2"))
	deadline := time.Now().Add(10 * time.Second)
	for {
		m.mu.Lock()
		total := m.total
		m.mu.Unlock()
		if total == want {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("total = %d after 10s, want %d", total, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
	stop()

	move := copyReq{from: "src", to: "dst", dest: "moved",
		opts: sdb.CopyOpts{Query: "DELETE FROM cats RETURNING *", To: "moved"}}
	m = newCopyMeter(io.Discard, move)
	m.countBeside(ctx, s, move)() // started and stopped: the goroutine has ended
	if m.total != -1 || count(t, mgr, "src", "cats") != cats {
		t.Errorf("a write: total %d, %d cats left of %d", m.total, count(t, mgr, "src", "cats"), cats)
	}
}

// TestRunCopyProgress: a copy big enough for progress lines gets them,
// each with its rate, and the percentage once the count is in; nothing
// is written after runCopy returns.
func TestRunCopyProgress(t *testing.T) {
	mgr := newCopyManager(t)
	dbh, err := mgr.DB("src")
	if err != nil {
		t.Fatal(err)
	}
	const rows = 2*copyProgressEvery + 10_000
	if _, err = dbh.Exec(`CREATE TABLE big (id INTEGER PRIMARY KEY, v TEXT);
		WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x + 1 FROM c WHERE x < ` + strconv.Itoa(rows) + `)
		INSERT INTO big SELECT x, 'v' || x FROM c`); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	st, err := runCopy(context.Background(), mgr, copyReq{from: "src", to: "dst", table: "big",
		opts: sdb.CopyOpts{Create: true}}, &buf)
	if err != nil {
		t.Fatal(err)
	}
	if st.Rows != rows {
		t.Fatalf("copied %d rows, want %d", st.Rows, rows)
	}
	out := buf.String()
	// which lines carry the total depends on when the count lands — on
	// SQLite well before the first 100,000 rows are loaded, but that is
	// not promised — so each line is checked for what it must hold either
	// way
	var progress int
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		switch {
		case strings.HasSuffix(line, ": "+strconv.Itoa(rows)+" rows to copy"):
		case strings.HasPrefix(line, "  src:big → dst:big: 100000 "), strings.HasPrefix(line, "  src:big → dst:big: 200000 "):
			progress++
			if strings.Contains(line, " of ") && !strings.Contains(line, " of "+strconv.Itoa(rows)+" rows (") {
				t.Errorf("a percentage of the wrong total: %q", line)
			}
		default:
			t.Errorf("unexpected line %q", line)
		}
	}
	if progress != 2 {
		t.Errorf("%d progress lines, want 2:\n%s", progress, out)
	}
	n := buf.Len()
	time.Sleep(50 * time.Millisecond)
	if buf.Len() != n {
		t.Errorf("written after runCopy returned: %q", buf.String()[n:])
	}
	t.Logf("progress:\n%s", out)
}
