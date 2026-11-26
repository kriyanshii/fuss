package main

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestObserveHTTP(t *testing.T) {
	if got := observeHTTP(nil, http.StatusOK); got != "ok" {
		t.Fatalf("200 = %s", got)
	}
	if got := observeHTTP(nil, http.StatusNotFound); got != "ok" {
		t.Fatalf("404 = %s", got)
	}
	if got := observeHTTP(nil, http.StatusServiceUnavailable); got != "error" {
		t.Fatalf("503 = %s", got)
	}
	if got := observeHTTP(&timeoutErr{}, 0); got != "timeout" {
		t.Fatalf("timeout = %s", got)
	}
	if got := observeHTTP(errors.New("connection refused"), 0); got != "error" {
		t.Fatalf("dial = %s", got)
	}
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "deadline exceeded" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

func TestBuildHistoryOrdersTheCutBetweenWriteAndRead(t *testing.T) {
	base := time.Date(2026, 11, 26, 9, 0, 0, 0, time.UTC)
	writeStart := base.Format(time.RFC3339Nano)
	writeEnd := base.Add(time.Millisecond).Format(time.RFC3339Nano)
	cut := base.Add(2 * time.Millisecond).Format(time.RFC3339Nano)
	readStart := base.Add(3 * time.Millisecond).Format(time.RFC3339Nano)
	readEnd := base.Add(4 * time.Millisecond).Format(time.RFC3339Nano)
	client := strings.Join([]string{
		`{"kind":"client","id":"w1","phase":"start","op":"write","key":"1-0","value":"v","at":"` + writeStart + `"}`,
		`{"kind":"client","id":"w1","phase":"end","op":"write","observe":"ok","at":"` + writeEnd + `"}`,
		`{"kind":"client","id":"r1","phase":"start","op":"read","key":"1-0","from":"node_1","at":"` + readStart + `"}`,
		`{"kind":"client","id":"r1","phase":"end","op":"read","observe":"ok","value":"v","at":"` + readEnd + `"}`,
		"",
	}, "\n")
	doc := isolateApplied{
		Target: "node_0", Direction: "both", LinksCut: 4, LinksTotal: 6,
		TriggerFired: true, CutTime: cut,
	}
	body := buildHistory([]byte(client), doc)
	text, err := renderHistory(body)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{
		"w1 starts: write 1-0=v",
		"w1 completes: ok",
		"Fuss applies partition: node_0",
		"r1 starts: read 1-0 from node_1",
		"r1 completes: value=v",
	}, "\n")
	if !strings.HasPrefix(text, want) {
		t.Fatalf("render =\n%s\nwant prefix\n%s", text, want)
	}
	if !strings.Contains(text, "evidence: write completed before the cut") {
		t.Fatalf("missing evidence line:\n%s", text)
	}
	if !strings.Contains(string(body), `"kind":"cut"`) || !strings.Contains(string(body), `"target":"node_0"`) {
		t.Fatalf("history missing cut: %s", body)
	}
}

func TestRenderTimeoutIsNotAValue(t *testing.T) {
	at := time.Date(2026, 11, 26, 9, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)
	body := []byte(`{"kind":"client","id":"w1","phase":"end","op":"write","observe":"timeout","at":"` + at + `"}` + "\n")
	text, err := renderHistory(body)
	if err != nil {
		t.Fatal(err)
	}
	if text != "w1 completes: timeout" {
		t.Fatalf("render = %q", text)
	}
}
