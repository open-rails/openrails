//go:build cgo

package sqlaudit

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/internal/merchantarchive"
	"github.com/open-rails/openrails/internal/modules/metrics"
)

const (
	genDir    = "../gen"
	allowPath = "../queries/AUDIT_ALLOWLIST.txt"
)

// TestQueryAudit is the CI gate. It needs the same throwaway vet database
// `sqlc vet` uses: export SQLC_DATABASE_URL="$(scripts/sqlc-vet-db.sh)", or run
// `task sqlc-check`.
func TestQueryAudit(t *testing.T) {
	url := os.Getenv("SQLC_DATABASE_URL")
	if url == "" {
		t.Skip("SQLC_DATABASE_URL unset — run `task sqlc-check` (builds the vet DB from migrations/)")
	}
	ctx := context.Background()

	queries, err := LoadQueries(genDir)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)

	cat, err := LoadCatalog(ctx, conn)
	if err != nil {
		t.Fatal(err)
	}
	// Beyond sqlc: stored function and trigger bodies, metrics compiled from
	// the registry, and the archive's per-table statements.
	functions, err := LoadFunctionQueries(ctx, conn)
	if err != nil {
		t.Fatal(err)
	}
	queries = append(queries, functions...)
	compiled, err := metrics.AuditStatements()
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range compiled {
		queries = append(queries, Query{Name: st.Name, Kind: "metrics", SQL: st.SQL, File: "internal/modules/metrics"})
	}
	for _, st := range merchantarchive.AuditStatements() {
		queries = append(queries, Query{Name: st.Name, Kind: "archive", SQL: st.SQL, File: "internal/merchantarchive"})
	}
	if err := PrepareSession(ctx, conn); err != nil {
		t.Fatal(err)
	}
	allow, err := LoadAllowlist(allowPath)
	if err != nil {
		t.Fatal(err)
	}

	// The test binary is the env boundary (#712); the package itself reads none.
	advisor := AdvisorConn(ctx, url, os.Getenv("SQLAUDIT_INDEX_ADVISOR") == "1")
	if advisor != nil {
		defer advisor.Close(ctx)
	}

	rep, err := Run(ctx, conn, advisor, queries, cat, allow)
	if err != nil {
		t.Fatal(err)
	}
	t.Log(rep.Census())
	if !rep.OK() {
		t.Fatalf("query audit failed — fix the query, or add a reviewed line to %s and its rationale to internal/db/queries/EXEMPTIONS.md\n%s", allowPath, rep)
	}
}
