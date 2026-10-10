//go:build e2e && integration

package ci_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/open-rails/authkit/iam"
	helpersauth "github.com/open-rails/helpers/auth"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/staffperm"
	"github.com/open-rails/openrails/server/internal/controlplane"
	"github.com/stretchr/testify/require"
)

// ListUserMerchants is a hosted product's merchant list: the live merchants a
// user holds a role in, by current name, with the user's role in each.
func TestUserMerchantsListing(t *testing.T) {
	f := newFixture(t)
	cp := f.newServer(t, reserving())
	ctx := t.Context()
	member, memberToken := newOwner(t, cp)
	owner, ownerToken := newOwner(t, cp)
	u, err := cp.AuthKit().User(ctx, iam.UserByID(member))
	require.NoError(t, err)

	own := uniqueName("a-own")
	mine, err := cp.ProvisionMerchant(ctx, billing.ProvisionMerchantParams{Slug: own, DisplayName: "Own Shop", OwnerUserID: member})
	require.NoError(t, err)
	viewed := uniqueName("b-viewed")
	theirs, err := cp.ProvisionMerchant(ctx, billing.ProvisionMerchantParams{Slug: viewed, OwnerUserID: owner})
	require.NoError(t, err)
	added, err := cp.InviteMerchantTeamMember(ctx, userActor(t, cp, ownerToken), theirs.MerchantID, billing.InviteTeamMemberParams{Email: *u.Email, Role: "viewer"})
	require.NoError(t, err)
	require.NotNil(t, added.Member)
	_, err = cp.ProvisionMerchant(ctx, billing.ProvisionMerchantParams{Slug: uniqueName("c-unrelated"), OwnerUserID: owner})
	require.NoError(t, err)

	list := func(token string) []billing.UserMerchant {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header.Set("Authorization", "Bearer "+token)
		merchants, err := cp.ListUserMerchants(ctx, r)
		require.NoError(t, err)
		return merchants
	}
	listed := list(memberToken)
	require.Len(t, listed, 2)
	require.Equal(t, []string{"merchant:*"}, listed[0].Permissions, "an owner holds the namespace")
	require.Equal(t, []string{staffperm.Read}, listed[1].Permissions, "a viewer reads only")
	viewerGrants := listed[1].Permissions
	require.Equal(t, []billing.UserMerchant{
		{ID: mine.MerchantID, Slug: own, DisplayName: "Own Shop", Role: "owner", Permissions: []string{"merchant:*"}},
		{ID: theirs.MerchantID, Slug: viewed, Role: "viewer", Permissions: viewerGrants},
	}, listed)

	renamed := uniqueName("a-renamed")
	require.NoError(t, operatorRename(ctx, cp, mine.MerchantID, renamed))
	require.Equal(t, renamed, list(memberToken)[0].Slug, "the list carries current names")
	result, err := cp.RetireUnusedMerchant(ctx, mine.MerchantID, mine.GroupID)
	require.NoError(t, err)
	require.True(t, result.Retired)
	require.Equal(t, []billing.UserMerchant{{ID: theirs.MerchantID, Slug: viewed, Role: "viewer", Permissions: viewerGrants}}, list(memberToken))
}

// A hosted product aggregates its own account response through the public
// Client, without taking a caller-selected user ID or joining AuthKit groups.
func TestHostUserMerchantListingUsesLiveSessionAndMembership(t *testing.T) {
	f := newFixture(t)
	cp := f.newServer(t, reserving())
	ctx := t.Context()
	member, token := newOwner(t, cp)
	owner, _ := newOwner(t, cp)
	mine, err := cp.ProvisionMerchant(ctx, billing.ProvisionMerchantParams{Slug: uniqueName("a-own"), DisplayName: "Own Shop", OwnerUserID: member})
	require.NoError(t, err)
	viewed, err := cp.ProvisionMerchant(ctx, billing.ProvisionMerchantParams{Slug: uniqueName("b-viewed"), DisplayName: "Viewed Shop", OwnerUserID: owner})
	require.NoError(t, err)
	_, err = cp.ProvisionMerchant(ctx, billing.ProvisionMerchantParams{Slug: uniqueName("c-unrelated"), OwnerUserID: owner})
	require.NoError(t, err)
	_, err = cp.AuthKit().SetGroupRole(ctx, iam.SystemIdentity(), iam.GroupByID(viewed.GroupID), iam.UserSubject(member), controlplane.MerchantViewer)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/me/accounts?user_id="+owner, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-User-ID", owner)
	list := func() []billing.UserMerchant {
		t.Helper()
		merchants, err := cp.ListUserMerchants(ctx, req)
		require.NoError(t, err)
		return merchants
	}
	initial := list()
	require.Len(t, initial, 2, "an unrelated owner's merchant is never exposed by caller input")
	require.Equal(t, mine.MerchantID, initial[0].ID)
	require.Equal(t, "owner", initial[0].Role)
	require.Equal(t, viewed.MerchantID, initial[1].ID)
	require.Equal(t, "viewer", initial[1].Role)

	_, err = cp.AuthKit().SetGroupRole(ctx, iam.SystemIdentity(), iam.GroupByID(viewed.GroupID), iam.UserSubject(member), controlplane.MerchantSupport)
	require.NoError(t, err)
	require.Equal(t, "support", list()[1].Role, "the same token/request sees the current role")
	require.NoError(t, cp.AuthKit().RemoveGroupMember(ctx, iam.SystemIdentity(), iam.GroupByID(viewed.GroupID), iam.UserSubject(member)))
	require.Len(t, list(), 1, "removed memberships are not cached in token claims")

	renamed := uniqueName("a-renamed")
	require.NoError(t, operatorRename(ctx, cp, mine.MerchantID, renamed))
	require.Equal(t, renamed, list()[0].Slug)
	require.Equal(t, mine.MerchantID, list()[0].ID, "renaming preserves billing identity")

	for _, bad := range []*http.Request{nil, httptest.NewRequest(http.MethodGet, "/", nil)} {
		_, err := cp.ListUserMerchants(ctx, bad)
		require.Error(t, err)
	}
	bad := req.Clone(ctx)
	bad.Header.Set("Authorization", "Bearer not-a-token")
	_, err = cp.ListUserMerchants(ctx, bad)
	require.Error(t, err)
	unbound, err := cp.AuthKit().MintAccessToken(ctx, member, iam.AccessTokenOptions{})
	require.NoError(t, err)
	bad.Header.Set("Authorization", "Bearer "+unbound.Value)
	_, err = cp.ListUserMerchants(ctx, bad)
	require.ErrorIs(t, err, helpersauth.ErrRevoked, "a valid JWT without a live sign-in does not borrow host authority")

	_, err = cp.AuthKit().RevokeAccountSessions(ctx, iam.UserIdentity(member), member)
	require.NoError(t, err)
	_, err = cp.ListUserMerchants(ctx, req)
	require.ErrorIs(t, err, helpersauth.ErrRevoked, "revocation is checked again even on the same request")
}
