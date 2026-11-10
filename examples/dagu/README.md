# Dagu (optional)

This case study is optional. The main quickstart uses the fake adapter and
does not need Dagu.

Dagu human-task controls still live in the core and are moving behind the
adapter.

Finding: [dagu#3246](https://github.com/dagucloud/dagu/issues/3246).

Dagu was the first target. `notes/` holds a full run of the skill loop on
Dagu's human-task completion: the blind predictions (`piece5-prediction.md`,
`repro-prediction.md`) and the issue draft (`issue-stranded-queued.md`).
`RULEBOOK.md` sections 7 and 8 walk through it, including where it fell
short of the rules.

## Install

- [Dagu 2.21.0](https://github.com/dagucloud/dagu/releases/tag/v2.21.0)

## Reproduce

From the repo root, after `go build -o fuss .`:

```bash
./fuss -target=<path to dagu> -dag=human -kill-at=queued -seed=1 -n=10 -json
```

Expected result: STRANDED 10 of 10.

Older shape (no `-adapter` flag):

```bash
./fuss -target=/path/to/dagu -dag=one -n=2
```

Integration tests that need a Dagu binary:

```bash
go test -run TestDaguIntegration
```

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
