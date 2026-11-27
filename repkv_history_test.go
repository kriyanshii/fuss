package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRepkvHistoryBundle(t *testing.T) {
	cmd := exec.Command("go", "build", "-o", "examples/repkv/repkv", "./examples/repkv")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	oldBundle := bundleRun
	oldEDN := exportEDNPath
	oldJSON := jsonOut
	jsonOut = false
	t.Cleanup(func() {
		bundleRun = oldBundle
		exportEDNPath = oldEDN
		jsonOut = oldJSON
	})

	correctDir := t.TempDir()
	bugDir := t.TempDir()
	correctCode := runRepkvScenario(t, "examples/repkv/repkv.yaml", correctDir)
	bugCode := runRepkvScenario(t, "examples/repkv/repkv-bug.yaml", bugDir)

	if correctCode != exitPass {
		t.Errorf("correct mode exit %d, want 0\n%s", correctCode, readTail(filepath.Join(correctDir, "history.jsonl")))
	}
	assertHistoryBundle(t, correctDir, false)

	if bugCode != exitFail {
		t.Errorf("bug mode exit %d, want 1\n%s", bugCode, readTail(filepath.Join(bugDir, "history.jsonl")))
	}
	assertHistoryBundle(t, bugDir, true)
	verdict := bundleVerdict(t, bugDir)
	if verdict != "NONLINEARIZABLE" {
		t.Errorf("bug verdict %s", verdict)
	}
	if bundleVerdict(t, correctDir) != "PASS" && correctCode == exitPass {
		t.Errorf("correct bundle verdict %s", bundleVerdict(t, correctDir))
	}
}

func runRepkvScenario(t *testing.T, yamlPath, saveDir string) int {
	t.Helper()
	adapter, err := LoadYAMLTarget(yamlPath)
	if err != nil {
		t.Fatal(err)
	}
	bundleRun = bundleCLI{saveDir: saveDir}
	return runAdapter(adapter, "", 1, 1, true, "")
}

func assertHistoryBundle(t *testing.T, dir string, wantHTML bool) {
	t.Helper()
	for _, name := range []string{"history.jsonl", "nemesis_events.json", "history.edn"} {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || len(bytes.TrimSpace(b)) == 0 {
			t.Errorf("%s: %v len %d", name, err, len(b))
		}
	}
	jsonl, err := os.ReadFile(filepath.Join(dir, "history.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	edn, err := os.ReadFile(filepath.Join(dir, "history.edn"))
	if err != nil {
		t.Fatal(err)
	}
	ops := bytes.Count(jsonl, []byte("\n"))
	if ops == 0 || strings.Count(string(edn), ":type :invoke") != ops {
		t.Errorf("jsonl ops %d edn invokes %d", ops, strings.Count(string(edn), ":type :invoke"))
	}
	if !strings.Contains(string(edn), ":f :nemesis") {
		t.Error("history.edn missing nemesis")
	}
	var events []NemesisEvent
	if err := json.Unmarshal(readBytes(t, filepath.Join(dir, "nemesis_events.json")), &events); err != nil {
		t.Fatal(err)
	}
	if len(events) == 0 {
		t.Fatal("no nemesis events")
	}
	var rec struct {
		CallNS int64 `json:"call_ns"`
	}
	line := bytes.Split(bytes.TrimSpace(jsonl), []byte("\n"))[0]
	if err := json.Unmarshal(line, &rec); err != nil {
		t.Fatal(err)
	}
	if rec.CallNS <= 0 || rec.CallNS > int64(time.Minute) || events[0].TimestampNS > int64(time.Minute) {
		t.Errorf("timestamps are not schedule-relative: call_ns=%d nemesis=%d", rec.CallNS, events[0].TimestampNS)
	}
	htmlPath := filepath.Join(dir, "porcupine.html")
	html, err := os.ReadFile(htmlPath)
	if wantHTML {
		if err != nil || !strings.Contains(strings.ToLower(string(html)), "html") {
			t.Errorf("porcupine.html: %v len %d", err, len(html))
		}
		return
	}
	if err == nil {
		t.Errorf("correct bundle wrote porcupine.html (%d bytes)", len(html))
	}
}

func bundleVerdict(t *testing.T, dir string) string {
	t.Helper()
	var doc struct {
		Verdict string `json:"verdict"`
	}
	if err := json.Unmarshal(readBytes(t, filepath.Join(dir, "bundle.json")), &doc); err != nil {
		t.Fatal(err)
	}
	return doc.Verdict
}

func readBytes(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func readTail(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return err.Error()
	}
	if len(b) > 800 {
		return string(b[:800])
	}
	return string(b)
}
