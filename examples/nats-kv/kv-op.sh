#!/bin/sh
# One http-less KV operation for exec_op.
# Exit 0 and print the raw value on a successful get.
# Exit 0 with empty stdout when the key is absent (Porcupine absent read).
# A fast connection error sleeps until Fuss cancels the command, which
# applyExecOp records as info. Any other non-zero exit is fail.
op=$1
target=$2
key=$3
value=$4
nats_bin=${NATS_BIN:-nats}

case $target in
  nats://*|tls://*) server=$target ;;
  *) server="nats://${target}" ;;
esac

out=$(mktemp)
err=$(mktemp)
trap 'rm -f "$out" "$err"' EXIT

conn_err() {
  grep -E -i -q 'connection refused|no servers available|i/o timeout|timeout|connection reset|broken pipe|no route to host|server closed the connection' "$1"
}

hold_for_info() {
  cat "$err" >&2
  # Short sleeps so a killed shell does not leave one 30s sleep behind.
  # Fuss cancels this command at workload.timeout and records info.
  i=0
  while [ "$i" -lt 150 ]; do
    i=$((i + 1))
    sleep 0.2
  done
  exit 1
}

if [ "$op" = "put" ]; then
  if "$nats_bin" --server="$server" kv put fuss_kv "$key" "$value" >"$out" 2>"$err"; then
    exit 0
  fi
  if conn_err "$err"; then
    hold_for_info
  fi
  cat "$err" >&2
  exit 1
fi

if [ "$op" != "get" ]; then
  echo "nats-kv: unknown op $op" >&2
  exit 1
fi

if "$nats_bin" --server="$server" kv get fuss_kv "$key" --raw >"$out" 2>"$err"; then
  cat "$out"
  exit 0
fi
if grep -F -i -q 'key not found' "$err" || grep -F -i -q 'key not found' "$out"; then
  exit 0
fi
if conn_err "$err" || conn_err "$out"; then
  hold_for_info
fi
cat "$err" >&2
exit 1
