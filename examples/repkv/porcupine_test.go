package main

import (
	"os"
	"strings"
	"testing"
)

func TestPorcupineTimeoutTranslations(t *testing.T) {
	// none: the timeout means the write did not take effect.
	// required-bound: the write must take effect, and it must do so by the timeout.
	// optional-bound: the write may be omitted, but if it takes effect it does so by the timeout.
	// optional-open: the write may be omitted or take effect after the timeout.
	cases := []struct {
		file                                        string
		none, requiredBound, optionalBound, openEnd bool
	}{
		{"testdata/histories/write-then-stale-read.jsonl", false, false, false, false},
		{"testdata/histories/read-during-write.jsonl", true, true, true, true},
		{"testdata/histories/timeout-then-new.jsonl", false, true, true, true},
		{"testdata/histories/timeout-then-initial.jsonl", true, false, true, true},
		{"testdata/histories/timeout-late-write.jsonl", false, false, false, true},
		{"testdata/histories/timeout-then-revert.jsonl", false, false, false, false},
		{"testdata/histories/overlap-agree.jsonl", true, true, true, true},
		{"testdata/histories/overlap-disagree.jsonl", false, false, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			b, err := os.ReadFile(tc.file)
			if err != nil {
				t.Fatal(err)
			}
			legal, _, err := checkHistory(b, "0")
			if err != nil {
				t.Fatal(err)
			}
			if legal != tc.openEnd {
				t.Fatalf("reference checker legal=%v, optional-open %v", legal, tc.openEnd)
			}
			ops, err := parseHistory(b)
			if err != nil {
				t.Fatal(err)
			}
			for _, mode := range []struct {
				name string
				want bool
			}{
				{effectNone, tc.none},
				{effectRequiredBound, tc.requiredBound},
				{effectOptionalBound, tc.optionalBound},
				{effectOptionalOpen, tc.openEnd},
			} {
				got := porcupineLegal(ops, mode.name, "0")
				if got != mode.want {
					t.Errorf("%s: porcupine %v, want %v", mode.name, got, mode.want)
				}
			}
		})
	}
}

func TestHistoryLineUsesTheOpenTranslation(t *testing.T) {
	revert, err := os.ReadFile("testdata/histories/timeout-then-revert.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	line, err := historyLine(revert, "0")
	if err != nil {
		t.Fatal(err)
	}
	if line != "history illegal" {
		t.Fatalf("revert: %s", line)
	}
	late, err := os.ReadFile("testdata/histories/timeout-late-write.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	line, err = historyLine(late, "0")
	if err != nil {
		t.Fatal(err)
	}
	if line != "history legal" {
		t.Fatalf("late write: %s", line)
	}
	text := formatHistoryCheck(late, "0")
	if !strings.HasPrefix(text, "history legal\ntranslation effect=optional bound=open\n") {
		t.Fatalf("format:\n%s", text)
	}
}
