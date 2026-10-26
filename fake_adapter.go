package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// FakeAdapter is a test adapter that simulates a simple program.
type FakeAdapter struct {
	shouldZombie   bool
	shouldLoseData bool
}

func NewFakeAdapter() *FakeAdapter {
	return &FakeAdapter{}
}

func (f *FakeAdapter) Name() string {
	return "fake"
}

func (f *FakeAdapter) ResolveBinary(targetFlag string) (string, string, error) {
	if targetFlag == "" {
		return "/bin/sh", "default", nil
	}
	return targetFlag, "-target", nil
}

func (f *FakeAdapter) Init(seed int64, dir, binaryPath string) (AdapterTrial, error) {
	t := NewBaseTrial(seed, dir, binaryPath, "fuss-fake")
	if err := os.MkdirAll(t.Home(), 0o755); err != nil {
		return nil, err
	}
	
	t.Data()["markerPath"] = filepath.Join(t.Home(), "state.txt")
	t.Data()["zombie"] = f.shouldZombie
	t.Data()["loseData"] = f.shouldLoseData
	
	return t, nil
}

func (f *FakeAdapter) Start(at AdapterTrial, log *os.File) (*exec.Cmd, error) {
	t, ok := at.(*BaseTrial)
	if !ok {
		return nil, fmt.Errorf("expected *BaseTrial, got %T", at)
	}
	
	markerPath := t.Data()["markerPath"].(string)
	
	// Write a script that marks progress
	script := fmt.Sprintf(`
		echo "start %s" >> %s
		echo "started %s" > %s
		sleep 0.2
		echo "middle %s" >> %s
		sleep 0.2
		echo "end %s" >> %s
		echo "finished %s" >> %s
		sleep 1
	`, t.RunID(), t.Record(), t.RunID(), markerPath,
		t.RunID(), t.Record(),
		t.RunID(), t.Record(), t.RunID(), markerPath)
	
	cmd := exec.Command("/bin/sh", "-c", script)
	cmd.Stdout = log
	cmd.Stderr = log
	
	return cmd, cmd.Start()
}

func (f *FakeAdapter) Status(at AdapterTrial) (AdapterStatus, error) {
	t, ok := at.(*BaseTrial)
	if !ok {
		return nil, fmt.Errorf("expected *BaseTrial, got %T", at)
	}
	
	markerPath := t.Data()["markerPath"].(string)
	content, err := os.ReadFile(markerPath)
	if err != nil {
		if os.IsNotExist(err) {
			return fakeStatus{State: "not-started", Zombie: false}, nil
		}
		return nil, err
	}
	
	zombie := t.Data()["zombie"].(bool)
	
	s := fakeStatus{State: "running", Zombie: zombie}
	if strings.Contains(string(content), "finished") {
		s.State = "completed"
	}
	
	return s, nil
}

func (f *FakeAdapter) Settle(query func() (AdapterStatus, error), deadline time.Duration) (AdapterStatus, error) {
	end := time.Now().Add(deadline)
	for {
		st, err := query()
		if err != nil {
			return st, err
		}
		if !st.IsRunning() {
			return st, nil
		}
		if time.Now().After(end) {
			if fst, ok := st.(fakeStatus); ok && fst.Zombie {
				return st, fmt.Errorf("ZOMBIE: still running after deadline")
			}
			return st, fmt.Errorf("timeout")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (f *FakeAdapter) Check(at AdapterTrial, killed bool, phase string) []error {
	t, ok := at.(*BaseTrial)
	if !ok {
		return []error{fmt.Errorf("expected *BaseTrial, got %T", at)}
	}
	
	var problems []error
	
	rec := t.RecordContent()
	markerPath := t.Data()["markerPath"].(string)
	
	// If it started, check consistency
	if strings.Contains(rec, "start "+t.RunID()) {
		content, err := os.ReadFile(markerPath)
		if err != nil {
			problems = append(problems, fmt.Errorf("failed to read marker: %w", err))
			return problems
		}
		
		// Check for false success
		if strings.Contains(rec, "end "+t.RunID()) && !killed {
			if !strings.Contains(string(content), "finished") {
				problems = append(problems, fmt.Errorf("FALSE SUCCESS: record shows end but marker doesn't show finished"))
			}
		}
		
		// If configured to lose data
		if t.Data()["loseData"].(bool) && killed {
			problems = append(problems, fmt.Errorf("DATA LOSS: configured to lose data"))
		}
	}
	
	return problems
}

func (f *FakeAdapter) RunControl(name string, seed int64, binaryPath string) error {
	switch name {
	case "zombie":
		fa := &FakeAdapter{shouldZombie: true}
		dir, err := os.MkdirTemp("", fmt.Sprintf("fake-ctl-zombie-%d-", seed))
		if err != nil {
			return err
		}
		defer os.RemoveAll(dir)
		
		t, err := fa.Init(seed, dir, "/bin/sh")
		if err != nil {
			return err
		}
		
		log, _ := os.Create(filepath.Join(t.Dir(), "control.log"))
		defer log.Close()
		
		cmd, err := fa.Start(t, log)
		if err != nil {
			return err
		}
		
		time.Sleep(50 * time.Millisecond)
		cmd.Process.Kill()
		cmd.Wait()
		
		query := func() (AdapterStatus, error) { return fa.Status(t) }
		_, err = fa.Settle(query, 100*time.Millisecond)
		if err == nil || !strings.Contains(err.Error(), "ZOMBIE") {
			return fmt.Errorf("control BROKEN: expected ZOMBIE, got %v", err)
		}
		return nil
		
	case "data-loss":
		fa := &FakeAdapter{shouldLoseData: true}
		dir, err := os.MkdirTemp("", fmt.Sprintf("fake-ctl-loss-%d-", seed))
		if err != nil {
			return err
		}
		defer os.RemoveAll(dir)
		
		t, err := fa.Init(seed, dir, "/bin/sh")
		if err != nil {
			return err
		}
		
		log, _ := os.Create(filepath.Join(t.Dir(), "control.log"))
		defer log.Close()
		
		cmd, err := fa.Start(t, log)
		if err != nil {
			return err
		}
		
		time.Sleep(50 * time.Millisecond)
		cmd.Process.Kill()
		cmd.Wait()
		
		problems := fa.Check(t, true, "middle")
		if len(problems) == 0 {
			return fmt.Errorf("control BROKEN: data loss not detected")
		}
		
		hasDataLoss := false
		for _, p := range problems {
			if strings.Contains(p.Error(), "DATA LOSS") {
				hasDataLoss = true
				break
			}
		}
		if !hasDataLoss {
			return fmt.Errorf("control BROKEN: expected DATA LOSS, got %v", problems)
		}
		return nil
		
	default:
		return fmt.Errorf("unknown control %q", name)
	}
}

func (f *FakeAdapter) Controls() []string {
	return []string{"zombie", "data-loss"}
}

// fakeStatus implements AdapterStatus.
type fakeStatus struct {
	State  string
	Zombie bool
}

func (s fakeStatus) String() string {
	if s.Zombie {
		return "running (zombie)"
	}
	return s.State
}

func (s fakeStatus) IsRunning() bool {
	return s.State == "running" || s.Zombie
}
