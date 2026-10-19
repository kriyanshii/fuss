package main

import (
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"time"
)

// waitGone blocks until no process has marker in its command line.
// It returns an error if the timeout passes first (STUCK) or pgrep fails.
func waitGone(marker string, timeout time.Duration) error {
	// pgrep -f takes a regex; temp paths contain dots.
	pattern := regexp.QuoteMeta(marker)
	deadline := time.Now().Add(timeout)
	for {
		alive, err := anyAlive(pattern)
		if err != nil {
			return err
		}
		if !alive {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("STUCK: process matching %q still alive after %v", marker, timeout)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// anyAlive runs pgrep directly, not through a shell, so no wrapper
// process carries the pattern in its arguments.
// pgrep exits 0 if something matched, 1 if nothing matched, and >1 on error.
// Treating an error as "nothing matched" would turn a broken pgrep into a false PASS.
func anyAlive(pattern string) (bool, error) {
	err := exec.Command("pgrep", "-f", pattern).Run()
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false, nil
	}
	return false, fmt.Errorf("pgrep failed: %w", err)
}
