package main

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// trial is one attack: Dagu started in a fresh home and possibly killed.
type trial struct {
	seed    int64
	shape   string // dagOne or dagTwo
	runID   string
	dir     string // kept on failure so the seed can be inspected
	dagPath string // the DAG file; dagu retry needs it to find the DAG name
	home    string
	record  string
	delay   time.Duration // when the kill was sent
	target  string        // stratified mode: the label the seed aimed at
	phase   string        // where the kill actually landed; counts use this, never target
	killed  bool          // Dagu actually died of SIGKILL
	took    time.Duration
	exit    int
	waitErr error
}

// How runTrial kills Dagu.
const (
	killAfterDelay = "delay"      // after the seeded delay
	killOnStart    = "on-start"   // as soon as the step's start line appears
	killAfterEnd   = "end"        // a seeded offset after the step's end line appears
	killStratified = "stratified" // a seeded label, then an offset after the event that opens it
	noKill         = "none"       // let it finish, for calibration
	killOnQueued   = "queued"     // -dag=human: when Dagu's status file shows Queued, plus a seeded offset
)

// runTrial starts Dagu for one seed, applies the kill, and waits for Dagu to exit.
// Setup failures are fatal: they mean the environment, not Dagu, is broken.
func runTrial(seed int64, how, prefix, shape string) trial {
	r := rand.New(rand.NewSource(seed))
	t := trial{seed: seed, shape: shape, runID: fmt.Sprintf("fuss-%d", seed), delay: pickKillDelay(r), phase: "not-killed"}
	switch how {
	case killAfterEnd:
		t.delay = pickEndOffset(r) // offset from the end line, not from start
	case killStratified:
		t.target, t.delay = pickStratum(r) // offset from the target's opening event
	}

	dir, err := os.MkdirTemp("", fmt.Sprintf("%s-%d-", prefix, seed))
	if err != nil {
		fatal(err)
	}
	t.dir, t.home, t.record = dir, filepath.Join(dir, "home"), filepath.Join(dir, "runs.txt")
	dagPath, err := writeDAG(dir, t.record, shape)
	if err != nil {
		fatal(err)
	}
	t.dagPath = dagPath
	log, err := os.Create(filepath.Join(dir, "dagu.log"))
	if err != nil {
		fatal(err)
	}
	defer log.Close()

	start := time.Now()
	cmd, err := startDagu(t.home, dagPath, t.runID, log)
	if err != nil {
		fatal(err)
	}
	// Wait in the background so stratified mode can notice Dagu exiting
	// before the event it is waiting for.
	var waitErr error
	done := make(chan struct{})
	go func() { waitErr = cmd.Wait(); close(done) }()

	sendKill := how != noKill
	switch how {
	case killOnStart:
		if err := waitFor(t.record, "start a ", 10*time.Millisecond, stepTimeout); err != nil {
			fatal(err)
		}
		t.delay = time.Since(start) // when the kill actually happened, not the unused seeded delay
	case killAfterEnd:
		// 1ms polling: the narrowest finalize window measured was 9ms.
		if err := waitFor(t.record, "end a ", time.Millisecond, stepTimeout); err != nil {
			fatal(err)
		}
		time.Sleep(t.delay)
	case killStratified:
		// If Dagu exits before the opening event, no kill is sent; the run
		// lands as after-exit and is not counted as a kill of its target.
		if sendKill = waitOpening(t, done); sendKill {
			time.Sleep(t.delay)
		}
	case killAfterDelay:
		time.Sleep(t.delay)
	}
	if sendKill {
		// Snapshot just before the kill: it shows where in Dagu's life the
		// kill landed, which a replay can be compared against.
		t.phase = recordPhase(t.record, shape)
		if how == killAfterEnd || how == killStratified {
			t.phase = finalizeLabel(t.home, t.runID)
		}
		if err := kill(cmd); err != nil {
			fatal(err)
		}
	}
	<-done
	t.waitErr = waitErr
	t.took = time.Since(start)
	t.killed = diedOfKill(cmd)
	t.exit = cmd.ProcessState.ExitCode()
	if (how == killAfterEnd || how == killStratified) && !t.killed {
		t.phase = "after-exit"
	}
	return t
}

// waitOpening polls every 1ms for the event that opens t.target. It returns
// false if Dagu exits first. Seeing a later label also counts as opened.
func waitOpening(t trial, done <-chan struct{}) bool {
	opened := func() bool {
		switch t.target {
		case "before-save":
			return strings.Contains(readRecord(t.record), "end a ")
		case "after-save":
			l := finalizeLabel(t.home, t.runID)
			return l == "after-save" || l == "after-compact"
		default: // after-compact
			return finalizeLabel(t.home, t.runID) == "after-compact"
		}
	}
	for !opened() {
		select {
		case <-done:
			return false
		case <-time.After(time.Millisecond):
		}
	}
	return true
}
