# YAML target config

Field reference for a Fuss target file. It is taken from `yaml_target.go` and
`yaml_target_test.go`. `LoadYAMLTarget` decodes the file into those structs.
A key that is not listed here is ignored. Durations use Go's
`time.ParseDuration` (`5s`, `100ms`). An empty or unparseable duration uses
the default named on that field.

Two accepted fields are never read. They are listed so they are not given a
purpose they do not have: `workload.until` and `kill.signal`.

## Template variables

`expandTemplates` replaces these thirteen names, once, in this order: `run_id`,
`binary`, `home`, `addr`, `seed`, `dir`, `key`, `n`, `i`, `port`,
`data_dir`, `peers`, `peers_via_proxy`. Then any `${peer_addr_<j>}` or
`${node_proxy_addr_<k>}` present in `vars`. Text inserted for an earlier
name is still scanned for a later name. Any other `${...}` is left as written.

| Variable | Value in `vars` | When it is set |
|---|---|---|
| `${home}` | `t.Home()` | Every expansion after `Init`. `Init` creates this directory. |
| `${dir}` | `t.Dir()` | Every expansion after `Init`. The trial temp directory. |
| `${seed}` | the seed, in decimal | Every expansion after `Init`. |
| `${run_id}` | `t.RunID()` | Every expansion after `Init`. `Init` passes the prefix `fuss-` plus `name` to `NewBaseTrial`. |
| `${addr}` | `127.0.0.1:<port>` | Every expansion after `Init`. Port is `port_base + seed % port_mod`. |
| `${binary}` | `t.BinaryPath()` | Every expansion after `Init`. The resolved binary path. |
| `${n}` | the string passed into `vars` | The workload request index (`0`, `1`, `2`, ...) while expanding `workload.http` url and body, `workload.ack.key`, and `workload.ack.record`. Empty on every other expansion. |
| `${key}` | see below | During the workload, the expanded `workload.ack.key`, or `<seed>-<n>` when that field is empty and `${n}` is non-empty. During an ack check, each key parsed from an `ack ` line. Otherwise empty. |
| `${i}` | node index as decimal | While expanding topology fields for one node (`0` .. `nodes-1`). Empty on every other expansion. |
| `${port}` | that node's listen port | Same topology expansion. From `topology.ports.base`, or a free port when that base is `0`. |
| `${data_dir}` | that node's data directory | Same topology expansion. The expanded `topology.data_dir`. |
| `${peer_addr_<j>}` | proxy listen addr to node `j` | Topology expansion when `topology.peers.via_proxy` is true. Empty for `j == i`. In `per_node` mode every peer uses the same inbound proxy for `j`. |
| `${peers}` | comma-joined peer proxy addrs | Same; every `${peer_addr_<j>}` for `j != i`. |
| `${node_proxy_addr_<k>}` | inbound proxy listen for node `k` | Topology expansion when `topology.proxy_mode` is `per_node` or `per_node_owned`. |
| `${peers_via_proxy}` | same as `${peers}` | Set with `per_node` or `per_node_owned` (and `via_proxy`); convenient when the start line should name the shared inbound fronts. |

These strings are expanded: `data_dir`, `args`, `restart.args`, `env` values,
`steps[].command`, `topology.data_dir`, `topology.start`,
`topology.ready.file`, `topology.ready.contains`, `topology.ready.command`,
`health.http.url`, `restart.health.http.url`,
`workload.http.url`, `workload.http.body`, `workload.ack.key`,
`workload.ack.record`, `kill.file.glob`, `kill.file.contains`, the same two
fields on a step's `kill.file`, `check.file_count.glob`,
`check.record_count.contains`, `check.command.command`,
`check.command.contains`, and `check.http.url`.

These are not expanded: `plant_record`, `plant_dir`, `binary.default`,
`binary.extra`, `env` keys, verdict strings, and `rules[].if` values.

## Top level

**`name`** (string, no default). Required. An empty name makes
`LoadYAMLTarget` return an error. Used as the target name, and as
`binary.default` when that is empty. Example: `name: toykv`

**`binary`** (object). Where to find the program when `-target` is empty.
Example: `binary: { default: toykv, extra: ["./toykv"] }`

**`binary.default`** (string, default: `name`). `LookPath` name used after
`binary.extra`. Example: `default: toykv`

**`binary.extra`** (list of strings, default: empty). Tried in order before
`PATH`. Each entry is made absolute and kept when it is a regular file.
Example: `extra: ["./toykv"]`

`-target` wins over both. The flag is resolved with `LookPath` and the source
is reported as `-target`.

**`args`** (list of strings, default: empty). Arguments to the binary on a
normal start. Also used on restart when `restart.args` is empty
(`TestStartArgsReuseOnEmptyRestart`). Example: `args: ["-data-dir", "${home}", "-addr", "${addr}"]`

**`env`** (map of string to string, default: empty). Appended to the current
environment for the start process, each step, and `check.command`. Values are
expanded. Keys are not. Example: `env: { DATA_DIR: "${home}" }`

**`data_dir`** (string, default: `${home}`). Expanded, created in `Init`, and
stored on the trial as `data_dir`. The process sees it only if `args` or
`env` passes it. Example: `data_dir: "${home}"`

**`port_base`** (int, default: `18700` when `<= 0`). Added to `seed % port_mod`
to form `${addr}`. Example: `port_base: 18700`

**`port_mod`** (int, default: `500` when `<= 0`). Modulus for the port.
Example: `port_mod: 500`

## `start`

Used when `steps` is empty. The process is `binary` plus `args`.

**`start.wait_exit`** (bool, default: `false`). `true` runs the process to
completion inside `start.timeout` and does not probe health or start the
workload. `false` starts it in the background, then health, then the workload.
Example: `wait_exit: false`

**`start.timeout`** (duration, default: empty, which uses `stepTimeout`).
Read only when `wait_exit` is true. Example: `timeout: 5s`

## `health`

**`health.http`** (object). After a background start, `waitYAMLHTTP` polls
this URL until it succeeds or `timeout` elapses. An empty `url` skips the
wait. `Status` also calls this URL: an empty `url` is state `unknown`, a
failed GET is `down`, and any HTTP response is `running`. `Status` always
uses GET and does not read `method`, `body`, or `want_status`.
Example: `health: { http: { method: GET, url: "http://${addr}/k/ready", timeout: 5s } }`

`restart.health.http` is the same object. `Start` waits on it when the
workload flag is false and `restart.health.http.url` is non-empty. Otherwise
`Start` waits on `health.http`. `Status` always reads `health.http.url`, not
the restart URL.

### `http` object

The same six fields are accepted wherever `yamlHTTP` is embedded. Only some
are read in each place.

| Field | Type | `health.http` and `restart.health.http` | `workload.http` | `check.http` |
|---|---|---|---|---|
| `method` | string | read, default `GET` | read, default `PUT` | read, default `GET` |
| `url` | string | read, default empty (skip the wait) | read, default empty (no workload loop) | read, default empty (ack check fails) |
| `body` | string | not read | read, sent as the body, default empty | not read |
| `timeout` | duration | read, default `5s` | read, per-request client timeout, default `100ms` | read, default `200ms` |
| `ack_status` | int | not read | read, default `200`. Only this status appends an ack line | not read |
| `want_status` | int | read, default `0`, and `0` means any status | not read | read, default `0`, and `0` means `200` |

Health probes use a 100ms client timeout and retry every 20ms until
`health.http.timeout`. Example: `method: GET`

## `workload`

Started only when `steps` is empty, `start.wait_exit` is false, the trial's
workload flag is true, and `workload.http.url` is non-empty. A goroutine
sends one request after another, with `${n}` equal to `0`, `1`, `2`, ... A
transport error ends the loop. Any other status is ignored and the loop
continues.

**`workload.until`** (string, no default). Accepted and never read. The loop
stops on a failed request, not on this value. Example: `until: kill`

**`workload.wait_first_ack`** (bool, default: `false`). When true, `Start`
waits until the trial record contains `ack `, or 5s, whichever comes first.
The 5s limit is fixed in `Start`. On timeout, `Start` kills the process and
returns `no PUT returned 200`. Example: `wait_first_ack: true`

**`workload.http`** (object). See the `http` table. Example: `http: { method: PUT, url: "http://${addr}/k/${seed}-${n}", body: "v", timeout: 100ms, ack_status: 200 }`

**`workload.ack.key`** (string, default: empty). Template for `${key}` during
the workload. When empty and `${n}` is non-empty, `${key}` is `<seed>-<n>`.
Example: `key: "${seed}-${n}"`

**`workload.ack.record`** (string, default: `ack ${key}\n`). Appended to the
trial record after a response whose status is `ack_status`. The ack check
only parses lines with the prefix `ack `, and `wait_first_ack` only looks
for `ack `. Example: `record: "ack ${key}\n"`

## `kill`

`LoadYAMLTarget` accepts `kill.at` of empty, `delay`, or `file`. Anything
else is an error. `kill.signal` is not read. `kill.at` and `kill.file` decide
whether `WaitBeforeKill` waits on a file. `kill.delay` is the window
`KillWindow` returns for a delay kill.

**`kill.signal`** (string, no default). Accepted and never read.
Example: `signal: SIGKILL`

**`kill.at`** (string, default: empty). `delay` and empty: `WaitBeforeKill`
does not handle the wait, and the run uses `kill.delay`. `file`: wait until
`kill.file` matches, then kill. A step can override the file spec; see
`steps`. Example: `at: delay`

**`kill.delay.min_ms`** (int, default: `0`). Low end of the delay window, in
milliseconds. Example: `min_ms: 0`

**`kill.delay.max_ms`** (int, default: `0`). High end of the window.
`KillWindow` returns `min_ms, max_ms` when `max_ms > min_ms`. Otherwise it
returns `0, 3000`. Example: `max_ms: 3000`

**`kill.file.glob`** (string, default: empty). Expanded and passed to
`filepath.Glob`. Example: `glob: "${home}/flag.txt"`

**`kill.file.contains`** (string, default: empty). Expanded. A matched file
must contain this substring. Example: `contains: READY`

**`kill.file.poll`** (duration, default: `1ms`). How often the glob is
checked. Example: `poll: 5ms`

**`kill.file.offset_ms`** (int, default: `0`). After a match, if this is `> 0`,
sleep a seed-chosen `0` to `offset_ms` milliseconds, inclusive
(`Intn(offset_ms + 1)`). Example: `offset_ms: 20`

**`kill.file.timeout`** (duration, default: `30s`). Give up if no file matches
in time. `WaitBeforeKill` returns that error. Example: `timeout: 5s`

## `restart`

Used when a check starts the target again. `checkAcks` sets the workload flag
false before calling `Start`, so the workload loop does not run.

**`restart.args`** (list of strings, default: empty). When non-empty and the
workload flag is false, these replace `args`. When empty, `args` is used.
Example: `args: ["-data-dir", "${home}", "-addr", "${addr}"]`

**`restart.health`** (object, same fields as `health`). When
`restart.health.http.url` is non-empty and the workload flag is false, that
HTTP spec is what `Start` waits on. Otherwise `health.http` is used.
Example: `health: { http: { method: GET, url: "http://${addr}/k/ready", timeout: 5s } }`

There is no separate restart `wait_exit`. Restart uses `start.wait_exit` and
`start.timeout`.

## `steps`

When `steps` has one or more entries, `Start` runs steps and does not launch
`binary` with `args`, and it does not probe `health` or start `workload`.
The binary is still resolved so the run can name it.
Each `command` is its own program: the first element is the executable.

Walk order for each step: `run_for` if set, otherwise `wait_exit`, otherwise
this step is the live process. Steps before the live one run during `Start`.
The first live step is the process that gets killed. Steps after it are
stored and run at the beginning of `Check`, logged to `after.log` in the
trial directory. If every step exits, `Start` runs `/bin/true` as the live
process.

After-steps ignore `wait_exit`. A step with `run_for` sleeps then signals.
Any other after-step runs to completion inside `timeout`.

**`steps[].name`** (string, default: empty). Used in error text.
Example: `name: hold`

**`steps[].command`** (list of strings, no default). Empty is an error.
Example: `command: ["/bin/sh", "-c", "echo A >> ${dir}/seq.txt"]`

**`steps[].wait_exit`** (bool, default: `false`). On the way to the live
step, `true` waits for the command to finish. Example: `wait_exit: true`

**`steps[].timeout`** (duration, default: empty, which uses `stepTimeout`).
Used for a `wait_exit` step during `Start`, and for every after-step that has
no `run_for`. Example: `timeout: 2s`

**`steps[].run_for`** (duration, default: empty). Non-empty: start the
command, sleep this long (an unparseable value uses `1s`), then send `stop`.
Example: `run_for: 50ms`

**`steps[].stop`** (string, default: empty, which sends `SIGTERM`). Read after
`run_for`. `SIGTERM` and `TERM` send `SIGTERM`. `SIGKILL` and `KILL` send
`SIGKILL`. Comparison is case-insensitive. Any other value is an error on
the start path (the process is then killed). On an after-step the signal
error is ignored. Example: `stop: SIGTERM`

**`steps[].kill`** (object, same fields as `kill`). On the live step, `at:
file` saves `kill.file` and `WaitBeforeKill` uses that spec instead of the
top-level `kill.file`. `signal` and `delay` on a step are accepted and not
read. A step `at` other than `file` is not read. The top-level `kill.at` is
still the one `LoadYAMLTarget` checks, and it is the fallback when no step
stored a file spec. Example: `kill: { at: file, file: { glob: "${dir}/seq.txt", contains: B, poll: 5ms, timeout: 5s } }`

## `topology`

When `topology.nodes` is greater than zero, `Start` launches that many peer
processes at the same time instead of `binary` plus `args`, and instead of
`steps`. Each node is its own process with its own data directory and port.
They stay up for the trial. `Start` also returns a `sleep` holder process so
the usual kill path still has one command to wait on. Configs with
`topology.nodes` unset or `0` behave as before.

If any node is not ready within `topology.ready.timeout`, the check verdict
is `UNKNOWN` (never `PASS`). At the end of `CheckNamed`, every node gets
`SIGTERM`, then `SIGKILL` after `topology.stop_grace`. Trial JSON evidence
includes `topology`: each node's `i`, `pid`, `port`, `data_dir`,
`start_time`, `ready`, `stop`, and `exit_status`.

**`topology.nodes`** (int, default: `0`). Count of peer processes. `0` or
absent disables topology. Example: `nodes: 3`

**`topology.data_dir`** (string, default: `${dir}/node${i}`). Expanded per
node and created before start. Example: `data_dir: "${dir}/node${i}"`

**`topology.ports.base`** (int, default: `0`). When `0`, each node gets a
free `127.0.0.1` port. When greater than zero, node `i` uses `base + i`.
Example: `ports: { base: 0 }`

**`topology.start`** (list of strings, required when `nodes > 0`). Command
for each node. The first element is the executable. Expanded with `${i}`,
`${data_dir}`, `${port}`, and the usual variables. Example:
`start: ["${binary}", "${data_dir}"]`

**`topology.ready.file`** (string, default: empty). Expanded path that must
exist (and match `contains` when that is set) for the node to be ready.
Example: `file: "${data_dir}/ready"`

**`topology.ready.contains`** (string, default: empty). Substring required
in the ready file, or in ready-command stdout when `file` is empty.
Example: `contains: READY`

**`topology.ready.command`** (list of strings, default: empty). Used when
`file` is empty. Combined output must be non-empty, or contain `contains`
when that is set. Example: `command: ["/bin/cat", "${data_dir}/ready"]`

**`topology.ready.timeout`** (duration, default: `5s`). Per-node ready wait.
Example: `timeout: 2s`

**`topology.stop_grace`** (duration, default: `2s`). After `SIGTERM`, wait
this long before `SIGKILL`. Example: `stop_grace: 500ms`

**`topology.peers.port_name`** (string, default: empty). Names which per-node
port is the peer port. With the single topology `${port}`, that port is the
peer port; client ports stay direct. Example: `port_name: raft`

**`topology.peers.via_proxy`** (bool, default: `false`). When true, Fuss starts
`proxy.Link`s for peer traffic (see `proxy_mode`). Node start templates get
`${peer_addr_j}` and `${peers}` pointing at those proxies. Example:
`via_proxy: true`

**`topology.proxy_mode`** (string, default: `per_pair`). How peer proxies are
shaped when `via_proxy` is true:

- `per_pair` — one `proxy.Link` per ordered pair `(i, j)`. Node `i` dials a
  distinct listen addr to reach `j`. This is the historical default.
- `per_node` — one inbound `proxy.Link` per node. Every peer dials the same
  listen addr to reach that node. Exposes `${node_proxy_addr_k}` and
  `${peers_via_proxy}`. Requires `via_proxy: true`.
- `per_node_owned` — one inbound proxy per node, same addresses as `per_node`.
  On accept, Fuss calls `OwnerPID` and tags the connection with the source
  node. A connection whose owner is not one of the topology PIDs is kept and
  recorded as an unknown owner. Requires `via_proxy: true`.

Example: `proxy_mode: per_node_owned`

## `fault`

Live fault while topology nodes keep running. Empty `isolate` disables it.

**`fault.isolate`** (string or object, default: empty). Cuts proxy links for
one node and leaves the process running.

- String form: `isolate: node_0` (direction defaults to `both`).
- Object form: `isolate: { node: node_0, direction: inbound }`.

**`fault.isolate.direction`** (string, default: `both`). Which links to cut:

- `both` — every link into or out of the node. Allowed for `per_pair` and
  `per_node_owned`.
- `inbound` — only links whose destination is the node.

With `topology.proxy_mode: per_node`, only `inbound` is allowed. Load rejects
`direction: both` because a shared inbound proxy cannot separate one node's
outbound flows from another's. Example:
`isolate: { node: node_0, direction: inbound }`

With `per_node_owned`, `both` cuts and refuses every connection whose source
or destination is the isolated node and keeps every other connection. Evidence
counts directed pairs (`links_cut` out of `links_total`, 6 for three nodes),
and `accepted`, `refused_while_cut`, and `closed_by_cut` are counted per source
node. An unknown owner on a cut link makes the trial `UNKNOWN`, not `PASS`.
The process Fuss starts must own the peer sockets (`exec` the node binary); a shell wrapper's child is an unknown owner, and that correctly gives `UNKNOWN`.

**`fault.pause`** (string or object, default: empty). Sends `SIGSTOP` to one
topology node's process after the same `fault.after` trigger as isolate.
Allowed alongside `fault.isolate`.

- String form: `pause: node_0` (resume defaults to `end`).
- Object form: `pause: { node: node_0, resume: never }`.

**`fault.pause.resume`** (string, default: `end`):

- `end` — send `SIGCONT` before topology teardown.
- `never` — leave the process stopped; teardown still `SIGKILL`s it.

Trial JSON `evidence.pause` records `pause_time`, `target`, `resume`,
`trigger_fired`, and `paused`. When the trigger never pauses a node, the
check verdict is `UNKNOWN` (never `PASS`).

**`fault.after.file`** (object, same fields as `kill.file`). Trigger for the
isolate and/or pause. When the trigger never fires, or after `Cut` no link
reports `closed_by_cut` or `refused_while_cut` growth, the check verdict is
`UNKNOWN` (never `PASS`). Trial JSON `evidence.isolate` records
`proxy_mode`, `direction`, `links_cut`, `links_total`, `trigger_fired`,
`cut_time`, and per-link `stats_before` / `stats_after`. `per_node_owned` also
records `unknown_owners`.
Example: `after: { file: { glob: "${dir}/cut_now", contains: NOW, poll: 10ms, timeout: 5s } }`

## `check`

Two modes. If `rules` is non-empty, or `file_count.glob` is non-empty, or
`record_count.contains` is non-empty, or `command.command` is non-empty,
`checkRules` runs and `for_each` is not used. Otherwise `checkAcks` runs.

### Ack check

**`check.for_each`** (string, default: empty). Empty and `ack` walk each ack
key. Any other value returns verdict `FAIL` with `check.for_each ... not
supported`. Example: `for_each: ack`

Keys come from record lines with the prefix `ack ` (`parseAckKeys`). The
text after `ack ` is the key. No such lines returns verdict `UNKNOWN` and
evidence `0`, and the target is not restarted.

**`check.http`** (object). Required in this mode: an empty `url` returns
`FAIL` with `check.http.url is required for for_each ack`. The check sets the
workload flag false, starts the target again (see `restart`), and requests
`url` once per key with `${key}` set to that key. Example: `http: { method: GET, url: "http://${addr}/k/${key}", want_status: 200, timeout: 200ms }`

**`check.miss_verdict`** (string, default: `FAIL`). Verdict when a key's
request fails or the status is not `want_status`. A failure to open the
check log or to restart is `FAIL`, not this value. A clean pass of every key
is `PASS`. Example: `miss_verdict: "LOST WRITE"`

### Counts, command, and rules

**`check.file_count.glob`** (string, default: empty). Expanded. The value is
how many paths `filepath.Glob` returns. A non-empty glob selects rules mode.
Example: `glob: "${home}/flag.txt"`

**`check.file_count.as`** (string, default: `files`). Name of that count for
`rules`. Example: `as: flags`

**`check.record_count.contains`** (string, default: empty). Expanded. The
value is `strings.Count` of this needle in the trial record. A non-empty
needle selects rules mode. Example: `contains: "start after"`

**`check.record_count.as`** (string, default: `matches`). Name of that count.
Example: `as: starts`

**`check.command.command`** (list of strings, default: empty). Non-empty
selects rules mode. The command is run and its combined output is stored.
Example: `command: ["/bin/cat", "${dir}/seq.txt"]`

**`check.command.as`** (string, default: `stdout`). Name of the output string.
The same prefix plus `_hits` holds the needle count when `contains` is set.
The same prefix plus `_err` holds `err.Error()` when the command returns an
error. The error does not by itself choose a verdict. Example: `as: seq`

**`check.command.contains`** (string, default: empty). Expanded needle. When
set, `<as>_hits` is how many times it occurs in the output. When empty, any
non-empty output counts as one piece of evidence. Example: `contains: C`

**`check.rules`** (list, default: empty). First match wins. If none match,
the verdict is `FAIL` and the problem text is `no rule matched` plus the
collected values. Evidence is the file count plus the record count, plus one
when the command needle hits (or the output is non-empty and `contains` is
empty). Evidence `0` is reported as `1`.

**`check.rules[].if`** (map, default: empty). Every entry must match. The key
suffix selects the comparison, and the prefix is the value name:

- `<name>_eq` numeric equal
- `<name>_gte` numeric `>=`
- `<name>_gt` numeric `>`
- `<name>_lte` numeric `<=`
- `<name>_lt` numeric `<`
- `<name>_contains` string contains. The string is `<name>`, or `<name>_out` when `<name>` is absent (`TestMatchYAMLRule`)

Any other suffix makes the whole `if` miss. An empty `if` does not match
unless `else` is set. Example: `if: { flags_eq: 1 }`

**`check.rules[].verdict`** (string, default: `FAIL` when the `if` matches).
Returned when `if` matches. Example: `verdict: PASS`

**`check.rules[].else`** (string, default: empty). When this is non-empty and
`if` is empty, that verdict is returned immediately, so put this rule last.
`else` is not paired with an `if` on the same rule. Example: `else: FAIL`

## `controls`

Map of name to a planted case. Names run in sorted order before seeds. Each
control calls `Init`, forces the workload flag false, plants, then runs the
check. It does not send the workload.

**`plant_record`** (string, default: empty). Appended to the trial record
exactly as written, with no template expansion. For an ack check the line
has to start with `ack `. Example: `plant_record: "ack planted\n"`

**`plant_dir`** (string, default: empty). A directory copied over the trial
home after that home is replaced. Resolved as the given path, then as that
path next to the config file. It must be a directory. Not expanded.
Example: `plant_dir: testdata/yaml/plant/fixture`

**`expect_verdict`** (string, default: `FAIL`). The check verdict must equal
this. Any other verdict makes `RunControl` return `control BROKEN: ...`.
Example: `expect_verdict: "LOST WRITE"`

## Annotated example

This is `examples/toykv/toykv.yaml`. Comments are the fields above applied to
that file. `workload.until` and `kill.signal` are in the file and are not
read.

```yaml
# Required target name. binary.default falls back to this.
name: toykv

# PATH name, then a binary built at ./toykv. -target overrides both.
binary:
  default: toykv
  extra: ["./toykv"]

# Workload start, and the restart start, because restart.args is empty.
# ${home} is the fresh trial home. ${addr} is 127.0.0.1:(port_base + seed % port_mod).
args: ["-data-dir", "${home}", "-addr", "${addr}"]

port_base: 18700   # default; with port_mod, seed 1 listens on 127.0.0.1:18701
port_mod: 500      # default
data_dir: "${home}" # default; created in Init. The process sees it via args.

start:
  wait_exit: false  # background the server, then health, then the workload

health:
  http:
    method: GET
    url: "http://${addr}/k/ready"  # polled up to timeout; empty url would skip this
    timeout: 5s                    # default for health

workload:
  until: kill          # accepted, never read
  wait_first_ack: true # Start blocks until the record contains "ack " (5s limit)
  http:
    method: PUT                          # default method for the workload
    url: "http://${addr}/k/${seed}-${n}" # ${n} is 0, 1, 2, ... in this loop only
    body: "v"
    timeout: 100ms                       # per-request client timeout; default
    ack_status: 200                      # only 200 appends an ack line; default
  ack:
    key: "${seed}-${n}"   # ${key} during the workload. This matches the default shape.
    record: "ack ${key}\n" # default text. The check only parses lines starting "ack ".

kill:
  signal: SIGKILL                 # accepted, never read
  at: delay                       # seeded sleep from delay, then the trial is killed
  delay: { min_ms: 0, max_ms: 3000 } # max > min, so the window is 0 and 3000

restart:
  # args omitted: the check's restart uses args above.
  health:
    http:
      method: GET
      url: "http://${addr}/k/ready" # used because the check starts with workload false
      timeout: 5s

check:
  for_each: ack # one GET per "ack <key>" line. No rules, so ack mode is used.
  http:
    method: GET
    url: "http://${addr}/k/${key}" # ${key} is the key from that record line
    want_status: 200               # 0 would also mean 200 in the ack check
    timeout: 200ms                 # default for check.http
  miss_verdict: "LOST WRITE"       # a missing key. Restart failure would be FAIL.

controls:
  lost-write:
    plant_record: "ack planted\n"      # not expanded. Plants one ack the store does not have.
    expect_verdict: "LOST WRITE"       # the check must return miss_verdict, or this control is broken
```
