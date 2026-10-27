# fuss

Seeded crash testing for unmodified binaries, as a skill for Claude Code,
Cursor, Codex and Gemini CLI. Every reported bug replays from its seed, 10 of
10.

Fuss uses a **target adapter architecture** to test different programs. Currently
supported targets:

- **Dagu**: DAG workflow scheduler (original target) - tests zombie detection, retry, human tasks
- **rqlite**: planned: example adapter exists, its check is not yet backed by a documented promise
- **fake**: Test adapter for development and unit tests

To add a new target, implement the `Adapter` interface in `adapter.go` (see
`dagu_adapter.go` and `rqlite_adapter.go` for examples).

## Why the AI never judges

An agent running the skill can suggest promises to test, draft code and draft
reports. It never decides whether the target passed or failed. Every verdict
comes from Fuss's own check, printed in its `-json` output:

- **A verdict has to replay.** A seed replays the same fault, and the check
  reads the same evidence the same way each time. A model's answer can change
  between two runs on the same input, so it cannot be replayed or audited.
- **A check is proven before it is trusted.** Each check must first catch a
  known broken state (a control). If a control fails, Fuss runs no seeds and
  exits 3. There is no such test for a model's judgement.
- **Models misread logs with confidence.** A wrong count or an overstated
  claim reads just as convincingly as a right one. The check counts the log
  the same way every time.

People pick which promise to test and approve each check. The model helps
with the work around them; the code decides.

## Running

```bash
# Build
go build -o fuss .

# Test the fake adapter (no external binary needed)
./fuss -adapter=fake -n=5

# Test Dagu (requires Dagu binary)
./fuss -adapter=dagu -dag=one -target=/path/to/dagu -n=10

# Test rqlite (requires rqlited binary)
./fuss -adapter=rqlite -target=/path/to/rqlited -n=10

# Run tests
go test ./...                          # Unit tests (no external binaries needed)
go test -run TestDaguIntegration       # Dagu integration tests (needs dagu)
go test -run TestRqliteIntegration     # rqlite integration tests (needs rqlited)

# Old Dagu mode (backward compatible, no -adapter flag)
./fuss -target=/path/to/dagu -dag=one -n=2
```

Add `-json` for one JSON line per trial, control and summary (seed, mode,
where the fault landed, verdict, evidence, trial dir) instead of the human
output. Without it, the output is unchanged.

To run the loop through an agent, see `skills/fuss/SKILL.md` and
`skills/fuss/INSTALL.md` (Claude Code: `/fuss <target and what to test>`).

Without `-target`, Fuss uses the default binary name from `PATH` (e.g., `dagu`
for the Dagu adapter, `rqlited` for rqlite), and stops with an error if not
found. Either way it prints the binary it resolved to stderr at startup
(`fuss: <target> = … (from …)`), so a run never silently uses the wrong one.

## Adding a New Target

To add a new target adapter:

1. **Create the adapter file** (e.g., `mytarget_adapter.go`)
2. **Implement the `Adapter` interface** from `adapter.go`:
   - `Name()` - target name
   - `ResolveBinary()` - find the target binary
   - `Init()` - set up a trial (create dirs, config files)
   - `Start()` - launch the target process
   - `Status()` - query target state
   - `Settle()` - wait for target to stop reporting "running" after crash
   - `Check()` - validate against target's promises
   - `RunControl()` - run control checks
   - `Controls()` - list available controls
3. **Add to main.go** in the `runWithAdapter` switch statement
4. **Write unit tests** using the fake adapter pattern
5. **Write integration tests** that require the real binary
6. **Update CI** in `.github/workflows/ci.yml` to install and test with the binary

See `dagu_adapter.go` (complex, with multiple modes) and `rqlite_adapter.go`
(simple, single-node durability) as examples.

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
