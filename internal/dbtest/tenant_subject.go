package dbtest

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"

	"github.com/open-rails/openrails/pkg/tenant"
)

// EnsureTenantSubjectID materializes the tenant_subjects FK row for integration
// tests that insert tenant-owned rows directly instead of going through repos.
func EnsureTenantSubjectID(ctx context.Context, t testing.TB, db bun.IDB, userID string) uuid.UUID {
	t.Helper()
	userID = strings.TrimSpace(userID)
	require.NotEmpty(t, userID, "test user id must be set")

	if uid, err := uuid.Parse(userID); err == nil {
		var tenantSubjectID uuid.UUID
		err = db.NewRaw(
			`INSERT INTO billing.tenant_subjects (id, tenant_id, issuer, subject)
			 VALUES (?, ?, ?, ?)
			 ON CONFLICT (id) DO UPDATE SET last_seen_at = now()
			 RETURNING id`,
			uid,
			tenant.DefaultID.UUID(),
			"openrails:self",
			userID,
		).Scan(ctx, &tenantSubjectID)
		require.NoError(t, err, "ensure tenant subject")
		return tenantSubjectID
	}

	var tenantSubjectID uuid.UUID
	err := db.NewRaw(
		`INSERT INTO billing.tenant_subjects (tenant_id, issuer, subject)
		 VALUES (?, ?, ?)
		 ON CONFLICT (tenant_id, issuer, subject) DO UPDATE SET last_seen_at = now()
		 RETURNING id`,
		tenant.DefaultID.UUID(),
		"openrails:legacy-user",
		userID,
	).Scan(ctx, &tenantSubjectID)
	require.NoError(t, err, "ensure tenant subject")
	return tenantSubjectID
}
