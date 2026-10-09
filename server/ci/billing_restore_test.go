//go:build e2e && integration

package ci_test

import (
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/server"
	"github.com/open-rails/openrails/server/internal/operator"
)

// The group-bound preparation used by the hosted CLI checks real destination
// AuthKit authority. Neither the archive UUID nor a foreign owner can rebind it.
func TestBillingRestoreTargetUsesDestinationAuthority(t *testing.T) {
	t.Parallel()
	slug := uniqueName("restore")
	mid := newFixture(t).runtime(t, slug).MerchantID()
	target := newFixture(t)
	srv := target.newServer(t, func(cfg *server.Config, _ *server.Deps) {
		cfg.Engine.DB = &openrails.DBConfig{URL: strings.TrimSpace(os.Getenv("OPENRAILS_E2E_DSN"))}
	})
	auth := srv.AuthKit()
	_, cp := operator.Of(srv)
	owner, outsider := authtest.NewUser(t, auth), authtest.NewUser(t, auth)
	ownerSubject := iam.UserSubject(owner.ID)
	group, err := auth.CreateGroup(t.Context(), iam.NewGroup{ID: uuid.NewString(), Persona: operator.MerchantType, Owner: &ownerSubject})
	require.NoError(t, err)
	req := operator.ProvisionMerchantForRestoreRequest{MerchantID: mid, Slug: slug, ExistingGroupID: group.ID, OwnerUserID: outsider.ID}
	_, err = operator.ProvisionMerchantForRestore(t.Context(), cp, req)
	require.ErrorIs(t, err, iam.ErrInsufficientAuthority)
	req.OwnerUserID = owner.ID
	prepared, err := operator.ProvisionMerchantForRestore(t.Context(), cp, req)
	require.NoError(t, err)
	require.Equal(t, mid, prepared.MerchantID)
	require.Equal(t, group.ID, prepared.GroupID)
	require.True(t, prepared.Created)
	again, err := operator.ProvisionMerchantForRestore(t.Context(), cp, req)
	require.NoError(t, err)
	require.False(t, again.Created)
	otherGroup, err := auth.CreateGroup(t.Context(), iam.NewGroup{ID: uuid.NewString(), Persona: operator.MerchantType, Owner: &ownerSubject})
	require.NoError(t, err)
	req.ExistingGroupID = otherGroup.ID
	_, err = operator.ProvisionMerchantForRestore(t.Context(), cp, req)
	require.ErrorIs(t, err, merchants.ErrMerchantRestoreConflict)
}
