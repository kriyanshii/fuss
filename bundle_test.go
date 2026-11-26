package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMissingReplayAckKeepsTheReadKey(t *testing.T) {
	facts := replayFacts{
		Verdict: "PASS",
		Read:    parseReadLine("PASS read_from=node_2 leader=self after_cut=yes cut_target=node_0 key=1-0"),
	}
	err := missingReplayAck(facts)
	if err == nil || err.Error() != `cannot save replay bundle: missing acknowledgement for key "1-0"` {
		t.Fatalf("err = %v", err)
	}
	facts.Ack = bundleAck{Line: "ack 1-0", Key: "1-0"}
	if err := missingReplayAck(facts); err != nil {
		t.Fatal(err)
	}
	facts.Ack = bundleAck{}
	facts.Verdict = "LOST WRITE"
	facts.Read = parseReadLine("LOST WRITE read_from=node_1 leader=self after_cut=yes value_missing=yes key=1-0")
	err = missingReplayAck(facts)
	if err == nil || err.Error() != `cannot save replay bundle: missing acknowledgement for key "1-0"` {
		t.Fatalf("lost write err = %v", err)
	}
	facts.Verdict = "UNKNOWN"
	if err := missingReplayAck(facts); err != nil {
		t.Fatal(err)
	}
}

func TestReplayMatchesLostWrite(t *testing.T) {
	want := &replayBundle{
		Verdict: "LOST WRITE",
		Ack:     bundleAck{Line: "ack 1-0", Key: "1-0"},
		Cut:     bundleCut{Target: "node_0", TriggerFired: true, LinksCut: 4},
	}
	got := replayFacts{
		Verdict: "LOST WRITE",
		Ack:     bundleAck{Line: "ack 1-0", Key: "1-0"},
		Cut:     bundleCut{Target: "node_0", Direction: "both", TriggerFired: true, LinksCut: 4},
		Read:    parseReadLine("LOST WRITE read_from=node_1 leader=self after_cut=yes value_missing=yes key=1-0"),
	}
	if err := replayMatches(want, got, true); err != nil {
		t.Fatal(err)
	}
	if got.Read.Key != "1-0" {
		t.Fatalf("read key %q", got.Read.Key)
	}
	got.Read = parseReadLine("UNKNOWN reason=read_error")
	if err := replayMatches(want, got, true); err == nil {
		t.Fatal("failed read matched a lost-write bundle")
	}
	got.Read = parseReadLine("LOST WRITE read_from=node_1 key=1-0")
	if err := replayMatches(want, got, true); err == nil {
		t.Fatal("read without after_cut matched")
	}
	got.Read = parseReadLine("LOST WRITE read_from=node_1 leader=self after_cut=yes value_missing=yes key=other")
	if err := replayMatches(want, got, true); err == nil {
		t.Fatal("different read key matched")
	}
}

func TestParseReadLine(t *testing.T) {
	r := parseReadLine("PASS read_from=node_2 leader=self after_cut=yes key=1-0\n")
	if !r.OK || r.Missing || r.From != "node_2" || r.LeaderID != "" || r.Key != "1-0" {
		t.Fatalf("%+v", r)
	}
	r = parseReadLine("PASS read_from=node_0 leader=self after_cut=yes leader_id=99 cut_target=node_1 key=1-0")
	if !r.OK || r.Missing || r.From != "node_0" || r.LeaderID != "99" || r.Key != "1-0" {
		t.Fatalf("%+v", r)
	}
	if line := formatReplayRead(parseReadLine("LOST WRITE read_from=node_1 leader=self after_cut=yes value_missing=yes key=1-0")); line != "key=1-0 read_from=node_1 status=ok found=false" {
		t.Fatalf("format %q", line)
	}
	r = parseReadLine("UNKNOWN reason=read_timeout")
	if r.OK || r.Missing || r.From != "" {
		t.Fatalf("%+v", r)
	}
}

func TestReplayMatchesSurvivorOtherThanCut(t *testing.T) {
	want := &replayBundle{
		Verdict: "PASS",
		Ack:     bundleAck{Line: "ack 1-0", Key: "1-0"},
		Cut:     bundleCut{Target: "node_1", TriggerFired: true, LinksCut: 4, LeaderID: "99"},
	}
	got := replayFacts{
		Verdict: "PASS",
		Ack:     bundleAck{Line: "ack 1-0", Key: "1-0"},
		Cut:     bundleCut{Target: "node_1", Direction: "both", TriggerFired: true, LinksCut: 4, LeaderID: "99"},
		Read:    parseReadLine("PASS read_from=node_0 leader=self after_cut=yes leader_id=99 cut_target=node_1 key=1-0"),
	}
	if err := replayMatches(want, got, true); err != nil {
		t.Fatal(err)
	}
	got.Read = parseReadLine("PASS read_from=node_1 leader=self after_cut=yes leader_id=99 cut_target=node_1 key=1-0")
	got.Cut.LeaderID = "99"
	if err := replayMatches(want, got, true); err == nil {
		t.Fatal("read from the cut node matched")
	}
	got.Read = parseReadLine("PASS read_from=node_0 leader=self after_cut=yes leader_id=100 cut_target=node_1 key=1-0")
	got.Cut.LeaderID = "100"
	if err := replayMatches(want, got, true); err != nil {
		t.Fatal(err)
	}
}

func TestReplayElectedLeaderDifferentNode(t *testing.T) {
	want := &replayBundle{
		Verdict: "PASS",
		Ack:     bundleAck{Line: "ack 1-0", Key: "1-0"},
		Cut:     bundleCut{Target: "node_2", Direction: "both", TriggerFired: true, LinksCut: 4, LeaderNode: "node_2"},
	}
	got := replayFacts{
		Verdict: "PASS",
		Ack:     bundleAck{Line: "ack 1-0", Key: "1-0"},
		Cut:     bundleCut{Target: "node_0", Direction: "both", TriggerFired: true, LinksCut: 4, LeaderNode: "node_0", LeaderID: "42"},
		Read:    parseReadLine("PASS read_from=node_1 leader=self after_cut=yes leader_id=42 cut_target=node_0 key=1-0"),
	}
	if err := replayMatches(want, got, false); err != nil {
		t.Fatal(err)
	}
	line := formatCutLine(want.Cut.Target, got.Cut, false)
	if line != "cut: saved=node_2 replay=node_0 direction=both links_cut=4 trigger_fired=true leader_id=42" {
		t.Fatalf("cut line %q", line)
	}
	if err := replayMatches(want, got, true); err == nil {
		t.Fatal("pinned scenario accepted a different node")
	}
	got.Cut.LeaderNode = "node_2"
	err := replayMatches(want, got, false)
	if err == nil || !strings.Contains(err.Error(), "leader before the cut was node_2") {
		t.Fatal(err)
	}
}

func TestWriteReplayBundleRoundTrip(t *testing.T) {
	dir := t.TempDir()
	scenario := filepath.Join(dir, "scenario.yaml")
	if err := os.WriteFile(scenario, []byte("name: demo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "bin")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "bundle")
	facts := replayFacts{
		Verdict:  "LOST WRITE",
		Ack:      bundleAck{Line: "ack 1-0", Key: "1-0"},
		Cut:      bundleCut{Target: "node_0", Direction: "both", TriggerFired: true, LinksCut: 4, LinksTotal: 6},
		Read:     parseReadLine("LOST WRITE read_from=node_1 leader=self after_cut=yes value_missing=yes"),
		Version:  "repkv",
		Checksum: "sha256:abc",
	}
	if err := writeReplayBundle(out, scenario, bin, "repkv-bug", 1, facts); err != nil {
		t.Fatal(err)
	}
	got, err := loadReplayBundle(out)
	if err != nil {
		t.Fatal(err)
	}
	if got.Format != 1 || got.Seed != 1 || got.Verdict != "LOST WRITE" {
		t.Fatalf("%+v", got)
	}
	if got.Ack.Line != "ack 1-0" || !got.Read.Missing || !got.Read.OK || got.Read.From != "node_1" {
		t.Fatalf("ack/read %+v %+v", got.Ack, got.Read)
	}
	if got.Cut.LinksCut != 4 || !got.Cut.TriggerFired || got.Cut.Target != "node_0" {
		t.Fatalf("cut %+v", got.Cut)
	}
	if got.Inputs.Scenario != "scenario.yaml" || got.ScenarioSHA256 == "" {
		t.Fatalf("inputs %+v sum %s", got.Inputs, got.ScenarioSHA256)
	}
	body, err := os.ReadFile(filepath.Join(out, "scenario.yaml"))
	if err != nil || string(body) != "name: demo\n" {
		t.Fatalf("scenario copy %q %v", body, err)
	}
}
