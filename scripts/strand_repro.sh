#!/bin/bash
# Reproducer without Fuss: kill `dagu human-task complete` after it marks the
# run Queued but before it writes the queue item. The run stays Queued with no
# queue item, the identical repeat says nothing is needed, and it never resumes.
#
# Usage: TARGET=/path/to/dagu ./strand_repro.sh   (default: dagu on PATH)
# Times the kill by reading the run's status.jsonl (Queued = 5, ir.Queued);
# that file layout is outside spec 031, so this is a test aid, not a contract.
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

# Busy-poll in pure bash (no fork per check): the first "status" on the
# last line is the run's own; nodes come after it.
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
# Spec line 424 says:  Human task h was already completed; DAG-run queued for resume.
# Observed:            Human task h was already completed.   repeat exit=0

"$DAGU" scheduler > scheduler.log 2>&1 & sp=$!
sleep 30; kill "$sp"; wait "$sp" 2>/dev/null
"$DAGU" status --run-id=r1 ht | grep '^Result:'             # still Result: Queued
echo "dir=$DAGU_HOME"
