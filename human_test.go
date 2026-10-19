package main

import (
	"io/fs"
	"os"
	"testing"
)

// The STRANDED check must tell a stranded run from a healthy one (RULEBOOK
// section 4). Both copies go through the same judgeStaged as the control.
// Each needs a 60s scheduler wait; -short skips it.
func TestStagedStrandedVsHealthy(t *testing.T) {
	if testing.Short() {
		t.Skip("stages two runs with a 60s scheduler wait each")
	}
	needDagu(t)
	stranded, err := fs.Sub(strandedFixture, "testdata/stranded")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat("testdata/healthy"); err != nil {
		t.Fatal("testdata/healthy is not chosen yet: copy a healthy run there so the check is shown to tell the two apart")
	}
	for _, c := range []struct {
		name, want string
		fsys       fs.FS
	}{
		{"stranded", "STRANDED", stranded},
		{"healthy", "RESUMED", os.DirFS("testdata/healthy")},
	} {
		tr, err := judgeStaged(c.fsys, c.name)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		t.Logf("%s: verdict=%s final=%s items=%d starts=%d repeat-exit=%d repeat=%q",
			c.name, tr.verdict, tr.final.Run, tr.items, tr.starts, tr.repeatExit, tr.repeatOut)
		if tr.verdict != c.want {
			t.Errorf("%s: verdict %s (final=%s items=%d starts=%d), want %s (dir=%s)",
				c.name, tr.verdict, tr.final.Run, tr.items, tr.starts, c.want, tr.dir)
			continue
		}
		os.RemoveAll(tr.dir)
	}
}

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

func TestStagedVerdict(t *testing.T) {
	cases := []struct {
		name         string
		run          string
		items, start int
		want         string
	}{
		{"resumed, queue item used up", "Succeeded", 0, 0, "RESUMED"},
		{"stranded", "Queued", 0, 0, "STRANDED"},
		{"still waiting", "Waiting", 0, 0, "NO-RESUME"},
		{"queued with item, not yet run", "Queued", 1, 0, "NO-RESUME"},
		{"record shows one resume", "Succeeded", 0, 1, "PASS"},
	}
	for _, c := range cases {
		if got := stagedVerdict(runStatus{Run: c.run}, c.items, c.start); got != c.want {
			t.Errorf("%s: stagedVerdict = %s, want %s", c.name, got, c.want)
		}
	}
}
