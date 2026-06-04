package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/nmi"
)

func intPtr(i int) *int { return &i }

// newMobiusAdapterWithServer builds a mobiusAdapter whose NMI client points at
// the given test server for both direct-post and query traffic.
func newMobiusAdapterWithServer(t *testing.T, serverURL string) *mobiusAdapter {
	t.Helper()
	client, err := nmi.NewClient("mobius", &config.NMIProviderSettings{
		SecurityKey: "test-security-key",
	}, false)
	if err != nil {
		t.Fatalf("new nmi client: %v", err)
	}
	client.DirectPostURL = serverURL
	client.QueryURL = serverURL
	svc := &Service{rt: &app.Runtime{NMIClients: map[string]*nmi.NMIClient{"mobius": client}}}
	return &mobiusAdapter{svc: svc}
}

func TestMobiusAdapter_DeterministicPlanIDFormat(t *testing.T) {
	id := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	got := mobiusDeterministicPlanID(id)
	want := "openrails-11111111-2222-3333-4444-555555555555"
	if got != want {
		t.Fatalf("plan_id format drift: got %q want %q", got, want)
	}
}

func TestMobiusAdapter_AutoCreateUnconfiguredIsPending(t *testing.T) {
	// No NMI client configured -> fall back to manual link.
	a := &mobiusAdapter{svc: &Service{rt: &app.Runtime{}}}
	_, err := a.AutoCreate(context.Background(), autoCreateContext{
		PriceID: uuid.New(), BillingCycleDays: intPtr(30),
	})
	if err != errPendingManualLink {
		t.Fatalf("expected errPendingManualLink, got %v", err)
	}
}

func TestMobiusAdapter_AutoCreateRejectsNilFrequency(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("response=1"))
	}))
	t.Cleanup(server.Close)
	a := newMobiusAdapterWithServer(t, server.URL)

	_, err := a.AutoCreate(context.Background(), autoCreateContext{
		PriceID: uuid.New(), UnitAmount: 999, BillingCycleDays: nil,
	})
	if err == nil {
		t.Fatal("expected error when billing_cycle_days is nil")
	}
}

func TestMobiusAdapter_AutoCreateFreshCreate(t *testing.T) {
	var addCalled bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		switch r.Form.Get("recurring") {
		case "add_plan":
			addCalled = true
			if got := r.Form.Get("day_frequency"); got != "30" {
				t.Errorf("day_frequency: got %q want 30", got)
			}
			if got := r.Form.Get("plan_amount"); got != "9.99" {
				t.Errorf("plan_amount: got %q want 9.99", got)
			}
			_, _ = w.Write([]byte("response=1"))
		default:
			// recurring_plans query -> not found
			_, _ = w.Write([]byte("<nm_response></nm_response>"))
		}
	}))
	t.Cleanup(server.Close)
	a := newMobiusAdapterWithServer(t, server.URL)

	priceID := uuid.New()
	ids, err := a.AutoCreate(context.Background(), autoCreateContext{
		PriceID: priceID, UnitAmount: 999, BillingCycleDays: intPtr(30),
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !addCalled {
		t.Fatal("expected AddRecurringPlan to be called for a missing plan")
	}
	if ids[models.ProcessorKeyPlanID] != mobiusDeterministicPlanID(priceID) {
		t.Fatalf("plan_id mismatch: %v", ids)
	}
	if ids[models.ProcessorKeyProvider] != "mobius" {
		t.Fatalf("provider mismatch: %v", ids)
	}
}

func TestMobiusAdapter_AutoCreateAttachNoDuplicate(t *testing.T) {
	priceID := uuid.New()
	planID := mobiusDeterministicPlanID(priceID)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("recurring") == "add_plan" {
			t.Error("AddRecurringPlan should NOT be called when plan already exists")
		}
		// recurring_plans query -> found
		_, _ = w.Write([]byte("<nm_response><plan><plan_id>" + planID +
			"</plan_id><plan_name>Premium</plan_name><plan_amount>9.99</plan_amount></plan></nm_response>"))
	}))
	t.Cleanup(server.Close)
	a := newMobiusAdapterWithServer(t, server.URL)

	ids, err := a.AutoCreate(context.Background(), autoCreateContext{
		PriceID: priceID, UnitAmount: 999, BillingCycleDays: intPtr(30),
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if ids[models.ProcessorKeyPlanID] != planID {
		t.Fatalf("plan_id mismatch: %v", ids)
	}
}

func TestMobiusAdapter_UpdateIsNoop(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("no NMI request expected: NMI plan Update is a no-op")
	}))
	t.Cleanup(server.Close)
	a := newMobiusAdapterWithServer(t, server.URL)

	ids := map[string]string{models.ProcessorKeyPlanID: "p", models.ProcessorKeyProvider: "mobius"}
	if err := a.Update(context.Background(), ids, mutableUpdate{}); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
}

func TestMobiusAdapter_VerifyDetectsDrift(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<nm_response><plan><plan_id>p</plan_id>" +
			"<plan_name>Remote Name</plan_name><plan_amount>5.00</plan_amount></plan></nm_response>"))
	}))
	t.Cleanup(server.Close)
	a := newMobiusAdapterWithServer(t, server.URL)

	ids := map[string]string{models.ProcessorKeyPlanID: "p", models.ProcessorKeyProvider: "mobius"}
	drift, missing, err := a.Verify(context.Background(), ids, &priceVerifyContext{
		UnitAmount: 999,
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if missing {
		t.Fatal("plan should not be reported missing")
	}
	fields := map[string]string{}
	for _, d := range drift {
		fields[d.Field] = d.RemoteValue
	}
	if fields["unit_amount"] != "500" {
		t.Fatalf("expected unit_amount drift (cents), got %v", drift)
	}
}

func TestMobiusAdapter_VerifyMissingPlan(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<nm_response></nm_response>"))
	}))
	t.Cleanup(server.Close)
	a := newMobiusAdapterWithServer(t, server.URL)

	ids := map[string]string{models.ProcessorKeyPlanID: "p", models.ProcessorKeyProvider: "mobius"}
	_, missing, err := a.Verify(context.Background(), ids, &priceVerifyContext{})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !missing {
		t.Fatal("expected missing=true for absent plan")
	}
}

func TestMobiusAdapter_VerifyUnconfiguredIsSyncDisabled(t *testing.T) {
	a := &mobiusAdapter{svc: &Service{rt: &app.Runtime{}}}
	drift, missing, err := a.Verify(context.Background(), map[string]string{models.ProcessorKeyPlanID: "p"}, &priceVerifyContext{})
	if err != nil || missing || drift != nil {
		t.Fatalf("expected sync_disabled (nil,false,nil), got drift=%v missing=%v err=%v", drift, missing, err)
	}
}
