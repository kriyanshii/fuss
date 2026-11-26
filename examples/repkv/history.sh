#!/bin/sh
# One after-ack partition of repkv, saved as a client history.
# The scenario is examples/repkv/repkv.yaml. Verdict lines are unchanged.
# Usage: ./examples/repkv/history.sh [history.jsonl]
set -eu
root=$(CDPATH= cd -- "$(dirname "$0")/../.." && pwd)
cd "$root"
go build -o fuss .
go build -o examples/repkv/repkv ./examples/repkv
out=${1:-/tmp/repkv-history.jsonl}
log=$(mktemp)
trap 'rm -f "$log"' EXIT
export REPKV_CLIENT_LOG=$log
export REPKV_HISTORY=$out
set +e
./fuss -target-config=examples/repkv/repkv.yaml -seed=1 -n=1 -json
st=$?
set -e
if [ -f "$out" ]; then
  echo "history: $out"
  ./examples/repkv/repkv -render-history "$out"
fi
exit "$st"
