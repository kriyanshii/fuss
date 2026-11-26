# Prediction: write times out during a leader partition

The write of `1-0=v` will replicate to a follower while the leader holds
back its response. Fuss will isolate the leader after replication is
confirmed and before the client receives a reply.

Expected client history:
1. The client starts `write(1-0, v)`.
2. Fuss cuts the leader while the write is still pending.
3. The client reports a timeout.
4. A read from the new leader returns `v` for `1-0`.

This history is legal: the write can take effect even though the client
never receives confirmation.

If the cut lands before replication is confirmed, or the read fails,
the run is inconclusive for this experiment.

## Observed result

Command:

```sh
./examples/repkv/inflight.sh /tmp/repkv-inflight-history.jsonl
```

That runs `./fuss -target-config=examples/repkv/repkv-inflight.yaml -seed=1 -n=1 -json`. Seed 1. Fuss resolved `repkv` to `/private/tmp/fuss-repkv-history/examples/repkv/repkv`.

Fuss printed `"verdict":"PASS"` and `PASS read_from=node_1 leader=self after_cut=yes key=1-0`. The cut in the trial JSON is `target` `node_0`, `cut_time` `2026-11-26T09:18:48.15694Z`, `links_cut` 4.

Trace:

```text
w1 starts: write 1-0=v
Fuss applies partition: node_0
w1 completes: timeout
r1 starts: read 1-0 from node_1
r1 completes: value=v
```

```json
{"kind":"client","id":"w1","phase":"start","op":"write","key":"1-0","value":"v","at":"2026-11-26T09:18:48.14155Z"}
{"kind":"cut","target":"node_0","direction":"both","links_cut":4,"links_total":6,"trigger_fired":true,"at":"2026-11-26T09:18:48.15694Z"}
{"kind":"client","id":"w1","phase":"end","op":"write","observe":"timeout","error":"Put \"http://127.0.0.1:56493/k/1-0\": context deadline exceeded (Client.Timeout exceeded while awaiting headers)","at":"2026-11-26T09:18:50.143477Z"}
{"kind":"client","id":"r1","phase":"start","op":"read","key":"1-0","from":"node_1","at":"2026-11-26T09:18:50.158844Z"}
{"kind":"client","id":"r1","phase":"end","op":"read","value":"v","observe":"ok","at":"2026-11-26T09:18:50.159129Z"}
```
