//go:build integration

package credits_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"

	"github.com/open-rails/openrails/internal/dbtest"
)

// TestMain terminates the shared dbtest Postgres container after this package's
// tests so the container is closed after use even when the testcontainers Ryuk
// reaper is unavailable (offline/sandboxed runs).
func TestMain(m *testing.M) { dbtest.RunMain(m) }

// seedTenantSubject materializes the billing.tenant_subjects row a direct
// payable-row insert needs to satisfy the tenant_subject_id FK (migrations
// 076/078, #317). The id IS the payable subject id (the credits/self-service
// deterministic scheme, issuer openrails:self); tenant_id is the default tenant.
func seedTenantSubject(t *testing.T, ctx context.Context, db *bun.DB, tsID uuid.UUID) {
	t.Helper()
	_, err := db.ExecContext(ctx,
		`INSERT INTO billing.tenant_subjects (id, tenant_id, issuer, subject)
		 VALUES (?, '00000000-0000-0000-0000-000000000001', 'openrails:self', ?::text)
		 ON CONFLICT DO NOTHING`, tsID, tsID)
	require.NoError(t, err)
}
