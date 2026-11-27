package history

import (
	"fmt"
	"sort"
	"strings"
)

// NemesisRecord is one schedule event, already stamped in scheduleStart
// nanoseconds. It is the EDN view of nemesis_events.json.
type NemesisRecord struct {
	TimestampNS int64
	Action      string
	Targets     []string
	Mode        string
	Direction   string
	Flow        string
	Details     string
}

type ednEvent struct {
	time int64
	rank int
	text string
}

// WriteEDN converts a history and a nemesis log into a Jepsen history.
// Each operation becomes an :invoke at call_ns and an :ok, :fail, or :info
// at return_ns. Nemesis actions are :info events on process :nemesis.
func WriteEDN(ops []OpRecord, nemesis []NemesisRecord) []byte {
	events := make([]ednEvent, 0, len(ops)*2+len(nemesis))
	for _, op := range ops {
		events = append(events,
			ednEvent{time: op.CallNS, rank: 1, text: ednOp(op, true)},
			ednEvent{time: op.ReturnNS, rank: 2, text: ednOp(op, false)},
		)
	}
	for _, n := range nemesis {
		events = append(events, ednEvent{time: n.TimestampNS, rank: 0, text: ednNemesis(n)})
	}
	sort.SliceStable(events, func(i, j int) bool {
		if events[i].time != events[j].time {
			return events[i].time < events[j].time
		}
		return events[i].rank < events[j].rank
	})
	var b strings.Builder
	b.WriteString("[\n")
	for i, e := range events {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(e.text)
	}
	if len(events) > 0 {
		b.WriteByte('\n')
	}
	b.WriteString("]\n")
	return []byte(b.String())
}

func ednOp(op OpRecord, invoke bool) string {
	return fmt.Sprintf("{:type %s, :f %s, :value %s, :process %d, :time %d}",
		ednType(op.Status, invoke), ednFn(op.Op), ednOpValue(op, invoke), op.Process, ednWhen(op, invoke))
}

func ednWhen(op OpRecord, invoke bool) int64 {
	if invoke {
		return op.CallNS
	}
	return op.ReturnNS
}

func ednType(status string, invoke bool) string {
	if invoke {
		return ":invoke"
	}
	switch status {
	case StatusOK:
		return ":ok"
	case StatusFail:
		return ":fail"
	default:
		return ":info"
	}
}

func ednFn(op string) string {
	switch op {
	case OpPut, OpGet, OpCas:
		return ":" + op
	default:
		return ednString(op)
	}
}

func ednOpValue(op OpRecord, invoke bool) string {
	switch op.Op {
	case OpGet:
		if invoke || op.Status != StatusOK {
			return ednString(op.Key)
		}
		if op.Value == "" {
			return "[" + ednString(op.Key) + " nil]"
		}
		return "[" + ednString(op.Key) + " " + ednString(op.Value) + "]"
	case OpCas:
		return "[" + ednString(op.Key) + " " + ednString(op.OldValue) + " " + ednString(op.Value) + "]"
	default:
		return "[" + ednString(op.Key) + " " + ednString(op.Value) + "]"
	}
}

func ednNemesis(n NemesisRecord) string {
	return fmt.Sprintf("{:type :info, :f :nemesis, :value %s, :process :nemesis, :time %d}",
		ednNemesisValue(n), n.TimestampNS)
}

func ednNemesisValue(n NemesisRecord) string {
	var b strings.Builder
	b.WriteString("{:action ")
	b.WriteString(ednString(n.Action))
	if len(n.Targets) > 0 {
		b.WriteString(", :targets [")
		for i, t := range n.Targets {
			if i > 0 {
				b.WriteByte(' ')
			}
			b.WriteString(ednString(t))
		}
		b.WriteByte(']')
	}
	writeKV := func(k, v string) {
		if v == "" {
			return
		}
		b.WriteString(", :")
		b.WriteString(k)
		b.WriteByte(' ')
		b.WriteString(ednString(v))
	}
	writeKV("mode", n.Mode)
	writeKV("direction", n.Direction)
	writeKV("flow", n.Flow)
	writeKV("details", n.Details)
	b.WriteByte('}')
	return b.String()
}

func ednString(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '\\', '"':
			b.WriteByte('\\')
			b.WriteByte(c)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if c < 0x20 {
				fmt.Fprintf(&b, `\u%04x`, c)
			} else {
				b.WriteByte(c)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}
