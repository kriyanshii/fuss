package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// DaguAdapter implements the Adapter interface for Dagu.
type DaguAdapter struct {
	shape string // dagOne, dagTwo, or dagHuman
}

func NewDaguAdapter(shape string) (*DaguAdapter, error) {
	switch shape {
	case dagOne, dagTwo, dagHuman:
		return &DaguAdapter{shape: shape}, nil
	default:
		return nil, fmt.Errorf("unknown DAG shape %q", shape)
	}
}

func (d *DaguAdapter) Name() string {
	return "dagu"
}

func (d *DaguAdapter) ResolveBinary(targetFlag string) (string, string, error) {
	return resolveDagu(targetFlag)
}

func (d *DaguAdapter) Init(seed int64, dir, binaryPath string) (AdapterTrial, error) {
	t := NewBaseTrial(seed, dir, binaryPath, "fuss")
	if err := os.MkdirAll(t.Home(), 0o755); err != nil {
		return nil, err
	}
	
	dagPath, err := writeDAG(dir, t.Record(), d.shape)
	if err != nil {
		return nil, err
	}
	t.Data()["dagPath"] = dagPath
	t.Data()["shape"] = d.shape
	
	return t, nil
}

func (d *DaguAdapter) Start(at AdapterTrial, log *os.File) (*exec.Cmd, error) {
	t, ok := at.(*BaseTrial)
	if !ok {
		return nil, fmt.Errorf("expected *BaseTrial, got %T", at)
	}
	
	dagPath := t.Data()["dagPath"].(string)
	return startDagu(t.Home(), dagPath, t.RunID(), log)
}

func (d *DaguAdapter) Status(at AdapterTrial) (AdapterStatus, error) {
	t, ok := at.(*BaseTrial)
	if !ok {
		return nil, fmt.Errorf("expected *BaseTrial, got %T", at)
	}
	
	return daguQueryStatus(t.Home(), t.RunID(), procStale)
}

func (d *DaguAdapter) Settle(query func() (AdapterStatus, error), deadline time.Duration) (AdapterStatus, error) {
	return daguSettle(query, deadline)
}

func (d *DaguAdapter) Check(at AdapterTrial, killed bool, phase string) []error {
	t, ok := at.(*BaseTrial)
	if !ok {
		return []error{fmt.Errorf("expected *BaseTrial, got %T", at)}
	}
	
	var problems []error
	
	// Wait for steps to be gone
	if err := waitGone(t.Record(), stepTimeout); err != nil {
		problems = append(problems, err)
	}
	
	// Get final status
	query := func() (AdapterStatus, error) { return daguQueryStatus(t.Home(), t.RunID(), procStale) }
	st, settleErr := daguSettle(query, settleTimeout)
	
	rec := t.RecordContent()
	dSt, _ := st.(daguStatus)
	
	switch {
	case errors.Is(settleErr, errNoRun):
		if err := checkNoRun(rec); err != nil {
			problems = append(problems, err)
		}
	case settleErr != nil:
		problems = append(problems, settleErr)
	}
	
	if err := checkNoFalseSuccess(dSt.toRunStatus(), rec, t.RunID()); err != nil {
		problems = append(problems, err)
	}
	
	return problems
}

func (d *DaguAdapter) RunControl(name string, seed int64, binaryPath string) error {
	switch name {
	case "zombie":
		return d.runZombieControl(seed, binaryPath)
	case "queue":
		// Human mode queue control - to be implemented if needed
		return fmt.Errorf("queue control not yet implemented")
	case "stranded":
		// Human mode stranded control - to be implemented if needed
		return fmt.Errorf("stranded control not yet implemented")
	default:
		return fmt.Errorf("unknown control %q", name)
	}
}

func (d *DaguAdapter) Controls() []string {
	if d.shape == dagHuman {
		return []string{"queue", "stranded"}
	}
	return []string{"zombie"}
}

func (d *DaguAdapter) runZombieControl(seed int64, binaryPath string) error {
	dir, err := os.MkdirTemp("", fmt.Sprintf("fuss-ctl-zombie-%d-", seed))
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	
	t := NewBaseTrial(seed, dir, binaryPath, "fuss-ctl-zombie")
	if err := os.MkdirAll(t.Home(), 0o755); err != nil {
		return err
	}
	
	dagPath, err := writeDAG(dir, t.Record(), dagOne)
	if err != nil {
		return err
	}
	
	log, err := os.Create(filepath.Join(dir, "dagu.log"))
	if err != nil {
		return err
	}
	defer log.Close()
	
	cmd, err := startDagu(t.Home(), dagPath, t.RunID(), log)
	if err != nil {
		return err
	}
	
	// Wait for start
	if err := waitFor(t.Record(), "start a ", 10*time.Millisecond, stepTimeout); err != nil {
		return err
	}
	
	// Kill immediately
	if err := kill(cmd); err != nil {
		return err
	}
	_ = cmd.Wait()
	
	// Check with long threshold - should report zombie
	slowQuery := func() (AdapterStatus, error) {
		return daguQueryStatus(t.Home(), t.RunID(), time.Hour)
	}
	
	_, err = daguSettle(slowQuery, settleTimeout)
	if err == nil || !strings.HasPrefix(err.Error(), "ZOMBIE") {
		return fmt.Errorf("zombie control BROKEN: expected ZOMBIE error, got %v", err)
	}
	
	// Verify normal threshold repairs it
	normalSt, err := daguQueryStatus(t.Home(), t.RunID(), procStale)
	if err != nil {
		return fmt.Errorf("zombie control BROKEN: normal query failed: %w", err)
	}
	
	if normalSt.(daguStatus).Run != "Failed" {
		return fmt.Errorf("zombie control BROKEN: expected Failed, got %s", normalSt.(daguStatus).Run)
	}
	
	return nil
}

// daguStatus implements AdapterStatus for Dagu.
type daguStatus struct {
	Run   string
	Step  string
	Steps map[string]string
}

func (s daguStatus) String() string {
	if s.Run == "none" {
		return "none"
	}
	return fmt.Sprintf("%s/%s", s.Run, s.Step)
}

func (s daguStatus) IsRunning() bool {
	return s.Run == "Running"
}

// toRunStatus converts daguStatus to runStatus for use with old code.
func (s daguStatus) toRunStatus() runStatus {
	return runStatus{Run: s.Run, Step: s.Step, Steps: s.Steps}
}

// fromRunStatus converts runStatus to daguStatus.
func fromRunStatus(rs runStatus) daguStatus {
	return daguStatus{Run: rs.Run, Step: rs.Step, Steps: rs.Steps}
}

// daguSettle wraps the original settle function to use AdapterStatus.
func daguSettle(query func() (AdapterStatus, error), deadline time.Duration) (AdapterStatus, error) {
	// Convert to old-style query function
	oldQuery := func() (runStatus, error) {
		st, err := query()
		if err != nil {
			return runStatus{}, err
		}
		if dst, ok := st.(daguStatus); ok {
			return dst.toRunStatus(), nil
		}
		return runStatus{}, fmt.Errorf("unexpected status type: %T", st)
	}
	
	st, err := settle(oldQuery, deadline)
	return fromRunStatus(st), err
}

// daguQueryStatus wraps the original queryStatus to return AdapterStatus.
func daguQueryStatus(home, runID string, stale time.Duration) (AdapterStatus, error) {
	st, err := queryStatus(home, runID, stale)
	if err != nil {
		return daguStatus{}, err
	}
	return fromRunStatus(st), nil
}
