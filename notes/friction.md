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
