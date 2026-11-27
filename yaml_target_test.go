package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestExpandTemplates(t *testing.T) {
	got := expandTemplates("http://${addr}/k/${seed}-${n}", map[string]string{
		"addr": "127.0.0.1:18701",
		"seed": "1",
		"n":    "3",
	})
	want := "http://127.0.0.1:18701/k/1-3"
	if got != want {
		t.Fatalf("expandTemplates = %q, want %q", got, want)
	}
}

func TestParseAckKeys(t *testing.T) {
	got := parseAckKeys("ack planted\nack 1-0\n")
	if len(got) != 2 || got[0] != "planted" || got[1] != "1-0" {
		t.Fatalf("parseAckKeys = %v", got)
	}
}

func TestMatchYAMLRule(t *testing.T) {
	vals := map[string]any{"starts": 1, "items": 0, "status_out": "Result: Queued\n"}
	if !matchYAMLRule(map[string]any{"starts_eq": 1}, vals) {
		t.Fatal("starts_eq 1")
	}
	if !matchYAMLRule(map[string]any{"status_contains": "Queued", "items_eq": 0}, vals) {
		t.Fatal("status_contains should read status_out")
	}
	if matchYAMLRule(map[string]any{"starts_gte": 2}, vals) {
		t.Fatal("starts_gte 2 should miss")
	}
}

func TestWaitFileContains(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "ready.txt")
	go func() {
		time.Sleep(30 * time.Millisecond)
		os.WriteFile(p, []byte("READY"), 0o644)
	}()
	if err := waitFileContains(p, "READY", 5*time.Millisecond, time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestLoadYAMLFileKill(t *testing.T) {
	a, err := LoadYAMLTarget("testdata/yaml/filekill/target.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if a.cfg.Kill.At != "file" || a.cfg.Kill.File.Contains != "READY" {
		t.Fatalf("kill %+v", a.cfg.Kill)
	}
	if a.cfg.Kill.File.Poll == "" {
		t.Fatal("poll interval required")
	}
}

func TestYAMLFileKillFixture(t *testing.T) {
	a, err := LoadYAMLTarget("testdata/yaml/filekill/target.yaml")
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
	cmd, err := a.Start(at, log)
	if err != nil {
		t.Fatal(err)
	}
	waited, handled, err := a.WaitBeforeKill(at, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !handled || waited <= 0 {
		t.Fatalf("file kill not handled waited=%v", waited)
	}
	if err := kill(cmd); err != nil {
		t.Fatal(err)
	}
	cmd.Wait()
	cr := a.CheckNamed(at, true, "file")
	if cr.Verdict != "PASS" {
		t.Fatalf("verdict %s problems %v", cr.Verdict, cr.Problems)
	}
}

func TestYAMLStepsFixture(t *testing.T) {
	a, err := LoadYAMLTarget("testdata/yaml/steps/target.yaml")
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
	cmd, err := a.Start(at, log)
	if err != nil {
		t.Fatal(err)
	}
	if _, handled, err := a.WaitBeforeKill(at, 1); err != nil || !handled {
		t.Fatalf("wait file kill handled=%v err=%v", handled, err)
	}
	_ = kill(cmd)
	cmd.Wait()
	cr := a.CheckNamed(at, true, "steps")
	if cr.Verdict != "PASS" {
		t.Fatalf("verdict %s problems %v seq=%q", cr.Verdict, cr.Problems, mustRead(filepath.Join(dir, "seq.txt")))
	}
	seq := mustRead(filepath.Join(dir, "seq.txt"))
	if seq != "A\nB\nC\n" {
		t.Fatalf("seq %q", seq)
	}
}

func TestYAMLCountsRules(t *testing.T) {
	a, err := LoadYAMLTarget("testdata/yaml/counts/target.yaml")
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
	cmd, err := a.Start(at, log)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		b, _ := os.ReadFile(filepath.Join(dir, "runs.txt"))
		if strings.Contains(string(b), "start after") {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	_ = kill(cmd)
	cmd.Wait()
	cr := a.CheckNamed(at, true, "counts")
	if cr.Verdict != "PASS" {
		t.Fatalf("verdict %s problems %v", cr.Verdict, cr.Problems)
	}
}

func TestHraftdYAMLLoads(t *testing.T) {
	a, err := LoadYAMLTarget("examples/hraftd/hraftd.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if a.cfg.Topology.Nodes != 3 || !a.cfg.Topology.ClientViaProxy || !a.cfg.Topology.Peers.ViaProxy {
		t.Fatalf("topology %+v", a.cfg.Topology)
	}
	if a.cfg.Workload.Driver != "http_kv" || a.cfg.Workload.Get.ValueFrom != "${key}" {
		t.Fatalf("workload %+v", a.cfg.Workload)
	}
	if a.cfg.Workload.Workers != 4 || a.cfg.Workload.Keyspace != 3 || a.cfg.Workload.timeout != 1500*time.Millisecond {
		t.Fatalf("workload pace workers=%d keyspace=%d timeout=%s", a.cfg.Workload.Workers, a.cfg.Workload.Keyspace, a.cfg.Workload.timeout)
	}
	p := a.cfg.Properties
	if len(p.Always) != 2 || p.Always[0].Name != "porcupine" || p.Always[0].Porcupine.timeout != 10*time.Second || p.Always[1].Name != "set" {
		t.Fatalf("always %+v", p.Always)
	}
	if len(p.Eventually) != 1 || p.Eventually[0].Name != "eventually_available" || p.Eventually[0].Eventually.N != 5 {
		t.Fatalf("eventually %+v", p.Eventually)
	}
	if len(p.Sometimes) != 2 || p.Sometimes[0].Name != "fault_during_inflight" || p.Sometimes[0].MinHits != 1 {
		t.Fatalf("sometimes0 %+v", p.Sometimes)
	}
	if p.Sometimes[1].Name != "fault_action_fired_isolate_node" || p.Sometimes[1].MinHits != 1 || p.Sometimes[1].FaultActionFired.Action != "isolate_node" {
		t.Fatalf("sometimes1 %+v", p.Sometimes[1])
	}
	weights := map[string]int{}
	for _, act := range a.cfg.Nemesis.Actions {
		weights[act.Type] = act.Weight
	}
	if weights["isolate_node"] != 3 || weights["partition_halves"] != 2 || weights["kill_node"] != 2 || weights["heal_all"] != 2 {
		t.Fatalf("weights %+v", weights)
	}
}

func TestConsulYAMLLoads(t *testing.T) {
	consistent, err := LoadYAMLTarget("examples/consul/consul-consistent.yaml")
	if err != nil {
		t.Fatal(err)
	}
	stale, err := LoadYAMLTarget("examples/consul/consul-stale.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range []*YAMLAdapter{consistent, stale} {
		topo := a.cfg.Topology
		if topo.Nodes != 3 || !topo.ClientViaProxy || !topo.Peers.ViaProxy || topo.Peers.PortName != "server" {
			t.Fatalf("%s topology %+v", a.cfg.Name, topo)
		}
		if topo.ProxyMode != "per_node_owned" {
			t.Fatalf("%s proxy_mode %s", a.cfg.Name, topo.ProxyMode)
		}
		if len(topo.Ports.Names) != 2 || topo.Ports.Names[0] != "server" || topo.Ports.Names[1] != "serf" {
			t.Fatalf("%s names %+v", a.cfg.Name, topo.Ports.Names)
		}
		w := a.cfg.Workload
		if w.Driver != "http_kv" || w.Workers != 4 || w.Keyspace != 3 || w.timeout != 1500*time.Millisecond {
			t.Fatalf("%s workload %+v timeout %s", a.cfg.Name, w, w.timeout)
		}
		if w.Targets != "${client_addrs}" || w.Put.Method != "PUT" || w.Get.ValueFrom != "" {
			t.Fatalf("%s calls put=%+v get=%+v", a.cfg.Name, w.Put, w.Get)
		}
		n := a.cfg.Nemesis
		if n.warmup != 4*time.Second || n.duration != 25*time.Second || n.interval != 3*time.Second || n.quiet != 5*time.Second {
			t.Fatalf("%s nemesis warmup %s duration %s interval %s quiet %s", a.cfg.Name, n.warmup, n.duration, n.interval, n.quiet)
		}
		weights := map[string]int{}
		modes := map[string]string{}
		for _, act := range n.Actions {
			weights[act.Type] = act.Weight
			modes[act.Type] = act.Mode
		}
		if weights["isolate_node"] != 3 || modes["isolate_node"] != "blackhole" || weights["partition_halves"] != 2 || modes["partition_halves"] != "reset" || weights["kill_node"] != 2 || weights["heal_all"] != 2 {
			t.Fatalf("%s weights %+v modes %+v", a.cfg.Name, weights, modes)
		}
		p := a.cfg.Properties
		if len(p.Always) != 2 || p.Always[0].Name != "porcupine" || p.Always[0].Porcupine.timeout != 10*time.Second || p.Always[1].Name != "set" {
			t.Fatalf("%s always %+v", a.cfg.Name, p.Always)
		}
		if len(p.Eventually) != 1 || p.Eventually[0].Eventually == nil || p.Eventually[0].Eventually.N != 5 {
			t.Fatalf("%s eventually %+v", a.cfg.Name, p.Eventually)
		}
		if len(p.Sometimes) != 3 || p.Sometimes[0].Name != "fault_during_inflight" || p.Sometimes[1].Name != "info_ops_observed" || p.Sometimes[2].Name != "fault_action_fired_isolate_node" || p.Sometimes[2].FaultActionFired.Action != "isolate_node" {
			t.Fatalf("%s sometimes %+v", a.cfg.Name, p.Sometimes)
		}
	}
	if consistent.cfg.Workload.Get.URL != "http://${target}/v1/kv/${key}?consistent&raw" {
		t.Fatalf("consistent get %s", consistent.cfg.Workload.Get.URL)
	}
	if stale.cfg.Workload.Get.URL != "http://${target}/v1/kv/${key}?stale&raw" {
		t.Fatalf("stale get %s", stale.cfg.Workload.Get.URL)
	}
}

func TestRqliteResultControls(t *testing.T) {
	a, err := LoadYAMLTarget("examples/rqlite/rqlite.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.RunControl("lost_write", 1, "/bin/true"); err != nil {
		t.Fatal(err)
	}
	if err := a.RunControl("result_absent", 1, "/bin/true"); err != nil {
		t.Fatal(err)
	}
}

func TestYAMLPlantDirControl(t *testing.T) {
	a, err := LoadYAMLTarget("testdata/yaml/plant/target.yaml")
	if err != nil {
		t.Fatal(err)
	}
	bin, _, err := a.ResolveBinary("")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.RunControl("planted", 1, bin); err != nil {
		t.Fatal(err)
	}
}

func TestCountMatchingFilesJSONFieldIgnoresFilename(t *testing.T) {
	dir := t.TempDir()
	q := filepath.Join(dir, "queue")
	if err := os.MkdirAll(q, 0o755); err != nil {
		t.Fatal(err)
	}
	// Filename does not contain the id; nested JSON path does.
	if err := os.WriteFile(filepath.Join(q, "item.json"), []byte(`{"dagRun":{"id":"abc-1"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	// Filename contains the id; JSON path does not.
	if err := os.WriteFile(filepath.Join(q, "item_abc-1.json"), []byte(`{"dagRun":{"id":"other"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	got := countMatchingFiles(filepath.Join(q, "*.json"), "", "dagRun.id", "abc-1")
	if got != 1 {
		t.Fatalf("countMatchingFiles = %d, want 1", got)
	}
	// Bare key name still finds a nested id.
	if got := countMatchingFiles(filepath.Join(q, "*.json"), "", "id", "abc-1"); got != 1 {
		t.Fatalf("bare id countMatchingFiles = %d, want 1", got)
	}
}

func TestYAMLFileCountJSONFieldIgnoresFilename(t *testing.T) {
	a, err := LoadYAMLTarget("testdata/yaml/filematch/target.yaml")
	if err != nil {
		t.Fatal(err)
	}
	bin, _, err := a.ResolveBinary("")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.RunControl("json-id", 1, bin); err != nil {
		t.Fatal(err)
	}
}

func TestLoadYAMLStepDelayReject(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "bad-delay.yaml")
	body := `
name: bad-delay
binary:
  default: true
  extra: ["/bin/true"]
steps:
  - name: wait
    delay: "35"
  - name: live
    command: ["/bin/true"]
`
	if err := os.WriteFile(cfg, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadYAMLTarget(cfg); err == nil {
		t.Fatal("expected load error for delay without unit")
	}
}

func TestExpandClientProxyVars(t *testing.T) {
	got := expandTemplates("${client_port} ${client_proxy} ${client_addr_1} ${client_addrs}", map[string]string{
		"client_port":   "9",
		"client_proxy":  "127.0.0.1:10",
		"client_addr_1": "127.0.0.1:11",
		"client_addrs":  "127.0.0.1:10,127.0.0.1:11",
	})
	want := "9 127.0.0.1:10 127.0.0.1:11 127.0.0.1:10,127.0.0.1:11"
	if got != want {
		t.Fatalf("expandTemplates = %q, want %q", got, want)
	}
}

func TestClientPortRangeOverlap(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "overlap.yaml")
	body := []byte(`
name: overlap
binary:
  default: hold
topology:
  nodes: 2
  ports:
    base: 20000
    client_base: 20001
  client_via_proxy: true
  start: ["${binary}", "${data_dir}"]
`)
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadYAMLTarget(path); err == nil {
		t.Fatal("expected overlapping client_base to be rejected")
	}
}

func TestExpandTemplatesName(t *testing.T) {
	got := expandTemplates("${dir}/${name}.yaml", map[string]string{
		"dir":  "/tmp/t",
		"name": "ht",
	})
	if got != "/tmp/t/ht.yaml" {
		t.Fatalf("expandTemplates = %q", got)
	}
}

func TestYAMLStepDelay(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "delay.yaml")
	marker := filepath.Join(dir, "after.txt")
	body := fmt.Sprintf(`
name: delay-step
binary:
  default: true
  extra: ["/bin/true"]
steps:
  - name: live
    command: ["/bin/sleep", "0.01"]
  - name: wait
    delay: 80ms
  - name: mark
    command: ["/bin/sh", "-c", "echo done > %s"]
    wait_exit: true
    timeout: 2s
check:
  command:
    command: ["/bin/cat", "%s"]
    as: out
  rules:
    - if: { out_contains: done }
      verdict: PASS
    - else: FAIL
`, marker, marker)
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
	start := time.Now()
	cmd, err := a.Start(at, log)
	if err != nil {
		t.Fatal(err)
	}
	cmd.Wait()
	cr := a.CheckNamed(at, true, "delay")
	if cr.Verdict != "PASS" {
		t.Fatalf("verdict %s problems %v", cr.Verdict, cr.Problems)
	}
	if elapsed := time.Since(start); elapsed < 80*time.Millisecond {
		t.Fatalf("elapsed %v; step delay should have waited >= 80ms", elapsed)
	}
}

func TestStartArgsReuseOnEmptyRestart(t *testing.T) {
	a := &YAMLAdapter{cfg: yamlTarget{
		Args:    []string{"-data-dir", "${home}"},
		Restart: yamlRestart{},
	}}
	t0 := NewBaseTrial(1, t.TempDir(), "/bin/false", "demo")
	t0.Data()["addr"] = "127.0.0.1:1"
	t0.Data()["workload"] = false
	got := a.startArgs(t0)
	if len(got) != 2 || got[0] != "-data-dir" || got[1] != t0.Home() {
		t.Fatalf("startArgs without restart.args = %v", got)
	}
}

func mustRead(p string) string {
	b, _ := os.ReadFile(p)
	return string(b)
}

func TestWriteIsolateApplied(t *testing.T) {
	dir := t.TempDir()
	writeIsolateApplied(dir, map[string]any{
		"target":        "node_0",
		"direction":     "both",
		"links_cut":     4,
		"links_total":   6,
		"trigger_fired": true,
		"cut_time":      "2026-11-25T00:00:00Z",
	})
	b, err := os.ReadFile(filepath.Join(dir, "isolate_applied.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc isolateAppliedDoc
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Target != "node_0" || doc.Direction != "both" || doc.LinksCut != 4 || doc.LinksTotal != 6 || !doc.TriggerFired {
		t.Fatalf("%+v", doc)
	}
	if _, err := os.Stat(filepath.Join(dir, ".isolate_applied.json.tmp")); !os.IsNotExist(err) {
		t.Fatalf("temp file left behind: %v", err)
	}
}
