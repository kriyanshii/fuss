package history

import (
	"strings"
	"testing"
)

func TestWriteEDN(t *testing.T) {
	ops := []OpRecord{
		{Process: 1, Op: OpPut, Key: "k1", Value: "v42", CallNS: 100, ReturnNS: 200, Status: StatusOK},
		{Process: 2, Op: OpGet, Key: "k1", Value: "", CallNS: 300, ReturnNS: 400, Status: StatusFail},
		{Process: 3, Op: OpPut, Key: "k1", Value: `say "hi"`, CallNS: 500, ReturnNS: 600, Status: StatusInfo},
		{Process: 4, Op: OpCas, Key: "k1", OldValue: "v42", Value: "v99", CallNS: 700, ReturnNS: 800, Status: StatusOK},
		{Process: 5, Op: OpGet, Key: "k1", Value: "", CallNS: 900, ReturnNS: 1000, Status: StatusOK},
	}
	nemesis := []NemesisRecord{{
		TimestampNS: 150,
		Action:      "isolate_node",
		Targets:     []string{"node_0"},
		Mode:        "reset",
		Direction:   "both",
		Flow:        "a_to_b",
		Details:     "recovery",
	}}
	edn := string(WriteEDN(ops, nemesis))
	for _, want := range []string{":invoke", ":ok", ":fail", ":info", ":f :nemesis", ":process :nemesis", ":action \"isolate_node\""} {
		if !strings.Contains(edn, want) {
			t.Fatalf("missing %s\n%s", want, edn)
		}
	}
	if !strings.Contains(edn, `["k1" nil]`) {
		t.Fatalf("absent get not encoded:\n%s", edn)
	}
	if !strings.Contains(edn, `"say \"hi\""`) {
		t.Fatalf("quote not escaped:\n%s", edn)
	}
	invoke := strings.Index(edn, ":type :invoke")
	nem := strings.Index(edn, ":f :nemesis")
	ok := strings.Index(edn, ":type :ok")
	if invoke < 0 || nem < 0 || ok < 0 || !(invoke < nem && nem < ok) {
		t.Fatalf("order invoke=%d nemesis=%d ok=%d\n%s", invoke, nem, ok, edn)
	}
	if strings.Count(edn, ":type :invoke") != len(ops) {
		t.Fatalf("invoke count\n%s", edn)
	}
}
