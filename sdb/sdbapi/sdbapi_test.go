package sdbapi

import (
	"bytes"
	"encoding/json"
	"slices"
	"testing"
)

// The embedded api.json is what the source says now. A change to sdb (or a
// type it aliases) without a regeneration fails here, with the command.
func TestAPIUpToDate(t *testing.T) {
	api, err := Build("../..")
	if err != nil {
		t.Fatal(err)
	}
	b, err := Encode(api)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b, JSON()) {
		t.Fatal("sdb/sdbapi/api.json is stale: run `go generate ./sdb/sdbapi`")
	}
}

// What the editor relies on: S's methods with their connection arguments,
// the host-only ones left out, and an alias described by its target.
func TestAPIShape(t *testing.T) {
	var api API
	if err := json.Unmarshal(JSON(), &api); err != nil {
		t.Fatal(err)
	}
	typ := func(name string) Type {
		i := slices.IndexFunc(api.Types, func(ty Type) bool { return ty.Name == name })
		if i < 0 {
			t.Fatalf("no type %s", name)
		}
		return api.Types[i]
	}
	method := func(ty Type, name string) (Func, bool) {
		i := slices.IndexFunc(ty.Methods, func(f Func) bool { return f.Name == name })
		if i < 0 {
			return Func{}, false
		}
		return ty.Methods[i], true
	}

	s := typ("S")
	for name, want := range map[string][]int{"Query": {0}, "Exec": {0}, "Copy": {0, 1}, "Print": nil, "Show": nil} {
		m, ok := method(s, name)
		if !ok {
			t.Errorf("S has no %s", name)
			continue
		}
		if !slices.Equal(m.ConnArgs, want) {
			t.Errorf("S.%s connArgs = %v, want %v", name, m.ConnArgs, want)
		}
	}
	if q, _ := method(s, "Query"); q.Sig != "func (s *S) Query(conn, query string, args ...any) (*Result, error)" ||
		len(q.Params) != 3 || q.Params[2] != (Param{Name: "args", Type: "...any"}) || q.Doc == "" {
		t.Errorf("S.Query = %+v", q)
	}
	for _, name := range hostOnly {
		if _, ok := method(s, name); ok {
			t.Errorf("host-only S.%s is offered to scripts", name)
		}
	}

	co := typ("CopyOpts")
	if co.Of != "etl.CopyOptions" || co.Kind != "struct" ||
		!slices.ContainsFunc(co.Fields, func(f Field) bool { return f.Name == "Create" && f.Type == "bool" && f.Doc != "" }) {
		t.Errorf("CopyOpts = %+v", co)
	}
	// a field with only a line comment still has a doc
	res := typ("Result")
	if i := slices.IndexFunc(res.Fields, func(f Field) bool { return f.Name == "Rows" }); i < 0 || res.Fields[i].Doc == "" {
		t.Errorf("Result.Rows = %+v", res.Fields)
	}
	if len(api.Funcs) == 0 || api.Funcs[0].Name != "IsCanceled" {
		t.Errorf("funcs = %+v", api.Funcs)
	}
}

// hostOnly names methods S really has, so a rename in sdb cannot quietly
// start offering one to scripts.
func TestHostOnlyExist(t *testing.T) {
	l := loader{root: "../..", pkgs: map[string]*loaded{}}
	p, err := l.load(sdbPkg)
	if err != nil {
		t.Fatal(err)
	}
	var have []string
	for _, ty := range p.doc.Types {
		if ty.Name != "S" {
			continue
		}
		for _, m := range ty.Methods {
			have = append(have, m.Name)
		}
		for _, f := range ty.Funcs { // constructors: sdb.New
			have = append(have, f.Name)
		}
	}
	for _, name := range hostOnly {
		if !slices.Contains(have, name) {
			t.Errorf("hostOnly names %s, which S does not have", name)
		}
	}
}
