package intents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/stripeapi"
	"github.com/open-rails/openrails/internal/modules/catalog"
)

// fakeStripeCatalogAPI scripts the read state per object id and records every
// archive write with its idempotency key.
type fakeStripeCatalogAPI struct {
	products map[string]*catalog.StripeProduct // nil entry = 404
	prices   map[string]*catalog.StripePrice
	readErr  error
	writeErr error

	productWrites map[string]catalog.UpdateProductParams
	priceWrites   map[string]catalog.UpdatePriceParams
}

func newFakeStripeCatalogAPI() *fakeStripeCatalogAPI {
	return &fakeStripeCatalogAPI{
		products:      map[string]*catalog.StripeProduct{},
		prices:        map[string]*catalog.StripePrice{},
		productWrites: map[string]catalog.UpdateProductParams{},
		priceWrites:   map[string]catalog.UpdatePriceParams{},
	}
}

func (f *fakeStripeCatalogAPI) FindProduct(_ context.Context, id string) (*catalog.StripeProduct, bool, error) {
	if f.readErr != nil {
		return nil, false, f.readErr
	}
	p, ok := f.products[id]
	if !ok || p == nil {
		return nil, false, nil
	}
	return p, true, nil
}

func (f *fakeStripeCatalogAPI) FindPrice(_ context.Context, id string) (*catalog.StripePrice, bool, error) {
	if f.readErr != nil {
		return nil, false, f.readErr
	}
	p, ok := f.prices[id]
	if !ok || p == nil {
		return nil, false, nil
	}
	return p, true, nil
}

func (f *fakeStripeCatalogAPI) UpdateProduct(_ context.Context, id string, params catalog.UpdateProductParams) error {
	if f.writeErr != nil {
		return f.writeErr
	}
	f.productWrites[id] = params
	if p := f.products[id]; p != nil && params.Active != nil {
		p.Active = *params.Active
	}
	return nil
}

func (f *fakeStripeCatalogAPI) UpdatePrice(_ context.Context, id string, params catalog.UpdatePriceParams) error {
	if f.writeErr != nil {
		return f.writeErr
	}
	f.priceWrites[id] = params
	if p := f.prices[id]; p != nil && params.Active != nil {
		p.Active = *params.Active
	}
	return nil
}

func stubCatalog(products []*models.Product, prices []*models.Price) catalogRowsLoader {
	return func(context.Context) ([]*models.Product, []*models.Price, error) {
		return products, prices, nil
	}
}

func archiveIntent(t *testing.T, intentType, objectID, markerKey string) gen.OpenrailsRailIntent {
	t.Helper()
	payload, err := json.Marshal(StripeArchivePayload{ObjectID: objectID, MarkerKey: markerKey})
	if err != nil {
		t.Fatal(err)
	}
	return gen.OpenrailsRailIntent{
		ID:             uuid.New(),
		Rail:           "stripe",
		IntentType:     intentType,
		Payload:        payload,
		IdempotencyKey: StripeArchiveIdempotencyKey(intentType, objectID),
		Origin:         string(OriginAdmin),
	}
}

func newProductArchiveHandler(api *fakeStripeCatalogAPI, loader catalogRowsLoader) *StripeArchiveProductHandler {
	return &StripeArchiveProductHandler{stripeArchiveCore{Stripe: api, LoadCatalog: loader, Policy: DefaultBackoff}}
}

func newPriceArchiveHandler(api *fakeStripeCatalogAPI, loader catalogRowsLoader) *StripeArchivePriceHandler {
	return &StripeArchivePriceHandler{stripeArchiveCore{Stripe: api, LoadCatalog: loader, Policy: DefaultBackoff}}
}

func TestStripeArchive_VerifyThenExecute(t *testing.T) {
	api := newFakeStripeCatalogAPI()
	api.products["prod_x"] = &catalog.StripeProduct{ID: "prod_x", Active: true}
	h := newProductArchiveHandler(api, stubCatalog(nil, nil))
	intent := archiveIntent(t, TypeStripeArchiveProduct, "prod_x", "retired")

	out := h.Execute(context.Background(), intent)
	if out.Class != OutcomeSucceeded {
		t.Fatalf("expected success, got %s (%s)", out.Class, out.Reason)
	}
	w, ok := api.productWrites["prod_x"]
	if !ok || w.Active == nil || *w.Active != false {
		t.Fatalf("expected an active=false write, got %+v", w)
	}
	// Ruling 2: the Stripe mutation carries the intent's idempotency_key.
	if w.IdempotencyKey != intent.IdempotencyKey {
		t.Errorf("write must carry the intent idempotency key, got %q want %q", w.IdempotencyKey, intent.IdempotencyKey)
	}
	if v, _ := out.Evidence["archived"].(bool); !v {
		t.Errorf("evidence must record the archive, got %+v", out.Evidence)
	}
}

func TestStripeArchive_AbsentIsSuccessWithoutWrite(t *testing.T) {
	api := newFakeStripeCatalogAPI() // no objects: 404
	h := newPriceArchiveHandler(api, stubCatalog(nil, nil))
	out := h.Execute(context.Background(), archiveIntent(t, TypeStripeArchivePrice, "price_gone", "k"))
	if out.Class != OutcomeSucceeded {
		t.Fatalf("absent = success, got %s (%s)", out.Class, out.Reason)
	}
	if v, _ := out.Evidence["verified_absent"].(bool); !v {
		t.Errorf("evidence must record verified_absent, got %+v", out.Evidence)
	}
	if len(api.priceWrites) != 0 {
		t.Error("no write may be sent for an absent object")
	}
}

func TestStripeArchive_AlreadyInactiveIsSuccessWithoutWrite(t *testing.T) {
	api := newFakeStripeCatalogAPI()
	api.prices["price_x"] = &catalog.StripePrice{ID: "price_x", Active: false}
	h := newPriceArchiveHandler(api, stubCatalog(nil, nil))
	out := h.Execute(context.Background(), archiveIntent(t, TypeStripeArchivePrice, "price_x", "k"))
	if out.Class != OutcomeSucceeded {
		t.Fatalf("already-inactive = success, got %s (%s)", out.Class, out.Reason)
	}
	if v, _ := out.Evidence["already_inactive"].(bool); !v {
		t.Errorf("evidence must record already_inactive, got %+v", out.Evidence)
	}
	if len(api.priceWrites) != 0 {
		t.Error("no write may be sent for an already-inactive object")
	}
}

func TestStripeArchive_ReadonlyChokeParks(t *testing.T) {
	api := newFakeStripeCatalogAPI()
	api.products["prod_x"] = &catalog.StripeProduct{ID: "prod_x", Active: true}
	api.writeErr = fmt.Errorf("post: %w", stripeapi.ErrProviderReadOnly)
	h := newProductArchiveHandler(api, stubCatalog(nil, nil))
	out := h.Execute(context.Background(), archiveIntent(t, TypeStripeArchiveProduct, "prod_x", "k"))
	if out.Class != OutcomeParked {
		t.Fatalf("readonly choke must park, got %s (%s)", out.Class, out.Reason)
	}
}

func TestStripeArchive_WriteFailureIsCleanlyRetryable(t *testing.T) {
	api := newFakeStripeCatalogAPI()
	api.products["prod_x"] = &catalog.StripeProduct{ID: "prod_x", Active: true}
	api.writeErr = errors.New("stripe 500")
	h := newProductArchiveHandler(api, stubCatalog(nil, nil))
	out := h.Execute(context.Background(), archiveIntent(t, TypeStripeArchiveProduct, "prod_x", "k"))
	// Archives are idempotent: a failed write retries blindly, never ambiguous.
	if out.Class != OutcomeRetryable {
		t.Fatalf("archive write failure must be retryable, got %s (%s)", out.Class, out.Reason)
	}
}

func TestStripeArchive_Verify(t *testing.T) {
	api := newFakeStripeCatalogAPI()
	api.prices["price_x"] = &catalog.StripePrice{ID: "price_x", Active: true}
	h := newPriceArchiveHandler(api, stubCatalog(nil, nil))
	intent := archiveIntent(t, TypeStripeArchivePrice, "price_x", "k")

	if out := h.Verify(context.Background(), intent); out.Class != OutcomeRetryable {
		t.Fatalf("still-active verify = retryable, got %s (%s)", out.Class, out.Reason)
	}
	api.prices["price_x"].Active = false
	if out := h.Verify(context.Background(), intent); out.Class != OutcomeSucceeded {
		t.Fatalf("inactive verify = success, got %s (%s)", out.Class, out.Reason)
	}
	delete(api.prices, "price_x")
	if out := h.Verify(context.Background(), intent); out.Class != OutcomeSucceeded {
		t.Fatalf("absent verify = success, got %s (%s)", out.Class, out.Reason)
	}
}

// TestStripeArchive_RelevanceFlipsWhenObjectJoinsCatalog pins ruling 4: the
// intent is relevant while the object is STILL an extra; once it is linked by
// id or content-matched by its marker, archiving the remote copy would be
// wrong -> superseded. The check reuses the detection predicate
// (catalog.ExtrasIndex) — detection and the ledger cannot disagree.
func TestStripeArchive_RelevanceFlipsWhenObjectJoinsCatalog(t *testing.T) {
	productID := uuid.New()
	cycle := 720

	t.Run("product: content key joins", func(t *testing.T) {
		h := newProductArchiveHandler(newFakeStripeCatalogAPI(), stubCatalog(nil, nil))
		intent := archiveIntent(t, TypeStripeArchiveProduct, "prod_x", "retired")

		rel, err := h.CheckRelevance(context.Background(), intent)
		if err != nil || !rel.Applicable {
			t.Fatalf("still an extra: must be relevant, got %+v err=%v", rel, err)
		}
		// The key "retired" appears locally -> the marker now content-matches.
		h.LoadCatalog = stubCatalog([]*models.Product{{ID: productID, Key: "retired"}}, nil)
		rel, err = h.CheckRelevance(context.Background(), intent)
		if err != nil || rel.Applicable {
			t.Fatalf("object joined the catalog: must supersede, got %+v err=%v", rel, err)
		}
	})

	t.Run("price: linked by id", func(t *testing.T) {
		h := newPriceArchiveHandler(newFakeStripeCatalogAPI(), stubCatalog(nil, nil))
		intent := archiveIntent(t, TypeStripeArchivePrice, "price_x", "retired.usd.900.30")

		rel, err := h.CheckRelevance(context.Background(), intent)
		if err != nil || !rel.Applicable {
			t.Fatalf("still an extra: must be relevant, got %+v err=%v", rel, err)
		}
		// A local price links price_x by id.
		linked := &models.Price{
			ID: uuid.New(), ProductID: productID, Amount: 900, Currency: "usd", AccessDurationHours: &cycle, AutoRenew: true,
			PSPLinks: map[string]map[string]string{
				"stripe": {models.RailKeyRail: "stripe", models.RailKeyStripePriceID: "price_x"},
			},
		}
		h.LoadCatalog = stubCatalog(nil, []*models.Price{linked})
		rel, err = h.CheckRelevance(context.Background(), intent)
		if err != nil || rel.Applicable {
			t.Fatalf("object linked locally: must supersede, got %+v err=%v", rel, err)
		}
	})

	t.Run("price: content key joins", func(t *testing.T) {
		h := newPriceArchiveHandler(newFakeStripeCatalogAPI(), stubCatalog(nil, nil))
		intent := archiveIntent(t, TypeStripeArchivePrice, "price_x", "retired.usd.900.30")

		prod := &models.Product{ID: productID, Key: "retired"}
		price := &models.Price{ID: uuid.New(), ProductID: productID, Amount: 900, Currency: "usd", AccessDurationHours: &cycle, AutoRenew: true}
		h.LoadCatalog = stubCatalog([]*models.Product{prod}, []*models.Price{price})
		rel, err := h.CheckRelevance(context.Background(), intent)
		if err != nil || rel.Applicable {
			t.Fatalf("content key matched locally: must supersede, got %+v err=%v", rel, err)
		}
	})
}

func TestStripeArchive_MalformedPayloadSupersedes(t *testing.T) {
	h := newProductArchiveHandler(newFakeStripeCatalogAPI(), stubCatalog(nil, nil))
	rel, err := h.CheckRelevance(context.Background(), gen.OpenrailsRailIntent{
		IntentType: TypeStripeArchiveProduct,
		Payload:    []byte(`{}`),
	})
	if err != nil || rel.Applicable {
		t.Fatalf("malformed payload must supersede (surfaces in reconcile), got %+v err=%v", rel, err)
	}
}

func TestStripeArchive_UnconfiguredParks(t *testing.T) {
	h := &StripeArchiveProductHandler{stripeArchiveCore{Stripe: nil, LoadCatalog: stubCatalog(nil, nil), Policy: DefaultBackoff}}
	out := h.Execute(context.Background(), archiveIntent(t, TypeStripeArchiveProduct, "prod_x", "k"))
	if out.Class != OutcomeParked {
		t.Fatalf("unconfigured stripe must park, got %s (%s)", out.Class, out.Reason)
	}
}
