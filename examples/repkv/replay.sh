#!/bin/sh
# Recreate the saved three-node lost-write run from a clean checkout.
# Prints the write acknowledgement, the applied partition, and the read.
set -eu
root=$(CDPATH= cd -- "$(dirname "$0")/../.." && pwd)
cd "$root"
go build -o fuss .
go build -o examples/repkv/repkv ./examples/repkv
exec ./fuss -replay-bundle examples/repkv/bundles/lost-write
