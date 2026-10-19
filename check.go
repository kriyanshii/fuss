package main

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
)

// judge checks a finished trial against rules 1 and 3 and returns Dagu's
// settled status with every problem found. Call it only after Dagu has exited.
func judge(t trial) (runStatus, []error) {
	var problems []error
	// Only meaningful now that Dagu is dead and reaped: nothing is left to launch the step.
	if err := waitGone(t.record, stepTimeout); err != nil {
		problems = append(problems, err)
	}
	query := func() (runStatus, error) { return queryStatus(t.home, t.runID, procStale) }
	st, settleErr := settle(query, settleTimeout)
	rec := readRecord(t.record)
	switch {
	case errors.Is(settleErr, errNoRun):
		st.Run = "none"
		if err := checkNoRun(rec); err != nil {
			problems = append(problems, err)
		}
	case settleErr != nil:
		problems = append(problems, settleErr)
	}
	if err := checkNoFalseSuccess(st, rec, t.runID); err != nil {
		problems = append(problems, err)
	}
	return st, problems
}

// settle polls query until the run stops saying Running (rule 3).
// It returns the last status seen and, if the deadline passes, a ZOMBIE error.
func settle(query func() (runStatus, error), deadline time.Duration) (runStatus, error) {
	end := time.Now().Add(deadline)
	for {
		st, err := query()
		if err != nil {
			return st, err
		}
		if st.Run != "Running" {
			return st, nil
		}
		if time.Now().After(end) {
			return st, fmt.Errorf("ZOMBIE: run still Running %v after Dagu died", deadline)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// checkNoRun decides whether Dagu having no record of the run is acceptable:
// only if the step never started. A started step with no run means Dagu lost it.
func checkNoRun(rec string) error {
	if strings.Contains(rec, "start a ") {
		return fmt.Errorf("LOST RUN: record shows the step started but Dagu has no such run")
	}
	return nil
}

// checkRetry is rule 4, plus rule 1 per step, for a retried run. snapshot is
// the settled status just before the retry; final is the settled status after.
func checkRetry(snapshot, final runStatus, rec, runID string, retryExit int) []error {
	var problems []error
	after := afterMarker(rec, runID)
	for _, name := range slices.Sorted(maps.Keys(snapshot.Steps)) {
		if snapshot.Steps[name] != "succeeded" {
			continue // re-running an unconfirmed step is what retry is for
		}
		// Start lines, not end lines: a re-run cut short still counts.
		if stepStarted(after, name, runID) {
			problems = append(problems, fmt.Errorf("RERAN CONFIRMED STEP: %s was succeeded before the retry, but the retry started it again", name))
		}
		if got := final.Steps[name]; got != "succeeded" {
			problems = append(problems, fmt.Errorf("REVERTED: %s was succeeded before the retry, now %q", name, got))
		}
	}
	for _, name := range slices.Sorted(maps.Keys(final.Steps)) {
		if final.Steps[name] == "succeeded" && !stepEnded(rec, name, runID) {
			problems = append(problems, fmt.Errorf("FALSE SUCCESS: Dagu says step %s succeeded but the record has no end line for it", name))
		}
	}
	if retryExit != 0 || final.Run != "Succeeded" {
		problems = append(problems, fmt.Errorf("RETRY DID NOT COMPLETE: exit=%d final run=%q", retryExit, final.Run))
	}
	return problems
}

// checkNoFalseSuccess is rule 1: Dagu must not claim success for a step
// the outside record never saw finish.
func checkNoFalseSuccess(st runStatus, rec, runID string) error {
	claimed := st.Run == "Succeeded" || st.Step == "succeeded"
	if claimed && !stepEnded(rec, "a", runID) {
		return fmt.Errorf("FALSE SUCCESS: Dagu says run=%s step=%s but record has no end line", st.Run, st.Step)
	}
	return nil
}
