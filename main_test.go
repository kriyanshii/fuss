package main

import (
	"flag"
	"os"
	"testing"
)

// TestMain resolves Dagu the way main does: some tests, like TestJudgeNoRun,
// run the real binary. Pass one with `go test ./... -args -target=/path/to/dagu`;
// without it, dagu on PATH is used. If none is found, daguBin stays empty and
// those tests skip instead of failing the unit suite.
func TestMain(m *testing.M) {
	target := flag.String("target", "", "path to the Dagu binary; empty uses dagu on PATH")
	flag.Parse()
	if bin, _, err := resolveDagu(*target); err == nil {
		daguBin = bin
	}
	os.Exit(m.Run())
}

// needDagu skips a test that runs the real binary when none was resolved.
func needDagu(t *testing.T) {
	t.Helper()
	if daguBin == "" {
		t.Skip("this test runs the real Dagu binary: pass -args -target=/path/to/dagu or put dagu on PATH")
	}
}
