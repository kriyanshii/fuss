package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// Adapter represents a target program that Fuss can test.
// Each adapter implements the target-specific logic for setup, execution, and checking.
type Adapter interface {
	// Name returns the adapter's name (e.g. "dagu", "rqlite")
	Name() string

	// ResolveBinary finds the target binary and returns its path and source
	ResolveBinary(targetFlag string) (path, source string, err error)

	// Init prepares a trial: creates directories, writes config files, returns trial data
	Init(seed int64, dir, binaryPath string) (AdapterTrial, error)

	// Start launches the target binary
	Start(at AdapterTrial, log *os.File) (*exec.Cmd, error)

	// Status queries the target's current state
	Status(at AdapterTrial) (AdapterStatus, error)

	// Settle waits for the target to stop reporting "running" after the process died
	Settle(query func() (AdapterStatus, error), deadline time.Duration) (AdapterStatus, error)

	// Check validates a finished trial against the target's promises
	Check(at AdapterTrial, killed bool, phase string) []error

	// RunControl runs a control check; returns nil if it passes
	RunControl(name string, seed int64, binaryPath string) error

	// Controls returns the list of control checks this adapter supports
	Controls() []string
}

// AdapterTrial holds the state for one trial. Each adapter can embed additional fields.
type AdapterTrial interface {
	Seed() int64
	RunID() string
	Dir() string
	Home() string
	Record() string
	RecordContent() string
	BinaryPath() string
	Data() map[string]interface{} // adapter-specific data
}

// AdapterStatus represents the target's state at a point in time.
type AdapterStatus interface {
	String() string
	IsRunning() bool
}

// BaseTrial is a standard implementation of AdapterTrial.
type BaseTrial struct {
	seed       int64
	runID      string
	dir        string
	home       string
	record     string
	binaryPath string
	data       map[string]interface{}
}

func NewBaseTrial(seed int64, dir, binaryPath, prefix string) *BaseTrial {
	return &BaseTrial{
		seed:       seed,
		runID:      fmt.Sprintf("%s-%d", prefix, seed),
		dir:        dir,
		home:       filepath.Join(dir, "home"),
		record:     filepath.Join(dir, "runs.txt"),
		binaryPath: binaryPath,
		data:       make(map[string]interface{}),
	}
}

func (t *BaseTrial) Seed() int64                  { return t.seed }
func (t *BaseTrial) RunID() string                { return t.runID }
func (t *BaseTrial) Dir() string                  { return t.dir }
func (t *BaseTrial) Home() string                 { return t.home }
func (t *BaseTrial) Record() string               { return t.record }
func (t *BaseTrial) BinaryPath() string           { return t.binaryPath }
func (t *BaseTrial) Data() map[string]interface{} { return t.data }
func (t *BaseTrial) LogPath() string              { return filepath.Join(t.dir, "target.log") }

func (t *BaseTrial) RecordContent() string {
	b, _ := os.ReadFile(t.record)
	return string(b)
}

// CheckResult is what a YAML (or other named) check returns. The core copies
// Verdict unless Evidence is 0 and Verdict is PASS (or empty), which becomes
// UNKNOWN.
type CheckResult struct {
	Verdict  string
	Problems []error
	Evidence int
}

func (c CheckResult) named() string {
	if c.Verdict != "" {
		return c.Verdict
	}
	if len(c.Problems) > 0 {
		return "FAIL"
	}
	return "PASS"
}

func verdictFromCheck(c CheckResult) string {
	v := c.named()
	if c.Evidence == 0 && (v == "PASS" || v == "") {
		return "UNKNOWN"
	}
	return v
}

// NamedChecker is an optional richer Check. YAML uses it so a miss can be
// LOST WRITE or STRANDED instead of FAIL.
type NamedChecker interface {
	CheckNamed(at AdapterTrial, killed bool, phase string) CheckResult
}

// KillWaiter is an optional pre-kill wait. YAML file-kill polls a glob, then
// sleeps a seeded offset, and reports that it already waited.
type KillWaiter interface {
	WaitBeforeKill(at AdapterTrial, seed int64) (waited time.Duration, handled bool, err error)
}
