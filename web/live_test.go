package web

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/rohanthewiz/dbc/config"
)

// TestLiveDerivedPoolClosedWhenLeft is N-082 against a real Postgres
// (opt-in, DBC_LIVE_PG_DSN, as db's live tests): two tabs on another of
// the server's databases ("live/<db>"). The first to leave keeps its pool
// open for the other; the last to leave closes it, and says so. The close
// is the only thing that logs "closed the connection", and only when a pool
// was open, so the second tab's line also proves the first left it alone.
func TestLiveDerivedPoolClosedWhenLeft(t *testing.T) {
	dsn := os.Getenv("DBC_LIVE_PG_DSN")
	if dsn == "" {
		t.Skip("set DBC_LIVE_PG_DSN to run against a live Postgres")
	}
	const other = "dbc_web_left_other"
	e := newTestEnv(t, func(cfg *config.Config, _ *Options) {
		cfg.Connections = append(cfg.Connections, config.Connection{Name: "live", Driver: "postgres", DSN: dsn})
	})
	mgr := e.srv.mgr
	drop := `DROP DATABASE IF EXISTS ` + other + ` WITH (FORCE)`
	for _, s := range []string{drop, `CREATE DATABASE ` + other} {
		if _, err := mgr.RunContext(context.Background(), "live", s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	t.Cleanup(func() {
		mgr.Drop("live")
		_, _ = mgr.Run("live", drop)
	})
	derived := config.DerivedName("live", other)

	connect := func(id string, s *stream, name string) {
		t.Helper()
		e.api("POST", "/api/v1/ws/"+id+"/connect", fmt.Sprintf(`{"name":%q}`, name), 200)
		ev, logs := s.await(t, "conn")
		if c := decodeData[connEvent](t, testEnvelope{Data: ev.Data}); c.Active != name || c.Failed {
			t.Fatalf("connect %s = %+v (%q)", name, c, logs)
		}
	}
	a, sa := e.open()
	b, sb := e.open()
	connect(a, sa, derived)
	connect(b, sb, derived)

	connect(a, sa, "demo-sqlite") // b is still on it: kept
	connect(b, sb, "demo-sqlite") // the last tab off it: closed
	sb.awaitLog(t, "closed the connection to "+derived+": no tab is on it")
	if mgr.Disconnect(derived) {
		t.Errorf("%s's pool was still open", derived)
	}
}
