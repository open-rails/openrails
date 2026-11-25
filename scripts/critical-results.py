#!/usr/bin/env python3
"""Require the reviewed executable Go cases, not incidental nested passes.

A shard (--shard K/N) runs the top-level tests critical/durations.json balances onto it; the last
shard runs every test the others do not, so the shards cover the package exactly once. Each shard
checks its own cases; the reports of all N together prove the complete suite.
"""
import argparse
from collections import Counter
import json
import os
from pathlib import Path
import re
import statistics
import subprocess
import sys

PACKAGE = "github.com/open-rails/openrails/critical"
SCENARIO = re.compile(r"(?:^|/)(OR-J\d{2}-S\d{2})(?:/|$)")
TEST_FUNC = re.compile(r"^func (Test\w+)\(\w+ \*testing\.T\)", re.M)
ROOT = Path(__file__).resolve().parent.parent


def unique_object(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError(f"duplicate JSON key: {key}")
        result[key] = value
    return result


def read_json(path):
    return json.loads(Path(path).read_text(), object_pairs_hook=unique_object)


def package_tests():
    """The critical package's top-level tests, from its integration-tagged test files."""
    listed = subprocess.check_output(
        ["go", "list", "-find", "-tags", "integration", "-f",
         "{{range .TestGoFiles}}{{$.Dir}}/{{.}}\n{{end}}{{range .XTestGoFiles}}{{$.Dir}}/{{.}}\n{{end}}", "./critical"],
        cwd=ROOT, text=True, env=os.environ | {"GOWORK": "off"})
    return {name for path in listed.split() for name in TEST_FUNC.findall(Path(path).read_text())}


def balance(count, tests, seconds):
    """Longest first onto the least loaded shard; a test without a measurement weighs the median."""
    known = [seconds[test] for test in tests if test in seconds]
    default = statistics.median(known) if known else 1.0
    loads, shards = [0.0] * count, [set() for _ in range(count)]
    for test in sorted(tests, key=lambda test: (-seconds.get(test, default), test)):
        index = min(range(count), key=lambda i: (loads[i], i))
        shards[index].add(test)
        loads[index] += seconds.get(test, default)
    return shards


def alternation(names):
    return "^(" + "|".join(sorted(re.escape(name) for name in names)) + ")$"


parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
parser.add_argument("manifest", type=Path)
parser.add_argument("events", type=Path, nargs="*", help="go test -json reports; several are the shards of one complete run")
parser.add_argument("--executables", type=Path, default=Path("critical/executables.json"))
parser.add_argument("--durations", type=Path, default=Path("critical/durations.json"))
parser.add_argument("--case", action="append", default=[])
parser.add_argument("--journey", action="append", default=[])
parser.add_argument("--shard", help="K/N: shard K of N")
parser.add_argument("--test-args", action="store_true", help="print the go test selection flags, one per line")
parser.add_argument("--write-durations", action="store_true", help="rewrite --durations from complete-run reports")
args = parser.parse_args()
focused = bool(args.case or args.journey)
shard = None
if args.shard:
    match = re.fullmatch(r"([1-9][0-9]*)/([1-9][0-9]*)", args.shard)
    if not match or int(match[1]) > int(match[2]) or focused:
        parser.error("--shard is K/N with 1 <= K <= N, without --case or --journey")
    shard = int(match[1]), int(match[2])
if args.test_args == bool(args.events):
    parser.error("give the events files or --test-args")
if args.test_args and not (focused or shard):
    parser.error("--test-args needs a selection or --shard")
if len(args.events) > 1 and (focused or shard):
    parser.error("several reports are the shards of a complete run")

if args.write_durations:
    seconds = {}
    for path in args.events:
        for line in path.read_text().splitlines():
            event = json.loads(line)
            name = event.get("Test", "")
            if event.get("Package") == PACKAGE and name and "/" not in name and event.get("Action") in ("pass", "fail"):
                seconds[name] = round(event["Elapsed"], 1)
    document = {"meaning": "Seconds each top-level critical test took in a complete run; they balance the CI shards. Regenerate with scripts/critical-results.py --write-durations.",
                "seconds": dict(sorted(seconds.items()))}
    args.durations.write_text(json.dumps(document, indent=2) + "\n")
    print(f"{len(seconds)} tests, {sum(seconds.values()):.0f}s")
    raise SystemExit(0)

manifest = read_json(args.manifest)
rows = manifest["scenarios"]
all_ids = {row["id"] for row in rows}
if len(rows) != len(all_ids) or len(all_ids) != manifest["scenario_count"]:
    raise SystemExit("critical manifest has duplicated or missing scenario identities")
executable_map = read_json(args.executables)
journeys = {row["journey"] for row in rows}
if len(args.case) != len(set(args.case)) or not set(args.case) <= all_ids or not set(args.journey) <= journeys:
    parser.error("selection must name unique, known scenario IDs and journeys")
bindings = executable_map["scenarios"]
if set(bindings) - all_ids:
    raise SystemExit("executable map names unknown scenarios")
owners = {}
for identifier, names in bindings.items():
    if not names:
        raise SystemExit(f"empty executable binding: {identifier}")
    for name in names:
        match = SCENARIO.search(name)
        if not match or match[1] != identifier or name in owners:
            raise SystemExit(f"invalid or duplicate executable binding: {name}")
        owners[name] = identifier

if shard:
    index, count = shard
    tests = package_tests() | {name.split("/")[0] for name in owners}
    shards = balance(count, tests, read_json(args.durations)["seconds"] if args.durations.exists() else {})
    others = set().union(*(tests for i, tests in enumerate(shards) if i != index - 1))
    if index < count:
        flag, names, owned = "-run", shards[index - 1], lambda test: test in shards[index - 1]
    else:
        flag, names, owned = "-skip", others, lambda test: test not in others
    required = {name for name in owners if owned(name.split("/")[0])}
    expected = {owners[name] for name in required}
else:
    expected = set(args.case) | {row["id"] for row in rows if row["journey"] in args.journey} if focused else all_ids
    if not expected:
        parser.error("empty selection")
    required = {name for name, identifier in owners.items() if identifier in expected}
    missing_bindings = expected - bindings.keys()
    if missing_bindings:
        raise SystemExit("missing reviewed executable bindings: " + ", ".join(sorted(missing_bindings)))
if args.test_args:
    if shard:
        print(f"{flag}\n{alternation(names)}")
    else:
        tops = sorted({name.split("/")[0] for name in required})
        print(f"-run\n^({'|'.join(tops)})$/^({'|'.join(sorted(expected))})$")
    raise SystemExit(0)

runs, passes, ran_in = Counter(), Counter(), {}
failures = []
for path in args.events:
    package_passed = False
    for line in path.read_text().splitlines():
        event = json.loads(line)
        if event.get("Package") != PACKAGE:
            continue
        action, name = event.get("Action"), event.get("Test", "")
        if action == "pass" and not name:
            package_passed = True
        if action in ("fail", "build-fail", "skip"):
            failures.append(f"{action}: {name or 'critical package'} ({path.parent.name})")
        if action == "run" and name and "/" not in name:
            ran_in.setdefault(name, []).append(path.parent.name)
        match = SCENARIO.search(name)
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
    if not package_passed:
        failures.append(f"critical package did not pass ({path.parent.name})")
for test, reports in sorted(ran_in.items()):
    if len(reports) > 1:
        failures.append(f"{test} ran {len(reports)} times: {', '.join(reports)}")
for name in sorted(required):
    if runs[name] != 1 or passes[name] != 1:
        failures.append(f"{name}: expected one run and pass; got {runs[name]} runs, {passes[name]} passes")
if failures:
    print("Critical qualification failed:\n" + "\n".join(failures), file=sys.stderr)
    raise SystemExit(1)
if focused:
    scope = "focused: qualifies nothing"
elif shard:
    scope = f"shard {args.shard}: qualifies nothing alone"
else:
    scope = "complete critical-first suite" + (f" across {len(args.events)} shards" if len(args.events) > 1 else "")
print(json.dumps({"selected_scenarios": len(expected), "required_executable_cases": len(required),
                  "passed_executable_cases": len(required), "scope": scope}))
