# hraftd

Three-node [hraftd](https://github.com/otoolep/hraftd) cluster. Pin **`97a01648bd9e`** (`v0.0.0-20260816012201-97a01648bd9e`, 2026-08-16). Not run in CI.

```sh
GOBIN="$(pwd)/examples/hraftd" go install github.com/otoolep/hraftd@v0.0.0-20260816012201-97a01648bd9e
go build -o fuss .
./fuss -target-config=examples/hraftd/hraftd.yaml -n=5 -seed=1 -json
```

`-save-bundle` records one seed, so `-n` must be 1. `-save-failed-bundle <dir>` writes each non-PASS seed to `<dir>/seed-<seed>/` and accepts `-n` greater than 1.

## What the process is given

| Flag | Value |
| --- | --- |
| `-id` | `node_0`, `node_1`, `node_2` |
| `-haddr` | `127.0.0.1:${client_port}` (the HTTP listen). Clients use `client_via_proxy`. |
| `-raddr` | `127.0.0.1:${port}` (the Raft bind). Peer proxies listen in front of that port (`via_proxy`, `per_node_owned`). |
| `-join` | Node 0 has none. Nodes 1 and 2 pass node 0's HTTP address, `${client_addr_0}`. |
| directory | `${data_dir}` |

hraftd's `-raddr` is both the bind address and the address `-join` registers. There is no advertise flag. A follower's ready command `POST`s `/join` again with `addr` set to `${node_proxy_addr_i}`, which is the documented join body (`id`, `addr`). The leader then dials the proxy. Re-joining node 0, the only voter, returns HTTP 500 and leaves its Raft address on the bind port, so dials to node 0 do not cross a proxy.

The start script waits until node 0's `/status` reports `"leader":{"id":"node_0"` before exec'ing a follower. hraftd exits if the `-join` dial fails.

## HTTP

`POST /key` with `{"<key>":"<value>"}` succeeds on the leader. Any other node returns an HTTP error. `GET /key/<key>` is answered from that node's local map, including a missing key as `{"<key>":""}`.

From the upstream README, section "Stale reads":

> Because any node will answer a GET request, and nodes may "fall behind" updates, stale reads are possible.

`value_from: "${key}"` reads that object. `http_kv` expands `value_from` before the lookup. `eventually_available.n` is how many `ok` operations in a row the quiet window must contain. There is no `consecutive_ok` field.

## This machine

`go test ./...` passed. Seed 1, `-n 5`, after the ready recheck kept `${node_proxy_addr_i}`:

| Seed | Fuss verdict | Reason |
| --- | --- | --- |
| 1 | `NONLINEARIZABLE` | `nonlinearizable` |
| 2 | `UNKNOWN` | `nodes_not_ready` |
| 3 | `NONLINEARIZABLE` | `nonlinearizable` |
| 4 | `NONLINEARIZABLE` | `nonlinearizable` |
| 5 | `UNKNOWN` | `nodes_not_ready` |

`set`, `eventually_available`, `fault_during_inflight`, and `fault_action_fired_isolate_node` hit on all five. A later `-n 1 -seed 1 -save-bundle notes/runs/hraftd-seed1` was `NONLINEARIZABLE` and wrote `porcupine.html`. These are not a 10-replay of one state. The `NONLINEARIZABLE` histories are the stale local GET the README describes.

## Promises

The check is the one above: a local GET may be stale, and a POST that is not the leader fails. Porcupine scores the history. A `NONLINEARIZABLE` result is that documented staleness, not a separate claim found by replaying one seed ten times.
