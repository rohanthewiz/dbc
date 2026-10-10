package pipeline

import (
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rohanthewiz/serr"
)

// A Plugin is a kind of node: its name ("sql.read"), which of the four
// kinds it builds, and — the part that makes it self-describing — its
// Fields. The fields are read by four consumers off one declaration, so a
// plugin never needs UI code of its own:
//
//	Fields ──► the inspector form in dbc web  (a conn field draws the connection picker)
//	       ──► --set k=v flags headless
//	       ──► Validate, run by Check and before New
//	       ──► the docs listing (dbc plugins, the assistant's summary)
//
// Built-ins register in this package's init; the Go-code plugins in
// package script's; user plugins — Go files in plugins_dir — through
// package script's loader, which swaps the whole user set at once
// (SetUserPlugins) whenever a file changes.
type Plugin struct {
	Name   string  `json:"name"`  // "sql.read": a family, a dot, a verb
	Kind   Kind    `json:"kind"`  // what New returns
	Label  string  `json:"label"` // a few words for the palette: "SQL query"
	Doc    string  `json:"doc"`   // a paragraph: what it does, what its fields mean
	Fields []Field `json:"fields"`
	// New builds a node from a config already validated and filled with
	// defaults. It returns a Source, Transform, Sink or Action by Kind.
	New func(cfg Config) (any, error) `json:"-"`
	// Check, when set, looks deeper at a config than Validate's field
	// rules can — a Go field compiled, a cron line parsed — and returns
	// problems as messages. It must not touch a database. A message that
	// starts "warning: " (or has it after its place: "code:3:1: warning:
	// …") is a warning; any other is an error, which keeps a run from
	// starting.
	Check func(cfg Config) []string `json:"-"`
	// File is the plugin file a user plugin was loaded from (absolute);
	// "" for one compiled into dbc. It is what tells the palette's Yours
	// section, `dbc plugins` and SetUserPlugins the two apart.
	File string `json:"file,omitempty"`
}

// Kind is what a plugin builds.
type Kind string

const (
	KindSource    Kind = "source"
	KindTransform Kind = "transform"
	KindSink      Kind = "sink"
	KindAction    Kind = "action"
)

// Field is one setting of a plugin, as the inspector shows it and Validate
// checks it. Every value is a string in the config (Config); Type says how
// to read it and what to draw.
type Field struct {
	Name     string    `json:"name"`
	Type     FieldType `json:"type"`
	Doc      string    `json:"doc"`
	Default  string    `json:"default,omitempty"`
	Required bool      `json:"required,omitempty"`
	Enum     []string  `json:"enum,omitempty"` // the values a FieldEnum takes
}

// FieldType is what a field holds, and so what the UI draws for it.
type FieldType string

const (
	FieldString   FieldType = "string"   // one line
	FieldText     FieldType = "text"     // several lines (one item per line, usually)
	FieldInt      FieldType = "int"      // an integer
	FieldBool     FieldType = "bool"     // true/false (also yes/no, 1/0)
	FieldDuration FieldType = "duration" // "30s", "5m"
	FieldEnum     FieldType = "enum"     // one of Field.Enum
	FieldConn     FieldType = "conn"     // a connection name: the picker
	FieldTable    FieldType = "table"    // a table on the conn field: the catalog picker
	FieldColumns  FieldType = "columns"  // column names, comma- or line-separated
	FieldSQL      FieldType = "sql"      // SQL: a Monaco editor in SQL mode
	FieldGo       FieldType = "go"       // Go: a Monaco editor in Go mode, checked
)

// Config is a node's settings: every value a string, as the JSON spec
// holds them and as ${…} substitution rewrites them. The typed readers
// below are what plugins use; they never fail on a value Validate passed.
type Config map[string]string

// Str is the value of key, or def when unset or blank.
func (c Config) Str(key, def string) string {
	if v, ok := c[key]; ok && strings.TrimSpace(v) != "" {
		return v
	}
	return def
}

// Int is the value of key as an integer, or def when unset.
func (c Config) Int(key string, def int) (int, error) {
	v := strings.TrimSpace(c[key])
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, serr.New("not an integer", "field", key, "value", v)
	}
	return n, nil
}

// Bool is the value of key as a boolean: true/false, yes/no, on/off,
// 1/0, in any case; unset or blank is false.
func (c Config) Bool(key string) (bool, error) {
	v := strings.ToLower(strings.TrimSpace(c[key]))
	switch v {
	case "", "false", "no", "off", "0":
		return false, nil
	case "true", "yes", "on", "1":
		return true, nil
	}
	return false, serr.New("not a boolean", "field", key, "value", v)
}

// Duration is the value of key as a duration ("30s"), or def when unset.
func (c Config) Duration(key string, def time.Duration) (time.Duration, error) {
	v := strings.TrimSpace(c[key])
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, serr.New("not a duration", "field", key, "value", v)
	}
	return d, nil
}

// List is the value of key split into items: on newlines and commas,
// each trimmed, blanks dropped. "id, name" and "id\nname" read alike.
func (c Config) List(key string) []string {
	var out []string
	for _, line := range strings.Split(c[key], "\n") {
		for _, item := range strings.Split(line, ",") {
			if item = strings.TrimSpace(item); item != "" {
				out = append(out, item)
			}
		}
	}
	return out
}

// Lines is the value of key split into lines, each trimmed, blanks
// dropped — for a field whose items may themselves contain commas (a
// filter rule, an argument value).
func (c Config) Lines(key string) []string {
	var out []string
	for _, line := range strings.Split(c[key], "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out
}

// Clone copies the config.
func (c Config) Clone() Config {
	out := make(Config, len(c))
	for k, v := range c {
		out[k] = v
	}
	return out
}

// The registry: every plugin by name. Registration happens at init, from
// this package and from package script; user plugin files are swapped in
// as a set by SetUserPlugins, at a host's start and again whenever the
// plugins directory changes — the lock covers that later case, which
// runs while checks and runs read the registry from other goroutines.
var (
	regMu   sync.RWMutex
	regByNm = map[string]Plugin{}
	// problems are the user plugin files the last load could not use,
	// kept beside the registry so a node naming one is told why it is
	// missing rather than just that it is
	problems []PluginProblem
)

// PluginProblem is a user plugin file that did not load: it does not
// compile, its var Plugin is malformed, an entry point its kind needs is
// missing or has the wrong signature, or its name is taken. Such a file
// is listed (with Err) but cannot be placed.
type PluginProblem struct {
	File string `json:"file"`           // the file's absolute path
	Name string `json:"name,omitempty"` // the plugin's name, when the file got as far as saying it
	Err  string `json:"error"`
}

// SetUserPlugins replaces every user plugin (File != "") with ps, and the
// problems list with probs, under one lock: a check or a run reading the
// registry meanwhile sees the old set or the new one, never a registry
// with the user plugins half gone. A plugin in ps whose name a built-in
// (or an earlier one in ps) already has is refused, and joins the
// problems. What comes back is the problems list as stored.
func SetUserPlugins(ps []Plugin, probs []PluginProblem) []PluginProblem {
	regMu.Lock()
	defer regMu.Unlock()
	for name, p := range regByNm {
		if p.File != "" {
			delete(regByNm, name)
		}
	}
	out := slices.Clone(probs)
	for _, p := range ps {
		if have, dup := regByNm[p.Name]; dup {
			why := "a plugin built into dbc is called " + p.Name
			if have.File != "" {
				why = p.Name + " is already the plugin of " + have.File
			}
			out = append(out, PluginProblem{File: p.File, Name: p.Name, Err: why})
			continue
		}
		regByNm[p.Name] = p
	}
	sort.Slice(out, func(i, j int) bool { return out[i].File < out[j].File })
	problems = out
	return slices.Clone(out)
}

// PluginProblems lists the user plugin files the last load could not use,
// by file.
func PluginProblems() []PluginProblem {
	regMu.RLock()
	defer regMu.RUnlock()
	return slices.Clone(problems)
}

// problemFor is the load problem of the plugin called name, if its file
// failed after naming it.
func problemFor(name string) (PluginProblem, bool) {
	regMu.RLock()
	defer regMu.RUnlock()
	for _, p := range problems {
		if p.Name == name {
			return p, true
		}
	}
	return PluginProblem{}, false
}

// Register adds a plugin. A name registered twice, a plugin without New,
// or an unknown Kind is a programming error and panics, at init, where it
// is seen on the first run.
func Register(p Plugin) {
	if p.Name == "" || p.New == nil {
		panic("pipeline: a plugin needs a Name and a New")
	}
	switch p.Kind {
	case KindSource, KindTransform, KindSink, KindAction:
	default:
		panic(fmt.Sprintf("pipeline: plugin %s has kind %q", p.Name, p.Kind))
	}
	regMu.Lock()
	defer regMu.Unlock()
	if _, dup := regByNm[p.Name]; dup {
		panic("pipeline: plugin registered twice: " + p.Name)
	}
	regByNm[p.Name] = p
}

// Unregister removes a plugin by name (a reloaded user plugin; a test).
func Unregister(name string) {
	regMu.Lock()
	defer regMu.Unlock()
	delete(regByNm, name)
}

// Lookup is the plugin called name.
func Lookup(name string) (Plugin, bool) {
	regMu.RLock()
	defer regMu.RUnlock()
	p, ok := regByNm[name]
	return p, ok
}

// Plugins lists every plugin, by kind (source, transform, sink, action)
// and then by name — the palette's order.
func Plugins() []Plugin {
	regMu.RLock()
	defer regMu.RUnlock()
	out := make([]Plugin, 0, len(regByNm))
	for _, p := range regByNm {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		if ki, kj := kindOrder(out[i].Kind), kindOrder(out[j].Kind); ki != kj {
			return ki < kj
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func kindOrder(k Kind) int {
	return slices.Index([]Kind{KindSource, KindTransform, KindSink, KindAction}, k)
}

// Field is the plugin's field called name.
func (p Plugin) Field(name string) (Field, bool) {
	for _, f := range p.Fields {
		if f.Name == name {
			return f, true
		}
	}
	return Field{}, false
}

// Defaults is cfg with every unset field's Default put in. The result is
// what New receives, so a plugin reads its defaults from one place.
func (p Plugin) Defaults(cfg Config) Config {
	out := cfg.Clone()
	for _, f := range p.Fields {
		if _, ok := out[f.Name]; !ok && f.Default != "" {
			out[f.Name] = f.Default
		}
	}
	return out
}

// Validate is what is wrong with cfg by the field declarations alone: a
// required field blank, an int or bool that does not parse, an enum value
// not offered, a key no field has. Each problem is one message naming the
// field; none means New may be called (after Defaults).
func (p Plugin) Validate(cfg Config) []string {
	var out []string
	for _, f := range p.Fields {
		v, set := cfg[f.Name]
		blank := strings.TrimSpace(v) == ""
		if f.Required && blank {
			out = append(out, fmt.Sprintf("%s: required", f.Name))
			continue
		}
		if !set || blank {
			continue
		}
		switch f.Type {
		case FieldInt:
			if _, err := strconv.Atoi(strings.TrimSpace(v)); err != nil {
				out = append(out, fmt.Sprintf("%s: not an integer: %q", f.Name, v))
			}
		case FieldBool:
			if _, err := cfg.Bool(f.Name); err != nil {
				out = append(out, fmt.Sprintf("%s: not a boolean: %q", f.Name, v))
			}
		case FieldDuration:
			if _, err := time.ParseDuration(strings.TrimSpace(v)); err != nil {
				out = append(out, fmt.Sprintf("%s: not a duration: %q", f.Name, v))
			}
		case FieldEnum:
			if !slices.Contains(f.Enum, strings.TrimSpace(v)) {
				out = append(out, fmt.Sprintf("%s: %q is not one of %s", f.Name, v, strings.Join(f.Enum, ", ")))
			}
		}
	}
	keys := make([]string, 0, len(cfg))
	for k := range cfg {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if _, ok := p.Field(k); !ok {
			out = append(out, fmt.Sprintf("%s: no such field on %s", k, p.Name))
		}
	}
	return out
}

// Summary is the registry as plain text for the assistant, sent beside the
// sdb API summary (sdbapi.Summary) with a script question: every plugin as
// one line — kind, name, the first sentence of its doc, its fields (* for
// required) — so "add a node that …" and "write a plugin that …" are
// answered with names that exist. The user's own plugins are listed too,
// marked, and the plugin file's shape is spelled out once at the end.
//
//	source     sql.read — Runs a query on a connection … [conn*, query*, args]
//	transform  mask.email (yours) — Mask the e-mail column … [column*]
//
// One sentence per plugin keeps it to a size worth sending every
// conversation; the full docs are `dbc plugins`.
func Summary() string {
	var sb strings.Builder
	sb.WriteString("Pipeline plugins (node kinds). A pipeline spec's node names one in \"plugin\" and sets its fields " +
		"in \"cfg\", every value a string (* = required); a script uses the same names with sdb.Cfg.\n")
	for _, p := range Plugins() {
		var fields []string
		for _, f := range p.Fields {
			n := f.Name
			if f.Required {
				n += "*"
			}
			fields = append(fields, n)
		}
		mine := ""
		if p.File != "" {
			mine = " (yours)"
		}
		fmt.Fprintf(&sb, "%-9s  %s%s — %s [%s]\n", p.Kind, p.Name, mine, firstSentence(p.Doc), strings.Join(fields, ", "))
	}
	sb.WriteString("A user plugin is a Go file in plugins_dir (package main): var Plugin = sdb.Plugin{Name, Kind (sdb.KindSource, " +
		"KindTransform, KindSink, KindAction), Label, Doc, Fields: []sdb.Field{{Name, Type (sdb.FieldString, FieldColumns, …), Doc, " +
		"Default, Required}}} and plain funcs by kind — source: Next(e *sdb.Env) (*sdb.Batch, error); transform: " +
		"Apply(e *sdb.Env, b *sdb.Batch) (*sdb.Batch, error); sink: Write(e *sdb.Env, b *sdb.Batch) error; action: " +
		"Run(e *sdb.Env) error — plus optional Open/Flush/Commit/Close. A node's settings are e.Cfg (e.Cfg.Str(\"column\", \"\")); " +
		"a file path goes through e.Path, which puts a relative one in files_dir as the file plugins do.\n")
	return sb.String()
}

// firstSentence is doc up to the end of its first sentence.
func firstSentence(doc string) string {
	doc = strings.Join(strings.Fields(doc), " ")
	if i := strings.Index(doc, ". "); i >= 0 {
		return doc[:i+1]
	}
	return doc
}
