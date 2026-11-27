package main

import (
	"encoding/json"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"fuss/proxy"
)

func TestPickWeightedDeterministic(t *testing.T) {
	weights := []int{1, 3, 2, 1}
	seq := func(seed int64, n int) []int {
		r := rand.New(rand.NewSource(seed))
		out := make([]int, n)
		for i := range out {
			out[i] = pickWeighted(r, weights)
		}
		return out
	}
	a := seq(42, 24)
	b := seq(42, 24)
	if strings.Join(ints(a), ",") != strings.Join(ints(b), ",") {
		t.Fatalf("same seed diverged %v vs %v", a, b)
	}
	c := seq(99, 24)
	if strings.Join(ints(a), ",") == strings.Join(ints(c), ",") {
		t.Fatal("different seeds produced the same weighted sequence")
	}
	counts := map[int]int{}
	r := rand.New(rand.NewSource(1))
	for i := 0; i < 7000; i++ {
		counts[pickWeighted(r, weights)]++
	}
	if counts[1] <= counts[2] || counts[2] <= counts[0] || counts[0] <= 0 {
		t.Fatalf("weights not honored: %v", counts)
	}
	if got := nemesisFireCount(100*time.Millisecond, 30*time.Millisecond); got != 4 {
		t.Fatalf("fire count %d", got)
	}
	if got := nemesisFireCount(100*time.Millisecond, 100*time.Millisecond); got != 1 {
		t.Fatalf("one fire %d", got)
	}
	if got := nemesisFireCount(0, 10*time.Millisecond); got != 0 {
		t.Fatalf("zero duration %d", got)
	}
	m1 := pickMinority(rand.New(rand.NewSource(7)), 5)
	m2 := pickMinority(rand.New(rand.NewSource(7)), 5)
	if strings.Join(ints(m1), ",") != strings.Join(ints(m2), ",") || len(m1) != 2 {
		t.Fatalf("minority %v %v", m1, m2)
	}
}

func ints(v []int) []string {
	out := make([]string, len(v))
	for i, n := range v {
		out[i] = strconv.Itoa(n)
	}
	return out
}

func TestLoadYAMLNemesisValidation(t *testing.T) {
	legacy, err := LoadYAMLTarget("testdata/yaml/topology/isolate.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if legacy.nemesisActive() || !legacy.cfg.Fault.Isolate.active() {
		t.Fatalf("legacy fault changed: nemesis=%v isolate=%q", legacy.nemesisActive(), legacy.cfg.Fault.Isolate.Node)
	}

	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "both",
			want: "mutually exclusive",
			body: `
name: both
binary: { default: hold }
topology:
  nodes: 1
  start: ["hold", "${data_dir}"]
fault:
  isolate: node_0
  after: { file: { glob: "${dir}/x", contains: X } }
nemesis:
  duration: 1s
  interval: 1s
  actions:
    - { type: heal_all }
`,
		},
		{
			name: "client",
			want: "client_via_proxy",
			body: `
name: client
binary: { default: hold }
topology:
  nodes: 1
  start: ["hold", "${data_dir}"]
nemesis:
  duration: 1s
  interval: 1s
  actions:
    - { type: isolate_client, target: "0", mode: reset }
`,
		},
		{
			name: "per-node",
			want: "direction inbound",
			body: `
name: per-node
binary: { default: hold }
topology:
  nodes: 2
  proxy_mode: per_node
  peers: { via_proxy: true }
  start: ["hold", "${data_dir}"]
nemesis:
  duration: 1s
  interval: 1s
  actions:
    - { type: isolate_node, target: "0", mode: reset, direction: both }
`,
		},
		{
			name: "leader",
			want: "node_file",
			body: `
name: leader
binary: { default: hold }
topology:
  nodes: 1
  start: ["hold", "${data_dir}"]
nemesis:
  duration: 1s
  interval: 1s
  actions:
    - { type: kill_node, target: leader, restart_delay: 1s }
`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "t.yaml")
			if err := os.WriteFile(path, []byte(tc.body), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := LoadYAMLTarget(path)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestNemesisScheduleDeterministic(t *testing.T) {
	buildTopologyHold(t)
	const seed = int64(7)
	a := runNemesisFixture(t, seed, nemesisMixYAML)
	b := runNemesisFixture(t, seed, nemesisMixYAML)
	if strings.Join(a, "\n") != strings.Join(b, "\n") {
		t.Fatalf("seed %d diverged\n%s\n---\n%s", seed, strings.Join(a, "\n"), strings.Join(b, "\n"))
	}
	c := runNemesisFixture(t, seed+1, nemesisMixYAML)
	if strings.Join(a, "\n") == strings.Join(c, "\n") {
		t.Fatal("different seeds produced the same nemesis schedule")
	}
	if len(a) < 2 || !strings.Contains(a[len(a)-1], "heal_all") || !strings.Contains(a[len(a)-1], "recovery") {
		t.Fatalf("schedule = %v", a)
	}
}

func TestNemesisQuietRestartAndBundle(t *testing.T) {
	buildTopologyHold(t)
	a, trial, rt, _ := startNemesisYAML(t, 11, nemesisQuietYAML)
	old := rt.Nodes[0].PID
	if _, _, err := a.WaitBeforeKill(trial, 11); err != nil {
		t.Fatal(err)
	}
	if rt.Nemesis == nil || !rt.Nemesis.Done || !rt.Nemesis.ReadyOK {
		t.Fatalf("nemesis %+v", rt.Nemesis)
	}
	quiet := a.cfg.Nemesis.quiet
	if rt.Nemesis.QuietFinished.Sub(rt.Nemesis.QuietStarted) < quiet {
		t.Fatalf("quiet %s, want >= %s", rt.Nemesis.QuietFinished.Sub(rt.Nemesis.QuietStarted), quiet)
	}
	events, ran := rt.nemesisEvents()
	if !ran || len(events) < 2 {
		t.Fatalf("events %v", events)
	}
	last := events[len(events)-1]
	if last.Action != "heal_all" || !strings.Contains(last.Details, "recovery") {
		t.Fatalf("last event %+v", last)
	}
	if last.TimestampNS < (a.cfg.Nemesis.warmup + a.cfg.Nemesis.duration).Nanoseconds() {
		t.Fatalf("recovery timestamp %d before the window ended", last.TimestampNS)
	}
	for i := 1; i < len(events); i++ {
		if events[i].TimestampNS < events[i-1].TimestampNS {
			t.Fatalf("timestamps went backwards: %+v", events)
		}
	}
	newPID := rt.Nodes[0].PID
	if newPID == old || !pidLive(newPID) || !procTracked(newPID) {
		t.Fatalf("restart pid old=%d new=%d tracked=%v", old, newPID, procTracked(newPID))
	}
	if pidLive(old) || !procWasWaited(old) {
		t.Fatalf("old pid %d alive=%v waited=%v", old, pidLive(old), procWasWaited(old))
	}

	out := t.TempDir()
	facts := replayFacts{Verdict: "PASS", Nemesis: events, HasNemesis: true, Version: "test", Checksum: "sha256:abc"}
	if err := writeReplayBundle(out, a.path, trial.BinaryPath(), a.Name(), 11, facts); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(out, "nemesis_events.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got []NemesisEvent
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != len(events) || got[0].Action == "" || got[len(got)-1].Details != last.Details {
		t.Fatalf("bundle events %+v", got)
	}

	cr := a.CheckNamed(trial, true, "nemesis")
	if cr.Verdict != "PASS" {
		t.Fatalf("verdict %s %v", cr.Verdict, cr.Problems)
	}
	if rt.Nemesis.CheckStarted.Before(rt.Nemesis.QuietFinished) {
		t.Fatal("check started before quiet_period finished")
	}
	deadline := time.Now().Add(2 * time.Second)
	for (pidLive(old) || pidLive(newPID)) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if pidLive(old) || pidLive(newPID) || !procWasWaited(newPID) {
		t.Fatalf("after stop old=%d new=%d waitedNew=%v", old, newPID, procWasWaited(newPID))
	}
	if err := procAudit(); err != nil {
		t.Fatal(err)
	}
}

func TestNemesisDirectionAndClientFlow(t *testing.T) {
	buildTopologyHold(t)
	a, trial, rt, _ := startNemesisYAML(t, 3, nemesisProxyYAML)
	rng := rand.New(rand.NewSource(3))
	origin := time.Now()
	yAct := yamlNemesisAction{
		Type: "isolate_node", Target: "0", Mode: "blackhole", Direction: "outbound", Flow: "a_to_b",
	}
	a.fireNemesis(trial.(*BaseTrial), rt, yAct, rng, origin)
	var outbound, inbound *topologyLink
	for _, l := range rt.Links {
		if l.From == 0 && l.To == 1 {
			outbound = l
		}
		if l.From == 1 && l.To == 0 {
			inbound = l
		}
	}
	if outbound == nil || inbound == nil {
		t.Fatal("missing peer links")
	}
	ab, ba := outbound.Link.Profiles()
	if ab.Mode != proxy.ModeBlackhole || ba.Mode != proxy.ModeHealthy {
		t.Fatalf("outbound profiles %s %s", ab.Mode, ba.Mode)
	}
	ab, ba = inbound.Link.Profiles()
	if ab.Mode != proxy.ModeHealthy || ba.Mode != proxy.ModeHealthy {
		t.Fatalf("inbound link was touched %s %s", ab.Mode, ba.Mode)
	}
	for _, l := range rt.ClientLinks {
		ab, ba = l.Link.Profiles()
		if ab.Mode != proxy.ModeHealthy || ba.Mode != proxy.ModeHealthy {
			t.Fatalf("client link %d mode %s", l.To, ab.Mode)
		}
	}

	a.fireNemesis(trial.(*BaseTrial), rt, yamlNemesisAction{
		Type: "isolate_client", Target: "1", Mode: "reset", Flow: "both",
	}, rng, origin)
	ab, ba = rt.ClientLinks[1].Link.Profiles()
	if ab.Mode != proxy.ModeReset || ba.Mode != proxy.ModeReset {
		t.Fatalf("client 1 profiles %s %s", ab.Mode, ba.Mode)
	}
	ab, _ = rt.ClientLinks[0].Link.Profiles()
	if ab.Mode != proxy.ModeHealthy {
		t.Fatalf("client 0 mode %s", ab.Mode)
	}
	ab, ba = outbound.Link.Profiles()
	if ab.Mode != proxy.ModeBlackhole || ba.Mode != proxy.ModeHealthy {
		t.Fatalf("peer link changed by isolate_client %s %s", ab.Mode, ba.Mode)
	}

	a.fireNemesis(trial.(*BaseTrial), rt, yamlNemesisAction{Type: "heal_all"}, rng, origin)
	ab, ba = outbound.Link.Profiles()
	if ab.Mode != proxy.ModeHealthy || ba.Mode != proxy.ModeHealthy {
		t.Fatalf("peer not healed %s %s", ab.Mode, ba.Mode)
	}
	ab, ba = rt.ClientLinks[1].Link.Profiles()
	if ab.Mode != proxy.ModeHealthy || ba.Mode != proxy.ModeHealthy {
		t.Fatalf("client not healed %s %s", ab.Mode, ba.Mode)
	}
}

func TestProcTrackerReapsRestart(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	noteRestartedProc(cmd)
	go func() {
		_ = cmd.Wait()
		noteProcWaited(pid)
	}()
	t.Cleanup(func() {
		if pidLive(pid) && cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	})
	stranded, _ := leftoverProcs()
	if !containsInt(stranded, pid) {
		t.Fatalf("pid %d not stranded in %v", pid, stranded)
	}
	reapTrackedProcs()
	deadline := time.Now().Add(2 * time.Second)
	for pidLive(pid) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if pidLive(pid) || !procWasWaited(pid) {
		t.Fatalf("pid %d alive=%v waited=%v", pid, pidLive(pid), procWasWaited(pid))
	}
	stranded, zombies := leftoverProcs()
	if containsInt(stranded, pid) || containsInt(zombies, pid) {
		t.Fatalf("left behind stranded=%v zombies=%v", stranded, zombies)
	}
}

func TestNemesisSaveBundle(t *testing.T) {
	buildTopologyHold(t)
	path := writeYAML(t, nemesisHealYAML)
	a, err := LoadYAMLTarget(path)
	if err != nil {
		t.Fatal(err)
	}
	prev := bundleRun
	t.Cleanup(func() { bundleRun = prev })
	out := t.TempDir()
	bundleRun = bundleCLI{saveDir: out}
	if code := runAdapter(a, "", 5, 1, true, ""); code != 0 {
		t.Fatalf("exit %d", code)
	}
	body, err := os.ReadFile(filepath.Join(out, "nemesis_events.json"))
	if err != nil {
		t.Fatal(err)
	}
	var events []NemesisEvent
	if err := json.Unmarshal(body, &events); err != nil {
		t.Fatal(err)
	}
	if len(events) < 2 || events[len(events)-1].Action != "heal_all" || !strings.Contains(events[len(events)-1].Details, "recovery") {
		t.Fatalf("events %+v", events)
	}
	if _, err := os.Stat(filepath.Join(out, "bundle.json")); err != nil {
		t.Fatal(err)
	}
}

func containsInt(s []int, v int) bool {
	for _, n := range s {
		if n == v {
			return true
		}
	}
	return false
}

const nemesisMixYAML = `
name: nemesis-mix
binary:
  default: hold
  extra: ["./testdata/yaml/topology/hold"]
topology:
  nodes: 3
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
  duration: 300ms
  interval: 30ms
  quiet_period: 20ms
  actions:
    - { type: heal_all, weight: 1 }
    - { type: pause_node, target: random, pause_duration: 20ms, weight: 5 }
    - { type: kill_node, target: random, restart_delay: 15ms, weight: 3 }
check:
  file_count:
    glob: "${dir}/node*/ready"
    as: ready_files
  rules:
    - if: { ready_files_eq: 3 }
      verdict: PASS
    - else: FAIL
`

const nemesisQuietYAML = `
name: nemesis-quiet
binary:
  default: hold
  extra: ["./testdata/yaml/topology/hold"]
topology:
  nodes: 3
  data_dir: "${dir}/node${i}"
  ports: { base: 0 }
  start: ["${binary}", "${data_dir}"]
  ready:
    file: "${data_dir}/ready"
    contains: READY
    timeout: 2s
  stop_grace: 300ms
nemesis:
  warmup: 10ms
  duration: 30ms
  interval: 30ms
  quiet_period: 150ms
  actions:
    - { type: kill_node, target: "0", restart_delay: 30s, weight: 1 }
check:
  file_count:
    glob: "${dir}/node*/ready"
    as: ready_files
  rules:
    - if: { ready_files_eq: 3 }
      verdict: PASS
    - else: FAIL
`

const nemesisProxyYAML = `
name: nemesis-proxy
binary:
  default: hold
  extra: ["./testdata/yaml/topology/hold"]
topology:
  nodes: 2
  data_dir: "${dir}/node${i}"
  client_via_proxy: true
  peers: { via_proxy: true }
  ports: { base: 0 }
  start: ["${binary}", "${data_dir}", "${port}", "${peers}"]
  ready:
    file: "${data_dir}/ready"
    contains: READY
    timeout: 2s
  stop_grace: 300ms
nemesis:
  duration: 20ms
  interval: 20ms
  actions:
    - { type: heal_all }
check:
  rules:
    - else: PASS
`

const nemesisHealYAML = `
name: nemesis-heal
binary:
  default: hold
  extra: ["./testdata/yaml/topology/hold"]
topology:
  nodes: 2
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
check:
  file_count:
    glob: "${dir}/node*/ready"
    as: ready_files
  rules:
    - if: { ready_files_eq: 2 }
      verdict: PASS
    - else: FAIL
`

func writeYAML(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "nemesis.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func startNemesisYAML(t *testing.T, seed int64, body string) (*YAMLAdapter, AdapterTrial, *topologyRuntime, *os.File) {
	t.Helper()
	path := writeYAML(t, body)
	a, err := LoadYAMLTarget(path)
	if err != nil {
		t.Fatal(err)
	}
	bin, _, err := a.ResolveBinary("")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	at, err := a.Init(seed, dir, bin)
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
	at.Data()["rng"] = rand.New(rand.NewSource(seed))
	return a, at, rt, log
}

func runNemesisFixture(t *testing.T, seed int64, body string) []string {
	t.Helper()
	a, at, rt, _ := startNemesisYAML(t, seed, body)
	if _, _, err := a.WaitBeforeKill(at, seed); err != nil {
		t.Fatal(err)
	}
	events, _ := rt.nemesisEvents()
	sigs := make([]string, len(events))
	for i, e := range events {
		sigs[i] = e.Action + "|" + strings.Join(e.Targets, ",") + "|" + e.Mode + "|" + e.Direction + "|" + e.Flow + "|" + e.Details
	}
	cr := a.CheckNamed(at, true, "nemesis")
	if cr.Verdict != "PASS" {
		t.Fatalf("seed %d verdict %s %v", seed, cr.Verdict, cr.Problems)
	}
	return sigs
}
