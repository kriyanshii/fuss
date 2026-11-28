#!/bin/sh
# JetStream is ready when /jsz answers and `nats server check jetstream`
# succeeds. Node 0 creates the R=3 bucket; every node waits until kv info
# sees it. Fuss retries this command until timeout.
set -e
i=$1
client_port=$2
port_monitor=$3
# nats kv add 0.5.2 has no allow-direct flag. "no-direct" sets
# allow_direct false on KV_fuss_kv through the stream update API.
# An empty 4th arg leaves the server default (AllowDirect on).
mode=${4-}
nats_bin=${NATS_BIN:-nats}
server="nats://127.0.0.1:${client_port}"

curl -sf "http://127.0.0.1:${port_monitor}/jsz" >/dev/null
"$nats_bin" server check jetstream --server="$server" >/dev/null
if [ "$i" = "0" ]; then
  "$nats_bin" --server="$server" kv add fuss_kv --replicas=3 >/dev/null 2>&1 \
    || "$nats_bin" --server="$server" kv info fuss_kv >/dev/null
  if [ "$mode" = "no-direct" ]; then
    # stream edit --no-allow-direct --force still asks
    # "Really operate on the KV stream?" and exits without a TTY.
    # The update API takes the same stream config field.
    info=$("$nats_bin" --server="$server" stream info KV_fuss_kv --json)
    cfg=$(printf '%s\n' "$info" | python3 -c 'import json,sys; c=json.load(sys.stdin)["config"]; c["allow_direct"]=False; print(json.dumps(c))')
    resp=$("$nats_bin" --server="$server" request --raw '$JS.API.STREAM.UPDATE.KV_fuss_kv' "$cfg")
    printf '%s\n' "$resp" | python3 -c 'import json,sys; d=json.load(sys.stdin); err=d.get("error");
assert not err, err
assert d["config"]["allow_direct"] is False'
  fi
else
  "$nats_bin" --server="$server" kv info fuss_kv >/dev/null
fi
printf 'READY\n'
