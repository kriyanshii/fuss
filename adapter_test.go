package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestFakeAdapterBasic tests the fake adapter's basic functionality.
func TestFakeAdapterBasic(t *testing.T) {
	adapter := NewFakeAdapter()
	
	if adapter.Name() != "fake" {
		t.Errorf("expected name 'fake', got %q", adapter.Name())
	}
	
	// Test binary resolution
	bin, source, err := adapter.ResolveBinary("")
	if err != nil {
		t.Fatalf("ResolveBinary failed: %v", err)
	}
	if bin != "/bin/sh" {
		t.Errorf("expected /bin/sh, got %s", bin)
	}
	if source != "default" {
		t.Errorf("expected source 'default', got %s", source)
	}
}

// TestFakeAdapterTrial tests running a trial with the fake adapter.
func TestFakeAdapterTrial(t *testing.T) {
	adapter := NewFakeAdapter()
	
	// Create temp dir
	dir, err := os.MkdirTemp("", "test-fake-")
	if err != nil {
		t.Fatalf("MkdirTemp failed: %v", err)
	}
	defer os.RemoveAll(dir)
	
	// Initialize trial
	at, err := adapter.Init(42, dir, "/bin/sh")
	if err != nil {
		t.Fatalf("Init failed: %v", err)
	}
	
	if at.Seed() != 42 {
		t.Errorf("expected seed 42, got %d", at.Seed())
	}
	
	if !strings.Contains(at.RunID(), "42") {
		t.Errorf("expected RunID to contain seed, got %s", at.RunID())
	}
	
	// Create log
	log, err := os.Create(filepath.Join(dir, "test.log"))
	if err != nil {
		t.Fatalf("Create log failed: %v", err)
	}
	defer log.Close()
	
	// Start
	cmd, err := adapter.Start(at, log)
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	
	// Wait for it to finish
	time.Sleep(time.Second)
	cmd.Wait()
	
	// Check the record
	rec := at.RecordContent()
	if !strings.Contains(rec, "start") {
		t.Errorf("record should contain 'start', got: %s", rec)
	}
	if !strings.Contains(rec, "end") {
		t.Errorf("record should contain 'end', got: %s", rec)
	}
	
	// Get status
	st, err := adapter.Status(at)
	if err != nil {
		t.Fatalf("Status failed: %v", err)
	}
	
	if st.IsRunning() {
		t.Errorf("process should not be running")
	}
	
	// Check
	problems := adapter.Check(at, false, "completed")
	if len(problems) > 0 {
		t.Errorf("check should pass, got problems: %v", problems)
	}
}

// TestFakeAdapterZombieControl tests the zombie control check.
func TestFakeAdapterZombieControl(t *testing.T) {
	adapter := NewFakeAdapter()
	
	// Run zombie control - should pass
	err := adapter.RunControl("zombie", 1, "/bin/sh")
	if err != nil {
		t.Errorf("zombie control should pass, got: %v", err)
	}
}

// TestFakeAdapterDataLossControl tests the data loss control check.
func TestFakeAdapterDataLossControl(t *testing.T) {
	adapter := NewFakeAdapter()
	
	// Run data loss control - should pass
	err := adapter.RunControl("data-loss", 1, "/bin/sh")
	if err != nil {
		t.Errorf("data-loss control should pass, got: %v", err)
	}
}

// TestFakeAdapterKill tests killing the fake adapter during execution.
func TestFakeAdapterKill(t *testing.T) {
	adapter := NewFakeAdapter()
	
	dir, err := os.MkdirTemp("", "test-fake-kill-")
	if err != nil {
		t.Fatalf("MkdirTemp failed: %v", err)
	}
	defer os.RemoveAll(dir)
	
	at, err := adapter.Init(99, dir, "/bin/sh")
	if err != nil {
		t.Fatalf("Init failed: %v", err)
	}
	
	log, err := os.Create(filepath.Join(dir, "test.log"))
	if err != nil {
		t.Fatalf("Create log failed: %v", err)
	}
	defer log.Close()
	
	cmd, err := adapter.Start(at, log)
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	
	// Kill it quickly
	time.Sleep(50 * time.Millisecond)
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("Kill failed: %v", err)
	}
	cmd.Wait()
	
	// Check should pass (no data loss configured)
	problems := adapter.Check(at, true, "middle")
	if len(problems) > 0 {
		t.Errorf("check should pass after kill, got problems: %v", problems)
	}
}

// TestAdapterInterface verifies all adapters implement the interface correctly.
func TestAdapterInterface(t *testing.T) {
	adapters := []struct {
		name string
		adapter Adapter
	}{
		{"fake", NewFakeAdapter()},
		// We can't test Dagu without a Dagu binary
		// {"dagu", mustNewDaguAdapter(t, "one")},
		// We can't test rqlite without an rqlited binary
		// {"rqlite", NewRqliteAdapter()},
	}
	
	for _, tc := range adapters {
		t.Run(tc.name, func(t *testing.T) {
			// Test that all required methods exist
			if tc.adapter.Name() == "" {
				t.Error("Name() returned empty")
			}
			
			controls := tc.adapter.Controls()
			if len(controls) == 0 {
				t.Error("Controls() returned empty list")
			}
		})
	}
}

// TestBaseTrial tests the BaseTrial implementation.
func TestBaseTrial(t *testing.T) {
	dir, err := os.MkdirTemp("", "test-trial-")
	if err != nil {
		t.Fatalf("MkdirTemp failed: %v", err)
	}
	defer os.RemoveAll(dir)
	
	trial := NewBaseTrial(123, dir, "/bin/test", "prefix")
	
	if trial.Seed() != 123 {
		t.Errorf("expected seed 123, got %d", trial.Seed())
	}
	
	if !strings.Contains(trial.RunID(), "prefix") {
		t.Errorf("expected RunID to contain prefix, got %s", trial.RunID())
	}
	
	if !strings.Contains(trial.RunID(), "123") {
		t.Errorf("expected RunID to contain seed, got %s", trial.RunID())
	}
	
	if trial.Dir() != dir {
		t.Errorf("expected dir %s, got %s", dir, trial.Dir())
	}
	
	if trial.BinaryPath() != "/bin/test" {
		t.Errorf("expected binary /bin/test, got %s", trial.BinaryPath())
	}
	
	// Test data storage
	trial.Data()["key"] = "value"
	if trial.Data()["key"] != "value" {
		t.Error("Data() storage not working")
	}
	
	// Test record
	err = os.WriteFile(trial.Record(), []byte("test content\n"), 0o644)
	if err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}
	
	content := trial.RecordContent()
	if content != "test content\n" {
		t.Errorf("expected 'test content\\n', got %q", content)
	}
}
