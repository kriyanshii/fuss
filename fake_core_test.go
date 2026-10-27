package main

import (
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// seed 149 makes runAdapterTrial sleep 99ms, inside the fake script's first
// 200ms sleep, so the SIGKILL lands on a live process.
const fakeSeededKillSeed int64 = 149

func TestFakeCoreSeededKill(t *testing.T) {
	adapter := NewFakeAdapter()
	bin, _, err := adapter.ResolveBinary("")
	if err != nil {
		t.Fatal(err)
	}
	wantDelay := time.Duration(rand.New(rand.NewSource(fakeSeededKillSeed)).Intn(3000)) * time.Millisecond
	if wantDelay != 99*time.Millisecond {
		t.Fatalf("seed %d delay changed: got %v, want 99ms", fakeSeededKillSeed, wantDelay)
	}

	got, err := runAdapterTrial(adapter, fakeSeededKillSeed, bin, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(got.dir) })

	if got.delay != wantDelay {
		t.Fatalf("delay = %v, want seeded %v", got.delay, wantDelay)
	}
	if !got.killed {
		t.Fatalf("seeded kill did not land: killed=%v phase=%s exit=%d", got.killed, got.phase, got.exit)
	}
	if got.verdict == "" {
		t.Fatal("core returned an empty verdict")
	}
}

func TestFakeCoreReplay10of10(t *testing.T) {
	adapter := NewFakeAdapter()
	bin, _, err := adapter.ResolveBinary("")
	if err != nil {
		t.Fatal(err)
	}

	var wantVerdict, wantPhase string
	var wantKilled bool
	for i := 0; i < 10; i++ {
		got, err := runAdapterTrial(adapter, fakeSeededKillSeed, bin, true)
		if err != nil {
			t.Fatalf("replay %d: %v", i+1, err)
		}
		t.Cleanup(func() { os.RemoveAll(got.dir) })
		if i == 0 {
			wantVerdict, wantPhase, wantKilled = got.verdict, got.phase, got.killed
			continue
		}
		if got.verdict != wantVerdict || got.phase != wantPhase || got.killed != wantKilled {
			t.Fatalf("replay %d diverged: verdict=%s phase=%s killed=%v, want verdict=%s phase=%s killed=%v",
				i+1, got.verdict, got.phase, got.killed, wantVerdict, wantPhase, wantKilled)
		}
	}
}

func TestFakeCoreControlCatchesStagedBroken(t *testing.T) {
	code := runAdapter(NewFakeAdapter(), "", 1, 1, true, "zombie")
	if code != exitPass {
		t.Fatalf("zombie control should catch the staged zombie and return %d, got %d", exitPass, code)
	}
	code = runAdapter(NewFakeAdapter(), "", 1, 1, true, "data-loss")
	if code != exitPass {
		t.Fatalf("data-loss control should catch the staged loss and return %d, got %d", exitPass, code)
	}
}

func TestFakeCoreControlMissExits3(t *testing.T) {
	code := runAdapter(missStagedControl{}, "", 1, 1, true, "zombie")
	if code != exitControlBroken {
		t.Fatalf("control that misses a staged broken state must return %d, got %d", exitControlBroken, code)
	}
}

func TestFakeCoreFullPath(t *testing.T) {
	code := runAdapter(NewFakeAdapter(), "", fakeSeededKillSeed, 1, true, "")
	if code != exitPass && code != exitFail {
		t.Fatalf("full fake path (controls then one seeded trial) returned %d, want 0 or 1", code)
	}
}

// missStagedControl stages a still-running status after the process is dead
// and Check reports nothing. The core must treat that as a broken control.
type missStagedControl struct{}

func (missStagedControl) Name() string { return "miss" }

func (missStagedControl) ResolveBinary(string) (string, string, error) {
	return "/bin/sh", "default", nil
}

func (m missStagedControl) Init(seed int64, dir, binaryPath string) (AdapterTrial, error) {
	t := NewBaseTrial(seed, dir, binaryPath, "miss")
	return t, os.MkdirAll(t.Home(), 0o755)
}

func (missStagedControl) Start(at AdapterTrial, log *os.File) (*exec.Cmd, error) {
	cmd := exec.Command("/bin/sh", "-c", "sleep 0.05")
	cmd.Stdout, cmd.Stderr = log, log
	return cmd, cmd.Start()
}

func (missStagedControl) Status(AdapterTrial) (AdapterStatus, error) {
	return fakeStatus{State: "running", Zombie: true}, nil
}

func (missStagedControl) Settle(query func() (AdapterStatus, error), _ time.Duration) (AdapterStatus, error) {
	return query()
}

func (missStagedControl) Check(AdapterTrial, bool, string) []error { return nil }

func (missStagedControl) Controls() []string { return []string{"zombie"} }

func (m missStagedControl) RunControl(name string, seed int64, binaryPath string) error {
	dir, err := os.MkdirTemp("", fmt.Sprintf("miss-ctl-%d-", seed))
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	t, err := m.Init(seed, dir, binaryPath)
	if err != nil {
		return err
	}
	log, err := os.Create(filepath.Join(t.Dir(), "control.log"))
	if err != nil {
		return err
	}
	defer log.Close()
	cmd, err := m.Start(t, log)
	if err != nil {
		return err
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	if problems := m.Check(t, true, "in-step"); len(problems) == 0 {
		return fmt.Errorf("control BROKEN: staged running-after-death not caught")
	}
	return nil
}
