#!/bin/sh
# Write this node's Consul agent config, then replace the shell with consul.
# Fuss expands the arguments before this script runs. Serf ports are published
# as files because a node's template only sees its own ${port_serf}.
set -e
i=$1
dir=$2
data_dir=$3
binary=$4
port_server=$5
port_serf=$6
client_port=$7

printf '%s\n' "$port_serf" > "$dir/serf_$i"
k=0
while [ "$k" -lt 3 ]; do
  tries=0
  while [ ! -s "$dir/serf_$k" ]; do
    tries=$((tries + 1))
    if [ "$tries" -gt 100 ]; then
      echo "consul: serf port file $k missing" >&2
      exit 1
    fi
    sleep 0.05
  done
  k=$((k + 1))
done
s0=$(cat "$dir/serf_0")
s1=$(cat "$dir/serf_1")
s2=$(cat "$dir/serf_2")
mkdir -p "$data_dir/state"
cat > "$data_dir/agent.json" <<EOF
{
  "node_name": "node_$i",
  "server": true,
  "bootstrap_expect": 3,
  "bind_addr": "127.0.0.1",
  "client_addr": "127.0.0.1",
  "advertise_addr": "127.0.0.1",
  "datacenter": "dc1",
  "data_dir": "$data_dir/state",
  "disable_update_check": true,
  "log_level": "WARN",
  "retry_join": ["127.0.0.1:$s0", "127.0.0.1:$s1", "127.0.0.1:$s2"],
  "retry_interval": "1s",
  "rejoin_after_leave": true,
  "leave_on_terminate": false,
  "ports": {
    "server": $port_server,
    "serf_lan": $port_serf,
    "http": $client_port,
    "dns": -1,
    "https": -1,
    "serf_wan": -1,
    "grpc": -1,
    "grpc_tls": -1
  }
}
EOF
exec "$binary" agent -config-file="$data_dir/agent.json"
