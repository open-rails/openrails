package service

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/billing"
	catalogwire "github.com/open-rails/openrails/catalog"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/currency"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/shared/apperr"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
)

func (s *Service) CreateProduct(ctx context.Context, req billing.CreateProductParams) (*billing.Product, error) {
	return catalogCommand(ctx, s, "product/"+req.Key, req, "", createProductPlan(req), func(ctx context.Context, s *Service) (*billing.Product, error) {
		return s.createProductCommand(ctx, req)
	})
}

func (s *Service) createProductCommand(ctx context.Context, req billing.CreateProductParams) (*billing.Product, error) {
	return catalogMutation(ctx, s, func(ctx context.Context, scoped *Service) (*billing.Product, error) {
		return scoped.createProduct(ctx, req)
	})
}

func (s *Service) createProduct(ctx context.Context, req billing.CreateProductParams) (*billing.Product, error) {
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return nil, pinErr
	}
	defer release()

	products, err := s.requireProductService()
	if err != nil {
		return nil, err
	}
	req.Key = strings.TrimSpace(req.Key)
	req.DisplayName = strings.TrimSpace(req.DisplayName)
	req.Description = strings.TrimSpace(req.Description)
	if req.Key == "" {
		return nil, apperr.Invalidf("key required")
	}
	if req.DisplayName == "" {
		return nil, apperr.Invalidf("display_name required")
	}

	credit, err := normalizeCreditGrant(req.CreditGrant)
	if err != nil {
		return nil, err
	}
	req.CreditGrant = credit
	now := time.Now().UTC()
	tid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	p := &models.Product{
		// #662: the product id is a pure function of its immutable natural key
		// (merchant_id, key) — same logical product → same id in every DB.
		ID:           uuidutil.DeterministicID(uuidutil.DeterministicNamespace, tid.UUID().String(), req.Key),
		MerchantID:   tid.UUID(),
		Key:          req.Key,
		DisplayName:  req.DisplayName,
		Description:  req.Description,
		Entitlements: req.Entitlements, CreditGrant: req.CreditGrant,
		RequiredEntitlements: req.RequiredEntitlements,
		TierGroup:            req.TierGroup,
		TierRank:             req.TierRank,
		Ownership:            req.Ownership,
		Archived:             req.Archived,
		CreatedAt:            now,
		UpdatedAt:            now,
	}
	if err := products.Create(ctx, p, s.keyEdit(ctx)); err != nil {
		return nil, catalogWrite(err)
	}
	// The database assigns the revision.
	created, err := products.GetByID(ctx, p.ID)
	if err != nil {
		return nil, productLookup(err)
	}
	return productToCatalogProduct(created), nil
}

// ErrProductTierGroupInUse reports a product identity conflict with live subscriptions.
var ErrProductTierGroupInUse = catalog.ErrProductTierGroupInUse

// UpdateProductRequest is the engine's product patch: a nil field is left
// as it is, and a Set flag writes its nullable field (nil clears it). An
// empty description clears it. SkipRailSync keeps the change local; the
// catalog application reconciles PSPs itself.
type UpdateProductRequest struct {
	CreditGrant    *catalogwire.CreditGrantSpec
	SetCreditGrant bool
	// DeferCreditPriceValidation is used only by an atomic catalog batch, which
	// validates the final product and live prices after applying every edit.
	DeferCreditPriceValidation bool
	DisplayName                *string
	Description                *string
	Entitlements               []string
	SetEntitlements            bool
	RequiredEntitlements       []string
	SetRequiredEntitlements    bool
	TierGroup                  *string
	SetTierGroup               bool
	TierRank                   *int
	Ownership                  catalogwire.Ownership
	SetOwnership               bool
	Archived                   *bool
	SkipRailSync               bool
	// ExpectedRevision refuses the edit unless the product is at it.
	ExpectedRevision *int64
}

// productPatch reads a merge patch: null clears description and tier_group.
// Entitlements and required entitlements must be lists; [] clears one and
// omission leaves it unchanged.
func productPatch(p billing.UpdateProductParams) (UpdateProductRequest, error) {
	var req UpdateProductRequest
	if p.DisplayName.Null || p.TierRank.Null || p.Archived.Null {
		return req, apperr.Invalidf("display_name, tier_rank and archived cannot be null")
	}
	if p.DisplayName.Set {
		req.DisplayName = &p.DisplayName.Value
	}
	if p.Description.Set {
		req.Description = &p.Description.Value
	}
	if p.Entitlements.Set {
		if p.Entitlements.Null || p.Entitlements.Value == nil {
			return req, apperr.Invalidf("entitlements must be a string list, not null; use [] for none")
		}
		req.SetEntitlements, req.Entitlements = true, p.Entitlements.Value
	}
	if p.RequiredEntitlements.Set {
		if p.RequiredEntitlements.Null || p.RequiredEntitlements.Value == nil {
			return req, apperr.Invalidf("required_entitlements must be a string list, not null; use [] for none")
		}
		req.SetRequiredEntitlements, req.RequiredEntitlements = true, p.RequiredEntitlements.Value
	}
	if p.CreditGrant.Set {
		req.SetCreditGrant = true
		if !p.CreditGrant.Null {
			req.CreditGrant = &p.CreditGrant.Value
		}
	}
	if p.TierGroup.Set {
		req.SetTierGroup = true
		if !p.TierGroup.Null {
			req.TierGroup = &p.TierGroup.Value
		}
	}
	if p.TierRank.Set {
		req.TierRank = &p.TierRank.Value
	}
	if p.Ownership.Set {
		req.SetOwnership, req.Ownership = true, p.Ownership.Value
	}
	if p.Archived.Set {
		req.Archived = &p.Archived.Value
	}
	req.ExpectedRevision = p.ExpectedRevision
	return req, nil
}

// UpdateProduct applies a merge patch to a product.
func (s *Service) UpdateProduct(ctx context.Context, id billing.ProductID, params billing.UpdateProductParams) (*billing.Product, error) {
	req, err := productPatch(params)
	if err != nil {
		return nil, err
	}
	return s.patchProduct(ctx, id, req)
}

func (s *Service) patchProduct(ctx context.Context, id billing.ProductID, req UpdateProductRequest) (*billing.Product, error) {
	return catalogCommand(ctx, s, "product/"+id.String(), req, "", productEditPlan(id, req), func(ctx context.Context, s *Service) (*billing.Product, error) {
		return catalogMutation(ctx, s, func(ctx context.Context, scoped *Service) (*billing.Product, error) {
			return scoped.updateProduct(ctx, id, req)
		})
	})
}

func (s *Service) updateProduct(ctx context.Context, id billing.ProductID, req UpdateProductRequest) (*billing.Product, error) {
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return nil, pinErr
	}
	defer release()

	products, err := s.requireProductService()
	if err != nil {
		return nil, err
	}
	if id.IsZero() {
		return nil, apperr.Invalidf("product_id required")
	}
	if req.ExpectedRevision != nil {
		current, err := products.GetByID(ctx, id.UUID())
		if err != nil {
			return nil, productLookup(err)
		}
		if err := checkRevision("product "+current.Key, req.ExpectedRevision, current.Revision); err != nil {
			return nil, err
		}
	}
	if req.SetCreditGrant {
		credit, err := normalizeCreditGrant(req.CreditGrant)
		if err != nil {
			return nil, err
		}
		req.CreditGrant = credit
		current, err := products.GetByID(ctx, id.UUID())
		if err != nil {
			return nil, productLookup(err)
		}
		if (current.CreditGrant == nil) != (credit == nil) {
			prices, err := s.ListPricesByProduct(ctx, id, false)
			if err != nil {
				return nil, err
			}
			for _, price := range prices {
				if price.BillingIntervalHours != nil {
					return nil, apperr.Invalidf("a product with recurring prices cannot change between service credit and access; create a new product")
				}
			}
		}
	}
	if !req.DeferCreditPriceValidation && (req.SetCreditGrant || req.Archived != nil && !*req.Archived) {
		current, err := products.GetByID(ctx, id.UUID())
		if err != nil {
			return nil, productLookup(err)
		}
		credit, archived := current.CreditGrant, current.Archived
		if req.SetCreditGrant {
			credit = req.CreditGrant
		}
		if req.Archived != nil {
			archived = *req.Archived
		}
		if !archived {
			if err := s.validateProductCreditUpdate(ctx, id, credit); err != nil {
				return nil, err
			}
		}
	}
	productID := id.UUID()
	p, keys, err := products.UpdateDefinition(ctx, productID, catalog.ProductDefinitionUpdateParams{
		DisplayName:     req.DisplayName,
		Description:     req.Description,
		Entitlements:    req.Entitlements,
		SetEntitlements: req.SetEntitlements, CreditGrant: req.CreditGrant, SetCreditGrant: req.SetCreditGrant,
		RequiredEntitlements: req.RequiredEntitlements, SetRequiredEntitlements: req.SetRequiredEntitlements,
		TierGroup:    req.TierGroup,
		SetTierGroup: req.SetTierGroup,
		TierRank:     req.TierRank,
		Ownership:    req.Ownership,
		SetOwnership: req.SetOwnership,
		Archived:     req.Archived,
		KeyEdit:      s.keyEdit(ctx),
	})
	if err != nil {
		return nil, productLookup(err)
	}
	if keys.Changed() {
		change := entitlementChange(p, keys)
		mid, err := merchant.Require(ctx)
		if err != nil {
			return nil, err
		}
		if err := announceKeyChanges(ctx, s.catalogDatabase().Gen(ctx), mid.UUID(), s.keyEdit(ctx).At, []*billing.EntitlementChange{&change}); err != nil {
			return nil, err
		}
		if s.catalogKeyChanges != nil {
			*s.catalogKeyChanges = append(*s.catalogKeyChanges, change)
		}
	}

	return productToCatalogProduct(p), nil
}

func productToCatalogProduct(p *models.Product) *billing.Product {
	v := p.View()
	return &v
}

// priceNaturalKeyNull is the canonical encoding of a SQL NULL price field for
// id derivation (#662). The prices_product_amount_window_key index is
// NULLS NOT DISTINCT (a NULL equals another NULL), so every absent value must
// hash to ONE fixed token — and it carries a NUL byte, which a decimal integer
// string can never contain, so it can never collide with a present value.
const priceNaturalKeyNull = "\x00null"

// priceDeterministicID derives a price's id from the immutable financial tuple
// that IS its identity — exactly the prices_product_amount_window_key
// columns (#662), canonicalized the SAME way that NULLS-NOT-DISTINCT unique
// index compares them (amount as decimal, currency lowercased, absent nullable
// fields → priceNaturalKeyNull). Equal terms therefore always hash equal, so a
// derived id can never violate the constraint; a reprice changes a frozen field
// and correctly hashes to a new id while the archived old row keeps its own.
func priceDeterministicID(productID uuid.UUID, key string, amount int64, currency string, accessDurationHours, billingIntervalHours *int, trialUnitAmount *int64, trialDurationHours *int) uuid.UUID {
	accessDur := priceNaturalKeyNull
	if accessDurationHours != nil {
		accessDur = strconv.Itoa(*accessDurationHours)
	}
	billingInterval := priceNaturalKeyNull
	if billingIntervalHours != nil {
		billingInterval = strconv.Itoa(*billingIntervalHours)
	}
	trialAmt := priceNaturalKeyNull
	if trialUnitAmount != nil {
		trialAmt = strconv.FormatInt(*trialUnitAmount, 10)
	}
	trialDur := priceNaturalKeyNull
	if trialDurationHours != nil {
		trialDur = strconv.Itoa(*trialDurationHours)
	}
	return uuidutil.DeterministicID(
		uuidutil.DeterministicNamespace,
		productID.String(), key,
		strconv.FormatInt(amount, 10),
		strings.ToLower(currency),
		accessDur,
		billingInterval,
		trialAmt,
		trialDur,
	)
}

// newPriceID is the id a price with req's complete terms at key takes when no
// stored price already holds them.
func newPriceID(productID uuid.UUID, key string, req billing.CreatePriceParams) uuid.UUID {
	id := priceDeterministicID(productID, key, req.UnitAmount, req.Currency, req.AccessDurationHours, req.BillingIntervalHours, req.TrialUnitAmount, req.TrialDurationHours)
	if req.CustomerAmount != nil {
		id = uuidutil.DeterministicID(id, "customer_amount", strconv.FormatInt(req.CustomerAmount.MinAmount, 10), strconv.FormatInt(req.CustomerAmount.MaxAmount, 10))
	}
	if req.Quantity != nil {
		id = uuidutil.DeterministicID(id, "quantity", strconv.Itoa(req.Quantity.Min), strconv.Itoa(req.Quantity.Max))
	}
	return id
}

// priceRequestCycleDays returns the billing cadence in days, independently of
// the access window. A nil interval means a one-time price.
func priceRequestCycleDays(req billing.CreatePriceParams) *int {
	if req.BillingIntervalHours == nil {
		return nil
	}
	days := *req.BillingIntervalHours / 24
	return &days
}

func (s *Service) CreatePrice(ctx context.Context, req billing.CreatePriceParams) (*billing.Price, error) {
	return catalogCommand(ctx, s, "price/create", req, "", createPricePlan(req), func(ctx context.Context, s *Service) (*billing.Price, error) { return s.createPriceCommand(ctx, req) })
}

func (s *Service) createPriceCommand(ctx context.Context, req billing.CreatePriceParams) (*billing.Price, error) {
	selectors := 0
	if !req.ProductID.IsZero() {
		selectors++
	}
	if req.ProductKey != "" {
		selectors++
		if strings.TrimSpace(req.ProductKey) == "" {
			return nil, apperr.Invalidf("product_key is invalid")
		}
	}
	if req.ProductData != nil {
		selectors++
	}
	if selectors != 1 {
		return nil, apperr.Invalidf("exactly one of product_id, product_key and product_data is required")
	}
	if req.ProductData != nil {
		return catalogMutation(ctx, s, func(ctx context.Context, scoped *Service) (*billing.Price, error) {
			return scoped.createPrice(ctx, req)
		})
	}
	return s.createPrice(ctx, req)
}

func (s *Service) createPrice(ctx context.Context, req billing.CreatePriceParams) (*billing.Price, error) {
	if req.ProductData != nil {
		return s.createPriceWithProduct(ctx, req)
	}
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return nil, pinErr
	}
	defer release()

	products, prices, err := s.requireCatalogServices()
	if err != nil {
		return nil, err
	}
	if req.ProductID.IsZero() && req.ProductKey == "" {
		return nil, apperr.Invalidf("product_id required")
	}
	// CUR-6: canonicalise at the price WRITE boundary. currency.ForPrice below
	// is case-insensitive, so without this a caller-supplied "usd" validated
	// fine and then failed the prices_currency_check CHECK at INSERT.
	req.Currency = money.NormalizeCurrency(req.Currency)
	defaultRecurringAccess(&req)
	if err := validateCatalogPriceTerms(req); err != nil {
		return nil, err
	}

	var product *models.Product
	if req.ProductKey != "" {
		product, err = products.GetByKey(ctx, req.ProductKey)
	} else {
		product, err = products.GetByID(ctx, req.ProductID.UUID())
	}
	if err != nil {
		return nil, productLookup(err)
	}
	req.ProductID = billing.ProductID(product.ID)
	if err := validateCreditPrice(product.CreditGrant, req); err != nil {
		return nil, err
	}

	// #662: the price id is a pure function of its immutable financial tuple —
	// exactly the prices_product_amount_window_key columns. A reprice hashes
	// to a NEW id (the archived old row keeps its own); equal terms always hash
	// equal, so the id can never violate that unique constraint.
	key, _ := resolvePriceKey(product, req)
	priceID := newPriceID(req.ProductID.UUID(), key, req)
	existing, err := prices.FindByTerms(ctx, req, key)
	if err == nil {
		priceID = existing.ID
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}

	if chain, err := prices.ListChainByKey(ctx, product.MerchantID, product.ID, key); err != nil {
		return nil, err
	} else if len(chain) > 0 && chain[0].ProviderOwned {
		return nil, apperr.Invalidf("price key %q is a provider price; create ours under a key of its own", key)
	}
	rails := map[string]map[string]string{}
	if prepared, ok := s.catalogPreparedLinks[[2]string{product.Key, req.Key}]; s.localCatalogOnly && ok {
		rails = cloneRails(prepared)
	} else if rails, err = s.createPriceLinks(ctx, req, priceID, existing); err != nil {
		return nil, err
	}

	price, err := catalogMutation(ctx, s, func(ctx context.Context, scoped *Service) (*models.Price, error) {
		products, err := scoped.requireProductService()
		if err != nil {
			return nil, err
		}
		current, err := products.GetByID(ctx, product.ID)
		if err != nil {
			return nil, productLookup(err)
		}
		if !reflect.DeepEqual(productToCatalogProduct(current), productToCatalogProduct(product)) {
			return nil, ErrCatalogConflict
		}
		if err := scoped.expectCatalogRevision(priceObject(product.Key, key), req.ExpectedRevision); err != nil {
			return nil, err
		}
		price, err := scoped.writeCatalogPrice(ctx, priceWrite{req: req, product: current, id: priceID, links: rails})
		if err != nil {
			return nil, err
		}
		prices, err := scoped.requirePriceService()
		if err != nil {
			return nil, err
		}
		if !sameCatalogLinks(price.PSPLinks, rails) {
			if err := prices.UpdatePSPLinks(ctx, price.ID, rails); err != nil {
				return nil, err
			}
		}
		return prices.GetByID(ctx, price.ID)
	})
	if err != nil {
		return nil, err
	}
	return priceToCatalogPrice(price), nil
}

// createPriceLinks are the bindings a price of ours created outside a catalog
// application takes: its stored version's, else the Solana plan a recurring
// price sells on, published when missing. A price that sells only on Solana
// and cannot is refused; otherwise it sells without Solana.
func (s *Service) createPriceLinks(ctx context.Context, req billing.CreatePriceParams, priceID uuid.UUID, existing *models.Price) (map[string]map[string]string, error) {
	links := map[string]map[string]string{}
	if existing != nil {
		maps.Copy(links, cloneRails(existing.PSPLinks))
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	accounts, err := merchants.Of(s.catalogDatabase()).PSPScopes(ctx, mid)
	if err != nil {
		return nil, err
	}
	if why := sellViaRefusal(req, accounts); why != "" && !req.Archived {
		return nil, fmt.Errorf("%w: %s", ErrPriceNotSellable, why)
	}
	account := solanaPSP(accounts)
	solanaOnly := len(req.SellVia) == 1 && req.SellVia[0] == catalogwire.SellViaSolana
	if req.Archived || req.BillingIntervalHours == nil || account == nil || !sellsVia(req.SellVia, catalogwire.SellViaSolana) || len(models.PSPLinksOnRail(links, models.RailSolana)) > 0 {
		return links, nil
	}
	if why := solanaPlanRefusal(req); why != "" {
		if solanaOnly {
			return nil, fmt.Errorf("%w: it sells only on Solana, and %s", ErrPriceNotSellable, why)
		}
		return links, nil
	}
	link, err := s.solanaPlanLink(ctx, solanaAccount{key: account.Key, id: account.ID}, req, priceID, nil)
	if err != nil {
		if solanaOnly {
			return nil, fmt.Errorf("%w: it sells only on Solana, and its plan is not published: %v", ErrPriceNotSellable, err)
		}
		log.WithContext(ctx).WithError(err).WithField("price_key", req.Key).Warn("catalog: price is not sold on Solana; its plan is not published")
		return links, nil
	}
	links[account.Key] = link
	return links, nil
}

// UpdatePriceRequest is the engine's price patch: nil fields are left as
// they are; SetSellVia writes SellVia (nil: every method).
type UpdatePriceRequest struct {
	Archived   *bool
	SellVia    []string
	SetSellVia bool
	// ExpectedRevision refuses the edit unless the price key is at it.
	ExpectedRevision *int64
}

// pricePatch reads a merge patch: a null sell_via sells through every method.
func pricePatch(p billing.UpdatePriceParams) (UpdatePriceRequest, error) {
	var req UpdatePriceRequest
	if p.Archived.Null {
		return req, apperr.Invalidf("archived cannot be null")
	}
	if p.Archived.Set {
		req.Archived = &p.Archived.Value
	}
	if p.SellVia.Set {
		if err := catalogwire.ValidateSellVia(p.SellVia.Value); err != nil {
			return req, apperr.Invalidf("%v", err)
		}
		req.SetSellVia, req.SellVia = true, p.SellVia.Value
	}
	req.ExpectedRevision = p.ExpectedRevision
	return req, nil
}

// UpdatePrice applies a merge patch to a price.
func (s *Service) UpdatePrice(ctx context.Context, id billing.PriceID, params billing.UpdatePriceParams) (*billing.Price, error) {
	return catalogCommand(ctx, s, "price/"+id.String(), params, "", func(ctx context.Context, s *Service, state catalogState) ([]catalogObject, error) {
		req, err := pricePatch(params)
		if err != nil {
			return nil, err
		}
		return priceEditPlan(id, req)(ctx, s, state)
	}, func(ctx context.Context, s *Service) (*billing.Price, error) {
		return s.updatePriceCommand(ctx, id, params)
	})
}

func (s *Service) updatePriceCommand(ctx context.Context, id billing.PriceID, params billing.UpdatePriceParams) (*billing.Price, error) {
	req, err := pricePatch(params)
	if err != nil {
		return nil, err
	}
	return s.patchPrice(ctx, id, req)
}

func (s *Service) patchPrice(ctx context.Context, id billing.PriceID, req UpdatePriceRequest) (*billing.Price, error) {
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return nil, pinErr
	}
	defer release()

	return s.updatePrice(ctx, id, req)
}

func (s *Service) updatePrice(ctx context.Context, id billing.PriceID, req UpdatePriceRequest) (*billing.Price, error) {
	prices, err := s.requirePriceService()
	if err != nil {
		return nil, err
	}
	if id.IsZero() {
		return nil, apperr.Invalidf("price_id required")
	}
	existing, err := prices.GetByPublicID(ctx, id)
	if err != nil {
		return nil, priceLookup(err)
	}
	if req.SetSellVia && existing.ProviderOwned {
		return nil, apperr.Invalidf("a provider price sells through its provider; sell_via is for ours")
	}
	priceID := existing.ID
	updated, err := catalogMutation(ctx, s, func(ctx context.Context, scoped *Service) (*models.Price, error) {
		prices, err := scoped.requirePriceService()
		if err != nil {
			return nil, err
		}
		current, err := prices.GetByID(ctx, priceID)
		if err != nil {
			return nil, priceLookup(err)
		}
		if err := checkRevision("price key "+current.Key, req.ExpectedRevision, current.KeyRevision); err != nil {
			return nil, err
		}
		if !reflect.DeepEqual(current, existing) {
			return nil, ErrCatalogConflict
		}
		if req.SetSellVia && !slices.Equal(current.SellVia, req.SellVia) {
			if err := prices.SetSellVia(ctx, priceID, req.SellVia); err != nil {
				return nil, priceLookup(err)
			}
		}
		if req.Archived != nil {
			lifecycle := *scoped
			lifecycle.localCatalogOnly = true
			if *req.Archived {
				_, err = lifecycle.deactivatePrice(ctx, id)
			} else {
				_, err = lifecycle.activatePrice(ctx, id)
			}
			if err != nil {
				return nil, err
			}
		}
		return prices.GetByID(ctx, priceID)
	})
	if err != nil {
		return nil, err
	}
	return priceToCatalogPrice(updated), nil
}

// priceToCatalogPrice maps the DB row into the response shape. A provider
// price's sync status is unknown until a verifying read fills it.
func priceToCatalogPrice(p *models.Price) *billing.Price {
	v := p.View()
	return &v
}

// defaultRecurringAccess gives a recurring price without access its billing
// interval: a subscription's access never runs forever.
func defaultRecurringAccess(req *billing.CreatePriceParams) {
	if req.BillingIntervalHours != nil && req.AccessDurationHours == nil {
		hours := *req.BillingIntervalHours
		req.AccessDurationHours = &hours
	}
}

func validateCatalogPriceTerms(req billing.CreatePriceParams) error {
	if err := catalogwire.ValidateSellVia(req.SellVia); err != nil {
		return apperr.Invalidf("%v", err)
	}
	if req.CustomerAmount != nil {
		if req.UnitAmount != 0 || req.BillingIntervalHours != nil || req.AccessDurationHours != nil || req.TrialUnitAmount != nil || req.TrialDurationHours != nil {
			return apperr.Invalidf("customer_amount requires a one-off price with unit_amount zero and no access or trial duration")
		}
		if req.CustomerAmount.MinAmount <= 0 || req.CustomerAmount.MaxAmount < req.CustomerAmount.MinAmount {
			return apperr.Invalidf("customer_amount requires positive inclusive min_amount and max_amount")
		}
		for _, amount := range []int64{req.CustomerAmount.MinAmount, req.CustomerAmount.MaxAmount} {
			if _, err := moneyutil.NativeToRailMinorExact(req.Currency, amount); err != nil {
				return apperr.Invalidf("customer_amount: %v", err)
			}
		}
	}
	if req.Quantity != nil {
		if req.BillingIntervalHours == nil || req.CustomerAmount != nil {
			return apperr.Invalidf("quantity sells a recurring price per seat; a one-off price has none")
		}
		if err := req.Quantity.Validate(); err != nil {
			return apperr.Invalidf("%v", err)
		}
	}
	if req.UnitAmount < 0 {
		return apperr.Invalidf("unit_amount must be non-negative")
	}
	if req.Currency == "" {
		return apperr.Invalidf("currency required")
	}
	// Access and billing are independent. Every finite value must fit the time
	// arithmetic used for access grants and scheduled billing.
	for _, duration := range []struct {
		name  string
		hours *int
	}{
		{"access_duration_hours", req.AccessDurationHours},
		{"billing_interval_hours", req.BillingIntervalHours},
		{"trial_duration_hours", req.TrialDurationHours},
	} {
		if duration.hours != nil && (*duration.hours <= 0 || *duration.hours > catalogwire.MaxDurationHours) {
			return apperr.Invalidf("%s must be between 1 and %d hours or null", duration.name, catalogwire.MaxDurationHours)
		}
	}
	// #622 trial: both-or-neither; non-negative amount (0 = free trial); positive
	// period; only on a recurring price (there is a "then recurring" part).
	if (req.TrialUnitAmount == nil) != (req.TrialDurationHours == nil) {
		return apperr.Invalidf("trial_unit_amount and trial_duration_hours must be set together")
	}
	if req.TrialUnitAmount != nil {
		if *req.TrialUnitAmount < 0 {
			return apperr.Invalidf("trial_unit_amount must be >= 0 (0 = free trial)")
		}
		if req.BillingIntervalHours == nil {
			return apperr.Invalidf("trial pricing requires billing_interval_hours")
		}
	}
	if _, err := currency.ForPrice(req.Currency); err != nil {
		return apperr.Invalidf("%v", err)
	}
	return nil
}

// samePriceTerms compares stored terms instead of assuming how an imported ID
// was minted. Financial identity includes the product-local key.
func samePriceTerms(p billing.Price, req billing.CreatePriceParams) bool {
	return p.ProductID == req.ProductID && p.Key == req.Key && p.UnitAmount == req.UnitAmount && strings.EqualFold(p.Currency, req.Currency) &&
		reflect.DeepEqual(p.BillingIntervalHours, req.BillingIntervalHours) && reflect.DeepEqual(p.AccessDurationHours, req.AccessDurationHours) &&
		reflect.DeepEqual(p.TrialUnitAmount, req.TrialUnitAmount) && reflect.DeepEqual(p.TrialDurationHours, req.TrialDurationHours) && reflect.DeepEqual(p.CustomerAmount, req.CustomerAmount) &&
		reflect.DeepEqual(p.Quantity, req.Quantity)
}

// errQuantityRail refuses a per-seat price on a rail that cannot bill seats.
const errQuantityRail = "quantity requires Stripe or NMI billed by OpenRails, without provider catalog links: provider schedules, CCBill and Solana bill one amount per period"
