package db

import (
	"crypto/tls"
	"crypto/x509"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/rohanthewiz/dbc/config"
)

// The fixtures in testdata/tlskey were made once with the openssl CLI (the
// commands are in its README). Every encrypted one's passphrase is
// fixturePass.
const fixturePass = "dbc-test-pass"

func fixture(name string) string { return filepath.Join("testdata", "tlskey", name) }

// TestDecryptKeyPEM: both encodings of both key types decrypt to the key
// the plain fixture holds, and the ways it can go wrong each say why.
func TestDecryptKeyPEM(t *testing.T) {
	for _, tc := range []struct{ enc, plain string }{
		{"rsa-legacy.key", "rsa.key"},
		{"rsa-pkcs8.key", "rsa.key"},
		{"ec-legacy.key", "ec.key"},
		{"ec-pkcs8.key", "ec.key"},
		{"ec-pkcs8-aes128-sha1.key", "ec.key"}, // the default PRF, left out of the file
	} {
		enc, err := os.ReadFile(fixture(tc.enc))
		if err != nil {
			t.Fatal(err)
		}
		out, err := decryptKeyPEM(enc, []byte(fixturePass))
		if err != nil {
			t.Errorf("%s: %v", tc.enc, err)
			continue
		}
		// the decrypted key and the plain one are the same key
		crt := map[string]string{"rsa.key": "rsa.crt", "ec.key": "ec.crt"}[tc.plain]
		certPEM, _ := os.ReadFile(fixture(crt))
		if _, err = tls.X509KeyPair(certPEM, out); err != nil {
			t.Errorf("%s: decrypted key does not match %s: %v", tc.enc, crt, err)
		}

		if _, err = decryptKeyPEM(enc, []byte("not-it")); err == nil ||
			!strings.Contains(err.Error(), "wrong tls_key_password") {
			t.Errorf("%s with a wrong passphrase: %v", tc.enc, err)
		}
	}

	for _, tc := range []struct{ file, want string }{
		{"ec-scrypt.key", "scrypt"},
		{"ec-des3.key", "only AES-CBC"},
		{"ec.key", "not encrypted"},
		{"ec.crt", "no PEM private key"},
	} {
		in, err := os.ReadFile(fixture(tc.file))
		if err != nil {
			t.Fatal(err)
		}
		_, err = decryptKeyPEM(in, []byte(fixturePass))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want an error mentioning %q", tc.file, err, tc.want)
		}
		if tc.want == "scrypt" && !strings.Contains(err.Error(), "openssl pkcs8") {
			t.Errorf("%s: the refusal does not say how to re-encrypt: %v", tc.file, err)
		}
	}
}

// TestClientCert: the passphrase comes from the variable tls_key_password
// names, read at the call; an unset one is an error naming it, never an
// empty passphrase tried. Errors never carry the passphrase.
func TestClientCert(t *testing.T) {
	opts := config.TLSOpts{TLS: "require", TLSCert: fixture("ec.crt"), TLSKey: fixture("ec-pkcs8.key"),
		TLSKeyPassword: "${DBC_TEST_TLS_KEY_PASS}"}

	if _, err := clientCert(opts); err == nil || !strings.Contains(err.Error(), "$DBC_TEST_TLS_KEY_PASS is not set") {
		t.Errorf("unset variable: %v", err)
	}
	t.Setenv("DBC_TEST_TLS_KEY_PASS", fixturePass)
	pair, err := clientCert(opts)
	if err != nil || pair.PrivateKey == nil || len(pair.Certificate) != 1 {
		t.Fatalf("clientCert: %+v, %v", pair, err)
	}
	t.Setenv("DBC_TEST_TLS_KEY_PASS", "wrong-one")
	if _, err = clientCert(opts); err == nil || !strings.Contains(err.Error(), "wrong tls_key_password") ||
		strings.Contains(err.Error(), "wrong-one") {
		t.Errorf("wrong passphrase: %v", err)
	}
	// a literal is refused here too, for a caller that skipped Check —
	// and is not echoed
	lit := opts
	lit.TLSKeyPassword = fixturePass
	if _, err = clientCert(lit); err == nil || strings.Contains(err.Error(), fixturePass) {
		t.Errorf("literal: %v", err)
	}
}

// TestMySQLTLSEncryptedKey: mysqlTLS loads the decrypted pair, and a server
// that demands a client certificate signed by its CA accepts it.
func TestMySQLTLSEncryptedKey(t *testing.T) {
	t.Setenv("DBC_TEST_TLS_KEY_PASS", fixturePass)
	certPEM, err := os.ReadFile(fixture("rsa.crt"))
	if err != nil {
		t.Fatal(err)
	}
	// the client certificate is self-signed, so it is its own CA
	clientCAs := x509.NewCertPool()
	clientCAs.AppendCertsFromPEM(certPEM)
	pki := newTestPKI(t)
	addr := serveTLS(t, &tls.Config{Certificates: []tls.Certificate{pki.server},
		ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: clientCAs})

	for _, key := range []string{"rsa-legacy.key", "rsa-pkcs8.key"} {
		tc, err := mysqlTLS(config.TLSOpts{TLS: "verify-full", TLSCA: pki.caPEM, TLSCert: fixture("rsa.crt"),
			TLSKey: fixture(key), TLSKeyPassword: "$DBC_TEST_TLS_KEY_PASS"}, "db.internal")
		if err != nil {
			t.Fatalf("%s: %v", key, err)
		}
		if err = handshake(addr, tc); err != nil {
			t.Errorf("%s: handshake with the decrypted key: %v", key, err)
		}
	}
}

// TestPgEncryptedKey: with tls_key_password the pair stays out of the DSN
// (pgx could not decrypt a PKCS#8 key, and a DSN's own sslcert/sslkey must
// not sneak back in), and pgClientCert puts the decrypted certificate on
// the parsed config — on every TLS attempt, and none of the plaintext ones.
func TestPgEncryptedKey(t *testing.T) {
	t.Setenv("DBC_TEST_TLS_KEY_PASS", fixturePass)
	opts := config.TLSOpts{TLS: "verify-full", TLSCert: fixture("ec.crt"), TLSKey: fixture("ec-pkcs8.key"),
		TLSKeyPassword: "${DBC_TEST_TLS_KEY_PASS}"}
	for _, dsn := range []string{
		"postgres://u@db.internal/app?sslcert=/elsewhere.pem&sslkey=/elsewhere.key",
		"host=db.internal,db2.internal user=u dbname=app sslcert=/elsewhere.pem sslkey=/elsewhere.key",
	} {
		out, err := pgTLSDSN(dsn, opts)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out, "ec-pkcs8.key") || strings.Contains(out, "DBC_TEST") {
			t.Fatalf("the key went into the DSN: %s", out)
		}
		// ParseConfig would fail on /elsewhere.* had they survived
		cfg, err := pgx.ParseConfig(out)
		if err != nil {
			t.Fatalf("%s: %v", out, err)
		}
		if len(cfg.TLSConfig.Certificates) != 0 {
			t.Fatalf("%s: pgx loaded a certificate itself", out)
		}
		if err = pgClientCert(cfg, opts); err != nil {
			t.Fatal(err)
		}
		if len(cfg.TLSConfig.Certificates) != 1 {
			t.Errorf("%s: no client certificate on the primary attempt", out)
		}
		for i, fb := range cfg.Fallbacks {
			if fb.TLSConfig != nil && len(fb.TLSConfig.Certificates) != 1 {
				t.Errorf("%s: fallback %d has TLS but no client certificate", out, i)
			}
		}
	}

	// prefer: the plaintext retry stays plaintext
	cfg, err := pgx.ParseConfig(must(pgTLSDSN("host=h", config.TLSOpts{TLS: "prefer",
		TLSCert: opts.TLSCert, TLSKey: opts.TLSKey, TLSKeyPassword: opts.TLSKeyPassword})))
	if err != nil {
		t.Fatal(err)
	}
	if err = pgClientCert(cfg, opts); err != nil {
		t.Fatal(err)
	}
	if len(cfg.TLSConfig.Certificates) != 1 || len(cfg.Fallbacks) != 1 || cfg.Fallbacks[0].TLSConfig != nil {
		t.Errorf("prefer: %+v", cfg)
	}

	// and through openPool, a bad passphrase fails the open, not the first query
	t.Setenv("DBC_TEST_TLS_KEY_PASS", "wrong-one")
	if _, err = openPool("pgx", "host=db.internal", opts, ""); err == nil ||
		!strings.Contains(err.Error(), "wrong tls_key_password") {
		t.Errorf("openPool with a wrong passphrase: %v", err)
	}

	// without a password nothing changes: the pair goes in the DSN, as before
	plain := config.TLSOpts{TLS: "verify-full", TLSCert: fixture("ec.crt"), TLSKey: fixture("ec.key")}
	out, err := pgTLSDSN("host=h", plain)
	if err != nil || !strings.Contains(out, "sslkey='"+fixture("ec.key")+"'") {
		t.Errorf("no password: %s, %v", out, err)
	}
}
