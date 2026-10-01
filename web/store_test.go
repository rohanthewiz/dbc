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
