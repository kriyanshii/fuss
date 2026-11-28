#!/bin/sh
# Write this node's nats.conf, then replace the shell with nats-server.
# Fuss expands the arguments. The server process must be the one Fuss
# started, so peer sockets have a topology owner.
set -e
i=$1
data_dir=$2
binary=$3
port_cluster=$4
port_monitor=$5
client_port=$6
advertise=$7
client_advertise=$8
shift 8
p0=$1
p1=$2
p2=$3

mkdir -p "$data_dir"
routes=""
for p in "$p0" "$p1" "$p2"; do
  if [ -n "$p" ]; then
    routes="${routes}    nats://${p}
"
  fi
done

cat > "$data_dir/nats.conf" <<EOF
server_name: node${i}
listen: 127.0.0.1:${client_port}
client_advertise: "${client_advertise}"
http: "127.0.0.1:${port_monitor}"
jetstream {
  store_dir: "${data_dir}/jetstream"
}
cluster {
  name: "fuss-nats"
  listen: "127.0.0.1:${port_cluster}"
  advertise: "${advertise}"
  routes: [
${routes}  ]
}
EOF
exec "$binary" -c "$data_dir/nats.conf"
