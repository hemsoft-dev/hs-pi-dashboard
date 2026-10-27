#!/usr/bin/env python3
"""Build pmd's T3 snapshot from one T3 statev2.sqlite, and optionally post it once.

Usage:
  snapshot.py DB_PATH [--environment ENV]
      print the snapshot as JSON
  snapshot.py DB_PATH --post URL --machine NAME --token-file PATH [--environment ENV]
      post it once to URL/v1/ingest/t3/NAME and print pmd's answer

The reference sender for the contract in docs/design.md (section 5, "The ingest contract",
#20). The hs-pi agent is the real sender; this script shows exactly what it must send, and
lets mini be checked against the live API by hand. It is not a service: run it once.

It sends facts read straight from the database. pmd applies the Chief's ownership rules
(chief scripts/t3-activity.py) to them. The database is opened read-only. The token is read
from a mode-600 file and never printed.
"""
import argparse
import json
import os
import re
import sqlite3
import stat
import sys
import urllib.error
import urllib.request
from datetime import datetime, timezone

ACTIVE = {"preparing", "starting", "running"}
FINISHED = {"completed", "interrupted", "failed", "cancelled"}


def stamp(t):
    return t.astimezone(timezone.utc).strftime("%Y-%m-%dT%H:%M:%S.%f")[:-3] + "Z"


def pr_facts(con, has_table, tid, payload):
    """Every PR the thread knows: t3-activity.py's sources, as repo, number, state and watch."""
    raw = [p for p in (payload.get("pullRequests") or []) +
           [payload.get("linkedPullRequest"), payload.get("branchPullRequest")] if isinstance(p, dict)]
    out = [{"repo": p.get("repository"), "number": p.get("number"),
            "state": (p.get("snapshot") or {}).get("state"), "watch": bool(p.get("watch"))} for p in raw]
    if has_table:
        for repo, number, snap in con.execute(
                "select repository, number, snapshot_json from projection_thread_pull_requests where thread_id = ?", (tid,)):
            out.append({"repo": repo, "number": number, "state": json.loads(snap or "{}").get("state"), "watch": False})
    unique = []
    for p in out:
        if p not in unique:
            unique.append(p)
    return unique


def build(db, environment=""):
    con = sqlite3.connect(f"file:{db}?mode=ro", uri=True)
    # One read transaction, so threads, runs and requests agree.
    con.execute("begin")
    generated = stamp(datetime.now(timezone.utc))
    has_table = con.execute("select 1 from sqlite_master where name = 'projection_thread_pull_requests'").fetchone()
    live = "t.archived_at is null and t.deleted_at is null"
    runs = {}
    for tid, status, ordinal, requested, completed in con.execute(
            "select r.thread_id, r.status, r.ordinal, r.requested_at, r.completed_at from orchestration_v2_projection_runs r"
            " join orchestration_v2_projection_threads t on t.thread_id = r.thread_id where " + live):
        runs.setdefault(tid, []).append((status, ordinal, completed or requested))
    threads = []
    for tid, title, project, tpayload, updated in con.execute(
            "select t.thread_id, t.title, coalesce(p.title, ''), t.payload_json, t.updated_at"
            " from orchestration_v2_projection_threads t left join projection_projects p on p.project_id = t.project_id"
            " where " + live + " order by t.thread_id"):
        payload = json.loads(tpayload or "{}")
        mine = runs.get(tid, [])
        last_done = max((n for s, n, _ in mine if s == "completed"), default=-1)
        latest = max(mine, key=lambda r: r[1], default=None)
        latest_run = {"status": latest[0], "at": latest[2]} if latest else None
        lineage = payload.get("lineage") or {}
        threads.append({
            "threadId": tid,
            "title": title or "",
            "project": project,
            "branch": payload.get("branch"),
            "creationSource": payload.get("creationSource"),
            "root": lineage.get("rootThreadId"),
            "parent": lineage.get("parentThreadId"),
            "model": (payload.get("modelSelection") or {}).get("model"),
            "updatedAt": updated,
            "latestRun": latest_run,
            "active": any(s in ACTIVE for s, _, _ in mine),
            "queued": any(s == "queued" and n > last_done for s, n, _ in mine),
            "settled": bool(latest_run and payload.get("settledOverride") == "settled"
                            and (payload.get("settledAt") or "") >= latest_run["at"]),
            "prs": pr_facts(con, has_table, tid, payload),
        })
    pending = []
    for tid, kind, created, rpayload in con.execute(
            "select r.thread_id, r.kind, r.created_at, r.payload_json from orchestration_v2_projection_runtime_requests r"
            " join orchestration_v2_projection_threads t on t.thread_id = r.thread_id"
            " where r.status = 'pending' and " + live + " order by r.created_at"):
        request = json.loads(rpayload or "{}")
        # A request on a finished run can never be answered; T3 leaves it pending. The request
        # ID carries the run ordinal, URL-encoded 0-3 times (t3-pending.py).
        m = re.search(r"ordinal%(?:25)*3A(\d+)", request.get("id") or "")
        if m and any(n == int(m[1]) and s in FINISHED for s, n, _ in runs.get(tid, [])):
            continue
        # Question text isn't in the database; the hs-pi agent reads it through its MCP.
        pending.append({"threadId": tid, "requestId": request.get("id"), "kind": kind, "createdAt": created,
                        "questions": []})
    con.close()
    return {"environment": environment, "generatedAt": generated, "threads": threads, "pending": pending}


def post(snapshot, url, machine, token_file):
    mode = os.stat(token_file).st_mode
    if mode & (stat.S_IRWXG | stat.S_IRWXO):
        sys.exit(f"{token_file} must be mode 600")
    with open(token_file) as f:
        token = f.read().strip()
    req = urllib.request.Request(url.rstrip("/") + "/v1/ingest/t3/" + machine, data=json.dumps(snapshot).encode(),
                                 headers={"Authorization": "Bearer " + token, "Content-Type": "application/json"},
                                 method="POST")
    try:
        with urllib.request.urlopen(req, timeout=30) as r:
            print(r.status, r.read().decode().strip())
    except urllib.error.HTTPError as e:
        print(e.code, e.read().decode().strip())
        sys.exit(1)


def main():
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("db")
    ap.add_argument("--environment", default="")
    ap.add_argument("--post", metavar="URL")
    ap.add_argument("--machine")
    ap.add_argument("--token-file")
    args = ap.parse_args()
    snapshot = build(args.db, args.environment)
    if not args.post:
        json.dump(snapshot, sys.stdout, indent=1)
        print()
        return
    if not args.machine or not args.token_file:
        ap.error("--post needs --machine and --token-file")
    post(snapshot, args.post, args.machine, args.token_file)


if __name__ == "__main__":
    main()
