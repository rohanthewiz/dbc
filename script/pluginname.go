package script

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"

	"github.com/rohanthewiz/dbc/pipeline"
)

// A plugin file's copy and its Name (N-191). Two files may not declare one
// plugin Name: the second does not load ("… is already the plugin of …").
// So a copy — an example copied a second time, a file duplicated — never
// loads as written. Both UIs' copy flows (dbc web's makePlugin, the TUI's)
// ask FitPluginName for the text to write: when the copy's Name is taken,
// it becomes the file's own (PluginNameFor), the rest of the text as it was.
//
//	gen_series-2.go, a copy of gen_series.go     Name: "gen.series"
//	                                           → Name: "gen.series-2"
//
// Only a Name written as a plain string literal is rewritten; one computed
// (a const, an expression) is the user's to change, and the copy is
// written as it is.

// FitPluginName returns text with its Plugin.Name rewritten to file's own
// name when the Name it has is taken (taken: another plugin, built in or a
// file's, already has it); from and to say what changed, both "" when
// nothing did. The new name is made free with -2, -3 … when the file's own
// is taken too.
func FitPluginName(text, file string, taken func(name string) bool) (out, from, to string) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, text, parser.SkipObjectResolution)
	if f == nil || err != nil {
		return text, "", ""
	}
	lit := pluginNameLit(f)
	if lit == nil {
		return text, "", ""
	}
	name, err := strconv.Unquote(lit.Value)
	if err != nil || !taken(name) {
		return text, "", ""
	}
	base := PluginNameFor(file)
	to = base
	for n := 2; to == name || taken(to); n++ {
		to = fmt.Sprintf("%s-%d", base, n)
	}
	tf := fset.File(lit.Pos())
	a, b := tf.Offset(lit.Pos()), tf.Offset(lit.End())
	return text[:a] + strconv.Quote(to) + text[b:], name, to
}

// PluginNameFor is the plugin name a file's name suggests, as the examples
// pair them: the stem, its underscores as dots ("mask_email.go" →
// "mask.email", "gen_series-2.go" → "gen.series-2"). A stem that would
// make no valid name (a trailing "_") keeps its underscores.
func PluginNameFor(file string) string {
	stem := strings.TrimSuffix(file, ".go")
	if name := strings.ReplaceAll(stem, "_", "."); pipeline.ValidName(name) {
		return name
	}
	return stem
}

// pluginNameLit is the string literal a plugin file's descriptor gives as
// its Name — var Plugin = sdb.Plugin{Name: "…", …} — or nil when there is
// no such literal (no descriptor, a computed Name).
func pluginNameLit(f *ast.File) *ast.BasicLit {
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.VAR {
			continue
		}
		for _, sp := range gd.Specs {
			vs, ok := sp.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, id := range vs.Names {
				if id.Name != "Plugin" || i >= len(vs.Values) {
					continue
				}
				lit, ok := vs.Values[i].(*ast.CompositeLit)
				if !ok {
					return nil
				}
				for _, el := range lit.Elts {
					kv, ok := el.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					if key, isID := kv.Key.(*ast.Ident); !isID || key.Name != "Name" {
						continue
					}
					if bl, ok := kv.Value.(*ast.BasicLit); ok && bl.Kind == token.STRING {
						return bl
					}
					return nil
				}
			}
		}
	}
	return nil
}

// PluginNameTaken says whether a plugin is already called name: one built
// into dbc, one a file loaded, or one a file declares that did not load
// (its problem names it), which would take the name back once fixed. The
// hosts' taken for FitPluginName.
func PluginNameTaken(name string) bool {
	if _, ok := pipeline.Lookup(name); ok {
		return true
	}
	for _, p := range pipeline.PluginProblems() {
		if p.Name == name {
			return true
		}
	}
	return false
}
