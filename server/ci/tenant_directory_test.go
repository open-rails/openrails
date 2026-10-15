//go:build e2e && integration

package ci_test

import (
	"context"
	"testing"

	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	"github.com/stretchr/testify/require"
)

func TestStandaloneAccountTenantSurvivesRestart(t *testing.T) {
	f := newFixture(t)
	first := f.newServer(t, nil)
	user := newAccount(t, first)
	token := authtest.SignIn(t, first.AuthKit(), user)
	require.NotEmpty(t, token.AccessToken)
	profile, err := first.AuthKit().User(t.Context(), iam.UserByID(user.ID))
	require.NoError(t, err)
	require.Equal(t, f.tenantID(), profile.TenantID)
	require.NoError(t, first.Close(context.Background()))

	restarted := f.newServer(t, nil)
	token = authtest.SignIn(t, restarted.AuthKit(), user)
	require.NotEmpty(t, token.AccessToken)
	profile, err = restarted.AuthKit().User(t.Context(), iam.UserByID(user.ID))
	require.NoError(t, err)
	require.Equal(t, user.ID, profile.ID)
	require.Equal(t, f.tenantID(), profile.TenantID)
}
