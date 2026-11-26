package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func buildTopologyHold(t *testing.T) string {
	t.Helper()
	dir := filepath.Join("testdata", "yaml", "topology")
	out := filepath.Join(dir, "hold")
	cmd := exec.Command("go", "build", "-o", "hold", "hold.go")
	cmd.Dir = dir
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build hold: %v\n%s", err, b)
	}
	t.Cleanup(func() { os.Remove(out) })
	abs, err := filepath.Abs(out)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(abs); err != nil {
		t.Fatal(err)
	}
	return abs
}

func pidAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil
}

func TestYAMLTopologyThreeAliveThenStopped(t *testing.T) {
	buildTopologyHold(t)
	a, err := LoadYAMLTarget("testdata/yaml/topology/target.yaml")
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
	defer log.Close()

	hold, err := a.Start(at, log)
	if err != nil {
		t.Fatal(err)
	}
	rt, ok := at.Data()["topology"].(*topologyRuntime)
	if !ok || rt == nil {
		t.Fatal("missing topology runtime")
	}
	t.Cleanup(func() {
		a.stopTopology(rt)
		if hold.Process != nil {
			_ = hold.Process.Kill()
			_ = hold.Wait()
		}
	})
	if !rt.AllReady {
		t.Fatal("expected all nodes ready")
	}
	if len(rt.Nodes) != 3 {
		t.Fatalf("nodes=%d", len(rt.Nodes))
	}
	pids := make([]int, 0, 3)
	for _, n := range rt.Nodes {
		if n == nil || n.PID <= 0 {
			t.Fatalf("bad node %+v", n)
		}
		if !pidAlive(n.PID) {
			t.Fatalf("pid %d not alive after start", n.PID)
		}
		pids = append(pids, n.PID)
	}
	// Same moment: all three still alive together.
	for _, pid := range pids {
		if !pidAlive(pid) {
			t.Fatalf("pid %d died before peers checked", pid)
		}
	}

	_ = hold.Process.Kill()
	_ = hold.Wait()

	cr := a.CheckNamed(at, true, "topology")
	if cr.Verdict != "PASS" {
		t.Fatalf("verdict %s problems %v", cr.Verdict, cr.Problems)
	}
	for _, pid := range pids {
		deadline := time.Now().Add(2 * time.Second)
		for pidAlive(pid) && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if pidAlive(pid) {
			t.Fatalf("pid %d still alive after check stop", pid)
		}
	}
	ev := rt.evidence()
	if len(ev) != 3 {
		t.Fatalf("evidence nodes=%d", len(ev))
	}
	for _, m := range ev {
		if m["pid"] == nil || m["pid"] == 0 {
			t.Fatalf("evidence missing pid: %v", m)
		}
		if m["start_time"] == "" {
			t.Fatalf("evidence missing start_time: %v", m)
		}
		if m["exit_status"] == nil {
			t.Fatalf("evidence missing exit_status: %v", m)
		}
		if m["stop"] == "" {
			t.Fatalf("evidence missing stop: %v", m)
		}
	}
}

func TestYAMLTopologyNeverReadyUNKNOWN(t *testing.T) {
	buildTopologyHold(t)
	a, err := LoadYAMLTarget("testdata/yaml/topology/never_ready.yaml")
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
	defer log.Close()

	hold, err := a.Start(at, log)
	if err != nil {
		t.Fatal(err)
	}
	rt := at.Data()["topology"].(*topologyRuntime)
	t.Cleanup(func() {
		a.stopTopology(rt)
		if hold.Process != nil {
			_ = hold.Process.Kill()
			_ = hold.Wait()
		}
	})
	if rt.AllReady {
		t.Fatal("expected not all ready")
	}
	_ = hold.Process.Kill()
	_ = hold.Wait()

	cr := a.CheckNamed(at, true, "topology")
	if cr.Verdict != "UNKNOWN" {
		t.Fatalf("verdict %s want UNKNOWN problems %v", cr.Verdict, cr.Problems)
	}
}

func TestYAMLWithoutTopologyUnchanged(t *testing.T) {
	a, err := LoadYAMLTarget("testdata/yaml/counts/target.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if a.cfg.Topology.Nodes != 0 {
		t.Fatalf("topology.nodes=%d", a.cfg.Topology.Nodes)
	}
}

func waitFileHas(t *testing.T, path, want string, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(path)
		if err == nil && strings.Contains(string(b), want) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %q in %s", want, path)
}

func readPings(dir string) map[string]bool {
	b, err := os.ReadFile(filepath.Join(dir, "pings"))
	if err != nil {
		return nil
	}
	out := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		parts := strings.Fields(line)
		if len(parts) == 2 {
			out[parts[0]] = parts[1] == "ok"
		}
	}
	return out
}

func TestYAMLTopologyIsolateNode0(t *testing.T) {
	buildTopologyHold(t)
	a, err := LoadYAMLTarget("testdata/yaml/topology/isolate.yaml")
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
	defer log.Close()

	hold, err := a.Start(at, log)
	if err != nil {
		t.Fatal(err)
	}
	rt := at.Data()["topology"].(*topologyRuntime)
	t.Cleanup(func() {
		a.stopTopology(rt)
		if hold.Process != nil {
			_ = hold.Process.Kill()
			_ = hold.Wait()
		}
	})
	if !rt.AllReady {
		t.Fatal("expected all ready")
	}
	if len(rt.Links) != 6 {
		t.Fatalf("links=%d want 6", len(rt.Links))
	}

	// Wait until every node can reach both peers through proxies.
	for i := 0; i < 3; i++ {
		waitFileHas(t, filepath.Join(rt.Nodes[i].DataDir, "ping_ok"), "OK", 3*time.Second)
	}

	if err := os.WriteFile(filepath.Join(dir, "cut_now"), []byte("NOW\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		rt.mu.Lock()
		fired := rt.Isolate != nil && rt.Isolate.TriggerFired
		rt.mu.Unlock()
		if fired {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("isolate trigger did not fire")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Node 0 stays alive; pings involving 0 fail; 1↔2 still work.
	if !pidAlive(rt.Nodes[0].PID) {
		t.Fatal("node 0 died after isolate")
	}
	deadline = time.Now().Add(3 * time.Second)
	for {
		p0 := readPings(rt.Nodes[0].DataDir)
		p1 := readPings(rt.Nodes[1].DataDir)
		p2 := readPings(rt.Nodes[2].DataDir)
		n0fail := true
		for _, ok := range p0 {
			if ok {
				n0fail = false
			}
		}
		n1to0fail := true
		n2to0fail := true
		n1to2ok := false
		n2to1ok := false
		for addr, ok := range p1 {
			// find which is peer 0 vs 2 via link listen addrs
			for _, l := range rt.Links {
				if l.From == 1 && l.To == 0 && l.Listen == addr {
					n1to0fail = !ok
				}
				if l.From == 1 && l.To == 2 && l.Listen == addr {
					n1to2ok = ok
				}
			}
		}
		for addr, ok := range p2 {
			for _, l := range rt.Links {
				if l.From == 2 && l.To == 0 && l.Listen == addr {
					n2to0fail = !ok
				}
				if l.From == 2 && l.To == 1 && l.Listen == addr {
					n2to1ok = ok
				}
			}
		}
		if n0fail && n1to0fail && n2to0fail && n1to2ok && n2to1ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("post-cut pings: n0=%v n1=%v n2=%v", p0, p1, p2)
		}
		time.Sleep(50 * time.Millisecond)
	}

	if !rt.cutObserved() {
		t.Fatal("expected cut evidence on a link")
	}
	assertIsolateAppliedFile(t, dir, 4)
	ev := rt.isolateEvidence()
	if ev["trigger_fired"] != true {
		t.Fatalf("evidence trigger_fired=%v", ev["trigger_fired"])
	}
	if ev["proxy_mode"] != "per_pair" {
		t.Fatalf("proxy_mode=%v want per_pair", ev["proxy_mode"])
	}
	if ev["direction"] != "both" {
		t.Fatalf("direction=%v want both", ev["direction"])
	}
	if ev["links_cut"] != 4 {
		t.Fatalf("links_cut=%v want 4", ev["links_cut"])
	}
	if ev["links_total"] != 6 {
		t.Fatalf("links_total=%v want 6", ev["links_total"])
	}
	links, _ := ev["links"].([]map[string]any)
	if len(links) != 6 {
		t.Fatalf("evidence links=%d", len(links))
	}

	_ = hold.Process.Kill()
	_ = hold.Wait()
	cr := a.CheckNamed(at, true, "topology")
	if cr.Verdict != "PASS" {
		t.Fatalf("verdict %s problems %v", cr.Verdict, cr.Problems)
	}
}

func TestYAMLTopologyPerNodeBothRejected(t *testing.T) {
	_, err := LoadYAMLTarget("testdata/yaml/topology/isolate_per_node_both.yaml")
	if err == nil {
		t.Fatal("expected load error for per_node + direction both")
	}
	if !strings.Contains(err.Error(), "per_node proxies can't separate") {
		t.Fatalf("error=%v", err)
	}
}

func TestYAMLTopologyPerNodeInboundIsolate(t *testing.T) {
	buildTopologyHold(t)
	a, err := LoadYAMLTarget("testdata/yaml/topology/isolate_per_node.yaml")
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
	defer log.Close()

	hold, err := a.Start(at, log)
	if err != nil {
		t.Fatal(err)
	}
	rt := at.Data()["topology"].(*topologyRuntime)
	t.Cleanup(func() {
		a.stopTopology(rt)
		if hold.Process != nil {
			_ = hold.Process.Kill()
			_ = hold.Wait()
		}
	})
	if !rt.AllReady {
		t.Fatal("expected all ready")
	}
	if len(rt.Links) != 3 {
		t.Fatalf("links=%d want 3 (one inbound proxy per node)", len(rt.Links))
	}
	if rt.ProxyMode != "per_node" {
		t.Fatalf("ProxyMode=%q", rt.ProxyMode)
	}

	for i := 0; i < 3; i++ {
		waitFileHas(t, filepath.Join(rt.Nodes[i].DataDir, "ping_ok"), "OK", 3*time.Second)
	}

	if err := os.WriteFile(filepath.Join(dir, "cut_now"), []byte("NOW\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		rt.mu.Lock()
		fired := rt.Isolate != nil && rt.Isolate.TriggerFired
		rt.mu.Unlock()
		if fired {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("isolate trigger did not fire")
		}
		time.Sleep(20 * time.Millisecond)
	}

	if !pidAlive(rt.Nodes[0].PID) {
		t.Fatal("node 0 died after isolate")
	}

	var proxy0, proxy1, proxy2 string
	for _, l := range rt.Links {
		switch l.To {
		case 0:
			proxy0 = l.Listen
		case 1:
			proxy1 = l.Listen
		case 2:
			proxy2 = l.Listen
		}
	}
	if proxy0 == "" || proxy1 == "" || proxy2 == "" {
		t.Fatalf("missing proxies: %q %q %q", proxy0, proxy1, proxy2)
	}

	// Inbound-only: 1↔2 still work; dials to node 0's proxy fail; node 0 may
	// still dial out through survivors' proxies (asymmetric partition).
	deadline = time.Now().Add(3 * time.Second)
	for {
		p1 := readPings(rt.Nodes[1].DataDir)
		p2 := readPings(rt.Nodes[2].DataDir)
		to0fail := !p1[proxy0] && !p2[proxy0]
		s12ok := p1[proxy2] && p2[proxy1]
		if to0fail && s12ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("post-cut pings: n1=%v n2=%v (want to0 fail, 1↔2 ok)", p1, p2)
		}
		time.Sleep(50 * time.Millisecond)
	}

	cutCount := 0
	for _, l := range rt.Links {
		if l.cut {
			cutCount++
			if l.To != 0 {
				t.Fatalf("unexpected cut on proxy to node %d", l.To)
			}
		}
	}
	if cutCount != 1 {
		t.Fatalf("cutCount=%d want 1", cutCount)
	}

	ev := rt.isolateEvidence()
	if ev["proxy_mode"] != "per_node" || ev["direction"] != "inbound" {
		t.Fatalf("evidence mode/dir = %v %v", ev["proxy_mode"], ev["direction"])
	}
	if ev["links_cut"] != 1 || ev["links_total"] != 3 {
		t.Fatalf("links_cut=%v links_total=%v", ev["links_cut"], ev["links_total"])
	}

	_ = hold.Process.Kill()
	_ = hold.Wait()
	cr := a.CheckNamed(at, true, "topology")
	if cr.Verdict != "PASS" {
		t.Fatalf("verdict %s problems %v", cr.Verdict, cr.Problems)
	}
}

func TestYAMLTopologyIsolateTriggerNeverUNKNOWN(t *testing.T) {
	buildTopologyHold(t)
	a, err := LoadYAMLTarget("testdata/yaml/topology/isolate_never.yaml")
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
	defer log.Close()

	hold, err := a.Start(at, log)
	if err != nil {
		t.Fatal(err)
	}
	rt := at.Data()["topology"].(*topologyRuntime)
	t.Cleanup(func() {
		a.stopTopology(rt)
		if hold.Process != nil {
			_ = hold.Process.Kill()
			_ = hold.Wait()
		}
	})

	_ = hold.Process.Kill()
	_ = hold.Wait()
	cr := a.CheckNamed(at, true, "topology")
	if cr.Verdict != "UNKNOWN" {
		t.Fatalf("verdict %s want UNKNOWN problems %v", cr.Verdict, cr.Problems)
	}
	ev := rt.isolateEvidence()
	if ev["trigger_fired"] != false {
		t.Fatalf("trigger_fired=%v", ev["trigger_fired"])
	}
}

func TestIsolateScenarioUnknownReasonKept(t *testing.T) {
	buildTopologyHold(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "unknown.yaml")
	body := []byte(`
name: isolate-unknown-reason
binary:
  default: hold
  extra: ["./testdata/yaml/topology/hold"]
topology:
  nodes: 3
  data_dir: "${dir}/node${i}"
  ports:
    base: 0
  peers:
    port_name: raft
    via_proxy: true
  start: ["${binary}", "${data_dir}", "${port}", "${peers}"]
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
      timeout: 200ms
check:
  command:
    command: ["/bin/sh", "-c", "echo UNKNOWN reason=leader_not_node_0"]
    as: out
  rules:
    - if: { out_contains: "UNKNOWN reason=leader_not_node_0" }
      verdict: UNKNOWN
    - else: FAIL
`)
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	a, err := LoadYAMLTarget(path)
	if err != nil {
		t.Fatal(err)
	}
	bin, _, err := a.ResolveBinary("")
	if err != nil {
		t.Fatal(err)
	}
	trialDir := t.TempDir()
	at, err := a.Init(1, trialDir, bin)
	if err != nil {
		t.Fatal(err)
	}
	log, err := os.Create(filepath.Join(trialDir, "log"))
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	hold, err := a.Start(at, log)
	if err != nil {
		t.Fatal(err)
	}
	rt := at.Data()["topology"].(*topologyRuntime)
	t.Cleanup(func() {
		a.stopTopology(rt)
		if hold.Process != nil {
			_ = hold.Process.Kill()
			_ = hold.Wait()
		}
	})
	_ = hold.Process.Kill()
	_ = hold.Wait()
	cr := a.CheckNamed(at, true, "topology")
	if cr.Verdict != "UNKNOWN" {
		t.Fatalf("verdict %s want UNKNOWN problems %v", cr.Verdict, cr.Problems)
	}
	for _, p := range cr.Problems {
		if strings.Contains(p.Error(), "trigger did not fire") {
			t.Fatalf("scenario reason was replaced: %v", cr.Problems)
		}
	}
	vals, _ := at.Data()["check_vals"].(map[string]any)
	out, _ := vals["out"].(string)
	if !strings.Contains(out, "UNKNOWN reason=leader_not_node_0") {
		t.Fatalf("check out %q", out)
	}
}

func TestYAMLTopologyIsolateNodeFile(t *testing.T) {
	buildTopologyHold(t)
	a, err := LoadYAMLTarget("testdata/yaml/topology/isolate_node_file.yaml")
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
	defer log.Close()
	hold, err := a.Start(at, log)
	if err != nil {
		t.Fatal(err)
	}
	rt := at.Data()["topology"].(*topologyRuntime)
	t.Cleanup(func() {
		a.stopTopology(rt)
		if hold.Process != nil {
			_ = hold.Process.Kill()
			_ = hold.Wait()
		}
	})
	if !rt.AllReady {
		t.Fatal("expected all ready")
	}
	if err := os.WriteFile(filepath.Join(dir, "leader_node"), []byte("node_1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cut_now"), []byte("NOW\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		b, err := os.ReadFile(filepath.Join(dir, "isolate_applied.json"))
		if err == nil {
			var doc isolateAppliedDoc
			if json.Unmarshal(b, &doc) == nil && doc.TriggerFired && doc.Target == "node_1" && doc.LinksCut == 4 {
				rt.mu.Lock()
				target := rt.Isolate.Target
				rt.mu.Unlock()
				if target != "node_1" {
					t.Fatalf("runtime target %s", target)
				}
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("isolate_applied.json = %s (%v)", b, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestIsolateNodeAndNodeFileRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "target.yaml")
	body := []byte("name: both\nbinary: { default: true }\nfault:\n  isolate:\n    node: node_0\n    node_file: ${dir}/leader_node\n    direction: both\n")
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadYAMLTarget(path)
	if err == nil || !strings.Contains(err.Error(), "not both") {
		t.Fatal(err)
	}
}

func assertIsolateAppliedFile(t *testing.T, dir string, wantCut int) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "isolate_applied.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc isolateAppliedDoc
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Target != "node_0" || !doc.TriggerFired || doc.LinksCut != wantCut {
		t.Fatalf("isolate_applied.json = %+v", doc)
	}
}

func TestYAMLTopologyPauseNode0(t *testing.T) {
	buildTopologyHold(t)
	a, err := LoadYAMLTarget("testdata/yaml/topology/pause.yaml")
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
	defer log.Close()

	hold, err := a.Start(at, log)
	if err != nil {
		t.Fatal(err)
	}
	rt := at.Data()["topology"].(*topologyRuntime)
	pids := make([]int, 0, 3)
	for _, n := range rt.Nodes {
		pids = append(pids, n.PID)
	}
	t.Cleanup(func() {
		a.stopTopology(rt)
		if hold.Process != nil {
			_ = hold.Process.Kill()
			_ = hold.Wait()
		}
		for _, pid := range pids {
			if pidAlive(pid) {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})
	if !rt.AllReady {
		t.Fatal("expected all ready")
	}
	for i := 0; i < 3; i++ {
		waitFileHas(t, filepath.Join(rt.Nodes[i].DataDir, "ping_ok"), "OK", 3*time.Second)
	}

	if err := os.WriteFile(filepath.Join(dir, "cut_now"), []byte("NOW\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		rt.mu.Lock()
		paused := rt.Pause != nil && rt.Pause.Paused
		rt.mu.Unlock()
		if paused {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("pause did not fire")
		}
		time.Sleep(20 * time.Millisecond)
	}

	if !pidAlive(rt.Nodes[0].PID) {
		t.Fatal("paused node 0 died")
	}
	for _, i := range []int{1, 2} {
		if !pidAlive(rt.Nodes[i].PID) {
			t.Fatalf("node %d died after pause", i)
		}
	}

	// Node 0 stopped answering; 1↔2 still exchange pings.
	deadline = time.Now().Add(3 * time.Second)
	for {
		p1 := readPings(rt.Nodes[1].DataDir)
		p2 := readPings(rt.Nodes[2].DataDir)
		n1to0fail := false
		n2to0fail := false
		n1to2ok := false
		n2to1ok := false
		for addr, ok := range p1 {
			for _, l := range rt.Links {
				if l.From == 1 && l.To == 0 && l.Listen == addr {
					n1to0fail = !ok
				}
				if l.From == 1 && l.To == 2 && l.Listen == addr {
					n1to2ok = ok
				}
			}
		}
		for addr, ok := range p2 {
			for _, l := range rt.Links {
				if l.From == 2 && l.To == 0 && l.Listen == addr {
					n2to0fail = !ok
				}
				if l.From == 2 && l.To == 1 && l.Listen == addr {
					n2to1ok = ok
				}
			}
		}
		if n1to0fail && n2to0fail && n1to2ok && n2to1ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("pause ping shape: p1=%v p2=%v", p1, p2)
		}
		time.Sleep(50 * time.Millisecond)
	}

	ev := rt.pauseEvidence()
	if ev["target"] != "node_0" || ev["resume"] != "end" || ev["paused"] != true {
		t.Fatalf("pause evidence %#v", ev)
	}
	if _, ok := ev["pause_time"].(string); !ok {
		t.Fatalf("pause_time missing in %#v", ev)
	}

	_ = hold.Process.Kill()
	_ = hold.Wait()
	cr := a.CheckNamed(at, true, "topology")
	if cr.Verdict != "PASS" {
		t.Fatalf("verdict %s problems %v", cr.Verdict, cr.Problems)
	}
	for _, pid := range pids {
		if pidAlive(pid) {
			t.Fatalf("stray pid %d after teardown", pid)
		}
	}
}

func TestYAMLTopologyPerNodeOwnedIsolate(t *testing.T) {
	a, at, rt, hold, dir := startPerNodeOwnedTrial(t)
	if len(rt.Links) != 3 {
		t.Fatalf("links=%d want 3 (one inbound proxy per node)", len(rt.Links))
	}

	for i := 0; i < 3; i++ {
		waitFileHas(t, filepath.Join(rt.Nodes[i].DataDir, "ping_ok"), "OK", 5*time.Second)
	}
	proxy := proxyByDest(rt)
	if err := os.WriteFile(filepath.Join(dir, "cut_now"), []byte("NOW\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitIsolateFired(t, rt)

	if !pidAlive(rt.Nodes[0].PID) {
		t.Fatal("node 0 died after isolate")
	}
	deadline := time.Now().Add(8 * time.Second)
	for {
		p0 := readPings(rt.Nodes[0].DataDir)
		p1 := readPings(rt.Nodes[1].DataDir)
		p2 := readPings(rt.Nodes[2].DataDir)
		n0fail := !p0[proxy[1]] && !p0[proxy[2]]
		keep := !p1[proxy[0]] && p1[proxy[2]] && !p2[proxy[0]] && p2[proxy[1]]
		if n0fail && keep {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("post-cut pings: n0=%v n1=%v n2=%v", p0, p1, p2)
		}
		time.Sleep(50 * time.Millisecond)
	}

	ev := rt.isolateEvidence()
	if ev["proxy_mode"] != "per_node_owned" || ev["direction"] != "both" {
		t.Fatalf("evidence mode/dir = %v %v", ev["proxy_mode"], ev["direction"])
	}
	if ev["links_cut"] != 4 || ev["links_total"] != 6 {
		t.Fatalf("links_cut=%v links_total=%v", ev["links_cut"], ev["links_total"])
	}
	links, _ := ev["links"].([]map[string]any)
	if len(links) != 6 {
		t.Fatalf("evidence links=%d", len(links))
	}
	unk, _ := ev["unknown_owners"].([]map[string]any)
	if len(unk) != 0 {
		t.Fatalf("unknown owners %#v", unk)
	}
	by := map[[2]int]map[string]any{}
	for _, L := range links {
		from, _ := L["from"].(int)
		to, _ := L["to"].(int)
		by[[2]int{from, to}] = L
	}
	keptRefused := statField(by[[2]int{1, 2}], "refused_while_cut") + statField(by[[2]int{2, 1}], "refused_while_cut")
	keptClosed := statField(by[[2]int{1, 2}], "closed_by_cut") + statField(by[[2]int{2, 1}], "closed_by_cut")
	if keptRefused != 0 || keptClosed != 0 {
		t.Fatalf("kept 1↔2 counters refused=%d closed=%d; links=%v", keptRefused, keptClosed, links)
	}
	if statField(by[[2]int{0, 1}], "refused_while_cut") == 0 || statField(by[[2]int{0, 2}], "refused_while_cut") == 0 {
		t.Fatalf("cut sources did not record their own refuses: %v", links)
	}

	_ = hold.Process.Kill()
	_ = hold.Wait()
	cr := a.CheckNamed(at, true, "topology")
	if cr.Verdict != "PASS" {
		t.Fatalf("verdict %s problems %v", cr.Verdict, cr.Problems)
	}
}

func TestYAMLTopologyPerNodeOwnedUnknown(t *testing.T) {
	a, at, rt, hold, dir := startPerNodeOwnedTrial(t)
	for i := 0; i < 3; i++ {
		waitFileHas(t, filepath.Join(rt.Nodes[i].DataDir, "ping_ok"), "OK", 5*time.Second)
	}
	proxy := proxyByDest(rt)
	c, err := net.DialTimeout("tcp", proxy[0], time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	deadline := time.Now().Add(3 * time.Second)
	for {
		ev := rt.isolateEvidence()
		unk, _ := ev["unknown_owners"].([]map[string]any)
		if len(unk) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for unknown owner")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := os.WriteFile(filepath.Join(dir, "cut_now"), []byte("NOW\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitIsolateFired(t, rt)
	deadline = time.Now().Add(3 * time.Second)
	for {
		ev := rt.isolateEvidence()
		unk, _ := ev["unknown_owners"].([]map[string]any)
		cutUnk := false
		for _, u := range unk {
			if u["cut"] == true && u["to"] == 0 {
				cutUnk = true
			}
		}
		if cutUnk && ev["links_cut"] == 4 && ev["links_total"] == 6 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("evidence after cut: %#v", ev)
		}
		time.Sleep(20 * time.Millisecond)
	}

	_ = hold.Process.Kill()
	_ = hold.Wait()
	cr := a.CheckNamed(at, true, "topology")
	if cr.Verdict != "UNKNOWN" {
		t.Fatalf("verdict %s want UNKNOWN problems %v", cr.Verdict, cr.Problems)
	}
}

func startPerNodeOwnedTrial(t *testing.T) (*YAMLAdapter, AdapterTrial, *topologyRuntime, *exec.Cmd, string) {
	t.Helper()
	buildTopologyHold(t)
	a, err := LoadYAMLTarget("testdata/yaml/topology/isolate_per_node_owned.yaml")
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

	hold, err := a.Start(at, log)
	if err != nil {
		t.Fatal(err)
	}
	rt := at.Data()["topology"].(*topologyRuntime)
	t.Cleanup(func() {
		a.stopTopology(rt)
		if hold.Process != nil {
			_ = hold.Process.Kill()
			_ = hold.Wait()
		}
	})
	if !rt.AllReady {
		t.Fatal("expected all ready")
	}
	return a, at, rt, hold, dir
}

func proxyByDest(rt *topologyRuntime) map[int]string {
	out := map[int]string{}
	for _, l := range rt.Links {
		out[l.To] = l.Listen
	}
	return out
}

func statField(link map[string]any, field string) int64 {
	if link == nil {
		return -1
	}
	after, _ := link["stats_after"].(map[string]any)
	switch n := after[field].(type) {
	case int64:
		return n
	case int:
		return int64(n)
	default:
		return -1
	}
}

func waitIsolateFired(t *testing.T, rt *topologyRuntime) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		rt.mu.Lock()
		fired := rt.Isolate != nil && rt.Isolate.TriggerFired
		rt.mu.Unlock()
		if fired {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("isolate trigger did not fire")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestLoadYAMLPauseResumeReject(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.yaml")
	body := []byte(`
name: bad-pause
binary: { default: true }
topology:
  nodes: 1
  start: ["true"]
fault:
  pause: { node: node_0, resume: sometimes }
  after: { file: { glob: "${dir}/x", contains: X } }
check:
  rules: [{ else: PASS }]
`)
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadYAMLTarget(path)
	if err == nil || !strings.Contains(err.Error(), "resume") {
		t.Fatalf("want resume reject, got %v", err)
	}
}

const namedPortShape = `
name: named-ports
binary:
  default: sleep
  extra: ["/bin/sleep"]
topology:
  nodes: 3
  data_dir: "${dir}/node${i}"
  ports:
    base: 0
    names: [raft, http]
  proxy_mode: per_node_owned
  peers:
    port_name: raft
    via_proxy: true
  start:
    - /bin/sh
    - -c
    - |
      printf '%s\n' "${port}" > "${data_dir}/legacy_port"
      printf '%s\n' "${port_raft}" > "${data_dir}/raft_port"
      printf '%s\n' "${port_http}" > "${data_dir}/http_port"
      printf '%s\n' "${addr_raft}" > "${data_dir}/raft_addr"
      printf '%s\n' "${addr_http}" > "${data_dir}/http_addr"
      exec /bin/sleep 30
  ready:
    file: "${data_dir}/http_port"
    timeout: 5s
  stop_grace: 200ms
`

func TestTwoRqliteNamedPortCopiesDoNotCollide(t *testing.T) {
	cfg := t.TempDir()
	path := filepath.Join(cfg, "ports.yaml")
	if err := os.WriteFile(path, []byte(namedPortShape), 0o644); err != nil {
		t.Fatal(err)
	}
	type running struct {
		a    *YAMLAdapter
		rt   *topologyRuntime
		hold *exec.Cmd
		dir  string
	}
	dirs := []string{t.TempDir(), t.TempDir()}
	out := make([]running, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			a, err := LoadYAMLTarget(path)
			if err != nil {
				errs[i] = err
				return
			}
			logf, err := os.Create(filepath.Join(dirs[i], "log"))
			if err != nil {
				errs[i] = err
				return
			}
			defer logf.Close()
			at, err := a.Init(int64(i+1), dirs[i], "/bin/sleep")
			if err != nil {
				errs[i] = err
				return
			}
			hold, err := a.Start(at, logf)
			if err != nil {
				errs[i] = err
				return
			}
			rt, _ := at.Data()["topology"].(*topologyRuntime)
			out[i] = running{a: a, rt: rt, hold: hold, dir: dirs[i]}
		}(i)
	}
	wg.Wait()
	t.Cleanup(func() {
		for _, r := range out {
			if r.a != nil && r.rt != nil {
				r.a.stopTopology(r.rt)
			}
			if r.hold != nil && r.hold.Process != nil {
				_ = r.hold.Process.Kill()
				_, _ = r.hold.Process.Wait()
			}
		}
	})
	for i, err := range errs {
		if err != nil {
			t.Fatalf("copy %d: %v", i, err)
		}
	}

	seen := map[string]string{}
	take := func(label, v string) {
		t.Helper()
		if v == "" {
			t.Fatalf("%s empty", label)
		}
		if prev, ok := seen[v]; ok {
			t.Fatalf("port %s used by %s and %s", v, prev, label)
		}
		seen[v] = label
	}
	for copy, r := range out {
		if r.rt == nil || !r.rt.AllReady {
			t.Fatalf("copy %d not ready", copy)
		}
		raftAddr := map[int]string{}
		httpAddr := map[int]string{}
		for i := 0; i < 3; i++ {
			node := filepath.Join(r.dir, "node"+strconv.Itoa(i))
			legacy := strings.TrimSpace(readPortFile(t, filepath.Join(node, "legacy_port")))
			raft := strings.TrimSpace(readPortFile(t, filepath.Join(node, "raft_port")))
			http := strings.TrimSpace(readPortFile(t, filepath.Join(node, "http_port")))
			ra := strings.TrimSpace(readPortFile(t, filepath.Join(node, "raft_addr")))
			ha := strings.TrimSpace(readPortFile(t, filepath.Join(node, "http_addr")))
			if ra != "127.0.0.1:"+raft || ha != "127.0.0.1:"+http {
				t.Fatalf("copy %d node %d addrs raft=%s http=%s ports %s %s", copy, i, ra, ha, raft, http)
			}
			if legacy == raft || legacy == http || raft == http {
				t.Fatalf("copy %d node %d ports not distinct legacy=%s raft=%s http=%s", copy, i, legacy, raft, http)
			}
			prefix := fmt.Sprintf("copy%d-node%d", copy, i)
			take(prefix+"/legacy", legacy)
			take(prefix+"/raft", raft)
			take(prefix+"/http", http)
			raftAddr[i] = ra
			httpAddr[i] = ha
		}
		if len(r.rt.Links) != 3 {
			t.Fatalf("copy %d links %d", copy, len(r.rt.Links))
		}
		for _, l := range r.rt.Links {
			if l.Upstream != raftAddr[l.To] {
				t.Fatalf("copy %d link to %d upstream %s want %s", copy, l.To, l.Upstream, raftAddr[l.To])
			}
			if l.Upstream == httpAddr[l.To] {
				t.Fatalf("copy %d proxied http %s", copy, l.Upstream)
			}
		}
	}
}

func TestNamedPortDuplicateRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dup.yaml")
	body := strings.Replace(namedPortShape, "names: [raft, http]", "names: [raft, raft]", 1)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	a, err := LoadYAMLTarget(path)
	if err != nil {
		t.Fatal(err)
	}
	logf, err := os.Create(filepath.Join(dir, "log"))
	if err != nil {
		t.Fatal(err)
	}
	defer logf.Close()
	at, err := a.Init(1, dir, "/bin/sleep")
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.Start(at, logf)
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("want duplicate name error, got %v", err)
	}
}

func readPortFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
