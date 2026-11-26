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

`repkv -check-history` reads that JSONL offline. It ignores `kind` `cut`.
The key starts at `-initial` (default `0`). A completed write has to sit
in the order. A timed-out write may be omitted, or placed at or after its
start. Exit 0 prints one legal order. Exit 1 prints why none exists.

```sh
./examples/repkv/repkv -check-history examples/repkv/testdata/histories/timeout-then-new.jsonl
./examples/repkv/repkv -check-history /tmp/repkv-inflight-history.jsonl -initial 0
```

The same JSONL files are the controls for a Porcupine register. A timed-out
write has two assumptions, and they are named separately. The effect is
`none` (the write did not happen), `required` (it must take effect), or
`optional` (it may be omitted). The bound is `timeout` (if it takes effect,
it does so by the client timeout) or `open` (it may take effect later).

`required` and `timeout` together reject a later read of the old value,
because the write is required to take effect, not only because the timeout
is a bound. `optional` and `timeout` still allow that read by omitting the
write, and reject a read of the old value followed by a read of the new one
(`timeout-late-write.jsonl`). Leaving the bound `open` accepts that late
write. A timed-out write of `1` followed by reads of `1` then `0`
(`timeout-then-revert.jsonl`) is illegal under every combination: one
ordering still has to explain the whole history. Overlapping writes are
legal when every later read matches one order, and illegal when the reads
require two (`overlap-agree.jsonl`, `overlap-disagree.jsonl`).

`repkv-inflight.yaml` feeds its recorded JSONL to Porcupine with
`effect=optional` and `bound=open`. The check prints `history legal` or
`history illegal`. That line is the history verdict. Fuss still verifies the
cut on its own. If the cut cannot be verified, the experiment verdict is
`UNKNOWN` even when the history is legal. The trial JSON keeps the history
line in `evidence.check.out` and the cut in `evidence.isolate`.

`repkv-read-error.yaml` is the same cluster and cut. The check dials a closed
port after the new leader is known, and the verdict is `UNKNOWN reason=read_error`.
