package db

import (
	"errors"
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5"

	"github.com/rohanthewiz/dbc/config"
)

// TestBuildDSNPostgres checks the built DSN by what pgx makes of it, not by
// its text alone: the point is that the server sees the fields as typed,
// whatever they hold.
func TestBuildDSNPostgres(t *testing.T) {
	// everything a URL would choke on, and the ' and \ that would end the
	// quotes the ${VAR} is expanded inside but for ExpandDSN's escaping
	t.Setenv("DBC_TEST_PW", `p@ss w/rd#1&x?= it's \ 100%`)
	dsn, err := BuildDSN("postgres", DSNParts{Host: " db.example.com ", Port: "6543", User: "app",
		Password: "${DBC_TEST_PW}", Database: "my db", Options: "application_name=dbc connect_timeout=7"})
	if err != nil {
		t.Fatal(err)
	}
	want := `host=db.example.com port=6543 user=app password='${DBC_TEST_PW}' dbname='my db' application_name=dbc connect_timeout=7`
	if dsn != want {
		t.Fatalf("dsn\n got %s\nwant %s", dsn, want)
	}
	// expanded as a connect expands it (config.ExpandDSN)
	cfg, err := pgx.ParseConfig(expand("postgres", dsn))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Host != "db.example.com" || cfg.Port != 6543 || cfg.User != "app" ||
		cfg.Password != `p@ss w/rd#1&x?= it's \ 100%` || cfg.Database != "my db" ||
		cfg.RuntimeParams["application_name"] != "dbc" {
		t.Fatalf("pgx read %+v", cfg.Config)
	}
	if cfg.ConnectTimeout.Seconds() != 7 {
		t.Fatalf("connect_timeout = %v", cfg.ConnectTimeout)
	}

	// a password typed in the field itself is escaped, quotes and all
	dsn, err = BuildDSN("postgres", DSNParts{Host: "h", Password: `it's a \ test`})
	if err != nil {
		t.Fatal(err)
	}
	if cfg, err = pgx.ParseConfig(dsn); err != nil || cfg.Password != `it's a \ test` {
		t.Fatalf("%s: password %q, %v", dsn, cfg.Password, err)
	}
}

func TestBuildDSNMySQL(t *testing.T) {
	t.Setenv("DBC_TEST_PW", "p@ss:w/rd")
	dsn, err := BuildDSN("mysql", DSNParts{Host: "db.example.com", Port: "3307", User: "app",
		Password: "${DBC_TEST_PW}", Database: "shop", Options: "parseTime=true loc=Local"})
	if err != nil {
		t.Fatal(err)
	}
	if want := "app:${DBC_TEST_PW}@tcp(db.example.com:3307)/shop?parseTime=true&loc=Local"; dsn != want {
		t.Fatalf("dsn\n got %s\nwant %s", dsn, want)
	}
	cfg, err := mysql.ParseDSN(expand("mysql", dsn))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.User != "app" || cfg.Passwd != "p@ss:w/rd" || cfg.Addr != "db.example.com:3307" ||
		cfg.DBName != "shop" || !cfg.ParseTime {
		t.Fatalf("driver read %+v", cfg)
	}

	// IPv6, no port: bracketed so the driver does not read the last group
	// as one
	dsn, err = BuildDSN("mysql", DSNParts{Host: "::1", User: "root"})
	if err != nil {
		t.Fatal(err)
	}
	if want := "root@tcp([::1])/"; dsn != want {
		t.Fatalf("got %s, want %s", dsn, want)
	}
}

func TestBuildDSNEmbedded(t *testing.T) {
	for _, tc := range []struct {
		drv  string
		p    DSNParts
		want string
	}{
		{"sqlite", DSNParts{File: "scratch.db"}, "scratch.db"},
		{"sqlite", DSNParts{File: "x", Options: "mode=memory cache=shared"}, "file:x?mode=memory&cache=shared"},
		{"sqlite", DSNParts{File: "file:x", Options: "mode=ro"}, "file:x?mode=ro"},
		{"bytdb", DSNParts{File: "~/notes.bytdb"}, "~/notes.bytdb"},
	} {
		got, err := BuildDSN(tc.drv, tc.p)
		if err != nil || got != tc.want {
			t.Errorf("%s %+v: got %q, %v; want %q", tc.drv, tc.p, got, err, tc.want)
		}
	}
}

// TestBuildDSNRefuses: each message is what the form shows, so it should
// name the field to fix.
func TestBuildDSNRefuses(t *testing.T) {
	for _, tc := range []struct {
		drv  string
		p    DSNParts
		want string
	}{
		{"postgres", DSNParts{}, "host"},
		{"postgres", DSNParts{Host: "h", Port: "54x32"}, "port"},
		{"postgres", DSNParts{Host: "h", Port: "70000"}, "port"},
		{"postgres", DSNParts{Host: "h", Options: "password=x"}, "its own field"},
		{"postgres", DSNParts{Host: "h", Options: "sslmode"}, "key=value"},
		{"postgres", DSNParts{Host: "h", Options: "options='-c x"}, "closing"},
		{"mysql", DSNParts{Host: "h", User: "a:b"}, "':'"},
		{"mysql", DSNParts{Host: "h", Password: "pw"}, "user name"},
		{"mysql", DSNParts{Host: "h", Database: "a/b"}, "'/'"},
		{"sqlite", DSNParts{}, "file"},
		{"bytdb", DSNParts{File: "f", Options: "a=b"}, "no options"},
		{"oracle", DSNParts{Host: "h"}, "unknown driver"},
	} {
		_, err := BuildDSN(tc.drv, tc.p)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s %+v: got %v, want an error mentioning %q", tc.drv, tc.p, err, tc.want)
		}
	}
}

// TestSplitDSNRoundTrip: what BuildDSN writes, SplitDSN reads back field
// for field — the edit form shows exactly what was saved.
func TestSplitDSNRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		drv string
		p   DSNParts
	}{
		{"postgres", DSNParts{Host: "h", Port: "5432", User: "u", Password: "${PGPASS}", Database: "d",
			Options: "application_name=dbc options='-c search_path=app'"}},
		{"postgres", DSNParts{Host: "h", User: "u", Password: `it's \ odd`, Database: "d"}},
		{"mysql", DSNParts{Host: "h", Port: "3306", User: "u", Password: "p@ss:w/rd", Database: "d",
			Options: "parseTime=true loc=Local"}},
		{"mysql", DSNParts{Host: "::1", Port: "3306", User: "u", Database: "d"}},
		{"sqlite", DSNParts{File: "x", Options: "mode=memory cache=shared"}},
		{"bytdb", DSNParts{File: "/tmp/a.bytdb"}},
	} {
		dsn, err := BuildDSN(tc.drv, tc.p)
		if err != nil {
			t.Fatalf("%+v: %v", tc.p, err)
		}
		got, err := SplitDSN(tc.drv, dsn)
		if err != nil {
			t.Fatalf("split %q: %v", dsn, err)
		}
		if got != tc.p {
			t.Errorf("round trip through %q\n got %+v\nwant %+v", dsn, got, tc.p)
		}
	}
}

// TestSplitDSNHandWritten: DSNs typed before the fields existed — URLs,
// ${VAR}s inside them, MySQL's short forms — come apart too.
func TestSplitDSNHandWritten(t *testing.T) {
	for _, tc := range []struct {
		drv, dsn string
		want     DSNParts
	}{
		{"postgres", "postgres://postgres:${PGPASS}@localhost:5432/mydb?sslmode=disable",
			DSNParts{Host: "localhost", Port: "5432", User: "postgres", Password: "${PGPASS}",
				Database: "mydb", Options: "sslmode=disable"}},
		{"pg", "postgresql://u:p%40ss@db/app",
			DSNParts{Host: "db", User: "u", Password: "p@ss", Database: "app"}},
		{"postgres", "host=/var/run/postgresql dbname=app",
			DSNParts{Host: "/var/run/postgresql", Database: "app"}},
		{"mysql", "user:${MYSQL_PASS}@tcp(localhost:3306)/mydb?parseTime=true",
			DSNParts{Host: "localhost", Port: "3306", User: "user", Password: "${MYSQL_PASS}",
				Database: "mydb", Options: "parseTime=true"}},
		{"mysql", "root@/dbc",
			DSNParts{Host: "127.0.0.1", Port: "3306", User: "root", Database: "dbc"}},
		{"sqlite", "file:dbcdemo?mode=memory&cache=shared",
			DSNParts{File: "dbcdemo", Options: "mode=memory cache=shared"}},
	} {
		got, err := SplitDSN(tc.drv, tc.dsn)
		if err != nil {
			t.Errorf("%q: %v", tc.dsn, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%q\n got %+v\nwant %+v", tc.dsn, got, tc.want)
		}
	}
}

func TestSplitDSNNotFields(t *testing.T) {
	for _, tc := range []struct{ drv, dsn string }{
		{"postgres", "postgres://u@h1:5432,h2:5432/app"},
		{"postgres", "host=h1,h2 dbname=app"},
		{"mysql", "u:p@unix(/tmp/mysql.sock)/app"},
		{"mysql", "no slash here"},
	} {
		if _, err := SplitDSN(tc.drv, tc.dsn); !errors.Is(err, ErrNotFields) {
			t.Errorf("%q: got %v, want ErrNotFields", tc.dsn, err)
		}
	}
}

func TestIsEnvRef(t *testing.T) {
	for s, want := range map[string]bool{
		"${PGPASS}": true, "$PGPASS": true, "": false, "secret": false,
		"x${PGPASS}": false, "${A}${B}": false,
	} {
		if got := IsEnvRef(s); got != want {
			t.Errorf("IsEnvRef(%q) = %v, want %v", s, got, want)
		}
	}
}

// expand is config.ExpandDSN, as a connect runs it, minus the warnings.
func expand(driver, s string) string {
	out, _ := config.ExpandDSN("test", driver, s)
	return out
}
