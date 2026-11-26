#!/usr/bin/env python3
"""Nothing OpenRails runs on its pool may outlive its transaction: the pool
may be a transaction pooler, which hands the server connection to another
client at commit.

- Every set_config in SQL is transaction-local (third argument true); the one
  parameterized SetConfig query is called with IsLocal: true.
- SQL and Go set no session-level setting (SET without LOCAL or TRANSACTION).
- A session advisory lock is taken only by the queries below, each called only
  from a file that dials a session of its own for it.
- LISTEN is issued only by internal/db's listener.
"""
import re
import subprocess
import sys
from pathlib import Path

root = Path(__file__).resolve().parent.parent

# Session-lock queries and the files that call them on a dialed session.
SESSION_LOCKS = {
    "LockBillingImport": {"internal/reconcile/declared_book.go"},
    "UnlockBillingImport": {"internal/reconcile/declared_book.go"},
    "LockUsagePolicy": {"internal/db/usage_policy_lock.go"},
    "LockMerchantManifestBootstrap": {"server/internal/bootstrap/merchant_manifest.go"},
}
# Go files that take a session lock or LISTEN on a session they dial.
SESSION_GO = {"internal/migrate/migrator.go", "internal/db/listen.go"}


def tracked(*patterns):
    out = subprocess.check_output(["git", "ls-files", "-z", "--", *patterns], cwd=root)
    return [p for p in out.decode().split("\0") if p and (root / p).exists()]


def calls(text, name):
    """Yield (offset, argument text) of each name( ... ) call."""
    for m in re.finditer(r"\b" + name + r"\s*\(", text):
        depth, i = 1, m.end()
        while i < len(text) and depth:
            depth += {"(": 1, ")": -1}.get(text[i], 0)
            i += 1
        yield m.start(), text[m.end() : i - 1]


def top_level_args(args):
    out, depth, cur = [], 0, ""
    for ch in args:
        if ch in "([":
            depth += 1
        elif ch in ")]":
            depth -= 1
        if ch == "," and depth == 0:
            out.append(cur.strip())
            cur = ""
        else:
            cur += ch
    out.append(cur.strip())
    return out


def line(text, offset):
    return text.count("\n", 0, offset) + 1


problems = []
query_names = {}
for path in tracked("internal/db/queries/*.sql", "internal/migrate/postgres/*.sql"):
    text = (root / path).read_text()
    for off, args in calls(text, "set_config"):
        parts = top_level_args(args)
        local = parts[-1].lower() if len(parts) == 3 else ""
        if local != "true" and not (path.endswith("/session.sql") and local == "sqlc.arg(is_local)::boolean"):
            problems.append(f"{path}:{line(text, off)}: set_config must be transaction-local (third argument true)")
    if "/queries/" in path:
        code = re.sub(r"--[^\n]*", lambda m: " " * len(m.group(0)), text)
        start = 0
        for stmt in code.split(";"):
            m = re.match(r"\s*SET\s+(?!LOCAL\b|TRANSACTION\b)", stmt, re.I)
            if m:
                problems.append(f"{path}:{line(text, start + m.end())}: a session-level SET; use SET LOCAL or set_config(..., true)")
            start += len(stmt) + 1
        name = None
        for i, ln in enumerate(text.splitlines(), 1):
            m = re.match(r"--\s*name:\s*(\w+)", ln)
            if m:
                name = m.group(1)
            if re.search(r"pg_(try_)?advisory_(un)?lock(_shared)?\s*\(", ln) and name not in SESSION_LOCKS:
                problems.append(f"{path}:{i}: {name} takes a session advisory lock; use pg_advisory_xact_lock, or dial a session and list it here")

for path in tracked("*.go"):
    if path.endswith("_test.go") or path.startswith(("critical/", "internal/db/gen/", "scripts/")):
        continue
    text = (root / path).read_text()
    for m in re.finditer(r"SetConfigParams\{([^}]*)\}", text):
        if not re.search(r"IsLocal:\s*true", m.group(1)):
            problems.append(f"{path}:{line(text, m.start())}: SetConfig must pass IsLocal: true")
    for m in re.finditer(r"[\"`]\s*SET\s+(?!LOCAL\b|TRANSACTION\b)", text):
        problems.append(f"{path}:{line(text, m.start())}: a session-level SET; use SET LOCAL or set_config(..., true)")
    if path not in SESSION_GO:
        for m in re.finditer(r"[\"`]\s*(LISTEN\s|SELECT\s+pg_(try_)?advisory_lock\()", text):
            problems.append(f"{path}:{line(text, m.start())}: LISTEN and session locks run on a dialed session (internal/db.Listen, internal/migrate)")
    for name, allowed in SESSION_LOCKS.items():
        for m in re.finditer(r"(gen\.New\([^)]*\)|Gen\([^)]*\)|GenDirectory\(\)|\bq)\." + name + r"\(", text):
            if path not in allowed:
                problems.append(f"{path}:{line(text, m.start())}: {name} holds a session lock; call it only on a dialed session ({', '.join(sorted(allowed))})")

if problems:
    print("Session state on the pool would survive its transaction:", file=sys.stderr)
    print("\n".join(problems), file=sys.stderr)
    sys.exit(1)
