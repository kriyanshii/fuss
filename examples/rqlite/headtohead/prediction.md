# Prediction: same rqlite history, two harnesses

The shared workload inserts `1-0=ok`, waits for the cut, and strong-reads that row from a surviving leader. Porcupine, with a timed-out write's effect optional and its completion bound left open, will call a completed insert followed by a read of `ok` legal.

On the Fuss side the cut is bidirectional. Fuss verifies it. The experiment verdict is `PASS`, `evidence.check.out` begins `history legal`, and `evidence.isolate` shows the cut of `node_0`.

This is wrong if that history is illegal, or if the verdict is `PASS` without a verified cut.
