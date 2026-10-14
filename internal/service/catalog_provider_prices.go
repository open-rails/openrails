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
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchants"
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
	cached string
}

// providerPriceTerms are a provider price's money terms.
type providerPriceTerms struct {
	unitAmount           int64
	currency             string
	billingIntervalHours *int
	trialUnitAmount      *int64
	trialDurationHours   *int
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

// readProviderPriceTerms reads the terms at the provider: the NMI plan, the
// Stripe price, or CCBill's declared terms. ids are the provider identifiers
// the price binds.
func (s *Service) readProviderPriceTerms(ctx context.Context, account merchants.PSPScope, decl catalogwire.ApplyProviderPrice) (providerPriceTerms, map[string]string, error) {
	var terms providerPriceTerms
	switch models.Rail(account.Rail) {
	case models.RailNMI:
		client, _, ok := s.nmiCatalogClient(ctx, account.AccountID)
		if !ok {
			return terms, nil, fmt.Errorf("NMI account %s is not armed: %w", account.Key, ErrProviderPriceUnreadable)
		}
		terms.currency = moneyutil.NormalizeCurrency(cmp.Or(decl.Currency, "USD"))
		detail, err := client.GetRecurringPlanDetailByID(ctx, decl.PlanID, terms.currency)
		if err != nil {
			return terms, nil, fmt.Errorf("read NMI plan %q: %v: %w", decl.PlanID, err, ErrProviderPriceUnreadable)
		}
		if !detail.Found {
			return terms, nil, apperr.Invalidf("NMI plan %q does not exist at %s: a provider price names an existing plan", decl.PlanID, account.Key)
		}
		if terms.unitAmount, err = moneyutil.RailMinorToNative(terms.currency, moneyutil.Cents(detail.AmountCents)); err != nil {
			return terms, nil, err
		}
		// A month-based plan counts 30 days a month.
		days := cmp.Or(detail.DayFrequency, detail.MonthFrequency*30)
		if days <= 0 {
			return terms, nil, apperr.Invalidf("NMI plan %q states no billing frequency", decl.PlanID)
		}
		hours := days * 24
		terms.billingIntervalHours = &hours
		return terms, map[string]string{models.RailKeyPlanID: decl.PlanID}, nil
	case models.RailStripe:
		reader, ok := s.stripeCatalogReader(ctx, account.AccountID)
		if !ok {
			return terms, nil, fmt.Errorf("Stripe account %s is not armed: %w", account.Key, ErrProviderPriceUnreadable)
		}
		remote, found, err := reader.FindPrice(ctx, decl.PriceID)
		if err != nil {
			return terms, nil, fmt.Errorf("read Stripe price %q: %v: %w", decl.PriceID, err, ErrProviderPriceUnreadable)
		}
		if !found || remote.ID != decl.PriceID {
			return terms, nil, apperr.Invalidf("Stripe price %q does not exist at %s: a provider price names an existing price", decl.PriceID, account.Key)
		}
		terms.currency = moneyutil.NormalizeCurrency(remote.Currency)
		if terms.unitAmount, err = moneyutil.RailMinorToNative(terms.currency, moneyutil.Cents(remote.UnitAmount)); err != nil {
			return terms, nil, err
		}
		if remote.Recurring != nil {
			days := stripeIntervalDays(remote.Recurring.Interval, remote.Recurring.Count)
			if days <= 0 {
				return terms, nil, apperr.Invalidf("Stripe price %q bills every %d %s, which OpenRails cannot measure", decl.PriceID, remote.Recurring.Count, remote.Recurring.Interval)
			}
			hours := days * 24
			terms.billingIntervalHours = &hours
		}
		ids := map[string]string{models.RailKeyStripePriceID: decl.PriceID}
		if remote.Product != "" {
			ids[models.RailKeyStripeProductID] = remote.Product
		}
		return terms, ids, nil
	case models.RailCCBill:
		terms.currency = moneyutil.NormalizeCurrency(decl.Currency)
		terms.unitAmount = *decl.UnitAmount
		terms.billingIntervalHours = decl.BillingIntervalHours
		terms.trialUnitAmount, terms.trialDurationHours = decl.TrialUnitAmount, decl.TrialDurationHours
		return terms, decl.References(), nil
	}
	return terms, nil, apperr.Invalidf("PSP %q runs no provider prices", account.Key)
}

// stripeIntervalDays is a Stripe recurring interval in days: a month is 30
// days, a year 365.
func stripeIntervalDays(interval string, count int) int {
	per := map[string]int{"day": 1, "week": 7, "month": 30, "year": 365}[interval]
	return per * max(count, 1)
}

// providerPriceRequest is the price a provider price is stored as: the
// provider's terms, with access for the billing interval unless declared,
// and no renewal when one-off or not recurring at the provider.
func providerPriceRequest(product *models.Product, key string, decl catalogwire.ApplyProviderPrice, terms providerPriceTerms, archived bool) billing.CreatePriceParams {
	req := billing.CreatePriceParams{ProductID: billing.ProductID(product.ID), Key: key, UnitAmount: terms.unitAmount, Currency: terms.currency,
		BillingIntervalHours: terms.billingIntervalHours, TrialUnitAmount: terms.trialUnitAmount, TrialDurationHours: terms.trialDurationHours, Archived: archived}
	req.AccessDurationHours = cmp.Or(decl.AccessDurationHours, terms.billingIntervalHours)
	if decl.OneOff {
		req.BillingIntervalHours, req.TrialUnitAmount, req.TrialDurationHours = nil, nil, nil
	}
	return req
}

// providerPriceID is a provider price's id: its terms', salted with the
// provider object it mirrors.
func providerPriceID(productID uuid.UUID, req billing.CreatePriceParams, pspID uuid.UUID, ids map[string]string) uuid.UUID {
	parts := []string{"provider", pspID.String()}
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
			terms = providerPriceTerms{unitAmount: current.Amount, currency: current.Currency, billingIntervalHours: current.ProviderBillingInterval(), trialUnitAmount: current.TrialUnitAmount, trialDurationHours: current.TrialDurationHours}
			ids = maps.Clone(link)
			err = nil
		}
	}
	if err != nil {
		return nil, err
	}
	out.request = providerPriceRequest(product, key, decl, terms, archived)
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
	return out, nil
}

// sameProviderRefs reports a stored binding naming the declared references.
func sameProviderRefs(link, refs map[string]string) bool {
	for k, v := range refs {
		if link[k] != v {
			return false
		}
	}
	return len(refs) > 0
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
	id := providerPriceID(product.ID, p.request, pspID, providerRefs(p.link))
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
		if samePriceTerms(version.View(), p.request) {
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
	for _, version := range chain {
		if version.ID != id && len(version.PSPLinks) > 0 {
			if err := prices.UpdatePSPLinks(ctx, version.ID, nil); err != nil {
				return nil, false, err
			}
		}
	}
	price, err := s.writeCatalogPrice(ctx, priceWrite{req: p.request, product: product, id: id, links: links, providerOwned: true})
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

// verifyProviderPrice reads a provider price's provider again and reports
// whether its terms still match the stored ones.
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
	if err != nil || !found {
		out.SyncStatus = billing.SyncStatusSyncDisabled
		return
	}
	if models.Rail(account.Rail) == models.RailCCBill {
		out.SyncStatus = billing.SyncStatusSyncDisabled
		return
	}
	decl := catalogwire.ApplyProviderPrice{PSP: key, PlanID: link[models.RailKeyPlanID], PriceID: link[models.RailKeyStripePriceID], Currency: price.Currency}
	if account.Rail == string(models.RailStripe) {
		decl.Currency = ""
	}
	terms, _, err := s.readProviderPriceTerms(ctx, account, decl)
	switch {
	case errors.Is(err, ErrProviderPriceUnreadable):
		out.SyncStatus = billing.SyncStatusUnknown
		return
	case err != nil:
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
