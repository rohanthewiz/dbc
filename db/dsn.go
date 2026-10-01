package db

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/rohanthewiz/serr"
)

// A DSN from fields, and fields from a DSN — for dbc web's connection form,
// where typing host, port, user, password and database one box at a time is
// easier than getting a whole DSN's punctuation right at once.
//
//	form fields ──BuildDSN──► DSN ──► stored, connected, as any typed one
//	stored DSN ──SplitDSN───► fields ──► the edit form (password withheld)
//
// The DSN is still what is stored and connected with: the fields are only a
// way of writing one, so a DSN built here behaves exactly like the same text
// typed, and every other path (the config file, --dsn, the TUI) is untouched.
//
// THE SHAPES BUILT, one per engine:
//
//	postgres  host='db.example.com' port=5432 user=app password='${PGPASS}' dbname=app
//	mysql     app:${MYSQL_PASS}@tcp(db.example.com:3306)/app?parseTime=true
//	sqlite    scratch.db                       (file:scratch.db?mode=ro with options)
//	bytdb     notes.bytdb
//
// Postgres gets libpq's keyword/value form rather than a URL on purpose. A
// URL needs the password percent-encoded, and a ${VAR} in it is expanded
// (config.ExpandDSN) after any encoding could happen — so a password from
// the environment holding @, / or # would split the URL in the wrong place.
// In the keyword form the value sits in quotes, which only ' and \ can
// break. MySQL's DSN needs no such care: go-sql-driver takes the password
// raw, everything between the first ':' and the last '@'.
//
// ${VAR} and $VAR stay as typed in every field, to be expanded at connect
// like any DSN's (config.ExpandDSN) — so the password can live in the
// environment rather than the saved file.

// DSNParts is a DSN taken apart into what a person fills in. Host, Port,
// User, Password and Database are the server engines'; File is the embedded
// ones'. Options is everything else the driver takes, as key=value pairs
// separated by spaces or & (a value may be 'single quoted', for Postgres).
type DSNParts struct {
	Host     string `json:"host,omitempty"`
	Port     string `json:"port,omitempty"`
	User     string `json:"user,omitempty"`
	Password string `json:"password,omitempty"`
	Database string `json:"database,omitempty"`
	File     string `json:"file,omitempty"`
	Options  string `json:"options,omitempty"`
}

// ErrNotFields is SplitDSN meeting a DSN the fields cannot hold without
// losing something — several Postgres hosts, a MySQL unix socket. The form
// then edits the DSN as text instead.
var ErrNotFields = errors.New("this DSN cannot be shown as separate fields")

// pgFieldKeys are the Postgres keywords that have fields of their own, so an
// option may not set them too — the option would silently win.
var pgFieldKeys = []string{"host", "port", "user", "password", "dbname"}

// BuildDSN writes the DSN for driver (any alias db.Driver accepts) from p.
// The error is the user's to read: a field missing or holding something the
// DSN cannot carry.
func BuildDSN(driver string, p DSNParts) (string, error) {
	drv, err := driverFor(driver)
	if err != nil {
		return "", err
	}
	// The password is left exactly as typed: leading or trailing spaces in
	// one are rare, but trimming them would be a silent wrong password.
	p.Host, p.Port, p.User = strings.TrimSpace(p.Host), strings.TrimSpace(p.Port), strings.TrimSpace(p.User)
	p.Database, p.File, p.Options = strings.TrimSpace(p.Database), strings.TrimSpace(p.File), strings.TrimSpace(p.Options)

	switch drv {
	case "pgx", "mysql":
		if p.Host == "" {
			return "", serr.New("enter the host (localhost for this machine)")
		}
		if p.Port != "" {
			if n, convErr := strconv.Atoi(p.Port); convErr != nil || n < 1 || n > 65535 {
				return "", serr.New("the port is a number from 1 to 65535", "port", p.Port)
			}
		}
		if drv == "pgx" {
			return buildPG(p)
		}
		return buildMySQL(p)
	case "sqlite":
		if p.File == "" {
			return "", serr.New("enter the database file (or a name with mode=memory in the options)")
		}
		if p.Options == "" {
			return p.File, nil
		}
		opts, optErr := parseOptions(p.Options)
		if optErr != nil {
			return "", optErr
		}
		// modernc's driver reads ?options only from a file: URI
		return "file:" + strings.TrimPrefix(p.File, "file:") + "?" + joinAmp(opts), nil
	default: // bytdb
		if p.File == "" {
			return "", serr.New("enter the database file")
		}
		if p.Options != "" {
			return "", serr.New("bytdb takes no options — its DSN is the file's path")
		}
		return p.File, nil
	}
}

// buildPG writes libpq's keyword/value form, the fields first and then the
// options, in a fixed order so the same fields always give the same text
// (which is how an edit that changed nothing is recognized as no change).
func buildPG(p DSNParts) (string, error) {
	opts, err := parseOptions(p.Options)
	if err != nil {
		return "", err
	}
	for _, kv := range opts {
		if slices.Contains(pgFieldKeys, strings.ToLower(kv[0])) {
			return "", serr.New("set "+kv[0]+" in its own field, not in the options", "option", kv[0])
		}
	}
	var pairs []string
	add := func(k, v string, always bool) {
		if v != "" {
			pairs = append(pairs, k+"="+pgQuote(v, always))
		}
	}
	add("host", p.Host, false)
	add("port", p.Port, false)
	add("user", p.User, false)
	// always quoted: a ${VAR} expands to whatever the environment holds,
	// spaces included, and only quotes keep that one value
	add("password", p.Password, true)
	add("dbname", p.Database, false)
	for _, kv := range opts {
		add(kv[0], kv[1], false)
	}
	return strings.Join(pairs, " "), nil
}

// pgQuote quotes a keyword/value DSN value when it needs it — always when
// asked, and whenever it is empty or holds a space, a quote, a backslash or
// a $ (which may expand to any of those) — escaping ' and \ inside.
func pgQuote(v string, always bool) string {
	if !always && v != "" && !strings.ContainsAny(v, " \t\n'\\$") {
		return v
	}
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(v) + "'"
}

// buildMySQL writes go-sql-driver's [user[:password]@]tcp(host:port)/db
// form. The driver splits user from password at the first ':' and the
// credentials from the address at the last '@', so a password may hold
// anything, a user anything but ':'. The database name ends at '?' and may
// not hold '/', which is how the driver finds where it starts.
func buildMySQL(p DSNParts) (string, error) {
	switch {
	case strings.Contains(p.User, ":"):
		return "", serr.New("a MySQL user name cannot contain ':' in a DSN")
	case p.Password != "" && p.User == "":
		return "", serr.New("a password needs a user name")
	case strings.ContainsAny(p.Database, "/?"):
		return "", serr.New("a MySQL database name cannot contain '/' or '?' in a DSN")
	}
	var b strings.Builder
	if p.User != "" {
		b.WriteString(p.User)
		if p.Password != "" {
			b.WriteString(":" + p.Password)
		}
		b.WriteByte('@')
	}
	addr := p.Host
	if p.Port != "" {
		addr = net.JoinHostPort(p.Host, p.Port) // brackets an IPv6 host
	} else if strings.Contains(p.Host, ":") {
		addr = "[" + p.Host + "]"
	}
	b.WriteString("tcp(" + addr + ")/" + p.Database)
	if p.Options != "" {
		opts, err := parseOptions(p.Options)
		if err != nil {
			return "", err
		}
		b.WriteString("?" + joinAmp(opts))
	}
	return b.String(), nil
}

// parseOptions reads key=value pairs separated by spaces or &, the two ways
// people write them (libpq's keyword form, a URL's query). A value may be
// 'single quoted' to hold spaces or &, with \' and \\ inside, as libpq
// allows. Order is kept: the DSN is written in the order typed.
func parseOptions(s string) ([][2]string, error) {
	var out [][2]string
	i := 0
	for {
		for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '&') {
			i++
		}
		if i >= len(s) {
			return out, nil
		}
		eq := strings.IndexByte(s[i:], '=')
		if eq <= 0 {
			end := strings.IndexAny(s[i:], " \t\n&")
			if end < 0 {
				end = len(s) - i
			}
			return nil, serr.New("options are key=value pairs, separated by spaces or &", "option", s[i:i+end])
		}
		key := strings.TrimSpace(s[i : i+eq])
		if strings.ContainsAny(key, " \t\n&'") {
			return nil, serr.New("options are key=value pairs, separated by spaces or &", "option", key)
		}
		i += eq + 1
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
				return nil, serr.New("an option's quoted value is missing its closing '", "option", key)
			}
		} else {
			for i < len(s) && !strings.ContainsRune(" \t\n&", rune(s[i])) {
				val.WriteByte(s[i])
				i++
			}
		}
		out = append(out, [2]string{key, val.String()})
	}
}

// joinAmp writes options as a URL-style query, values as typed: a MySQL or
// SQLite option is copied into the DSN the way the user would have typed it
// there (loc=Local, _pragma=busy_timeout(5000)), not re-encoded under them.
func joinAmp(opts [][2]string) string {
	parts := make([]string, len(opts))
	for i, kv := range opts {
		parts[i] = kv[0] + "=" + kv[1]
	}
	return strings.Join(parts, "&")
}

// envRef matches the ${VAR} and $VAR references config.ExpandDSN expands.
var envRef = regexp.MustCompile(`\$\{[^}]*\}|\$[A-Za-z_][A-Za-z0-9_]*`)

// SplitDSN takes a stored DSN apart into fields, for the edit form: the
// inverse of BuildDSN, and of the common hand-written shapes too — a
// Postgres URL as well as the keyword form. ${VAR}s come back as typed.
//
// A DSN the fields could not hold faithfully is ErrNotFields; one the
// driver could not read either is an error too. Either way the form falls
// back to editing the DSN as text, which loses nothing.
func SplitDSN(driver, dsn string) (DSNParts, error) {
	drv, err := driverFor(driver)
	if err != nil {
		return DSNParts{}, err
	}
	// url.Parse refuses { and } in a user name or password, so references
	// are swapped for plain-letter stand-ins while parsing and put back in
	// every field after. The stand-in cannot collide with the DSN's own
	// text: it carries a counter beyond any the DSN contains.
	refs := map[string]string{}
	tag := "dbcref"
	for strings.Contains(dsn, tag) {
		tag += "x"
	}
	masked := envRef.ReplaceAllStringFunc(dsn, func(m string) string {
		k := fmt.Sprintf("%s%dz", tag, len(refs))
		refs[k] = m
		return k
	})

	var p DSNParts
	switch drv {
	case "pgx":
		p, err = splitPG(masked)
	case "mysql":
		p, err = splitMySQL(masked)
	case "sqlite":
		rest := strings.TrimPrefix(masked, "file:")
		p.File, p.Options, _ = strings.Cut(rest, "?")
		p.Options = strings.ReplaceAll(p.Options, "&", " ")
	default:
		p.File = masked
	}
	if err != nil {
		return DSNParts{}, err
	}
	unmask := func(s string) string {
		for k, v := range refs {
			s = strings.ReplaceAll(s, k, v)
		}
		return s
	}
	for _, f := range []*string{&p.Host, &p.Port, &p.User, &p.Password, &p.Database, &p.File, &p.Options} {
		*f = unmask(*f)
	}
	return p, nil
}

// splitPG reads either Postgres form. Options come back space-separated,
// quoted where needed, in the form BuildDSN reads back.
func splitPG(dsn string) (DSNParts, error) {
	var p DSNParts
	var opts []string
	addOpt := func(k, v string) { opts = append(opts, k+"="+pgQuote(v, false)) }

	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			// url.Parse's error quotes the URL, password and all
			return p, ErrNotFields
		}
		if strings.Contains(u.Host, ",") {
			return p, ErrNotFields // several hosts: one Host field cannot hold them
		}
		p.Host, p.Port = u.Hostname(), u.Port()
		if u.User != nil {
			p.User = u.User.Username()
			p.Password, _ = u.User.Password()
		}
		p.Database = strings.TrimPrefix(u.Path, "/")
		q := u.Query()
		keys := make([]string, 0, len(q))
		for k := range q {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			addOpt(k, q.Get(k))
		}
		p.Options = strings.Join(opts, " ")
		return p, nil
	}

	kvs, err := parseOptions(dsn)
	if err != nil {
		return p, ErrNotFields
	}
	for _, kv := range kvs {
		switch strings.ToLower(kv[0]) {
		case "host":
			if strings.Contains(kv[1], ",") {
				return p, ErrNotFields
			}
			p.Host = kv[1]
		case "port":
			p.Port = kv[1]
		case "user":
			p.User = kv[1]
		case "password":
			p.Password = kv[1]
		case "dbname":
			p.Database = kv[1]
		default:
			addOpt(kv[0], kv[1])
		}
	}
	p.Options = strings.Join(opts, " ")
	return p, nil
}

// splitMySQL reads [user[:password]@][tcp[(addr)]]/dbname[?params], finding
// the pieces as go-sql-driver does: the last '/' starts the database, the
// last '@' before it ends the credentials, the first ':' in those ends the
// user. Only TCP fits the fields; a unix socket is ErrNotFields.
func splitMySQL(dsn string) (DSNParts, error) {
	var p DSNParts
	slash := strings.LastIndexByte(dsn, '/')
	if slash < 0 {
		return p, ErrNotFields
	}
	head, tail := dsn[:slash], dsn[slash+1:]
	p.Database, p.Options, _ = strings.Cut(tail, "?")
	p.Options = strings.ReplaceAll(p.Options, "&", " ")

	netAddr := head
	if at := strings.LastIndexByte(head, '@'); at >= 0 {
		cred := head[:at]
		netAddr = head[at+1:]
		p.User, p.Password, _ = strings.Cut(cred, ":")
	}
	addr := ""
	switch {
	case netAddr == "":
		addr = "" // the driver's default, 127.0.0.1:3306
	case strings.HasPrefix(netAddr, "tcp(") && strings.HasSuffix(netAddr, ")"):
		addr = netAddr[len("tcp(") : len(netAddr)-1]
	case netAddr == "tcp":
	default:
		return p, ErrNotFields
	}
	if addr == "" {
		p.Host, p.Port = "127.0.0.1", "3306"
		return p, nil
	}
	if h, port, err := net.SplitHostPort(addr); err == nil {
		p.Host, p.Port = h, port
	} else {
		p.Host = strings.Trim(addr, "[]")
	}
	return p, nil
}

// IsEnvRef reports whether s is nothing but one ${VAR} or $VAR reference.
// dbc web's edit form shows such a password, which names where the secret
// lives rather than being it, and withholds any other.
func IsEnvRef(s string) bool {
	return s != "" && envRef.FindString(s) == s
}
