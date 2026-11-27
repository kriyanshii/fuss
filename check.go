package main

import (
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"fuss/history"
)

// judge checks a finished trial against rules 1 and 3 and returns Dagu's
// settled status with every problem found. Call it only after Dagu has exited.
func judge(t trial) (runStatus, []error) {
	var problems []error
	// Only meaningful now that Dagu is dead and reaped: nothing is left to launch the step.
	if err := waitGone(t.record, stepTimeout); err != nil {
		problems = append(problems, err)
	}
	query := func() (runStatus, error) { return queryStatus(t.home, t.runID, procStale) }
	st, settleErr := settle(query, settleTimeout)
	rec := readRecord(t.record)
	switch {
	case errors.Is(settleErr, errNoRun):
		st.Run = "none"
		if err := checkNoRun(rec); err != nil {
			problems = append(problems, err)
		}
	case settleErr != nil:
		problems = append(problems, settleErr)
	}
	if err := checkNoFalseSuccess(st, rec, t.runID); err != nil {
		problems = append(problems, err)
	}
	return st, problems
}

// settle polls query until the run stops saying Running (rule 3).
// It returns the last status seen and, if the deadline passes, a ZOMBIE error.
func settle(query func() (runStatus, error), deadline time.Duration) (runStatus, error) {
	end := time.Now().Add(deadline)
	for {
		st, err := query()
		if err != nil {
			return st, err
		}
		if st.Run != "Running" {
			return st, nil
		}
		if time.Now().After(end) {
			return st, fmt.Errorf("ZOMBIE: run still Running %v after Dagu died", deadline)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// checkNoRun decides whether Dagu having no record of the run is acceptable:
// only if the step never started. A started step with no run means Dagu lost it.
func checkNoRun(rec string) error {
	if strings.Contains(rec, "start a ") {
		return fmt.Errorf("LOST RUN: record shows the step started but Dagu has no such run")
	}
	return nil
}

// checkRetry is rule 4, plus rule 1 per step, for a retried run. snapshot is
// the settled status just before the retry; final is the settled status after.
func checkRetry(snapshot, final runStatus, rec, runID string, retryExit int) []error {
	var problems []error
	after := afterMarker(rec, runID)
	for _, name := range slices.Sorted(maps.Keys(snapshot.Steps)) {
		if snapshot.Steps[name] != "succeeded" {
			continue // re-running an unconfirmed step is what retry is for
		}
		// Start lines, not end lines: a re-run cut short still counts.
		if stepStarted(after, name, runID) {
			problems = append(problems, fmt.Errorf("RERAN CONFIRMED STEP: %s was succeeded before the retry, but the retry started it again", name))
		}
		if got := final.Steps[name]; got != "succeeded" {
			problems = append(problems, fmt.Errorf("REVERTED: %s was succeeded before the retry, now %q", name, got))
		}
	}
	for _, name := range slices.Sorted(maps.Keys(final.Steps)) {
		if final.Steps[name] == "succeeded" && !stepEnded(rec, name, runID) {
			problems = append(problems, fmt.Errorf("FALSE SUCCESS: Dagu says step %s succeeded but the record has no end line for it", name))
		}
	}
	if retryExit != 0 || final.Run != "Succeeded" {
		problems = append(problems, fmt.Errorf("RETRY DID NOT COMPLETE: exit=%d final run=%q", retryExit, final.Run))
	}
	return problems
}

// checkNoFalseSuccess is rule 1: Dagu must not claim success for a step
// the outside record never saw finish.
func checkNoFalseSuccess(st runStatus, rec, runID string) error {
	claimed := st.Run == "Succeeded" || st.Step == "succeeded"
	if claimed && !stepEnded(rec, "a", runID) {
		return fmt.Errorf("FALSE SUCCESS: Dagu says run=%s step=%s but record has no end line", st.Run, st.Step)
	}
	return nil
}

// coreSpec is one trial's history, the quiet window, and which checkers are on.
// htmlPath is the -save-bundle directory; an empty path skips the file write.
type coreSpec struct {
	ops              []history.OpRecord
	obs              []history.Observation
	quietStart       int64
	quietEnd         int64
	porcupine        bool
	porcupineTimeout time.Duration
	set              bool
	eventually       bool
	eventuallyN      int
	htmlPath         string
}

// runCoreChecks applies porcupine, set, and eventually_available.
// An illegal history wins over a lost write, and a lost write wins over a
// liveness miss. A porcupine timeout still keeps its HTML trace.
func runCoreChecks(spec coreSpec) (CheckResult, []byte) {
	var problems []error
	var html []byte
	porcupineKind := ""
	if spec.porcupine {
		report := history.CheckPorcupine(spec.ops, spec.porcupineTimeout)
		porcupineKind = report.Kind
		html = report.HTML
		if report.Err != nil {
			problems = append(problems, report.Err)
		}
		if report.Kind == history.PorcupineIllegal || report.Kind == history.PorcupineTimeout {
			if spec.htmlPath != "" {
				if err := report.WriteHTML(filepath.Join(spec.htmlPath, "porcupine.html")); err != nil {
					problems = append(problems, err)
				}
			}
		}
	}
	setKind, setReason := "pass", ""
	if spec.set {
		setKind, setReason = history.CheckSet(spec.ops, spec.obs)
	}
	evKind, evReason := "pass", ""
	if spec.eventually {
		evKind, evReason = history.CheckEventually(spec.ops, spec.quietStart, spec.quietEnd, spec.eventuallyN)
	}
	verdict, reason := "PASS", ""
	switch {
	case porcupineKind == history.PorcupineIllegal:
		verdict, reason = "NONLINEARIZABLE", "nonlinearizable"
		problems = append(problems, fmt.Errorf("porcupine: history is not linearizable"))
	case spec.set && setKind == "fail":
		verdict, reason = "LOST WRITE", "lost_write"
		problems = append(problems, fmt.Errorf("set: %s", setReason))
	case spec.eventually && evKind == "fail":
		verdict, reason = "UNAVAILABLE", "eventually_available"
		problems = append(problems, fmt.Errorf("eventually_available: %s", evReason))
	case porcupineKind == history.PorcupineTimeout:
		verdict, reason = "UNKNOWN", "porcupine_timeout"
		problems = append(problems, fmt.Errorf("porcupine: timed out"))
	case porcupineKind == history.PorcupineEmpty:
		verdict, reason = "UNKNOWN", "empty_history"
		problems = append(problems, fmt.Errorf("porcupine: no completed operations"))
	case spec.set && setKind == "unknown":
		verdict, reason = "UNKNOWN", "read_back"
		problems = append(problems, fmt.Errorf("set: %s", setReason))
	case spec.eventually && evKind == "unknown":
		verdict, reason = "UNKNOWN", "eventually_available"
		problems = append(problems, fmt.Errorf("eventually_available: %s", evReason))
	}
	if setKind == "fail" && verdict != "LOST WRITE" && setReason != "" {
		problems = append(problems, fmt.Errorf("set: %s", setReason))
	}
	if evKind == "fail" && verdict != "UNAVAILABLE" && evReason != "" {
		problems = append(problems, fmt.Errorf("eventually_available: %s", evReason))
	}
	evidence := 0
	for _, op := range spec.ops {
		if op.Status == history.StatusOK {
			evidence++
		}
	}
	if evidence == 0 {
		evidence = 1
	}
	return CheckResult{Verdict: verdict, Reason: reason, Problems: problems, Evidence: evidence}, html
}

// faultDuringInflight reports whether a real fault timestamp fell inside a
// client operation. The closed interval is call_ns <= timestamp_ns <= return_ns.
// heal_all is excluded: the recovery heal always runs while workers are still
// in flight, so counting it would make the assertion true on every workload trial.
func faultDuringInflight(ops []history.OpRecord, events []NemesisEvent) bool {
	for _, ev := range events {
		if !isFaultAction(ev.Action) || !nemesisActionFired(ev) {
			continue
		}
		for _, op := range ops {
			if op.ReturnNS < op.CallNS {
				continue
			}
			if op.CallNS <= ev.TimestampNS && ev.TimestampNS <= op.ReturnNS {
				return true
			}
		}
	}
	return false
}

func isFaultAction(action string) bool {
	switch action {
	case "isolate_node", "isolate_client", "partition_halves", "kill_node", "pause_node":
		return true
	default:
		return false
	}
}

// nemesisActionFired is false when the action was skipped or failed before it
// changed anything. "error=" covers target and kill failures. "no peer links"
// is an isolate that cut nothing.
func nemesisActionFired(ev NemesisEvent) bool {
	if ev.Action == "" {
		return false
	}
	if strings.Contains(ev.Details, "error=") || strings.Contains(ev.Details, "no peer links") {
		return false
	}
	return true
}

func infoOpsObserved(ops []history.OpRecord) bool {
	for _, op := range ops {
		if op.Status == history.StatusInfo {
			return true
		}
	}
	return false
}

func faultActionFired(events []NemesisEvent, action string) bool {
	for _, ev := range events {
		if ev.Action == action && nemesisActionFired(ev) {
			return true
		}
	}
	return false
}

// leaderChanged is true when the sampled node_file values are not all equal.
// "" is a sample: a missing file that later names a leader is a change, and a
// file that stays missing is not.
func leaderChanged(samples []string) bool {
	if len(samples) == 0 {
		return false
	}
	first := samples[0]
	for _, s := range samples[1:] {
		if s != first {
			return true
		}
	}
	return false
}

func verdictRank(v string) int {
	switch v {
	case "NONLINEARIZABLE":
		return 5
	case "LOST WRITE":
		return 4
	case "UNAVAILABLE":
		return 3
	case "FAIL":
		return 2
	case "UNKNOWN":
		return 1
	default:
		return 0
	}
}

func propertyNote(cr CheckResult) string {
	if len(cr.Problems) > 0 && cr.Problems[0] != nil {
		return cr.Problems[0].Error()
	}
	if cr.Reason != "" {
		return cr.Reason
	}
	return cr.Verdict
}

func (y *YAMLAdapter) finishCheck(t *BaseTrial, base CheckResult) CheckResult {
	if !y.propertiesActive() {
		return base
	}
	outcomes, prop := y.evalProperties(t)
	t.Data()["properties"] = outcomes
	if base.Verdict != "" && base.Verdict != "PASS" {
		return base
	}
	if prop.Verdict != "" && prop.Verdict != "PASS" {
		return prop
	}
	if base.Verdict == "" {
		base.Verdict = "PASS"
	}
	if base.Evidence < 1 {
		base.Evidence = 1
	}
	return base
}

func (y *YAMLAdapter) propertiesNeedReadBack() bool {
	for _, p := range y.cfg.Properties.Always {
		if p.Porcupine != nil || p.Set != nil {
			return true
		}
	}
	return false
}

func (y *YAMLAdapter) maybeReadBack(t *BaseTrial) {
	if !y.propertiesNeedReadBack() {
		return
	}
	if _, done := t.Data()["read_back_done"]; done {
		return
	}
	log, _ := t.Data()["history"].(*history.Log)
	if log == nil {
		t.Data()["read_back_done"] = true
		return
	}
	targets, _ := t.Data()["workload_targets"].([]string)
	obs := y.readBack(t, log, targets)
	t.Data()["read_back_obs"] = obs
	t.Data()["read_back_done"] = true
}

func (y *YAMLAdapter) evalProperties(t *BaseTrial) ([]PropertyOutcome, CheckResult) {
	y.maybeReadBack(t)
	var ops []history.OpRecord
	if log, ok := t.Data()["history"].(*history.Log); ok && log != nil {
		ops = log.Snapshot()
	}
	events := trialNemesisEvents(t)
	samples, _ := t.Data()["leader_samples"].(map[string][]string)
	vars := y.vars(t, "")
	var outcomes []PropertyOutcome
	worst := CheckResult{Verdict: "PASS", Evidence: 1}
	consider := func(o PropertyOutcome, cr CheckResult) {
		outcomes = append(outcomes, o)
		if o.Kind == "sometimes" || o.Hit {
			return
		}
		if verdictRank(cr.Verdict) > verdictRank(worst.Verdict) {
			if cr.Evidence < 1 {
				cr.Evidence = 1
			}
			worst = cr
		}
	}
	for _, p := range y.cfg.Properties.Always {
		hit, note, cr := y.evalDeclared(t, p, ops)
		consider(PropertyOutcome{Name: p.Name, Kind: "always", Hit: hit, Note: note}, cr)
	}
	for _, p := range y.cfg.Properties.Eventually {
		hit, note, cr := y.evalDeclared(t, p, ops)
		consider(PropertyOutcome{Name: p.Name, Kind: "eventually", Hit: hit, Note: note}, cr)
	}
	for _, p := range y.cfg.Properties.Sometimes {
		path := ""
		if p.LeaderChanged != nil {
			path = expandTemplates(p.LeaderChanged.NodeFile, vars)
		}
		hit := evalSometimes(p, ops, events, samples, path)
		note := ""
		if !hit {
			note = "not observed"
		}
		consider(PropertyOutcome{Name: p.Name, Kind: "sometimes", Hit: hit, Note: note}, CheckResult{})
	}
	return outcomes, worst
}

func trialNemesisEvents(t *BaseTrial) []NemesisEvent {
	rt, ok := t.Data()["topology"].(*topologyRuntime)
	if !ok || rt == nil {
		return nil
	}
	ev, _ := rt.nemesisEvents()
	return ev
}

func evalSometimes(p yamlProperty, ops []history.OpRecord, events []NemesisEvent, samples map[string][]string, nodeFile string) bool {
	switch {
	case p.FaultDuringInflight.enabled():
		return faultDuringInflight(ops, events)
	case p.InfoOpsObserved.enabled():
		return infoOpsObserved(ops)
	case p.FaultActionFired != nil:
		return faultActionFired(events, p.FaultActionFired.Action)
	case p.LeaderChanged != nil:
		return leaderChanged(samples[nodeFile])
	default:
		return false
	}
}

func (y *YAMLAdapter) evalDeclared(t *BaseTrial, p yamlProperty, ops []history.OpRecord) (bool, string, CheckResult) {
	if p.hasRuleCheck() {
		cr := y.evalRules(t, p.yamlCheck, false)
		if cr.Verdict == "PASS" {
			return true, "", cr
		}
		return false, propertyNote(cr), cr
	}
	if p.Porcupine == nil && p.Set == nil && p.Eventually == nil {
		cr := CheckResult{Verdict: "FAIL", Evidence: 1, Problems: []error{fmt.Errorf("%s: no checker", p.Name)}}
		return false, cr.Problems[0].Error(), cr
	}
	if _, ok := t.Data()["history"].(*history.Log); !ok {
		cr := CheckResult{
			Verdict:  "UNKNOWN",
			Reason:   "no_history",
			Evidence: 1,
			Problems: []error{fmt.Errorf("%s: workload history was not recorded", p.Name)},
		}
		return false, cr.Problems[0].Error(), cr
	}
	qs, _ := t.Data()["quiet_start_ns"].(int64)
	qe, _ := t.Data()["quiet_end_ns"].(int64)
	timeout := 5 * time.Second
	if p.Porcupine != nil && p.Porcupine.timeout > 0 {
		timeout = p.Porcupine.timeout
	}
	n := 1
	if p.Eventually != nil && p.Eventually.N > 0 {
		n = p.Eventually.N
	}
	var obs []history.Observation
	if p.Porcupine != nil || p.Set != nil {
		obs, _ = t.Data()["read_back_obs"].([]history.Observation)
	}
	cr, html := runCoreChecks(coreSpec{
		ops:              ops,
		obs:              obs,
		quietStart:       qs,
		quietEnd:         qe,
		porcupine:        p.Porcupine != nil,
		porcupineTimeout: timeout,
		set:              p.Set != nil,
		eventually:       p.Eventually != nil,
		eventuallyN:      n,
		htmlPath:         bundleRun.saveDir,
	})
	if len(html) > 0 {
		t.Data()["porcupine_html"] = html
	}
	if cr.Verdict == "PASS" {
		return true, "", cr
	}
	return false, propertyNote(cr), cr
}
