package theme

import "testing"

func TestFromHost(t *testing.T) {
	host := map[string]string{
		"bg": "#101418", "fg": "#e0e0e0", "muted": "#8899aa", "line": "#223344",
		"accent": "#5aa9e6", "warn": "#e6c15a", "err": "#e65a5a", "panel": "#141a20",
	}
	p, ok := FromHost(host)
	if !ok {
		t.Fatal("a complete host theme should map")
	}
	if p.Bg != "#101418" || p.Accent != "#5aa9e6" || p.Panel != "#141a20" || p.Panel2 != "#141a20" {
		t.Errorf("palette = %+v", p)
	}
	if want := Blend("#5aa9e6", "#101418", HostSelAlpha); p.Sel != want {
		t.Errorf("sel = %s, want %s", p.Sel, want)
	}
	host["accent"] = "rgba(1,2,3,0.5)"
	if _, ok := FromHost(host); ok {
		t.Error("a non-hex core key abandons the whole palette")
	}
	if _, ok := FromHost(nil); ok {
		t.Error("no colors, no palette")
	}
}
