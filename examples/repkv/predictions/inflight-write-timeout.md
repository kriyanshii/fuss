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
