// Package migrationprefix holds the behaviour proof for
// scripts/migration-prefix-collision.sh (or#919, ported from tensorhub
// th#1790).
//
// Every case below builds REAL git repositories in temp dirs — an "origin" that
// owns the numbers and a clone that plays the lane — and runs the ACTUAL script
// against them over a real `git fetch`. Nothing here mocks git, the remote, or
// the diff, because the property under test is precisely that the script goes
// to the network for master rather than trusting the tree it was handed.
//
// Detection is never inlined in these tests. tensorhub's original guard test
// carried its own copy of the detection logic and had been green since the day
// it was written — which is indistinguishable from having quietly stopped
// checking, because the real tree is never dirty. These tests drive the real
// script over hand-built duplicates and assert it goes RED.
package migrationprefix

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// scriptPath resolves scripts/migration-prefix-collision.sh from this package.
func scriptPath(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	abs, err := filepath.Abs(filepath.Join(wd, "..", "..", "scripts", "migration-prefix-collision.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(abs); err != nil {
		t.Fatalf("guard script not found at %s: %v", abs, err)
	}
	return abs
}

// cleanEnv is os.Environ() with every GITHUB_* variable stripped. Without it the
// tests inherit the runner's own GITHUB_STEP_SUMMARY and append their fixture
// verdicts to the real job summary — and any future event-keyed branch in the
// script would read the runner's event instead of the fixture's. A test whose
// behaviour depends on where it runs is not evidence.
func cleanEnv(extra ...string) []string {
	out := make([]string, 0, len(os.Environ())+len(extra))
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "GITHUB_") {
			continue
		}
		out = append(out, kv)
	}
	return append(out, extra...)
}

type repo struct {
	t   *testing.T
	dir string
}

func (r *repo) git(args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = r.dir
	cmd.Env = cleanEnv(
		"GIT_CONFIG_NOSYSTEM=1",
		"HOME="+r.dir,
		"GIT_AUTHOR_DATE=2026-08-11T04:10:00Z",
		"GIT_COMMITTER_DATE=2026-08-11T04:10:00Z",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func (r *repo) configure() {
	r.git("config", "user.email", "prefix@test.invalid")
	r.git("config", "user.name", "migration prefix test")
	r.git("config", "commit.gpgsign", "false")
}

// migration writes a migration file with a plausible body. Content is
// irrelevant to this gate — it reads names — but a real file keeps the fixture
// honest about what the tree looks like.
func (r *repo) migration(name string) {
	r.t.Helper()
	p := filepath.Join(r.dir, "migrations", "postgres", name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("SELECT 1;\n"), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

func (r *repo) write(rel, body string) {
	r.t.Helper()
	p := filepath.Join(r.dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

func (r *repo) commit(msg string) {
	r.t.Helper()
	r.git("add", "-A")
	r.git("commit", "-q", "--allow-empty", "-m", msg)
}

// installScript copies the real script into the repo under test. The script
// resolves the repository from its own location (the house pattern), so it has
// to live inside the tree it is judging.
func (r *repo) installScript() {
	r.t.Helper()
	body, err := os.ReadFile(scriptPath(r.t))
	if err != nil {
		r.t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(r.dir, "scripts"), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.dir, "scripts", "migration-prefix-collision.sh"), body, 0o755); err != nil {
		r.t.Fatal(err)
	}
}

// newOrigin builds the repository that OWNS the numbers, with master holding
// the given migrations.
func newOrigin(t *testing.T, migrations ...string) *repo {
	t.Helper()
	r := &repo{t: t, dir: filepath.Join(t.TempDir(), "origin")}
	if err := os.MkdirAll(r.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	r.git("init", "-q", "-b", "master")
	r.configure()
	for _, m := range migrations {
		r.migration(m)
	}
	r.commit("master base")
	return r
}

// clone plays the lane: branched from master at some earlier point, and — the
// whole point — never fetching again on its own.
func clone(t *testing.T, origin *repo) *repo {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "lane")
	cmd := exec.Command("git", "clone", "-q", origin.dir, dir)
	cmd.Env = cleanEnv("GIT_CONFIG_NOSYSTEM=1", "HOME="+dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("clone: %v\n%s", err, out)
	}
	r := &repo{t: t, dir: dir}
	r.configure()
	r.git("checkout", "-q", "-b", "lane")
	r.installScript()
	return r
}

type result struct {
	out  string
	code int
}

func (r *repo) run(env ...string) result {
	r.t.Helper()
	cmd := exec.Command("bash", filepath.Join(r.dir, "scripts", "migration-prefix-collision.sh"))
	cmd.Dir = r.dir
	cmd.Env = cleanEnv(append([]string{
		"GIT_CONFIG_NOSYSTEM=1",
		"HOME=" + r.dir,
	}, env...)...)
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		r.t.Fatalf("running the guard: %v\n%s", err, out)
	}
	return result{out: string(out), code: code}
}

func (res result) mustContain(t *testing.T, needles ...string) {
	t.Helper()
	for _, n := range needles {
		if !strings.Contains(res.out, n) {
			t.Errorf("output does not mention %q:\n%s", n, res.out)
		}
	}
}

// TestLiveIncidentReplay is the or#908/or#911 outage as it actually happened:
// the lane branched when 0004 was the tip and drafted its own 0005; while it
// sat open, the sibling merged ITS 0005 to master. Both PRs read CLEAN. The
// point of the case is that the lane repository is never told — the script has
// to go and find out.
func TestLiveIncidentReplay(t *testing.T) {
	origin := newOrigin(t,
		"0001_schema.up.sql",
		"0004_grants_credit_deposit_once.up.sql",
	)
	lane := clone(t, origin)

	lane.migration("0005_spend_delegation_provenance.up.sql")
	lane.commit("or#911: spend delegation provenance")

	// Green before the sibling lands — the state every collided PR was in.
	if res := lane.run(); res.code != 0 {
		t.Fatalf("expected a clean verdict before the sibling merged, got %d:\n%s", res.code, res.out)
	}

	// The sibling merges its 0005 to master. Nothing touches the lane.
	origin.migration("0005_customer_business_profiles.up.sql")
	origin.commit("or#908: customer business profiles")

	res := lane.run()
	if res.code == 0 {
		t.Fatalf("the guard passed a tree that would take master's boot down:\n%s", res.out)
	}
	res.mustContain(t,
		"prefix 0005",
		"migrations/postgres/0005_spend_delegation_provenance.up.sql",
		"migrations/postgres/0005_customer_business_profiles.up.sql",
		"git rebase origin/master",
	)
}

// TestFreeNumberPasses: the ordinary case. A lane that drafted the next free
// number, on a master that has not moved into it, is not this job's business.
func TestFreeNumberPasses(t *testing.T) {
	origin := newOrigin(t, "0001_schema.up.sql", "0005_customer_business_profiles.up.sql")
	lane := clone(t, origin)
	lane.migration("0006_spend_delegation_provenance.up.sql")
	lane.commit("or#911: spend delegation provenance, renumbered")

	res := lane.run()
	if res.code != 0 {
		t.Fatalf("a free number was reported as a collision (%d):\n%s", res.code, res.out)
	}
	res.mustContain(t, "no number in this tree is claimed by a different file")
}

// TestNoMigrationsPassesTrivially: the overwhelming majority of PRs. A tree
// carrying no migration files at all must not need master resolved to say so —
// this is the case that must stay cheap while it runs on every open PR.
func TestNoMigrationsPassesTrivially(t *testing.T) {
	origin := newOrigin(t)
	lane := clone(t, origin)
	lane.write("README.md", "a change that has nothing to do with migrations\n")
	lane.commit("docs")

	// Point the remote at nothing at all: if the fast path is real, the script
	// never reaches the network and this still passes.
	lane.git("remote", "set-url", "origin", filepath.Join(lane.dir, "does-not-exist"))

	res := lane.run()
	if res.code != 0 {
		t.Fatalf("a PR with no migrations was not a trivial pass (%d):\n%s", res.code, res.out)
	}
	res.mustContain(t, "no migrations in this tree")
}

// TestUnchangedMigrationsPass: a PR that touches no migration at all, in a repo
// that HAS them, and where master has since gained one. Every number the tree
// holds is master's own, so there is nothing to report — the second-most common
// shape, and the one a naive "compare the sets" check gets wrong.
func TestUnchangedMigrationsPass(t *testing.T) {
	origin := newOrigin(t, "0001_schema.up.sql", "0004_grants_credit_deposit_once.up.sql")
	lane := clone(t, origin)
	lane.commit("or#919: no migrations here")

	origin.migration("0005_customer_business_profiles.up.sql")
	origin.commit("sibling merge")

	if res := lane.run(); res.code != 0 {
		t.Fatalf("a migration-free change was failed (%d):\n%s", res.code, res.out)
	}
}

// TestUpDownPairIsOneMigration: up and down are two FILES and one migration.
// They share a ledger key legally, so a lane adding the missing `.down.sql` for
// a migration master already has must not read as a second claim on the number.
func TestUpDownPairIsOneMigration(t *testing.T) {
	origin := newOrigin(t, "0001_schema.up.sql", "0005_customer_business_profiles.up.sql")
	lane := clone(t, origin)
	lane.migration("0005_customer_business_profiles.down.sql")
	lane.migration("0006_spend_delegation_provenance.up.sql")
	lane.migration("0006_spend_delegation_provenance.down.sql")
	lane.commit("or#911: spend delegation provenance, both directions")

	res := lane.run()
	if res.code != 0 {
		t.Fatalf("an up/down pair was mistaken for a duplicate claim (%d):\n%s", res.code, res.out)
	}
}

// TestDuplicatePrefixRepairPasses is hotfix PR #278's exact shape: master's own
// chain already holds one number twice (or#908 and or#911 merged it from stale
// bases) and this change is the repair, renaming one of them to the next free
// number. The file that KEEPS its number is still one of master's own files, so
// a check written as "master's slug differs from mine" would fail the very
// change that fixes master. This is why the rule is set membership.
func TestDuplicatePrefixRepairPasses(t *testing.T) {
	origin := newOrigin(t,
		"0001_schema.up.sql",
		"0005_customer_business_profiles.up.sql",
		"0005_spend_delegation_provenance.up.sql",
	)
	lane := clone(t, origin)
	lane.git("mv",
		"migrations/postgres/0005_spend_delegation_provenance.up.sql",
		"migrations/postgres/0006_spend_delegation_provenance.up.sql")
	lane.commit("hotfix: repair the duplicate 0005 (the PR #278 shape)")

	res := lane.run()
	if res.code != 0 {
		t.Fatalf("the duplicate-prefix REPAIR was reported as a collision (%d):\n%s", res.code, res.out)
	}
}

// TestUnreachableMasterIsUnverified: the soft-fail contract. This job runs on
// every open PR, so a network or permissions problem must never turn into a red
// tick on somebody else's unrelated change — but it must not quietly claim a
// pass either. UNVERIFIED, loudly, exit 0.
func TestUnreachableMasterIsUnverified(t *testing.T) {
	origin := newOrigin(t, "0001_schema.up.sql", "0005_customer_business_profiles.up.sql")
	lane := clone(t, origin)
	lane.migration("0006_spend_delegation_provenance.up.sql")
	lane.commit("or#911: spend delegation provenance")
	lane.git("remote", "set-url", "origin", filepath.Join(lane.dir, "gone"))

	summaryPath := filepath.Join(t.TempDir(), "summary.md")
	res := lane.run("GITHUB_STEP_SUMMARY=" + summaryPath)
	if res.code != 0 {
		t.Fatalf("an unreachable remote failed the PR (%d) instead of reporting UNVERIFIED:\n%s", res.code, res.out)
	}
	res.mustContain(t, "UNVERIFIED", "::warning title=migration prefix UNVERIFIED::", "could not fetch origin/master")

	body, err := os.ReadFile(summaryPath)
	if err != nil {
		t.Fatalf("no job summary written: %v", err)
	}
	if !strings.Contains(string(body), "UNVERIFIED") {
		t.Errorf("the job summary does not say UNVERIFIED, so a reader sees only a green tick:\n%s", body)
	}
}

// TestChainSquashIsNotApplicable: a chain squash (or#893 is this repo's
// precedent) rewrites every number by design, so comparing numbers with master
// answers a question nobody asked. Reviewing the squash is the PR reviewer's
// job; this gate stands down rather than adding a second opinion.
func TestChainSquashIsNotApplicable(t *testing.T) {
	origin := newOrigin(t,
		"0001_schema.up.sql",
		"0002_drop_credit_purchase_round.up.sql",
		"0005_customer_business_profiles.up.sql",
	)
	lane := clone(t, origin)
	for _, m := range []string{"0001_schema.up.sql", "0002_drop_credit_purchase_round.up.sql", "0005_customer_business_profiles.up.sql"} {
		if err := os.Remove(filepath.Join(lane.dir, "migrations", "postgres", m)); err != nil {
			t.Fatal(err)
		}
	}
	lane.migration("0001_squashed.up.sql")
	lane.commit("chain squash\n\nChain-squash: 0001_squashed.up.sql")

	res := lane.run()
	if res.code != 0 {
		t.Fatalf("a chain squash was reported as a collision (%d):\n%s", res.code, res.out)
	}
	res.mustContain(t, "chain-squash shape")
}

// TestCollisionSummaryNamesBothFiles: the failure has to be actionable from the
// PR page alone. Whoever reads it is mid-merge and needs the number and the
// file on master, not a pointer to a log.
func TestCollisionSummaryNamesBothFiles(t *testing.T) {
	origin := newOrigin(t, "0001_schema.up.sql")
	lane := clone(t, origin)
	lane.migration("0002_drop_credit_purchase_round.up.sql")
	lane.commit("lane 0002")
	origin.migration("0002_sibling_landed_first.up.sql")
	origin.commit("sibling merge")

	summaryPath := filepath.Join(t.TempDir(), "summary.md")
	res := lane.run("GITHUB_STEP_SUMMARY=" + summaryPath)
	if res.code == 0 {
		t.Fatalf("collision not detected:\n%s", res.out)
	}
	body, err := os.ReadFile(summaryPath)
	if err != nil {
		t.Fatalf("no job summary written: %v", err)
	}
	for _, want := range []string{"COLLISION", "0002", "0002_drop_credit_purchase_round.up.sql", "0002_sibling_landed_first.up.sql"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("job summary omits %q:\n%s", want, body)
		}
	}
}

// TestVerdictNamesTheShaItMeasured: the verdict is only as fresh as the fetch it
// made, so it has to say which master it read. Without that a green tick is
// unfalsifiable after the fact.
func TestVerdictNamesTheShaItMeasured(t *testing.T) {
	origin := newOrigin(t, "0001_schema.up.sql")
	lane := clone(t, origin)
	lane.migration("0002_drop_credit_purchase_round.up.sql")
	lane.commit("lane 0002")
	origin.commit("master moves without touching migrations")

	sha := origin.git("rev-parse", "HEAD")
	res := lane.run()
	if res.code != 0 {
		t.Fatalf("unexpected failure (%d):\n%s", res.code, res.out)
	}
	res.mustContain(t, sha[:12])
}
