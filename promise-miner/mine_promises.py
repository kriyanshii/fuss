#!/usr/bin/env python3
"""Mine Dagu's specs for promises Fuss could test from the outside.

What it does: sends each spec in ~/dagu/specs to Claude and asks for the
crash, restart, concurrency, and at-most-once promises the spec makes, each
with an exact quote, the fault that would test it, and what you could observe
from outside the binary. It does NOT write checker rules. You read the
catalog and write the rules yourself.

Usage (on your Mac, stdlib only):
  export ANTHROPIC_API_KEY=...
  python3 mine_promises.py --dry-run          # count tokens, estimate cost, no API calls
  python3 mine_promises.py                    # run with claude-opus-5-5
  python3 mine_promises.py --model claude-fable-5-1   # deeper, pricier second pass

Output: promises.jsonl (one line per spec) and promises.md (readable catalog).
Re-running skips specs already in promises.jsonl, so a crash mid-run is safe.
"""
import argparse, glob, json, os, re, sys, time, urllib.request, urllib.error

PRICES = {  # USD per million tokens, from Anthropic's models overview (Oct 2026)
    "claude-fable-5-1": (10, 50),
    "claude-opus-5-5": (4, 20),
    "claude-sonnet-5-5": (2, 10),
}

# The Dagu defaults reproduce the original prompts exactly; --target,
# --interfaces and --workload adapt them to another program.
DAGU_INTERFACES = """It can start/stop the
binary, set DAGU_HOME, write DAG YAML files, call the CLI/HTTP API, read files
on disk, and make DAG steps append to its own record file."""

SYSTEM = """You help build Fuss, a Go tool that attacks an UNMODIFIED {target} binary
from outside (process kill, disk-full, clock skew), records its own history in
a file outside {target}, and checks that history against {target}'s written promises.
Fuss cannot change {target}'s code or read its internals. {interfaces}"""

ASK = """Here is one {target} spec ({name}). List every promise in it that Fuss could
test from outside under faults. Focus on: crash or restart safety, atomic
writes, at-most-once / exactly-once behavior, locking, retries, idempotency,
scheduling across restarts, and ordering. Skip promises about secrets, UI, or
"what a step will not do".

Reply with JSON only, no prose, in this shape:
{{"spec": "{name}", "promises": [
  {{"quote": "exact sentence(s) from the spec",
    "plain": "the promise in one plain sentence",
    "fault": "process-kill | disk-full | clock-skew | concurrent-calls | restart | other",
    "when_to_inject": "the moment the fault should hit",
    "outside_observation": "what Fuss can see from outside to tell if it held",
    "workload_sketch": "{workload} that exercises it",
    "why_it_might_break": "one honest guess, labeled as a guess",
    "confidence": "high | medium | low that this is testable from outside"}}
]}}
Return {{"spec": "{name}", "promises": []}} if there are none.

SPEC:
{body}"""


def call(model, key, system, prompt, max_tokens):
    req = urllib.request.Request(
        "https://api.anthropic.com/v1/messages",
        data=json.dumps({
            "model": model, "max_tokens": max_tokens, "system": system,
            "messages": [{"role": "user", "content": prompt}],
        }).encode(),
        headers={"x-api-key": key, "anthropic-version": "2023-06-01",
                 "content-type": "application/json"},
    )
    for attempt in range(5):
        try:
            with urllib.request.urlopen(req, timeout=600) as r:
                return json.load(r)
        except urllib.error.HTTPError as e:
            body = e.read().decode(errors="replace")
            if e.code in (429, 500, 502, 503, 529) and attempt < 4:
                time.sleep(2 ** attempt * 5)
                continue
            sys.exit(f"API error {e.code}: {body}")


def parse(text):
    text = text.strip()
    if text.startswith("```"):
        text = text.split("\n", 1)[1].rsplit("```", 1)[0]
    return json.loads(text)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--specs", default=os.path.expanduser("~/dagu/specs"))
    ap.add_argument("--model", default="claude-opus-5-5")
    ap.add_argument("--max-tokens", type=int, default=16000)
    ap.add_argument("--out", default="promises")
    ap.add_argument("--dry-run", action="store_true")
    ap.add_argument("--target", default="Dagu", help="the program under test, as named in the prompts")
    ap.add_argument("--interfaces", default=DAGU_INTERFACES,
                    help="what Fuss can do to the target from outside; the model judges testability against this")
    ap.add_argument("--workload", default="a minimal DAG or CLI sequence",
                    help="what a workload sketch should be, e.g. a minimal sequence of HTTP API calls")
    ap.add_argument("--all-md", action="store_true",
                    help="read every *.md in --specs, not only Dagu-style NNN-*.md and IMPLEMENTATION.md")
    a = ap.parse_args()
    system = SYSTEM.format(target=a.target, interfaces=a.interfaces)

    files = sorted(glob.glob(os.path.join(a.specs, "*.md")))
    if not a.all_md:
        files = [f for f in files if re.match(r"\d{3}-", os.path.basename(f))
                 or os.path.basename(f) == "IMPLEMENTATION.md"]
    if not files:
        sys.exit(f"no specs found in {a.specs}")

    in_tok = sum(len(open(f).read()) for f in files) // 4 + len(files) * 600
    pin, pout = PRICES.get(a.model, (0, 0))
    est = in_tok / 1e6 * pin + len(files) * 6000 / 1e6 * pout
    print(f"{len(files)} specs, ~{in_tok:,} input tokens, model {a.model}, "
          f"rough cost ${est:.0f} (assumes ~6k output+thinking tokens per spec)")
    if a.dry_run:
        return

    key = os.environ.get("ANTHROPIC_API_KEY") or sys.exit("set ANTHROPIC_API_KEY")
    jsonl = f"{a.out}.{a.model}.jsonl"
    done = set()
    if os.path.exists(jsonl):
        done = {json.loads(l)["spec"] for l in open(jsonl) if l.strip()}

    spent = 0.0
    with open(jsonl, "a") as out:
        for f in files:
            name = os.path.basename(f)
            if name in done:
                continue
            prompt = ASK.format(target=a.target, workload=a.workload, name=name, body=open(f).read())
            resp = call(a.model, key, system, prompt, a.max_tokens)
            u = resp.get("usage", {})
            spent += u.get("input_tokens", 0) / 1e6 * pin + u.get("output_tokens", 0) / 1e6 * pout
            text = "".join(b.get("text", "") for b in resp["content"] if b.get("type") == "text")
            try:
                rec = parse(text)
            except Exception:
                rec = {"spec": name, "promises": [], "raw": text}
            rec["spec"] = name
            # "max_tokens" means the reply was cut off; its promises are incomplete or missing.
            rec["stop_reason"] = resp.get("stop_reason")
            out.write(json.dumps(rec) + "\n"); out.flush()
            flag = ""
            if rec["stop_reason"] != "end_turn":
                flag = f"  STOPPED: {rec['stop_reason']}"
            if "raw" in rec:
                flag += "  REPLY DID NOT PARSE (kept as raw)"
            print(f"{name}: {len(rec['promises'])} promises  (running cost ~${spent:.2f}){flag}")

    recs = [json.loads(l) for l in open(jsonl) if l.strip()]
    with open(f"{a.out}.{a.model}.md", "w") as md:
        md.write(f"# {a.target} promises Fuss could test ({a.model})\n\n"
                 "Candidates only. Check every quote against the spec and write the rules yourself.\n\n")
        for r in recs:
            if not r["promises"]:
                continue
            md.write(f"## {r['spec']}\n\n")
            for p in r["promises"]:
                md.write(f"- **{p.get('plain','')}** ({p.get('fault','')}, {p.get('confidence','')})\n"
                         f"  - Quote: > {p.get('quote','')}\n"
                         f"  - Inject: {p.get('when_to_inject','')}\n"
                         f"  - Observe: {p.get('outside_observation','')}\n"
                         f"  - Workload: {p.get('workload_sketch','')}\n"
                         f"  - Guess: {p.get('why_it_might_break','')}\n")
            md.write("\n")
    print(f"wrote {jsonl} and {a.out}.{a.model}.md")


if __name__ == "__main__":
    main()
