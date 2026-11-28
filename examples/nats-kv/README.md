# NATS JetStream KV

Three-node [`nats-server`](https://github.com/nats-io/nats-server) cluster with one JetStream KV bucket, `fuss_kv`, replicas 3. Not run in CI. Leave the server binary unmodified.

```sh
# Release archives: https://github.com/nats-io/nats-server/releases
# and https://github.com/nats-io/natscli/releases
# Put nats-server on PATH or at examples/nats-kv/nats-server.
# Put the nats CLI on PATH (or set NATS_BIN).
go build -o fuss .
./fuss -target-config=examples/nats-kv/nats-kv.yaml -n=5 -seed=1 -save-failed-bundle /tmp/nats-runs -json
```

Run that from the repository root. The start, ready, and `exec_op` commands use paths relative to the current directory.

`-save-bundle` records one seed, so `-n` must be 1. `-save-failed-bundle <dir>` writes each non-PASS seed to `<dir>/seed-<seed>/`.

## What each node is given

`examples/nats-kv/start-node.sh` writes `${data_dir}/nats.conf` and then `exec`s `nats-server -c` that file. The shell is replaced so the server process owns its cluster sockets (`per_node_owned`).

| Setting | Value |
| --- | --- |
| `server_name` | `node0`, `node1`, `node2` |
| `listen` | `127.0.0.1:${client_port}`. Clients use `client_via_proxy` and `${client_addrs}`. |
| `client_advertise` | `${client_proxy}`, the client proxy for this node. Documented for a server behind a NAT, so a client learns the proxy address rather than the bind port. |
| `http` | `127.0.0.1:${port_monitor}`. Not proxied. Ready uses `GET /jsz`. |
| `jetstream.store_dir` | `${data_dir}/jetstream` |
| `cluster.name` | `fuss-nats` |
| `cluster.listen` | `127.0.0.1:${port_cluster}`. `peers.port_name: cluster` and `via_proxy: true`, so this is the proxy upstream. |
| `cluster.advertise` | `${node_proxy_addr_i}`, the inbound peer proxy. Other servers dial that address. |
| `cluster.routes` | `nats://` plus each non-empty `${peer_addr_j}`. Self is empty. A self-route is ignored by the server. |

`proxy_mode` is `per_node_owned`. `isolate_node` and `partition_halves` sit on those peer proxies, which is the route and Raft path.

## Workload

`exec_op` runs `examples/nats-kv/kv-op.sh`, which calls the `nats` CLI:

| Op | Command |
| --- | --- |
| Put | `nats --server=nats://${target} kv put fuss_kv ${key} ${value}` |
| Get | `nats --server=nats://${target} kv get fuss_kv ${key} --raw` |

`applyExecOp` maps a zero exit to `ok`. An empty get stdout is an absent value. A non-zero exit is `fail`. A command cancelled by the workload timeout is `info`.

The wrapper keeps that mapping for the two NATS outcomes the CLI does not already separate:

- `key not found` exits 0 with an empty stdout, so Porcupine sees an absent read.
- A connection error (`connection refused`, `no servers available`, `i/o timeout`, `timeout`, `connection reset`, `broken pipe`, `no route to host`, `server closed the connection`) sleeps until Fuss cancels the process, so the status is `info`.
- Any other non-zero exit stays `fail`.

Ready creates the bucket once: node 0 runs `nats kv add fuss_kv --replicas=3`, and if that fails it falls through to `nats kv info fuss_kv`. The other nodes wait on `kv info`. Fuss retries the ready command until `READY` or the 30s timeout.

`nats kv add` in CLI 0.5.2 has no allow-direct flag. `nats stream edit KV_fuss_kv --no-allow-direct --force` is the stream flag, and it still asks "Really operate on the KV stream?" and exits with `cannot ask for confirmation without a terminal`. `nats-kv.yaml` leaves the server default, which sets `AllowDirect` on the `KV_fuss_kv` stream. `nats-kv-no-direct.yaml` is the same topology, workload, nemesis, and properties, and passes `no-direct` to `ready.sh`. Node 0 reads `nats stream info KV_fuss_kv --json` and publishes that config with `allow_direct` false to `$JS.API.STREAM.UPDATE.KV_fuss_kv`. **Reading:** `nats.go` v1.56.0 sets `useDirect` from `info.Config.AllowDirect` when `kv get` opens the bucket, so a false flag uses `GetLastMsg` without `DirectGet`. Five seeds from seed 1 of `nats-kv-no-direct.yaml`: seeds 1, 3, 4, and 5 `PASS`; seed 2 `UNAVAILABLE`. Summary `pass` 4, `fail` 1. `porcupine` hits 5, `passed` true. `eventually_available` hits 4, `passed` false. `set` and the three `sometimes` properties each hit 5.

`eventually_available` takes `n`, not `consecutive_ok`. `n: 5` is the quiet-window streak of `ok` operations. The workload `rate` is 20, the same as the hraftd scenario.

## Limits

- The start script writes `nats.conf` under the trial data directory before `exec`. It does not write the JetStream store after `nats-server` is running. The server creates `${data_dir}/jetstream` itself.
- Five seeds from seed 1, after the quiet recheck expands `${port_monitor}`: seeds 1, 2, and 4 `PASS`; seeds 3 and 5 `NONLINEARIZABLE`. Summary `pass` 3, `fail` 2. `porcupine` hits 3, `passed` false. `set`, `eventually_available`, `fault_during_inflight`, `info_ops_observed`, and `fault_action_fired_isolate_node` each hit 5. Quote the `-json` verdict. The bundles are not in this tree.
- `nats server check jetstream` and `kv add` / `kv info` / `kv put` / `kv get` are the CLI commands above. A missing `nats` binary fails the ready command.
