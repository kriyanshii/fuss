# War stories

fuss puts an unmodified binary behind a userspace L4 proxy. The proxy attributes sockets by PID (`per_node_owned`), so a scenario can reset, blackhole, or degrade a link with no root and no iptables. Client calls are written to `history.jsonl` on the same clock as `nemesis_events.json`, and Porcupine decides whether that history is linearizable. A failing trial is a directory (`-save-failed-bundle`, or `-save-bundle` when `-n` is 1) that `-replay-bundle` loads again. A handler test does not assemble a Raft byte stream or a concurrent client history. The failures below showed up only when the real process and the recorded history were both present.

## When a userspace blackhole spliced etcd's Raft stream

`ModeBlackhole` keeps the TCP connection open. In the relay, that mode calls `markDirtyLocked` and returns without writing the chunk to the other side (`proxy/fault.go`). The sender's `Write` succeeds because the proxy already read the bytes. The peer blocks until its own read deadline. `proxy/fault.go` says this is not TCP send-buffer backpressure.

The first heal left that socket up. `notes/friction.md` records the reading: the proxy kept the sockets open, discarded bytes, then forwarded later bytes on the same connection. TCP does not do that. A dropped segment is retransmitted, and the receiver's stream has no hole. Closing the connection starts a new stream. Discarding bytes the sender already handed to `Write`, then forwarding the next bytes on the same connection, deletes the middle of a length-prefixed frame. The following bytes are parsed as the next message.

etcd v3.7.4, seed 1, both read levels, was `UNKNOWN` with `reason` `nodes_not_ready`. Node 2's `exit_status` was `2`. Fuss did not turn the crash into a verdict. `target.log` did. The line is `panic: index, 54, is out of range [1]` in `raft.(*raftLog).maybeAppend`, called from `handleAppendEntries`.

`Link.Heal` is `Apply` of `ModeHealthy` on `DirBoth`. That path calls `takeDirtyLocked` and `closeTracked`: a connection that discarded bytes on a direction which is no longer `ModeBlackhole` is closed (`proxy/fault.go`, `proxy/link.go`). `TestBlackholeHealClosesDirtyConn` is the contract. A socket that wrote during the blackhole is closed on `Heal`. A socket that stayed idle still echoes. A new dial echoes. Peers have to dial again. `382bc9e` (`proxy: add blackhole and degraded L4 modes`) is the commit that contains this close.

Seed 1 after that change, on this machine, etcd v3.7.4:

| Config | Fuss verdict | Reason |
| --- | --- | --- |
| `etcd-serializable.yaml` | `NONLINEARIZABLE` | `nonlinearizable` |
| `etcd-linearizable.yaml` | `PASS` | |

Both trials had node 2 `ready` true, `exit_status` `-1`, `stop` `SIGTERM`. `fault_during_inflight` hits were 1. The kept serializable `target.log` has no `panic`. `porcupine.html` is `notes/runs/etcd-blackhole-s/porcupine.html`. The linearizable bundle is `notes/runs/etcd-blackhole-l`. The same pair of verdicts on seed 1 with `mode: reset` is `notes/runs/etcd-reset-s` and `notes/runs/etcd-reset-l`. The serializable `NONLINEARIZABLE` is etcd's documented stale range. The linearizable range on the same cluster and the same fault passed.

## fuss catching a real bug in repkv's correct mode

`examples/repkv/repkv-http.yaml` runs three nodes in `-mode correct` under `http_kv` (3 workers, keyspace 1, targets `${client_addrs}`). The nemesis isolates `node_0` with `mode: reset` for 400 ms. `TestRepkvHistoryBundle` requires that scenario to exit 0 and to write no `porcupine.html`. Seed 1 exited 1. Fuss's verdict was `NONLINEARIZABLE`.

`commit` replicates, then stores on the leader (`examples/repkv/main.go`). `replicateAll` sends `REPL <leader> <key> <value>`. The peer stores the key when `cur == lead` and answers `REPL_OK`. The leader writes `n.store` only after every follower has acked, then the handler returns HTTP 200. Before `01da2a7`, `GET /k/<key>` returned `n.store` on every node. A follower could answer with a value the leader had not stored yet.

The seed-1 history on `k0`, in nanoseconds from the schedule start:

| op | process | call_ns | return_ns | status |
| --- | --- | --- | --- | --- |
| `put k0 v1-0` | 1 | 100313500 | 101022834 | ok |
| `put k0 v2-7` | 2 | 700605084 | 1028103542 | ok |
| `get k0 v2-7` | 3 | 933655542 | 934003834 | ok |
| `get k0 v1-0` | 1 | 967823459 | 968232459 | ok |

`nemesis_events.json` for that trial: `isolate_node` of `node_0`, `mode` `reset`, `direction` `both`, at `350561084`; `heal_all` (`details` `recovery`) at `750722542`. The put of `v2-7` starts during the cut and returns after the heal. The get of `v2-7` returns while that put is still open. The next completed get returns `v1-0`.

`porcupineOps` (`history/check.go`) drops any status other than `ok` or `info`. Failed puts never enter the model. `stepKV` lets an `ok` put move the register to that value. An `ok` get is legal only when the register exists and equals the returned value. The get of `v2-7` can sit inside the open put. The get of `v1-0` returns after that get has already returned, so it cannot observe the previous register. Porcupine reports `NONLINEARIZABLE`.

`01da2a7` (`serve correct-mode reads from the leader only`) changes `handleK`. In `correct` mode, a `GET` whose `n.leader != n.id` returns HTTP 503 with body `not leader`. `ClassifyHTTP` maps that 503 to `fail`, and `porcupineOps` omits it. A successful read is the leader's store, which `commit` updates only after `replicateAll` returns. Bug mode still serves the local map. The Porcupine check was not changed. `go test ./...` passed after the change: `fuss`, `examples/repkv`, `history`, and `proxy`.

## What real targets forced us to fix

`notes/friction.md` is the log. Verdict lines are Fuss `-json`. Lines marked as a reading are from a target log or from reading code.

**etcd v3.7.4.** `PUT /v3/kv/put` with body `k0` returned `501` and `{"code":12, "message":"Method Not Allowed"}`. `POST` with `{"key":"azA=","value":"djE="}` returned `200` and a header object. `POST /v3/kv/range` for a missing key returned `200` with a header and no `kvs`. `http_kv` gained `headers`, `value_from`, `value_encoding: base64`, `error_from`, and `${key_b64}` / `${value_b64}` / `${old_b64}`. A 301, 302, 307, or 308 is followed with the original method and body (`applyHTTPCall` in `workload.go`). Seed 1 with only the inbound proxy blackholed was `PASS`, and `nemesis_events.json` said `shared_outbound_unfiltered`: the minority's dials land on the other nodes' shared proxies. `ApplyWhere` now puts the same profile on those tagged connections. `reset` already used `CutWhere`.

**rqlite v10.7.4**, `examples/rqlite/rqlite-http.yaml`, seed 1. Verdict `PASS`. `fault_during_inflight` hits were 1. The nemesis drew `kill_node` on node 1 twice (restart after 1s) and did not draw the isolate. History: 110 ok puts, 136 ok gets, 11 put `info`, 6 get `info`. Get values were the raw strings (`v1-0`), so `value_from: results.0.values.0.0` matched the JSON. No redirect failure showed up on this seed. This seed was not re-run with `blackhole`.

**hraftd `97a01648`.** A GET body is `{"k0":"v1-0"}`. `value_from` is expanded like `body`, so `${key}` selects that field. The seed-1 history has a get of `k0` with value `v1-0`. `-save-bundle` with `-n 5` prints `fuss: -save-bundle records one seed; -n must be 1` and exits 2. `-save-failed-bundle <dir>` writes each non-PASS seed to `<dir>/seed-<seed>/`. `eventually_available` takes `n`, not `consecutive_ok`. The five-seed run had `eventually_available` hits 5. `-raddr` is the bind address and the address `-join` registers. There is no advertise flag. Re-joining node 0, the only voter, returned HTTP 500 and left its Raft address on the bind port. The first post-quiet probe expanded `${node_proxy_addr_i}` to empty and POSTed `"addr":""`. Those five seeds were all `UNKNOWN` / `nodes_not_ready`. After the recheck copied the peer addresses, seeds 1, 3, and 4 were `NONLINEARIZABLE` / `nonlinearizable`, and seeds 2 and 5 were `UNKNOWN` / `nodes_not_ready`. `set` and `eventually_available` hit on all five. `porcupine.html` for a later seed-1 run is `notes/runs/hraftd-seed1/porcupine.html`. That run's verdict was `NONLINEARIZABLE`. Seed 5's node 1 `exit_status` was `1`. The log shows Raft dialing the bind port and getting `connection refused` while that process is down.

**Consul.** `201ff75` adds `examples/consul/consul-consistent.yaml` and `examples/consul/consul-stale.yaml`. No Fuss `-json` verdict for those scenarios is recorded. Consul's agent config uses `ports.server` for both `RPCBindAddr` and `RPCAdvertiseAddr`. Members dial `127.0.0.1:${port_server}`, the proxy upstream, not the proxy listen address. `isolate_node` and `partition_halves` act on the peer proxies and do not cut a Raft dial that never enters the proxy. Serf LAN is left direct: it speaks TCP and UDP, and the proxy is TCP only. `kill_node` still stops the process. The same bind-and-advertise shape is what hraftd's `-raddr` already forced.
