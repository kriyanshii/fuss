// Package history is the canonical operation log for a fuss trial.
// call_ns and return_ns are nanoseconds since the same scheduleStart the
// nemesis log uses, so a checker can place a timeout next to a partition.
package history

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"time"
)

const (
	OpPut = "put"
	OpGet = "get"
	OpCas = "cas"

	StatusOK   = "ok"
	StatusFail = "fail"
	StatusInfo = "info"
)

// OpRecord is one client operation. old_value is set for cas.
// A get with status ok and an empty value is a completed read of a missing key.
type OpRecord struct {
	Process  int    `json:"process"`
	Op       string `json:"op"`
	Key      string `json:"key"`
	Value    string `json:"value"`
	OldValue string `json:"old_value,omitempty"`
	CallNS   int64  `json:"call_ns"`
	ReturnNS int64  `json:"return_ns"`
	Status   string `json:"status"`
}

// Log appends operations against one scheduleStart.
type Log struct {
	mu     sync.Mutex
	origin time.Time
	ops    []OpRecord
}

func NewLog(origin time.Time) *Log {
	return &Log{origin: origin}
}

// Now is nanoseconds since scheduleStart. The epoch is the time passed to
// NewLog, which is the nemesis scheduleStart, not the wall clock.
func (l *Log) Now() int64 {
	if l == nil || l.origin.IsZero() {
		return 0
	}
	ns := time.Since(l.origin).Nanoseconds()
	if ns < 0 {
		return 0
	}
	return ns
}

func (l *Log) Append(op OpRecord) {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.ops = append(l.ops, op)
	l.mu.Unlock()
}

func (l *Log) Snapshot() []OpRecord {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]OpRecord, len(l.ops))
	copy(out, l.ops)
	return out
}

// ClassifyHTTP maps a completed HTTP response to a history status.
// 2xx is ok. A GET 404 is a completed read of absence (ok, absent), not a
// rejected write. Other 4xx and 5xx are fail: the server refused the call.
// Anything else is indeterminate.
func ClassifyHTTP(op string, code int) (status string, absent bool) {
	if code >= 200 && code < 300 {
		return StatusOK, false
	}
	if op == OpGet && code == http.StatusNotFound {
		return StatusOK, true
	}
	if code >= 400 && code < 600 {
		return StatusFail, false
	}
	return StatusInfo, false
}

// WriteJSONL encodes one compact JSON object per line.
func WriteJSONL(ops []OpRecord) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	for _, op := range ops {
		if err := enc.Encode(op); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), nil
}

// ReadJSONL reads a history.jsonl payload. Blank lines are skipped.
func ReadJSONL(r io.Reader) ([]OpRecord, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var out []OpRecord
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var op OpRecord
		if err := json.Unmarshal(line, &op); err != nil {
			return nil, err
		}
		out = append(out, op)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
