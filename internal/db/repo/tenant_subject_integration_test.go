//go:build integration

package repo

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/pkg/tenant"
)

func TestEnsureTenantSubjectID_UUIDReusesExistingPayableID(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.SharedPGXPool(t)

	tenantID := tenant.DefaultID.UUID()
	userID := uuid.New()
	createdAt := time.Now().UTC().Add(-time.Hour)
	_, err := pool.Exec(ctx,
		`INSERT INTO billing.tenant_subjects (id, tenant_id, issuer, subject, created_at, last_seen_at)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		userID, tenantID, "http://doujins:2052", userID.String(), createdAt, createdAt,
	)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM billing.tenant_subjects WHERE id = $1`, userID)
	})

	resolved, err := EnsureTenantSubjectID(ctx, pool, tenantID, userID.String())
	require.NoError(t, err)
	require.Equal(t, userID, resolved)

	var issuer string
	var lastSeenAt time.Time
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT issuer, last_seen_at FROM billing.tenant_subjects WHERE id = $1`, userID,
	).Scan(&issuer, &lastSeenAt))
	require.Equal(t, "http://doujins:2052", issuer)
	require.True(t, lastSeenAt.After(createdAt))
}
