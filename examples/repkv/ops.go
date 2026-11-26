package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// A client event is one side of an operation: phase start is written
// immediately before the request, phase end when the client has a response
// or an error. id pairs the two lines. observe is what the client saw
// (ok, timeout, or error). It is not a claim that the write took effect.
type clientEvent struct {
	Kind    string `json:"kind"`
	ID      string `json:"id"`
	Phase   string `json:"phase"`
	Op      string `json:"op"`
	Key     string `json:"key,omitempty"`
	Value   string `json:"value,omitempty"`
	From    string `json:"from,omitempty"`
	Observe string `json:"observe,omitempty"`
	Missing bool   `json:"missing,omitempty"`
	Error   string `json:"error,omitempty"`
	At      string `json:"at"`
}

// A cut event is Fuss's isolate evidence, copied from isolate_applied.json.
type cutEvent struct {
	Kind         string `json:"kind"`
	Target       string `json:"target"`
	Direction    string `json:"direction,omitempty"`
	LinksCut     int    `json:"links_cut"`
	LinksTotal   int    `json:"links_total,omitempty"`
	TriggerFired bool   `json:"trigger_fired"`
	At           string `json:"at"`
}

type recorder struct {
	path string
}

func openRecorder() *recorder {
	return &recorder{path: os.Getenv("REPKV_CLIENT_LOG")}
}

func (r *recorder) start(id, op, key, value, from string) {
	if r == nil || r.path == "" {
		return
	}
	r.append(clientEvent{
		Kind:  "client",
		ID:    id,
		Phase: "start",
		Op:    op,
		Key:   key,
		Value: value,
		From:  from,
		At:    time.Now().UTC().Format(time.RFC3339Nano),
	})
}

func (r *recorder) end(id, op, observe, value, errText string, missing bool) {
	if r == nil || r.path == "" {
		return
	}
	r.append(clientEvent{
		Kind:    "client",
		ID:      id,
		Phase:   "end",
		Op:      op,
		Observe: observe,
		Value:   value,
		Missing: missing,
		Error:   errText,
		At:      time.Now().UTC().Format(time.RFC3339Nano),
	})
}

func (r *recorder) append(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	f, err := os.OpenFile(r.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	_, _ = f.Write(append(b, '\n'))
	_ = f.Close()
}

// observeHTTP classifies what the client saw. A timeout is a timeout even
// when the server may already have applied the write. A definitive HTTP
// answer, including 404, is ok. Anything else is error.
func observeHTTP(err error, status int) string {
	if err != nil {
		if isTimeout(err) {
			return "timeout"
		}
		return "error"
	}
	if status == http.StatusOK || status == http.StatusNotFound {
		return "ok"
	}
	return "error"
}

func isTimeout(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || os.IsTimeout(err) {
		return true
	}
	var t interface{ Timeout() bool }
	return errors.As(err, &t) && t.Timeout()
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// writeSavedHistory merges the client log with Fuss's isolate file and
// writes one JSONL history. It does nothing unless REPKV_HISTORY is set.
func writeSavedHistory(dir string) {
	out := os.Getenv("REPKV_HISTORY")
	logPath := os.Getenv("REPKV_CLIENT_LOG")
	if out == "" || logPath == "" {
		return
	}
	client, err := os.ReadFile(logPath)
	if err != nil {
		client = nil
	}
	doc, _ := readIsolateApplied(dir)
	body := buildHistory(client, doc)
	_ = os.WriteFile(out, body, 0o644)
}

func buildHistory(clientJSONL []byte, doc isolateApplied) []byte {
	type timed struct {
		at  time.Time
		raw []byte
	}
	var rows []timed
	sc := bufio.NewScanner(bytes.NewReader(clientJSONL))
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var ev clientEvent
		if json.Unmarshal(line, &ev) != nil || ev.At == "" {
			continue
		}
		at, err := time.Parse(time.RFC3339Nano, ev.At)
		if err != nil {
			continue
		}
		raw := append([]byte(nil), line...)
		rows = append(rows, timed{at: at, raw: raw})
	}
	if doc.CutTime != "" {
		cut := cutEvent{
			Kind:         "cut",
			Target:       doc.Target,
			Direction:    doc.Direction,
			LinksCut:     doc.LinksCut,
			LinksTotal:   doc.LinksTotal,
			TriggerFired: doc.TriggerFired,
			At:           doc.CutTime,
		}
		if b, err := json.Marshal(cut); err == nil {
			at, err := time.Parse(time.RFC3339Nano, doc.CutTime)
			if err == nil {
				rows = append(rows, timed{at: at, raw: b})
			}
		}
	}
	// Insertion order breaks ties, so a start written before an end at the
	// same timestamp stays first.
	for i := 1; i < len(rows); i++ {
		j := i
		for j > 0 && rows[j].at.Before(rows[j-1].at) {
			rows[j], rows[j-1] = rows[j-1], rows[j]
			j--
		}
	}
	var buf bytes.Buffer
	for _, row := range rows {
		buf.Write(row.raw)
		buf.WriteByte('\n')
	}
	return buf.Bytes()
}

func renderHistory(history []byte) (string, error) {
	var lines []string
	var writeEnd, cutAt, readStart time.Time
	var haveWriteEnd, haveCut, haveReadStart bool
	sc := bufio.NewScanner(bytes.NewReader(history))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			return "", err
		}
		at, _ := m["at"].(string)
		ts, tsErr := time.Parse(time.RFC3339Nano, at)
		switch m["kind"] {
		case "cut":
			target, _ := m["target"].(string)
			lines = append(lines, "Fuss applies partition: "+target)
			if tsErr == nil {
				cutAt = ts
				haveCut = true
			}
		case "client":
			id, _ := m["id"].(string)
			phase, _ := m["phase"].(string)
			op, _ := m["op"].(string)
			switch phase {
			case "start":
				key, _ := m["key"].(string)
				if op == "write" {
					value, _ := m["value"].(string)
					lines = append(lines, fmt.Sprintf("%s starts: write %s=%s", id, key, value))
				} else {
					from, _ := m["from"].(string)
					if from == "" {
						from = "surviving leader"
					}
					lines = append(lines, fmt.Sprintf("%s starts: read %s from %s", id, key, from))
				}
				if op == "read" && tsErr == nil {
					readStart = ts
					haveReadStart = true
				}
			case "end":
				observe, _ := m["observe"].(string)
				if op == "read" && observe == "ok" {
					if missing, _ := m["missing"].(bool); missing {
						lines = append(lines, id+" completes: missing")
					} else {
						value, _ := m["value"].(string)
						lines = append(lines, fmt.Sprintf("%s completes: value=%s", id, value))
					}
				} else {
					lines = append(lines, id+" completes: "+observe)
				}
				if op == "write" && tsErr == nil {
					writeEnd = ts
					haveWriteEnd = true
				}
			}
		}
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	if haveWriteEnd && haveCut && haveReadStart && writeEnd.Before(cutAt) && readStart.After(cutAt) {
		lines = append(lines, fmt.Sprintf("evidence: write completed before the cut (%s < %s); read started after it (%s)",
			writeEnd.Format(time.RFC3339Nano), cutAt.Format(time.RFC3339Nano), readStart.Format(time.RFC3339Nano)))
	}
	return strings.Join(lines, "\n"), nil
}
