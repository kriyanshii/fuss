# PostgreSQL 15.19 synchronous commit, defined before the run

The prediction and the after-the-run note below are the trial notes, moved out of `/tmp`. They are unchanged. The example in `examples/postgres` now writes `standby_before_cut=sync` for the sync state this note called `standby=sync`. That state was read from `pg_stat_replication` before `cut_now`.

Guarantee, quoted from PostgreSQL 15 docs, page "20.5. Write Ahead Log":

> When set to `remote_apply`, commits will wait until replies from the current synchronous standby(s) indicate they have received the commit record of the transaction and applied it, so that it has become visible to queries on the standby(s), and also written to durable storage on the standbys.

URL: https://www.postgresql.org/docs/15/runtime-config-wal.html#GUC-SYNCHRONOUS-COMMIT

Same promise in "27.2.8. Synchronous Replication" (https://www.postgresql.org/docs/15/warm-standby.html#SYNCHRONOUS-REPLICATION): a commit with `synchronous_commit = remote_apply` waits until the synchronous standby reports that it has replayed the transaction, making it visible to queries.

## Lines defined before the run

A write is acknowledged only after `psql` exits 0 on the primary. The key is `${seed}-0` (seed 1 → `1-0`). The value is `ok`. The fault is an isolate of the primary (`node_0`) after that acknowledgement. The read is `SELECT v FROM fuss_kv WHERE k = '<key>'` on the standby (`node_1`), which is not killed.

- PASS: `PASS read_from=node_1 standby=sync after_cut=yes cut_target=node_0 key=<key>`
- LOST WRITE: `LOST WRITE read_from=node_1 standby=sync after_cut=yes value_missing=yes cut_target=node_0 key=<key>`
- UNKNOWN: `UNKNOWN reason=<reason>` when the standby never reaches `streaming`/`sync`, the commit command fails, the sync standby is gone before the cut, the cut file is missing or names another node, or the standby read fails or does not parse. A failed read is not a missing value.

## Prediction (not edited after the run)

I expect PASS. `remote_apply` means `psql` does not return success until the standby has applied the commit, so an isolate of the primary's replication path after that cannot remove the row from the standby.

This prediction is wrong if the standby's `SELECT` exits 0 and returns no row. That line is LOST WRITE.

## After the run (prediction above left as written)

Seed 1 with `proxy_mode: per_node` and `fault.isolate.direction: inbound` exited 0. Fuss `-json` field `verdict` was `PASS`. `evidence.check.out` was `PASS read_from=node_1 standby=sync after_cut=yes cut_target=node_0 key=1-0`. `evidence.isolate` was an inbound cut of `node_0`, `trigger_fired` true, `links_cut` 1.

An earlier seed-1 run with `per_node_owned` and `direction: both` had `verdict` `UNKNOWN`. `evidence.isolate.unknown_owners` listed two connections to the primary proxy with `cut` true. The standby read had already returned `ok`; Fuss would not call that PASS.
