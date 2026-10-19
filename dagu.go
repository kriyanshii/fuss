package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// daguBin is the Dagu binary every command runs. main sets it once from
// resolveDagu before anything else, and prints it.
var daguBin string

// resolveDagu returns the absolute path of target, or of dagu on PATH when
// target is empty, and which of the two it came from.
func resolveDagu(target string) (path, source string, err error) {
	name, source := "dagu", "PATH"
	if target != "" {
		name, source = target, "-target"
	}
	p, err := exec.LookPath(name)
	if err != nil {
		return "", "", fmt.Errorf("no Dagu binary (%s %q): %w; pass -target or put dagu on PATH", source, name, err)
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", "", err
	}
	return abs, source, nil
}

// DAG shapes. "one" must stay byte-for-byte the same so earlier seeds replay.
const (
	dagOne   = "one"   // step a: start, sleep 2, end
	dagTwo   = "two"   // step a then b (depends on a): start, sleep 1, end each
	dagHuman = "human" // human task h, then after (depends on h): start, sleep 1, end
)

// writeDAG writes a DAG whose steps mark start and end in the record.
// The record path is baked in because Dagu does not pass our environment
// to steps; it also makes the steps findable by waitGone.
func writeDAG(dir, record, shape string) (string, error) {
	var dag string
	switch shape {
	case dagOne:
		dag = fmt.Sprintf(`steps:
  - name: a
    command: sh -c 'echo "start a $DAG_RUN_ID" >> %[1]s; sleep 2; echo "end a $DAG_RUN_ID" >> %[1]s'
`, record)
	case dagTwo:
		dag = fmt.Sprintf(`steps:
  - name: a
    command: sh -c 'echo "start a $DAG_RUN_ID" >> %[1]s; sleep 1; echo "end a $DAG_RUN_ID" >> %[1]s'
  - name: b
    depends: [a]
    command: sh -c 'echo "start b $DAG_RUN_ID" >> %[1]s; sleep 1; echo "end b $DAG_RUN_ID" >> %[1]s'
`, record)
	case dagHuman:
		dag = fmt.Sprintf(`steps:
  - id: h
    action: human.task
    with:
      prompt: Continue?
  - id: after
    depends: [h]
    run: sh -c 'echo "start after $DAG_RUN_ID" >> %[1]s; sleep 1; echo "end after $DAG_RUN_ID" >> %[1]s'
`, record)
	default:
		return "", fmt.Errorf("unknown DAG shape %q", shape)
	}
	path := filepath.Join(dir, "rec.yaml")
	return path, os.WriteFile(path, []byte(dag), 0o644)
}

// startDagu launches `dagu start` without waiting for it.
// Dagu's own output goes to log so it never mixes with Fuss output.
func startDagu(home, dagPath, runID string, log *os.File) (*exec.Cmd, error) {
	cmd := exec.Command(daguBin, "start", "--run-id="+runID, dagPath)
	cmd.Env = daguEnv(home, procStale)
	cmd.Stdout = log
	cmd.Stderr = log
	return cmd, cmd.Start()
}

// Dagu marks a dead run Failed once its proc heartbeat is older than the
// stale threshold (default 90s). Lowering it makes rule 3 testable in seconds;
// the heartbeat interval must stay below the threshold or Dagu rejects the config.
const (
	procHeartbeat = 1 * time.Second
	procStale     = 5 * time.Second
)

// daguEnv is the environment for every Dagu command Fuss runs.
// stale is procStale everywhere except the zombie control's reader.
func daguEnv(home string, stale time.Duration) []string {
	return append(os.Environ(),
		"DAGU_HOME="+home,
		"DAGU_PROC_HEARTBEAT_INTERVAL="+procHeartbeat.String(),
		"DAGU_PROC_STALE_THRESHOLD="+stale.String(),
	)
}
