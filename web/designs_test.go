package web

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// TestSaveDesignStore: the revision check and rename table of SaveDesign,
// on a file store (reopened, so the rows really are on disk) and a memory
// one, which must agree.
func TestSaveDesignStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "web.bytdb")
	file, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	mem, _ := OpenStore("")
	const d1, d2 = `{"tables":[{"id":"t1","schema":"main","name":"orders"}]}`, `{"tables":[]}`
	for name, st := range map[string]*Store{"file": file, "memory": mem} {
		a, err := st.SaveDesign("", "", SavedDesign{Name: "Orders", Conn: "lite", Design: d1, SQL: "SELECT 1"})
		if err != nil {
			t.Fatalf("%s: create: %v", name, err)
		}
		if _, err = st.SaveDesign("", "", SavedDesign{Name: "Orders", Design: d2}); !errors.Is(err, errDesignExists) {
			t.Fatalf("%s: create onto a taken name: %v", name, err)
		}
		// a save from a stale copy is refused; from the current one it lands
		if _, err = st.SaveDesign("Orders", "stale", SavedDesign{Name: "Orders", Design: d2}); !errors.Is(err, errDesignChanged) {
			t.Fatalf("%s: stale save: %v", name, err)
		}
		b, err := st.SaveDesign("Orders", a.Rev(), SavedDesign{Name: "Orders", Conn: "lite", Design: d2, SQL: "SELECT 2"})
		if err != nil || b.Rev() == a.Rev() {
			t.Fatalf("%s: save: %v (rev %s → %s)", name, err, a.Rev(), b.Rev())
		}
		// rename, and a rename onto a taken name
		if _, err = st.SaveDesign("", "", SavedDesign{Name: "Other", Design: d1}); err != nil {
			t.Fatal(err)
		}
		if _, err = st.SaveDesign("Orders", b.Rev(), SavedDesign{Name: "Other", Design: d2}); !errors.Is(err, errDesignExists) {
			t.Fatalf("%s: rename onto a taken name: %v", name, err)
		}
		if _, err = st.SaveDesign("Orders", b.Rev(), SavedDesign{Name: "Orders by month", Conn: "lite", Design: d2, SQL: "SELECT 2"}); err != nil {
			t.Fatalf("%s: rename: %v", name, err)
		}
		if _, err = st.SaveDesign("Orders", b.Rev(), SavedDesign{Name: "Orders", Design: d2}); !errors.Is(err, errNoDesign) {
			t.Fatalf("%s: save under the old name: %v", name, err)
		}
		if err = st.DeleteDesign("Other"); err != nil {
			t.Fatal(err)
		}
		// a design tab keeps its design's name and revision
		if err = st.SaveTab(Tab{ID: "d", Title: "Orders by month", Design: d2, DesignName: "Orders by month", DesignRev: b.Rev()}); err != nil {
			t.Fatal(err)
		}
	}
	if err = file.Close(); err != nil {
		t.Fatal(err)
	}
	if file, err = OpenStore(path); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	for name, st := range map[string]*Store{"file": file, "memory": mem} {
		list, err := st.Designs()
		if err != nil || len(list) != 1 || list[0].Name != "Orders by month" || list[0].SQL != "SELECT 2" || list[0].Conn != "lite" {
			t.Fatalf("%s: designs = %+v, %v", name, list, err)
		}
		tabs, _ := st.Tabs()
		if len(tabs) != 1 || tabs[0].DesignName != "Orders by month" || tabs[0].DesignRev != list[0].Rev() {
			t.Fatalf("%s: tabs = %+v", name, tabs)
		}
	}
}

// TestDesignsAPI: the list, a save and its revision, the refusals in
// words, and a tab tied to a saved design.
func TestDesignsAPI(t *testing.T) {
	e := newTestEnv(t)
	type listed struct {
		Designs []designRow `json:"designs"`
	}
	type saved struct{ Name, Rev string }
	save := func(req designSaveReq, want int) testEnvelope {
		t.Helper()
		b, _ := json.Marshal(req)
		return e.api("POST", "/api/v1/designs", string(b), want)
	}
	const d = `{"tables":[{"id":"t1","schema":"main","name":"orders","alias":"o"},{"id":"t2","schema":"main","name":"customers"}]}`
	a := decodeData[saved](t, save(designSaveReq{Name: "Orders by month", Conn: "lite", Design: d, SQL: "SELECT 1"}, 200))
	got := decodeData[listed](t, e.api("GET", "/api/v1/designs", "", 200)).Designs
	if len(got) != 1 || got[0].Rev != a.Rev || strings.Join(got[0].Tables, ",") != "orders,customers" || got[0].SQL != "SELECT 1" {
		t.Fatalf("list = %+v", got)
	}
	// refusals: a name taken (409), a stale revision (409), a name that
	// is not one (400), JSON that is not a design (400), a design deleted
	// under the tab (404)
	save(designSaveReq{Name: "Orders by month", Design: d}, 409)
	if env := save(designSaveReq{Old: "Orders by month", Rev: "x", Name: "Orders by month", Design: d}, 409); !strings.Contains(env.Error, "changed since") {
		t.Errorf("stale: %q", env.Error)
	}
	save(designSaveReq{Name: " padded", Design: d}, 400)
	save(designSaveReq{Name: "a/b", Design: d}, 400)
	save(designSaveReq{Name: strings.Repeat("x", 41), Design: d}, 400)
	save(designSaveReq{Name: "Broken", Design: "{"}, 400)
	save(designSaveReq{Old: "Gone", Rev: "x", Name: "Gone", Design: d}, 404)

	b := decodeData[saved](t, save(designSaveReq{Old: "Orders by month", Rev: a.Rev, Name: "Orders by month", Conn: "lite", Design: d, SQL: "SELECT 2"}, 200))
	if b.Rev == a.Rev {
		t.Fatal("a save with new SQL kept its revision")
	}
	// a tab ties itself to it; a tab that is not a design tab cannot
	tab := func(tb Tab, want int) {
		t.Helper()
		j, _ := json.Marshal(tb)
		e.api("PUT", "/api/v1/tabs/"+tb.ID, string(j), want)
	}
	tab(Tab{ID: "d1", Title: "Orders by month", Design: d, DesignName: "Orders by month", DesignRev: b.Rev}, 200)
	tab(Tab{ID: "q1", Title: "Query 1", Buffer: "SELECT 1", DesignName: "Orders by month"}, 400)
	tab(Tab{ID: "d2", Title: "x", Design: d, DesignName: "a/b"}, 400)

	e.api("POST", "/api/v1/designs/delete", `{"name":"Orders by month"}`, 200)
	if got = decodeData[listed](t, e.api("GET", "/api/v1/designs", "", 200)).Designs; len(got) != 0 {
		t.Fatalf("after delete: %+v", got)
	}
}
