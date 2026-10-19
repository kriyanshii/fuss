package main

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// strandedFixture is a real stranded run from scripts/strand_repro.sh: run
// Queued, h succeeded, no queue item. Its encryption key, stale locks and
// logs were left out, and the OS user name was replaced.
//
//go:embed all:testdata/stranded
var strandedFixture embed.FS

var stagedRunDir = regexp.MustCompile(`^dag-run_\d{8}_\d{6}Z_(.+)$`)

// stagedRun finds the one DAG-run in a staged home and returns its DAG name
// and run ID, read from the run-store layout (see README limits).
func stagedRun(home string) (dag, runID string, err error) {
	m, _ := filepath.Glob(filepath.Join(home, "data", "dag-runs", "*", "dag-runs", "*", "*", "*", "dag-run_*"))
	if len(m) != 1 {
		return "", "", fmt.Errorf("staged home has %d DAG-runs, want 1", len(m))
	}
	sub := stagedRunDir.FindStringSubmatch(filepath.Base(m[0]))
	if sub == nil {
		return "", "", fmt.Errorf("unrecognised DAG-run dir %q", filepath.Base(m[0]))
	}
	rel, err := filepath.Rel(filepath.Join(home, "data", "dag-runs"), m[0])
	if err != nil {
		return "", "", err
	}
	return strings.SplitN(rel, string(filepath.Separator), 2)[0], sub[1], nil
}

// judgeStaged copies a staged Dagu home into a fresh temp dir and runs the
// final steps of a human trial on it: status, the repeat complete, scheduler
// wait, status, queue items, record, verdict. It writes only into its own temp dir, never a real Dagu
// state dir. The fixture's DAG writes no record, so starts is always 0 here
// and stagedVerdict judges from the status and the queue items.
func judgeStaged(fsys fs.FS, name string) (humanTrial, error) {
	dir, err := os.MkdirTemp("", "fuss-staged-"+name+"-")
	if err != nil {
		return humanTrial{}, err
	}
	t := humanTrial{dir: dir, home: filepath.Join(dir, "home"), record: filepath.Join(dir, "runs.txt")}
	if err := os.CopyFS(t.home, fsys); err != nil {
		return t, fmt.Errorf("stage %s: %w", name, err)
	}
	dag, runID, err := stagedRun(t.home)
	if err != nil {
		return t, err
	}
	t.runID = runID
	if _, err := humanStatusFor(t.home, dag, runID); err != nil {
		return t, err
	}
	// The repeat complete, as in runHumanTrial.
	c := humanCmd(t.home, completeArgsFor(dag, runID)...)
	closeOut := outFiles(c, t.dir, "complete2")
	err = runWithin(c, repeatTimeout)
	closeOut()
	if c.ProcessState == nil {
		return t, fmt.Errorf("repeat complete: %w", err)
	}
	t.repeatExit = c.ProcessState.ExitCode()
	t.repeatHung = errors.Is(err, errTimedOut)
	t.repeatOut = readTrimmed(filepath.Join(t.dir, "complete2.out"))
	t.repeatErr = readTrimmed(filepath.Join(t.dir, "complete2.err"))
	if err := runScheduler(t, schedulerWait); err != nil {
		return t, err
	}
	if t.final, err = humanStatusFor(t.home, dag, runID); err != nil {
		return t, err
	}
	t.items = queueItems(t.home, runID)
	t.starts = strings.Count(readRecord(t.record), "start after "+runID+"\n")
	t.verdict = stagedVerdict(t.final, t.items, t.starts)
	return t, nil
}

// stagedVerdict is humanVerdict for a staged run, whose DAG writes no record,
// so "resumed" is judged from the final status: Succeeded with its queue item
// used up is RESUMED. Anything else, such as a run still waiting, is judged as
// usual. Real trials keep humanVerdict, where a missing record is a warning.
func stagedVerdict(st runStatus, items, starts int) string {
	if starts == 0 && st.Run == "Succeeded" && items == 0 {
		return "RESUMED"
	}
	return humanVerdict(st, items, starts)
}

// strandedControl checks the STRANDED check catches a known stranded run
// (RULEBOOK section 4). A control must FAIL: it returns nil only if the
// staged run is judged STRANDED.
func strandedControl() error {
	fsys, err := fs.Sub(strandedFixture, "testdata/stranded")
	if err != nil {
		return err
	}
	t, err := judgeStaged(fsys, "stranded")
	if err != nil {
		return fmt.Errorf("%w (dir=%s)", err, t.dir)
	}
	if t.verdict != "STRANDED" {
		return fmt.Errorf("staged stranded run judged %s (final=%s items=%d starts=%d), want STRANDED (dir=%s)",
			t.verdict, t.final.Run, t.items, t.starts, t.dir)
	}
	os.RemoveAll(t.dir)
	return nil
}
