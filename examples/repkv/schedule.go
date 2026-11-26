package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// scheduleCheck reports two facts, then an experiment line.
//
// The schedule fact is whether Fuss's cut fell in the requested window.
// The history fact is Porcupine's answer and does not depend on that window.
// The experiment is PASS only when the schedule occurred, the history is
// legal, and a read started after the cut. A missing post-cut read keeps
// the history fact and marks the experiment unknown.
func scheduleCheck(data []byte, initial, schedule string) string {
	hist, err := historyLine(data, initial)
	if err != nil {
		hist = "history unknown reason=" + err.Error()
	}
	ops, _ := parseHistory(data)
	var write *histOp
	for i := range ops {
		if ops[i].op == "write" {
			write = &ops[i]
			break
		}
	}
	cut, hasCut := parseCut(data)
	window := observedWindow(write, cut, hasCut)
	occurred := window == schedule && schedule != ""
	read := readAfterCut(ops, cut, hasCut)
	var b strings.Builder
	switch {
	case schedule != "before" && schedule != "pending" && schedule != "after":
		fmt.Fprintf(&b, "experiment unknown reason=bad_schedule\n")
	case !read:
		fmt.Fprintf(&b, "experiment unknown reason=no_post_cut_read\n")
	case !occurred:
		fmt.Fprintf(&b, "experiment unknown reason=schedule_miss\n")
	case strings.HasPrefix(hist, "history illegal"):
		fmt.Fprintf(&b, "experiment illegal\n")
	case strings.HasPrefix(hist, "history legal"):
		fmt.Fprintf(&b, "experiment pass\n")
	default:
		fmt.Fprintf(&b, "experiment unknown reason=history\n")
	}
	if occurred {
		fmt.Fprintf(&b, "schedule %s occurred\n", schedule)
	} else {
		fmt.Fprintf(&b, "schedule %s missed\n", schedule)
	}
	fmt.Fprintf(&b, "window %s\n", window)
	fmt.Fprintf(&b, "%s\n", hist)
	if read {
		fmt.Fprintf(&b, "read after cut\n")
	}
	fmt.Fprintf(&b, "translation effect=optional bound=open\n")
	b.Write(data)
	return b.String()
}

func observedWindow(write *histOp, cut time.Time, hasCut bool) string {
	if !hasCut || write == nil {
		return "none"
	}
	switch {
	case cut.Before(write.start):
		return "before"
	case write.start.Before(cut) && cut.Before(write.end):
		return "pending"
	case write.observe == "ok" && write.end.Before(cut):
		return "after"
	default:
		return "none"
	}
}

func readAfterCut(ops []histOp, cut time.Time, hasCut bool) bool {
	if !hasCut {
		return false
	}
	for _, op := range ops {
		if op.op == "read" && op.definite() && op.start.After(cut) {
			return true
		}
	}
	return false
}

func parseCut(data []byte) (time.Time, bool) {
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var ev struct {
			Kind string `json:"kind"`
			At   string `json:"at"`
		}
		if json.Unmarshal([]byte(line), &ev) != nil || ev.Kind != "cut" || ev.At == "" {
			continue
		}
		at, err := time.Parse(time.RFC3339Nano, ev.At)
		if err != nil {
			continue
		}
		return at, true
	}
	return time.Time{}, false
}
