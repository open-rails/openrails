package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/billing"
	catalogwire "github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchants"
	railreg "github.com/open-rails/openrails/internal/modules/payments/rails"
	"github.com/open-rails/openrails/internal/shared/apperr"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
)

// catalogApplicationPreparation is what a document needs from outside the
// catalog transaction: the Solana plans its recurring prices sell on, its
// provider prices' terms read from their providers, and what it could not do.
type catalogApplicationPreparation struct {
	replay   *billing.CatalogApplicationReceipt
	revision int64
	// links are our prices' bindings: Solana plans, and those of superseded
	// PSPs kept by id.
	links     map[[2]string]map[string]map[string]string
	providers map[[2]string]*providerPrice
	accounts  map[uuid.UUID]merchants.PSPScope
	warnings  []billing.CatalogWarning
	refused   map[[2]string]bool
}

// solanaPlanCheck is a recurring price of ours whose Solana plan the
// preparation finds or publishes.
type solanaPlanCheck struct {
	productKey, key string
	request         billing.CreatePriceParams
	priceID         uuid.UUID
	solanaOnly      bool
}

// providerPriceCheck is a declared provider price whose terms the
// preparation reads.
type providerPriceCheck struct {
	product *models.Product
	key     string
	decl    catalogwire.ApplyProviderPrice
	current *models.Price
}

func (p *catalogApplicationPreparation) warn(productKey, key string, refused bool, format string, args ...any) {
	p.warnings = append(p.warnings, billing.CatalogWarning{Key: key, ProductKey: productKey, Refused: refused, Message: fmt.Sprintf(format, args...)})
	if refused {
		p.refused[[2]string{productKey, key}] = true
	}
}

// catalogApplicationGate runs under the merchant lock. An already committed
// content hash replays forever, even after other catalog writes.
func (s *Service) catalogApplicationGate(ctx context.Context, digest [32]byte) (int64, *billing.CatalogApplicationReceipt, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return 0, nil, err
	}
	q := s.catalogDatabase().Gen(ctx)
	revision, err := q.GetCatalogRevision(ctx, mid.UUID())
	if err != nil {
		return 0, nil, err
	}
	previous, err := q.GetCatalogApplicationByHash(ctx, gen.GetCatalogApplicationByHashParams{MerchantID: mid.UUID(), RequestSha256: digest[:]})
	if errors.Is(err, pgx.ErrNoRows) {
		return revision, nil, nil
	}
	if err != nil {
		return 0, nil, err
	}
	var receipt billing.CatalogApplicationReceipt
	if err := json.Unmarshal(previous.Result, &receipt); err != nil {
		return 0, nil, err
	}
	receipt.Replayed = true
	receipt.EntitlementChanges = []billing.EntitlementChange{}
	return revision, &receipt, nil
}

// prepareCatalogApplication reads what the document needs from outside the
// catalog: its provider prices' terms, and the Solana plans of its recurring
// prices, published when missing. Snapshot collection and the final commit
// each fence the merchant, but no transaction spans provider I/O, so a failed
// read or publish leaves the catalog untouched. A committed retry returns
// before contacting a provider.
func (s *Service) prepareCatalogApplication(ctx context.Context, params catalogwire.Application, digest [32]byte) (*catalogApplicationPreparation, error) {
	var solanaChecks []solanaPlanCheck
	var providerChecks []providerPriceCheck
	var accounts []merchants.PSPScope
	prepared, err := catalogReadLocked(ctx, s, func(ctx context.Context, scoped *Service) (*catalogApplicationPreparation, error) {
		revision, replay, err := scoped.catalogApplicationGate(ctx, digest)
		if err != nil {
			return nil, err
		}
		out := &catalogApplicationPreparation{replay: replay, revision: revision, links: map[[2]string]map[string]map[string]string{},
			providers: map[[2]string]*providerPrice{}, accounts: map[uuid.UUID]merchants.PSPScope{}, warnings: []billing.CatalogWarning{}, refused: map[[2]string]bool{}}
		if replay != nil {
			return out, nil
		}
		mid, err := merchant.Require(ctx)
		if err != nil {
			return nil, err
		}
		if accounts, err = merchants.Of(scoped.catalogDatabase()).PSPScopes(ctx, mid); err != nil {
			return nil, err
		}
		for _, productKey := range slices.Sorted(maps.Keys(params.Products)) {
			declared := params.Products[productKey]
			product, err := scoped.GetProductByKey(ctx, productKey)
			if err != nil && !errors.Is(err, billing.ErrNotFound) {
				return nil, err
			}
			if product == nil {
				if !declared.DisplayName.Set {
					return nil, apperr.Invalidf("new product %q requires display_name; cannot archive unknown product", productKey)
				}
				product = &billing.Product{ID: billing.ProductID(uuidutil.DeterministicID(uuidutil.DeterministicNamespace, mid.UUID().String(), productKey)), Key: productKey, DisplayName: declared.DisplayName.Value}
			}
			if declared.CreditGrant.Set {
				product.CreditGrant = nil
				if !declared.CreditGrant.Null {
					product.CreditGrant = &declared.CreditGrant.Value
				}
			}
			credit, err := normalizeCreditGrant(product.CreditGrant)
			if err != nil {
				return nil, err
			}
			product.CreditGrant = credit
			if declared.Archived.Set {
				product.Archived = declared.Archived.Value
			}
			prices, err := scoped.ListPricesByProduct(ctx, product.ID, false)
			if err != nil {
				return nil, err
			}
			byKey := map[string][]billing.Price{}
			byID := map[string]billing.Price{}
			for _, p := range prices {
				byKey[p.Key] = append(byKey[p.Key], p)
				byID[p.ID.String()] = p
			}
			for _, priceKey := range slices.Sorted(maps.Keys(declared.Prices)) {
				decl := declared.Prices[priceKey]
				if prior := byKey[priceKey]; len(prior) > 0 && prior[0].Provider != nil {
					return nil, apperr.Invalidf("price key %q of product %q is a provider price: declare it under provider_prices", priceKey, productKey)
				}
				current, request, err := catalogApplicationPriceRequest(product, priceKey, decl, byKey, byID)
				if err != nil {
					return nil, err
				}
				if err := validateCatalogPriceTerms(request); err != nil {
					return nil, err
				}
				if err := validateCreditPrice(product.CreditGrant, request); err != nil {
					return nil, err
				}
				same := current != nil && samePriceTerms(*current, request)
				links := map[string]map[string]string{}
				var stored map[string]string
				if current != nil {
					stored, err = scoped.storedPriceLinks(ctx, current.ID, links)
					if err != nil {
						return nil, err
					}
				}
				out.links[[2]string{product.Key, priceKey}] = links
				live := !request.Archived && !product.Archived
				methods := request.SellVia
				solanaOnly := len(methods) == 1 && methods[0] == catalogwire.SellViaSolana
				if !live {
					if same && stored != nil {
						links[stored[models.RailKeyPSPID]] = stored
					}
					continue
				}
				if why := sellViaRefusal(request, accounts); why != "" {
					out.warn(product.Key, priceKey, true, "price %q of product %q cannot sell: %s", priceKey, productKey, why)
					continue
				}
				if request.BillingIntervalHours == nil || !sellsVia(methods, catalogwire.SellViaSolana) || solanaPSP(accounts) == nil {
					continue
				}
				if why := solanaPlanRefusal(request); why != "" {
					if solanaOnly {
						out.warn(product.Key, priceKey, true, "price %q of product %q sells only on Solana, and %s", priceKey, productKey, why)
					}
					continue
				}
				if same && stored != nil && stored[solanaKeyPlanPDA] != "" {
					links[stored[models.RailKeyPSPID]] = stored
					continue
				}
				priceID, err := scoped.catalogPriceStorageID(ctx, billing.ProductID(product.ID), request, current, same)
				if err != nil {
					return nil, err
				}
				solanaChecks = append(solanaChecks, solanaPlanCheck{productKey: product.Key, key: priceKey, request: request, priceID: priceID, solanaOnly: solanaOnly})
			}
			for _, priceKey := range slices.Sorted(maps.Keys(declared.ProviderPrices)) {
				var current *models.Price
				if versions := byKey[priceKey]; len(versions) > 0 {
					if versions[0].Provider == nil {
						return nil, apperr.Invalidf("price key %q of product %q is one of ours: a provider price takes a key of its own", priceKey, productKey)
					}
					prices, err := scoped.requirePriceService()
					if err != nil {
						return nil, err
					}
					if current, err = scoped.currentPriceVersion(ctx, prices, product.ID.UUID(), priceKey); err != nil {
						return nil, err
					}
				}
				providerChecks = append(providerChecks, providerPriceCheck{product: &models.Product{ID: product.ID.UUID(), Key: product.Key, Archived: product.Archived}, key: priceKey, decl: declared.ProviderPrices[priceKey], current: current})
			}
		}
		return out, nil
	})
	if err != nil || prepared.replay != nil {
		return prepared, err
	}
	// A pinned connection carries scope only; it does not hold a transaction or
	// authored merchant lock while these provider calls run.
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	for _, check := range providerChecks {
		p, err := s.prepareProviderPrice(ctx, accounts, check.product, check.key, check.decl, check.current)
		if errors.Is(err, ErrProviderPriceUnreadable) {
			prepared.warn(check.product.Key, check.key, true, "provider price %q of product %q: %v", check.key, check.product.Key, err)
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("provider price %q of product %q: %w", check.key, check.product.Key, err)
		}
		if p.cached != "" {
			prepared.warn(check.product.Key, check.key, false, "provider price %q of product %q keeps its cached terms: %s", check.key, check.product.Key, p.cached)
		}
		prepared.providers[[2]string{check.product.Key, check.key}] = p
		prepared.accounts[p.account.ID] = p.account
	}
	if len(solanaChecks) > 0 {
		account := solanaPSP(accounts)
		for _, check := range solanaChecks {
			link, err := s.solanaPlanLink(ctx, solanaAccount{key: account.Key, id: account.ID}, check.request, check.priceID, nil)
			if err != nil {
				reason := err.Error()
				if isNoCranker(err) {
					reason = "the deployment lists no solana.crankers to own its plan"
				}
				if check.solanaOnly {
					prepared.warn(check.productKey, check.key, true, "price %q of product %q sells only on Solana and cannot: %s", check.key, check.productKey, reason)
				} else {
					prepared.warn(check.productKey, check.key, false, "price %q of product %q is not sold on Solana: %s", check.key, check.productKey, reason)
				}
				continue
			}
			prepared.links[[2]string{check.productKey, check.key}][account.Key] = link
			prepared.accounts[account.ID] = *account
		}
	}
	return prepared, nil
}

// storedPriceLinks are a stored version's bindings: those of superseded PSPs,
// kept by id for the subscriptions they still renew, go into retained; its
// current Solana plan is returned.
func (s *Service) storedPriceLinks(ctx context.Context, id billing.PriceID, retained map[string]map[string]string) (map[string]string, error) {
	prices, err := s.requirePriceService()
	if err != nil {
		return nil, err
	}
	stored, err := prices.GetByPublicID(ctx, id)
	if err != nil {
		return nil, err
	}
	var plan map[string]string
	for key, link := range stored.PSPLinks {
		if _, err := uuid.Parse(key); err == nil {
			retained[key] = cloneStringMap(link)
			continue
		}
		if link[models.RailKeyRail] == string(models.RailSolana) {
			plan = cloneStringMap(link)
		}
	}
	return plan, nil
}

// solanaPSP is the merchant's live Solana PSP, nil when it has none.
func solanaPSP(accounts []merchants.PSPScope) *merchants.PSPScope {
	for i, a := range accounts {
		if a.Rail == string(models.RailSolana) && a.Declared && !a.Archived && !a.Superseded {
			return &accounts[i]
		}
	}
	return nil
}

// sellsVia reports whether methods (nil: every method) include method.
func sellsVia(methods []string, method string) bool {
	return len(methods) == 0 || slices.Contains(methods, method)
}

// sellViaRefusal is why a price of ours restricted to some methods cannot
// sell through any of them, "" when one can. Unrestricted prices sell through
// whatever the merchant arms.
func sellViaRefusal(req billing.CreatePriceParams, accounts []merchants.PSPScope) string {
	if len(req.SellVia) == 0 {
		return ""
	}
	recurring := req.BillingIntervalHours != nil
	trial := req.TrialUnitAmount != nil || req.TrialDurationHours != nil
	var why []string
	if slices.Contains(req.SellVia, catalogwire.SellViaCard) {
		card := false
		for _, a := range accounts {
			rail := models.Rail(a.Rail)
			if !a.Archived && a.Declared && !a.Superseded && railreg.SellsOnLocalTerms(rail) && railreg.CanSellNew(rail, recurring, trial) && moneyutil.RequireFiatCurrency(req.Currency) == nil {
				card = true
			}
		}
		if card {
			return ""
		}
		why = append(why, "no card PSP of the merchant can sell it")
	}
	if slices.Contains(req.SellVia, catalogwire.SellViaSolana) {
		if solanaPSP(accounts) == nil {
			why = append(why, "the merchant declares no Solana PSP")
		} else {
			return ""
		}
	}
	return strings.Join(why, ", and ")
}

// catalogPriceStorageID is the stored id a declared price commits under: the
// current price's when the terms are unchanged, else the new terms' id.
func (s *Service) catalogPriceStorageID(ctx context.Context, productID billing.ProductID, req billing.CreatePriceParams, current *billing.Price, same bool) (uuid.UUID, error) {
	if current == nil || !same {
		return newPriceID(productID.UUID(), req.Key, req), nil
	}
	prices, err := s.requirePriceService()
	if err != nil {
		return uuid.Nil, err
	}
	stored, err := prices.GetByPublicID(ctx, current.ID)
	if err != nil {
		return uuid.Nil, err
	}
	return stored.ID, nil
}

func cloneStringMap(in map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range in {
		out[k] = v
	}
	return out
}
func selectCatalogApplicationPSP(accounts []merchants.PSPScope, key string, link map[string]string) (merchants.PSPScope, bool, error) {
	var selected merchants.PSPScope
	found := false
	for _, row := range accounts {
		rowKey := row.Key
		// A superseded PSP's label names another account; only its id selects it.
		if row.Superseded && link["psp_id"] != row.ID.String() {
			continue
		}
		if rawID := link["psp_id"]; rawID != "" {
			if row.ID.String() != rawID {
				continue
			}
			if rowKey != key {
				return selected, false, apperr.Invalidf("provider key and account id disagree")
			}
		}
		if rowKey != key {
			continue
		}
		if rail := link["rail"]; rail != "" && !strings.EqualFold(rail, row.Rail) {
			return selected, false, apperr.Invalidf("provider rail and account disagree")
		}
		if found {
			return selected, false, apperr.Invalidf("provider %q is ambiguous", key)
		}
		selected = row
		found = true
	}
	return selected, found, nil
}
func sameCatalogApplicationPSP(a, b merchants.PSPScope) bool {
	return a.ID == b.ID && a.Rail == b.Rail && a.Environment == b.Environment && a.AccountID == b.AccountID && a.Archived == b.Archived && a.Key == b.Key && reflect.DeepEqual(a.CustodianID, b.CustodianID) && a.Revision == b.Revision && reflect.DeepEqual(a.Settings, b.Settings)
}
func (s *Service) revalidateCatalogApplicationProviders(ctx context.Context, prepared *catalogApplicationPreparation) error {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	ids := make([]uuid.UUID, 0, len(prepared.accounts))
	for id := range prepared.accounts {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i].String() < ids[j].String() })
	for _, id := range ids {
		if _, err := s.catalogDatabase().Gen(ctx).LockCatalogApplicationPSP(ctx, gen.LockCatalogApplicationPSPParams{MerchantID: mid.UUID(), ID: id}); err != nil {
			return err
		}
		current, found, err := merchants.Of(s.catalogDatabase()).PSPScopeByID(ctx, mid, id)
		if err != nil {
			return err
		}
		if !found || !sameCatalogApplicationPSP(current, prepared.accounts[id]) {
			return ErrCatalogConflict
		}
	}
	return nil
}

func sameCatalogLinks(a, b map[string]map[string]string) bool {
	return len(a) == 0 && len(b) == 0 || reflect.DeepEqual(a, b)
}
