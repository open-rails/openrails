//go:build integration

package critical_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/iam"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/merchantbinding"
	"github.com/stretchr/testify/require"
)

// merchantGroups reads a binding's live group from the world's AuthKit, as
// adapters/authkit.MerchantGroups does.
type merchantGroups struct{ client *authkit.Client }

func (g merchantGroups) LookupMerchantGroup(ctx context.Context, binding billing.MerchantBinding) (billing.MerchantGroup, error) {
	tenant, err := iam.ParseTenantID(binding.TenantID)
	if err != nil {
		return billing.MerchantGroup{}, err
	}
	view, err := g.client.ForTenant(ctx, tenant)
	if err != nil {
		return billing.MerchantGroup{}, err
	}
	group, err := view.Group(ctx, iam.GroupByID(binding.ID.String()))
	if err != nil {
		return billing.MerchantGroup{}, err
	}
	return groupMetadata(group), nil
}

func groupMetadata(group iam.Group) billing.MerchantGroup {
	return billing.MerchantGroup{ID: group.ID, TenantID: group.TenantID.String(), Persona: group.Persona.String(), Deleted: group.DeletedAt != nil}
}

// bindMerchant adopts binding's merchant, creating its AuthKit group in the
// binding's transaction as adapters/authkit.BindMerchant does; abort fails
// the transaction after the group was created.
func bindMerchant(ctx context.Context, a *authorityWorld, binding billing.MerchantBinding, abort error) error {
	tenant, err := iam.ParseTenantID(binding.TenantID)
	if err != nil {
		return err
	}
	view, err := a.auth.ForTenant(ctx, tenant)
	if err != nil {
		return err
	}
	return merchantbinding.Bind(ctx, a.pool, billing.DatabaseConfig(a.cfg.Database), binding, func(ctx context.Context, tx pgx.Tx) error {
		group, err := view.CreateGroup(ctx, iam.NewGroup{ID: binding.ID.String(), TenantID: tenant, Persona: a.merchant.Persona}, authkit.InTx(tx))
		if err != nil {
			return err
		}
		if abort != nil {
			return abort
		}
		return merchantbinding.ValidateGroup(binding, groupMetadata(group))
	})
}

func TestTenantBinding(t *testing.T) {
	t.Run("OR-J01-S18/binding-adopts-once-and-never-changes", func(t *testing.T) {
		a := newAuthorityWorld(t, func(w *world) {
			setCfgMerchant(&w.cfg, "gamma", billing.MerchantDeclaration{MerchantDocument: billing.MerchantDocument{DisplayName: "Gamma"}})
		})
		gamma, err := declaredMerchant(a.engine, "gamma")
		require.NoError(t, err)
		tenant := iam.NewTenantID()
		require.NoError(t, a.auth.CreateTenant(t.Context(), iam.Tenant{ID: tenant, Slug: "gamma-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]}))
		binding := billing.MerchantBinding{ID: gamma.MerchantID(), TenantID: tenant.String()}
		groups := merchantGroups{client: a.auth}
		merchants := pgx.Identifier{a.schema, "merchants"}.Sanitize()
		recorded := func() (group *string, tenant *uuid.UUID) {
			require.NoError(t, a.pool.QueryRow(t.Context(), "SELECT permission_group_id, tenant_id FROM "+merchants+" WHERE id=$1", gamma.MerchantID().UUID()).Scan(&group, &tenant))
			return group, tenant
		}

		// The group and the binding commit together: an abort after the
		// group was created leaves neither.
		abort := errors.New("abort after the group")
		require.ErrorIs(t, bindMerchant(t.Context(), a, binding, abort), abort)
		_, bound := recorded()
		require.Nil(t, bound)
		_, err = groups.LookupMerchantGroup(t.Context(), binding)
		require.Error(t, err, "the aborted transaction created no group")

		require.NoError(t, bindMerchant(t.Context(), a, binding, nil))
		group, bound := recorded()
		require.NotNil(t, group)
		require.Equal(t, gamma.MerchantID().String(), *group)
		require.NotNil(t, bound)
		require.Equal(t, tenant.String(), bound.String())
		live, err := groups.LookupMerchantGroup(t.Context(), binding)
		require.NoError(t, err)
		require.NoError(t, merchantbinding.ValidateGroup(binding, live))

		// A replay changes nothing.
		require.NoError(t, bindMerchant(t.Context(), a, binding, nil))
		replayGroup, replayTenant := recorded()
		require.Equal(t, *group, *replayGroup)
		require.Equal(t, *bound, *replayTenant)

		// Another tenant is refused before any group is made.
		other := billing.MerchantBinding{ID: gamma.MerchantID(), TenantID: iam.NewTenantID().String()}
		err = merchantbinding.Bind(t.Context(), a.pool, billing.DatabaseConfig(a.cfg.Database), other, func(context.Context, pgx.Tx) error {
			t.Error("a conflicting binding ensured a group")
			return nil
		})
		require.ErrorContains(t, err, "conflicting binding")
		_, kept := recorded()
		require.Equal(t, tenant.String(), kept.String())

		// The schema refuses a change, even to no tenant.
		for _, change := range []struct {
			sql  string
			args []any
		}{
			{"UPDATE " + merchants + " SET tenant_id=$1 WHERE id=$2", []any{uuid.New(), gamma.MerchantID().UUID()}},
			{"UPDATE " + merchants + " SET tenant_id=NULL WHERE id=$1", []any{gamma.MerchantID().UUID()}},
		} {
			_, err = a.pool.Exec(t.Context(), change.sql, change.args...)
			require.ErrorContains(t, err, "merchant tenant binding is immutable")
		}

		// A declaration naming another tenant is not served; the adopted one is.
		declare := func(tenant string) {
			declaration := cfgMerchant(a.cfg, "gamma")
			declaration.Binding = &billing.MerchantBinding{ID: gamma.MerchantID(), TenantID: tenant}
			setCfgMerchant(&a.cfg, "gamma", declaration)
		}
		a.deps.MerchantGroups = groups
		declare(iam.NewTenantID().String())
		a.restart(t)
		_, err = declaredMerchant(a.engine, "gamma")
		require.Error(t, err, "a mismatched declared binding is refused")
		require.Contains(t, configurationRefusal(a.engine), "merchants.gamma: openrails: verify declared merchant binding")
		declare(tenant.String())
		a.restart(t)
		served, err := declaredMerchant(a.engine, "gamma")
		require.NoError(t, err)
		require.Equal(t, gamma.MerchantID(), served.MerchantID())
	})
	t.Run("OR-J01-S19/a-customer-of-another-tenant-is-refused-at-me", func(t *testing.T) {
		a := newAuthorityWorld(t)
		require.NoError(t, bindMerchant(t.Context(), a, billing.MerchantBinding{ID: a.alpha.MerchantID(), TenantID: a.tenant.String()}, nil))
		status, raw := a.request(t, http.MethodGet, "/v1/me", a.aliceToken, nil)
		require.Equal(t, http.StatusOK, status, "a merchant bound to the customers' tenant serves them: %s", raw)

		betaToken := a.signedToken(t, func(_ map[string]any, claims map[string]any) {
			claims["group_id"] = a.beta.MerchantID().String()
		})
		status, raw = a.request(t, http.MethodGet, "/v1/me", betaToken, nil)
		require.Equal(t, http.StatusOK, status, "an unbound merchant admits the issuer's account: %s", raw)
		merchants := pgx.Identifier{a.schema, "merchants"}.Sanitize()
		_, err := a.pool.Exec(t.Context(), "UPDATE "+merchants+" SET permission_group_id=id::text, tenant_id=$1 WHERE id=$2", uuid.New(), a.beta.MerchantID().UUID())
		require.NoError(t, err)
		status, raw = a.request(t, http.MethodGet, "/v1/me", betaToken, nil)
		require.Equal(t, http.StatusForbidden, status, string(raw))
		require.Contains(t, string(raw), `"code":"customer_tenant_mismatch"`)
	})
}
