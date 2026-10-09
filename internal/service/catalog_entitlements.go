package service

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
	catalogwire "github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

// keyEdit attributes a key change to now on the engine clock and to the
// catalog application, the authenticated operator, or the API.
func (s *Service) keyEdit(ctx context.Context) catalog.KeyEdit {
	actor := s.catalogKeyActor
	if actor == "" {
		if uc, ok := billingauth.FromContext(ctx); ok && strings.TrimSpace(uc.UserID) != "" {
			actor = uc.UserID
		}
	}
	if actor == "" {
		actor = "api"
	}
	if len(actor) > 255 {
		actor = actor[:255]
	}
	return catalog.KeyEdit{At: s.now().UTC(), Actor: actor}
}

func entitlementChange(p *models.Product, change catalog.KeyChange) billing.EntitlementChange {
	return billing.EntitlementChange{
		ProductID: billing.ProductID(p.ID), ProductKey: p.Key,
		Added: nonNil(change.Added), Removed: nonNil(change.Removed),
	}
}

func nonNil(keys []string) []string {
	if keys == nil {
		return []string{}
	}
	return keys
}

// ReplaceEntitlements moves every product granting each From key to its To
// key in one catalog edit, or removes From where To is empty. Holders follow
// the products, so they lose From and gain To at once. It is recorded as a
// catalog application; each call applies anew.
func (s *Service) ReplaceEntitlements(ctx context.Context, params billing.ReplaceEntitlementsParams) (*billing.CatalogApplicationReceipt, error) {
	if err := validateReplacements(params.Pairs); err != nil {
		return nil, err
	}
	if err := s.checkCatalogWritePolicy(ctx); err != nil {
		return nil, err
	}
	return catalogMutation(ctx, s, func(ctx context.Context, scoped *Service) (*billing.CatalogApplicationReceipt, error) {
		mid, err := merchant.Require(ctx)
		if err != nil {
			return nil, err
		}
		q := scoped.catalogDatabase().Gen(ctx)
		base, err := q.GetCatalogRevision(ctx, mid.UUID())
		if err != nil {
			return nil, err
		}
		request, err := json.Marshal(struct {
			Kind         string                           `json:"kind"`
			BaseRevision int64                            `json:"base_revision"`
			Pairs        []billing.EntitlementReplacement `json:"pairs"`
		}{"replace_entitlements", base, params.Pairs})
		if err != nil {
			return nil, err
		}
		digest := sha256.Sum256(request)
		receipt := &billing.CatalogApplicationReceipt{ApplicationID: fmt.Sprintf("sha256:%x", digest), BaseRevision: base, EntitlementChanges: []billing.EntitlementChange{}}
		if err := q.SetCatalogBatchMerchant(ctx, mid.String()); err != nil {
			return nil, err
		}
		edit := scoped.keyEdit(ctx)
		edit.Actor = receipt.ApplicationID
		changes := map[uuid.UUID]*billing.EntitlementChange{}
		var order []uuid.UUID
		for _, pair := range params.Pairs {
			var to *string
			if pair.To != "" {
				to = &pair.To
			}
			rows, err := q.ReplaceEntitlement(ctx, gen.ReplaceEntitlementParams{MerchantID: mid.UUID(), FromKey: pair.From, ToKey: to, At: edit.At, Actor: edit.Actor})
			if err != nil {
				return nil, err
			}
			for _, row := range rows {
				change := changes[row.ProductID]
				if change == nil {
					change = &billing.EntitlementChange{ProductID: billing.ProductID(row.ProductID), ProductKey: row.ProductKey, Added: []string{}, Removed: []string{}}
					changes[row.ProductID] = change
					order = append(order, row.ProductID)
				}
				change.Removed = append(change.Removed, pair.From)
				if row.Opened {
					change.Added = append(change.Added, pair.To)
				}
			}
		}
		if len(order) > 0 {
			if err := q.StepProductRevisions(ctx, gen.StepProductRevisionsParams{MerchantID: mid.UUID(), ProductIds: order}); err != nil {
				return nil, err
			}
		}
		for _, id := range order {
			change := changes[id]
			slices.Sort(change.Added)
			slices.Sort(change.Removed)
			receipt.EntitlementChanges = append(receipt.EntitlementChanges, *change)
			scoped.catalogAfterCommit(ctx, func(ctx context.Context, s *Service) { s.syncStripeFeatures(ctx, id) })
		}
		slices.SortFunc(receipt.EntitlementChanges, func(a, b billing.EntitlementChange) int { return strings.Compare(a.ProductKey, b.ProductKey) })
		receipt.ProductsChanged = len(order)
		if receipt.AppliedRevision, err = q.AdvanceCatalogRevision(ctx, mid.UUID()); err != nil {
			return nil, err
		}
		if err := scoped.recordCatalogReceipt(ctx, receipt, 0, digest); err != nil {
			return nil, err
		}
		if err := q.SetCatalogBatchMerchant(ctx, ""); err != nil {
			return nil, err
		}
		return receipt, nil
	})
}

// recordCatalogReceipt stores the compact receipt; per-product key changes
// are returned to the caller, not retained.
func (s *Service) recordCatalogReceipt(ctx context.Context, receipt *billing.CatalogApplicationReceipt, schemaVersion int, digest [32]byte) error {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	stored := *receipt
	stored.EntitlementChanges = nil
	result, err := json.Marshal(storedReceipt(stored))
	if err != nil {
		return err
	}
	return s.catalogDatabase().Gen(ctx).InsertCatalogApplication(ctx, gen.InsertCatalogApplicationParams{
		MerchantID: mid.UUID(), ApplicationID: receipt.ApplicationID, SchemaVersion: int64(schemaVersion), RequestSha256: digest[:],
		BaseRevision: receipt.BaseRevision, AppliedRevision: receipt.AppliedRevision, Result: result,
	})
}

// storedCatalogReceipt is the retained receipt shape.
type storedCatalogReceipt struct {
	ApplicationID   string `json:"application_id"`
	BaseRevision    int64  `json:"base_revision"`
	AppliedRevision int64  `json:"applied_revision"`
	Replayed        bool   `json:"replayed"`
	ProductsChanged int    `json:"products_changed"`
	PricesChanged   int    `json:"prices_changed"`
}

func storedReceipt(r billing.CatalogApplicationReceipt) storedCatalogReceipt {
	return storedCatalogReceipt{ApplicationID: r.ApplicationID, BaseRevision: r.BaseRevision, AppliedRevision: r.AppliedRevision,
		Replayed: r.Replayed, ProductsChanged: r.ProductsChanged, PricesChanged: r.PricesChanged}
}

func validateReplacements(pairs []billing.EntitlementReplacement) error {
	if len(pairs) == 0 {
		return apperr.Invalidf("pairs is required").WithParam("pairs")
	}
	if len(pairs) > billing.MaxEntitlementReplacements {
		return apperr.Invalidf("at most %d pairs per replacement", billing.MaxEntitlementReplacements).WithParam("pairs")
	}
	from := map[string]bool{}
	to := map[string]bool{}
	for _, pair := range pairs {
		keys := []string{pair.From}
		if pair.To != "" {
			keys = append(keys, pair.To)
		}
		if _, err := catalogwire.NormalizeEntitlements(keys); err != nil {
			return apperr.Invalidf("%v", err).WithParam("pairs")
		}
		if from[pair.From] {
			return apperr.Invalidf("duplicate from key %q", pair.From).WithParam("pairs")
		}
		from[pair.From] = true
		if pair.To != "" {
			to[pair.To] = true
		}
	}
	for key := range from {
		if to[key] {
			return apperr.Invalidf("key %q is both replaced and a replacement", key).WithParam("pairs")
		}
	}
	return nil
}

// syncStripeFeatures mirrors a product's keys onto its Stripe Product, best
// effort, once one exists; drift surfaces on the next verified read.
func (s *Service) syncStripeFeatures(ctx context.Context, productID uuid.UUID) {
	if s.rt == nil || s.rt.Config == nil {
		return
	}
	stripeProductID := s.lookupStripeProductID(ctx, productID)
	if stripeProductID == "" {
		return
	}
	product, err := s.GetProduct(ctx, billing.ProductID(productID))
	if err != nil {
		return
	}
	stripeSvc := &catalog.StripeCatalogService{StripeClients: s.rt.StripeClients, Config: s.rt.Config, Rails: s.rt.RailConfigs}
	_ = stripeSvc.SyncProductFeatures(ctx, stripeProductID, product.Entitlements)
}
