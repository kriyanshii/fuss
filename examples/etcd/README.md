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
| Isolate shape | **Symmetric** isolate of the elected leader (`direction: both`) on `proxy_mode: per_node_owned`. The driver writes that member's `node_N` into `${dir}/leader_node` after the write is acknowledged, and Fuss cuts whatever name is in the file. One advertised peer URL per member. Owner PID separates flows on that shared inbound proxy, so the leader's outbound links are cut and the other pair keeps talking. No `fault.pause`. | Fuss `topology.proxy_mode` / `fault.isolate.node_file` in `skills/fuss/CONFIG.md` |
| Expected verdict | **PASS** when the cut's `target` is the member that acknowledged the write and a linearizable `get` on a surviving leader still returns the value. **UNKNOWN** when leadership changes before the cut (`reason=leader_changed`), when the applied cut names a different member (`reason=cut_missed_leader`), or when the get fails. A missing value after a successful get is **LOST WRITE**. | Derived from the sources above. No official isolate checklist. |

**Peer transport (reading, not a verdict).** At v3.7.4, `rafthttp` keeps long-lived streams to each peer and redials when a stream dies; a pipeline of short HTTP posts is the fallback until the stream is up. A cut has to close the current sockets and refuse the redial. If a redial is accepted after the cut, Fuss’s `-json` evidence is the record. Do not paper over that with a sleep. Source: [rafthttp/peer.go @ v3.7.4](https://github.com/etcd-io/etcd/blob/v3.7.4/server/etcdserver/api/rafthttp/peer.go).

**Jepsen.** [etcd 3.4.3](https://jepsen.io/analyses/etcd-3.4.3) found key-value operations strict-serializable under partitions, and locks unsafe. That is not a certification of v3.7.4. This scenario does not use locks or watches.

**Still flagged**

- Advertise-peer URLs are documented for membership reachability, not for “put a proxy here.”
- v3.7.4’s API page says strict serializability for KV calls, and linearizability by default except watches. The check uses a linearizable get, which is the default, not `--consistency=s`.
- No primary-source “PASS for isolate-then-read” matrix. The expected verdict is derived.

## Scenario

1. Three members boot with the same `--initial-cluster`, each advertising its proxy as `--initial-advertise-peer-urls` and listening for peers on the real port behind that proxy. Client URLs are direct.
2. Poll `etcdctl endpoint status` on all three members for up to 10s. The member whose `header.member_id` equals `leader` is the leader. If none, or more than one, the verdict is `UNKNOWN reason=no_leader` and nothing is acknowledged.
3. `etcdctl put` the key to that member. etcdctl returns only after the write is committed. A put that fails is `UNKNOWN reason=write_failed` and is not an acknowledgement. The result line records that member's id as `leader_id`.
4. Check the same member is still leader, then write its `node_N` to `${dir}/leader_node` and ask Fuss to cut. If the leader changed, the verdict is `UNKNOWN reason=leader_changed` and there is no cut and no read. The driver waits for `${dir}/isolate_applied.json` (`trigger_fired` true, `links_cut` > 0, `target` equal to that `node_N`). A different target is `UNKNOWN reason=cut_missed_leader`. A missing file is `UNKNOWN reason=cut_not_applied`.
5. Poll `etcdctl endpoint status` on the other two members for up to 10s until one reports itself leader.
6. `etcdctl get` that key from the new leader, `-w json`, default linearizable read, with a 2s dial timeout and a 5s command timeout.

A get is a verdict only when etcdctl exits 0 and the JSON parses. No `kvs` → `LOST WRITE read_from=node_N leader=self after_cut=yes value_missing=yes leader_id=... cut_target=node_M key=...`. Value `ok` → `PASS read_from=node_N leader=self after_cut=yes leader_id=... cut_target=node_M key=...`. The `key` is the key the get requested. A different value → `UNKNOWN reason=read_unexpected`. A non-zero etcdctl status, including a timeout, → `UNKNOWN reason=read_failed`. Unparseable JSON → `UNKNOWN reason=read_malformed`. Check rules require the PASS or LOST WRITE line for whichever survivor answered. Trial JSON `evidence.check.out` carries that line, and `evidence.isolate.target` is the member Fuss cut.

Seeds 1 to 4 on this machine, etcd v3.7.4, exit 0:

| Seed | Cut target | Leader id | Read |
| --- | --- | --- | --- |
| 1 | `node_0` | `208163414524601980` | `PASS read_from=node_1 leader=self after_cut=yes` |
| 2 | `node_0` | `10800844815880550838` | `PASS read_from=node_1 leader=self after_cut=yes` |
| 3 | `node_1` | `4754515268961374412` | `PASS read_from=node_0 leader=self after_cut=yes` |
| 4 | `node_2` | `11894529343470141433` | `PASS read_from=node_0 leader=self after_cut=yes` |

Each trial had `links_cut` 4 and `trigger_fired` true. The read came from a member other than the cut target.

`examples/etcd/bundles/pass` is one saved seed-1 run of this scenario. The bundle records the leader id, the member Fuss cut, and the key the linearizable get checked.

`-save-bundle` and `-replay-bundle` use the same bundle format as `examples/repkv/bundles/lost-write`. The replay prints `ack: key=...` and `read: key=...` and fails when those keys differ. It also prints the saved cut target and the replayed cut target. A later run may elect a different member. The replay still passes when that member was the leader before a two-way cut and a different member returned the key. A cut of a follower does not pass. If the leader changes before the cut, the trial is `UNKNOWN` with `reason=leader_changed` or `reason=cut_missed_leader`.
