#!/usr/bin/env python3
import importlib.util
import json
import os
import sys
import textwrap

sys.dont_write_bytecode = True
from pathlib import Path
import subprocess
import tempfile
import unittest

spec = importlib.util.spec_from_file_location("scope", Path(__file__).with_name("ci-scope.py"))
scope = importlib.util.module_from_spec(spec)
spec.loader.exec_module(scope)


class ScopeTests(unittest.TestCase):
    def active(self, changes):
        return {key for key, value in scope.select(changes)[0].items() if value}

    def test_docs_keep_cheap_guards_only(self):
        self.assertEqual(self.active([("M", "docs/architecture.md"), ("A", "README.md")]), set())

    def test_browser_region_keeps_all_browser_consumers(self):
        self.assertEqual(self.active([("M", "sdk/billing-ui/src/client.ts")]), {"browser", "security"})
        self.assertEqual(self.active([("M", "web/pay/src/pay.tsx")]), {"browser", "security"})

    def test_examples_keep_go_and_browser_hosts(self):
        self.assertEqual(self.active([("A", "examples/own-auth/main.go")]), {"core", "examples", "browser", "security"})

    def test_chart_is_isolated(self):
        self.assertEqual(self.active([("M", "deploy/helm/openrails/templates/deployment.yaml")]), {"chart", "security"})

    def test_shared_and_unknown_paths_are_broad(self):
        for path in ("engine.go", "internal/config/runtime.go", "migrations/0114.sql", "go.mod", "sdk/billing-ui/pnpm-lock.yaml", "docs/api/routes.md", "new-directory/check.ts"):
            with self.subTest(path=path):
                self.assertTrue(set(scope.SOURCE) <= self.active([("M", path)]))

    def test_ci_and_deletion_include_artifact_checks(self):
        for change in (("M", ".github/workflows/ci.yaml"), ("D", "docs/old.md"), ("R100", "docs/old.md"), ("M", "Dockerfile")):
            with self.subTest(change=change):
                self.assertEqual(self.active([change]), set(scope.ALL))

    def test_empty_is_never_a_quiet_skip(self):
        self.assertEqual(self.active([]), set(scope.ALL))

    def test_mixed_regions_union(self):
        self.assertEqual(self.active([("M", "web/pay/src/pay.tsx"), ("M", "deploy/helm/openrails/values.yaml")]), {"browser", "chart", "security"})

    def test_real_git_rename_is_not_lost(self):
        with tempfile.TemporaryDirectory() as directory:
            def git(*args):
                return subprocess.check_output(["git", "-C", directory, *args], text=True).strip()
            git("init", "-q")
            git("config", "user.name", "test")
            git("config", "user.email", "test@example.invalid")
            Path(directory, "old.md").write_text("a documented example\n")
            git("add", ".")
            git("commit", "-qm", "base")
            base = git("rev-parse", "HEAD")
            git("mv", "old.md", "new.md")
            git("commit", "-qm", "rename")
            result = subprocess.check_output(["python3", str(Path(__file__).with_name("ci-scope.py").resolve()), "--base", base, "--head", git("rev-parse", "HEAD")], cwd=directory, text=True)
            self.assertIn('"artifacts": true', result)


class WorkflowBoundaryTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.workflow = (Path(__file__).resolve().parent.parent / '.github/workflows/ci.yaml').read_text()
        scope_block = cls.workflow.split('      - name: Verify the candidate and select suites', 1)[1].split('\n  guards:', 1)[0]
        cls.scope_shell = textwrap.dedent(scope_block.split('        run: |\n', 1)[1])
        cls.scope_shell = cls.scope_shell.replace('scripts/ci-scope.py', str(Path(__file__).with_name('ci-scope.py').resolve()))
        cls.gate_python = textwrap.dedent(cls.workflow.split("          python3 - <<'CHECK'\n", 1)[1].split('          CHECK', 1)[0])

    def test_gate_rejects_failed_cancelled_or_missing_selected_suites(self):
        jobs = ['guards', 'go-test', 'lint', 'console', 'chart', 'sql', 'e2e-shards', 'examples-e2e', 'security', 'browser', 'artifact-release', 'artifact-image']
        good = {job: {'result': 'success'} for job in jobs}
        good['scope'] = {'result': 'success', 'outputs': {key: 'true' for key in scope.ALL}}
        def run(value):
            return subprocess.run([sys.executable, '-c', self.gate_python], env={**os.environ, 'RESULTS': json.dumps(value)}, capture_output=True)
        self.assertEqual(run(good).returncode, 0)
        for status in ['failure', 'cancelled', 'skipped']:
            broken = json.loads(json.dumps(good))
            broken['sql']['result'] = status
            self.assertNotEqual(run(broken).returncode, 0, status)
        good['scope']['outputs'].pop('runtime')
        self.assertNotEqual(run(good).returncode, 0)

    def test_real_synthetic_merge_and_changed_base(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            origin, repo = root/'origin.git', root/'repo'
            subprocess.run(['git', 'init', '--bare', '-q', str(origin)], check=True)
            repo.mkdir()
            def git(*args):
                return subprocess.check_output(['git', '-C', str(repo), *args], text=True, stderr=subprocess.DEVNULL).strip()
            git('init', '-b', 'master', '-q')
            git('config', 'user.name', 'test')
            git('config', 'user.email', 'test@example.invalid')
            git('config', 'commit.gpgsign', 'false')
            git('remote', 'add', 'origin', str(origin))
            (repo/'README.md').write_text('base\n')
            git('add', '.'); git('commit', '-qm', 'base'); git('push', '-q', 'origin', 'master')
            base = git('rev-parse', 'HEAD')
            git('checkout', '-qb', 'feature')
            (repo/'README.md').write_text('proposed prose\n')
            git('commit', '-qam', 'feature')
            head = git('rev-parse', 'HEAD')
            git('checkout', '-q', 'master'); git('merge', '--no-ff', '-qm', 'synthetic', 'feature')
            merged = git('rev-parse', 'HEAD')
            out = root/'outputs'; out.touch()
            environment = {**os.environ, 'EVENT':'pull_request', 'BASE_REF':'master', 'BASE_SHA':base, 'HEAD_SHA':head,
                           'GITHUB_SHA':merged, 'GITHUB_REF':'refs/pull/1/merge', 'GITHUB_OUTPUT':str(out),
                           'GITHUB_STEP_SUMMARY':str(root/'summary'), 'RUNNER_TEMP':str(root)}
            def verify(**updates):
                return subprocess.run(['bash', '-c', self.scope_shell], cwd=repo, env={**environment, **updates}, capture_output=True)
            self.assertEqual(verify().returncode, 0)
            self.assertIn('runtime=false', out.read_text())
            self.assertNotEqual(verify(HEAD_SHA=base).returncode, 0)
            self.assertNotEqual(verify(GITHUB_REF='refs/heads/feature').returncode, 0)
            (repo/'later.md').write_text('base advanced\n'); git('add', '.'); git('commit', '-qm', 'later'); git('push', '-q', 'origin', 'master')
            git('checkout', '--detach', '-q', merged)
            self.assertNotEqual(verify().returncode, 0)


if __name__ == "__main__":
    unittest.main()
