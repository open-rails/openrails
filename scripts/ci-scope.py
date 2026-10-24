#!/usr/bin/env python3
"""Conservative OpenRails suite selection for an already-verified Git comparison."""
import argparse
import json
from pathlib import Path
import re
import subprocess

SOURCE = ("core", "runtime", "browser", "examples", "chart", "security")
ALL = (*SOURCE, "prose", "artifacts")


def full():
    return {key: key != "prose" for key in ALL}


def select(changes):
    selected = {key: False for key in ALL}
    reasons = []
    if not changes:
        return full(), ["empty or unknown comparison: full qualification"]
    for status, path in changes:
        if status not in ("A", "M"):
            return full(), ["deleted, renamed or unusual Git status: full qualification"]
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
        if path.endswith(".md") and ("/" not in path or path.startswith(("docs/", "sdk/billing-ui/", "compatibility/")) or path in (".claude/CLAUDE.md", "server/web/console/README.md")):
            selected["prose"] = True
            reasons.append("documentation contract: " + path)
            continue
        if path.startswith(("sdk/billing-ui/src/client", "sdk/billing-ui/src/api", "sdk/billing-ui/src/types", "sdk/billing-ui/src/hooks")) or path == "sdk/billing-ui/src/index.ts":
            selected.update({key: True for key in SOURCE})
            reasons.append("shared SDK client/API: " + path)
            continue
        frontend = path.startswith(("sdk/billing-ui/src/", "web/pay/src/", "server/web/console/src/"))
        if frontend and (any(part in {"api", "client", "types", "hooks", "services", "store", "stores"} for part in Path(path).parts) or any(word in Path(path).stem.lower() for word in ("api", "client", "request", "fetch"))):
            selected.update({key: True for key in SOURCE})
            reasons.append("shared frontend API/client: " + path)
            continue
        browser_roots = tuple(root + region for root in ("sdk/billing-ui/src/", "web/pay/src/", "server/web/console/src/") for region in ("components/", "ui/", "styles/", "locales/", "assets/")) + ("sdk/billing-ui/tests/", "web/pay/tests/", "server/web/console/tests/")
        if path.startswith(browser_roots) and Path(path).suffix in (".ts", ".tsx", ".js", ".jsx", ".css", ".svg", ".png", ".webp"):
            selected["browser"] = selected["security"] = True
            reasons.append("isolated browser source: " + path)
            continue
        if path.startswith(("examples/own-auth/", "examples/standalone/", "examples/embedded/")):
            for key in ("core", "examples", "browser", "security"):
                selected[key] = True
            reasons.append("example application: " + path)
            continue
        if path.startswith("deploy/helm/openrails/"):
            selected["chart"] = selected["security"] = True
            reasons.append("Helm chart: " + path)
            continue
        selected.update({key: True for key in SOURCE})
        reasons.append("unclassified/shared source: " + path)
    # The ordinary core suite already runs the whole contract package.
    if selected["core"]:
        selected["prose"] = False
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


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--base")
    parser.add_argument("--head")
    parser.add_argument("--full", action="store_true")
    parser.add_argument("--github-output", type=Path)
    args = parser.parse_args()
    if args.full:
        selected, reasons = full(), ["explicit full qualification"]
    else:
        if not all(re.fullmatch(r"[0-9a-f]{40}", value or "") for value in (args.base, args.head)):
            parser.error("base and head must be full commit SHAs")
        selected, reasons = select(read_changes(args.base, args.head))
    if args.github_output:
        with args.github_output.open("a") as output:
            for key, value in selected.items():
                output.write(f"{key}={str(value).lower()}\n")
    print(json.dumps({"suites": selected, "reasons": reasons}, indent=2))


if __name__ == "__main__":
    main()
