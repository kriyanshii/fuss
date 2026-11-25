package main

import (
	"errors"
	"net/http"
	"testing"
)

func TestClassifyRead(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		body      string
		err       error
		kind, why string
	}{
		{"present", http.StatusOK, "v", nil, "pass", ""},
		{"present padded", http.StatusOK, "v\n", nil, "pass", ""},
		{"missing", http.StatusNotFound, "404 page not found", nil, "lost", ""},
		{"wrong value", http.StatusOK, "other", nil, "unknown", "read_unexpected"},
		{"empty 200", http.StatusOK, "", nil, "unknown", "read_unexpected"},
		{"status 500", http.StatusInternalServerError, "", nil, "unknown", "read_error"},
		{"dial", 0, "", errors.New("connection refused"), "unknown", "read_error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kind, why := classifyRead(tc.status, tc.body, tc.err, "v")
			if kind != tc.kind || why != tc.why {
				t.Fatalf("classifyRead = %s %s, want %s %s", kind, why, tc.kind, tc.why)
			}
			line := verifyLine(kind, why, 1)
			switch kind {
			case "pass":
				if line != "PASS read_from=node_1 leader=self after_cut=yes" {
					t.Fatalf("line %q", line)
				}
			case "lost":
				if line != "LOST WRITE read_from=node_1 leader=self after_cut=yes value_missing=yes" {
					t.Fatalf("line %q", line)
				}
			default:
				if line != "UNKNOWN reason="+tc.why {
					t.Fatalf("line %q", line)
				}
			}
		})
	}
}

func TestAppliedOK(t *testing.T) {
	if appliedOK(isolateApplied{}, "node_0") {
		t.Fatal("empty doc is not an applied cut")
	}
	if appliedOK(isolateApplied{Target: "node_1", TriggerFired: true, LinksCut: 4}, "node_0") {
		t.Fatal("wrong target")
	}
	if !appliedOK(isolateApplied{Target: "node_0", TriggerFired: true, LinksCut: 4}, "node_0") {
		t.Fatal("applied cut")
	}
}
