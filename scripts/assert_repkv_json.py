#!/usr/bin/env python3
"""Assert fuss -json trials for a three-node repkv isolate run.

Usage: assert_repkv_json.py <json-log> <verdict> <count>

PASS and LOST WRITE require an applied partition and a read from a surviving
leader (node_1 or node_2) after the cut. UNKNOWN requires reason=read_error
and must not be classified as LOST WRITE.
"""

import json
import sys


def main():
    if len(sys.argv) != 4:
        print("usage: assert_repkv_json.py <json> <verdict> <count>", file=sys.stderr)
        return 2
    path, expected, count_s = sys.argv[1:]
    count = int(count_s)
    trials = []
    with open(path) as f:
        for raw in f:
            line = raw.strip()
            if not line:
                continue
            rec = json.loads(line)
            if rec.get("type") == "trial":
                trials.append(rec)
    if len(trials) != count:
        print("trials %d want %d" % (len(trials), count), file=sys.stderr)
        return 1
    for i, trial in enumerate(trials):
        if trial.get("verdict") != expected:
            print("trial %d verdict %r want %r" % (i, trial.get("verdict"), expected), file=sys.stderr)
            return 1
        ev = trial.get("evidence") or {}
        iso = ev.get("isolate") or {}
        if iso.get("trigger_fired") is not True or num(iso.get("links_cut")) < 1:
            print("trial %d partition did not land" % i, file=sys.stderr)
            return 1
        if iso.get("target") != "node_0":
            print("trial %d isolate target %r" % (i, iso.get("target")), file=sys.stderr)
            return 1
        if not partition_observed(iso):
            print("trial %d no link closed or refused after the cut" % i, file=sys.stderr)
            return 1
        out = ((ev.get("check") or {}).get("out")) or ""
        if expected == "UNKNOWN":
            if "reason=read_error" not in out or "LOST WRITE" in out:
                print("trial %d read-error output %r" % (i, out), file=sys.stderr)
                return 1
            continue
        if "read_from=node_1" not in out and "read_from=node_2" not in out:
            print("trial %d read was not from a surviving leader: %r" % (i, out), file=sys.stderr)
            return 1
        if "leader=self" not in out or "after_cut=yes" not in out:
            print("trial %d read line missing leader or cut: %r" % (i, out), file=sys.stderr)
            return 1
        if expected == "LOST WRITE" and "value_missing=yes" not in out:
            print("trial %d lost write without a successful miss: %r" % (i, out), file=sys.stderr)
            return 1
    print("all %d trials verdict %s; partition landed; read checked" % (count, expected))
    return 0


def partition_observed(iso):
    for link in iso.get("links") or []:
        if not link.get("cut"):
            continue
        before = link.get("stats_before") or {}
        after = link.get("stats_after") or {}
        if num(after.get("closed_by_cut")) > num(before.get("closed_by_cut")):
            return True
        if num(after.get("refused_while_cut")) > num(before.get("refused_while_cut")):
            return True
    return False


def num(v):
    if isinstance(v, bool) or v is None:
        return 0
    if isinstance(v, (int, float)):
        return v
    return 0


if __name__ == "__main__":
    sys.exit(main())
