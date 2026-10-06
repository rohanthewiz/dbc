package db

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/rohanthewiz/dbc/config"
)

func TestNoticeString(t *testing.T) {
	cases := []struct {
		n    Notice
		want string
	}{
		{Notice{Severity: "NOTICE", Message: "users : 42"}, "NOTICE: users : 42"},
		{Notice{Severity: "WARNING", Message: "m", Detail: "d", Hint: "h"}, "WARNING: m — DETAIL: d — HINT: h"},
		{Notice{Severity: "INFO", Message: "m", Hint: "h"}, "INFO: m — HINT: h"},
	}
	for _, c := range cases {
		if got := c.n.String(); got != c.want {
			t.Errorf("String() = %q, want %q", got, c.want)
		}
	}
}

// Past maxNotices the buffer counts rather than keeps, and take says how
// many it left out; take and reset both start the next statement clean.
func TestNoticeBufCapAndTake(t *testing.T) {
	b := &noticeBuf{}
	for range maxNotices + 5 {
		b.add(Notice{Severity: "NOTICE", Message: "x"})
	}
	got := b.take()
	if len(got) != maxNotices+1 {
		t.Fatalf("take: %d notices, want %d (cap + the summary)", len(got), maxNotices+1)
	}
	if last := got[len(got)-1].Message; !strings.Contains(last, "5 more notices") {
		t.Errorf("summary = %q, want it to count the 5 dropped", last)
	}
	if again := b.take(); again != nil {
		t.Errorf("second take = %v, want nil", again)
	}
	b.add(Notice{Message: "stale"})
	b.reset()
	if got := b.take(); got != nil {
		t.Errorf("take after reset = %v, want nil", got)
	}
}

// A session on an engine without notices has none to give, and does not
// register anything to unregister.
func TestSessionNoticesNilOffPostgres(t *testing.T) {
	mgr := NewManager(&config.Config{MaxRows: 10, Connections: []config.Connection{
		{Name: "mem", Driver: "sqlite", DSN: "file:" + memName(t) + "?mode=memory&cache=shared"},
	}})
	defer mgr.Close()
	s, err := mgr.Session(context.Background(), "mem")
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	defer s.Close()
	if _, err = s.Run(context.Background(), "SELECT 1"); err != nil {
		t.Fatalf("run: %v", err)
	}
	if n := s.Notices(); n != nil {
		t.Errorf("Notices = %v, want nil", n)
	}
	if s.noticeKey != nil {
		t.Error("a non-Postgres session registered a notice sink")
	}
}

// RAISE NOTICE reaches the session that ran it — in order, kept even when
// the statement then fails — and only that statement's: the next Run starts
// empty. RAISE EXCEPTION's DETAIL and HINT reach its error. Closing the
// session unregisters its sink.
func TestLiveSessionNotices(t *testing.T) {
	ctx := context.Background()
	mgr := liveMgr(t, "DBC_LIVE_PG_DSN", "postgres")
	s, err := mgr.Session(ctx, "live")
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	if _, err = s.Run(ctx, `DO $$
		DECLARE r record;
		BEGIN
			FOR r IN SELECT g FROM generate_series(1, 3) g LOOP
				RAISE NOTICE '% : %', 'row', r.g;
			END LOOP;
			RAISE WARNING 'careful' USING HINT = 'look here';
		END $$`); err != nil {
		t.Fatalf("do: %v", err)
	}
	var got []string
	for _, n := range s.Notices() {
		got = append(got, n.String())
	}
	want := []string{"NOTICE: row : 1", "NOTICE: row : 2", "NOTICE: row : 3", "WARNING: careful — HINT: look here"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("notices = %q, want %q", got, want)
	}

	// raised on the way to an exception: still reported
	_, err = s.Run(ctx, `DO $$ BEGIN RAISE NOTICE 'before'; RAISE EXCEPTION 'boom'; END $$`)
	if err == nil {
		t.Fatal("RAISE EXCEPTION: want an error")
	}
	if ns := s.Notices(); len(ns) != 1 || ns[0].Message != "before" {
		t.Errorf("notices before the exception = %v, want [before]", ns)
	}

	// RAISE EXCEPTION's DETAIL and HINT reach the error text
	_, err = s.Run(ctx, `DO $$ BEGIN RAISE EXCEPTION 'boom' USING DETAIL = 'the detail', HINT = 'the hint'; END $$`)
	if err == nil || !strings.Contains(err.Error(), "ERROR: boom (SQLSTATE P0001) — DETAIL: the detail — HINT: the hint") {
		t.Errorf("RAISE EXCEPTION … USING DETAIL, HINT: err = %v", err)
	}

	if _, err = s.Run(ctx, `SELECT 1`); err != nil {
		t.Fatalf("select: %v", err)
	}
	if ns := s.Notices(); ns != nil {
		t.Errorf("a quiet statement's notices = %v, want nil", ns)
	}

	key := s.noticeKey
	if key == nil {
		t.Fatal("a Postgres session registered no notice sink")
	}
	_ = s.Close()
	if _, ok := noticeSinks.Load(key); ok {
		t.Error("Close left the session's notice sink registered")
	}
}

// A Postgres error's DETAIL and HINT join its text, in the notice lines'
// style; the *PgError stays reachable, and an error without them, or not
// from Postgres, is returned as it was.
func TestWithPgDetail(t *testing.T) {
	plain := errors.New("not postgres")
	if got := withPgDetail(plain); got != plain {
		t.Errorf("non-Postgres error changed: %v", got)
	}
	bare := &pgconn.PgError{Severity: "ERROR", Message: "boom", Code: "P0001"}
	if got := withPgDetail(bare); got != error(bare) {
		t.Errorf("error without detail or hint changed: %v", got)
	}
	full := &pgconn.PgError{Severity: "ERROR", Message: "boom", Code: "22023", Detail: "d", Hint: "h"}
	got := withPgDetail(full)
	if want := "ERROR: boom (SQLSTATE 22023) — DETAIL: d — HINT: h"; got.Error() != want {
		t.Errorf("text = %q, want %q", got.Error(), want)
	}
	var pe *pgconn.PgError
	if !errors.As(got, &pe) || pe != full {
		t.Error("the *PgError is no longer reachable with errors.As")
	}
}

// CONTEXT condenses to one line: frames innermost first joined by "←", the
// DO block's own frame dropped, whitespace collapsed, a long frame cut and
// a deep chain ended with "…".
func TestPgContext(t *testing.T) {
	long := strings.Repeat("x", pgContextRunes+10)
	cases := []struct{ name, where, want string }{
		{"empty", "", ""},
		{"only the DO block", "PL/pgSQL function inline_code_block line 1 at RAISE", ""},
		{"a function called from a DO block",
			"PL/pgSQL function check_job(text,integer) line 7 at RAISE\n" +
				"SQL statement \"SELECT check_job('nightly', 5)\"\n" +
				"PL/pgSQL function inline_code_block line 1 at PERFORM",
			"PL/pgSQL function check_job(text,integer) line 7 at RAISE ← SQL statement \"SELECT check_job('nightly', 5)\""},
		{"a multi-line statement in a frame",
			"SQL statement \"SELECT a\n\t  FROM b\"",
			"SQL statement \"SELECT a FROM b\""},
		{"a multi-line statement with a quoted identifier, then its caller",
			"SQL statement \"SELECT \"Order\"\n  FROM t\"\nPL/pgSQL function f() line 2 at PERFORM",
			"SQL statement \"SELECT \"Order\" FROM t\" ← PL/pgSQL function f() line 2 at PERFORM"},
		{"a long frame", long, strings.Repeat("x", pgContextRunes) + "…"},
		{"a deep chain", "f1\nf2\nf3\nf4\nf5\nf6", "f1 ← f2 ← f3 ← f4 ← …"},
	}
	for _, c := range cases {
		if got := pgContext(c.where); got != c.want {
			t.Errorf("%s: pgContext = %q, want %q", c.name, got, c.want)
		}
	}
}

// A non-Postgres connection runs RunNotices on the pool, as RunContext
// does, and has no notices to return.
func TestRunNoticesOffPostgres(t *testing.T) {
	mgr := NewManager(&config.Config{MaxRows: 10, Connections: []config.Connection{
		{Name: "mem", Driver: "sqlite", DSN: "file:" + memName(t) + "?mode=memory&cache=shared"},
	}})
	defer mgr.Close()
	res, notices, err := mgr.RunNotices(context.Background(), "mem", "SELECT 7")
	if err != nil || len(res.Rows) != 1 || res.Rows[0][0] != "7" {
		t.Fatalf("RunNotices = %+v, %v", res, err)
	}
	if notices != nil {
		t.Errorf("notices = %v, want nil", notices)
	}
}

// A pooled run on Postgres returns its statement's notices, success or
// failure, and leaves no sink behind on the connection it gave back.
// RAISE EXCEPTION from a function carries the function in CONTEXT.
func TestLiveRunNotices(t *testing.T) {
	ctx := context.Background()
	mgr := liveMgr(t, "DBC_LIVE_PG_DSN", "postgres")
	res, notices, err := mgr.RunNotices(ctx, "live",
		`DO $$ BEGIN RAISE NOTICE 'one'; RAISE NOTICE 'two'; END $$`)
	if err != nil || res == nil {
		t.Fatalf("run: %v", err)
	}
	if len(notices) != 2 || notices[0].Message != "one" || notices[1].Message != "two" {
		t.Errorf("notices = %v, want [one two]", notices)
	}
	n := 0
	noticeSinks.Range(func(any, any) bool { n++; return true })
	if n != 0 {
		t.Errorf("%d notice sinks left registered after a pooled run", n)
	}

	liveExec(t, mgr, `CREATE OR REPLACE FUNCTION dbc_live_check(n int) RETURNS void AS $$
		BEGIN
			RAISE NOTICE 'checking %', n;
			RAISE EXCEPTION 'n (%) is too big', n;
		END $$ LANGUAGE plpgsql`)
	t.Cleanup(func() { _, _ = mgr.Run("live", `DROP FUNCTION IF EXISTS dbc_live_check(int)`) })
	_, notices, err = mgr.RunNotices(ctx, "live", `DO $$ BEGIN PERFORM dbc_live_check(5); END $$`)
	if err == nil {
		t.Fatal("want the function's exception")
	}
	if len(notices) != 1 || notices[0].Message != "checking 5" {
		t.Errorf("notices before the exception = %v", notices)
	}
	if want := "CONTEXT: PL/pgSQL function dbc_live_check(integer) line 4 at RAISE ← SQL statement \"SELECT dbc_live_check(5)\""; !strings.Contains(err.Error(), want) {
		t.Errorf("err = %v, want a CONTEXT naming the function", err)
	}
}
