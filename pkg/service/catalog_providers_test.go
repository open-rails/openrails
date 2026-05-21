package service

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/db/models"
)

// newUnconfiguredService returns a *Service whose runtime has no Stripe config,
// so the stripe adapter's AutoCreate returns errPendingManualLink. Suitable for
// exercising the dispatcher logic without any network / DB access.
func newUnconfiguredService() *Service {
	return &Service{rt: &app.Runtime{}}
}

// -- Per-provider adapter unit tests -----------------------------------------

func TestCCBillAdapter_Attach(t *testing.T) {
	a := &ccbillAdapter{}
	ids, err := a.Attach(context.Background(), map[string]string{
		models.ProcessorKeyCCBillFormName: "premium",
		models.ProcessorKeyCCBillFlexID:   "abc-123",
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if ids[models.ProcessorKeyCCBillFormName] != "premium" || ids[models.ProcessorKeyCCBillFlexID] != "abc-123" {
		t.Fatalf("unexpected ids: %v", ids)
	}

	if _, err := a.Attach(context.Background(), map[string]string{"form_name": "premium"}); err == nil {
		t.Fatal("expected error when flex_id missing")
	}
}

func TestCCBillAdapter_AutoCreatePending(t *testing.T) {
	a := &ccbillAdapter{}
	_, err := a.AutoCreate(context.Background(), autoCreateContext{})
	if err != errPendingManualLink {
		t.Fatalf("expected errPendingManualLink, got %v", err)
	}
	tmpl := a.PendingActionTemplate(uuid.New())
	if tmpl.Provider != "ccbill" || tmpl.Action != "create_flexform" {
		t.Fatalf("unexpected template: %+v", tmpl)
	}
}

func TestMobiusAdapter_Attach(t *testing.T) {
	a := &mobiusAdapter{}
	ids, err := a.Attach(context.Background(), map[string]string{models.ProcessorKeyPlanID: "premium_monthly"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if ids[models.ProcessorKeyPlanID] != "premium_monthly" || ids[models.ProcessorKeyProvider] != "mobius" {
		t.Fatalf("unexpected ids: %v", ids)
	}
	if _, err := a.Attach(context.Background(), map[string]string{}); err == nil {
		t.Fatal("expected error when plan_id missing")
	}
}

func TestMobiusAdapter_AutoCreatePending(t *testing.T) {
	a := &mobiusAdapter{}
	_, err := a.AutoCreate(context.Background(), autoCreateContext{})
	if err != errPendingManualLink {
		t.Fatalf("expected errPendingManualLink, got %v", err)
	}
	tmpl := a.PendingActionTemplate(uuid.New())
	if tmpl.Provider != "mobius" || tmpl.Action != "create_recurring_plan" {
		t.Fatalf("unexpected template: %+v", tmpl)
	}
	if tmpl.PatchRequired["provider_links"]["mobius"]["plan_id"] == "" {
		t.Fatalf("expected patch_required to mention plan_id, got %+v", tmpl.PatchRequired)
	}
}

func TestStripeAdapter_AttachRequiresPriceID(t *testing.T) {
	a := &stripeAdapter{svc: newUnconfiguredService()}
	if _, err := a.Attach(context.Background(), map[string]string{}); err == nil {
		t.Fatal("expected error when price_id missing")
	}
	ids, err := a.Attach(context.Background(), map[string]string{
		models.ProcessorKeyStripePriceID:   "price_xxx",
		models.ProcessorKeyStripeProductID: "prod_yyy",
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if ids[models.ProcessorKeyStripePriceID] != "price_xxx" || ids[models.ProcessorKeyStripeProductID] != "prod_yyy" {
		t.Fatalf("unexpected ids: %v", ids)
	}
}

func TestStripeAdapter_AutoCreateUnconfiguredIsPending(t *testing.T) {
	a := &stripeAdapter{svc: newUnconfiguredService()}
	_, err := a.AutoCreate(context.Background(), autoCreateContext{PriceID: uuid.New(), ProductID: uuid.New()})
	if err != errPendingManualLink {
		t.Fatalf("expected errPendingManualLink when stripe unconfigured, got %v", err)
	}
}

// -- Dispatcher (resolveProviders) tests -------------------------------------

func TestResolveProviders_AllLinked(t *testing.T) {
	s := newUnconfiguredService()
	productID := uuid.New()
	priceID := uuid.New()
	req := CreatePriceRequest{
		ProductID:  productID,
		UnitAmount: 999,
		Currency:   "usd",
		Providers:  []string{"stripe", "ccbill", "mobius"},
		ProviderLinks: map[string]map[string]string{
			"stripe": {models.ProcessorKeyStripePriceID: "price_xxx"},
			"ccbill": {"form_name": "premium", "flex_id": "abc-123"},
			"mobius": {"plan_id": "premium_monthly"},
		},
	}
	processors, states, pending, err := s.resolveProviders(context.Background(), &models.Product{ID: productID}, req, priceID)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("expected no pending actions, got %v", pending)
	}
	for _, name := range []string{"stripe", "ccbill", "mobius"} {
		if states[name].Status != ProviderStatusLinked {
			t.Errorf("%s: expected linked, got %s", name, states[name].Status)
		}
		if len(processors[name]) == 0 {
			t.Errorf("%s: expected processors entry", name)
		}
	}
}

func TestResolveProviders_MixedLinkedAndPending(t *testing.T) {
	s := newUnconfiguredService()
	productID := uuid.New()
	priceID := uuid.New()
	req := CreatePriceRequest{
		ProductID:  productID,
		UnitAmount: 999,
		Currency:   "usd",
		Providers:  []string{"ccbill", "mobius"},
		ProviderLinks: map[string]map[string]string{
			"ccbill": {"form_name": "premium", "flex_id": "abc-123"},
			// mobius intentionally has no link -> pending
		},
	}
	processors, states, pending, err := s.resolveProviders(context.Background(), &models.Product{ID: productID}, req, priceID)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if states["ccbill"].Status != ProviderStatusLinked {
		t.Errorf("ccbill: expected linked, got %s", states["ccbill"].Status)
	}
	if states["mobius"].Status != ProviderStatusPendingManualLink {
		t.Errorf("mobius: expected pending_manual_link, got %s", states["mobius"].Status)
	}
	if _, ok := processors["mobius"]; ok {
		t.Error("mobius should not have a processors entry while pending")
	}
	if len(pending) != 1 || pending[0].Provider != "mobius" {
		t.Fatalf("expected one mobius pending action, got %v", pending)
	}
}

func TestResolveProviders_AllPending(t *testing.T) {
	s := newUnconfiguredService()
	productID := uuid.New()
	req := CreatePriceRequest{
		ProductID:  productID,
		UnitAmount: 999,
		Currency:   "usd",
		Providers:  []string{"ccbill", "mobius"},
	}
	_, states, pending, err := s.resolveProviders(context.Background(), &models.Product{ID: productID}, req, uuid.New())
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if states["ccbill"].Status != ProviderStatusPendingManualLink || states["mobius"].Status != ProviderStatusPendingManualLink {
		t.Fatalf("expected both pending, got %+v", states)
	}
	if len(pending) != 2 {
		t.Fatalf("expected two pending actions, got %d: %v", len(pending), pending)
	}
}

func TestResolveProviders_UnknownProviderDropped(t *testing.T) {
	s := newUnconfiguredService()
	productID := uuid.New()
	req := CreatePriceRequest{
		ProductID:  productID,
		UnitAmount: 999,
		Currency:   "usd",
		Providers:  []string{"paypal"}, // not in dispatch table
	}
	processors, states, pending, err := s.resolveProviders(context.Background(), &models.Product{ID: productID}, req, uuid.New())
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(processors) != 0 || len(states) != 0 || len(pending) != 0 {
		t.Fatalf("expected unknown provider to be dropped, got processors=%v states=%v pending=%v", processors, states, pending)
	}
}

func TestResolveProviders_LinkOnlyInProviderLinks(t *testing.T) {
	// A provider supplied only via provider_links (absent from Providers) is
	// still attached.
	s := newUnconfiguredService()
	productID := uuid.New()
	req := CreatePriceRequest{
		ProductID:  productID,
		UnitAmount: 999,
		Currency:   "usd",
		ProviderLinks: map[string]map[string]string{
			"ccbill": {"form_name": "premium", "flex_id": "abc-123"},
		},
	}
	_, states, _, err := s.resolveProviders(context.Background(), &models.Product{ID: productID}, req, uuid.New())
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if states["ccbill"].Status != ProviderStatusLinked {
		t.Fatalf("expected ccbill linked, got %s", states["ccbill"].Status)
	}
}
