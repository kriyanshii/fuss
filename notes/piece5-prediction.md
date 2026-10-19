# Piece 5 prediction, written before the run

Run: `./fuss -dag=human -kill-at=queued -queued-offset=30 -seed=1 -n=20 -replay=3`

## Where the flip from STRANDED to PASS falls

**Best guess: between 8 and 12ms after Queued is seen. I'd be unsurprised anywhere from 4 to 16ms.**

### Why

Between the status file showing Queued and the queue item appearing, `complete` does:

1. a directory sync after the status rename (`syncDir`, `internal/cmn/fileutil/replace_unix.go:21-27`)
2. releasing the status lock, then taking the queue record lock (a `mkdir` plus a token file)
3. writing the item's temp file and syncing it (`fileutil.go:375-394`)
4. the rename that makes the item visible

So that's at least two full syncs. I haven't measured a single sync on this machine; I'm assuming 2-6ms each.

Floor: in piece 4, all 20 kills sent within 1ms of seeing Queued landed before the item, so the window is at least about 1ms.

## Expected shape by offset

| Seeds | Offsets | Expected |
|---|---|---|
| 7, 18, 2, 3, 13, 4 | 1.2-5.5ms | STRANDED 3/3, stable |
| 20 | 8.3ms | the most likely edge seed; may diverge |
| 10, 16 | 12.4ms | PASS more likely than not; may diverge |
| the rest | 16.5-29.1ms | PASS 3/3 |

For large offsets I expect `finished-first` rather than `after-item`. After the item, `complete` only updates the index and exits. So past roughly the window plus a few ms, the process is already gone. Seeds whose offset sits near *that* edge may diverge on label (after-item vs finished-first) while the verdict stays PASS.

Expected: 1-4 seeds report REPLAY DIVERGED. No DOUBLE-RESUME, no NO-RESUME.

## What would prove this wrong

- The flip below 4ms or above 16ms means my guess about how long a sync takes is off.
- STRANDED at large offsets means something else is slow between Queued and the item.
- Divergence spread across many seeds rather than near one or two edges means the window length varies a lot between runs, which a fixed offset can't pin.

## User's prediction (added after the run finished, before the log was shown)

"My guess: the window closes about 30ms after Queued, because I think the queueing steps take that long. If I'm right, all 20 seeds come out STRANDED. If any seed with an offset under 30ms is PASS, I'm wrong."
