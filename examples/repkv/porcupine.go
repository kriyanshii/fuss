package main

import (
	"fmt"
	"time"

	"github.com/anishathalye/porcupine"
)

// A timed-out write has two separate assumptions.
//
//	effect: none, required, or optional. none records the write as a failure.
//	  required means the write must take effect. optional means it may be omitted.
//	bound: timeout or open. timeout means that if the write takes effect, it
//	  does so by the client timeout. open means it may take effect later.
//
// effect=required and bound=timeout together reject a later read of the old
// value, because the write is required, not only because the timeout is a bound.
// effect=optional and bound=timeout still allow that read by omitting the write,
// and reject a read of the old value followed by a read of the new one, because
// the write cannot be ordered after the timeout.
const (
	effectNone          = "none"
	effectRequiredBound = "required-bound"
	effectOptionalBound = "optional-bound"
	effectOptionalOpen  = "optional-open"
)

func historyLine(data []byte, initial string) (string, error) {
	ops, err := parseHistory(data)
	if err != nil {
		return "", err
	}
	if len(ops) == 0 {
		return "history unknown reason=empty", nil
	}
	if porcupineLegal(ops, effectOptionalOpen, initial) {
		return "history legal", nil
	}
	return "history illegal", nil
}

func porcupineLegal(ops []histOp, mode, initial string) bool {
	return porcupine.CheckOperations(registerModel(initial, mode != effectNone && mode != effectRequiredBound), toPorcupine(ops, mode))
}

type regInput struct {
	write bool
	value string
}

func registerModel(initial string, optional bool) porcupine.Model {
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
	if !optional {
		return porcupine.Model{
			Init: func() interface{} { return initial },
			Step: step,
		}
	}
	nm := porcupine.NondeterministicModel{
		Init: func() []interface{} { return []interface{}{initial} },
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
		if op.op == "write" && op.observe != "ok" {
			switch mode {
			case effectNone:
				out = "fail"
			case effectRequiredBound:
				out = "ok"
			case effectOptionalBound:
				out = "unknown"
			default:
				out = "unknown"
				ret = last + int64(time.Second)
			}
		} else if op.op == "write" {
			out = "ok"
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

func formatHistoryCheck(data []byte, initial string) string {
	line, err := historyLine(data, initial)
	if err != nil {
		return fmt.Sprintf("history unknown reason=%s\n", err.Error())
	}
	return line + "\ntranslation effect=optional bound=open\n" + string(data)
}
