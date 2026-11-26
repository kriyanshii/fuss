package main

import (
	"os"
	"testing"
	"time"

	"github.com/anishathalye/porcupine"
)

// Three ways to hand a timed-out write to Porcupine. The return timestamp is
// a closed bound on when the operation may take effect.
//
//	failed: the write did not happen. A later read of the new value is illegal.
//	bounded: the write succeeded by the timeout. A later read of the old value is illegal.
//	open: the effect is unknown and the interval runs past every other operation,
//	      so the write may land late or not at all.
const (
	translateFailed  = "failed"
	translateBounded = "bounded"
	translateOpen    = "open"
)

type regInput struct {
	write bool
	value string
}

func TestPorcupineTimeoutTranslations(t *testing.T) {
	cases := []struct {
		file                     string
		failed, bounded, openEnd bool
	}{
		{"testdata/histories/write-then-stale-read.jsonl", false, false, false},
		{"testdata/histories/read-during-write.jsonl", true, true, true},
		{"testdata/histories/timeout-then-new.jsonl", false, true, true},
		{"testdata/histories/timeout-then-initial.jsonl", true, false, true},
		{"testdata/histories/timeout-late-write.jsonl", false, false, true},
		{"testdata/histories/overlap-agree.jsonl", true, true, true},
		{"testdata/histories/overlap-disagree.jsonl", false, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			b, err := os.ReadFile(tc.file)
			if err != nil {
				t.Fatal(err)
			}
			legal, _, err := checkHistory(b, "0")
			if err != nil {
				t.Fatal(err)
			}
			if legal != tc.openEnd {
				t.Fatalf("reference checker legal=%v, open translation %v", legal, tc.openEnd)
			}
			ops, err := parseHistory(b)
			if err != nil {
				t.Fatal(err)
			}
			for _, mode := range []struct {
				name string
				want bool
			}{
				{translateFailed, tc.failed},
				{translateBounded, tc.bounded},
				{translateOpen, tc.openEnd},
			} {
				got := porcupine.CheckOperations(registerModel(mode.name == translateOpen), toPorcupine(ops, mode.name))
				if got != mode.want {
					t.Errorf("%s: porcupine %v, want %v", mode.name, got, mode.want)
				}
			}
		})
	}
}

func registerModel(unknown bool) porcupine.Model {
	step := func(state, input, output interface{}) (bool, interface{}) {
		st := state.(string)
		in := input.(regInput)
		out := output.(string)
		if !in.write {
			if out != st {
				return false, state
			}
			return true, st
		}
		switch out {
		case "ok":
			return true, in.value
		case "fail":
			return true, st
		default:
			return false, state
		}
	}
	if !unknown {
		return porcupine.Model{
			Init: func() interface{} { return "0" },
			Step: step,
		}
	}
	nm := porcupine.NondeterministicModel{
		Init: func() []interface{} { return []interface{}{"0"} },
		Step: func(state, input, output interface{}) []interface{} {
			st := state.(string)
			in := input.(regInput)
			out := output.(string)
			if !in.write {
				if out != st {
					return nil
				}
				return []interface{}{st}
			}
			switch out {
			case "ok":
				return []interface{}{in.value}
			case "fail":
				return []interface{}{st}
			case "unknown":
				return []interface{}{st, in.value}
			default:
				return nil
			}
		},
	}
	return nm.ToModel()
}

func toPorcupine(ops []histOp, mode string) []porcupine.Operation {
	var last int64
	for _, op := range ops {
		if ns := op.end.UnixNano(); ns > last {
			last = ns
		}
	}
	hist := make([]porcupine.Operation, 0, len(ops))
	for i, op := range ops {
		in := regInput{write: op.op == "write", value: op.value}
		out := op.value
		ret := op.end.UnixNano()
		if op.op == "write" {
			switch {
			case op.observe == "ok":
				out = "ok"
			case mode == translateFailed:
				out = "fail"
			case mode == translateBounded:
				out = "ok"
			default:
				out = "unknown"
				ret = last + int64(time.Second)
			}
		}
		hist = append(hist, porcupine.Operation{
			ClientId: i,
			Input:    in,
			Call:     op.start.UnixNano(),
			Output:   out,
			Return:   ret,
		})
	}
	return hist
}
