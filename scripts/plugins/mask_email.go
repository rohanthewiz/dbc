//go:build ignore

// Masks an e-mail column: the part before the @ becomes a short hash and the
// domain stays, so rows still group by domain and one address always masks
// the same way.
//
// A pipeline plugin: copy it into plugins_dir (~/.config/dbc/plugins) and
// mask.email joins the palette under Yours, and `dbc plugins`.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/rohanthewiz/dbc/sdb"
)

// Plugin describes the node: its name in a spec, its kind, and the settings
// the inspector draws as a form (and a spec's "cfg" sets).
var Plugin = sdb.Plugin{
	Name:  "mask.email",
	Kind:  sdb.KindTransform,
	Label: "Mask e-mail",
	Fields: []sdb.Field{
		{Name: "column", Type: sdb.FieldString, Required: true, Doc: "The column holding the addresses."},
		{Name: "salt", Type: sdb.FieldString, Doc: "Mixed into the hash, so a masked address cannot be found by hashing guesses."},
	},
}

// Apply is called once per batch. e.Cfg is this node's settings; b's rows
// may be changed in place and handed on.
func Apply(e *sdb.Env, b *sdb.Batch) (*sdb.Batch, error) {
	name := e.Cfg.Str("column", "")
	c := b.Col(name)
	if c < 0 {
		return nil, fmt.Errorf("no column %s (the rows have %s)", name, strings.Join(b.Names(), ", "))
	}
	salt := e.Cfg.Str("salt", "")
	for i := range b.Rows {
		s, ok := b.Rows[i][c].(string)
		if !ok {
			continue // NULL, or not text: left as it is
		}
		local, domain, found := strings.Cut(s, "@")
		if !found {
			continue
		}
		sum := sha256.Sum256([]byte(salt + strings.ToLower(local)))
		// built in a variable, then stored: the interpreter mis-stores an
		// operator's result put straight into a row (dbc's check warns)
		masked := hex.EncodeToString(sum[:5]) + "@" + strings.ToLower(domain)
		b.Rows[i][c] = masked
	}
	return b, nil
}
