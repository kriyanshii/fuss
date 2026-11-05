package main

import (
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
