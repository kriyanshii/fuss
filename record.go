package main

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// readRecord returns the outside record; a missing file is an empty record.
func readRecord(record string) string {
	b, _ := os.ReadFile(record)
	return string(b)
}

// stepStarted reports whether the record shows step starting for runID.
func stepStarted(rec, step, runID string) bool {
	return strings.Contains(rec, "start "+step+" "+runID+"\n")
}

// stepEnded reports whether the record shows step finishing for runID.
func stepEnded(rec, step, runID string) bool {
	return strings.Contains(rec, "end "+step+" "+runID+"\n")
}

// afterMarker returns the record after Fuss's "retry <runID>" line, i.e. what
// the retry attempt wrote; "" if there is no marker.
func afterMarker(rec, runID string) string {
	marker := "retry " + runID + "\n"
	i := strings.Index(rec, marker)
	if i < 0 {
		return ""
	}
	return rec[i+len(marker):]
}

// waitFor blocks until the record contains line, checking every poll.
func waitFor(record, line string, poll, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for !strings.Contains(readRecord(record), line) {
		if time.Now().After(deadline) {
			return fmt.Errorf("record has no %q after %v", line, timeout)
		}
		time.Sleep(poll)
	}
	return nil
}

// recordPhase names how far the steps got, judged only from the outside record.
func recordPhase(record, shape string) string {
	rec := readRecord(record)
	if shape == dagTwo {
		switch {
		case strings.Contains(rec, "end b "):
			return "after"
		case strings.Contains(rec, "start b "):
			return "in-b"
		case strings.Contains(rec, "end a "):
			return "between"
		case strings.Contains(rec, "start a "):
			return "in-a"
		default:
			return "before-a"
		}
	}
	switch {
	case strings.Contains(rec, "end a "):
		return "after-step"
	case strings.Contains(rec, "start a "):
		return "in-step"
	default:
		return "before-step"
	}
}
