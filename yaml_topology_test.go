package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
	ev := rt.isolateEvidence()
	if ev["trigger_fired"] != true {
		t.Fatalf("evidence trigger_fired=%v", ev["trigger_fired"])
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
