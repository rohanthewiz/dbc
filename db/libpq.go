package db

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/rohanthewiz/serr"

	"github.com/rohanthewiz/dbc/config"
)

// A Postgres connection as libpq settings, for the PostgreSQL client tools
// dbc runs (pg_dump, pg_restore — see package pgdump). They speak libpq, not
// pgx, so the connection dbc would open has to be restated in libpq's terms:
//
//	config.Connection ──LibpqFor──► LibpqConn
//	  dsn (URL or key=value)          Settings: host, port, user, dbname,
//	  tls, tls_ca, tls_cert, tls_key            sslmode, sslrootcert, …
//	  tls_key_password ($VAR)                   sslpassword (from $VAR)
//	  Database ("prod/otherdb")       Password  (kept apart, see below)
//	                                  Dropped   (DSN keys libpq rejects)
//
// The same precedence as openPool: the DSN, then the connection's tls keys
// over the DSN's own ssl* (pgTLSDSN), then a derived connection's database
// over the DSN's dbname. What the DSN leaves out is left out here too, so
// the tool falls back on the same PG* environment and defaults pgx would.
//
// WHY A SERVICE FILE AND NOT A COMMAND LINE. A conninfo string on the tool's
// command line would put the password (and an encrypted key's sslpassword)
// in `ps` output for every user on the machine. Instead the settings go in a
// libpq service file that only the current user can read (ServiceFile), and
// the tool is given just "service=NAME". The password goes in the child's
// PGPASSWORD rather than the file: a service file cannot hold a value with
// a trailing space, and a password may have one.
//
// WHY PARSE THE DSN HERE AND NOT WITH pgx. pgx.ParseConfig resolves a DSN
// into a dialable config — it loads the certificate files and folds sslmode
// into a *tls.Config — so the settings as written are gone by the time it
// returns. libpq needs them as written.

// LibpqConn is a Postgres connection restated for libpq.
type LibpqConn struct {
	// Settings are libpq keywords with their values, in the order the DSN
	// wrote them (later overrides replace in place). Password is not among
	// them.
	Settings [][2]string
	// Password is the DSN's password, "" when it has none (libpq then falls
	// back on PGPASSWORD or ~/.pgpass, as pgx does).
	Password string
	// Dropped names DSN keys libpq does not know — the server run-time
	// parameters pgx sends at startup (search_path=…). They are left out
	// because libpq refuses a connection with an unknown keyword. pgx's own
	// pool and cache keys are left out silently: they mean nothing to a
	// single tool run.
	Dropped []string
}

// libpqKeywords are the connection keywords libpq accepts (PostgreSQL 18's
// list, less "replication", which no dump wants). A key outside it would
// make libpq fail with "invalid connection option".
var libpqKeywords = []string{
	"host", "hostaddr", "port", "dbname", "user", "password", "passfile",
	"require_auth", "channel_binding", "connect_timeout", "client_encoding",
	"options", "application_name", "fallback_application_name",
	"keepalives", "keepalives_idle", "keepalives_interval", "keepalives_count",
	"tcp_user_timeout", "gssencmode", "sslmode", "sslnegotiation",
	"sslcompression", "sslcert", "sslkey", "sslpassword", "sslcertmode",
	"sslrootcert", "sslcrl", "sslcrldir", "sslsni", "requirepeer",
	"ssl_min_protocol_version", "ssl_max_protocol_version", "krbsrvname",
	"gsslib", "gssdelegation", "service", "target_session_attrs",
	"load_balance_hosts", "sslkeylogfile",
	"oauth_issuer", "oauth_client_id", "oauth_client_secret", "oauth_scope",
}

// pgxOnlyKeys are the DSN keys pgx and pgxpool consume themselves: dropped
// without a note, since they configure the Go driver, not the session.
var pgxOnlyKeys = []string{
	"pool_max_conns", "pool_min_conns", "pool_min_idle_conns", "pool_max_conn_lifetime",
	"pool_max_conn_lifetime_jitter", "pool_max_conn_idle_time", "pool_health_check_period",
	"statement_cache_capacity", "description_cache_capacity", "default_query_exec_mode",
	"min_read_buffer_size", "servicefile",
}

// LibpqFor restates cc, a Postgres connection, as libpq settings. The error
// is the user's to read; it never quotes the DSN, which holds the password.
func LibpqFor(cc config.Connection) (LibpqConn, error) {
	drv, err := driverFor(cc.Driver)
	if err != nil {
		return LibpqConn{}, err
	}
	if drv != "pgx" {
		return LibpqConn{}, serr.New("not a Postgres connection", "conn", cc.Name, "driver", cc.Driver)
	}
	var pairs [][2]string
	if strings.HasPrefix(cc.DSN, "postgres://") || strings.HasPrefix(cc.DSN, "postgresql://") {
		pairs, err = libpqURL(cc.DSN)
	} else {
		pairs, err = libpqKeywordValues(cc.DSN)
	}
	if err != nil {
		return LibpqConn{}, serr.Wrap(err, "conn", cc.Name)
	}

	var lc LibpqConn
	set := func(k, v string) {
		for i := range lc.Settings {
			if lc.Settings[i][0] == k {
				lc.Settings[i][1] = v // a later setting wins, as in libpq and pgx
				return
			}
		}
		lc.Settings = append(lc.Settings, [2]string{k, v})
	}
	for _, kv := range pairs {
		k, v := kv[0], kv[1]
		switch {
		case k == "password":
			lc.Password = v
		case k == "ssl" && strings.EqualFold(v, "true"):
			// the JDBC spelling libpq accepts in a URL, and only there
			set("sslmode", "require")
		case k == "service":
			// The tool is pointed at dbc's own service file, and libpq does
			// not nest one service in another.
			return LibpqConn{}, serr.New("a DSN that names a service= cannot be handed to the PostgreSQL tools yet — "+
				"write the service's settings into the DSN instead", "conn", cc.Name)
		case slices.Contains(libpqKeywords, k):
			set(k, v)
		case slices.Contains(pgxOnlyKeys, k):
		default:
			lc.Dropped = append(lc.Dropped, k)
		}
	}

	// the tls keys win over the DSN's ssl*, as pgTLSDSN has them
	t := cc.TLSOpts
	if t.TLS != "" {
		set("sslmode", t.TLS)
	}
	if t.TLSCA != "" {
		set("sslrootcert", t.TLSCA)
	}
	if t.TLSCert != "" {
		set("sslcert", t.TLSCert)
	}
	if t.TLSKey != "" {
		set("sslkey", t.TLSKey)
	}
	if t.TLSKeyPassword != "" {
		// libpq (OpenSSL) decrypts the key itself given its passphrase, so
		// unlike openPool nothing is decrypted here. The variable is read
		// now and its absence is an error, as clientCert has it: an empty
		// passphrase tried against the key would fail less tellingly.
		name := t.KeyPasswordVar()
		if name == "" {
			return LibpqConn{}, serr.New("tls_key_password must name an environment variable, as ${VAR}")
		}
		pass, ok := os.LookupEnv(name)
		if !ok {
			return LibpqConn{}, serr.New("tls_key_password: $" + name + " is not set — export the key's passphrase in it")
		}
		set("sslpassword", pass)
	}
	// verify-full with no CA named anywhere: pgx checks the server against
	// the system's trust store (and config/tls.go promises that), while
	// libpq would look for ~/.postgresql/root.crt and fail without it.
	// "system" is libpq's (16+) word for the trust store. Only when that
	// file is absent: with it there, libpq's own default already works,
	// with any version of the tools.
	if mode, _ := lc.get("sslmode"); mode == config.TLSVerifyFull {
		if _, named := lc.get("sslrootcert"); !named && os.Getenv("PGSSLROOTCERT") == "" && !homeRootCert() {
			set("sslrootcert", "system")
		}
	}
	if cc.Database != "" {
		set("dbname", cc.Database) // a derived "<base>/<database>" connection
	}

	// A service file holds one setting per line, trailing blanks trimmed,
	// with no quoting: a value that cannot survive that is refused rather
	// than silently changed.
	for _, kv := range lc.Settings {
		if strings.ContainsAny(kv[1], "\r\n") || strings.TrimRight(kv[1], " \t") != kv[1] {
			return LibpqConn{}, serr.New("this DSN setting cannot be passed to the PostgreSQL tools "+
				"(it ends in a blank or spans lines)", "conn", cc.Name, "key", kv[0])
		}
	}
	return lc, nil
}

// get looks up a setting.
func (lc LibpqConn) get(k string) (string, bool) {
	for _, kv := range lc.Settings {
		if kv[0] == k {
			return kv[1], true
		}
	}
	return "", false
}

// homeRootCert reports whether libpq's default root certificate file,
// ~/.postgresql/root.crt, exists.
func homeRootCert() bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	_, err = os.Stat(filepath.Join(home, ".postgresql", "root.crt"))
	return err == nil
}

// ServiceFile renders the settings as a libpq service file with one
// service, name. It holds no password (see LibpqConn), but may hold an
// sslpassword: write it where only the current user can read it.
func (lc LibpqConn) ServiceFile(name string) []byte {
	var b strings.Builder
	b.WriteString("# written by dbc for one run of a PostgreSQL tool; removed after it\n")
	fmt.Fprintf(&b, "[%s]\n", name)
	for _, kv := range lc.Settings {
		fmt.Fprintf(&b, "%s=%s\n", kv[0], kv[1])
	}
	return []byte(b.String())
}

// libpqURL reads a postgres:// URL into keyword/value pairs the way libpq
// (and pgx) read one:
//
//	postgres://user:pass@h1:5432,h2:5433/db?sslmode=require&application_name=x
//	         └userinfo┘ └──── hosts ────┘└db┘└─────────── params ───────────┘
//
// net/url is not used for the authority because it rejects a multi-host
// list ("h1:5432,h2" is not a valid port); the credentials end at the LAST
// '@', since a password may hold an unencoded one.
func libpqURL(dsn string) ([][2]string, error) {
	_, rest, _ := strings.Cut(dsn, "://")
	authority, tail := rest, ""
	if i := strings.IndexAny(rest, "/?"); i >= 0 {
		authority, tail = rest[:i], rest[i:]
	}
	path, query, _ := strings.Cut(tail, "?")

	var out [][2]string
	add := func(k, v string) { out = append(out, [2]string{k, v}) }
	unesc := func(what, s string) (string, error) {
		u, err := url.PathUnescape(s)
		if err != nil {
			// the value is not quoted: it may be the password
			return "", serr.New("the postgres URL has a bad %-escape in its " + what)
		}
		return u, nil
	}

	if at := strings.LastIndexByte(authority, '@'); at >= 0 {
		user, pass, hasPass := strings.Cut(authority[:at], ":")
		authority = authority[at+1:]
		u, err := unesc("user name", user)
		if err != nil {
			return nil, err
		}
		if u != "" {
			add("user", u)
		}
		if hasPass {
			p, err := unesc("password", pass)
			if err != nil {
				return nil, err
			}
			add("password", p)
		}
	}
	if authority != "" {
		var hosts, ports []string
		anyPort := false
		for hp := range strings.SplitSeq(authority, ",") {
			h, p := hp, ""
			if strings.HasPrefix(hp, "[") { // [::1]:5432
				if end := strings.IndexByte(hp, ']'); end > 0 {
					h, p = hp[1:end], strings.TrimPrefix(hp[end+1:], ":")
				}
			} else if i := strings.LastIndexByte(hp, ':'); i >= 0 {
				h, p = hp[:i], hp[i+1:]
			}
			h, err := unesc("host", h) // a socket directory is written %2Ftmp
			if err != nil {
				return nil, err
			}
			hosts, ports = append(hosts, h), append(ports, p)
			anyPort = anyPort || p != ""
		}
		add("host", strings.Join(hosts, ","))
		if anyPort {
			// libpq takes one port per host; an empty one is the default
			add("port", strings.Join(ports, ","))
		}
	}
	if db := strings.TrimPrefix(path, "/"); db != "" {
		d, err := unesc("database name", db)
		if err != nil {
			return nil, err
		}
		add("dbname", d)
	}
	// split by hand rather than url.ParseQuery, which returns a map and so
	// loses the order a repeated key's last-one-wins depends on
	for kv := range strings.SplitSeq(query, "&") {
		if kv == "" {
			continue
		}
		k, v, _ := strings.Cut(kv, "=")
		ku, err := url.QueryUnescape(k)
		if err != nil {
			return nil, serr.New("the postgres URL has a bad %-escape in a parameter name")
		}
		vu, err := url.QueryUnescape(v)
		if err != nil {
			return nil, serr.New("the postgres URL has a bad %-escape in a parameter", "param", ku)
		}
		add(ku, vu)
	}
	return out, nil
}

// libpqKeywordValues reads libpq's keyword/value form:
//
//	host=db port=5432 password='it\'s a secret' dbname = app
//
// Pairs are separated by whitespace, '=' may have blanks around it, and a
// value is either 'single-quoted' or runs to the next blank; in both, a
// backslash takes the next character literally. This is libpq's grammar
// (conninfo_parse), which parseOptions only approximates — it also splits
// on '&' for the DSN form's options box, which would cut a password in two.
func libpqKeywordValues(s string) ([][2]string, error) {
	var out [][2]string
	isSpace := func(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v' }
	i := 0
	for {
		for i < len(s) && isSpace(s[i]) {
			i++
		}
		if i >= len(s) {
			return out, nil
		}
		start := i
		for i < len(s) && s[i] != '=' && !isSpace(s[i]) {
			i++
		}
		key := s[start:i]
		for i < len(s) && isSpace(s[i]) {
			i++
		}
		if key == "" || i >= len(s) || s[i] != '=' {
			return nil, serr.New(`the postgres DSN is not key=value pairs (missing "=" after a key)`, "key", key)
		}
		i++ // '='
		for i < len(s) && isSpace(s[i]) {
			i++
		}
		var val strings.Builder
		if i < len(s) && s[i] == '\'' {
			i++
			closed := false
			for i < len(s) {
				c := s[i]
				if c == '\\' && i+1 < len(s) {
					val.WriteByte(s[i+1])
					i += 2
					continue
				}
				i++
				if c == '\'' {
					closed = true
					break
				}
				val.WriteByte(c)
			}
			if !closed {
				return nil, serr.New("a quoted value in the postgres DSN is missing its closing '", "key", key)
			}
		} else {
			for i < len(s) && !isSpace(s[i]) {
				if s[i] == '\\' && i+1 < len(s) {
					i++
				}
				val.WriteByte(s[i])
				i++
			}
		}
		out = append(out, [2]string{key, val.String()})
	}
}
