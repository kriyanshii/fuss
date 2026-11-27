package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"math"
	"math/rand"
	"net/http"
	"net/url"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"fuss/history"
)

// workloadSettle is how long to wait after cancelling workers so an
// in-flight server-side commit can finish before read-back.
const workloadSettle = 200 * time.Millisecond

func (y *YAMLAdapter) workloadActive() bool {
	return strings.TrimSpace(y.cfg.Workload.Driver) != ""
}

func (y *YAMLAdapter) historyChecks() bool {
	c := y.cfg.Check
	return c.Porcupine != nil || c.Set != nil || c.Eventually != nil
}

func validateWorkload(path string, cfg *yamlTarget) error {
	checkers := cfg.Check.Porcupine != nil || cfg.Check.Set != nil || cfg.Check.Eventually != nil
	w := &cfg.Workload
	if checkers && strings.TrimSpace(w.Driver) == "" {
		return fmt.Errorf("target-config %s: porcupine, set, and eventually_available require workload.driver", path)
	}
	if strings.TrimSpace(w.Driver) == "" {
		return nil
	}
	switch w.Driver {
	case "http_kv", "exec_op":
	default:
		return fmt.Errorf("target-config %s: workload.driver %q not supported (http_kv or exec_op)", path, w.Driver)
	}
	if !cfg.Nemesis.active() {
		return fmt.Errorf("target-config %s: workload.driver requires nemesis", path)
	}
	if w.Workers <= 0 {
		w.Workers = 1
	}
	if w.Workers > 1024 {
		return fmt.Errorf("target-config %s: workload.workers %d is too large", path, w.Workers)
	}
	if w.Keyspace <= 0 {
		w.Keyspace = 1
	}
	if w.Rate <= 0 {
		w.Rate = 10
	}
	if strings.TrimSpace(w.Timeout) == "" {
		w.timeout = 200 * time.Millisecond
	} else {
		d, err := time.ParseDuration(w.Timeout)
		if err != nil || d <= 0 {
			return fmt.Errorf("target-config %s: workload.timeout %q", path, w.Timeout)
		}
		w.timeout = d
	}
	switch w.Driver {
	case "http_kv":
		if strings.TrimSpace(w.Targets) == "" {
			return fmt.Errorf("target-config %s: workload.targets is required", path)
		}
		if w.Put.URL == "" && w.Get.URL == "" && w.Cas.URL == "" {
			return fmt.Errorf("target-config %s: http_kv needs a put, get, or cas template", path)
		}
	case "exec_op":
		if len(w.Exec.Command) == 0 && len(w.Exec.Put) == 0 && len(w.Exec.Get) == 0 && len(w.Exec.Cas) == 0 {
			return fmt.Errorf("target-config %s: exec_op needs a command", path)
		}
	}
	needsRead := cfg.Check.Porcupine != nil || cfg.Check.Set != nil
	if needsRead && w.Driver == "http_kv" && w.Get.URL == "" {
		return fmt.Errorf("target-config %s: porcupine and set need workload.get", path)
	}
	if needsRead && w.Driver == "exec_op" && len(w.Exec.Get) == 0 && len(w.Exec.Command) == 0 {
		return fmt.Errorf("target-config %s: porcupine and set need workload.exec.get", path)
	}
	for _, call := range []yamlKVCall{w.Put, w.Get, w.Cas} {
		switch strings.TrimSpace(call.ValueEncoding) {
		case "", "base64":
		default:
			return fmt.Errorf("target-config %s: value_encoding %q (want base64)", path, call.ValueEncoding)
		}
		if call.ValueEncoding != "" && strings.TrimSpace(call.ValueFrom) == "" {
			return fmt.Errorf("target-config %s: value_encoding requires value_from", path)
		}
	}
	if p := cfg.Check.Porcupine; p != nil {
		if strings.TrimSpace(p.Timeout) == "" {
			p.timeout = 5 * time.Second
		} else {
			d, err := time.ParseDuration(p.Timeout)
			if err != nil || d < 0 {
				return fmt.Errorf("target-config %s: check.porcupine.timeout %q", path, p.Timeout)
			}
			p.timeout = d
		}
	}
	return nil
}

func (w yamlWorkload) opNames() []string {
	var ops []string
	switch w.Driver {
	case "http_kv":
		if w.Put.URL != "" {
			ops = append(ops, history.OpPut)
		}
		if w.Get.URL != "" {
			ops = append(ops, history.OpGet)
		}
		if w.Cas.URL != "" {
			ops = append(ops, history.OpCas)
		}
	case "exec_op":
		if len(w.Exec.Put) > 0 || len(w.Exec.Command) > 0 {
			ops = append(ops, history.OpPut)
		}
		if len(w.Exec.Get) > 0 || len(w.Exec.Command) > 0 {
			ops = append(ops, history.OpGet)
		}
		if len(w.Exec.Cas) > 0 {
			ops = append(ops, history.OpCas)
		}
	}
	return ops
}

func clientTrialVars(rt *topologyRuntime) map[string]string {
	if rt == nil {
		return map[string]string{}
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	n := len(rt.ClientPorts)
	if len(rt.ClientProxies) > n {
		n = len(rt.ClientProxies)
	}
	m := map[string]string{}
	addrs := make([]string, 0, n)
	for i := 0; i < n; i++ {
		addr := ""
		if i < len(rt.ClientProxies) && rt.ClientProxies[i] != "" {
			addr = rt.ClientProxies[i]
		} else if i < len(rt.ClientPorts) {
			addr = "127.0.0.1:" + strconv.Itoa(rt.ClientPorts[i])
		}
		if addr == "" {
			continue
		}
		m["client_addr_"+strconv.Itoa(i)] = addr
		addrs = append(addrs, addr)
	}
	m["client_addrs"] = strings.Join(addrs, ",")
	return m
}

func (y *YAMLAdapter) workloadVars(t *BaseTrial, rt *topologyRuntime) map[string]string {
	vars := y.vars(t, "")
	for k, v := range clientTrialVars(rt) {
		vars[k] = v
	}
	return vars
}

func expandWorkload(s string, vars map[string]string) string {
	s = expandTemplates(s, vars)
	for _, k := range []string{"target", "key", "value", "old", "op", "process", "key_b64", "value_b64", "old_b64"} {
		s = strings.ReplaceAll(s, "${"+k+"}", vars[k])
	}
	return s
}

func addKVEncodings(vars map[string]string) {
	if vars == nil {
		return
	}
	vars["key_b64"] = base64.StdEncoding.EncodeToString([]byte(vars["key"]))
	vars["value_b64"] = base64.StdEncoding.EncodeToString([]byte(vars["value"]))
	vars["old_b64"] = base64.StdEncoding.EncodeToString([]byte(vars["old"]))
}

func splitTargets(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// startWorkload runs workers from now until stop. Each worker's RNG is
// derived from the trial RNG, so the op stream replays with the seed.
// The caller must invoke this only when a driver is configured: deriving
// here consumes the trial RNG before nemesis actions draw their targets.
func (y *YAMLAdapter) startWorkload(t *BaseTrial, rt *topologyRuntime, rng *rand.Rand, origin time.Time) (func(), error) {
	if !y.workloadActive() {
		return func() {}, nil
	}
	if rng == nil {
		return nil, fmt.Errorf("workload: trial rng is missing")
	}
	spec := y.cfg.Workload
	ops := spec.opNames()
	if len(ops) == 0 {
		return nil, fmt.Errorf("workload: no operations configured")
	}
	base := y.workloadVars(t, rt)
	targets := splitTargets(expandWorkload(spec.Targets, base))
	if spec.Driver == "http_kv" && len(targets) == 0 {
		return nil, fmt.Errorf("workload: no targets from %q", spec.Targets)
	}
	if spec.Driver == "exec_op" && strings.TrimSpace(spec.Targets) != "" && len(targets) == 0 {
		return nil, fmt.Errorf("workload: no targets from %q", spec.Targets)
	}
	if len(targets) == 0 {
		targets = []string{""}
	}
	log := history.NewLog(origin)
	t.Data()["history"] = log
	t.Data()["workload_targets"] = targets
	client := &http.Client{Timeout: spec.timeout}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	interval := time.Duration(float64(time.Second) / spec.Rate)
	if interval <= 0 {
		interval = time.Microsecond
	}
	tokens := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				select {
				case tokens <- struct{}{}:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	for i := 0; i < spec.Workers; i++ {
		process := i + 1
		wr := deriveRand(rng)
		wg.Add(1)
		go func(process int, wr *rand.Rand) {
			defer wg.Done()
			w := &opRunner{
				process:  process,
				rng:      wr,
				log:      log,
				keyspace: spec.Keyspace,
				ops:      ops,
				targets:  targets,
				timeout:  spec.timeout,
				last:     map[string]string{},
				invoke: func(ctx context.Context, op, key, value, old, target string) (string, string, bool) {
					return y.doOp(ctx, base, client, op, key, value, old, target, process)
				},
			}
			w.loop(ctx, tokens)
		}(process, wr)
	}
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			wg.Wait()
			time.Sleep(workloadSettle)
		})
	}
	return stop, nil
}

type opRunner struct {
	process  int
	rng      *rand.Rand
	log      *history.Log
	keyspace int
	ops      []string
	targets  []string
	timeout  time.Duration
	last     map[string]string
	seq      int
	invoke   func(ctx context.Context, op, key, value, old, target string) (status, got string, absent bool)
}

func (w *opRunner) loop(ctx context.Context, tokens <-chan struct{}) {
	first := true
	for {
		select {
		case <-ctx.Done():
			return
		case <-tokens:
			if ctx.Err() != nil {
				return
			}
			force, pin := false, ""
			if first && w.process == 1 && w.hasOp(history.OpPut) {
				force = true
				if len(w.targets) > 0 {
					pin = w.targets[0]
				}
			}
			first = false
			w.once(ctx, force, pin)
		}
	}
}

func (w *opRunner) hasOp(name string) bool {
	for _, op := range w.ops {
		if op == name {
			return true
		}
	}
	return false
}

func (w *opRunner) once(ctx context.Context, forcePut bool, pinTarget string) {
	if ctx.Err() != nil || len(w.ops) == 0 || len(w.targets) == 0 || w.keyspace <= 0 {
		return
	}
	op := w.ops[w.rng.Intn(len(w.ops))]
	if forcePut {
		op = history.OpPut
	}
	key := fmt.Sprintf("k%d", w.rng.Intn(w.keyspace))
	value := fmt.Sprintf("v%d-%d", w.process, w.seq)
	w.seq++
	target := w.targets[w.rng.Intn(len(w.targets))]
	if pinTarget != "" {
		target = pinTarget
	}
	old := w.last[key]
	call := w.log.Now()
	reqCtx, cancel := context.WithTimeout(ctx, w.timeout)
	status, got, absent := w.invoke(reqCtx, op, key, value, old, target)
	cancel()
	ret := w.log.Now()
	recVal := value
	if op == history.OpGet {
		recVal = got
		if absent || status != history.StatusOK {
			recVal = ""
		}
	}
	if status == history.StatusOK && (op == history.OpPut || op == history.OpCas) {
		w.last[key] = value
	}
	rec := history.OpRecord{
		Process:  w.process,
		Op:       op,
		Key:      key,
		Value:    recVal,
		CallNS:   call,
		ReturnNS: ret,
		Status:   status,
	}
	if op == history.OpCas {
		rec.OldValue = old
	}
	w.log.Append(rec)
}

func (y *YAMLAdapter) doOp(ctx context.Context, base map[string]string, client *http.Client, op, key, value, old, target string, process int) (string, string, bool) {
	vars := maps.Clone(base)
	if vars == nil {
		vars = map[string]string{}
	}
	vars["target"] = target
	vars["key"] = key
	vars["value"] = value
	vars["old"] = old
	vars["op"] = op
	vars["process"] = strconv.Itoa(process)
	addKVEncodings(vars)
	if y.cfg.Workload.Driver == "exec_op" {
		return applyExecOp(ctx, y.execArgs(op, vars), op)
	}
	spec := y.cfg.Workload.Put
	switch op {
	case history.OpGet:
		spec = y.cfg.Workload.Get
	case history.OpCas:
		spec = y.cfg.Workload.Cas
	}
	method := spec.Method
	if method == "" {
		switch op {
		case history.OpGet:
			method = http.MethodGet
		case history.OpCas:
			method = http.MethodPost
		default:
			method = http.MethodPut
		}
	}
	headers := make(map[string]string, len(spec.Headers))
	for k, v := range spec.Headers {
		headers[k] = expandWorkload(v, vars)
	}
	return applyHTTPCall(ctx, client, httpCall{
		Method:        method,
		URL:           expandWorkload(spec.URL, vars),
		Body:          expandWorkload(spec.Body, vars),
		Headers:       headers,
		ValueFrom:     expandWorkload(spec.ValueFrom, vars),
		ValueEncoding: spec.ValueEncoding,
		ErrorFrom:     expandWorkload(spec.ErrorFrom, vars),
	}, op)
}

func (y *YAMLAdapter) execArgs(op string, vars map[string]string) []string {
	spec := y.cfg.Workload.Exec
	var argv []string
	switch op {
	case history.OpPut:
		argv = spec.Put
	case history.OpGet:
		argv = spec.Get
	case history.OpCas:
		argv = spec.Cas
	}
	if len(argv) == 0 {
		argv = spec.Command
	}
	out := make([]string, len(argv))
	for i, a := range argv {
		out[i] = expandWorkload(a, vars)
	}
	return out
}

// httpCall is one http_kv request. ValueFrom is a dotted JSON path
// (kvs.0.value). ValueEncoding base64 decodes that string. ErrorFrom is a
// path that makes a 2xx body a refusal when it is a non-empty string.
// Redirects are followed with the original method and body, including 301.
type httpCall struct {
	Method        string
	URL           string
	Body          string
	Headers       map[string]string
	ValueFrom     string
	ValueEncoding string
	ErrorFrom     string
}

const httpRedirectHops = 5

// applyHTTPOp maps transport errors (timeout, EOF, reset, refused) to info
// and a received status line through ClassifyHTTP.
func applyHTTPOp(ctx context.Context, client *http.Client, method, url, body, op string) (status, value string, absent bool) {
	return applyHTTPCall(ctx, client, httpCall{Method: method, URL: url, Body: body}, op)
}

func applyHTTPCall(ctx context.Context, client *http.Client, call httpCall, op string) (status, value string, absent bool) {
	if client == nil {
		client = &http.Client{}
	}
	c := *client
	c.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	resp, err := doHTTPFollow(ctx, &c, call)
	if err != nil {
		return history.StatusInfo, "", false
	}
	defer resp.Body.Close()
	status, absent = history.ClassifyHTTP(op, resp.StatusCode)
	buf, readErr := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	needJSON := call.ErrorFrom != "" || (op == history.OpGet && call.ValueFrom != "")
	var doc any
	parsed := false
	if status == history.StatusOK && !absent && needJSON {
		if readErr != nil {
			return history.StatusInfo, "", false
		}
		if err := json.Unmarshal(buf, &doc); err != nil {
			return history.StatusInfo, "", false
		}
		parsed = true
		if call.ErrorFrom != "" {
			if msg, ok := lookupJSON(doc, call.ErrorFrom); ok && msg != "" {
				return history.StatusFail, "", false
			}
		}
	}
	if op != history.OpGet || status != history.StatusOK {
		return status, "", absent
	}
	if readErr != nil {
		return history.StatusInfo, "", false
	}
	if absent {
		return status, "", true
	}
	if call.ValueFrom == "" {
		return status, strings.TrimRight(string(buf), "\r\n"), false
	}
	if !parsed {
		if err := json.Unmarshal(buf, &doc); err != nil {
			return history.StatusInfo, "", false
		}
	}
	raw, ok := lookupJSON(doc, call.ValueFrom)
	if !ok {
		return status, "", true
	}
	if call.ValueEncoding == "base64" {
		dec, err := base64.StdEncoding.DecodeString(raw)
		if err != nil {
			return history.StatusInfo, "", false
		}
		raw = string(dec)
	}
	if raw == "" {
		return status, "", true
	}
	return status, raw, false
}

func doHTTPFollow(ctx context.Context, client *http.Client, call httpCall) (*http.Response, error) {
	next := call.URL
	var resp *http.Response
	for hop := 0; hop <= httpRedirectHops; hop++ {
		var rdr io.Reader
		if call.Body != "" {
			rdr = strings.NewReader(call.Body)
		}
		req, err := http.NewRequestWithContext(ctx, call.Method, next, rdr)
		if err != nil {
			return nil, err
		}
		for k, v := range call.Headers {
			req.Header.Set(k, v)
		}
		resp, err = client.Do(req)
		if err != nil {
			return nil, err
		}
		if hop == httpRedirectHops || !isRedirect(resp.StatusCode) {
			return resp, nil
		}
		loc := resp.Header.Get("Location")
		if loc == "" {
			return resp, nil
		}
		resp.Body.Close()
		resolved, err := resolveRedirect(next, loc)
		if err != nil {
			return nil, err
		}
		next = resolved
	}
	return resp, nil
}

func isRedirect(code int) bool {
	switch code {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		return true
	default:
		return false
	}
}

func resolveRedirect(current, location string) (string, error) {
	base, err := url.Parse(current)
	if err != nil {
		return "", err
	}
	ref, err := url.Parse(location)
	if err != nil {
		return "", err
	}
	return base.ResolveReference(ref).String(), nil
}

func lookupJSON(v any, path string) (string, bool) {
	cur := v
	for _, p := range strings.Split(path, ".") {
		if p == "" {
			return "", false
		}
		if i, err := strconv.Atoi(p); err == nil {
			arr, ok := cur.([]any)
			if !ok || i < 0 || i >= len(arr) {
				return "", false
			}
			cur = arr[i]
			continue
		}
		obj, ok := cur.(map[string]any)
		if !ok {
			return "", false
		}
		next, ok := obj[p]
		if !ok || next == nil {
			return "", false
		}
		cur = next
	}
	return jsonScalar(cur)
}

func jsonScalar(v any) (string, bool) {
	switch x := v.(type) {
	case string:
		return x, true
	case bool:
		return strconv.FormatBool(x), true
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return "", false
		}
		if x == math.Trunc(x) && x >= math.MinInt64 && x <= math.MaxInt64 {
			return strconv.FormatInt(int64(x), 10), true
		}
		return strconv.FormatFloat(x, 'f', -1, 64), true
	default:
		return "", false
	}
}

func applyExecOp(ctx context.Context, args []string, op string) (status, value string, absent bool) {
	if len(args) == 0 || args[0] == "" {
		return history.StatusFail, "", false
	}
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = io.Discard
	err := cmd.Run()
	if ctx.Err() != nil {
		return history.StatusInfo, "", false
	}
	if err != nil {
		return history.StatusFail, "", false
	}
	if op == history.OpGet {
		v := strings.TrimRight(stdout.String(), "\r\n")
		if v == "" {
			return history.StatusOK, "", true
		}
		return history.StatusOK, v, false
	}
	return history.StatusOK, "", false
}

func (y *YAMLAdapter) checkHistory(t *BaseTrial) CheckResult {
	log, _ := t.Data()["history"].(*history.Log)
	if log == nil {
		return CheckResult{
			Verdict:  "UNKNOWN",
			Reason:   "no_history",
			Evidence: 1,
			Problems: []error{fmt.Errorf("workload history was not recorded")},
		}
	}
	var obs []history.Observation
	if y.cfg.Check.Porcupine != nil || y.cfg.Check.Set != nil {
		targets, _ := t.Data()["workload_targets"].([]string)
		obs = y.readBack(t, log, targets)
		t.Data()["read_back_obs"] = obs
		t.Data()["read_back_done"] = true
	}
	qs, _ := t.Data()["quiet_start_ns"].(int64)
	qe, _ := t.Data()["quiet_end_ns"].(int64)
	timeout := 5 * time.Second
	if p := y.cfg.Check.Porcupine; p != nil && p.timeout > 0 {
		timeout = p.timeout
	}
	spec := coreSpec{
		ops:              log.Snapshot(),
		obs:              obs,
		quietStart:       qs,
		quietEnd:         qe,
		porcupine:        y.cfg.Check.Porcupine != nil,
		porcupineTimeout: timeout,
		set:              y.cfg.Check.Set != nil,
		eventually:       y.cfg.Check.Eventually != nil,
		eventuallyN:      1,
		htmlPath:         bundleRun.saveDir,
	}
	if y.cfg.Check.Eventually != nil && y.cfg.Check.Eventually.N > 0 {
		spec.eventuallyN = y.cfg.Check.Eventually.N
	}
	cr, html := runCoreChecks(spec)
	if len(html) > 0 {
		t.Data()["porcupine_html"] = html
	}
	return cr
}

func (y *YAMLAdapter) readBack(t *BaseTrial, log *history.Log, targets []string) []history.Observation {
	latest := history.LatestOK(log.Snapshot())
	if len(latest) == 0 || len(targets) == 0 {
		return nil
	}
	keys := make([]string, 0, len(latest))
	for k := range latest {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	base := y.workloadVars(t, nil)
	if rt, ok := t.Data()["topology"].(*topologyRuntime); ok {
		base = y.workloadVars(t, rt)
	}
	client := &http.Client{Timeout: y.cfg.Workload.timeout}
	var obs []history.Observation
	for _, key := range keys {
		for _, target := range targets {
			ctx, cancel := context.WithTimeout(context.Background(), y.cfg.Workload.timeout)
			call := log.Now()
			status, got, absent := y.doOp(ctx, base, client, history.OpGet, key, "", "", target, 0)
			cancel()
			ret := log.Now()
			val := got
			if absent || status != history.StatusOK {
				val = ""
			}
			log.Append(history.OpRecord{
				Process:  0,
				Op:       history.OpGet,
				Key:      key,
				Value:    val,
				CallNS:   call,
				ReturnNS: ret,
				Status:   status,
			})
			obs = append(obs, history.Observation{Key: key, Value: val, Absent: absent, Status: status})
		}
	}
	return obs
}
