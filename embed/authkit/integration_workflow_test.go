//go:build integration

package authkit_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	coreauth "github.com/open-rails/authkit"
	authcore "github.com/open-rails/authkit/embedded"
	billingauthkit "github.com/open-rails/openrails/embed/authkit"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/requestauth"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

func TestIntegrationLiveAuthorityAndNativeIdentity(t *testing.T) {
	dsn := dbtest.SharedSuperuserDSN(t)
	t.Cleanup(dbtest.TerminateShared)
	ctx := t.Context()
	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := "identity_1047_" + uuid.NewString()[:8]
	require.NoError(t, authcore.ApplyMigrations(ctx, pool, schema))
	t.Cleanup(func() {
		_, err := pool.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		require.NoError(t, err)
	})
	const issuer = "https://identity1047.test"
	runtime, err := authcore.New(authcore.Config{Schema: schema, Keys: authcore.KeysConfig{AllowEphemeralDevKeys: true},
		Token:     authcore.TokenConfig{Issuer: issuer, IssuedAudiences: []string{"billing"}, ExpectedAudiences: []string{"billing"}},
		Ephemeral: authcore.EphemeralConfig{AllowMemory: true}, TwoFactor: authcore.TwoFactorConfig{Mode: authcore.TwoFactorDisabled},
		RBAC: []authcore.PersonaDef{
			authcore.IntrinsicRootPersona(authcore.RoleDef{Name: "billing-operator", Permissions: []string{"root:merchant:payments:refund"}}),
			{Name: "merchant", Parent: coreauth.RootPersona, Roles: []authcore.RoleDef{{Name: "viewer", Permissions: []string{"merchant:payments:read"}}}},
		},
	}, authcore.Deps{Postgres: pool})
	require.NoError(t, err)
	t.Cleanup(runtime.Close)
	client := runtime.Client()
	owner, err := client.CreateUser(ctx, "owner1047@example.test", "owner1047")
	require.NoError(t, err)
	staff, err := client.CreateUser(ctx, "staff1047@example.test", "staff1047")
	require.NoError(t, err)
	operator, err := client.CreateUser(ctx, "operator1047@example.test", "operator1047")
	require.NoError(t, err)
	groupA := coreauth.GroupRef{Persona: "merchant", Instance: "alpha"}
	groupB := coreauth.GroupRef{Persona: "merchant", Instance: "bravo"}
	groupIDs := map[string]string{}
	for _, group := range []coreauth.GroupRef{groupA, groupB} {
		id, err := client.CreatePermissionGroup(ctx, coreauth.CreatePermissionGroupRequest{Persona: group.Persona, InstanceSlug: group.Instance, ParentPersona: coreauth.RootPersona, OwnerSubjectID: owner.ID})
		require.NoError(t, err)
		groupIDs[group.Instance] = id
	}
	require.NoError(t, client.OperatorAssignGroupRole(ctx, groupA, coreauth.UserSubject(staff.ID), "viewer"))
	require.NoError(t, client.OperatorAssignGroupRole(ctx, coreauth.RootGroup(), coreauth.UserSubject(operator.ID), "billing-operator"))
	integration, err := billingauthkit.New(billingauthkit.Config{Verifier: runtime.Verifier(), Client: client, AuthorityIssuer: issuer,
		Authority: func(_ context.Context, q billingauth.Requirement) (billingauthkit.Authority, error) {
			if q.Scope != billingauth.MerchantScope {
				return billingauthkit.Authority{}, nil
			}
			return billingauthkit.Authority{Group: coreauth.GroupRef{Persona: "merchant", Instance: q.Target.MerchantSlug}, Permission: coreauth.Perm(q.Permission)}, nil
		},
		PlatformAuthority: func(_ context.Context, q billingauth.Requirement) (billingauthkit.Authority, error) {
			if q.Permission != "merchant:payments:refund" {
				return billingauthkit.Authority{}, nil
			}
			return billingauthkit.Authority{Group: coreauth.RootGroup(), Permission: "root:merchant:payments:refund"}, nil
		},
	})
	require.NoError(t, err)
	mint := func(id string) string {
		token, _, err := client.MintAccessToken(ctx, id, map[string]any{"roles": []string{"owner"}, "permissions": []string{"merchant:*"}})
		require.NoError(t, err)
		return token
	}
	staffToken, operatorToken := mint(staff.ID), mint(operator.ID)
	check := func(token, slug, permission string, want int) {
		t.Helper()
		r := requestauth.Begin(httptest.NewRequest(http.MethodPost, "https://billing.test/v2/merchant/payments", nil))
		r.Header.Set("Authorization", "Bearer "+token)
		identity, err := integration.Authentication.AuthenticateRequest(r.Context(), r)
		require.NoError(t, err)
		if identity.Kind == billingauth.NativeUser {
			require.Empty(t, identity.Permissions, "native token role/permission extras cannot confer authority")
		}
		err = integration.Authorization.Authorize(r.Context(), r, identity, billingauth.Requirement{Scope: billingauth.MerchantScope, Permission: permission, Target: billingauth.Target{MerchantID: merchant.ID(uuid.New()), MerchantSlug: slug, AuthorityGroupID: groupIDs[slug]}})
		if want == 0 {
			require.NoError(t, err)
		} else {
			var gate billingauth.GateError
			require.ErrorAs(t, err, &gate)
			require.Equal(t, want, gate.Status)
		}
	}
	check(staffToken, "alpha", "merchant:payments:read", 0)
	check(staffToken, "bravo", "merchant:payments:read", 403)
	// Personal customer ownership does not satisfy a merchant refund operation.
	check(staffToken, "alpha", "merchant:payments:refund", 403)
	require.NoError(t, client.OperatorUnassignGroupRole(ctx, groupA, coreauth.UserSubject(staff.ID), "viewer"))
	check(staffToken, "alpha", "merchant:payments:read", 403)
	require.NoError(t, client.OperatorAssignGroupRole(ctx, groupA, coreauth.UserSubject(staff.ID), "viewer"))
	require.NoError(t, client.BanUser(ctx, staff.ID, nil, nil, owner.ID))
	check(staffToken, "alpha", "merchant:payments:read", 0) // valid native JWT, live permissions, no implicit ban lookup
	check(operatorToken, "alpha", "merchant:payments:refund", 0)
	check(operatorToken, "bravo", "merchant:payments:refund", 0)
	check(operatorToken, "bravo", "merchant:payment-providers:update", 403)
	// An actual opaque API key remains bounded to its immutable permission group.
	_, key, err := client.MintAPIKeyWithOptions(ctx, groupA, coreauth.APIKeyMintOptions{Name: "read-only", Role: "viewer", CreatedBy: owner.ID})
	require.NoError(t, err)
	check(key, "alpha", "merchant:payments:read", 0)
	check(key, "bravo", "merchant:payments:read", 403)
	check(key, "alpha", "merchant:payments:refund", 403)

}
