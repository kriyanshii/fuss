package main

import (
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"fuss/proxy"

	"gopkg.in/yaml.v3"
)

type yamlTarget struct {
	Name      string                 `yaml:"name"`
	Binary    yamlBinary             `yaml:"binary"`
	Args      []string               `yaml:"args"`
	Env       map[string]string      `yaml:"env"`
	DataDir   string                 `yaml:"data_dir"`
	PortBase  int                    `yaml:"port_base"`
	PortMod   int                    `yaml:"port_mod"`
	Start     yamlStart              `yaml:"start"`
	Health    yamlHealth             `yaml:"health"`
	Workload  yamlWorkload           `yaml:"workload"`
	Kill      yamlKill               `yaml:"kill"`
	Restart   yamlRestart            `yaml:"restart"`
	Steps     []yamlStep             `yaml:"steps"`
	Topology  yamlTopology           `yaml:"topology"`
	Fault     yamlFault              `yaml:"fault"`
	Check     yamlCheck              `yaml:"check"`
	Controls  map[string]yamlControl `yaml:"controls"`
}

// yamlTopology starts N peer processes for one trial. Empty Nodes means no topology.
type yamlTopology struct {
	Nodes     int           `yaml:"nodes"`
	DataDir   string        `yaml:"data_dir"`
	Ports     yamlTopoPorts `yaml:"ports"`
	Peers     yamlTopoPeers `yaml:"peers"`
	Start     []string      `yaml:"start"`
	Ready     yamlTopoReady `yaml:"ready"`
	StopGrace string        `yaml:"stop_grace"`
}

// yamlTopoPeers routes peer traffic through Fuss proxy.Link when ViaProxy is set.
type yamlTopoPeers struct {
	PortName string `yaml:"port_name"` // which per-node port is the peer port (the topology ${port})
	ViaProxy bool   `yaml:"via_proxy"`
}

// yamlFault injects a live fault while topology nodes keep running.
type yamlFault struct {
	Isolate string         `yaml:"isolate"` // "node_0" (leader later)
	After   yamlFaultAfter `yaml:"after"`
}

type yamlFaultAfter struct {
	File yamlKillFile `yaml:"file"`
}

type yamlTopoPorts struct {
	Base int `yaml:"base"` // 0 means pick a free port per node
}

// yamlTopoReady is a per-node readiness check. File (path exists, optional
// contains) and command (stdout contains) are both accepted; file is tried
// first when set.
type yamlTopoReady struct {
	File     string   `yaml:"file"`
	Contains string   `yaml:"contains"`
	Command  []string `yaml:"command"`
	Timeout  string   `yaml:"timeout"`
}

type yamlBinary struct {
	Default string   `yaml:"default"`
	Extra   []string `yaml:"extra"`
}

type yamlStart struct {
	WaitExit bool   `yaml:"wait_exit"`
	Timeout  string `yaml:"timeout"`
}

type yamlHealth struct {
	HTTP yamlHTTP `yaml:"http"`
}

type yamlHTTP struct {
	Method     string `yaml:"method"`
	URL        string `yaml:"url"`
	Body       string `yaml:"body"`
	Timeout    string `yaml:"timeout"`
	AckStatus  int    `yaml:"ack_status"`
	WantStatus int    `yaml:"want_status"`
}

type yamlWorkload struct {
	Until        string   `yaml:"until"`
	WaitFirstAck bool     `yaml:"wait_first_ack"`
	HTTP         yamlHTTP `yaml:"http"`
	Ack          yamlAck  `yaml:"ack"`
}

type yamlAck struct {
	Record string `yaml:"record"`
	Key    string `yaml:"key"`
}

type yamlKill struct {
	Signal string       `yaml:"signal"`
	At     string       `yaml:"at"`
	Delay  yamlDelay    `yaml:"delay"`
	File   yamlKillFile `yaml:"file"`
}

type yamlDelay struct {
	MinMS int `yaml:"min_ms"`
	MaxMS int `yaml:"max_ms"`
}

type yamlKillFile struct {
	Glob     string `yaml:"glob"`
	Contains string `yaml:"contains"`
	Poll     string `yaml:"poll"`
	OffsetMS int    `yaml:"offset_ms"`
	Timeout  string `yaml:"timeout"`
}

type yamlRestart struct {
	Args   []string   `yaml:"args"`
	Health yamlHealth `yaml:"health"`
}

type yamlStep struct {
	Name     string   `yaml:"name"`
	Command  []string `yaml:"command"`
	WaitExit bool     `yaml:"wait_exit"`
	Timeout  string   `yaml:"timeout"`
	RunFor   string   `yaml:"run_for"`
	Stop     string   `yaml:"stop"`
	Kill     yamlKill `yaml:"kill"`
}

type yamlCheck struct {
	ForEach     string        `yaml:"for_each"`
	HTTP        yamlHTTP      `yaml:"http"`
	MissVerdict string        `yaml:"miss_verdict"`
	FileCount   yamlFileCount `yaml:"file_count"`
	RecordCount yamlRecCount  `yaml:"record_count"`
	Command     yamlCmdCheck  `yaml:"command"`
	Rules       []yamlRule    `yaml:"rules"`
}

type yamlFileCount struct {
	Glob string `yaml:"glob"`
	As   string `yaml:"as"`
}

type yamlRecCount struct {
	Contains string `yaml:"contains"`
	As       string `yaml:"as"`
}

type yamlCmdCheck struct {
	Command  []string `yaml:"command"`
	As       string   `yaml:"as"`
	Contains string   `yaml:"contains"`
}

type yamlRule struct {
	If      map[string]any `yaml:"if"`
	Verdict string         `yaml:"verdict"`
	Else    string         `yaml:"else"`
}

type yamlControl struct {
	PlantRecord   string `yaml:"plant_record"`
	PlantDir      string `yaml:"plant_dir"`
	ExpectVerdict string `yaml:"expect_verdict"`
}

// YAMLAdapter runs a target described by a YAML file.
type YAMLAdapter struct {
	path string
	cfg  yamlTarget
}

func LoadYAMLTarget(path string) (*YAMLAdapter, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("target-config %s: %w", path, err)
	}
	var cfg yamlTarget
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return nil, fmt.Errorf("target-config %s: %w", path, err)
	}
	if cfg.Name == "" {
		return nil, fmt.Errorf("target-config %s: name is required", path)
	}
	switch cfg.Kill.At {
	case "", "delay", "file":
	default:
		return nil, fmt.Errorf("target-config %s: kill.at %q not supported (delay or file)", path, cfg.Kill.At)
	}
	if cfg.PortMod <= 0 {
		cfg.PortMod = 500
	}
	if cfg.PortBase <= 0 {
		cfg.PortBase = 18700
	}
	return &YAMLAdapter{path: path, cfg: cfg}, nil
}

func (y *YAMLAdapter) Name() string { return y.cfg.Name }

func (y *YAMLAdapter) ResolveBinary(targetFlag string) (string, string, error) {
	if targetFlag != "" {
		p, err := exec.LookPath(targetFlag)
		if err != nil {
			return "", "", fmt.Errorf("no binary (-target %q): %w", targetFlag, err)
		}
		abs, err := filepath.Abs(p)
		if err != nil {
			return "", "", err
		}
		return abs, "-target", nil
	}
	for _, extra := range y.cfg.Binary.Extra {
		p, err := filepath.Abs(extra)
		if err != nil {
			continue
		}
		st, err := os.Stat(p)
		if err == nil && !st.IsDir() {
			return p, extra, nil
		}
	}
	name := y.cfg.Binary.Default
	if name == "" {
		name = y.cfg.Name
	}
	p, err := exec.LookPath(name)
	if err != nil {
		return "", "", fmt.Errorf("no %s binary (%q, extras %v): %w", y.cfg.Name, name, y.cfg.Binary.Extra, err)
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", "", err
	}
	return abs, "PATH", nil
}

func (y *YAMLAdapter) Init(seed int64, dir, binaryPath string) (AdapterTrial, error) {
	t := NewBaseTrial(seed, dir, binaryPath, "fuss-"+y.cfg.Name)
	if err := os.MkdirAll(t.Home(), 0o755); err != nil {
		return nil, err
	}
	addr := fmt.Sprintf("127.0.0.1:%d", y.cfg.PortBase+int(seed%int64(y.cfg.PortMod)))
	t.Data()["addr"] = addr
	t.Data()["workload"] = true
	vars := y.vars(t, "")
	dataDir := y.cfg.DataDir
	if dataDir == "" {
		dataDir = "${home}"
	}
	dataDir = expandTemplates(dataDir, vars)
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, err
	}
	t.Data()["data_dir"] = dataDir
	return t, nil
}

func (y *YAMLAdapter) vars(t *BaseTrial, n string) map[string]string {
	addr, _ := t.Data()["addr"].(string)
	m := map[string]string{
		"home":   t.Home(),
		"dir":    t.Dir(),
		"seed":   strconv.FormatInt(t.Seed(), 10),
		"run_id": t.RunID(),
		"addr":   addr,
		"binary": t.BinaryPath(),
		"n":      n,
		"key":    "",
	}
	if y.cfg.Workload.Ack.Key != "" {
		m["key"] = expandTemplates(y.cfg.Workload.Ack.Key, m)
	} else if n != "" {
		m["key"] = m["seed"] + "-" + n
	}
	return m
}

func expandTemplates(s string, vars map[string]string) string {
	keys := []string{"run_id", "binary", "home", "addr", "seed", "dir", "key", "n", "i", "port", "data_dir", "peers"}
	for _, k := range keys {
		s = strings.ReplaceAll(s, "${"+k+"}", vars[k])
	}
	for k, v := range vars {
		if strings.HasPrefix(k, "peer_addr_") {
			s = strings.ReplaceAll(s, "${"+k+"}", v)
		}
	}
	return s
}

func (y *YAMLAdapter) expandSlice(in []string, vars map[string]string) []string {
	out := make([]string, len(in))
	for i, a := range in {
		out[i] = expandTemplates(a, vars)
	}
	return out
}

func (y *YAMLAdapter) applyEnv(cmd *exec.Cmd, vars map[string]string) {
	if len(y.cfg.Env) == 0 {
		return
	}
	env := append([]string{}, os.Environ()...)
	for k, v := range y.cfg.Env {
		env = append(env, k+"="+expandTemplates(v, vars))
	}
	cmd.Env = env
}

func (y *YAMLAdapter) Start(at AdapterTrial, log *os.File) (*exec.Cmd, error) {
	t, ok := at.(*BaseTrial)
	if !ok {
		return nil, fmt.Errorf("expected *BaseTrial, got %T", at)
	}
	if y.cfg.Topology.Nodes > 0 {
		return y.startTopology(t, log)
	}
	if len(y.cfg.Steps) > 0 {
		return y.startSteps(t, log)
	}
	vars := y.vars(t, "")
	args := y.startArgs(t)
	cmd := exec.Command(t.BinaryPath(), args...)
	cmd.Stdout = log
	cmd.Stderr = log
	y.applyEnv(cmd, vars)
	if y.cfg.Start.WaitExit {
		to := parseYAMLDuration(y.cfg.Start.Timeout, stepTimeout)
		if err := runWithin(cmd, to); err != nil {
			return nil, err
		}
		return cmd, nil
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	h := y.healthSpec(t)
	if h.URL != "" {
		if err := waitYAMLHTTP(h, vars, parseYAMLDuration(h.Timeout, 5*time.Second)); err != nil {
			cmd.Process.Kill()
			cmd.Wait()
			return nil, err
		}
	}
	if workload, _ := t.Data()["workload"].(bool); workload && y.cfg.Workload.HTTP.URL != "" {
		go y.putUntilDead(t)
		if y.cfg.Workload.WaitFirstAck {
			if err := waitFirstAck(t, 5*time.Second); err != nil {
				cmd.Process.Kill()
				cmd.Wait()
				return nil, err
			}
		}
	}
	return cmd, nil
}

func (y *YAMLAdapter) startSteps(t *BaseTrial, log *os.File) (*exec.Cmd, error) {
	vars := y.vars(t, "")
	var after []yamlStep
	sawLive := false
	var live *exec.Cmd
	for i, st := range y.cfg.Steps {
		if sawLive {
			break
		}
		args := y.expandSlice(st.Command, vars)
		if len(args) == 0 {
			return nil, fmt.Errorf("step %q: empty command", st.Name)
		}
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Stdout = log
		cmd.Stderr = log
		y.applyEnv(cmd, vars)
		if st.RunFor != "" {
			d := parseYAMLDuration(st.RunFor, time.Second)
			if err := cmd.Start(); err != nil {
				return nil, fmt.Errorf("step %q: %w", st.Name, err)
			}
			time.Sleep(d)
			if err := signalNamed(cmd, st.Stop); err != nil {
				cmd.Process.Kill()
			}
			cmd.Wait()
			continue
		}
		if st.WaitExit {
			to := parseYAMLDuration(st.Timeout, stepTimeout)
			if err := runWithin(cmd, to); err != nil {
				return nil, fmt.Errorf("step %q: %w", st.Name, err)
			}
			continue
		}
		if err := cmd.Start(); err != nil {
			return nil, fmt.Errorf("step %q: %w", st.Name, err)
		}
		if st.Kill.At == "file" {
			t.Data()["file_kill"] = st.Kill.File
		}
		live = cmd
		sawLive = true
		if i+1 < len(y.cfg.Steps) {
			after = y.cfg.Steps[i+1:]
		}
		break
	}
	t.Data()["after_steps"] = after
	if live != nil {
		return live, nil
	}
	cmd := exec.Command("/bin/true")
	cmd.Stdout = log
	cmd.Stderr = log
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd, nil
}

func signalNamed(cmd *exec.Cmd, name string) error {
	if cmd.Process == nil {
		return nil
	}
	switch strings.ToUpper(name) {
	case "", "SIGTERM", "TERM":
		return cmd.Process.Signal(syscall.SIGTERM)
	case "SIGKILL", "KILL":
		return cmd.Process.Kill()
	default:
		return fmt.Errorf("unknown stop signal %q", name)
	}
}

// topologyNode is one peer process in a topology trial.
type topologyNode struct {
	Index      int
	PID        int
	Port       int
	DataDir    string
	StartTime  time.Time
	ExitStatus *int
	Ready      bool
	StopHow    string // "SIGTERM", "SIGKILL", or empty if not stopped by us
	cmd        *exec.Cmd
	waitDone   chan struct{}
}

// topologyLink is one directed proxy.Link from node From to node To's peer port.
type topologyLink struct {
	From        int
	To          int
	Link        *proxy.Link
	Listen      string
	StatsBefore proxy.Stats
	StatsAfter  proxy.Stats
	CutTime     time.Time
	cut         bool
}

type isolateState struct {
	Target       string
	TriggerFired bool
	CutTime      time.Time
	Done         bool // waiter finished (fired or timed out)
}

type topologyRuntime struct {
	mu       sync.Mutex
	Nodes    []*topologyNode
	Links    []*topologyLink
	Isolate  *isolateState
	AllReady bool
	Stopped  bool
}

func (rt *topologyRuntime) evidence() []map[string]any {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	out := make([]map[string]any, 0, len(rt.Nodes))
	for _, n := range rt.Nodes {
		if n == nil {
			continue
		}
		m := map[string]any{
			"i":          n.Index,
			"pid":        n.PID,
			"port":       n.Port,
			"data_dir":   n.DataDir,
			"start_time": n.StartTime.UTC().Format(time.RFC3339Nano),
			"ready":      n.Ready,
			"stop":       n.StopHow,
		}
		if n.ExitStatus != nil {
			m["exit_status"] = *n.ExitStatus
		} else {
			m["exit_status"] = nil
		}
		out = append(out, m)
	}
	return out
}

func statsMap(s proxy.Stats) map[string]any {
	m := map[string]any{
		"open":              s.Open,
		"accepted":          s.Accepted,
		"closed_by_cut":     s.ClosedByCut,
		"refused_while_cut": s.RefusedWhileCut,
	}
	if !s.LastCut.IsZero() {
		m["last_cut"] = s.LastCut.UTC().Format(time.RFC3339Nano)
	}
	if !s.LastHeal.IsZero() {
		m["last_heal"] = s.LastHeal.UTC().Format(time.RFC3339Nano)
	}
	return m
}

func (rt *topologyRuntime) isolateEvidence() map[string]any {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.Isolate == nil {
		return nil
	}
	links := make([]map[string]any, 0, len(rt.Links))
	for _, l := range rt.Links {
		if l == nil || l.Link == nil {
			continue
		}
		after := l.StatsAfter
		if l.cut {
			after = l.Link.Stats()
			l.StatsAfter = after
		}
		m := map[string]any{
			"from":         l.From,
			"to":           l.To,
			"listen":       l.Listen,
			"cut":          l.cut,
			"stats_before": statsMap(l.StatsBefore),
			"stats_after":  statsMap(after),
		}
		if !l.CutTime.IsZero() {
			m["cut_time"] = l.CutTime.UTC().Format(time.RFC3339Nano)
		}
		links = append(links, m)
	}
	out := map[string]any{
		"target":        rt.Isolate.Target,
		"trigger_fired": rt.Isolate.TriggerFired,
		"links":         links,
	}
	if !rt.Isolate.CutTime.IsZero() {
		out["cut_time"] = rt.Isolate.CutTime.UTC().Format(time.RFC3339Nano)
	}
	return out
}

func (rt *topologyRuntime) cutObserved() bool {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	for _, l := range rt.Links {
		if l == nil || !l.cut || l.Link == nil {
			continue
		}
		st := l.Link.Stats()
		l.StatsAfter = st
		if st.ClosedByCut > l.StatsBefore.ClosedByCut || st.RefusedWhileCut > l.StatsBefore.RefusedWhileCut {
			return true
		}
	}
	return false
}

func freePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

func (y *YAMLAdapter) startTopology(t *BaseTrial, log *os.File) (*exec.Cmd, error) {
	topo := y.cfg.Topology
	if len(topo.Start) == 0 {
		return nil, fmt.Errorf("topology.start is required when topology.nodes > 0")
	}
	n := topo.Nodes
	ports := make([]int, n)
	for i := 0; i < n; i++ {
		if topo.Ports.Base == 0 {
			p, err := freePort()
			if err != nil {
				return nil, fmt.Errorf("topology: free port: %w", err)
			}
			ports[i] = p
		} else {
			ports[i] = topo.Ports.Base + i
		}
	}

	rt := &topologyRuntime{Nodes: make([]*topologyNode, n)}
	peerAddrs := make([][]string, n) // peerAddrs[i][j] = addr node i uses to reach j
	if topo.Peers.ViaProxy {
		for i := 0; i < n; i++ {
			peerAddrs[i] = make([]string, n)
		}
		for i := 0; i < n; i++ {
			for j := 0; j < n; j++ {
				if i == j {
					continue
				}
				upstream := "127.0.0.1:" + strconv.Itoa(ports[j])
				link, err := proxy.New("127.0.0.1:0", upstream)
				if err != nil {
					y.stopTopology(rt)
					return nil, fmt.Errorf("topology proxy %d->%d: %w", i, j, err)
				}
				if err := link.Start(); err != nil {
					y.stopTopology(rt)
					return nil, fmt.Errorf("topology proxy %d->%d start: %w", i, j, err)
				}
				addr := link.Addr().String()
				tl := &topologyLink{From: i, To: j, Link: link, Listen: addr}
				rt.Links = append(rt.Links, tl)
				peerAddrs[i][j] = addr
			}
		}
	}

	var mu sync.Mutex
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			base := y.vars(t, "")
			base["i"] = strconv.Itoa(i)
			dataDirTmpl := topo.DataDir
			if dataDirTmpl == "" {
				dataDirTmpl = "${dir}/node${i}"
			}
			dataDir := expandTemplates(dataDirTmpl, base)
			if err := os.MkdirAll(dataDir, 0o755); err != nil {
				errs[i] = err
				return
			}
			vars := y.vars(t, "")
			vars["i"] = strconv.Itoa(i)
			vars["data_dir"] = dataDir
			vars["port"] = strconv.Itoa(ports[i])
			y.addPeerVars(vars, peerAddrs, i)
			args := y.expandSlice(topo.Start, vars)
			if len(args) == 0 {
				errs[i] = fmt.Errorf("topology.start expanded empty for node %d", i)
				return
			}
			cmd := exec.Command(args[0], args[1:]...)
			cmd.Stdout = log
			cmd.Stderr = log
			y.applyEnv(cmd, vars)
			startTime := time.Now()
			if err := cmd.Start(); err != nil {
				errs[i] = fmt.Errorf("topology node %d: %w", i, err)
				return
			}
			node := &topologyNode{
				Index:     i,
				PID:       cmd.Process.Pid,
				Port:      ports[i],
				DataDir:   dataDir,
				StartTime: startTime,
				cmd:       cmd,
				waitDone:  make(chan struct{}),
			}
			go func() {
				_ = cmd.Wait()
				mu.Lock()
				code := -1
				if cmd.ProcessState != nil {
					code = cmd.ProcessState.ExitCode()
				}
				node.ExitStatus = &code
				mu.Unlock()
				close(node.waitDone)
			}()
			mu.Lock()
			rt.Nodes[i] = node
			mu.Unlock()
		}(i)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			y.stopTopology(rt)
			return nil, err
		}
	}

	readyTo := parseYAMLDuration(topo.Ready.Timeout, 5*time.Second)
	allReady := true
	var readyWG sync.WaitGroup
	for i := 0; i < n; i++ {
		readyWG.Add(1)
		go func(i int) {
			defer readyWG.Done()
			node := rt.Nodes[i]
			vars := y.vars(t, "")
			vars["i"] = strconv.Itoa(i)
			vars["data_dir"] = node.DataDir
			vars["port"] = strconv.Itoa(node.Port)
			y.addPeerVars(vars, peerAddrs, i)
			if err := y.waitTopologyReady(node, vars, readyTo); err != nil {
				mu.Lock()
				allReady = false
				mu.Unlock()
				return
			}
			mu.Lock()
			node.Ready = true
			mu.Unlock()
		}(i)
	}
	readyWG.Wait()
	rt.AllReady = allReady
	t.Data()["topology"] = rt

	if y.cfg.Fault.Isolate != "" {
		rt.Isolate = &isolateState{Target: y.cfg.Fault.Isolate}
		go y.watchIsolate(t, rt)
	}

	hold := exec.Command("sleep", "3600")
	hold.Stdout = log
	hold.Stderr = log
	if err := hold.Start(); err != nil {
		y.stopTopology(rt)
		return nil, err
	}
	return hold, nil
}

func (y *YAMLAdapter) addPeerVars(vars map[string]string, peerAddrs [][]string, i int) {
	if len(peerAddrs) == 0 {
		return
	}
	var peers []string
	for j, addr := range peerAddrs[i] {
		key := "peer_addr_" + strconv.Itoa(j)
		vars[key] = addr
		if addr != "" {
			peers = append(peers, addr)
		}
	}
	vars["peers"] = strings.Join(peers, ",")
}

func parseIsolateNode(s string) (int, error) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "node_") {
		return -1, fmt.Errorf("fault.isolate %q: want node_<index>", s)
	}
	return strconv.Atoi(strings.TrimPrefix(s, "node_"))
}

func (y *YAMLAdapter) watchIsolate(t *BaseTrial, rt *topologyRuntime) {
	spec := y.cfg.Fault.After.File
	vars := y.vars(t, "")
	glob := expandTemplates(spec.Glob, vars)
	contains := expandTemplates(spec.Contains, vars)
	poll := parseYAMLDuration(spec.Poll, time.Millisecond)
	timeout := parseYAMLDuration(spec.Timeout, 30*time.Second)
	err := waitFileContains(glob, contains, poll, timeout)

	rt.mu.Lock()
	if rt.Isolate == nil {
		rt.mu.Unlock()
		return
	}
	rt.Isolate.Done = true
	if err != nil {
		rt.mu.Unlock()
		return
	}
	rt.Isolate.TriggerFired = true
	target := expandTemplates(y.cfg.Fault.Isolate, vars)
	rt.Isolate.Target = target
	rt.mu.Unlock()

	idx, perr := parseIsolateNode(target)
	if perr != nil {
		return
	}
	y.cutIsolate(rt, idx)
}

func (y *YAMLAdapter) cutIsolate(rt *topologyRuntime, nodeIdx int) {
	rt.mu.Lock()
	cutAt := time.Now()
	if rt.Isolate != nil {
		rt.Isolate.CutTime = cutAt
	}
	var toCut []*topologyLink
	for _, l := range rt.Links {
		if l == nil || l.Link == nil {
			continue
		}
		if l.From == nodeIdx || l.To == nodeIdx {
			toCut = append(toCut, l)
		}
	}
	rt.mu.Unlock()

	for _, l := range toCut {
		before := l.Link.Stats()
		l.Link.Cut()
		rt.mu.Lock()
		l.StatsBefore = before
		l.CutTime = cutAt
		l.cut = true
		l.StatsAfter = l.Link.Stats()
		rt.mu.Unlock()
	}
}

// checkIsolate returns a non-empty Verdict when the isolate fault cannot be proven.
func (y *YAMLAdapter) checkIsolate(rt *topologyRuntime) CheckResult {
	wait := parseYAMLDuration(y.cfg.Fault.After.File.Timeout, 30*time.Second) + 2*time.Second
	deadline := time.Now().Add(wait)
	for {
		rt.mu.Lock()
		done := rt.Isolate != nil && rt.Isolate.Done
		fired := rt.Isolate != nil && rt.Isolate.TriggerFired
		rt.mu.Unlock()
		if done {
			if !fired {
				return CheckResult{
					Verdict:  "UNKNOWN",
					Evidence: 1,
					Problems: []error{fmt.Errorf("fault.isolate: trigger did not fire")},
				}
			}
			break
		}
		if time.Now().After(deadline) {
			return CheckResult{
				Verdict:  "UNKNOWN",
				Evidence: 1,
				Problems: []error{fmt.Errorf("fault.isolate: trigger did not fire")},
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	obsDeadline := time.Now().Add(2 * time.Second)
	for {
		if rt.cutObserved() {
			return CheckResult{}
		}
		if time.Now().After(obsDeadline) {
			return CheckResult{
				Verdict:  "UNKNOWN",
				Evidence: 1,
				Problems: []error{fmt.Errorf("fault.isolate: no link closed or refused after cut")},
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (y *YAMLAdapter) waitTopologyReady(node *topologyNode, vars map[string]string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-node.waitDone:
			return fmt.Errorf("topology node %d exited before ready", node.Index)
		default:
		}
		if y.topologyReadyOnce(vars) {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf("topology node %d not ready in %v", node.Index, timeout)
}

func (y *YAMLAdapter) topologyReadyOnce(vars map[string]string) bool {
	r := y.cfg.Topology.Ready
	if r.File != "" {
		p := expandTemplates(r.File, vars)
		b, err := os.ReadFile(p)
		if err != nil {
			return false
		}
		if r.Contains == "" || strings.Contains(string(b), expandTemplates(r.Contains, vars)) {
			return true
		}
		return false
	}
	if len(r.Command) > 0 {
		args := y.expandSlice(r.Command, vars)
		cmd := exec.Command(args[0], args[1:]...)
		y.applyEnv(cmd, vars)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return false
		}
		if r.Contains == "" {
			return len(out) > 0
		}
		return strings.Contains(string(out), expandTemplates(r.Contains, vars))
	}
	// No ready check configured: treat as ready once started.
	return true
}

func (y *YAMLAdapter) stopTopology(rt *topologyRuntime) {
	if rt == nil {
		return
	}
	rt.mu.Lock()
	if rt.Stopped {
		rt.mu.Unlock()
		return
	}
	rt.Stopped = true
	nodes := append([]*topologyNode(nil), rt.Nodes...)
	links := append([]*topologyLink(nil), rt.Links...)
	rt.mu.Unlock()

	for _, l := range links {
		if l != nil && l.Link != nil {
			_ = l.Link.Close()
		}
	}

	grace := parseYAMLDuration(y.cfg.Topology.StopGrace, 2*time.Second)
	for _, n := range nodes {
		if n == nil || n.cmd == nil || n.cmd.Process == nil {
			continue
		}
		_ = n.cmd.Process.Signal(syscall.SIGTERM)
	}
	deadline := time.Now().Add(grace)
	for _, n := range nodes {
		if n == nil || n.waitDone == nil {
			continue
		}
		remaining := time.Until(deadline)
		if remaining < 0 {
			remaining = 0
		}
		select {
		case <-n.waitDone:
			rt.mu.Lock()
			if n.StopHow == "" {
				n.StopHow = "SIGTERM"
			}
			rt.mu.Unlock()
		case <-time.After(remaining):
		}
	}
	for _, n := range nodes {
		if n == nil || n.cmd == nil || n.cmd.Process == nil {
			continue
		}
		select {
		case <-n.waitDone:
			continue
		default:
		}
		_ = n.cmd.Process.Kill()
		<-n.waitDone
		rt.mu.Lock()
		n.StopHow = "SIGKILL"
		rt.mu.Unlock()
	}
}

func waitYAMLHTTP(h yamlHTTP, vars map[string]string, d time.Duration) error {
	deadline := time.Now().Add(d)
	method := h.Method
	if method == "" {
		method = http.MethodGet
	}
	url := expandTemplates(h.URL, vars)
	client := &http.Client{Timeout: 100 * time.Millisecond}
	for time.Now().Before(deadline) {
		req, err := http.NewRequest(method, url, nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err == nil {
			resp.Body.Close()
			if h.WantStatus == 0 || resp.StatusCode == h.WantStatus {
				return nil
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("health %s %s did not succeed in %v", method, url, d)
}

func (y *YAMLAdapter) putUntilDead(t *BaseTrial) {
	h := y.cfg.Workload.HTTP
	to := parseYAMLDuration(h.Timeout, 100*time.Millisecond)
	client := &http.Client{Timeout: to}
	method := h.Method
	if method == "" {
		method = http.MethodPut
	}
	want := h.AckStatus
	if want == 0 {
		want = http.StatusOK
	}
	for n := 0; ; n++ {
		vars := y.vars(t, strconv.Itoa(n))
		url := expandTemplates(h.URL, vars)
		body := expandTemplates(h.Body, vars)
		req, err := http.NewRequest(method, url, strings.NewReader(body))
		if err != nil {
			return
		}
		resp, err := client.Do(req)
		if err != nil {
			return
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode == want {
			line := y.cfg.Workload.Ack.Record
			if line == "" {
				line = "ack ${key}\n"
			}
			line = expandTemplates(line, vars)
			if err := appendToFile(t.Record(), line); err != nil {
				return
			}
		}
	}
}

func parseYAMLDuration(s string, def time.Duration) time.Duration {
	if s == "" {
		return def
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return def
	}
	return d
}

func (y *YAMLAdapter) Status(at AdapterTrial) (AdapterStatus, error) {
	t, ok := at.(*BaseTrial)
	if !ok {
		return nil, fmt.Errorf("expected *BaseTrial, got %T", at)
	}
	if y.cfg.Health.HTTP.URL == "" {
		return yamlStatus{State: "unknown"}, nil
	}
	vars := y.vars(t, "")
	client := &http.Client{Timeout: 100 * time.Millisecond}
	url := expandTemplates(y.cfg.Health.HTTP.URL, vars)
	resp, err := client.Get(url)
	if err != nil {
		return yamlStatus{State: "down"}, nil
	}
	resp.Body.Close()
	return yamlStatus{State: "running"}, nil
}

func (y *YAMLAdapter) Settle(query func() (AdapterStatus, error), deadline time.Duration) (AdapterStatus, error) {
	end := time.Now().Add(deadline)
	for {
		st, err := query()
		if err != nil {
			return st, err
		}
		if !st.IsRunning() {
			return st, nil
		}
		if time.Now().After(end) {
			return st, fmt.Errorf("timeout")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (y *YAMLAdapter) startArgs(t *BaseTrial) []string {
	src := y.cfg.Args
	if w, _ := t.Data()["workload"].(bool); !w && len(y.cfg.Restart.Args) > 0 {
		src = y.cfg.Restart.Args
	}
	return y.expandSlice(src, y.vars(t, ""))
}

func (y *YAMLAdapter) healthSpec(t *BaseTrial) yamlHTTP {
	if w, _ := t.Data()["workload"].(bool); !w && y.cfg.Restart.Health.HTTP.URL != "" {
		return y.cfg.Restart.Health.HTTP
	}
	return y.cfg.Health.HTTP
}

func (y *YAMLAdapter) Check(at AdapterTrial, killed bool, phase string) []error {
	return y.CheckNamed(at, killed, phase).Problems
}

func (y *YAMLAdapter) CheckNamed(at AdapterTrial, killed bool, phase string) CheckResult {
	t, ok := at.(*BaseTrial)
	if !ok {
		return CheckResult{Verdict: "FAIL", Problems: []error{fmt.Errorf("expected *BaseTrial, got %T", at)}}
	}
	if rt, ok := t.Data()["topology"].(*topologyRuntime); ok {
		defer y.stopTopology(rt)
		if !rt.AllReady {
			return CheckResult{
				Verdict:  "UNKNOWN",
				Evidence: 1,
				Problems: []error{fmt.Errorf("topology: a node was not ready in time")},
			}
		}
		if y.cfg.Fault.Isolate != "" {
			if cr := y.checkIsolate(rt); cr.Verdict != "" {
				return cr
			}
		}
	}
	if after, ok := t.Data()["after_steps"].([]yamlStep); ok {
		if err := y.runAfterSteps(t, after); err != nil {
			return CheckResult{Verdict: "FAIL", Problems: []error{err}, Evidence: 1}
		}
	}
	if len(y.cfg.Check.Rules) > 0 || y.cfg.Check.FileCount.Glob != "" || y.cfg.Check.RecordCount.Contains != "" || len(y.cfg.Check.Command.Command) > 0 {
		return y.checkRules(t)
	}
	return y.checkAcks(t)
}

func (y *YAMLAdapter) runAfterSteps(t *BaseTrial, steps []yamlStep) error {
	vars := y.vars(t, "")
	log, err := os.OpenFile(filepath.Join(t.Dir(), "after.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer log.Close()
	for _, st := range steps {
		args := y.expandSlice(st.Command, vars)
		if len(args) == 0 {
			return fmt.Errorf("step %q: empty command", st.Name)
		}
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Stdout = log
		cmd.Stderr = log
		y.applyEnv(cmd, vars)
		if st.RunFor != "" {
			if err := cmd.Start(); err != nil {
				return err
			}
			time.Sleep(parseYAMLDuration(st.RunFor, time.Second))
			_ = signalNamed(cmd, st.Stop)
			cmd.Wait()
			continue
		}
		to := parseYAMLDuration(st.Timeout, stepTimeout)
		if err := runWithin(cmd, to); err != nil {
			return fmt.Errorf("step %q: %w", st.Name, err)
		}
	}
	return nil
}

func (y *YAMLAdapter) checkRules(t *BaseTrial) CheckResult {
	vars := y.vars(t, "")
	vals := map[string]any{}
	evidence := 0
	if y.cfg.Check.FileCount.Glob != "" {
		g := expandTemplates(y.cfg.Check.FileCount.Glob, vars)
		m, _ := filepath.Glob(g)
		n := len(m)
		name := y.cfg.Check.FileCount.As
		if name == "" {
			name = "files"
		}
		vals[name] = n
		evidence += n
	}
	if y.cfg.Check.RecordCount.Contains != "" {
		needle := expandTemplates(y.cfg.Check.RecordCount.Contains, vars)
		n := strings.Count(t.RecordContent(), needle)
		name := y.cfg.Check.RecordCount.As
		if name == "" {
			name = "matches"
		}
		vals[name] = n
		evidence += n
	}
	if len(y.cfg.Check.Command.Command) > 0 {
		args := y.expandSlice(y.cfg.Check.Command.Command, vars)
		cmd := exec.Command(args[0], args[1:]...)
		y.applyEnv(cmd, vars)
		out, err := cmd.CombinedOutput()
		s := string(out)
		name := y.cfg.Check.Command.As
		if name == "" {
			name = "stdout"
		}
		vals[name] = s
		if y.cfg.Check.Command.Contains != "" {
			want := expandTemplates(y.cfg.Check.Command.Contains, vars)
			vals[name+"_hits"] = strings.Count(s, want)
			if strings.Contains(s, want) {
				evidence++
			}
		} else if len(s) > 0 {
			evidence++
		}
		if err != nil {
			vals[name+"_err"] = err.Error()
		}
	}
	if evidence == 0 {
		evidence = 1
	}
	for _, r := range y.cfg.Check.Rules {
		if r.Else != "" && len(r.If) == 0 {
			return CheckResult{Verdict: r.Else, Evidence: evidence}
		}
		if matchYAMLRule(r.If, vals) {
			v := r.Verdict
			if v == "" {
				v = "FAIL"
			}
			return CheckResult{Verdict: v, Evidence: evidence}
		}
	}
	return CheckResult{Verdict: "FAIL", Evidence: evidence, Problems: []error{fmt.Errorf("no rule matched %v", vals)}}
}

func matchYAMLRule(ifm map[string]any, vals map[string]any) bool {
	if len(ifm) == 0 {
		return false
	}
	for k, want := range ifm {
		switch {
		case strings.HasSuffix(k, "_eq"):
			if asInt(vals[strings.TrimSuffix(k, "_eq")]) != asInt(want) {
				return false
			}
		case strings.HasSuffix(k, "_gte"):
			if asInt(vals[strings.TrimSuffix(k, "_gte")]) < asInt(want) {
				return false
			}
		case strings.HasSuffix(k, "_gt"):
			if asInt(vals[strings.TrimSuffix(k, "_gt")]) <= asInt(want) {
				return false
			}
		case strings.HasSuffix(k, "_lte"):
			if asInt(vals[strings.TrimSuffix(k, "_lte")]) > asInt(want) {
				return false
			}
		case strings.HasSuffix(k, "_lt"):
			if asInt(vals[strings.TrimSuffix(k, "_lt")]) >= asInt(want) {
				return false
			}
		case strings.HasSuffix(k, "_contains"):
			name := strings.TrimSuffix(k, "_contains")
			s, ok := stringVal(vals, name)
			if !ok {
				s, ok = stringVal(vals, name+"_out")
			}
			if !ok || !strings.Contains(s, fmt.Sprint(want)) {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func stringVal(vals map[string]any, name string) (string, bool) {
	v, ok := vals[name]
	if !ok || v == nil {
		return "", false
	}
	s, ok := v.(string)
	if ok {
		return s, true
	}
	return fmt.Sprint(v), true
}

func asInt(v any) int {
	switch x := v.(type) {
	case int:
		return x
	case int64:
		return int(x)
	case float64:
		return int(x)
	case string:
		n, _ := strconv.Atoi(x)
		return n
	default:
		n, _ := strconv.Atoi(fmt.Sprint(v))
		return n
	}
}

func (y *YAMLAdapter) checkAcks(t *BaseTrial) CheckResult {
	acked := parseAckKeys(t.RecordContent())
	if len(acked) == 0 {
		return CheckResult{Verdict: "UNKNOWN", Evidence: 0}
	}
	if y.cfg.Check.ForEach != "" && y.cfg.Check.ForEach != "ack" {
		return CheckResult{Verdict: "FAIL", Evidence: len(acked), Problems: []error{fmt.Errorf("check.for_each %q not supported", y.cfg.Check.ForEach)}}
	}
	if y.cfg.Check.HTTP.URL == "" {
		return CheckResult{Verdict: "FAIL", Evidence: len(acked), Problems: []error{fmt.Errorf("check.http.url is required for for_each ack")}}
	}

	t.Data()["workload"] = false
	log, err := os.Create(filepath.Join(t.Dir(), "check.log"))
	if err != nil {
		return CheckResult{Verdict: "FAIL", Evidence: len(acked), Problems: []error{fmt.Errorf("check log: %w", err)}}
	}
	defer log.Close()

	cmd, err := y.Start(t, log)
	if err != nil {
		return CheckResult{Verdict: "FAIL", Evidence: len(acked), Problems: []error{fmt.Errorf("restart: %w", err)}}
	}
	defer func() {
		if cmd.Process != nil {
			cmd.Process.Kill()
			cmd.Wait()
		}
	}()

	h := y.cfg.Check.HTTP
	method := h.Method
	if method == "" {
		method = http.MethodGet
	}
	want := h.WantStatus
	if want == 0 {
		want = http.StatusOK
	}
	client := &http.Client{Timeout: parseYAMLDuration(h.Timeout, 200*time.Millisecond)}
	var problems []error
	for _, key := range acked {
		vars := y.vars(t, "")
		vars["key"] = key
		url := expandTemplates(h.URL, vars)
		req, err := http.NewRequest(method, url, nil)
		if err != nil {
			problems = append(problems, err)
			continue
		}
		resp, err := client.Do(req)
		if err != nil {
			problems = append(problems, fmt.Errorf("GET %s: %w", key, err))
			continue
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != want {
			problems = append(problems, fmt.Errorf("key %q missing after restart (status %d)", key, resp.StatusCode))
		}
	}
	if len(problems) > 0 {
		v := y.cfg.Check.MissVerdict
		if v == "" {
			v = "FAIL"
		}
		return CheckResult{Verdict: v, Evidence: len(acked), Problems: problems}
	}
	return CheckResult{Verdict: "PASS", Evidence: len(acked)}
}

func parseAckKeys(rec string) []string {
	var keys []string
	for _, line := range strings.Split(rec, "\n") {
		if strings.HasPrefix(line, "ack ") {
			key := strings.TrimSpace(strings.TrimPrefix(line, "ack "))
			if key != "" {
				keys = append(keys, key)
			}
		}
	}
	return keys
}

func waitFirstAck(t *BaseTrial, d time.Duration) error {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if strings.Contains(t.RecordContent(), "ack ") {
			return nil
		}
		time.Sleep(5 * time.Millisecond)
	}
	return fmt.Errorf("no PUT returned 200")
}

func (y *YAMLAdapter) WaitBeforeKill(at AdapterTrial, seed int64) (time.Duration, bool, error) {
	t, ok := at.(*BaseTrial)
	if !ok {
		return 0, false, fmt.Errorf("expected *BaseTrial, got %T", at)
	}
	spec, ok := t.Data()["file_kill"].(yamlKillFile)
	if !ok {
		if y.cfg.Kill.At != "file" {
			return 0, false, nil
		}
		spec = y.cfg.Kill.File
	}
	vars := y.vars(t, "")
	start := time.Now()
	if err := waitFileContains(expandTemplates(spec.Glob, vars), expandTemplates(spec.Contains, vars), parseYAMLDuration(spec.Poll, time.Millisecond), parseYAMLDuration(spec.Timeout, 30*time.Second)); err != nil {
		return time.Since(start), true, err
	}
	if spec.OffsetMS > 0 {
		off := rand.New(rand.NewSource(seed)).Intn(spec.OffsetMS + 1)
		time.Sleep(time.Duration(off) * time.Millisecond)
	}
	return time.Since(start), true, nil
}

func waitFileContains(glob, needle string, poll, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		matches, _ := filepath.Glob(glob)
		for _, p := range matches {
			b, err := os.ReadFile(p)
			if err != nil {
				continue
			}
			if strings.Contains(string(b), needle) {
				return nil
			}
		}
		time.Sleep(poll)
	}
	return fmt.Errorf("no file matching %q contained %q in %v", glob, needle, timeout)
}

func (y *YAMLAdapter) RunControl(name string, seed int64, binaryPath string) error {
	ctl, ok := y.cfg.Controls[name]
	if !ok {
		return fmt.Errorf("unknown control %q", name)
	}
	dir, err := os.MkdirTemp("", fmt.Sprintf("fuss-ctl-%s-%d-", y.cfg.Name, seed))
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	t, err := y.Init(seed, dir, binaryPath)
	if err != nil {
		return err
	}
	t.Data()["workload"] = false
	if ctl.PlantDir != "" {
		src, err := y.resolvePlant(ctl.PlantDir)
		if err != nil {
			return err
		}
		if err := os.RemoveAll(t.Home()); err != nil {
			return err
		}
		if err := os.MkdirAll(t.Home(), 0o755); err != nil {
			return err
		}
		if err := os.CopyFS(t.Home(), os.DirFS(src)); err != nil {
			return fmt.Errorf("plant_dir %s: %w", src, err)
		}
	}
	if ctl.PlantRecord != "" {
		if err := appendToFile(t.Record(), ctl.PlantRecord); err != nil {
			return err
		}
	}
	cr := y.CheckNamed(t, true, "control")
	want := ctl.ExpectVerdict
	if want == "" {
		want = "FAIL"
	}
	if cr.Verdict == want {
		return nil
	}
	return fmt.Errorf("control BROKEN: staged state judged %s, want %s (%v)", cr.Verdict, want, cr.Problems)
}

func (y *YAMLAdapter) resolvePlant(p string) (string, error) {
	cands := []string{p, filepath.Join(filepath.Dir(y.path), p)}
	for _, c := range cands {
		st, err := os.Stat(c)
		if err == nil && st.IsDir() {
			abs, err := filepath.Abs(c)
			if err != nil {
				return "", err
			}
			return abs, nil
		}
	}
	return "", fmt.Errorf("plant_dir %q not found", p)
}

func (y *YAMLAdapter) Controls() []string {
	names := make([]string, 0, len(y.cfg.Controls))
	for n := range y.cfg.Controls {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}

func (y *YAMLAdapter) KillWindow() (minMS, maxMS int) {
	minMS = y.cfg.Kill.Delay.MinMS
	maxMS = y.cfg.Kill.Delay.MaxMS
	if maxMS <= minMS {
		return 0, 3000
	}
	return minMS, maxMS
}

type yamlStatus struct {
	State string
}

func (s yamlStatus) String() string  { return s.State }
func (s yamlStatus) IsRunning() bool { return s.State == "running" }
