#!/bin/sh
# Ten seeds of one pending window on unmodified rqlite.
# The client asks for the cut after the write start is recorded and before
# it reads the execute response. The summary counts how many cuts landed
# inside that open write.
#
#   ./examples/rqlite/pending/pending.sh
#   ./examples/rqlite/pending/pending.sh /tmp/rqlite-pending
set -eu
root=$(CDPATH= cd -- "$(dirname "$0")/../../.." && pwd)
cd "$root"
go build -o fuss .
go build -o examples/repkv/repkv ./examples/repkv

out=${1:-/tmp/rqlite-pending}
rm -rf "$out"
mkdir -p "$out"
fail=0
for seed in 1 2 3 4 5 6 7 8 9 10; do
  dest=$out/seed-$seed
  mkdir -p "$dest"
  echo "=== seed $seed ==="
  set +e
  ./fuss -target-config=examples/rqlite/pending/fuss.yaml -seed="$seed" -n=1 -json \
    > "$dest/trial.jsonl"
  st=$?
  set -e
  python3 - "$dest" <<'PY'
import json, sys
from pathlib import Path
dest = Path(sys.argv[1])
verdict = ""
check = ""
isolate = None
for line in (dest / "trial.jsonl").read_text().splitlines():
    line = line.strip()
    if not line:
        continue
    doc = json.loads(line)
    if doc.get("type") != "trial":
        continue
    verdict = doc.get("verdict", "")
    isolate = doc.get("evidence", {}).get("isolate")
    check = doc.get("evidence", {}).get("check", {}).get("out", "")
print("verdict:", verdict)
for row in check.splitlines():
    if row.startswith(("schedule ", "history ", "experiment ", "window ", "read ")):
        print(row)
if isolate is not None:
    (dest / "cut.json").write_text(json.dumps(isolate, indent=2) + "\n")
marker = "translation effect=optional bound=open\n"
if marker in check:
    (dest / "history.jsonl").write_text(check.split(marker, 1)[1])
PY
  if [ "$st" -eq 2 ] || [ "$st" -eq 3 ]; then
    echo "fuss exit $st" >&2
    fail=1
  fi
done
python3 - "$out" <<'PY'
import json, sys
from pathlib import Path
out = Path(sys.argv[1])
windows = {}
verdicts = {}
n = 0
for trial in sorted(out.glob("seed-*/trial.jsonl")):
    verdict = ""
    window = ""
    for line in trial.read_text().splitlines():
        line = line.strip()
        if not line:
            continue
        doc = json.loads(line)
        if doc.get("type") != "trial":
            continue
        n += 1
        verdict = doc.get("verdict", "")
        check = doc.get("evidence", {}).get("check", {}).get("out", "")
        for row in check.splitlines():
            if row.startswith("window "):
                window = row.split(" ", 1)[1]
    verdicts[verdict] = verdicts.get(verdict, 0) + 1
    windows[window or "missing"] = windows.get(window or "missing", 0) + 1
print(f"seeds: {n}")
for name in ("pending", "before", "after", "none", "missing"):
    if windows.get(name):
        print(f"window {name}: {windows[name]}")
for name in sorted(verdicts):
    print(f"verdict {name}: {verdicts[name]}")
PY
exit "$fail"
