package dbtest

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/pkg/tenant"
)

// EnsureTenantSubjectIDPgx materializes the tenant_subjects FK row for
// integration tests that insert tenant-owned rows directly instead of going
// through repos. Callers pass the test app-DB's pool
// (dbtest.OpenAppDB(t, dsn).Pool()) or any gen.DBTX.
func EnsureTenantSubjectIDPgx(ctx context.Context, t testing.TB, qx gen.DBTX, userID string) uuid.UUID {
	t.Helper()
	userID = strings.TrimSpace(userID)
	require.NotEmpty(t, userID, "test user id must be set")

	q := gen.New(qx)
	if uid, err := uuid.Parse(userID); err == nil {
		id, err := q.UpsertSelfTenantSubject(ctx, gen.UpsertSelfTenantSubjectParams{
			ID:       uid,
			TenantID: tenant.DefaultID.UUID(),
			Issuer:   "openrails:self",
			Subject:  userID,
		})
		require.NoError(t, err, "ensure tenant subject")
		return id
	}

	id, err := q.UpsertLegacyTenantSubject(ctx, gen.UpsertLegacyTenantSubjectParams{
		TenantID: tenant.DefaultID.UUID(),
		Issuer:   "openrails:legacy-user",
		Subject:  userID,
	})
	require.NoError(t, err, "ensure tenant subject")
	return id
}
