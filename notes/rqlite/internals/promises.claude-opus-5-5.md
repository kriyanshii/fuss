# rqlite promises Fuss could test (claude-opus-5-5)

Candidates only. Check every quote against the spec and write the rules yourself.

## repo-db-design.md

- **Any write that rqlite acknowledged survives a hard crash of a node, even though SQLite runs with SYNCHRONOUS=OFF.** (process-kill, high)
  - Quote: > rqlite is comfortable with this because every committed write has already been durably stored in the Raft log on a quorum of nodes. If a node crashes and its SQLite file is corrupt, the cluster has the data and the node can be rebuilt from the snapshot store and the log.
  - Inject: SIGKILL one or more nodes (including the leader) while a stream of acknowledged writes is in flight; also kill a single-node cluster right after an ack.
  - Observe: After restart, a strong-consistency read (/db/query?level=strong) returns every row whose /db/execute returned success in Fuss's history log. No acknowledged row is missing.
  - Workload: Start a 3-node cluster with documented flags. CREATE TABLE t(id INTEGER PRIMARY KEY, v). Run a loop of POST /db/execute INSERT with unique ids, logging each ack. Kill -9 a random node mid-loop and restart it with the same data dir. At the end, SELECT id FROM t with level=strong on every node and diff against the acked ids.
  - Guess: Guess: if the restarting node trusts a torn SQLite/WAL file instead of rebuilding from snapshot plus log, it could serve stale or missing rows.
- **Killing the rqlited process must not delete or truncate the SQLite WAL file. Writes that are only in the WAL must still be recoverable.** (process-kill, medium)
  - Quote: > When rqlite closes the database — including on hard process exit — the WAL must remain intact.
  - Inject: SIGKILL after writes have been acknowledged but before the next snapshot. Use a large -raft-snap threshold and a long snapshot interval so no snapshot intervenes.
  - Observe: Read-only listing of the data dir right after the kill shows the -wal file still present with nonzero size. After restart, all acked rows are present.
  - Workload: Start a single node with a high snapshot threshold. Insert N rows and record acks. Record the size of the db.sqlite-wal file in the data dir (read only). SIGKILL. Re-read the size of the -wal file. Restart and SELECT count(*).
  - Guess: Guess: some other close path (store shutdown hooks) might checkpoint or remove the WAL independently of the driver setting.
- **A graceful shutdown leaves the WAL in place rather than checkpointing it into the main file, and the next start handles it without losing data.** (restart, low)
  - Quote: > No checkpoint on close, even on graceful shutdown. Surviving the WAL is more important than a tidy on-disk file. On the next start, the rest of rqlite decides what to do with the leftover WAL.
  - Inject: SIGTERM (graceful stop) after acknowledged writes and before any snapshot.
  - Observe: The -wal file still exists in the data dir after a clean exit. The main db file mtime/size is unchanged by the shutdown. All acked rows are visible after restart.
  - Workload: Single node with a large snapshot threshold. Insert rows. Snapshot the data dir file sizes and mtimes. SIGTERM and wait for exit. Re-read the sizes. Restart and verify the row count.
  - Guess: Guess: the Store may take a snapshot on graceful shutdown, which legitimately checkpoints. That would make the WAL vanish for a different reason, so the observation may be ambiguous.
- **On a brand-new node, the -wal and -shm files exist as soon as the database is opened, before any write.** (restart, medium)
  - Quote: > `Open` also forces the WAL files into existence at open time when WAL mode is requested.
  - Inject: Immediately after first start of a fresh node, and again after a restart with no writes in between.
  - Observe: Read-only directory listing shows the db, -wal and -shm files once /readyz reports ready, with zero writes issued.
  - Workload: Start rqlited on an empty data dir. Poll /readyz. List the data dir. SIGKILL, restart, and list again.
  - Guess: Guess: the TRUNCATE checkpoint or a startup cleanup step could remove the -wal file instead of leaving it at zero length.
- **Even after a write connection is discarded (for example, because a request timed out), SQLite never starts auto-checkpointing on its own.** (other, low)
  - Quote: > The PRAGMA is therefore executed by each driver's connect hook, so every connection — including any replacement — has autocheckpointing disabled.
  - Inject: Force write-request timeouts or cancellations (short documented timeout parameters, or client disconnects mid-request) so the writer connection may be replaced. Then keep writing well past 1000 WAL pages.
  - Observe: With snapshots effectively disabled by a high threshold and interval, the -wal file size grows monotonically past ~1000 pages and is never reset between snapshots. /status snapshot counters do not move during the window.
  - Workload: Single node with a huge -raft-snap value. Issue some large bulk execute requests with a tiny timeout and abort the client connections. Then insert multi-KB rows in a loop. Sample the -wal file size every second (read only) and check it only grows.
  - Guess: Guess: a code path that opens a connection without the hook, or a driver swap, could miss the PRAGMA. A WAL reset would then show up as the file shrinking or stopping growth.
- **Concurrent write requests are applied one at a time with no lost updates, and the WAL only shrinks when a snapshot happens.** (concurrent-calls, medium)
  - Quote: > writes are serialized, the WAL never disappears unexpectedly, and the checkpoint cadence is dictated by the snapshot subsystem rather than by SQLite's autocheckpointer.
  - Inject: Many clients issuing read-modify-write statements simultaneously, with an occasional node kill.
  - Observe: The final counter equals the number of acked increments. -wal size decreases only around times when /status shows a new snapshot.
  - Workload: CREATE TABLE c(id PRIMARY KEY, n). Insert (1,0). Have 32 goroutines POST UPDATE c SET n=n+1 WHERE id=1 and log acks. Sample /status snapshot info and the -wal size. At the end, read n with level=strong and compare to the ack count.
  - Guess: Guess: retries on leader change could double-apply a non-idempotent statement, giving n greater than acks. That would be a retry issue rather than a serialization issue.
- **PRAGMAs touching journal_mode, wal_autocheckpoint, wal_checkpoint, synchronous, or query_only are refused without being written to the Raft log. Harmless PRAGMAs are accepted.** (restart, high)
  - Quote: > The Store rejects any user statement matching one of these before it reaches Raft. The list is deliberately narrow: only PRAGMAs that would invalidate rqlite's coordination with SQLite. Tuning PRAGMAs like `cache_size` are fine and pass through unchanged.
  - Inject: Send breaking PRAGMAs, then restart the node to see whether anything was logged and replayed.
  - Observe: /db/execute returns an error for each breaking PRAGMA. The Raft applied/commit index in /status does not advance for it. After restart, the db header bytes 18-19 still indicate WAL mode and writes still work. cache_size is accepted.
  - Workload: Record the /status raft index. POST /db/execute ["PRAGMA journal_mode=DELETE"], ["PRAGMA wal_autocheckpoint=10"], ["PRAGMA synchronous=FULL"], [" pragma  Query_Only = 1"]. Re-read the raft index. POST PRAGMA cache_size=-2000 and expect success. Kill -9, restart, read the db header from disk, and insert a row.
  - Guess: Guess: odd casing, whitespace, comments, or schema-qualified forms (main.journal_mode) may slip past the regex-free matcher and reach Raft.
- **A long-running read cannot stall snapshots or writes, and an interrupted or busy checkpoint never corrupts data.** (concurrent-calls, medium)
  - Quote: > The net effect is that no reader, no matter how slow, can prevent forward progress. Each checkpoint either truncates, makes partial progress that the next attempt continues from, or returns retryable-busy without touching state. Snapshot creation can keep up with write traffic.
  - Inject: Hold a very slow read query open while writes push the log past the snapshot threshold. Optionally SIGKILL during the busy checkpoint window.
  - Observe: /status shows snapshot index and count advancing, and write latency stays bounded while the slow query runs. After any kill and restart, all acked rows are present and the -wal file is eventually truncated to small size.
  - Workload: Start with a small -raft-snap (e.g. 100). In parallel, POST /db/query with a deliberately expensive recursive CTE that runs for tens of seconds, and insert rows in a tight loop. Poll /status every second. Kill -9 the node mid-run, restart, and verify acked rows.
  - Guess: Guess: the salt-comparison logic for the 'all pages moved but not truncated' case could misjudge a WAL reset after restart, producing a snapshot with missing or duplicated frames.
- **Once a snapshot completes, the checkpointed pages are fsynced, so a crash right after a snapshot does not lose data.** (process-kill, low)
  - Quote: > pages that have just landed in the database file must be durable on disk before the snapshot subsystem treats them as committed.
  - Inject: SIGKILL immediately after /status shows a new snapshot index, before further writes.
  - Observe: After restart, all rows acked before the snapshot are present and the node starts without a corruption error.
  - Workload: Use a small -raft-snap. Insert rows until /status reports a snapshot. SIGKILL at once, restart, and SELECT count(*) with level=strong on that node alone (single-node cluster).
  - Guess: Guess: process kill does not drop the OS page cache, so a missing fsync would only show under power loss. This test may pass even if the promise is broken.
- **A WAL left half-written by a crash is read only up to its last valid frame, and the node still starts and serves consistent data.** (process-kill, medium)
  - Quote: > stops at the first invalid frame — which is also how it detects the end of the valid prefix in a WAL that may contain trailing garbage from interrupted writes.
  - Inject: SIGKILL during a large bulk write, so the WAL is likely mid-append.
  - Observe: The node restarts successfully. Each bulk transaction is visible either entirely or not at all. All acked transactions are present.
  - Workload: Repeatedly POST /db/execute?transaction with 1000 large inserts tagged by batch id, and SIGKILL at random offsets during the request. Restart and SELECT batch, count(*) GROUP BY batch. Every batch count must be 0 or 1000, and every acked batch must be 1000.
  - Guess: Guess: a torn trailing frame whose salt happens to match could be accepted in the fast (non-checksum) scan mode, putting partial data into a snapshot.
- **Queries and writes running at the same time as a database load or restore never fail with a 'database closed' error and see either the old or the new database.** (concurrent-calls, medium)
  - Quote: > The wrapper exists because callers — the FSM, the HTTP layer, the auto-backup uploader, the CDC collator — all hold a long-lived reference to "the database" and must not see a closed `*DB` while a swap is in flight.
  - Inject: While many concurrent reads and writes are running, issue /db/load (or trigger a restore by restarting a lagging follower so it installs a snapshot).
  - Observe: No HTTP responses report closed-database errors. Each query result matches either the pre-load or the post-load dataset, never a mix or an empty database.
  - Workload: Prepare SQLite file B with table t containing marker 'B'. The live db has marker 'A'. Run 16 goroutines doing SELECT marker FROM t and inserts. POST /db/load with B. Classify every response.
  - Guess: Guess: a code path such as backup or the CDC column-name provider might hold the inner *DB outside the RWMutex and hit the closed handle.
- **Loading a non-SQLite file via /db/load is rejected and leaves the existing database untouched, including across a restart.** (restart, high)
  - Quote: > magic-byte checks on file headers, used before swapping a file in or accepting a restore payload.
  - Inject: Right after a rejected load, kill and restart the node.
  - Observe: /db/load returns an error. Data queried before and after the restart is identical to the pre-load state.
  - Workload: Insert known rows. POST /db/load with random bytes, then with a truncated SQLite header, then with a raw WAL file. Expect errors. SIGKILL, restart, and compare a full SELECT.
  - Guess: Guess: the file might be staged or renamed on disk before validation, so a crash after rejection could leave a corrupt file in place.
- **A backup taken during heavy writes is a transactionally consistent, standalone SQLite file in DELETE journal mode.** (concurrent-calls, high)
  - Quote: > uses SQLite's online backup API (`copyDatabaseConnection` → `sqlite.Backup.Step`) to copy a consistent snapshot to a new file while writes remain in flight. The destination is set to `journal_mode=DELETE` so the result is a portable, single-file SQLite database.
  - Inject: Call GET /db/backup repeatedly while transactional writes maintain an invariant.
  - Observe: Every downloaded file has a valid SQLite header with bytes 18-19 indicating rollback (DELETE) mode. It opens standalone and satisfies the invariant (e.g. the sum across accounts is constant).
  - Workload: Create accounts with a fixed total. Run transfers via /db/execute?transaction (debit+credit) in several goroutines. Loop GET /db/backup (and ?vacuum). Open each file with a local sqlite and check SUM(balance) and PRAGMA journal_mode.
  - Guess: Guess: the Serialize path for WAL databases could copy bytes while a write commits, if the temp-file backup is skipped for some request variant.
- **A SQL dump taken during writes can be loaded into a fresh node and reproduces a consistent copy of the data.** (concurrent-calls, medium)
  - Quote: > writes a SQL text rendering of the schema and data, suitable for restore through `/db/load?fmt=sql`.
  - Inject: Take the dump while writes are in flight.
  - Observe: Loading the dump into a fresh node succeeds, and the resulting data satisfies the same invariants as the source at some point in time.
  - Workload: Same transfer workload as the backup test. GET /db/backup?fmt=sql. Start a fresh node. POST the text to /db/load?fmt=sql. Query the invariant.
  - Guess: Guess: the dump may read tables in separate read transactions, giving a cross-table inconsistent view under concurrent writes.
- **A bulk request with several autocommit statements produces exactly one CDC event group, stamped with that request's Raft index and containing all of its changes.** (process-kill, medium)
  - Quote: > The `CDCCollator` accumulates events across every commit in an entry and the Store retrieves them once the entry is applied. This keeps delivery, back-pressure, and Raft-index stamping in the Store, leaves the hook path free of I/O, and means a multi-statement entry yields one complete event group.
  - Inject: Send bulk requests, and SIGKILL the leader shortly after some are acknowledged.
  - Observe: The Fuss HTTP sink, configured via rqlited's documented CDC flag, receives one group per bulk request with a strictly increasing Raft index and all statement rows. Duplicates or splits after the restart are recorded.
  - Workload: Run Fuss's HTTP sink and start rqlited with CDC pointing at it. POST /db/execute with 5 INSERTs (no transaction). Repeat with ids, killing and restarting mid-run. Group received events by index and check completeness and monotonicity.
  - Guess: Guess: after a restart, entries replayed from the log could be re-emitted, or an entry applied just before the kill could be lost from the CDC stream.
- **In a bulk request where a later statement fails, CDC still reports the earlier statements that committed and reports nothing for the failed one.** (other, medium)
  - Quote: > The commit hook moves `pending` onto the end of `events`; the rollback hook discards `pending` and leaves `events` alone. The split exists because a single change entry can contain several autocommit transactions — a bulk request without an enclosing transaction commits once per statement — so one entry can see many commits, and a rollback of a later statement must not throw away the events of earlier statements that did commit.
  - Inject: Bulk request whose third statement violates a constraint.
  - Observe: The CDC sink receives events exactly matching the rows that a strong read shows were committed.
  - Workload: POST /db/execute [INSERT id=1, INSERT id=2, INSERT id=1 (PK conflict), INSERT id=3]. Compare SELECT id FROM t with the ids in the received CDC group.
  - Guess: Guess: a rollback hook firing for the failed statement could clear accumulated events if the pending/events split regressed.
- **When a multi-row statement with OR FAIL errors partway, CDC still reports the rows it actually committed.** (other, medium)
  - Quote: > Events must not be discarded merely because a statement returns an error: SQLite's `FAIL` conflict resolution can retain and commit changes made before the error.
  - Inject: Single UPDATE OR FAIL / INSERT OR FAIL that hits a conflict midway.
  - Observe: The rows that changed in the table (strong read) equal the rows reported in CDC events.
  - Workload: Insert rows 1..10 with a UNIQUE column. POST INSERT OR FAIL INTO t SELECT ... producing a conflict at row 5. Diff table state against the CDC sink.
  - Guess: Guess: an error return might trigger a DB-API rollback path that discards pending events, even though SQLite kept the changes.
- **CDC events carry the column names that existed when their statement committed, even if a later statement in the same request alters the table.** (other, medium)
  - Quote: > They are resolved at commit rather than when `Events` is called because a later transaction in the same entry may alter the table, and each event must carry the column names that were in effect when its transaction committed.
  - Inject: Single bulk request mixing an INSERT, an ALTER TABLE ADD COLUMN, and another INSERT.
  - Observe: The first event lists the old columns; the second event lists the new columns.
  - Workload: POST /db/execute [INSERT INTO t(a) VALUES(1), ALTER TABLE t ADD COLUMN b, INSERT INTO t(a,b) VALUES(2,3)]. Inspect column names in the CDC group.
  - Guess: Guess: the per-table column cache could outlive a single commit and return stale names for the second insert.
- **After a /db/load (or a snapshot restore), CDC keeps emitting events for subsequent writes.** (restart, medium)
  - Quote: > Because the hooks live on the writer connection, a `Swap` silently removes them along with the old connection; the Store re-registers them on the next entry after a Load.
  - Inject: Immediately after /db/load, and after a follower restart that installs a snapshot.
  - Observe: The CDC sink receives events for every write acknowledged after the load. No acked post-load write is missing from the stream.
  - Workload: With CDC enabled: insert rows (events seen), POST /db/load with a valid file, insert more rows, then check the sink for post-load events. Also kill a follower, write past the snapshot threshold, and restart it to force a restore.
  - Guess: Guess: re-registration may only happen for the Load path, not the Restore or recovery paths, which also call Swap.

## repo-readme.md

- **All SQL statements sent together in one API request take effect together or not at all, even if the node crashes partway.** (process-kill, high)
  - Quote: > **Atomic Requests**: Execute multiple SQL statements atomically within a single API request.
  - Inject: While a multi-statement /db/execute request is in flight: after the request is sent but before the HTTP response arrives. Repeat with randomized delays of 0-50 ms after send.
  - Observe: After restart, query the table. The rows written by that request must be either all present or all absent, never a partial subset. The outcome must also match the client log: if a 200 response with no per-statement errors was received, all rows must be present.
  - Workload: 1) Start rqlited -node-id 1 -http-addr localhost:4001 data/. 2) POST /db/execute with ["CREATE TABLE t (id INTEGER PRIMARY KEY, batch INTEGER)"]. 3) Loop: POST /db/execute?transaction with ["INSERT INTO t VALUES(k*100+0,k)", ... "INSERT INTO t VALUES(k*100+99,k)"]. Log send time, response, and k to Fuss's history file. 4) SIGKILL rqlited mid-request. 5) Restart with the same data dir. 6) GET /db/query?level=strong&q=SELECT batch, COUNT(*) FROM t GROUP BY batch. Every count must be 0 or 100, and every acked batch must equal 100.
  - Guess: Guess: the README does not say whether atomicity needs the documented 'transaction' query parameter. Without it, a statement in the middle that fails, or a crash during SQLite apply after the Raft commit, could leave a partial batch visible. Fuss should run both with and without ?transaction and report them separately.
- **If one statement in a multi-statement request fails, none of that request's statements are kept.** (process-kill, medium)
  - Quote: > **Atomic Requests**: Execute multiple SQL statements atomically within a single API request.
  - Inject: During a batch where a late statement is designed to fail (for example, a primary-key violation). Also kill right after the error response returns, to check that the rollback survives restart.
  - Observe: After the response, and again after restart, none of the batch's earlier, valid inserts are visible.
  - Workload: POST /db/execute?transaction with ["INSERT INTO t VALUES(5000,1)", "INSERT INTO t VALUES(5001,1)", "INSERT INTO t VALUES(1,1)"], where id 1 already exists. Record the response. SIGKILL, restart, then GET /db/query?level=strong&q=SELECT COUNT(*) FROM t WHERE id IN (5000,5001). The count must be 0.
  - Guess: Guess: on replay of the Raft log after restart, a failed transaction's earlier statements might be re-applied differently than on the original apply.
- **In a multi-node cluster, killing one node does not stop the cluster from serving writes and reads, and does not lose writes the cluster acknowledged.** (process-kill, high)
  - Quote: > **High Availability**: Fully replicated database provides fault-tolerance. Outage of a node doesn't impact the cluster.
  - Inject: Kill one node of a 3-node cluster at random times during a steady write workload. Include the moment just after the leader acks a write, and kills of the current leader (found via GET /status).
  - Observe: 1) Writes sent to a surviving node keep succeeding after a short election window. 2) Every write acked before or after the kill is present when queried with level=strong from any surviving node. 3) After the killed node restarts and rejoins, it serves the same data.
  - Workload: Start 3 nodes: node1 with -http-addr :4001 -raft-addr :4002, and nodes 2 and 3 with -join localhost:4002. Create table t. Run a writer loop that POSTs /db/execute ["INSERT INTO t VALUES(n, ...)"] to any live node, follows redirects, and logs n plus the ack to the history file. SIGKILL the leader. Keep writing to the survivors. Restart the killed node. Query SELECT id FROM t with level=strong on each node. All acked ids must be present, and no id may be inserted twice.
  - Guess: Guess: a write acked by the old leader just before it is killed might not be counted as committed by the new leader. Or a client retry after a timeout during the election could apply a non-idempotent write twice. That double-apply would be the client's fault rather than rqlite's, so Fuss should use unique ids and treat timeouts as indeterminate.
- **Data whose write was acknowledged survives a crash and restart of the node.** (restart, medium)
  - Quote: > Use rqlite to reliably store your most important data, ensuring it's always available to your applications
  - Inject: SIGKILL a single node immediately after receiving a 200 response to a write. Also kill at random points during a write burst.
  - Observe: After restart, every write whose success Fuss logged is returned by a level=strong query. Writes that timed out may be present or absent, but never partially.
  - Workload: Single node: loop POST /db/execute ["INSERT INTO t VALUES(n,'x')"], logging the ack per n. SIGKILL rqlited. Restart with the same data dir. GET /db/query?level=strong&q=SELECT id FROM t. Compare against the acked set.
  - Guess: Guess: if writes are acked before the Raft log is fsynced, an abrupt kill loses them. This is likely under a host crash, not just a process kill. Fuss can only simulate a process kill, so it should note that limit.
- **Running out of disk does not corrupt existing data or cause acknowledged writes to be lost; writes should fail visibly instead.** (disk-full, medium)
  - Quote: > Use rqlite to reliably store your most important data, ensuring it's always available to your applications
  - Inject: Fill the filesystem holding the data directory, using a small loop-mounted or tmpfs volume, during a steady write workload, then free space and restart.
  - Observe: 1) Writes during the full-disk period either get an error response or succeed and stay durable. 2) After space is freed and the node restarts, it starts cleanly and all previously acked rows are present.
  - Workload: Run rqlited with its data dir on a 64 MB tmpfs. Write loop with large TEXT values, logging acks. Run dd to fill the remaining space. Keep writing, recording errors. rm the filler file. SIGKILL and restart. Query with level=strong and compare against the acked set.
  - Guess: Guess: a partially written Raft log segment or snapshot from the full-disk moment might fail to load on restart, so the node will not start or drops entries.
- **A backup can be taken while the database is serving writes, and the backup is a consistent, valid snapshot.** (concurrent-calls, medium)
  - Quote: > **Effortless Backups**: Hot [backups](https://rqlite.io/docs/guides/backup/)
  - Inject: Request GET /db/backup while concurrent atomic multi-statement writes are running. Optionally kill the node right after the backup completes.
  - Observe: 1) The downloaded file opens as a valid SQLite database (Fuss's own sqlite read). 2) Every batch in it is complete (all or none of each batch's rows). 3) Every write acked before the backup request started is present.
  - Workload: Writer goroutines POST /db/execute?transaction with 10-row batches, logging acks. Concurrently, curl GET localhost:4001/db/backup -o b.sqlite several times. For each backup, open it read-only, run SELECT batch, COUNT(*) GROUP BY batch, and check that each count is 0 or 10 and that all batches acked before the backup are present.
  - Guess: Guess: a backup that copies the SQLite file while a write is being applied could capture a torn state if it does not snapshot under a lock or use the SQLite backup API.
- **Restoring from a SQLite file loads that database's contents into rqlite, and the restored data survives a restart.** (process-kill, low)
  - Quote: > [restore directly from SQLite](https://rqlite.io/docs/guides/backup/#restoring-from-sqlite)
  - Inject: SIGKILL right after the restore (load) request returns success. Separately, kill during the restore request.
  - Observe: 1) If the restore was acked, the restart shows exactly the source file's rows. 2) If the restore was killed mid-way, the database shows either the old contents or the full new contents, not a mix. The all-or-nothing part is a guess, because the README does not promise it.
  - Workload: Create src.sqlite with known rows. POST it to the documented load endpoint (/db/load with Content-Type application/octet-stream). Record the response. SIGKILL. Restart. GET /db/query?level=strong for row counts and checksums, and compare against src.sqlite.
  - Guess: Guess: the restore may replace the database outside the normal Raft log path, so a kill before the next snapshot could leave old data or a mix after restart.
- **Reads at the strongest documented consistency level reflect all previously acknowledged writes, even across leader changes. Writes in the default (non-queued) mode are durable once acked.** (clock-skew, low)
  - Quote: > **Tunable Consistency**: Customize [read consistency](https://rqlite.io/docs/api/read-consistency/) and [durability](https://rqlite.io/docs/api/queued-writes/) to match your application's needs.
  - Inject: Skew the clock on the leader or a follower by several seconds (for example, by running the node under faketime or a time namespace) while alternating writes and level=strong reads. Combine with killing the leader.
  - Observe: Each level=strong read issued after a write's ack must include that write; a linearizability check runs over Fuss's history file. Lower levels (none, weak) may serve stale data, so Fuss records that as expected rather than as a violation.
  - Workload: 3-node cluster. Client A: POST /db/execute ["UPDATE kv SET v=n WHERE k=1"] and log the ack. Client B: GET /db/query?level=strong&q=SELECT v FROM kv WHERE k=1 against random nodes. Inject clock skew on the leader, then SIGKILL it. Check that no strong read returns a value older than the latest write acked before the read started.
  - Guess: Guess: if any read level relies on leader leases tied to wall-clock or monotonic timing, skew could let a deposed leader serve stale reads. The README does not say which level is lease-based, so the exact expectations must come from the linked read-consistency doc.

## repo-snapshot-design.md

- **Once log entries are covered by a snapshot and truncated, the snapshot store still holds the full database state up to that point. No acknowledged write may be lost.** (process-kill, high)
  - Quote: > If the Raft log is truncated up to index N, the snapshot store must be able to produce a complete database state at index N. Everything in this package exists to maintain that invariant.
  - Inject: Repeatedly SIGKILL the node right after a snapshot is taken. Detect this by polling GET /status for an advancing snapshot index, or by watching a new snapshot directory appear in the data dir.
  - Observe: After restart, a strong read returns every row whose POST /db/execute was acknowledged. No unacknowledged row appears twice or out of order.
  - Workload: 1. Start `rqlited -node-id 1 -raft-snap 8 -raft-snap-int 1s <datadir>`.
2. POST /db/execute CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT).
3. Loop: POST /db/execute INSERT INTO t VALUES(k,'x'), logging each acknowledgment to the Fuss history file.
4. kill -9 when a snapshot appears, then restart.
5. GET /db/query?level=strong&q=SELECT id FROM t, then diff against the history.
  - Guess: Guess: if the log is truncated before the incremental WAL move is durably fsynced, a kill could leave a gap between the snapshot and the remaining log.
- **A snapshot directory either appears whole under its final name or not at all. Startup cleans up leftover .tmp directories.** (process-kill, high)
  - Quote: > The sink writes to a temporary directory (`<id>.tmp`) and atomically renames it into place on successful `Close`.
  - Inject: While a snapshot is being written, i.e. while a `*.tmp` directory exists in the snapshot store directory under the data dir.
  - Observe: After restart: no `*.tmp` directories remain. Every non-tmp snapshot directory has a meta.json and at least one data file, each with a .crc32 sidecar. Acknowledged data is intact.
  - Workload: 1. Start with -raft-snap 8.
2. Run a heavy insert loop.
3. Poll the snapshot store directory every few ms and kill -9 as soon as a `.tmp` dir is seen.
4. Restart.
5. List the directories and run a strong SELECT count(*) / id check.
  - Guess: Guess: the rename may not be followed by an fsync of the parent directory, so after a kill the rename could be lost or a half-written directory could be left behind.
- **Every data.db and .wal file in every snapshot has a sidecar whose checksum matches the file contents, even after crashes.** (process-kill, high)
  - Quote: > Every data file (DB or WAL) has a `.crc32` sidecar file containing its CRC32 checksum.
  - Inject: During snapshot creation and during reaping (checkpoint and sidecar recompute).
  - Observe: With the node stopped, Fuss reads each data file, computes CRC32 itself, and compares the result to the sidecar. Any missing or mismatching sidecar is a violation.
  - Workload: 1. Run an insert loop with -raft-snap 8.
2. kill -9 at random points.
3. Before restarting, walk the snapshot dirs (read-only) and verify CRC32 against each .crc32 file.
4. Restart and repeat.
  - Guess: Guess: during reap the DB is checkpointed (step 3) before the sidecar is recomputed (step 4). A kill between them leaves a stale sidecar until the plan is re-executed. Check whether it is fixed after restart, not just before.
- **On disk there is a full snapshot containing data.db. Every snapshot newer than the newest full snapshot contains only .wal files.** (process-kill, medium)
  - Quote: > The invariant is: after the newest full snapshot, all subsequent snapshots must be incremental.
  - Inject: At random times during sustained writes with frequent snapshots, including during reap.
  - Observe: Sort the snapshot dirs by the term, index and ID in meta.json and the dir name. Check that at least one dir has data.db and that no dir after the newest full lacks .wal files. The node must also restart successfully.
  - Workload: 1. Start with -raft-snap 4 -raft-snap-int 500ms.
2. Run an insert loop with random kill -9 and restart.
3. After each stop, inspect the directory contents and meta.json files read-only.
  - Guess: Guess: a crash after an incremental is committed but while FULL_NEEDED handling is in flux could leave an incremental with no full base.
- **If finishing an incremental snapshot fails, the node exits instead of continuing with a broken chain. On restart it recovers from the last good snapshot without losing acknowledged writes.** (disk-full, medium)
  - Quote: > If `Close` fails during an incremental snapshot, the sink calls a fatal function that terminates the process. This is a deliberate design choice: an interrupted incremental snapshot breaks the WAL chain, and a hard restart (which recovers from the last good snapshot) is preferable to requiring a full snapshot, which would pause write traffic for longer.
  - Inject: Fill the filesystem holding the data dir (e.g. a small loop/tmpfs mount) just as an incremental snapshot is being finalized.
  - Observe: The rqlited process exits on its own with a non-zero status and does not keep serving with a broken chain. After freeing space and restarting, a strong read shows all acknowledged writes.
  - Workload: 1. Mount a small filesystem and use it as the data dir.
2. Start with -raft-snap 8.
3. Run an insert loop.
4. When a snapshot begins, fill the disk with a filler file.
5. Observe whether the process exits.
6. Delete the filler and restart.
7. Run a strong SELECT and diff against the history.
  - Guess: Guess: a failure that occurs before Close (e.g. in Write or staging) might not reach the fatal path, so the node keeps running with an inconsistent chain.
- **A crash at any point during reaping is fully repaired on restart. The pending plan is re-run to completion and no data is lost or duplicated.** (process-kill, medium)
  - Quote: > The package uses a **plan-execute pattern**: the entire sequence of operations is serialized as a JSON plan file (`REAP_PLAN`) before any mutations begin. If the process crashes mid-execution, `Store.check()` on startup detects the plan file and re-executes it. Every operation in the plan (rename, remove, checkpoint, etc.) is idempotent, so re-execution is safe.
  - Inject: While a REAP_PLAN file exists in the snapshot store directory. Repeat kills during the restart's re-execution too, to test idempotency.
  - Observe: After a clean restart: REAP_PLAN is gone. Exactly one full snapshot remains, and its meta.json term/index equals the newest pre-reap snapshot's. CRC sidecars match. A strong read returns all acknowledged rows.
  - Workload: 1. Start with -raft-snap 4 -raft-snap-int 500ms so incrementals pile up and trigger the reaper (the reap threshold flag name, if exposed, is unknown).
2. Run an insert loop.
3. Poll for REAP_PLAN and kill -9 when it appears.
4. Restart, and optionally kill again within ms of startup.
5. Restart cleanly.
6. Inspect files and run a strong SELECT.
  - Guess: Guess: a checkpoint op re-run against an already-checkpointed DB, or a rename whose source is already gone, may not truly be idempotent.
- **If reaping fails while running, the node must not serve snapshots built from the half-reaped store. A restart completes the plan first.** (disk-full, low)
  - Quote: > If reaping fails while the store is running, `Open` returns `ErrReapPending` while the `REAP_PLAN` file remains. A partially executed plan may have changed the database without updating its snapshot metadata, so it cannot be served safely. Retrying `Reap`, or restarting the store, completes the pending plan before snapshot reads can resume.
  - Inject: Fill the disk while REAP_PLAN exists, i.e. during the checkpoint step.
  - Observe: While REAP_PLAN persists, a new node joining via -join must not end up with data that differs from the leader. It should fail or wait instead. After freeing space and restarting the leader, REAP_PLAN disappears and the joiner's level=none reads match the leader.
  - Workload: 1. Start leader N1 on a small mount with -raft-snap 4.
2. Run inserts until a REAP_PLAN appears.
3. Fill the disk.
4. Start N2 with -join to N1.
5. Compare N2 /db/query?level=none against the history.
6. Free space and restart N1.
7. Recheck N2.
  - Guess: Guess: a snapshot opened just before the failure, or a Raft-internal Open retry, might stream partially checkpointed files with stale metadata.
- **A snapshot sharing term and index with an existing one (e.g. the renamed full produced by reaping) always sorts as newer, even if the clock jumped backwards.** (clock-skew, low)
  - Quote: > Snapshot IDs are generated so that a snapshot always sorts as newer than any existing snapshot with the same term and index: the timestamp field is the larger of the current wall-clock time and one more than the largest timestamp found among those existing snapshots. Ordering therefore stays correct even if the system clock moves backwards between the creation of two such snapshots.
  - Inject: Step the system clock backwards by minutes or hours right before a reap renames the full snapshot directory, or between two snapshots at the same term/index.
  - Observe: After the reap, the timestamp parsed from the new directory name exceeds the timestamps of earlier dirs with the same term/index. After restart, the node serves the newest data, with no rollback of acknowledged writes.
  - Workload: 1. Start with -raft-snap 4.
2. Run inserts until incrementals accumulate.
3. Set the clock back 1h.
4. Keep inserting to trigger a reap.
5. Read the directory names and meta.json files.
6. Restart and run a strong SELECT.
  - Guess: Guess: the max-timestamp scan might only consider snapshots matching exactly, or the ID format may lose precision, so the same timestamp is produced and the ID sort ties or inverts.
- **A slow follower receiving a snapshot does not stop the leader from taking new snapshots and truncating its log.** (other, medium)
  - Quote: > This means the leader can continue creating new snapshots while simultaneously streaming an older snapshot to a lagging follower.
  - Inject: While the leader is streaming a snapshot to a follower that has been throttled. Throttle it by SIGSTOP/SIGCONT cycles or by limiting its disk/network externally.
  - Observe: During the transfer, new snapshot directories keep appearing on the leader, the snapshot index in GET /status keeps advancing, and the leader's Raft log file stays bounded. Writes stay acknowledged without large latency spikes.
  - Workload: 1. Start N1 with -raft-snap 8 and preload many rows so snapshots are large.
2. Start N2 with -join and immediately SIGSTOP it intermittently.
3. Keep inserting on N1.
4. Sample N1's snapshot dir, raft log size and /status every second.
  - Guess: Guess: the reaper waits for the write lock behind the long-lived reader, and if Create also contends with the pending writer, snapshot creation could still stall.
- **Reaping never deletes or rewrites snapshot files while a snapshot is being streamed, so the receiver always gets a consistent snapshot.** (concurrent-calls, low)
  - Quote: > Reaping requires the write lock and waits for all active readers to finish.
  - Inject: Trigger reaping on the leader (by producing many snapshots) while a joining follower is mid-transfer.
  - Observe: After the transfer, the new follower's level=none reads contain exactly the leader's acknowledged rows up to the snapshot point. The follower does not crash or report a checksum failure.
  - Workload: 1. Start N1 with -raft-snap 4 and preload data.
2. Start N2 with -join.
3. While N2 is receiving, run rapid inserts on N1 to force snapshots and a reap.
4. Compare SELECT results between N1 (strong) and N2 (level=none, after catch-up).
  - Guess: Guess: if the streamer lazily opens WAL files from later incrementals after acquiring the lock, a reap racing before lock acquisition could remove them.
- **A node that installs a snapshot from the leader ends up with exactly the leader's state, even if it is killed mid-install and restarted.** (process-kill, medium)
  - Quote: > `Restore` reads a protobuf-framed snapshot stream (the same format produced by `SnapshotStreamer`) and writes the resulting SQLite database to a destination path. If the stream contains WAL files, they are extracted to temporary files and checkpointed into the database.
  - Inject: Kill the follower while it is receiving or restoring a snapshot that contains WAL files.
  - Observe: After the follower restarts and catches up, its level=none query results equal the leader's strong results, with no missing or duplicate rows.
  - Workload: 1. Start N1 with -raft-snap 8 and insert many rows.
2. Start N2 with -join.
3. kill -9 N2 during catch-up, while its data dir is growing.
4. Restart N2.
5. Compare SELECT * between N1 and N2.
  - Guess: Guess: a kill after the DB is written but before the WAL checkpoint could leave a DB the follower treats as complete.
- **After a failed snapshot attempt, the staged WAL is kept and reused by the next attempt, without losing or double-applying those changes.** (disk-full, low)
  - Quote: > A key property of the staging directory is **persistence across failed snapshots**. If `Persist` fails after the WAL has been written and closed (but before the snapshot is finalized), the staging directory retains the WAL files. The next snapshot attempt can pick them up rather than requiring a full snapshot.
  - Inject: Fill the disk between WAL staging and snapshot finalization, then free space before the next snapshot attempt.
  - Observe: Two things to check:
- Correctness: all acknowledged rows are present exactly once after the next snapshot and a restart.
- Optional: the next snapshot dir contains only .wal files, no data.db.
  - Workload: 1. Use a small mount as the data dir and start with -raft-snap 8.
2. Run an insert loop.
3. Fill the disk at a snapshot attempt.
4. Free space and continue inserting until the next snapshot.
5. kill -9 and restart.
6. Run a strong SELECT and inspect the snapshot dirs.
  - Guess: Guess: stale staged WAL might be re-applied on top of a snapshot that already includes it, or be combined with new WAL in the wrong order.
- **The first snapshot, and the first snapshot after a broken chain, is a full snapshot containing data.db.** (restart, medium)
  - Quote: > The `FULL_NEEDED` flag file signals that the next snapshot must be a full snapshot rather than an incremental one. This is set when no snapshots exist (first snapshot), or when the system detects the incremental chain is broken and cannot be continued.
  - Inject: Kill the node right after FULL_NEEDED appears and before the first snapshot completes, then restart.
  - Observe: The oldest surviving snapshot after the first snapshot completes contains data.db. FULL_NEEDED is gone after a full snapshot succeeds. Data is intact.
  - Workload: 1. Start a fresh node with -raft-snap 4.
2. Insert rows.
3. Watch for FULL_NEEDED and kill -9 at that moment.
4. Restart.
5. Insert until a snapshot completes.
6. Inspect the dirs and run a strong SELECT.
  - Guess: Guess: if FULL_NEEDED is created after store init rather than atomically with it, a kill in between could allow an incremental with no base.
- **Upgrading a data dir from an older rqlite format survives a crash mid-upgrade. The old format is only removed after the migration succeeds.** (process-kill, medium)
  - Quote: > These use the same plan-execute pattern as reaping for crash safety. The v7 format stored the database as a gzip-compressed blob; v8 stored it as a bare `.db` file at the store root; v10 stores it inside the snapshot directory as `data.db` with a CRC32 sidecar. Upgrades are run once on startup and remove the old format directory after successful migration.
  - Inject: Kill the new v10 binary within milliseconds of its first startup on a data dir created by an unmodified v8 (or v7) binary. Repeat on the next startup.
  - Observe: Either the old-format directory still exists, or a valid v10 snapshot dir with data.db and a matching .crc32 does. Never neither. After a clean start, a strong read returns all rows written under the old version.
  - Workload: 1. Start an unmodified rqlite 8.x binary with -raft-snap 4.
2. Insert rows and stop it.
3. Start the v10 binary on the same dir and kill -9 after random 0-200ms delays, several times.
4. Start cleanly.
5. Inspect the directories and run a strong SELECT.
  - Guess: Guess: the old directory might be removed by a plan step that runs before the new data.db is fsynced.

## repo-store-design.md

- **A write that rqlite acknowledges is replicated and durable, so it survives node crashes and restarts.** (process-kill, high)
  - Quote: > The package's job is to take the simple-sounding contract "a SQL request goes in, a result comes out, replicated and durable" and make it actually true on top of Raft consensus, SQLite, a custom snapshot store, and a SQLite driver with hooks.
  - Inject: Immediately after a 200 response to /db/execute arrives. Separately, mid-request before the response, and during a snapshot.
  - Observe: After restarting all killed nodes and electing a leader, a level=strong read returns every row whose insert was acknowledged. Rows from unacknowledged requests may be present or absent but never duplicated or partial.
  - Workload: Start a 3-node cluster. CREATE TABLE t(id INTEGER PRIMARY KEY, v). Loop INSERT INTO t VALUES(n, n) via POST /db/execute and log acknowledgements to the Fuss history file. Randomly SIGKILL the leader or a follower and restart it with the same flags. At the end, run GET /db/query?level=strong 'SELECT id FROM t' and diff against the history.
  - Guess: Guess: steady-state synchronous=OFF means SQLite pages may be lost on crash, so durability relies entirely on Raft log replay. A bug in the restart fast path could skip that replay.
- **Every acknowledged execute carries the Raft index of its committed entry, and these indices increase in commit order.** (concurrent-calls, medium)
  - Quote: > **`Execute`** — write-only path. Always goes through Raft consensus. Leader-only. Returns the row counts and the Raft index of the committed entry.
  - Inject: While many clients write concurrently, with occasional leader kills.
  - Observe: Raft indices in execute responses are unique across all acknowledged writes, never decrease for sequential writes from one client, and agree with the order seen in a later strong read (for example, an autoincrement id ordered by index).
  - Workload: Run N goroutines issuing POST /db/execute (requesting the raft_index in the response if the API exposes it) with INSERT INTO t(v) VALUES(clientid_seq). Record each index. Kill the leader periodically. Check uniqueness and per-client monotonicity.
  - Guess: Guess: after a leader change, a retried or forwarded request might report the index of a different entry.
- **A /db/request containing any write is committed through Raft and is therefore durable and replicated, even if it also contains reads.** (process-kill, medium)
  - Quote: > **`Request`** — unified path that accepts a mix of reads and writes. If the request contains *any* writes, or asks for `STRONG`, it goes through Raft. If it is all reads at a non-strong level, it falls through to a direct SQLite query — the same code path as `Query`.
  - Inject: Right after acknowledgement of a mixed read/write /db/request.
  - Observe: The write part is visible on all nodes after restart. The Raft applied index in /status advances for mixed requests but not for pure weak reads.
  - Workload: POST /db/request with ["INSERT INTO t VALUES(...)", "SELECT COUNT(*) FROM t"]. Compare the /status raft applied index before and after. Repeat with pure SELECT at level=weak. Kill the leader after acknowledgement, restart, and verify the row is present.
  - Guess: Guess: misclassifying a statement as read-only (for example, a write inside a CTE or RETURNING clause) could route it to the local SQLite path without replication.
- **A level=none read with a freshness bound fails, rather than returning data, when the follower has not heard from the leader within that window. With strict, it also fails when the follower is behind the leader's commit index.** (clock-skew, high)
  - Quote: > **`NONE`** — direct local read, no leader check. Optionally bounded by a `freshness` window: if the leader has not been heard from in too long (or, with `strict`, the local FSM is behind the leader's known commit index), the read fails with `ErrStaleRead`. The freshness check is in `IsStaleRead` (`state.go`), and depends on `appendedAtTime` — the leader-clock time captured from `AppendEntries` RPCs by `NodeTransport`.
  - Inject: Skew the leader's clock relative to a follower (forward and backward). Separately, kill the leader and query the follower after the freshness window elapses.
  - Observe: The follower returns a stale-read error instead of rows once the leader has been gone longer than the freshness value. Reads that succeed with freshness_strict never miss a write acknowledged before the read was sent.
  - Workload: Start a 3-node cluster. Write rows continuously. Query the follower with GET /db/query?level=none&freshness=1s[&freshness_strict] 'SELECT MAX(id) FROM t'. SIGKILL the leader, wait 2s, query the follower again and expect a stale error. Repeat with leader clock skewed by ±30s.
  - Guess: Guess: appendedAtTime is a leader-clock timestamp compared against the follower's clock, so skew could make stale reads look fresh, or fresh reads look stale.
- **A weak read is served only by a node that believes it is leader. Other nodes refuse it locally (the HTTP layer may forward or redirect instead).** (process-kill, medium)
  - Quote: > **`WEAK`** — direct local read, but the node must be the leader. Cheap and the default for most reads. Non-leaders return `ErrNotLeader`.
  - Inject: Kill the leader, then issue weak reads to the surviving followers before a new leader is elected.
  - Observe: With redirect enabled, followers return a redirect or not-leader error rather than local data. No weak read ever returns data from a node that /status shows as follower at that moment.
  - Workload: GET /db/query?level=weak&redirect against each node while SIGKILLing the leader. Record responses and /status raft state.
  - Guess: Guess: the leadership check is a local flag, so a partitioned old leader may still believe it is leader. Weak is not promised linearizable, so this tests only the local check.
- **A linearizable read always sees every write acknowledged before the read was issued.** (process-kill, high)
  - Quote: > **`LINEARIZABLE`** — guaranteed to reflect everything committed before the request started.
  - Inject: During concurrent writes and reads. Kill the leader at random points, including between a write acknowledgement and a following read.
  - Observe: The recorded history of register writes and linearizable reads passes a linearizability checker. No read returns a value older than one already acknowledged before it started.
  - Workload: Model a set of keys as registers. Clients do UPDATE kv SET v=? WHERE k=? via /db/execute and SELECT v FROM kv WHERE k=? via /db/query?level=linearizable, logging invoke and complete times. Kill and restart the leader every few seconds. Check the history with a Porcupine-style checker.
  - Guess: Guess: the commit-index capture, heartbeat verification and term check race with a leadership change between steps.
- **A freshly elected leader never returns stale data for its first linearizable read in a new term.** (process-kill, medium)
  - Quote: > There is one wrinkle: if no Strong read has yet gone through the Raft log in the current term, the linearizable read upgrades itself to a Strong read so the leader can establish that it has actually committed something in this term. Without that upgrade, a freshly-elected leader could return a result based on a stale committed index.
  - Inject: Kill the leader right after a write is acknowledged, then send a linearizable read to the newly elected leader as soon as /status shows it is leader.
  - Observe: The first linearizable read on the new leader includes the last write acknowledged by the old leader. /status raft applied_index advances by one on that first read in the new term, but not on later ones.
  - Workload: Loop: INSERT a row with id=n, wait for acknowledgement, SIGKILL the leader, poll /status on the others until one is leader, immediately GET /db/query?level=linearizable 'SELECT MAX(id) FROM t' on it, and check the result is n or higher. Restart the killed node.
  - Guess: Guess: the per-term flag might not be reset when term changes happen without this node observing them, such as a quick re-election of the same node.
- **Strong reads are ordered in the Raft log with writes and are linearizable.** (process-kill, high)
  - Quote: > **`STRONG`** — the read is itself sent through `raft.Apply` as a `COMMAND_TYPE_QUERY` entry. Slowest, but the read is processed by every node's FSM in log order, which makes it trivially linearizable.
  - Inject: During mixed writes and strong reads, with leader kills.
  - Observe: The history of writes and level=strong reads is linearizable. Each strong read advances the Raft index.
  - Workload: Use the same register workload as for linearizable, with level=strong. Kill and restart nodes. Run a linearizability check.
  - Guess: Guess: a strong read applied, but whose response was lost on leader death, could be retried by a forwarding layer. That is harmless for reads but tests the retry path.
- **level=auto behaves like weak on voter nodes and like none (local read) on read-only nodes.** (process-kill, medium)
  - Quote: > `AUTO` resolves to `WEAK` on voters and `NONE` on non-voters.
  - Inject: Kill the leader so no leader exists, then read with level=auto on a voter follower and on a non-voter.
  - Observe: The non-voter still answers from local data. The voter follower returns not-leader (or forwards or redirects) rather than local data.
  - Workload: Start 3 voters and 1 node with -raft-non-voter=true. Write rows. SIGKILL the leader. GET /db/query?level=auto&redirect against the non-voter and against a voter follower.
  - Guess: Guess: the voter/non-voter status could be read from a stale configuration after membership changes.
- **Snapshots, backups and the startup CRC check never run at the same time, so backups taken during snapshot activity are consistent.** (concurrent-calls, medium)
  - Quote: > **Acquire the snapshot CAS gate** (`snapshotCAS`). This is mutually exclusive with backups and with the startup clean-snapshot CRC verification — only one of {snapshot, backup, CRC verify} runs at a time, system-wide.
  - Inject: Fire GET /db/backup repeatedly while a write load forces frequent snapshots, and right after a restart while the CRC check is still running.
  - Observe: Every downloaded backup is a valid SQLite file that passes integrity_check and whose contents match a prefix of the acknowledged write history. Backup calls may fail or wait, but never return corrupt data.
  - Workload: Start rqlited with a low snapshot threshold (for example -raft-snap=50 and a short -raft-snap-int). Write continuously. Run concurrent GET /db/backup calls and validate each file with sqlite3 'PRAGMA integrity_check'. Restart the node and issue backups immediately.
  - Guess: Guess: if a backup path skips the gate, it could read db.sqlite while a checkpoint is rewriting pages.
- **Data captured by a snapshot is durable, so a crash right after or during a snapshot does not lose committed data.** (process-kill, medium)
  - Quote: > **Switch SQLite to `synchronous=FULL`.** Snapshots assume that every page that has reached the database file is durable; FULL synchronous mode guarantees that.
  - Inject: During snapshot creation, and immediately after one completes (detectable from new files appearing in the snapshot directory).
  - Observe: After restart, all acknowledged writes are present and the database passes integrity_check through a query.
  - Workload: Use a low snapshot threshold. Write continuously. Watch the data directory's snapshot folder and SIGKILL when a new snapshot directory appears or is still being written. Restart and verify against history.
  - Guess: Guess: process kill does not drop the OS page cache, so this mostly tests logic rather than fsync. A real power-loss model is outside Fuss's reach.
- **A failed incremental snapshot does not break the snapshot chain: the next snapshot is a full one and no data is lost.** (disk-full, medium)
  - Quote: > If the snapshot was invoked, *failed*, and the WAL staging directory is no longer present (so the WAL data has been moved into the snapshot tmp dir and lost when that was cleaned up), then the chain is broken and the next snapshot must be a full one — `OnRelease` calls `snapshotStore.SetDueNext(Full)`.
  - Inject: Fill the disk holding the data directory just as an incremental snapshot is being persisted, then free space.
  - Observe: The next successful snapshot in the snapshot directory is a full one (it contains a database file rather than only a WAL). After a kill and restart, all acknowledged writes are present.
  - Workload: Put the data directory on a small loop or tmpfs mount. Use a low snapshot threshold with writes. Once an initial full snapshot exists, fill the filesystem with a ballast file until a snapshot error appears in the logs. Delete the ballast, write more, wait for the next snapshot, inspect the snapshot directory, SIGKILL, restart, and verify.
  - Guess: Guess: if the failure happens after the staging directory moves but before the error propagates, the FULL_NEEDED flag might not be set or not persisted across a restart.
- **An abandoned incremental snapshot loses no WAL data; its contents are included in the next snapshot.** (restart, low)
  - Quote: > If the snapshot was never invoked at all (Raft decided it did not need it), the staging directory is left intact and the next snapshot picks up where this one left off, packaging the leftover WAL together with whatever is newly compacted.
  - Inject: Kill the process while staged WAL exists in the staging directory but no snapshot has completed, then restart.
  - Observe: After further snapshots and a restart, all acknowledged writes are present. The staging directory contents disappear once a later snapshot succeeds.
  - Workload: Use a low WAL-size snapshot threshold with writes. Poll the data directory for staging WAL files and SIGKILL when they appear. Restart, write more, force more snapshots, restart again, and verify against history.
  - Guess: Guess: leftover staging data from a previous process might be ignored or mis-ordered after restart.
- **After each successful snapshot, a clean_snapshot file exists whose mtime, size and CRC32 match the current SQLite file.** (process-kill, medium)
  - Quote: > After every successful snapshot, `createSnapshotFingerprint` writes a small JSON file (`clean_snapshot`) to the Raft directory containing:

- The SQLite file's modification time
- Its size
- Its CRC32
  - Inject: Kill immediately after a snapshot completes, and while the fingerprint file is being written.
  - Observe: Read clean_snapshot and compare its fields to stat and CRC32 of db.sqlite, measured right after the snapshot with writes paused. After a kill, the file is either absent or well-formed JSON, never truncated, and the restart still yields correct data.
  - Workload: Start a node with a low snapshot threshold. Write past the threshold, pause writes, wait for the snapshot, read clean_snapshot, and compute CRC32 of db.sqlite. Repeat, with SIGKILL timed right after the snapshot directory appears.
  - Guess: Guess: a non-atomic write of the fingerprint could leave a torn JSON file that causes a startup failure.
- **Restarting with a matching fingerprint reuses the existing database file, and the restarted node still serves exactly the committed data.** (restart, high)
  - Quote: > On `Open`, the Store checks for this file. If it exists, the SQLite file's mtime and size still match, and the snapshot store has at least one snapshot, the Store sets `raftConfig.NoSnapshotRestoreOnStart = true` — telling Raft to bring up the FSM without re-restoring from the snapshot. The local SQLite file is reused as-is.
  - Inject: Do a clean restart (SIGTERM) after a snapshot. Also SIGKILL after post-snapshot writes, which change the database file.
  - Observe: In both cases, after restart a strong read matches the acknowledged history. A clean restart is noticeably faster on a large database, and the logs show the restore was skipped.
  - Workload: Load a large dataset. Force a snapshot. Restart with SIGTERM, time /readyz, and verify rows. Then write more rows (forcing a checkpoint if possible), SIGKILL, restart, and verify that all acknowledged rows exist.
  - Guess: Guess: mtime has coarse granularity, so a modification within the same timestamp tick with an unchanged size could wrongly enable the fast path. The async CRC check is then the only safety net.
- **Loading a database or installing a snapshot from the leader removes the fingerprint first, so a crash afterwards cannot reuse a stale file.** (process-kill, high)
  - Quote: > Any code path that invalidates the SQLite file relative to the snapshot store — `fsmRestore`, `RecoverNode`, `Load`, manual swap — removes the fingerprint file before swapping. On the next restart, the absence of the fingerprint forces a normal snapshot-restore.
  - Inject: During and immediately after POST /db/load, and on a follower while it installs a snapshot sent by the leader.
  - Observe: Poll the data directory: clean_snapshot disappears before or as the load or restore happens and reappears only after a later snapshot. After a kill and restart, the node serves the loaded data or the leader's data, never pre-load data.
  - Workload: Write dataset A. Snapshot so the fingerprint exists. POST /db/load with SQLite file B and SIGKILL at random offsets during or after it. Restart and query: the result must be entirely A (load not acknowledged) or entirely B, matching the acknowledgement. For the follower case, stop the follower, write many entries plus a snapshot on the leader, start the follower to trigger a snapshot install, and kill it mid-install.
  - Guess: Guess: if the swap happens before the fingerprint removal is durable, a crash in between leaves a fingerprint that matches neither file.
- **A node silent longer than the reap timeout is removed from the cluster automatically, and only then.** (clock-skew, high)
  - Quote: > If the failing node has been silent for longer than `ReapTimeout` (or `ReapReadOnlyTimeout` for non-voters), and the timeouts are non-zero, the leader removes it from the configuration.
  - Inject: Kill a voter or non-voter and leave it down. Separately, skew the leader's clock or kill the leader during the countdown.
  - Observe: /nodes on the leader stops listing the dead node after roughly the configured timeout, not much earlier. Live nodes are never reaped. With timeouts at 0, nobody is reaped.
  - Workload: Start 3 voters and 1 non-voter with reap timeouts set to 30s and 15s. SIGKILL one voter and the non-voter. Poll GET /nodes every second and record removal times. Repeat with leader clock jumps.
  - Guess: Guess: the silence duration is computed from wall-clock time, so clock jumps could reap a healthy node early or never.
- **Auto-restore runs at most once, on the node's first leadership, and never overwrites data written after it.** (restart, medium)
  - Quote: > It is one-shot: subsequent leader transitions do not trigger it again, and the restore file is removed from disk after the attempt — successful or not. If a different node becomes leader first, this node's restore is silently skipped.
  - Inject: After the restore completes and new writes are acknowledged, force leader changes and restart the node with the same auto-restore flags.
  - Observe: Rows written after the restore survive later leader transitions. No second restore wipes them. Any downloaded restore file in the data directory is gone after the attempt.
  - Workload: Start a single node or cluster with -auto-restore pointing to a config whose source holds a SQLite file with dataset R. Wait for leader and verify R. Insert extra rows. Kill the leader to force re-election, then restart all nodes. Verify R plus the extra rows.
  - Guess: Guess: the one-shot flag lives in memory, so after a process restart the restore might rerun unless something else guards it.
- **With bootstrap-expect, concurrently started nodes form exactly one cluster with one configuration.** (concurrent-calls, high)
  - Quote: > The Store accumulates the notifying set in `notifyingNodes` and, when the count reaches `BootstrapExpect`, triggers a single `BootstrapCluster` call with the full set. `notifyMu` and the `bootstrapped` flag ensure exactly one node performs the bootstrap.
  - Inject: Start all nodes simultaneously. Also kill and restart nodes mid-formation.
  - Observe: Every node's /nodes and /status report the same leader and the same member set of size N. There is never a split into two clusters with different leaders.
  - Workload: Launch 3 or 5 rqlited processes at the same instant with -bootstrap-expect=N and -join listing all. Randomly SIGKILL one during the first seconds and restart it. Poll /status on all nodes, then write via one node and strong-read via another.
  - Guess: Guess: per-node flags guarantee only that each node bootstraps once, not that only one node bootstraps. Different nodes reaching the count at the same time could both bootstrap.
- **A read-only (non-voter) node never bootstraps a cluster.** (restart, medium)
  - Quote: > Read-only nodes deliberately disable this path (they require `BootstrapExpect=0`) so they never accidentally bootstrap a cluster.
  - Inject: At startup of a non-voter configured with bootstrap-expect greater than 0.
  - Observe: rqlited refuses to start (non-zero exit or error) or never forms a cluster by itself. It never becomes a leader.
  - Workload: Start rqlited with -raft-non-voter=true -bootstrap-expect=3 -join=... and check the exit code and /status.
  - Guess: Guess: the check may be enforced only in some startup paths.
- **Rejoining with a new ID at the same address, or the same ID at a new address, leaves exactly one configuration entry for that node.** (restart, high)
  - Quote: > Before adding, the leader checks whether a server with the same ID *or* address already exists; if so, the existing entry is removed first. This handles the case where a node is restarted with a new ID at the same address, or a new node reuses an old ID.
  - Inject: Kill a follower, wipe nothing, restart it with a different -node-id at the same raft address (or the same ID on a different port), and have it join.
  - Observe: GET /nodes on the leader shows exactly one entry for that address and ID. Cluster size and quorum math are unchanged, and writes still commit.
  - Workload: Start a 3-node cluster. SIGKILL node3. Start a new process using a fresh data directory with -node-id=node3b at node3's address and -join pointing at the leader. Poll /nodes. Write and strong-read.
  - Guess: Guess: the remove-then-add is two configuration changes, so a leader crash between them could leave the cluster one voter short.
- **When the SQLite WAL grows past the configured size, a snapshot happens even if the log-entry threshold is not reached, including after restarts.** (restart, medium)
  - Quote: > **WAL-size snapshot ticker** (`runWALSnapshotting`) — periodic check (jittered) of the SQLite WAL file. If it exceeds `SnapshotThresholdWALSize`, the ticker triggers a snapshot.
  - Inject: Restart the node while the WAL is above or near the threshold.
  - Observe: A new snapshot appears in the snapshot directory shortly after the WAL file (db.sqlite-wal) exceeds the threshold. The WAL file then shrinks, and this keeps happening after restart.
  - Workload: Start with a high entry-count threshold and a low WAL-size threshold. Run a few large-row inserts. Watch db.sqlite-wal size and the snapshot directory. SIGKILL and restart mid-growth, then watch again.
  - Guess: Guess: the ticker might not start, or might measure the wrong file, after a restart through the fast path.
- **With CDC enabled, change events keep flowing after a database load, snapshot restore or restart.** (restart, medium)
  - Quote: > The Store registers the SQLite preupdate, commit, and rollback hooks when it opens the database, and re-registers them after every database swap (Restore, Load, ReadFrom), since SQLite hooks belong to a connection.
  - Inject: After POST /db/load, after a follower installs a snapshot, and after a process restart.
  - Observe: A Fuss-hosted CDC webhook keeps receiving events for writes made after the swap or restart. There is no gap where acknowledged writes produce no events.
  - Workload: Start rqlited with -cdc-config pointing to a Fuss HTTP endpoint. Insert rows and confirm events arrive. POST /db/load a new database, insert more rows, and confirm events. SIGKILL, restart, insert, and confirm events.
  - Guess: Guess: a swap path added later might forget to re-register hooks, silently stopping events.
- **Auto-vacuum runs during a snapshot once its interval has elapsed, without corrupting data or losing concurrent writes.** (concurrent-calls, low)
  - Quote: > **Run auto-vacuum and auto-optimize if their interval has elapsed.** This is deliberately scheduled inside the snapshot path because it is the one moment the database is guaranteed not to be receiving writes.
  - Inject: Send writes concurrently while a snapshot with vacuum is due. Also restart across the interval boundary.
  - Observe: The db.sqlite size drops after deletes plus a snapshot once the interval has passed. All acknowledged writes made during that time are present, and integrity_check passes.
  - Workload: Start with a short auto-vacuum interval flag and a low snapshot threshold. Insert many rows, DELETE most, keep writing, and wait for the snapshot. Compare db.sqlite size. Restart and repeat. Verify history.
  - Guess: Guess: the interval timestamp may live only in memory, so restarts could postpone vacuum indefinitely or run it on every snapshot.

