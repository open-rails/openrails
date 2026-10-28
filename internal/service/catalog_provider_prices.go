package service

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	catalogwire "github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/catalog/providerobjects"
	"github.com/open-rails/openrails/internal/shared/apperr"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
)

// ErrProviderPriceUnreadable: the provider did not answer for a provider
// price's terms.
var ErrProviderPriceUnreadable = apperr.New(http.StatusBadGateway, "provider_price_unreadable", "the provider did not answer for the provider price's terms; retry")

// providerPrice is a provider price ready to write: its PSP, the provider's
// object (the binding) and the terms the provider states, or CCBill declares.
type providerPrice struct {
	productKey, key string
	account         merchants.PSPScope
	request         billing.CreatePriceParams
	link            map[string]string
	// cached is why the stored terms were kept: the provider did not answer.
	cached        string
	months        *int
	accessDefault bool
}

// providerPriceTerms are a provider price's money terms.
type providerPriceTerms struct {
	unitAmount            int64
	currency              string
	billingIntervalHours  *int
	billingIntervalMonths *int
	trialUnitAmount       *int64
	trialDurationHours    *int
}

// checkProviderPriceRail refuses a declaration its PSP's rail does not take:
// NMI names a plan, Stripe a price, CCBill a FlexForm with declared terms.
func checkProviderPriceRail(pspKey, rail string, decl catalogwire.ApplyProviderPrice) error {
	refs := decl.References()
	switch models.Rail(rail) {
	case models.RailNMI:
		if decl.PlanID == "" || len(refs) != 1 {
			return apperr.Invalidf("PSP %q is on NMI: a provider price names its plan_id", pspKey)
		}
		if decl.DeclaresTerms() {
			return apperr.Invalidf("an NMI plan's terms are read from NMI: declare plan_id, and currency or access_duration when needed")
		}
	case models.RailStripe:
		if decl.PriceID == "" || len(refs) != 1 {
			return apperr.Invalidf("PSP %q is on Stripe: a provider price names its price_id", pspKey)
		}
		if decl.DeclaresTerms() || decl.Currency != "" {
			return apperr.Invalidf("a Stripe price's terms are read from Stripe: declare price_id, and access_duration when needed")
		}
	case models.RailCCBill:
		if decl.PlanID != "" || decl.PriceID != "" || len(refs) == 0 {
			return apperr.Invalidf("PSP %q is on CCBill: a provider price names its flex_id and recurring_billing_option_id", pspKey)
		}
		if decl.UnitAmount == nil {
			return apperr.Invalidf("CCBill has no read API: a CCBill provider price declares its amount and billing_interval")
		}
		if err := moneyutil.RequireFiatCurrency(decl.Currency); err != nil {
			return apperr.Invalidf("%v", err)
		}
	default:
		return apperr.Invalidf("PSP %q is on %s, which runs no provider prices", pspKey, rail)
	}
	return nil
}

// readProviderPriceTerms reads a provider price's terms from the observed
// catalog (the NMI plan, the Stripe price), reading the object by id when none
// is stored, or CCBill's declared terms. ids are the provider identifiers the
// price binds.
func (s *Service) readProviderPriceTerms(ctx context.Context, account merchants.PSPScope, decl catalogwire.ApplyProviderPrice) (providerPriceTerms, map[string]string, error) {
	var ref string
	switch models.Rail(account.Rail) {
	case models.RailCCBill:
		terms := providerPriceTerms{currency: moneyutil.NormalizeCurrency(decl.Currency), unitAmount: *decl.UnitAmount, billingIntervalHours: decl.BillingIntervalHours,
			trialUnitAmount: decl.TrialUnitAmount, trialDurationHours: decl.TrialDurationHours}
		return terms, decl.References(), nil
	case models.RailNMI:
		ref = decl.PlanID
	case models.RailStripe:
		ref = decl.PriceID
	default:
		return providerPriceTerms{}, nil, apperr.Invalidf("PSP %q runs no provider prices", account.Key)
	}
	objects := s.providerObjects()
	if objects == nil {
		return providerPriceTerms{}, nil, fmt.Errorf("the provider catalog is not wired: %w", ErrProviderPriceUnreadable)
	}
	obj, err := objects.Get(ctx, account, providerobjects.Price, ref)
	if err != nil {
		return providerPriceTerms{}, nil, err
	}
	if obj == nil {
		if obj, err = objects.ObserveByID(ctx, account, providerobjects.Price, ref, nil, providerobjects.ViaRead); err != nil {
			return providerPriceTerms{}, nil, fmt.Errorf("read %s %q at %s: %v: %w", account.Rail, ref, account.Key, err, ErrProviderPriceUnreadable)
		}
	}
	if obj == nil {
		return providerPriceTerms{}, nil, apperr.Invalidf("%s %q does not exist at %s: a provider price names an existing one", providerObjectNoun(account.Rail), ref, account.Key)
	}
	return observedTerms(account, decl, obj)
}

func providerObjectNoun(rail string) string {
	if rail == string(models.RailNMI) {
		return "NMI plan"
	}
	return "Stripe price"
}

// observedTerms are the terms an observed NMI plan or Stripe price states.
// NMI names no currency: the declaration's, else USD, prices it. A deleted
// object's last stated terms still map: its grandfathered subscriptions land.
func observedTerms(account merchants.PSPScope, decl catalogwire.ApplyProviderPrice, obj *providerobjects.Object) (providerPriceTerms, map[string]string, error) {
	noun := providerObjectNoun(account.Rail)
	terms := providerPriceTerms{currency: obj.Currency}
	ids := map[string]string{models.RailKeyStripePriceID: obj.Ref}
	if models.Rail(account.Rail) == models.RailNMI {
		terms.currency = moneyutil.NormalizeCurrency(cmp.Or(decl.Currency, "USD"))
		ids = map[string]string{models.RailKeyPlanID: obj.Ref}
	} else if obj.ProductRef != "" {
		ids[models.RailKeyStripeProductID] = obj.ProductRef
	}
	if _, known := moneyutil.LookupCurrency(obj.Currency); obj.Amount == "" && obj.Currency != "" && !known {
		return terms, nil, apperr.Invalidf("%s %q is priced in %s, which OpenRails does not support", noun, obj.Ref, obj.Currency)
	}
	if obj.Amount == "" {
		return terms, nil, apperr.Invalidf("%s %q has no fixed amount: a provider price needs one", noun, obj.Ref)
	}
	minor, err := moneyutil.DecimalToRailMinor(terms.currency, obj.Amount)
	if err != nil {
		return terms, nil, apperr.Invalidf("%s %q: %v", noun, obj.Ref, err)
	}
	if terms.unitAmount, err = moneyutil.RailMinorToNative(terms.currency, minor); err != nil {
		return terms, nil, err
	}
	switch {
	case obj.IntervalMonths > 0:
		terms.billingIntervalMonths = new(obj.IntervalMonths)
	case obj.IntervalHours > 0:
		terms.billingIntervalHours = new(obj.IntervalHours)
	case models.Rail(account.Rail) == models.RailNMI:
		return terms, nil, apperr.Invalidf("NMI plan %q states no single billing frequency", obj.Ref)
	}
	return terms, ids, nil
}

func (s *Service) providerObjects() *providerobjects.Observer {
	if s == nil || s.rt == nil {
		return nil
	}
	return s.rt.ProviderObjects()
}

// providerPriceRequest is the price a provider price is stored as: the
// provider's terms, with access for the billing interval unless declared,
// and no renewal when one-off or not recurring at the provider.
func providerPriceRequest(product *models.Product, key string, decl catalogwire.ApplyProviderPrice, terms providerPriceTerms, archived bool) billing.CreatePriceParams {
	req := billing.CreatePriceParams{ProductID: billing.ProductID(product.ID), Key: key, UnitAmount: terms.unitAmount, Currency: terms.currency,
		BillingIntervalHours: terms.billingIntervalHours, TrialUnitAmount: terms.trialUnitAmount, TrialDurationHours: terms.trialDurationHours, Archived: archived}
	if decl.AccessDurationHours.Set && !decl.AccessDurationHours.Null {
		req.AccessDurationHours = new(decl.AccessDurationHours.Value)
	}
	if decl.OneOff {
		req.BillingIntervalHours, req.TrialUnitAmount, req.TrialDurationHours = nil, nil, nil
	}
	return req
}

// providerPriceID is a provider price's id: its terms', salted with the
// provider object it mirrors.
func providerPriceID(productID uuid.UUID, req billing.CreatePriceParams, pspID uuid.UUID, ids map[string]string, months *int, defaultAccess bool) uuid.UUID {
	parts := []string{"provider", pspID.String(), "calendar_months", optionalHours(months), "default_access", strconv.FormatBool(defaultAccess)}
	for _, k := range slices.Sorted(maps.Keys(ids)) {
		parts = append(parts, k, ids[k])
	}
	return uuidutil.DeterministicID(newPriceID(productID, req.Key, req), parts...)
}

// prepareProviderPrice resolves a declaration's PSP and reads its terms; it
// runs outside any transaction. current is the key's stored live (else
// newest) version. A provider that does not answer keeps current's terms
// when it mirrors the same object.
func (s *Service) prepareProviderPrice(ctx context.Context, accounts []merchants.PSPScope, product *models.Product, key string, decl catalogwire.ApplyProviderPrice, current *models.Price) (*providerPrice, error) {
	if current != nil && !current.ProviderOwned {
		return nil, apperr.Invalidf("price key %q is one of ours: a provider price takes a key of its own", key)
	}
	account, found, err := selectCatalogApplicationPSP(accounts, decl.PSP, nil)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, apperr.Invalidf("provider price %q names PSP %q, which the merchant does not declare", key, decl.PSP)
	}
	if err := checkProviderPriceRail(decl.PSP, account.Rail, decl); err != nil {
		return nil, err
	}
	archived := decl.Archived.Value
	if !decl.Archived.Set && current != nil {
		archived = current.Archived
	}
	out := &providerPrice{productKey: product.Key, key: key, account: account}
	terms, ids, err := s.readProviderPriceTerms(ctx, account, decl)
	if errors.Is(err, ErrProviderPriceUnreadable) && current != nil {
		if _, link, ok := current.ProviderLink(); ok && link[models.RailKeyPSPID] == account.ID.String() && sameProviderRefs(link, decl.References()) {
			out.cached = err.Error()
			terms = providerPriceTerms{unitAmount: current.Amount, currency: current.Currency, billingIntervalHours: current.ProviderBillingInterval(), billingIntervalMonths: current.ProviderBillingMonths(), trialUnitAmount: current.TrialUnitAmount, trialDurationHours: current.TrialDurationHours}
			ids = maps.Clone(link)
			err = nil
		}
	}
	if err != nil {
		return nil, err
	}
	out.request = providerPriceRequest(product, key, decl, terms, archived)
	out.months = terms.billingIntervalMonths
	if decl.OneOff {
		out.months = nil
	}
	out.accessDefault = !decl.AccessDurationHours.Set
	if err := validateCatalogPriceTerms(out.request); err != nil {
		return nil, err
	}
	out.link = map[string]string{models.RailKeyRail: account.Rail, models.RailKeyPSPID: account.ID.String()}
	for k, v := range ids {
		if k != models.RailKeyRail && k != models.RailKeyPSPID {
			out.link[k] = v
		}
	}
	out.link[models.RailKeyProviderBillingInterval] = optionalHours(terms.billingIntervalHours)
	out.link[models.RailKeyProviderBillingMonths] = optionalHours(terms.billingIntervalMonths)
	return out, nil
}

// sameProviderRefs reports a stored binding naming the declared references.
func sameProviderRefs(link, refs map[string]string) bool {
	return len(refs) > 0 && maps.Equal(providerRefs(link), refs)
}

// providerRefs are a binding's provider identifiers, the declaration's names.
func providerRefs(link map[string]string) map[string]string {
	out := map[string]string{}
	for _, k := range []string{models.RailKeyPlanID, models.RailKeyStripePriceID, models.RailKeyCCBillFlexID, models.RailKeyCCBillRecurringBillingOption} {
		if v := link[k]; v != "" {
			out[k] = v
		}
	}
	return out
}

// writeProviderPrice stores a prepared provider price: the version its terms
// name takes the key and its provider object, which leaves earlier versions.
// It reports whether anything changed.
func (s *Service) writeProviderPrice(ctx context.Context, product *models.Product, p *providerPrice) (*models.Price, bool, error) {
	prices, err := s.requirePriceService()
	if err != nil {
		return nil, false, err
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, false, err
	}
	pspID := p.account.ID
	id := providerPriceID(product.ID, p.request, pspID, providerRefs(p.link), p.months, p.accessDefault)
	links := map[string]map[string]string{p.account.Key: p.link}
	chain, err := prices.ListChainByKey(ctx, mid.UUID(), product.ID, p.key)
	if err != nil {
		return nil, false, err
	}
	for _, version := range chain {
		if !version.ProviderOwned {
			return nil, false, apperr.Invalidf("price key %q is one of ours: a provider price takes a key of its own", p.key)
		}
		if _, link, bound := version.ProviderLink(); bound && (link[models.RailKeyPSPID] != pspID.String() || !sameProviderRefs(link, providerRefs(p.link))) {
			return nil, false, apperr.Invalidf("provider price key %q names another provider object: use a new key for a different PSP or reference", p.key)
		}
		if samePriceTerms(version.View(), p.request) && optionalHours(version.BillingIntervalMonths) == optionalHours(p.months) && version.ProviderAccessDefault == p.accessDefault {
			// Populated upgrades preserve their existing identities, and old
			// terms reactivate their existing immutable financial row.
			id = version.ID
		}
	}
	for _, version := range chain {
		if version.ID == id && version.Archived == p.request.Archived && sameCatalogLinks(version.PSPLinks, links) {
			return version, false, nil
		}
	}
	released, err := s.providerObjectHolders(ctx, product.Key, p, chain, id)
	if err != nil {
		return nil, false, err
	}
	for _, version := range chain {
		if version.ID != id && len(version.PSPLinks) > 0 {
			released = append(released, version.ID)
		}
	}
	for _, holder := range released {
		if err := prices.UpdatePSPLinks(ctx, holder, nil); err != nil {
			return nil, false, err
		}
	}
	price, err := s.writeCatalogPrice(ctx, priceWrite{req: p.request, product: product, id: id, links: links, providerOwned: true, providerPSPID: &pspID, providerSnapshot: func() map[string]string {
		snapshot := maps.Clone(p.link)
		snapshot["psp_key"] = p.account.Key
		return snapshot
	}(), providerMonths: p.months, providerAccessDefault: p.accessDefault})
	if err != nil {
		return nil, false, err
	}
	if !sameCatalogLinks(price.PSPLinks, links) {
		if err := prices.UpdatePSPLinks(ctx, price.ID, links); err != nil {
			return nil, false, err
		}
	}
	price, err = prices.GetByID(ctx, price.ID)
	return price, true, err
}

// providerObjectHolders are the prices outside the key's chain still bound to
// its provider object: archived or deleted ones (an earlier key, a migrated
// split) give it up. A live one under another key is refused: one provider
// object has one mirror.
func (s *Service) providerObjectHolders(ctx context.Context, productKey string, p *providerPrice, chain []*models.Price, id uuid.UUID) ([]uuid.UUID, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	refs := providerRefs(p.link)
	ref := func(k string) *string {
		if v := refs[k]; v != "" {
			return &v
		}
		return nil
	}
	holders, err := s.catalogDatabase().Gen(ctx).ListProviderObjectHolders(ctx, gen.ListProviderObjectHoldersParams{MerchantID: mid.UUID(), PspID: p.account.ID,
		PlanID: ref(models.RailKeyPlanID), PriceRef: ref(models.RailKeyStripePriceID), RecurringBillingOptionID: ref(models.RailKeyCCBillRecurringBillingOption)})
	if err != nil {
		return nil, err
	}
	var released []uuid.UUID
	for _, h := range holders {
		if h.ID == id || slices.ContainsFunc(chain, func(v *models.Price) bool { return v.ID == h.ID }) {
			continue
		}
		if !h.Archived && !h.Deleted {
			return nil, fmt.Errorf("provider price %q of product %q: %s is already mirrored by live provider price %q of product %q; declare it under that key, or archive that key first: %w",
				p.key, productKey, providerObject(p.account.Key, p.link), h.Key, h.ProductKey, ErrCatalogConflict.WithParam(fmt.Sprintf("products.%s.provider_prices.%s", productKey, p.key)))
		}
		released = append(released, h.ID)
	}
	return released, nil
}

// CreateProviderPrice links a price a provider owns and runs to a product:
// its terms are read from the provider (declared for CCBill), and nothing is
// written to the provider.
func (s *Service) CreateProviderPrice(ctx context.Context, params billing.CreateProviderPriceParams) (*billing.Price, error) {
	decl := catalogwire.ApplyProviderPrice{PSP: params.PSP, PlanID: params.PlanID, PriceID: params.PriceID, FlexID: params.FlexID, RecurringBillingOptionID: params.RecurringBillingOptionID,
		Currency: params.Currency, UnitAmount: params.UnitAmount, BillingIntervalHours: params.BillingIntervalHours, AccessDurationHours: params.AccessDurationHours,
		TrialUnitAmount: params.TrialUnitAmount, TrialDurationHours: params.TrialDurationHours, OneOff: params.OneOff, Archived: catalogwire.Value(params.Archived)}
	if err := decl.Validate(); err != nil {
		return nil, apperr.Invalidf("%v", err)
	}
	if strings.TrimSpace(params.Key) == "" || strings.TrimSpace(params.Key) != params.Key {
		return nil, apperr.Invalidf("key required")
	}
	if params.ProductID.IsZero() == (params.ProductKey == "") {
		return nil, apperr.Invalidf("exactly one of product_id and product_key is required")
	}
	return catalogCommand(ctx, s, "provider-price/create", params, "", func(ctx context.Context, s *Service, state catalogState) ([]catalogObject, error) {
		product, err := s.providerPriceProduct(ctx, params)
		if err != nil {
			return nil, err
		}
		return []catalogObject{priceObject(product.Key, params.Key)}, nil
	}, func(ctx context.Context, s *Service) (*billing.Price, error) {
		product, err := s.providerPriceProduct(ctx, params)
		if err != nil {
			return nil, err
		}
		mid, err := merchant.Require(ctx)
		if err != nil {
			return nil, err
		}
		accounts, err := merchants.Of(s.catalogDatabase()).PSPScopes(ctx, mid)
		if err != nil {
			return nil, err
		}
		prices, err := s.requirePriceService()
		if err != nil {
			return nil, err
		}
		current, err := s.currentPriceVersion(ctx, prices, product.ID, params.Key)
		if err != nil {
			return nil, err
		}
		prepared, err := s.prepareProviderPrice(ctx, accounts, product, params.Key, decl, current)
		if err != nil {
			return nil, err
		}
		price, err := catalogMutation(ctx, s, func(ctx context.Context, scoped *Service) (*models.Price, error) {
			price, _, err := scoped.writeProviderPrice(ctx, product, prepared)
			return price, err
		})
		if err != nil {
			return nil, err
		}
		return priceToCatalogPrice(price), nil
	})
}

func (s *Service) providerPriceProduct(ctx context.Context, params billing.CreateProviderPriceParams) (*models.Product, error) {
	products, err := s.requireProductService()
	if err != nil {
		return nil, err
	}
	var product *models.Product
	if params.ProductKey != "" {
		product, err = products.GetByKey(ctx, params.ProductKey)
	} else {
		product, err = products.GetByID(ctx, params.ProductID.UUID())
	}
	if err != nil {
		return nil, productLookup(err)
	}
	return product, nil
}

// currentPriceVersion is a key's live version, else its newest; nil when
// the key holds none.
func (s *Service) currentPriceVersion(ctx context.Context, prices interface {
	ListChainByKey(context.Context, uuid.UUID, uuid.UUID, string) ([]*models.Price, error)
}, productID uuid.UUID, key string) (*models.Price, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	chain, err := prices.ListChainByKey(ctx, mid.UUID(), productID, key)
	if err != nil || len(chain) == 0 {
		return nil, err
	}
	for _, p := range chain {
		if !p.Archived {
			return p, nil
		}
	}
	return chain[len(chain)-1], nil
}

// verifyProviderPrice reads a provider price's object at its provider again,
// records it, and reports whether its terms still match the stored ones: an
// unreachable provider is unknown, a gone object missing and retired.
func (s *Service) verifyProviderPrice(ctx context.Context, price *models.Price, out *billing.PriceProvider) {
	if out == nil {
		return
	}
	key, link, ok := price.ProviderLink()
	if !ok {
		return
	}
	pspID, _ := uuid.Parse(link[models.RailKeyPSPID])
	mid, err := merchant.Require(ctx)
	if err != nil {
		return
	}
	account, found, err := merchants.Of(s.catalogDatabase()).PSPScopeByID(ctx, mid, pspID)
	if err != nil || !found || models.Rail(account.Rail) == models.RailCCBill {
		out.SyncStatus = billing.SyncStatusSyncDisabled
		return
	}
	decl := catalogwire.ApplyProviderPrice{PSP: key, PlanID: link[models.RailKeyPlanID], PriceID: link[models.RailKeyStripePriceID], Currency: price.Currency}
	ref := cmp.Or(decl.PlanID, decl.PriceID)
	objects := s.providerObjects()
	if objects == nil {
		out.SyncStatus = billing.SyncStatusUnknown
		return
	}
	// The price's stored terms record a gone object OpenRails never observed
	// (a price mapped before the catalog was observed), so it retires.
	stated := providerobjects.Stated(ref, price.Amount, price.Currency, models.Rail(account.Rail) == models.RailStripe, price.ProviderBillingInterval(), price.ProviderBillingMonths())
	obj, err := objects.ObserveByID(ctx, account, providerobjects.Price, ref, stated, providerobjects.ViaRead)
	if err != nil {
		out.SyncStatus = billing.SyncStatusUnknown
		return
	}
	if obj == nil || obj.DeletedAt != nil {
		out.SyncStatus, out.Retired = billing.SyncStatusMissing, true
		return
	}
	out.Retired = obj.Retired
	terms, _, err := observedTerms(account, decl, obj)
	if err != nil {
		out.SyncStatus = billing.SyncStatusMissing
		return
	}
	drift := []billing.DriftField{}
	if terms.unitAmount != price.Amount || terms.currency != price.Currency {
		drift = append(drift, billing.DriftField{Field: "unit_amount", OpenRailsValue: moneyutil.FormatAmount(price.Amount, price.Currency), RemoteValue: moneyutil.FormatAmount(terms.unitAmount, terms.currency)})
	}
	stored, remote := price.ProviderBillingInterval(), terms.billingIntervalHours
	if (stored == nil) != (remote == nil) || stored != nil && *stored != *remote {
		drift = append(drift, billing.DriftField{Field: "billing_interval_hours", OpenRailsValue: optionalHours(stored), RemoteValue: optionalHours(remote)})
	}
	if optionalHours(price.ProviderBillingMonths()) != optionalHours(terms.billingIntervalMonths) {
		drift = append(drift, billing.DriftField{Field: "billing_interval_months", OpenRailsValue: optionalHours(price.ProviderBillingMonths()), RemoteValue: optionalHours(terms.billingIntervalMonths)})
	}
	out.Drift = drift
	out.SyncStatus = billing.SyncStatusInSync
	if len(drift) > 0 {
		out.SyncStatus = billing.SyncStatusDrifted
	}
}

func optionalHours(h *int) string {
	if h == nil {
		return "none"
	}
	return strconv.Itoa(*h)
}
