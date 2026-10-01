package db

import (
	"crypto/tls"
	"crypto/x509"
	"database/sql/driver"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"

	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5"
	"github.com/rohanthewiz/serr"

	"github.com/rohanthewiz/dbc/config"
)

// A connection's TLS settings (config.TLSOpts: tls, tls_ca, tls_cert,
// tls_key) become each driver's own, here, as its pool is opened:
//
//	openPool(drv, dsn, opts, database)
//	  │
//	  ├─ pgx    ─► pgTLSDSN: the settings become the DSN's sslmode,
//	  │            sslrootcert, sslcert and sslkey, replacing any it had;
//	  │            pgx.ParseConfig then builds the TLS config itself
//	  │            (with tls_key_password: no sslcert/sslkey — openPool
//	  │            attaches the pair decrypted by clientCert, tlskey.go)
//	  │
//	  ├─ mysql  ─► mysqlConnector: the DSN is parsed, and a *tls.Config
//	  │            built by mysqlTLS is set on it — replacing its tls param
//	  │
//	  └─ sqlite, bytdb ─► refused: a local file has no connection to encrypt
//
// WHY TWO ROUTES. Postgres's DSN can already say everything the keys can,
// and pgx's handling of it is libpq's to the letter — multi-host fallbacks,
// sslmode=prefer's plaintext retry, verify-ca's chain-only check, SNI. Going
// through the DSN reuses all of that rather than re-creating it. MySQL's DSN
// cannot name a CA file or a client certificate (go-sql-driver takes those
// only as a *tls.Config), so for MySQL the config is built here, with
// libpq's meaning for each mode so a mode reads the same on both engines.
//
// mysql.RegisterTLSConfig would also work, but it is a process-wide
// registry keyed by name: two connections, or an edit in dbc web, would
// have to manage names and their lifetimes. Setting Config.TLS on a
// connector of the pool's own needs neither.
//
// A file that cannot be read (a CA, a certificate or its key) fails the
// open, so a Test in dbc web says so before anything is saved, and a
// connect says so instead of the server's less telling handshake error.

// tlsOpen applies TLS settings for the driver drv: for Postgres it returns
// the DSN to parse, for MySQL a connector to open the pool with (and the DSN
// unchanged). A nil connector means "sql.Open the DSN as before".
func tlsOpen(drv, dsn string, t config.TLSOpts) (string, driver.Connector, error) {
	if !t.Set() {
		return dsn, nil, nil
	}
	switch drv {
	case "pgx":
		out, err := pgTLSDSN(dsn, t)
		return out, nil, err
	case "mysql":
		c, err := mysqlConnector(dsn, t, "")
		return dsn, c, err
	}
	return "", nil, serr.New("tls settings are for postgres and mysql — this driver opens a local file, with no connection to encrypt",
		"driver", drv)
}

// pgTLSDSN rewrites a Postgres DSN to carry t as libpq parameters. pgx
// takes either form of DSN, and each is rewritten in its own syntax:
//
//	postgres://u@h/db?sslmode=disable  ─►  postgres://u@h/db?sslmode=verify-full&sslrootcert=/ca.pem
//	host=h dbname=db sslmode=disable   ─►  host=h dbname=db sslmode=disable sslmode='verify-full' sslrootcert='/ca.pem'
//
// In the keyword form the new settings are appended rather than spliced in:
// pgx reads the pairs into a map, so a later key replaces an earlier one,
// which spares parsing (and possibly mangling) the user's own quoting. Only
// the keys t sets are written; a DSN's own sslrootcert, say, still applies
// when tls_ca is not given.
//
// The result holds the DSN's password like the input did; it goes only to
// pgx.ParseConfig, whose errors leave the password out.
func pgTLSDSN(dsn string, t config.TLSOpts) (string, error) {
	params := [][2]string{}
	add := func(k, v string) {
		if v != "" {
			params = append(params, [2]string{k, v})
		}
	}
	add("sslmode", t.TLS)
	add("sslrootcert", t.TLSCA)
	if t.TLSKeyPassword == "" {
		add("sslcert", t.TLSCert)
		add("sslkey", t.TLSKey)
	} else {
		// An encrypted key: pgx would try (and, for PKCS#8, fail) to
		// decrypt it itself, so the pair stays out of the DSN and openPool
		// attaches the certificate dbc decrypted (tlskey.go). Set to ""
		// rather than left out, so a sslcert/sslkey in the DSN — or in
		// PGSSLCERT/PGSSLKEY, which pgx reads too — cannot add a second,
		// conflicting pair.
		params = append(params, [2]string{"sslcert", ""}, [2]string{"sslkey", ""})
	}

	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			// url.Parse's error quotes the URL, password and all
			return "", serr.New("the postgres DSN is not a valid URL, so tls settings cannot be added to it")
		}
		q := u.Query()
		for _, p := range params {
			q.Set(p[0], p[1])
		}
		u.RawQuery = q.Encode()
		return u.String(), nil
	}

	var b strings.Builder
	b.WriteString(dsn)
	for _, p := range params {
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		// libpq quoting: single quotes, with \ and ' escaped by a backslash,
		// so a path with spaces or quotes survives
		b.WriteString(p[0] + "='" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(p[1]) + "'")
	}
	return b.String(), nil
}

// pgClientCert attaches t's client certificate to a parsed Postgres config
// when its key is encrypted (tls_key_password) — the one case pgTLSDSN
// leaves the pair out of the DSN for, since pgx cannot decrypt a PKCS#8
// key itself (see tlskey.go). Otherwise it does nothing: pgx has loaded
// the pair from the DSN already.
//
// pgx builds one TLS config per host and per fallback (a multi-host DSN,
// or sslmode=prefer's plaintext retry), so the certificate goes on every
// one that has TLS. A nil TLSConfig is a plaintext attempt, and stays one.
func pgClientCert(pcfg *pgx.ConnConfig, t config.TLSOpts) error {
	if t.TLSKeyPassword == "" || t.TLSCert == "" {
		return nil
	}
	pair, err := clientCert(t)
	if err != nil {
		return err
	}
	if pcfg.TLSConfig != nil {
		pcfg.TLSConfig.Certificates = []tls.Certificate{pair}
	}
	for _, fb := range pcfg.Fallbacks {
		if fb.TLSConfig != nil {
			fb.TLSConfig.Certificates = []tls.Certificate{pair}
		}
	}
	return nil
}

// mysqlConnector parses a MySQL DSN and opens its connector per
// mysqlConfig: with the TLS config t asks for, and on database when that is
// set.
func mysqlConnector(dsn string, t config.TLSOpts, database string) (driver.Connector, error) {
	mc, err := mysqlConfig(dsn, t, database)
	if err != nil {
		return nil, err
	}
	return mysql.NewConnector(mc)
}

// mysqlConfig parses a MySQL DSN and applies what the DSN's text does not
// say: t's TLS config, when t is set, replacing whatever the DSN's tls param
// said; and database, when set, replacing the DSN's database (a connection
// derived onto another database, config.DatabaseSep). Unset, each leaves the
// DSN's own setting alone. It is mysqlConnector's whole decision, apart
// from the connector — whose Config is not exported — so tests read it here.
func mysqlConfig(dsn string, t config.TLSOpts, database string) (*mysql.Config, error) {
	mc, err := mysql.ParseDSN(dsn)
	if err != nil {
		return nil, err
	}
	if database != "" {
		mc.DBName = database
	}
	if !t.Set() {
		return mc, nil
	}
	host := mc.Addr
	if h, _, splitErr := net.SplitHostPort(mc.Addr); splitErr == nil {
		host = h
	}
	tc, err := mysqlTLS(t, host)
	if err != nil {
		return nil, err
	}
	// TLSConfig is the DSN's tls= param by name; TLS, when set, takes
	// precedence over it in the driver. "false" with TLS nil is the
	// driver's own spelling of no TLS, so disable needs no special case.
	mc.TLS, mc.TLSConfig = tc, "false"
	mc.AllowFallbackToPlaintext = t.TLS == config.TLSPrefer
	return mc, nil
}

// mysqlTLS builds the *tls.Config for a MySQL server at host, giving each
// mode libpq's meaning (see config/tls.go):
//
//	disable      nil — no TLS
//	prefer       encrypt, check nothing (and mysqlConnector allows plaintext)
//	require      encrypt, check nothing — unless tls_ca is set: then verify-ca
//	verify-ca    InsecureSkipVerify + verifyChain: the chain, not the name
//	verify-full  Go's own verification: chain against RootCAs (nil = the
//	             system's) and the certificate's names against ServerName
//
// InsecureSkipVerify in verify-ca is not "insecure" there: it only turns off
// Go's built-in check, which always includes the host name, so verifyChain
// can do the chain half alone. That is how pgx implements verify-ca too.
func mysqlTLS(t config.TLSOpts, host string) (*tls.Config, error) {
	if t.TLS == config.TLSDisable {
		return nil, nil
	}
	tc := &tls.Config{ServerName: host}
	if t.TLSCA != "" {
		roots, err := loadCA(t.TLSCA)
		if err != nil {
			return nil, err
		}
		tc.RootCAs = roots
	}
	if t.TLSCert != "" {
		pair, err := clientCert(t) // decrypting tls_key when it has a password
		if err != nil {
			return nil, err
		}
		tc.Certificates = []tls.Certificate{pair}
	}

	mode := t.TLS
	if mode == config.TLSRequire && t.TLSCA != "" {
		mode = config.TLSVerifyCA // libpq: naming a CA asks for it to be checked
	}
	switch mode {
	case config.TLSPrefer, config.TLSRequire:
		tc.InsecureSkipVerify = true
	case config.TLSVerifyCA:
		tc.InsecureSkipVerify = true
		tc.VerifyPeerCertificate = verifyChain(tc.RootCAs)
	case config.TLSVerifyFull:
		// the zero value of the rest is Go's full verification
	default:
		// config.TLSOpts.Check refuses any other mode before it gets here;
		// failing closed covers a caller that skipped it
		return nil, serr.New("unknown tls mode", "tls", t.TLS)
	}
	return tc, nil
}

// loadCA reads a PEM bundle of CA certificates into a pool. A file with no
// certificate in it is an error rather than an empty pool: an empty pool
// trusts nothing, which would fail every handshake with an "unknown
// authority" that hides the real cause (a key or a DER file given instead).
func loadCA(path string) (*x509.CertPool, error) {
	pem, err := os.ReadFile(path)
	if err != nil {
		return nil, serr.Wrap(fmt.Errorf("tls_ca: %w", err)) // os's error names the path
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, serr.New("tls_ca " + path + " holds no PEM certificate")
	}
	return pool, nil
}

// verifyChain is a tls.Config.VerifyPeerCertificate that checks the server's
// chain against roots (nil: the system's trust store) and nothing else — no
// host name, which is what makes verify-ca differ from verify-full. The
// server's own certificate comes first; the rest are intermediates it sent.
func verifyChain(roots *x509.CertPool) func([][]byte, [][]*x509.Certificate) error {
	return func(raw [][]byte, _ [][]*x509.Certificate) error {
		if len(raw) == 0 {
			return errors.New("the server sent no certificate")
		}
		certs := make([]*x509.Certificate, len(raw))
		for i, der := range raw {
			c, err := x509.ParseCertificate(der)
			if err != nil {
				return serr.Wrap(err, "op", "parse the server's certificate")
			}
			certs[i] = c
		}
		opts := x509.VerifyOptions{Roots: roots, Intermediates: x509.NewCertPool()}
		for _, c := range certs[1:] {
			opts.Intermediates.AddCert(c)
		}
		_, err := certs[0].Verify(opts)
		return err
	}
}
