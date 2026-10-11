//go:build e2e && integration

package ci_test

import (
	"os"
	"strings"
	"testing"

	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/server"
	"github.com/open-rails/openrails/server/internal/operator"
)

// The group-bound preparation used by the hosted CLI checks real destination
// AuthKit authority: the merchant's group is the one whose id is the
// merchant's, and only its live owner prepares it. Neither the archive nor a
// foreign owner binds it.
func TestBillingRestoreTargetUsesDestinationAuthority(t *testing.T) {
	t.Parallel()
	slug := uniqueName("restore")
	_, source := newFixture(t).runtime(t, slug)
	mid := source.MerchantID()
	target := newFixture(t)
	srv := target.newServer(t, func(cfg *server.Config, _ *server.Deps) {
		cfg.Engine.DB = &openrails.DBConfig{URL: strings.TrimSpace(os.Getenv("OPENRAILS_E2E_DSN"))}
	})
	auth := srv.AuthKit()
	_, cp := operator.Of(srv)
	owner, outsider := authtest.NewUser(t, auth), authtest.NewUser(t, auth)
	req := operator.ProvisionMerchantForRestoreRequest{MerchantID: mid, Slug: slug, OwnerUserID: owner.ID}
	_, err := operator.ProvisionMerchantForRestore(t.Context(), cp, req)
	require.ErrorIs(t, err, iam.ErrGroupNotFound, "the destination group comes first")
	ownerSubject := iam.UserSubject(owner.ID)
	_, err = auth.CreateGroup(t.Context(), iam.NewGroup{ID: mid.String(), Persona: operator.MerchantType, Owner: &ownerSubject})
	require.NoError(t, err)
	_, err = operator.ProvisionMerchantForRestore(t.Context(), cp, operator.ProvisionMerchantForRestoreRequest{MerchantID: mid, Slug: slug, OwnerUserID: outsider.ID})
	require.ErrorIs(t, err, iam.ErrInsufficientAuthority)
	prepared, err := operator.ProvisionMerchantForRestore(t.Context(), cp, req)
	require.NoError(t, err)
	require.Equal(t, mid, prepared.MerchantID)
	require.Equal(t, mid.String(), prepared.GroupID)
	require.True(t, prepared.Created)
	again, err := operator.ProvisionMerchantForRestore(t.Context(), cp, req)
	require.NoError(t, err)
	require.False(t, again.Created)
	scope, err := srv.MerchantScope(t.Context(), mid)
	require.NoError(t, err)
	require.Equal(t, mid.String(), scope.ID, "the restored merchant's staff hold their permissions in its group")
}
