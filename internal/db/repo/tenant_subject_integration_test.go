//go:build integration

package repo

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/dbtest"
)

func TestEnsureMerchantSubjectID_UUIDReusesExistingPayableID(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.SharedPGXPool(t)

	dbtest.EnsureTestTenant(ctx, t, pool)
	tenantID := dbtest.TestTenantID.UUID()
	userID := uuid.New()
	createdAt := time.Now().UTC().Add(-time.Hour)
	_, err := pool.Exec(ctx,
		`INSERT INTO openrails.merchant_subjects (id, merchant_id, issuer, subject, created_at, last_seen_at)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		userID, tenantID, "http://doujins:2052", userID.String(), createdAt, createdAt,
	)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM openrails.merchant_subjects WHERE id = $1`, userID)
	})

	resolved, err := EnsureMerchantSubjectID(ctx, pool, tenantID, userID.String())
	require.NoError(t, err)
	require.Equal(t, userID, resolved)

	var issuer string
	var lastSeenAt time.Time
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT issuer, last_seen_at FROM openrails.merchant_subjects WHERE id = $1`, userID,
	).Scan(&issuer, &lastSeenAt))
	require.Equal(t, "http://doujins:2052", issuer)
	require.True(t, lastSeenAt.After(createdAt))
}

// Payable identities are UUID-only (#364): non-UUID subjects are rejected, the
// empty subject stays a documented no-op.
func TestEnsureMerchantSubjectID_RejectsNonUUIDSubject(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.SharedPGXPool(t)

	_, err := EnsureMerchantSubjectID(ctx, pool, dbtest.TestTenantID.UUID(), "legacy-user-123")
	require.ErrorContains(t, err, "UUID-only")

	id, err := EnsureMerchantSubjectID(ctx, pool, dbtest.TestTenantID.UUID(), "")
	require.NoError(t, err)
	require.Equal(t, uuid.Nil, id)
}
