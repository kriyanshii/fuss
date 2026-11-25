#!/bin/sh
# Replay a saved rqlite bundle. Same format as examples/repkv/bundles/lost-write.
# Usage: ./examples/rqlite/replay.sh path/to/bundle
set -eu
if [ $# -ne 1 ]; then
  echo "usage: $0 <bundle-dir>" >&2
  exit 2
fi
root=$(CDPATH= cd -- "$(dirname "$0")/../.." && pwd)
cd "$root"
go build -o fuss .
exec ./fuss -replay-bundle "$1"
