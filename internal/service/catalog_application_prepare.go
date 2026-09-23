package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	catalogmodule "github.com/open-rails/openrails/internal/modules/catalog"
	railreg "github.com/open-rails/openrails/internal/modules/payments/rails"
	"github.com/open-rails/openrails/internal/shared/apperr"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
	"github.com/open-rails/openrails/pkg/merchant"
)

type catalogReferenceVerifier func(context.Context, string, string, string, string, CreatePriceRequest, map[string]string) (map[string]string, error)

type catalogReferenceCheck struct {
	key, provider, productKey string
	account                   gen.OpenrailsPsp
	request                   CreatePriceRequest
	link                      map[string]string
}
type catalogApplicationPreparation struct {
	replay   *openrails.CatalogApplicationReceipt
	links    map[string]map[string]map[string]string
	accounts map[uuid.UUID]gen.OpenrailsPsp
	checks   []catalogReferenceCheck
}

func (s *Service) catalogApplicationReplay(ctx context.Context, params openrails.CatalogApplyParams, digest [32]byte) (*openrails.CatalogApplicationReceipt, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	previous, err := s.catalogDatabase().Gen(ctx).GetCatalogApplication(ctx, gen.GetCatalogApplicationParams{MerchantID: mid.UUID(), ApplicationID: params.ApplicationID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(previous.RequestSha256, digest[:]) {
		return nil, apperr.New(409, "catalog_application_conflict", "application_id already committed with different content")
	}
	var receipt openrails.CatalogApplicationReceipt
	if err := json.Unmarshal(previous.Result, &receipt); err != nil {
		return nil, err
	}
	receipt.Replayed = true
	return &receipt, nil
}

// Prepare only observes remote references. Snapshot collection and the final
// local commit each fence the merchant, but no transaction spans provider I/O.
// A committed retry returns before resolving targets or contacting a provider.
func (s *Service) prepareCatalogApplication(ctx context.Context, params openrails.CatalogApplyParams, digest [32]byte, verify catalogReferenceVerifier) (*catalogApplicationPreparation, error) {
	prepared, err := catalogMutation(ctx, s, func(ctx context.Context, scoped *Service) (*catalogApplicationPreparation, error) {
		replay, err := scoped.catalogApplicationReplay(ctx, params, digest)
		if err != nil {
			return nil, err
		}
		out := &catalogApplicationPreparation{replay: replay, links: map[string]map[string]map[string]string{}, accounts: map[uuid.UUID]gen.OpenrailsPsp{}}
		if replay != nil {
			return out, nil
		}
		mid, err := merchant.Require(ctx)
		if err != nil {
			return nil, err
		}
		q := scoped.catalogDatabase().Gen(ctx)
		revision, err := q.GetCatalogRevision(ctx, mid.UUID())
		if err != nil {
			return nil, err
		}
		if revision != *params.ExpectedRevision {
			return nil, apperr.New(409, "catalog_revision_conflict", fmt.Sprintf("catalog revision is %d; expected %d", revision, *params.ExpectedRevision))
		}
		var target uuid.UUID
		if params.CatalogID != "" {
			id, err := openrails.ParseCatalogID(params.CatalogID)
			if err != nil {
				return nil, apperr.Invalidf("invalid catalog_id")
			}
			row, err := catalogmodule.NewCatalogRepo(scoped.catalogDatabase()).Get(ctx, id.UUID())
			if err != nil {
				return nil, productLookup(err)
			}
			target = row.ID
		} else {
			row, err := q.GetDefaultApplicationCatalog(ctx, mid.UUID())
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return nil, err
			}
			target = row.ID
		}
		accounts, err := q.ListPSPsForMerchant(ctx, gen.ListPSPsForMerchantParams{MerchantID: mid.UUID()})
		if err != nil {
			return nil, err
		}
		for _, declared := range params.Products {
			product, err := scoped.GetProductByKey(ctx, declared.Key)
			if err != nil && !errors.Is(err, openrails.ErrNotFound) {
				return nil, err
			}
			if product != nil && product.CatalogID.UUID() != target {
				return nil, ErrCatalogConflict
			}
			if product == nil {
				if !declared.DisplayName.Set {
					return nil, apperr.Invalidf("new product %q requires display_name; cannot archive unknown product", declared.Key)
				}
				product = &CatalogProduct{ID: openrails.ProductID(uuidutil.DeterministicID(uuidutil.DeterministicNamespace, mid.UUID().String(), declared.Key)), Key: declared.Key, DisplayName: declared.DisplayName.Value}
			}
			reactivatingProduct := product.Archived && declared.Archived.Set && !declared.Archived.Value
			if declared.Archived.Set {
				product.Archived = declared.Archived.Value
			}
			prices, err := scoped.ListPricesByProduct(ctx, product.ID, false)
			if err != nil {
				return nil, err
			}
			byKey := map[string][]CatalogPrice{}
			byID := map[string]CatalogPrice{}
			for _, p := range prices {
				byKey[p.Key] = append(byKey[p.Key], p)
				byID[p.ID.String()] = p
			}
			references := declared.Prices
			if reactivatingProduct && !params.Prune {
				// Parent activation also makes omitted live children available.
				// Inspect their references without adding them to the declaration
				// or changing their rows. Pruned children will remain unavailable.
				named := map[string]bool{}
				for _, price := range declared.Prices {
					named[price.Key] = true
				}
				references = append([]openrails.CatalogApplyPrice(nil), declared.Prices...)
				for _, price := range prices {
					if !price.Archived && !named[price.Key] && len(price.Providers) > 0 {
						references = append(references, openrails.CatalogApplyPrice{Key: price.Key, ID: price.ID.String()})
					}
				}
			}
			for _, decl := range references {
				current, request, err := catalogApplicationPriceRequest(product, decl, byKey, byID)
				if err != nil {
					return nil, err
				}
				if err := validateCatalogPriceTerms(request); err != nil {
					return nil, err
				}
				links := catalogPriceLinks(current)
				same := current != nil && current.ID.UUID() == priceDeterministicID(product.ID.UUID(), request.UnitAmount, request.Currency, request.AccessDurationHours, request.AutoRenew, request.TrialUnitAmount, request.TrialDurationHours)
				wanted := map[string]bool{}
				if decl.PSPs.Set {
					for _, key := range decl.PSPs.Value {
						wanted[key] = true
					}
				} else {
					for key := range links {
						wanted[key] = true
					}
				}
				if decl.PSPLinks.Set {
					if len(decl.PSPLinks.Value) == 0 && len(links) > 0 {
						return nil, apperr.Invalidf("provider binding removal requires a separate provider workflow")
					}
					for key, link := range decl.PSPLinks.Value {
						if len(link) == 0 {
							return nil, apperr.Invalidf("empty provider links request removal; catalog applications cannot remove bindings")
						}
						wanted[key] = true
					}
				}
				if decl.PSPs.Set {
					for key := range links {
						if !wanted[key] {
							return nil, apperr.Invalidf("provider binding removal requires a separate provider workflow")
						}
					}
				}
				out.links[decl.Key] = map[string]map[string]string{}
				names := make([]string, 0, len(wanted))
				for key := range wanted {
					names = append(names, key)
				}
				sort.Strings(names)
				for _, key := range names {
					link := links[key]
					if supplied, ok := decl.PSPLinks.Value[key]; ok {
						link = supplied
					}
					unchanged := same && catalogLinkContains(links[key], link) && (request.Archived || (!current.Archived && !reactivatingProduct))
					if unchanged {
						out.links[decl.Key][key] = cloneStringMap(links[key])
						continue
					}
					account, found, err := selectCatalogApplicationPSP(accounts, key, link, scoped.catalogProviderEnvironment())
					if err != nil {
						return nil, err
					}
					rail := key
					if found {
						rail = account.Rail
					}
					if (request.TrialUnitAmount != nil || request.TrialDurationHours != nil) && !railreg.SupportsCatalogTrial(models.Rail(rail)) {
						return nil, fmt.Errorf("%w: PSP %q on rail %s cannot execute trial first-phase terms", ErrTrialUnsupportedOnRail, key, rail)
					}
					if found {
						if account.Archived && !request.Archived {
							return nil, apperr.Invalidf("provider %q is archived", key)
						}
						// A local engine offer still selects an account. Its identity and
						// eligibility must survive the gap before the final commit.
						out.accounts[account.ID] = account
					}
					if len(link) == 0 && scoped.localEnginePrice(rail, priceRequestCycleDays(request)) {
						continue
					}
					if !found {
						return nil, apperr.Invalidf("provider %q has no matching merchant account", key)
					}
					verificationRequest := request
					verificationRequest.Archived = verificationRequest.Archived || product.Archived
					out.checks = append(out.checks, catalogReferenceCheck{key: decl.Key, provider: key, productKey: product.Key, account: account, request: verificationRequest, link: cloneStringMap(link)})
				}
			}
		}
		return out, nil
	})
	if err != nil || prepared.replay != nil {
		return prepared, err
	}
	// A pinned connection carries scope only; it does not hold a transaction or
	// authored merchant lock while these strictly read-only provider calls run.
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	for _, check := range prepared.checks {
		link := cloneStringMap(check.link)
		delete(link, "psp_id")
		verified, err := verify(ctx, check.provider, check.account.Rail, check.account.AccountID, check.productKey, check.request, link)
		if err != nil {
			return nil, err
		}
		if verified == nil {
			return nil, apperr.Invalidf("provider %q returned no verified reference", check.provider)
		}
		verified["rail"] = check.account.Rail
		verified["psp_id"] = check.account.ID.String()
		prepared.links[check.key][check.provider] = verified
	}
	return prepared, nil
}

func catalogPriceLinks(price *CatalogPrice) map[string]map[string]string {
	out := map[string]map[string]string{}
	if price != nil {
		for key, state := range price.Providers {
			out[key] = cloneStringMap(state.IDs)
		}
	}
	return out
}
func cloneStringMap(in map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range in {
		out[k] = v
	}
	return out
}
func catalogLinkContains(stored, declared map[string]string) bool {
	if len(stored) == 0 {
		return false
	}
	for k, v := range declared {
		if stored[k] != v {
			return false
		}
	}
	return true
}
func selectCatalogApplicationPSP(accounts []gen.OpenrailsPsp, key string, link map[string]string, environment string) (gen.OpenrailsPsp, bool, error) {
	var selected gen.OpenrailsPsp
	found := false
	for _, row := range accounts {
		rowKey := row.ID.String()
		if row.Key != nil {
			rowKey = *row.Key
		}
		if rawID := link["psp_id"]; rawID != "" {
			if row.ID.String() != rawID {
				continue
			}
			if rowKey != key {
				return selected, false, apperr.Invalidf("provider key and account id disagree")
			}
		}
		if rowKey != key || row.Environment != environment {
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
func sameCatalogApplicationPSP(a, b gen.OpenrailsPsp) bool {
	return a.ID == b.ID && a.MerchantID == b.MerchantID && a.Rail == b.Rail && a.Environment == b.Environment && a.AccountID == b.AccountID && a.Archived == b.Archived && reflect.DeepEqual(a.Key, b.Key) && reflect.DeepEqual(a.CustodianID, b.CustodianID) && bytes.Equal(a.Evidence, b.Evidence)
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
		current, err := s.catalogDatabase().Gen(ctx).LockCatalogApplicationPSP(ctx, gen.LockCatalogApplicationPSPParams{MerchantID: mid.UUID(), ID: id})
		if err != nil {
			return err
		}
		if !sameCatalogApplicationPSP(current, prepared.accounts[id]) {
			return ErrCatalogConflict
		}
	}
	return nil
}

func sameCatalogLinks(a, b map[string]map[string]string) bool {
	return len(a) == 0 && len(b) == 0 || reflect.DeepEqual(a, b)
}
