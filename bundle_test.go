package main

import (
	"os"
	"path/filepath"
	"testing"
)

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
		Read:    parseReadLine("LOST WRITE read_from=node_1 leader=self after_cut=yes value_missing=yes"),
	}
	if err := replayMatches(want, got); err != nil {
		t.Fatal(err)
	}
	got.Read = parseReadLine("UNKNOWN reason=read_error")
	if err := replayMatches(want, got); err == nil {
		t.Fatal("failed read matched a lost-write bundle")
	}
	got.Read = parseReadLine("LOST WRITE read_from=node_1")
	if err := replayMatches(want, got); err == nil {
		t.Fatal("read without after_cut matched")
	}
}

func TestParseReadLine(t *testing.T) {
	r := parseReadLine("PASS read_from=node_2 leader=self after_cut=yes\n")
	if !r.OK || r.Missing || r.From != "node_2" {
		t.Fatalf("%+v", r)
	}
	r = parseReadLine("UNKNOWN reason=read_timeout")
	if r.OK || r.Missing || r.From != "" {
		t.Fatalf("%+v", r)
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
