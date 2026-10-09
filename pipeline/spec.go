package pipeline

import (
	"bytes"
	"encoding/json"
	"regexp"
	"slices"
	"strings"

	"github.com/rohanthewiz/serr"
)

// Spec is a pipeline as data: what the JSON file in the pipelines
// directory holds, what the canvas edits, and what Builder produces from
// Go. Fragments run in order.
//
//	{
//	  "name": "orders-nightly",
//	  "desc": "Yesterday's orders into the warehouse",
//	  "params": { "days": { "default": "1", "doc": "How many days back" } },
//	  "fragments": [
//	    { "name": "orders", "batch": 1000,
//	      "nodes": [
//	        { "id": "src", "plugin": "sql.read",  "cfg": { "conn": "prod", "query": "SELECT … ${days} …" } },
//	        { "id": "dst", "plugin": "sql.write", "cfg": { "conn": "warehouse", "table": "stg.orders" } } ],
//	      "edges": [ ["src", "dst"] ] },
//	    { "name": "merge",
//	      "nodes": [ { "id": "up", "plugin": "sql.exec", "cfg": { "conn": "warehouse", "sql": "INSERT …" } } ] }
//	  ]
//	}
type Spec struct {
	Name      string           `json:"name"`
	Desc      string           `json:"desc,omitempty"`
	Params    map[string]Param `json:"params,omitempty"`
	Fragments []Fragment       `json:"fragments"`
}

// Param is one parameter a run may set: ${name} in any node's config. A
// parameter without a default must be given on each run.
type Param struct {
	Default string `json:"default"`
	Doc     string `json:"doc,omitempty"`
}

// Fragment is one unit of batch processing: a tree of nodes rooted at a
// source, with transforms along the branches and sinks at the leaves — or
// a single action node. Edges connect nodes; a node has at most one
// incoming edge, so the rows' path is unambiguous.
type Fragment struct {
	Name string `json:"name"`
	// Batch is rows per batch; 0 means DefaultBatch.
	Batch int `json:"batch,omitempty"`
	// OnError is what a failure of this fragment does to the pipeline:
	// "stop" (the default) ends the run; "continue" logs and goes on.
	OnError string `json:"on_error,omitempty"`
	Nodes   []Node `json:"nodes"`
	Edges   []Edge `json:"edges,omitempty"`
	// UI is the canvas's own: a node's position, by id. Never read by a run.
	UI map[string][]float64 `json:"ui,omitempty"`
}

// DefaultBatch is rows per batch when a fragment does not say.
const DefaultBatch = 1000

// Node is one plugin instance. Fn is set only by Builder, for a Go func
// standing in for a plugin (FragmentBuilder.Func): it never reaches JSON.
type Node struct {
	ID     string `json:"id"`
	Plugin string `json:"plugin"`
	Cfg    Config `json:"cfg,omitempty"`
	Fn     any    `json:"-"`
}

// Edge is "rows flow from From to To"; in JSON a two-element array.
type Edge struct {
	From, To string
}

func (e Edge) MarshalJSON() ([]byte, error) {
	return json.Marshal([2]string{e.From, e.To})
}

func (e *Edge) UnmarshalJSON(b []byte) error {
	var pair []string
	if err := json.Unmarshal(b, &pair); err != nil || len(pair) != 2 {
		return serr.New(`an edge is ["from", "to"]`, "got", string(b))
	}
	e.From, e.To = pair[0], pair[1]
	return nil
}

// Parse reads a spec from its JSON. A key the spec does not have is an
// error (a typo'd "fragment" would otherwise silently drop everything),
// as is anything Check would refuse outright — but Check's rules are not
// applied here: a spec being edited parses, so the editor can show what is
// wrong with it.
func Parse(text string) (*Spec, error) {
	dec := json.NewDecoder(strings.NewReader(text))
	dec.DisallowUnknownFields()
	var s Spec
	if err := dec.Decode(&s); err != nil {
		return nil, serr.Wrap(err, "op", "parse pipeline")
	}
	return &s, nil
}

// JSON is the spec as the file holds it: two-space indented, one node per
// line group, so a diff reads as the change it is.
func (s *Spec) JSON() (string, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false) // "<" in SQL stays "<"
	if err := enc.Encode(s); err != nil {
		return "", serr.Wrap(err, "op", "encode pipeline")
	}
	// an array of scalars — an edge ["src", "dst"], a position [40, 80] —
	// on one line: the encoder's one-element-per-line form makes a
	// two-word edge six lines, and the file is meant to be read
	return scalarArrayRe.ReplaceAllStringFunc(buf.String(), func(m string) string {
		inner := strings.TrimSpace(m[1 : len(m)-1])
		var items []string
		for _, it := range strings.Split(inner, ",") {
			items = append(items, strings.TrimSpace(it))
		}
		return "[" + strings.Join(items, ", ") + "]"
	}), nil
}

// scalarArrayRe matches a JSON array whose elements are all strings
// without quotes inside them or numbers, as the encoder indents it.
var scalarArrayRe = regexp.MustCompile(`\[\s*((?:"[^"\\\n]*"|-?[0-9.eE+\-]+)(?:,\s*(?:"[^"\\\n]*"|-?[0-9.eE+\-]+))*)\s*\]`)

// Fragment is the fragment called name.
func (s *Spec) Fragment(name string) *Fragment {
	for i := range s.Fragments {
		if s.Fragments[i].Name == name {
			return &s.Fragments[i]
		}
	}
	return nil
}

// Node is the node with id.
func (f *Fragment) Node(id string) *Node {
	for i := range f.Nodes {
		if f.Nodes[i].ID == id {
			return &f.Nodes[i]
		}
	}
	return nil
}

// Children are the ids rows flow to from id, in edge order.
func (f *Fragment) Children(id string) []string {
	var out []string
	for _, e := range f.Edges {
		if e.From == id {
			out = append(out, e.To)
		}
	}
	return out
}

// Parent is the id rows flow to id from, or "".
func (f *Fragment) Parent(id string) string {
	for _, e := range f.Edges {
		if e.To == id {
			return e.From
		}
	}
	return ""
}

// Roots are the nodes with no incoming edge, in node order — the source,
// in a well-formed fragment.
func (f *Fragment) Roots() []string {
	var out []string
	for _, n := range f.Nodes {
		if f.Parent(n.ID) == "" {
			out = append(out, n.ID)
		}
	}
	return out
}

// Order lists the node ids reachable from root in depth-first, edge
// order: every node after its parent, which is the order transforms are
// flushed in and children are visited in. It stops at a cycle (a node
// seen twice is not listed again).
func (f *Fragment) Order(root string) []string {
	var out []string
	seen := map[string]bool{}
	var walk func(id string)
	walk = func(id string) {
		if seen[id] {
			return
		}
		seen[id] = true
		out = append(out, id)
		for _, c := range f.Children(id) {
			walk(c)
		}
	}
	walk(root)
	return out
}

// BatchSize is the fragment's batch size, defaulted.
func (f *Fragment) BatchSize() int {
	if f.Batch > 0 {
		return f.Batch
	}
	return DefaultBatch
}

// Clone copies the spec deeply enough that editing the copy's fragments,
// nodes and configs leaves the original alone (Fn values are shared).
func (s *Spec) Clone() *Spec {
	c := *s
	c.Params = map[string]Param{}
	for k, v := range s.Params {
		c.Params[k] = v
	}
	c.Fragments = make([]Fragment, len(s.Fragments))
	for i, f := range s.Fragments {
		cf := f
		cf.Nodes = make([]Node, len(f.Nodes))
		for j, n := range f.Nodes {
			cn := n
			cn.Cfg = n.Cfg.Clone()
			cf.Nodes[j] = cn
		}
		cf.Edges = slices.Clone(f.Edges)
		cf.UI = map[string][]float64{}
		for k, v := range f.UI {
			cf.UI[k] = slices.Clone(v)
		}
		c.Fragments[i] = cf
	}
	return &c
}

// Substitution: ${name} in a config value is a parameter ("${days}"), a
// value an earlier fragment published ("${frag.orders.rows}"), or, once
// jobs exist, a run value ("${run.date}"). It is text substitution — the
// value is spliced in as written — because parameters are the user's own
// values (the run dialog, --param), not data; a SQL field that needs a
// value bound rather than spliced names it in the plugin's args field.

// refRe matches one reference: a name of letters, digits, _ . and -.
var refRe = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_.\-]*)\}`)

// Refs lists the names referenced in s, in order, each once.
func Refs(s string) []string {
	var out []string
	for _, m := range refRe.FindAllStringSubmatch(s, -1) {
		if !slices.Contains(out, m[1]) {
			out = append(out, m[1])
		}
	}
	return out
}

// Subst replaces every ${name} in s with what lookup returns for name. A
// name lookup does not know is an error naming it, so a typo'd parameter
// fails the node before it runs rather than reaching the database as the
// text "${dasy}".
func Subst(s string, lookup func(name string) (string, bool)) (string, error) {
	var missing string
	out := refRe.ReplaceAllStringFunc(s, func(m string) string {
		name := m[2 : len(m)-1]
		if v, ok := lookup(name); ok {
			return v
		}
		if missing == "" {
			missing = name
		}
		return m
	})
	if missing != "" {
		return "", serr.New("unknown reference", "ref", "${"+missing+"}")
	}
	return out, nil
}

// SubstConfig is Subst over every value of cfg, into a new Config.
func SubstConfig(cfg Config, lookup func(name string) (string, bool)) (Config, error) {
	out := make(Config, len(cfg))
	for k, v := range cfg {
		s, err := Subst(v, lookup)
		if err != nil {
			return nil, serr.Wrap(err, "field", k)
		}
		out[k] = s
	}
	return out, nil
}

// nameRe is what a pipeline, fragment or node may be called: it names a
// file (the spec), a log prefix and a variable (frag.<fragment>.rows), so
// letters, digits, _ - and . only, not starting with a dot or a dash.
var nameRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.\-]{0,99}$`)

// ValidName reports whether s may name a pipeline, fragment or node.
func ValidName(s string) bool {
	return nameRe.MatchString(s) && !strings.HasSuffix(s, ".")
}
