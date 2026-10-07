package web

import (
	"path/filepath"
	"testing"
)

// TestMoveLayout moves keys as one simultaneous rename, on the file store
// and the memory-only one: a chain ("a" → "a/b" while "a/b" → "a/b/b") must
// not read a value another move just wrote, a missing from key leaves its
// to key alone, and an old key is deleted rather than blanked.
func TestMoveLayout(t *testing.T) {
	file, err := OpenStore(filepath.Join(t.TempDir(), "web.bytdb"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	mem, err := OpenStore("")
	if err != nil {
		t.Fatal(err)
	}
	for name, st := range map[string]*Store{"file": file, "memory": mem} {
		t.Run(name, func(t *testing.T) {
			if err := st.SetLayout(map[string]string{
				"p.a": "=one", "p.a/b": "=two", "p.keep": "=k", "p.gone2": "=old",
			}); err != nil {
				t.Fatal(err)
			}
			if err := st.MoveLayout(map[string]string{
				"p.a": "p.a/b", "p.a/b": "p.a/b/b", "p.gone": "p.gone2",
			}); err != nil {
				t.Fatal(err)
			}
			got, err := st.Layout()
			if err != nil {
				t.Fatal(err)
			}
			want := map[string]string{"p.a/b": "=one", "p.a/b/b": "=two", "p.keep": "=k", "p.gone2": "=old"}
			for k, v := range want {
				if got[k] != v {
					t.Errorf("%s = %q, want %q", k, got[k], v)
				}
			}
			if _, ok := got["p.a"]; ok {
				t.Error("p.a is still saved")
			}
		})
	}
}

// TestSaveTabScript keeps a script tab's script name (the script column,
// added by ALTER), on the file store across a reopen — the reopen runs the
// schema's ALTERs a second time, which IF NOT EXISTS must take — and on
// the memory-only one. A query tab's is "".
func TestSaveTabScript(t *testing.T) {
	path := filepath.Join(t.TempDir(), "web.bytdb")
	file, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	mem, err := OpenStore("")
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range []*Store{file, mem} {
		if err = st.SaveTab(Tab{ID: "a", Title: "copy.go", Script: "copy.go"}); err != nil {
			t.Fatal(err)
		}
		if err = st.SaveTab(Tab{ID: "b", Title: "Query 1", Conn: "lite", Buffer: "SELECT 1"}); err != nil {
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
		tabs, err := st.Tabs()
		if err != nil {
			t.Fatal(err)
		}
		if len(tabs) != 2 || tabs[0].Script != "copy.go" || tabs[1].Script != "" || tabs[1].Buffer != "SELECT 1" {
			t.Errorf("%s: tabs = %+v", name, tabs)
		}
	}
}
