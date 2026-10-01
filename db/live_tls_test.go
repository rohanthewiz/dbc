package db

import (
	"os"
	"strings"
	"testing"

	"github.com/rohanthewiz/dbc/config"
)

// The live TLS tests connect to real servers with each TLS mode and ask the
// server whether the session is encrypted — the one witness that cannot be
// fooled by a client-side config that looks right. Opt-in like the other
// live tests (see live_test.go):
//
//	# Postgres with TLS on: the image's snakeoil certificate will do
//	docker run -d --rm --name dbc-live-pgtls -e POSTGRES_PASSWORD=pw -e POSTGRES_DB=dbc \
//	  -p 55433:5432 postgres:17 -c ssl=on \
//	  -c ssl_cert_file=/etc/ssl/certs/ssl-cert-snakeoil.pem \
//	  -c ssl_key_file=/etc/ssl/private/ssl-cert-snakeoil.key
//	# MySQL 8.x has TLS on out of the box, with a CA of its own
//	docker cp dbc-live-my:/var/lib/mysql/ca.pem /tmp/dbc-my-ca.pem
//
//	DBC_LIVE_PG_TLS_DSN='postgres://postgres:pw@127.0.0.1:55433/dbc?sslmode=disable' \
//	DBC_LIVE_MYSQL_DSN='root:pw@tcp(127.0.0.1:53306)/dbc' \
//	DBC_LIVE_MYSQL_CA=/tmp/dbc-my-ca.pem \
//	go test ./db -run LiveTLS -v
//
// The Postgres DSN says sslmode=disable on purpose: the tls key must win
// over it. DBC_LIVE_MYSQL_CA (and DBC_LIVE_PG_CA, for a server with a CA of
// your own) are optional; without one the verify-* cases that need it skip.

// liveTLS is a Manager on env's DSN with opts as the connection's TLS.
func liveTLS(t *testing.T, env, driver string, opts config.TLSOpts) *Manager {
	t.Helper()
	return liveMgr(t, env, driver, func(c *config.Config) { c.Connections[0].TLSOpts = opts })
}

// liveOne runs q and returns the first column of its first row.
func liveOne(t *testing.T, mgr *Manager, q string) string {
	t.Helper()
	res, err := mgr.Run("live", q)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	if len(res.Rows) == 0 {
		return ""
	}
	return res.Rows[0][len(res.Rows[0])-1]
}

// liveRefused opens with opts and wants the connect refused, with a word of
// why in the error.
func liveRefused(t *testing.T, env, driver string, opts config.TLSOpts, why string) {
	t.Helper()
	_, err := liveTLS(t, env, driver, opts).DB("live")
	if err == nil {
		t.Fatalf("%+v connected; want it refused", opts)
	}
	if !strings.Contains(err.Error(), why) {
		t.Fatalf("%+v refused with %v; want it to mention %q", opts, err, why)
	}
}

func TestLiveTLSPostgres(t *testing.T) {
	const env = "DBC_LIVE_PG_TLS_DSN"
	const sslQ = "SELECT ssl FROM pg_stat_ssl WHERE pid = pg_backend_pid()"

	// require beats the DSN's sslmode=disable, and the server agrees
	if got := liveOne(t, liveTLS(t, env, "postgres", config.TLSOpts{TLS: "require"}), sslQ); got != "true" {
		t.Fatalf("tls=require: pg_stat_ssl.ssl = %q", got)
	}
	if got := liveOne(t, liveTLS(t, env, "postgres", config.TLSOpts{TLS: "disable"}), sslQ); got != "false" {
		t.Fatalf("tls=disable: pg_stat_ssl.ssl = %q", got)
	}
	// prefer takes TLS when the server offers it
	if got := liveOne(t, liveTLS(t, env, "postgres", config.TLSOpts{TLS: "prefer"}), sslQ); got != "true" {
		t.Fatalf("tls=prefer: pg_stat_ssl.ssl = %q", got)
	}
	// a self-signed certificate is not trusted by the system
	liveRefused(t, env, "postgres", config.TLSOpts{TLS: "verify-full"}, "certificate")

	if ca := os.Getenv("DBC_LIVE_PG_CA"); ca != "" {
		mgr := liveTLS(t, env, "postgres", config.TLSOpts{TLS: "verify-ca", TLSCA: ca})
		if got := liveOne(t, mgr, sslQ); got != "true" {
			t.Fatalf("tls=verify-ca: pg_stat_ssl.ssl = %q", got)
		}
	}
}

func TestLiveTLSMySQL(t *testing.T) {
	const env = "DBC_LIVE_MYSQL_DSN"
	const sslQ = "SHOW SESSION STATUS LIKE 'Ssl_cipher'"

	if got := liveOne(t, liveTLS(t, env, "mysql", config.TLSOpts{TLS: "require"}), sslQ); got == "" {
		t.Fatal("tls=require: the session has no cipher")
	}
	if got := liveOne(t, liveTLS(t, env, "mysql", config.TLSOpts{TLS: "prefer"}), sslQ); got == "" {
		t.Fatal("tls=prefer: the session has no cipher, though the server offers TLS")
	}
	// MySQL's auto-generated CA is no public one
	liveRefused(t, env, "mysql", config.TLSOpts{TLS: "verify-full"}, "certificate")

	ca := os.Getenv("DBC_LIVE_MYSQL_CA")
	if ca == "" {
		t.Log("DBC_LIVE_MYSQL_CA unset: skipping the verify-ca/verify-full cases against the server's own CA")
		return
	}
	mgr := liveTLS(t, env, "mysql", config.TLSOpts{TLS: "verify-ca", TLSCA: ca})
	if got := liveOne(t, mgr, sslQ); got == "" {
		t.Fatal("tls=verify-ca: the session has no cipher")
	}
	// the right CA, but its server certificate names
	// "MySQL_Server_..._Auto_Generated_Server_Certificate", not the host
	// dialed: verify-full must refuse what verify-ca accepted
	liveRefused(t, env, "mysql", config.TLSOpts{TLS: "verify-full", TLSCA: ca}, "certificate")
}
