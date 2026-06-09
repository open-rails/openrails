package dbtest

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"

	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/tenant"
)

// EnsureTenantSubjectID materializes the tenant_subjects FK row for integration
// tests that insert tenant-owned rows directly instead of going through repos.
func EnsureTenantSubjectID(ctx context.Context, t testing.TB, db bun.IDB, userID string) uuid.UUID {
	t.Helper()
	tenantSubjectID := identity.TenantSubjectIDFromString(userID).UUID()
	require.NotEqual(t, uuid.Nil, tenantSubjectID, "test user id must be a UUID tenant subject")
	_, err := db.NewRaw(
		`INSERT INTO billing.tenant_subjects (id, tenant_id, issuer, subject)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT (id) DO UPDATE SET last_seen_at = now()`,
		tenantSubjectID,
		tenant.DefaultID.UUID(),
		"openrails:self",
		userID,
	).Exec(ctx)
	require.NoError(t, err, "ensure tenant subject")
	return tenantSubjectID
}
