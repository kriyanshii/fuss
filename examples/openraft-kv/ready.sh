#!/bin/sh
# Wait until this node's API answers, and on node 0 form the 3-voter cluster
# once: POST /init [], /add-learner for ids 2 and 3, /change-membership [1,2,3].
# A later ready probe only checks /readiness. The store is in memory, so a
# restarted process does not keep the log; re-running /init would not put it back.
set -e
i=$1
dir=$2
client_port=$3
base="http://127.0.0.1:${client_port}"

curl -sf "$base/readiness" >/dev/null
if [ "$i" != "0" ] || [ -f "$dir/bootstrapped" ]; then
  printf 'READY\n'
  exit 0
fi

k=0
while [ "$k" -lt 3 ]; do
  tries=0
  while [ ! -s "$dir/api_$k" ] || [ ! -s "$dir/raft_$k" ]; do
    tries=$((tries + 1))
    if [ "$tries" -gt 100 ]; then
      echo "openraft-kv: port file $k missing" >&2
      exit 1
    fi
    sleep 0.05
  done
  k=$((k + 1))
done

post() {
  path=$1
  body=$2
  curl -sf -X POST -H 'Content-Type: application/json' -d "$body" "$base$path"
}

init=$(post /init '[]')
case $init in
  *'"Ok"'*) ;;
  *NotAllowed*) ;;
  *)
    printf '%s\n' "$init" >&2
    exit 1
    ;;
esac

n=1
while [ "$n" -lt 3 ]; do
  id=$((n + 1))
  api=$(cat "$dir/api_$n")
  raft=$(cat "$dir/raft_$n")
  tries=0
  while ! curl -sf "http://127.0.0.1:${api}/readiness" >/dev/null; do
    tries=$((tries + 1))
    if [ "$tries" -gt 100 ]; then
      echo "openraft-kv: node $id api not up" >&2
      exit 1
    fi
    sleep 0.05
  done
  added=$(post /add-learner "{\"node_id\":${id},\"api_addr\":\"127.0.0.1:${api}\",\"raft_addr\":\"127.0.0.1:${raft}\"}")
  case $added in
    *'"Ok"'*) ;;
    *)
      printf '%s\n' "$added" >&2
      exit 1
      ;;
  esac
  n=$((n + 1))
done

changed=$(post /change-membership '[1,2,3]')
case $changed in
  *'"Ok"'*) ;;
  *)
    printf '%s\n' "$changed" >&2
    exit 1
    ;;
esac
: > "$dir/bootstrapped"
printf 'READY\n'
