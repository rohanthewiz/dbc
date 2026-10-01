package config

import (
	"regexp"
	"strconv"
	"strings"
)

// Expanding a ${VAR} where it lands in a DSN.
//
// A plain os.Expand pastes the variable's value into the DSN's text as is.
// That is right where the DSN's syntax gives the spot no special meaning (a
// host, a SQLite path, a whole DSN kept in one variable), and wrong wherever
// it does: a password from the environment holding ' or \ ends a quoted
// libpq value early, and one holding @, /, # or % splits a URL in the wrong
// place. The value is the secret as the user has it everywhere else
// (PGPASSWORD, a vault), not pre-escaped for one DSN syntax, so the escaping
// is dbc's to do — the same escaping db.BuildDSN gives a password typed into
// the field.
//
// HOW. The DSN is expanded twice over: first os.Expand, with every reference
// replaced by a marker (so the reference syntax, $$ and ${} included, stays
// exactly os.Expand's), then the marked text is walked the way the driver's
// own parser walks it, and each marker is replaced by its value escaped for
// the spot it sits in:
//
//	postgres keyword/value  host=h password='${PW}'  ─► inside '…': \ and ' backslashed
//	                        password=${PW}           ─► bare: as is if it needs nothing,
//	                                                     else the whole value re-quoted
//	postgres URL            postgres://u:${PW}@h/${DB}?sslpassword=${K}
//	                        userinfo, path, query values ─► percent-encoded
//	mysql                   u:${PW}@tcp(h)/${DB}?loc=${TZ}
//	                        userinfo ─► as is (the driver takes the password raw:
//	                                    first ':' to last '@')
//	                        database, param values ─► the few bytes that split them
//	                                    percent-encoded (the driver unescapes them)
//	anything else           as is
//
// A reference anywhere else — a keyword, a host, the scheme, a whole DSN
// from one variable — is pasted as is, as before: there it is the user's own
// DSN text, which escaping would break.
//
// THE %XX RULE. Wherever an expansion is percent-encoded (the URL's parts,
// MySQL's database and params), a '%' already followed by two hex digits is
// left alone; only a bare '%' is encoded. Before this escaping existed, the
// one way to get a URL-breaking password through a ${VAR} was to keep it
// pre-encoded in the environment (p%40ss for p@ss), and a config that works
// today must not break. A raw value holding "%41" is read as "A" either way
// — it was misread before too — so leaving %XX alone costs nothing that
// worked; a bare "100%", which failed to parse before, now works.

// refMark is the stand-in for reference i while the DSN is walked. NUL
// cannot occur in a DSN anyone could type, and a marker holds no character
// any of the walks below looks for (quote, space, '=', '@', '/', '?', '&').
func refMark(i int) string { return "\x00" + strconv.Itoa(i) + "\x00" }

var refMarkRE = regexp.MustCompile("\x00([0-9]+)\x00")

// dsnShape is the DSN syntax a driver's DSN is parsed with, as far as
// escaping an expansion goes.
type dsnShape int

const (
	shapeRaw       dsnShape = iota // nothing to escape: SQLite and bytdb paths, unknown drivers
	shapePGKeyword                 // libpq keyword/value: host=h password='…'
	shapePGURL                     // postgres:// or postgresql://
	shapeMySQL                     // go-sql-driver: user:pass@tcp(addr)/db?params
)

// shapeOf picks the shape for driver (any alias db.Driver accepts) and the
// DSN as marked. A Postgres DSN is a URL exactly when pgx would take it for
// one — the same case-sensitive prefixes pgconn.ParseConfig checks.
func shapeOf(driver, marked string) dsnShape {
	switch strings.ToLower(driver) {
	case "postgres", "postgresql", "pg", "pgx":
		if strings.HasPrefix(marked, "postgres://") || strings.HasPrefix(marked, "postgresql://") {
			return shapePGURL
		}
		return shapePGKeyword
	case "mysql", "mariadb":
		return shapeMySQL
	}
	return shapeRaw
}

// fillRefs replaces the markers in marked with vals, escaped for where each
// one sits in a DSN of the given shape.
func fillRefs(shape dsnShape, marked string, vals []string) string {
	switch shape {
	case shapePGKeyword:
		return fillPGKeyword(marked, vals)
	case shapePGURL:
		return fillPGURL(marked, vals)
	case shapeMySQL:
		return fillMySQL(marked, vals)
	}
	return subst(marked, vals, nil)
}

// subst replaces every marker in s with its value, passed through esc (nil:
// as is).
func subst(s string, vals []string, esc func(string) string) string {
	if !strings.Contains(s, "\x00") {
		return s
	}
	return refMarkRE.ReplaceAllStringFunc(s, func(m string) string {
		i, _ := strconv.Atoi(m[1 : len(m)-1])
		v := vals[i]
		if esc != nil {
			v = esc(v)
		}
		return v
	})
}

// ---------------------------------------------------------------------------
// Postgres keyword/value
// ---------------------------------------------------------------------------

// pgSpace is the whitespace pgconn's keyword/value parser separates on.
func pgSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\v' || c == '\f'
}

// pgQuoteEsc escapes a value for inside libpq's single quotes. pgconn reads
// a quoted value by turning \\ into \ and \' into ', so those are the two
// escapes; db.BuildDSN's pgQuote writes the same.
var pgQuoteEsc = strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace

// fillPGKeyword walks marked as pgconn's parseKeywordValueSettings does —
// keyword up to '=', whitespace, then a 'quoted' or bare value — filling
// each value's markers for its kind of value. Keywords and the separators
// between pairs are filled as is.
//
// A DSN pgconn would refuse (no '=', an unterminated quote) is filled as is
// from that point on, so pgconn still refuses it, with its own message.
func fillPGKeyword(marked string, vals []string) string {
	var b strings.Builder
	s := marked
	for len(s) > 0 {
		eq := strings.IndexByte(s, '=')
		if eq < 0 {
			b.WriteString(subst(s, vals, nil))
			break
		}
		b.WriteString(subst(s[:eq+1], vals, nil))
		s = s[eq+1:]
		i := 0
		for i < len(s) && pgSpace(s[i]) {
			i++
		}
		b.WriteString(s[:i])
		s = s[i:]
		if len(s) == 0 {
			break
		}
		if s[0] == '\'' {
			end := 1
			for end < len(s) && s[end] != '\'' {
				if s[end] == '\\' {
					end++ // the escaped byte is not the closing quote
				}
				end++
			}
			if end >= len(s) {
				b.WriteString(subst(s, vals, nil)) // unterminated: pgconn's to report
				break
			}
			b.WriteString("'" + subst(s[1:end], vals, pgQuoteEsc) + "'")
			s = s[end+1:]
			continue
		}
		end := 0
		for end < len(s) && !pgSpace(s[end]) {
			if s[end] == '\\' {
				end++
			}
			end++
		}
		end = min(end, len(s))
		b.WriteString(fillPGBare(s[:end], vals))
		s = s[end:]
	}
	return b.String()
}

// fillPGBare fills a bare (unquoted) keyword value. A bare value cannot hold
// whitespace at all — pgconn ends it there, and keeps a backslash before a
// space rather than dropping it — so an expansion that holds whitespace, a
// quote or a backslash, or a value that would come out empty (which would
// make the next pair this one's value), turns the whole value into a quoted
// one: its typed text unescaped as pgconn would, the expansions put in, the
// lot quoted. Otherwise the value is filled as is, so a DSN whose variables
// need no care comes out exactly as plain expansion would make it.
func fillPGBare(tok string, vals []string) string {
	if !strings.Contains(tok, "\x00") {
		return tok
	}
	plain := subst(tok, vals, nil)
	// only what the expansions brought in is checked: the typed text around
	// them already reads right, backslash escapes and all
	if plain != "" && !strings.ContainsAny(expansions(tok, vals), " \t\n\r\v\f'\\") {
		return plain
	}
	unesc := strings.ReplaceAll(strings.ReplaceAll(tok, `\\`, `\`), `\'`, `'`)
	return "'" + pgQuoteEsc(subst(unesc, vals, nil)) + "'"
}

// expansions is the values of the markers in s, run together: what a check
// for characters the expansions brought in looks at.
func expansions(s string, vals []string) string {
	var b strings.Builder
	for _, m := range refMarkRE.FindAllStringSubmatch(s, -1) {
		i, _ := strconv.Atoi(m[1])
		b.WriteString(vals[i])
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// URLs
// ---------------------------------------------------------------------------

// isHex reports whether c is a hex digit.
func isHex(c byte) bool {
	return '0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F'
}

// preEncoded reports whether v[i], a '%', starts a %XX escape already —
// the user's pre-encoded value, kept as is (see THE %XX RULE above).
func preEncoded(v string, i int) bool {
	return i+2 < len(v) && isHex(v[i+1]) && isHex(v[i+2])
}

// pctByte writes c percent-encoded.
func pctByte(b *strings.Builder, c byte) {
	const hex = "0123456789ABCDEF"
	b.WriteByte('%')
	b.WriteByte(hex[c>>4])
	b.WriteByte(hex[c&15])
}

// pctEncode percent-encodes v: every byte but the URL's unreserved ones
// (RFC 3986: letters, digits, - . _ ~), those in keep, and a '%' that
// already starts a %XX escape.
func pctEncode(v, keep string) string {
	var b strings.Builder
	for i := 0; i < len(v); i++ {
		c := v[i]
		switch {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9',
			c == '-', c == '.', c == '_', c == '~', strings.IndexByte(keep, c) >= 0,
			c == '%' && preEncoded(v, i):
			b.WriteByte(c)
		default:
			pctByte(&b, c)
		}
	}
	return b.String()
}

// pctSome percent-encodes only the bytes of v in set: for a parser that
// unescapes a value but splits the DSN on just a few characters, and reads
// some values (MySQL's charset list) without unescaping them at all. A '%'
// that already starts a %XX escape is kept, as in pctEncode.
func pctSome(v, set string) string {
	if !strings.ContainsAny(v, set) {
		return v
	}
	var b strings.Builder
	for i := 0; i < len(v); i++ {
		c := v[i]
		if strings.IndexByte(set, c) >= 0 && !(c == '%' && preEncoded(v, i)) {
			pctByte(&b, c)
		} else {
			b.WriteByte(c)
		}
	}
	return b.String()
}

// userinfoEsc encodes an expansion in a URL's user:password. A ':' is left
// alone: in the password it is harmless (url.Parse splits at the first
// one), and in the user part it lets one variable hold "user:password".
func userinfoEsc(v string) string { return pctEncode(v, ":") }

// componentEsc encodes an expansion in a URL's path or a query value; pgx
// reads both through net/url, which decodes them.
func componentEsc(v string) string { return pctEncode(v, "") }

// fillPGURL fills a Postgres URL as net/url splits it — scheme://, the
// authority up to the first / ? or #, userinfo up to the authority's last
// '@', the path, the query, the fragment — encoding expansions in the
// userinfo, the path (the database) and the query's values. The host part,
// query keys and the fragment are filled as is: a ${PGHOST}:${PGPORT} or a
// list of hosts is the user's own text there.
func fillPGURL(marked string, vals []string) string {
	k := strings.Index(marked, "://") + len("://")
	var b strings.Builder
	b.WriteString(marked[:k])
	rest := marked[k:]
	authEnd := strings.IndexAny(rest, "/?#")
	if authEnd < 0 {
		authEnd = len(rest)
	}
	auth := rest[:authEnd]
	if at := strings.LastIndexByte(auth, '@'); at >= 0 {
		b.WriteString(subst(auth[:at], vals, userinfoEsc))
		b.WriteString(subst(auth[at:], vals, nil))
	} else {
		b.WriteString(subst(auth, vals, nil))
	}
	tail, frag := rest[authEnd:], ""
	if h := strings.IndexByte(tail, '#'); h >= 0 {
		tail, frag = tail[:h], tail[h:]
	}
	path, query := tail, ""
	if q := strings.IndexByte(tail, '?'); q >= 0 {
		path, query = tail[:q], tail[q+1:]
	}
	b.WriteString(subst(path, vals, componentEsc))
	if strings.Contains(tail, "?") {
		b.WriteString("?" + fillQuery(query, vals, componentEsc))
	}
	b.WriteString(subst(frag, vals, nil))
	return b.String()
}

// fillQuery fills a URL-style query, key=value pairs joined by &: values
// through esc, keys (and a pair with no '=') as is.
func fillQuery(q string, vals []string, esc func(string) string) string {
	pairs := strings.Split(q, "&")
	for i, p := range pairs {
		if eq := strings.IndexByte(p, '='); eq >= 0 {
			pairs[i] = subst(p[:eq+1], vals, nil) + subst(p[eq+1:], vals, esc)
		} else {
			pairs[i] = subst(p, vals, nil)
		}
	}
	return strings.Join(pairs, "&")
}

// ---------------------------------------------------------------------------
// MySQL
// ---------------------------------------------------------------------------

// fillMySQL fills a go-sql-driver DSN as mysql.ParseDSN splits it: at the
// LAST '/', then the database up to the first '?', then params split on '&'.
//
// Everything before that '/' — user, password, protocol, address — is
// filled as is: the driver takes the password raw (from the first ':' to
// the last '@' before the '/'), so it may hold any character but none needs
// escaping, and an encoded one would be taken literally. The database is
// path-unescaped by the driver, and the free-form params query-unescaped,
// so there only the bytes that would split them are encoded: a bare '%'
// (not one starting a %XX escape: THE %XX RULE),
// '/' (it would become the last '/'), '?' in the database, '&' and '+' in a
// value. Encoding no more keeps a value the driver reads without unescaping
// (charset=utf8mb4,utf8) as it was.
func fillMySQL(marked string, vals []string) string {
	slash := strings.LastIndexByte(marked, '/')
	if slash < 0 {
		return subst(marked, vals, nil)
	}
	head, tail := marked[:slash+1], marked[slash+1:]
	dbName, params, hasParams := strings.Cut(tail, "?")
	out := subst(head, vals, nil) + subst(dbName, vals, func(v string) string { return pctSome(v, "%/?") })
	if hasParams {
		out += "?" + fillQuery(params, vals, func(v string) string { return pctSome(v, "%/&+") })
	}
	return out
}
