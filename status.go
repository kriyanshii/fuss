package main

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"time"
)

// runStatus is what Dagu says about one run, read through its own CLI.
type runStatus struct {
	Run   string            // e.g. Running, Succeeded, Failed
	Step  string            // state of step a, e.g. running, succeeded, failed
	Steps map[string]string // state of every step, by name
}

var (
	runLine = regexp.MustCompile(`(?m)^(?:Status|Result): (\w+)$`)
	// A step that never ran has no duration: "└─b [failed]".
	stepLine = regexp.MustCompile(`─(\w+)(?: \([^)]*\))? \[(\w+)\]`)
)

// errNoRun means Dagu has no record of the run at all, e.g. it was killed
// during first-start setup before creating it.
var errNoRun = errors.New("dagu has no such run")

// queryStatus runs `dagu status` with the given stale threshold. Note: reading
// status is not passive; Dagu permanently repairs a stale run to Failed when read.
func queryStatus(home, runID string, stale time.Duration) (runStatus, error) {
	cmd := exec.Command(daguBin, "status", "--run-id="+runID, "rec")
	cmd.Env = daguEnv(home, stale)
	out, err := cmd.CombinedOutput()
	if err != nil {
		// The CLI has no machine-readable output; this text is the only signal.
		if bytes.Contains(out, []byte("dag-run ID not found: "+runID)) {
			return runStatus{}, errNoRun
		}
		return runStatus{}, fmt.Errorf("dagu status: %w\n%s", err, out)
	}
	return parseStatus(out)
}

// parseStatus reads the run state and every step's state from `dagu status` text.
// Step a must be present: every DAG shape has it.
func parseStatus(out []byte) (runStatus, error) {
	run := runLine.FindAllSubmatch(out, -1)
	steps := map[string]string{}
	for _, m := range stepLine.FindAllSubmatch(out, -1) {
		steps[string(m[1])] = string(m[2])
	}
	if run == nil || steps["a"] == "" {
		return runStatus{}, fmt.Errorf("dagu status: unrecognised output:\n%s", out)
	}
	return runStatus{Run: string(run[len(run)-1][1]), Step: steps["a"], Steps: steps}, nil
}
