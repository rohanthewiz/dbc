package script

import (
	"fmt"
	"go/ast"
	"go/types"
	"strings"
)

// WHY SOME METHOD NAMES ARE KEPT. A method can be called by its name from
// code the script never sees: fmt calls String() on any value it prints,
// encoding/json calls MarshalJSON, database/sql calls Scan. They find it
// at run time, by asserting the value to an interface (fmt.Stringer,
// json.Marshaler, sql.Scanner), so nothing in the script names the method
// at that call. The recheck cannot see it either: the imports are stand-ins
// (WHY THE IMPORTS ARE FAKED), so fmt.Stringer does not exist to it, and a
// renamed String() leaves a script that compiles and type-checks the same
// and quietly prints differently.
//
//	type cents int
//	func (c cents) String() string { … }   ── F2 → Text
//	s.Print("%v", c)                         prints 1999, not $19.99,
//	                                         and nothing failed to compile
//
// So a method on a concrete type whose name AND signature are a well-known
// interface's is refused both ways: renamed away (it stops being called)
// and renamed to (it starts being called). Error() is among them though
// error is a universe type the recheck knows: the recheck catches only a
// value the script itself uses as an error, not one fmt finds by asserting.
//
// WHY THE SIGNATURE TOO. A method satisfies the interface only with its
// exact signature, and a script's String(width int) string or Len() int64
// is nobody's: the name alone would refuse renames that are safe. The
// signature is read from the declaration's syntax, since a parameter of an
// imported type (fmt.State, driver.Value) has no type under stand-in
// imports. An interface's own method is not refused: renaming it changes
// which types satisfy that interface, which the recheck sees at every use
// of it the script makes.
//
// The list is the standard library's dynamically found methods a script
// is likely to meet: printing, encoding, database/sql, errors, io, sort.
// It is a heuristic for the common case, not every interface there is.

// wellKnown is one method that code outside the script finds by name.
type wellKnown struct {
	sig   string // name and signature, as sigText writes them
	iface string // the interface it satisfies
	by    string // who calls it and why, a clause of the sentence
}

// wellKnownMethods, by method name. A name can have more than one: Scan is
// sql.Scanner's and fmt.Scanner's, with different signatures.
var wellKnownMethods = func() map[string][]wellKnown {
	m := map[string][]wellKnown{}
	for _, w := range []wellKnown{
		{"Error() string", "error", "fmt and s.Print call it to print the value"},
		{"String() string", "fmt.Stringer", "fmt and s.Print call it to print the value"},
		{"GoString() string", "fmt.GoStringer", "fmt's %#v calls it"},
		{"Format(fmt.State, rune)", "fmt.Formatter", "fmt and s.Print call it to print the value"},
		{"Scan(fmt.ScanState, rune) error", "fmt.Scanner", "fmt's Scan functions call it"},

		{"MarshalJSON() ([]byte, error)", "json.Marshaler", "encoding/json calls it"},
		{"UnmarshalJSON([]byte) error", "json.Unmarshaler", "encoding/json calls it"},
		{"MarshalText() ([]byte, error)", "encoding.TextMarshaler", "encoding/json and other encoders call it"},
		{"UnmarshalText([]byte) error", "encoding.TextUnmarshaler", "encoding/json and other decoders call it"},
		{"MarshalBinary() ([]byte, error)", "encoding.BinaryMarshaler", "encoding/gob and other encoders call it"},
		{"UnmarshalBinary([]byte) error", "encoding.BinaryUnmarshaler", "encoding/gob and other decoders call it"},
		{"MarshalXML(*xml.Encoder, xml.StartElement) error", "xml.Marshaler", "encoding/xml calls it"},
		{"UnmarshalXML(*xml.Decoder, xml.StartElement) error", "xml.Unmarshaler", "encoding/xml calls it"},

		{"Scan(any) error", "sql.Scanner", "database/sql calls it to scan a row into the value"},
		{"Value() (driver.Value, error)", "driver.Valuer", "database/sql calls it to pass the value as an argument"},

		{"Unwrap() error", "errors' Unwrap", "errors.Is and errors.As call it"},
		{"Unwrap() []error", "errors' Unwrap", "errors.Is and errors.As call it"},
		{"Is(error) bool", "errors' Is", "errors.Is calls it"},
		{"As(any) bool", "errors' As", "errors.As calls it"},

		{"Read([]byte) (int, error)", "io.Reader", "io and anything taking an io.Reader call it"},
		{"Write([]byte) (int, error)", "io.Writer", "io, fmt.Fprint and anything taking an io.Writer call it"},
		{"Close() error", "io.Closer", "anything taking an io.Closer calls it"},
		{"WriteTo(io.Writer) (int64, error)", "io.WriterTo", "io.Copy calls it"},
		{"ReadFrom(io.Reader) (int64, error)", "io.ReaderFrom", "io.Copy calls it"},

		{"Len() int", "sort.Interface", "sort.Sort and container/heap call it"},
		{"Less(int, int) bool", "sort.Interface", "sort.Sort and container/heap call it"},
		{"Swap(int, int)", "sort.Interface", "sort.Sort and container/heap call it"},
		{"Push(any)", "heap.Interface", "container/heap calls it"},
		{"Pop() any", "heap.Interface", "container/heap calls it"},
	} {
		name := w.sig[:strings.IndexByte(w.sig, '(')]
		m[name] = append(m[name], w)
	}
	return m
}()

// keptMethod says why renaming method obj to name would change what code
// outside the script calls (see WHY SOME METHOD NAMES ARE KEPT): "" when
// obj is not a concrete type's method, or neither its name nor name with
// its signature is a well-known one. name == obj.Name() asks about the
// rename away, used up front by fixed; any other name, the rename to.
func (c *checked) keptMethod(obj types.Object, name string) string {
	fn, ok := obj.(*types.Func)
	if !ok {
		return ""
	}
	sig, _ := fn.Type().(*types.Signature)
	if sig == nil || sig.Recv() == nil || types.IsInterface(sig.Recv().Type()) {
		return "" // a func, or an interface's own method: the recheck's
	}
	ft := c.funcType(fn)
	if ft == nil {
		return ""
	}
	// a rename changes only the name in front: the signature is the
	// declared one either way
	params, results := c.sigText(ft)
	for _, w := range wellKnownMethods[name] {
		if w.sig != name+params+results {
			continue
		}
		if name == fn.Name() {
			return fmt.Sprintf("%s%s%s satisfies %s: %s, by that name, out of the script's sight. "+
				"Renamed, it would quietly stop being called; rename it by hand if nothing relies on it",
				name, params, results, w.iface, w.by)
		}
		return fmt.Sprintf("as %s%s%s, %s would satisfy %s: %s, so the script would start doing something new. "+
			"Rename it by hand if that is what you want", name, params, results, fn.Name(), w.iface, w.by)
	}
	return ""
}

// funcType is the syntax of method fn's declaration: nil when the script
// does not declare it (it always does for a method Rename reaches).
func (c *checked) funcType(fn *types.Func) *ast.FuncType {
	for _, d := range c.f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv != nil && fd.Name.Pos() == fn.Pos() {
			return fd.Type
		}
	}
	return nil
}

// sigText writes ft's parameters and results as wellKnownMethods spells
// them: "(fmt.State, rune)" and " ([]byte, error)", " string" for one
// result, "" for none. Parameter names are dropped, and each name in
// `i, j int` counts as a parameter.
func (c *checked) sigText(ft *ast.FuncType) (params, results string) {
	list := func(fl *ast.FieldList) []string {
		var ts []string
		if fl == nil {
			return ts
		}
		for _, f := range fl.List {
			t := c.typeText(f.Type)
			for range max(len(f.Names), 1) {
				ts = append(ts, t)
			}
		}
		return ts
	}
	params = "(" + strings.Join(list(ft.Params), ", ") + ")"
	switch rs := list(ft.Results); len(rs) {
	case 0:
	case 1:
		results = " " + rs[0]
	default:
		results = " (" + strings.Join(rs, ", ") + ")"
	}
	return params, results
}

// typeText is type expression e in one spelling per type, so a signature
// compares as text:
//   - a universe alias by its short name: uint8 → byte, int32 → rune,
//     interface{} → any
//   - an imported type by its package's own name, whatever the import
//     calls it: f.State under `import f "fmt"` → fmt.State
//   - a type the script declares, prefixed with main., so a script's own
//     `type error …` is never Go's error
func (c *checked) typeText(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.Ident:
		if o := c.info.Uses[t]; o != nil && o.Parent() != types.Universe {
			return "main." + t.Name
		}
		switch t.Name {
		case "uint8":
			return "byte"
		case "int32":
			return "rune"
		}
		return t.Name
	case *ast.SelectorExpr:
		if x, ok := t.X.(*ast.Ident); ok {
			if pn, ok := c.info.Uses[x].(*types.PkgName); ok {
				return pn.Imported().Name() + "." + t.Sel.Name
			}
		}
	case *ast.StarExpr:
		return "*" + c.typeText(t.X)
	case *ast.ArrayType:
		if t.Len == nil {
			return "[]" + c.typeText(t.Elt)
		}
	case *ast.Ellipsis:
		return "..." + c.typeText(t.Elt)
	case *ast.InterfaceType:
		if t.Methods == nil || len(t.Methods.List) == 0 {
			return "any"
		}
	case *ast.ParenExpr:
		return c.typeText(t.X)
	}
	return types.ExprString(e)
}
