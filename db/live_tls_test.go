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

// The encrypted-key live tests (N-075) connect to a server that admits a
// client by its certificate ALONE — no password — so a connect proves the
// key dbc decrypted was the one used. They need a PKI of their own: a CA
// the server trusts for clients, a client certificate it signed (CN
// "postgres" for Postgres), and the client key encrypted both ways with
// the passphrase "dbc-test-pass":
//
//	openssl req -x509 -new -newkey rsa:2048 -nodes -keyout ca.key -subj /CN=test-ca -days 2 -out ca.crt
//	openssl req -new -newkey rsa:2048 -nodes -keyout server.key -subj /CN=localhost -out server.csr
//	printf 'subjectAltName=DNS:localhost,IP:127.0.0.1\n' > san.ext
//	openssl x509 -req -in server.csr -CA ca.crt -CAkey ca.key -CAcreateserial -days 2 -extfile san.ext -out server.crt
//	openssl req -new -newkey rsa:2048 -nodes -keyout client.key -subj /CN=postgres -out client.csr
//	openssl x509 -req -in client.csr -CA ca.crt -CAkey ca.key -CAcreateserial -days 2 -out client.crt
//	openssl pkcs8 -topk8 -in client.key -v2 aes-256-cbc -v2prf hmacWithSHA256 -passout pass:dbc-test-pass -out client-pkcs8.key
//	openssl rsa -in client.key -aes256 -traditional -passout pass:dbc-test-pass -out client-legacy.key
//
// Postgres: copy the directory to /certs in the container before its first
// start, with an initdb hook that turns TLS on with server.* and ca.crt and
// writes pg_hba.conf as "local all all trust" + "hostssl all all all cert".
// MySQL: start mysqld with --ssl-ca/--ssl-cert/--ssl-key from the same
// files and CREATE USER certuser REQUIRE X509 (no password). Then:
//
//	DBC_LIVE_PG_CERT_DSN='postgres://postgres@127.0.0.1:55471/dbc' \
//	DBC_LIVE_MYSQL_CERT_DSN='certuser@tcp(127.0.0.1:53371)/dbc' \
//	DBC_LIVE_CERT_DIR=<the directory> go test ./db -run LiveTLSEncryptedKey -v

// liveEncryptedKey connects to env's DSN with the client key from
// DBC_LIVE_CERT_DIR in each encoding, and checks the refusals: no client
// certificate, and a wrong passphrase (which must fail before any dial).
func liveEncryptedKey(t *testing.T, env, driver, sslQ string) {
	t.Helper()
	dir := os.Getenv("DBC_LIVE_CERT_DIR")
	if dir == "" || os.Getenv(env) == "" {
		t.Skipf("set %s and DBC_LIVE_CERT_DIR to run against a server that wants a client certificate", env)
	}
	t.Setenv("DBC_LIVE_KEY_PASS", "dbc-test-pass")
	opts := func(key string) config.TLSOpts {
		return config.TLSOpts{TLS: "verify-full", TLSCA: dir + "/ca.crt", TLSCert: dir + "/client.crt",
			TLSKey: dir + "/" + key, TLSKeyPassword: "${DBC_LIVE_KEY_PASS}"}
	}
	for _, key := range []string{"client-pkcs8.key", "client-legacy.key"} {
		if got := liveOne(t, liveTLS(t, env, driver, opts(key)), sslQ); got == "" || got == "false" {
			t.Errorf("%s: the session is not encrypted (%q)", key, got)
		}
	}
	// the server turns away a client with no certificate...
	if _, err := liveTLS(t, env, driver, config.TLSOpts{TLS: "verify-full", TLSCA: dir + "/ca.crt"}).DB("live"); err == nil {
		t.Error("connected with no client certificate")
	}
	// ...and a wrong passphrase never gets as far as the server
	t.Setenv("DBC_LIVE_KEY_PASS", "not-it")
	liveRefused(t, env, driver, opts("client-pkcs8.key"), "wrong tls_key_password")
}

func TestLiveTLSEncryptedKeyPostgres(t *testing.T) {
	liveEncryptedKey(t, "DBC_LIVE_PG_CERT_DSN", "postgres",
		"SELECT ssl FROM pg_stat_ssl WHERE pid = pg_backend_pid()")
}

func TestLiveTLSEncryptedKeyMySQL(t *testing.T) {
	liveEncryptedKey(t, "DBC_LIVE_MYSQL_CERT_DSN", "mysql", "SHOW SESSION STATUS LIKE 'Ssl_cipher'")
}
