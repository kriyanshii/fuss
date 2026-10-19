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
4. **Verdicts come only from Fuss.** Quote the `verdict` field of Fuss's
   `-json` output. Never turn your own reading of logs, code or output into a
   verdict, and never "correct" a verdict Fuss printed.
5. **People decide two things:** which promise to test, and whether a check
   is approved. You may suggest both. You may not choose for them.

## Before you start

- Fuss judges only Dagu today. The Dagu-specific parts are mixed into the
  core; a separate adapter per target is planned. **If the target is not
  Dagu, stop after step 2** and say that Fuss has no adapter for it yet. Do
  not invent a check outside Fuss to fill the gap.
- Build Fuss with `go build -o fuss .` and pass the target's binary with
  `-target=/path/to/binary` (default: the target on `PATH`). Fuss prints the
  binary it resolved to stderr; check it is the one you meant.

## The loop

### 1. Mine promises

Run the promise miner over the target's docs to list candidate promises, each
quoted exactly with file and line. The miner is `promise-miner/mine_promises.py`
(not yet in this repo; ask the person where it is). It calls an AI model, so
show the person the list of files it will read and their total size, and wait
for their OK before running it.

Then **the person picks one promise.** Do not rank or filter the list for
them unless asked, and never pick one yourself.

A good promise uses absolute words ("must", "always", "never", "is not rolled
back") and names a fault ("fails", "crashes", "is killed"). If it leans on a
phrase the docs do not define, write down how it is being read.

### 2. Propose a check

Propose a pass/fail check that reads only from outside the target: its own
output, its documented commands or API, and a record Fuss writes outside it.
Say what each verdict means and what the check reads.

**The person approves the check** before it is trusted. A check is trusted
only after it is shown to catch a broken state, staged outside the target's
real state, or a real past bug. In Fuss that is a control (`-control=zombie`,
`-control=stranded`); a broken control makes Fuss exit 3 and run no seeds.

### 3. Write a blind prediction

Before running anything, write a prediction to `notes/<target>-<topic>-prediction.md`:

- what you expect and why,
- what result would prove the prediction wrong.

Write it before the run and do not edit it afterwards. If the person also
wants to guess, add theirs under its own heading before showing any results.

### 4. Run and replay

Run Fuss with `-json` and read its lines. Each line is one JSON object with a
`type`:

- `control`: `name`, `ok`. If any control has `"ok": false`, stop. Fuss did
  not run seeds and nothing it says is trustworthy until the control is fixed.
- `trial`: `seed`, `rep`, `mode`, `label` (where the fault landed),
  `verdict`, `dir` (kept unless PASS), `evidence`.
- `seed_summary`: `verdicts`, `labels`, `diverged` (human mode).
- `summary`: totals for the run.

Exit codes: 0 all PASS, 1 some trial not PASS, 2 setup broken (the
environment, not the target), 3 a control is broken.

Replay one seed 10 times: `-replay=10` with `-dag=human`; other modes reject
`-replay`, so run the same `-seed=N -n=1` 10 times. The bar is replay to the same
**state**, not just the same verdict: same `verdict` and same `label` in all
10. Then:

- **10 of 10, same state, a written promise broken:** a bug.
- **Fewer than 10 of 10, or labels differ:** "unknown". Report it as REPLAY
  DIVERGED. Suggest a better trigger to the person (for example one tied to
  an event the target writes, like `-kill-at=queued`), but do not report a bug.
- **Clean replay, no promise broken:** not a bug, however odd it looks.

Whether a finding holds across *different* faults is a separate claim. Make it
with a separate run, such as a sweep of seeded offsets (`-queued-offset=N`
with many seeds and `-replay=3`).

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

## Worked example

`notes/` holds a full run of this loop on Dagu's human-task completion: the
blind predictions (`piece5-prediction.md`, `repro-prediction.md`) and the
issue draft (`issue-stranded-queued.md`). `RULEBOOK.md` sections 7 and 8 walk
through it, including where it fell short of the rules.
