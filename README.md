# fuss

Fuss is a seeded, replayable fault-injection harness meant to test any Go
program from the outside, without modifying it. The same seed replays the same
attack.

Dagu is the first target. Today the Dagu-specific parts (DAG modes, status
parsing, verdicts) are mixed into the core; separating them into a Dagu adapter
is planned.

## Running

```
go build -o fuss .
./fuss -target=/path/to/dagu -dag=human -kill-at=queued -seed=1 -n=2 -replay=10
go test ./... -args -target=/path/to/dagu
```

Without `-target`, Fuss uses `dagu` from `PATH`, and stops with an error if
neither exists. Either way it prints the binary it resolved to stderr at
startup (`fuss: dagu = … (from …)`), so a run never silently uses the wrong
one. The scripts resolve the same way from a `TARGET` variable:
`TARGET=/path/to/dagu scripts/strand_repro.sh` (the standalone reproducer
from the issue) and `scripts/kill_complete.sh`.

`notes/` holds predictions written down before each run, kept next to what the
run actually showed, and the issue draft as it was before filing (the filed
issue is the current version).

## Limits

### Reading Dagu's files

Spec 031 puts "the persistence format and run-store file layout" outside the
spec (`specs/031-human-task.md:443-444`). Fuss reads those files anyway in
these places. It only reads them; it never writes into `DAGU_HOME`.

| What | Read for | File |
|---|---|---|
| `status.jsonl` (run status) | timing the kill in `-dag=human -kill-at=queued`; labels in `-kill-at=end`, `-kill-at=stratified` and `-measure` | `measure.go` |
| `data/queue/*/item_*_<run-id>.json` | the human-task labels, **and** the STRANDED verdict ("no queue item") | `human.go` |

For `-kill-at=queued`, the status file only decides *when* to kill. Labels and
verdicts never use it. It also costs a read every millisecond while
`complete` runs, so Fuss's polling may shift Dagu's timing.

The STRANDED verdict does rely on the queue directory layout. That layout was
seen by hand, not read from a spec. Before any seed runs, the queue control
checks that an unkilled `complete` leaves exactly one item that Fuss can see.
If Fuss counts zero there, nothing runs.

Every other verdict comes from the outside record (`start after <run-id>`
lines) and `dagu status`.

If Dagu changes either layout, `-kill-at=queued` stops with an error (no
status file for a Waiting run), or the queue control fails. A changed status
encoding could instead mean Queued is never seen; those trials print
`queued-seen=never`.

### Undocumented env vars in the older modes (an exception)

Fuss's rule is to change Dagu's settings only through documented `DAGU_*`
env vars. The older modes break it: `-dag=one`, `-dag=two`, the zombie
control (which runs first by default) and `-measure` set
`DAGU_PROC_HEARTBEAT_INTERVAL=1s` and `DAGU_PROC_STALE_THRESHOLD=5s`
(`daguEnv`, `dagu.go`). The zombie control's staged reader uses a 1h stale
threshold instead (`zombieReader`, `control.go`).

Why: Dagu marks a dead run Failed only after its heartbeat is older than the
stale threshold, 90s by default. Lowering it makes rule 3 testable in seconds.
The settings themselves are documented as config-file keys
(`proc.heartbeat_interval`, `proc.stale_threshold` in Dagu's config schema),
but their env var form appears only in Dagu's code, not its README. This is
left as a documented exception rather than changed, because changing those
modes risks byte-for-byte replay of old `-dag=one` seeds.

The human-task finding does not use them. `-dag=human` runs every Dagu
command with only `DAGU_HOME` and `DAGU_AUTH_MODE=none` added to the inherited
environment, and it exits before the zombie control. The issue's shell
reproducer sets the same two. Both inherit the shell's environment, so a
`DAGU_PROC_*` already exported there would pass through. Check with
`env | grep DAGU_` before a run.
