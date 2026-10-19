package main

import (
	"encoding/json"
	"fmt"
	"time"
)

// jsonOut switches stdout to one JSON object per line (-json): one per trial,
// control and summary. Human output is the default; it is printed by say and
// sayln, which are exactly fmt.Printf and fmt.Println when jsonOut is off.
var jsonOut bool

func say(format string, a ...any) {
	if !jsonOut {
		fmt.Printf(format, a...)
	}
}

func sayln(a ...any) {
	if !jsonOut {
		fmt.Println(a...)
	}
}

// emit prints rec as one JSON line with -json, and nothing without it.
// Keys come out sorted, so the same record always prints the same line.
func emit(rec map[string]any) {
	if !jsonOut {
		return
	}
	b, err := json.Marshal(rec)
	if err != nil {
		fatal(err)
	}
	fmt.Println(string(b))
}

// ms is a duration in milliseconds, to the microsecond.
func ms(d time.Duration) float64 {
	return float64(d.Microseconds()) / 1000
}

// errStrings turns problems into strings for JSON.
func errStrings(errs []error) []string {
	out := []string{}
	for _, e := range errs {
		out = append(out, e.Error())
	}
	return out
}
