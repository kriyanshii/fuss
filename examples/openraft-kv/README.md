# OpenRaft KV

Three-node [`databendlabs/openraft`](https://github.com/databendlabs/openraft) `examples/raft-kv-memstore` cluster. The binary is `raft-key-value`. Not run in CI. Leave the binary unmodified.

The source checked for this file is commit `0b464b9da0e53fdd887cfef24b8235fcfbb0ee5c` (a shallow clone of the default branch).

```sh
git clone https://github.com/databendlabs/openraft.git
cargo build --manifest-path openraft/examples/raft-kv-memstore/Cargo.toml --bin raft-key-value
# Put target/debug/raft-key-value on PATH, or at examples/openraft-kv/raft-key-value.
go build -o fuss .
./fuss -target-config=examples/openraft-kv/openraft-kv.yaml -n=5 -seed=1 -save-failed-bundle /tmp/openraft-runs -json
```

Run Fuss from the repository root. The start and ready commands use paths relative to the current directory.

`-save-bundle` records one seed, so `-n` must be 1. `-save-failed-bundle <dir>` writes each non-PASS seed to `<dir>/seed-<seed>/`.

## Addresses

The CLI is `--id`, `--api-addr`, and `--raft-addr`. There is no separate advertise flag. `start_example_raft_node` binds each address and stores the same string on the process. `POST /init` with `[]` publishes this process's `raft_addr` and `api_addr` as the node record. Peers dial `raft_addr` for `/append`, `/vote`, `/snapshot`, and `/transfer-leader`. Clients use `api_addr`.

That is one port for bind and for the address other nodes dial. `isolate_node` and `partition_halves` are not in this file. They would sit on a peer proxy the Raft dial never enters. The nemesis is `isolate_client` (`blackhole` and `reset`) on the HTTP client proxies, `kill_node` (`restart_delay: 2s`), `pause_node` (`pause_duration: 2s`), and `heal_all`. `properties.sometimes` watches `fault_action_fired` for `kill_node`.

| Flag | Value |
| --- | --- |
| `--id` | `1`, `2`, `3` for Fuss nodes `0`, `1`, `2` |
| `--api-addr` | `127.0.0.1:${client_port}`. Clients use `client_via_proxy` and `${client_addrs}`. |
| `--raft-addr` | `127.0.0.1:${port_raft}`. Not proxied. |

The log and the key-value map are in memory (`log-mem`, `sm-mem`). A restarted process does not keep them. The ready probe does not re-run `/init` after the first successful membership change.

## Cluster setup

`examples/openraft-kv/ready.sh` on node 0, once:

1. `POST /init` with body `[]`. A second call returns `NotAllowed` and is ignored.
2. `POST /add-learner` with `{"node_id":2,"api_addr":"127.0.0.1:<api>","raft_addr":"127.0.0.1:<raft>"}`, then the same for node id 3. The ports come from files the start script writes under the trial directory.
3. `POST /change-membership` with `[1,2,3]`.

Each of those calls returns HTTP 200. Success is a JSON body that contains `"Ok"`. The other nodes only need `GET /readiness` to answer. Fuss retries the ready command until `READY` or 30s.

## HTTP

| Call | Request | Body |
| --- | --- | --- |
| Write | `POST /write` | `{"Set":{"key":"${key}","value":"${value}"}}` |
| Linearizable read | `POST /linearizable_read` | `"${key}"` (a JSON string) |
| Local read | `POST /read` | same body. This scenario does not call it. |
| Follower linearizable read | `POST /follower_read` | same body. This scenario does not call it. |

`/read` reads this node's state machine and can be stale. `/linearizable_read` calls `ensure_linearizable` on the leader, then reads locally. `/follower_read` asks the leader to authorize the read. The workload uses `/linearizable_read`.

A successful read is `{"Ok":{"value":{"value":"<text>","version":<log index>}}}` or `{"Ok":{"value":null}}` when the key is absent. `value_from: Ok.value.value` is the text. A null `value` has no `value.value`, which `http_kv` records as an absent `ok` get.

A write or linearizable read this node will not serve is still HTTP 200, with `{"Err":...}`. `error_from: Err` records `fail` when that value is present and not null, so `ForwardToLeader`, `QuorumNotEnough`, and `LogEntryDiscarded` are all refusals. A missing `Err`, JSON null, or empty string is not.

`eventually_available` takes `n`, not `consecutive_ok`. `n: 5` is the quiet-window streak of `ok` operations. The workload `rate` is 20.

## Limits

- The start script writes `api_<i>` and `raft_<i>` under the trial directory before `exec`. It does not write a data directory for the server. The server keeps the log and the map in memory.
- `--raft-addr` is the bind address and the dial address. Peer-proxy faults are not in the nemesis.
- `error_from: Err` fails the call when `Err` is any non-null JSON value. There is no Fuss `-json` verdict until `raft-key-value` is built and run.
- No Fuss verdict is recorded in this tree until a run prints one. Quote the `-json` verdict.
