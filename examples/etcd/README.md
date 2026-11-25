# etcd (v0 isolate)

Three-node etcd cluster exercised through Fuss userspace proxies. Pin
**etcd v3.7.4** (and the `etcdctl` shipped in that same archive). Not run in CI.

```sh
# official archive contains both etcd and etcdctl; etcd --version prints 3.7.4
# https://github.com/etcd-io/etcd/releases/tag/v3.7.4
go build -o fuss .
./fuss -target-config=examples/etcd/etcd.yaml -n=10 -seed=1 -json
```

## Promises (write the verdict first)

| Fact | Pin | Source |
| --- | --- | --- |
| Version | **v3.7.4**, published 2026-11-19T00:06:43Z. Not a same-day release (today is 2026-11-24). Previous patch is v3.7.3 (2026-10-22). | [Release v3.7.4](https://github.com/etcd-io/etcd/releases/tag/v3.7.4) · [v3.7.3](https://github.com/etcd-io/etcd/releases/tag/v3.7.3) |
| Peer / advertise flags | `--listen-peer-urls` is the real peer listen. `--initial-advertise-peer-urls` is the URL other members dial (one proxy front per member). Client API stays direct: `--listen-client-urls` and `--advertise-client-urls`. | [Configuration options (v3.7)](https://etcd.io/docs/v3.7/op-guide/configuration/) · [embed.Config defaults @ v3.7.4](https://github.com/etcd-io/etcd/blob/v3.7.4/server/embed/config.go) |
| Write ack | A completed KV operation is committed through consensus. The client learns that when the server responds. Do not treat a timeout as an ack. | [API guarantees (v3.7)](https://etcd.io/docs/v3.7/learning/api_guarantees/) (“Operation completed”) |
| Read level | **Linearizable** (etcdctl default). Do not pass `--consistency=s`. Serializable reads may be stale. | [API guarantees (v3.7)](https://etcd.io/docs/v3.7/learning/api_guarantees/) · [etcdctl README @ v3.7.4](https://github.com/etcd-io/etcd/blob/v3.7.4/etcdctl/README.md) |
| Election defaults | Heartbeat interval **100ms** (`TickMs`), election timeout **1000ms** (`ElectionMs`). Scenario waits up to **10s** for a new leader. | [config.go @ v3.7.4](https://github.com/etcd-io/etcd/blob/v3.7.4/server/embed/config.go) (`TickMs: 100`, `ElectionMs: 1000`) · [Tuning (v3.7)](https://etcd.io/docs/v3.7/tuning/) |
| Isolate shape | **Symmetric** isolate of `node_0` (`direction: both`) on `proxy_mode: per_node_owned`. One advertised peer URL per member, same as rqlite, but owner PID separates flows on that shared inbound proxy, so outbound from node 0 is cut without cutting 1↔2. No `fault.pause`. | Fuss `topology.proxy_mode` / `fault.isolate.direction` in `skills/fuss/CONFIG.md` |
| Expected verdict | **PASS** when `node_0` was the leader before the cut and a linearizable `get` on the new leader still returns the value. **UNKNOWN** when `node_0` did not win the election (`reason=leader_not_node_0`), or when the get fails. A missing value after a successful get is **LOST WRITE**. | Derived from the sources above. No official isolate checklist. |

**Peer transport (reading, not a verdict).** At v3.7.4, `rafthttp` keeps long-lived streams to each peer and redials when a stream dies; a pipeline of short HTTP posts is the fallback until the stream is up. A cut has to close the current sockets and refuse the redial. If a redial is accepted after the cut, Fuss’s `-json` evidence is the record. Do not paper over that with a sleep. Source: [rafthttp/peer.go @ v3.7.4](https://github.com/etcd-io/etcd/blob/v3.7.4/server/etcdserver/api/rafthttp/peer.go).

**Jepsen.** [etcd 3.4.3](https://jepsen.io/analyses/etcd-3.4.3) found key-value operations strict-serializable under partitions, and locks unsafe. That is not a certification of v3.7.4. This scenario does not use locks or watches.

**Still flagged**

- Advertise-peer URLs are documented for membership reachability, not for “put a proxy here.”
- v3.7.4’s API page says strict serializability for KV calls, and linearizability by default except watches. The check uses a linearizable get, which is the default, not `--consistency=s`.
- No primary-source “PASS for isolate-then-read” matrix. The expected verdict is derived.

## Scenario

1. Three members boot with the same `--initial-cluster`, each advertising its proxy as `--initial-advertise-peer-urls` and listening for peers on the real port behind that proxy. Client URLs are direct.
2. Poll `etcdctl endpoint status` on node 0 for up to 10s. This version always cuts `node_0`. If node 0 is not the leader, the verdict is `UNKNOWN reason=leader_not_node_0` and nothing is acknowledged. Any member can win the first election; this scenario does not move the leader.
3. `etcdctl put` the key to node 0. etcdctl returns only after the write is committed. A put that fails is `UNKNOWN reason=write_failed` and is not an acknowledgement.
4. Isolate `node_0` with `direction: both`. No pause. The driver waits for `${dir}/isolate_applied.json` (`trigger_fired` true, `links_cut` > 0, `target` `node_0`) before it reads. If that file does not appear, the verdict is `UNKNOWN reason=cut_not_applied`.
5. Poll `etcdctl endpoint status` on nodes 1 and 2 for up to 10s until one reports `header.member_id == leader`.
6. `etcdctl get` that key from the new leader, `-w json`, default linearizable read, with a 2s dial timeout and a 5s command timeout.

A get is a verdict only when etcdctl exits 0 and the JSON parses. No `kvs` → `LOST WRITE read_from=node_1|node_2 leader=self after_cut=yes value_missing=yes`. Value `ok` → `PASS read_from=... leader=self after_cut=yes`. A different value → `UNKNOWN reason=read_unexpected`. A non-zero etcdctl status, including a timeout, → `UNKNOWN reason=read_failed`. Unparseable JSON → `UNKNOWN reason=read_malformed`. Check rules require the full PASS or LOST WRITE line. Trial JSON `evidence.check.out` carries that line.

`-save-bundle` and `-replay-bundle` use the same bundle format as `examples/repkv/bundles/lost-write`.
