# Prediction: pending cut on unmodified rqlite

The workload inserts `1-0=ok` and then strong-reads that key. The binary is the installed `rqlited`, unmodified. Fuss cuts `node_0` in both directions. The client writes `cut_now` only after the history file contains the write's start line, and before this process reads the execute response. There is no gate inside rqlite.

Porcupine uses `effect=optional` and `bound=open`. The register starts empty. A trial is `PASS` only when the cut time falls strictly inside that open write and a read starts after the cut. Any other window keeps the history line and the experiment is `UNKNOWN`.

Across seeds 1 through 10, I expect the insert to be acknowledged before Fuss applies the partition on most seeds. The observed window is then `after`, `schedule pending` is missed, and the Fuss verdict is `UNKNOWN`, with `history legal` when the read returns `ok`. An HTTP error from rqlite is `rejected`: the write did not take effect. A transport failure is `error`: the write may still have taken effect.

This is wrong if all 10 trials print `"verdict":"PASS"` and `schedule pending occurred`.
