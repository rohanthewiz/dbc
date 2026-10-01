package db

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/rohanthewiz/dbc/config"
)

// The TLS tests run real handshakes: a TLS listener on loopback, with a
// certificate from a CA made for the test, and the client side built by
// the code under test. That checks what each mode actually accepts and
// refuses, which inspecting the *tls.Config fields alone would not.
//
//	testPKI ─► ca.pem, server cert (db.internal), client cert, other-ca.pem
//	serveTLS(server config) ─► addr
//	handshake(addr, client config) ─► nil, or why it failed

// testPKI is a throwaway PKI written to dir: a CA, a server certificate it
// signed for the name db.internal, a client certificate it signed, and a
// second, unrelated CA to be the wrong one.
type testPKI struct {
	caPEM, otherCAPEM     string // paths
	clientCert, clientKey string // paths
	server                tls.Certificate
	pool                  *x509.CertPool // trusts ca
}

func newTestPKI(t *testing.T) testPKI {
	t.Helper()
	dir := t.TempDir()
	ca, caKey := selfSigned(t, "dbc test CA")
	other, _ := selfSigned(t, "some other CA")
	srv, srvKey := signed(t, ca, caKey, "db.internal", x509.ExtKeyUsageServerAuth)
	cli, cliKey := signed(t, ca, caKey, "dbc-client", x509.ExtKeyUsageClientAuth)

	p := testPKI{
		caPEM:      writePEM(t, dir, "ca.pem", "CERTIFICATE", ca.Raw),
		otherCAPEM: writePEM(t, dir, "other.pem", "CERTIFICATE", other.Raw),
		clientCert: writePEM(t, dir, "client.pem", "CERTIFICATE", cli.Raw),
		clientKey:  writePEM(t, dir, "client.key", "EC PRIVATE KEY", ecDER(t, cliKey)),
		server:     tls.Certificate{Certificate: [][]byte{srv.Raw}, PrivateKey: srvKey},
		pool:       x509.NewCertPool(),
	}
	p.pool.AddCert(ca)
	return p
}

func selfSigned(t *testing.T, cn string) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	c, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return c, key
}

func signed(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey, name string,
	use x509.ExtKeyUsage) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: name},
		DNSNames: []string{name}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{use}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	c, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return c, key
}

func ecDER(t *testing.T, k *ecdsa.PrivateKey) []byte {
	t.Helper()
	der, err := x509.MarshalECPrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	return der
}

func writePEM(t *testing.T, dir, name, typ string, der []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// serveTLS accepts TLS connections on loopback with cfg until the test
// ends. Each one that completes its handshake is sent "ok": under TLS 1.3 a
// server that rejects the client's certificate does so after the client's
// half of the handshake is done, so the client only learns of it on its
// first read — handshake reads that byte.
func serveTLS(t *testing.T, cfg *tls.Config) string {
	t.Helper()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = c.Close() }()
				_ = c.SetDeadline(time.Now().Add(5 * time.Second))
				if c.(*tls.Conn).Handshake() == nil {
					_, _ = c.Write([]byte("ok"))
				}
			}()
		}
	}()
	return ln.Addr().String()
}

// handshake dials addr with cfg and reports whether the server's "ok" came
// back — i.e. whether both sides accepted the handshake.
func handshake(addr string, cfg *tls.Config) error {
	c, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", addr, cfg)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 2)
	if _, err = c.Read(buf); err != nil {
		return err
	}
	return nil
}

// TestMySQLTLSModes is the table of what each mode accepts, against a
// server whose certificate is for db.internal and signed by the test CA.
// host is the name the client dials by, which verify-full checks.
func TestMySQLTLSModes(t *testing.T) {
	pki := newTestPKI(t)
	addr := serveTLS(t, &tls.Config{Certificates: []tls.Certificate{pki.server}})

	for _, tc := range []struct {
		name string
		opts config.TLSOpts
		host string
		ok   bool
		why  string // a word of the refusal, when !ok
	}{
		{"verify-full, right CA, right name", config.TLSOpts{TLS: "verify-full", TLSCA: pki.caPEM}, "db.internal", true, ""},
		{"verify-full, right CA, wrong name", config.TLSOpts{TLS: "verify-full", TLSCA: pki.caPEM}, "127.0.0.1", false, "127.0.0.1"},
		{"verify-full, wrong CA", config.TLSOpts{TLS: "verify-full", TLSCA: pki.otherCAPEM}, "db.internal", false, "unknown authority"},
		{"verify-full, system roots", config.TLSOpts{TLS: "verify-full"}, "db.internal", false, "certificate"},
		{"verify-ca, wrong name is fine", config.TLSOpts{TLS: "verify-ca", TLSCA: pki.caPEM}, "127.0.0.1", true, ""},
		{"verify-ca, wrong CA", config.TLSOpts{TLS: "verify-ca", TLSCA: pki.otherCAPEM}, "db.internal", false, "unknown authority"},
		{"require checks nothing", config.TLSOpts{TLS: "require"}, "127.0.0.1", true, ""},
		{"require with a CA checks the chain", config.TLSOpts{TLS: "require", TLSCA: pki.otherCAPEM}, "db.internal", false, "unknown authority"},
		{"prefer checks nothing", config.TLSOpts{TLS: "prefer", TLSCA: pki.otherCAPEM}, "x", true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := mysqlTLS(tc.opts, tc.host)
			if err != nil {
				t.Fatal(err)
			}
			err = handshake(addr, cfg)
			switch {
			case tc.ok && err != nil:
				t.Fatalf("refused: %v", err)
			case !tc.ok && err == nil:
				t.Fatal("accepted, want refused")
			case !tc.ok && !strings.Contains(err.Error(), tc.why):
				t.Fatalf("refused with %v, want it to mention %q", err, tc.why)
			}
		})
	}

	if cfg, err := mysqlTLS(config.TLSOpts{TLS: "disable"}, "h"); cfg != nil || err != nil {
		t.Errorf("disable: got %v, %v; want no TLS config", cfg, err)
	}
	if _, err := mysqlTLS(config.TLSOpts{TLS: "verify_full"}, "h"); err == nil {
		t.Error("an unknown mode was accepted")
	}
}

// TestMySQLTLSClientCert: a server that demands a client certificate gets
// the one tls_cert and tls_key name, and turns the client away without it.
func TestMySQLTLSClientCert(t *testing.T) {
	pki := newTestPKI(t)
	addr := serveTLS(t, &tls.Config{Certificates: []tls.Certificate{pki.server},
		ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: pki.pool})

	with, err := mysqlTLS(config.TLSOpts{TLS: "verify-full", TLSCA: pki.caPEM,
		TLSCert: pki.clientCert, TLSKey: pki.clientKey}, "db.internal")
	if err != nil {
		t.Fatal(err)
	}
	if err = handshake(addr, with); err != nil {
		t.Fatalf("with a client certificate: %v", err)
	}
	without, err := mysqlTLS(config.TLSOpts{TLS: "verify-full", TLSCA: pki.caPEM}, "db.internal")
	if err != nil {
		t.Fatal(err)
	}
	if err = handshake(addr, without); err == nil {
		t.Fatal("the server accepted a client with no certificate")
	}
}

func TestMySQLTLSBadFiles(t *testing.T) {
	pki := newTestPKI(t)
	dir := t.TempDir()
	notPEM := filepath.Join(dir, "ca.der")
	if err := os.WriteFile(notPEM, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		opts config.TLSOpts
		want string
	}{
		{config.TLSOpts{TLS: "verify-full", TLSCA: filepath.Join(dir, "missing.pem")}, "missing.pem"},
		{config.TLSOpts{TLS: "verify-full", TLSCA: notPEM}, "no PEM certificate"},
		{config.TLSOpts{TLS: "verify-full", TLSCert: pki.clientCert, TLSKey: pki.caPEM}, "tls_key"},
	} {
		_, err := mysqlTLS(tc.opts, "h")
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%+v: got %v, want an error mentioning %q", tc.opts, err, tc.want)
		}
	}
}

// TestMySQLConnectorTLS: the connector carries the built config in place of
// the DSN's own tls=, and only prefer may fall back to plaintext. The
// connector's Config is not exported, so this goes through mysqlConnector's
// building blocks: ParseDSN is what it starts from.
func TestMySQLConnectorTLS(t *testing.T) {
	pki := newTestPKI(t)
	for _, mode := range config.TLSModes {
		opts := config.TLSOpts{TLS: mode}
		if mode != config.TLSDisable {
			opts.TLSCA = pki.caPEM
		}
		if _, err := mysqlConnector("u:p@tcp(db.internal:3306)/d?tls=skip-verify", opts); err != nil {
			t.Errorf("%s: %v", mode, err)
		}
	}
	if _, err := mysqlConnector("not a dsn", config.TLSOpts{TLS: "require"}); err == nil {
		t.Error("a bad DSN was accepted")
	}
}

// TestPgTLSDSN: the settings land in the DSN in its own syntax, replacing
// the DSN's, and pgx reads them back as the TLS config they describe.
func TestPgTLSDSN(t *testing.T) {
	pki := newTestPKI(t)
	opts := config.TLSOpts{TLS: "verify-full", TLSCA: pki.caPEM, TLSCert: pki.clientCert, TLSKey: pki.clientKey}

	for _, dsn := range []string{
		"postgres://u:pw@db.internal:5432/app?sslmode=disable",
		"host=db.internal port=5432 user=u password='pw' dbname=app sslmode=disable",
	} {
		out, err := pgTLSDSN(dsn, opts)
		if err != nil {
			t.Fatalf("%s: %v", dsn, err)
		}
		cfg, err := pgx.ParseConfig(out)
		if err != nil {
			t.Fatalf("%s: %v", out, err)
		}
		tc := cfg.TLSConfig
		switch {
		case tc == nil:
			t.Fatalf("%s: no TLS — the DSN's sslmode=disable won", out)
		case tc.InsecureSkipVerify || tc.ServerName != "db.internal" || tc.RootCAs == nil:
			t.Fatalf("%s: not verify-full: %+v", out, tc)
		case len(tc.Certificates) != 1:
			t.Fatalf("%s: client certificate not loaded", out)
		case len(cfg.Fallbacks) != 0:
			t.Fatalf("%s: verify-full must not fall back to plaintext", out)
		}
		if cfg.User != "u" || cfg.Password != "pw" || cfg.Database != "app" {
			t.Fatalf("%s: the rest of the DSN changed: %+v", out, cfg.Config)
		}
	}

	// a path with a space and a quote survives the keyword form's quoting
	dir := filepath.Join(t.TempDir(), "my certs")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	odd := filepath.Join(dir, "it's ca.pem")
	bs, _ := os.ReadFile(pki.caPEM)
	if err := os.WriteFile(odd, bs, 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := pgTLSDSN("host=db.internal", config.TLSOpts{TLS: "verify-ca", TLSCA: odd})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pgx.ParseConfig(out); err != nil {
		t.Fatalf("%s: %v", out, err)
	}

	// disable and prefer, as pgx implements them
	cfg, err := pgx.ParseConfig(must(pgTLSDSN("postgres://h/app?sslmode=require", config.TLSOpts{TLS: "disable"})))
	if err != nil || cfg.TLSConfig != nil {
		t.Fatalf("disable: TLS %v, %v", cfg.TLSConfig, err)
	}
	cfg, err = pgx.ParseConfig(must(pgTLSDSN("host=h", config.TLSOpts{TLS: "prefer"})))
	if err != nil || cfg.TLSConfig == nil || len(cfg.Fallbacks) != 1 || cfg.Fallbacks[0].TLSConfig != nil {
		t.Fatalf("prefer: want TLS with a plaintext fallback; got %+v, %v", cfg, err)
	}
}

// TestPgTLSHandshake: the TLS config pgx builds from the rewritten DSN
// passes and fails the same handshakes the MySQL one does — the two
// engines give each mode one meaning.
func TestPgTLSHandshake(t *testing.T) {
	pki := newTestPKI(t)
	addr := serveTLS(t, &tls.Config{Certificates: []tls.Certificate{pki.server}})
	for _, tc := range []struct {
		host string
		opts config.TLSOpts
		ok   bool
	}{
		{"db.internal", config.TLSOpts{TLS: "verify-full", TLSCA: pki.caPEM}, true},
		{"localhost", config.TLSOpts{TLS: "verify-full", TLSCA: pki.caPEM}, false},
		{"localhost", config.TLSOpts{TLS: "verify-ca", TLSCA: pki.caPEM}, true},
		{"db.internal", config.TLSOpts{TLS: "verify-ca", TLSCA: pki.otherCAPEM}, false},
		{"db.internal", config.TLSOpts{TLS: "require", TLSCA: pki.otherCAPEM}, false},
		{"localhost", config.TLSOpts{TLS: "require"}, true},
	} {
		cfg, err := pgx.ParseConfig(must(pgTLSDSN("host="+tc.host, tc.opts)))
		if err != nil {
			t.Fatal(err)
		}
		if err = handshake(addr, cfg.TLSConfig); (err == nil) != tc.ok {
			t.Errorf("%s %+v: handshake error %v, want ok=%v", tc.host, tc.opts, err, tc.ok)
		}
	}
}

func TestTLSOpenEmbedded(t *testing.T) {
	for _, drv := range []string{"sqlite", "bytdb"} {
		if _, _, err := tlsOpen(drv, "x.db", config.TLSOpts{TLS: "require"}); err == nil {
			t.Errorf("%s: TLS settings accepted for a local file", drv)
		}
		dsn, c, err := tlsOpen(drv, "x.db", config.TLSOpts{})
		if err != nil || c != nil || dsn != "x.db" {
			t.Errorf("%s with no TLS: got %q, %v, %v; want the DSN untouched", drv, dsn, c, err)
		}
	}
}

func must(s string, err error) string {
	if err != nil {
		panic(err)
	}
	return s
}
