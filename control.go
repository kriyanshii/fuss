package main

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// zombieReader is longer than any control run, so to this reader the killed
// run's proc file still looks fresh and Dagu keeps reporting Running.
const zombieReader = time.Hour

// zombieControl stages a stuck Running and checks Fuss catches it for the
// right reason. A control must FAIL: it returns nil only if Fuss reported
// ZOMBIE and every precondition and cross-check held.
func zombieControl(t trial) []string {
	var broken []string
	if !t.killed {
		broken = append(broken, "Dagu was not killed by SIGKILL")
	}
	if t.phase != "in-step" {
		broken = append(broken, "kill landed "+t.phase+", want in-step")
	}
	if err := waitGone(t.record, stepTimeout); err != nil {
		broken = append(broken, err.Error())
	}

	// All long-threshold reads must come first: one read with procStale
	// repairs the run on disk for good.
	slow := func() (runStatus, error) { return queryStatus(t.home, t.runID, zombieReader) }
	st, err := settle(slow, settleTimeout)
	switch {
	case err == nil || !strings.HasPrefix(err.Error(), "ZOMBIE"):
		broken = append(broken, fmt.Sprintf("want ZOMBIE from the 1h reader, got status=%+v err=%v", st, err))
	case st.Run != "Running" || st.Step != "running":
		broken = append(broken, fmt.Sprintf("ZOMBIE but last status %+v, want Running/running", st))
	}

	// Cross-check: the normal reader must repair it, proving the run was
	// really stale and the reader's threshold was the only thing hiding it.
	after, err := queryStatus(t.home, t.runID, procStale)
	if err != nil || after.Run != "Failed" || after.Step != "failed" {
		broken = append(broken, fmt.Sprintf("normal reader did not repair: status=%+v err=%v", after, err))
	}
	if stepEnded(readRecord(t.record), "a", t.runID) {
		broken = append(broken, "record shows the step finished")
	}
	return broken
}

// runZombieControl runs the control once and prints its verdict.
// It reports whether the control is OK; a broken control's dir is kept.
func runZombieControl(seed int64) bool {
	// The control always uses the one-step DAG: it checks Fuss, not the DAG.
	t := runTrial(seed, killOnStart, "fuss-ctl-zombie", dagOne)
	broken := zombieControl(t)
	if len(broken) == 0 {
		say("control=zombie seed=%d OK (ZOMBIE caught for the right reason)\n", seed)
		emit(map[string]any{"type": "control", "name": "zombie", "seed": seed, "ok": true})
		os.RemoveAll(t.dir)
		return true
	}
	say("control=zombie seed=%d BROKEN kill-at=%v killed=%v phase=%s dir=%s\n",
		seed, t.delay.Round(time.Millisecond), t.killed, t.phase, t.dir)
	for _, b := range broken {
		sayln("  -", b)
	}
	emit(map[string]any{"type": "control", "name": "zombie", "seed": seed, "ok": false, "dir": t.dir,
		"evidence": map[string]any{"kill_ms": ms(t.delay), "killed": t.killed, "label": t.phase, "problems": broken}})
	return false
}
