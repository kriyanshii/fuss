# repkv

Tiny three-node replicated KV for Fuss isolate scenarios. Stdlib only.

- Client HTTP: `PUT /k/{key}`, `GET /k/{key}`, `GET /leader`
- Peer TCP through Fuss proxies when `topology.peers.via_proxy` is set
- `-mode=correct`: leader acks a PUT only after a follower confirms replication
- `-mode=bug`: leader acks immediately; replication waits for a gate file

```sh
go build -o examples/repkv/repkv ./examples/repkv
go build -o fuss .
./fuss -target-config=examples/repkv/repkv.yaml -n=10 -seed=1 -json
./fuss -target-config=examples/repkv/repkv-bug.yaml -n=10 -seed=1 -json
```

## Replay

`examples/repkv/bundles/lost-write` is one saved bug-mode run. It holds the
scenario, the Fuss revision, the repkv version and checksum, the seed, the
acknowledged write, the applied partition, and the read. From a clean checkout:

```sh
./examples/repkv/replay.sh
```

That rebuilds Fuss and repkv, runs the saved scenario and seed, and prints:

```
ack: key=1-0
cut: target=node_0 direction=both links_cut=4 trigger_fired=true
read: key=1-0 read_from=node_1 status=ok found=false
verdict: LOST WRITE
```

Exit 0 means the new run showed those three facts, the read named the same key as the acknowledgement, and the verdict matched. The
target's own exit status for bug mode is 1, because the verdict is LOST WRITE.

`-adapter=repkv` loads `examples/repkv/repkv.yaml`. Prefer `-target-config` so
the path is explicit.

## Client history

`history.sh` runs the existing after-ack scenario once and writes a JSONL
history. Each client operation is two lines with the same `id`: `phase` `start`
immediately before the request, and `phase` `end` when the client sees `ok`,
`timeout`, or `error`. A successful read's end line includes `value`. The cut
line is Fuss's `isolate_applied.json` (`target`, `cut_time`), not a time the
driver guessed.

```sh
./examples/repkv/history.sh /tmp/repkv-history.jsonl
```

The value in this scenario is `v`, and the key is `1-0`. A run whose write
finished before the partition and whose read started after it renders as:

```text
w1 starts: write 1-0=v
w1 completes: ok
Fuss applies partition: node_0
r1 starts: read 1-0 from node_1
r1 completes: value=v
```

`node_1` may be `node_2`. The verdict line is unchanged. `observe` is only
what the client saw. Whether a timed-out write took effect is a separate fact.

`repkv-read-error.yaml` is the same cluster and cut. The check dials a closed
port after the new leader is known, and the verdict is `UNKNOWN reason=read_error`.
