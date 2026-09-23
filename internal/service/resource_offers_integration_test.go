//go:build integration

package service

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/catalogscope"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

func TestResourceDiscoveryAndExactAccessPreserveMerchantAndCatalogScope(t *testing.T) {
	s, ctx := applicationService(t)
	mid, err := merchant.Require(ctx)
	require.NoError(t, err)
	for _, owner := range []string{"alice", "bob"} {
		cat, err := catalog.NewCatalogRepo(s.rt.DB).Ensure(ctx, &owner)
		require.NoError(t, err)
		application := applicationParams(t, s, ctx)
		application.CatalogID = openrails.CatalogID(cat.ID).String()
		product := applicationProduct(owner)
		product.EntitlementsSpec = openrails.CatalogValue(map[string]*int{"post:exact": nil})
		application.Products = []openrails.CatalogApplyProduct{product}
		_, err = s.ApplyCatalog(ctx, application)
		require.NoError(t, err)
		ownerCtx, err := catalogscope.WithOwner(ctx, catalogscope.Scope{MerchantID: mid, CatalogID: cat.ID, OwnerSubject: owner})
		require.NoError(t, err)
		offers, err := s.ListOffersForEntitlement(ownerCtx, "post:exact", openrails.OfferListParams{Kind: openrails.OfferPermanent})
		require.NoError(t, err)
		require.Len(t, offers.Data, 1)
		require.Equal(t, owner, offers.Data[0].ProductKey)
	}
	all, err := s.ListOffersForEntitlement(ctx, "post:exact", openrails.OfferListParams{Kind: openrails.OfferPermanent, Limit: 1})
	require.NoError(t, err)
	require.True(t, all.HasMore)
	other, otherCtx := applicationService(t)
	_, err = other.ListOffersForEntitlement(otherCtx, "post:exact", openrails.OfferListParams{Kind: openrails.OfferPermanent, Cursor: all.NextCursor})
	require.ErrorIs(t, err, openrails.ErrInvalid)
	none, err := other.ListOffersForEntitlement(otherCtx, "post:exact", openrails.OfferListParams{Kind: openrails.OfferPermanent})
	require.NoError(t, err)
	require.Empty(t, none.Data)
	none, err = s.ListOffersForEntitlement(ctx, "post:EXACT", openrails.OfferListParams{Kind: openrails.OfferPermanent})
	require.NoError(t, err)
	require.Empty(t, none.Data)
	user := uuid.NewString()
	dbtest.EnsureCustomerIDPgxFor(ctx, t, s.rt.DB.Pool(), mid.UUID(), user)
	s.rt.EntitlementService = entitlements.NewEntitlementService(s.rt.DB)
	_, err = s.rt.EntitlementService.PushNewEntitlement(ctx, entitlements.PushNewEntitlementParams{UserID: user, Entitlement: "post:exact", Indefinite: true, SourceType: models.EntitlementSourceAdmin, SourceID: uuid.New()})
	require.NoError(t, err)
	access, err := s.CheckEntitlements(ctx, user, []string{"post:exact", "post:EXACT", "unknown"}, time.Time{})
	require.NoError(t, err)
	require.Equal(t, map[string]bool{"post:exact": true, "post:EXACT": false, "unknown": false}, access)
	other.rt.EntitlementService = entitlements.NewEntitlementService(other.rt.DB)
	access, err = other.CheckEntitlements(otherCtx, user, []string{"post:exact"}, time.Time{})
	require.NoError(t, err)
	require.False(t, access["post:exact"])
	_, err = s.CheckEntitlements(ctx, user, make([]string, 101), time.Time{})
	require.ErrorIs(t, err, openrails.ErrInvalid)
}
