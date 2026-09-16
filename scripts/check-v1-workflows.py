#!/usr/bin/env python3
"""Run the release workflows and require actual, non-skipped top-level passes."""
import json
import os
from pathlib import Path
import subprocess
import sys

root = Path(__file__).resolve().parents[1]
os.chdir(root)
rows = [line.split("\t") for line in (root / "compatibility/workflows.tsv").read_text().splitlines() if line.strip()]
if not rows or any(len(row) != 2 for row in rows):
    sys.exit("invalid or empty v1 workflow manifest")
module = "github.com/open-rails/openrails"
required = {(module + (path[1:] if path != "." else ""), test) for path, test in rows}
if len(required) != len(rows):
    sys.exit("duplicate v1 workflow manifest row")
packages = sorted({row[0] for row in rows})
tests = "^(" + "|".join(sorted({row[1] for row in rows})) + ")$"
command = ["go", "test", "-json", "-count=1", "-p", "1", "-parallel", "1", "-tags", "integration", "-timeout", "25m", "-run", tests, *packages]
report = root / ".reports/v1-workflows.jsonl"
report.parent.mkdir(parents=True, exist_ok=True)
seen = {}
with report.open("w") as log:
    process = subprocess.Popen(command, stdout=subprocess.PIPE, stderr=None, text=True, env={**os.environ, "GOMAXPROCS": os.environ.get("GOMAXPROCS", "2")})
    assert process.stdout is not None
    for line in process.stdout:
        log.write(line)
        event = json.loads(line)
        action, package, test = event.get("Action"), event.get("Package"), event.get("Test")
        if action in {"pass", "fail", "skip"} and (package, test) in required:
            seen[(package, test)] = action
            print(f"{action}: {package}.{test}", flush=True)
        if action == "fail":
            print(event, file=sys.stderr, flush=True)
    status = process.wait()
missing = sorted(key for key in required if seen.get(key) != "pass")
if status or missing:
    print(f"v1 workflow qualification failed: exit={status}, missing/non-pass={missing}; details in {report}", file=sys.stderr)
    sys.exit(1)
print(f"Qualified {len(required)} actual workflows; detailed receipt: {report}")
