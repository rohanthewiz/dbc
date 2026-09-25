package config

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

func TestAddRemoveConn(t *testing.T) {
	c := &Config{Connections: []Connection{{Name: "a", Driver: "sqlite"}}}
	before := c.Conns()

	if err := c.AddConn(Connection{Name: "b", Driver: "sqlite", Web: true}); err != nil {
		t.Fatal(err)
	}
	if err := c.AddConn(Connection{Name: "a"}); !errors.Is(err, ErrConnExists) {
		t.Fatalf("duplicate add: %v", err)
	}
	if got, ok := c.ConnByName("b"); !ok || !got.Web {
		t.Fatalf("b = %+v, %v", got, ok)
	}
	// copy-on-write: a slice taken before the add is untouched by it
	if len(before) != 1 {
		t.Fatalf("an earlier snapshot changed: %+v", before)
	}

	if !c.RemoveConn("a") || c.RemoveConn("a") {
		t.Fatal("RemoveConn should report true once, then false")
	}
	if names := fmt.Sprint(c.Conns()); !strings.Contains(names, "b") || strings.Contains(names, "{a ") {
		t.Fatalf("after remove: %s", names)
	}
}

// Readers and writers side by side; run with -race to mean anything.
func TestConnsConcurrent(t *testing.T) {
	c := &Config{}
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			name := fmt.Sprintf("c%d", i)
			_ = c.AddConn(Connection{Name: name})
			c.RemoveConn(name)
		}()
		go func() {
			defer wg.Done()
			for range 50 {
				for _, cn := range c.Conns() {
					_ = cn.Name
				}
				c.ConnByName("c1")
			}
		}()
	}
	wg.Wait()
}

func TestExpandDSN(t *testing.T) {
	t.Setenv("DBC_X_SET", "pw")
	got, warns := ExpandDSN("n", "u:${DBC_X_SET}@h/${DBC_X_SURELY_UNSET}")
	if got != "u:pw@h/" {
		t.Fatalf("expanded = %q", got)
	}
	if len(warns) != 1 || !strings.Contains(warns[0], `"n"`) || !strings.Contains(warns[0], "DBC_X_SURELY_UNSET") {
		t.Fatalf("warnings = %q", warns)
	}
}
