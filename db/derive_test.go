package db

import (
	"testing"

	"github.com/rohanthewiz/dbc/config"
)

// A MySQL connection derived onto another database (N-079) opens on that
// database, whatever the DSN named — or when it named none — with
// everything else of the DSN kept: the address, the credentials, the
// params, and the TLS config when the connection has TLS settings.
func TestMySQLConfigDerived(t *testing.T) {
	const dsn = "app:pw@tcp(db.internal:3306)/shop?parseTime=true&tls=skip-verify"
	for _, c := range []struct {
		dsn, database, want string
	}{
		{dsn, "", "shop"},           // the configured connection: the DSN's own
		{dsn, "reports", "reports"}, // derived: replaced
		{"app:pw@tcp(db.internal:3306)/", "reports", "reports"}, // a DSN with no database
		{"app:pw@tcp(db.internal:3306)/", "", ""},
	} {
		mc, err := mysqlConfig(c.dsn, config.TLSOpts{}, c.database)
		if err != nil {
			t.Fatalf("%q on %q: %v", c.dsn, c.database, err)
		}
		if mc.DBName != c.want || mc.Addr != "db.internal:3306" || mc.User != "app" || mc.Passwd != "pw" {
			t.Errorf("%q on %q: DBName %q, Addr %q, User %q", c.dsn, c.database, mc.DBName, mc.Addr, mc.User)
		}
		// with no TLS settings the DSN's own tls= param still governs
		if c.dsn == dsn && (mc.TLSConfig != "skip-verify" || mc.TLS == nil) {
			t.Errorf("the DSN's tls param was lost: %q %v", mc.TLSConfig, mc.TLS)
		}
	}

	// the TLS path: one connector carries both the built TLS config and
	// the derived database
	pki := newTestPKI(t)
	opts := config.TLSOpts{TLS: "verify-full", TLSCA: pki.caPEM}
	mc, err := mysqlConfig(dsn, opts, "reports")
	if err != nil {
		t.Fatal(err)
	}
	if mc.DBName != "reports" || mc.TLS == nil || mc.TLS.ServerName != "db.internal" || mc.TLSConfig != "false" {
		t.Errorf("TLS + derived: DBName %q, TLS %v, TLSConfig %q", mc.DBName, mc.TLS, mc.TLSConfig)
	}
	if _, err := mysqlConnector(dsn, opts, "reports"); err != nil {
		t.Errorf("mysqlConnector: %v", err)
	}
	if _, err := mysqlConfig("not a dsn", config.TLSOpts{}, "reports"); err == nil {
		t.Error("a bad DSN was accepted")
	}
}

// openPool takes a derived MySQL connection through its own connector —
// sql.OpenDB, which dials nothing until used — and still refuses a DSN it
// cannot parse rather than failing at the first ping.
func TestOpenPoolMySQLDerived(t *testing.T) {
	dbh, err := openPool("mysql", "app:pw@tcp(127.0.0.1:1)/shop", config.TLSOpts{}, "reports")
	if err != nil {
		t.Fatal(err)
	}
	_ = dbh.Close()
	if _, err := openPool("mysql", "not a dsn", config.TLSOpts{}, "reports"); err == nil {
		t.Error("a bad DSN was accepted")
	}
}

// The whole path from a config file's DSN with ${VAR} references to the
// pool's settings: ExpandDSN escapes the values for where they land, the
// derived connection keeps that expanded DSN, and the pool opens on the
// derived database with the password intact — however many of the DSN's
// own delimiters it holds.
func TestMySQLDerivedFromExpandedDSN(t *testing.T) {
	t.Setenv("DBC_N079_PW", "p@ss:/w?rd")
	t.Setenv("DBC_N079_DB", "shop")
	dsn, warns := config.ExpandDSN("my", "mysql", "app:${DBC_N079_PW}@tcp(h:3306)/${DBC_N079_DB}?parseTime=true")
	if len(warns) != 0 {
		t.Fatalf("warnings: %v", warns)
	}
	cfg := &config.Config{Connections: []config.Connection{{Name: "my", Driver: "mysql", DSN: dsn}}}
	base, ok := cfg.ConnByName("my")
	if !ok || DefaultDatabase(base) != "shop" {
		t.Fatalf("base %+v: default database %q", base, DefaultDatabase(base))
	}
	cc, ok := cfg.ConnByName("my/reports")
	if !ok || cc.Base != "my" || cc.Database != "reports" {
		t.Fatalf("my/reports = %+v, %v", cc, ok)
	}
	mc, err := mysqlConfig(cc.DSN, cc.TLSOpts, cc.Database)
	if err != nil {
		t.Fatal(err)
	}
	if mc.Passwd != "p@ss:/w?rd" || mc.DBName != "reports" || !mc.ParseTime {
		t.Errorf("Passwd %q, DBName %q, ParseTime %v", mc.Passwd, mc.DBName, mc.ParseTime)
	}
	if DefaultDatabase(cc) != "reports" {
		t.Errorf("the derived connection's database is %q", DefaultDatabase(cc))
	}
}
