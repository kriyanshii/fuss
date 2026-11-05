# YAML file-kill, steps, counts, plant_dir prediction, written before the run

Runs:

- `go test ./...`
- `./fuss -adapter=toykv -seed=1 -n=10 -json`
- `./fuss -target-config=examples/toykv/toykv-fsync.yaml -seed=1 -n=10 -json`

## Expected

- tests exit 0 with no Dagu on PATH (shell fixtures cover file kill, steps,
  counts/rules, plant_dir)
- toykv.yaml: LOST WRITE 10/10, lost-write control ok=true, acks > 0
- toykv-fsync.yaml: PASS 10/10, acks > 0

## Why

File-kill and steps are generic. Delay-kill toykv is unchanged: first ack
still starts the 0-3000ms clock. fsync YAML still passes `-fsync`.

## This prediction is wrong if

- any toykv trial is UNKNOWN, or
- a shell fixture test fails, or
- fsync is not PASS 10/10 with acks counted.
