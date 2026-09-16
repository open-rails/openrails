//go:build integration

package main

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/integrationharness"
)

func TestMain(m *testing.M) { dbtest.RunMain(m) }

func TestStandaloneExampleRuns(t *testing.T) {
	ctx := context.Background()
	server := integrationharness.New(t, ctx).StartStandalone("USD")
	env := map[string]string{
		"OPENRAILS_URL": server.BaseURL, "OPENRAILS_API_KEY": server.Token,
		"OPENRAILS_MERCHANT_ID": dbtest.TestMerchantID.String(),
	}
	require.NoError(t, run(ctx, func(key string) string { return env[key] }))
	env["OPENRAILS_MERCHANT_ID"] = uuid.NewString()
	require.ErrorIs(t, run(ctx, func(key string) string { return env[key] }), openrails.ErrConflict)
}
