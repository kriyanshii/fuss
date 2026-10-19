package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"
)

// statusSucceeded and statusQueued are ir.Succeeded and ir.Queued (internal/ir/status.go).
const (
	statusSucceeded = 4
	statusQueued    = 5
)

// finalizeTimes are when each finalize event was first seen; zero means not seen.
// Dagu's status file is read here only to label timing, never to judge a run.
type finalizeTimes struct {
	end, saved, compacted, exited time.Time
	polls                         int
	start                         time.Time
}

// measureOnce runs Dagu to completion with no kill, polling every millisecond.
// With watchStatus false it never reads Dagu's status file, matching what kill
// mode observes before its kill; comparing the two shows Fuss's observer effect.
func measureOnce(seed int64, watchStatus bool) (finalizeTimes, error) {
	var ft finalizeTimes
	dir, err := os.MkdirTemp("", fmt.Sprintf("fuss-measure-%d-", seed))
	if err != nil {
		return ft, err
	}
	defer os.RemoveAll(dir)
	home, record := filepath.Join(dir, "home"), filepath.Join(dir, "runs.txt")
	dagPath, err := writeDAG(dir, record, dagOne)
	if err != nil {
		return ft, err
	}
	log, err := os.Create(filepath.Join(dir, "dagu.log"))
	if err != nil {
		return ft, err
	}
	defer log.Close()

	runID := fmt.Sprintf("fuss-%d", seed)
	ft.start = time.Now()
	cmd, err := startDagu(home, dagPath, runID, log)
	if err != nil {
		return ft, err
	}
	exited := make(chan time.Time, 1)
	go func() { cmd.Wait(); exited <- time.Now() }()

	var statusPath string
	var firstIno uint64
	for {
		// Poll once more after exit so events just before it are not missed.
		var done bool
		select {
		case ft.exited = <-exited:
			done = true
		default:
		}
		now := time.Now()
		ft.polls++
		if ft.end.IsZero() && strings.Contains(readRecord(record), "end a ") {
			ft.end = now
		}
		if watchStatus && statusPath == "" {
			statusPath = findStatusFile(home, runID)
		}
		if statusPath != "" {
			last, _, ino := statusSnapshot(statusPath)
			if firstIno == 0 {
				firstIno = ino
			}
			if ft.saved.IsZero() && last == statusSucceeded {
				ft.saved = now
			}
			if ft.compacted.IsZero() && ino != 0 && ino != firstIno {
				ft.compacted = now
			}
		}
		if done {
			return ft, nil
		}
		time.Sleep(time.Millisecond)
	}
}

// findStatusFile locates the run's status.jsonl, or "" if not created yet.
func findStatusFile(home, runID string) string {
	m, _ := filepath.Glob(filepath.Join(home, "data/dag-runs/rec/dag-runs/*/*/*", "dag-run_*_"+runID, "*", "status.jsonl"))
	if len(m) == 0 {
		return ""
	}
	return m[0]
}

// statusSnapshot returns the top-level status of the last complete line,
// the number of complete lines, and the file's inode.
func statusSnapshot(path string) (last, lines int, ino uint64) {
	b, err := os.ReadFile(path)
	if err != nil {
		return -1, 0, 0
	}
	if fi, err := os.Stat(path); err == nil {
		ino = uint64(fi.Sys().(*syscall.Stat_t).Ino)
	}
	b = b[:bytes.LastIndexByte(b, '\n')+1] // drop a partial trailing line
	all := bytes.Split(bytes.TrimSpace(b), []byte("\n"))
	var st struct {
		Status int `json:"status"`
	}
	if len(b) == 0 || json.Unmarshal(all[len(all)-1], &st) != nil {
		return -1, 0, ino
	}
	return st.Status, len(all), ino
}

// finalizeLabel names how far Dagu got in finalizing, from its status file.
// Compaction rewrites the file to one line, so a single Succeeded line means
// compacted. For labels only: verdicts come from the record and dagu status.
func finalizeLabel(home, runID string) string {
	path := findStatusFile(home, runID)
	if path == "" {
		return "no-status-file"
	}
	last, lines, _ := statusSnapshot(path)
	switch {
	case last != statusSucceeded:
		return "before-save"
	case lines > 1:
		return "after-save"
	default:
		return "after-compact"
	}
}

// runMeasure prints the distribution of each finalize gap over n runs.
func runMeasure(seed int64, n int, watchStatus bool) {
	gaps := map[string][]time.Duration{}
	order := []string{"end→saved", "saved→compacted", "compacted→exit", "end→exit"}
	missing := map[string]int{}
	var polls int
	var pollTime time.Duration
	for s := seed; s < seed+int64(n); s++ {
		ft, err := measureOnce(s, watchStatus)
		if err != nil {
			fatal(err)
		}
		polls += ft.polls
		pollTime += ft.exited.Sub(ft.start)
		add := func(name string, from, to time.Time) {
			if from.IsZero() || to.IsZero() {
				missing[name]++
				return
			}
			gaps[name] = append(gaps[name], to.Sub(from))
		}
		add("end→saved", ft.end, ft.saved)
		add("saved→compacted", ft.saved, ft.compacted)
		add("compacted→exit", ft.compacted, ft.exited)
		add("end→exit", ft.end, ft.exited)
	}
	fmt.Printf("%d runs, poll interval achieved: %v\n", n, (pollTime / time.Duration(polls)).Round(10*time.Microsecond))
	for _, name := range order {
		g := gaps[name]
		if len(g) == 0 {
			fmt.Printf("%-16s never seen (%d runs)\n", name, missing[name])
			continue
		}
		slices.Sort(g)
		q := func(p float64) time.Duration { return g[int(p*float64(len(g)-1))].Round(100 * time.Microsecond) }
		fmt.Printf("%-16s min=%-8v p50=%-8v p90=%-8v max=%-8v not-seen=%d\n", name, q(0), q(0.5), q(0.9), q(1), missing[name])
	}
}
