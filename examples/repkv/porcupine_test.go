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

func TestRejectedWriteIsNotATimeout(t *testing.T) {
	rejectedNew := hist(
		`{"kind":"client","id":"w1","phase":"start","op":"write","key":"k","value":"1","at":"2026-11-26T00:00:00Z"}`,
		`{"kind":"client","id":"w1","phase":"end","op":"write","observe":"rejected","at":"2026-11-26T00:00:01Z"}`,
		`{"kind":"client","id":"r1","phase":"start","op":"read","key":"k","at":"2026-11-26T00:00:02Z"}`,
		`{"kind":"client","id":"r1","phase":"end","op":"read","observe":"ok","value":"1","at":"2026-11-26T00:00:03Z"}`,
	)
	rejectedOld := hist(
		`{"kind":"client","id":"w1","phase":"start","op":"write","key":"k","value":"1","at":"2026-11-26T00:00:00Z"}`,
		`{"kind":"client","id":"w1","phase":"end","op":"write","observe":"rejected","at":"2026-11-26T00:00:01Z"}`,
		`{"kind":"client","id":"r1","phase":"start","op":"read","key":"k","at":"2026-11-26T00:00:02Z"}`,
		`{"kind":"client","id":"r1","phase":"end","op":"read","observe":"ok","value":"0","at":"2026-11-26T00:00:03Z"}`,
	)
	transportNew := hist(
		`{"kind":"client","id":"w1","phase":"start","op":"write","key":"k","value":"1","at":"2026-11-26T00:00:00Z"}`,
		`{"kind":"client","id":"w1","phase":"end","op":"write","observe":"error","at":"2026-11-26T00:00:01Z"}`,
		`{"kind":"client","id":"r1","phase":"start","op":"read","key":"k","at":"2026-11-26T00:00:02Z"}`,
		`{"kind":"client","id":"r1","phase":"end","op":"read","observe":"ok","value":"1","at":"2026-11-26T00:00:03Z"}`,
	)
	for _, mode := range []string{effectNone, effectRequiredBound, effectOptionalBound, effectOptionalOpen} {
		if porcupineLegal(mustOps(t, rejectedNew), mode, "0") {
			t.Errorf("%s accepted a rejected write followed by the new value", mode)
		}
		if !porcupineLegal(mustOps(t, rejectedOld), mode, "0") {
			t.Errorf("%s rejected a refused write followed by the old value", mode)
		}
	}
	if !porcupineLegal(mustOps(t, transportNew), effectOptionalOpen, "0") {
		t.Fatal("optional-open rejected a transport error followed by the new value")
	}
	if porcupineLegal(mustOps(t, transportNew), effectNone, "0") {
		t.Fatal("effect none accepted a transport error followed by the new value")
	}
	legal, text, err := checkHistory(rejectedNew, "0")
	if err != nil {
		t.Fatal(err)
	}
	if legal || !strings.Contains(text, "the rejected write `w1` did not take effect") {
		t.Fatalf("reference checker: legal=%v text=%s", legal, text)
	}
	legal, _, err = checkHistory(rejectedOld, "0")
	if err != nil || !legal {
		t.Fatalf("reference checker old value: legal=%v err=%v", legal, err)
	}
	legal, _, err = checkHistory(transportNew, "0")
	if err != nil || !legal {
		t.Fatalf("reference checker transport: legal=%v err=%v", legal, err)
	}
}

func hist(lines ...string) []byte {
	return []byte(strings.Join(lines, "\n") + "\n")
}

func mustOps(t *testing.T, data []byte) []histOp {
	t.Helper()
	ops, err := parseHistory(data)
	if err != nil {
		t.Fatal(err)
	}
	return ops
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
