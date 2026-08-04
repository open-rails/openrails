package dbtest

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/gen"
)

// EnsureCustomerIDPgx materializes the customers FK row for
// integration tests that insert merchant-owned rows directly instead of going
// through repos. Callers pass the test app-DB's pool
// (dbtest.OpenAppDB(t, dsn).Pool()) or any gen.DBTX. Payable identities are
// UUID-only (#364), so the test user id must be a UUID — the row id IS the
// subject UUID.
func EnsureCustomerIDPgx(ctx context.Context, t testing.TB, qx gen.DBTX, userID string) uuid.UUID {
	t.Helper()
	userID = strings.TrimSpace(userID)
	require.NotEmpty(t, userID, "test user id must be set")

	uid, err := uuid.Parse(userID)
	require.NoError(t, err, "test user id must be a UUID (#364): %q", userID)

	// No default merchant (#336): materialize the canonical test merchant first so
	// the customers FK resolves, then pin the customer under it.
	EnsureTestMerchant(ctx, t, qx)
	return ensureCustomerUnder(ctx, t, qx, TestMerchantID.UUID(), uid, userID)
}

// EnsureCustomerIDPgxFor is EnsureCustomerIDPgx under an EXPLICIT merchant, for
// tests that own a merchant of their own rather than sharing the canonical one.
// customers is RLS-forced, so a handle pinned to merchant X cannot materialize a
// customer under the canonical test merchant — the WITH CHECK refuses it. The
// caller is expected to have created the merchant row already.
func EnsureCustomerIDPgxFor(ctx context.Context, t testing.TB, qx gen.DBTX, merchantID uuid.UUID, userID string) uuid.UUID {
	t.Helper()
	userID = strings.TrimSpace(userID)
	require.NotEmpty(t, userID, "test user id must be set")
	uid, err := uuid.Parse(userID)
	require.NoError(t, err, "test user id must be a UUID (#364): %q", userID)
	return ensureCustomerUnder(ctx, t, qx, merchantID, uid, userID)
}

func ensureCustomerUnder(ctx context.Context, t testing.TB, qx gen.DBTX, merchantID, uid uuid.UUID, subject string) uuid.UUID {
	t.Helper()
	id, err := gen.New(qx).EnsureCustomer(ctx, gen.EnsureCustomerParams{
		ID:         uid,
		MerchantID: merchantID,
		Subject:    &subject,
	})
	require.NoError(t, err, "ensure customer")
	return id
}
