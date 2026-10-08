//go:build e2e && integration

package ci_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/open-rails/authkit/iam"
	helpersauth "github.com/open-rails/helpers/auth"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/stretchr/testify/require"
)

// GET /v1/merchants is the console's merchant list (#1106): the live merchants
// a user holds a role in, by current name, with the user's role in each.
func TestUserMerchantsListing(t *testing.T) {
	f := newFixture(t)
	cp := f.attachControlPlane(t, reserving())
	ctx := t.Context()
	handler, err := standaloneHandler(cp)
	require.NoError(t, err)
	member, memberToken := newUser(t, cp)
	owner, ownerToken := newOwner(t, cp)
	verifyEmail(t, cp, member)
	u, err := cp.AuthKit().User(ctx, iam.UserByID(member))
	require.NoError(t, err)

	own := uniqueName("a-own")
	mine, err := cp.ProvisionMerchant(ctx, billing.ProvisionMerchantParams{Slug: own, DisplayName: "Own Shop", OwnerUserID: member})
	require.NoError(t, err)
	viewed := uniqueName("b-viewed")
	theirs, err := cp.ProvisionMerchant(ctx, billing.ProvisionMerchantParams{Slug: viewed, OwnerUserID: owner})
	require.NoError(t, err)
	w := call(t, handler, ownerToken, http.MethodPost, "/v1/merchant/team/invites", viewed, map[string]string{"email": *u.Email, "role": "viewer"})
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	_, err = cp.ProvisionMerchant(ctx, billing.ProvisionMerchantParams{Slug: uniqueName("c-unrelated"), OwnerUserID: owner})
	require.NoError(t, err)

	list := func(token string) []billing.UserMerchant {
		w := call(t, handler, token, http.MethodGet, "/v1/merchants", "", nil)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		var body billing.ListPage[billing.UserMerchant]
		require.NoError(t, json.NewDecoder(w.Body).Decode(&body))
		require.Empty(t, body.Next, "one page")
		return body.Items
	}
	require.Equal(t, []billing.UserMerchant{
		{ID: mine.MerchantID, Slug: own, DisplayName: "Own Shop", Role: "owner"},
		{ID: theirs.MerchantID, Slug: viewed, Role: "viewer"},
	}, list(memberToken))

	renamed := uniqueName("a-renamed")
	require.NoError(t, operatorRename(ctx, cp, mine.MerchantID, renamed))
	require.Equal(t, renamed, list(memberToken)[0].Slug, "the list carries current names")
	result, err := cp.RetireUnusedMerchant(ctx, mine.MerchantID, mine.GroupID)
	require.NoError(t, err)
	require.True(t, result.Retired)
	require.Equal(t, []billing.UserMerchant{{ID: theirs.MerchantID, Slug: viewed, Role: "viewer"}}, list(memberToken))

	w = call(t, handler, "not-a-token", http.MethodGet, "/v1/merchants", "", nil)
	require.Equal(t, http.StatusUnauthorized, w.Code)
}

// A hosted product aggregates its own account response through the public
// Client, without taking a caller-selected user ID or joining AuthKit groups.
func TestHostUserMerchantListingUsesLiveSessionAndMembership(t *testing.T) {
	f := newFixture(t)
	cp := f.attachControlPlane(t, reserving())
	ctx := t.Context()
	member, token := newOwner(t, cp)
	owner, _ := newOwner(t, cp)
	mine, err := cp.ProvisionMerchant(ctx, billing.ProvisionMerchantParams{Slug: uniqueName("a-own"), DisplayName: "Own Shop", OwnerUserID: member})
	require.NoError(t, err)
	viewed, err := cp.ProvisionMerchant(ctx, billing.ProvisionMerchantParams{Slug: uniqueName("b-viewed"), DisplayName: "Viewed Shop", OwnerUserID: owner})
	require.NoError(t, err)
	_, err = cp.ProvisionMerchant(ctx, billing.ProvisionMerchantParams{Slug: uniqueName("c-unrelated"), OwnerUserID: owner})
	require.NoError(t, err)
	_, err = cp.AuthKit().SetGroupRole(ctx, iam.SystemActor(), iam.GroupByID(viewed.GroupID), iam.UserSubject(member), controlplane.MerchantViewer)
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

	_, err = cp.AuthKit().SetGroupRole(ctx, iam.SystemActor(), iam.GroupByID(viewed.GroupID), iam.UserSubject(member), controlplane.MerchantSupport)
	require.NoError(t, err)
	require.Equal(t, "support", list()[1].Role, "the same token/request sees the current role")
	require.NoError(t, cp.AuthKit().RemoveGroupMember(ctx, iam.SystemActor(), iam.GroupByID(viewed.GroupID), iam.UserSubject(member)))
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

	_, err = cp.AuthKit().RevokeAccountSessions(ctx, iam.UserActor(member), member)
	require.NoError(t, err)
	_, err = cp.ListUserMerchants(ctx, req)
	require.ErrorIs(t, err, helpersauth.ErrRevoked, "revocation is checked again even on the same request")
}
