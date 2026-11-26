# Prediction: Porcupine judges the in-flight history

The in-flight scenario records one timed-out write of `1-0=v` and a later read of `v`. Porcupine, with the write's effect optional and its completion bound left open, will call that history legal.

Fuss verifies the cut separately. On this run the cut lands, so the experiment verdict is `PASS`. The trial JSON shows `history legal` in `evidence.check.out` and the applied cut in `evidence.isolate`.

This prediction is wrong if the history is reported illegal, if the experiment verdict is not `PASS`, or if a legal history is reported as `PASS` when the cut was not verified.
