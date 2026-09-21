package catalogpublish

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	billingservice "github.com/open-rails/openrails/internal/service"
)

// Only the error-returning read is scripted. Successful catalog state belongs
// to the real Client/HTTP workflow, not a second in-memory catalog backend.
type refusedCatalogLookup struct {
	applier
	err error
}

func (a refusedCatalogLookup) GetProductByKey(context.Context, string) (*billingservice.CatalogProduct, error) {
	return nil, a.err
}

func TestCatalogPlannerRefusesIncompleteReadsAndInvalidInput(t *testing.T) {
	price := Price{Currency: "USD", UnitAmount: 1_000_000, Duration: "30d", AutoRenew: true}
	manifest := func(prices ...Price) *Manifest {
		return &Manifest{TierGroups: []TierGroup{{Key: "memberships", Products: []Product{{Key: "premium", DisplayName: "Premium", Prices: prices}}}}}
	}
	for _, cause := range []error{nil, errors.New("database unavailable"), errors.New("provider lookup failed")} {
		plan, err := plan(context.Background(), refusedCatalogLookup{err: cause}, manifest(price))
		if err == nil || plan != nil {
			t.Fatalf("incomplete read produced a plan: %v, %v", plan, err)
		}
		if cause != nil && !errors.Is(err, cause) {
			t.Fatalf("lost operational error: %v", err)
		}
		if errors.Is(err, openrails.ErrInvalid) || errors.Is(err, openrails.ErrNotFound) {
			t.Fatalf("operational failure became caller input: %v", err)
		}
	}
	for _, kind := range []string{"duration", "trial.duration", "key collision"} {
		p := price
		switch kind {
		case "duration":
			p.Duration = "invalid"
		case "trial.duration":
			p.Trial = &PriceTrial{Duration: "invalid"}
		}
		m := manifest(p)
		if kind == "key collision" {
			m = manifest(price, price)
		}
		_, err := plan(context.Background(), refusedCatalogLookup{err: openrails.ErrNotFound}, m)
		if !errors.Is(err, openrails.ErrInvalid) {
			t.Fatalf("%s lacks typed input refusal: %v", kind, err)
		}
	}
}

type catalogLinkRecorder struct {
	applier
	id      openrails.PriceID
	request billingservice.UpdatePriceRequest
	calls   int
}

func (a *catalogLinkRecorder) UpdatePrice(_ context.Context, id openrails.PriceID, req billingservice.UpdatePriceRequest) (*billingservice.CatalogPrice, error) {
	a.id, a.request, a.calls = id, req, a.calls+1
	return &billingservice.CatalogPrice{ID: id}, nil
}

func TestCatalogPriceLinkRotationPreservesUnchangedBindings(t *testing.T) {
	declared := map[string]map[string]string{"mobius": {"plan_id": "already-linked"}, "solana": {"token": "DUSD"}}
	current := map[string]billingservice.ProviderState{"mobius": {IDs: map[string]string{"plan_id": "already-linked", "provider": "mobius"}}, "solana": {IDs: map[string]string{"mint_symbol": "USDC", "plan_pda": "observed-plan"}}}
	want := map[string]map[string]string{"solana": {"token": "DUSD"}}
	delta := pspLinksToRotate(declared, current)
	if !reflect.DeepEqual(want, delta) {
		t.Fatalf("rotation must contain only changed PSP: %v", delta)
	}
	priceID := openrails.PriceID(uuid.New())
	plan := &ApplyPlan{Groups: []GroupPlan{{Key: "membership", Products: []ProductPlan{{Key: "premium", Action: ProductUnchanged, UpdateID: openrails.ProductID(uuid.New()), Prices: []PricePlan{{Action: PriceUnchanged, ExistingID: priceID, PSPLinks: delta}}}}}}}
	if !plan.HasChanges() || !strings.Contains(planString(plan), "rotate psp_links: solana") {
		t.Fatalf("rotation omitted from operator plan: %s", planString(plan))
	}
	recorder := &catalogLinkRecorder{}
	if _, err := applyWithOptions(context.Background(), recorder, plan, ApplyOptions{Insert: true, Prune: true}); err != nil {
		t.Fatal(err)
	}
	if recorder.calls != 0 {
		t.Fatal("link rotation bypassed overwrite option")
	}
	result, err := apply(context.Background(), recorder, plan)
	if err != nil {
		t.Fatal(err)
	}
	if result.PricesRelinked != 1 || recorder.calls != 1 || recorder.id != priceID || recorder.request.ReplacePSPLinks || !reflect.DeepEqual(recorder.request.PSPLinks, want) {
		t.Fatalf("wrong merge request: %+v", recorder)
	}
	current["solana"] = billingservice.ProviderState{IDs: map[string]string{"token": "DUSD", "mint_symbol": "DUSD", "plan_pda": "observed-plan"}}
	if got := pspLinksToRotate(declared, current); got != nil {
		t.Fatalf("converged links are not quiet: %v", got)
	}
	plan.Groups[0].Products[0].Prices[0].PSPLinks = nil
	if plan.HasChanges() {
		t.Fatal("converged plan still reports a mutation")
	}
}
