// Package sdbapi describes the sdb package — what a script can call on
// s *sdb.S, and the fields and methods of the types it hands back — as
// data, for the script editor's completion and hover in dbc web.
//
// The description is generated from the sdb source with go/doc and
// embedded (api.json), not written by hand, so it cannot drift from the
// code: TestAPIUpToDate rebuilds it from the source and fails when the
// embedded copy differs. After changing sdb (or a type it aliases):
//
//	go generate ./sdb/sdbapi
//
// Why build time and not run time: the binary has no source to read, and
// reflection would give method names and types but none of the doc
// comments, which are most of what hover is for.
//
//	sdb/sdb.go, sdb/etl.go ── go/doc ──► S's methods, package funcs, types
//	      │ type CopyOpts = etl.CopyOptions
//	      └──── follow the alias ──► etl/*.go ── go/doc ──► its fields, methods
//	                                                          (named CopyOpts)
//	                                 ▼
//	                            api.json (embedded) ──► GET /api/v1/scripts/api
//
// Only one level is followed: CopyOpts' fields are described, but not a
// type one of those fields names (explain.Plan's Node). A script reaching
// that far has go doc.
package sdbapi

import (
	_ "embed"
	"encoding/json"
	"go/ast"
	"go/doc"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/rohanthewiz/serr"
)

//go:generate go run gen.go

// apiJSON is the generated description (see the package comment).
//
//go:embed api.json
var apiJSON []byte

// JSON is the embedded description, as GET /api/v1/scripts/api serves it.
func JSON() []byte { return apiJSON }

// Encode is api as api.json holds it: indented, so a regeneration's diff
// reads as the API change it is.
func Encode(api *API) ([]byte, error) {
	b, err := json.MarshalIndent(api, "", "  ")
	if err != nil {
		return nil, serr.Wrap(err, "op", "encode sdb api")
	}
	return append(b, '\n'), nil
}

// API is the sdb package as an editor sees it.
type API struct {
	Package string `json:"package"` // "sdb": what a script's import is called
	Doc     string `json:"doc"`
	Funcs   []Func `json:"funcs"` // package-level: sdb.IsCanceled
	Types   []Type `json:"types"`
}

// Func is a function or a method.
type Func struct {
	Name   string  `json:"name"`
	Sig    string  `json:"sig"` // as declared: "func (s *S) Query(conn, query string, args ...any) (*Result, error)"
	Doc    string  `json:"doc"`
	Params []Param `json:"params"` // one per name, so a completion can make a snippet
	// ConnArgs are the positions of the parameters that name a connection
	// (string parameters called conn, src or dst): inside those string
	// literals the editor offers connection names. Derived from the names
	// so a new method gets it without a list to update.
	ConnArgs []int `json:"connArgs,omitempty"`
}

// Param is one parameter. A variadic one's type starts with "...".
type Param struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// Type is a type the sdb package declares. For an alias of another
// package's type (CopyOpts = etl.CopyOptions), Of names the target and the
// fields and methods are the target's.
type Type struct {
	Name    string  `json:"name"`
	Of      string  `json:"of,omitempty"` // "etl.CopyOptions"
	Kind    string  `json:"kind"`         // "struct", "interface" or "other"
	Doc     string  `json:"doc"`
	Fields  []Field `json:"fields,omitempty"`
	Methods []Func  `json:"methods,omitempty"`
}

// Field is an exported struct field.
type Field struct {
	Name string `json:"name"`
	Type string `json:"type"`
	Doc  string `json:"doc"` // its doc comment, or else its line comment
}

// sdbPkg is the package described, by import path; modPrefix is the module
// it is in, so an alias's target package can be found on disk under root.
const (
	modPrefix = "github.com/rohanthewiz/dbc/"
	sdbPkg    = modPrefix + "sdb"
)

// hostOnly are the S methods a script does not call: the host builds the
// session (New), attaches the stop (WithContext), turns on the DDL log
// (LogDDL), releases what Run left open (Release) and asks afterwards
// whether the sidebar needs relisting (CatalogChanged). Offering them in
// completion would invite a script to break its own run. TestHostOnlyExist
// keeps the list in step with sdb.
var hostOnly = []string{"New", "WithContext", "LogDDL", "Release", "CatalogChanged"}

// connParams are the parameter names that mean "a connection name".
var connParams = []string{"conn", "src", "dst"}

// Build reads the sdb package under root (the module's root directory) and
// describes it. go generate calls it; so does the drift test.
func Build(root string) (*API, error) {
	l := loader{root: root, pkgs: map[string]*loaded{}}
	p, err := l.load(sdbPkg)
	if err != nil {
		return nil, err
	}
	api := &API{Package: p.doc.Name, Doc: strings.TrimSpace(p.doc.Doc)}
	for _, f := range p.doc.Funcs {
		api.Funcs = append(api.Funcs, l.fn(p, f.Decl, f.Doc))
	}
	for _, t := range p.doc.Types {
		ty, err := l.typ(p, t)
		if err != nil {
			return nil, err
		}
		api.Types = append(api.Types, ty)
	}
	return api, nil
}

// loaded is one parsed package: its docs, and each file's imports (an
// alias is resolved through the imports of the file that declares it).
type loaded struct {
	fset *token.FileSet
	doc  *doc.Package
	// imports maps a package's local name to its import path, over all of
	// the package's files. Files of one package do not disagree on a name
	// in this repo; were they to, the last one read would win.
	imports map[string]string
}

type loader struct {
	root string
	pkgs map[string]*loaded
}

// load parses the package at import path ip (one in this module), its
// non-test files only.
func (l *loader) load(ip string) (*loaded, error) {
	if p, ok := l.pkgs[ip]; ok {
		return p, nil
	}
	rel, ok := strings.CutPrefix(ip, modPrefix)
	if !ok {
		return nil, serr.New("not a package of this module", "pkg", ip)
	}
	dir := filepath.Join(l.root, filepath.FromSlash(rel))
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, serr.Wrap(err, "op", "read package", "dir", dir)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	imports := map[string]string{}
	for _, e := range ents {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, n), nil, parser.ParseComments)
		if err != nil {
			return nil, serr.Wrap(err, "op", "parse", "file", n)
		}
		if isIgnored(f) {
			continue // gen.go and other //go:build ignore files
		}
		for _, im := range f.Imports {
			path, _ := strconv.Unquote(im.Path.Value)
			name := path[strings.LastIndex(path, "/")+1:]
			if im.Name != nil {
				name = im.Name.Name
			}
			imports[name] = path
		}
		files = append(files, f)
	}
	d, err := doc.NewFromFiles(fset, files, ip)
	if err != nil {
		return nil, serr.Wrap(err, "op", "doc", "pkg", ip)
	}
	p := &loaded{fset: fset, doc: d, imports: imports}
	l.pkgs[ip] = p
	return p, nil
}

// isIgnored reports a file built only on request (//go:build ignore).
func isIgnored(f *ast.File) bool {
	for _, cg := range f.Comments {
		if cg.Pos() > f.Package {
			break
		}
		for _, c := range cg.List {
			if strings.HasPrefix(c.Text, "//go:build") && strings.Contains(c.Text, "ignore") {
				return true
			}
		}
	}
	return false
}

// typ describes t; an alias of another package's type is described by its
// target's fields and methods, under the alias's name and doc.
func (l *loader) typ(p *loaded, t *doc.Type) (Type, error) {
	spec := typeSpec(t)
	out := Type{Name: t.Name, Doc: strings.TrimSpace(t.Doc), Kind: "other"}
	if spec == nil {
		return out, nil
	}
	if sel, ok := spec.Type.(*ast.SelectorExpr); ok && spec.Assign.IsValid() {
		pkg, _ := sel.X.(*ast.Ident)
		if pkg == nil {
			return out, nil
		}
		ip, ok := p.imports[pkg.Name]
		if !ok || !strings.HasPrefix(ip, modPrefix) {
			return out, nil // an alias outside the module: name only
		}
		tp, err := l.load(ip)
		if err != nil {
			return out, err
		}
		out.Of = pkg.Name + "." + sel.Sel.Name
		for _, tt := range tp.doc.Types {
			if tt.Name == sel.Sel.Name {
				l.members(tp, tt, &out)
				break
			}
		}
		return out, nil
	}
	l.members(p, t, &out)
	return out, nil
}

// members fills in t's kind, exported fields and exported methods. S's
// host-only methods and constructors (t.Funcs: sdb.New) are left out — a
// script is handed its S, it never makes one.
func (l *loader) members(p *loaded, t *doc.Type, out *Type) {
	spec := typeSpec(t)
	if spec == nil {
		return
	}
	switch st := spec.Type.(type) {
	case *ast.StructType:
		out.Kind = "struct"
		for _, f := range st.Fields.List {
			fdoc := f.Doc.Text()
			if fdoc == "" {
				fdoc = f.Comment.Text()
			}
			for _, n := range f.Names {
				if n.IsExported() {
					out.Fields = append(out.Fields, Field{Name: n.Name, Type: l.src(p, f.Type), Doc: strings.TrimSpace(fdoc)})
				}
			}
		}
	case *ast.InterfaceType:
		out.Kind = "interface"
	}
	for _, m := range t.Methods {
		if out.Name == "S" && slices.Contains(hostOnly, m.Name) {
			continue
		}
		out.Methods = append(out.Methods, l.fn(p, m.Decl, m.Doc))
	}
}

// typeSpec is the TypeSpec of a doc.Type (one per type, as go/doc groups
// them).
func typeSpec(t *doc.Type) *ast.TypeSpec {
	for _, s := range t.Decl.Specs {
		if ts, ok := s.(*ast.TypeSpec); ok && ts.Name.Name == t.Name {
			return ts
		}
	}
	return nil
}

// fn describes a function or method declaration.
func (l *loader) fn(p *loaded, d *ast.FuncDecl, docText string) Func {
	// print the declaration without its body or comment: the signature
	head := *d
	head.Body, head.Doc = nil, nil
	f := Func{Name: d.Name.Name, Sig: l.src(p, &head), Doc: strings.TrimSpace(docText), Params: []Param{}}
	for _, fld := range d.Type.Params.List {
		ty := l.src(p, fld.Type)
		names := fld.Names
		if len(names) == 0 {
			names = []*ast.Ident{{Name: "_"}}
		}
		for _, n := range names {
			if ty == "string" && slices.Contains(connParams, n.Name) {
				f.ConnArgs = append(f.ConnArgs, len(f.Params))
			}
			f.Params = append(f.Params, Param{Name: n.Name, Type: ty})
		}
	}
	return f
}

// src prints a node as Go source, on one line.
func (l *loader) src(p *loaded, n ast.Node) string {
	var b strings.Builder
	_ = printer.Fprint(&b, p.fset, n)
	return b.String()
}
