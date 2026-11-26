#!/bin/sh
# Run the before, pending, and after fault schedules.
# Usage:
#   ./examples/repkv/schedules.sh
#   ./examples/repkv/schedules.sh before
#   ./examples/repkv/schedules.sh pending /tmp/repkv-schedules
set -eu
root=$(CDPATH= cd -- "$(dirname "$0")/../.." && pwd)
cd "$root"
go build -o fuss .
go build -o examples/repkv/repkv ./examples/repkv

schedules="before pending after"
out=/tmp/repkv-schedules
if [ "${1:-}" = "before" ] || [ "${1:-}" = "pending" ] || [ "${1:-}" = "after" ]; then
  schedules=$1
  out=${2:-$out}
elif [ "${1:-}" != "" ]; then
  out=$1
fi

run_one() {
  sched=$1
  dest=$out/$sched
  rm -rf "$dest"
  mkdir -p "$dest"
  log=$(mktemp)
  echo "=== $sched ==="
  set +e
  REPKV_SCHEDULE=$sched REPKV_CLIENT_LOG=$log REPKV_HISTORY=$dest/history.jsonl \
    ./fuss -target-config=examples/repkv/repkv-schedules.yaml -seed=1 -n=1 -json \
    > "$dest/trial.jsonl"
  st=$?
  set -e
  rm -f "$log"
  python3 - "$dest/trial.jsonl" "$dest/cut.json" <<'PY'
import json, sys
trial, cut = sys.argv[1], sys.argv[2]
verdict = ""
for line in open(trial):
    line = line.strip()
    if not line:
        continue
    doc = json.loads(line)
    if doc.get("type") != "trial":
        continue
    verdict = doc.get("verdict", "")
    isolate = doc.get("evidence", {}).get("isolate")
    with open(cut, "w") as f:
        json.dump(isolate, f, indent=2)
        f.write("\n")
    check = doc.get("evidence", {}).get("check", {}).get("out", "")
    print("verdict:", verdict)
    for row in check.splitlines():
        if row.startswith("schedule ") or row.startswith("history ") or row.startswith("experiment ") or row.startswith("window ") or row.startswith("read "):
            print(row)
PY
  return "$st"
}

fail=0
for sched in $schedules; do
  if ! run_one "$sched"; then
    fail=1
  fi
done
exit "$fail"
