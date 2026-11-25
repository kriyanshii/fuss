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
ack: ack 1-0
cut: target=node_0 direction=both links_cut=4 trigger_fired=true
read: LOST WRITE read_from=node_1 leader=self after_cut=yes value_missing=yes
verdict: LOST WRITE
```

Exit 0 means the new run showed those three facts and the same verdict. The
target's own exit status for bug mode is 1, because the verdict is LOST WRITE.

`-adapter=repkv` loads `examples/repkv/repkv.yaml`. Prefer `-target-config` so
the path is explicit.

`repkv-read-error.yaml` is the same cluster and cut. The check dials a closed
port after the new leader is known, and the verdict is `UNKNOWN reason=read_error`.
