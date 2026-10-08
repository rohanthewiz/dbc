package script

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"regexp"
	"slices"
	"strings"
)

// Go to definition and usages in a script: the name under the caret, where
// the script declares it, and every place the script uses it. The script
// editor's F12 / Ctrl+click and Shift+F12 (web/scripts.go, scripts.js) ask
// here, as the SQL editor's ask sqlcomplete.Resolve. F2's rename
// (rename.go) starts from the same answer.
//
//	src, caret ─► go/parser ─► go/types (imports faked, errors ignored)
//	                              │
//	          ident at the caret ─┴► its types.Object ─► every ident with that
//	                                 object (Info.Defs + Info.Uses), in order
//
// WHY go/types AND NOT A TEXT SEARCH. Scripts shadow freely — err in every
// block, a loop's i and the next loop's i, a param named like a package —
// and a search by name would merge them. The type checker binds each ident
// to the object it really means, scope by scope, and also reaches through
// selectors and composite-literal keys to a field or method the script
// declares itself (row.Total, Totals{Sum: 1}).
//
// WHY THE IMPORTS ARE FAKED. A real importer needs the standard library's
// source or export data on the machine running dbc, which an installed
// binary cannot count on, and package sdb is not a package go/types can load
// from inside the binary at all. So every import is an empty, complete
// package under the right name: sdb.S, fmt.Sprintf and friends are
// "undefined", a variable of such a type has an invalid type, and the
// checker records those errors and carries on (Config.Error swallows them).
// What it binds is exactly what the script itself declares — which is all
// go to definition could point at anyway: nothing outside the script is in
// the editor.
//
// WHAT AN IMPORTED MEMBER GETS. s.Query, res.Rows and sdb.CopyOpts stay
// unresolved under fake imports, so they have no declaration to jump to,
// but their usages are still worth listing. They are matched by shape: the
// same name selected from the same variable or package (every s.Query of
// the one s, every sdb.Copy) — narrower than by type, which is unknown, but
// never wrong about which s is meant.
//
// The checker is the same code `go vet` trusts, run over a file of a few
// hundred lines: well under a millisecond, so the editor asks on every
// F12 rather than keeping an index that would go stale as the user types.

// Span is a byte range of the script, [From, To).
type Span struct {
	From int `json:"from"`
	To   int `json:"to"`
}

// Symbol is what the caret is on. Kind "" means nothing that resolves (a
// keyword, a literal, white space): the editor has nowhere to go. Def is nil
// when the declaration is not in the script (a builtin such as len or
// error, or a member of an imported package); Uses then still lists every
// occurrence. Uses includes the declaration itself, in buffer order.
type Symbol struct {
	Kind string `json:"kind"` // var, const, type, func, method, field, package, label, builtin
	Name string `json:"name"`
	At   Span   `json:"at"`  // the occurrence under the caret
	Def  *Span  `json:"def"` // nil: declared outside the script
	Uses []Span `json:"uses"`
	// Fixed says why F2 cannot rename it; "" when it can (rename.go).
	Fixed string `json:"fixed,omitempty"`
}

// Resolve finds the symbol at byte offset caret of src. A caret just past a
// name (where it rests after typing one) counts as on it.
//
// A script with syntax errors still resolves: go/parser hands back the tree
// it could build, and the checker binds what is in it. A panic deep in the
// checker over odd input is a symbol with no kind, never a crash of the
// server that asked.
func Resolve(src string, caret int) (sym Symbol) {
	defer func() {
		if r := recover(); r != nil {
			sym = Symbol{Uses: []Span{}}
		}
	}()
	c := checkSrc(src)
	if c == nil {
		return Symbol{Uses: []Span{}}
	}
	sym, _ = c.resolve(caret)
	return sym
}

// checked is a script parsed and type-checked once, for Resolve and for
// Rename (which checks twice: the script, then the script renamed).
type checked struct {
	fset *token.FileSet
	f    *ast.File
	tf   *token.File
	pkg  *types.Package
	info *types.Info
	errs []types.Error // every error the checker reported, in order

	// a type switch's per-clause objects folded onto one; see key
	alias    map[types.Object]types.Object
	tsIdents map[*ast.Ident]types.Object
}

// checkSrc parses and checks src; nil when the parser could build nothing.
func checkSrc(src string) *checked {
	fset := token.NewFileSet()
	// SkipObjectResolution: go/types resolves scopes itself; the parser's
	// older, deprecated ast.Object pass would only be thrown away
	f, _ := parser.ParseFile(fset, "script.go", src, parser.AllErrors|parser.SkipObjectResolution)
	if f == nil {
		return nil
	}
	c := &checked{fset: fset, f: f, tf: fset.File(f.Pos())}
	c.info = &types.Info{
		Defs:      map[*ast.Ident]types.Object{},
		Uses:      map[*ast.Ident]types.Object{},
		Implicits: map[ast.Node]types.Object{},
		Scopes:    map[ast.Node]*types.Scope{},
	}
	conf := types.Config{
		Importer: fakeImporter{},
		// keep going past every error (see WHY THE IMPORTS ARE FAKED);
		// Rename compares the list before and after
		Error: func(err error) {
			if te, ok := err.(types.Error); ok {
				c.errs = append(c.errs, te)
			}
		},
	}
	c.pkg, _ = conf.Check("main", fset, []*ast.File{f}, c.info)
	c.alias, c.tsIdents = typeSwitchAliases(f, c.info)
	return c
}

// off is p as a byte offset of the script.
func (c *checked) off(p token.Pos) int { return c.tf.Offset(p) }

// line is the 1-based line of byte offset off, for messages.
func (c *checked) line(off int) int { return c.tf.Line(c.tf.Pos(off)) }

// key is the object ident i means, nil for none.
//
// A type switch's x in `switch x := v.(type)` declares no object of its
// own: each case clause gets an implicit one, so x in one clause and x in
// the next are different objects to the checker. To the reader they are
// one name, so every clause's object, and the switch's ident itself, are
// folded onto one representative before comparing.
func (c *checked) key(i *ast.Ident) types.Object {
	if rep, ok := c.tsIdents[i]; ok {
		return rep
	}
	o := c.info.Defs[i]
	if o == nil {
		o = c.info.Uses[i]
	}
	if rep, ok := c.alias[o]; ok {
		return rep
	}
	return o
}

// resolve is Resolve over a checked script, also handing back the object
// the caret is on (nil for none, or an imported member) for Rename.
func (c *checked) resolve(caret int) (Symbol, types.Object) {
	sym := Symbol{Uses: []Span{}}
	id := identAt(c.f, caret, c.off)
	if id == nil || id.Name == "_" {
		return sym, nil
	}
	at := Span{c.off(id.Pos()), c.off(id.End())}

	obj := c.key(id)
	if obj == nil {
		// unresolved: an imported package's member, selected from a
		// variable or the package name (see WHAT AN IMPORTED MEMBER GETS)
		sym = selectorUses(c.f, c.info, id, at, c.off)
		if sym.Kind != "" {
			sym.Fixed = sym.Name + " belongs to an imported package: renaming it here would not rename it there"
		}
		return sym, nil
	}

	sym.Name, sym.At, sym.Kind = id.Name, at, kindOf(obj)
	// every ident bound to the same object, declaration included. An
	// ident can be in both maps (an embedded field defines the field and
	// uses the type), so uses are deduped by offset.
	seen := map[int]bool{}
	ast.Inspect(c.f, func(n ast.Node) bool {
		i, ok := n.(*ast.Ident)
		if !ok || c.key(i) != obj || seen[c.off(i.Pos())] {
			return true
		}
		seen[c.off(i.Pos())] = true
		sym.Uses = append(sym.Uses, Span{c.off(i.Pos()), c.off(i.End())})
		return true
	})
	sym.Def = declSpan(c.f, obj, c.tf, c.off)
	if sym.Def != nil && !seen[sym.Def.From] {
		// an unnamed import is declared by its path literal, which no
		// ident covers: it joins the list beside the uses
		sym.Uses = append(sym.Uses, *sym.Def)
	}
	slices.SortFunc(sym.Uses, func(a, b Span) int { return a.From - b.From })
	sym.Fixed = c.fixed(obj, sym)
	return sym, obj
}

// identAt is the identifier at byte offset caret: one the caret is inside
// (or at the start of), else one it rests just after — so `foo▮.Bar` is
// foo, and `foo▮)` is still foo.
func identAt(f *ast.File, caret int, off func(token.Pos) int) *ast.Ident {
	var inside, after *ast.Ident
	ast.Inspect(f, func(n ast.Node) bool {
		if inside != nil {
			return false
		}
		i, ok := n.(*ast.Ident)
		if !ok {
			return true
		}
		switch a, b := off(i.Pos()), off(i.End()); {
		case a <= caret && caret < b:
			inside = i
		case caret == b:
			after = i
		}
		return true
	})
	if inside != nil {
		return inside
	}
	return after
}

// typeSwitchAliases folds each `switch x := v.(type)`'s per-clause implicit
// objects onto one representative, the first clause's: alias maps every
// clause's object to it, and tsIdents maps the switch's own x ident (which
// the checker binds to nothing) to it too.
//
// The representative's position is the switch's x (go/types declares each
// clause's object at lhs.Pos()), so declSpan points a jump at the switch.
func typeSwitchAliases(f *ast.File, info *types.Info) (alias map[types.Object]types.Object, tsIdents map[*ast.Ident]types.Object) {
	alias, tsIdents = map[types.Object]types.Object{}, map[*ast.Ident]types.Object{}
	ast.Inspect(f, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSwitchStmt)
		if !ok {
			return true
		}
		as, ok := ts.Assign.(*ast.AssignStmt)
		if !ok || len(as.Lhs) != 1 {
			return true // `switch v.(type)`: nothing declared
		}
		var rep types.Object
		for _, c := range ts.Body.List {
			o := info.Implicits[c]
			if o == nil {
				continue
			}
			if rep == nil {
				rep = o
			}
			alias[o] = rep
		}
		if lhs, ok := as.Lhs[0].(*ast.Ident); ok && rep != nil {
			tsIdents[lhs] = rep
		}
		return true
	})
	return alias, tsIdents
}

// selectorUses answers for an ident the checker left unbound: the Sel of
// pkg.Name or v.Name where pkg or v is bound but the member is not, which
// under fake imports means a member of an imported package (sdb.Copy,
// s.Query, res.Rows). Its uses are every selector of the same name from
// the same package or variable. Anything else unbound (an undefined name,
// a struct key of an imported type) is no symbol.
func selectorUses(f *ast.File, info *types.Info, id *ast.Ident, at Span, off func(token.Pos) int) Symbol {
	sym := Symbol{Uses: []Span{}}
	var base types.Object // what id is selected from
	ast.Inspect(f, func(n ast.Node) bool {
		if se, ok := n.(*ast.SelectorExpr); ok && se.Sel == id {
			if x, ok := se.X.(*ast.Ident); ok {
				base = info.Uses[x]
			}
			return false
		}
		return base == nil
	})
	if base == nil {
		return sym
	}
	ast.Inspect(f, func(n ast.Node) bool {
		se, ok := n.(*ast.SelectorExpr)
		if !ok || se.Sel.Name != id.Name {
			return true
		}
		if x, ok := se.X.(*ast.Ident); ok && info.Uses[x] == base {
			sym.Uses = append(sym.Uses, Span{off(se.Sel.Pos()), off(se.Sel.End())})
		}
		return true
	})
	sym.Kind, sym.Name, sym.At = "member", id.Name, at
	return sym
}

// kindOf names what obj is, for the editor to show.
func kindOf(obj types.Object) string {
	if obj.Parent() == types.Universe {
		return "builtin" // len, error, nil, true, int…
	}
	switch o := obj.(type) {
	case *types.Var:
		if o.IsField() {
			return "field"
		}
		return "var"
	case *types.Const:
		return "const"
	case *types.TypeName:
		return "type"
	case *types.Func:
		if o.Signature().Recv() != nil {
			return "method"
		}
		return "func"
	case *types.PkgName:
		return "package"
	case *types.Label:
		return "label"
	}
	return "builtin"
}

// declSpan is where the script declares obj: its name at the declaration,
// or for an import without a name, the path literal. nil when obj is not
// declared in this file (a builtin, a universe type).
func declSpan(f *ast.File, obj types.Object, tf *token.File, off func(token.Pos) int) *Span {
	p := obj.Pos()
	if !p.IsValid() || int(p) < tf.Base() || int(p) > tf.Base()+tf.Size() {
		return nil
	}
	if _, ok := obj.(*types.PkgName); ok {
		for _, im := range f.Imports {
			if im.Pos() != p {
				continue
			}
			if im.Name != nil {
				return &Span{off(im.Name.Pos()), off(im.Name.End())}
			}
			return &Span{off(im.Path.Pos()), off(im.Path.End())}
		}
	}
	return &Span{off(p), off(p) + len(obj.Name())}
}

// fakeImporter gives every import an empty, complete package under the
// name the Go convention gives its path; see WHY THE IMPORTS ARE FAKED.
// The checker caches imports by path, so one call per path per check.
type fakeImporter struct{}

func (fakeImporter) Import(p string) (*types.Package, error) {
	pkg := types.NewPackage(p, pkgName(p))
	pkg.MarkComplete()
	return pkg, nil
}

// majorRe matches a module major-version element: the "v2" of
// github.com/x/y/v2, or the ".v3" of gopkg.in/yaml.v3.
var majorRe = regexp.MustCompile(`^v\d+$|\.v\d+$`)

// pkgName is the name a package at import path p goes by unless the import
// renames it: the last path element, skipping a major-version element
// (…/y/v2 is y) and trimming a gopkg.in-style suffix (yaml.v3 is yaml).
// A guess wrong for some third-party path costs only that package's
// selectors their usages; a script imports the standard library and sdb,
// where it is always right.
func pkgName(p string) string {
	elems := strings.Split(p, "/")
	last := elems[len(elems)-1]
	if len(elems) > 1 && majorRe.MatchString(last) && !strings.Contains(last, ".") {
		last = elems[len(elems)-2]
	}
	if i := strings.LastIndex(last, ".v"); i > 0 && majorRe.MatchString(last[i:]) {
		last = last[:i]
	}
	return last
}
