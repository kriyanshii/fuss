# fuss Architecture & Engineering Roadmap

This document defines the product direction, architectural upgrades, and concrete implementation tasks for [kriyanshii/fuss](https://github.com/kriyanshii/fuss). It is written as an executable specification for engineers and AI coding assistants (Cursor) working in this repository.

## 1. Positioning & Core Thesis

**Goal:** Make fuss the "Zero-Root, CI-Native Jepsen in a Single Binary" for unmodified distributed systems.

fuss already has two strong differentiators:

1. Zero-root userspace network partitioning via `proxy/link.go` and PID-based socket ownership (`proxy/owner.go` with `per_node_owned`).
2. Reproducible failure bundles (`-save-bundle`, `-replay-bundle`) and harness self-checks (`control.go`: zombie, stranded).

### Explicit Non-Goals

- Do not try to out-hypervisor Antithesis. fuss controls fault timing via `-seed`, not target goroutine scheduling, `time.Now()`, or disk I/O. Never market or design fuss as full-stack Deterministic Simulation Testing (DST); own seeded, reproducible black-box fault injection.
- Do not rebuild Jepsen's Clojure/SSH/LXC orchestrator. Keep fuss a single static Go binary that runs on a developer laptop or GitHub Actions runner in seconds without iptables, root privileges, or the JVM.
- Defer `promise-miner/` work. Do not invest in automated doc-to-test generation until the core fault engine, history recorder, and checkers reliably catch real bugs in etcd, rqlite, and valkey.

## 2. Current Gaps vs. Jepsen & Antithesis

| Area | Current State in fuss | Gap to Close |
| --- | --- | --- |
| Network Faults | `proxy.Link.Cut()` / `Heal()` closes sockets (`ClosedByCut`) and refuses new ones (`RefusedWhileCut`) | Real networks drop packets silently (blackholes without TCP RST), add latency/jitter, and drop a percentage of packets |
| Client Isolation | `via_proxy: true` proxies peer-to-peer traffic; client ports are direct | Clients cannot be partitioned from the leader or routed only to a stale minority node |
| Fault Schedule | Single-shot fault trigger per trial (`yaml_target.go`, `inject.go`) | Real consensus bugs require overlapping, multi-step nemesis loops (partition -> elect -> heal -> kill) |
| Workload & History | Custom `-drive` / `-inflight` flags inside `examples/repkv/main.go` | Harness must own the concurrent client loop and record a canonical operation history (invoke/ok/fail/info) |
| Checkers | Rule checks + [Porcupine](https://github.com/anishathalye/porcupine) isolated inside `examples/repkv/porcupine.go` | Promote Porcupine + `VisualizePath` HTML output into fuss core; add set, bank, eventually, and Jepsen EDN export |
| Coverage Telemetry | `pickStratum` targets specific windows, but silent misses aren't surfaced | Add Antithesis-style SOMETIMES / EVENTUALLY property assertions across `-n` trials |

## 3. Implementation Roadmap

Execute these phases in order. Keep existing CLI flags (`-seed`, `-n`, `-json`, `-save-bundle`, `-replay-bundle`, `-self-check`) and exit codes (0 pass, 1 fail, 2 setup error, 3 control broken) backward-compatible.

### Phase 1: Upgrade `proxy/` into an L4 Chaos Proxy

Target files: `proxy/link.go`, `proxy/owner.go`, `yaml_target.go`

Currently, `Cut()` immediately closes TCP connections, giving targets a fast EOF/ECONNRESET and masking timeout/lease bugs.

#### Requirements

1. Replace binary Cut/Heal state with a per-link `FaultProfile`:

```go
type LinkMode string

const (
    ModeHealthy   LinkMode = "healthy"
    ModeReset     LinkMode = "reset"     // Current Cut(): close active conns, refuse new ones
    ModeBlackhole LinkMode = "blackhole" // Accept/keep TCP conns open, silently drop/stall bytes (no RST/EOF)
    ModeDegraded  LinkMode = "degraded"  // Apply latency, jitter, and probabilistic drop
)

type FaultProfile struct {
    Mode      LinkMode
    Latency   time.Duration // Base one-way delay
    Jitter    time.Duration // +/- uniform random delay seeded by trial RNG
    DropRate  float64       // [0.0, 1.0] probability of discarding one already-read TCP chunk. The socket stays up, so framing desynchronizes. New dials are still accepted.
    Direction string        // "both", "a_to_b", "b_to_a" (asymmetric partitions)
}
```

2. Silent Blackhole (`ModeBlackhole`):
   - Do not close active TCP sockets when transitioning to `ModeBlackhole`.
   - On `io.Copy` / relay loops inside `proxy/link.go`, read from the source socket (or pause reading to trigger TCP backpressure/window zeroing) and discard bytes without forwarding or closing until `Heal()` or socket timeout.
   - For new incoming connections while blackholed, accept the TCP handshake (or stall Dial up to a configurable hold duration) and drop payload bytes so the application experiences a hung peer rather than `ECONNREFUSED`.
3. Asymmetric & Degraded Links:
   - Support directional profiles (`a_to_b` vs `b_to_a`) so node A can hear node B's heartbeats while B drops A's votes/append replies.
   - Use the trial's seeded `*rand.Rand` for Jitter and DropRate so degraded runs remain seed-reproducible.
4. Client-Port Proxying:
   - Extend topology in `yaml_target.go` so client ports can optionally set `client_via_proxy: true`.
   - Expose client-to-node links to the nemesis so tests can partition a client from the leader while allowing the client to still query a stale minority node.

#### Acceptance Criteria

- `go test ./proxy/...` covers `ModeReset`, `ModeBlackhole` (verifying a client/peer blocks until its own read/write deadline rather than receiving immediate EOF), `ModeDegraded` (latency bounds + drop rate), and asymmetric directionality.
- Existing repkv tests pass unchanged using `ModeReset`.

#### Settled proxy semantics

- Each relay reads the live `FaultProfile` under the link mutex on every chunk. `Apply` on an already-open connection takes effect on the next chunk. A delay already in progress is woken, and that chunk is classified again before it is written. `ModeReset` still closes the sockets.
- `DropRate` discards one already-read TCP chunk and forwards what follows. That removes an arbitrary byte range and desynchronizes framing. New dials are still accepted. This is a framing fault, not a refused connection and not a stall.
- `ModeBlackhole` reads and discards. The sender's `Write` succeeds. The peer blocks until its read deadline. Targets that depend on TCP send-buffer backpressure (`Write` blocking) see no stall. A pause/throttle mode is not part of this proxy.

### Phase 2: Composable Nemesis Loop in `yaml_target.go`

Target files: `yaml_target.go`, `inject.go`, `trial.go`

Replace the single-trigger fault model with an optional continuous nemesis schedule while keeping legacy single fault blocks working.

#### Proposed YAML Schema

```yaml
nemesis:
  seed_from_trial: true
  warmup: 2s          # Let cluster elect initial leader before faults begin
  duration: 30s       # Total active chaos window
  interval: 3s        # Time between nemesis actions
  quiet_period: 5s    # Mandatory heal + recovery window before final checks
  actions:
    - type: isolate_node
      target: leader          # "leader" (via node_file), "random", or explicit index
      mode: blackhole         # "reset" | "blackhole" | "degraded"
      direction: both         # which links: "both" | "inbound" | "outbound"
      flow: both              # byte flow inside a link: "both" (default) | "a_to_b" | "b_to_a"
      weight: 3
    - type: partition_halves  # Split cluster into majority vs minority
      mode: reset
      weight: 2
    - type: kill_node
      target: random          # SIGKILL + automatic restart after restart_delay
      restart_delay: 2s
      weight: 2
    - type: pause_node        # SIGSTOP for pause_duration, then SIGCONT
      target: random
      pause_duration: 2s
      weight: 1
    - type: heal_all
      weight: 2
```

#### Requirements

1. Nemesis Event Log: Every action fired (`timestamp_ns`, `action_type`, `target_nodes`, `fault_profile`) must be recorded in a structured slice (`[]NemesisEvent`) and saved into the trial bundle (`bundle.go`).
2. Process Lifecycle Safety: Track all restarted PIDs under the nemesis loop and wire them into `control.go` (zombie / stranded checks) so restarted nodes are always cleanly reaped on exit or failure.
3. Mandatory Recovery Phase: Before running post-fault checks, execute `HealAll()`, restart any killed-but-unrestarted nodes, and wait `quiet_period` (or until ready probes succeed).
4. `direction` selects links (`both`, `inbound`, `outbound`), matching `fault.isolate`. `flow` selects the byte direction inside a link (`both`, `a_to_b`, `b_to_a`). A missing `flow` is `both`.

#### Acceptance Criteria

- A single fuss trial can run a 30-second randomized sequence of partitions, blackholes, SIGKILL+restarts, and heals driven purely by `-seed` and YAML.
- Failure bundles (`-save-bundle`) contain `nemesis_events.json` with exact relative timestamps.

### Phase 3: Core History Capture, Porcupine, & Visualization

Target files: new `history/` package, `check.go`, `bundle.go`, `examples/repkv/`

Move workload generation, operation history logging, and [Porcupine](https://github.com/anishathalye/porcupine) checking out of `examples/repkv/` into fuss core.

1. Canonical History Format (`history.jsonl`)

Every operation executed by a fuss workload driver emits a JSONL record:

```json
{"process": 1, "op": "put", "key": "k1", "value": "v42", "call_ns": 1020300, "return_ns": 1085400, "status": "ok"}
{"process": 2, "op": "get", "key": "k1", "value": "v42", "call_ns": 1050000, "return_ns": 1090000, "status": "ok"}
{"process": 3, "op": "put", "key": "k1", "value": "v99", "call_ns": 1060000, "return_ns": 6060000, "status": "info"}
```

- `status: "ok"` — operation definitely succeeded.
- `status: "fail"` — operation definitely failed (e.g., HTTP 500 / explicit error before commit; did not take effect).
- `status: "info"` — indeterminate (client timeout, EOF, connection reset). Checkers must treat `"info"` operations as may or may not have taken effect after `call_ns`.

2. Pluggable Workload Drivers

Start with two built-in drivers configured in YAML so unmodified targets don't need custom Go code:

- `http_kv`: Configurable PUT/POST/GET/DELETE URL templates, keyspace size, concurrency (workers), request rate, and timeout -> status mapping (ok, fail, info).
- `exec_op`: Invokes a target's CLI or helper script per op (slower, but works for any binary without HTTP).

3. Built-in Checkers (`check.go`)

1. `porcupine` (Linearizability):
   - Built-in Register / KV model in core fuss (supporting get, put, cas if configured).
   - Properly models `status: "info"` operations as open-ended until the end of the history.
   - Automatic Visual Trace: Whenever Porcupine returns non-linearizable (or `CheckOperationsVerbose` times out), call `porcupine.VisualizePath(...)` and save `porcupine.html` inside the `-save-bundle` directory. Include the `NemesisEvent` timeline markers if possible, or write a companion `timeline.html` / summary.
2. `set` (No Lost Acknowledged Writes):
   - Track all unique values with `status: "ok"` on put/add. After `quiet_period`, read back all keys/sets and fail if any acknowledged write is missing.
3. `eventually_available` (Liveness):
   - Verify that during `quiet_period` (after `heal_all`), at least N consecutive reads and writes succeed with `status: "ok"`. Catch clusters that stay deadlocked after a partition heals.
4. Jepsen EDN Export (`-export-edn` or bundle artifact):
   - Provide a lightweight serializer from `history.jsonl` + `nemesis_events.json` to Jepsen-compatible `.edn` so users can run [Elle](https://arcadedb.com/blog/arcadedb-jepsen-tests-34-pass/) or Knossos externally without rewriting tests.

#### Acceptance Criteria

- `examples/repkv/` is refactored to use core fuss `http_kv` workload + core porcupine checker, deleting duplicate linearizability boilerplate from `examples/repkv/porcupine.go`.
- Running repkv in bug mode produces a failure bundle containing `history.jsonl`, `nemesis_events.json`, and an inspectable `porcupine.html`.

### Phase 4: Antithesis-Style Property Telemetry (ALWAYS / SOMETIMES / EVENTUALLY)

Target files: `main.go`, `trial.go`, `measure.go`, `yaml_target.go`

Across `-n` trials, a test suite that passes 50/50 times is useless if the fault never actually overlapped with an interesting state (e.g., the leader was never killed mid-replication).

#### Requirements

1. Aggregate Trial Property Tracking:
   - **ALWAYS:** Must hold on every trial (e.g., porcupine linearizability, set durability). Any single violation fails the suite immediately (or after `-n` with failure summary).
   - **EVENTUALLY:** Must hold after `quiet_period` on every trial (e.g., `eventually_available`, cluster health probe).
   - **SOMETIMES:** Coverage assertions evaluated across the entire `-n` run (e.g., `fault_hit_inflight_write >= 1`, `leader_changed >= 1`, `minority_read_attempted >= 1`).
2. Surface SOMETIMES Failures in CLI & `-json`:
   - If a declared SOMETIMES condition has 0 hits across all `-n` trials, exit non-zero (or emit a distinct warning/status in `-json` output) so CI catches "vacuously green" fault tests.

### Phase 5: Showcase Real Targets

Once Phases 1–3 land, validate fuss against real systems already in `examples/`:

1. etcd / rqlite: Write declarative YAML scenarios using nemesis + `http_kv` + porcupine + `eventually_available` with zero custom Go adapter code where possible.
2. Document 1–2 real or historical bug reproductions (e.g., stale read under asymmetric partition or split-vote liveness stall) with saved bundles so new users can run `fuss -replay-bundle` on day one and see `porcupine.html` pinpoint the violation.

## 4. Cursor Implementation Guidelines

- Preserve determinism of the harness: Any randomness added in `proxy/` (jitter, drops) or nemesis (action selection, target picking) must derive from the trial's seeded `*rand.Rand` (`inject.go`). Never call global `math/rand` directly.
- Keep `go test ./...` fast and rootless: All unit and integration tests for `proxy/`, `history/`, and nemesis must run without sudo, iptables, or external containers.
- Bundle everything on failure: Whenever a trial fails in `-save-bundle` mode, ensure `history.jsonl`, `nemesis_events.json`, `porcupine.html` (if applicable), node stdout/stderr logs, and the scenario YAML are all written atomically into the bundle.
