// Tiny topology fixture: write a ready marker under data_dir, then sleep.
// Optional second arg "never" skips the marker so readiness times out.
package main

import (
	"os"
	"path/filepath"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		os.Exit(2)
	}
	dir := os.Args[1]
	never := len(os.Args) >= 3 && os.Args[2] == "never"
	_ = os.MkdirAll(dir, 0o755)
	if !never {
		_ = os.WriteFile(filepath.Join(dir, "ready"), []byte("READY\n"), 0o644)
	}
	time.Sleep(30 * time.Second)
}
