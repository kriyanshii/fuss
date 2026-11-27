package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"fuss/history"
)

// bundleCLI is set from main. saveDir writes one bundle after a single seed.
// failedDir writes one bundle per non-PASS seed, under seed-<seed>/.
// want, when set, is a saved bundle the run must reproduce.
type bundleCLI struct {
	saveDir   string
	failedDir string
	want      *replayBundle
	got       *replayFacts
	// pinNode is true when the scenario names fault.isolate.node.
	// A node_file scenario isolates whoever was leader, so the node index may change.
	pinNode bool
}

var bundleRun bundleCLI

// exportEDNPath is set from -export-edn. One seed writes a Jepsen history there.
var exportEDNPath string

// replayBundle is the portable record of one seeded run.
// format 1. scenario.yaml sits next to bundle.json.
type replayBundle struct {
	Format         int          `json:"format"`
	FussRevision   string       `json:"fuss_revision"`
	Target         bundleTarget `json:"target"`
	Seed           int64        `json:"seed"`
	Scenario       string       `json:"scenario"`
	ScenarioSHA256 string       `json:"scenario_sha256"`
	Inputs         bundleInputs `json:"inputs"`
	Ack            bundleAck    `json:"ack"`
	Cut            bundleCut    `json:"cut"`
	Read           bundleRead   `json:"read"`
	Verdict        string       `json:"verdict"`
}

type bundleTarget struct {
	Name     string `json:"name"`
	Binary   string `json:"binary"`
	Version  string `json:"version,omitempty"`
	Checksum string `json:"checksum"`
}

type bundleInputs struct {
	Seed     int64  `json:"seed"`
	N        int    `json:"n"`
	Scenario string `json:"scenario"`
}

type bundleAck struct {
	Line string `json:"line"`
	Key  string `json:"key,omitempty"`
}

type bundleCut struct {
	Target       string `json:"target"`
	Direction    string `json:"direction"`
	TriggerFired bool   `json:"trigger_fired"`
	LinksCut     int    `json:"links_cut"`
	LinksTotal   int    `json:"links_total"`
	CutTime      string `json:"cut_time,omitempty"`
	LeaderID     string `json:"leader_id,omitempty"`
	LeaderNode   string `json:"leader_node,omitempty"`
}

type bundleRead struct {
	Text     string `json:"text"`
	Key      string `json:"key,omitempty"`
	From     string `json:"from,omitempty"`
	OK       bool   `json:"ok"`
	Missing  bool   `json:"missing"`
	LeaderID string `json:"leader_id,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

type replayFacts struct {
	Verdict       string
	Ack           bundleAck
	Cut           bundleCut
	Read          bundleRead
	Version       string
	Checksum      string
	Nemesis       []NemesisEvent
	HasNemesis    bool
	History       []history.OpRecord
	HasHistory    bool
	PorcupineHTML []byte
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

func fussRevision() string {
	cmd := exec.Command("git", "rev-parse", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		return "unknown"
	}
	rev := strings.TrimSpace(string(out))
	dirty := exec.Command("git", "status", "--porcelain")
	if b, err := dirty.Output(); err == nil && len(bytes.TrimSpace(b)) > 0 {
		rev += "-dirty"
	}
	return rev
}

func probeVersion(bin string) string {
	for _, arg := range []string{"-version", "--version"} {
		cmd := exec.Command(bin, arg)
		var buf bytes.Buffer
		cmd.Stdout = &buf
		cmd.Stderr = &buf
		timer := time.AfterFunc(2*time.Second, func() {
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
		})
		err := cmd.Run()
		timer.Stop()
		if err != nil {
			continue
		}
		line := strings.TrimSpace(buf.String())
		if i := strings.IndexByte(line, '\n'); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		if line != "" {
			return line
		}
	}
	return ""
}

func readAckLine(record string) bundleAck {
	for _, ln := range strings.Split(record, "\n") {
		ln = strings.TrimSpace(ln)
		if strings.HasPrefix(ln, "ack ") {
			return bundleAck{Line: ln, Key: strings.TrimSpace(strings.TrimPrefix(ln, "ack "))}
		}
	}
	return bundleAck{}
}

func parseReadLine(text string) bundleRead {
	r := bundleRead{Text: strings.TrimSpace(text)}
	if strings.Contains(r.Text, "after_cut=yes") &&
		(strings.HasPrefix(r.Text, "PASS ") || r.Text == "PASS" || strings.HasPrefix(r.Text, "LOST WRITE")) {
		r.OK = true
	}
	r.Missing = strings.Contains(r.Text, "value_missing=yes")
	r.From = lineField(r.Text, "read_from=")
	r.LeaderID = lineField(r.Text, "leader_id=")
	r.Key = lineField(r.Text, "key=")
	r.Reason = lineField(r.Text, "reason=")
	return r
}

// formatReplayRead is the labeled read line. status=ok means the query
// completed and was parsed. found is false when that query showed the key missing.
func formatReplayRead(r bundleRead) string {
	if !r.OK {
		return fmt.Sprintf("key=%s read_from=%s status=failed", r.Key, r.From)
	}
	found := "true"
	if r.Missing {
		found = "false"
	}
	return fmt.Sprintf("key=%s read_from=%s status=ok found=%s", r.Key, r.From, found)
}

func lineField(text, key string) string {
	i := strings.Index(text, key)
	if i < 0 {
		return ""
	}
	rest := text[i+len(key):]
	if j := strings.IndexAny(rest, " \n\t"); j >= 0 {
		rest = rest[:j]
	}
	return rest
}

func cutFromEvidence(ev map[string]any) bundleCut {
	if ev == nil {
		return bundleCut{}
	}
	c := bundleCut{
		LinksCut:   asInt(ev["links_cut"]),
		LinksTotal: asInt(ev["links_total"]),
	}
	c.Target, _ = ev["target"].(string)
	c.Direction, _ = ev["direction"].(string)
	c.TriggerFired, _ = ev["trigger_fired"].(bool)
	c.CutTime, _ = ev["cut_time"].(string)
	return c
}

func collectReplayFacts(result *adapterTrialResult, binaryPath string) replayFacts {
	ack := bundleAck{}
	if result != nil && result.dir != "" {
		b, _ := os.ReadFile(filepath.Join(result.dir, "runs.txt"))
		ack = readAckLine(string(b))
	}
	var cut bundleCut
	var read bundleRead
	var nemesis []NemesisEvent
	var hist []history.OpRecord
	var porcupineHTML []byte
	verdict := ""
	hasNemesis := false
	hasHistory := false
	if result != nil {
		verdict = result.verdict
		cut = cutFromEvidence(result.isolate)
		if result.check != nil {
			if s, ok := result.check["out"].(string); ok {
				read = parseReadLine(s)
			}
		}
		if cut.LeaderID == "" {
			cut.LeaderID = read.LeaderID
		}
		// cut_target in the check line is the member the scenario observed as leader.
		cut.LeaderNode = lineField(read.Text, "cut_target=")
		nemesis = result.nemesis
		hasNemesis = result.nemesisRan
		hist = result.history
		hasHistory = result.hasHistory
		porcupineHTML = result.porcupineHTML
	}
	sum, _ := fileSHA256(binaryPath)
	return replayFacts{
		Verdict:       verdict,
		Ack:           ack,
		Cut:           cut,
		Read:          read,
		Version:       probeVersion(binaryPath),
		Checksum:      sum,
		Nemesis:       nemesis,
		HasNemesis:    hasNemesis,
		History:       hist,
		HasHistory:    hasHistory,
		PorcupineHTML: porcupineHTML,
	}
}

// missingReplayAck is why a PASS or LOST WRITE bundle cannot be saved.
// The live verdict stays whatever the check printed. The save step is what
// fails, naming the key the read already reported.
func missingReplayAck(facts replayFacts) error {
	if facts.Verdict != "PASS" && facts.Verdict != "LOST WRITE" {
		return nil
	}
	if facts.Ack.Key != "" || facts.Read.Key == "" {
		return nil
	}
	return fmt.Errorf("cannot save replay bundle: missing acknowledgement for key %q", facts.Read.Key)
}

func writeReplayBundle(dir, scenarioPath, binaryPath, targetName string, seed int64, facts replayFacts) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	src, err := os.ReadFile(scenarioPath)
	if err != nil {
		return fmt.Errorf("save bundle: scenario: %w", err)
	}
	scenarioCopy := filepath.Join(dir, "scenario.yaml")
	if err := os.WriteFile(scenarioCopy, src, 0o644); err != nil {
		return err
	}
	sum, err := fileSHA256(scenarioCopy)
	if err != nil {
		return err
	}
	b := replayBundle{
		Format:       1,
		FussRevision: fussRevision(),
		Target: bundleTarget{
			Name:     targetName,
			Binary:   filepath.Base(binaryPath),
			Version:  facts.Version,
			Checksum: facts.Checksum,
		},
		Seed:           seed,
		Scenario:       scenarioPath,
		ScenarioSHA256: sum,
		Inputs: bundleInputs{
			Seed:     seed,
			N:        1,
			Scenario: "scenario.yaml",
		},
		Ack:     facts.Ack,
		Cut:     facts.Cut,
		Read:    facts.Read,
		Verdict: facts.Verdict,
	}
	body, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	if err := os.WriteFile(filepath.Join(dir, "bundle.json"), body, 0o644); err != nil {
		return err
	}
	if facts.HasNemesis {
		if err := writeNemesisEvents(dir, facts.Nemesis); err != nil {
			return err
		}
	}
	if facts.HasHistory {
		if err := writeHistoryFiles(dir, facts.History, facts.Nemesis); err != nil {
			return err
		}
	}
	if len(facts.PorcupineHTML) > 0 {
		if err := os.WriteFile(filepath.Join(dir, "porcupine.html"), facts.PorcupineHTML, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// copyTrialLog adds the trial's combined stdout/stderr to a bundle directory.
func copyTrialLog(bundleDir, trialDir string) error {
	src := filepath.Join(trialDir, "target.log")
	in, err := os.Open(src)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer in.Close()
	out, err := os.Create(filepath.Join(bundleDir, "target.log"))
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

func writeHistoryFiles(dir string, ops []history.OpRecord, nemesis []NemesisEvent) error {
	body, err := history.WriteJSONL(ops)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "history.jsonl"), body, 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "history.edn"), history.WriteEDN(ops, nemesisRecords(nemesis)), 0o644)
}

func writeEDNFile(path string, facts replayFacts) error {
	return os.WriteFile(path, history.WriteEDN(facts.History, nemesisRecords(facts.Nemesis)), 0o644)
}

func nemesisRecords(events []NemesisEvent) []history.NemesisRecord {
	out := make([]history.NemesisRecord, len(events))
	for i, e := range events {
		out[i] = history.NemesisRecord{
			TimestampNS: e.TimestampNS,
			Action:      e.Action,
			Targets:     e.Targets,
			Mode:        e.Mode,
			Direction:   e.Direction,
			Flow:        e.Flow,
			Details:     e.Details,
		}
	}
	return out
}

// writeNemesisEvents writes the trial's nemesis log. Timestamps are
// nanoseconds since the schedule started.
func writeNemesisEvents(dir string, events []NemesisEvent) error {
	if events == nil {
		events = []NemesisEvent{}
	}
	body, err := json.MarshalIndent(events, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	return os.WriteFile(filepath.Join(dir, "nemesis_events.json"), body, 0o644)
}

func loadReplayBundle(dir string) (*replayBundle, error) {
	b, err := os.ReadFile(filepath.Join(dir, "bundle.json"))
	if err != nil {
		return nil, fmt.Errorf("replay bundle: %w", err)
	}
	var got replayBundle
	if err := json.Unmarshal(b, &got); err != nil {
		return nil, fmt.Errorf("replay bundle: %w", err)
	}
	if got.Format != 1 {
		return nil, fmt.Errorf("replay bundle: format %d is not supported", got.Format)
	}
	if _, err := os.Stat(filepath.Join(dir, "scenario.yaml")); err != nil {
		return nil, fmt.Errorf("replay bundle: scenario.yaml: %w", err)
	}
	return &got, nil
}

// replayMatches reports why a new run does not show the saved failure or pass.
// A lost-write bundle matches only when the new run recorded an acknowledgement,
// an applied partition, and a successful read that found the value missing.
// pinNode requires the same node index. Otherwise the scenario isolates the
// elected leader, and a different index still matches when that index was the leader.
func replayMatches(want *replayBundle, got replayFacts, pinNode bool) error {
	var problems []string
	if got.Verdict != want.Verdict {
		problems = append(problems, fmt.Sprintf("verdict %s, bundle %s", got.Verdict, want.Verdict))
	}
	if got.Ack.Line == "" {
		problems = append(problems, "no write acknowledgement")
	} else if want.Ack.Line != "" && got.Ack.Line != want.Ack.Line {
		problems = append(problems, fmt.Sprintf("ack %q, bundle %q", got.Ack.Line, want.Ack.Line))
	}
	if !got.Cut.TriggerFired || got.Cut.LinksCut < 1 {
		problems = append(problems, "partition was not applied")
	} else if pinNode {
		if want.Cut.Target != "" && got.Cut.Target != want.Cut.Target {
			problems = append(problems, fmt.Sprintf("cut target %s, bundle %s", got.Cut.Target, want.Cut.Target))
		}
	} else if err := electedLeaderCut(got); err != nil {
		problems = append(problems, err.Error())
	}
	switch want.Verdict {
	case "LOST WRITE", "PASS":
		if !got.Read.OK {
			problems = append(problems, "read was not a successful read after the cut")
		}
		if got.Read.From == "" || !strings.HasPrefix(got.Read.From, "node_") || got.Read.From == got.Cut.Target {
			problems = append(problems, "read was not from a surviving leader")
		}
		if got.Ack.Key == "" || got.Read.Key == "" || got.Ack.Key != got.Read.Key {
			problems = append(problems, fmt.Sprintf("ack key %q, read key %q", got.Ack.Key, got.Read.Key))
		}
	}
	if want.Verdict == "LOST WRITE" && !got.Read.Missing {
		problems = append(problems, "read did not show the value missing")
	}
	if want.Verdict == "PASS" && got.Read.Missing {
		problems = append(problems, "read showed the value missing")
	}
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("%s", strings.Join(problems, "; "))
}

// electedLeaderCut checks that this run applied a two-way cut to the member
// it observed as leader before the cut.
func electedLeaderCut(got replayFacts) error {
	if got.Cut.Direction != "both" {
		return fmt.Errorf("cut direction %s, want both", got.Cut.Direction)
	}
	if got.Cut.LeaderNode == "" {
		return fmt.Errorf("leader before the cut was not recorded")
	}
	if got.Cut.Target != got.Cut.LeaderNode {
		return fmt.Errorf("cut %s, leader before the cut was %s", got.Cut.Target, got.Cut.LeaderNode)
	}
	return nil
}

func formatCutLine(saved string, cut bundleCut, pinNode bool) string {
	leaderID := cut.LeaderID
	if pinNode {
		line := fmt.Sprintf("cut: target=%s direction=%s links_cut=%d trigger_fired=%t",
			cut.Target, cut.Direction, cut.LinksCut, cut.TriggerFired)
		if leaderID != "" {
			line += " leader_id=" + leaderID
		}
		return line
	}
	line := fmt.Sprintf("cut: saved=%s replay=%s direction=%s links_cut=%d trigger_fired=%t",
		saved, cut.Target, cut.Direction, cut.LinksCut, cut.TriggerFired)
	if leaderID != "" {
		line += " leader_id=" + leaderID
	}
	return line
}

func printReplayReport(want *replayBundle, got *replayFacts, pinNode bool) {
	fmt.Printf("replay: seed=%d fuss_revision=%s target_version=%s target_checksum=%s\n",
		want.Seed, want.FussRevision, want.Target.Version, want.Target.Checksum)
	if got == nil {
		fmt.Println("ack:")
		fmt.Println("cut:")
		fmt.Println("read:")
		fmt.Println("verdict:")
		return
	}
	if got.Checksum != "" && got.Checksum != want.Target.Checksum {
		fmt.Printf("replay: this_target_checksum=%s\n", got.Checksum)
	}
	if got.Version != "" && got.Version != want.Target.Version {
		fmt.Printf("replay: this_target_version=%s\n", got.Version)
	}
	fmt.Printf("ack: key=%s\n", got.Ack.Key)
	cut := got.Cut
	if cut.LeaderID == "" {
		cut.LeaderID = got.Read.LeaderID
	}
	fmt.Println(formatCutLine(want.Cut.Target, cut, pinNode))
	fmt.Printf("read: %s\n", formatReplayRead(got.Read))
	if got.Verdict == "UNKNOWN" && got.Read.Reason != "" {
		fmt.Printf("verdict: UNKNOWN reason=%s\n", got.Read.Reason)
		return
	}
	fmt.Printf("verdict: %s\n", got.Verdict)
}

// openReplayBundle loads a directory written by -save-bundle or
// -save-failed-bundle and checks scenario.yaml against the saved checksum.
func openReplayBundle(dir string) (*replayBundle, error) {
	b, err := loadReplayBundle(dir)
	if err != nil {
		return nil, err
	}
	scenario := filepath.Join(dir, "scenario.yaml")
	sum, err := fileSHA256(scenario)
	if err != nil {
		return nil, err
	}
	if sum != b.ScenarioSHA256 {
		return nil, fmt.Errorf("scenario.yaml checksum %s, bundle says %s", sum, b.ScenarioSHA256)
	}
	return b, nil
}

func replaySavedBundle(dir string, json bool) int {
	b, err := openReplayBundle(dir)
	if err != nil {
		fatal(err)
	}
	scenario := filepath.Join(dir, "scenario.yaml")
	adapter, err := LoadYAMLTarget(scenario)
	if err != nil {
		fatal(err)
	}
	jsonOut = json
	bundleRun.want = b
	bundleRun.got = nil
	bundleRun.pinNode = adapter.isolatePinsNode()
	return runAdapter(adapter, "", b.Seed, 1, true, "")
}
