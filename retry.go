package main

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

// retryResult is what the retry phase saw. No verdicts are drawn here.
type retryResult struct {
	snapshot runStatus // settled status just before the retry
	skipped  error     // why no retry was run, if none was
	exit     int       // dagu retry's exit code; -1 if it timed out
	final    runStatus // settled status after the retry
	notes    []error   // STUCK, ZOMBIE or status errors seen along the way
}

// runRetry settles the killed run, marks the record, retries, and settles again.
// Call it only after judge, when Dagu has exited.
func runRetry(t trial) retryResult {
	var r retryResult
	query := func() (runStatus, error) { return queryStatus(t.home, t.runID, procStale) }

	// dagu retry does not repair a stale Running run and refuses it, so the
	// normal reader must settle (and repair) it first.
	if err := waitGone(t.record, stepTimeout); err != nil {
		r.notes = append(r.notes, err)
	}
	snap, err := settle(query, settleTimeout)
	if err != nil {
		r.skipped = err
		return r
	}
	r.snapshot = snap

	// Steps see the same DAG_RUN_ID on every attempt, so this marker is the
	// only thing separating the attempts in the record.
	if err := appendLine(t.record, "retry "+t.runID); err != nil {
		fatal(err)
	}

	log, err := os.OpenFile(filepath.Join(t.dir, "dagu.log"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		fatal(err)
	}
	defer log.Close()
	ctx, cancel := context.WithTimeout(context.Background(), stepTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, daguBin, "retry", "--run-id="+t.runID, t.dagPath)
	cmd.Env = daguEnv(t.home, procStale)
	cmd.Stdout, cmd.Stderr = log, log
	var exitErr *exec.ExitError
	if err := cmd.Run(); err != nil && !errors.As(err, &exitErr) {
		fatal(err) // could not start dagu at all
	}
	r.exit = cmd.ProcessState.ExitCode()
	if ctx.Err() != nil {
		r.notes = append(r.notes, fmt.Errorf("STUCK: dagu retry still running after %v; killed", stepTimeout))
	}

	if err := waitGone(t.record, stepTimeout); err != nil {
		r.notes = append(r.notes, err)
	}
	final, err := settle(query, settleTimeout)
	if err != nil {
		r.notes = append(r.notes, err)
	}
	r.final = final
	return r
}

// appendLine adds one line to the record, as Fuss (not a step).
func appendLine(record, line string) error {
	f, err := os.OpenFile(record, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(line + "\n"); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// fmtSteps prints step states in name order, e.g. "a=succeeded b=failed".
func fmtSteps(st runStatus) string {
	var parts []string
	for _, name := range slices.Sorted(maps.Keys(st.Steps)) {
		parts = append(parts, name+"="+st.Steps[name])
	}
	return strings.Join(parts, " ")
}

// printRetry prints what the retry phase saw for one seed.
func printRetry(t trial, r retryResult) {
	if r.skipped != nil {
		fmt.Printf("  retry skipped: %v\n", r.skipped)
		return
	}
	fmt.Printf("  snapshot: run=%s %s\n", r.snapshot.Run, fmtSteps(r.snapshot))
	fmt.Printf("  retry: exit=%d\n", r.exit)
	fmt.Printf("  final:    run=%s %s\n", r.final.Run, fmtSteps(r.final))
	fmt.Printf("  after marker: %q\n", afterMarker(readRecord(t.record), t.runID))
	for _, n := range r.notes {
		fmt.Println("  note:", n)
	}
}
