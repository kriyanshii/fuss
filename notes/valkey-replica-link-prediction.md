# Valkey replica link, defined before the run

Guarantee under test, as specified for this run: after the replica has already returned a value, cutting the replication connection leaves that value readable on the replica.

How that is being read. Valkey's replication page says replicas are exact copies of the primary, and that when the link breaks the replica reconnects and tries to catch up. It does not say a link break deletes keys the replica already applied. `replica-serve-stale-data yes` is the default in the 9.2.0 `valkey.conf`: while the link is down the replica still answers clients, possibly with out-of-date data. This run uses that default. It does not restart the primary. The same page's warning, that a primary which restarts with an empty dataset is replicated as empty, is a different fault and is not this test.

## Lines defined before the run

The key is `${seed}-0`. The value is `ok`. A write is not the acknowledgement. The acknowledgement is a `GET` on the replica, through `valkey-cli --no-raw`, whose line is `"ok"`, while `INFO replication` reports `master_link_status:up`. Only then is `ack` recorded and the primary's inbound proxy cut. The check is a second `GET` of the same key on the replica's own port.

- PASS: `PASS read_from=node_1 replica_before_cut=ok after_cut=yes cut_target=node_0 key=<key>`
- LOST WRITE: `LOST WRITE read_from=node_1 replica_before_cut=ok after_cut=yes value_missing=yes cut_target=node_0 key=<key>`
- UNKNOWN: replication never confirmed, the value or the link is gone before the cut, the cut is not the one requested, or the post-cut `GET` fails or does not parse. A failed `GET` is not a missing key. `(nil)` on a successful `GET` is the missing key.

## Prediction

I expect PASS. The value is already in the replica's dataset before the cut, and the default stale-data setting tells the replica to keep answering `GET` while the link is down. Closing the connection the replica opened, and refusing the reconnect, does not replace that dataset.

This prediction is wrong if the post-cut `GET` exits 0 and the line is `(nil)`. That line is LOST WRITE.
