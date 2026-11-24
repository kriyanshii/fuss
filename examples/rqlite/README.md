# rqlite (v0 isolate)

Three-node rqlite cluster exercised through Fuss userspace proxies. Pin
**rqlited v10.7.4** (the patch release before v10.7.5) on `PATH`, then:

```sh
# build or install rqlited v10.7.4 so `rqlited -version` prints v10.7.4
go build -o fuss .
./fuss -target-config=examples/rqlite/rqlite.yaml -n=10 -seed=1 -json
```

CI does not install or run rqlite; this example is local-only.

## Promises (write the verdict first)

| Fact | Pin | Source |
| --- | --- | --- |
| Version | **v10.7.4** | [Release v10.7.4](https://github.com/rqlite/rqlite/releases/tag/v10.7.4) |
| Peer / advertise flags | `-raft-addr` (real listen), `-raft-adv-addr` (what peers dial), `-http-addr` (client API, direct) | [Configuring rqlite](https://rqlite.io/docs/guides/config/) |
| Write ack | Non-queued `POST /db/execute` is accepted only after a **Raft majority commit** (do not use `?queue`) | [FAQ](https://rqlite.io/docs/faq/) · [Queued Writes](https://rqlite.io/docs/api/queued-writes/) |
| Read level | **`level=strong`** for the post-isolate check (default is `weak`) | [Read Consistency](https://rqlite.io/docs/api/read-consistency/) |
| Election defaults | Heartbeat **1s**, election **1s**, commit **50ms**; leader lease flag `0s` → Raft default **500ms**. Scenario waits up to **10s** for a new leader. | [flags.toml @ v10.7.4](https://github.com/rqlite/rqlite/blob/v10.7.4/cmd/rqlited/flags.toml) · [hashicorp/raft DefaultConfig](https://github.com/hashicorp/raft/blob/v1.8.0/config.go) |
| Expected verdict | **PASS**: majority-committed row remains visible via `strong` read on the new leader after a symmetric isolate of node 0, with no pause | Derived from the sources above |

**v0 limits:** rqlite advertises one Raft address per node. `per_node_owned`
still uses that one inbound proxy per node, and tags each accepted connection
with the source process. `direction: both` cuts connections where node 0 is
the source or the destination and leaves 1↔2 up, so the leader’s outbound
heartbeats stop without `fault.pause`. See
[Configuring rqlite](https://rqlite.io/docs/guides/config/) (`-raft-adv-addr`)
and Fuss `topology.proxy_mode` / `fault.isolate.direction` in
`skills/fuss/CONFIG.md`.

## Scenario

1. Node 0 bootstraps; nodes 1 and 2 `-join` its Raft/mux address
   (`host:port` of node 0’s proxy, not an `http://` URL).
2. Create a table; `POST` one row to node 0 **without** `?queue`; require HTTP 200.
3. Isolate `node_0` with `direction: both`. No pause.
4. Poll `/status` on nodes 1 and 2 for up to 10s until one reports
   `store.raft.state == Leader`.
5. `GET /db/query?level=strong` for that row from the new leader.

Verdicts from the drive script: row present on the new leader →
`PASS read_from=node_1|node_2 raft_state=Leader after_cut=yes` (check rules
require that full line, so a bare `PASS` is not enough); missing →
`LOST WRITE`; no new leader among nodes 1/2 (or Fuss cut evidence
missing) → `UNKNOWN`. Trial JSON `evidence.check.out` carries that line.
