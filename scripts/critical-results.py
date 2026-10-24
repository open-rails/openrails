#!/usr/bin/env python3
"""Require the reviewed executable Go cases, not incidental nested passes."""
import argparse
from collections import Counter
import json
import os
from pathlib import Path
import re
import sys


def unique_object(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError(f"duplicate JSON key: {key}")
        result[key] = value
    return result


parser = argparse.ArgumentParser()
parser.add_argument("manifest", type=Path)
parser.add_argument("events", type=Path)
parser.add_argument("--executables", type=Path, default=Path("critical/executables.json"))
parser.add_argument("--case", action="append", default=[])
args = parser.parse_args()
if args.case and os.environ.get("CI") == "true":
    parser.error("focused local cases cannot qualify hosted CI")
manifest = json.loads(args.manifest.read_text(), object_pairs_hook=unique_object)
rows = manifest["scenarios"]
all_ids = {row["id"] for row in rows}
if len(rows) != len(all_ids) or len(all_ids) != manifest["scenario_count"]:
    raise SystemExit("critical manifest has duplicated or missing scenario identities")
expected = set(args.case) if args.case else all_ids
if not expected or not expected <= all_ids or len(args.case) != len(set(args.case)):
    parser.error("selection must name unique, known scenario IDs")
bindings = json.loads(args.executables.read_text(), object_pairs_hook=unique_object)["scenarios"]
if set(bindings) - all_ids:
    raise SystemExit("executable map names unknown scenarios")
missing_bindings = expected - bindings.keys()
if missing_bindings:
    raise SystemExit("missing reviewed executable bindings: " + ", ".join(sorted(missing_bindings)))
pattern = re.compile(r"(?:^|/)(OR-J\d{2}-S\d{2})(?:/|$)")
owners = {}
for identifier, names in bindings.items():
    if not names:
        raise SystemExit(f"empty executable binding: {identifier}")
    for name in names:
        match = pattern.search(name)
        if not match or match[1] != identifier or name in owners:
            raise SystemExit(f"invalid or duplicate executable binding: {name}")
        owners[name] = identifier
required = {name for name, identifier in owners.items() if identifier in expected}
runs, passes = Counter(), Counter()
failures = []
package_passed = False
for line in args.events.read_text().splitlines():
    event = json.loads(line)
    if event.get("Package") != "github.com/open-rails/openrails/critical":
        continue
    action, name = event.get("Action"), event.get("Test", "")
    if action == "pass" and not name:
        package_passed = True
    if action in ("fail", "build-fail", "skip"):
        failures.append(f"{action}: {name or 'critical package'}")
    match = pattern.search(name)
    if match is None:
        continue
    identifier = match[1]
    if identifier not in all_ids or identifier not in bindings:
        failures.append(f"unmapped executed scenario: {identifier}")
        continue
    if not any(name == root or name.startswith(root + "/") for root in bindings[identifier]):
        failures.append(f"unregistered executable case: {name}")
    if name in owners:
        if action == "run":
            runs[name] += 1
        elif action == "pass":
            passes[name] += 1
for name in sorted(required):
    if runs[name] != 1 or passes[name] != 1:
        failures.append(f"{name}: expected one run and pass; got {runs[name]} runs, {passes[name]} passes")
if not package_passed:
    failures.append("critical package did not pass")
if failures:
    print("Critical qualification failed:\n" + "\n".join(failures), file=sys.stderr)
    raise SystemExit(1)
print(json.dumps({"selected_scenarios": len(expected), "required_executable_cases": len(required),
                  "passed_executable_cases": len(required),
                  "scope": "focused local only" if args.case else "complete critical-first suite"}))
