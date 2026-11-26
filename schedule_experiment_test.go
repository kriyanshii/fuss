package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestFussRefusesTheWrongScheduleExperiment is the outer check around the
// schedule runner. A legal client history is not an experiment pass.
func TestFussRefusesTheWrongScheduleExperiment(t *testing.T) {
	hold := buildTopologyHold(t)
	repkv := buildRepkvChecker(t)

	t.Run("withhold the cut", func(t *testing.T) {
		cr, out := runScheduleControl(t, hold, repkv, scheduleControl{
			fire:     false,
			timeout:  "200ms",
			schedule: "after",
			initial:  "",
			history: func(time.Time) string {
				return afterHistory(time.Date(2026, 11, 26, 0, 0, 0, 0, time.UTC), "v", "v")
			},
		})
		if cr.Verdict != "UNKNOWN" {
			t.Fatalf("verdict %s want UNKNOWN problems %v\n%s", cr.Verdict, cr.Problems, out)
		}
		if !strings.Contains(problemText(cr), "trigger did not fire") {
			t.Fatalf("problems %v", cr.Problems)
		}
		if !strings.Contains(out, "history legal\n") || !strings.Contains(out, "experiment pass\n") {
			t.Fatalf("check out:\n%s", out)
		}
	})

	t.Run("pending misses the window", func(t *testing.T) {
		var cutFired bool
		cr, out := runScheduleControl(t, hold, repkv, scheduleControl{
			fire:     true,
			timeout:  "5s",
			schedule: "pending",
			initial:  "",
			history: func(cut time.Time) string {
				cutFired = true
				return afterHistory(cut, "v", "v")
			},
		})
		if !cutFired {
			t.Fatal("cut timestamp was not used")
		}
		if cr.Verdict != "UNKNOWN" {
			t.Fatalf("verdict %s want UNKNOWN problems %v\n%s", cr.Verdict, cr.Problems, out)
		}
		if strings.Contains(problemText(cr), "trigger did not fire") {
			t.Fatalf("cut was withheld: %v", cr.Problems)
		}
		for _, want := range []string{
			"experiment unknown reason=schedule_miss\n",
			"schedule pending missed\n",
			"history legal\n",
			"window after\n",
			"read after cut\n",
		} {
			if !strings.Contains(out, want) {
				t.Fatalf("missing %q\n%s", want, out)
			}
		}
	})

	t.Run("stale read", func(t *testing.T) {
		cr, out := runScheduleControl(t, hold, repkv, scheduleControl{
			fire:     true,
			timeout:  "5s",
			schedule: "after",
			initial:  "0",
			history: func(cut time.Time) string {
				return afterHistory(cut, "1", "0")
			},
		})
		if cr.Verdict != "ILLEGAL" {
			t.Fatalf("verdict %s want ILLEGAL problems %v\n%s", cr.Verdict, cr.Problems, out)
		}
		for _, want := range []string{
			"experiment illegal\n",
			"schedule after occurred\n",
			"history illegal\n",
			"read after cut\n",
		} {
			if !strings.Contains(out, want) {
				t.Fatalf("missing %q\n%s", want, out)
			}
		}
	})
}

type scheduleControl struct {
	fire     bool
	timeout  string
	schedule string
	initial  string
	history  func(cut time.Time) string
}

func runScheduleControl(t *testing.T, hold, repkv string, c scheduleControl) (CheckResult, string) {
	t.Helper()
	cfg := filepath.Join(t.TempDir(), "schedule.yaml")
	body := fmt.Sprintf(`
name: schedule-control
binary:
  default: hold
  extra: [%q]
topology:
  nodes: 3
  data_dir: "${dir}/node${i}"
  ports:
    base: 0
  peers:
    port_name: raft
    via_proxy: true
  start: [%q, "${data_dir}", "${port}", "${peers}"]
  ready:
    file: "${data_dir}/ready"
    contains: READY
    timeout: 2s
  stop_grace: 200ms
fault:
  isolate: node_0
  after:
    file:
      glob: "${dir}/cut_now"
      contains: NOW
      poll: 10ms
      timeout: %s
check:
  command:
    command: [%q, "-check-history", "${dir}/history.jsonl", "-porcupine", "-schedule", %q, "-initial", %q]
    as: out
  rules:
    - if: { out_contains: "experiment unknown" }
      verdict: UNKNOWN
    - if: { out_contains: "experiment illegal" }
      verdict: ILLEGAL
    - if: { out_contains: "experiment pass" }
      verdict: PASS
    - else: FAIL
`, hold, hold, c.timeout, repkv, c.schedule, c.initial)
	if err := os.WriteFile(cfg, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	a, err := LoadYAMLTarget(cfg)
	if err != nil {
		t.Fatal(err)
	}
	bin, _, err := a.ResolveBinary("")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	at, err := a.Init(1, dir, bin)
	if err != nil {
		t.Fatal(err)
	}
	log, err := os.Create(filepath.Join(dir, "log"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { log.Close() })
	holdProc, err := a.Start(at, log)
	if err != nil {
		t.Fatal(err)
	}
	rt := at.Data()["topology"].(*topologyRuntime)
	t.Cleanup(func() {
		a.stopTopology(rt)
		if holdProc.Process != nil {
			_ = holdProc.Process.Kill()
			_, _ = holdProc.Process.Wait()
		}
	})
	if !rt.AllReady {
		t.Fatal("expected all ready")
	}
	var cut time.Time
	if c.fire {
		if err := os.WriteFile(filepath.Join(dir, "cut_now"), []byte("NOW\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		cut = waitCutTime(t, dir)
	}
	if err := os.WriteFile(filepath.Join(dir, "history.jsonl"), []byte(c.history(cut)), 0o644); err != nil {
		t.Fatal(err)
	}
	cr := a.CheckNamed(at, true, "topology")
	vals, _ := at.Data()["check_vals"].(map[string]any)
	out, _ := vals["out"].(string)
	return cr, out
}

func waitCutTime(t *testing.T, dir string) time.Time {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(filepath.Join(dir, "isolate_applied.json"))
		if err == nil {
			var doc isolateAppliedDoc
			if json.Unmarshal(b, &doc) == nil && doc.TriggerFired && doc.LinksCut > 0 && doc.CutTime != "" {
				at, err := time.Parse(time.RFC3339Nano, doc.CutTime)
				if err != nil {
					t.Fatal(err)
				}
				return at
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("cut did not land")
	return time.Time{}
}

func afterHistory(cut time.Time, wrote, read string) string {
	return strings.Join([]string{
		histLine(map[string]any{
			"kind": "client", "id": "w1", "phase": "start", "op": "write",
			"key": "1-0", "value": wrote, "at": stamp(cut.Add(-2 * time.Second)),
		}),
		histLine(map[string]any{
			"kind": "client", "id": "w1", "phase": "end", "op": "write",
			"observe": "ok", "at": stamp(cut.Add(-time.Second)),
		}),
		histLine(map[string]any{
			"kind": "cut", "target": "node_0", "direction": "both",
			"links_cut": 4, "trigger_fired": true, "at": stamp(cut),
		}),
		histLine(map[string]any{
			"kind": "client", "id": "r1", "phase": "start", "op": "read",
			"key": "1-0", "from": "node_1", "at": stamp(cut.Add(time.Second)),
		}),
		histLine(map[string]any{
			"kind": "client", "id": "r1", "phase": "end", "op": "read",
			"observe": "ok", "value": read, "at": stamp(cut.Add(2 * time.Second)),
		}),
	}, "\n") + "\n"
}

func histLine(v map[string]any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func stamp(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

func problemText(cr CheckResult) string {
	parts := make([]string, len(cr.Problems))
	for i, p := range cr.Problems {
		parts[i] = p.Error()
	}
	return strings.Join(parts, "\n")
}

func buildRepkvChecker(t *testing.T) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "repkv")
	cmd := exec.Command("go", "build", "-o", out, "./examples/repkv")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build repkv: %v\n%s", err, b)
	}
	return out
}
