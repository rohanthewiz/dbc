package pipeline

import (
	"strconv"
	"strings"
)

// Builder makes a Spec from Go — the pipeline API a script uses, with one
// thing the JSON cannot say: a Go func as a node (Func). A script gets it
// as sdb.NewPipeline:
//
//	p := sdb.NewPipeline("adhoc").Param("days", "1")
//	f := p.Fragment("orders").Batch(2000)
//	src := f.Node("sql.read", sdb.Cfg{"conn": "prod", "query": "SELECT … ${days} …"})
//	clean := f.Func(func(b *sdb.Batch) (*sdb.Batch, error) { …; return b, nil })
//	dst := f.Node("sql.write", sdb.Cfg{"conn": "warehouse", "table": "stg.orders", "truncate": "true"})
//	f.Wire(src, clean, dst)
//	p.Fragment("merge").Node("sql.exec", sdb.Cfg{"conn": "warehouse", "sql": "INSERT …"})
//	st, err := s.RunPipeline(p, sdb.PipelineOpts{})
//
// Then (for a linear fragment) saves the Wire: each Then is wired from
// the node added before it.
type Builder struct {
	spec Spec
}

// New starts a pipeline called name.
func New(name string) *Builder {
	return &Builder{spec: Spec{Name: name, Params: map[string]Param{}}}
}

// Desc sets the description.
func (b *Builder) Desc(d string) *Builder {
	b.spec.Desc = d
	return b
}

// Param declares a parameter with its default ("" for none) and,
// optionally, a line of doc.
func (b *Builder) Param(name, def string, doc ...string) *Builder {
	p := Param{Default: def}
	if len(doc) > 0 {
		p.Doc = strings.Join(doc, " ")
	}
	b.spec.Params[name] = p
	return b
}

// Fragment adds a fragment and returns it to fill.
func (b *Builder) Fragment(name string) *FragmentBuilder {
	b.spec.Fragments = append(b.spec.Fragments, Fragment{Name: name})
	return &FragmentBuilder{b: b, i: len(b.spec.Fragments) - 1}
}

// Spec is the pipeline built so far. It is the builder's own: Run reads
// it, and a later Fragment call extends it.
func (b *Builder) Spec() *Spec {
	if len(b.spec.Params) == 0 {
		b.spec.Params = nil
	}
	return &b.spec
}

// FragmentBuilder fills one fragment.
type FragmentBuilder struct {
	b    *Builder
	i    int
	last string // the node Then wires from
	seq  int
}

func (f *FragmentBuilder) frag() *Fragment { return &f.b.spec.Fragments[f.i] }

// Batch sets the fragment's batch size.
func (f *FragmentBuilder) Batch(n int) *FragmentBuilder {
	f.frag().Batch = n
	return f
}

// OnError sets what the fragment's failure does: "stop" (default) or "continue".
func (f *FragmentBuilder) OnError(mode string) *FragmentBuilder {
	f.frag().OnError = mode
	return f
}

// Node adds a node of plugin with cfg and returns its id, made from the
// plugin's last word ("read", then "read2").
func (f *FragmentBuilder) Node(plugin string, cfg Config) string {
	return f.Named(f.newID(plugin), plugin, cfg)
}

// Named is Node with the id chosen.
func (f *FragmentBuilder) Named(id, plugin string, cfg Config) string {
	f.frag().Nodes = append(f.frag().Nodes, Node{ID: id, Plugin: plugin, Cfg: cfg})
	f.last = id
	return id
}

// Then is Node wired from the node added last — a linear fragment reads
// as src, Then, Then.
func (f *FragmentBuilder) Then(plugin string, cfg Config) string {
	from := f.last
	id := f.Node(plugin, cfg)
	if from != "" {
		f.Wire(from, id)
	}
	return id
}

// Func adds a Go value as a node: a Source, Transform, Sink or Action, or
// a func of one of these shapes —
//
//	func(e *Env) (*Batch, error)             a source: the next batch, nil at the end
//	func(b *Batch) (*Batch, error)           a transform
//	func(e *Env, b *Batch) (*Batch, error)   a transform with the Env
//	func(e *Env, b *Batch) error             a sink (its rows are counted; nothing to commit)
//	func(e *Env) error                       an action
//
// Its id is the kind, numbered. A spec with a Func cannot be written to
// JSON or exported as a script (the func has no text); it runs.
func (f *FragmentBuilder) Func(fn any) string {
	kind, _, err := nodeKind(Node{Fn: fn})
	name := "func"
	if err == nil {
		name = string(kind)
	}
	return f.NamedFunc(f.newID(name), fn)
}

// NamedFunc is Func with the id chosen.
func (f *FragmentBuilder) NamedFunc(id string, fn any) string {
	f.frag().Nodes = append(f.frag().Nodes, Node{ID: id, Plugin: "func", Fn: fn})
	f.last = id
	return id
}

// ThenFunc is Func wired from the node added last.
func (f *FragmentBuilder) ThenFunc(fn any) string {
	from := f.last
	id := f.Func(fn)
	if from != "" {
		f.Wire(from, id)
	}
	return id
}

// Wire connects the nodes in a chain: rows flow from each to the next.
func (f *FragmentBuilder) Wire(ids ...string) *FragmentBuilder {
	for i := 1; i < len(ids); i++ {
		f.frag().Edges = append(f.frag().Edges, Edge{From: ids[i-1], To: ids[i]})
	}
	if len(ids) > 0 {
		f.last = ids[len(ids)-1]
	}
	return f
}

// newID is an id from the plugin's last word, unique in the fragment.
func (f *FragmentBuilder) newID(plugin string) string {
	base := plugin[strings.LastIndex(plugin, ".")+1:]
	if base == "" {
		base = "node"
	}
	id := base
	for n := 2; f.frag().Node(id) != nil; n++ {
		id = base + strconv.Itoa(n)
	}
	return id
}
