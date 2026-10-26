package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestDaguIntegration tests the Dagu adapter with a real Dagu binary.
// Run with: go test -v -run TestDaguIntegration -args -target=/path/to/dagu
func TestDaguIntegration(t *testing.T) {
	// Check if Dagu binary is available
	bin, source, err := resolveDagu(*testTarget)
	if err != nil {
		t.Skip("Dagu binary not available:", err)
	}
	t.Logf("Using Dagu binary: %s (from %s)", bin, source)
	
	adapter, err := NewDaguAdapter(dagOne)
	if err != nil {
		t.Fatalf("NewDaguAdapter failed: %v", err)
	}
	
	// Run zombie control
	t.Run("ZombieControl", func(t *testing.T) {
		err := adapter.RunControl("zombie", 1, bin)
		if err != nil {
			t.Errorf("Zombie control failed: %v", err)
		}
	})
	
	// Run a simple trial
	t.Run("SimpleTrial", func(t *testing.T) {
		dir, err := os.MkdirTemp("", "test-dagu-integration-")
		if err != nil {
			t.Fatalf("MkdirTemp failed: %v", err)
		}
		defer os.RemoveAll(dir)
		
		at, err := adapter.Init(42, dir, bin)
		if err != nil {
			t.Fatalf("Init failed: %v", err)
		}
		
		log, err := os.Create(at.(*BaseTrial).LogPath())
		if err != nil {
			t.Fatalf("Create log failed: %v", err)
		}
		defer log.Close()
		
		cmd, err := adapter.Start(at, log)
		if err != nil {
			t.Fatalf("Start failed: %v", err)
		}
		
		// Let it run briefly, then kill
		// time.Sleep(100 * time.Millisecond)
		// _ = cmd.Process.Kill()
		
		// Wait for completion
		_ = cmd.Wait()
		
		// Check the record
		rec := at.RecordContent()
		if !strings.Contains(rec, "start a") {
			t.Errorf("record should contain 'start a', got: %s", rec)
		}
		
		// Get status
		st, err := adapter.Status(at)
		if err != nil {
			t.Errorf("Status failed: %v", err)
		} else if st != nil {
			t.Logf("Final status: %s", st.String())
		}
		
		// Run checks
		problems := adapter.Check(at, false, "completed")
		if len(problems) > 0 {
			t.Errorf("Check found problems: %v", problems)
		}
	})
}

// TestRqliteIntegration tests the rqlite adapter with a real rqlited binary.
// Run with: go test -v -run TestRqliteIntegration -args -target=/path/to/rqlited
func TestRqliteIntegration(t *testing.T) {
	// Check if rqlite binary is available
	adapter := NewRqliteAdapter()
	bin, source, err := adapter.ResolveBinary(*testTarget)
	if err != nil {
		// Try default
		bin, source, err = adapter.ResolveBinary("")
		if err != nil {
			t.Skip("rqlited binary not available:", err)
		}
	}
	
	// Verify the binary actually exists and is executable
	if _, err := exec.LookPath(bin); err != nil {
		t.Skip("rqlited binary not found:", err)
	}
	
	t.Logf("Using rqlited binary: %s (from %s)", bin, source)
	
	// Run durability control
	t.Run("DurabilityControl", func(t *testing.T) {
		err := adapter.RunControl("durability", 1, bin)
		if err != nil {
			t.Errorf("Durability control failed: %v", err)
		}
	})
	
	// Run a simple trial
	t.Run("SimpleTrial", func(t *testing.T) {
		dir, err := os.MkdirTemp("", "test-rqlite-integration-")
		if err != nil {
			t.Fatalf("MkdirTemp failed: %v", err)
		}
		defer os.RemoveAll(dir)
		
		at, err := adapter.Init(42, dir, bin)
		if err != nil {
			t.Fatalf("Init failed: %v", err)
		}
		
		log, err := os.Create(at.(*BaseTrial).LogPath())
		if err != nil {
			t.Fatalf("Create log failed: %v", err)
		}
		defer log.Close()
		
		cmd, err := adapter.Start(at, log)
		if err != nil {
			t.Fatalf("Start failed: %v", err)
		}
		
		// Clean shutdown
		defer func() {
			if cmd.Process != nil {
				cmd.Process.Kill()
				cmd.Wait()
			}
		}()
		
		// Get status to verify it's running
		st, err := adapter.Status(at)
		if err != nil {
			t.Errorf("Status failed: %v", err)
		} else if st != nil {
			t.Logf("Status: %s", st.String())
			if !st.IsRunning() {
				t.Error("rqlite should be running")
			}
		}
	})
}

// testTarget is set by -args -target=... on the command line for integration tests.
var testTarget = new(string)

func init() {
	// This will be set by -args -target=...
	*testTarget = ""
}
