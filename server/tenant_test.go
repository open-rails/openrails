package server_test

import (
	"context"
	"testing"

	"github.com/open-rails/openrails/server"
	"github.com/stretchr/testify/require"
)

func TestMissingPlatformTenantRefusesBeforeDatabaseBoot(t *testing.T) {
	_, err := server.New(context.Background(), server.Config{}, server.Deps{})
	require.ErrorContains(t, err, "auth.default_tenant_id (AUTH_DEFAULT_TENANT_ID)")
}
