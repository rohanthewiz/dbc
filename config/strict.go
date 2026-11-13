package config

import (
	"fmt"
	"reflect"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"
)

// Keys a TOML file holds that nothing decoded.
//
// WHY. The TOML decoder skips a key no struct field names, without a word.
// For most settings a typo then costs that setting alone — max_row = 50000
// leaves max_rows at 1000 — but in a connection it can cost the protection
// the file asked for: ssl = "verify-full" or tls_mode = … in place of tls
// connects with no certificate check at all, while a bad *value* of tls
// fails the load on purpose (TLSOpts.Check). So the two files dbc reads
// settings from report every key they did not decode:
//
//	config.toml       top level      ─► a warning (Config.Warnings), naming the key
//	                  [[connection]] ─► the load fails, naming the connection and key
//	connections.toml  top level      ─► a warning
//	                  [[connection]] ─► that entry is skipped, with a warning, and the
//	                                    file is not rewritten until it is fixed
//
// A connection's keys are refused where the others only warn because a
// connection is what reaches a server with credentials: one that cannot be
// what was meant should not connect at all, TLSOpts.Check's reasoning. The
// top-level keys are local preferences (row caps, timeouts, a theme), where
// carrying on with a warning is the kinder failure, as checkPlanTheme and the
// other value checks already do for a bad value.
//
// connections.toml skips one entry rather than failing, as MergeSaved does for
// a bad tls there: the file is dbc's, read by every start, and the config
// file's connections still work. Its rewrite is refused while a stray key is
// in it because the rewrite encodes SavedConn, which would drop the key
// silently — and the entry, skipped until then, would come back without it,
// which is the very connection-without-TLS this is here to stop.
//
// HOW. md.Undecoded() lists the keys as dotted paths, but a [[connection]]
// entry has no index in its path: the second entry's ssl is
// "connection.ssl", the same as the first's. md.Keys() is in document order,
// though, and has one "connection" key per [[connection]] header, so a walk
// counting those headers knows which entry each key sits in:
//
//	md.Keys()                 entry   undecoded?
//	max_row                    -      yes ─► top level: "max_row"
//	connection                 0      (header)
//	connection.name            0
//	connection.ssl             0      yes ─► entry 0: "ssl"
//	connection                 1      (header)
//	connection.sub             1      yes ─► entry 1: "sub"
//	connection.sub.x           1      yes ─► under "sub", already said
//
// An inline array (connection = [{…}, {…}]) has a single "connection" key for
// all its entries, so there the entry cannot be told, and the message says "a
// [[connection]] entry" without a name.

// strayKey is one key a file holds that decoded into nothing.
type strayKey struct {
	Key   string // dotted, from the top level or from inside its entry: "max_row", "ssl", "sub"
	Table bool   // a table rather than a value: [ai], or sub = { … } in an entry
	Conn  bool   // inside a [[connection]] entry
	Entry int    // that entry's index in the file; -1 when Conn is false or it cannot be told
}

// connTable is the TOML key of the connection array, in both files.
const connTable = "connection"

// strayKeys lists md's undecoded keys in document order, sorted into top
// level and [[connection]] entries as described above. A key under one
// already listed (ai.model under [ai]) is left out: the table's line says it.
func strayKeys(md toml.MetaData) []strayKey {
	undecoded := map[string]bool{}
	for _, k := range md.Undecoded() {
		undecoded[k.String()] = true
	}
	if len(undecoded) == 0 {
		return nil
	}
	// "ArrayHash" is [[connection]] headers, one key each; an inline
	// array of tables is "Array", with one key for all of its entries
	headers := md.Type(connTable) == "ArrayHash"
	entry := -1
	var out []strayKey
	var said []toml.Key // listed keys, to leave out what sits under them
	for _, k := range md.Keys() {
		if len(k) == 1 && k[0] == connTable {
			if headers {
				entry++
			}
			// a new entry: its keys repeat the last entry's paths
			// ("connection.sub" again), so what was said there is
			// no cover for them
			said = said[:0]
			continue
		}
		if !undecoded[k.String()] || under(k, said) {
			continue
		}
		said = append(said, k)
		t := md.Type(k...)
		sk := strayKey{Key: k.String(), Entry: -1, Table: t == "Hash" || t == "ArrayHash"}
		if k[0] == connTable && len(k) > 1 {
			sk.Key, sk.Conn = toml.Key(k[1:]).String(), true
			if headers {
				sk.Entry = entry
			}
		}
		out = append(out, sk)
	}
	return out
}

// under reports whether k sits inside one of the keys in said.
func under(k toml.Key, said []toml.Key) bool {
	for _, p := range said {
		if len(p) < len(k) && slices.Equal(k[:len(p)], p) {
			return true
		}
	}
	return false
}

// tomlKeys lists the keys a struct type decodes, in field order: each
// field's tag name (or its own name, untagged), an untagged embedded
// struct's keys flattened as the decoder flattens TLSOpts, and nothing for
// a "-" field or an unexported one. Read from the types rather than kept as
// a list, so a new setting is in every message the day it is added.
func tomlKeys(t reflect.Type) []string {
	var out []string
	for f := range t.Fields() {
		tag, _, _ := strings.Cut(f.Tag.Get("toml"), ",")
		switch {
		case tag == "-" || !f.IsExported():
			continue
		case f.Anonymous && tag == "" && f.Type.Kind() == reflect.Struct:
			out = append(out, tomlKeys(f.Type)...)
			continue
		case tag == "":
			tag = f.Name
		}
		out = append(out, tag)
	}
	return out
}

// Each file's keys, top level and per entry, for the messages.
var (
	configKeys     = tomlKeys(reflect.TypeFor[Config]())
	connectionKeys = tomlKeys(reflect.TypeFor[Connection]())
	savedConnKeys  = tomlKeys(reflect.TypeFor[SavedConn]())
)

// describe is a stray key as a message says it: its name, "table [ai]"
// for a table, and the known key it is most likely a typo of, if one is
// near enough (max_row ─► max_rows).
func (s strayKey) describe(known []string) string {
	what := s.Key
	if s.Table {
		what = "table [" + s.Key + "]"
	}
	if near := nearKey(s.Key, known); near != "" {
		return fmt.Sprintf("%s (did you mean %s?)", what, near)
	}
	return what
}

// keyAliases are other tools' names for dbc's TLS keys — libpq's and the
// MySQL clients' — which a config written from habit, or copied from a
// connection string, is likely to use. They are the very keys the strict
// check is for (an ssl = "verify-full" that is ignored connects unverified),
// and no edit distance finds tls from them: "ssl" is nearer "dsn".
var keyAliases = map[string]string{
	"ssl": "tls", "sslmode": "tls", "ssl_mode": "tls", "ssl-mode": "tls",
	"tls_mode": "tls", "tlsmode": "tls", "tls-mode": "tls",
	"sslrootcert": "tls_ca", "ssl_ca": "tls_ca", "ssl-ca": "tls_ca", "ca": "tls_ca", "tls_ca_file": "tls_ca",
	"sslcert": "tls_cert", "ssl_cert": "tls_cert", "ssl-cert": "tls_cert", "cert": "tls_cert",
	"sslkey": "tls_key", "ssl_key": "tls_key", "ssl-key": "tls_key", "key": "tls_key",
	"sslpassword": "tls_key_password", "ssl_password": "tls_key_password",
}

// nearKey is the key in known that key most likely stands for: an alias's
// key (keyAliases) when known has it, else the nearest by edit distance
// when it is near enough to be a typo of it — one edit for a key of up to
// four characters, two for a longer one, so a short key is not "near"
// every other short one ("ssl" is two edits from "dsn"). "" when none is.
func nearKey(key string, known []string) string {
	key = strings.ToLower(key)
	if k, ok := keyAliases[key]; ok && slices.Contains(known, k) {
		return k
	}
	limit := 2
	if len(key) <= 4 {
		limit = 1
	}
	best, bestD := "", limit+1
	for _, k := range known {
		if d := editDistance(key, k); d < bestD {
			best, bestD = k, d
		}
	}
	return best
}

// editDistance is the optimal-string-alignment distance between a and b:
// Levenshtein's insert, delete and substitute, plus a swap of two adjacent
// characters as one edit — the commonest slip at a keyboard ("dns" for
// "dsn"), which Levenshtein counts as two. In bytes: TOML keys a person
// types are ASCII in practice, and a multi-byte one only ever reads as a
// little farther from every key than it is.
func editDistance(a, b string) int {
	// d[i][j] is the distance between a[:i] and b[:j]; keys are short, so
	// the whole table costs nothing worth saving
	d := make([][]int, len(a)+1)
	for i := range d {
		d[i] = make([]int, len(b)+1)
		d[i][0] = i
	}
	for j := range d[0] {
		d[0][j] = j
	}
	for i := 1; i <= len(a); i++ {
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			d[i][j] = min(d[i-1][j]+1, d[i][j-1]+1, d[i-1][j-1]+cost)
			if i > 1 && j > 1 && a[i-1] == b[j-2] && a[i-2] == b[j-1] {
				d[i][j] = min(d[i][j], d[i-2][j-2]+1)
			}
		}
	}
	return d[len(a)][len(b)]
}

// entryName is how a message names the [[connection]] entry a stray key
// sits in: its name when it has one and the entry is known.
func entryName(s strayKey, name func(i int) string) string {
	if s.Entry >= 0 {
		if n := name(s.Entry); n != "" {
			return fmt.Sprintf("connection %q", n)
		}
		return fmt.Sprintf("[[connection]] entry %d", s.Entry+1)
	}
	return "a [[connection]] entry"
}
