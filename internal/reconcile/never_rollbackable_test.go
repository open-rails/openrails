package reconcile

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// or#859 §4: the never-rollbackable register, enforced against the code rather
// than asserted in prose.
//
// The rule is narrow and therefore checkable: NO QUERY ON AN UNDO PATH MAY WRITE
// A REGISTERED TABLE. It is deliberately not "nothing writes these tables" —
// findings advance, runs finish, the retention sweep expires dedup rows, and
// those are ordinary forward writes. What must never happen is a REVERSAL
// reaching into the money spine, the grant log, the lifecycle trail or the
// record of what we did to the outside world.
//
// The check walks the undo implementations, collects the sqlc queries they call,
// and reads the SQL those names resolve to. Adding a query to an undo path
// therefore puts it under this guard automatically — the failure mode a
// hand-maintained list has is the one this closes.

// undoImplementations are the files that perform or plan a reversal.
var undoImplementations = []string{
	"destructive_run_undo.go",
	"destructive_run_converge.go",
	"prune.go", // RollbackDestructiveRun lives here alongside the prune itself
}

var (
	genCallRe   = regexp.MustCompile(`\b(?:tq|q|database\.Gen\(ctx\)|r\.DB\.Gen\(ctx\)|appDB\.Gen\(ctx\))\.([A-Z]\w+)\(ctx`)
	queryNameRe = regexp.MustCompile(`(?m)^--\s*name:\s*(\w+)\s`)
	writeStmtRe = regexp.MustCompile(`(?is)\b(INSERT\s+INTO|UPDATE|DELETE\s+FROM)\s+openrails\.(\w+)`)
)

// loadQueryBodies indexes every sqlc query in internal/db/queries by name.
func loadQueryBodies(t *testing.T) map[string]string {
	t.Helper()
	dir := filepath.Join("..", "db", "queries")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read query dir: %v", err)
	}
	bodies := map[string]string{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		text := string(raw)
		locs := queryNameRe.FindAllStringSubmatchIndex(text, -1)
		for i, loc := range locs {
			name := text[loc[2]:loc[3]]
			end := len(text)
			if i+1 < len(locs) {
				end = locs[i+1][0]
			}
			bodies[name] = text[loc[1]:end]
		}
	}
	if len(bodies) < 100 {
		t.Fatalf("query index looks wrong: found only %d queries", len(bodies))
	}
	return bodies
}

func TestNeverRollbackableRegisterIsEnforcedOnEveryUndoPath(t *testing.T) {
	bodies := loadQueryBodies(t)

	checked := 0
	for _, file := range undoImplementations {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		for _, m := range genCallRe.FindAllStringSubmatch(string(raw), -1) {
			name := m[1]
			body, ok := bodies[name]
			if !ok {
				continue // not a sqlc query (a helper with the same call shape)
			}
			checked++
			for _, w := range writeStmtRe.FindAllStringSubmatch(body, -1) {
				verb, table := strings.ToUpper(strings.Fields(w[1])[0]), w[2]
				if reason, forbidden := NeverRollbackableTables[table]; forbidden {
					t.Errorf("undo path %s calls %s, which %ss openrails.%s — that table is never rolled back: %s",
						file, name, verb, table, reason)
				}
			}
		}
	}
	// A scan that matched nothing would pass silently, which is the one result
	// this test must never produce.
	if checked < 10 {
		t.Fatalf("the undo-path scan resolved only %d sqlc queries; the call pattern has drifted and this guard is not checking anything", checked)
	}
}

// TestSupersedeIsTheOnlyRailIntentWriteOnAnUndoPath pins the single deliberate
// exception. rail_intents is Class A in the spec and absent from the register
// because moving a queued row to `superseded` is a FORWARD lifecycle transition,
// not a rollback of an append-only log — it is how an undo neutralises a
// provider write that has not fired. The line that keeps it an exception rather
// than a hole is the status predicate: only unfired rows, never a delete, never
// a rewrite of one that already executed.
func TestSupersedeIsTheOnlyRailIntentWriteOnAnUndoPath(t *testing.T) {
	bodies := loadQueryBodies(t)

	body, ok := bodies["SupersedeUnfiredRailIntentsForRun"]
	if !ok {
		t.Fatal("SupersedeUnfiredRailIntentsForRun is gone; the undo can no longer neutralise queued provider writes")
	}
	if !strings.Contains(body, "status IN ('pending', 'failed_retryable')") {
		t.Errorf("the supersede must be confined to UNFIRED intents; an in_flight or succeeded row may already have reached the provider:\n%s", body)
	}
	for _, w := range writeStmtRe.FindAllStringSubmatch(body, -1) {
		if strings.HasPrefix(strings.ToUpper(w[1]), "DELETE") {
			t.Error("an intent is transitioned, never deleted: it is the record of what we did to the outside world")
		}
	}

	// Nothing on an undo path may DELETE from rail_intents.
	for _, file := range undoImplementations {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		for _, m := range genCallRe.FindAllStringSubmatch(string(raw), -1) {
			b, ok := bodies[m[1]]
			if !ok {
				continue
			}
			for _, w := range writeStmtRe.FindAllStringSubmatch(b, -1) {
				if w[2] == "rail_intents" && strings.HasPrefix(strings.ToUpper(w[1]), "DELETE") {
					t.Errorf("undo path %s calls %s, which DELETEs rail_intents — %s", file, m[1], railIntentsForwardOnlyReason)
				}
			}
		}
	}
}

// TestRunKindsAreClassifiedExactlyOnce: every kind the ledger can hold is either
// reversible, registered as unrecoverable, or refused as unconverted. A kind
// that fell through all three would be reversed by a code path that does not
// know what it is undoing.
func TestRunKindsAreClassifiedExactlyOnce(t *testing.T) {
	for kind := range ReversibleRunKinds {
		if _, dup := UnrecoverableRunKinds[kind]; dup {
			t.Errorf("kind %q is both reversible and unrecoverable", kind)
		}
		if err := classifyRunKind(kind); err != nil {
			t.Errorf("kind %q is reversible but classifyRunKind refused it: %v", kind, err)
		}
	}
	for kind, why := range UnrecoverableRunKinds {
		err := classifyRunKind(kind)
		if err == nil {
			t.Errorf("kind %q is unrecoverable but classifyRunKind allowed it", kind)
			continue
		}
		if !strings.Contains(err.Error(), why[:20]) {
			t.Errorf("the refusal for %q must carry its reason, got: %v", kind, err)
		}
	}
	// The kinds the schema declares but nobody has converted yet.
	for _, kind := range []string{"declared_import", "plan_migration", "catalog_push"} {
		err := classifyRunKind(kind)
		if err == nil || !strings.Contains(err.Error(), "not yet converted") {
			t.Errorf("unconverted kind %q must be refused by name, got: %v", kind, err)
		}
	}
}
