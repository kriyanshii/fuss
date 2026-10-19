# strand_repro.sh hit-rate prediction, written before any run

**Guess: 8 of 10 runs land in the window.**

Why: each loop iteration is all bash built-ins (`kill -0`, `read` over a status
file of a few KB, one regex match), so I expect one check to take roughly
0.5-2ms. The window after Queued typically lasts about 20ms (piece 5 sweep), so
a check should normally catch Queued with plenty of the window left. I'm
leaving 2 misses for slow trials. A miss is more likely to come from the read
itself being slow (bash `read` goes line by line through the whole file each
time) than from the spinning slowing Dagu down, but I'm not confident about
that.

What would prove this wrong:
- 5 or fewer hits means a bash check is far slower than I think, or the
  spinning shrinks the window.
- Misses labelled "killed=no" (complete finished before Queued was ever seen)
  point at the loop being slow or starved. Misses with "killed=yes, queue
  items=1" mean Queued was caught too late.
