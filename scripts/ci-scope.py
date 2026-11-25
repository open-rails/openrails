#!/usr/bin/env python3
"""Each CI tier's plan: conservative suite selection over a verified Git comparison, and the critical runs."""
import argparse
import json
from pathlib import Path
import re
import subprocess

SOURCE = ("core", "runtime", "browser", "chart", "security")
ALL = (*SOURCE, "artifacts")
# The whole critical suite runs on this many runners (scripts/critical-results.py --shard).
CRITICAL_SHARDS = 8
# Merge groups run the whole critical suite. Turn it off while the RC is red: every group would
# fail, the fixes included.
QUEUE_RUNS_CRITICAL = True
TIERS = ("light", "queue", "push", "full", "focused")


def full():
    return {key: True for key in ALL}


def select(changes):
    selected = {key: False for key in ALL}
    reasons = []
    if not changes:
        return full(), ["empty or unknown comparison: full qualification"]
    for status, path in changes:
        if status not in ("A", "M"):
            return full(), [f"deleted, renamed or unusual Git status ({status}): {path}; full qualification"]
        # Dependency, generated API, schema and CI changes can cross all boundaries.
        name = Path(path).name
        if (name in ("go.mod", "go.sum", "package.json", "pnpm-lock.yaml", "package-lock.json", "yarn.lock", "Chart.lock")
                or path.startswith((".github/", "scripts/", "tools/", "api/", "docs/api/", "migrations/", "internal/db/"))
                or "generated" in path.split("/") or path.endswith(".sql")):
            selected.update({key: True for key in SOURCE})
            selected["artifacts"] |= path.startswith((".github/", "scripts/", "tools/")) or name in ("go.mod", "go.sum", "package.json", "pnpm-lock.yaml", "package-lock.json", "yarn.lock", "Chart.lock")
            reasons.append("shared/dependency/schema/CI input: " + path)
            continue
        if path in ("Dockerfile", ".dockerignore", ".goreleaser.yaml"):
            selected.update(full())
            reasons.append("artifact build input: " + path)
            continue
        if path == "LICENSE":
            reasons.append("license prose: " + path)
            continue
        if path.endswith(".md") and ("/" not in path or path.startswith(("docs/", "web/", "compatibility/")) or path == ".claude/CLAUDE.md"):
            reasons.append("documentation-only input; behavioral suites unchanged: " + path)
            continue
        if path.startswith(("web/billing-ui/src/client", "web/billing-ui/src/api", "web/billing-ui/src/types", "web/billing-ui/src/hooks")) or path == "web/billing-ui/src/index.ts":
            selected.update({key: True for key in SOURCE})
            reasons.append("shared SDK client/API: " + path)
            continue
        frontend = path.startswith(("web/billing-ui/src/", "web/pay/src/", "web/console/src/"))
        if frontend and (any(part in {"api", "client", "types", "hooks", "services", "store", "stores"} for part in Path(path).parts) or any(word in Path(path).stem.lower() for word in ("api", "client", "request", "fetch"))):
            selected.update({key: True for key in SOURCE})
            reasons.append("shared frontend API/client: " + path)
            continue
        browser_roots = tuple(root + region for root in ("web/billing-ui/src/", "web/pay/src/", "web/console/src/") for region in ("components/", "ui/", "styles/", "locales/", "assets/")) + ("web/billing-ui/tests/", "web/pay/tests/", "web/console/tests/")
        if path.startswith(browser_roots) and Path(path).suffix in (".ts", ".tsx", ".js", ".jsx", ".css", ".svg", ".png", ".webp"):
            selected["browser"] = selected["security"] = True
            reasons.append("isolated browser source: " + path)
            continue
        if path.startswith("examples/"):
            for key in ("core", "browser", "security"):
                selected[key] = True
            reasons.append("example application: " + path)
            continue
        if path.startswith("deploy/helm/openrails/"):
            selected["chart"] = selected["security"] = True
            reasons.append("Helm chart: " + path)
            continue
        selected.update({key: True for key in SOURCE})
        reasons.append("unclassified/shared source: " + path)
    return selected, reasons


def read_changes(base, head):
    raw = subprocess.check_output(["git", "diff", "--name-status", "-z", "--find-renames", base, head, "--"])
    tokens = raw.decode().split("\0")
    if tokens[-1] != "":
        raise ValueError("incomplete Git diff")
    changes = []
    cursor = 0
    while cursor < len(tokens) - 1:
        status, path = tokens[cursor:cursor + 2]
        cursor += 2
        changes.append((status, path))
        if status.startswith(("R", "C")):
            if cursor >= len(tokens) - 1:
                raise ValueError("incomplete renamed path")
            cursor += 1
    return changes


def critical_runs(tier, selection):
    """The critical selection (all, a focused list or none) and the runners it takes."""
    if tier == "focused":
        return " ".join(selection), ["selected"]
    if tier == "full" or (tier == "queue" and QUEUE_RUNS_CRITICAL):
        return "all", [f"{shard}/{CRITICAL_SHARDS}" for shard in range(1, CRITICAL_SHARDS + 1)]
    # A matrix needs a value even when its job is skipped.
    return "", ["none"]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--tier", choices=TIERS, required=True)
    parser.add_argument("--base", help="light, queue, push: the tested base; a push without one selects every suite")
    parser.add_argument("--head")
    parser.add_argument("--critical", default="", help="focused: journeys and scenarios (J24 OR-J04-S80)")
    parser.add_argument("--github-output", type=Path)
    args = parser.parse_args()
    selection = args.critical.split()
    if args.tier == "focused":
        if not selection:
            parser.error("a focused run names journeys or scenarios")
        for item in selection:
            if not re.fullmatch(r"J[0-9]{2}|OR-J[0-9]{2}-S[0-9]{2}", item):
                parser.error(f"not a journey or scenario: {item}")
    elif selection:
        parser.error("only a focused run takes --critical")
    compared = args.tier in ("light", "queue", "push") and (bool(args.base) or args.tier != "push")
    if compared and not all(re.fullmatch(r"[0-9a-f]{40}", value or "") for value in (args.base, args.head)):
        parser.error("base and head must be full commit SHAs")
    if args.tier == "full":
        selected, reasons = full(), ["explicit full qualification"]
    elif args.tier == "focused":
        selected, reasons = {key: False for key in ALL}, ["focused critical run: " + " ".join(selection)]
    elif compared:
        selected, reasons = select(read_changes(args.base, args.head))
    else:
        selected, reasons = full(), ["push with no previous commit on the branch: every suite"]
    if args.tier == "push":
        # release.yaml and docker-publish.yaml publish on a push event; a push never calls them.
        selected["artifacts"] = False
    critical, shards = critical_runs(args.tier, selection)
    if args.github_output:
        with args.github_output.open("a") as output:
            output.write(f"tier={args.tier}\n")
            for key, value in selected.items():
                output.write(f"{key}={str(value).lower()}\n")
            output.write(f"critical={critical}\nshards={json.dumps(shards)}\n")
    explanations = {}
    for suite, enabled in selected.items():
        if not enabled:
            explanations[suite] = {"selected": False, "reason": "No changed input reaches this suite under the explicit isolation rules."}
        elif not compared:
            explanations[suite] = {"selected": True, "reason": reasons[0]}
        else:
            inputs = [reason for change in read_changes(args.base, args.head)
                      for flags, why in [select([change])] if flags[suite] for reason in why]
            explanations[suite] = {"selected": True, "affected_inputs": len(inputs),
                                   "examples": inputs[:8] or reasons}
    print(json.dumps({"tier": args.tier, "suites": selected, "critical": critical or None, "critical_runners": len(shards) if critical else 0,
                      "reasons": reasons, "suite_explanations": explanations}, indent=2))


if __name__ == "__main__":
    main()
