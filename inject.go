package main

import (
	"errors"
	"math/rand"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// pickKillDelay picks how long after start to kill Dagu.
// 0-3s spans first-start setup, the 2s step, and a little past a normal finish.
func pickKillDelay(r *rand.Rand) time.Duration {
	return time.Duration(r.Intn(3000)) * time.Millisecond
}

// pickEndOffset picks how long after the end line to kill Dagu.
// 0-90ms is 1.5x the slowest measured end→exit (61ms), so about a third
// of kills land after exit as a built-in negative.
func pickEndOffset(r *rand.Rand) time.Duration {
	return time.Duration(r.Intn(90_000)) * time.Microsecond
}

// stratum is a finalize label to aim at. The kill comes up to maxOffset after
// the event that opens the label; maxOffset is the window's shortest measured
// length, so most kills land where aimed.
type stratum struct {
	label     string
	maxOffset time.Duration
}

var strata = []stratum{
	{"before-save", 7 * time.Millisecond},   // end→saved: kill-mode bracket 8.3–11.4ms, -measure min 7.6ms
	{"after-save", 20 * time.Millisecond},   // saved→compacted: -measure min 21.4ms (no kill-mode bracket yet)
	{"after-compact", 9 * time.Millisecond}, // compacted→exit: -measure min 9.4ms (no kill-mode bracket yet)
}

// pickStratum picks the label to aim at, then the offset past its opening event.
func pickStratum(r *rand.Rand) (string, time.Duration) {
	s := strata[r.Intn(len(strata))]
	return s.label, time.Duration(r.Int63n(int64(s.maxOffset)))
}

// The window for killing `dagu human-task complete`, measured from its launch.
// 70-110ms brackets the queued-before-item kills seen by hand at 80 and 100ms
// (scripts/kill_complete.sh, one trial each).
const (
	completeKillMin  = 70 * time.Millisecond
	completeKillSpan = 40 * time.Millisecond
)

// pickCompleteKill picks how long after launching complete to kill it.
func pickCompleteKill(r *rand.Rand) time.Duration {
	return completeKillMin + time.Duration(r.Int63n(int64(completeKillSpan)))
}

// pickQueuedOffset picks how long after Queued is seen to kill complete:
// 0 to maxOffset, or exactly 0 when maxOffset is 0.
func pickQueuedOffset(r *rand.Rand, maxOffset time.Duration) time.Duration {
	if maxOffset <= 0 {
		return 0
	}
	return time.Duration(r.Int63n(int64(maxOffset/time.Microsecond))) * time.Microsecond
}

// kill sends SIGKILL to the process. A process that already exited is not an error.
func kill(cmd *exec.Cmd) error {
	err := cmd.Process.Signal(syscall.SIGKILL)
	if errors.Is(err, os.ErrProcessDone) {
		return nil
	}
	return err
}

// diedOfKill reports whether a waited-for process was ended by SIGKILL.
// Signal succeeding is not proof: an exited but unreaped process accepts it.
func diedOfKill(cmd *exec.Cmd) bool {
	ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
	return ok && ws.Signaled() && ws.Signal() == syscall.SIGKILL
}
