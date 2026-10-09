package db

import (
	"reflect"
	"strings"
	"testing"

	"github.com/rohanthewiz/dbc/config"
)

func TestLibpqForKeywordDSN(t *testing.T) {
	cc := config.Connection{Name: "pg", Driver: "postgres",
		DSN: `host=db.example.com port = 6543 user=app password='it\'s a secret ' dbname=app ` +
			`search_path=reporting pool_max_conns=4 application_name=dbc sslmode=disable`}
	lc, err := LibpqFor(cc)
	if err != nil {
		t.Fatal(err)
	}
	want := [][2]string{{"host", "db.example.com"}, {"port", "6543"}, {"user", "app"}, {"dbname", "app"},
		{"application_name", "dbc"}, {"sslmode", "disable"}}
	if !reflect.DeepEqual(lc.Settings, want) {
		t.Fatalf("settings\n got %q\nwant %q", lc.Settings, want)
	}
	// kept apart, trailing blank and all: a service file would trim it
	if lc.Password != "it's a secret " {
		t.Fatalf("password %q", lc.Password)
	}
	// a run-time parameter is reported; pgx's own pool key is not
	if !reflect.DeepEqual(lc.Dropped, []string{"search_path"}) {
		t.Fatalf("dropped %q", lc.Dropped)
	}
	sf := string(lc.ServiceFile("dbc"))
	if !strings.Contains(sf, "\n[dbc]\nhost=db.example.com\nport=6543\n") || strings.Contains(sf, "secret") {
		t.Fatalf("service file:\n%s", sf)
	}
}

func TestLibpqForURL(t *testing.T) {
	cc := config.Connection{Name: "pg", Driver: "pg",
		DSN: "postgres://app:p%40ss@word@h1:5432,[::1]:5433,h3/my%20db?sslmode=require&connect_timeout=5&sslmode=verify-ca"}
	lc, err := LibpqFor(cc)
	if err != nil {
		t.Fatal(err)
	}
	want := [][2]string{{"user", "app"}, {"host", "h1,::1,h3"}, {"port", "5432,5433,"}, {"dbname", "my db"},
		{"sslmode", "verify-ca"}, {"connect_timeout", "5"}}
	if !reflect.DeepEqual(lc.Settings, want) {
		t.Fatalf("settings\n got %q\nwant %q", lc.Settings, want)
	}
	// %40 decoded, and the credentials end at the last '@'
	if lc.Password != "p@ss@word" {
		t.Fatalf("password %q", lc.Password)
	}

	// a socket directory, and the JDBC ssl=true
	lc, err = LibpqFor(config.Connection{Driver: "postgres", DSN: "postgresql://%2Fvar%2Frun%2Fpostgresql/app?ssl=true"})
	if err != nil {
		t.Fatal(err)
	}
	want = [][2]string{{"host", "/var/run/postgresql"}, {"dbname", "app"}, {"sslmode", "require"}}
	if !reflect.DeepEqual(lc.Settings, want) {
		t.Fatalf("settings\n got %q\nwant %q", lc.Settings, want)
	}
}

// The tls keys and a derived connection's database override the DSN, as
// they do when dbc opens the pool.
func TestLibpqForTLSAndDerivedDatabase(t *testing.T) {
	t.Setenv("DBC_TEST_KEY_PASS", "hunter2")
	cc := config.Connection{Name: "pg/sales", Driver: "postgres", Database: "sales",
		DSN: "host=h dbname=app sslmode=disable sslrootcert=/old/ca.pem",
		TLSOpts: config.TLSOpts{TLS: "verify-ca", TLSCA: "/certs/ca.pem", TLSCert: "/certs/me.pem",
			TLSKey: "/certs/me.key", TLSKeyPassword: "${DBC_TEST_KEY_PASS}"}}
	lc, err := LibpqFor(cc)
	if err != nil {
		t.Fatal(err)
	}
	want := [][2]string{{"host", "h"}, {"dbname", "sales"}, {"sslmode", "verify-ca"}, {"sslrootcert", "/certs/ca.pem"},
		{"sslcert", "/certs/me.pem"}, {"sslkey", "/certs/me.key"}, {"sslpassword", "hunter2"}}
	if !reflect.DeepEqual(lc.Settings, want) {
		t.Fatalf("settings\n got %q\nwant %q", lc.Settings, want)
	}

	cc.TLSOpts.TLSKeyPassword = "${DBC_TEST_UNSET_VAR}"
	if _, err = LibpqFor(cc); err == nil || !strings.Contains(err.Error(), "DBC_TEST_UNSET_VAR is not set") {
		t.Fatalf("unset passphrase var: %v", err)
	}
}

// verify-full with no CA anywhere means the system trust store, as pgx
// reads it — libpq's spelling is sslrootcert=system.
func TestLibpqForVerifyFullUsesSystemRoots(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // no ~/.postgresql/root.crt
	t.Setenv("PGSSLROOTCERT", "")
	lc, err := LibpqFor(config.Connection{Driver: "postgres", DSN: "host=h sslmode=verify-full"})
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := lc.get("sslrootcert"); v != "system" {
		t.Fatalf("sslrootcert %q, settings %q", v, lc.Settings)
	}
	lc, _ = LibpqFor(config.Connection{Driver: "postgres", DSN: "host=h sslmode=verify-full sslrootcert=/ca.pem"})
	if v, _ := lc.get("sslrootcert"); v != "/ca.pem" {
		t.Fatalf("a named CA must stay: %q", v)
	}
}

func TestLibpqForRefuses(t *testing.T) {
	for _, tc := range []struct {
		cc   config.Connection
		want string
	}{
		{config.Connection{Driver: "mysql", DSN: "u@tcp(h)/db"}, "not a Postgres connection"},
		{config.Connection{Driver: "postgres", DSN: "service=prod"}, "names a service="},
		{config.Connection{Driver: "postgres", DSN: "host=h application_name='trailing '"}, "ends in a blank"},
		{config.Connection{Driver: "postgres", DSN: "host=h dbname='open"}, "missing its closing"},
		{config.Connection{Driver: "postgres", DSN: "host=h justakey"}, "not key=value"},
		{config.Connection{Driver: "postgres", DSN: "postgres://u:%zz@h/db"}, "bad %-escape in its password"},
	} {
		_, err := LibpqFor(tc.cc)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%q: got %v, want %q", tc.cc.DSN, err, tc.want)
		}
		if err != nil && strings.Contains(err.Error(), "%zz") {
			t.Errorf("the error quotes the DSN: %v", err)
		}
	}
}
