package main

import "testing"

func TestReplayDiverged(t *testing.T) {
	cases := []struct {
		name     string
		verdicts map[string]int
		labels   map[string]int
		want     bool
	}{
		{"same state every replay", map[string]int{"STRANDED": 3}, map[string]int{"queued-before-item": 3}, false},
		{"verdicts split", map[string]int{"STRANDED": 2, "PASS": 1}, map[string]int{"queued-before-item": 2, "after-item": 1}, true},
		// Both PASS, but the kill landed in different places.
		{"labels split, verdicts match", map[string]int{"PASS": 3}, map[string]int{"after-item": 2, "finished-first": 1}, true},
		{"single replay", map[string]int{"PASS": 1}, map[string]int{"after-item": 1}, false},
	}
	for _, c := range cases {
		if got := replayDiverged(c.verdicts, c.labels); got != c.want {
			t.Errorf("%s: replayDiverged = %v, want %v", c.name, got, c.want)
		}
	}
}
