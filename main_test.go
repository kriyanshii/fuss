package main

import (
	"encoding/json"
	"flag"
	"os"
	"testing"
)

// TestMain resolves Dagu the way main does: some tests, like TestJudgeNoRun,
// run the real binary. Pass one with `go test ./... -args -target=/path/to/dagu`;
// without it, dagu on PATH is used. If none is found, daguBin stays empty and
// those tests skip instead of failing the unit suite.
func TestMain(m *testing.M) {
	target := flag.String("target", "", "path to the Dagu binary; empty uses dagu on PATH")
	flag.Parse()
	if bin, _, err := resolveDagu(*target); err == nil {
		daguBin = bin
	}
	os.Exit(m.Run())
}

func TestTrialRecordProxyOwnerReason(t *testing.T) {
	rec := trialRecord("postgres", 1, &adapterTrialResult{
		verdict: "UNKNOWN",
		reason:  "proxy_owner_unknown",
		phase:   "before-start",
		status:  "unknown",
		exit:    -1,
		isolate: map[string]any{
			"proxy_mode": "per_node_owned",
			"unknown_owners": []map[string]any{{
				"to":     0,
				"listen": "127.0.0.1:51772",
				"remote": "127.0.0.1:51900",
				"cut":    true,
			}},
		},
		check: map[string]any{
			"out": "PASS read_from=node_1 standby_before_cut=sync after_cut=yes cut_target=node_0 key=1-0\n",
		},
	})
	if rec["reason"] != "proxy_owner_unknown" || rec["verdict"] != "UNKNOWN" {
		t.Fatalf("reason/verdict = %v %v", rec["reason"], rec["verdict"])
	}
	if _, buried := rec["evidence"].(map[string]any)["reason"]; buried {
		t.Fatalf("reason nested in evidence: %v", rec["evidence"])
	}
	ev := rec["evidence"].(map[string]any)
	iso := ev["isolate"].(map[string]any)
	unk, _ := iso["unknown_owners"].([]map[string]any)
	if len(unk) != 1 || unk[0]["remote"] != "127.0.0.1:51900" || unk[0]["listen"] != "127.0.0.1:51772" || unk[0]["cut"] != true {
		t.Fatalf("socket details = %#v", unk)
	}
	if ev["check"].(map[string]any)["out"] == "" {
		t.Fatal("scenario line dropped")
	}
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got["reason"] != "proxy_owner_unknown" {
		t.Fatalf("marshaled reason %v", got["reason"])
	}
}

// needDagu skips a test that runs the real binary when none was resolved.
func needDagu(t *testing.T) {
	t.Helper()
	if daguBin == "" {
		t.Skip("this test runs the real Dagu binary: pass -args -target=/path/to/dagu or put dagu on PATH")
	}
}
