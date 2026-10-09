package pipeline

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/rohanthewiz/dbc/etl"
	"github.com/rohanthewiz/serr"
)

// Options shape a Run.
type Options struct {
	// Params override the spec's parameter defaults. A name the spec does
	// not declare is an error; a declared parameter with no default and
	// no override is one too.
	Params map[string]string
	// Log receives each line of the run's log: the fragments starting and
	// ending, every node's Logf. Nil means the host's Print.
	Log func(line string)
	// Progress, when set, is called with a fragment's counters so far — a
	// snapshot, safe to keep — when the fragment starts (status running,
	// its nodes listed with zero counts), after every batch, and once more
	// when it ends, with its final status (succeeded, failed, canceled, or
	// skipped: not run, by Fragment or in a preview). So a host sees every
	// fragment's state change through this one callback, and its counters
	// in between. A host that draws it coalesces the batches; the runner
	// does not throttle.
	Progress func(f FragmentStats)
	// Fragment, when set, runs only the fragment of that name; the others
	// are skipped. A value an earlier fragment would have published is
	// then unknown (pass it as a param, or run them all).
	Fragment string
	// PreviewRows > 0 makes the run a preview: the source stops after
	// that many rows, every sink is replaced by a preview sink (the rows
	// go to the results view, nothing is written), and actions are
	// skipped. The way to try a transform on real data.
	PreviewRows int
	// Run is the run's own values, ${run.<name>} in any node's config:
	// the host's run id, what triggered it, the job it is a step of (see
	// RunVars). "date" and "started" default to the run's start when the
	// host does not set them, so ${run.date} works in a script's
	// s.RunPipeline as it does under the jobs engine.
	Run map[string]string
}

// RunVars are the names ${run.<name>} may use, and what each holds. Check
// refuses any other, so a typo is caught before the run rather than as an
// "unknown reference" mid-way.
var RunVars = map[string]string{
	"id":      "the run's id (20261009-020000-7f3a); empty outside the jobs engine",
	"date":    "the day the run started, YYYY-MM-DD in local time",
	"time":    "the time the run started, HH:MM:SS in local time",
	"started": "the instant the run started, RFC 3339",
	"trigger": "what started it: manual, schedule, webhook, script, cli",
	"job":     "the job this pipeline runs in; empty for a pipeline run on its own",
}

// Run runs a pipeline on h: each fragment in order, stopping at the first
// failure unless that fragment says on_error: continue. The returned
// RunStats is complete even on error (the error is in it too), so a host
// has every fragment's outcome either way. A spec that Check refuses is
// refused here with the same diags, before any fragment runs.
func Run(ctx context.Context, h Host, spec *Spec, opt Options) (*RunStats, error) {
	if ctx == nil {
		ctx = h.Ctx()
	}
	r := &runner{ctx: ctx, h: h, spec: spec, opt: opt, vars: map[string]string{}}
	r.log = opt.Log
	if r.log == nil {
		r.log = func(line string) { h.Print("%s", line) }
	}
	st := &RunStats{Pipeline: spec.Name, Status: Running, Started: time.Now(), Preview: opt.PreviewRows > 0}
	r.stats = st
	r.run = runValues(opt.Run, st.Started)
	fail := func(err error) (*RunStats, error) {
		st.Ended = time.Now()
		st.Status = Failed
		if ctx.Err() != nil || errors.Is(err, context.Canceled) {
			st.Status = Canceled
		}
		st.Error = err.Error()
		return st, err
	}
	if diags := Check(spec, CheckOptions{}); HasError(diags) {
		var msgs []string
		for _, d := range diags {
			if d.Severity == SevError {
				msgs = append(msgs, d.String())
			}
		}
		return fail(serr.New("pipeline does not check out", "pipeline", spec.Name, "problems", strings.Join(msgs, "; ")))
	}
	if err := r.resolveParams(); err != nil {
		return fail(err)
	}
	if opt.Fragment != "" && spec.Fragment(opt.Fragment) == nil {
		return fail(serr.New("no such fragment", "pipeline", spec.Name, "fragment", opt.Fragment))
	}
	for i := range spec.Fragments {
		f := &spec.Fragments[i]
		if opt.Fragment != "" && f.Name != opt.Fragment {
			fs := FragmentStats{Name: f.Name, Status: Skipped}
			st.Fragments = append(st.Fragments, fs)
			r.report(fs)
			continue
		}
		fs, err := r.runFragment(f)
		st.Fragments = append(st.Fragments, fs)
		for k, v := range fs.Vars {
			r.vars["frag."+f.Name+"."+k] = v
		}
		if err != nil {
			if f.OnError == "continue" && ctx.Err() == nil {
				r.log(fmt.Sprintf("fragment %s failed, going on (on_error: continue): %v", f.Name, err))
				continue
			}
			return fail(err)
		}
	}
	st.Ended = time.Now()
	st.Status = Succeeded
	r.log(st.String())
	return st, nil
}

type runner struct {
	ctx    context.Context
	h      Host
	spec   *Spec
	opt    Options
	params map[string]string
	vars   map[string]string
	run    map[string]string // ${run.…}: Options.Run over the defaults, keyed "run.<name>"
	stats  *RunStats
	log    func(string)
}

// runValues is the run's ${run.…} values, keyed as they are referenced:
// every name in RunVars present (empty when neither the host nor a
// default gives one, so a spec that names ${run.id} still runs from a
// script), the clock ones from the run's start.
func runValues(given map[string]string, started time.Time) map[string]string {
	t := started.Local()
	out := map[string]string{
		"run.date":    t.Format("2006-01-02"),
		"run.time":    t.Format("15:04:05"),
		"run.started": t.Format(time.RFC3339),
	}
	for name := range RunVars {
		if _, ok := out["run."+name]; !ok {
			out["run."+name] = ""
		}
	}
	for k, v := range given {
		out["run."+k] = v
	}
	return out
}

// resolveParams settles the run's parameters: the spec's defaults, then
// the overrides, each of which must be declared.
func (r *runner) resolveParams() error {
	r.params = map[string]string{}
	for name, p := range r.spec.Params {
		r.params[name] = p.Default
	}
	for name, v := range r.opt.Params {
		if _, ok := r.spec.Params[name]; !ok {
			return serr.New("not a parameter of the pipeline", "pipeline", r.spec.Name, "param", name)
		}
		r.params[name] = v
	}
	for name, p := range r.spec.Params {
		if r.params[name] == "" && p.Default == "" {
			if _, given := r.opt.Params[name]; !given {
				return serr.New("parameter has no default and was not given", "pipeline", r.spec.Name, "param", name)
			}
		}
	}
	return nil
}

// report hands a fragment's stats to the host's Progress, as a snapshot:
// the node slice and the published values are copied, so the runner's
// later counting — and its writes into Vars as the fragment ends — do not
// move what the host kept, nor race the host reading it on another
// goroutine (the jobs engine copies its records while a run goes).
func (r *runner) report(fs FragmentStats) {
	if r.opt.Progress == nil {
		return
	}
	fs.Nodes = slices.Clone(fs.Nodes)
	fs.Vars = maps.Clone(fs.Vars)
	r.opt.Progress(fs)
}

// lookup resolves a ${…} reference: a param, then a published value,
// then a run value.
func (r *runner) lookup(name string) (string, bool) {
	if v, ok := r.params[name]; ok {
		return v, true
	}
	if v, ok := r.vars[name]; ok {
		return v, true
	}
	v, ok := r.run[name]
	return v, ok
}

// nodeInst is one node, built and ready to run.
type nodeInst struct {
	id     string
	kind   Kind
	plugin string
	impl   any // Source, Transform, Sink or Action
	env    *Env
	stats  *NodeStats
	opened bool // a sink's Open was called
}

// fragRun is one fragment's run in progress.
type fragRun struct {
	r     *runner
	f     *Fragment
	stats *FragmentStats
	nodes map[string]*nodeInst
	order []string // node ids, source first, each after its parent
	stop  bool     // set through Env.Stop
}

// runFragment runs one fragment through to its sinks' commits.
//
//	build every node (substitute ${…}, validate, New)
//	action?  ──► Run it; done
//	direct?  ──► sql.read/sql.table on Postgres straight into sql.write on
//	             Postgres, nothing between: etl.Copy's COPY-to-COPY
//	otherwise:
//	   src.Open
//	   loop: b := src.Next ─► push through the tree ─► progress
//	         until nil, or a node said Stop, or ctx is done
//	   each transform's Flush, in order, pushed on likewise
//	   each sink's Commit, in node order         ◄─ all-or-nothing per sink
//	   src.Close(true)                           ◄─ the source commits last
//	on any failure: every sink's Abort, src.Close(false), the error names
//	the fragment, node and batch
func (r *runner) runFragment(f *Fragment) (FragmentStats, error) {
	if r.opt.PreviewRows > 0 {
		f = previewShape(f, r.opt.PreviewRows)
	}
	fr := &fragRun{r: r, f: f, nodes: map[string]*nodeInst{}}
	fs := FragmentStats{Name: f.Name, Status: Running, Started: time.Now(), Vars: map[string]string{}}
	fr.stats = &fs
	done := func(err error) (FragmentStats, error) {
		fs.Ended = time.Now()
		switch {
		case err == nil:
			fs.Status = Succeeded
		case r.ctx.Err() != nil || errors.Is(err, context.Canceled):
			fs.Status = Canceled
			fs.Error = err.Error()
		default:
			fs.Status = Failed
			fs.Error = err.Error()
		}
		r.report(fs) // the end, with its final status (see Options.Progress)
		return fs, err
	}
	for _, n := range f.Nodes {
		inst, err := fr.build(n)
		if err != nil {
			return done(err)
		}
		fr.nodes[n.ID] = inst
		fs.Nodes = append(fs.Nodes, NodeStats{ID: n.ID, Plugin: inst.plugin})
	}
	// the stats' node slots are what the instances count into
	for i := range fs.Nodes {
		fr.nodes[fs.Nodes[i].ID].stats = &fs.Nodes[i]
	}
	r.log(fmt.Sprintf("fragment %s: %s", f.Name, fr.describe()))
	r.report(fs) // the start: running, every node listed with nothing counted yet

	// an action
	if len(f.Nodes) == 1 && fr.nodes[f.Nodes[0].ID].kind == KindAction {
		n := fr.nodes[f.Nodes[0].ID]
		if r.opt.PreviewRows > 0 {
			fs.Status = Skipped
			fs.Ended = time.Now()
			r.log(fmt.Sprintf("fragment %s: an action; skipped in a preview", f.Name))
			r.report(fs)
			return fs, nil
		}
		t0 := time.Now()
		st, err := n.impl.(Action).Run(n.env)
		n.stats.Elapsed = time.Since(t0)
		n.stats.Batches = 1
		if err != nil {
			return done(fr.nodeErr(n, err))
		}
		n.stats.Out = st.Rows
		fs.Rows = st.Rows
		fr.publish(st)
		fs.Vars["rows"] = strconv.FormatInt(fs.Rows, 10)
		r.log(fmt.Sprintf("fragment %s done: %d rows in %s", f.Name, fs.Rows, fs.Elapsed().Round(time.Millisecond)))
		return done(nil)
	}

	roots := f.Roots()
	if len(roots) != 1 {
		return done(serr.New("a fragment has one source", "fragment", f.Name))
	}
	fr.order = f.Order(roots[0])
	src := fr.nodes[roots[0]]

	// the direct shape
	if r.opt.PreviewRows == 0 {
		if ok, err := fr.direct(src); ok || err != nil {
			if err != nil {
				return done(err)
			}
			r.log(fmt.Sprintf("fragment %s done: %d rows in %s (direct COPY)", f.Name, fs.Rows, fs.Elapsed().Round(time.Millisecond)))
			return done(nil)
		}
	}

	err := fr.stream(src)
	if err == nil {
		r.log(fmt.Sprintf("fragment %s done: %d rows in %s", f.Name, fs.Rows, fs.Elapsed().Round(time.Millisecond)))
	}
	return done(err)
}

// describe is the fragment's shape for the log: "sql.read → go.transform → sql.write, preview".
func (fr *fragRun) describe() string {
	var parts []string
	for _, n := range fr.f.Nodes {
		parts = append(parts, fr.nodes[n.ID].plugin)
	}
	return strings.Join(parts, " → ")
}

// build makes one node: its config substituted, defaulted and validated,
// then the plugin's New (or the Go func wrapped).
func (fr *fragRun) build(n Node) (*nodeInst, error) {
	kind, p, err := nodeKind(n)
	if err != nil {
		return nil, serr.Wrap(err, "fragment", fr.f.Name, "node", n.ID)
	}
	inst := &nodeInst{id: n.ID, kind: kind, plugin: p.Name}
	frag, node := fr.f.Name, n.ID
	inst.env = &Env{
		Ctx: fr.r.ctx, S: fr.r.h, Params: fr.r.params, Vars: fr.r.vars, Batch: fr.f.BatchSize(),
		Pipeline: fr.r.spec.Name, Fragment: frag, Node: node, stop: &fr.stop,
		Logf: func(format string, args ...any) {
			fr.r.log(fmt.Sprintf("[%s/%s] %s", frag, node, fmt.Sprintf(format, args...)))
		},
	}
	if n.Fn != nil {
		inst.impl = wrapFunc(n.Fn)
		return inst, nil
	}
	cfg, err := SubstConfig(n.Cfg, fr.r.lookup)
	if err != nil {
		return nil, serr.Wrap(err, "fragment", frag, "node", node)
	}
	cfg = p.Defaults(cfg)
	if msgs := p.Validate(cfg); len(msgs) > 0 {
		return nil, serr.New("bad node config", "fragment", frag, "node", node, "problems", strings.Join(msgs, "; "))
	}
	inst.impl, err = p.New(cfg)
	if err != nil {
		return nil, serr.Wrap(err, "fragment", frag, "node", node, "plugin", p.Name)
	}
	return inst, nil
}

// nodeErr names the node an error came from.
func (fr *fragRun) nodeErr(n *nodeInst, err error) error {
	if n.stats != nil {
		n.stats.Error = err.Error()
	}
	return serr.Wrap(err, "fragment", fr.f.Name, "node", n.id, "plugin", n.plugin,
		"batch", strconv.FormatInt(n.stats.Batches, 10))
}

// publish records a sink's or action's Stats on the fragment.
func (fr *fragRun) publish(st Stats) {
	for k, v := range st.Vars {
		fr.stats.Vars[k] = v
	}
	if st.Direct {
		fr.stats.Direct = true
	}
}

// stream is the batch loop of a fragment that is not an action and not
// direct.
func (fr *fragRun) stream(src *nodeInst) (err error) {
	source := src.impl.(Source)
	var transforms, sinks []*nodeInst
	for _, id := range fr.order {
		switch n := fr.nodes[id]; n.kind {
		case KindTransform:
			transforms = append(transforms, n)
		case KindSink:
			sinks = append(sinks, n)
		}
	}
	// what to undo on the way out: every sink not committed is aborted,
	// the source closed as failed, transforms closed either way
	ok := false
	srcOpen := false
	defer func() {
		if !ok {
			for _, s := range sinks {
				if s.opened {
					_ = s.impl.(Sink).Abort()
				}
			}
			if srcOpen {
				_ = source.Close(false)
			}
		}
		for _, t := range transforms {
			if t.opened {
				if cerr := t.impl.(Transform).Close(); cerr != nil {
					fr.r.log(fmt.Sprintf("[%s/%s] close: %v", fr.f.Name, t.id, cerr))
				}
			}
		}
	}()

	if err = source.Open(src.env); err != nil {
		return fr.nodeErr(src, err)
	}
	srcOpen = true
	// the source's engine, for a sink creating a table on the same one
	for _, n := range fr.nodes {
		n.env.SourceEngine = src.env.SourceEngine
	}
	for _, t := range transforms {
		if err = t.impl.(Transform).Open(t.env); err != nil {
			return fr.nodeErr(t, err)
		}
		t.opened = true
	}

	for !fr.stop {
		if err = fr.r.ctx.Err(); err != nil {
			return serr.Wrap(err, "fragment", fr.f.Name, "op", "canceled")
		}
		t0 := time.Now()
		b, nerr := source.Next(src.env)
		src.stats.Elapsed += time.Since(t0)
		if nerr != nil {
			return fr.nodeErr(src, nerr)
		}
		if b == nil {
			break
		}
		src.stats.Batches++
		src.stats.Out += int64(b.Len())
		if b.Len() > 0 {
			if err = fr.push(src, b); err != nil {
				return err
			}
		}
		fr.progress()
	}
	// flushes, upstream first, so a flushed batch runs on through the
	// transforms below it before they flush themselves
	for _, t := range transforms {
		t0 := time.Now()
		b, ferr := t.impl.(Transform).Flush(t.env)
		t.stats.Elapsed += time.Since(t0)
		if ferr != nil {
			return fr.nodeErr(t, ferr)
		}
		if b != nil && b.Len() > 0 {
			t.stats.Out += int64(b.Len())
			if err = fr.push(t, b); err != nil {
				return err
			}
		}
	}
	// a sink no batch reached: opened with the source's columns when
	// nothing between could have changed them, so its truncate and create
	// still happen (a reload from an empty source empties the table)
	for _, s := range sinks {
		if s.opened {
			continue
		}
		if cols := fr.sourceColsFor(src, s); cols != nil {
			if err = s.impl.(Sink).Open(s.env, cols); err != nil {
				return fr.nodeErr(s, err)
			}
			s.opened = true
		} else {
			fr.r.log(fmt.Sprintf("[%s/%s] no rows reached it; nothing written", fr.f.Name, s.id))
		}
	}
	var wrote, shown int64
	wroteAny := false
	for _, s := range sinks {
		if !s.opened {
			continue
		}
		t0 := time.Now()
		st, cerr := s.impl.(Sink).Commit(s.env)
		s.stats.Elapsed += time.Since(t0)
		if cerr != nil {
			return fr.nodeErr(s, cerr)
		}
		s.stats.Out = st.Rows
		if st.Shown {
			shown = max(shown, st.Rows)
		} else {
			wrote += st.Rows
			wroteAny = true
		}
		fr.publish(st)
		s.opened = false // committed: nothing to abort
	}
	// the fragment's rows are what was written; a fragment that only
	// showed rows (a preview, a report) counts what it showed
	fr.stats.Rows = wrote
	if !wroteAny {
		fr.stats.Rows = shown
	}
	fr.stats.Vars["rows"] = strconv.FormatInt(fr.stats.Rows, 10)
	ok = true
	// last, so a source that changed rows (DELETE … RETURNING) commits
	// only once every load has
	if err = source.Close(true); err != nil {
		return fr.nodeErr(src, err)
	}
	srcOpen = false
	fr.progress()
	return nil
}

// push hands b to each child of from. Every child but the last gets its
// own copy: a transform may edit a batch in place, and two consumers of
// one batch must not see each other's edits.
func (fr *fragRun) push(from *nodeInst, b *Batch) error {
	kids := fr.f.Children(from.id)
	for i, id := range kids {
		cb := b
		if i < len(kids)-1 {
			cb = b.Clone()
		}
		if err := fr.feed(fr.nodes[id], cb); err != nil {
			return err
		}
	}
	return nil
}

// feed gives one node a batch: a transform applies it and pushes what
// comes out; a sink writes it.
func (fr *fragRun) feed(n *nodeInst, b *Batch) error {
	n.stats.In += int64(b.Len())
	n.stats.Batches++
	t0 := time.Now()
	switch n.kind {
	case KindTransform:
		out, err := n.impl.(Transform).Apply(n.env, b)
		n.stats.Elapsed += time.Since(t0)
		if err != nil {
			return fr.nodeErr(n, err)
		}
		if out == nil || out.Len() == 0 {
			return nil
		}
		n.stats.Out += int64(out.Len())
		return fr.push(n, out)
	case KindSink:
		s := n.impl.(Sink)
		if !n.opened {
			if err := s.Open(n.env, b.Cols); err != nil {
				n.stats.Elapsed += time.Since(t0)
				return fr.nodeErr(n, err)
			}
			n.opened = true
		}
		err := s.Write(n.env, b)
		n.stats.Elapsed += time.Since(t0)
		if err != nil {
			return fr.nodeErr(n, err)
		}
		return nil
	}
	return serr.New("rows reached a node that takes none", "fragment", fr.f.Name, "node", n.id, "kind", string(n.kind))
}

// sourceColsFor is the source's columns when they reach sink unchanged —
// no transform on the way — and the source can say what they are; else
// nil.
func (fr *fragRun) sourceColsFor(src, sink *nodeInst) []Col {
	for id := sink.id; id != src.id; {
		id = fr.f.Parent(id)
		if id == "" || id != src.id && fr.nodes[id].kind == KindTransform {
			return nil
		}
	}
	if c, ok := src.impl.(interface{ Cols() []Col }); ok {
		return c.Cols()
	}
	return nil
}

// progress reports the fragment's counters so far.
func (fr *fragRun) progress() { fr.r.report(*fr.stats) }

// direct runs the fragment as one etl.Copy when it has the shape — a
// Postgres sql.read or sql.table straight into a Postgres sql.write — and
// reports whether it did. Both plugins say what they would do through
// directSource and directSink; a field either has that Copy cannot take
// (a setup statement, an order) makes them say no.
func (fr *fragRun) direct(src *nodeInst) (bool, error) {
	if len(fr.order) != 2 {
		return false, nil
	}
	dst := fr.nodes[fr.order[1]]
	ds, ok := src.impl.(directSource)
	if !ok {
		return false, nil
	}
	dk, ok := dst.impl.(directSink)
	if !ok {
		return false, nil
	}
	from, table, opt, ok := ds.directSource()
	if !ok {
		return false, nil
	}
	to, dest, create, truncate, batch, ok := dk.directSink()
	if !ok {
		return false, nil
	}
	fc, err := fr.r.h.ETLConn(from)
	if err != nil {
		return false, fr.nodeErr(src, err)
	}
	tc, err := fr.r.h.ETLConn(to)
	if err != nil {
		return false, fr.nodeErr(dst, err)
	}
	if fc.Engine != etl.Postgres || tc.Engine != etl.Postgres {
		return false, nil
	}
	opt.To, opt.Create, opt.Truncate, opt.BatchSize = dest, create, truncate, batch
	opt.ProgressEvery = int64(fr.f.BatchSize())
	opt.Progress = func(n int64) {
		src.stats.Out, dst.stats.In, dst.stats.Out = n, n, n
		src.stats.Batches++
		dst.stats.Batches++
		fr.progress()
	}
	src.env.SourceEngine = fc.Engine.String()
	t0 := time.Now()
	st, err := etl.Copy(fr.r.ctx, fc, table, tc, opt)
	src.stats.Elapsed, dst.stats.Elapsed = time.Since(t0), time.Since(t0)
	if err != nil {
		return true, serr.Wrap(err, "fragment", fr.f.Name, "op", "direct copy")
	}
	src.stats.Out, dst.stats.In, dst.stats.Out = st.Rows, st.Rows, st.Rows
	fr.stats.Rows = st.Rows
	fr.publish(Stats{Rows: st.Rows, Skipped: st.Skipped, Direct: st.Direct,
		Vars: map[string]string{"rows": strconv.FormatInt(st.Rows, 10)}})
	fr.stats.Direct = st.Direct
	fr.progress()
	return true, nil
}

// directSource is a source that can be the source side of an etl.Copy:
// the connection, the table (or "" with opt.Query set), and the options
// Copy needs from it (Query, Where, Columns, Args).
type directSource interface {
	directSource() (conn, table string, opt etl.CopyOptions, ok bool)
}

// directSink is a sink that can be the destination side of an etl.Copy.
type directSink interface {
	directSink() (conn, table string, create, truncate bool, batch int, ok bool)
}

// previewShape is f as a preview runs it: a rows.limit after the source,
// every sink a preview sink, and a preview sink after any transform that
// had no sink below it — so the preview shows what the last transform of
// each branch produces.
func previewShape(f *Fragment, n int) *Fragment {
	// an action fragment is skipped by the runner; nothing to reshape
	if len(f.Nodes) == 1 {
		if kind, _, err := nodeKind(f.Nodes[0]); err == nil && kind == KindAction {
			return f
		}
	}
	c := *f
	c.Nodes = slices.Clone(f.Nodes)
	c.Edges = slices.Clone(f.Edges)
	rows := strconv.Itoa(n)
	var leaves []string
	for i, node := range c.Nodes {
		kind, _, err := nodeKind(node)
		if err != nil {
			continue
		}
		switch kind {
		case KindSink:
			c.Nodes[i] = Node{ID: node.ID, Plugin: "preview", Cfg: Config{"rows": rows, "title": node.ID}}
		case KindTransform:
			if len(c.Children(node.ID)) == 0 {
				leaves = append(leaves, node.ID)
			}
		}
	}
	for _, id := range leaves {
		pid := id + ".preview"
		c.Nodes = append(c.Nodes, Node{ID: pid, Plugin: "preview", Cfg: Config{"rows": rows, "title": id}})
		c.Edges = append(c.Edges, Edge{From: id, To: pid})
	}
	if roots := c.Roots(); len(roots) == 1 {
		src := roots[0]
		// a source with no edges at all (a bare source) also gets a preview
		if len(c.Children(src)) == 0 {
			c.Nodes = append(c.Nodes, Node{ID: src + ".preview", Plugin: "preview", Cfg: Config{"rows": rows, "title": src}})
			c.Edges = append(c.Edges, Edge{From: src, To: src + ".preview"})
		}
		lim := Node{ID: "limit.preview", Plugin: "rows.limit", Cfg: Config{"rows": rows}}
		c.Nodes = append(c.Nodes, lim)
		for i := range c.Edges {
			if c.Edges[i].From == src {
				c.Edges[i].From = lim.ID
			}
		}
		c.Edges = append([]Edge{{From: src, To: lim.ID}}, c.Edges...)
	}
	return &c
}

// wrapFunc turns a Builder's Go value into a node. Interface values pass
// through; funcs get the trivial wrapper of their kind.
func wrapFunc(fn any) any {
	switch v := fn.(type) {
	case Source, Transform, Sink, Action:
		return v
	case func(*Env) (*Batch, error):
		return &funcSource{next: v}
	case func(*Batch) (*Batch, error):
		return &funcTransform{apply: func(_ *Env, b *Batch) (*Batch, error) { return v(b) }}
	case func(*Env, *Batch) (*Batch, error):
		return &funcTransform{apply: v}
	case func(*Env, *Batch) error:
		return &funcSink{write: v}
	case func(*Env) error:
		return &funcAction{run: v}
	}
	return nil
}

type funcSource struct {
	SourceBase
	next func(*Env) (*Batch, error)
}

func (s *funcSource) Next(e *Env) (*Batch, error) { return s.next(e) }

type funcTransform struct {
	TransformBase
	apply func(*Env, *Batch) (*Batch, error)
}

func (t *funcTransform) Apply(e *Env, b *Batch) (*Batch, error) { return t.apply(e, b) }

type funcSink struct {
	SinkBase
	write func(*Env, *Batch) error
	n     int64
}

func (s *funcSink) Open(*Env, []Col) error { return nil }
func (s *funcSink) Write(e *Env, b *Batch) error {
	if err := s.write(e, b); err != nil {
		return err
	}
	s.n += int64(b.Len())
	return nil
}
func (s *funcSink) Commit(*Env) (Stats, error) {
	return Stats{Rows: s.n, Vars: map[string]string{"rows": strconv.FormatInt(s.n, 10)}}, nil
}

type funcAction struct{ run func(*Env) error }

func (a *funcAction) Run(e *Env) (Stats, error) { return Stats{}, a.run(e) }
