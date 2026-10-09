package pipeline

import (
	"fmt"
	"maps"
	"reflect"
	"regexp"
	"slices"
	"strings"
)

// Diag is one problem Check found, at a place in the spec: "name",
// "params.days", "orders" (a fragment), "orders/dst" (a node),
// "orders/dst.table" (a field), "orders:edge src→dst".
type Diag struct {
	Where    string `json:"where"`
	Severity string `json:"severity"` // SevError or SevWarning
	Msg      string `json:"msg"`
}

// The two severities. An error means Run would refuse or fail; a warning
// is a spec that runs but probably does not do what was meant.
const (
	SevError   = "error"
	SevWarning = "warning"
)

func (d Diag) String() string {
	msg := d.Msg
	if d.Severity == SevWarning {
		msg = "warning: " + msg
	}
	if d.Where == "" {
		return msg
	}
	return d.Where + ": " + msg
}

// HasError reports whether any diag is an error.
func HasError(ds []Diag) bool {
	return slices.ContainsFunc(ds, func(d Diag) bool { return d.Severity == SevError })
}

// CheckOptions narrow a Check. Conns, when not nil, is the configured
// connection names: a conn field naming another is an error. Nil skips
// that check (an editor with no session; a spec meant for another
// machine).
type CheckOptions struct {
	Conns []string
}

// Check finds what is wrong with a spec without running it — the same
// rules Run applies before touching a database, so an editor can mark
// them as the user types, and `dbc pipeline check` can gate a repo:
//
//	names        pipeline, params, fragments and nodes are valid and unique
//	plugins      every node's plugin exists; its config passes Validate
//	             (required fields, ints, bools, enums, no unknown keys) and
//	             the plugin's own Check (a Go field compiles)
//	references   every ${…} is a param, an earlier fragment's value
//	             (frag.<name>.…) or a run value (run.…)
//	connections  conn fields name a configured connection (CheckOptions)
//	shape        a fragment is one action, or a tree: exactly one source,
//	             every node with at most one incoming edge, sinks at the
//	             leaves, everything reachable from the source
//
// Diags come back in spec order. A clean spec is nil.
func Check(s *Spec, opt CheckOptions) []Diag {
	c := &checker{opt: opt}
	if !ValidName(s.Name) {
		c.errorf("name", "not a pipeline name: letters, digits, . _ - (got %q)", s.Name)
	}
	known := map[string]bool{} // what ${…} may name so far
	for name := range s.Params {
		if !ValidName(name) {
			c.errorf("params."+name, "not a parameter name: letters, digits, . _ -")
		}
		known[name] = true
	}
	if len(s.Fragments) == 0 {
		c.errorf("fragments", "a pipeline needs at least one fragment")
	}
	seenFrag := map[string]bool{}
	for i := range s.Fragments {
		f := &s.Fragments[i]
		where := f.Name
		switch {
		case !ValidName(f.Name):
			where = fmt.Sprintf("fragments[%d]", i)
			c.errorf(where, "not a fragment name: letters, digits, . _ - (got %q)", f.Name)
		case seenFrag[f.Name]:
			c.errorf(where, "two fragments are called %q", f.Name)
		}
		seenFrag[f.Name] = true
		if f.Batch < 0 {
			c.errorf(where, "batch must be positive")
		}
		if f.OnError != "" && f.OnError != "stop" && f.OnError != "continue" {
			c.errorf(where, `on_error is "stop" or "continue", not %q`, f.OnError)
		}
		c.fragment(f, where, known)
		// later fragments may read what this one publishes
		known["frag."+f.Name+"."] = true
	}
	return c.out
}

type checker struct {
	opt CheckOptions
	out []Diag
}

func (c *checker) errorf(where, format string, args ...any) {
	c.out = append(c.out, Diag{Where: where, Severity: SevError, Msg: fmt.Sprintf(format, args...)})
}

func (c *checker) warnf(where, format string, args ...any) {
	c.out = append(c.out, Diag{Where: where, Severity: SevWarning, Msg: fmt.Sprintf(format, args...)})
}

// fragment checks one fragment's nodes, configs, edges and shape.
func (c *checker) fragment(f *Fragment, where string, known map[string]bool) {
	if len(f.Nodes) == 0 {
		c.errorf(where, "a fragment needs at least one node")
		return
	}
	kinds := map[string]Kind{}
	seen := map[string]bool{}
	for i := range f.Nodes {
		n := &f.Nodes[i]
		nw := where + "/" + n.ID
		switch {
		case !ValidName(n.ID):
			nw = fmt.Sprintf("%s/nodes[%d]", where, i)
			c.errorf(nw, "not a node id: letters, digits, . _ - (got %q)", n.ID)
		case seen[n.ID]:
			c.errorf(nw, "two nodes are called %q", n.ID)
		}
		seen[n.ID] = true
		kind, p, err := nodeKind(*n)
		if err != nil {
			c.errorf(nw, "%s", err)
			continue
		}
		kinds[n.ID] = kind
		if n.Fn != nil {
			continue // a Go func has no config to check
		}
		for _, msg := range p.Validate(n.Cfg) {
			c.errorf(nw, "%s", msg)
		}
		for k, v := range n.Cfg {
			for _, ref := range Refs(v) {
				if !refKnown(ref, known) {
					if strings.HasPrefix(ref, "run.") {
						c.errorf(nw+"."+k, "${%s} is not a run value (%s)", ref, runVarNames())
						continue
					}
					c.errorf(nw+"."+k, "${%s} is not a parameter or an earlier fragment's value", ref)
				}
			}
			if fd, ok := p.Field(k); ok && fd.Type == FieldConn && c.opt.Conns != nil &&
				strings.TrimSpace(v) != "" && len(Refs(v)) == 0 && !slices.Contains(c.opt.Conns, strings.TrimSpace(v)) {
				c.errorf(nw+"."+k, "no connection called %q", strings.TrimSpace(v))
			}
		}
		if p.Check != nil {
			for _, msg := range p.Check(p.Defaults(n.Cfg)) {
				if checkWarning(msg) {
					c.warnf(nw, "%s", msg)
				} else {
					c.errorf(nw, "%s", msg)
				}
			}
		}
	}
	// edges name nodes, once, and never a node to itself
	for _, e := range f.Edges {
		ew := fmt.Sprintf("%s:edge %s→%s", where, e.From, e.To)
		if !seen[e.From] || !seen[e.To] {
			c.errorf(ew, "names a node the fragment does not have")
			continue
		}
		if e.From == e.To {
			c.errorf(ew, "a node cannot feed itself")
		}
	}
	// shape
	var sources, sinks, actions []string
	for _, n := range f.Nodes {
		switch kinds[n.ID] {
		case KindSource:
			sources = append(sources, n.ID)
		case KindSink:
			sinks = append(sinks, n.ID)
		case KindAction:
			actions = append(actions, n.ID)
		}
	}
	if len(actions) > 0 {
		if len(f.Nodes) > 1 {
			c.errorf(where, "an action (%s) is a fragment of its own: no other nodes beside it", actions[0])
		}
		if len(f.Edges) > 0 {
			c.errorf(where, "an action has no edges")
		}
		return
	}
	switch len(sources) {
	case 0:
		c.errorf(where, "a fragment needs a source (or a single action)")
		return
	case 1:
	default:
		c.errorf(where, "one source per fragment, not %d (%s)", len(sources), strings.Join(sources, ", "))
	}
	for _, n := range f.Nodes {
		parents := 0
		for _, e := range f.Edges {
			if e.To == n.ID {
				parents++
			}
		}
		nw := where + "/" + n.ID
		switch {
		case kinds[n.ID] == KindSource && parents > 0:
			c.errorf(nw, "a source has no input")
		case kinds[n.ID] != KindSource && parents > 1:
			c.errorf(nw, "a node takes one input, not %d", parents)
		case kinds[n.ID] == KindSink && len(f.Children(n.ID)) > 0:
			c.errorf(nw, "a sink has no output")
		}
	}
	if len(sources) == 1 {
		reach := f.Order(sources[0])
		for _, n := range f.Nodes {
			if !slices.Contains(reach, n.ID) {
				c.errorf(where+"/"+n.ID, "not connected to the source %s", sources[0])
			}
		}
	}
	if len(sinks) == 0 {
		c.warnf(where, "no sink: the rows go nowhere")
	}
}

// warningRe is a plugin Check message that is a warning: "warning: …",
// or the same after the place it points at ("code:3:1: warning: …", as
// script's checks of a Go field write them).
var warningRe = regexp.MustCompile(`^(?:[\w.]+(?::\d+){0,2}: )?warning: `)

// checkWarning reports whether a plugin Check message is a warning — legal
// but probably not what was meant — rather than an error, which would
// keep the pipeline from running.
func checkWarning(msg string) bool { return warningRe.MatchString(msg) }

// refKnown reports whether ${ref} may be resolved: a param, one of the
// RunVars as "run.<name>", or "frag.<earlier fragment>.<anything>".
func refKnown(ref string, known map[string]bool) bool {
	if known[ref] {
		return true
	}
	if name, ok := strings.CutPrefix(ref, "run."); ok {
		_, ok = RunVars[name]
		return ok
	}
	if strings.HasPrefix(ref, "frag.") {
		rest := strings.TrimPrefix(ref, "frag.")
		if i := strings.Index(rest, "."); i > 0 {
			return known["frag."+rest[:i]+"."]
		}
	}
	return false
}

// runVarNames lists the run values for a message: "run.date, run.id, …".
func runVarNames() string {
	var names []string
	for _, n := range slices.Sorted(maps.Keys(RunVars)) {
		names = append(names, "run."+n)
	}
	return strings.Join(names, ", ")
}

// nodeKind is what a node builds: by its plugin, or for a Builder's Go
// func by the func's shape. An interface value (a Source made in Go) is
// taken as it is.
func nodeKind(n Node) (Kind, Plugin, error) {
	if n.Fn == nil {
		p, ok := Lookup(n.Plugin)
		if !ok {
			if n.Plugin == "" {
				return "", Plugin{}, fmt.Errorf("no plugin named")
			}
			// a user plugin whose file is broken says so, and why — "no
			// plugin" would send the user looking for a typo instead
			if pr, broken := problemFor(n.Plugin); broken {
				return "", Plugin{}, fmt.Errorf("plugin %q did not load from %s: %s", n.Plugin, pr.File, pr.Err)
			}
			return "", Plugin{}, fmt.Errorf("no plugin %q (dbc plugins lists them)", n.Plugin)
		}
		return p.Kind, p, nil
	}
	switch n.Fn.(type) {
	case Source, func(*Env) (*Batch, error):
		return KindSource, Plugin{Name: "func.source", Kind: KindSource}, nil
	case Transform, func(*Batch) (*Batch, error), func(*Env, *Batch) (*Batch, error):
		return KindTransform, Plugin{Name: "func.transform", Kind: KindTransform}, nil
	case Sink, func(*Env, *Batch) error:
		return KindSink, Plugin{Name: "func.sink", Kind: KindSink}, nil
	case Action, func(*Env) error:
		return KindAction, Plugin{Name: "func.action", Kind: KindAction}, nil
	}
	return "", Plugin{}, fmt.Errorf("a Go node is a Source, Transform, Sink or Action, or a func of one of these shapes: "+
		"func(*Env) (*Batch, error), func(*Batch) (*Batch, error), func(*Env, *Batch) (*Batch, error), "+
		"func(*Env, *Batch) error, func(*Env) error — not %s", reflect.TypeOf(n.Fn))
}
