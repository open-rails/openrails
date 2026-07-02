package service

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// stripeAdapter implements providerAdapter for the Stripe catalog provider.
// It supports Attach (with optional remote verification), AutoCreate
// (find-or-create via metadata search and lookup_key), Verify, and Update.
type stripeAdapter struct {
	svc *Service
}

func (a *stripeAdapter) Name() string { return "stripe" }

func (a *stripeAdapter) PendingActionTemplate(_ uuid.UUID) PendingAction {
	// Stripe always supports AutoCreate; this should never fire under normal
	// configuration. The dispatcher only invokes PendingActionTemplate when
	// AutoCreate returns errPendingManualLink — for stripe that's an edge case
	// (no config). We still surface a helpful hint.
	return PendingAction{
		Provider: "stripe",
		Action:   "configure_stripe",
		Hint:     "Stripe is not configured; set stripe.secret_key in config and retry, or PATCH /merchant/catalog/prices/{id} with provider_links.stripe.price_id",
	}
}

// Attach validates supplied Stripe link ids. When Stripe is configured the
// linked Price is round-tripped against the API and its substance verified
// against the OpenRails price: the Price must exist and match unit_amount,
// currency, the recurring interval/duration, and — when the operator supplied a
// product_id — the expected Product association. A missing or mismatched Price
// is a loud error. When Stripe is not configured there is no read API to verify
// against, so the ids are stored as operator-owned.
func (a *stripeAdapter) Attach(ctx context.Context, link map[string]string, in autoCreateContext) (map[string]string, error) {
	link = normalizeLinkMap(link)
	priceID := strings.TrimSpace(link[models.RailKeyStripePriceID])
	if priceID == "" {
		// A Stripe price_id (price_xxx) is STRIPE-GENERATED: it cannot be created
		// at an operator-chosen id, so a price_id link must already exist. The
		// Stripe analog of NMI's "create at my chosen id" is the client-chosen
		// lookup_key — a link supplying only a lookup_key is find-or-created at
		// that key (AutoCreate's flow, but at the operator's key).
		if lookupKey := strings.TrimSpace(link[providerLookupKey]); lookupKey != "" {
			if in.RemoteWritesDisabled {
				// find-or-create at the key is a potential write; defer it rather
				// than silently storing an unverified key as linked.
				return nil, fmt.Errorf("stripe lookup_key %q find-or-create: %w", lookupKey, errRemoteWritesDisabled)
			}
			in.LookupKey = lookupKey
			ids, err := a.AutoCreate(ctx, in)
			if errors.Is(err, errPendingManualLink) {
				// Stripe not configured: store the operator-chosen key as-is.
				return map[string]string{providerLookupKey: lookupKey}, nil
			}
			return ids, err
		}
		return nil, fmt.Errorf("stripe link requires provider_links.stripe.price_id (an existing Stripe Price) or lookup_key (find-or-create at a chosen key)")
	}
	out := map[string]string{
		models.RailKeyStripePriceID: priceID,
	}
	suppliedProductID := strings.TrimSpace(link[models.RailKeyStripeProductID])
	if suppliedProductID != "" {
		out[models.RailKeyStripeProductID] = suppliedProductID
	}

	if !a.stripeConfigured() {
		return out, nil
	}
	stripeSvc := &catalog.StripeCatalogService{Config: a.svc.rt.Config, Rails: a.svc.rt.Rails}
	remote, err := stripeSvc.RetrievePrice(ctx, priceID)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "not found") {
			return nil, fmt.Errorf("stripe price %q not found; create it or fix provider_links.stripe.price_id", priceID)
		}
		return nil, fmt.Errorf("verify stripe price %q: %w", priceID, err)
	}
	if in.UnitAmount > 0 && moneyutil.CentsToMicros(remote.UnitAmount) != in.UnitAmount {
		return nil, fmt.Errorf("stripe price %q unit_amount (%d cents) does not match catalog price (%d micros)", priceID, remote.UnitAmount, in.UnitAmount)
	}
	if in.Currency != "" && !strings.EqualFold(strings.TrimSpace(remote.Currency), strings.TrimSpace(in.Currency)) {
		return nil, fmt.Errorf("stripe price %q currency (%s) does not match catalog price (%s)", priceID, remote.Currency, in.Currency)
	}
	if in.BillingCycleDays != nil && *in.BillingCycleDays > 0 {
		wantInterval, wantCount := catalog.StripeIntervalForDays(*in.BillingCycleDays)
		switch {
		case remote.Recurring == nil:
			return nil, fmt.Errorf("stripe price %q is one-time but catalog price is recurring (%d days)", priceID, *in.BillingCycleDays)
		case remote.Recurring.Interval != wantInterval || remote.Recurring.Count != wantCount:
			return nil, fmt.Errorf("stripe price %q recurring terms (%d %s) do not match catalog price (%d %s)", priceID, remote.Recurring.Count, remote.Recurring.Interval, wantCount, wantInterval)
		}
	} else if remote.Recurring != nil {
		return nil, fmt.Errorf("stripe price %q is recurring but catalog price is one-time", priceID)
	}
	// Product association: if the operator pinned a product_id it must be the
	// Price's actual product; otherwise adopt the Price's product as canonical.
	remoteProduct := strings.TrimSpace(remote.Product)
	if suppliedProductID != "" && remoteProduct != "" && !strings.EqualFold(remoteProduct, suppliedProductID) {
		return nil, fmt.Errorf("stripe price %q belongs to product %q, not the linked product %q", priceID, remoteProduct, suppliedProductID)
	}
	if suppliedProductID == "" && remoteProduct != "" {
		out[models.RailKeyStripeProductID] = remoteProduct
	}
	if lk := strings.TrimSpace(remote.LookupKey); lk != "" {
		out[providerLookupKey] = lk
	}
	return out, nil
}

// stripeConfigured reports whether a usable Stripe secret key is available.
func (a *stripeAdapter) stripeConfigured() bool {
	if a.svc == nil || a.svc.rt == nil || a.svc.rt.Config == nil {
		return false
	}
	stripeProc := a.svc.rt.Rails.GetStripeRail()
	return stripeProc != nil && stripeProc.Stripe != nil && strings.TrimSpace(stripeProc.Stripe.SecretKey) != ""
}

// stripeServiceFor builds the StripeCatalogService for a target account (#641):
// empty → the rail's primary; an account_id → a service keyed to THAT account's
// secret. ok=false when no usable Stripe credentials for the target.
func (a *stripeAdapter) stripeServiceFor(targetAccountID string) (*catalog.StripeCatalogService, bool) {
	if a.svc == nil || a.svc.rt == nil || a.svc.rt.Config == nil {
		return nil, false
	}
	if strings.TrimSpace(targetAccountID) == "" {
		proc := a.svc.rt.Rails.GetStripeRail()
		if proc == nil || proc.Stripe == nil || strings.TrimSpace(proc.Stripe.SecretKey) == "" {
			return nil, false
		}
		return &catalog.StripeCatalogService{Config: a.svc.rt.Config, Rails: a.svc.rt.Rails}, true
	}
	proc, ok := a.svc.rt.Rails.FindByAccountID(models.RailStripe, targetAccountID)
	if !ok || proc.Stripe == nil || strings.TrimSpace(proc.Stripe.SecretKey) == "" {
		return nil, false
	}
	// Single-entry rail set with the target as the (implicit) primary, so the
	// StripeCatalogService resolves THAT account's secret key.
	return &catalog.StripeCatalogService{
		Config: a.svc.rt.Config,
		Rails:  config.RailMerchantAccountSet{"stripe": {Rail: models.RailStripe, Stripe: proc.Stripe}},
	}, true
}

// AutoCreate implements the find-or-create flow for Stripe. Identity is
// CONTENT-based (catalog product keys), so it is wipe-safe — re-syncing after a DB
// rebuild finds the same Stripe objects rather than duplicating them. The
// discovery chain (in order):
//
//  1. If a Stripe Product already exists for this OpenRails product (via
//     metadata search on the content key openrails_product_key=<product_key>),
//     reuse it.
//  2. Otherwise create a new Stripe Product carrying openrails_product_key
//     (+ informational openrails_product_id) and an idempotency key derived
//     from the product key.
//  3. With the Product in hand, find-or-create the Stripe Price under the
//     deterministic content lookup_key
//     ("openrails.<product_key>.<currency>.<unit_amount>.<cycle>"). If a price
//     with the same lookup_key already exists, attach to it. Otherwise mint a
//     new one carrying openrails_price_key (+ informational openrails_price_id)
//     and the lookup_key.
//
// Returns the canonical ids to persist on rails["stripe"].
func (a *stripeAdapter) AutoCreate(ctx context.Context, in autoCreateContext) (map[string]string, error) {
	stripeSvc, ok := a.stripeServiceFor(in.TargetAccountID)
	if !ok {
		// Same error string used by stripe_catalog.go so callers can detect
		// the "not configured" case via substring (Verify, etc).
		return nil, errPendingManualLink
	}

	productKey := strings.TrimSpace(in.ProductKey)
	priceContentKey := openRailsPriceContentKey(in.ProductKey, in.Currency, in.UnitAmount, in.BillingCycleDays)
	benefitFingerprint := productBenefitFingerprint(in.Product)

	// Step 1: discover an existing Stripe Product for this OpenRails product,
	// matching on the content key (product key) so it survives DB wipes.
	stripeProductID := ""
	if productKey != "" {
		if existing, err := stripeSvc.SearchProductsByMetadata(ctx, catalog.StripeMetadataOpenRailsProductKey, productKey); err == nil {
			for _, p := range existing {
				if strings.TrimSpace(p.ID) != "" {
					stripeProductID = p.ID
					break
				}
			}
		}
	}
	// Step 2: create the Stripe Product if discovery did not find one.
	if stripeProductID == "" {
		name := ""
		desc := ""
		if in.Product != nil {
			name = in.Product.DisplayName
			desc = in.Product.Description
		}
		id, err := stripeSvc.CreateProduct(ctx, catalog.CreateProductParams{
			Name:           name,
			Description:    desc,
			IdempotencyKey: "openrails-product-" + productKey,
			Metadata: map[string]string{
				catalog.StripeMetadataOpenRailsProductKey:         productKey,
				catalog.StripeMetadataOpenRailsRecoveryVersion:    openRailsRecoveryVersion,
				catalog.StripeMetadataOpenRailsBenefitFingerprint: benefitFingerprint,
				// Informational only — not used for matching.
				catalog.StripeMetadataOpenRailsProductID: in.ProductID.String(),
			},
		})
		if err != nil {
			return nil, err
		}
		stripeProductID = id
	}

	// #586: mirror this product's entitlements onto the Stripe Product as
	// Features, so the Stripe catalog carries the SAME entitlements as OpenRails.
	// One-way (OpenRails -> Stripe); OpenRails stays the source of truth.
	// Best-effort: a feature-sync failure must not fail the price link — catalog
	// drift surfaces on the next reconcile, like the other Stripe propagations.
	if !in.RemoteWritesDisabled && in.Product != nil && len(in.Product.EntitlementsSpec) > 0 {
		keys := make([]string, 0, len(in.Product.EntitlementsSpec))
		for k := range in.Product.EntitlementsSpec {
			keys = append(keys, k)
		}
		if err := stripeSvc.SyncProductFeatures(ctx, stripeProductID, keys); err != nil {
			log.WithContext(ctx).WithError(err).WithField("stripe_product_id", stripeProductID).
				Warn("stripe entitlement-feature sync failed (best-effort); drift surfaces on reconcile")
		}
	}

	// Step 3: find-or-create the Stripe Price under the content lookup_key.
	stripePriceID := ""
	if lookup := strings.TrimSpace(in.LookupKey); lookup != "" {
		if existing, err := stripeSvc.ListPricesByLookupKey(ctx, lookup); err == nil {
			for _, p := range existing {
				if strings.TrimSpace(p.ID) != "" {
					stripePriceID = p.ID
					break
				}
			}
		}
	}
	if stripePriceID == "" {
		unitAmountCents, err := moneyutil.MicrosToCentsExact(in.UnitAmount)
		if err != nil {
			return nil, err
		}
		id, err := stripeSvc.CreatePrice(ctx, catalog.CreatePriceParams{
			StripeProductID:  stripeProductID,
			UnitAmount:       unitAmountCents,
			Currency:         in.Currency,
			BillingCycleDays: in.BillingCycleDays,
			LookupKey:        in.LookupKey,
			IdempotencyKey:   "openrails-price-" + priceContentKey,
			Metadata: map[string]string{
				catalog.StripeMetadataOpenRailsPriceKey:           priceContentKey,
				catalog.StripeMetadataOpenRailsProductKey:         productKey,
				catalog.StripeMetadataOpenRailsRecoveryVersion:    openRailsRecoveryVersion,
				catalog.StripeMetadataOpenRailsBenefitFingerprint: benefitFingerprint,
				// Informational only — not used for matching.
				catalog.StripeMetadataOpenRailsPriceID:   in.PriceID.String(),
				catalog.StripeMetadataOpenRailsProductID: in.ProductID.String(),
			},
		})
		if err != nil {
			return nil, err
		}
		stripePriceID = id
	}

	ids := map[string]string{
		models.RailKeyStripePriceID:   stripePriceID,
		models.RailKeyStripeProductID: stripeProductID,
	}
	if lookup := strings.TrimSpace(in.LookupKey); lookup != "" {
		ids[providerLookupKey] = lookup
	}
	return ids, nil
}

// Verify performs a live retrieve of the Stripe Price (and its Product) and
// computes per-field drift vs. the OpenRails snapshot.
func (a *stripeAdapter) Verify(ctx context.Context, ids map[string]string, local *priceVerifyContext) ([]DriftField, bool, error) {
	if a.svc == nil || a.svc.rt == nil || a.svc.rt.Config == nil {
		return nil, false, fmt.Errorf("stripe is not configured")
	}
	stripeProc := a.svc.rt.Rails.GetStripeRail()
	if stripeProc == nil || stripeProc.Stripe == nil || strings.TrimSpace(stripeProc.Stripe.SecretKey) == "" {
		return nil, false, fmt.Errorf("stripe is not configured")
	}
	priceID := strings.TrimSpace(ids[models.RailKeyStripePriceID])
	if priceID == "" {
		return nil, false, fmt.Errorf("stripe price_id missing on local rails map")
	}
	stripeSvc := &catalog.StripeCatalogService{Config: a.svc.rt.Config, Rails: a.svc.rt.Rails}
	remote, err := stripeSvc.RetrievePrice(ctx, priceID)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "not found") {
			return nil, true, nil
		}
		return nil, false, err
	}
	drift := []DriftField{}
	if local != nil {
		if local.IsActive != remote.Active {
			drift = append(drift, DriftField{
				Field:          "is_active",
				OpenRailsValue: strconv.FormatBool(local.IsActive),
				RemoteValue:    strconv.FormatBool(remote.Active),
			})
		}
		remoteUnitAmountMicros := moneyutil.CentsToMicros(remote.UnitAmount)
		if local.UnitAmount != remoteUnitAmountMicros {
			drift = append(drift, DriftField{
				Field:          "unit_amount",
				OpenRailsValue: strconv.FormatInt(local.UnitAmount, 10),
				RemoteValue:    strconv.FormatInt(remoteUnitAmountMicros, 10),
			})
		}
		if !strings.EqualFold(local.Currency, remote.Currency) {
			drift = append(drift, DriftField{
				Field:          "currency",
				OpenRailsValue: local.Currency,
				RemoteValue:    remote.Currency,
			})
		}
	}
	return drift, false, nil
}

// verifyStripeProduct is a product-side helper retained for internal use only.
// It is NOT exposed through the public catalog API (per issue #208: products
// have no user-facing provider linkage). It exists so that internal lookups
// (UpdateProduct's best-effort Stripe propagation) can decide whether to push
// changes. Returns drift, missing, configured, error.
func (a *stripeAdapter) verifyStripeProduct(ctx context.Context, stripeProductID string, local *models.Product) ([]DriftField, bool, bool, error) {
	if a.svc == nil || a.svc.rt == nil || a.svc.rt.Config == nil {
		return nil, false, false, nil
	}
	stripeProc := a.svc.rt.Rails.GetStripeRail()
	if stripeProc == nil || stripeProc.Stripe == nil || strings.TrimSpace(stripeProc.Stripe.SecretKey) == "" {
		return nil, false, false, nil
	}
	stripeProductID = strings.TrimSpace(stripeProductID)
	if stripeProductID == "" {
		return nil, false, true, fmt.Errorf("stripe_product_id required")
	}
	stripeSvc := &catalog.StripeCatalogService{Config: a.svc.rt.Config, Rails: a.svc.rt.Rails}
	remote, err := stripeSvc.RetrieveProduct(ctx, stripeProductID)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "not found") {
			return nil, true, true, nil
		}
		return nil, false, true, err
	}
	drift := []DriftField{}
	if local != nil {
		if strings.TrimSpace(local.DisplayName) != strings.TrimSpace(remote.Name) {
			drift = append(drift, DriftField{
				Field:          "display_name",
				OpenRailsValue: local.DisplayName,
				RemoteValue:    remote.Name,
			})
		}
		if strings.TrimSpace(local.Description) != strings.TrimSpace(remote.Description) {
			drift = append(drift, DriftField{
				Field:          "description",
				OpenRailsValue: local.Description,
				RemoteValue:    remote.Description,
			})
		}
		if localActive := local.IsPurchasable(); localActive != remote.Active {
			drift = append(drift, DriftField{
				Field:          "is_active",
				OpenRailsValue: strconv.FormatBool(localActive),
				RemoteValue:    strconv.FormatBool(remote.Active),
			})
		}
	}
	return drift, false, true, nil
}

// Update propagates mutable fields (active) to the Stripe Price.
func (a *stripeAdapter) Update(ctx context.Context, ids map[string]string, mutable mutableUpdate) error {
	if a.svc == nil || a.svc.rt == nil || a.svc.rt.Config == nil {
		return nil
	}
	stripeProc := a.svc.rt.Rails.GetStripeRail()
	if stripeProc == nil || stripeProc.Stripe == nil || strings.TrimSpace(stripeProc.Stripe.SecretKey) == "" {
		return nil
	}
	priceID := strings.TrimSpace(ids[models.RailKeyStripePriceID])
	if priceID == "" {
		return nil
	}
	stripeSvc := &catalog.StripeCatalogService{Config: a.svc.rt.Config, Rails: a.svc.rt.Rails}
	params := catalog.UpdatePriceParams{}
	if mutable.IsActive != nil {
		active := *mutable.IsActive
		params.Active = &active
	}
	return stripeSvc.UpdatePrice(ctx, priceID, params)
}
