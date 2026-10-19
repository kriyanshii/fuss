#!/bin/sh
# kill_complete.sh DELAY [RUN_ID]
# One trial: get a run to Waiting, SIGKILL `dagu human-task complete` DELAY
# seconds after launching it, label where the kill landed from what is on
# disk, then repeat the same complete (no kill) and let a scheduler resume.
# The delay only spreads the kills; the label comes from observed state.
# Reads Dagu's state, never writes it. Settings only via documented DAGU_* vars.
set -u
D=$(command -v "${TARGET:-dagu}") || { echo "no Dagu binary: set TARGET or put dagu on PATH"; exit 2; }
echo "dagu = $D" >&2
DELAY=${1:?usage: kill_complete.sh DELAY [RUN_ID]}
RID=${2:-k$$}
W=/tmp/ht2/$RID
R=$W/record.txt
export DAGU_HOME=$W/home DAGU_AUTH_MODE=none

rm -rf "$W"; mkdir -p "$W/home"; : > "$R"
cat > "$W/ht.yaml" <<EOF
steps:
  - id: h
    action: human.task
    with:
      prompt: Continue?
  - id: after
    depends: [h]
    run: sh -c 'echo "start after \$DAG_RUN_ID" >> $R; sleep 1; echo "end after \$DAG_RUN_ID" >> $R'
EOF

# snap prints the run result, h's state, and the queue items for RID.
snap() {
	"$D" status --run-id="$RID" ht 2>/dev/null | grep -E '^Result:|─h '
	n=$(find "$W/home/data/queue" -name "item_*_$RID.json" 2>/dev/null | wc -l | tr -d ' ')
	echo "queue items: $n"
}

"$D" start --run-id="$RID" "$W/ht.yaml" > "$W/start.log" 2>&1
if ! "$D" status --run-id="$RID" ht 2>/dev/null | grep -q '^Result: Waiting'; then
	echo "SETUP: run did not reach Waiting; see $W/start.log"; exit 2
fi

echo "== kill complete after ${DELAY}s"
"$D" human-task complete --run-id="$RID" --step=h ht > "$W/c1.out" 2> "$W/c1.err" &
pid=$!
sleep "$DELAY"
kill -KILL "$pid" 2>/dev/null && killed=yes || killed=no
wait "$pid"; c1=$?
echo "killed=$killed exit=$c1"
echo "stdout: $(cat "$W/c1.out")"; echo "stderr: $(cat "$W/c1.err")"
S=$(snap); echo "$S"

# Label from state, in the order the code writes it:
# a) h succeeded  b) run Queued  c) queue item.
items=$(echo "$S" | sed -n 's/^queue items: //p')
if [ "$killed" = no ]; then label=finished-first
elif echo "$S" | grep -q '─h \[waiting\]'; then label=before-store
elif echo "$S" | grep -q '^Result: Waiting'; then label=store-before-queued   # a done, b not
elif [ "$items" = 0 ]; then label=queued-before-item                       # b done, c not
else label=after-item
fi
echo "LABEL=$label"

echo "== repeat complete (no kill)"
"$D" human-task complete --run-id="$RID" --step=h ht > "$W/c2.out" 2> "$W/c2.err"; c2=$?
echo "exit=$c2"; echo "stdout: $(cat "$W/c2.out")"; echo "stderr: $(cat "$W/c2.err")"
snap

echo "== scheduler for 8s"
"$D" scheduler > "$W/sched.log" 2>&1 &
sp=$!
sleep 8
kill "$sp"; wait "$sp" 2>/dev/null
snap
echo "record:"; cat "$R"
echo "start-after count: $(grep -c '^start after' "$R")"
echo "dir=$W"
