---
name: fuss
description: Crash-test an unmodified program against a promise in its own docs, using Fuss. Use when asked to find, check or reproduce a durability, recovery or crash-safety bug with seeded fault injection. Verdicts come only from Fuss, never from you.
---

# Fuss: seeded crash testing, as a loop

You help a person test whether a program keeps a promise written in its docs
when it is attacked from outside, for example killed at a bad moment. Fuss
injects the fault from a seed, judges the result, and replays it. Your job is
to run that loop carefully and report what Fuss's logs show. You never decide
whether the target passed or failed.

The full method is in `RULEBOOK.md` at the repo root. This file is the loop.
The YAML field reference is `skills/fuss/CONFIG.md`.

## Hard rules

These hold at every step. If a step would break one, stop and tell the person.

1. **Never modify the target's binary.** Do not patch, rebuild or wrap it to
   change its behaviour.
2. **Never write into the target's state directory.** Fuss gives every trial
   its own fresh temp home. Reading the target's files is allowed, but each
   place it is done must be listed in the README's limits.
3. **Change the target's settings only through options its docs describe.**
   Any exception must be written in the README first, with what it breaks and
   why.
4. **Verdicts come only from Fuss's `-json` verdict field.** Quote that
   field. Never turn your own reading of logs, code or output into a
   verdict, and never "correct" a verdict Fuss printed.
5. **People decide two things:** which promise to test, and whether a check
   is approved. You may suggest both. You may not choose for them.

## Before you start

- Build Fuss with `go build -o fuss .`.
- A new target is a YAML file, `<name>.yaml`. Write it from
  `skills/fuss/CONFIG.md`. Run it with `./fuss -target-config <name>.yaml -json`.
- Pass the target's binary with `-target=/path/to/binary` when `binary.default`
  and `binary.extra` will not find it. Fuss prints the binary it resolved to
  stderr; check it is the one you meant.
- `dagu` and `rqlite` Go adapters exist only as older examples
  (`dagu_adapter.go`, `rqlite_adapter.go`).

## The loop

### 1. Mine promises

Run the promise miner over the target's docs to list candidate promises, each
quoted exactly with file and line. The miner is `promise-miner/mine_promises.py`
(stdlib only). It calls an AI model, so show the person the list of files it
will read and their total size, and wait for their OK before running it.

From `promise-miner/`, set `ANTHROPIC_API_KEY`, then
`python3 mine_promises.py --dry-run` (count tokens, estimate cost, no API
calls), `python3 mine_promises.py` (model `claude-opus-5-5`), or
`python3 mine_promises.py --model claude-fable-5-1` (deeper, pricier second
pass). It writes `promises.jsonl` (one line per spec) and `promises.md`.
Re-running skips specs already in `promises.jsonl`.

Then **the person picks one promise.** Do not rank or filter the list for
them unless asked, and never pick one yourself.

A good promise uses absolute words ("must", "always", "never", "is not rolled
back") and names a fault ("fails", "crashes", "is killed"). If it leans on a
phrase the docs do not define, write down how it is being read.

### 2. Draft the YAML config

Draft `<name>.yaml` from `skills/fuss/CONFIG.md`. Map the promise the person
picked onto these parts:

- **workload**: what gets acknowledged, and the line recorded outside the
  target when that acknowledgement arrives.
- **kill**: where the fault lands. `at: delay` is a seeded wait. `at: file`
  waits for a line the target writes.
- **restart**: how the target comes back so the check can read it.
- **check**: what is read back from outside the target (its own output, a
  documented command or API, or the record Fuss writes outside it).
  `miss_verdict` names the broken promise. A rules check names that promise
  with `rules[].verdict` instead.
- **controls**: a planted record the check must catch. `expect_verdict` is
  the verdict that planted case must produce. A check is trusted only after
  this catches a broken state.

Show the YAML to the person and wait for them to approve it before running.
That approval is the check approval in hard rule 5.

### 3. Write a blind prediction

Before running anything, write a prediction to `notes/<target>-<topic>-prediction.md`:

- what you expect and why,
- what result would prove the prediction wrong.

Write it before the run and do not edit it afterwards. If the person also
wants to guess, add theirs under its own heading before showing any results.

### 4. Run and replay

Run Fuss on the approved file with `-target-config <file> -json` and read its
lines. Each line is one JSON object with a `type`:

- `control`: `name`, `ok`. If any control has `"ok": false`, stop. Fuss did
  not run seeds and nothing it says is trustworthy until the control is fixed.
- `trial`: `seed`, `mode`, `label` (where the fault landed), `verdict`,
  `dir` (kept unless PASS), `evidence`.
- `summary`: totals for the run.

Exit codes: 0 all PASS, 1 some trial not PASS, 2 setup broken (the config or
the environment, not the target), 3 a control is broken.

Replay one seed multiple times with `-target-config <file> -seed=N -n=1` run
10 times. The bar is replay to the same **state**, not just the same verdict:
same `verdict` and same `label` in all 10. Then:

- **10 of 10, same state, a written promise broken:** a bug.
- **Fewer than 10 of 10, or labels differ:** "unknown". Report it as REPLAY
  DIVERGED. Suggest a better trigger to the person (for example a kill tied to
  an event the target writes, `kill: at: file`, see `CONFIG.md`), but do not
  report a bug.
- **Clean replay, no promise broken:** not a bug, however odd it looks.

Whether a finding holds across *different* faults is a separate claim. Make it
with a separate run, such as a sweep of many seeds (`-seed=1 -n=20`).

Compare the results with your prediction and say plainly if it was wrong.

### 5. Report

- Make only claims a log shows. Every number must point to a Fuss JSON line
  or a count the person can rerun (for example a `grep -c` over the log).
  Count from the log; do not round or remember.
- Label anything you learned from reading the target's code as **reading**,
  not as a result.
- Keep claims as narrow as the evidence: the machine, the OS, the times
  actually waited.
- Give a plain reproducer that needs only the target and a shell, no Fuss. It
  must refuse to report a failure it did not produce (print MISSED and exit
  non-zero), like `scripts/strand_repro.sh`.
- Do not file issues or push anything. The person publishes.

## Config won't run

- **Exit 2.** The config or the environment is wrong: the YAML did not load,
  `-target-config` was combined with `-adapter`, the binary was not found, the
  process did not start, or a file kill timed out. Fix the YAML and tell the
  person what you changed. This is not a verdict on the target.
- **Exit 3.** A control did not catch its planted case (`"ok": false`). The
  check is not trusted. Stop and show the person the control line. Do not run
  seeds and do not report a bug.

## Worked example

`notes/` holds a full run of this loop on Dagu's human-task completion: the
blind predictions (`piece5-prediction.md`, `repro-prediction.md`) and the
issue draft (`issue-stranded-queued.md`). `RULEBOOK.md` sections 7 and 8 walk
through it, including where it fell short of the rules.

`examples/toykv` is the five-minute YAML example. `toykv.yaml` is the config
the annotated example in `CONFIG.md` copies. `toykv-fsync.yaml` is that file
with `-fsync` added to `args`. Build and the two runs are in the repo README
and in `examples/toykv/README.md`.
