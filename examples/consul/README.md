# Consul

Three-node Consul server cluster. Not run in CI. Install the release `consul` binary and leave the agent unmodified.

```sh
# HashiCorp archive for this machine. consul version prints the version.
# https://github.com/hashicorp/consul/releases
# Put the binary on PATH, or at examples/consul/consul.
go build -o fuss .
./fuss -target-config=examples/consul/consul-consistent.yaml -n=5 -seed=1 -json
./fuss -target-config=examples/consul/consul-stale.yaml -n=5 -seed=1 -save-failed-bundle /tmp/consul-runs -json
```

`-save-bundle` records one seed, so `-n` must be 1. `-save-failed-bundle <dir>` writes each non-PASS seed to `<dir>/seed-<seed>/`.

## What each node is given

The start command is `examples/consul/start-node.sh`. Before `exec`, it writes `${data_dir}/agent.json` and Consul's state directory `${data_dir}/state`. That file is the documented `-config-file`. The script does not edit the state directory after Consul is running.

| Setting | Value |
| --- | --- |
| `ports.server` | `${port_server}`. `peers.port_name: server` and `via_proxy: true`, so the peer proxy's upstream is this TCP port. |
| `ports.serf_lan` | `${port_serf}`, a different free port. Not proxied. Serf speaks TCP and UDP; the proxy is TCP only. |
| `ports.http` | `${client_port}`. Clients use `client_via_proxy` and `${client_addrs}`. |
| `ports.dns`, `https`, `serf_wan`, `grpc`, `grpc_tls` | `-1` (disabled). Servers would otherwise all try to bind gRPC TLS on 8503. |
| `retry_join` | `127.0.0.1` plus each node's Serf LAN port. `retry_interval` is `1s` (the default is 30s). |
| `bootstrap_expect` | 3 |

Consul's agent config uses `ports.server` for both `RPCBindAddr` and `RPCAdvertiseAddr`. There is no documented advertise port that differs from the bind port. Members therefore dial `127.0.0.1:${port_server}`, which is the upstream, not the proxy listen address. `isolate_node` and `partition_halves` act on the peer proxies. They do not cut a Raft dial that never enters the proxy. Serf is direct on purpose. `kill_node` still stops that process.

Ready is `GET /v1/status/leader` on the real HTTP port. The body is `""` until a leader exists, then a quoted `127.0.0.1:port`. The probe requires `127.0.0.1`.

## HTTP

| Call | Request |
| --- | --- |
| Write | `PUT /v1/kv/${key}` with the plain value as the body |
| Consistent read | `GET /v1/kv/${key}?consistent&raw` |
| Stale read | `GET /v1/kv/${key}?stale&raw` |

`?raw` is the value bytes, not a JSON document, so the get has no `value_from`. A missing key is HTTP 404. `http_kv` records that get as `ok` with an empty value.

`eventually_available` takes `n`, not `consecutive_ok`. `n: 5` is the quiet-window streak of `ok` operations.

## Limits

- The start script writes `agent.json` under the trial data directory. It does not write Raft or Serf state itself.
- Raft advertise and bind share `ports.server`, so peer-proxy faults do not sit on the Raft path. See the table above.
- No primary-source claim that a stale read must be nonlinearizable, or that a consistent read must survive this nemesis. Porcupine scores the recorded history. Quote Fuss's `-json` verdict.
