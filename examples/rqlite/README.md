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
| Expected verdict | **PASS** when node 0 was leader, the execute body acknowledged the row, and a `strong` read on the new leader returns it after the cut. A failed or unparseable read is **UNKNOWN**. A successful read with no row is **LOST WRITE**. | Derived from the sources above |

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
2. Wait until node 0’s `/status` reports `Leader`. If it does not, the
   verdict is `UNKNOWN reason=leader_not_node_0` and nothing is acknowledged.
3. Create a table and `POST` one row to node 0 **without** `?queue`. HTTP 200
   is not enough: the JSON body must be a `results` entry with no `error`,
   and the insert must report `rows_affected` of at least 1. Only then is
   the write acknowledged.
4. Isolate `node_0` with `direction: both`. No pause. The driver waits for
   `${dir}/isolate_applied.json` (`trigger_fired` true, `links_cut` > 0,
   `target` `node_0`) before it reads. If that file does not appear, the
   verdict is `UNKNOWN reason=cut_not_applied`.
5. Poll `/status` on nodes 1 and 2 for up to 10s until one reports
   `store.raft.state == Leader`.
6. `GET /db/query?level=strong` for that row from the new leader.

A query is a verdict only when it returns HTTP 200 and a `results` entry
with `columns` or `values` and no `error`. Row present →
`PASS read_from=node_1|node_2 raft_state=Leader after_cut=yes`. Row absent →
`LOST WRITE read_from=... raft_state=Leader after_cut=yes value_missing=yes`.
Timeout (`http` code `000`) → `UNKNOWN reason=read_timeout`. Any other HTTP
status → `UNKNOWN reason=read_failed`. A statement `error` →
`UNKNOWN reason=read_error`. Unparseable JSON → `UNKNOWN reason=read_malformed`.
Check rules require the full PASS or LOST WRITE line. Trial JSON
`evidence.check.out` carries that line. The line is written to
`${home}/result.txt` (`${dir}/home/result.txt`). A control plants the same
path: `examples/rqlite/fixture-lost/result.txt` is a LOST WRITE line and must
be judged `LOST WRITE`. A control with no planted file must be judged
`UNKNOWN` with `reason=result_absent`. Fuss runs those controls before any
seed.

Save the run and replay it with the same bundle format as repkv:

```sh
./fuss -target-config=examples/rqlite/rqlite.yaml -seed=1 -n=1 -save-bundle=/tmp/rqlite-bundle
./examples/rqlite/replay.sh /tmp/rqlite-bundle
```
