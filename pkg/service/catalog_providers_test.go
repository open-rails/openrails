package service

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/config"
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
	}, autoCreateContext{})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if ids[models.ProcessorKeyCCBillFormName] != "premium" || ids[models.ProcessorKeyCCBillFlexID] != "abc-123" {
		t.Fatalf("unexpected ids: %v", ids)
	}

	if _, err := a.Attach(context.Background(), map[string]string{"form_name": "premium"}, autoCreateContext{}); err == nil {
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
	ids, err := a.Attach(context.Background(), map[string]string{models.ProcessorKeyPlanID: "premium_monthly"}, autoCreateContext{})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if ids[models.ProcessorKeyPlanID] != "premium_monthly" || ids[models.ProcessorKeyProvider] != "mobius" {
		t.Fatalf("unexpected ids: %v", ids)
	}
	if _, err := a.Attach(context.Background(), map[string]string{}, autoCreateContext{}); err == nil {
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
	if _, err := a.Attach(context.Background(), map[string]string{}, autoCreateContext{}); err == nil {
		t.Fatal("expected error when price_id missing")
	}
	ids, err := a.Attach(context.Background(), map[string]string{
		models.ProcessorKeyStripePriceID:   "price_xxx",
		models.ProcessorKeyStripeProductID: "prod_yyy",
	}, autoCreateContext{})
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

// -- Mode-gated catalog writes (#346) ----------------------------------------

// TestResolveProviders_RemoteWritesDisabledDefersAutoCreate verifies that in
// limited/readonly mode the dispatcher never calls AutoCreate: every provider
// slot defers to pending_manual_link with the mode message, and the price
// still applies locally.
func TestResolveProviders_RemoteWritesDisabledDefersAutoCreate(t *testing.T) {
	svc := &Service{rt: &app.Runtime{Config: &config.Config{Mode: config.ModeLimited}}}
	priceID := uuid.New()
	processors, states, pending, err := svc.resolveProviders(context.Background(), &models.Product{Slug: "premium"}, CreatePriceRequest{
		Providers:  []string{"stripe", "mobius"},
		UnitAmount: 2300,
		Currency:   "usd",
	}, priceID)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(processors) != 0 {
		t.Fatalf("no provider objects may be linked in limited mode, got %v", processors)
	}
	for _, name := range []string{"stripe", "mobius"} {
		st, ok := states[name]
		if !ok || st.Status != ProviderStatusPendingManualLink {
			t.Fatalf("%s: expected pending_manual_link, got %+v", name, st)
		}
		if st.Message != remoteWritesDisabledMessage {
			t.Fatalf("%s: expected mode message, got %q", name, st.Message)
		}
	}
	if len(pending) != 2 {
		t.Fatalf("expected 2 pending actions, got %d", len(pending))
	}
}

// TestMobiusAdapter_AttachMissingPlanDeferredWhenWritesDisabled verifies the
// Attach find-or-create half: an explicit link whose NMI plan does NOT exist
// must defer (errRemoteWritesDisabled), never create, when writes are blocked.
func TestMobiusAdapter_AttachMissingPlanDeferredWhenWritesDisabled(t *testing.T) {
	var sawWrite bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("report_type") != "" {
			// Query API: report the plan as missing.
			w.Write([]byte(`<nm_response></nm_response>`))
			return
		}
		sawWrite = true // any direct-post request would be a remote write
		w.Write([]byte("response=1"))
	}))
	defer server.Close()

	a := newMobiusAdapterWithServer(t, server.URL)
	cycle := 30
	_, err := a.Attach(context.Background(), map[string]string{models.ProcessorKeyPlanID: "premium-usd-2300-30"}, autoCreateContext{
		ProductSlug: "premium", UnitAmount: 2300, Currency: "usd", BillingCycleDays: &cycle,
		RemoteWritesDisabled: true,
	})
	if !errors.Is(err, errRemoteWritesDisabled) {
		t.Fatalf("expected errRemoteWritesDisabled, got %v", err)
	}
	if sawWrite {
		t.Fatal("adapter performed a direct-post write while writes were disabled")
	}
}
