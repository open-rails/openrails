//go:build integration

package main

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/integrationharness"
)

func TestMain(m *testing.M) { dbtest.RunMain(m) }

func TestEmbeddedExampleRuns(t *testing.T) {
	ctx := context.Background()
	h := integrationharness.New(t, ctx)
	env := map[string]string{"OPENRAILS_DATABASE_URL": h.DSN, "OPENRAILS_MERCHANT": "example-embedded"}
	require.NoError(t, run(ctx, func(key string) string { return env[key] }))
	require.Error(t, run(ctx, func(string) string { return "" }))
}
