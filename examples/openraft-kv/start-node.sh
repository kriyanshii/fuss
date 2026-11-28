#!/bin/sh
# Replace the shell with raft-key-value. --api-addr and --raft-addr are each
# both the bind address and the address published to the cluster, so this
# script passes the real listen ports, not the Fuss proxy addresses.
# Port files let node 0's ready script name the other nodes' bind addresses.
set -e
i=$1
dir=$2
binary=$3
client_port=$4
port_raft=$5
id=$((i + 1))

mkdir -p "$dir"
printf '%s\n' "$client_port" > "$dir/api_$i"
printf '%s\n' "$port_raft" > "$dir/raft_$i"
exec "$binary" --id "$id" --api-addr "127.0.0.1:${client_port}" --raft-addr "127.0.0.1:${port_raft}"
