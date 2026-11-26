# Prediction: three fault schedules for one repkv write

Each schedule writes `1-0=v`, then reads that key from a surviving leader. The gates force when Fuss is asked to cut `node_0`. Porcupine uses `effect=optional` and `bound=open`. The register starts empty.

- **before:** the cut is applied, and only then does the client invoke the write. The write is not acknowledged. The post-cut read does not return `v`. The history is legal, the schedule occurred, and the experiment verdict is `PASS`.
- **pending:** the client invokes the write, the cut lands while that call is still open, and the client then times out. The post-cut read returns `v`. The history is legal, the schedule occurred, and the experiment verdict is `PASS`.
- **after:** the client observes a successful write, and only then does the cut land. The post-cut read returns `v`. The history is legal, the schedule occurred, and the experiment verdict is `PASS`.

This is wrong if any of the three Fuss verdicts is not `PASS`, or if a run with no post-cut read is `PASS`.

## Observed result

Command: `./examples/repkv/schedules.sh /tmp/repkv-schedules`

That runs `./fuss -target-config=examples/repkv/repkv-schedules.yaml -seed=1 -n=1 -json` once per schedule. Fuss resolved `repkv` to `/private/tmp/fuss-repkv-history/examples/repkv/repkv`. Each cut is `target` `node_0`, `direction` `both`, `links_cut` 4, `links_total` 6, `trigger_fired` true.

- **before:** `"verdict":"PASS"`. Cut `2026-11-26T10:12:09.260179Z`, then `w1` starts `2026-11-26T10:12:09.273289Z` and ends `observe` `error`. `r1` from `node_1` is `missing` true. The check says `schedule before occurred`, `history legal`, `read after cut`.
- **pending:** `"verdict":"PASS"`. `w1` starts `2026-11-26T10:12:09.702501Z`, cut `2026-11-26T10:12:09.715015Z`, `w1` ends `observe` `timeout` at `2026-11-26T10:12:11.704385Z`. `r1` from `node_1` returns `v`. The check says `schedule pending occurred`, `history legal`, `read after cut`.
- **after:** `"verdict":"PASS"`. `w1` ends `observe` `ok` at `2026-11-26T10:12:11.851537Z`, then the cut at `2026-11-26T10:12:11.853956Z`. `r1` from `node_1` returns `v`. The check says `schedule after occurred`, `history legal`, `read after cut`.

`./examples/repkv/schedules.sh after /tmp/repkv-schedules-one` reruns one schedule. Fuss printed `"verdict":"PASS"` with `schedule after occurred`, `history legal`, and `read after cut`.
