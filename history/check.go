package history

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"github.com/anishathalye/porcupine"
)

const (
	PorcupinePass    = "pass"
	PorcupineIllegal = "illegal"
	PorcupineTimeout = "timeout"
	PorcupineEmpty   = "empty"
)

// PorcupineReport is the linearizability result. HTML is set when the
// history is illegal or the checker times out. WriteHTML calls
// porcupine.VisualizePath.
type PorcupineReport struct {
	Kind      string
	HTML      []byte
	Err       error
	visualize func(path string) error
}

// WriteHTML saves the trace. A pass, or a report with nothing to draw,
// does not create a file.
func (p PorcupineReport) WriteHTML(path string) error {
	if path == "" || p.visualize == nil {
		return nil
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return p.visualize(path)
}

type kvIn struct {
	Op    string
	Key   string
	Value string
	Old   string
}

type kvOut struct {
	Value   string
	Absent  bool
	Unknown bool
}

// reg is one key. The zero value is absence. It is comparable so the
// porcupine model can use ==.
type reg struct {
	Exists bool
	Value  string
}

// CheckPorcupine runs the KV model (put, get, cas).
// status fail is omitted: the server rejected the call before it could take
// effect. status info keeps its call time and has its return extended to the
// end of the history, because a client timeout is not the moment the write
// landed or was abandoned.
func CheckPorcupine(ops []OpRecord, timeout time.Duration) PorcupineReport {
	pop := porcupineOps(ops)
	if len(pop) == 0 {
		return PorcupineReport{Kind: PorcupineEmpty}
	}
	model := kvModel()
	res, info := porcupine.CheckOperationsVerbose(model, pop, timeout)
	report := PorcupineReport{}
	switch res {
	case porcupine.Ok:
		return PorcupineReport{Kind: PorcupinePass}
	case porcupine.Illegal:
		report.Kind = PorcupineIllegal
	default:
		report.Kind = PorcupineTimeout
	}
	var buf bytes.Buffer
	if err := porcupine.Visualize(model, info, &buf); err != nil {
		report.Err = err
	} else {
		report.HTML = buf.Bytes()
	}
	report.visualize = func(path string) error {
		return porcupine.VisualizePath(model, info, path)
	}
	return report
}

func porcupineOps(ops []OpRecord) []porcupine.Operation {
	var end int64
	for _, op := range ops {
		if op.CallNS > end {
			end = op.CallNS
		}
		if op.ReturnNS > end {
			end = op.ReturnNS
		}
	}
	out := make([]porcupine.Operation, 0, len(ops))
	for _, op := range ops {
		if op.Status != StatusOK && op.Status != StatusInfo {
			continue
		}
		ret := op.ReturnNS
		unknown := op.Status == StatusInfo
		if unknown && end > ret {
			ret = end
		}
		if ret < op.CallNS {
			ret = op.CallNS
		}
		absent := op.Op == OpGet && op.Status == StatusOK && op.Value == ""
		val := op.Value
		if absent {
			val = ""
		}
		out = append(out, porcupine.Operation{
			ClientId: op.Process,
			Input:    kvIn{Op: op.Op, Key: op.Key, Value: op.Value, Old: op.OldValue},
			Call:     op.CallNS,
			Output:   kvOut{Value: val, Absent: absent, Unknown: unknown},
			Return:   ret,
			Metadata: op.Status,
		})
	}
	return out
}

func kvModel() porcupine.Model {
	nm := porcupine.NondeterministicModel{
		Partition: partitionKV,
		Init: func() []interface{} {
			return []interface{}{reg{}}
		},
		Step:              stepKV,
		DescribeOperation: describeKV,
		DescribeState:     describeReg,
		DescribeOperationMetadata: func(info interface{}) string {
			s, _ := info.(string)
			return s
		},
	}
	return nm.ToModel()
}

func partitionKV(history []porcupine.Operation) [][]porcupine.Operation {
	groups := map[string][]porcupine.Operation{}
	var keys []string
	for _, op := range history {
		key := ""
		if in, ok := op.Input.(kvIn); ok {
			key = in.Key
		}
		if _, seen := groups[key]; !seen {
			keys = append(keys, key)
		}
		groups[key] = append(groups[key], op)
	}
	sort.Strings(keys)
	out := make([][]porcupine.Operation, 0, len(keys))
	for _, k := range keys {
		out = append(out, groups[k])
	}
	return out
}

func stepKV(state, input, output interface{}) []interface{} {
	st, ok := state.(reg)
	if !ok {
		return nil
	}
	in, ok := input.(kvIn)
	if !ok {
		return nil
	}
	out, ok := output.(kvOut)
	if !ok {
		return nil
	}
	switch in.Op {
	case OpPut:
		next := reg{Exists: true, Value: in.Value}
		if out.Unknown {
			return []interface{}{st, next}
		}
		return []interface{}{next}
	case OpGet:
		if out.Unknown {
			return []interface{}{st}
		}
		if out.Absent {
			if st.Exists {
				return nil
			}
			return []interface{}{st}
		}
		if st.Exists && st.Value == out.Value {
			return []interface{}{st}
		}
		return nil
	case OpCas:
		matched := (st.Exists && st.Value == in.Old) || (!st.Exists && in.Old == "")
		next := reg{Exists: true, Value: in.Value}
		if out.Unknown {
			if matched {
				return []interface{}{st, next}
			}
			return []interface{}{st}
		}
		if !matched {
			return nil
		}
		return []interface{}{next}
	default:
		return nil
	}
}

func describeKV(input, output interface{}) string {
	in, _ := input.(kvIn)
	out, _ := output.(kvOut)
	switch in.Op {
	case OpGet:
		if out.Unknown {
			return "get " + in.Key + " ?"
		}
		if out.Absent {
			return "get " + in.Key + " -> <absent>"
		}
		return "get " + in.Key + " -> " + out.Value
	case OpCas:
		if out.Unknown {
			return fmt.Sprintf("cas %s %s -> %s ?", in.Key, in.Old, in.Value)
		}
		return fmt.Sprintf("cas %s %s -> %s", in.Key, in.Old, in.Value)
	default:
		if out.Unknown {
			return fmt.Sprintf("put %s %s ?", in.Key, in.Value)
		}
		return fmt.Sprintf("put %s %s", in.Key, in.Value)
	}
}

func describeReg(state interface{}) string {
	st, ok := state.(reg)
	if !ok {
		return fmt.Sprint(state)
	}
	if !st.Exists {
		return "<absent>"
	}
	return strconv.Quote(st.Value)
}

// Observation is one post-quiet read of a key.
type Observation struct {
	Key    string
	Value  string
	Absent bool
	Status string
}

// LatestOK is the acknowledged put or cas with the greatest return time
// for each key. An earlier ok write was superseded.
func LatestOK(ops []OpRecord) map[string]OpRecord {
	best := map[string]OpRecord{}
	idx := map[string]int{}
	for i, op := range ops {
		if op.Status != StatusOK || (op.Op != OpPut && op.Op != OpCas) {
			continue
		}
		prev, ok := best[op.Key]
		if !ok || op.ReturnNS > prev.ReturnNS || (op.ReturnNS == prev.ReturnNS && i > idx[op.Key]) {
			best[op.Key] = op
			idx[op.Key] = i
		}
	}
	return best
}

// CheckSet fails when a definitive read-back does not show the latest
// acknowledged value. A later info write may have overwritten it; an absent
// read cannot, because info puts do not delete.
func CheckSet(ops []OpRecord, obs []Observation) (kind, reason string) {
	latest := LatestOK(ops)
	if len(latest) == 0 {
		return "pass", ""
	}
	if len(obs) == 0 {
		return "unknown", "no read-back"
	}
	seen := map[string]bool{}
	for _, ob := range obs {
		lw, ok := latest[ob.Key]
		if !ok || ob.Status != StatusOK {
			continue
		}
		if ob.Absent {
			return "fail", fmt.Sprintf("key %s: acknowledged %q missing", ob.Key, lw.Value)
		}
		if ob.Value == lw.Value {
			seen[ob.Key] = true
			continue
		}
		if infoCouldOverwrite(ops, ob.Key, ob.Value, lw) {
			seen[ob.Key] = true
			continue
		}
		return "fail", fmt.Sprintf("key %s: acknowledged %q missing, read %q", ob.Key, lw.Value, ob.Value)
	}
	for key := range latest {
		if !seen[key] {
			return "unknown", fmt.Sprintf("key %s: read-back indeterminate", key)
		}
	}
	return "pass", ""
}

func infoCouldOverwrite(ops []OpRecord, key, value string, latest OpRecord) bool {
	for _, op := range ops {
		if op.Key != key || op.Status != StatusInfo {
			continue
		}
		if op.Op != OpPut && op.Op != OpCas {
			continue
		}
		if op.Value == value && op.CallNS >= latest.CallNS {
			return true
		}
	}
	return false
}

// CheckEventually requires n consecutive ok operations whose calls fall in
// [quietStart, quietEnd). n <= 0 means one. The window is the quiet period
// after heal and restart.
func CheckEventually(ops []OpRecord, quietStart, quietEnd int64, n int) (kind, reason string) {
	if n <= 0 {
		n = 1
	}
	if quietEnd < quietStart {
		return "unknown", "quiet window is inverted"
	}
	window := make([]OpRecord, 0, len(ops))
	for _, op := range ops {
		if op.CallNS >= quietStart && op.CallNS < quietEnd {
			window = append(window, op)
		}
	}
	sort.Slice(window, func(i, j int) bool {
		if window[i].CallNS != window[j].CallNS {
			return window[i].CallNS < window[j].CallNS
		}
		if window[i].ReturnNS != window[j].ReturnNS {
			return window[i].ReturnNS < window[j].ReturnNS
		}
		return window[i].Process < window[j].Process
	})
	streak, best := 0, 0
	for _, op := range window {
		if op.Status == StatusOK {
			streak++
			if streak > best {
				best = streak
			}
			continue
		}
		streak = 0
	}
	if best >= n {
		return "pass", ""
	}
	return "fail", fmt.Sprintf("quiet streak %d, want %d", best, n)
}
