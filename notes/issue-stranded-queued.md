# `human-task complete` killed after marking the run Queued strands it, and the identical repeat reports success without queueing

`dagu human-task complete` marks the DAG-run Queued before it writes the queue
item. If the process dies between those two writes, the run says Queued but
there is nothing in the queue for the scheduler to run.

The way to recover is to run the same completion command again. For a run
that still needs resume, the spec says that repeat prints
`Human task <step> was already completed; DAG-run queued for resume.` and
queues it (`specs/031-human-task.md:424`). On a stranded run it instead prints
`Human task <step> was already completed.`, which is the spec's line for
"no resume is needed" (`:423`). It exits 0 and queues nothing, so the run
stays Queued.

## Expected vs actual

**Expected:** The identical repeat prints
`Human task h was already completed; DAG-run queued for resume.` and writes the
missing queue item, so the run resumes (`specs/031-human-task.md:424`).

**Actual:** The identical repeat prints `Human task h was already completed.`
and exits 0 without writing a queue item, so the run stays Queued and is not
resumed. In one run the stranded state was still there 83 minutes later: a
scheduler started then ran for 40s and did not resume it (there was no
scheduler running in between).

## Reproduce

Requires bash and `dagu` on PATH, or `TARGET=/path/to/dagu`. It kills `complete` the moment the run's
`status.jsonl` shows Queued (`5`, `ir.Queued`), then checks that no queue item
was written. If the kill lands too late it prints `MISSED` and exits 1, so it
never reports a stranded run it did not produce. Reading `status.jsonl` relies
on the run-store layout, which spec 031 leaves out of scope (`:443-444`); it is
used only to time the kill.

```bash
#!/bin/bash
set -u
DAGU=$(command -v "${TARGET:-dagu}") || { echo "no Dagu binary: set TARGET or put dagu on PATH"; exit 2; }
echo "dagu = $DAGU" >&2
export DAGU_HOME=$(mktemp -d) DAGU_AUTH_MODE=none
cd "$DAGU_HOME" || exit 2

cat > ht.yaml <<'EOF'
steps:
  - id: h
    action: human.task
    with:
      prompt: Continue?
  - id: after
    depends: [h]
    run: echo resumed
EOF

"$DAGU" start --run-id=r1 ht.yaml > start.log 2>&1
"$DAGU" status --run-id=r1 ht | grep '^Result:'             # Result: Waiting
files=( data/dag-runs/ht/dag-runs/*/*/*/dag-run_*_r1/*/status.jsonl )
f=${files[0]}
[ -f "$f" ] || { echo "no status.jsonl; layout differs"; exit 2; }

# Pure-bash poll: the first "status" on the last line is the run's own.
"$DAGU" human-task complete --run-id=r1 --step=h ht & pid=$!
hit=no
while kill -0 "$pid" 2>/dev/null; do
	last=; while IFS= read -r l; do last=$l; done < "$f"
	if [[ $last =~ \"status\":([0-9]+) ]] && [ "${BASH_REMATCH[1]}" = 5 ]; then
		kill -9 "$pid"; hit=yes; break
	fi
done
wait "$pid" 2>/dev/null
items=$(find data/queue -name '*_r1.json' 2>/dev/null | wc -l | tr -d ' ')
if [ "$hit" = no ] || [ "$items" != 0 ]; then
	echo "MISSED the window (killed=$hit, queue items=$items); run again. dir=$DAGU_HOME"
	exit 1
fi
echo "killed complete after Queued, before the queue item"

"$DAGU" status --run-id=r1 ht | grep -E '─h |^Result:'    # h [succeeded], Result: Queued
echo "queue items for r1: $items"                          # 0

"$DAGU" human-task complete --run-id=r1 --step=h ht; echo "repeat exit=$?"
# Spec line 424:  Human task h was already completed; DAG-run queued for resume.
# Observed:       Human task h was already completed.   repeat exit=0

"$DAGU" scheduler > scheduler.log 2>&1 & sp=$!
sleep 30; kill "$sp"; wait "$sp" 2>/dev/null
"$DAGU" status --run-id=r1 ht | grep '^Result:'             # still Result: Queued
echo "dir=$DAGU_HOME"
```

Observed output (every run identical apart from the temp dir):

```
Result: Waiting
killed complete after Queued, before the queue item
├─h (0s) [succeeded]
Result: Queued
queue items for r1: 0
Human task h was already completed.
repeat exit=0
Result: Queued
```

## How often

All on one macOS machine:

- This script: stranded the run in 10 of 10 runs, with no misses.
- An external fault-injection harness (not part of Dagu), killing `complete`
  within about 0.2ms of Queued appearing: stranded in 20 of 20 runs.
- The same harness, killing at seeded offsets of 0-30ms after Queued: every
  kill that landed before the queue item was written stranded the run,
  33 of 33. On this machine that was usually any kill up to 17-21ms after
  Queued appeared; in one slow trial a kill 46ms after Queued still stranded
  it. The bug is the order of the two writes; the window's length only
  changes how often a crash lands in it.

## Likely cause (from reading the code, not from debugging a live run)

- `queue.PrepareRetry` persists status Queued
  (`internal/queue/enqueue_retry.go:186-217`) before `queueStore.Enqueue`
  writes the item (`:146`). Between them are at least two fsyncs and a lock
  handoff, which is why the window is tens of milliseconds.
- If the process dies in between, the rollback at `:151` never runs.
- On the repeat, `PrepareRetry` returns early because the status is already
  Queued (`:173-174`), so `enqueueResume` reports `Queued: false` and the CLI
  prints the "already completed" line (`internal/cmd/human_task.go`,
  `runHumanTaskCompleteWith`).

## A note on the spec reading

This report reads a run that is Queued with no queue item as one that "still
needs resume" (row `:424`), since nothing will ever run it. The spec does not
define that phrase.

Lines `:510-517` ("Completion is durable before resume is requested ...
repeating the same completion retries resume") are written about synchronous
resume failures. Applying them to a killed process is this report's reading,
not the spec's literal text. The repeat is the only recovery the spec offers,
and here it cannot recover the run.

## Environment

- macOS 26.6.2 (25G83), APFS
- Dagu built from source at `9f5966a2c` (`dagu version` prints `0.0.0`)
- Default local file storage; `DAGU_AUTH_MODE=none`
