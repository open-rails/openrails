//go:build integration

package controlplane

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/pkg/merchant"
)

func TestListActiveMerchantIDs(t *testing.T) {
	ctx := context.Background()
	super := dbtest.SharedSuperuserPGXPool(t)
	cp, err := New(ctx, &config.Config{
		Env:  "test",
		DB:   &config.DBConfig{},
		Auth: &config.AuthConfig{Issuer: "https://openrails.test", MintDisabled: true},
	}, super)
	require.NoError(t, err)

	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:10]
	activeA, activeB, deleted := uuid.New(), uuid.New(), uuid.New()
	_, err = super.Exec(ctx, `
		INSERT INTO openrails.merchants (id, slug, status, deleted_at)
		VALUES ($1, $2, 'active', NULL), ($3, $4, 'active', NULL), ($5, $6, 'deleted', now())`,
		activeA, "host-page-a-"+suffix,
		activeB, "host-page-b-"+suffix,
		deleted, "host-page-deleted-"+suffix,
	)
	require.NoError(t, err)

	ids, err := cp.ListActiveMerchantIDs(ctx, 200, 0)
	require.NoError(t, err)
	require.Contains(t, ids, merchant.ID(activeA))
	require.Contains(t, ids, merchant.ID(activeB))
	require.NotContains(t, ids, merchant.ID(deleted))
}
