# Valkey 9.2.0, one replication link

One primary (`node_0`) and one replica (`node_1`). The replica is an exact copy
of the primary and dials the primary to stay that way
([Replication](https://valkey.io/topics/replication/)). This scenario writes
one key, waits until the replica returns it, cuts that replication
connection, and reads the key on the replica again.

Pin **9.2.0**. `valkey-server --version` must print `Valkey server v=9.2.0`.
The tag was published 2026-11-19; the release notes date it 18 November 2026,
commit `d23684e`
([Release 9.2.0](https://github.com/valkey-io/valkey/releases/tag/9.2.0)).
`valkey-cli` must be the matching binary, in the same directory as
`valkey-server`. Not run in CI. The prediction written before the first trial
is `notes/valkey-replica-link-prediction.md`.

```sh
go build -o fuss .
./fuss -target-config=examples/valkey/valkey.yaml \
  -target=/path/to/valkey-server -seed=1 -n=1 -json
```

Seeds 1 through 10 are `-seed=1 -n=10` with the same `-target`. Fuss prints
the binary it resolved to stderr (`fuss: valkey = …`). Check that path before
reading the verdict. The verdict is the JSON field `"verdict"`.

## Which connection is cut

The replica's `replicaof` is `${node_proxy_addr_0}`: the inbound proxy in
front of the primary. That proxy is the replication connection. The fault is
`proxy_mode: per_node` and `direction: inbound` on `node_0`.

The replica's client port is the address from `--port`, recorded in the trial
JSON as `topology` entry `i` 1, field `port`. The post-cut `GET` dials
`127.0.0.1` on that port. Fuss does not proxy it. Cutting the primary's
inbound proxy closes the connection the replica opened and refuses a
reconnect. It does not close the replica's listen socket, so a client can
still read the replica.

In a PASS line, `replica_before_cut=ok` is the replica `GET` of `"ok"` taken
while `INFO replication` reported `master_link_status:up`, before the cut.
`after_cut=yes` is the second `GET` of the same key on that client port. The
isolate object should show `target` `node_0`, `trigger_fired` true, and
`links_cut` 1. The link with `"to": 0` is the one that was cut
(`closed_by_cut` on that link). The link with `"to": 1` stays uncut; clients
were never on it.

`replica-serve-stale-data yes` is the documented default. The scenario sets
it on the command line so the post-cut `GET` is the reply Valkey documents
for a replica whose link is down: it still answers, possibly with old data.
This is not a promotion and not a primary restart.

## Troubleshooting

On this Mac the 9.2.0 tarball did not link against the default
CommandLineTools SDK. `ld` stopped with `unknown architecture arm64e.x1-macos`
while reading `MacOSX27.0.sdk` tbd files (`libSystem.B.tbd` and others).
`OPTIMIZATION=-O2` only drops `-flto`; the same `ld` error remained. The
binary used here was produced by pointing the build at an older SDK, without
patching Valkey:

```sh
export SDKROOT=/Library/Developer/CommandLineTools/SDKs/MacOSX26.sdk
make -j"$(sysctl -n hw.ncpu)" BUILD_TLS=no OPTIMIZATION=-O2
```

`sha=00000000` in `--version` means the tarball was not a git checkout. The
version token must still be `v=9.2.0`. If `valkey-cli` is not next to
`valkey-server`, the scenario exits before Fuss has a verdict.

A `(nil)` reply from `valkey-cli --no-raw` is a successful read of a missing
key. A non-zero `valkey-cli` is a failed query. Those are different result
lines: `value_missing=yes` versus `UNKNOWN reason=read_failed`.

## Recorded run

Clean checkout `64c4e32`, binary `/tmp/valkey-9.2.0/src/valkey-server`
(`Valkey server v=9.2.0`). Command:
`./fuss -target-config=examples/valkey/valkey.yaml -target=/tmp/valkey-9.2.0/src/valkey-server -seed=1 -n=10 -json`.
Exit 0. Summary: `"pass":10`, `"fail":0`, `"acks":10`, `"seeds":10`.

Every trial's `"verdict"` is `PASS` and `"label"` is `before-start`. None has
a `reason` field. Each `evidence.check.out` is
`PASS read_from=node_1 replica_before_cut=ok after_cut=yes cut_target=node_0 key=<seed>-0`,
with `acks` 1. Each isolate block has `proxy_mode` `per_node`, `direction`
`inbound`, `target` `node_0`, `trigger_fired` true, `links_cut` 1. The link
`"to":0` is `"cut":true`, with `stats_before` accepted 1 and open 1, and
`stats_after` accepted 2, `closed_by_cut` 1, `refused_while_cut` 1, open 0.
The link `"to":1` is `"cut":false`. `took_ms` is 6617.947, 6351.735,
6571.296, 6582.928, 6583.857, 6346.321, 6545.093, 6554.379, 6329.49,
6565.008 for seeds 1 through 10. No trial was UNKNOWN.
