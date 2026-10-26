package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// RqliteAdapter implements the Adapter interface for rqlite.
// It tests crash durability: acknowledged writes must survive a crash.
type RqliteAdapter struct {
	httpPort int
	raftPort int
}

func NewRqliteAdapter() *RqliteAdapter {
	return &RqliteAdapter{
		httpPort: 24001, // use non-default port to avoid conflicts
		raftPort: 24002,
	}
}

func (r *RqliteAdapter) Name() string {
	return "rqlite"
}

func (r *RqliteAdapter) ResolveBinary(targetFlag string) (string, string, error) {
	name, source := "rqlited", "PATH"
	if targetFlag != "" {
		name, source = targetFlag, "-target"
	}
	p, err := exec.LookPath(name)
	if err != nil {
		return "", "", fmt.Errorf("no rqlited binary (%s %q): %w", source, name, err)
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", "", err
	}
	return abs, source, nil
}

func (r *RqliteAdapter) Init(seed int64, dir, binaryPath string) (AdapterTrial, error) {
	t := NewBaseTrial(seed, dir, binaryPath, "fuss-rqlite")
	if err := os.MkdirAll(t.Home(), 0o755); err != nil {
		return nil, err
	}
	
	t.Data()["httpPort"] = r.httpPort
	t.Data()["raftPort"] = r.raftPort
	t.Data()["nodeID"] = fmt.Sprintf("node-%d", seed)
	t.Data()["tableCreated"] = false
	
	return t, nil
}

func (r *RqliteAdapter) Start(at AdapterTrial, log *os.File) (*exec.Cmd, error) {
	t, ok := at.(*BaseTrial)
	if !ok {
		return nil, fmt.Errorf("expected *BaseTrial, got %T", at)
	}
	
	nodeID := t.Data()["nodeID"].(string)
	httpAddr := fmt.Sprintf("localhost:%d", r.httpPort)
	raftAddr := fmt.Sprintf("localhost:%d", r.raftPort)
	
	cmd := exec.Command(
		t.BinaryPath(),
		"-node-id", nodeID,
		"-http-addr", httpAddr,
		"-raft-addr", raftAddr,
		t.Home(),
	)
	
	cmd.Stdout = log
	cmd.Stderr = log
	
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	
	// Wait for rqlite to be ready
	ready := false
	for i := 0; i < 50; i++ {
		if r.isReady(r.httpPort) {
			ready = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	
	if !ready {
		cmd.Process.Kill()
		return nil, fmt.Errorf("rqlite did not become ready")
	}
	
	// Create test table if not already created
	if !t.Data()["tableCreated"].(bool) {
		if err := r.execute(r.httpPort, "CREATE TABLE IF NOT EXISTS test (tag TEXT PRIMARY KEY)"); err != nil {
			cmd.Process.Kill()
			return nil, fmt.Errorf("failed to create table: %w", err)
		}
		t.Data()["tableCreated"] = true
		
		// Record table creation in the record file
		appendToFile(t.Record(), fmt.Sprintf("table created %s\n", t.RunID()))
	}
	
	return cmd, nil
}

func (r *RqliteAdapter) isReady(port int) bool {
	// /status is 200 before Raft elects a leader; writes then fail with
	// "leader not found". /readyz is 200 only once the node can serve.
	url := fmt.Sprintf("http://localhost:%d/readyz", port)
	resp, err := http.Get(url)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == 200
}

func (r *RqliteAdapter) Status(at AdapterTrial) (AdapterStatus, error) {
	url := fmt.Sprintf("http://localhost:%d/status", r.httpPort)
	
	resp, err := http.Get(url)
	if err != nil {
		return rqliteStatus{State: "unreachable"}, nil
	}
	defer resp.Body.Close()
	
	if resp.StatusCode != 200 {
		return rqliteStatus{State: "error"}, nil
	}
	
	return rqliteStatus{State: "running"}, nil
}

func (r *RqliteAdapter) Settle(query func() (AdapterStatus, error), deadline time.Duration) (AdapterStatus, error) {
	// rqlite doesn't have zombie states - once process is dead, it's dead
	st, err := query()
	return st, err
}

func (r *RqliteAdapter) Check(at AdapterTrial, killed bool, phase string) []error {
	t, ok := at.(*BaseTrial)
	if !ok {
		return []error{fmt.Errorf("expected *BaseTrial, got %T", at)}
	}
	
	var problems []error
	
	if !killed {
		// No crash, no durability check needed
		return problems
	}
	
	// Parse acknowledged writes from the record
	acked := r.parseAckedWrites(t.RecordContent())
	if len(acked) == 0 {
		// No writes were acknowledged, nothing to check
		return problems
	}
	
	// Restart rqlite to check durability
	log, err := os.Create(filepath.Join(t.Dir(), "check.log"))
	if err != nil {
		problems = append(problems, fmt.Errorf("failed to create check log: %w", err))
		return problems
	}
	defer log.Close()
	
	cmd, err := r.Start(t, log)
	if err != nil {
		problems = append(problems, fmt.Errorf("failed to restart for check: %w", err))
		return problems
	}
	defer func() {
		if cmd.Process != nil {
			cmd.Process.Kill()
			cmd.Wait()
		}
	}()
	
	// Check each acknowledged write
	for _, tag := range acked {
		if !r.rowExists(r.httpPort, tag) {
			problems = append(problems, fmt.Errorf("LOST WRITE: tag %q was acknowledged but missing after restart", tag))
		}
	}
	
	return problems
}

func (r *RqliteAdapter) parseAckedWrites(rec string) []string {
	var tags []string
	for _, line := range strings.Split(rec, "\n") {
		if strings.HasPrefix(line, "ack ") {
			tag := strings.TrimSpace(strings.TrimPrefix(line, "ack "))
			if tag != "" {
				tags = append(tags, tag)
			}
		}
	}
	return tags
}

func (r *RqliteAdapter) rowExists(port int, tag string) bool {
	url := fmt.Sprintf("http://localhost:%d/db/query?level=strong", port)
	query := fmt.Sprintf(`[["SELECT count(*) FROM test WHERE tag = '%s'"]]`, tag)
	
	resp, err := http.Post(url, "application/json", strings.NewReader(query))
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	
	if resp.StatusCode != 200 {
		return false
	}
	
	body, _ := io.ReadAll(resp.Body)
	var result map[string]interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		return false
	}
	
	results, ok := result["results"].([]interface{})
	if !ok || len(results) == 0 {
		return false
	}
	
	firstResult, ok := results[0].(map[string]interface{})
	if !ok {
		return false
	}
	
	values, ok := firstResult["values"].([]interface{})
	if !ok || len(values) == 0 {
		return false
	}
	
	firstRow, ok := values[0].([]interface{})
	if !ok || len(firstRow) == 0 {
		return false
	}
	
	count, ok := firstRow[0].(float64)
	return ok && count > 0
}

func (r *RqliteAdapter) execute(port int, sql string) error {
	url := fmt.Sprintf("http://localhost:%d/db/execute", port)
	stmt := fmt.Sprintf(`[["%s"]]`, sql)
	
	resp, err := http.Post(url, "application/json", strings.NewReader(stmt))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("execute failed: %d %s", resp.StatusCode, body)
	}
	
	return nil
}

func (r *RqliteAdapter) RunControl(name string, seed int64, binaryPath string) error {
	if name != "durability" {
		return fmt.Errorf("unknown control %q", name)
	}
	
	// Control: write a row, acknowledge it, kill, restart, verify it persists
	dir, err := os.MkdirTemp("", fmt.Sprintf("fuss-ctl-durability-%d-", seed))
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	
	t := NewBaseTrial(seed, dir, binaryPath, "fuss-rqlite-ctl")
	t.Data()["httpPort"] = r.httpPort
	t.Data()["raftPort"] = r.raftPort
	t.Data()["nodeID"] = fmt.Sprintf("node-ctl-%d", seed)
	t.Data()["tableCreated"] = false
	
	if err := os.MkdirAll(t.Home(), 0o755); err != nil {
		return err
	}
	
	log, err := os.Create(filepath.Join(t.Dir(), "control.log"))
	if err != nil {
		return err
	}
	defer log.Close()
	
	// Start rqlite
	cmd, err := r.Start(t, log)
	if err != nil {
		return fmt.Errorf("control: failed to start: %w", err)
	}
	
	// Insert a row
	tag := fmt.Sprintf("control-%d", seed)
	if err := r.execute(r.httpPort, fmt.Sprintf("INSERT INTO test (tag) VALUES ('%s')", tag)); err != nil {
		cmd.Process.Kill()
		return fmt.Errorf("control: failed to insert: %w", err)
	}
	
	// Acknowledge it in the record
	appendToFile(t.Record(), fmt.Sprintf("ack %s\n", tag))
	
	// Kill rqlite
	if err := cmd.Process.Kill(); err != nil {
		return fmt.Errorf("control: failed to kill: %w", err)
	}
	cmd.Wait()
	
	// Restart and check
	log2, err := os.Create(filepath.Join(t.Dir(), "control2.log"))
	if err != nil {
		return err
	}
	defer log2.Close()
	
	// Set tableCreated to true so Start doesn't try to create it again
	t.Data()["tableCreated"] = true
	
	cmd2, err := r.Start(t, log2)
	if err != nil {
		return fmt.Errorf("control: failed to restart: %w", err)
	}
	defer func() {
		cmd2.Process.Kill()
		cmd2.Wait()
	}()
	
	// Check durability
	if !r.rowExists(r.httpPort, tag) {
		return fmt.Errorf("control BROKEN: acknowledged write was lost")
	}
	
	return nil
}

func (r *RqliteAdapter) Controls() []string {
	return []string{"durability"}
}

// rqliteStatus implements AdapterStatus for rqlite.
type rqliteStatus struct {
	State string
}

func (s rqliteStatus) String() string {
	return s.State
}

func (s rqliteStatus) IsRunning() bool {
	return s.State == "running"
}

func appendToFile(path, content string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(content)
	return err
}
