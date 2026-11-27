package main

import (
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
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
	// Drop nemesis restarts left behind by an aborted trial before this control.
	guardControlProcs()
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

// trackedProc is a child the trial started. waited is set by the goroutine
// that calls Cmd.Wait, which is what keeps the child from staying a zombie.
type trackedProc struct {
	cmd    *exec.Cmd
	waited bool
}

// procTracker is the process list for nemesis restarts. fatal reaps it on
// abort. procAudit is the zombie / stranded self-check for those PIDs.
type procTracker struct {
	mu    sync.Mutex
	procs map[int]*trackedProc
}

var trialProcs = &procTracker{procs: map[int]*trackedProc{}}

func trackProc(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	trialProcs.track(cmd)
}

// noteRestartedProc registers a node the nemesis started again.
func noteRestartedProc(cmd *exec.Cmd) {
	trackProc(cmd)
}

func noteProcWaited(pid int) {
	trialProcs.noteWaited(pid)
}

func procWasWaited(pid int) bool {
	return trialProcs.wasWaited(pid)
}

func procTracked(pid int) bool {
	return trialProcs.tracked(pid)
}

func (p *procTracker) track(cmd *exec.Cmd) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.procs[cmd.Process.Pid] = &trackedProc{cmd: cmd}
}

func (p *procTracker) noteWaited(pid int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if tp, ok := p.procs[pid]; ok {
		tp.waited = true
	}
}

func (p *procTracker) wasWaited(pid int) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	tp, ok := p.procs[pid]
	return ok && tp.waited
}

func (p *procTracker) tracked(pid int) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, ok := p.procs[pid]
	return ok
}

// reapTrackedProcs signals every child that has not been Waited.
// The goroutine blocked in Cmd.Wait reaps it. fatal calls this before exit
// so a restarted node is not left running.
func reapTrackedProcs() {
	trialProcs.mu.Lock()
	var cmds []*exec.Cmd
	for _, tp := range trialProcs.procs {
		if tp != nil && !tp.waited && tp.cmd != nil && tp.cmd.Process != nil {
			cmds = append(cmds, tp.cmd)
		}
	}
	trialProcs.mu.Unlock()
	for _, cmd := range cmds {
		_ = cmd.Process.Signal(syscall.SIGKILL)
	}
}

// guardControlProcs runs before the zombie and stranded self-checks so a
// restarted node from an aborted trial is not still in the process table.
func guardControlProcs() {
	reapTrackedProcs()
}

// procAudit reports tracked processes that are still running (stranded) or
// have exited without Wait (zombie).
func procAudit() error {
	stranded, zombies := leftoverProcs()
	if len(stranded) == 0 && len(zombies) == 0 {
		return nil
	}
	return fmt.Errorf("stranded %v zombie %v", stranded, zombies)
}

func leftoverProcs() (stranded, zombies []int) {
	trialProcs.mu.Lock()
	var pids []int
	for pid, tp := range trialProcs.procs {
		if tp == nil || tp.waited {
			continue
		}
		pids = append(pids, pid)
	}
	trialProcs.mu.Unlock()
	for _, pid := range pids {
		alive, zombie := inspectProc(pid)
		if zombie {
			zombies = append(zombies, pid)
		} else if alive {
			stranded = append(stranded, pid)
		}
	}
	sort.Ints(stranded)
	sort.Ints(zombies)
	return stranded, zombies
}

func processAlive(pid int) bool {
	alive, zombie := inspectProc(pid)
	return alive || zombie
}

func inspectProc(pid int) (alive, zombie bool) {
	if pid <= 0 {
		return false, false
	}
	if err := syscall.Kill(pid, 0); err != nil {
		return false, false
	}
	out, err := exec.Command("ps", "-o", "state=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return true, false
	}
	st := strings.TrimSpace(string(out))
	if strings.Contains(st, "Z") {
		return false, true
	}
	if st == "" {
		return true, false
	}
	return true, false
}
