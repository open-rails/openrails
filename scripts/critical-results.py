#!/usr/bin/env python3
"""Require real Go test execution for each selected critical behavior ID."""
import argparse
import json
from pathlib import Path
import re
import sys

parser = argparse.ArgumentParser()
parser.add_argument("manifest", type=Path)
parser.add_argument("events", type=Path)
parser.add_argument("--case", action="append", default=[])
args = parser.parse_args()
manifest = json.loads(args.manifest.read_text())
all_ids = {row["id"] for row in manifest["scenarios"]}
if len(all_ids) != manifest["scenario_count"]:
    raise SystemExit("critical manifest has duplicated or missing scenario identities")
expected = set(args.case) if args.case else all_ids
if not expected or not expected <= all_ids:
    raise SystemExit("critical selection must name known nonempty scenario IDs")
seen = set()
passed = set()
failures = []
package_passed = False
pattern = re.compile(r"(?:^|/)(OR-J\d{2}-S\d{2})(?:/|$)")
for line in args.events.read_text().splitlines():
    event = json.loads(line)
    if event.get("Package") != "github.com/open-rails/openrails/critical":
        continue
    action = event.get("Action")
    name = event.get("Test", "")
    if action == "pass" and not name:
        package_passed = True
    if action in ("fail", "skip"):
        failures.append(f"{action}: {name or 'critical package'}")
    match = pattern.search(name)
    if match is None:
        continue
    identifier = match[1]
    if identifier not in all_ids:
        failures.append(f"unmapped executed scenario: {identifier}")
    if action == "run":
        seen.add(identifier)
    if action == "pass":
        passed.add(identifier)
missing = expected - (seen & passed)
if missing:
    failures.append("missing executed passes: " + ", ".join(sorted(missing)))
if not package_passed:
    failures.append("critical package did not pass")
if failures:
    print("Critical qualification failed:\n" + "\n".join(failures), file=sys.stderr)
    raise SystemExit(1)
print(json.dumps({"selected": len(expected), "executed_passes": len(expected),
                  "scope": "focused local only" if args.case else "complete critical-first suite"}))
