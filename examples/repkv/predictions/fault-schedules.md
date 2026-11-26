# Prediction: three fault schedules for one repkv write

Each schedule writes `1-0=v`, then reads that key from a surviving leader. The gates force when Fuss is asked to cut `node_0`. Porcupine uses `effect=optional` and `bound=open`. The register starts empty.

- **before:** the cut is applied, and only then does the client invoke the write. The write is not acknowledged. The post-cut read does not return `v`. The history is legal, the schedule occurred, and the experiment verdict is `PASS`.
- **pending:** the client invokes the write, the cut lands while that call is still open, and the client then times out. The post-cut read returns `v`. The history is legal, the schedule occurred, and the experiment verdict is `PASS`.
- **after:** the client observes a successful write, and only then does the cut land. The post-cut read returns `v`. The history is legal, the schedule occurred, and the experiment verdict is `PASS`.

This is wrong if any of the three Fuss verdicts is not `PASS`, or if a run with no post-cut read is `PASS`.
