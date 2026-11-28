package main

import (
	"encoding/json"
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
	Name       string                 `yaml:"name"`
	Binary     yamlBinary             `yaml:"binary"`
	Args       []string               `yaml:"args"`
	Env        map[string]string      `yaml:"env"`
	DataDir    string                 `yaml:"data_dir"`
	PortBase   int                    `yaml:"port_base"`
	PortMod    int                    `yaml:"port_mod"`
	Start      yamlStart              `yaml:"start"`
	Health     yamlHealth             `yaml:"health"`
	Workload   yamlWorkload           `yaml:"workload"`
	Kill       yamlKill               `yaml:"kill"`
	Restart    yamlRestart            `yaml:"restart"`
	Steps      []yamlStep             `yaml:"steps"`
	Topology   yamlTopology           `yaml:"topology"`
	Fault      yamlFault              `yaml:"fault"`
	Nemesis    yamlNemesis            `yaml:"nemesis"`
	Check      yamlCheck              `yaml:"check"`
	Properties yamlProperties         `yaml:"properties"`
	Controls   map[string]yamlControl `yaml:"controls"`
}

// yamlTopology starts N peer processes for one trial. Empty Nodes means no topology.
type yamlTopology struct {
	Nodes          int           `yaml:"nodes"`
	DataDir        string        `yaml:"data_dir"`
	Ports          yamlTopoPorts `yaml:"ports"`
	Peers          yamlTopoPeers `yaml:"peers"`
	ProxyMode      string        `yaml:"proxy_mode"` // "per_pair" (default), "per_node", or "per_node_owned"
	ClientViaProxy bool          `yaml:"client_via_proxy"`
	Start          []string      `yaml:"start"`
	Ready          yamlTopoReady `yaml:"ready"`
	StopGrace      string        `yaml:"stop_grace"`
}

// yamlTopoPeers routes peer traffic through Fuss proxy.Link when ViaProxy is set.
type yamlTopoPeers struct {
	PortName string `yaml:"port_name"` // which per-node port is the peer port (the topology ${port})
	ViaProxy bool   `yaml:"via_proxy"`
}

// yamlFaultIsolate is fault.isolate: a string "node_0", or {node, node_file, direction}.
// node_file is read when the trigger fires, so a scenario can name the target
// after it has seen which process is leader.
type yamlFaultIsolate struct {
	Node      string `yaml:"node"`
	NodeFile  string `yaml:"node_file"`
	Direction string `yaml:"direction"` // "both" (default) or "inbound"
}

func (f *yamlFaultIsolate) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind {
	case yaml.ScalarNode:
		f.Node = value.Value
		return nil
	case yaml.MappingNode:
		var raw struct {
			Node      string `yaml:"node"`
			NodeFile  string `yaml:"node_file"`
			Direction string `yaml:"direction"`
		}
		if err := value.Decode(&raw); err != nil {
			return err
		}
		f.Node = raw.Node
		f.NodeFile = raw.NodeFile
		f.Direction = raw.Direction
		return nil
	case yaml.AliasNode:
		if value.Alias == nil {
			return fmt.Errorf("fault.isolate: empty alias")
		}
		return f.UnmarshalYAML(value.Alias)
	default:
		return fmt.Errorf("fault.isolate: want string or {node, direction}")
	}
}

func (f yamlFaultIsolate) active() bool {
	return strings.TrimSpace(f.Node) != "" || strings.TrimSpace(f.NodeFile) != ""
}

// isolatePinsNode reports whether replay must cut the same node index.
// A node_file scenario names the leader at runtime, so a later run may cut
// a different index and still isolate the elected leader.
func (y *YAMLAdapter) isolatePinsNode() bool {
	if strings.TrimSpace(y.cfg.Fault.Isolate.NodeFile) != "" && strings.TrimSpace(y.cfg.Fault.Isolate.Node) == "" {
		return false
	}
	return true
}

func (f yamlFaultIsolate) direction() string {
	d := strings.TrimSpace(f.Direction)
	if d == "" {
		return "both"
	}
	return d
}

// yamlFaultPause is fault.pause: a string "node_0", or {node, resume}.
type yamlFaultPause struct {
	Node   string `yaml:"node"`
	Resume string `yaml:"resume"` // "end" (default) or "never"
}

func (f *yamlFaultPause) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind {
	case yaml.ScalarNode:
		f.Node = value.Value
		return nil
	case yaml.MappingNode:
		var raw struct {
			Node   string `yaml:"node"`
			Resume string `yaml:"resume"`
		}
		if err := value.Decode(&raw); err != nil {
			return err
		}
		f.Node = raw.Node
		f.Resume = raw.Resume
		return nil
	case yaml.AliasNode:
		if value.Alias == nil {
			return fmt.Errorf("fault.pause: empty alias")
		}
		return f.UnmarshalYAML(value.Alias)
	default:
		return fmt.Errorf("fault.pause: want string or {node, resume}")
	}
}

func (f yamlFaultPause) active() bool { return strings.TrimSpace(f.Node) != "" }

func (f yamlFaultPause) resume() string {
	r := strings.TrimSpace(f.Resume)
	if r == "" {
		return "end"
	}
	return r
}

// yamlFault injects a live fault while topology nodes keep running.
type yamlFault struct {
	Isolate yamlFaultIsolate `yaml:"isolate"`
	Pause   yamlFaultPause   `yaml:"pause"`
	After   yamlFaultAfter   `yaml:"after"`
}

type yamlFaultAfter struct {
	File yamlKillFile `yaml:"file"`
}

// yamlNemesis is a seeded sequence of faults. It replaces a single fault: block.
// Random draws use the trial *rand.Rand. direction chooses links; flow chooses
// the byte direction inside a link.
type yamlNemesis struct {
	SeedFromTrial bool                `yaml:"seed_from_trial"`
	Warmup        string              `yaml:"warmup"`
	Duration      string              `yaml:"duration"`
	Interval      string              `yaml:"interval"`
	QuietPeriod   string              `yaml:"quiet_period"`
	NodeFile      string              `yaml:"node_file"`
	Actions       []yamlNemesisAction `yaml:"actions"`

	warmup   time.Duration
	duration time.Duration
	interval time.Duration
	quiet    time.Duration
}

func (n yamlNemesis) active() bool { return len(n.Actions) > 0 }

// yamlNemesisAction is one weighted step. target is leader, random, or a node index.
type yamlNemesisAction struct {
	Type          string  `yaml:"type"`
	Target        string  `yaml:"target"`
	NodeFile      string  `yaml:"node_file"`
	Mode          string  `yaml:"mode"`
	Direction     string  `yaml:"direction"`
	Flow          string  `yaml:"flow"`
	Latency       string  `yaml:"latency"`
	Jitter        string  `yaml:"jitter"`
	DropRate      float64 `yaml:"drop_rate"`
	Weight        int     `yaml:"weight"`
	RestartDelay  string  `yaml:"restart_delay"`
	PauseDuration string  `yaml:"pause_duration"`

	latency      time.Duration
	jitter       time.Duration
	restartDelay time.Duration
	pauseFor     time.Duration
}

// NemesisEvent is one fired action. TimestampNS is nanoseconds since the
// schedule started, including warmup.
type NemesisEvent struct {
	TimestampNS int64    `json:"timestamp_ns"`
	Action      string   `json:"action"`
	Targets     []string `json:"targets,omitempty"`
	Mode        string   `json:"mode,omitempty"`
	Direction   string   `json:"direction,omitempty"`
	Flow        string   `json:"flow,omitempty"`
	Details     string   `json:"details,omitempty"`
}

type yamlTopoPorts struct {
	Base       int      `yaml:"base"`        // 0 means pick a free port per node
	ClientBase int      `yaml:"client_base"` // 0 means a free client port per node when client_via_proxy is set
	Names      []string `yaml:"names"`       // extra per-node ports; ${port} stays the original listen port
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
	Until        string           `yaml:"until"`
	WaitFirstAck bool             `yaml:"wait_first_ack"`
	HTTP         yamlHTTP         `yaml:"http"`
	Ack          yamlAck          `yaml:"ack"`
	Driver       string           `yaml:"driver"`
	Workers      int              `yaml:"workers"`
	Rate         float64          `yaml:"rate"`
	Keyspace     int              `yaml:"keyspace"`
	Timeout      string           `yaml:"timeout"`
	Targets      string           `yaml:"targets"`
	Put          yamlKVCall       `yaml:"put"`
	Get          yamlKVCall       `yaml:"get"`
	Cas          yamlKVCall       `yaml:"cas"`
	Exec         yamlExecWorkload `yaml:"exec"`
	timeout      time.Duration
}

type yamlKVCall struct {
	Method        string            `yaml:"method"`
	URL           string            `yaml:"url"`
	Body          string            `yaml:"body"`
	Headers       map[string]string `yaml:"headers"`
	ValueFrom     string            `yaml:"value_from"`
	ValueEncoding string            `yaml:"value_encoding"`
	ErrorFrom     string            `yaml:"error_from"`
}

type yamlExecWorkload struct {
	Command []string `yaml:"command"`
	Put     []string `yaml:"put"`
	Get     []string `yaml:"get"`
	Cas     []string `yaml:"cas"`
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
	Delay    string   `yaml:"delay"`
	RunFor   string   `yaml:"run_for"`
	Stop     string   `yaml:"stop"`
	Kill     yamlKill `yaml:"kill"`
}

type yamlCheck struct {
	ForEach     string               `yaml:"for_each"`
	HTTP        yamlHTTP             `yaml:"http"`
	MissVerdict string               `yaml:"miss_verdict"`
	FileCount   yamlFileCount        `yaml:"file_count"`
	RecordCount yamlRecCount         `yaml:"record_count"`
	Command     yamlCmdCheck         `yaml:"command"`
	Rules       []yamlRule           `yaml:"rules"`
	Porcupine   *yamlPorcupineCheck  `yaml:"porcupine"`
	Set         *yamlSetCheck        `yaml:"set"`
	Eventually  *yamlEventuallyCheck `yaml:"eventually_available"`
}

// yamlProperties is optional coverage next to check:. always and eventually
// must pass on every trial. sometimes is counted across the -n run.
type yamlProperties struct {
	Always     []yamlProperty `yaml:"always"`
	Eventually []yamlProperty `yaml:"eventually"`
	Sometimes  []yamlProperty `yaml:"sometimes"`
}

type yamlProperty struct {
	Name                string `yaml:"name"`
	MinHits             int    `yaml:"min_hits"`
	yamlCheck           `yaml:",inline"`
	FaultDuringInflight *yamlFlag             `yaml:"fault_during_inflight"`
	InfoOpsObserved     *yamlFlag             `yaml:"info_ops_observed"`
	FaultActionFired    *yamlFaultActionFired `yaml:"fault_action_fired"`
	LeaderChanged       *yamlLeaderChanged    `yaml:"leader_changed"`
}

// yamlFlag is present when the key is set to {} or true.
type yamlFlag struct{ on bool }

func (f *yamlFlag) UnmarshalYAML(n *yaml.Node) error {
	f.on = true
	if n.Kind == yaml.ScalarNode {
		switch strings.ToLower(strings.TrimSpace(n.Value)) {
		case "false", "no", "0":
			f.on = false
		}
	}
	return nil
}

func (f *yamlFlag) enabled() bool { return f != nil && f.on }

type yamlFaultActionFired struct {
	Action string `yaml:"action"`
}

type yamlLeaderChanged struct {
	NodeFile string `yaml:"node_file"`
}

type yamlPorcupineCheck struct {
	Timeout string `yaml:"timeout"`
	timeout time.Duration
}

type yamlSetCheck struct{}

type yamlEventuallyCheck struct {
	N int `yaml:"n"`
}

type yamlFileCount struct {
	Glob      string `yaml:"glob"`
	As        string `yaml:"as"`
	Contains  string `yaml:"contains"`
	JSONField string `yaml:"json_field"`
	Equals    string `yaml:"equals"`
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
	mode := strings.TrimSpace(cfg.Topology.ProxyMode)
	if mode == "" {
		mode = "per_pair"
	}
	switch mode {
	case "per_pair", "per_node", "per_node_owned":
	default:
		return nil, fmt.Errorf("target-config %s: topology.proxy_mode %q not supported (per_pair, per_node, or per_node_owned)", path, cfg.Topology.ProxyMode)
	}
	if (mode == "per_node" || mode == "per_node_owned") && !cfg.Topology.Peers.ViaProxy {
		return nil, fmt.Errorf("target-config %s: topology.proxy_mode %s requires topology.peers.via_proxy", path, mode)
	}
	cfg.Topology.ProxyMode = mode
	if cfg.Topology.ClientViaProxy && cfg.Topology.Nodes > 0 && cfg.Topology.Ports.Base > 0 && cfg.Topology.Ports.ClientBase > 0 {
		if portRangesOverlap(cfg.Topology.Ports.Base, cfg.Topology.Ports.ClientBase, cfg.Topology.Nodes) {
			return nil, fmt.Errorf("target-config %s: topology.ports.base and topology.ports.client_base ranges overlap", path)
		}
	}

	dir := cfg.Fault.Isolate.direction()
	switch dir {
	case "both", "inbound":
	default:
		return nil, fmt.Errorf("target-config %s: fault.isolate direction %q not supported (both or inbound)", path, cfg.Fault.Isolate.Direction)
	}
	cfg.Fault.Isolate.Direction = dir
	if mode == "per_node" && cfg.Fault.Isolate.active() && dir == "both" {
		return nil, fmt.Errorf("target-config %s: topology.proxy_mode per_node cannot use fault.isolate direction both; per_node proxies can't separate one node's outbound links (use direction: inbound)", path)
	}
	if strings.TrimSpace(cfg.Fault.Isolate.Node) != "" && strings.TrimSpace(cfg.Fault.Isolate.NodeFile) != "" {
		return nil, fmt.Errorf("target-config %s: fault.isolate accepts node or node_file, not both", path)
	}
	if cfg.Fault.Isolate.active() && strings.TrimSpace(cfg.Fault.Isolate.Node) == "" && strings.TrimSpace(cfg.Fault.Isolate.NodeFile) == "" {
		return nil, fmt.Errorf("target-config %s: fault.isolate.node or fault.isolate.node_file is required", path)
	}
	resume := cfg.Fault.Pause.resume()
	switch resume {
	case "end", "never":
	default:
		return nil, fmt.Errorf("target-config %s: fault.pause resume %q not supported (end or never)", path, cfg.Fault.Pause.Resume)
	}
	cfg.Fault.Pause.Resume = resume
	if cfg.Fault.Pause.active() && cfg.Fault.Pause.Node == "" {
		return nil, fmt.Errorf("target-config %s: fault.pause.node is required", path)
	}
	for _, st := range cfg.Steps {
		if strings.TrimSpace(st.Delay) == "" {
			continue
		}
		if _, err := time.ParseDuration(st.Delay); err != nil {
			return nil, fmt.Errorf("target-config %s: steps[%q].delay %q: %w", path, st.Name, st.Delay, err)
		}
	}
	if err := validateNemesis(path, &cfg); err != nil {
		return nil, err
	}
	if err := validateWorkload(path, &cfg); err != nil {
		return nil, err
	}
	if err := validateProperties(path, &cfg); err != nil {
		return nil, err
	}
	return &YAMLAdapter{path: path, cfg: cfg}, nil
}

func validateNemesis(path string, cfg *yamlTarget) error {
	n := &cfg.Nemesis
	if !n.active() {
		return nil
	}
	if cfg.Fault.Isolate.active() || cfg.Fault.Pause.active() {
		return fmt.Errorf("target-config %s: nemesis and fault are mutually exclusive", path)
	}
	if cfg.Kill.At == "file" {
		return fmt.Errorf("target-config %s: nemesis and kill.at file are mutually exclusive", path)
	}
	if cfg.Topology.Nodes <= 0 {
		return fmt.Errorf("target-config %s: nemesis requires topology.nodes", path)
	}
	var err error
	if n.warmup, err = parseNemesisDuration(path, "warmup", n.Warmup, true); err != nil {
		return err
	}
	if n.duration, err = parseNemesisDuration(path, "duration", n.Duration, false); err != nil {
		return err
	}
	if n.interval, err = parseNemesisDuration(path, "interval", n.Interval, false); err != nil {
		return err
	}
	if n.quiet, err = parseNemesisDuration(path, "quiet_period", n.QuietPeriod, true); err != nil {
		return err
	}
	for i := range n.Actions {
		if err := validateNemesisAction(path, cfg, &n.Actions[i]); err != nil {
			return err
		}
	}
	return nil
}

func validateProperties(path string, cfg *yamlTarget) error {
	p := &cfg.Properties
	if len(p.Always) == 0 && len(p.Eventually) == 0 && len(p.Sometimes) == 0 {
		return nil
	}
	seen := map[string]bool{}
	claim := func(name string) error {
		if seen[name] {
			return fmt.Errorf("target-config %s: properties name %q is duplicated", path, name)
		}
		seen[name] = true
		return nil
	}
	for i := range p.Always {
		prop := &p.Always[i]
		prop.Name = prop.defaultName("always")
		if err := claim(prop.Name); err != nil {
			return err
		}
		if prop.sometimesEvaluators() > 0 {
			return fmt.Errorf("target-config %s: properties.always %q cannot use a sometimes evaluator", path, prop.Name)
		}
		if prop.Eventually != nil {
			return fmt.Errorf("target-config %s: properties.always %q: eventually_available belongs under properties.eventually", path, prop.Name)
		}
		if !prop.hasAlwaysChecker() {
			return fmt.Errorf("target-config %s: properties.always %q needs porcupine, set, or a rule check", path, prop.Name)
		}
		if prop.hasRuleCheck() && (prop.Porcupine != nil || prop.Set != nil) {
			return fmt.Errorf("target-config %s: properties.always %q mixes a rule check with porcupine or set", path, prop.Name)
		}
		if err := prop.parsePorcupineTimeout(path, "properties.always"); err != nil {
			return err
		}
	}
	for i := range p.Eventually {
		prop := &p.Eventually[i]
		prop.Name = prop.defaultName("eventually")
		if err := claim(prop.Name); err != nil {
			return err
		}
		if prop.Eventually == nil || prop.Porcupine != nil || prop.Set != nil || prop.hasRuleCheck() || prop.sometimesEvaluators() > 0 {
			return fmt.Errorf("target-config %s: properties.eventually %q needs only eventually_available", path, prop.Name)
		}
	}
	for i := range p.Sometimes {
		prop := &p.Sometimes[i]
		if prop.MinHits < 0 {
			return fmt.Errorf("target-config %s: properties.sometimes min_hits must be >= 0", path)
		}
		if prop.MinHits == 0 {
			prop.MinHits = 1
		}
		prop.Name = prop.defaultName("sometimes")
		if err := claim(prop.Name); err != nil {
			return err
		}
		if prop.sometimesEvaluators() != 1 {
			return fmt.Errorf("target-config %s: properties.sometimes %q needs exactly one of fault_during_inflight, info_ops_observed, fault_action_fired, leader_changed", path, prop.Name)
		}
		if prop.hasAlwaysChecker() || prop.Eventually != nil {
			return fmt.Errorf("target-config %s: properties.sometimes %q cannot include a check", path, prop.Name)
		}
		if prop.FaultActionFired != nil && !knownNemesisAction(prop.FaultActionFired.Action) {
			return fmt.Errorf("target-config %s: properties.sometimes %q fault_action_fired.action %q is not a nemesis action", path, prop.Name, prop.FaultActionFired.Action)
		}
		if prop.LeaderChanged != nil && strings.TrimSpace(prop.LeaderChanged.NodeFile) == "" {
			return fmt.Errorf("target-config %s: properties.sometimes %q leader_changed.node_file is required", path, prop.Name)
		}
	}
	needsHistory, needsRead := false, false
	for _, prop := range append(append([]yamlProperty{}, p.Always...), p.Eventually...) {
		if prop.Porcupine != nil || prop.Set != nil || prop.Eventually != nil {
			needsHistory = true
		}
		if prop.Porcupine != nil || prop.Set != nil {
			needsRead = true
		}
	}
	if needsHistory && strings.TrimSpace(cfg.Workload.Driver) == "" {
		return fmt.Errorf("target-config %s: properties porcupine, set, and eventually_available require workload.driver", path)
	}
	if needsRead && cfg.Workload.Driver == "http_kv" && cfg.Workload.Get.URL == "" {
		return fmt.Errorf("target-config %s: properties porcupine and set need workload.get", path)
	}
	if needsRead && cfg.Workload.Driver == "exec_op" && len(cfg.Workload.Exec.Get) == 0 && len(cfg.Workload.Exec.Command) == 0 {
		return fmt.Errorf("target-config %s: properties porcupine and set need workload.exec.get", path)
	}
	return nil
}

func knownNemesisAction(action string) bool {
	switch action {
	case "isolate_node", "isolate_client", "partition_halves", "kill_node", "pause_node", "heal_all":
		return true
	default:
		return false
	}
}

func (p yamlProperty) hasRuleCheck() bool {
	return len(p.Rules) > 0 || p.FileCount.Glob != "" || p.RecordCount.Contains != "" || len(p.Command.Command) > 0
}

func (p yamlProperty) hasAlwaysChecker() bool {
	return p.Porcupine != nil || p.Set != nil || p.hasRuleCheck()
}

func (p yamlProperty) sometimesEvaluators() int {
	n := 0
	if p.FaultDuringInflight.enabled() {
		n++
	}
	if p.InfoOpsObserved.enabled() {
		n++
	}
	if p.FaultActionFired != nil {
		n++
	}
	if p.LeaderChanged != nil {
		n++
	}
	return n
}

func (p yamlProperty) faultAssertion() bool {
	return p.FaultDuringInflight.enabled() || p.FaultActionFired != nil
}

func (p yamlProperty) defaultName(kind string) string {
	if name := strings.TrimSpace(p.Name); name != "" {
		return name
	}
	switch {
	case p.Porcupine != nil:
		return "porcupine"
	case p.Set != nil:
		return "set"
	case p.Eventually != nil:
		return "eventually_available"
	case p.hasRuleCheck():
		return "rules"
	case p.FaultDuringInflight.enabled():
		return "fault_during_inflight"
	case p.InfoOpsObserved.enabled():
		return "info_ops_observed"
	case p.FaultActionFired != nil && p.FaultActionFired.Action != "":
		return "fault_action_fired_" + p.FaultActionFired.Action
	case p.LeaderChanged != nil:
		return "leader_changed"
	default:
		return kind
	}
}

func (p *yamlProperty) parsePorcupineTimeout(path, where string) error {
	if p.Porcupine == nil {
		return nil
	}
	raw := strings.TrimSpace(p.Porcupine.Timeout)
	if raw == "" {
		p.Porcupine.timeout = 5 * time.Second
		return nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < 0 {
		return fmt.Errorf("target-config %s: %s %q porcupine.timeout %q", path, where, p.Name, p.Porcupine.Timeout)
	}
	p.Porcupine.timeout = d
	return nil
}

func parseNemesisDuration(path, field, raw string, allowEmpty bool) (time.Duration, error) {
	if strings.TrimSpace(raw) == "" {
		if allowEmpty {
			return 0, nil
		}
		return 0, fmt.Errorf("target-config %s: nemesis.%s is required", path, field)
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("target-config %s: nemesis.%s %q: %w", path, field, raw, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("target-config %s: nemesis.%s must be > 0", path, field)
	}
	return d, nil
}

func validateNemesisAction(path string, cfg *yamlTarget, a *yamlNemesisAction) error {
	switch a.Type {
	case "isolate_node", "isolate_client", "partition_halves", "kill_node", "pause_node", "heal_all":
	default:
		return fmt.Errorf("target-config %s: nemesis action type %q is not supported", path, a.Type)
	}
	if a.Weight <= 0 {
		a.Weight = 1
	}
	if a.Type == "isolate_client" && !cfg.Topology.ClientViaProxy {
		return fmt.Errorf("target-config %s: isolate_client requires topology.client_via_proxy", path)
	}
	needsMode := a.Type == "isolate_node" || a.Type == "isolate_client" || a.Type == "partition_halves"
	if needsMode {
		switch a.Mode {
		case "reset", "blackhole", "degraded":
		default:
			return fmt.Errorf("target-config %s: nemesis %s mode %q not supported (reset, blackhole, or degraded)", path, a.Type, a.Mode)
		}
	}
	if a.Direction == "" {
		if a.Type == "isolate_node" || a.Type == "partition_halves" {
			a.Direction = "both"
		}
	} else {
		switch a.Direction {
		case "both", "inbound", "outbound":
		default:
			return fmt.Errorf("target-config %s: nemesis direction %q not supported (both, inbound, or outbound)", path, a.Direction)
		}
	}
	if a.Flow == "" {
		if needsMode {
			a.Flow = "both"
		}
	} else {
		switch a.Flow {
		case "both", "a_to_b", "b_to_a":
		default:
			return fmt.Errorf("target-config %s: nemesis flow %q not supported (both, a_to_b, or b_to_a)", path, a.Flow)
		}
	}
	if cfg.Topology.ProxyMode == "per_node" && a.Type == "isolate_node" && a.Direction != "inbound" {
		return fmt.Errorf("target-config %s: topology.proxy_mode per_node nemesis isolate_node requires direction inbound", path)
	}
	if a.DropRate < 0 || a.DropRate > 1 {
		return fmt.Errorf("target-config %s: nemesis drop_rate %v not in [0,1]", path, a.DropRate)
	}
	var err error
	if a.latency, err = optionalNemesisDuration(path, "latency", a.Latency); err != nil {
		return err
	}
	if a.jitter, err = optionalNemesisDuration(path, "jitter", a.Jitter); err != nil {
		return err
	}
	if a.Type == "kill_node" {
		if a.restartDelay, err = optionalNemesisDuration(path, "restart_delay", a.RestartDelay); err != nil {
			return err
		}
	}
	if a.Type == "pause_node" {
		if strings.TrimSpace(a.PauseDuration) == "" {
			return fmt.Errorf("target-config %s: pause_node requires pause_duration", path)
		}
		if a.pauseFor, err = optionalNemesisDuration(path, "pause_duration", a.PauseDuration); err != nil {
			return err
		}
		if a.pauseFor <= 0 {
			return fmt.Errorf("target-config %s: pause_duration must be > 0", path)
		}
	}
	switch a.Type {
	case "isolate_node", "isolate_client", "kill_node", "pause_node":
		if err := validateNemesisTarget(path, cfg, a); err != nil {
			return err
		}
	}
	return nil
}

func optionalNemesisDuration(path, field, raw string) (time.Duration, error) {
	if strings.TrimSpace(raw) == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("target-config %s: nemesis %s %q: %w", path, field, raw, err)
	}
	if d < 0 {
		return 0, fmt.Errorf("target-config %s: nemesis %s must be >= 0", path, field)
	}
	return d, nil
}

func validateNemesisTarget(path string, cfg *yamlTarget, a *yamlNemesisAction) error {
	switch strings.TrimSpace(a.Target) {
	case "random":
		return nil
	case "leader":
		if strings.TrimSpace(a.NodeFile) == "" && strings.TrimSpace(cfg.Nemesis.NodeFile) == "" {
			return fmt.Errorf("target-config %s: nemesis target leader requires node_file", path)
		}
		return nil
	case "":
		return fmt.Errorf("target-config %s: nemesis %s requires target", path, a.Type)
	default:
		idx, err := parseNodeRef(a.Target)
		if err != nil {
			return fmt.Errorf("target-config %s: nemesis target %q: %w", path, a.Target, err)
		}
		if idx < 0 || idx >= cfg.Topology.Nodes {
			return fmt.Errorf("target-config %s: nemesis target %q is outside topology.nodes", path, a.Target)
		}
		return nil
	}
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
		"name":   y.cfg.Name,
	}
	if y.cfg.Workload.Ack.Key != "" {
		m["key"] = expandTemplates(y.cfg.Workload.Ack.Key, m)
	} else if n != "" {
		m["key"] = m["seed"] + "-" + n
	}
	return m
}

func expandTemplates(s string, vars map[string]string) string {
	keys := []string{"run_id", "binary", "home", "addr", "seed", "dir", "key", "n", "i", "port", "data_dir", "peers", "peers_via_proxy", "name", "client_port", "client_proxy", "client_addrs"}
	for _, k := range keys {
		s = strings.ReplaceAll(s, "${"+k+"}", vars[k])
	}
	for k, v := range vars {
		if strings.HasPrefix(k, "peer_addr_") || strings.HasPrefix(k, "node_proxy_addr_") || strings.HasPrefix(k, "client_addr_") || strings.HasPrefix(k, "port_") || strings.HasPrefix(k, "addr_") {
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
		if st.Delay != "" {
			// Validated non-empty at load; zero only if empty (skipped above).
			time.Sleep(mustYAMLDuration(st.Delay))
		}
		args := y.expandSlice(st.Command, vars)
		if len(args) == 0 {
			if st.Delay != "" {
				continue
			}
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
	args       []string
	env        []string
	gen        int
	pauseGen   int
	dead       bool
	paused     bool
	restarting bool
}

// topologyLink is one directed proxy.Link from node From to node To's peer port.
type topologyLink struct {
	From        int
	To          int
	Link        *proxy.Link
	Listen      string
	Upstream    string
	StatsBefore proxy.Stats
	StatsAfter  proxy.Stats
	tagBefore   map[int]proxy.Stats // per source, snapshotted at cut
	CutTime     time.Time
	cut         bool
	cutFrom     map[int]struct{} // per_node_owned: sources cut on this inbound proxy
	Kind        string           // "client" for a client-port proxy; empty for a peer proxy
}

// unknownOwner is a connection whose socket owner was not one of the topology PIDs.
type unknownOwner struct {
	To     int
	Listen string
	Remote string
	Cut    bool
}

type isolateState struct {
	Target       string
	Direction    string
	TriggerFired bool
	CutTime      time.Time
	Done         bool   // waiter finished (fired or timed out)
	Fault        string // set when the trigger fired but no node could be cut
}

type pauseState struct {
	Target       string
	Resume       string // "end" or "never"
	TriggerFired bool
	PauseTime    time.Time
	Paused       bool // SIGSTOP was sent
	Done         bool // waiter finished (fired or timed out)
}

type topologyRuntime struct {
	mu        sync.Mutex
	ProxyMode string
	Nodes     []*topologyNode
	Links     []*topologyLink
	// ClientLinks are proxies in front of each node's client port when
	// client_via_proxy is set. Peer isolate does not cut them; a nemesis can.
	ClientLinks   []*topologyLink
	ClientPorts   []int
	ClientProxies []string
	// PeerAddrs and NodeProxies are the addresses start scripts and ready
	// probes expand. The post-quiet recheck has to see them too.
	PeerAddrs   [][]string
	NodeProxies []string
	// NamedPorts is port_<name> for each node. The quiet recheck expands
	// ready commands with the same names the start command saw.
	NamedPorts map[string][]int
	Unknown    []unknownOwner
	Isolate    *isolateState
	Pause      *pauseState
	Nemesis    *nemesisRun
	log        io.Writer
	AllReady   bool
	Stopped    bool
}

// nemesisRun is the schedule for one trial: the event log and the recovery clock.
type nemesisRun struct {
	Events        []NemesisEvent
	Done          bool
	ReadyOK       bool
	Err           string
	QuietStarted  time.Time
	QuietFinished time.Time
	QuietStartNS  int64
	QuietEndNS    int64
	CheckStarted  time.Time
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
	if rt.ProxyMode == "per_node_owned" {
		return rt.ownedIsolateEvidence()
	}
	links := make([]map[string]any, 0, len(rt.Links))
	cutN := 0
	total := 0
	for _, l := range rt.Links {
		if l == nil || l.Link == nil {
			continue
		}
		total++
		after := l.StatsAfter
		if l.cut {
			cutN++
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
	dir := rt.Isolate.Direction
	if dir == "" {
		dir = "both"
	}
	mode := rt.ProxyMode
	if mode == "" {
		mode = "per_pair"
	}
	out := map[string]any{
		"target":        rt.Isolate.Target,
		"trigger_fired": rt.Isolate.TriggerFired,
		"proxy_mode":    mode,
		"direction":     dir,
		"links_cut":     cutN,
		"links_total":   total,
		"links":         links,
	}
	if !rt.Isolate.CutTime.IsZero() {
		out["cut_time"] = rt.Isolate.CutTime.UTC().Format(time.RFC3339Nano)
	}
	return out
}

// ownedIsolateEvidence reports directed pairs, not the shared inbound proxies.
// Caller holds rt.mu.
func (rt *topologyRuntime) ownedIsolateEvidence() map[string]any {
	n := len(rt.Nodes)
	byTo := make(map[int]*topologyLink, len(rt.Links))
	for _, l := range rt.Links {
		if l == nil || l.Link == nil {
			continue
		}
		byTo[l.To] = l
	}
	total := n * (n - 1)
	if total < 0 {
		total = 0
	}
	links := make([]map[string]any, 0, total)
	cutN := 0
	for from := 0; from < n; from++ {
		for to := 0; to < n; to++ {
			if from == to {
				continue
			}
			phys := byTo[to]
			cut := false
			listen := ""
			var before, after proxy.Stats
			var cutTime time.Time
			if phys != nil {
				listen = phys.Listen
				if phys.tagBefore != nil {
					before = phys.tagBefore[from]
				}
				if phys.cut {
					cut = true
				}
				if _, ok := phys.cutFrom[from]; ok {
					cut = true
				}
				if phys.cut || len(phys.cutFrom) > 0 {
					after = phys.Link.TagStats(from)
				} else {
					after = before
				}
				if cut {
					cutTime = phys.CutTime
					cutN++
				}
			}
			m := map[string]any{
				"from":         from,
				"to":           to,
				"listen":       listen,
				"cut":          cut,
				"stats_before": statsMap(before),
				"stats_after":  statsMap(after),
			}
			if !cutTime.IsZero() {
				m["cut_time"] = cutTime.UTC().Format(time.RFC3339Nano)
			}
			links = append(links, m)
		}
	}
	dir := rt.Isolate.Direction
	if dir == "" {
		dir = "both"
	}
	unk := make([]map[string]any, 0, len(rt.Unknown))
	for _, u := range rt.Unknown {
		unk = append(unk, map[string]any{
			"to":     u.To,
			"listen": u.Listen,
			"remote": u.Remote,
			"cut":    u.Cut,
		})
	}
	out := map[string]any{
		"target":         rt.Isolate.Target,
		"trigger_fired":  rt.Isolate.TriggerFired,
		"proxy_mode":     "per_node_owned",
		"direction":      dir,
		"links_cut":      cutN,
		"links_total":    total,
		"links":          links,
		"unknown_owners": unk,
	}
	if !rt.Isolate.CutTime.IsZero() {
		out["cut_time"] = rt.Isolate.CutTime.UTC().Format(time.RFC3339Nano)
	}
	return out
}

func (rt *topologyRuntime) pauseEvidence() map[string]any {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.Pause == nil {
		return nil
	}
	out := map[string]any{
		"target":        rt.Pause.Target,
		"resume":        rt.Pause.Resume,
		"trigger_fired": rt.Pause.TriggerFired,
		"paused":        rt.Pause.Paused,
	}
	if !rt.Pause.PauseTime.IsZero() {
		out["pause_time"] = rt.Pause.PauseTime.UTC().Format(time.RFC3339Nano)
	}
	return out
}

func (rt *topologyRuntime) cutObserved() bool {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	for _, l := range rt.Links {
		if l == nil || l.Link == nil || (!l.cut && len(l.cutFrom) == 0) {
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

var (
	reservedPortMu sync.Mutex
	reservedPorts  = map[int]struct{}{}
)

func portRangesOverlap(a, b, n int) bool {
	if n <= 0 {
		return false
	}
	a2 := a + n - 1
	b2 := b + n - 1
	return a <= b2 && b <= a2
}

// freePort asks the kernel for an ephemeral port and remembers it for this
// process. Two topologies started together therefore do not receive the same
// port after the listener is closed.
func freePort() (int, error) {
	reservedPortMu.Lock()
	defer reservedPortMu.Unlock()
	var last error
	for try := 0; try < 64; try++ {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return 0, err
		}
		p := ln.Addr().(*net.TCPAddr).Port
		ln.Close()
		if _, ok := reservedPorts[p]; ok {
			last = fmt.Errorf("port %d already reserved", p)
			continue
		}
		reservedPorts[p] = struct{}{}
		return p, nil
	}
	if last == nil {
		last = fmt.Errorf("no free port")
	}
	return 0, last
}

func validPortName(name string) bool {
	if name == "" || name == "port" {
		return false
	}
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
		case i > 0 && (r == '_' || (r >= '0' && r <= '9')):
		default:
			return false
		}
	}
	return true
}

func (y *YAMLAdapter) allocateNodePorts(n int) ([]int, map[string][]int, error) {
	seen := map[string]bool{}
	for _, name := range y.cfg.Topology.Ports.Names {
		if !validPortName(name) {
			return nil, nil, fmt.Errorf("topology.ports.names: invalid name %q", name)
		}
		if seen[name] {
			return nil, nil, fmt.Errorf("topology.ports.names: duplicate %q", name)
		}
		seen[name] = true
	}
	legacy := make([]int, n)
	named := map[string][]int{}
	for _, name := range y.cfg.Topology.Ports.Names {
		named[name] = make([]int, n)
	}
	for i := 0; i < n; i++ {
		p, err := y.onePort(n, i, 0)
		if err != nil {
			return nil, nil, err
		}
		legacy[i] = p
		for slot, name := range y.cfg.Topology.Ports.Names {
			p, err := y.onePort(n, i, slot+1)
			if err != nil {
				return nil, nil, err
			}
			named[name][i] = p
		}
	}
	return legacy, named, nil
}

func (y *YAMLAdapter) onePort(n, i, slot int) (int, error) {
	base := y.cfg.Topology.Ports.Base
	if base == 0 {
		p, err := freePort()
		if err != nil {
			return 0, fmt.Errorf("topology: free port: %w", err)
		}
		return p, nil
	}
	return base + n*slot + i, nil
}

func setNodePortVars(vars map[string]string, port int, named map[string][]int, i int) {
	vars["port"] = strconv.Itoa(port)
	for name, row := range named {
		p := strconv.Itoa(row[i])
		vars["port_"+name] = p
		vars["addr_"+name] = "127.0.0.1:" + p
	}
}

func peerUpstreamPort(portName string, legacy []int, named map[string][]int, j int) int {
	if row, ok := named[portName]; ok {
		return row[j]
	}
	return legacy[j]
}

func (y *YAMLAdapter) startTopology(t *BaseTrial, log *os.File) (*exec.Cmd, error) {
	topo := y.cfg.Topology
	if len(topo.Start) == 0 {
		return nil, fmt.Errorf("topology.start is required when topology.nodes > 0")
	}
	n := topo.Nodes
	ports, namedPorts, err := y.allocateNodePorts(n)
	if err != nil {
		return nil, err
	}

	rt := &topologyRuntime{Nodes: make([]*topologyNode, n), ProxyMode: topo.ProxyMode, log: log, NamedPorts: namedPorts}
	peerAddrs := make([][]string, n) // peerAddrs[i][j] = addr node i uses to reach j
	var nodeProxies []string         // per_node: inbound proxy listen for node k
	var clientPorts []int
	var clientProxies []string
	if topo.Peers.ViaProxy {
		for i := 0; i < n; i++ {
			peerAddrs[i] = make([]string, n)
		}
		switch topo.ProxyMode {
		case "per_node", "per_node_owned":
			nodeProxies = make([]string, n)
			for j := 0; j < n; j++ {
				upstream := "127.0.0.1:" + strconv.Itoa(peerUpstreamPort(topo.Peers.PortName, ports, namedPorts, j))
				link, err := proxy.New("127.0.0.1:0", upstream)
				if err != nil {
					y.stopTopology(rt)
					return nil, fmt.Errorf("topology proxy node %d: %w", j, err)
				}
				if err := link.Start(); err != nil {
					y.stopTopology(rt)
					return nil, fmt.Errorf("topology proxy node %d start: %w", j, err)
				}
				addr := link.Addr().String()
				// From=-1: shared inbound proxy; every peer dials this listen addr.
				rt.Links = append(rt.Links, &topologyLink{From: -1, To: j, Link: link, Listen: addr, Upstream: upstream})
				nodeProxies[j] = addr
			}
			for i := 0; i < n; i++ {
				for j := 0; j < n; j++ {
					if i == j {
						continue
					}
					peerAddrs[i][j] = nodeProxies[j]
				}
			}
		default: // per_pair
			for i := 0; i < n; i++ {
				for j := 0; j < n; j++ {
					if i == j {
						continue
					}
					upstream := "127.0.0.1:" + strconv.Itoa(peerUpstreamPort(topo.Peers.PortName, ports, namedPorts, j))
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
					tl := &topologyLink{From: i, To: j, Link: link, Listen: addr, Upstream: upstream}
					rt.Links = append(rt.Links, tl)
					peerAddrs[i][j] = addr
				}
			}
		}
	}
	if topo.ClientViaProxy {
		clientPorts = make([]int, n)
		clientProxies = make([]string, n)
		for i := 0; i < n; i++ {
			p := topo.Ports.ClientBase + i
			if topo.Ports.ClientBase == 0 {
				fp, err := freePort()
				if err != nil {
					y.stopTopology(rt)
					return nil, fmt.Errorf("topology: free client port: %w", err)
				}
				p = fp
			}
			clientPorts[i] = p
			upstream := "127.0.0.1:" + strconv.Itoa(p)
			link, err := proxy.New("127.0.0.1:0", upstream)
			if err != nil {
				y.stopTopology(rt)
				return nil, fmt.Errorf("topology client proxy %d: %w", i, err)
			}
			tl := &topologyLink{From: -1, To: i, Kind: "client", Link: link}
			rt.ClientLinks = append(rt.ClientLinks, tl)
			if err := link.Start(); err != nil {
				y.stopTopology(rt)
				return nil, fmt.Errorf("topology client proxy %d start: %w", i, err)
			}
			tl.Listen = link.Addr().String()
			clientProxies[i] = tl.Listen
		}
		rt.ClientPorts = clientPorts
		rt.ClientProxies = clientProxies
	}
	rt.PeerAddrs = peerAddrs
	rt.NodeProxies = nodeProxies

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
			setNodePortVars(vars, ports[i], namedPorts, i)
			y.addPeerVars(vars, peerAddrs, nodeProxies, i)
			y.addClientVars(vars, clientPorts, clientProxies, i)
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
			trackProc(cmd)
			done := make(chan struct{})
			node := &topologyNode{
				Index:     i,
				PID:       cmd.Process.Pid,
				Port:      ports[i],
				DataDir:   dataDir,
				StartTime: startTime,
				cmd:       cmd,
				waitDone:  done,
				args:      append([]string(nil), args...),
				env:       append([]string(nil), cmd.Env...),
			}
			go y.watchCmd(rt, node, cmd, done, node.PID)
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
	if topo.ProxyMode == "per_node_owned" {
		for _, l := range rt.Links {
			if l == nil || l.Link == nil {
				continue
			}
			l.Link.SetTagger(y.ownedTagger(rt, l.To, l.Listen))
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
			setNodePortVars(vars, node.Port, namedPorts, i)
			y.addPeerVars(vars, peerAddrs, nodeProxies, i)
			y.addClientVars(vars, clientPorts, clientProxies, i)
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
	// Set before the trigger watchers start. They read this map, and a later
	// write would race with that read.
	t.Data()["topology_hold"] = true

	if y.cfg.Fault.Isolate.active() {
		rt.Isolate = &isolateState{
			Target:    y.cfg.Fault.Isolate.Node,
			Direction: y.cfg.Fault.Isolate.direction(),
		}
		go y.watchIsolate(t, rt)
	}
	if y.cfg.Fault.Pause.active() {
		rt.Pause = &pauseState{
			Target: y.cfg.Fault.Pause.Node,
			Resume: y.cfg.Fault.Pause.resume(),
		}
		go y.watchPause(t, rt)
	}

	hold := exec.Command("sleep", "3600")
	hold.Stdout = log
	hold.Stderr = log
	if err := hold.Start(); err != nil {
		y.stopTopology(rt)
		return nil, err
	}
	// The hold is fuss's sleep, not a target node. Calibration (-kill=false)
	// stops it after the schedule so the trial can reach its checks.
	return hold, nil
}

func (y *YAMLAdapter) addPeerVars(vars map[string]string, peerAddrs [][]string, nodeProxies []string, i int) {
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
	if len(nodeProxies) > 0 {
		for k, addr := range nodeProxies {
			vars["node_proxy_addr_"+strconv.Itoa(k)] = addr
		}
		vars["peers_via_proxy"] = vars["peers"]
	}
}

// addClientVars exposes the real client listen port and the proxy clients dial.
// client_port is what the node binds. client_proxy and client_addr_j are the
// proxy addresses when client_via_proxy is set.
func (y *YAMLAdapter) addClientVars(vars map[string]string, ports []int, proxies []string, i int) {
	if i < 0 || i >= len(ports) {
		return
	}
	vars["client_port"] = strconv.Itoa(ports[i])
	addrs := make([]string, len(ports))
	for j, port := range ports {
		addr := "127.0.0.1:" + strconv.Itoa(port)
		if j < len(proxies) && proxies[j] != "" {
			addr = proxies[j]
		}
		vars["client_addr_"+strconv.Itoa(j)] = addr
		addrs[j] = addr
	}
	vars["client_proxy"] = addrs[i]
	vars["client_addrs"] = strings.Join(addrs, ",")
}

func parseIsolateNode(s string) (int, error) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "node_") {
		return -1, fmt.Errorf("fault node %q: want node_<index>", s)
	}
	return strconv.Atoi(strings.TrimPrefix(s, "node_"))
}

func (y *YAMLAdapter) watchFaultTrigger(t *BaseTrial) error {
	spec := y.cfg.Fault.After.File
	vars := y.vars(t, "")
	glob := expandTemplates(spec.Glob, vars)
	contains := expandTemplates(spec.Contains, vars)
	poll := parseYAMLDuration(spec.Poll, time.Millisecond)
	timeout := parseYAMLDuration(spec.Timeout, 30*time.Second)
	return waitFileContains(glob, contains, poll, timeout)
}

func (y *YAMLAdapter) watchIsolate(t *BaseTrial, rt *topologyRuntime) {
	err := y.watchFaultTrigger(t)
	vars := y.vars(t, "")

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
	target, terr := y.isolateTarget(vars)
	rt.Isolate.Target = target
	direction := y.cfg.Fault.Isolate.direction()
	rt.Isolate.Direction = direction
	if terr != nil {
		rt.Isolate.Fault = terr.Error()
		rt.mu.Unlock()
		return
	}
	rt.mu.Unlock()

	idx, perr := parseIsolateNode(target)
	if perr != nil {
		rt.mu.Lock()
		rt.Isolate.Fault = perr.Error()
		rt.mu.Unlock()
		return
	}
	y.cutIsolate(rt, idx, direction, t.Dir())
}

// isolateTarget is the node name at the moment the trigger fires.
// node_file wins over a static node: the scenario writes node_<index> there
// after it has observed the leader.
func (y *YAMLAdapter) isolateTarget(vars map[string]string) (string, error) {
	if file := strings.TrimSpace(y.cfg.Fault.Isolate.NodeFile); file != "" {
		path := expandTemplates(file, vars)
		b, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("node file: %w", err)
		}
		line := strings.TrimSpace(string(b))
		if i := strings.IndexAny(line, "\r\n"); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		if line == "" {
			return "", fmt.Errorf("node file %s is empty", path)
		}
		return line, nil
	}
	return strings.TrimSpace(expandTemplates(y.cfg.Fault.Isolate.Node, vars)), nil
}

func (y *YAMLAdapter) watchPause(t *BaseTrial, rt *topologyRuntime) {
	err := y.watchFaultTrigger(t)
	vars := y.vars(t, "")

	rt.mu.Lock()
	if rt.Pause == nil {
		rt.mu.Unlock()
		return
	}
	rt.Pause.Done = true
	if err != nil {
		rt.mu.Unlock()
		return
	}
	rt.Pause.TriggerFired = true
	target := expandTemplates(y.cfg.Fault.Pause.Node, vars)
	rt.Pause.Target = target
	resume := y.cfg.Fault.Pause.resume()
	rt.Pause.Resume = resume
	rt.mu.Unlock()

	idx, perr := parseIsolateNode(target)
	if perr != nil {
		return
	}
	y.pauseNode(rt, idx)
}

func (y *YAMLAdapter) pauseNode(rt *topologyRuntime, nodeIdx int) {
	rt.mu.Lock()
	var n *topologyNode
	if nodeIdx >= 0 && nodeIdx < len(rt.Nodes) {
		n = rt.Nodes[nodeIdx]
	}
	rt.mu.Unlock()
	if n == nil || n.cmd == nil || n.cmd.Process == nil {
		return
	}
	if err := n.cmd.Process.Signal(syscall.SIGSTOP); err != nil {
		return
	}
	rt.mu.Lock()
	if rt.Pause != nil {
		rt.Pause.PauseTime = time.Now()
		rt.Pause.Paused = true
	}
	rt.mu.Unlock()
}

// isolateAppliedDoc is ${dir}/isolate_applied.json, written after Cut returns.
// Scenarios wait for it before accepting a post-cut read.
type isolateAppliedDoc struct {
	Target       string `json:"target"`
	Direction    string `json:"direction"`
	LinksCut     int    `json:"links_cut"`
	LinksTotal   int    `json:"links_total"`
	TriggerFired bool   `json:"trigger_fired"`
	CutTime      string `json:"cut_time,omitempty"`
}

func appliedFromEvidence(ev map[string]any) isolateAppliedDoc {
	if ev == nil {
		return isolateAppliedDoc{}
	}
	doc := isolateAppliedDoc{
		LinksCut:   asInt(ev["links_cut"]),
		LinksTotal: asInt(ev["links_total"]),
	}
	doc.Target, _ = ev["target"].(string)
	doc.Direction, _ = ev["direction"].(string)
	doc.TriggerFired, _ = ev["trigger_fired"].(bool)
	doc.CutTime, _ = ev["cut_time"].(string)
	return doc
}

func writeIsolateApplied(dir string, ev map[string]any) {
	if dir == "" || ev == nil {
		return
	}
	b, err := json.Marshal(appliedFromEvidence(ev))
	if err != nil {
		return
	}
	b = append(b, '\n')
	tmp := filepath.Join(dir, ".isolate_applied.json.tmp")
	final := filepath.Join(dir, "isolate_applied.json")
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, final)
}

func (y *YAMLAdapter) cutIsolate(rt *topologyRuntime, nodeIdx int, direction, dir string) {
	rt.mu.Lock()
	cutAt := time.Now()
	if rt.Isolate != nil {
		rt.Isolate.CutTime = cutAt
		rt.Isolate.Direction = direction
	}
	mode := rt.ProxyMode
	type cutOp struct {
		link    *topologyLink
		partial bool // cut one source on a shared inbound proxy
	}
	var ops []cutOp
	for _, l := range rt.Links {
		if l == nil || l.Link == nil {
			continue
		}
		switch {
		case direction == "inbound" && l.To == nodeIdx:
			ops = append(ops, cutOp{link: l})
		case direction != "inbound" && mode == "per_node_owned" && l.From < 0:
			if l.To == nodeIdx {
				ops = append(ops, cutOp{link: l})
			} else {
				ops = append(ops, cutOp{link: l, partial: true})
			}
		case direction != "inbound" && (l.From == nodeIdx || l.To == nodeIdx):
			ops = append(ops, cutOp{link: l})
		}
	}
	rt.mu.Unlock()

	for _, op := range ops {
		l := op.link
		before := l.Link.Stats()
		tagBefore := l.Link.AllTagStats()
		if op.partial {
			src := nodeIdx
			l.Link.CutWhere(func(tag int, known bool) bool {
				return known && tag == src
			})
		} else {
			l.Link.Cut()
		}
		rt.mu.Lock()
		l.StatsBefore = before
		l.tagBefore = tagBefore
		l.CutTime = cutAt
		l.StatsAfter = l.Link.Stats()
		if op.partial {
			if l.cutFrom == nil {
				l.cutFrom = map[int]struct{}{}
			}
			l.cutFrom[nodeIdx] = struct{}{}
		} else {
			l.cut = true
			for i := range rt.Unknown {
				if rt.Unknown[i].Listen == l.Listen {
					rt.Unknown[i].Cut = true
				}
			}
		}
		rt.mu.Unlock()
	}
	writeIsolateApplied(dir, rt.isolateEvidence())
}

// ownedTagger maps an accepted client socket to the topology node that owns it.
func (y *YAMLAdapter) ownedTagger(rt *topologyRuntime, to int, listen string) func(net.Addr) (int, bool) {
	return func(remote net.Addr) (int, bool) {
		if remote == nil {
			rt.addUnknown(unknownOwner{To: to, Listen: listen})
			return 0, false
		}
		for attempt := 0; attempt < 3; attempt++ {
			pids, by := rt.nodePIDs()
			if len(pids) == 0 {
				return -1, true
			}
			pid, err := proxy.OwnerPID(remote, pids)
			if err == nil {
				if idx, ok := by[pid]; ok {
					return idx, true
				}
			}
			if attempt+1 < 3 {
				time.Sleep(15 * time.Millisecond)
			}
		}
		rt.addUnknown(unknownOwner{To: to, Listen: listen, Remote: remote.String()})
		return 0, false
	}
}

func (rt *topologyRuntime) nodePIDs() ([]int, map[int]int) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	pids := make([]int, 0, len(rt.Nodes))
	by := make(map[int]int, len(rt.Nodes))
	for _, n := range rt.Nodes {
		if n == nil || n.PID <= 0 {
			continue
		}
		pids = append(pids, n.PID)
		by[n.PID] = n.Index
	}
	return pids, by
}

func (rt *topologyRuntime) addUnknown(u unknownOwner) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	for _, l := range rt.Links {
		if l != nil && l.Listen == u.Listen && l.cut {
			u.Cut = true
			break
		}
	}
	rt.Unknown = append(rt.Unknown, u)
}

func (rt *topologyRuntime) unknownOnCutLink() bool {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	for _, u := range rt.Unknown {
		if u.Cut {
			return true
		}
	}
	return false
}

// checkIsolate returns a non-empty Verdict when the isolate fault cannot be proven.
func (y *YAMLAdapter) checkIsolate(rt *topologyRuntime) CheckResult {
	wait := parseYAMLDuration(y.cfg.Fault.After.File.Timeout, 30*time.Second) + 2*time.Second
	deadline := time.Now().Add(wait)
	for {
		rt.mu.Lock()
		done := rt.Isolate != nil && rt.Isolate.Done
		fired := rt.Isolate != nil && rt.Isolate.TriggerFired
		fault := ""
		if rt.Isolate != nil {
			fault = rt.Isolate.Fault
		}
		rt.mu.Unlock()
		if done {
			if !fired {
				return CheckResult{
					Verdict:  "UNKNOWN",
					Evidence: 1,
					Problems: []error{fmt.Errorf("fault.isolate: trigger did not fire")},
				}
			}
			if fault != "" {
				return CheckResult{
					Verdict:  "UNKNOWN",
					Evidence: 1,
					Problems: []error{fmt.Errorf("fault.isolate: %s", fault)},
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
			if rt.unknownOnCutLink() {
				return CheckResult{
					Verdict:  "UNKNOWN",
					Evidence: 1,
					Problems: []error{fmt.Errorf("fault.isolate: unknown owner on a cut link")},
				}
			}
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

// checkPause returns a non-empty Verdict when the pause fault cannot be proven.
func (y *YAMLAdapter) checkPause(rt *topologyRuntime) CheckResult {
	wait := parseYAMLDuration(y.cfg.Fault.After.File.Timeout, 30*time.Second) + 2*time.Second
	deadline := time.Now().Add(wait)
	for {
		rt.mu.Lock()
		done := rt.Pause != nil && rt.Pause.Done
		fired := rt.Pause != nil && rt.Pause.TriggerFired
		paused := rt.Pause != nil && rt.Pause.Paused
		rt.mu.Unlock()
		if done {
			if !fired || !paused {
				return CheckResult{
					Verdict:  "UNKNOWN",
					Evidence: 1,
					Problems: []error{fmt.Errorf("fault.pause: trigger did not pause a node")},
				}
			}
			return CheckResult{}
		}
		if time.Now().After(deadline) {
			return CheckResult{
				Verdict:  "UNKNOWN",
				Evidence: 1,
				Problems: []error{fmt.Errorf("fault.pause: trigger did not pause a node")},
			}
		}
		time.Sleep(10 * time.Millisecond)
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
	links = append(links, rt.ClientLinks...)
	var cont []*exec.Cmd
	for _, n := range nodes {
		if n != nil && n.paused && n.cmd != nil && n.cmd.Process != nil {
			cont = append(cont, n.cmd)
			n.paused = false
			n.pauseGen++
		}
	}
	pausedIdx := -1
	resume := "end"
	paused := false
	if rt.Pause != nil && rt.Pause.Paused {
		paused = true
		resume = rt.Pause.Resume
		if idx, err := parseIsolateNode(rt.Pause.Target); err == nil {
			pausedIdx = idx
		}
	}
	rt.mu.Unlock()

	for _, l := range links {
		if l != nil && l.Link != nil {
			_ = l.Link.Close()
		}
	}

	for _, cmd := range cont {
		if cmd != nil && cmd.Process != nil {
			_ = cmd.Process.Signal(syscall.SIGCONT)
		}
	}
	// A SIGSTOP'd process ignores SIGTERM until continued. resume:end sends
	// SIGCONT first; resume:never leaves it stopped and relies on SIGKILL.
	if paused && resume == "end" && pausedIdx >= 0 && pausedIdx < len(nodes) {
		if n := nodes[pausedIdx]; n != nil && n.cmd != nil && n.cmd.Process != nil {
			_ = n.cmd.Process.Signal(syscall.SIGCONT)
		}
	}

	grace := parseYAMLDuration(y.cfg.Topology.StopGrace, 2*time.Second)
	for i, n := range nodes {
		if n == nil || n.cmd == nil || n.cmd.Process == nil {
			continue
		}
		// Still-stopped nodes (resume:never) skip SIGTERM; SIGKILL reaps them.
		if paused && resume == "never" && i == pausedIdx {
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

// mustYAMLDuration parses a duration that LoadYAMLTarget already validated.
func mustYAMLDuration(s string) time.Duration {
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0
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
			return y.finishCheck(t, CheckResult{
				Verdict:  "UNKNOWN",
				Reason:   "nodes_not_ready",
				Evidence: 1,
				Problems: []error{fmt.Errorf("topology: a node was not ready in time")},
			})
		}
		if y.nemesisActive() {
			if rt.Nemesis != nil && rt.Nemesis.CheckStarted.IsZero() {
				rt.Nemesis.CheckStarted = time.Now()
			}
			if rt.Nemesis == nil || !rt.Nemesis.Done {
				return y.finishCheck(t, CheckResult{
					Verdict:  "UNKNOWN",
					Reason:   "nemesis_not_finished",
					Evidence: 1,
					Problems: []error{fmt.Errorf("nemesis: schedule did not finish")},
				})
			}
			if !rt.Nemesis.ReadyOK {
				msg := rt.Nemesis.Err
				if msg == "" {
					msg = "nodes not ready after quiet_period"
				}
				return y.finishCheck(t, CheckResult{
					Verdict:  "UNKNOWN",
					Reason:   "nodes_not_ready",
					Evidence: 1,
					Problems: []error{fmt.Errorf("nemesis: %s", msg)},
				})
			}
		}
		if y.cfg.Fault.Isolate.active() {
			if cr := y.checkIsolate(rt); cr.Verdict != "" {
				// The scenario's own UNKNOWN (failed read, wrong leader, cut
				// file missing) names the reason. A legal or illegal history
				// does not override a cut Fuss could not prove: the experiment
				// stays UNKNOWN. The check output stays, so the history
				// conclusion is still visible next to that verdict.
				if y.usesRulesCheck() {
					rules := y.checkRules(t)
					if rules.Verdict == "UNKNOWN" {
						return rules
					}
				}
				return cr
			}
		}
		if y.cfg.Fault.Pause.active() {
			if cr := y.checkPause(rt); cr.Verdict != "" {
				return cr
			}
		}
	}
	if after, ok := t.Data()["after_steps"].([]yamlStep); ok {
		if err := y.runAfterSteps(t, after); err != nil {
			return CheckResult{Verdict: "FAIL", Problems: []error{err}, Evidence: 1}
		}
	}
	if y.historyChecks() {
		return y.finishCheck(t, y.checkHistory(t))
	}
	if y.usesRulesCheck() {
		return y.finishCheck(t, y.checkRules(t))
	}
	if y.propertiesActive() {
		return y.finishCheck(t, CheckResult{Verdict: "PASS", Evidence: 1})
	}
	return y.checkAcks(t)
}

func (y *YAMLAdapter) propertiesActive() bool {
	p := y.cfg.Properties
	return len(p.Always) > 0 || len(p.Eventually) > 0 || len(p.Sometimes) > 0
}

func (y *YAMLAdapter) usesRulesCheck() bool {
	c := y.cfg.Check
	return len(c.Rules) > 0 || c.FileCount.Glob != "" || c.RecordCount.Contains != "" || len(c.Command.Command) > 0
}

func (y *YAMLAdapter) runAfterSteps(t *BaseTrial, steps []yamlStep) error {
	vars := y.vars(t, "")
	log, err := os.OpenFile(filepath.Join(t.Dir(), "after.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer log.Close()
	for _, st := range steps {
		if st.Delay != "" {
			time.Sleep(mustYAMLDuration(st.Delay))
		}
		args := y.expandSlice(st.Command, vars)
		if len(args) == 0 {
			if st.Delay != "" {
				continue
			}
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
	return y.evalRules(t, y.cfg.Check, true)
}

func (y *YAMLAdapter) evalRules(t *BaseTrial, c yamlCheck, publish bool) CheckResult {
	vars := y.vars(t, "")
	vals := map[string]any{}
	evidence := 0
	if c.FileCount.Glob != "" {
		g := expandTemplates(c.FileCount.Glob, vars)
		contains := expandTemplates(c.FileCount.Contains, vars)
		equals := expandTemplates(c.FileCount.Equals, vars)
		n := countMatchingFiles(g, contains, c.FileCount.JSONField, equals)
		name := c.FileCount.As
		if name == "" {
			name = "files"
		}
		vals[name] = n
		evidence += n
	}
	if c.RecordCount.Contains != "" {
		needle := expandTemplates(c.RecordCount.Contains, vars)
		n := strings.Count(t.RecordContent(), needle)
		name := c.RecordCount.As
		if name == "" {
			name = "matches"
		}
		vals[name] = n
		evidence += n
	}
	if len(c.Command.Command) > 0 {
		args := y.expandSlice(c.Command.Command, vars)
		cmd := exec.Command(args[0], args[1:]...)
		y.applyEnv(cmd, vars)
		out, err := cmd.CombinedOutput()
		s := string(out)
		name := c.Command.As
		if name == "" {
			name = "stdout"
		}
		vals[name] = s
		if c.Command.Contains != "" {
			want := expandTemplates(c.Command.Contains, vars)
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
	// Surface check vals (e.g. command stdout) in trial JSON evidence.check.
	// A property rule check leaves the scenario check's vals in place.
	if publish {
		t.Data()["check_vals"] = vals
	}
	for _, r := range c.Rules {
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

// countMatchingFiles is filepath.Glob, then optionally keeps only files whose
// contents match. An empty contains and jsonField counts every glob hit, as
// before. jsonField is either a dotted path (dagRun.id) or a bare key name
// searched recursively through objects and arrays.
func countMatchingFiles(glob, contains, jsonField, equals string) int {
	m, _ := filepath.Glob(glob)
	if contains == "" && jsonField == "" {
		return len(m)
	}
	n := 0
	for _, p := range m {
		st, err := os.Stat(p)
		if err != nil || st.IsDir() {
			continue
		}
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		if contains != "" && !strings.Contains(string(b), contains) {
			continue
		}
		if jsonField != "" && !jsonFieldEquals(b, jsonField, equals) {
			continue
		}
		n++
	}
	return n
}

func jsonFieldEquals(b []byte, field, want string) bool {
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return false
	}
	if strings.Contains(field, ".") {
		return jsonPathEquals(v, strings.Split(field, "."), want)
	}
	return jsonHasFieldEquals(v, field, want)
}

func jsonPathEquals(v any, path []string, want string) bool {
	cur := v
	for _, p := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return false
		}
		cur, ok = m[p]
		if !ok {
			return false
		}
	}
	return fmt.Sprint(cur) == want
}

func jsonHasFieldEquals(v any, field, want string) bool {
	switch x := v.(type) {
	case map[string]any:
		if got, ok := x[field]; ok && fmt.Sprint(got) == want {
			return true
		}
		for _, child := range x {
			if jsonHasFieldEquals(child, field, want) {
				return true
			}
		}
	case []any:
		for _, child := range x {
			if jsonHasFieldEquals(child, field, want) {
				return true
			}
		}
	}
	return false
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
	if y.nemesisActive() {
		rng, _ := t.Data()["rng"].(*rand.Rand)
		if rng == nil {
			rng = rand.New(rand.NewSource(seed))
		}
		start := time.Now()
		if err := y.runNemesis(t, rng); err != nil {
			return time.Since(start), true, err
		}
		return time.Since(start), true, nil
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
	if y.nemesisActive() {
		return 0, 0
	}
	minMS = y.cfg.Kill.Delay.MinMS
	maxMS = y.cfg.Kill.Delay.MaxMS
	if maxMS <= minMS {
		return 0, 3000
	}
	return minMS, maxMS
}

func (y *YAMLAdapter) nemesisActive() bool { return y.cfg.Nemesis.active() }

type yamlStatus struct {
	State string
}

func (s yamlStatus) String() string  { return s.State }
func (s yamlStatus) IsRunning() bool { return s.State == "running" }

func (y *YAMLAdapter) watchCmd(rt *topologyRuntime, n *topologyNode, cmd *exec.Cmd, done chan struct{}, pid int) {
	_ = cmd.Wait()
	noteProcWaited(pid)
	code := -1
	if cmd.ProcessState != nil {
		code = cmd.ProcessState.ExitCode()
	}
	rt.mu.Lock()
	if n != nil && n.cmd == cmd {
		n.ExitStatus = &code
		n.dead = true
	}
	rt.mu.Unlock()
	close(done)
}

func (rt *topologyRuntime) nemesisEvents() (events []NemesisEvent, ran bool) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.Nemesis == nil {
		return nil, false
	}
	out := make([]NemesisEvent, len(rt.Nemesis.Events))
	copy(out, rt.Nemesis.Events)
	return out, true
}

func (rt *topologyRuntime) recordNemesis(origin time.Time, e NemesisEvent) {
	e.TimestampNS = time.Since(origin).Nanoseconds()
	if e.TimestampNS < 0 {
		e.TimestampNS = 0
	}
	rt.mu.Lock()
	if rt.Nemesis == nil {
		rt.Nemesis = &nemesisRun{}
	}
	rt.Nemesis.Events = append(rt.Nemesis.Events, e)
	rt.mu.Unlock()
}

// runNemesis drives the schedule from the trial RNG, then heals, restarts
// dead nodes, waits quiet_period, and re-checks ready before the caller checks.
func (y *YAMLAdapter) runNemesis(t *BaseTrial, rng *rand.Rand) error {
	rt, ok := t.Data()["topology"].(*topologyRuntime)
	if !ok || rt == nil {
		return fmt.Errorf("nemesis: topology is not running")
	}
	spec := y.cfg.Nemesis
	weights := make([]int, len(spec.Actions))
	for i, a := range spec.Actions {
		weights[i] = a.Weight
	}
	fires := nemesisFireCount(spec.duration, spec.interval)
	picks := make([]int, fires)
	for i := range picks {
		picks[i] = pickWeighted(rng, weights)
	}
	rt.mu.Lock()
	if rt.Nemesis == nil {
		rt.Nemesis = &nemesisRun{}
	}
	rt.mu.Unlock()
	// origin is scheduleStart for both nemesis_events.json and history.jsonl.
	origin := time.Now()
	t.Data()["schedule_start"] = origin
	if watch := y.startLeaderWatch(t); watch != nil {
		defer func() {
			t.Data()["leader_samples"] = watch.finish()
		}()
	}
	var stop func()
	if y.workloadActive() {
		var err error
		stop, err = y.startWorkload(t, rt, rng, origin)
		if err != nil {
			return err
		}
		defer stop()
	}
	quietStart, quietEnd := scheduleNemesis(spec.warmup, spec.duration, spec.interval, spec.quiet, fires, func(i int) {
		if i < 0 || i >= len(picks) {
			return
		}
		y.fireNemesis(t, rt, spec.Actions[picks[i]], rng, origin)
	}, func() {
		y.nemesisRecover(t, rt, origin)
	}, func() {
		if stop != nil {
			stop()
		}
		y.nemesisReady(t, rt)
	})
	startNS := quietStart.Sub(origin).Nanoseconds()
	endNS := quietEnd.Sub(origin).Nanoseconds()
	if startNS < 0 {
		startNS = 0
	}
	if endNS < startNS {
		endNS = startNS
	}
	rt.mu.Lock()
	rt.Nemesis.QuietStarted = quietStart
	rt.Nemesis.QuietFinished = quietEnd
	rt.Nemesis.QuietStartNS = startNS
	rt.Nemesis.QuietEndNS = endNS
	rt.Nemesis.Done = true
	rt.mu.Unlock()
	t.Data()["quiet_start_ns"] = startNS
	t.Data()["quiet_end_ns"] = endNS
	return nil
}

// leaderWatch polls node_file values for leader_changed. Samples include a
// missing file as "", so empty then a leader counts as a change, and a file
// that never exists does not.
type leaderWatch struct {
	mu      sync.Mutex
	once    sync.Once
	samples map[string][]string
	paths   []string
	stop    chan struct{}
	done    chan struct{}
}

func (y *YAMLAdapter) startLeaderWatch(t *BaseTrial) *leaderWatch {
	var paths []string
	seen := map[string]bool{}
	vars := y.vars(t, "")
	for _, p := range y.cfg.Properties.Sometimes {
		if p.LeaderChanged == nil {
			continue
		}
		path := expandTemplates(p.LeaderChanged.NodeFile, vars)
		if path == "" || seen[path] {
			continue
		}
		seen[path] = true
		paths = append(paths, path)
	}
	if len(paths) == 0 {
		return nil
	}
	w := &leaderWatch{
		samples: map[string][]string{},
		paths:   paths,
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
	w.sample()
	go w.run()
	return w
}

func (w *leaderWatch) sample() {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, path := range w.paths {
		v := readLeaderValue(path)
		seq := w.samples[path]
		if len(seq) == 0 || seq[len(seq)-1] != v {
			w.samples[path] = append(seq, v)
		}
	}
}

func readLeaderValue(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	if i := strings.IndexAny(string(b), "\r\n"); i >= 0 {
		return strings.TrimSpace(string(b)[:i])
	}
	return strings.TrimSpace(string(b))
}

func (w *leaderWatch) run() {
	defer close(w.done)
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-w.stop:
			w.sample()
			return
		case <-tick.C:
			w.sample()
		}
	}
}

func (w *leaderWatch) finish() map[string][]string {
	w.once.Do(func() { close(w.stop) })
	<-w.done
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make(map[string][]string, len(w.samples))
	for k, v := range w.samples {
		out[k] = append([]string(nil), v...)
	}
	return out
}

func (y *YAMLAdapter) fireNemesis(t *BaseTrial, rt *topologyRuntime, action yamlNemesisAction, rng *rand.Rand, origin time.Time) {
	ev := NemesisEvent{
		Action:    action.Type,
		Mode:      action.Mode,
		Direction: action.Direction,
		Flow:      action.Flow,
		Details:   actionDetails(action, ""),
	}
	switch action.Type {
	case "heal_all":
		y.nemesisHealAll(rt)
	case "isolate_node":
		idx, err := y.nemesisTarget(t, action, len(rt.Nodes), rng)
		if err != nil {
			ev.Details = actionDetails(action, "error="+err.Error())
			break
		}
		ev.Targets = []string{nodeName(idx)}
		ev.Details = actionDetails(action, y.isolateNode(rt, idx, action, rng))
	case "isolate_client":
		idx, err := y.nemesisTarget(t, action, len(rt.Nodes), rng)
		if err != nil {
			ev.Details = actionDetails(action, "error="+err.Error())
			break
		}
		ev.Targets = []string{nodeName(idx)}
		ev.Details = actionDetails(action, y.isolateClient(rt, idx, action, rng))
	case "partition_halves":
		targets, details := y.partitionHalves(rt, action, rng)
		ev.Targets = targets
		ev.Details = actionDetails(action, details)
	case "kill_node":
		idx, err := y.nemesisTarget(t, action, len(rt.Nodes), rng)
		if err != nil {
			ev.Details = actionDetails(action, "error="+err.Error())
			break
		}
		ev.Targets = []string{nodeName(idx)}
		ev.Details = actionDetails(action, y.killNodeLater(t, rt, idx, action.restartDelay))
	case "pause_node":
		idx, err := y.nemesisTarget(t, action, len(rt.Nodes), rng)
		if err != nil {
			ev.Details = actionDetails(action, "error="+err.Error())
			break
		}
		ev.Targets = []string{nodeName(idx)}
		ev.Details = actionDetails(action, y.pauseNodeFor(rt, idx, action.pauseFor))
	default:
		ev.Details = actionDetails(action, "error=unknown action")
	}
	rt.recordNemesis(origin, ev)
}

func actionDetails(a yamlNemesisAction, extra string) string {
	var parts []string
	if a.Latency != "" {
		parts = append(parts, "latency="+a.Latency)
	}
	if a.Jitter != "" {
		parts = append(parts, "jitter="+a.Jitter)
	}
	if a.DropRate > 0 {
		parts = append(parts, fmt.Sprintf("drop_rate=%g", a.DropRate))
	}
	if a.Type == "kill_node" && a.RestartDelay != "" {
		parts = append(parts, "restart_delay="+a.RestartDelay)
	}
	if a.Type == "pause_node" && a.PauseDuration != "" {
		parts = append(parts, "pause_duration="+a.PauseDuration)
	}
	if extra != "" {
		parts = append(parts, extra)
	}
	return strings.Join(parts, " ")
}

func nodeName(i int) string { return "node_" + strconv.Itoa(i) }

func (y *YAMLAdapter) nemesisTarget(t *BaseTrial, action yamlNemesisAction, n int, rng *rand.Rand) (int, error) {
	switch strings.TrimSpace(action.Target) {
	case "random":
		return pickNodeIndex(rng, n), nil
	case "leader":
		file := action.NodeFile
		if strings.TrimSpace(file) == "" {
			file = y.cfg.Nemesis.NodeFile
		}
		path := expandTemplates(file, y.vars(t, ""))
		return readNodeFile(path)
	default:
		idx, err := parseNodeRef(action.Target)
		if err != nil {
			return -1, err
		}
		if idx < 0 || idx >= n {
			return -1, fmt.Errorf("node %d outside 0..%d", idx, n-1)
		}
		return idx, nil
	}
}

func parseNodeRef(s string) (int, error) {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "node_") {
		s = strings.TrimPrefix(s, "node_")
	}
	if s == "" {
		return -1, fmt.Errorf("empty node")
	}
	return strconv.Atoi(s)
}

func readNodeFile(path string) (int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return -1, fmt.Errorf("node file: %w", err)
	}
	line := strings.TrimSpace(string(b))
	if i := strings.IndexAny(line, "\r\n"); i >= 0 {
		line = strings.TrimSpace(line[:i])
	}
	if line == "" {
		return -1, fmt.Errorf("node file %s is empty", path)
	}
	return parseNodeRef(line)
}

func faultProfile(a yamlNemesisAction) proxy.FaultProfile {
	flow := a.Flow
	if flow == "" {
		flow = proxy.DirBoth
	}
	return proxy.FaultProfile{
		Mode:      proxy.LinkMode(a.Mode),
		Latency:   a.latency,
		Jitter:    a.jitter,
		DropRate:  a.DropRate,
		Direction: flow,
	}
}

func applyLink(l *proxy.Link, p proxy.FaultProfile, rng *rand.Rand) error {
	if l == nil {
		return nil
	}
	return l.Apply(p, deriveRand(rng))
}

func (y *YAMLAdapter) isolateNode(rt *topologyRuntime, idx int, action yamlNemesisAction, rng *rand.Rand) string {
	links := rt.peerLinkSnap()
	apply, sourceCut := classifyLinks(links, idx, action.Direction)
	var notes []string
	profile := faultProfile(action)
	for _, l := range apply {
		if err := applyLink(l.Link, profile, rng); err != nil {
			notes = append(notes, err.Error())
		}
	}
	if action.Mode == "reset" {
		for _, l := range sourceCut {
			l.Link.CutWhere(func(tag int, known bool) bool {
				return known && tag == idx
			})
		}
	} else if len(sourceCut) > 0 {
		for _, l := range sourceCut {
			if err := l.Link.ApplyWhere(func(tag int, known bool) bool {
				return known && tag == idx
			}, profile); err != nil {
				notes = append(notes, err.Error())
			}
		}
	}
	if len(apply) == 0 && len(sourceCut) == 0 {
		notes = append(notes, "no peer links")
	}
	return strings.Join(notes, " ")
}

func (y *YAMLAdapter) isolateClient(rt *topologyRuntime, idx int, action yamlNemesisAction, rng *rand.Rand) string {
	links := rt.clientLinkSnap()
	n := 0
	var notes []string
	profile := faultProfile(action)
	for _, l := range links {
		if l.To != idx {
			continue
		}
		n++
		if err := applyLink(l.Link, profile, rng); err != nil {
			notes = append(notes, err.Error())
		}
	}
	if n == 0 {
		notes = append(notes, "no client link")
	}
	return strings.Join(notes, " ")
}

func (y *YAMLAdapter) partitionHalves(rt *topologyRuntime, action yamlNemesisAction, rng *rand.Rand) ([]string, string) {
	n := len(rt.Nodes)
	minor := pickMinority(rng, n)
	if len(minor) == 0 {
		return nil, "skipped single node"
	}
	minorSet := map[int]struct{}{}
	for _, i := range minor {
		minorSet[i] = struct{}{}
	}
	var majority []int
	for i := 0; i < n; i++ {
		if _, ok := minorSet[i]; !ok {
			majority = append(majority, i)
		}
	}
	slices.Sort(minor)
	links := rt.peerLinkSnap()
	profile := faultProfile(action)
	var notes []string
	applied := 0
	for _, l := range links {
		if !partitionLink(l, minorSet, action.Direction) {
			continue
		}
		if l.From < 0 && !setHas(minorSet, l.To) {
			match := func(tag int, known bool) bool {
				if !known {
					return false
				}
				_, ok := minorSet[tag]
				return ok
			}
			if action.Mode == "reset" {
				l.Link.CutWhere(match)
			} else if err := l.Link.ApplyWhere(match, profile); err != nil {
				notes = append(notes, err.Error())
				continue
			}
			applied++
			continue
		}
		if err := applyLink(l.Link, profile, rng); err != nil {
			notes = append(notes, err.Error())
			continue
		}
		applied++
	}
	detail := fmt.Sprintf("minority=%s majority=%s links=%d", nodeList(minor), nodeList(majority), applied)
	if len(notes) > 0 {
		detail += " " + strings.Join(notes, " ")
	}
	names := make([]string, 0, n)
	for i := 0; i < n; i++ {
		names = append(names, nodeName(i))
	}
	return names, detail
}

func partitionLink(l *topologyLink, minority map[int]struct{}, direction string) bool {
	if l == nil || l.Link == nil || l.Kind == "client" {
		return false
	}
	if direction == "" {
		direction = "both"
	}
	fromMin := setHas(minority, l.From)
	toMin := setHas(minority, l.To)
	if l.From < 0 {
		switch direction {
		case "outbound":
			return !toMin
		case "inbound":
			return toMin
		default:
			return true
		}
	}
	cross := fromMin != toMin
	if !cross {
		return false
	}
	switch direction {
	case "inbound":
		return toMin
	case "outbound":
		return fromMin
	default:
		return true
	}
}

func setHas(m map[int]struct{}, i int) bool {
	_, ok := m[i]
	return ok
}

func nodeList(idx []int) string {
	if len(idx) == 0 {
		return "-"
	}
	names := make([]string, len(idx))
	for i, n := range idx {
		names[i] = nodeName(n)
	}
	return strings.Join(names, ",")
}

func classifyLinks(links []*topologyLink, idx int, direction string) (apply, sourceCut []*topologyLink) {
	if direction == "" {
		direction = "both"
	}
	for _, l := range links {
		if l == nil || l.Link == nil || l.Kind == "client" {
			continue
		}
		switch direction {
		case "inbound":
			if l.To == idx {
				apply = append(apply, l)
			}
		case "outbound":
			if l.From == idx {
				apply = append(apply, l)
			} else if l.From < 0 && l.To != idx {
				sourceCut = append(sourceCut, l)
			}
		default:
			if l.From == idx || l.To == idx {
				apply = append(apply, l)
			} else if l.From < 0 && l.To != idx {
				sourceCut = append(sourceCut, l)
			}
		}
	}
	return apply, sourceCut
}

func (rt *topologyRuntime) peerLinkSnap() []*topologyLink {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	out := make([]*topologyLink, 0, len(rt.Links))
	for _, l := range rt.Links {
		if l != nil && l.Link != nil && l.Kind != "client" {
			out = append(out, l)
		}
	}
	return out
}

func (rt *topologyRuntime) clientLinkSnap() []*topologyLink {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	out := make([]*topologyLink, 0, len(rt.ClientLinks))
	for _, l := range rt.ClientLinks {
		if l != nil && l.Link != nil {
			out = append(out, l)
		}
	}
	return out
}

func (y *YAMLAdapter) nemesisHealAll(rt *topologyRuntime) {
	links := append(rt.peerLinkSnap(), rt.clientLinkSnap()...)
	healthy := proxy.FaultProfile{Mode: proxy.ModeHealthy, Direction: proxy.DirBoth}
	for _, l := range links {
		_ = l.Link.Apply(healthy, nil)
		rt.mu.Lock()
		l.cut = false
		l.cutFrom = nil
		rt.mu.Unlock()
	}
}

func (y *YAMLAdapter) nemesisRecover(t *BaseTrial, rt *topologyRuntime, origin time.Time) {
	y.nemesisHealAll(rt)
	y.nemesisUnpauseAll(rt)
	notes := y.nemesisRestartDead(t, rt)
	detail := "recovery"
	if notes != "" {
		detail += " " + notes
	}
	rt.recordNemesis(origin, NemesisEvent{Action: "heal_all", Details: detail})
}

func (y *YAMLAdapter) nemesisUnpauseAll(rt *topologyRuntime) {
	rt.mu.Lock()
	var cmds []*exec.Cmd
	for _, n := range rt.Nodes {
		if n == nil || !n.paused {
			continue
		}
		n.pauseGen++
		n.paused = false
		if n.cmd != nil && n.cmd.Process != nil {
			cmds = append(cmds, n.cmd)
		}
	}
	rt.mu.Unlock()
	for _, cmd := range cmds {
		_ = cmd.Process.Signal(syscall.SIGCONT)
	}
}

func (y *YAMLAdapter) nemesisRestartDead(t *BaseTrial, rt *topologyRuntime) string {
	var notes []string
	for i := range rt.Nodes {
		if !y.nodeNeedsRestart(rt, i) {
			continue
		}
		if err := y.restartNode(rt, i, 0, true); err != nil {
			notes = append(notes, fmt.Sprintf("%s: %s", nodeName(i), err.Error()))
		}
	}
	return strings.Join(notes, " ")
}

func (y *YAMLAdapter) nodeNeedsRestart(rt *topologyRuntime, i int) bool {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if i < 0 || i >= len(rt.Nodes) || rt.Nodes[i] == nil {
		return false
	}
	n := rt.Nodes[i]
	if n.restarting {
		return false
	}
	if n.paused && pidLive(n.PID) {
		return false
	}
	return n.dead || n.ExitStatus != nil || !pidLive(n.PID)
}

func pidLive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}

func (y *YAMLAdapter) killNodeLater(t *BaseTrial, rt *topologyRuntime, idx int, delay time.Duration) string {
	gen, err := y.killNode(rt, idx)
	if err != nil {
		return "error=" + err.Error()
	}
	if delay <= 0 {
		if err := y.restartNode(rt, idx, gen, false); err != nil {
			return "error=" + err.Error()
		}
		return "restarted"
	}
	time.AfterFunc(delay, func() {
		_ = y.restartNode(rt, idx, gen, false)
	})
	return "restart_in=" + delay.String()
}

func (y *YAMLAdapter) killNode(rt *topologyRuntime, idx int) (int, error) {
	rt.mu.Lock()
	if rt.Stopped {
		rt.mu.Unlock()
		return 0, fmt.Errorf("topology stopped")
	}
	if idx < 0 || idx >= len(rt.Nodes) || rt.Nodes[idx] == nil {
		rt.mu.Unlock()
		return 0, fmt.Errorf("no node %d", idx)
	}
	n := rt.Nodes[idx]
	n.gen++
	gen := n.gen
	n.dead = true
	n.paused = false
	n.pauseGen++
	n.StopHow = "SIGKILL"
	cmd := n.cmd
	done := n.waitDone
	rt.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return gen, fmt.Errorf("node %d has no process", idx)
	}
	_ = cmd.Process.Signal(syscall.SIGKILL)
	if done != nil {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			return gen, fmt.Errorf("node %d did not exit after SIGKILL", idx)
		}
	}
	return gen, nil
}

func (y *YAMLAdapter) restartNode(rt *topologyRuntime, idx, expectGen int, force bool) error {
	rt.mu.Lock()
	if rt.Stopped || idx < 0 || idx >= len(rt.Nodes) || rt.Nodes[idx] == nil {
		rt.mu.Unlock()
		return nil
	}
	n := rt.Nodes[idx]
	if n.restarting {
		rt.mu.Unlock()
		return nil
	}
	if !force && n.gen != expectGen {
		rt.mu.Unlock()
		return nil
	}
	alive := n.PID > 0 && !n.dead && n.ExitStatus == nil && pidLive(n.PID)
	if alive {
		rt.mu.Unlock()
		return nil
	}
	n.restarting = true
	n.gen++
	args := append([]string(nil), n.args...)
	env := append([]string(nil), n.env...)
	dataDir := n.DataDir
	port := n.Port
	rt.mu.Unlock()
	return y.spawnClaimed(rt, idx, args, env, dataDir, port)
}

func (y *YAMLAdapter) spawnClaimed(rt *topologyRuntime, idx int, args, env []string, dataDir string, port int) error {
	clearRestarting := func() {
		rt.mu.Lock()
		if idx >= 0 && idx < len(rt.Nodes) && rt.Nodes[idx] != nil {
			rt.Nodes[idx].restarting = false
		}
		rt.mu.Unlock()
	}
	if len(args) == 0 {
		clearRestarting()
		return fmt.Errorf("node %d: no start args", idx)
	}
	cmd := exec.Command(args[0], args[1:]...)
	if len(env) > 0 {
		cmd.Env = append([]string(nil), env...)
	}
	if rt.log != nil {
		cmd.Stdout = rt.log
		cmd.Stderr = rt.log
	}
	if err := cmd.Start(); err != nil {
		rt.mu.Lock()
		if rt.Nodes[idx] != nil {
			rt.Nodes[idx].dead = true
			rt.Nodes[idx].restarting = false
		}
		rt.mu.Unlock()
		return err
	}
	rt.mu.Lock()
	stopped := rt.Stopped
	rt.mu.Unlock()
	if stopped {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		noteProcWaited(cmd.Process.Pid)
		clearRestarting()
		return nil
	}
	noteRestartedProc(cmd)
	pid := cmd.Process.Pid
	done := make(chan struct{})
	rt.mu.Lock()
	n := rt.Nodes[idx]
	n.cmd = cmd
	n.PID = pid
	n.Port = port
	n.DataDir = dataDir
	n.StartTime = time.Now()
	n.ExitStatus = nil
	n.dead = false
	n.paused = false
	n.Ready = false
	n.StopHow = ""
	n.waitDone = done
	n.args = args
	n.env = env
	n.restarting = false
	rt.mu.Unlock()
	go y.watchCmd(rt, n, cmd, done, pid)
	return nil
}

func (y *YAMLAdapter) pauseNodeFor(rt *topologyRuntime, idx int, d time.Duration) string {
	rt.mu.Lock()
	if rt.Stopped || idx < 0 || idx >= len(rt.Nodes) || rt.Nodes[idx] == nil {
		rt.mu.Unlock()
		return "error=no node"
	}
	n := rt.Nodes[idx]
	cmd := n.cmd
	if cmd == nil || cmd.Process == nil {
		rt.mu.Unlock()
		return "error=no process"
	}
	n.pauseGen++
	gen := n.pauseGen
	n.paused = true
	rt.mu.Unlock()
	if err := cmd.Process.Signal(syscall.SIGSTOP); err != nil {
		rt.mu.Lock()
		if n.pauseGen == gen {
			n.paused = false
		}
		rt.mu.Unlock()
		return "error=" + err.Error()
	}
	time.AfterFunc(d, func() {
		y.contNode(rt, idx, gen)
	})
	return ""
}

func (y *YAMLAdapter) contNode(rt *topologyRuntime, idx, gen int) {
	rt.mu.Lock()
	if idx < 0 || idx >= len(rt.Nodes) || rt.Nodes[idx] == nil {
		rt.mu.Unlock()
		return
	}
	n := rt.Nodes[idx]
	if n.pauseGen != gen || !n.paused {
		rt.mu.Unlock()
		return
	}
	cmd := n.cmd
	n.paused = false
	rt.mu.Unlock()
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Signal(syscall.SIGCONT)
	}
}

func (y *YAMLAdapter) nemesisReady(t *BaseTrial, rt *topologyRuntime) {
	timeout := parseYAMLDuration(y.cfg.Topology.Ready.Timeout, 5*time.Second)
	deadline := time.Now().Add(timeout)
	ok := true
	var failed []string
	for i := range rt.Nodes {
		if y.waitNodeReadyAlive(t, rt, i, deadline) {
			continue
		}
		ok = false
		failed = append(failed, nodeName(i))
	}
	rt.mu.Lock()
	if rt.Nemesis == nil {
		rt.Nemesis = &nemesisRun{}
	}
	rt.Nemesis.ReadyOK = ok
	if !ok {
		rt.Nemesis.Err = "nodes not ready after quiet_period: " + strings.Join(failed, ",")
	}
	rt.mu.Unlock()
}

func (y *YAMLAdapter) waitNodeReadyAlive(t *BaseTrial, rt *topologyRuntime, i int, deadline time.Time) bool {
	for {
		rt.mu.Lock()
		var n *topologyNode
		if i >= 0 && i < len(rt.Nodes) {
			n = rt.Nodes[i]
		}
		var pid int
		var dataDir string
		var port int
		paused := false
		if n != nil {
			pid = n.PID
			dataDir = n.DataDir
			port = n.Port
			paused = n.paused
		}
		rt.mu.Unlock()
		alive, zombie := inspectProc(pid)
		if n != nil && alive && !zombie && !paused {
			rt.mu.Lock()
			clientPorts := append([]int(nil), rt.ClientPorts...)
			clientProxies := append([]string(nil), rt.ClientProxies...)
			peerAddrs := rt.PeerAddrs
			nodeProxies := rt.NodeProxies
			named := rt.NamedPorts
			rt.mu.Unlock()
			vars := y.vars(t, "")
			vars["i"] = strconv.Itoa(i)
			vars["data_dir"] = dataDir
			vars["port"] = strconv.Itoa(port)
			setNodePortVars(vars, port, named, i)
			y.addClientVars(vars, clientPorts, clientProxies, i)
			y.addPeerVars(vars, peerAddrs, nodeProxies, i)
			if y.topologyReadyOnce(vars) {
				return true
			}
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
}
