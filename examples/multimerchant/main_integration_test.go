//go:build integration

package main

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/integrationharness"
)

func TestMain(m *testing.M) { dbtest.RunMain(m) }

func TestMultiMerchantExampleRuns(t *testing.T) {
	ctx := context.Background()
	h := integrationharness.New(t, ctx)
	dbtest.EnsureTestMerchant(ctx, t, h.Pool())
	second := uuid.New()
	_, err := h.Pool().Exec(ctx, `INSERT INTO billing.merchants (id, slug, status) VALUES ($1, 'example-second', 'active')`, second)
	require.NoError(t, err)
	env := map[string]string{
		"OPENRAILS_DATABASE_URL": h.DSN,
		"OPENRAILS_MERCHANT_IDS": dbtest.TestMerchantID.String() + "," + second.String(),
	}
	require.NoError(t, run(ctx, func(key string) string { return env[key] }))
}
