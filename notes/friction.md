# Friction log

What real targets forced while pointing Fuss at them. Verdicts below are copied from Fuss `-json`. Lines marked **reading** are from a target log or from reading code, not from a verdict.

## etcd v3.7.4, `http_kv`

Binary: `examples/etcd/etcd` (3.7.4). Machine: darwin arm64.

1. **JSON body, not a raw value.** A one-node probe: `PUT /v3/kv/put` with body `k0` returned `501` and `{"code":12, "message":"Method Not Allowed"}`. `POST` with `{"key":"azA=","value":"djE="}` returned `200` and a header object, not the value. `POST /v3/kv/range` for a missing key returned `200` with a header and no `kvs`, not `404`. `http_kv` now has `headers`, `value_from`, `value_encoding: base64`, `error_from`, and `${key_b64}` / `${value_b64}` / `${old_b64}`. A 301, 302, 307, or 308 is followed with the original method and body.

2. **Ready recheck dropped `${client_port}`.** The first `etcd-serializable.yaml` seed 1 was `UNKNOWN`, label `before-start`, and the `-json` line had no `reason`. The nodes' `/health` was fine. The post-quiet ready check built its command without `client_port`, so curl had an empty port. The recheck now gets the same client variables as start. The `UNKNOWN` from a failed recheck sets `reason` `nodes_not_ready`.

3. **`blackhole` did not partition a `per_node_owned` node.** Seed 1 with only the inbound proxy blackholed: verdict `PASS`. `nemesis_events.json` detail was `shared_outbound_unfiltered`. The minority's own dials land on the other nodes' shared proxies, and blackhole did not apply to that tag. `ApplyWhere` now puts the same profile on those tagged connections. `reset` already used `CutWhere`.

4. **Healing a blackhole resumed the spliced TCP stream.** Seed 1 of both read levels was `UNKNOWN` `reason` `nodes_not_ready`. Node 2's evidence `exit_status` was `2`. **Reading:** `target.log` has `panic: index, 54, is out of range [1]` in `raft.(*raftLog).maybeAppend` / `handleAppendEntries`. The proxy kept the sockets open, discarded bytes, then forwarded later bytes on the same connection. Fuss did not turn that panic into a verdict. `Apply` now closes a connection that discarded bytes once that direction leaves `ModeBlackhole`. Seed 1 after that change: serializable `NONLINEARIZABLE` / `nonlinearizable`, linearizable `PASS`. Both trials had node 2 `ready` true, `exit_status` `-1`, `stop` `SIGTERM`. **Reading:** the kept serializable `target.log` has no `panic`. `porcupine.html` is `notes/runs/etcd-blackhole-s/porcupine.html`. The earlier `mode: reset` seed-1 verdicts were the same pair; that bundle is `notes/runs/etcd-reset-s`.

5. **`/health` is a bad ready check after a partition.** etcd's `/health` can stay false while the process is up. The configs use `GET /livez`, whose body is `ok`. It still cannot succeed if the process has already exited, which is what happened in item 4.

6. **`SOMETIMES` was reported unmet on the `UNKNOWN` trials.** `CheckNamed` returned before `finishCheck`, so `fault_during_inflight` was never evaluated and the summary said `hits=0`. Those early returns now go through `finishCheck`. The trial verdict stays `UNKNOWN`. The reset trials, which reached the checker, had `hits=1`.

## rqlite v10.7.4, `http_kv`

Binary: `examples/rqlite/rqlited` (v10.7.4). Config: `examples/rqlite/rqlite-http.yaml`. Seed 1.

Fuss `verdict` was `PASS`. `fault_during_inflight` `hits=1`. The nemesis drew `kill_node` on node 1 twice (restart after 1s) and did not draw the isolate. History: 110 ok puts, 136 ok gets, 11 put `info`, 6 get `info`. Get values were the raw strings (`v1-0`), so `value_from: results.0.values.0.0` matched rqlite's JSON. No redirect failure showed up on this seed.

**Bookkeeping.** Trial evidence lists node 1 `ready: false` after the kills, while the verdict is `PASS`. `killNode` sets `Ready` false and the post-quiet probe does not set it back. The probe still passed, or the checker would have been `UNKNOWN`.

The isolate action is `reset`. That choice was made while a healed blackhole still spliced the Raft stream (item 4). The splice close is in place now; this seed was not re-run with `blackhole`.

## hraftd `97a01648`, `http_kv`

Binary: `examples/hraftd/hraftd`. Config: `examples/hraftd/hraftd.yaml`.

1. **`value_from` did not expand `${key}`.** A GET body is `{"k0":"v1-0"}`. `value_from` is now expanded like `body`. The seed-1 bundle history has a get of `k0` with value `v1-0`.

2. **`-save-bundle` with `-n 5` does not run.** Fuss printed `fuss: -save-bundle records one seed; -n must be 1` and `exit status 2`. That check is unchanged. `-save-failed-bundle <dir>` is the multi-seed path: each non-PASS seed is written to `<dir>/seed-<seed>/`, and `-replay-bundle` loads that directory.

3. **`eventually_available` takes `n`, not `consecutive_ok`.** `n` is the quiet-window streak of `ok` operations. The five-seed run had `eventually_available` hits 5.

4. **`-raddr` is the bind address and the address `-join` registers.** There is no advertise flag. Followers `POST /join` with the peer-proxy address after start. **Reading:** re-joining node 0, the only voter, returned HTTP 500 and left its Raft address on the bind port.

5. **The post-quiet ready probe did not see `${node_proxy_addr_i}`.** The shell expanded it to empty, and the probe `POST`ed `"addr":""`. The first five seeds were all `UNKNOWN` / `nodes_not_ready`, while the porcupine property note was `porcupine: history is not linearizable`. The recheck now copies the peer addresses. The next five seeds: 1, 3, and 4 `NONLINEARIZABLE` / `nonlinearizable`; 2 and 5 `UNKNOWN` / `nodes_not_ready`. `set` and `eventually_available` hit on all five. `fault_during_inflight` and `fault_action_fired_isolate_node` hit on all five. `porcupine.html` for a later seed-1 run is `notes/runs/hraftd-seed1/porcupine.html`. That run's verdict was also `NONLINEARIZABLE`.

`notes/hraftd-prediction.md` expected a mix and said it was wrong if every trial was `UNKNOWN`. The first five were all `UNKNOWN`. That prediction was wrong. `notes/hraftd-prediction-2.md` expected the mix above. It matches.

Seed 5's node 1 `exit_status` was `1`. **Reading:** `target.log` shows Raft dialing the bind port and getting `connection refused` while that process is down. `-join` still targets node 0, so a follower restart while node 0 is down exits before `exec`.

## nats-server v2.15.3, JetStream KV, `exec_op`

Binaries: `/usr/local/bin/nats-server` v2.15.3, `/usr/local/bin/nats` 0.5.2. Config: `examples/nats-kv/nats-kv.yaml`. Machine: darwin arm64. `fuss: nats-kv = /usr/local/bin/nats-server (from PATH)`.

1. **The quiet recheck dropped `${port_monitor}`.** The first five seeds, seed 1, `-save-failed-bundle /tmp/nats-runs`, all printed `"verdict":"UNKNOWN"`, label `before-start`. The trial JSON has no `reason` field. Evidence `acks` was 1 on each, which is the placeholder `CheckNamed` stores on an early return, not an operation count. Restarted nodes were `ready: false`; one node on each seed stayed `ready: true`. Summary: `acks` 5, `fail` 5, `pass` 0. `porcupine` hits 4, `passed` false. `set` and `eventually_available` hit 5. The three `sometimes` properties hit 5. Only `/tmp/nats-runs/seed-3/porcupine.html` was written. `notes/nats-kv-prediction.md` expected no `UNKNOWN`. That prediction was wrong on the verdict. It was right that `info_ops_observed` would hit.

2. **Named ports are now on the runtime.** `waitNodeReadyAlive` calls `setNodePortVars` with the ports the start command saw. `TestQuietRecheckKeepsNamedPorts` checks `${port_monitor}`. The second five seeds, seed 1, `/tmp/nats-runs-2`: seed 1 `PASS`, seed 2 `PASS`, seed 3 `NONLINEARIZABLE`, seed 4 `PASS`, seed 5 `NONLINEARIZABLE`. Summary: `acks` 531, `fail` 2, `pass` 3, `failed_bundles` seed-3 and seed-5. `porcupine` hits 3, `passed` false. `set`, `eventually_available`, `fault_during_inflight`, `info_ops_observed`, and `fault_action_fired_isolate_node` each hits 5, `passed` true. `notes/nats-kv-recheck-prediction.md` expected no `UNKNOWN`. It matches. Seed 3 evidence `acks` 198. Seed 5 evidence `acks` 330. **Reading:** those match the `ok` lines in each `history.jsonl` (seed 3: 69 put, 129 get; seed 5: 142 put, 188 get). The `PASS` trials printed `acks` 1. That is the placeholder `finishCheck` returns when every property passes and the base result was `Evidence: 1`. It is not the operation count. The same trials still list a restarted node `ready: false`, same bookkeeping as rqlite: `killNode` clears `Ready`, and a passing probe does not set it back.

`client_advertise` is set to the client proxy. **Reading:** seed 1's generated `nats.conf` (first run) had cluster `listen` on the real port and `advertise` on the peer proxy, and the log had `JetStream cluster new metadata leader` before the first `kill_node`. The server creates another `jetstream` directory under `store_dir`.

## openraft `raft-kv-memstore`, `http_kv`

Source inspected: `github.com/databendlabs/openraft` commit `0b464b9da0e53fdd887cfef24b8235fcfbb0ee5c`, `examples/raft-kv-memstore`. Config: `examples/openraft-kv/openraft-kv.yaml`. No Fuss `-json` verdict. `cargo`, `rustc`, and `rustup` are not on PATH (`/Users/kriyanshi/.cargo/env` is missing), so `raft-key-value` was not built and the five-seed run was not started.

`--api-addr` and `--raft-addr` are each the bind address and the address stored in membership. There is no advertise flag. Peer `isolate_node` / `partition_halves` are not in the nemesis. Client HTTP goes through `client_via_proxy`.

Every admin, write, and read handler returns HTTP 200 with a serde `Result` (`{"Ok":...}` or `{"Err":...}`). A non-empty scalar was the only `error_from` match, so `Err.QuorumNotEnough` and `Err.LogEntryDiscarded` stayed `ok` when the path was `Err.ForwardToLeader.reason`. `error_from: Err` now treats any non-null JSON value as `fail`, including those objects. A missing path, JSON null, or empty string does not. A missing `Ok.value.value` on a get is an absent read, which is the right mapping for `{"Ok":{"value":null}}`.

The log and the map are in memory. `kill_node` restarts an empty process. `ready.sh` writes `$dir/bootstrapped` after the first `/change-membership` and does not call `/init` again.
