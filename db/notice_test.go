package db

import (
	"context"
	"strings"
	"testing"

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
// empty. Closing the session unregisters its sink.
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
