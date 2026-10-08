package script

import (
	"errors"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"slices"
	"strings"
)

// Rename in a script (F2 in dbc web's script tabs, web/scripts.go): every
// use of a name the script declares, as Resolve finds them scope by scope,
// gets the new name — or the rename is refused, with a sentence saying why.
//
//	src, caret, name ─► Resolve's symbol ─► fixed?  (an import's member, a
//	                                │                builtin, Run, embedded)
//	                                ├──────► taken?  (Scope.Lookup at the
//	                                │                 declaration, LookupParent
//	                                │                 at every use)
//	                                ├──────► the edits: each use → name
//	                                └──────► recheck: the renamed script, type
//	                                          checked again, must bind every
//	                                          ident as before and add no error
//
// WHY BOTH A SCOPE CHECK AND A RECHECK. The scope check is the common case
// said clearly: the name is already declared beside the one renamed, or a
// use of it sits inside a block that declares the new name, which would
// take the use over. It cannot see everything a rename can break — a field
// or method colliding with another of its type, a promoted field newly
// shadowed, an outer variable's uses captured by the renamed one inside its
// scope, a type that is embedded and so names a field. The recheck sees all
// of that at once, by the type checker's own rules: run go/types over the
// renamed text, and every ident must mean what it meant before (the renamed
// ones the renamed object, all others theirs), with no error that was not
// there already. A rename that passes can only have renamed. It costs a
// second check of a few hundred lines: well under a millisecond.
//
// WHAT IS REFUSED UP FRONT (Symbol.Fixed, so the rename box does not open):
// a member of an imported package (s.Query — its declaration is not in the
// script), a builtin (len, error), the script's Run (dbc calls it by that
// name), and an embedded field or a type something embeds (the field is
// named after the type, so one would have to follow the other, and its
// selectors with it; renaming them by hand is clearer than a rename that
// quietly reaches into other structs).

// Edit replaces [From, To) of the script with Text. From == To inserts.
type Edit struct {
	From int    `json:"from"`
	To   int    `json:"to"`
	Text string `json:"text"`
}

// NotOnSymbol is the sentence for a caret on nothing renamable, shared
// with the editor's rename box so both say it in the same words.
const NotOnSymbol = "rename works on a name the script declares: a variable, constant, type, function, method, field, label or import"

// Rename is the edits that rename the symbol at byte offset caret of src
// to name. The error is a sentence for the user: nothing renamable there,
// the symbol cannot be renamed (Symbol.Fixed), the name is not a Go name,
// or the renamed script would mean something else.
func Rename(src string, caret int, name string) (edits []Edit, err error) {
	defer func() {
		if r := recover(); r != nil {
			edits, err = nil, errors.New("the script could not be read well enough to rename in")
		}
	}()
	c := checkSrc(src)
	var sym Symbol
	var obj types.Object
	if c != nil {
		sym, obj = c.resolve(caret)
	}
	switch {
	case sym.Kind == "" || obj == nil && sym.Fixed == "":
		return nil, errors.New("nothing to rename here: " + NotOnSymbol)
	case sym.Fixed != "":
		return nil, errors.New(sym.Fixed)
	}

	name = strings.TrimSpace(name)
	switch {
	case name == "":
		return nil, errors.New("the new name is empty")
	case name == "_":
		return nil, fmt.Errorf("_ is the blank identifier: nothing could use %s after", sym.Name)
	case token.IsKeyword(name):
		return nil, fmt.Errorf("%s is a Go keyword", name)
	case !token.IsIdentifier(name):
		return nil, fmt.Errorf("%q is not a Go name", name)
	case name == sym.Name:
		return []Edit{}, nil
	}

	if msg := c.taken(obj, sym, name); msg != "" {
		return nil, errors.New(msg)
	}
	edits = make([]Edit, 0, len(sym.Uses))
	for _, u := range sym.Uses {
		if _, ok := obj.(*types.PkgName); ok && sym.Def != nil && u == *sym.Def && isPathLit(src[u.From:u.To]) {
			// an unnamed import is declared by its path: it gets a name
			// in front of it, `import "fmt"` → `import f "fmt"`
			edits = append(edits, Edit{From: u.From, To: u.From, Text: name + " "})
			continue
		}
		edits = append(edits, Edit{From: u.From, To: u.To, Text: name})
	}
	if msg := c.recheck(src, edits, sym.Name, name); msg != "" {
		return nil, errors.New(msg)
	}
	return edits, nil
}

// isPathLit reports whether s is an import path literal rather than a name.
func isPathLit(s string) bool { return s != "" && (s[0] == '"' || s[0] == '`') }

// fixed says why obj, which the caret is on, cannot be renamed; "" when
// it can. See WHAT IS REFUSED UP FRONT.
func (c *checked) fixed(obj types.Object, sym Symbol) string {
	if sym.Kind == "builtin" {
		return sym.Name + " is predeclared by Go, not declared in the script"
	}
	switch o := obj.(type) {
	case *types.Func:
		if o.Name() == "Run" && c.pkg != nil && o.Parent() == c.pkg.Scope() {
			return "Run is the script's entry point: dbc calls it by that name"
		}
	case *types.Var:
		if o.Embedded() {
			return sym.Name + " is an embedded field, named after its type: rename the type and the field by hand"
		}
	case *types.TypeName:
		// an embedded field's ident is in Defs (the field) and Uses (the
		// type) both: one whose type is obj means a struct embeds it
		for id, d := range c.info.Defs {
			if v, ok := d.(*types.Var); ok && v.Embedded() && c.info.Uses[id] == obj {
				return fmt.Sprintf("%s is embedded in a struct on line %d, whose field is named after it: "+
					"rename the type and the field by hand", sym.Name, c.line(c.off(id.Pos())))
			}
		}
	}
	return ""
}

// declScope is the scope obj is declared in, nil for a field or method
// (go/types gives them none; the recheck covers their collisions).
//
// A type switch's representative (see checked.key) is the first case
// clause's object, but its name is the switch's, seen in every clause: its
// scope is the switch's, the clause scope's parent.
func (c *checked) declScope(obj types.Object) *types.Scope {
	s := obj.Parent()
	if s != nil && c.alias[obj] == obj {
		return s.Parent()
	}
	return s
}

// objLine is the line obj is declared on, 0 when not in the script.
func (c *checked) objLine(o types.Object) int {
	p := o.Pos()
	if !p.IsValid() || int(p) < c.tf.Base() || int(p) > c.tf.Base()+c.tf.Size() {
		return 0
	}
	return c.tf.Line(p)
}

// taken is the scope check (see WHY BOTH): "" when name is free for obj,
// else why not.
//
//	package ─ file (imports) ─ func Run ─ if ─ for …
//	   │  one namespace,          │
//	   │  checked both ways       └ obj declared here: name must not be
//	   │                            declared here too, and at each use
//	   │                            (deeper), no block between the use
//	   └ ...                        and here may declare name either
func (c *checked) taken(obj types.Object, sym Symbol, name string) string {
	scope := c.declScope(obj)
	if scope == nil {
		return ""
	}
	if o := scope.Lookup(name); o != nil && o != obj {
		return fmt.Sprintf("%s is already declared in this scope, on line %d", name, c.objLine(o))
	}
	if _, ok := obj.(*types.Label); ok {
		return "" // labels: one namespace per function, nothing nests
	}
	fileScope := c.info.Scopes[c.f]
	if fileScope == nil || c.pkg == nil {
		return "" // the recheck still guards
	}
	// a package-level name and an import may not share a name either,
	// though go/types keeps them in two scopes
	switch scope {
	case c.pkg.Scope():
		if o := fileScope.Lookup(name); o != nil {
			return fmt.Sprintf("%s is already the name of an import, on line %d", name, c.objLine(o))
		}
	case fileScope:
		if o := c.pkg.Scope().Lookup(name); o != nil {
			return fmt.Sprintf("%s is already declared in the script, on line %d", name, c.objLine(o))
		}
	}
	// at each use, LookupParent finds what name would mean there: a
	// declaration in a block between the use and obj's scope would take
	// the renamed use over. One at obj's scope was refused above, and one
	// outside it is shadowed by the renamed obj, as intended.
	for _, u := range sym.Uses {
		if sym.Def != nil && u == *sym.Def {
			continue
		}
		pos := c.tf.Pos(u.From)
		inner := fileScope.Innermost(pos)
		if inner == nil {
			continue
		}
		found, o := inner.LookupParent(name, pos)
		if o == nil {
			continue
		}
		for s := inner; s != nil && s != scope; s = s.Parent() {
			if s == found {
				return fmt.Sprintf("the %s on line %d would then mean the %s declared on line %d",
					sym.Name, c.line(u.From), name, c.objLine(o))
			}
		}
	}
	return ""
}

// recheck type-checks src with edits applied and compares (see WHY BOTH):
// "" when every ident means what it meant and no error is new, else the
// first difference as a sentence.
func (c *checked) recheck(src string, edits []Edit, old, name string) string {
	n := checkSrc(applyEdits(src, edits))
	if n == nil {
		return fmt.Sprintf("renaming %s to %s left a script that could not be read", old, name)
	}
	back := backMapper(edits)
	self := func(off int) int { return off }

	// binding is what an ident means, comparable across the two checks:
	// the declaration's offset in the original text when the script
	// declares it, else the object itself (Universe's are shared by every
	// check), else nothing.
	type binding struct {
		decl int // -1: not declared in the script
		ext  types.Object
	}
	bind := func(k *checked, i *ast.Ident, toOld func(int) int) binding {
		o := k.key(i)
		if o == nil {
			return binding{decl: -1}
		}
		if p := o.Pos(); p.IsValid() && int(p) >= k.tf.Base() && int(p) <= k.tf.Base()+k.tf.Size() {
			return binding{decl: toOld(k.off(p))}
		}
		return binding{decl: -1, ext: o}
	}
	say := func(b binding) string {
		switch {
		case b.decl >= 0:
			return fmt.Sprintf("the one declared on line %d", c.line(b.decl))
		case b.ext != nil:
			return "Go's predeclared " + b.ext.Name()
		}
		return "nothing"
	}

	// the parse is the same tree either way (a name for a name changes no
	// syntax), so idents pair up by where they are in the original text;
	// one a rename inserted (an import's new name) has no partner and is
	// skipped — its object is checked through the uses it binds
	was := map[int]binding{}
	ast.Inspect(c.f, func(nd ast.Node) bool {
		if i, ok := nd.(*ast.Ident); ok {
			was[c.off(i.Pos())] = bind(c, i, self)
		}
		return true
	})
	var diff string
	diffToNothing := false // the ident means nothing after: a declaration lost
	ast.Inspect(n.f, func(nd ast.Node) bool {
		i, ok := nd.(*ast.Ident)
		if !ok || diff != "" {
			return diff == ""
		}
		at := back(n.off(i.Pos()))
		before, ok := was[at]
		if !ok {
			return true
		}
		if now := bind(n, i, back); now != before {
			diff = fmt.Sprintf("renaming %s to %s would change what %s on line %d means: %s, not %s",
				old, name, i.Name, c.line(at), say(now), say(before))
			diffToNothing = now == binding{decl: -1}
		}
		return true
	})

	// a new error: one at a place that had none. Counting alone would
	// miss a rename that fixed one error and made another.
	had := map[int]bool{}
	for _, e := range c.errs {
		if e.Pos.IsValid() {
			had[c.off(e.Pos)] = true
		}
	}
	var broke string
	for _, e := range n.errs {
		if !e.Pos.IsValid() {
			continue
		}
		if at := back(n.off(e.Pos)); !had[at] {
			broke = fmt.Sprintf("renaming %s to %s would not compile: %s (line %d)", old, name, e.Msg, c.line(at))
			break
		}
	}

	// Which to say when both: a capture usually also leaves a variable
	// unused, and "x on line 9 would mean the one on line 8" says why
	// better than "declared and not used: x". But a binding lost to
	// nothing is a declaration the checker refused (a duplicate field,
	// "B redeclared"), and its error is the reason.
	if diff != "" && (!diffToNothing || broke == "") {
		return diff
	}
	return broke
}

// applyEdits is src with edits (non-overlapping, any order) applied.
func applyEdits(src string, edits []Edit) string {
	es := slices.Clone(edits)
	slices.SortFunc(es, func(a, b Edit) int { return a.From - b.From })
	var b strings.Builder
	at := 0
	for _, e := range es {
		b.WriteString(src[at:e.From])
		b.WriteString(e.Text)
		at = e.To
	}
	b.WriteString(src[at:])
	return b.String()
}

// backMapper maps a byte offset of the edited text to the original's: one
// before an edit moves by the edits before it, one inside an edit's new
// text is the edit's start.
//
//	original  ..a.. [old] ..b..
//	edited    ..a.. [new text] ..b..
//	                 └ all → old's From; b shifts back by len(new)-len(old)
func backMapper(edits []Edit) func(int) int {
	es := slices.Clone(edits)
	slices.SortFunc(es, func(a, b Edit) int { return a.From - b.From })
	return func(off int) int {
		shift := 0
		for _, e := range es {
			from := e.From + shift
			if off < from {
				break
			}
			if off < from+len(e.Text) {
				return e.From
			}
			shift += len(e.Text) - (e.To - e.From)
		}
		return off - shift
	}
}
