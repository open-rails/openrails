#!/usr/bin/env python3
"""Exercise each tier's plan, the critical shards and the source-branch rule through their CLIs."""
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile

sys.dont_write_bytecode = True
scripts = Path(__file__).resolve().parent
root = scripts.parent
selector = scripts / "ci-scope.py"
results = scripts / "critical-results.py"
spec = importlib.util.spec_from_file_location("ci_scope", selector)
ci_scope = importlib.util.module_from_spec(spec)
spec.loader.exec_module(ci_scope)
keys = {"core", "runtime", "browser", "chart", "security", "artifacts"}
source = keys - {"artifacts"}
identity = {"GIT_AUTHOR_NAME": "Paul Fidika", "GIT_AUTHOR_EMAIL": "paul@fidika.com",
            "GIT_COMMITTER_NAME": "Paul Fidika", "GIT_COMMITTER_EMAIL": "paul@fidika.com"}
env = os.environ | identity
shards = [f"{shard}/{ci_scope.CRITICAL_SHARDS}" for shard in range(1, ci_scope.CRITICAL_SHARDS + 1)]


def plan(*args, cwd=None):
    with tempfile.TemporaryDirectory(prefix="openrails-plan-") as temporary:
        output = Path(temporary) / "github-output"
        result = json.loads(subprocess.check_output(["python3", str(selector), *args, "--github-output", str(output)], cwd=cwd, text=True))
        emitted = dict(line.split("=", 1) for line in output.read_text().splitlines())
    assert set(result["suites"]) == keys and result["reasons"] and set(result["suite_explanations"]) == keys, result
    assert emitted == {"tier": result["tier"], "critical": result["critical"] or "", "shards": emitted["shards"],
                       **{key: str(value).lower() for key, value in result["suites"].items()}}, emitted
    return {key for key, selected in result["suites"].items() if selected}, emitted["critical"], json.loads(emitted["shards"])


cases = [
    ("docs", [("M", "README.md")], set()),
    ("guide", [("M", "docs/guide.md")], set()),
    ("example-gate", [("M", "examples/gate/main.go")], {"core", "browser", "security"}),
    ("example-guide", [("A", "examples/guide/checkout.go")], {"core", "browser", "security"}),
    ("license", [("M", "LICENSE")], set()),
    ("isolated-ui", [("M", "web/console/src/components/button.tsx")], {"browser", "security"}),
    ("client", [("M", "web/billing-ui/src/client/client.ts")], source),
    ("unknown", [("A", "new-domain/new.go")], source),
    ("fixture-module", [("M", "critical/server-fixture/server_test.go")], source),
    ("shared-dependency", [("M", "go.mod")], keys),
    ("query", [("M", "internal/db/queries/customer.sql")], source),
    ("deleted-doc", [("D", "README.md")], keys),
    ("renamed-doc", [("R", "README.md")], keys),
    ("mixed", [("M", "README.md"), ("M", "web/billing-ui/src/client/client.ts")], source),
    ("empty", [], keys),
]
queue_critical = ("all", shards) if ci_scope.QUEUE_RUNS_CRITICAL else ("", ["none"])
for label, changes, expected in cases:
    with tempfile.TemporaryDirectory(prefix="openrails-selector-") as temporary:
        repo = Path(temporary)
        def git(*args):
            return subprocess.check_output(["git", "-c", "commit.gpgsign=false", "-c", "core.hooksPath=/dev/null", *args], cwd=repo, env=env, text=True).strip()
        git("init", "-q", "--initial-branch=main")
        for name in {"seed.txt", *(path for status, path in changes if status != "A")}:
            p = repo / name
            p.parent.mkdir(parents=True, exist_ok=True)
            p.write_text("original " + name + "\n")
        git("add", ".")
        git("commit", "-q", "-m", "selector baseline")
        base = git("rev-parse", "HEAD")
        for status, name in changes:
            p = repo / name
            if status == "D":
                p.unlink()
            elif status == "R":
                p.rename(repo / "RENAMED.md")
            else:
                p.parent.mkdir(parents=True, exist_ok=True)
                p.write_text("changed " + name + "\n")
        if changes:
            git("add", "-A")
            git("commit", "-q", "-m", "selector input")
        head = git("rev-parse", "HEAD")
        compared = ["--base", base, "--head", head]
        assert plan("--tier", "light", *compared, cwd=repo) == (expected, "", ["none"]), label
        assert plan("--tier", "queue", *compared, cwd=repo) == (expected, *queue_critical), label
        # A push never calls the publishing workflows.
        assert plan("--tier", "push", *compared, cwd=repo) == (expected - {"artifacts"}, "", ["none"]), label
assert plan("--tier", "push", "--head", "0" * 40) == (keys - {"artifacts"}, "", ["none"])
assert plan("--tier", "full") == (keys, "all", shards)
assert plan("--tier", "focused", "--critical", "J23 OR-J24-S47") == (set(), "J23 OR-J24-S47", ["selected"])
for refused in (["--tier", "light", "--base", "missing", "--head", "missing"], ["--tier", "light"],
                ["--tier", "focused"], ["--tier", "focused", "--critical", "J23 all"], ["--tier", "full", "--critical", "J23"]):
    assert subprocess.run(["python3", str(selector), *refused], capture_output=True).returncode != 0, refused

# Critical shards: the -run sets and the last shard's -skip complement cover the
# package's top-level tests exactly once, and only the N reports together pass.
listed = subprocess.check_output(["go", "list", "-find", "-tags", "integration", "-f",
                                  "{{range .XTestGoFiles}}{{$.Dir}}/{{.}}\n{{end}}{{range .TestGoFiles}}{{$.Dir}}/{{.}}\n{{end}}", "./critical"],
                                 cwd=root, text=True, env=os.environ | {"GOWORK": "off"})
tests = {line.split("(")[0][len("func "):] for path in listed.split() for line in Path(path).read_text().splitlines()
         if line.startswith("func Test") and "*testing.T)" in line}
bindings = json.loads((root / "critical/executables.json").read_text())["scenarios"]


def selection(shard):
    flag, pattern = subprocess.check_output(["python3", str(results), "critical/coverage.json", "--test-args", "--shard", shard], cwd=root, text=True).splitlines()
    return flag, set(filter(None, pattern[2:-2].split("|")))


for count in (1, ci_scope.CRITICAL_SHARDS):
    picked = [selection(f"{shard}/{count}") for shard in range(1, count + 1)]
    assert [flag for flag, _ in picked] == ["-run"] * (count - 1) + ["-skip"], picked
    ran = [names for _, names in picked[:-1]]
    ran.append(tests - set().union(*ran))
    assert picked[-1][1] == set().union(*ran[:-1]) and sum(map(len, ran)) == len(tests) == len(set().union(*ran)), count


def report(directory, names):
    directory.mkdir(parents=True)
    with (directory / "events.jsonl").open("w") as events:
        # A registered parent is a binding of its own; a slash may sit inside one t.Run name.
        for name in sorted(names | {name.split("/")[0] for name in names}):
            for action in ("run", "pass"):
                events.write(json.dumps({"Action": action, "Package": "github.com/open-rails/openrails/critical", "Test": name, "Elapsed": 1}) + "\n")
        events.write(json.dumps({"Action": "pass", "Package": "github.com/open-rails/openrails/critical", "Elapsed": 1}) + "\n")
    return directory / "events.jsonl"


def check(*args):
    return subprocess.run(["python3", str(results), "critical/coverage.json", *map(str, args)], cwd=root, capture_output=True, text=True).returncode == 0


with tempfile.TemporaryDirectory(prefix="openrails-shards-") as temporary:
    names = {name for paths in bindings.values() for name in paths}
    ran = [names_ for _, names_ in (selection(shard) for shard in shards[:-1])]
    owned = [{name for name in names if name.split("/")[0] in tops} for tops in ran]
    owned.append(names - set().union(*owned))
    reports = [report(Path(temporary) / f"critical-results-{index}", part) for index, part in enumerate(owned)]
    assert all(check(path, "--shard", shard) for path, shard in zip(reports, shards))
    assert check(*reports)
    assert not check(*reports[:-1]), "a missing shard passed"
    duplicate = next(iter(owned[0]))
    assert not check(*reports, report(Path(temporary) / "again", {duplicate})), "a test that ran twice passed"

check_source = scripts / "ci-source-branch.sh"
repo = "open-rails/openrails"
for head_repo, ref, admitted in [
    (repo, "rc/1.2.0", True), (repo, "rc/0.280.0", True), (repo, "chore/release-v1.2.3", True),
    (repo, "chore/release-v1.2.3-rc.1", True), ("fork/openrails", "rc/1.2.0", False),
    (repo, "feat/thing", False), (repo, "release/v1.2.3", False), (repo, "chore/release-notes", False),
    (repo, "rc/", False), (repo, "rc/a/b", False), (repo, "xrc/1.2.0", False),
    (repo, "rc/audit-2026-11", False), (repo, "rc/v1.2.0", False), (repo, "rc/1.2", False),
]:
    ran = subprocess.run(["bash", str(check_source), repo, head_repo, ref], capture_output=True, text=True)
    assert (ran.returncode == 0) == admitted, (head_repo, ref, ran.stderr)
    assert admitted or "::error::master refuses" in ran.stderr, (ref, ran.stderr)
print(f"Tier plans passed {len(cases)} real-Git cases in the light, queue and push tiers plus full, focused and refused plans; "
      f"{ci_scope.CRITICAL_SHARDS} critical shards partition {len(tests)} tests and prove coverage only together.")
