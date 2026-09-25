package db

import (
	"context"
	"errors"
	"os"
	"strings"
	"time"

	"github.com/rohanthewiz/bytdb"
	"github.com/rohanthewiz/serr"

	"github.com/rohanthewiz/dbc/config"
)

// ProbeResult is what a connection test found. OK with a Note is a test that
// did not dial at all but has something to say (see Probe).
type ProbeResult struct {
	OK   bool
	Took time.Duration // open + ping; zero when nothing was dialed
	Note string        // words for the user when OK is true but qualified
}

// Probe tests a connection that is not (yet) in the config: it opens a pool
// with the same driver setup the Manager uses (driverFor, sqliteDSN,
// openPool), pings it under timeout, and closes it again. Nothing is cached
// — the point is to learn whether the DSN works before it is saved, without
// leaving a pool behind for a connection that may never be added.
//
// The embedded engines open a file, and opening a file that does not exist
// creates it. A test must not leave an empty database behind at a mistyped
// path, so for those a missing file is reported without opening anything:
//
//	sqlite in-memory (mode=memory) ─► open + ping (nothing to create)
//	sqlite / bytdb, file missing ───► OK, Note: "will be created on first connect"
//	sqlite / bytdb, file present ───► open + ping
//	postgres / mysql ───────────────► open + ping
//
// A bytdb file held by another process — or by this one, when a configured
// connection already has it open — fails with ErrInUse, as a real connect
// would. The error never carries the DSN: callers show it to the user.
func Probe(ctx context.Context, cc config.Connection, timeout time.Duration) (ProbeResult, error) {
	drv, err := driverFor(cc.Driver)
	if err != nil {
		return ProbeResult{}, err
	}
	if path := embeddedPath(drv, cc.DSN); path != "" {
		if _, statErr := os.Stat(path); errors.Is(statErr, os.ErrNotExist) {
			return ProbeResult{OK: true, Note: "the file does not exist yet — it will be created on first connect"}, nil
		}
	}
	dsn := cc.DSN
	if drv == "sqlite" {
		dsn = sqliteDSN(dsn)
	}

	start := time.Now()
	dbh, err := openPool(drv, dsn)
	if err != nil {
		if errors.Is(err, bytdb.ErrLocked) {
			return ProbeResult{}, inUse(cc.Name, err)
		}
		// no "dsn" field: the error goes to the browser (see web/respond.go)
		return ProbeResult{}, serr.Wrap(err, "conn", cc.Name, "driver", drv)
	}
	defer func() { _ = dbh.Close() }()

	pctx, cancel := ctx, context.CancelFunc(func() {})
	if timeout > 0 {
		pctx, cancel = context.WithTimeout(ctx, timeout)
	}
	defer cancel()
	if err = dbh.PingContext(pctx); err != nil {
		if errors.Is(pctx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
			// dbc's own cap fired, not the caller's: say so in plain words,
			// since the drivers' own text for it ("context deadline
			// exceeded", "i/o timeout") does not name the host being down
			return ProbeResult{}, serr.New("no answer within "+timeout.String()+
				" — is the host reachable and the port right?", "conn", cc.Name)
		}
		return ProbeResult{}, wrapRunErr(ctx, err, cc.Name, "op", "ping")
	}
	return ProbeResult{OK: true, Took: time.Since(start)}, nil
}

// embeddedPath is the file an embedded engine's DSN names, or "" when it
// names none (a server engine, or an in-memory SQLite database). SQLite's
// DSN is a path or a file: URI, either with ?options; bytdb's is a path.
func embeddedPath(drv, dsn string) string {
	switch drv {
	case "sqlite":
		if strings.Contains(dsn, "mode=memory") || dsn == ":memory:" {
			return ""
		}
		dsn = strings.TrimPrefix(dsn, "file:")
	case "bytdb":
	default:
		return ""
	}
	if i := strings.IndexByte(dsn, '?'); i >= 0 {
		dsn = dsn[:i]
	}
	return dsn
}
