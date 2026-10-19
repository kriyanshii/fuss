package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// errNoRun matches Dagu's error text, the one place a dagu status error could
// push toward PASS. Against the real binary: a run Dagu never created must be
// LOST RUN if the record shows it started, and fine if the record is empty.
func TestJudgeNoRun(t *testing.T) {
	needDagu(t)
	for _, tt := range []struct {
		name, rec string
		wantLost  bool
	}{
		{"started", "start a fuss-never-created\n", true},
		{"never started", "", false},
	} {
		dir := t.TempDir()
		tr := trial{runID: "fuss-never-created", home: filepath.Join(dir, "home"), record: filepath.Join(dir, "runs.txt")}
		if err := os.WriteFile(tr.record, []byte(tt.rec), 0o644); err != nil {
			t.Fatal(err)
		}
		st, problems := judge(tr)
		if st.Run != "none" {
			t.Fatalf("%s: status %+v, problems %v: errNoRun did not match Dagu's output", tt.name, st, problems)
		}
		lost := len(problems) == 1 && strings.HasPrefix(problems[0].Error(), "LOST RUN")
		if lost != tt.wantLost || (!tt.wantLost && len(problems) > 0) {
			t.Errorf("%s: problems %v, wantLost=%v", tt.name, problems, tt.wantLost)
		}
	}
}

// A run that never leaves Running is reported as ZOMBIE once the deadline passes.
func TestSettleZombie(t *testing.T) {
	running := func() (runStatus, error) { return runStatus{Run: "Running", Step: "running"}, nil }
	start := time.Now()
	st, err := settle(running, time.Second)
	if err == nil || !strings.HasPrefix(err.Error(), "ZOMBIE") {
		t.Fatalf("got %v, want ZOMBIE", err)
	}
	if took := time.Since(start); took < time.Second {
		t.Fatalf("gave up after %v, before the 1s deadline", took)
	}
	if st.Run != "Running" {
		t.Fatalf("last status %q, want Running", st.Run)
	}
}

// A run that Dagu repairs is returned as soon as it stops saying Running.
func TestSettleRepaired(t *testing.T) {
	calls := 0
	repairs := func() (runStatus, error) {
		calls++
		if calls < 3 {
			return runStatus{Run: "Running", Step: "running"}, nil
		}
		return runStatus{Run: "Failed", Step: "failed"}, nil
	}
	st, err := settle(repairs, 10*time.Second)
	if err != nil || st.Run != "Failed" || calls != 3 {
		t.Fatalf("got %+v err=%v after %d calls, want Failed after 3", st, err, calls)
	}
}

// A query error is passed through untouched, so errNoRun reaches the caller.
func TestSettleError(t *testing.T) {
	noRun := func() (runStatus, error) { return runStatus{}, errNoRun }
	if _, err := settle(noRun, 10*time.Second); !errors.Is(err, errNoRun) {
		t.Fatalf("got %v, want errNoRun", err)
	}
}

// Rule 1 must fail exactly when Dagu claims success the record never saw.
func TestCheckNoFalseSuccess(t *testing.T) {
	const id = "fuss-1"
	started := "start a fuss-1\n"
	ended := started + "end a fuss-1\n"
	tests := []struct {
		name     string
		st       runStatus
		rec      string
		wantFail bool
	}{
		{"succeeded and ended", runStatus{Run: "Succeeded", Step: "succeeded"}, ended, false},
		{"succeeded, no end", runStatus{Run: "Succeeded", Step: "succeeded"}, started, true},
		{"step succeeded, no end", runStatus{Run: "Failed", Step: "succeeded"}, started, true},
		{"succeeded, empty record", runStatus{Run: "Succeeded", Step: "succeeded"}, "", true},
		{"end from another run", runStatus{Run: "Succeeded", Step: "succeeded"}, "end a fuss-2\n", true},
		{"failed, no end", runStatus{Run: "Failed", Step: "failed"}, started, false},
		{"failed but ended", runStatus{Run: "Failed", Step: "failed"}, ended, false},
	}
	for _, tt := range tests {
		err := checkNoFalseSuccess(tt.st, tt.rec, id)
		if (err != nil) != tt.wantFail {
			t.Errorf("%s: got %v, wantFail=%v", tt.name, err, tt.wantFail)
		}
	}
}

// No run in Dagu is fine only if the step never started.
func TestCheckNoRun(t *testing.T) {
	if err := checkNoRun(""); err != nil {
		t.Errorf("empty record: got %v, want nil", err)
	}
	if err := checkNoRun("start a fuss-1\n"); err == nil {
		t.Error("started step with no run: got nil, want LOST RUN")
	}
}

// Rule 4: one case per verdict plus a clean pass. Snapshot is the settled
// status before the retry; the record holds both attempts split by the marker.
func TestCheckRetry(t *testing.T) {
	const id = "fuss-1"
	st := func(run string, steps ...string) runStatus {
		m := map[string]string{}
		for i := 0; i+1 < len(steps); i += 2 {
			m[steps[i]] = steps[i+1]
		}
		return runStatus{Run: run, Step: m["a"], Steps: m}
	}
	killedInB := "start a fuss-1\nend a fuss-1\nstart b fuss-1\nretry fuss-1\n"
	tests := []struct {
		name     string
		snapshot runStatus
		final    runStatus
		rec      string
		exit     int
		want     []string // verdict prefixes, in order
	}{
		{"clean pass", st("Failed", "a", "succeeded", "b", "failed"), st("Succeeded", "a", "succeeded", "b", "succeeded"),
			killedInB + "start b fuss-1\nend b fuss-1\n", 0, nil},
		{"unconfirmed step re-run is fine", st("Failed", "a", "failed", "b", "failed"), st("Succeeded", "a", "succeeded", "b", "succeeded"),
			"start a fuss-1\nretry fuss-1\nstart a fuss-1\nend a fuss-1\nstart b fuss-1\nend b fuss-1\n", 0, nil},
		// The re-run of a is cut short: a start line but no end line.
		{"reran confirmed step", st("Failed", "a", "succeeded", "b", "failed"), st("Succeeded", "a", "succeeded", "b", "succeeded"),
			killedInB + "start a fuss-1\nstart b fuss-1\nend b fuss-1\n", 0, []string{"RERAN CONFIRMED STEP"}},
		{"start line of another run", st("Failed", "a", "succeeded", "b", "failed"), st("Succeeded", "a", "succeeded", "b", "succeeded"),
			killedInB + "start a fuss-10\nstart b fuss-1\nend b fuss-1\n", 0, nil},
		{"reverted", st("Failed", "a", "succeeded", "b", "failed"), st("Failed", "a", "failed", "b", "failed"),
			killedInB, 1, []string{"REVERTED", "RETRY DID NOT COMPLETE"}},
		{"false success on b", st("Failed", "a", "succeeded", "b", "failed"), st("Succeeded", "a", "succeeded", "b", "succeeded"),
			killedInB, 0, []string{"FALSE SUCCESS"}},
		{"retry exited non-zero", st("Failed", "a", "succeeded", "b", "failed"), st("Failed", "a", "succeeded", "b", "failed"),
			killedInB + "start b fuss-1\n", 1, []string{"RETRY DID NOT COMPLETE"}},
		{"exit 0 but run not succeeded", st("Failed", "a", "succeeded", "b", "failed"), st("Running", "a", "succeeded", "b", "running"),
			killedInB + "start b fuss-1\n", 0, []string{"RETRY DID NOT COMPLETE"}},
	}
	for _, tt := range tests {
		got := checkRetry(tt.snapshot, tt.final, tt.rec, id, tt.exit)
		ok := len(got) == len(tt.want)
		for i := 0; ok && i < len(got); i++ {
			ok = strings.HasPrefix(got[i].Error(), tt.want[i])
		}
		if !ok {
			t.Errorf("%s: got %v, want %v", tt.name, got, tt.want)
		}
	}
}
