package network

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/authkit/iam"
	"github.com/open-rails/openrails/internal/db"
	"github.com/stretchr/testify/require"
)

func TestAccountEventCursorScopeAndRetention(t *testing.T) {
	dsn := os.Getenv("SQLC_DATABASE_URL")
	if dsn == "" {
		t.Skip("SQLC_DATABASE_URL is required")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	require.NoError(t, err)
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := "network_events_" + uuid.New().String()[:8]
	_, err = pool.Exec(t.Context(), "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize())
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
	})
	_, err = pool.Exec(t.Context(), "CREATE TABLE "+pgx.Identifier{schema, "network_event_cursors"}.Sanitize()+" (issuer text, tenant_id uuid, cursor text NOT NULL DEFAULT '', updated_at timestamptz NOT NULL DEFAULT now(), PRIMARY KEY(issuer,tenant_id))")
	require.NoError(t, err)
	database, err := db.NewWithPGXPool(pool, schema)
	require.NoError(t, err)
	s := &Service{db: database, tenant: iam.NewTenantID(), issuer: "https://account.example"}
	cursor := func() string {
		var out string
		err := pool.QueryRow(t.Context(), "SELECT COALESCE((SELECT cursor FROM "+pgx.Identifier{schema, "network_event_cursors"}.Sanitize()+" WHERE issuer=$1 AND tenant_id=$2),'')", s.issuer, uuid.UUID(s.tenant)).Scan(&out)
		require.NoError(t, err)
		return out
	}
	count, err := s.pollEvents(t.Context(), func(_ context.Context, q iam.EventQuery) (iam.EventPage, error) {
		require.Empty(t, q.After)
		require.False(t, q.Authority)
		return iam.EventPage{NextCursor: "first"}, nil
	})
	require.NoError(t, err)
	require.Zero(t, count)
	require.Equal(t, "first", cursor())
	_, err = s.pollEvents(t.Context(), func(context.Context, iam.EventQuery) (iam.EventPage, error) {
		return iam.EventPage{}, iam.ErrEventCursorExpired
	})
	require.ErrorIs(t, err, iam.ErrEventCursorExpired)
	require.Equal(t, "first", cursor(), "retention loss never resets progress")
	_, err = s.pollEvents(t.Context(), func(context.Context, iam.EventQuery) (iam.EventPage, error) {
		return iam.EventPage{NextCursor: "foreign", Events: []iam.Event{{Version: 1, TenantID: iam.NewTenantID()}}}, nil
	})
	require.ErrorContains(t, err, "different account tenant")
	require.Equal(t, "first", cursor())
	_, err = s.pollEvents(t.Context(), func(context.Context, iam.EventQuery) (iam.EventPage, error) {
		return iam.EventPage{}, errors.New("source unavailable")
	})
	require.ErrorContains(t, err, "source unavailable")
	require.Equal(t, "first", cursor())
}
