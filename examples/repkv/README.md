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

There is no saved trial-directory replay for these YAML scenarios. A failing
trial keeps its temp dir (`dir_kept: true`) for inspection, but replaying the
finding means re-running the same seed (no root required; Fuss uses userspace
proxies):

```sh
./fuss -target-config=examples/repkv/repkv-bug.yaml -seed=1 -n=1 -json
```

Re-run that ten times; the bar is LOST WRITE every time. `-adapter=repkv`
loads the same `repkv.yaml` via `examples/repkv/repkv.yaml`, but prefer
`-target-config` so the path is explicit.
