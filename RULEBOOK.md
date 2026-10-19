# Fuss rulebook (first draft)

## 1. What Fuss is

Fuss is a black-box fault-injection harness. It attacks unmodified programs
from the outside with faults picked by a seed, checks whether the program
keeps the promises written in its docs, and replays every failure from its
seed.

Fuss is not a simulator. It controls the faults it injects, not the target's
own timing. A seed replays the same fault, but the target may take a different
path each time. That is why replay is judged (section 6) and not assumed.

## 2. Outside rules

1. Never modify the target's binary.
2. Never write into the target's state directory. Reading it is allowed.
3. Change the target's settings only through options its docs describe.

Any exception must be written down in the README, with what breaks the rule,
why, and what the finding does or does not depend on. Example: the README's
note on `DAGU_PROC_*`, which the older Dagu modes set even though those env
vars are not in Dagu's docs.

Reading the target's files is allowed, but each place Fuss does it must be
listed in the README's limits. If a verdict depends on such a read, a control
must check that Fuss can read it correctly before any seed runs.

## 3. Picking a promise

A good promise:

- uses absolute wording ("must", "always", "never", "exactly one", "is not
  rolled back") and names a fault ("fails", "crashes", "exits", "is killed"),
- is quoted exactly from the target's docs or spec, with file and line, and a
  person has checked the quote against the source,
- can be attacked and checked entirely from outside the target.

If the promise depends on a phrase the docs do not define, write down how you
read it before you test it. That reading will be part of the report.

## 4. Writing the check

- The check gives pass or fail. Nothing in between.
- It reads only from outside: the target's own output, its documented
  commands or API, and a record Fuss writes outside the target.
- Before trusting it, show that it catches a broken state, staged outside the
  target's real state, or a real past bug. If the check never fails on a run
  that is known to be bad, it proves nothing. A control may stage a broken
  state in its own temp directory, the way the zombie control does. That does
  not break "never modify the binary", which is about the target under test.
  Building a broken copy of the target is possible but blurs that rule, so
  prefer a staged state.

## 5. Predicting

Before running, write down what you expect, in a file, with the time it was
written. Include:

- the expected result and why,
- what result would prove the prediction wrong.

Do not change a prediction after the run. If it was wrong, say so and say what
that teaches.

## 6. Replay

- A seed must replay to the same state, not just the same verdict.
- Each trial gets a label for where the fault landed, read from outside after
  the fault. If replays of one seed get different labels, the seed has
  diverged, even if every verdict matches.
- A diverged seed is reported as REPLAY DIVERGED. It is not a pass and not a
  fail.

## 7. Judging

- A bug is a broken written promise that replays from its seed 10 times out
  of 10.
- A failure that replays fewer than 10 times out of 10 is "unknown". Improve
  the trigger until it replays, or drop it. Do not report it as a bug.
- A clean replay with no broken promise is not a bug, however odd it looks.
- The 10 of 10 bar is about replay: one seed reaches the same state 10 times.
  Whether a finding holds across different faults is a separate claim, made
  by a separate run, such as a sweep of seeded offsets.

### Worked example: Dagu human-task completion

- **Promise.** Spec 031 says repeating an identical completion on a run that
  still needs resume prints `Human task <step> was already completed; DAG-run
  queued for resume.` (`specs/031-human-task.md:424`), and that repeating the
  completion "retries resume" (`:510-517`). Reading: a run that is Queued with
  no queue item "still needs resume", since nothing will run it. The spec does
  not define that phrase, and `:510-517` is written about synchronous
  failures, not a killed process.
- **Check.** STRANDED means: the run is Queued, there is no queue item, and
  the outside record shows no resumed step after 60s of scheduler time.
  Limit: "no queue item" reads Dagu's queue directory, not a public
  interface. A queue control checks that Fuss can see a queue item before any
  seed runs. The check was not shown to catch a deliberately broken Dagu, so
  this example does not fully meet section 4.
- **First trigger: unknown.** Killing `complete` at a fixed delay gave
  STRANDED in 3 of 10 replays for seed 1 and 1 of 10 for seed 2. The kill was
  sent at almost the same time every replay, but Dagu's own writes took a
  different time each run. Under this rule that is "unknown", not a bug.
- **Better trigger: bug.** Killing `complete` the moment its status file shows
  Queued gave STRANDED 10 of 10 for seed 1 and 10 of 10 for seed 2. That meets
  the replay bar. With no offset the seed picks nothing, so the two seeds ran
  the same attack; that says nothing about other faults, which is what the
  sweep is for.
- **Sweep.** With seeded offsets of 0 to 30ms, 5 of 20 seeds diverged, all
  near the window's edges. Those seeds are "unknown" on their own. Every kill
  that landed before the queue item was STRANDED, 33 of 33.

## 8. Reporting

- Make only claims a log supports. Every number points to a log line or a
  count you can rerun.
- Mark anything learned from reading code as reading, not as a result.
- Include a plain reproducer that needs nothing but the target and a shell.
  It must refuse to report a failure it did not produce.
- Keep claims as narrow as the evidence: one machine, one OS, the times you
  actually waited.

### Worked example: Dagu human-task report

- **Numbers, each from a log:** 10 of 10 with the shell reproducer, 20 of 20
  with the Queued trigger, 33 of 33 kills before the queue item in the sweep.
  A first draft said 31 of 31. Counting the log fixed it.
- **Reading, marked as reading:** the cause (Dagu writes Queued before the
  queue item, and the repeat returns early on a Queued run) came from reading
  `internal/queue/enqueue_retry.go`, not from debugging a live run.
- **Narrowed claim:** a stranded run was still stuck 83 minutes later, but no
  scheduler ran in between. The report says that, not "a scheduler failed for
  83 minutes".
- **Reproducer:** `scripts/strand_repro.sh` uses only bash and `dagu`. If the
  kill lands too late, it prints MISSED and exits 1.

## 9. Roles

- People pick the promises and approve each check before it is trusted.
- An AI may suggest promises, draft code, and draft reports. A person reviews
  them.
- No verdict depends on an AI model. Verdicts come only from the check, the
  target's output, and the outside record.
