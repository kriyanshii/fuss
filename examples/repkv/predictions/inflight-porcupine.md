# Prediction: Porcupine judges the in-flight history

The in-flight scenario records one timed-out write of `1-0=v` and a later read of `v`. Porcupine, with the write's effect optional and its completion bound left open, will call that history legal.

Fuss verifies the cut separately. On this run the cut lands, so the experiment verdict is `PASS`. The trial JSON shows `history legal` in `evidence.check.out` and the applied cut in `evidence.isolate`.

This prediction is wrong if the history is reported illegal, if the experiment verdict is not `PASS`, or if a legal history is reported as `PASS` when the cut was not verified.

## Observed result

Command: `./examples/repkv/inflight.sh /tmp/repkv-inflight-porcupine.jsonl`

That runs `./fuss -target-config=examples/repkv/repkv-inflight.yaml -seed=1 -n=1 -json`. Seed 1. Fuss resolved `repkv` to `/private/tmp/fuss-repkv-history/examples/repkv/repkv`.

Fuss printed `"verdict":"PASS"`. `evidence.check.out` begins `history legal` and `translation effect=optional bound=open`. `evidence.isolate` is `target` `node_0`, `direction` `both`, `links_cut` 4, `links_total` 6, `trigger_fired` true, `cut_time` `2026-11-26T09:57:56.679958Z`.

The recorded history:

- `w1` starts: write `1-0=v` at `2026-11-26T09:57:56.658401Z`
- Fuss applies the partition at `2026-11-26T09:57:56.679958Z`
- `w1` completes: timeout at `2026-11-26T09:57:58.659842Z`
- `r1` starts: read `1-0` from `node_1` at `2026-11-26T09:57:58.675202Z`
- `r1` completes: value `v` at `2026-11-26T09:57:58.675615Z`
