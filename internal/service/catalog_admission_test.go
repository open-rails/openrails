package service

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
)

func TestCatalogAdmissionExcludesCosmeticAndDerivedFields(t *testing.T) {
	product := productObject("paid")
	meter := meterObject("requests")
	before := catalogState{product: {fields: map[string]json.RawMessage{"display_name": catalogJSON("Old"), "description": catalogJSON("Old text"), "archived": catalogJSON(false), "rate_cards": catalogJSON([]string{"price"})}}, meter: {fields: map[string]json.RawMessage{"event_type": catalogJSON("request")}, hidden: catalogJSON("old projection")}}
	after := catalogState{product: {fields: map[string]json.RawMessage{"display_name": catalogJSON("New"), "description": catalogJSON("New text"), "archived": catalogJSON(false), "rate_cards": catalogJSON([]string{"price"})}}, meter: {fields: map[string]json.RawMessage{"event_type": catalogJSON("request")}, hidden: catalogJSON("new projection")}}
	if got := substantiveCatalogTargets(before, after); len(got) != 0 {
		t.Fatalf("cosmetic/derived-only edit counted: %v", got)
	}
	after[product].fields["archived"] = catalogJSON(true)
	if got := substantiveCatalogTargets(before, after); len(got) != 1 || got[0] != product {
		t.Fatalf("archive targets: %v", got)
	}
	delete(after, product)
	if got := substantiveCatalogTargets(before, after); len(got) != 1 || got[0] != product {
		t.Fatalf("delete targets: %v", got)
	}
}

func TestProductAdmissionPlanCountsOneTargetAndRejectsStaleRevision(t *testing.T) {
	id := billing.ProductID(uuid.New())
	obj := productObject("paid")
	state := catalogState{obj: {productID: id.UUID(), revision: 4, fields: map[string]json.RawMessage{"archived": catalogJSON(false), "tier_rank": catalogJSON(1)}}}
	same, changed := false, true
	rank := 2
	cases := []struct {
		name string
		req  UpdateProductRequest
		want int
	}{
		{"noop", UpdateProductRequest{Archived: &same}, 0},
		{"archive", UpdateProductRequest{Archived: &changed}, 1},
		{"multiple fields one product", UpdateProductRequest{Archived: &changed, TierRank: &rank}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := productEditPlan(id, tc.req)(context.Background(), nil, state)
			if err != nil || len(got) != tc.want {
				t.Fatalf("targets %v err %v", got, err)
			}
		})
	}
	old := int64(3)
	if _, err := productEditPlan(id, UpdateProductRequest{Archived: &changed, ExpectedRevision: &old})(context.Background(), nil, state); err == nil {
		t.Fatal("stale revision admitted")
	}
}
