package script

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"github.com/traefik/yaegi/interp"

	"github.com/rohanthewiz/dbc/pipeline"
	"github.com/rohanthewiz/dbc/scripts"
	"github.com/rohanthewiz/dbc/sdb"
	"github.com/rohanthewiz/serr"
)

// The Go-code plugins: a node whose behaviour is Go in its config,
// interpreted by the engine scripts use, and a node that runs a script.
// They live here, not in package pipeline, because they need the
// interpreter (newInterp), which needs sdb, which imports pipeline.
//
//	go.transform   func Apply(b *sdb.Batch) (*sdb.Batch, error)   per batch
//	go.source      func Next(e *sdb.Env) (*sdb.Batch, error)       until it returns nil
//	go.action      func Run(s *sdb.S) error                        a script's own shape
//	script.run     a saved script by name                          a script as a fragment
//
// The code is compiled once per run — a fresh interpreter, the snippet
// evaluated, the entry points looked up by name and kept as func values
// — and called once per BATCH from then on. Crossing from compiled code
// into the interpreter costs a reflective call; per batch, that cost is
// divided by the batch size, which is what makes interpreted Go viable
// for a load of any size (the row loop inside Apply is interpreted too,
// and that part is simply slower than compiled Go: see the benchmark in
// plugins_bench_test.go and the number in the README).
//
// A snippet need not be a whole file. Without a package clause it is
// wrapped (WrapSnippet): package main, and an import for every standard
// package it uses by name from a known list, plus sdb — so a three-line
// Apply needs no boilerplate. With a package clause it is used as it is.

func init() {
	pipeline.Register(pipeline.Plugin{
		Name: "go.transform", Kind: pipeline.KindTransform, Label: "Go transform",
		Doc: "Reshapes each batch with Go: func Apply(b *sdb.Batch) (*sdb.Batch, error), which may edit b in place and " +
			"return it, return another batch (other columns), or nil to drop the batch. Optional: " +
			"func Open(e *sdb.Env) error before the first batch, func Flush(e *sdb.Env) (*sdb.Batch, error) after the last " +
			"(rows held back, say an aggregation), func Close() error at the end. Apply may also take the Env first: " +
			"func Apply(e *sdb.Env, b *sdb.Batch) (*sdb.Batch, error) — e.S is the session (e.S.Query, e.S.Print), " +
			"e.Params the parameters. Without a package clause the code is wrapped: standard packages it names are imported for it.",
		Fields: []pipeline.Field{
			{Name: "code", Type: pipeline.FieldGo, Required: true, Doc: "The Go code, with func Apply."},
		},
		New: func(cfg pipeline.Config) (any, error) {
			n, err := newGoNode(cfg["code"])
			if err != nil {
				return nil, err
			}
			t := &goTransform{node: n}
			if err := n.entry("Apply", true, &t.apply1, &t.apply2); err != nil {
				return nil, err
			}
			_ = n.entry("Open", false, &t.open)
			_ = n.entry("Flush", false, &t.flush)
			_ = n.entry("Close", false, &t.close)
			return t, nil
		},
		Check: func(cfg pipeline.Config) []string { return checkSnippet(cfg["code"], "Apply") },
	})
	pipeline.Register(pipeline.Plugin{
		Name: "go.source", Kind: pipeline.KindSource, Label: "Go source",
		Doc: "Makes the rows with Go: func Next(e *sdb.Env) (*sdb.Batch, error), called until it returns nil — " +
			"generate rows, read a file with os, call an API with net/http, anything the standard library can. " +
			"e.Batch is the batch size wanted. Optional: func Open(e *sdb.Env) error, func Close() error. " +
			"Columns come from the first batch: sdb.NewBatch(cols, rows) or &sdb.Batch{Cols: …, Rows: …}.",
		Fields: []pipeline.Field{
			{Name: "code", Type: pipeline.FieldGo, Required: true, Doc: "The Go code, with func Next."},
		},
		New: func(cfg pipeline.Config) (any, error) {
			n, err := newGoNode(cfg["code"])
			if err != nil {
				return nil, err
			}
			s := &goSource{node: n}
			if err := n.entry("Next", true, &s.next); err != nil {
				return nil, err
			}
			_ = n.entry("Open", false, &s.open)
			_ = n.entry("Close", false, &s.close)
			return s, nil
		},
		Check: func(cfg pipeline.Config) []string { return checkSnippet(cfg["code"], "Next") },
	})
	pipeline.Register(pipeline.Plugin{
		Name: "go.action", Kind: pipeline.KindAction, Label: "Go action",
		Doc: "A fragment that is a script: func Run(s *sdb.S) error, with everything a script can do " +
			"(s.Query, s.Exec, s.Copy, s.Export, s.Print). Rows it writes it commits itself.",
		Fields: []pipeline.Field{
			{Name: "code", Type: pipeline.FieldGo, Required: true, Doc: "The Go code, with func Run."},
		},
		New: func(cfg pipeline.Config) (any, error) {
			return &goAction{code: cfg["code"]}, nil
		},
		Check: func(cfg pipeline.Config) []string { return checkSnippet(cfg["code"], "Run") },
	})
	pipeline.Register(pipeline.Plugin{
		Name: "script.run", Kind: pipeline.KindAction, Label: "Run a script",
		Doc: "Runs a saved dbc script as a fragment: a file path, a name in the scripts directory (.go optional), " +
			"or a built-in example. Every script written so far is a valid fragment.",
		Fields: []pipeline.Field{
			{Name: "name", Type: pipeline.FieldString, Required: true, Doc: "The script: a path, a name, or an example."},
		},
		New: func(cfg pipeline.Config) (any, error) {
			return &scriptRun{name: strings.TrimSpace(cfg["name"])}, nil
		},
	})
}

// knownImports are the standard packages a wrapped snippet may use
// without an import of its own, by the name it uses them under.
var knownImports = map[string]string{
	"bytes": "bytes", "errors": "errors", "fmt": "fmt", "io": "io", "json": "encoding/json",
	"csv": "encoding/csv", "hex": "encoding/hex", "base64": "encoding/base64", "math": "math",
	"rand": "math/rand", "os": "os", "regexp": "regexp", "slices": "slices", "sort": "sort",
	"strconv": "strconv", "strings": "strings", "time": "time", "unicode": "unicode",
	"utf8": "unicode/utf8", "maps": "maps", "http": "net/http", "url": "net/url",
	"filepath": "path/filepath", "bufio": "bufio", "sha256": "crypto/sha256", "md5": "crypto/md5",
	"sdb": sdbPath,
}

// WrapSnippet makes a whole script of a snippet: with a package clause it
// is returned as it is (and headerLines is 0); without one, "package
// main" and an import block of the known packages it names go before it,
// and headerLines says how many lines that added, so a diag's line can be
// mapped back to the snippet's. A snippet that does not parse gets every
// known import, and the compile pass says what is wrong.
func WrapSnippet(code string) (src string, headerLines int) {
	trimmed := strings.TrimLeft(code, "\n\r\t ")
	if hasPackageClause(trimmed) {
		return code, 0
	}
	used := map[string]bool{"sdb": true}
	f, err := parser.ParseFile(token.NewFileSet(), "", "package main\n"+code, 0)
	if err == nil {
		ast.Inspect(f, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok {
				if id, ok := sel.X.(*ast.Ident); ok && knownImports[id.Name] != "" {
					used[id.Name] = true
				}
			}
			return true
		})
	} else {
		for name := range knownImports {
			used[name] = true
		}
	}
	names := make([]string, 0, len(used))
	for name := range used {
		names = append(names, name)
	}
	sort.Strings(names)
	var sb strings.Builder
	sb.WriteString("package main\n\nimport (\n")
	for _, name := range names {
		path := knownImports[name]
		if path[strings.LastIndex(path, "/")+1:] == name {
			fmt.Fprintf(&sb, "\t%q\n", path)
		} else {
			fmt.Fprintf(&sb, "\t%s %q\n", name, path)
		}
	}
	sb.WriteString(")\n\n")
	header := sb.String()
	return header + code, strings.Count(header, "\n")
}

// hasPackageClause reports whether code starts (after comments and build
// lines) with a package clause.
func hasPackageClause(code string) bool {
	f, err := parser.ParseFile(token.NewFileSet(), "", code, parser.PackageClauseOnly)
	return err == nil && f != nil && f.Name != nil
}

// checkSnippet is the Check of a go.* field: the script checks, less the
// Run signature, plus the presence of the entry point. Messages name the
// snippet's own lines.
func checkSnippet(code, entry string) []string {
	src, header := WrapSnippet(code)
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "code", src, parser.ParseComments)
	if err != nil {
		var out []string
		for _, d := range parseDiags(err) {
			out = append(out, "code:"+shiftDiag(d, header).String())
		}
		return out
	}
	var out []string
	found := false
	for _, d := range f.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Name.Name == entry {
			found = true
		}
	}
	if !found {
		out = append(out, fmt.Sprintf("code: no func %s: it is what the node calls", entry))
	}
	for _, d := range lintMapCommaOk(fset, f) {
		out = append(out, "code:"+shiftDiag(d, header).String())
	}
	for _, d := range compileDiags(src) {
		out = append(out, "code:"+shiftDiag(d, header).String())
	}
	return out
}

// shiftDiag maps a diag on the wrapped source back onto the snippet.
func shiftDiag(d Diag, header int) Diag {
	if d.Line > header {
		d.Line -= header
	} else if header > 0 {
		d.Line = 1
	}
	return d
}

// goNode is a compiled snippet: its interpreter, kept for the run.
type goNode struct {
	i *interp.Interpreter
}

// newGoNode compiles code in a fresh interpreter.
func newGoNode(code string) (*goNode, error) {
	src, header := WrapSnippet(code)
	i, err := newInterp()
	if err != nil {
		return nil, err
	}
	if _, err = i.Eval(src); err != nil {
		d := shiftDiag(yaegiDiag(err.Error()), header)
		return nil, serr.New("the Go code does not compile", "at", d.String())
	}
	return &goNode{i: i}, nil
}

// entry looks up main.<name> and stores it in the first of dsts (pointers
// to func types) whose type it has. required makes a missing func an
// error; a present func of the wrong shape always is.
func (n *goNode) entry(name string, required bool, dsts ...any) error {
	v, err := n.i.Eval("main." + name)
	if err != nil {
		if required {
			return serr.New("the Go code has no func "+name, "want", wantSig(name))
		}
		return nil
	}
	for _, dst := range dsts {
		dv := reflect.ValueOf(dst).Elem()
		if v.Type().AssignableTo(dv.Type()) {
			dv.Set(v)
			return nil
		}
	}
	return serr.New("func "+name+" has the wrong signature", "want", wantSig(name), "got", v.Type().String())
}

// wantSig spells the entry points as the docs do.
func wantSig(name string) string {
	switch name {
	case "Apply":
		return "func Apply(b *sdb.Batch) (*sdb.Batch, error), or func Apply(e *sdb.Env, b *sdb.Batch) (*sdb.Batch, error)"
	case "Next":
		return "func Next(e *sdb.Env) (*sdb.Batch, error)"
	case "Open":
		return "func Open(e *sdb.Env) error"
	case "Flush":
		return "func Flush(e *sdb.Env) (*sdb.Batch, error)"
	case "Close":
		return "func Close() error"
	}
	return wantRun
}

// call runs fn, turning a panic in the interpreted code into an error.
func call(what string, fn func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = serr.New(fmt.Sprintf("the Go code panicked in %s: %v", what, r))
		}
	}()
	return fn()
}

type goTransform struct {
	node   *goNode
	apply1 func(*pipeline.Batch) (*pipeline.Batch, error)
	apply2 func(*pipeline.Env, *pipeline.Batch) (*pipeline.Batch, error)
	open   func(*pipeline.Env) error
	flush  func(*pipeline.Env) (*pipeline.Batch, error)
	close  func() error
}

func (t *goTransform) Open(e *pipeline.Env) error {
	if t.open == nil {
		return nil
	}
	return call("Open", func() error { return t.open(e) })
}

func (t *goTransform) Apply(e *pipeline.Env, b *pipeline.Batch) (out *pipeline.Batch, err error) {
	err = call("Apply", func() error {
		if t.apply2 != nil {
			out, err = t.apply2(e, b)
		} else {
			out, err = t.apply1(b)
		}
		return err
	})
	return out, err
}

func (t *goTransform) Flush(e *pipeline.Env) (out *pipeline.Batch, err error) {
	if t.flush == nil {
		return nil, nil
	}
	err = call("Flush", func() error { out, err = t.flush(e); return err })
	return out, err
}

func (t *goTransform) Close() error {
	if t.close == nil {
		return nil
	}
	return call("Close", t.close)
}

type goSource struct {
	node  *goNode
	next  func(*pipeline.Env) (*pipeline.Batch, error)
	open  func(*pipeline.Env) error
	close func() error
}

func (s *goSource) Open(e *pipeline.Env) error {
	if s.open == nil {
		return nil
	}
	return call("Open", func() error { return s.open(e) })
}

func (s *goSource) Next(e *pipeline.Env) (out *pipeline.Batch, err error) {
	err = call("Next", func() error { out, err = s.next(e); return err })
	return out, err
}

func (s *goSource) Close(bool) error {
	if s.close == nil {
		return nil
	}
	return call("Close", s.close)
}

// goAction runs its code as a script would run: through RunSource, with
// the session itself (the Host must be an *sdb.S).
type goAction struct {
	code string
}

func (a *goAction) Run(e *pipeline.Env) (pipeline.Stats, error) {
	s, ok := e.S.(*sdb.S)
	if !ok {
		return pipeline.Stats{}, serr.New("go.action needs a script session (sdb.S) to run in")
	}
	src, header := WrapSnippet(a.code)
	if err := RunSource(e.Where(), src, s); err != nil {
		if header > 0 {
			// the error's positions are on the wrapped source
			err = serr.Wrap(err, "note", fmt.Sprintf("line numbers are %d more than the code's", header))
		}
		return pipeline.Stats{}, err
	}
	return pipeline.Stats{}, nil
}

// scriptRun runs a saved script: a path, a name in the session's scripts
// directory, or an example — the lookup config.FindScript does, without
// config (sdb cannot import it).
type scriptRun struct {
	name string
}

func (a *scriptRun) Run(e *pipeline.Env) (pipeline.Stats, error) {
	s, ok := e.S.(*sdb.S)
	if !ok {
		return pipeline.Stats{}, serr.New("script.run needs a script session (sdb.S) to run in")
	}
	if st, err := os.Stat(a.name); err == nil && !st.IsDir() {
		return pipeline.Stats{}, Run(a.name, s)
	}
	if !strings.ContainsAny(a.name, `/\`) {
		names := []string{a.name}
		if !strings.HasSuffix(a.name, ".go") {
			names = append(names, a.name+".go")
		}
		if dir := s.Paths().ScriptsDir; dir != "" {
			for _, n := range names {
				p := filepath.Join(dir, n)
				if st, err := os.Stat(p); err == nil && !st.IsDir() {
					return pipeline.Stats{}, Run(p, s)
				}
			}
		}
		for _, n := range names {
			if ex, ok := scripts.ExampleByName(n); ok {
				e.Logf("running the built-in example %s", ex.Name)
				return pipeline.Stats{}, RunSource("example:"+ex.Name, ex.Text, s)
			}
		}
	}
	return pipeline.Stats{}, serr.New("no such script", "name", a.name, "dir", s.Paths().ScriptsDir)
}
