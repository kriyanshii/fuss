package main

import (
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"fuss/history"
)

func TestFaultDuringInflight(t *testing.T) {
	ops := []history.OpRecord{
		{CallNS: 10, ReturnNS: 20, Status: history.StatusOK},
		{CallNS: 30, ReturnNS: 40, Status: history.StatusOK},
	}
	overlap := []NemesisEvent{
		{TimestampNS: 10, Action: "kill_node"},
		{TimestampNS: 20, Action: "isolate_node"},
		{TimestampNS: 15, Action: "pause_node"},
		{TimestampNS: 30, Action: "partition_halves"},
		{TimestampNS: 40, Action: "isolate_client"},
	}
	for _, ev := range overlap {
		if !faultDuringInflight(ops, []NemesisEvent{ev}) {
			t.Fatalf("timestamp %d should overlap", ev.TimestampNS)
		}
	}
	outside := []NemesisEvent{
		{TimestampNS: 9, Action: "kill_node"},
		{TimestampNS: 21, Action: "kill_node"},
		{TimestampNS: 15, Action: "heal_all", Details: "recovery"},
		{TimestampNS: 15, Action: "kill_node", Details: "error=dead"},
		{TimestampNS: 15, Action: "isolate_node", Details: "no peer links"},
	}
	for _, ev := range outside {
		if faultDuringInflight(ops, []NemesisEvent{ev}) {
			t.Fatalf("timestamp %d action %s details %q should not overlap", ev.TimestampNS, ev.Action, ev.Details)
		}
	}
	if faultDuringInflight(nil, overlap) {
		t.Fatal("no ops")
	}
	inverted := []history.OpRecord{{CallNS: 50, ReturnNS: 40, Status: history.StatusOK}}
	if faultDuringInflight(inverted, []NemesisEvent{{TimestampNS: 45, Action: "pause_node"}}) {
		t.Fatal("inverted interval")
	}
	point := []history.OpRecord{{CallNS: 7, ReturnNS: 7, Status: history.StatusInfo}}
	if !faultDuringInflight(point, []NemesisEvent{{TimestampNS: 7, Action: "kill_node"}}) {
		t.Fatal("point interval")
	}
}

func TestInfoOpsObserved(t *testing.T) {
	if infoOpsObserved(nil) {
		t.Fatal("nil")
	}
	if infoOpsObserved([]history.OpRecord{{Status: history.StatusOK}, {Status: history.StatusFail}}) {
		t.Fatal("no info")
	}
	if !infoOpsObserved([]history.OpRecord{{Status: history.StatusOK}, {Status: history.StatusInfo}}) {
		t.Fatal("info")
	}
}

func TestFaultActionFired(t *testing.T) {
	events := []NemesisEvent{
		{Action: "isolate_node", Details: "no peer links"},
		{Action: "kill_node", Details: "error=dead"},
		{Action: "heal_all", Details: "recovery"},
		{Action: "pause_node", Details: "pause_duration=10ms"},
		{Action: "isolate_client", Details: "mode=reset"},
	}
	if faultActionFired(events, "isolate_node") {
		t.Fatal("noop isolate")
	}
	if faultActionFired(events, "kill_node") {
		t.Fatal("kill error")
	}
	if !faultActionFired(events, "heal_all") {
		t.Fatal("recovery heal")
	}
	if !faultActionFired(events, "pause_node") {
		t.Fatal("pause")
	}
	if !faultActionFired(events, "isolate_client") {
		t.Fatal("isolate client")
	}
	if faultActionFired(events, "partition_halves") {
		t.Fatal("absent")
	}
}

func TestLeaderChanged(t *testing.T) {
	if leaderChanged(nil) || leaderChanged([]string{""}) || leaderChanged([]string{"node_0", "node_0"}) {
		t.Fatal("unchanged")
	}
	if !leaderChanged([]string{"", "node_0"}) {
		t.Fatal("empty to leader")
	}
	if !leaderChanged([]string{"node_0", "node_1", "node_0"}) {
		t.Fatal("flip back")
	}
}

func TestUnmetSometimesCalibration(t *testing.T) {
	fault := []propertyTally{{
		Name: "killed", Kind: "sometimes", MinHits: 1, Fault: true, Hits: 0, Trials: 3,
	}}
	if len(unmetSometimes(fault, false)) != 1 {
		t.Fatal("expected unmet fault")
	}
	if unmetSometimes(fault, true) != nil {
		t.Fatal("calibration should skip a fault assertion")
	}
	info := []propertyTally{{
		Name: "saw_info", Kind: "sometimes", MinHits: 2, Fault: false, Hits: 1, Trials: 3,
	}}
	got := unmetSometimes(info, true)
	if len(got) != 1 || got[0].Name != "saw_info" {
		t.Fatalf("non-fault still unmet in calibration: %+v", got)
	}
	info[0].Hits = 2
	if unmetSometimes(info, false) != nil {
		t.Fatal("min_hits met")
	}
	always := propertyTally{Name: "lin", Kind: "always", Hits: 2, Trials: 2}
	if ok, _ := always.passed(false); !ok {
		t.Fatal("always passed")
	}
	always.Hits = 1
	if ok, _ := always.passed(false); ok {
		t.Fatal("always missed a trial")
	}
}

func TestLoadProperties(t *testing.T) {
	path := writeYAML(t, `
name: prop-load
workload:
  driver: http_kv
  targets: "127.0.0.1:1"
  get:
    url: "http://${target}/k"
topology:
  nodes: 1
nemesis:
  duration: 1ms
  interval: 1ms
  actions:
    - { type: heal_all }
properties:
  always:
    - name: lin
      porcupine:
        timeout: 1s
  eventually:
    - name: back
      eventually_available:
        n: 1
  sometimes:
    - name: saw_info
      info_ops_observed: {}
    - fault_action_fired:
        action: kill_node
`)
	a, err := LoadYAMLTarget(path)
	if err != nil {
		t.Fatal(err)
	}
	if a.cfg.Properties.Always[0].Porcupine == nil || a.cfg.Properties.Always[0].Porcupine.timeout != time.Second {
		t.Fatalf("porcupine timeout %+v", a.cfg.Properties.Always[0].Porcupine)
	}
	if a.cfg.Properties.Sometimes[0].MinHits != 1 {
		t.Fatalf("min_hits %d", a.cfg.Properties.Sometimes[0].MinHits)
	}
	if a.cfg.Properties.Sometimes[1].Name != "fault_action_fired_kill_node" {
		t.Fatalf("name %q", a.cfg.Properties.Sometimes[1].Name)
	}
	bad := writeYAML(t, `
name: prop-bad
properties:
  sometimes:
    - name: nope
      porcupine: {}
`)
	if _, err := LoadYAMLTarget(bad); err == nil {
		t.Fatal("sometimes porcupine should be rejected")
	}
}

func TestSometimesUnmetAndHit(t *testing.T) {
	buildTopologyHold(t)
	restoreBundleGlobals(t)

	code, stdout, stderr := captureOutput(t, func() int {
		return runAdapter(loadPropAdapter(t, killPropertyYAML(false)), "", 1, 2, true, "")
	})
	if code != 1 || !strings.Contains(stderr, "UNMET_SOMETIMES killed hits=0 min_hits=1 trials=2") {
		t.Fatalf("exit %d\nstderr:\n%s\nstdout:\n%s", code, stderr, stdout)
	}
	if !strings.Contains(stdout, "UNMET_SOMETIMES") || !strings.Contains(stdout, "killed sometimes 0/2") {
		t.Fatalf("human summary missing:\n%s", stdout)
	}

	jsonOut = true
	code, stdout, stderr = captureOutput(t, func() int {
		return runAdapter(loadPropAdapter(t, killPropertyYAML(true)), "", 1, 2, true, "")
	})
	jsonOut = false
	if code != 0 {
		t.Fatalf("exit %d\nstderr:\n%s\nstdout:\n%s", code, stderr, stdout)
	}
	summary := summaryObject(t, stdout)
	props, _ := summary["properties"].([]any)
	if len(props) != 1 {
		t.Fatalf("summary %+v", summary)
	}
	item, _ := props[0].(map[string]any)
	if item["name"] != "killed" || item["kind"] != "sometimes" || item["passed"] != true {
		t.Fatalf("property %+v", item)
	}
	hits, _ := item["hits"].(float64)
	total, _ := item["total_trials"].(float64)
	if hits < 1 || total != 2 {
		t.Fatalf("hits=%v total=%v", hits, total)
	}
}

func TestSometimesFaultCalibration(t *testing.T) {
	buildTopologyHold(t)
	restoreBundleGlobals(t)
	body := `
name: prop-fault
binary:
  default: hold
  extra: ["./testdata/yaml/topology/hold"]
topology:
  nodes: 1
  data_dir: "${dir}/node${i}"
  ports: { base: 0 }
  start: ["${binary}", "${data_dir}"]
  ready:
    file: "${data_dir}/ready"
    contains: READY
    timeout: 2s
  stop_grace: 300ms
nemesis:
  warmup: 5ms
  duration: 20ms
  interval: 20ms
  quiet_period: 10ms
  actions:
    - { type: heal_all, weight: 1 }
properties:
  sometimes:
    - name: killed
      min_hits: 1
      fault_action_fired:
        action: kill_node
`
	code, stdout, stderr := captureOutput(t, func() int {
		return runAdapter(loadPropAdapter(t, body), "", 1, 2, true, "")
	})
	if code != 1 || !strings.Contains(stderr, "UNMET_SOMETIMES killed hits=0 min_hits=1 trials=2") {
		t.Fatalf("exit %d\nstderr:\n%s\nstdout:\n%s", code, stderr, stdout)
	}
	code, stdout, stderr = captureOutput(t, func() int {
		return runAdapter(loadPropAdapter(t, body), "", 1, 2, false, "")
	})
	if code != 0 || !strings.Contains(stderr, "SOMETIMES killed skipped (calibration, -kill=false)") {
		t.Fatalf("exit %d\nstderr:\n%s\nstdout:\n%s", code, stderr, stdout)
	}
	if !strings.Contains(stdout, "skipped (calibration, -kill=false)") {
		t.Fatalf("human summary:\n%s", stdout)
	}
}

func killPropertyYAML(fireKill bool) string {
	action := `{ type: heal_all, weight: 1 }`
	if fireKill {
		action = `{ type: kill_node, target: "0", restart_delay: 10ms, weight: 1 }`
	}
	return `
name: prop-kill
binary:
  default: hold
  extra: ["./testdata/yaml/topology/hold"]
topology:
  nodes: 1
  data_dir: "${dir}/node${i}"
  ports: { base: 0 }
  start: ["${binary}", "${data_dir}"]
  ready:
    file: "${data_dir}/ready"
    contains: READY
    timeout: 2s
  stop_grace: 300ms
nemesis:
  warmup: 5ms
  duration: 20ms
  interval: 20ms
  quiet_period: 200ms
  actions:
    - ` + action + `
properties:
  sometimes:
    - name: killed
      min_hits: 1
      fault_action_fired:
        action: kill_node
`
}

func loadPropAdapter(t *testing.T, body string) *YAMLAdapter {
	t.Helper()
	a, err := LoadYAMLTarget(writeYAML(t, body))
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func restoreBundleGlobals(t *testing.T) {
	t.Helper()
	prevBundle := bundleRun
	prevJSON := jsonOut
	prevEDN := exportEDNPath
	t.Cleanup(func() {
		bundleRun = prevBundle
		jsonOut = prevJSON
		exportEDNPath = prevEDN
	})
	bundleRun = bundleCLI{}
	exportEDNPath = ""
	jsonOut = false
}

func captureOutput(t *testing.T, fn func() int) (int, string, string) {
	t.Helper()
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldOut, oldErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = outW, errW
	defer func() {
		os.Stdout, os.Stderr = oldOut, oldErr
	}()
	code := fn()
	_ = outW.Close()
	_ = errW.Close()
	outB, _ := io.ReadAll(outR)
	errB, _ := io.ReadAll(errR)
	return code, string(outB), string(errB)
}

func summaryObject(t *testing.T, stdout string) map[string]any {
	t.Helper()
	var summary map[string]any
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			continue
		}
		if m["type"] == "summary" {
			summary = m
		}
	}
	if summary == nil {
		t.Fatalf("no summary in\n%s", stdout)
	}
	return summary
}
