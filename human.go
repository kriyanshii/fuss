package main

import (
	"errors"
	"fmt"
	"maps"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"
)

// repeatTimeout covers Dagu's 30s enqueue timeout: a killed complete can leave
// the run lock behind, and the repeat waits for it before failing.
// schedulerWait is how long the scheduler gets to resume the run.
const (
	repeatTimeout = 45 * time.Second
	schedulerWait = 60 * time.Second
	schedulerStop = 10 * time.Second
)

var errTimedOut = errors.New("timed out")

// humanTrial is one kill of `dagu human-task complete` and what followed.
type humanTrial struct {
	seed       int64
	rep        int
	runID      string
	dir        string // kept unless PASS
	home       string
	record     string
	delay      time.Duration // seeded; -kill-at=delay
	offset     time.Duration // seeded; -kill-at=queued, after Queued is seen
	queuedSeen time.Duration // -kill-at=queued: when the status file showed Queued; 0 if never
	itemAtKill int           // -kill-at=queued: queue items just before the kill; -1 if no kill
	at         time.Duration // when the kill was actually sent
	killed     bool          // complete died of SIGKILL
	label      string        // where the kill landed, from state after it
	repeatExit int
	repeatOut  string
	repeatErr  string
	repeatHung bool
	final      runStatus // after the scheduler wait
	items      int       // queue items after the scheduler wait
	starts     int       // "start after <runID>" lines in the record
	verdict    string
}

// humanEnv sets only documented DAGU_* variables. DAGU_AUTH_MODE=none keeps
// Dagu's auth warning off stderr, which the spec says must be empty.
func humanEnv(home string) []string {
	return append(os.Environ(), "DAGU_HOME="+home, "DAGU_AUTH_MODE=none")
}

func humanCmd(home string, args ...string) *exec.Cmd {
	cmd := exec.Command(daguBin, args...)
	cmd.Env = humanEnv(home)
	return cmd
}

// completeArgs completes h. It must name the DAG: complete rejects a path.
func completeArgs(runID string) []string {
	return completeArgsFor("rec", runID)
}

// completeArgsFor is completeArgs for a run of any DAG name, e.g. a staged one.
func completeArgsFor(dag, runID string) []string {
	return []string{"human-task", "complete", "--run-id=" + runID, "--step=h", dag}
}

// humanStatus is parseStatus for this DAG, which has h and after, not a.
func humanStatus(home, runID string) (runStatus, error) {
	return humanStatusFor(home, "rec", runID)
}

// humanStatusFor is humanStatus for a run of any DAG name, e.g. a staged one.
func humanStatusFor(home, dag, runID string) (runStatus, error) {
	out, err := humanCmd(home, "status", "--run-id="+runID, dag).CombinedOutput()
	if err != nil {
		return runStatus{}, fmt.Errorf("dagu status: %w\n%s", err, out)
	}
	run := runLine.FindAllSubmatch(out, -1)
	steps := map[string]string{}
	for _, m := range stepLine.FindAllSubmatch(out, -1) {
		steps[string(m[1])] = string(m[2])
	}
	if run == nil || steps["h"] == "" {
		return runStatus{}, fmt.Errorf("dagu status: unrecognised output:\n%s", out)
	}
	return runStatus{Run: string(run[len(run)-1][1]), Step: steps["h"], Steps: steps}, nil
}

// queueItems counts runID's queue item files. The layout was seen by hand,
// not read from a spec; queueControl catches it changing.
func queueItems(home, runID string) int {
	m, _ := filepath.Glob(filepath.Join(home, "data", "queue", "*", "item_*_"+runID+".json"))
	return len(m)
}

// humanLabel names where the kill landed from state alone, in the order
// complete writes it: h succeeded, run Queued, queue item.
func humanLabel(killed bool, st runStatus, items int) string {
	switch {
	case !killed:
		return "finished-first"
	case st.Step == "waiting":
		return "before-store"
	case st.Run == "Waiting":
		return "store-before-queued"
	case items == 0:
		return "queued-before-item"
	default:
		return "after-item"
	}
}

// humanVerdict judges by the record: the run must resume exactly once.
// NO-RESUME is no resume without the stranded shape, e.g. the run left Waiting.
func humanVerdict(st runStatus, items, starts int) string {
	switch {
	case starts >= 2:
		return "DOUBLE-RESUME"
	case starts == 1:
		return "PASS"
	case st.Run == "Queued" && items == 0:
		return "STRANDED"
	default:
		return "NO-RESUME"
	}
}

// newHumanTrial makes a fresh home and gets the run Waiting on h.
// The seed picks the kill delay, so every replay of a seed uses the same one.
// The offset is drawn after the delay, so -kill-at=delay seeds are unchanged.
func newHumanTrial(seed int64, rep int, maxOffset time.Duration) humanTrial {
	r := rand.New(rand.NewSource(seed))
	t := humanTrial{seed: seed, rep: rep, runID: fmt.Sprintf("fuss-%d", seed), delay: pickCompleteKill(r), itemAtKill: -1}
	t.offset = pickQueuedOffset(r, maxOffset)
	dir, err := os.MkdirTemp("", fmt.Sprintf("fuss-human-%d-", seed))
	if err != nil {
		fatal(err)
	}
	t.dir, t.home, t.record = dir, filepath.Join(dir, "home"), filepath.Join(dir, "runs.txt")
	dagPath, err := writeDAG(dir, t.record, dagHuman)
	if err != nil {
		fatal(err)
	}
	if err := startWaiting(t, dagPath); err != nil {
		fatal(fmt.Errorf("seed %d: %w (dir=%s)", seed, err, dir))
	}
	return t
}

// startWaiting runs `dagu start`, which exits by itself once the run waits.
func startWaiting(t humanTrial, dagPath string) error {
	log, err := os.Create(filepath.Join(t.dir, "dagu.log"))
	if err != nil {
		return err
	}
	defer log.Close()
	cmd := humanCmd(t.home, "start", "--run-id="+t.runID, dagPath)
	cmd.Stdout, cmd.Stderr = log, log
	if err := runWithin(cmd, stepTimeout); err != nil {
		return fmt.Errorf("dagu start: %w", err)
	}
	st, err := humanStatus(t.home, t.runID)
	if err != nil {
		return err
	}
	if st.Run != "Waiting" || st.Step != "waiting" {
		return fmt.Errorf("run is %s with h %s, want Waiting with h waiting", st.Run, st.Step)
	}
	return nil
}

// runWithin runs cmd and kills it if it outlives timeout.
func runWithin(cmd *exec.Cmd, timeout time.Duration) error {
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(timeout):
		_ = kill(cmd)
		<-done
		return fmt.Errorf("still running after %v: %w", timeout, errTimedOut)
	}
}

// outFiles sends cmd's stdout and stderr to <name>.out and <name>.err in dir.
// Files, not pipes: Wait would block on pipes a child inherited.
func outFiles(cmd *exec.Cmd, dir, name string) (closeAll func()) {
	out, err := os.Create(filepath.Join(dir, name+".out"))
	if err != nil {
		fatal(err)
	}
	errf, err := os.Create(filepath.Join(dir, name+".err"))
	if err != nil {
		fatal(err)
	}
	cmd.Stdout, cmd.Stderr = out, errf
	return func() { out.Close(); errf.Close() }
}

func readTrimmed(path string) string {
	b, _ := os.ReadFile(path)
	return strings.TrimSpace(string(b))
}

// runScheduler runs `dagu scheduler` for d, then stops it.
func runScheduler(t humanTrial, d time.Duration) error {
	log, err := os.Create(filepath.Join(t.dir, "scheduler.log"))
	if err != nil {
		return err
	}
	defer log.Close()
	cmd := humanCmd(t.home, "scheduler")
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return fmt.Errorf("scheduler exited before %v: %v", d, err)
	case <-time.After(d):
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-done:
	case <-time.After(schedulerStop):
		_ = kill(cmd)
		<-done
	}
	return nil
}

// killWhenQueued polls Dagu's status file every 1ms and SIGKILLs complete
// t.offset after its last line shows Queued. No kill is sent if complete
// exits first. The file is only read. Reading it relies on Dagu's run-store
// layout, which spec 031 puts out of scope (line 444), so it only times the
// kill; labels and verdicts come from dagu status, the queue dir and the record.
func (t *humanTrial) killWhenQueued(cmd *exec.Cmd, start time.Time, statusPath, where string) {
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	deadline := time.After(stepTimeout)
	for {
		if last, _, _ := statusSnapshot(statusPath); last == statusQueued {
			t.queuedSeen = time.Since(start)
			break
		}
		select {
		case <-done:
			return // finished (or failed) before Queued was seen
		case <-deadline:
			_ = kill(cmd)
			<-done
			fatal(fmt.Errorf("%s: complete neither showed Queued nor exited in %v", where, stepTimeout))
		case <-time.After(time.Millisecond):
		}
	}
	time.Sleep(t.offset)
	t.itemAtKill = queueItems(t.home, t.runID) // read just before the kill
	t.at = time.Since(start)
	if err := kill(cmd); err != nil {
		fatal(err)
	}
	<-done
}

// runHumanTrial kills complete at the seeded delay, repeats it, lets the
// scheduler run, and judges the run by the record.
func runHumanTrial(seed int64, rep int, how string, maxOffset time.Duration) humanTrial {
	t := newHumanTrial(seed, rep, maxOffset)
	where := fmt.Sprintf("seed %d rep %d (dir=%s)", seed, rep, t.dir)

	var statusPath string
	if how == killOnQueued {
		// Found before complete starts: the run is Waiting, so the file exists.
		if statusPath = findStatusFile(t.home, t.runID); statusPath == "" {
			fatal(fmt.Errorf("%s: no status.jsonl for a Waiting run; Dagu's file layout changed?", where))
		}
	}
	c1 := humanCmd(t.home, completeArgs(t.runID)...)
	close1 := outFiles(c1, t.dir, "complete1")
	if err := c1.Start(); err != nil {
		fatal(err)
	}
	start := time.Now()
	if how == killOnQueued {
		t.killWhenQueued(c1, start, statusPath, where)
	} else {
		time.Sleep(t.delay)
		t.at = time.Since(start)
		if err := kill(c1); err != nil {
			fatal(err)
		}
		_ = c1.Wait() // a kill makes this an error; diedOfKill tells which
	}
	close1()
	t.killed = diedOfKill(c1)
	st, err := humanStatus(t.home, t.runID)
	if err != nil {
		fatal(fmt.Errorf("%s: %w", where, err))
	}
	t.label = humanLabel(t.killed, st, queueItems(t.home, t.runID))

	c2 := humanCmd(t.home, completeArgs(t.runID)...)
	close2 := outFiles(c2, t.dir, "complete2")
	err = runWithin(c2, repeatTimeout)
	close2()
	if c2.ProcessState == nil {
		fatal(fmt.Errorf("%s: repeat complete: %w", where, err))
	}
	t.repeatExit = c2.ProcessState.ExitCode()
	t.repeatHung = errors.Is(err, errTimedOut)
	t.repeatOut = readTrimmed(filepath.Join(t.dir, "complete2.out"))
	t.repeatErr = readTrimmed(filepath.Join(t.dir, "complete2.err"))

	if err := runScheduler(t, schedulerWait); err != nil {
		fatal(fmt.Errorf("%s: %w", where, err))
	}
	if t.final, err = humanStatus(t.home, t.runID); err != nil {
		fatal(fmt.Errorf("%s: %w", where, err))
	}
	t.items = queueItems(t.home, t.runID)
	t.starts = strings.Count(readRecord(t.record), "start after "+t.runID+"\n")
	t.verdict = humanVerdict(t.final, t.items, t.starts)
	return t
}

// queueControl checks Fuss can see a queue item before any verdict relies on
// "no queue item": an unkilled complete must queue exactly one. If Fuss
// counted zero here, every trial would look STRANDED.
func queueControl(seed int64) error {
	t := newHumanTrial(seed, 0, 0)
	out, err := humanCmd(t.home, completeArgs(t.runID)...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("unkilled complete: %w\n%s (dir=%s)", err, out, t.dir)
	}
	if want := "Completed human task h; DAG-run queued for resume.\n"; string(out) != want {
		return fmt.Errorf("unkilled complete printed %q, want %q (dir=%s)", out, want, t.dir)
	}
	st, err := humanStatus(t.home, t.runID)
	if err != nil {
		return err
	}
	if n := queueItems(t.home, t.runID); st.Run != "Queued" || n != 1 {
		return fmt.Errorf("after unkilled complete: run %s, %d queue items; want Queued, 1 (dir=%s)", st.Run, n, t.dir)
	}
	os.RemoveAll(t.dir)
	return nil
}

// runHuman runs n seeds from seed, each replay times, and returns the exit code.
// how is killAfterDelay or killOnQueued; maxOffset applies to killOnQueued.
func runHuman(seed int64, n, replay int, how string, maxOffset time.Duration) int {
	if err := queueControl(seed); err != nil {
		fmt.Println("CONTROL BROKEN: Fuss cannot see queue items; no seeds run:", err)
		return exitControlBroken
	}
	// The STRANDED check must catch a known stranded run before any verdict counts.
	if err := strandedControl(); err != nil {
		fmt.Println("CONTROL BROKEN: the STRANDED check missed a staged stranded run; no seeds run:", err)
		return exitControlBroken
	}
	failed, diverged := 0, 0
	for s := seed; s < seed+int64(n); s++ {
		verdicts, labels := map[string]int{}, map[string]int{}
		inWindow, inWindowStranded := 0, 0 // killOnQueued: kills that landed queued-before-item
		for rep := 1; rep <= replay; rep++ {
			t := runHumanTrial(s, rep, how, maxOffset)
			verdicts[t.verdict]++
			labels[t.label]++
			hung := ""
			if t.repeatHung {
				hung = fmt.Sprintf(" (killed after %v)", repeatTimeout)
			}
			if how == killOnQueued {
				queued, at := "never", "none"
				if t.queuedSeen > 0 {
					queued, at = t.queuedSeen.Round(time.Microsecond).String(), t.at.Round(time.Microsecond).String()
				}
				fmt.Printf("seed=%d rep=%d %s label=%s offset=%v queued-seen=%s kill-sent=%s item-at-kill=%d killed=%v repeat-exit=%d%s repeat=%q final=%s items=%d starts=%d\n",
					s, rep, t.verdict, t.label, t.offset.Round(time.Microsecond), queued, at, t.itemAtKill,
					t.killed, t.repeatExit, hung, t.repeatOut, t.final.Run, t.items, t.starts)
				if t.label == "queued-before-item" {
					inWindow++
					if t.verdict == "STRANDED" {
						inWindowStranded++
					}
				}
				// Fuss saw Queued, yet Dagu's state after the kill says it never got there.
				if t.queuedSeen > 0 && (t.label == "before-store" || t.label == "store-before-queued") {
					fmt.Printf("  ! saw Queued in the status file but dagu status says %s after the kill\n", t.label)
				}
			} else {
				fmt.Printf("seed=%d rep=%d %s label=%s delay=%v at=%v killed=%v repeat-exit=%d%s repeat=%q final=%s items=%d starts=%d\n",
					s, rep, t.verdict, t.label, t.delay.Round(time.Microsecond), t.at.Round(time.Microsecond),
					t.killed, t.repeatExit, hung, t.repeatOut, t.final.Run, t.items, t.starts)
			}
			if t.repeatErr != "" {
				fmt.Printf("  repeat stderr: %s\n", t.repeatErr)
			}
			if t.verdict == "PASS" {
				os.RemoveAll(t.dir) // a replay recreates it
				continue
			}
			failed++
			fmt.Printf("  dir=%s\n", t.dir)
		}
		fmt.Printf("seed=%d summary: %s | labels: %s\n", s, fmtCounts(verdicts, replay), fmtCounts(labels, replay))
		if how == killOnQueued {
			missed := maps.Clone(labels)
			delete(missed, "queued-before-item")
			fmt.Printf("seed=%d window: %d/%d kills landed queued-before-item, %d/%d of those STRANDED | missed %d: %s\n",
				s, inWindow, replay, inWindowStranded, inWindow, replay-inWindow, fmtCounts(missed, replay))
			// A report, not a verdict: an offset at the window's edge can split.
			if replayDiverged(verdicts, labels) {
				diverged++
				fmt.Printf("seed=%d REPLAY DIVERGED: replays did not reach the same state\n", s)
			}
		}
	}
	if how == killOnQueued && replay > 1 {
		fmt.Printf("replay diverged: %d/%d seeds\n", diverged, n)
	}
	if failed > 0 {
		return exitFail
	}
	return exitPass
}

// replayDiverged reports whether a seed's replays reached different states.
// Same verdict is not enough: a different label means the kill landed
// somewhere else, so that counts as diverged too.
func replayDiverged(verdicts, labels map[string]int) bool {
	return len(verdicts) > 1 || len(labels) > 1
}

// fmtCounts prints counts as "KEY k/total", sorted by key.
func fmtCounts(m map[string]int, total int) string {
	var parts []string
	for _, k := range slices.Sorted(maps.Keys(m)) {
		parts = append(parts, fmt.Sprintf("%s %d/%d", k, m[k], total))
	}
	return strings.Join(parts, ", ")
}
