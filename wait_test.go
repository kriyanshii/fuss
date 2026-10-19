package main

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A process carrying the marker keeps waitGone busy until it exits.
func TestWaitGone(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "runs.txt")
	cmd := exec.Command("sh", "-c", "sleep 1; : "+marker)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go cmd.Wait() // reap it so it doesn't linger as a zombie

	start := time.Now()
	if err := waitGone(marker, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took < 800*time.Millisecond {
		t.Fatalf("returned after %v; should have waited for the sleep", took)
	}
}

// A process that outlives the timeout produces STUCK, not success.
func TestWaitGoneStuck(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "runs.txt")
	cmd := exec.Command("sh", "-c", "sleep 3; : "+marker)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()

	err := waitGone(marker, 500*time.Millisecond)
	if err == nil || !strings.HasPrefix(err.Error(), "STUCK") {
		t.Fatalf("got %v, want STUCK", err)
	}
}

// With nothing running, pgrep must not match itself.
func TestWaitGoneNothing(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "runs.txt")
	start := time.Now()
	if err := waitGone(marker, 2*time.Second); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 500*time.Millisecond {
		t.Fatalf("took %v with nothing running; pgrep may be matching itself", took)
	}
}
