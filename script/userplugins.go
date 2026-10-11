package script

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/rohanthewiz/dbc/pipeline"
	"github.com/rohanthewiz/serr"
)

// User plugins: Go files in plugins_dir (~/.config/dbc/plugins), each one
// kind of pipeline node, loaded beside the built-ins. A plugin file is a
// package main with a descriptor and plain funcs — no interface to
// implement, so nothing crosses the interpreter boundary but func values
// (which yaegi hands to compiled code as ordinary Go funcs):
//
//	//go:build ignore
//
//	// Mask the e-mail column: keep the domain, hash the local part.
//	package main
//
//	import "github.com/rohanthewiz/dbc/sdb"
//
//	var Plugin = sdb.Plugin{
//		Name: "mask.email", Kind: sdb.KindTransform, Label: "Mask e-mail",
//		Fields: []sdb.Field{{Name: "column", Type: sdb.FieldString, Required: true, Doc: "…"}},
//	}
//
//	func Apply(e *sdb.Env, b *sdb.Batch) (*sdb.Batch, error) {
//		c := b.Col(e.Cfg.Str("column", ""))
//		…
//	}
//
// The funcs a kind calls, by name (* required):
//
//	source     Next*(e) (*Batch, error)   Open(e) error   Close() error | Close(ok bool) error
//	transform  Apply*(b) | Apply*(e, b) (*Batch, error)   Open(e) error   Flush(e) (*Batch, error)   Close() error
//	sink       Write*(e, b) error   Open(e, cols []Col) error   Commit(e) (Stats, error)   Abort() error
//	action     Run*(e) error | Run*(e) (Stats, error)
//	any kind   Check(cfg Cfg) []string — a config's problems, for the editor's check (no database!)
//
// A node's settings are e.Cfg: the plugin's Fields, substituted and with
// their defaults, as the built-ins get them in New.
//
// LOADING. Each file is compiled once per load, in an interpreter of its
// own, to read var Plugin and to bind and type-check the entry points —
// so a broken file is found when it is loaded, not when a run first meets
// it. Then each NODE built from the plugin gets a fresh interpreter of
// its own, compiled from the source read at load time:
//
//	load:  file ──► interpreter L ──► var Plugin, entry points checked,
//	                                  Check (if any) bound to L
//	run:   node A ──► interpreter A ──► Apply …   ┐ globals per node, and no
//	       node B ──► interpreter B ──► Apply …   ┘ interpreter shared by two runs
//
// Why a fresh interpreter per node rather than one per file: interpreted
// globals are how a plugin keeps state (a lookup map, a counter), and
// two nodes of one plugin — in one fragment, or in two runs the engine
// runs at once — must not share them; nor may two goroutines call into
// one interpreter. The compile costs milliseconds per node per run, as a
// go.transform's does. (Plan §9 sketched one compile per process; this
// is the deviation, for those two reasons.)
//
// SWAPPING. The whole user set is swapped at once (pipeline.SetUserPlugins)
// on every load, so a reader never sees half of it. SyncPlugins reloads
// only when the directory changed — a file added, removed, or written —
// so a host can call it as often as it likes: dbc web and the TUI call it
// every couple of seconds and on their own saves; the CLI once at start.
// A node already built keeps the code it was built from; a run's later
// fragments get the new one.

// pluginsMu serializes loads; lastStamp is the directory as the last load
// saw it (stampOf), lastDir the directory it was.
var (
	pluginsMu sync.Mutex
	lastDir   string
	lastStamp string
	loaded    bool
)

// SyncPlugins loads the plugin files in dir when the directory changed
// since the last load (or nothing was loaded yet, or dir is another
// directory), and reports whether it did. Cheap when nothing changed: one
// directory read and a stat per file.
func SyncPlugins(dir string) bool {
	pluginsMu.Lock()
	defer pluginsMu.Unlock()
	st := stampOf(dir)
	if loaded && dir == lastDir && st == lastStamp {
		return false
	}
	loadPluginsLocked(dir)
	lastDir, lastStamp, loaded = dir, st, true
	return true
}

// LoadPlugins loads every plugin file in dir now, whatever changed, and
// returns the files that did not load. A missing directory is no plugins
// and no problem.
func LoadPlugins(dir string) []pipeline.PluginProblem {
	pluginsMu.Lock()
	defer pluginsMu.Unlock()
	probs := loadPluginsLocked(dir)
	lastDir, lastStamp, loaded = dir, stampOf(dir), true
	return probs
}

// LoadPluginFile loads one plugin file on its own — compiled, var Plugin
// read and checked, its funcs bound — without registering it, and says
// why it cannot be a plugin, or nil. Its name is checked against the
// built-ins, not against other plugin files. For `dbc plugins --check`.
func LoadPluginFile(path string) *pipeline.PluginProblem {
	p, prob := loadPluginFile(path)
	if prob != nil {
		return prob
	}
	if have, ok := pipeline.Lookup(p.Name); ok && have.File == "" {
		return &pipeline.PluginProblem{File: path, Name: p.Name, Err: "a plugin built into dbc is called " + p.Name}
	}
	return nil
}

// pluginFiles lists dir's plugin files: *.go, no dot files, by name.
func pluginFiles(dir string) []string {
	if dir == "" {
		return nil
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		out = append(out, filepath.Join(dir, e.Name()))
	}
	sort.Strings(out)
	return out
}

// stampOf is what SyncPlugins compares: each plugin file's name, size and
// modification time. An editor's save changes the time (and usually the
// size); a file added or removed changes the list.
func stampOf(dir string) string {
	var sb strings.Builder
	for _, p := range pluginFiles(dir) {
		st, err := os.Stat(p)
		if err != nil {
			continue
		}
		fmt.Fprintf(&sb, "%s|%d|%d\n", filepath.Base(p), st.Size(), st.ModTime().UnixNano())
	}
	return sb.String()
}

func loadPluginsLocked(dir string) []pipeline.PluginProblem {
	var ps []pipeline.Plugin
	var probs []pipeline.PluginProblem
	for _, path := range pluginFiles(dir) {
		p, prob := loadPluginFile(path)
		if prob != nil {
			probs = append(probs, *prob)
			continue
		}
		ps = append(ps, p)
	}
	return pipeline.SetUserPlugins(ps, probs)
}

// loadPluginFile compiles one plugin file and makes it a pipeline.Plugin,
// or says why it cannot be one.
func loadPluginFile(path string) (p pipeline.Plugin, prob *pipeline.PluginProblem) {
	fail := func(name, format string, args ...any) (pipeline.Plugin, *pipeline.PluginProblem) {
		return pipeline.Plugin{}, &pipeline.PluginProblem{File: path, Name: name, Err: fmt.Sprintf(format, args...)}
	}
	// a panic in yaegi, or in the file's own init code, is this file's
	// problem, not the host's
	defer func() {
		if r := recover(); r != nil {
			p, prob = fail("", "the plugin panicked while loading: %v", r)
		}
	}()
	bs, err := os.ReadFile(path)
	if err != nil {
		return fail("", "%v", err)
	}
	src := string(bs)
	// the parser first: its positions are exact, and the file's doc
	// comment is the plugin's doc when var Plugin gives none
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, parser.ParseComments)
	if err != nil {
		return fail("", "%s", parseDiags(err)[0])
	}
	n, err := compilePluginSource(src)
	if err != nil {
		return fail("", "%v", err)
	}
	v, err := n.i.Eval("main.Plugin")
	if err != nil {
		return fail("", "no var Plugin: a plugin file declares var Plugin = sdb.Plugin{Name: …, Kind: …, Fields: …}")
	}
	desc, ok := v.Interface().(pipeline.Plugin)
	if !ok {
		return fail("", "var Plugin is a %s, not an sdb.Plugin", v.Type())
	}
	if msg := checkDescriptor(&desc); msg != "" {
		return fail(desc.Name, "%s", msg)
	}
	if desc.Doc == "" {
		desc.Doc = pluginDoc(f)
	}
	if desc.Doc == "" {
		desc.Doc = desc.Label
	}
	// bind the entry points once now, so a missing or mistyped func is a
	// load problem rather than every run's
	if _, err := bindPlugin(n, desc.Kind); err != nil {
		return fail(desc.Name, "%v", err)
	}
	kind := desc.Kind
	out := pipeline.Plugin{Name: desc.Name, Kind: kind, Label: desc.Label, Doc: desc.Doc, Fields: desc.Fields, File: path}
	out.New = func(pipeline.Config) (any, error) {
		// the node's own interpreter (see the file comment); its settings
		// reach it as e.Cfg, set by the runner beside New's cfg
		node, err := compilePluginSource(src)
		if err != nil {
			return nil, err
		}
		return bindPlugin(node, kind)
	}
	if chk, err := n.i.Eval("main.Check"); err == nil {
		fn, ok := chk.Interface().(func(pipeline.Config) []string)
		if !ok {
			return fail(desc.Name, "func Check has the wrong signature: want func Check(cfg sdb.Cfg) []string, got %s", chk.Type())
		}
		// Check runs in the load's interpreter, from whichever goroutine
		// asks (a web check, the TUI's): one at a time
		var mu sync.Mutex
		out.Check = func(cfg pipeline.Config) (msgs []string) {
			mu.Lock()
			defer mu.Unlock()
			defer func() {
				if r := recover(); r != nil {
					msgs = []string{fmt.Sprintf("the plugin's Check panicked: %v", r)}
				}
			}()
			return fn(cfg)
		}
	}
	return out, nil
}

// pluginDoc is a plugin file's own description, for a descriptor without
// a Doc: the first paragraph of the comment above the package clause —
// the package doc, or, written as scripts are (the comment, then
// //go:build ignore, then a blank line), the file's first comment, its
// build line left out. The rest of the comment (how to install the file,
// say) stays in the file.
func pluginDoc(f *ast.File) string {
	cg := f.Doc
	if cg == nil && len(f.Comments) > 0 && f.Comments[0].Pos() < f.Package {
		cg = f.Comments[0]
	}
	if cg == nil {
		return ""
	}
	var words []string
	for _, line := range strings.Split(cg.Text(), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "go:build") || strings.HasPrefix(line, "+build") {
			continue
		}
		if line == "" && len(words) > 0 {
			break // the first paragraph only
		}
		words = append(words, strings.Fields(line)...)
	}
	return strings.Join(words, " ")
}

// compilePluginSource evaluates a plugin file's source in a fresh
// interpreter (package-level vars and init run, as they must for var
// Plugin to have a value). Unlike a go.* snippet the file is whole: it is
// never wrapped, so positions are the file's own.
func compilePluginSource(src string) (*goNode, error) {
	i, err := newInterp()
	if err != nil {
		return nil, err
	}
	if _, err = i.Eval(src); err != nil {
		return nil, serr.New("the plugin does not compile", "at", yaegiDiag(err.Error()).String())
	}
	return &goNode{i: i, want: pluginSig}, nil
}

// checkDescriptor checks var Plugin as a plugin file wrote it, and fills
// what may be left out (the label). "" means it is usable.
func checkDescriptor(p *pipeline.Plugin) string {
	if !pipeline.ValidName(p.Name) {
		return fmt.Sprintf("Plugin.Name %q is not a plugin name: letters, digits, . _ - (say \"family.verb\": mask.email)", p.Name)
	}
	switch p.Kind {
	case pipeline.KindSource, pipeline.KindTransform, pipeline.KindSink, pipeline.KindAction:
	default:
		return fmt.Sprintf("Plugin.Kind %q is not sdb.KindSource, KindTransform, KindSink or KindAction", p.Kind)
	}
	if p.Label == "" {
		p.Label = p.Name
	}
	seen := map[string]bool{}
	for i, f := range p.Fields {
		where := fmt.Sprintf("Fields[%d]", i)
		if f.Name != "" {
			where = "field " + f.Name
		}
		switch {
		case !pipeline.ValidName(f.Name):
			return fmt.Sprintf("%s: a field needs a Name of letters, digits, . _ -", where)
		case seen[f.Name]:
			return fmt.Sprintf("%s: two fields have this name", where)
		case !slices.Contains(fieldTypes, f.Type):
			return fmt.Sprintf("%s: Type %q is not one of the sdb.Field* types", where, f.Type)
		case f.Type == pipeline.FieldEnum && len(f.Enum) == 0:
			return fmt.Sprintf("%s: an enum field lists its values in Enum", where)
		case f.Type == pipeline.FieldEnum && f.Default != "" && !slices.Contains(f.Enum, f.Default):
			return fmt.Sprintf("%s: Default %q is not one of its Enum values", where, f.Default)
		}
		seen[f.Name] = true
		if f.Doc == "" {
			p.Fields[i].Doc = f.Name
		}
	}
	return ""
}

var fieldTypes = []pipeline.FieldType{pipeline.FieldString, pipeline.FieldText, pipeline.FieldInt, pipeline.FieldBool,
	pipeline.FieldDuration, pipeline.FieldEnum, pipeline.FieldConn, pipeline.FieldTable, pipeline.FieldColumns,
	pipeline.FieldSQL, pipeline.FieldGo}

// snippetEntries is every name bindPlugin binds, over all four kinds: a
// go field's top-level func of such a name is called by dbc, so F2 does
// not rename it (rename.go fixed), as it does not rename a script's Run.
var snippetEntries = map[string]bool{
	"Next": true, "Open": true, "Close": true, "Apply": true, "Flush": true,
	"Write": true, "Commit": true, "Abort": true, "Run": true,
}

// bindPlugin looks up the entry points kind calls in n and returns the
// node: a Source, Transform, Sink or Action. A required func missing, or
// any of them with the wrong signature, is an error naming what was
// wanted. The go.* node types do the calling (and turn a panic into an
// error); a plugin file is a go.* snippet with a descriptor.
func bindPlugin(n *goNode, kind pipeline.Kind) (any, error) {
	switch kind {
	case pipeline.KindSource:
		s := &goSource{node: n}
		return s, firstErr(n.entry("Next", true, &s.next), n.entry("Open", false, &s.open),
			n.entry("Close", false, &s.close, &s.closeOK))
	case pipeline.KindTransform:
		t := &goTransform{node: n}
		return t, firstErr(n.entry("Apply", true, &t.apply1, &t.apply2), n.entry("Open", false, &t.open),
			n.entry("Flush", false, &t.flush), n.entry("Close", false, &t.close))
	case pipeline.KindSink:
		s := &goSink{node: n}
		return s, firstErr(n.entry("Write", true, &s.write), n.entry("Open", false, &s.open),
			n.entry("Commit", false, &s.commit), n.entry("Abort", false, &s.abort))
	case pipeline.KindAction:
		a := &goRunAction{node: n}
		return a, firstErr(n.entry("Run", true, &a.run1, &a.run2))
	}
	return nil, serr.New("no such plugin kind", "kind", string(kind))
}

func firstErr(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

// pluginSig spells a plugin file's entry points as the docs do (goNode's
// want for a plugin node; a go.* snippet uses wantSig).
func pluginSig(name string) string {
	switch name {
	case "Close":
		return "func Close() error, or func Close(ok bool) error (a source: ok is false when the fragment failed)"
	case "Write":
		return "func Write(e *sdb.Env, b *sdb.Batch) error"
	case "Open":
		return "func Open(e *sdb.Env) error (a sink: func Open(e *sdb.Env, cols []sdb.Col) error)"
	case "Commit":
		return "func Commit(e *sdb.Env) (sdb.Stats, error)"
	case "Abort":
		return "func Abort() error"
	case "Run":
		return "func Run(e *sdb.Env) error, or func Run(e *sdb.Env) (sdb.Stats, error)"
	}
	return wantSig(name)
}

// goSink is a sink written in Go: go.sink's snippet, or a sink plugin
// file. Only Write is required; without Commit the stats are the rows
// written, and without Abort a failure has nothing to undo — a sink that
// buffers or holds a transaction should have both.
type goSink struct {
	node   *goNode
	write  func(*pipeline.Env, *pipeline.Batch) error
	open   func(*pipeline.Env, []pipeline.Col) error
	commit func(*pipeline.Env) (pipeline.Stats, error)
	abort  func() error
	rows   int64
	done   bool // committed or aborted: Abort is then a no-op
}

func (s *goSink) Open(e *pipeline.Env, cols []pipeline.Col) error {
	if s.open == nil {
		return nil
	}
	return call("Open", func() error { return s.open(e, cols) })
}

func (s *goSink) Write(e *pipeline.Env, b *pipeline.Batch) error {
	if err := call("Write", func() error { return s.write(e, b) }); err != nil {
		return err
	}
	s.rows += int64(b.Len())
	return nil
}

func (s *goSink) Commit(e *pipeline.Env) (st pipeline.Stats, err error) {
	s.done = true
	if s.commit == nil {
		return pipeline.Stats{Rows: s.rows, Vars: map[string]string{"rows": strconv.FormatInt(s.rows, 10)}}, nil
	}
	err = call("Commit", func() error { st, err = s.commit(e); return err })
	return st, err
}

// Abort is idempotent and a no-op after Commit, as pipeline.Sink requires,
// so the plugin's own Abort is called at most once and never after its
// Commit.
func (s *goSink) Abort() error {
	if s.done {
		return nil
	}
	s.done = true
	if s.abort == nil {
		return nil
	}
	return call("Abort", s.abort)
}

// goRunAction is an action plugin file: Run with the node's Env (its
// settings in e.Cfg, the session in e.S) — unlike go.action, whose code
// is a script's func Run(s *sdb.S) error.
type goRunAction struct {
	node *goNode
	run1 func(*pipeline.Env) error
	run2 func(*pipeline.Env) (pipeline.Stats, error)
}

func (a *goRunAction) Run(e *pipeline.Env) (st pipeline.Stats, err error) {
	err = call("Run", func() error {
		if a.run2 != nil {
			st, err = a.run2(e)
		} else {
			err = a.run1(e)
		}
		return err
	})
	return st, err
}

// ─── the editor's check ─────────────────────────────────────────────────

// CheckPlugin finds what is wrong with a plugin file without running it —
// the plugin file's script.Check, for the web's markers, the TUI's check
// when $EDITOR exits and `dbc plugins --check`:
//
//  1. parse                       syntax errors (and stop)
//  2. shape, from the AST         package main; var Plugin = sdb.Plugin{…};
//     its Name and Kind when written as literals;
//     the funcs that Kind calls, with their signatures
//  3. lint                        m[k], _ = … (yaegi drops the write)
//  4. compile                     yaegi's compile pass: undefined names, types
//
// Like Check it never executes the file — not var Plugin's initializer
// either — so what it can say about the descriptor is what the source
// spells out literally; the loader, which does run it, has the last word.
func CheckPlugin(name, src string) []Diag {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, name, src, parser.ParseComments)
	if err != nil {
		return parseDiags(err)
	}
	var out []Diag
	out = append(out, checkPluginShape(fset, f)...)
	out = append(out, lintMapCommaOk(fset, f)...)
	out = append(out, lintStoreComputed(fset, f)...)
	out = append(out, compileDiags(src)...)
	slices.SortStableFunc(out, func(a, b Diag) int {
		if a.Line != b.Line {
			return a.Line - b.Line
		}
		return a.Col - b.Col
	})
	return out
}

// pluginEntries are each kind's funcs, required first, with the
// signatures accepted — written with "sdb." for the file's own name for
// the package, and parameter names left out.
var pluginEntries = map[pipeline.Kind][]struct {
	name     string
	required bool
	sigs     []string
}{
	pipeline.KindSource: {
		{"Next", true, []string{"func(*sdb.Env) (*sdb.Batch, error)"}},
		{"Open", false, []string{"func(*sdb.Env) error"}},
		{"Close", false, []string{"func() error", "func(bool) error"}},
	},
	pipeline.KindTransform: {
		{"Apply", true, []string{"func(*sdb.Batch) (*sdb.Batch, error)", "func(*sdb.Env, *sdb.Batch) (*sdb.Batch, error)"}},
		{"Open", false, []string{"func(*sdb.Env) error"}},
		{"Flush", false, []string{"func(*sdb.Env) (*sdb.Batch, error)"}},
		{"Close", false, []string{"func() error"}},
	},
	pipeline.KindSink: {
		{"Write", true, []string{"func(*sdb.Env, *sdb.Batch) error"}},
		{"Open", false, []string{"func(*sdb.Env, []sdb.Col) error"}},
		{"Commit", false, []string{"func(*sdb.Env) (sdb.Stats, error)"}},
		{"Abort", false, []string{"func() error"}},
	},
	pipeline.KindAction: {
		{"Run", true, []string{"func(*sdb.Env) error", "func(*sdb.Env) (sdb.Stats, error)"}},
	},
}

// checkPluginShape is CheckPlugin's second pass: what the loader will ask
// of the file, read off the syntax tree.
func checkPluginShape(fset *token.FileSet, f *ast.File) []Diag {
	at := func(pos token.Pos, sev, format string, args ...any) Diag {
		p := fset.Position(pos)
		return Diag{Line: p.Line, Col: p.Column, Severity: sev, Msg: fmt.Sprintf(format, args...)}
	}
	if f.Name.Name != "main" {
		return []Diag{at(f.Name.Pos(), SevError, "package %s: a plugin file is package main", f.Name.Name)}
	}
	sdbName := ""
	for _, im := range f.Imports {
		if path, _ := strconv.Unquote(im.Path.Value); path == sdbPath {
			sdbName = "sdb"
			if im.Name != nil {
				sdbName = im.Name.Name
			}
		}
	}
	// var Plugin = sdb.Plugin{…}
	var lit *ast.CompositeLit
	var litPos token.Pos
	funcs := map[string]*ast.FuncDecl{}
	for _, d := range f.Decls {
		switch d := d.(type) {
		case *ast.GenDecl:
			for _, sp := range d.Specs {
				vs, ok := sp.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, id := range vs.Names {
					if id.Name == "Plugin" && i < len(vs.Values) {
						litPos = id.Pos()
						lit, _ = vs.Values[i].(*ast.CompositeLit)
					} else if id.Name == "Plugin" {
						litPos = id.Pos()
					}
				}
			}
		case *ast.FuncDecl:
			if d.Recv == nil {
				funcs[d.Name.Name] = d
			}
		}
	}
	if litPos == token.NoPos {
		return []Diag{at(f.Package, SevError, "no var Plugin: a plugin file declares var Plugin = sdb.Plugin{Name: …, Kind: …, Fields: …}")}
	}
	var out []Diag
	if lit == nil {
		// computed, not a literal: the loader will see what it is
		return nil
	}
	var kind pipeline.Kind
	for _, el := range lit.Elts {
		kv, ok := el.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		key, _ := kv.Key.(*ast.Ident)
		if key == nil {
			continue
		}
		switch key.Name {
		case "Name":
			if bl, ok := kv.Value.(*ast.BasicLit); ok && bl.Kind == token.STRING {
				name, _ := strconv.Unquote(bl.Value)
				if !pipeline.ValidName(name) {
					out = append(out, at(bl.Pos(), SevError, "%q is not a plugin name: letters, digits, . _ -", name))
				} else if p, ok := pipeline.Lookup(name); ok && p.File == "" {
					out = append(out, at(bl.Pos(), SevError, "a plugin built into dbc is called %s: pick another name", name))
				}
			}
		case "Kind":
			// sdb.KindTransform, or the string itself
			switch v := kv.Value.(type) {
			case *ast.SelectorExpr:
				if x, ok := v.X.(*ast.Ident); ok && x.Name == sdbName {
					kind = pipeline.Kind(strings.ToLower(strings.TrimPrefix(v.Sel.Name, "Kind")))
				}
			case *ast.BasicLit:
				s, _ := strconv.Unquote(v.Value)
				kind = pipeline.Kind(s)
			}
			if _, ok := pluginEntries[kind]; !ok && kind != "" {
				out = append(out, at(kv.Value.Pos(), SevError, "Kind is sdb.KindSource, KindTransform, KindSink or KindAction"))
				kind = ""
			}
		}
	}
	if kind == "" {
		return out
	}
	for _, want := range pluginEntries[kind] {
		fn := funcs[want.name]
		if fn == nil {
			if want.required {
				out = append(out, at(litPos, SevError, "no func %s: a %s plugin's %s is what the node calls (%s)",
					want.name, kind, want.name, pluginSig(want.name)))
			}
			continue
		}
		got := funcSig(fset, fn.Type, sdbName)
		if !slices.Contains(want.sigs, got) {
			out = append(out, at(fn.Name.Pos(), SevError, "%s must be %s, not %s", want.name,
				strings.Join(want.sigs, " or "), got))
		}
	}
	return out
}

// funcSig renders a func type without parameter names, the file's name for
// sdb written as "sdb" — "func(*sdb.Env, *sdb.Batch) error" — so it can
// be compared with pluginEntries' spellings.
func funcSig(fset *token.FileSet, t *ast.FuncType, sdbName string) string {
	typ := func(e ast.Expr) string {
		var sb strings.Builder
		_ = printer.Fprint(&sb, fset, e)
		s := sb.String()
		if sdbName != "" && sdbName != "sdb" && sdbName != "." {
			s = strings.ReplaceAll(s, sdbName+".", "sdb.")
		}
		return s
	}
	list := func(fl *ast.FieldList) []string {
		var out []string
		if fl == nil {
			return nil
		}
		for _, fd := range fl.List {
			n := max(len(fd.Names), 1)
			for range n {
				out = append(out, typ(fd.Type))
			}
		}
		return out
	}
	s := "func(" + strings.Join(list(t.Params), ", ") + ")"
	switch res := list(t.Results); len(res) {
	case 0:
	case 1:
		s += " " + res[0]
	default:
		s += " (" + strings.Join(res, ", ") + ")"
	}
	return s
}
