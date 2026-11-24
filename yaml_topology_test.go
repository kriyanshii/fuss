package main

import (
	"os"
	"os/exec"
	"path/filepath"
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
