package history

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPorcupineInfoOpenUntilEnd(t *testing.T) {
	// The info put times out at 20, but the old value is still visible at 28
	// and the new value shows up at 40. That is legal only if the timed-out
	// put stays open until the history ends.
	open := []OpRecord{
		{Process: 1, Op: OpPut, Key: "k", Value: "v1", CallNS: 0, ReturnNS: 10, Status: StatusOK},
		{Process: 2, Op: OpPut, Key: "k", Value: "v2", CallNS: 15, ReturnNS: 20, Status: StatusInfo},
		{Process: 3, Op: OpGet, Key: "k", Value: "v1", CallNS: 22, ReturnNS: 28, Status: StatusOK},
		{Process: 4, Op: OpGet, Key: "k", Value: "v2", CallNS: 40, ReturnNS: 50, Status: StatusOK},
	}
	if got := CheckPorcupine(open, time.Second); got.Kind != PorcupinePass {
		t.Fatalf("open info put: %s", got.Kind)
	}

	// A value cannot be read before its put was invoked, even if that put
	// later times out.
	early := []OpRecord{
		{Process: 1, Op: OpGet, Key: "k", Value: "v2", CallNS: 6, ReturnNS: 8, Status: StatusOK},
		{Process: 2, Op: OpPut, Key: "k", Value: "v2", CallNS: 10, ReturnNS: 12, Status: StatusInfo},
	}
	if got := CheckPorcupine(early, time.Second); got.Kind != PorcupineIllegal {
		t.Fatalf("get before invoke: %s", got.Kind)
	}

	// A definite failed put did not take effect.
	failed := []OpRecord{
		{Process: 1, Op: OpPut, Key: "k", Value: "v2", CallNS: 0, ReturnNS: 10, Status: StatusFail},
		{Process: 2, Op: OpGet, Key: "k", Value: "", CallNS: 20, ReturnNS: 30, Status: StatusOK},
	}
	if got := CheckPorcupine(failed, time.Second); got.Kind != PorcupinePass {
		t.Fatalf("failed put then absent get: %s", got.Kind)
	}
	seen := []OpRecord{
		{Process: 1, Op: OpPut, Key: "k", Value: "v2", CallNS: 0, ReturnNS: 10, Status: StatusFail},
		{Process: 2, Op: OpGet, Key: "k", Value: "v2", CallNS: 20, ReturnNS: 30, Status: StatusOK},
	}
	if got := CheckPorcupine(seen, time.Second); got.Kind != PorcupineIllegal {
		t.Fatalf("failed put then value: %s", got.Kind)
	}
}

func TestPorcupineIllegalWritesHTML(t *testing.T) {
	ops := []OpRecord{
		{Process: 1, Op: OpPut, Key: "k", Value: "a", CallNS: 0, ReturnNS: 10, Status: StatusOK},
		{Process: 2, Op: OpGet, Key: "k", Value: "", CallNS: 20, ReturnNS: 30, Status: StatusOK},
	}
	report := CheckPorcupine(ops, time.Second)
	if report.Kind != PorcupineIllegal {
		t.Fatalf("kind %s", report.Kind)
	}
	if report.Err != nil {
		t.Fatal(report.Err)
	}
	if !strings.Contains(strings.ToLower(string(report.HTML)), "html") {
		t.Fatalf("html %d bytes", len(report.HTML))
	}
	path := filepath.Join(t.TempDir(), "porcupine.html")
	if err := report.WriteHTML(path); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.ToLower(string(body)), "html") {
		t.Fatalf("file %d bytes", len(body))
	}
}

func TestPorcupineTimeoutKeepsHTML(t *testing.T) {
	ops := make([]OpRecord, 0, 18)
	for i := 0; i < 18; i++ {
		ops = append(ops, OpRecord{
			Process:  i + 1,
			Op:       OpPut,
			Key:      "k",
			Value:    "v",
			CallNS:   int64(i),
			ReturnNS: 100,
			Status:   StatusInfo,
		})
	}
	report := CheckPorcupine(ops, time.Nanosecond)
	switch report.Kind {
	case PorcupineTimeout:
		if len(report.HTML) == 0 {
			t.Fatal("timeout produced no html")
		}
		path := filepath.Join(t.TempDir(), "sub", "porcupine.html")
		if err := report.WriteHTML(path); err != nil {
			t.Fatal(err)
		}
	case PorcupinePass, PorcupineIllegal:
		// A fast machine can finish before the timeout. The illegal path
		// covers VisualizePath; this case only forbids a silent timeout.
	default:
		t.Fatalf("kind %s", report.Kind)
	}
}

func TestCheckSet(t *testing.T) {
	ops := []OpRecord{
		{Op: OpPut, Key: "k", Value: "a", CallNS: 1, ReturnNS: 10, Status: StatusOK},
		{Op: OpPut, Key: "k", Value: "b", CallNS: 11, ReturnNS: 20, Status: StatusOK},
		{Op: OpPut, Key: "k", Value: "c", CallNS: 21, ReturnNS: 30, Status: StatusInfo},
	}
	if kind, _ := CheckSet(ops, []Observation{{Key: "k", Value: "b", Status: StatusOK}}); kind != "pass" {
		t.Fatalf("latest: %s", kind)
	}
	if kind, _ := CheckSet(ops, []Observation{{Key: "k", Value: "c", Status: StatusOK}}); kind != "pass" {
		t.Fatalf("info overwrite: %s", kind)
	}
	if kind, _ := CheckSet(ops, []Observation{{Key: "k", Value: "a", Status: StatusOK}}); kind != "fail" {
		t.Fatalf("stale: %s", kind)
	}
	if kind, _ := CheckSet(ops, []Observation{{Key: "k", Absent: true, Status: StatusOK}}); kind != "fail" {
		t.Fatalf("absent: %s", kind)
	}
	if kind, _ := CheckSet(nil, nil); kind != "pass" {
		t.Fatalf("no writes: %s", kind)
	}
	if kind, _ := CheckSet(ops, []Observation{{Key: "k", Status: StatusInfo}}); kind != "unknown" {
		t.Fatalf("indeterminate: %s", kind)
	}
}

func TestCheckEventually(t *testing.T) {
	ops := []OpRecord{
		{Process: 1, Op: OpGet, CallNS: 50, Status: StatusOK},
		{Process: 1, Op: OpPut, CallNS: 100, Status: StatusFail},
		{Process: 2, Op: OpGet, CallNS: 110, Status: StatusOK},
		{Process: 1, Op: OpPut, CallNS: 120, Status: StatusOK},
		{Process: 2, Op: OpGet, CallNS: 130, Status: StatusFail},
		{Process: 1, Op: OpGet, CallNS: 200, Status: StatusOK},
	}
	if kind, _ := CheckEventually(ops, 100, 200, 2); kind != "pass" {
		t.Fatalf("streak 2: %s", kind)
	}
	if kind, _ := CheckEventually(ops, 100, 200, 3); kind != "fail" {
		t.Fatalf("streak 3: %s", kind)
	}
	if kind, _ := CheckEventually(ops, 100, 200, 0); kind != "pass" {
		t.Fatalf("default n: %s", kind)
	}
	if kind, _ := CheckEventually(nil, 100, 200, 1); kind != "fail" {
		t.Fatalf("empty window: %s", kind)
	}
}
