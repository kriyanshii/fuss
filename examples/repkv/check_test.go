package main

import (
	"os"
	"strings"
	"testing"
)

func TestHistoryControls(t *testing.T) {
	cases := []struct {
		file  string
		legal bool
		want  string
	}{
		{
			"testdata/histories/write-then-stale-read.jsonl",
			false,
			"Violation: the completed write `w1` must precede `r1`, which returned `0`.",
		},
		{
			"testdata/histories/read-during-write.jsonl",
			true,
			"Legal ordering: `r1` is ordered before `w1`.",
		},
		{
			"testdata/histories/timeout-then-new.jsonl",
			true,
			"Legal ordering: the timed-out write took effect before `r1`.",
		},
		{
			"testdata/histories/timeout-then-initial.jsonl",
			true,
			"Legal ordering: the timed-out write did not take effect before `r1`.",
		},
		{
			"testdata/histories/timeout-late-write.jsonl",
			true,
			"Legal ordering: the timed-out write took effect before `r2`.",
		},
		{
			"testdata/histories/overlap-agree.jsonl",
			true,
			"Legal ordering: w1 then w2 then r1 then r2.",
		},
		{
			"testdata/histories/overlap-disagree.jsonl",
			false,
			"Violation: no single ordering agrees with every read.",
		},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			b, err := os.ReadFile(tc.file)
			if err != nil {
				t.Fatal(err)
			}
			legal, text, err := checkHistory(b, "0")
			if err != nil {
				t.Fatal(err)
			}
			if legal != tc.legal || text != tc.want {
				t.Fatalf("legal=%v text=%q\nwant legal=%v %q", legal, text, tc.legal, tc.want)
			}
		})
	}
}

func TestInflightTraceIgnoresTheCut(t *testing.T) {
	body := strings.Join([]string{
		`{"kind":"client","id":"w1","phase":"start","op":"write","key":"1-0","value":"v","at":"2026-11-26T09:18:48.14155Z"}`,
		`{"kind":"cut","target":"node_0","direction":"both","links_cut":4,"trigger_fired":true,"at":"2026-11-26T09:18:48.15694Z"}`,
		`{"kind":"client","id":"w1","phase":"end","op":"write","observe":"timeout","error":"context deadline exceeded","at":"2026-11-26T09:18:50.143477Z"}`,
		`{"kind":"client","id":"r1","phase":"start","op":"read","key":"1-0","from":"node_1","at":"2026-11-26T09:18:50.158844Z"}`,
		`{"kind":"client","id":"r1","phase":"end","op":"read","value":"v","observe":"ok","at":"2026-11-26T09:18:50.159129Z"}`,
		"",
	}, "\n")
	legal, text, err := checkHistory([]byte(body), "0")
	if err != nil {
		t.Fatal(err)
	}
	if !legal || text != "Legal ordering: the timed-out write took effect before `r1`." {
		t.Fatalf("legal=%v text=%q", legal, text)
	}
}
