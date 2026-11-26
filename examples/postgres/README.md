# PostgreSQL 15.19, one replication link

One primary, one hot standby. The standby's WAL stream dials the proxy in
front of the primary. Fuss cuts that inbound proxy after a `remote_apply`
commit, then the standby reads the row back. This is a replication-link
interruption. It is not a promotion, a failover, or a power loss: the standby
is not promoted, and the primary is not killed.

Not run in CI. The prediction written before the first trial is
`notes/postgres-remote-apply-prediction.md`.

```sh
# postgres, initdb, psql, and pg_basebackup from the same PostgreSQL 15.19 install.
# `postgres --version` must print: postgres (PostgreSQL) 15.19
# Homebrew's postgresql@15 keg moves; if it is not 15.19, pass -target.
go build -o fuss .
./fuss -target-config=examples/postgres/postgres.yaml -seed=1 -n=1 -json
```

Fuss prints the binary it resolved to stderr (`fuss: postgres = …`). Check
that path before reading the verdict.

## Version

| Fact | Pin | Source |
| --- | --- | --- |
| Version | **15.19**, published 2026-08-13. Not a same-day release (today is 2026-11-25). The 15.19 notes describe fixes from 15.18. **15.20** was published 2026-11-12; this example was not run on it. | [Release 15.19](https://www.postgresql.org/docs/release/15.19/) · [15.19 announcement](https://www.postgresql.org/about/news/postgresql-186-1711-1615-1519-1424-and-19-beta-3-released-3365/) · [Release 15.20](https://www.postgresql.org/docs/release/15.20/) |
| Write ack | `synchronous_commit=remote_apply`. The commit waits until the synchronous standby has replayed the commit record, so the row is visible to queries there, and has written it to durable storage on the standby. | [synchronous_commit (15)](https://www.postgresql.org/docs/15/runtime-config-wal.html#GUC-SYNCHRONOUS-COMMIT) · [Synchronous Replication (15)](https://www.postgresql.org/docs/15/warm-standby.html#SYNCHRONOUS-REPLICATION) |
| Who dials | Streaming replication: the standby connects to the primary. `primary_conninfo` is that connection string. In this file it is `host` and `port` of `${node_proxy_addr_0}`. | [Streaming Replication (15)](https://www.postgresql.org/docs/15/warm-standby.html#STREAMING-REPLICATION) · [primary_conninfo (15)](https://www.postgresql.org/docs/15/runtime-config-replication.html#GUC-PRIMARY-CONNINFO) |
| Isolate shape | `proxy_mode: per_node`, `direction: inbound` on `node_0`. Client sessions dial `127.0.0.1:${port}` and do not cross the proxy. | This file. The etcd contrast is below. |

## psql

Every `psql` in the scenario is the same invocation. The acknowledgement is
the commit script. The polls add `-A -t` so the answer is one field.

```sh
PGOPTIONS='-c statement_timeout=15s' \
  psql -X -w -h 127.0.0.1 -p "$PRIMARY_PORT" -d postgres -v ON_ERROR_STOP=1 <<'SQL'
BEGIN;
CREATE TABLE fuss_kv (k text PRIMARY KEY, v text);
INSERT INTO fuss_kv (k, v) VALUES ('1-0', 'ok');
COMMIT;
SQL
```

| Flag | Why |
| --- | --- |
| `-X` | Do not read `~/.psqlrc`. A startup file must not turn `ON_ERROR_STOP` off. |
| `-w` | Never prompt for a password. A prompt would not be an acknowledgement. |
| `-h 127.0.0.1 -p` | The primary's real listen port. Not the proxy, and not a Unix socket. The scenario clears `PGHOST`, `PGPORT`, and the other `PG*` variables before this, so the environment cannot redirect it. |
| `-d postgres` | The database `initdb` creates. |
| `-v ON_ERROR_STOP=1` | Stop on the first SQL error and exit 3. |
| `statement_timeout=15s` | A wait that never finishes is an error, not an acknowledgement. |

`ON_ERROR_STOP` is what makes the exit status mean the transaction committed.
psql's default is to keep going after a SQL error, and a script can still
exit 0. In this four-statement script a failed `INSERT` aborts the
transaction, `COMMIT` rolls it back, and that `COMMIT` itself succeeds. Without
`ON_ERROR_STOP` that is exit 0 for a transaction that did not commit. With it,
psql exits 3 and the driver writes `UNKNOWN reason=write_failed` and does not
append `ack`. Exit 0 is the only acknowledgement.

Polls use the same `-v ON_ERROR_STOP=1` and then `|| true`, because a failed
poll is not an acknowledgement. The commit script does not.

Source: [psql (15)](https://www.postgresql.org/docs/15/app-psql.html), exit
status and `ON_ERROR_STOP`. Exit 0 is normal, 2 is a lost connection in a
script, 3 is a SQL error when `ON_ERROR_STOP` is set.

## Scenario

1. Node 0 runs `initdb` and starts as the primary (`wal_level=replica`, `synchronous_commit=remote_apply`). Node 1 takes a base backup from the primary's real port, writes `standby.signal`, and `exec`s postgres with `primary_conninfo` pointed at the primary's proxy and `application_name=standby1`. The base backup is one shot. The long-lived WAL stream is the standby process, and that stream dials the proxy.
2. Wait until `pg_stat_replication` shows `standby1` in `streaming`, then set `synchronous_standby_names=standby1` and reload. Wait until that row is `streaming sync`, `synchronous_commit` is `remote_apply`, and `synchronous_standby_names` is `standby1`.
3. Run the commit script above. Exit 0 appends `ack <seed>-0` to `${dir}/runs.txt`. Anything else is `UNKNOWN reason=write_failed`.
4. Read `pg_stat_replication` once more. The result line's `standby_before_cut=sync` is this read, taken before `cut_now` is written. It is not a measurement after the cut. If the row is no longer `streaming sync`, the verdict is `UNKNOWN reason=standby_lost` and there is no cut.
5. Write `node_0` to `${dir}/leader_node` and `NOW` to `${dir}/cut_now`. Wait for `${dir}/isolate_applied.json` with `target` `node_0`, `trigger_fired` true, and `links_cut` greater than 0. A different target is `UNKNOWN reason=cut_missed_leader`. A missing file is `UNKNOWN reason=cut_not_applied`.
6. `SELECT v FROM fuss_kv WHERE k = '<key>'` on the standby, with `statement_timeout=10s` and `ON_ERROR_STOP=1`.

Value `ok` → `PASS read_from=node_1 standby_before_cut=sync after_cut=yes cut_target=node_0 key=...`. No row → `LOST WRITE` with `value_missing=yes` and the same other fields. A non-zero `psql` → `UNKNOWN reason=read_failed`. Any other value → `UNKNOWN reason=read_unexpected`. A failed read is not a missing value.

## Why inbound is enough

The connection this test has to interrupt is the one the standby opens. The
primary does not dial the standby for this WAL stream. The proxy in front of
the primary accepts that connection. Cutting it closes the stream and refuses
the reconnect. Client `psql` sessions are on the real port, so the cut does
not drop them.

etcd is the other shape. Each member dials the others, and the fault has to
stop traffic both ways for the leader while the remaining pair stays up. One
shared inbound proxy cannot tell those dials apart, so that scenario uses
`per_node_owned` and `direction: both`. The PostgreSQL result is the case
where the inbound cut is the whole fault, because there is only one
replication connection and the standby is the side that opens it.

| | PostgreSQL | etcd |
| --- | --- | --- |
| What has to stop | The standby's WAL stream to the primary | Peer traffic both ways for the leader, while the other pair stays up |
| Who dials | The standby, via `primary_conninfo`, at `${node_proxy_addr_0}` | Each member, at the others' advertised peer URLs (the inbound proxies) |
| Mode | `per_node`, `direction: inbound` on `node_0` | `per_node_owned`, `direction: both` on the leader |
| Why | Cutting the primary's inbound proxy closes the standby's connection and refuses the reconnect | A shared inbound proxy accepts every peer. Inbound-only leaves the leader's own dials up. A source tag is what lets `both` cut the leader and leave the other pair up |

The same table is in `skills/fuss/AUTHORING.md`.

`per_node_owned` does not fit this stream. Fuss attributes a socket to a
source only when the dialing process is a PID it started. **Reading** of
PostgreSQL: the WAL receiver is a child of the postmaster, and `pg_stat_activity.backend_type`
names it `walreceiver` ([Monitoring (15)](https://www.postgresql.org/docs/15/monitoring-stats.html)).
The scenario `exec`s the postmaster. The child is an unknown owner.

The same scenario with `proxy_mode: per_node_owned` and `direction: both`,
seed 1, same 15.19 binary, exit 1. Fuss `-json` `"verdict"` was `"UNKNOWN"`
and top-level `"reason"` was `"proxy_owner_unknown"`. `evidence.check.out`
was the PASS line below, so the query had succeeded. `unknown_owners` still
listed the sockets, both `cut` true, `to` 0, `listen` `127.0.0.1:53757`,
`remote` `127.0.0.1:53820` and `127.0.0.1:53832`. The verdict stays `UNKNOWN`
because Fuss could not verify the fault. The trial notes' earlier owned run
had those sockets and no `reason` field.

## What a PASS is allowed to mean

With `remote_apply` set, and `node_1` observed as the synchronous standby
before the cut, the standby returned the acknowledged value after Fuss closed
the WAL receiver's connection and refused a reconnect. `standby_before_cut=sync`
is that observation. `after_cut=yes` means the read ran after
`isolate_applied.json` named `node_0`.

One seed shows the scenario is expressible. It does not show promotion,
failover, or power loss.

Seed 1 of this file, PostgreSQL 15.19 at
`/usr/local/opt/postgresql@15/bin/postgres`, exit 0. `"verdict"` was `"PASS"`.
`evidence.check.out` was:

```
PASS read_from=node_1 standby_before_cut=sync after_cut=yes cut_target=node_0 key=1-0
```

`evidence.isolate` was `proxy_mode` `per_node`, `direction` `inbound`,
`target` `node_0`, `trigger_fired` true, `links_cut` 1. The cut link (`to` 0)
went from `accepted` 1 and `open` 1 to `accepted` 2, `closed_by_cut` 1,
`refused_while_cut` 1, and `open` 0. `took_ms` was 2350.743. The summary line
was `"pass":1`, `"fail":0`.
