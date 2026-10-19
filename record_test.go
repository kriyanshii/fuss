package main

import (
	"os"
	"path/filepath"
	"testing"
)

// Step lines only count for the named step and the exact run ID.
func TestStepLines(t *testing.T) {
	rec := "start a fuss-1\nend a fuss-1\nstart b fuss-1\nend b fuss-10\n"
	if !stepStarted(rec, "a", "fuss-1") || !stepEnded(rec, "a", "fuss-1") || !stepStarted(rec, "b", "fuss-1") {
		t.Error("missed a line that is there")
	}
	if stepEnded(rec, "b", "fuss-1") {
		t.Error("end b fuss-10 counted as end b fuss-1")
	}
}

func TestRecordPhaseTwoSteps(t *testing.T) {
	for _, tt := range []struct{ rec, want string }{
		{"", "before-a"},
		{"start a x\n", "in-a"},
		{"start a x\nend a x\n", "between"},
		{"start a x\nend a x\nstart b x\n", "in-b"},
		{"start a x\nend a x\nstart b x\nend b x\n", "after"},
	} {
		path := filepath.Join(t.TempDir(), "runs.txt")
		if err := os.WriteFile(path, []byte(tt.rec), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := recordPhase(path, dagTwo); got != tt.want {
			t.Errorf("record %q: got %s, want %s", tt.rec, got, tt.want)
		}
	}
}
