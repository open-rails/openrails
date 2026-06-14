package dbtest

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/gen"
)

// EnsureMerchantSubjectIDPgx materializes the tenant_subjects FK row for
// integration tests that insert tenant-owned rows directly instead of going
// through repos. Callers pass the test app-DB's pool
// (dbtest.OpenAppDB(t, dsn).Pool()) or any gen.DBTX. Payable identities are
// UUID-only (#364), so the test user id must be a UUID — the row id IS the
// subject UUID.
func EnsureMerchantSubjectIDPgx(ctx context.Context, t testing.TB, qx gen.DBTX, userID string) uuid.UUID {
	t.Helper()
	userID = strings.TrimSpace(userID)
	require.NotEmpty(t, userID, "test user id must be set")

	uid, err := uuid.Parse(userID)
	require.NoError(t, err, "test user id must be a UUID (#364): %q", userID)

	// No default tenant (#336): materialize the canonical test tenant first so
	// the tenant_subjects FK resolves, then pin the subject under it.
	EnsureTestTenant(ctx, t, qx)
	id, err := gen.New(qx).UpsertSelfMerchantSubject(ctx, gen.UpsertSelfMerchantSubjectParams{
		ID:       uid,
		MerchantID: TestTenantID.UUID(),
		Issuer:   "openrails:self",
		Subject:  uid.String(),
	})
	require.NoError(t, err, "ensure tenant subject")
	return id
}
