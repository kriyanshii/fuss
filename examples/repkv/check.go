package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// A history operation is one client call. A completed write (observe ok)
// must sit in the order inside its interval. A rejected write also completed:
// the server refused it, so it stays in the order and does not change the
// register. A timed-out write, or a write that ended in a transport error,
// may be left out, or placed anywhere at or after its start. Cut events
// are not operations: they are how the fixture injects the fault.
type histOp struct {
	id      string
	op      string
	value   string
	start   time.Time
	end     time.Time
	observe string
}

func (o histOp) definite() bool { return o.observe == "ok" }

// rejected is a completed refusal. The write is in the history and it did
// not take effect.
func (o histOp) rejected() bool { return o.observe == "rejected" }

// uncertain is a write whose effect the client could not see: a timeout,
// or a transport error with no HTTP response.
func (o histOp) uncertain() bool {
	return o.op == "write" && !o.definite() && !o.rejected()
}

func (o histOp) completed() bool { return o.definite() || o.rejected() }

// checkHistory reports whether the client JSONL admits a legal order for
// one register that starts at initial. The explanation is one order that
// fits the observations, not the server's commit time.
func checkHistory(data []byte, initial string) (legal bool, explanation string, err error) {
	ops, err := parseHistory(data)
	if err != nil {
		return false, "", err
	}
	fixed, optional := splitOptional(ops)
	n := len(optional)
	for mask := 0; mask < 1<<n; mask++ {
		chosen := append([]histOp{}, fixed...)
		var omitted []histOp
		for i, op := range optional {
			if mask&(1<<i) != 0 {
				chosen = append(chosen, op)
			} else {
				omitted = append(omitted, op)
			}
		}
		found := false
		var winner []histOp
		permute(chosen, func(order []histOp) bool {
			if !respectsRealtime(order) || !readsMatch(order, initial) {
				return false
			}
			winner = append([]histOp(nil), order...)
			found = true
			return true
		})
		if found {
			return true, explainOrder(winner, omitted, initial), nil
		}
	}
	return false, violationText(ops), nil
}

func splitOptional(ops []histOp) (fixed, optional []histOp) {
	for _, op := range ops {
		if op.op == "read" && !op.definite() {
			continue
		}
		if op.uncertain() {
			optional = append(optional, op)
			continue
		}
		fixed = append(fixed, op)
	}
	return fixed, optional
}

func parseHistory(data []byte) ([]histOp, error) {
	byID := map[string]*histOp{}
	var order []string
	var key string
	sc := bufio.NewScanner(bytes.NewReader(data))
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var ev clientEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			return nil, fmt.Errorf("line %d: %w", lineNo, err)
		}
		if ev.Kind != "client" {
			continue
		}
		if ev.Key != "" {
			if key == "" {
				key = ev.Key
			} else if ev.Key != key {
				return nil, fmt.Errorf("checker handles one key, saw %s and %s", key, ev.Key)
			}
		}
		op := byID[ev.ID]
		if op == nil {
			op = &histOp{id: ev.ID, op: ev.Op}
			byID[ev.ID] = op
			order = append(order, ev.ID)
		}
		if ev.Op != "" {
			op.op = ev.Op
		}
		at, err := time.Parse(time.RFC3339Nano, ev.At)
		if err != nil {
			return nil, fmt.Errorf("line %d: time: %w", lineNo, err)
		}
		switch ev.Phase {
		case "start":
			op.start = at
			if ev.Op == "write" {
				op.value = ev.Value
			}
		case "end":
			op.end = at
			op.observe = ev.Observe
			if ev.Op == "read" && ev.Observe == "ok" && !ev.Missing {
				op.value = ev.Value
			}
		default:
			return nil, fmt.Errorf("line %d: phase %q", lineNo, ev.Phase)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	var ops []histOp
	for _, id := range order {
		op := *byID[id]
		if op.start.IsZero() || op.end.IsZero() {
			return nil, fmt.Errorf("operation %s is missing a start or end", id)
		}
		if op.op != "read" && op.op != "write" {
			return nil, fmt.Errorf("operation %s has op %q", id, op.op)
		}
		ops = append(ops, op)
	}
	return ops, nil
}

func permute(ops []histOp, visit func([]histOp) bool) bool {
	cur := append([]histOp(nil), ops...)
	var rec func(int) bool
	rec = func(i int) bool {
		if i == len(cur) {
			return visit(append([]histOp(nil), cur...))
		}
		for j := i; j < len(cur); j++ {
			cur[i], cur[j] = cur[j], cur[i]
			if rec(i + 1) {
				return true
			}
			cur[i], cur[j] = cur[j], cur[i]
		}
		return false
	}
	return rec(0)
}

// respectsRealtime requires a completed operation that finishes before
// another operation starts to come first. A timed-out write has no
// completion, so its end does not force it ahead of a later call.
func respectsRealtime(order []histOp) bool {
	for i := range order {
		for j := i + 1; j < len(order); j++ {
			if mustBefore(order[j], order[i]) {
				return false
			}
		}
	}
	return true
}

func mustBefore(a, b histOp) bool {
	return a.completed() && a.end.Before(b.start)
}

func readsMatch(order []histOp, initial string) bool {
	cur := initial
	for _, op := range order {
		switch op.op {
		case "write":
			if op.rejected() {
				continue
			}
			cur = op.value
		case "read":
			if op.definite() && cur != op.value {
				return false
			}
		}
	}
	return true
}

func explainOrder(order, omitted []histOp, initial string) string {
	timeouts := 0
	for _, op := range order {
		if op.uncertain() {
			timeouts++
		}
	}
	timeouts += len(omitted)
	for i, op := range order {
		if op.op != "read" || !op.definite() {
			continue
		}
		if prev := previousWrite(order, i); prev != nil && prev.uncertain() && prev.value == op.value {
			return fmt.Sprintf("Legal ordering: the timed-out write took effect before `%s`.", op.id)
		}
	}
	if timeouts > 0 {
		for i, op := range order {
			if op.op == "read" && op.definite() && op.value == initial && previousTimeout(order, i) == nil {
				return fmt.Sprintf("Legal ordering: the timed-out write did not take effect before `%s`.", op.id)
			}
		}
	}
	for i, op := range order {
		if op.op != "read" || op.value != initial {
			continue
		}
		for _, later := range order[i+1:] {
			if later.op == "write" && later.definite() {
				return fmt.Sprintf("Legal ordering: `%s` is ordered before `%s`.", op.id, later.id)
			}
		}
	}
	if len(order) == 0 {
		return "Legal ordering: no operations."
	}
	ids := make([]string, len(order))
	for i, op := range order {
		ids[i] = op.id
	}
	return "Legal ordering: " + strings.Join(ids, " then ") + "."
}

func previousWrite(order []histOp, i int) *histOp {
	for j := i - 1; j >= 0; j-- {
		if order[j].op == "write" {
			return &order[j]
		}
	}
	return nil
}

func previousTimeout(order []histOp, i int) *histOp {
	for j := i - 1; j >= 0; j-- {
		if order[j].uncertain() {
			return &order[j]
		}
	}
	return nil
}

func violationText(ops []histOp) string {
	for _, r := range ops {
		if r.op != "read" || !r.definite() {
			continue
		}
		var prior []histOp
		for _, w := range ops {
			if w.op == "write" && w.definite() && w.end.Before(r.start) {
				prior = append(prior, w)
			}
		}
		if len(prior) == 1 && prior[0].value != r.value {
			return fmt.Sprintf("Violation: the completed write `%s` must precede `%s`, which returned `%s`.", prior[0].id, r.id, r.value)
		}
		if len(prior) == 0 {
			for _, w := range ops {
				if w.op == "write" && w.rejected() && w.end.Before(r.start) && w.value == r.value {
					return fmt.Sprintf("Violation: the rejected write `%s` did not take effect, and `%s` returned `%s`.", w.id, r.id, r.value)
				}
			}
		}
	}
	return "Violation: no single ordering agrees with every read."
}
