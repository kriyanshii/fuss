package main

import "testing"

// Real `dagu status` output for a two-step run killed in b and then repaired
// (paths shortened). The wrapped command lines must not confuse the parser.
const twoStepStatus = `Failed - 2026-10-18T15:26:21+05:30

dag: rec (7.0s)
├─log: /TMP/home/logs/rec/dag-run_20261018_095621Z_probe/dag-run_20261018.152621.416.probe.log
│
├─a (1.0s) [succeeded]
│ ├─sh -c 'echo "start a $DAG_RUN_ID" >>
│ │   /TMP/runs.t
│ │   xt; sleep 1; echo "end a $DAG_RUN_ID" >>
│ │   /TMP/runs.t
│ │   xt'
│ │
│ └─stderr: /TMP/home/logs/rec/dag-run_20261018_095621Z_probe/run_20261018_095621Z_6fb52d/a.20261018.152621.469.probe.err
│
└─b (6.0s) [failed]
  ├─sh -c 'echo "start b $DAG_RUN_ID" >>
  │   /TMP/runs.txt
  │   ; sleep 1; echo "end b $DAG_RUN_ID" >>
  │   /TMP/runs.txt
  │   '
  │
  └─error: process terminated unexpectedly - stale local process detected

Result: Failed
`

func TestParseStatusTwoSteps(t *testing.T) {
	st, err := parseStatus([]byte(twoStepStatus))
	if err != nil {
		t.Fatal(err)
	}
	if st.Run != "Failed" || st.Step != "succeeded" || len(st.Steps) != 2 ||
		st.Steps["a"] != "succeeded" || st.Steps["b"] != "failed" {
		t.Fatalf("got %+v", st)
	}
}

// Real output for a run killed in a: b never ran, so it has no duration.
func TestParseStatusStepNeverRan(t *testing.T) {
	st, err := parseStatus([]byte("├─a (6.0s) [failed]\n└─b [failed]\nResult: Failed\n"))
	if err != nil || st.Steps["a"] != "failed" || st.Steps["b"] != "failed" {
		t.Fatalf("got %+v err=%v, want a=failed b=failed", st, err)
	}
}

// Output without step a is rejected, not read as an empty success.
func TestParseStatusNoStepA(t *testing.T) {
	if _, err := parseStatus([]byte("└─b (1.0s) [succeeded]\n\nResult: Succeeded\n")); err == nil {
		t.Fatal("want error for output without step a")
	}
}
