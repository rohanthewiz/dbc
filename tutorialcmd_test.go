package main

import "testing"

// Lessons print as §N, so `dbc tutorial step §6` works as well as 6.
func TestTrimSection(t *testing.T) {
	for in, want := range map[string]string{"6": "6", "§6": "6", "§": "§", "": "", "§12": "12"} {
		if got := trimSection(in); got != want {
			t.Errorf("trimSection(%q) = %q, want %q", in, got, want)
		}
	}
}
