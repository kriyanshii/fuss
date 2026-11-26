#!/bin/sh
# Launch, cut, and package only. The client is the same workload.py.
# Toxiproxy sits in front of each Raft listener. Disabling node 0's proxy
# stops inbound dials. It cannot drop only node 0's outbound dials: nodes 1
# and 2 share those proxy listeners, and a localhost dial has no source identity.
set -eu
root=$(CDPATH= cd -- "$(dirname "$0")/../../.." && pwd)
cd "$root"
out=${1:-/tmp/rqlite-toxiproxy-run}
rm -rf "$out"
mkdir -p "$out/node0" "$out/node1" "$out/node2"
api=127.0.0.1:18474
base=19210
http0=$((base + 0))
http1=$((base + 1))
http2=$((base + 2))
raft0=$((base + 10))
raft1=$((base + 11))
raft2=$((base + 12))
proxy0=$((base + 20))
proxy1=$((base + 21))
proxy2=$((base + 22))
printf '%s\n' "127.0.0.1:$http0" > "$out/http_0"
printf '%s\n' "127.0.0.1:$http1" > "$out/http_1"
printf '%s\n' "127.0.0.1:$http2" > "$out/http_2"
toxiproxy-server -host 127.0.0.1 -port 18474 > "$out/toxiproxy.log" 2>&1 &
tox_pid=$!
cleanup() {
  kill "$tox_pid" 2>/dev/null || true
  wait "$tox_pid" 2>/dev/null || true
  for pid in $(cat "$out/pids" 2>/dev/null); do
    kill "$pid" 2>/dev/null || true
  done
}
trap cleanup EXIT
for _ in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20; do
  if toxiproxy-cli --host "$api" list >/dev/null 2>&1; then
    break
  fi
  sleep 0.1
done
toxiproxy-cli --host "$api" create --listen "127.0.0.1:$proxy0" --upstream "127.0.0.1:$raft0" node0
toxiproxy-cli --host "$api" create --listen "127.0.0.1:$proxy1" --upstream "127.0.0.1:$raft1" node1
toxiproxy-cli --host "$api" create --listen "127.0.0.1:$proxy2" --upstream "127.0.0.1:$raft2" node2
: > "$out/pids"
start_node() {
  i=$1
  http=$2
  raft=$3
  adv=$4
  shift 4
  /usr/local/bin/rqlited -node-id "node_${i}" \
    -http-addr "127.0.0.1:$http" \
    -raft-addr "127.0.0.1:$raft" \
    -raft-adv-addr "127.0.0.1:$adv" \
    "$@" \
    "$out/node${i}/rqlite" > "$out/node${i}.log" 2>&1 &
  echo $! >> "$out/pids"
}
start_node 0 "$http0" "$raft0" "$proxy0"
start_node 1 "$http1" "$raft1" "$proxy1" -join "127.0.0.1:$proxy0" -join-attempts 30 -join-interval 500ms
start_node 2 "$http2" "$raft2" "$proxy2" -join "127.0.0.1:$proxy0" -join-attempts 30 -join-interval 500ms
python3 examples/rqlite/headtohead/workload.py "$out" "1-0" &
work=$!
toggled=0
for _ in $(seq 1 80); do
  if [ -s "$out/cut_now" ]; then
    toxiproxy-cli --host "$api" toggle node0 > "$out/proxy-toggle.txt" 2>&1
    toxiproxy-cli --host "$api" inspect node0 > "$out/proxy-inspect.txt" 2>&1 || true
    toggled=1
    break
  fi
  sleep 0.25
done
python3 - "$out" "$proxy0" "$toggled" <<'PY'
import json, socket, sys
from datetime import datetime, timezone
out, port, toggled = sys.argv[1], int(sys.argv[2]), sys.argv[3] == "1"
refused = False
err = ""
if toggled:
    try:
        s = socket.create_connection(("127.0.0.1", port), 1)
        s.close()
    except OSError as exc:
        refused = True
        err = str(exc)
verified = toggled and refused
t = datetime.now(timezone.utc)
at = t.strftime("%Y-%m-%dT%H:%M:%S.") + f"{t.microsecond:06d}Z"
doc = {
    "target": "node_0",
    "direction": "inbound",
    "links_cut": 1 if verified else 0,
    "trigger_fired": verified,
    "at": at,
    "proxy_disabled": verified,
    "listen_refused": refused,
    "dial_error": err,
}
name = "cut_applied.json" if verified else "cut_unverified.json"
with open(out + "/" + name, "w") as f:
    json.dump(doc, f)
    f.write("\n")
PY
wait "$work" || true
./examples/repkv/repkv -check-history "$out/history.jsonl" -porcupine -initial "" > "$out/checker.txt" || true
printf '%s\n' "package: $out"
echo "--- checker ---"
cat "$out/checker.txt" 2>/dev/null || true
echo "--- cut ---"
cat "$out/cut_applied.json" 2>/dev/null || cat "$out/cut_unverified.json" 2>/dev/null || true
echo
echo "--- inspect ---"
cat "$out/proxy-inspect.txt" 2>/dev/null || true
