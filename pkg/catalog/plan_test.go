package catalog

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	billingservice "github.com/open-rails/openrails/pkg/service"
)

// fakeApplier is an in-memory Applier for plan/apply tests. No DB or network.
type fakeApplier struct {
	products map[string]*billingservice.CatalogProduct // by key
	prices   map[uuid.UUID][]billingservice.CatalogPrice

	createdProducts  []billingservice.CreateProductRequest
	updatedProducts  []uuid.UUID
	deactivatedProds []uuid.UUID
	createdPrices    []billingservice.CreatePriceRequest
	activatedPrices  []uuid.UUID
	archivedPrices   []uuid.UUID
	relabeledPrices  map[uuid.UUID]string
}

func newFakeApplier() *fakeApplier {
	return &fakeApplier{
		products: map[string]*billingservice.CatalogProduct{},
		prices:   map[uuid.UUID][]billingservice.CatalogPrice{},
	}
}

func (f *fakeApplier) seedProduct(key, tierGroup string, rank int, archived bool) *billingservice.CatalogProduct {
	tg := tierGroup
	p := &billingservice.CatalogProduct{
		ID:          uuid.New(),
		Key:         key,
		DisplayName: key,
		TierGroup:   &tg,
		TierRank:    rank,
		Archived:    archived,
	}
	f.products[key] = p
	return p
}

func (f *fakeApplier) seedPrice(productID uuid.UUID, amount int64, currency string, cycleHours int, archived bool, providers ...string) {
	states := map[string]billingservice.ProviderState{}
	for _, provider := range providers {
		states[provider] = billingservice.ProviderState{Status: billingservice.ProviderStatusLinked}
	}
	f.prices[productID] = append(f.prices[productID], billingservice.CatalogPrice{
		ID:                  uuid.New(),
		ProductID:           productID,
		Archived:            archived,
		UnitAmount:          amount,
		Currency:            currency,
		AccessDurationHours: &cycleHours,
		AutoRenew:           true,
		Providers:           states,
	})
}

func (f *fakeApplier) GetProductByKey(_ context.Context, key string) (*billingservice.CatalogProduct, error) {
	if p, ok := f.products[key]; ok {
		return p, nil
	}
	return nil, fmt.Errorf("product not found: %s", key)
}

func (f *fakeApplier) ListProducts(_ context.Context, opts billingservice.ListProductsOptions) ([]billingservice.CatalogProduct, int64, error) {
	var out []billingservice.CatalogProduct
	for _, p := range f.products {
		if opts.ActiveOnly && p.Archived {
			continue
		}
		if opts.TierGroup != "" && (p.TierGroup == nil || !strings.EqualFold(*p.TierGroup, opts.TierGroup)) {
			continue
		}
		out = append(out, *p)
	}
	return out, int64(len(out)), nil
}

func (f *fakeApplier) CreateProduct(_ context.Context, req billingservice.CreateProductRequest) (*billingservice.CatalogProduct, error) {
	f.createdProducts = append(f.createdProducts, req)
	p := &billingservice.CatalogProduct{ID: uuid.New(), Key: req.Key, DisplayName: req.DisplayName, TierGroup: req.TierGroup, TierRank: req.TierRank, Archived: req.Archived}
	f.products[req.Key] = p
	return p, nil
}

func (f *fakeApplier) UpdateProduct(_ context.Context, id uuid.UUID, _ billingservice.UpdateProductRequest) (*billingservice.CatalogProduct, error) {
	f.updatedProducts = append(f.updatedProducts, id)
	return &billingservice.CatalogProduct{ID: id}, nil
}

func (f *fakeApplier) DeactivateProduct(_ context.Context, id uuid.UUID) (*billingservice.CatalogProduct, error) {
	f.deactivatedProds = append(f.deactivatedProds, id)
	return &billingservice.CatalogProduct{ID: id}, nil
}

func (f *fakeApplier) ListPricesByProduct(_ context.Context, productID uuid.UUID, activeOnly bool) ([]billingservice.CatalogPrice, error) {
	var out []billingservice.CatalogPrice
	for _, p := range f.prices[productID] {
		if activeOnly && p.Archived {
			continue
		}
		out = append(out, p)
	}
	return out, nil
}

func (f *fakeApplier) CreatePrice(_ context.Context, req billingservice.CreatePriceRequest) (*billingservice.CatalogPrice, error) {
	f.createdPrices = append(f.createdPrices, req)
	return &billingservice.CatalogPrice{ID: uuid.New(), Key: req.Key, ProductID: req.ProductID, UnitAmount: req.UnitAmount, Currency: req.Currency, Archived: req.Archived}, nil
}

func (f *fakeApplier) ActivatePrice(_ context.Context, id uuid.UUID) (*billingservice.CatalogPrice, error) {
	f.activatedPrices = append(f.activatedPrices, id)
	return &billingservice.CatalogPrice{ID: id}, nil
}

func (f *fakeApplier) DeactivatePrice(_ context.Context, id uuid.UUID) (*billingservice.CatalogPrice, error) {
	f.archivedPrices = append(f.archivedPrices, id)
	return &billingservice.CatalogPrice{ID: id}, nil
}

func (f *fakeApplier) SetPriceKey(_ context.Context, id uuid.UUID, key string) (*billingservice.CatalogPrice, error) {
	if f.relabeledPrices == nil {
		f.relabeledPrices = map[uuid.UUID]string{}
	}
	f.relabeledPrices[id] = key
	return &billingservice.CatalogPrice{ID: id, Key: key}, nil
}

var _ Applier = (*fakeApplier)(nil)

// --- helpers ---

func loadFrom(t *testing.T, body string) *Manifest {
	t.Helper()
	m, err := Load(writeManifest(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return m
}

func findProduct(plan *ApplyPlan, key string) *ProductPlan {
	for gi := range plan.Groups {
		for pi := range plan.Groups[gi].Products {
			if plan.Groups[gi].Products[pi].Key == key {
				return &plan.Groups[gi].Products[pi]
			}
		}
	}
	return nil
}

const planManifest = `
version: 1
products:
  - key: initiate
    display_name: Novice
    tier_group: cozy
    tier_rank: 1
    prices:
      - {currency: usd, unit_amount: 1200, duration: 30d, auto_renew: true, psps: [stripe]}
  - key: craftsman
    display_name: Craftsman
    tier_group: cozy
    tier_rank: 2
    prices:
      - {currency: usd, unit_amount: 1300, duration: 30d, auto_renew: true, psps: [stripe]}
`

func TestPlan_CreateWhenEmpty(t *testing.T) {
	m := loadFrom(t, planManifest)
	f := newFakeApplier()

	plan, err := Plan(context.Background(), f, m)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	for _, key := range []string{"initiate", "craftsman"} {
		pp := findProduct(plan, key)
		if pp == nil || pp.Action != ProductCreate {
			t.Fatalf("%s: want create, got %+v", key, pp)
		}
		if len(pp.Prices) != 1 || pp.Prices[0].Action != PriceCreate {
			t.Fatalf("%s: want 1 price create, got %+v", key, pp.Prices)
		}
		// providers are carried explicitly from the price onto the create request.
		if got := pp.Prices[0].CreateReq.PSPs; len(got) != 1 || got[0] != "stripe" {
			t.Fatalf("%s: providers not set on create req: %v", key, got)
		}
	}
	if !plan.HasChanges() {
		t.Fatal("expected changes")
	}
}

func TestPlan_InactiveManifestObjectsCreateArchived(t *testing.T) {
	m := loadFrom(t, `
version: 1
products:
  - key: legacy
    display_name: Legacy
    archived: true
    prices:
      - currency: usd
        unit_amount: 1200
        archived: true
`)
	f := newFakeApplier()

	plan, err := Plan(context.Background(), f, m)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	pp := findProduct(plan, "legacy")
	if !pp.CreateReq.Archived {
		t.Fatal("product create must be archived")
	}
	if !pp.Prices[0].CreateReq.Archived {
		t.Fatal("price create must be archived")
	}
}

func TestPlan_UnchangedAndUpdate(t *testing.T) {
	m := loadFrom(t, planManifest)
	f := newFakeApplier()

	// initiate is fully converged -> unchanged.
	initiate := f.seedProduct("initiate", "cozy", 1, false)
	initiate.DisplayName = "Novice"
	f.seedPrice(initiate.ID, 1200, "USD", 30*24, false, "stripe")

	// craftsman has a different display name + active matching price -> product update, price unchanged.
	craftsman := f.seedProduct("craftsman", "cozy", 2, false)
	craftsman.DisplayName = "OLD NAME"
	f.seedPrice(craftsman.ID, 1300, "USD", 30*24, false, "stripe")

	plan, err := Plan(context.Background(), f, m)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	ip := findProduct(plan, "initiate")
	if ip.Action != ProductUnchanged {
		t.Fatalf("initiate: want unchanged, got %s", ip.Action)
	}
	if ip.Prices[0].Action != PriceUnchanged {
		t.Fatalf("initiate price: want unchanged, got %s", ip.Prices[0].Action)
	}

	cp := findProduct(plan, "craftsman")
	if cp.Action != ProductUpdate {
		t.Fatalf("craftsman: want update, got %s", cp.Action)
	}
	if cp.Prices[0].Action != PriceUnchanged {
		t.Fatalf("craftsman price: want unchanged, got %s", cp.Prices[0].Action)
	}
}

func TestPlan_PriceSetSemantics_ArchiveAndCreate(t *testing.T) {
	// Manifest declares craftsman @ $13; OpenRails has an active $12 (no longer
	// declared) -> archive $12, create $13. The $12 is "starter 12 -> 13".
	m := loadFrom(t, planManifest)
	f := newFakeApplier()
	craftsman := f.seedProduct("craftsman", "cozy", 2, false)
	craftsman.DisplayName = "Craftsman"
	f.seedPrice(craftsman.ID, 1200, "USD", 30*24, false) // undeclared old price

	plan, err := Plan(context.Background(), f, m)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	cp := findProduct(plan, "craftsman")

	var creates, archives int
	for _, pr := range cp.Prices {
		switch pr.Action {
		case PriceCreate:
			creates++
		case PriceArchive:
			archives++
		}
	}
	if creates != 1 || archives != 1 {
		t.Fatalf("want 1 create + 1 archive, got %d create %d archive: %+v", creates, archives, cp.Prices)
	}
}

// A declared price matches an existing one on financial substance alone; a
// provider-set drift must NOT spawn a second row — the
// unique_prices_product_amount_window key (no provider column) would reject it,
// crashing the converge. The declared price below differs from the stored one
// only by provider set, so it must match (here: archive, since archived:true).
func TestPlan_SameTermsDifferentProvidersMatchExistingPrice(t *testing.T) {
	m := loadFrom(t, `
version: 1
products:
  - key: premium
    display_name: Premium
    prices:
      - {currency: usd, unit_amount: 23000000, duration: 30d, auto_renew: true, psps: [solana], archived: true}
`)
	f := newFakeApplier()
	premium := f.seedProduct("premium", "default", 0, false)
	premium.DisplayName = "Premium"
	f.seedPrice(premium.ID, 23_000_000, "USD", 30*24, false, "mobius", "ccbill", "solana")

	plan, err := Plan(context.Background(), f, m)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	pp := findProduct(plan, "premium")
	for _, pr := range pp.Prices {
		if pr.Action == PriceCreate {
			t.Fatalf("provider drift must not create a second price: %+v", pp.Prices)
		}
	}
}

// Trial is part of identity: a declared price that adds a trial first phase must
// NOT match an otherwise-identical existing price with no trial — they are
// distinct rows under the unique key, so the trial variant is a create.
func TestPlan_TrialMakesPriceDistinctFromNonTrial(t *testing.T) {
	m := loadFrom(t, `
version: 1
products:
  - key: premium
    display_name: Premium
    prices:
      - {currency: usd, unit_amount: 23000000, duration: 30d, auto_renew: true, psps: [mobius], trial: {unit_amount: 100, duration: 7d}}
`)
	f := newFakeApplier()
	premium := f.seedProduct("premium", "default", 0, false)
	premium.DisplayName = "Premium"
	// Existing price has the same recurring substance but NO trial.
	f.seedPrice(premium.ID, 23_000_000, "USD", 30*24, false, "mobius")

	plan, err := Plan(context.Background(), f, m)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	pp := findProduct(plan, "premium")
	var creates int
	for _, pr := range pp.Prices {
		if pr.Action == PriceCreate {
			creates++
		}
	}
	if creates != 1 {
		t.Fatalf("trial variant must be a distinct create, got %+v", pp.Prices)
	}
}

func TestPlanWithOptions_AdditiveDoesNotArchiveMissingProductsOrPrices(t *testing.T) {
	m := loadFrom(t, planManifest)
	f := newFakeApplier()
	craftsman := f.seedProduct("craftsman", "cozy", 2, false)
	craftsman.DisplayName = "Craftsman"
	f.seedPrice(craftsman.ID, 1200, "USD", 30*24, false) // undeclared old price
	f.seedProduct("expert", "cozy", 3, false)            // undeclared product

	plan, err := PlanWithOptions(context.Background(), f, m, PlanOptions{})
	if err != nil {
		t.Fatalf("PlanWithOptions: %v", err)
	}
	if len(plan.Groups) != 1 || len(plan.Groups[0].RemovedProducts) != 0 {
		t.Fatalf("additive plan must not archive missing products: %+v", plan.Groups)
	}
	cp := findProduct(plan, "craftsman")
	var archives int
	for _, pr := range cp.Prices {
		if pr.Action == PriceArchive {
			archives++
		}
	}
	if archives != 0 {
		t.Fatalf("additive plan must not archive missing prices: %+v", cp.Prices)
	}
}

func TestPlan_ReactivateArchivedDeclaredPrice(t *testing.T) {
	m := loadFrom(t, planManifest)
	f := newFakeApplier()
	craftsman := f.seedProduct("craftsman", "cozy", 2, false)
	craftsman.DisplayName = "Craftsman"
	// The declared $13 price exists but is archived -> activate.
	f.seedPrice(craftsman.ID, 1300, "USD", 30*24, true, "stripe")

	plan, err := Plan(context.Background(), f, m)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	cp := findProduct(plan, "craftsman")
	if len(cp.Prices) != 1 || cp.Prices[0].Action != PriceActivate {
		t.Fatalf("want 1 activate, got %+v", cp.Prices)
	}
}

func TestPlan_RemovedProductArchived(t *testing.T) {
	// Manifest declares initiate + craftsman; OpenRails also has a stray "expert"
	// active product in the same tier group -> archive expert.
	m := loadFrom(t, planManifest)
	f := newFakeApplier()
	f.seedProduct("expert", "cozy", 3, false)

	plan, err := Plan(context.Background(), f, m)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	var removed []string
	for _, g := range plan.Groups {
		for _, rp := range g.RemovedProducts {
			removed = append(removed, rp.Key)
		}
	}
	if len(removed) != 1 || removed[0] != "expert" {
		t.Fatalf("want expert archived, got %v", removed)
	}
}

func TestApply_DrivesFacade(t *testing.T) {
	m := loadFrom(t, planManifest)
	f := newFakeApplier()
	// craftsman exists with old $12 active; initiate is new.
	craftsman := f.seedProduct("craftsman", "cozy", 2, false)
	craftsman.DisplayName = "Craftsman"
	f.seedPrice(craftsman.ID, 1200, "USD", 30*24, false)
	// stray product to archive.
	f.seedProduct("expert", "cozy", 3, false)

	plan, err := Plan(context.Background(), f, m)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	res, err := Apply(context.Background(), f, plan)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if res.ProductsCreated != 1 { // initiate
		t.Fatalf("want 1 product created, got %d", res.ProductsCreated)
	}
	if res.PricesCreated != 2 { // initiate $12 + craftsman $13
		t.Fatalf("want 2 prices created, got %d", res.PricesCreated)
	}
	if res.PricesArchived != 1 { // craftsman old $12
		t.Fatalf("want 1 price archived, got %d", res.PricesArchived)
	}
	if res.ProductsArchived != 1 { // expert
		t.Fatalf("want 1 product archived, got %d", res.ProductsArchived)
	}
	if len(f.deactivatedProds) != 1 {
		t.Fatalf("expert should be deactivated via facade, got %v", f.deactivatedProds)
	}
}

func TestApply_Idempotent(t *testing.T) {
	m := loadFrom(t, planManifest)
	f := newFakeApplier()
	// Seed a fully-converged catalog.
	ini := f.seedProduct("initiate", "cozy", 1, false)
	ini.DisplayName = "Novice"
	f.seedPrice(ini.ID, 1200, "USD", 30*24, false, "stripe")
	cra := f.seedProduct("craftsman", "cozy", 2, false)
	cra.DisplayName = "Craftsman"
	f.seedPrice(cra.ID, 1300, "USD", 30*24, false, "stripe")

	plan, err := Plan(context.Background(), f, m)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan.HasChanges() {
		t.Fatalf("converged catalog should have no changes:\n%s", plan.String())
	}
	res, err := Apply(context.Background(), f, plan)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if res.ProductsCreated+res.ProductsUpdated+res.ProductsArchived+res.PricesCreated+res.PricesActivated+res.PricesArchived != 0 {
		t.Fatalf("idempotent apply mutated something: %+v", res)
	}
}
