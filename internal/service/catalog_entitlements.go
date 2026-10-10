package service

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
	catalogwire "github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/catalog"
)

// keyEdit attributes a key change to now on the engine clock and to the
// catalog application, the authenticated operator, or the API.
func (s *Service) keyEdit(ctx context.Context) catalog.KeyEdit {
	actor := s.catalogKeyActor
	if actor == "" {
		actor = editActor(ctx)
	}
	if len(actor) > 255 {
		actor = actor[:255]
	}
	return catalog.KeyEdit{At: s.now().UTC(), Actor: actor}
}

// editActor is who made an edit: the signed-in user, or the API.
func editActor(ctx context.Context) string {
	if uc, ok := billingauth.FromContext(ctx); ok && strings.TrimSpace(uc.UserID) != "" && len(uc.UserID) <= 255 {
		return uc.UserID
	}
	return "api"
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

// replaceEntitlements moves every product granting each From key to its To
// key, or removes From where To is empty, inside a catalog application.
// Holders follow the products, so they lose From and gain To at once.
func (s *Service) replaceEntitlements(ctx context.Context, pairs []catalogwire.EntitlementReplacement, receipt *billing.CatalogApplicationReceipt) error {
	if len(pairs) == 0 {
		return nil
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	q := s.catalogDatabase().Gen(ctx)
	edit := s.keyEdit(ctx)
	changes := map[uuid.UUID]*billing.EntitlementChange{}
	var order []uuid.UUID
	for _, pair := range pairs {
		var to *string
		if pair.To != "" {
			to = &pair.To
		}
		rows, err := q.ReplaceEntitlement(ctx, gen.ReplaceEntitlementParams{MerchantID: mid.UUID(), FromKey: pair.From, ToKey: to, At: edit.At, Actor: edit.Actor})
		if err != nil {
			return err
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
			return err
		}
	}
	announced := make([]*billing.EntitlementChange, len(order))
	for i, id := range order {
		slices.Sort(changes[id].Added)
		slices.Sort(changes[id].Removed)
		announced[i] = changes[id]
	}
	if err := announceKeyChanges(ctx, q, mid.UUID(), edit.At, announced); err != nil {
		return err
	}
	slices.SortFunc(announced, func(a, b *billing.EntitlementChange) int { return strings.Compare(a.ProductKey, b.ProductKey) })
	for _, change := range announced {
		receipt.EntitlementChanges = append(receipt.EntitlementChanges, *change)
		id := change.ProductID.UUID()
		s.catalogAfterCommit(ctx, func(ctx context.Context, s *Service) { s.syncStripeFeatures(ctx, id) })
	}
	receipt.ProductsChanged += len(order)
	return nil
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

// announceKeyChanges counts each changed product's holders into its change
// and tells the host, in the edit's transaction.
func announceKeyChanges(ctx context.Context, q *gen.Queries, merchantID uuid.UUID, at time.Time, changes []*billing.EntitlementChange) error {
	if len(changes) == 0 {
		return nil
	}
	ids := make([]uuid.UUID, len(changes))
	for i, c := range changes {
		ids[i] = c.ProductID.UUID()
	}
	rows, err := q.CountLiveProductHolders(ctx, gen.CountLiveProductHoldersParams{MerchantID: merchantID, ProductIds: ids, At: at})
	if err != nil {
		return err
	}
	holders := make(map[uuid.UUID]int64, len(rows))
	for _, row := range rows {
		holders[row.ProductID] = row.Holders
	}
	for _, c := range changes {
		c.Holders = holders[c.ProductID.UUID()]
		data, err := json.Marshal(billing.ProductEntitlementsChangedEvent{ProductID: c.ProductID, ProductKey: c.ProductKey, Added: nonNil(c.Added), Removed: nonNil(c.Removed), Holders: c.Holders})
		if err != nil {
			return err
		}
		if err := q.EnqueueProductEntitlementsChanged(ctx, gen.EnqueueProductEntitlementsChangedParams{MerchantID: merchantID, ProductID: c.ProductID.UUID(), OccurredAt: at, Data: data}); err != nil {
			return err
		}
	}
	return nil
}
