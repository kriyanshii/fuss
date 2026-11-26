#!/bin/sh
# One in-flight write: replicate, hold the response, cut the leader, time out.
# Usage: ./examples/repkv/inflight.sh [history.jsonl]
set -eu
root=$(CDPATH= cd -- "$(dirname "$0")/../.." && pwd)
cd "$root"
go build -o fuss .
go build -o examples/repkv/repkv ./examples/repkv
out=${1:-/tmp/repkv-inflight-history.jsonl}
log=$(mktemp)
trap 'rm -f "$log"' EXIT
export REPKV_CLIENT_LOG=$log
export REPKV_HISTORY=$out
set +e
./fuss -target-config=examples/repkv/repkv-inflight.yaml -seed=1 -n=1 -json
st=$?
set -e
if [ -f "$out" ]; then
  echo "history: $out"
  ./examples/repkv/repkv -render-history "$out"
fi
exit "$st"
