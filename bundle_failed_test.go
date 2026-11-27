package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestFailedBundleSeedDir(t *testing.T) {
	if failedBundleSeedDir("/tmp/out", 4, "PASS") != "" {
		t.Fatal("PASS wrote a path")
	}
	if failedBundleSeedDir("", 4, "FAIL") != "" {
		t.Fatal("empty root wrote a path")
	}
	got := failedBundleSeedDir("/tmp/out", 4, "NONLINEARIZABLE")
	want := filepath.Join("/tmp/out", "seed-4")
	if got != want {
		t.Fatalf("path %s, want %s", got, want)
	}
	if failedBundleSeedDir("/tmp/out", 9, "UNKNOWN") == "" {
		t.Fatal("UNKNOWN skipped")
	}
}

func TestSaveFailedBundleFlags(t *testing.T) {
	if err := validateBundleFlags("/a", "/b", "", 1); err == nil || !strings.Contains(err.Error(), "cannot be set together") {
		t.Fatal(err)
	}
	if err := validateBundleFlags("/a", "", "/r", 1); err == nil {
		t.Fatal("save-bundle with replay-bundle was accepted")
	}
	if err := validateBundleFlags("", "/b", "/r", 1); err == nil {
		t.Fatal("save-failed-bundle with replay-bundle was accepted")
	}
	err := validateBundleFlags("/a", "", "", 3)
	if err == nil || err.Error() != "-save-bundle records one seed; -n must be 1" {
		t.Fatalf("save-bundle -n: %v", err)
	}
	if err := validateBundleFlags("", "/b", "", 10); err != nil {
		t.Fatal(err)
	}

	bin := buildFuss(t)
	cmd := exec.Command(bin, "-save-bundle", "a", "-save-failed-bundle", "b")
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != exitSetup {
		t.Fatalf("exit %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "cannot be set together") {
		t.Fatalf("stderr/stdout:\n%s", out)
	}
}

func TestSaveFailedBundleSkipsPassWritesFail(t *testing.T) {
	buildRepkv(t)
	old := bundleRun
	oldJSON := jsonOut
	t.Cleanup(func() {
		bundleRun = old
		jsonOut = oldJSON
	})

	passDir := t.TempDir()
	jsonOut = false
	bundleRun = bundleCLI{failedDir: passDir}
	pass, err := LoadYAMLTarget("examples/repkv/repkv-http.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if code := runAdapter(pass, "", 1, 2, true, ""); code != exitPass {
		t.Fatalf("passing seeds exit %d", code)
	}
	ents, err := os.ReadDir(passDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 0 {
		t.Fatalf("passing seeds wrote %v", ents)
	}

	bugDir := t.TempDir()
	jsonOut = true
	bundleRun = bundleCLI{failedDir: bugDir}
	bug, err := LoadYAMLTarget("examples/repkv/repkv-http-bug.yaml")
	if err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := captureOutput(t, func() int {
		return runAdapter(bug, "", 1, 2, true, "")
	})
	if code != exitFail {
		t.Fatalf("failing seeds exit %d\n%s\n%s", code, stdout, stderr)
	}
	summary := summaryObject(t, stdout)
	raw, ok := summary["failed_bundles"].([]any)
	if !ok || len(raw) != 2 {
		t.Fatalf("failed_bundles %#v", summary["failed_bundles"])
	}
	for _, seed := range []int64{1, 2} {
		dir := filepath.Join(bugDir, fmt.Sprintf("seed-%d", seed))
		for _, name := range []string{"porcupine.html", "history.jsonl", "nemesis_events.json", "history.edn", "scenario.yaml", "bundle.json", "target.log"} {
			st, err := os.Stat(filepath.Join(dir, name))
			if err != nil {
				t.Fatalf("%s/%s: %v", dir, name, err)
			}
			if name != "target.log" && st.Size() == 0 {
				t.Fatalf("%s/%s is empty", dir, name)
			}
		}
		b, err := openReplayBundle(dir)
		if err != nil {
			t.Fatal(err)
		}
		if b.Seed != seed || b.Verdict != "NONLINEARIZABLE" {
			t.Fatalf("seed %d bundle seed %d verdict %s", seed, b.Seed, b.Verdict)
		}
		found := false
		for _, p := range raw {
			if p == dir {
				found = true
			}
		}
		if !found {
			t.Fatalf("summary missing %s in %#v", dir, raw)
		}
	}

	jsonOut = false
	humanDir := t.TempDir()
	bundleRun = bundleCLI{failedDir: humanDir}
	code, stdout, stderr = captureOutput(t, func() int {
		return runAdapter(bug, "", 1, 1, true, "")
	})
	if code != exitFail {
		t.Fatalf("human seed exit %d\n%s", code, stderr)
	}
	humanPath := filepath.Join(humanDir, "seed-1")
	if !strings.Contains(stdout, "failed bundle: "+humanPath) {
		t.Fatalf("stdout missing bundle path:\n%s", stdout)
	}

	bin := buildFuss(t)
	replay := exec.Command(bin, "-replay-bundle", filepath.Join(bugDir, "seed-1"))
	replayOut, err := replay.CombinedOutput()
	var ee *exec.ExitError
	if err != nil && !errors.As(err, &ee) {
		t.Fatalf("replay: %v\n%s", err, replayOut)
	}
	if ee != nil && ee.ExitCode() == exitSetup {
		t.Fatalf("replay did not load\n%s", replayOut)
	}
	if !strings.Contains(string(replayOut), "verdict:") {
		t.Fatalf("replay output:\n%s", replayOut)
	}
}

func buildFuss(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "fuss")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	return bin
}

func buildRepkv(t *testing.T) {
	t.Helper()
	cmd := exec.Command("go", "build", "-o", "examples/repkv/repkv", "./examples/repkv")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
}
