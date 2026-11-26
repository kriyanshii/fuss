#!/usr/bin/env python3
"""One rqlite register workload. Both harnesses call this and only this.

The harness starts the cluster, applies the cut, and writes either
isolate_applied.json or cut_applied.json. This file records the client
history and does not know which harness did the cut.
"""

import json
import sys
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
from datetime import datetime, timezone
from pathlib import Path

DIR = Path(sys.argv[1])
KEY = sys.argv[2]
HISTORY = DIR / "history.jsonl"


def now():
    t = datetime.now(timezone.utc)
    return t.strftime("%Y-%m-%dT%H:%M:%S.") + f"{t.microsecond:06d}Z"


def emit(ev):
    with HISTORY.open("a") as f:
        f.write(json.dumps(ev, separators=(",", ":")) + "\n")


def finish():
    DIR.joinpath("drive_done").write_text("DONE\n")
    raise SystemExit(0)


def call(method, url, payload, timeout):
    data = None
    headers = {}
    if payload is not None:
        data = json.dumps(payload).encode()
        headers["Content-Type"] = "application/json"
    req = urllib.request.Request(url, data=data, headers=headers, method=method)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            return resp.status, resp.read(), None
    except urllib.error.HTTPError as err:
        return err.code, err.read(), None
    except Exception as err:
        return 0, b"", err


def is_timeout(err):
    if isinstance(err, TimeoutError):
        return True
    reason = getattr(err, "reason", None)
    return isinstance(reason, TimeoutError)


def observe(status, err):
    """ok completed the call. timeout means the client gave up.

    rejected is a statement result whose own error field says the write
    did not apply. A top-level error such as "leadership lost while
    committing log" stays error: that response can arrive after the write
    is already committed. A transport failure is error for the same reason.
    """
    if err is not None:
        if is_timeout(err):
            return "timeout"
        return "error"
    if status == 200:
        return "ok"
    return "error"


def http_addr(i):
    return DIR.joinpath(f"http_{i}").read_text().strip()


def ready(addr):
    status, body, err = call("GET", f"http://{addr}/readyz", None, 2)
    return err is None and status == 200 and b"node ok" in body


def raft_state(addr):
    status, body, err = call("GET", f"http://{addr}/status", None, 2)
    if err is not None or status != 200:
        return ""
    try:
        doc = json.loads(body)
    except json.JSONDecodeError:
        return ""
    return doc.get("store", {}).get("raft", {}).get("state", "")


def insert_ok(body):
    try:
        doc = json.loads(body)
    except json.JSONDecodeError:
        return False
    results = doc.get("results")
    if not isinstance(results, list) or not results or not isinstance(results[0], dict):
        return False
    row = results[0]
    if row.get("error"):
        return False
    n = row.get("rows_affected")
    return isinstance(n, int) and not isinstance(n, bool) and n >= 1


def query_value(body):
    try:
        doc = json.loads(body)
    except json.JSONDecodeError:
        return "error", ""
    results = doc.get("results")
    if not isinstance(results, list) or not results or not isinstance(results[0], dict):
        return "error", ""
    row = results[0]
    if row.get("error"):
        return "error", ""
    if "columns" not in row and "values" not in row:
        return "error", ""
    values = row.get("values") or []
    if not values:
        return "missing", ""
    return "present", "ok"


def wait_cut():
    deadline = time.time() + 20
    while time.time() < deadline:
        for name in ("isolate_applied.json", "cut_applied.json"):
            path = DIR / name
            if path.is_file() and path.stat().st_size > 0:
                return path
        time.sleep(0.05)
    return None


def statement_refused(body):
    """True when rqlite returned a statement error and no top-level commit error."""
    try:
        doc = json.loads(body)
    except json.JSONDecodeError:
        return False
    if doc.get("error"):
        return False
    results = doc.get("results")
    if not isinstance(results, list) or not results or not isinstance(results[0], dict):
        return False
    return bool(results[0].get("error"))


def record_write(status, body, err):
    obs = observe(status, err)
    if obs == "ok" and not insert_ok(body):
        obs = "rejected" if statement_refused(body) else "error"
    detail = "" if err is None else str(err)
    if err is None and obs in ("rejected", "error") and body:
        detail = body.decode("utf-8", "replace")[:256]
    emit({
        "kind": "client", "id": "w1", "phase": "end", "op": "write",
        "observe": obs, "error": detail, "at": now(),
    })
    return obs


def insert_then_cut(addr):
    emit({
        "kind": "client", "id": "w1", "phase": "start", "op": "write",
        "key": KEY, "value": "ok", "at": now(),
    })
    status, body, err = call(
        "POST",
        f"http://{addr}/db/execute",
        [["INSERT INTO kv(k, v) VALUES(?, ?)", KEY, "ok"]],
        5,
    )
    obs = record_write(status, body, err)
    DIR.joinpath("cut_now").write_text("NOW\n")
    return obs


def insert_while_open(addr):
    box = {}

    def run():
        emit({
            "kind": "client", "id": "w1", "phase": "start", "op": "write",
            "key": KEY, "value": "ok", "at": now(),
        })
        status, body, err = call(
            "POST",
            f"http://{addr}/db/execute",
            [["INSERT INTO kv(k, v) VALUES(?, ?)", KEY, "ok"]],
            8,
        )
        box["obs"] = record_write(status, body, err)

    th = threading.Thread(target=run)
    th.start()
    deadline = time.time() + 5
    while time.time() < deadline:
        text = HISTORY.read_text() if HISTORY.exists() else ""
        if '"id":"w1"' in text and '"phase":"start"' in text:
            break
        time.sleep(0.005)
    else:
        th.join()
        return "error"
    DIR.joinpath("cut_now").write_text("NOW\n")
    th.join()
    return box.get("obs", "error")


def main():
    HISTORY.touch()
    deadline = time.time() + 20
    addrs = None
    while time.time() < deadline:
        try:
            got = [http_addr(i) for i in range(3)]
            if all(ready(a) for a in got):
                addrs = got
                break
        except OSError:
            pass
        time.sleep(0.1)
    if not addrs:
        finish()
    leader = False
    deadline = time.time() + 15
    while time.time() < deadline:
        if raft_state(addrs[0]) == "Leader":
            leader = True
            break
        time.sleep(0.1)
    if not leader:
        finish()
    status, body, err = call(
        "POST",
        f"http://{addrs[0]}/db/execute",
        ["CREATE TABLE IF NOT EXISTS kv (k TEXT PRIMARY KEY, v TEXT)"],
        5,
    )
    if err is not None or status != 200:
        finish()
    # pending: ask for the cut after the write start is on disk and before
    # this process reads the execute response. No other gate.
    pending = len(sys.argv) > 3 and sys.argv[3] == "pending"
    obs = insert_while_open(addrs[0]) if pending else insert_then_cut(addrs[0])
    if obs == "ok":
        with (DIR / "runs.txt").open("a") as f:
            f.write(f"ack {KEY}\n")
    applied = wait_cut()
    if applied is not None:
        try:
            doc = json.loads(applied.read_text())
        except json.JSONDecodeError:
            doc = {}
        emit({
            "kind": "cut",
            "target": doc.get("target", ""),
            "direction": doc.get("direction", ""),
            "links_cut": doc.get("links_cut", 0),
            "trigger_fired": bool(doc.get("trigger_fired")),
            "at": doc.get("cut_time") or doc.get("at") or now(),
        })
        new = None
        deadline = time.time() + 12
        while time.time() < deadline and new is None:
            for i in (1, 2):
                if raft_state(addrs[i]) == "Leader":
                    new = i
                    break
            if new is None:
                time.sleep(0.1)
        if new is not None:
            q = urllib.parse.urlencode({
                "q": f"SELECT v FROM kv WHERE k='{KEY}'",
                "level": "strong",
            })
            emit({
                "kind": "client", "id": "r1", "phase": "start", "op": "read",
                "key": KEY, "from": f"node_{new}", "at": now(),
            })
            status, body, err = call("GET", f"http://{addrs[new]}/db/query?{q}", None, 5)
            obs = observe(status, err)
            value = ""
            missing = False
            if obs == "ok":
                kind, value = query_value(body)
                if kind == "missing":
                    missing = True
                    value = ""
                elif kind != "present":
                    obs = "error"
            end = {
                "kind": "client", "id": "r1", "phase": "end", "op": "read",
                "observe": obs, "at": now(),
            }
            if obs == "ok":
                end["value"] = value
                if missing:
                    end["missing"] = True
            if err is not None:
                end["error"] = str(err)
            emit(end)
    finish()


if __name__ == "__main__":
    main()
