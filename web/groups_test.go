package web

import (
	"testing"
)

// Tab groups (groups.go): the page owns them; the server merges one
// window's write with the members other windows hold, and moves connection
// groups on a rename (TestConnEdit).

func TestMergeTabGroups(t *testing.T) {
	held := map[string]bool{"x": true, "y": true}
	for _, c := range []struct {
		name, given, old string
		elsewhere        map[string]bool
		want             string
	}{
		{"no other window: as given",
			`[{"name":"a","members":["k"],"color":0}]`, `[{"name":"b","members":["x"],"color":1}]`, nil,
			`[{"name":"a","members":["k"],"color":0}]`},
		{"another window's members join the same-named group, case-insensitively",
			`[{"name":"wip","members":["k"],"color":0}]`, `[{"name":"WIP","members":["x","gone"],"color":0}]`, held,
			`[{"name":"wip","members":["k","x"],"color":0}]`},
		{"a group only another window shows comes back with only its keys",
			`[]`, `[{"name":"b","members":["x","gone"],"collapsed":true,"color":2}]`, held,
			`[{"name":"b","members":["x"],"collapsed":true,"color":2}]`},
		{"a connection group's exclusions held elsewhere are kept",
			`[{"name":"p","conn":"prod","color":0}]`, `[{"name":"p","conn":"prod","exclude":["y"],"color":0}]`, held,
			`[{"name":"p","conn":"prod","exclude":["y"],"color":0}]`},
		{"a group holding none of another window's tabs is the writer's to drop",
			`[]`, `[{"name":"p","conn":"prod","exclude":["gone"],"color":0}]`, held,
			`[]`},
		{"not doubled",
			`[{"name":"a","members":["x"],"color":0}]`, `[{"name":"a","members":["x"],"color":0}]`, held,
			`[{"name":"a","members":["x"],"color":0}]`},
		{"an old value that does not parse: as given",
			`[{"name":"a","color":0}]`, `{nope`, held,
			`[{"name":"a","color":0}]`},
		{"a given value that does not parse: as given",
			`{nope`, `[{"name":"b","members":["x"],"color":1}]`, held,
			`{nope`},
	} {
		if got := mergeTabGroups(c.given, c.old, c.elsewhere); got != c.want {
			t.Errorf("%s:\n got %s\nwant %s", c.name, got, c.want)
		}
	}
}

// A layout write of "groups" keeps the members another window holds, and
// lets the writer drop its own.
func TestLayoutKeepsOtherWindowsGroups(t *testing.T) {
	e := newTestEnv(t)
	a, _ := e.openWin()
	b, _ := e.openWin()
	e.saveIn(a, "k1", "", 200)
	e.saveIn(a, "k2", "", 200)
	e.saveIn(b, "k3", "", 200)
	e.api("PUT", "/api/v1/layout?win="+a, `{"groups":"[{\"name\":\"wip\",\"members\":[\"k1\",\"k2\"],\"color\":0}]"}`, 200)
	// B booted before A grouped, so it knows no "wip"; its own group goes in
	// beside A's, which comes back for A's tabs
	e.api("PUT", "/api/v1/layout?win="+b, `{"groups":"[{\"name\":\"mine\",\"members\":[\"k3\"],\"color\":1}]"}`, 200)
	l := decodeData[map[string]string](t, e.api("GET", "/api/v1/layout", "", 200))
	want := `[{"name":"mine","members":["k3"],"color":1},{"name":"wip","members":["k1","k2"],"color":0}]`
	if l[groupsKey] != want {
		t.Fatalf("groups = %s\nwant %s", l[groupsKey], want)
	}
	// A ungroups wip (its tabs are all A's): gone, and B's group stays
	e.api("PUT", "/api/v1/layout?win="+a, `{"groups":"[]"}`, 200)
	l = decodeData[map[string]string](t, e.api("GET", "/api/v1/layout", "", 200))
	if want := `[{"name":"mine","members":["k3"],"color":1}]`; l[groupsKey] != want {
		t.Fatalf("groups after ungroup = %s\nwant %s", l[groupsKey], want)
	}
}
