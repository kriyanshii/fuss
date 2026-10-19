package main

import (
	"flag"
	"fmt"
	"maps"
	"os"
	"slices"
	"time"
)

// stepTimeout is about 5x the slowest calibrated run (2.55s).
// settleTimeout is 3x procStale; repair was measured at ~4s with procStale=5s.
// Change them when the measurement changes, never to make a STUCK or ZOMBIE go away.
const (
	stepTimeout   = 13 * time.Second
	settleTimeout = 3 * procStale
)

// Exit codes.
const (
	exitPass          = 0
	exitFail          = 1
	exitSetup         = 2 // the environment, not Dagu, is broken
	exitControlBroken = 3 // Fuss cannot be trusted here; no seeds were run
)

func main() {
	seed := flag.Int64("seed", 1, "first seed; the same seed replays the same run")
	n := flag.Int("n", 1, "number of seeds to run, starting at -seed")
	doKill := flag.Bool("kill", true, "kill Dagu after the seeded delay; false runs to completion for calibration")
	killAt := flag.String("kill-at", "delay", `"delay": seeded delay from start; "end": seeded 0-90ms offset after the end line; "stratified": seeded label, then offset after its opening event; "queued" (-dag=human): when Dagu's status file shows Queued`)
	shape := flag.String("dag", dagOne, `DAG shape: "one" (step a), "two" (a then b), or "human" (kill human-task complete); "one" keeps earlier seeds replayable`)
	target := flag.String("target", "", "path to the Dagu binary to test; empty uses dagu on PATH")
	replay := flag.Int("replay", 1, "with -dag=human, run each seed this many times; a seed always picks the same kill delay")
	queuedOffset := flag.Int("queued-offset", 0, "with -kill-at=queued, the seed picks a kill 0 to this many ms after Queued is seen; 0 kills at once")
	selfCheck := flag.Bool("self-check", true, "run the zombie control first; if it is broken, no seeds are run")
	control := flag.String("control", "", `"zombie" runs only the control: a staged stuck Running that Fuss must catch`)
	measure := flag.Int("measure", 0, "if >0, run this many unkilled runs and print finalize timing; no verdicts")
	measureStatus := flag.Bool("measure-status", true, "with -measure, also poll Dagu's status file (false shows Fuss's observer effect)")
	flag.Parse()

	// Printed to stderr so a run never silently uses the wrong binary,
	// while stdout keeps only verdicts.
	bin, source, err := resolveDagu(*target)
	if err != nil {
		fatal(err)
	}
	daguBin = bin
	fmt.Fprintf(os.Stderr, "fuss: dagu = %s (from %s)\n", daguBin, source)

	// The human mode judges by queue items and the record, not by the zombie
	// rule, so it runs its own control instead of the zombie control.
	if *shape == dagHuman {
		if *killAt != killAfterDelay && *killAt != killOnQueued {
			fatal(fmt.Errorf("-dag=human takes -kill-at=delay or -kill-at=queued, not %q", *killAt))
		}
		if *queuedOffset != 0 && *killAt != killOnQueued {
			fatal(fmt.Errorf("-queued-offset needs -kill-at=queued"))
		}
		if *queuedOffset < 0 {
			fatal(fmt.Errorf("-queued-offset must not be negative"))
		}
		os.Exit(runHuman(*seed, *n, *replay, *killAt, time.Duration(*queuedOffset)*time.Millisecond))
	}
	if *replay != 1 || *queuedOffset != 0 {
		fatal(fmt.Errorf("-replay and -queued-offset need -dag=human"))
	}

	if *measure > 0 {
		runMeasure(*seed, *measure, *measureStatus)
		return
	}

	switch *control {
	case "":
	case "zombie":
		if !runZombieControl(*seed) {
			os.Exit(exitControlBroken)
		}
		return
	default:
		fatal(fmt.Errorf("unknown -control %q", *control))
	}

	// A broken control means a PASS proves nothing, and whatever broke it
	// (binary, status format, ignored settings) would break every seed too.
	if *selfCheck && !runZombieControl(*seed) {
		fmt.Println("CONTROL BROKEN: Fuss cannot be trusted here; no seeds run")
		os.Exit(exitControlBroken)
	}

	how := killAfterDelay
	switch {
	case !*doKill:
		how = noKill
	case *killAt == "end":
		how = killAfterEnd
	case *killAt == "stratified":
		how = killStratified
	case *killAt != "delay":
		fatal(fmt.Errorf("unknown -kill-at %q", *killAt))
	}
	switch {
	case *shape != dagOne && *shape != dagTwo:
		fatal(fmt.Errorf("unknown -dag %q", *shape))
	case *shape == dagTwo && (how == killAfterEnd || how == killStratified):
		// Those modes aim at the end of step a, which is not the end of a two-step run.
		fatal(fmt.Errorf("-kill-at=%s needs -dag=one", *killAt))
	case *shape == dagTwo:
		fmt.Println("note: -dag=two settles, retries and checks rule 4 after each kill")
	}
	failed := 0
	var phases []string                     // first-seen order, for stable output
	outcomes := map[string]map[string]int{} // phase -> "Run/Step" -> count
	hits := map[string]int{}                // stratified: "target → landed" -> count
	for s := *seed; s < *seed+int64(*n); s++ {
		t := runTrial(s, how, "fuss", *shape)
		st, problems := judge(t)
		var rr retryResult
		if t.shape == dagTwo {
			rr = runRetry(t)
			problems = append(problems, rr.notes...)
			if rr.skipped == nil {
				problems = append(problems, checkRetry(rr.snapshot, rr.final, readRecord(t.record), t.runID, rr.exit)...)
			}
		}
		verdict := "PASS"
		if len(problems) > 0 {
			verdict = "FAIL"
			failed++
		}
		fmt.Printf("seed=%d %s delay=%v phase=%s killed=%v status=%s/%s\n",
			s, verdict, t.delay, t.phase, t.killed, st.Run, st.Step)
		if t.target != "" {
			hits[t.target+" → "+t.phase]++
			if t.target != t.phase {
				fmt.Printf("  MISSED target=%s landed=%s\n", t.target, t.phase)
			}
		}
		if outcomes[t.phase] == nil {
			phases = append(phases, t.phase)
			outcomes[t.phase] = map[string]int{}
		}
		outcomes[t.phase][st.Run+"/"+st.Step]++
		if t.shape == dagTwo {
			printRetry(t, rr)
		}
		// The label comes from Dagu's own file, so this is reported, not judged.
		if (t.phase == "after-save" || t.phase == "after-compact" || t.phase == "after-exit") && st.Run != "Succeeded" {
			fmt.Printf("  ! SURPRISE (not a verdict): Dagu had saved Succeeded before the kill but now reports %s/%s; dir=%s\n", st.Run, st.Step, t.dir)
			verdict = "SURPRISE" // keep the dir
		}
		if verdict == "PASS" {
			os.RemoveAll(t.dir) // a replay recreates it
			continue
		}
		for _, p := range problems {
			fmt.Println("  -", p)
		}
		fmt.Printf("  took=%v exit=%d err=%v dir=%s\n", t.took.Round(time.Millisecond), t.exit, t.waitErr, t.dir)
		fmt.Printf("  record: %q\n", readRecord(t.record))
	}
	fmt.Println("phases at kill:")
	for _, p := range phases {
		total := 0
		for _, c := range outcomes[p] {
			total += c
		}
		fmt.Printf("  %-14s %2d  %v\n", p, total, outcomes[p])
	}
	if len(hits) > 0 {
		fmt.Println("target → landed:")
		for _, k := range slices.Sorted(maps.Keys(hits)) {
			fmt.Printf("  %-30s %2d\n", k, hits[k])
		}
	}
	fmt.Printf("%d/%d PASS\n", *n-failed, *n)
	if failed > 0 {
		os.Exit(exitFail)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "fuss:", err)
	os.Exit(exitSetup)
}
