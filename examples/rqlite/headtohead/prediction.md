# Prediction: same rqlite history, two harnesses

The shared workload inserts `1-0=ok`, waits for the cut, and strong-reads that row from a surviving leader. Porcupine, with a timed-out write's effect optional and its completion bound left open, will call a completed insert followed by a read of `ok` legal.

On the Fuss side the cut is bidirectional. Fuss verifies it. The experiment verdict is `PASS`, `evidence.check.out` begins `history legal`, and `evidence.isolate` shows the cut of `node_0`.

This is wrong if that history is illegal, or if the verdict is `PASS` without a verified cut.

## Observed result

Binary: `/usr/local/bin/rqlited`, `rqlited v10.7.5`. Checker: `./examples/repkv/repkv -check-history FILE -porcupine -initial ""`, translation `effect=optional bound=open`. Workload: `examples/rqlite/headtohead/workload.py`.

Fuss command: `./fuss -target-config=examples/rqlite/headtohead/fuss.yaml -seed=1 -n=1 -json`

Fuss printed `"verdict":"PASS"`. `evidence.check.out` begins `history legal`. The history is a completed write of `1-0=ok` (`2026-11-26T10:01:58.339195Z` to `2026-11-26T10:01:58.356269Z`), then the cut, then `r1` from `node_1` returning `ok` (`2026-11-26T10:01:59.914760Z` to `2026-11-26T10:01:59.929176Z`). `evidence.isolate` is `target` `node_0`, `direction` `both`, `links_cut` 4, `links_total` 6, `trigger_fired` true, `proxy_mode` `per_node_owned`, `cut_time` `2026-11-26T10:01:58.366967Z`, `unknown_owners` empty.

Toxiproxy command: `sh examples/rqlite/headtohead/toxiproxy.sh /tmp/rqlite-toxiproxy-run`

The same checker printed `history legal`. The history is only the completed write of `1-0=ok` (`2026-11-26T10:03:13.245460Z` to `2026-11-26T10:03:13.264726Z`) and a cut line. There is no read. `cut_applied.json` says `direction` `inbound`, `links_cut` 1, `listen_refused` true, dial error `connection refused`. Node 0's log says it became leader at `15:33:12` and was still leader at shutdown (`stepping down as Leader before shutdown` at `15:33:25`). No new leader was recorded.
