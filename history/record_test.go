package history

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestJSONLRoundTrip(t *testing.T) {
	ops := []OpRecord{
		{Process: 1, Op: OpPut, Key: "k1", Value: "v42", CallNS: 1020300, ReturnNS: 1085400, Status: StatusOK},
		{Process: 2, Op: OpGet, Key: "k1", Value: "v42", CallNS: 1050000, ReturnNS: 1090000, Status: StatusOK},
		{Process: 3, Op: OpCas, Key: "k1", Value: "v99", OldValue: "v42", CallNS: 1100000, ReturnNS: 1200000, Status: StatusFail},
		{Process: 3, Op: OpPut, Key: "k1", Value: "v99", CallNS: 1060000, ReturnNS: 6060000, Status: StatusInfo},
	}
	body, err := WriteJSONL(ops)
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSpace(body), []byte("\n"))
	if len(lines) != len(ops) {
		t.Fatalf("lines %d", len(lines))
	}
	var put map[string]any
	if err := json.Unmarshal(lines[0], &put); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"process", "op", "key", "value", "call_ns", "return_ns", "status"} {
		if _, ok := put[k]; !ok {
			t.Fatalf("missing %s in %s", k, lines[0])
		}
	}
	if _, ok := put["old_value"]; ok {
		t.Fatalf("empty old_value was serialized: %s", lines[0])
	}
	if !bytes.Contains(lines[2], []byte(`"old_value"`)) {
		t.Fatalf("cas lost old_value: %s", lines[2])
	}
	got, err := ReadJSONL(bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(ops) {
		t.Fatalf("got %d", len(got))
	}
	for i := range ops {
		if got[i] != ops[i] {
			t.Fatalf("op %d: %+v", i, got[i])
		}
	}
}

func TestLogClockUsesOrigin(t *testing.T) {
	origin := time.Now()
	log := NewLog(origin)
	time.Sleep(2 * time.Millisecond)
	ns := log.Now()
	if ns < int64(time.Millisecond) || ns > int64(time.Second) {
		t.Fatalf("Now %d is not a short offset from scheduleStart", ns)
	}
	log.Append(OpRecord{Process: 1, Op: OpPut, Key: "k", Value: "v", CallNS: ns, ReturnNS: ns, Status: StatusOK})
	if got := log.Snapshot(); len(got) != 1 || got[0].CallNS != ns {
		t.Fatalf("%+v", got)
	}
}

func TestClassifyHTTP(t *testing.T) {
	cases := []struct {
		op         string
		code       int
		want       string
		wantAbsent bool
	}{
		{OpPut, 200, StatusOK, false},
		{OpPut, 201, StatusOK, false},
		{OpPut, 500, StatusFail, false},
		{OpPut, 409, StatusFail, false},
		{OpGet, 200, StatusOK, false},
		{OpGet, 404, StatusOK, true},
		{OpGet, 503, StatusFail, false},
		{OpCas, 400, StatusFail, false},
		{OpPut, 0, StatusInfo, false},
	}
	for _, tc := range cases {
		got, absent := ClassifyHTTP(tc.op, tc.code)
		if got != tc.want || absent != tc.wantAbsent {
			t.Fatalf("%s %d: got %s absent=%v", tc.op, tc.code, got, absent)
		}
	}
	if strings.Contains(StatusInfo, " ") {
		t.Fatal("status constants are tokens")
	}
}
