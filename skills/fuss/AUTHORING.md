# From a guarantee to a scenario

One path. The program is etcd v3.7.4. The file is `examples/etcd/etcd.yaml`.
Field names and defaults live in `CONFIG.md`. This page is the order those
fields were filled, and what each verdict is allowed to mean.

Fuss does not change the etcd binary. Peer traffic goes through proxies Fuss
owns. The scenario writes its own result line. The verdict is that line,
matched by `check.rules`, printed in `-json`.

## 1. Name the guarantee

Write the sentence under test before the YAML, and name the three outcomes.

etcd's API guarantees say a completed KV operation is committed through
consensus, and the client learns that when the server responds. A timeout is
not that response. The read that checks the key is linearizable, which is
etcdctl's default. Do not pass `--consistency=s`. Sources and the version pin
are in `examples/etcd/README.md`.

For this fault, a symmetric cut of the member that was leader:

- **PASS.** The put was acknowledged. Fuss cut that member. A different member
  became leader and a linearizable get returned the value.
- **LOST WRITE.** That get completed and parsed, and the key was absent.
- **UNKNOWN.** There was no single leader, the put failed, the leader changed
  before the cut, the cut named a different member, the cut file never
  appeared, no other member became leader, or the get failed or did not parse.
  The line includes `reason=`.

A failed get is UNKNOWN. It is not evidence the value is missing.

## 2. Verify the acknowledgement

The workload is one `etcdctl put` of a key that includes the seed (`${seed}-0`).
etcdctl returns only after the write is committed. Record the acknowledgement
only after that command exits 0:

```
ack ${KEY}
```

The line is appended to `${dir}/runs.txt`. A put that fails is
`UNKNOWN reason=write_failed` and is not an acknowledgement. The later get
uses the same key. Replay fails when the ack key and the read key differ.

## 3. Route peer traffic

Three nodes. Client URLs stay off the proxies. Each node binds a free client
port and writes it to `${dir}/http_${i}`. Peer listen is the port Fuss
assigned, `${port}`:

```
--listen-peer-urls http://127.0.0.1:${port}
```

A scenario that needs a second listener lists it under `ports.names`.
`${port}` stays the original listen port. `examples/rqlite` asks for `raft`
and `http`. HTTP is `${addr_http}` and clients dial it directly. Raft listen
is `${addr_raft}`. `peers.port_name: raft` makes the proxy upstream that
Raft address. Two copies of that scenario can start together: each named
port is chosen free, and one Fuss process does not reuse a free port it has
already handed out.

The address other members dial is the proxy in front of that node,
`${node_proxy_addr_0}` and the same for 1 and 2:

```
--initial-advertise-peer-urls http://${node_proxy_addr_k}
--initial-cluster node_0=http://${node_proxy_addr_0},...
```

The topology block that creates those addresses:

```yaml
topology:
  nodes: 3
  proxy_mode: per_node_owned
  peers:
    port_name: peer
    via_proxy: true
fault:
  isolate:
    direction: both
```

`per_node_owned` tags each accepted peer connection with the source process.
`direction: both` cuts links whose source or destination is the isolated
member and leaves the other pair up. The process Fuss recorded has to be etcd
itself. The start script `exec`s the binary. A shell that stays as the parent
does not own the peer sockets, and the cut is then UNKNOWN.

## Choosing the proxy

Pick the mode from the connections the fault has to interrupt. The two
examples below are that choice. `examples/postgres/README.md` is the
PostgreSQL side in full, including the `psql` invocation.

| | PostgreSQL | etcd |
| --- | --- | --- |
| What has to stop | The standby's WAL stream to the primary | Peer traffic both ways for the leader, while the other pair stays up |
| Who dials | The standby. `primary_conninfo` is `host` and `port` of `${node_proxy_addr_0}`, the proxy in front of the primary. Streaming replication is the standby connecting to the primary. | Each member dials the others' advertised peer URLs. Those URLs are the inbound proxies. |
| `proxy_mode` | `per_node` | `per_node_owned` |
| `direction` | `inbound` on the primary (`node_0`) | `both` on the leader |
| Why that cut is enough | One replication connection, opened by the standby, accepted by the primary's proxy. Cutting that proxy closes it and refuses the reconnect. Client sessions dial `127.0.0.1:${port}` and are not on the proxy. | One inbound proxy per member accepts every peer. Cutting only inbound to the leader leaves the leader's own dials to the others up. Cutting a whole proxy without a source tag also takes the other pair with it. The source tag is what lets `both` cut the leader and leave the other pair up. |

The PostgreSQL seed is the concrete case. The standby dials the primary, the
cut is that inbound proxy, and the standby then returns the acknowledged
value. The counters on that proxy (`closed_by_cut`, `refused_while_cut`) are
the interruption. Nothing in that result depends on reading the proxy
implementation.

Valkey is the same choice. The replica's `replicaof` is
`${node_proxy_addr_0}`. Clients dial `127.0.0.1:${port}`. `examples/valkey`
cuts that inbound proxy after a `GET` on the replica has already returned the
value.

`per_node_owned` is the wrong tool for that PostgreSQL stream. It records a
source only when the dialing process is a PID Fuss started. The WAL receiver
is a child of the postmaster the scenario `exec`s, so those sockets are
unknown owners. Fuss then prints `"verdict":"UNKNOWN"` and
`"reason":"proxy_owner_unknown"`.
`evidence.isolate.unknown_owners` still lists `to`, `listen`, `remote`, and
`cut`. `evidence.check` still holds the scenario line when that line is not
itself `UNKNOWN`. A succeeded query is not a verified cut.

## 4. Select the leader at runtime

Any member can win the first election. The scenario does not set
`fault.isolate.node`.

Poll `etcdctl endpoint status -w json` on all three client URLs until exactly
one row has `header.member_id` equal to `leader`. Put to that member. Check
the same member id again. If it changed, finish
`UNKNOWN reason=leader_changed` and do not cut. If it is the same member,
write its name and then the trigger:

```
printf '%s\n' "$LEAD_NODE" > ${dir}/leader_node
echo NOW > ${dir}/cut_now
```

`LEAD_NODE` is `node_0`, `node_1`, or `node_2`. The fault block:

```yaml
fault:
  isolate:
    node_file: "${dir}/leader_node"
    direction: both
  after:
    file:
      glob: "${dir}/cut_now"
      contains: NOW
```

Fuss reads the first line of `node_file` when `cut_now` contains `NOW`.

The driver loop lives inside node 0's start script. Fuss replaces `${i}` in
that script before the shell runs, so `node_${i}` is `node_0` for every
member the loop visits. A shell variable is `node_$i`, with no braces. The
node's own `--name "node_${i}"` stays braced, because that one is the index
of the process being started.

## 5. Decide the verdict

Do not read until `${dir}/isolate_applied.json` exists and its `target` is
the leader just written, `trigger_fired` is true, and `links_cut` is greater
than 0. A missing file is `UNKNOWN reason=cut_not_applied`. A different
target is `UNKNOWN reason=cut_missed_leader`.

Then poll the other two members until one reports itself leader, and
`etcdctl get` the same key from that member (`-w json`, dial timeout 2s,
command timeout 5s). The scenario writes one line to `${dir}/result.txt`:

```
PASS read_from=node_1 leader=self after_cut=yes leader_id=... cut_target=node_0 key=1-0
LOST WRITE read_from=node_1 leader=self after_cut=yes value_missing=yes leader_id=... cut_target=node_0 key=1-0
UNKNOWN reason=read_failed
```

`read_from` is the member that answered. It is one of the two that were not
cut. `cut_target` is the member Fuss was asked to cut. `key` is the key in
the ack line.

The check cats that file. Rules match the line. First match wins. Each
survivor has its own PASS rule and its own LOST WRITE rule, because the
survivor is chosen at runtime. An `UNKNOWN` line matches `out_contains:
UNKNOWN`. Anything else is FAIL.

```yaml
check:
  command:
    command: ["/bin/cat", "${dir}/result.txt"]
    as: out
  rules:
    - if: { out_contains: "LOST WRITE read_from=node_1 leader=self after_cut=yes value_missing=yes" }
      verdict: "LOST WRITE"
    - if: { out_contains: "PASS read_from=node_1 leader=self after_cut=yes" }
      verdict: PASS
    - if: { out_contains: "UNKNOWN" }
      verdict: UNKNOWN
    - else: FAIL
```

The full file repeats the PASS and LOST WRITE rules for `node_0` and
`node_2`. Trial JSON `evidence.check.out` is the line. `evidence.isolate`
is the cut: `target`, `direction`, `links_cut`, `trigger_fired`.

`${dir}/drive_done` contains `DONE` when the result line is written. `kill.at:
file` waits for that so the check runs after the get, and the nodes stay up
until then.

## 6. Run it

```bash
go build -o fuss .
./fuss -target-config=examples/etcd/etcd.yaml -target=./examples/etcd/etcd -seed=1 -n=1 -json
```

Exit 0 and `"verdict":"PASS"` is the clean run. Quote `verdict` from that
JSON line. The same run can be saved and replayed:

```bash
./fuss -target-config=examples/etcd/etcd.yaml -seed=1 -n=1 -save-bundle=/tmp/etcd-bundle
./fuss -replay-bundle=/tmp/etcd-bundle
```

A `node_file` replay prints `cut: saved=node_X replay=node_Y`. A different
index still passes when that index was the leader before the cut and a
different member returned the same key. A scenario that sets `isolate.node`
to a fixed name requires that same name on replay.

## The result file

`${dir}` is the trial directory. `${home}` is `${dir}/home`. `plant_dir`
copies a fixture onto the trial home.

The live driver and the checker use one path, `${home}/result.txt`.

- **Live.** The scenario writes the PASS, LOST WRITE, or UNKNOWN line to
  `${home}/result.txt`. The check reads that file.
- **Control.** `result.txt` at the root of the `plant_dir` directory is
  copied to `${home}/result.txt`. The same check reads it. No nodes are
  started. `examples/rqlite/fixture-lost/result.txt` is a valid LOST WRITE
  line, and the `lost_write` control must judge it `LOST WRITE`.
- **Absent.** A control with no `plant_dir` leaves the file missing. The
  rqlite check prints `UNKNOWN reason=result_absent` and must judge that
  `UNKNOWN`. A missing file is not a lost write.

```yaml
check:
  command:
    command: ["/bin/sh", "-c", "if [ -f \"${home}/result.txt\" ]; then cat \"${home}/result.txt\"; else printf '%s\\n' 'UNKNOWN reason=result_absent'; fi"]
    as: out
controls:
  lost_write:
    plant_dir: fixture-lost
    expect_verdict: "LOST WRITE"
  result_absent:
    expect_verdict: UNKNOWN
```

`plant_dir: fixture-lost` is resolved next to the scenario file. The fixture
contains only `result.txt`.
