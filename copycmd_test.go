package main

import (
	"bytes"
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

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
