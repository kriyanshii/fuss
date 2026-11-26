# Prediction: pending cut on unmodified rqlite

The workload inserts `1-0=ok` and then strong-reads that key. The binary is the installed `rqlited`, unmodified. Fuss cuts `node_0` in both directions. The client writes `cut_now` only after the history file contains the write's start line, and before this process reads the execute response. There is no gate inside rqlite.

Porcupine uses `effect=optional` and `bound=open`. The register starts empty. A trial is `PASS` only when the cut time falls strictly inside that open write and a read starts after the cut. Any other window keeps the history line and the experiment is `UNKNOWN`.

Across seeds 1 through 10, I expect the insert to be acknowledged before Fuss applies the partition on most seeds. The observed window is then `after`, `schedule pending` is missed, and the Fuss verdict is `UNKNOWN`, with `history legal` when the read returns `ok`. An HTTP error from rqlite is `rejected`: the write did not take effect. A transport failure is `error`: the write may still have taken effect.

This is wrong if all 10 trials print `"verdict":"PASS"` and `schedule pending occurred`.

## Observed result

The prediction was wrong. All 10 trials printed `"verdict":"PASS"` and `schedule pending occurred`.

Command: `./examples/rqlite/pending/pending.sh /tmp/rqlite-pending`

That runs `./fuss -target-config=examples/rqlite/pending/fuss.yaml -seed=N -n=1 -json` for seeds 1 through 10. Fuss resolved `rqlited` to `/usr/local/bin/rqlited`, `rqlited v10.7.5`. Each cut is `target` `node_0`, `direction` `both`, `links_cut` 4, `links_total` 6, `trigger_fired` true. Each check says `window pending`, `history legal`, and `read after cut`.

The execute response on every seed was `{"results":[],"error":"leadership lost while committing log"}`, recorded as `observe` `error`. The post-cut read returned `ok` on 4 seeds and was `missing` on 6. Both are legal under `effect=optional` and `bound=open`, because that response does not say whether the write took effect.

An earlier sample recorded that same body as `rejected`. Two of those 10 trials then printed `"verdict":"ILLEGAL"`: the schedule had occurred, and the read returned `ok` after a write the checker had been told did not happen. That body is a commit error, not a refusal before apply, so the client keeps it as `error`. Saved histories and cuts from the corrected run are under `/tmp/rqlite-pending/seed-1` through `seed-10`.
