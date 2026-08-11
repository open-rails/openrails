//go:build integration

package service_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/money"
	billingservice "github.com/open-rails/openrails/pkg/service"
)

// or#912: the embedded-caller facade surface. The resolution semantics are
// exercised in internal/modules/entitlements; this proves the pkg/service
// method pins its own merchant connection (or#900) and maps the winner —
// immutable identifiers + display name + product ref — for a host calling it
// during token issuance.
func TestOr912_ServiceResolveEffectiveTier(t *testing.T) {
	ctx := dbtest.WithTestMerchant(context.Background())
	dbi := dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID())
	pool := dbi.Pool()
	dbtest.EnsureTestMerchant(ctx, t, pool)

	rt := &app.Runtime{
		DB:                 dbi,
		MoneyService:       money.NewMoneyService(dbi),
		EntitlementService: entitlements.NewEntitlementService(dbi),
	}
	svc, err := billingservice.New(rt)
	require.NoError(t, err)

	suffix := uuid.NewString()[:8]
	group := "svcgrp_" + suffix
	entLow := "svc_tier_1_" + suffix
	entHigh := "svc_tier_2_" + suffix

	seed := func(key, name string, rank int, ent string) uuid.UUID {
		spec, err := json.Marshal(map[string]*int{ent: nil})
		require.NoError(t, err)
		id := uuid.New()
		_, err = pool.Exec(ctx, `
			INSERT INTO openrails.products (id, merchant_id, key, display_name, entitlements_spec, tier_group, tier_rank)
			VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			id, dbtest.TestMerchantID.UUID(), key, name, spec, group, rank)
		require.NoError(t, err)
		t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM openrails.products WHERE id = $1`, id) })
		return id
	}
	seed("svc_basic_"+suffix, "Basic", 1, entLow)
	seed("svc_pro_"+suffix, "Pro", 2, entHigh)

	userID := uuid.NewString()
	customerID := dbtest.EnsureCustomerIDPgx(ctx, t, pool, userID)
	now := time.Now().UTC().Truncate(time.Second)
	es := entitlements.NewEntitlementService(dbi)
	for _, ent := range []string{entLow, entHigh} {
		sid := uuid.New()
		require.NoError(t, es.Insert(ctx, &models.Entitlement{
			CustomerID:  customerID,
			Entitlement: ent,
			StartAt:     now.Add(-time.Hour),
			SourceID:    &sid,
			SourceType:  models.EntitlementSourceAdmin,
		}))
	}

	tier, err := svc.ResolveEffectiveTier(ctx, userID, group)
	require.NoError(t, err)
	require.NotNil(t, tier)
	require.Equal(t, group, tier.Group)
	require.Equal(t, entHigh, tier.Entitlement, "the token claim value: the winner's IMMUTABLE entitlement identifier")
	require.Equal(t, "Pro", tier.DisplayName)
	require.Equal(t, 2, tier.TierRank)
	require.Equal(t, "svc_pro_"+suffix, tier.ProductKey)
	require.NotEmpty(t, tier.ProductID)

	none, err := svc.ResolveEffectiveTier(ctx, uuid.NewString(), group)
	require.NoError(t, err)
	require.Nil(t, none, "no active entitlements is a nil tier, never an error — the host applies its default")
}
