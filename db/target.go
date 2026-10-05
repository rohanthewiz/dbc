package db

import (
	"net"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5"

	"github.com/rohanthewiz/dbc/config"
)

// Target is where a connection actually lands: the server and the database
// on it. It is what a SQL console is kept per (see userdata.ConsoleFile),
// rather than the connection's name, because a name is only a label:
//
//	"prod" and "prod-ro"   ─► same host, same database ─► ONE console
//	"prod/analytics"       ─► same host, other database ─► its own console
//	renaming "prod"        ─► same target ─► the console stays
//
// Host carries the port ("db.example.com:5432"), since two servers on one
// machine are two different sets of databases. The user does not count:
// the same database seen as another role is the same schema to write
// queries against.
type Target struct {
	Host     string
	Database string
}

// targetLocal is the Host of an embedded engine's target (SQLite, bytdb):
// there is no server, the file is the database.
const targetLocal = "local"

// ConsoleTarget resolves the Target of cc from its DSN, the way the driver
// will: pgx's defaults and PG* environment for Postgres, go-sql-driver's
// for MySQL, the absolute file path for SQLite and bytdb (the drivers open a
// relative path against the working directory, so "scratch.db" in two
// projects is two databases).
//
// A DSN it cannot read still gets a Target — Host "", the connection's name
// as the Database — so the console falls back to being per connection
// rather than being lost, or shared with every other unreadable DSN.
func ConsoleTarget(cc config.Connection) Target {
	byName := Target{Database: cc.Name}
	drv, err := driverFor(cc.Driver)
	if err != nil {
		return byName
	}
	switch drv {
	case "pgx":
		pcfg, err := pgx.ParseConfig(cc.DSN)
		if err != nil {
			return byName
		}
		t := Target{Host: net.JoinHostPort(pcfg.Host, strconv.Itoa(int(pcfg.Port))), Database: pcfg.Database}
		if cc.Database != "" {
			t.Database = cc.Database // a derived "<base>/<database>" connection
		}
		return t
	case "mysql":
		mc, err := mysql.ParseDSN(cc.DSN)
		if err != nil {
			return byName
		}
		t := Target{Host: mc.Addr, Database: mc.DBName}
		if cc.Database != "" {
			t.Database = cc.Database
		}
		return t
	case "sqlite":
		path := sqlitePath(cc.DSN)
		if path == "" {
			// in-memory: nothing outlives the process, so there is no
			// database to share a console with — keep it per connection
			return Target{Host: "memory", Database: cc.Name}
		}
		return Target{Host: targetLocal, Database: absPath(path)}
	default: // bytdb: the DSN is the file
		return Target{Host: targetLocal, Database: absPath(cc.DSN)}
	}
}

// sqlitePath is the file a SQLite DSN opens, "" for an in-memory database.
// modernc accepts "path", "file:path" and either with "?options".
func sqlitePath(dsn string) string {
	path := strings.TrimPrefix(dsn, "file:")
	query := ""
	if i := strings.IndexByte(path, '?'); i >= 0 {
		path, query = path[:i], path[i+1:]
	}
	if path == "" || path == ":memory:" || strings.Contains(query, "mode=memory") {
		return ""
	}
	return path
}

// absPath makes path absolute where it can; a path it cannot resolve is
// kept as written, which still names one console consistently.
func absPath(path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		return abs
	}
	return path
}
