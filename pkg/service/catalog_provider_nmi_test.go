package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/nmi"
)

func intPtr(i int) *int { return &i }

// newMobiusAdapterWithServer builds a nmiAdapter whose NMI client points at
// the given test server for both direct-post and query traffic.
func newMobiusAdapterWithServer(t *testing.T, serverURL string) *nmiAdapter {
	t.Helper()
	client, err := nmi.NewClient("mobius", &config.NMIProviderSettings{
		SecurityKey: "test-security-key",
	}, false)
	if err != nil {
		t.Fatalf("new nmi client: %v", err)
	}
	client.DirectPostURL = serverURL
	client.QueryURL = serverURL
	svc := &Service{rt: &app.Runtime{NMIClients: map[string]*nmi.NMIClient{"nmi": client}}}
	return &nmiAdapter{svc: svc}
}

func TestMobiusAdapter_DeterministicPlanIDFormat(t *testing.T) {
	got := nmiDeterministicPlanID("premium", "usd", 23_000_000, intPtr(30))
	want := "premium-usd-23000000-30"
	if got != want {
		t.Fatalf("plan_id format drift: got %q want %q", got, want)
	}
	// No "openrails-"/merchant/application prefix: the content key IS the whole id.
	if strings.HasPrefix(got, "openrails-") {
		t.Fatalf("generated plan_id must not carry an openrails- prefix: %q", got)
	}
	// Content-addressed: no price-UUID input, so it is stable across a fresh DB;
	// unchanged by cosmetic edits; distinct when money terms change.
	if nmiDeterministicPlanID("premium", "usd", 23_000_000, intPtr(30)) != want {
		t.Error("plan_id must be deterministic for identical content")
	}
	if nmiDeterministicPlanID("premium", "usd", 29_000_000, intPtr(30)) == want {
		t.Error("a different amount must yield a different plan_id")
	}
	if nmiDeterministicPlanID("premium", "usd", 23_000_000, intPtr(365)) == want {
		t.Error("a different cycle must yield a different plan_id")
	}
	if nmiDeterministicPlanID("basic", "usd", 23_000_000, intPtr(30)) == want {
		t.Error("a different product key must yield a different plan_id")
	}
}

func TestMobiusAdapter_AutoCreateUnconfiguredIsPending(t *testing.T) {
	// No NMI client configured -> fall back to manual link.
	a := &nmiAdapter{svc: &Service{rt: &app.Runtime{}}}
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
		PriceID: uuid.New(), UnitAmount: 9_990_000, BillingCycleDays: nil,
	})
	if err == nil {
		t.Fatal("expected error when recurring day cadence is nil")
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
		PriceID: priceID, ProductKey: "pro", Currency: "usd", UnitAmount: 9_990_000, BillingCycleDays: intPtr(30),
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !addCalled {
		t.Fatal("expected AddRecurringPlan to be called for a missing plan")
	}
	if ids[models.RailKeyPlanID] != nmiDeterministicPlanID("pro", "usd", 9_990_000, intPtr(30)) {
		t.Fatalf("plan_id mismatch: %v", ids)
	}
	if ids[models.RailKeyProvider] != "mobius" {
		t.Fatalf("provider mismatch: %v", ids)
	}
}

// TestMobiusAdapter_AutoCreateTargetsSecondaryAccount proves #641 catalog
// secondary-sync routing: with a TargetAccountID set, AutoCreate uses THAT
// account's NMI client (not the primary alias), so a catalog push reaches the
// secondary account.
func TestMobiusAdapter_AutoCreateTargetsSecondaryAccount(t *testing.T) {
	var primaryCalled, secondaryCalled bool
	mkServer := func(flag *bool) *httptest.Server {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = r.ParseForm()
			if r.Form.Get("recurring") == "add_plan" {
				*flag = true
				_, _ = w.Write([]byte("response=1"))
				return
			}
			_, _ = w.Write([]byte("<nm_response></nm_response>"))
		}))
		t.Cleanup(s.Close)
		return s
	}
	primarySrv := mkServer(&primaryCalled)
	secondarySrv := mkServer(&secondaryCalled)

	mkClient := func(url string) *nmi.NMIClient {
		c, err := nmi.NewClient("acct", &config.NMIProviderSettings{SecurityKey: "k"}, false)
		if err != nil {
			t.Fatalf("new nmi client: %v", err)
		}
		c.DirectPostURL = url
		c.QueryURL = url
		return c
	}
	svc := &Service{rt: &app.Runtime{NMIClients: map[string]*nmi.NMIClient{
		"nmi":    mkClient(primarySrv.URL),   // primary (rail-key alias)
		"100002": mkClient(secondarySrv.URL), // secondary, keyed by account_id
	}}}
	a := &nmiAdapter{svc: svc}

	if _, err := a.AutoCreate(context.Background(), autoCreateContext{
		PriceID: uuid.New(), ProductKey: "pro", Currency: "usd", UnitAmount: 9_990_000,
		BillingCycleDays: intPtr(30), TargetAccountID: "100002",
	}); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !secondaryCalled {
		t.Fatal("secondary account's client must receive the plan create")
	}
	if primaryCalled {
		t.Fatal("primary account's client must NOT be called when targeting the secondary")
	}
}

func TestMobiusAdapter_AutoCreateAttachNoDuplicate(t *testing.T) {
	// A FRESH-DB price gets a new random UUID, but the content-addressed plan_id is
	// unchanged, so find-or-attach reattaches to the existing NMI plan (no create).
	priceID := uuid.New()
	planID := nmiDeterministicPlanID("pro", "usd", 9_990_000, intPtr(30))
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
		PriceID: priceID, ProductKey: "pro", Currency: "usd", UnitAmount: 9_990_000, BillingCycleDays: intPtr(30),
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if ids[models.RailKeyPlanID] != planID {
		t.Fatalf("plan_id mismatch: %v", ids)
	}
}

// nmiPlanQueryXML renders a single-plan recurring_plans query response.
func nmiPlanQueryXML(planID, planName, planAmount, dayFrequency string) string {
	return "<nm_response><plan><plan_id>" + planID + "</plan_id><plan_name>" + planName +
		"</plan_name><plan_amount>" + planAmount + "</plan_amount><day_frequency>" + dayFrequency +
		"</day_frequency></plan></nm_response>"
}

func TestMobiusAdapter_AttachValidatesLinkAndCreatesNothing(t *testing.T) {
	// A supplied link to a plan that EXISTS and MATCHES the price money terms is
	// accepted, and Attach must never create a plan (no add_plan).
	planID := "premium-usd-999-30"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("recurring") == "add_plan" {
			t.Error("Attach must not create an NMI plan when a valid link is supplied")
		}
		_, _ = w.Write([]byte(nmiPlanQueryXML(planID, "Premium", "9.99", "30")))
	}))
	t.Cleanup(server.Close)
	a := newMobiusAdapterWithServer(t, server.URL)

	ids, err := a.Attach(context.Background(),
		map[string]string{models.RailKeyPlanID: planID},
		autoCreateContext{ProductKey: "premium", Currency: "usd", UnitAmount: 9_990_000, BillingCycleDays: intPtr(30)})
	if err != nil {
		t.Fatalf("valid link should attach cleanly, got %v", err)
	}
	if ids[models.RailKeyPlanID] != planID || ids[models.RailKeyProvider] != "mobius" {
		t.Fatalf("unexpected ids: %v", ids)
	}
}

func TestMobiusAdapter_AttachCreatesMissingPlanAtOperatorID(t *testing.T) {
	// NMI plan_ids are client-creatable, so a link to a not-yet-existing plan_id
	// is a find-or-CREATE at that operator-chosen id (e.g. "premium").
	var addCalled bool
	var addedPlanID, addedFreq, addedAmount string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("recurring") == "add_plan" {
			addCalled = true
			addedPlanID = r.Form.Get("plan_id")
			addedFreq = r.Form.Get("day_frequency")
			addedAmount = r.Form.Get("plan_amount")
			_, _ = w.Write([]byte("response=1"))
			return
		}
		_, _ = w.Write([]byte("<nm_response></nm_response>")) // query: not found
	}))
	t.Cleanup(server.Close)
	a := newMobiusAdapterWithServer(t, server.URL)

	ids, err := a.Attach(context.Background(),
		map[string]string{models.RailKeyPlanID: "premium"},
		autoCreateContext{ProductKey: "premium", Currency: "usd", UnitAmount: 9_990_000, BillingCycleDays: intPtr(30)})
	if err != nil {
		t.Fatalf("missing plan should be created, got %v", err)
	}
	if !addCalled {
		t.Fatal("expected AddRecurringPlan to be called for a missing operator-supplied plan_id")
	}
	if addedPlanID != "premium" || addedFreq != "30" || addedAmount != "9.99" {
		t.Fatalf("created plan with wrong terms: id=%q freq=%q amount=%q", addedPlanID, addedFreq, addedAmount)
	}
	if ids[models.RailKeyPlanID] != "premium" {
		t.Fatalf("unexpected ids: %v", ids)
	}
}

func TestMobiusAdapter_AttachMissingPlanRequiresCycleToCreate(t *testing.T) {
	// A missing plan_id with no billing cycle cannot be created (NMI plans need a
	// frequency) -> loud, actionable error.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("recurring") == "add_plan" {
			t.Error("must not create an NMI plan without a billing cycle")
		}
		_, _ = w.Write([]byte("<nm_response></nm_response>")) // query: not found
	}))
	t.Cleanup(server.Close)
	a := newMobiusAdapterWithServer(t, server.URL)

	_, err := a.Attach(context.Background(),
		map[string]string{models.RailKeyPlanID: "premium"},
		autoCreateContext{UnitAmount: 9_990_000, BillingCycleDays: nil})
	if err == nil || !strings.Contains(err.Error(), "recurring day cadence") {
		t.Fatalf("expected a loud recurring day cadence error, got %v", err)
	}
}

func TestMobiusAdapter_AttachRejectsAmountMismatch(t *testing.T) {
	planID := "premium-usd-999-30"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Remote plan bills 5.00 (500 cents) but the catalog price is 9_990_000 micros.
		_, _ = w.Write([]byte(nmiPlanQueryXML(planID, "Premium", "5.00", "30")))
	}))
	t.Cleanup(server.Close)
	a := newMobiusAdapterWithServer(t, server.URL)

	_, err := a.Attach(context.Background(),
		map[string]string{models.RailKeyPlanID: planID},
		autoCreateContext{UnitAmount: 9_990_000, BillingCycleDays: intPtr(30)})
	if err == nil || !strings.Contains(err.Error(), "amount") {
		t.Fatalf("expected an amount-mismatch error, got %v", err)
	}
}

func TestMobiusAdapter_AttachRejectsCycleMismatch(t *testing.T) {
	planID := "premium-usd-999-30"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Remote plan bills every 365 days but the catalog price is 30-day.
		_, _ = w.Write([]byte(nmiPlanQueryXML(planID, "Premium", "9.99", "365")))
	}))
	t.Cleanup(server.Close)
	a := newMobiusAdapterWithServer(t, server.URL)

	_, err := a.Attach(context.Background(),
		map[string]string{models.RailKeyPlanID: planID},
		autoCreateContext{UnitAmount: 9_990_000, BillingCycleDays: intPtr(30)})
	if err == nil || !strings.Contains(err.Error(), "billing cycle") {
		t.Fatalf("expected a billing-cycle-mismatch error, got %v", err)
	}
}

func TestMobiusAdapter_UpdateIsNoop(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("no NMI request expected: NMI plan Update is a no-op")
	}))
	t.Cleanup(server.Close)
	a := newMobiusAdapterWithServer(t, server.URL)

	ids := map[string]string{models.RailKeyPlanID: "p", models.RailKeyProvider: "mobius"}
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

	ids := map[string]string{models.RailKeyPlanID: "p", models.RailKeyProvider: "mobius"}
	drift, missing, err := a.Verify(context.Background(), ids, &priceVerifyContext{
		UnitAmount: 9_990_000,
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
	if fields["unit_amount"] != "5000000" {
		t.Fatalf("expected unit_amount drift (micros), got %v", drift)
	}
}

func TestMobiusAdapter_VerifyMissingPlan(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<nm_response></nm_response>"))
	}))
	t.Cleanup(server.Close)
	a := newMobiusAdapterWithServer(t, server.URL)

	ids := map[string]string{models.RailKeyPlanID: "p", models.RailKeyProvider: "mobius"}
	_, missing, err := a.Verify(context.Background(), ids, &priceVerifyContext{})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !missing {
		t.Fatal("expected missing=true for absent plan")
	}
}

func TestMobiusAdapter_VerifyUnconfiguredIsSyncDisabled(t *testing.T) {
	a := &nmiAdapter{svc: &Service{rt: &app.Runtime{}}}
	drift, missing, err := a.Verify(context.Background(), map[string]string{models.RailKeyPlanID: "p"}, &priceVerifyContext{})
	if err != nil || missing || drift != nil {
		t.Fatalf("expected sync_disabled (nil,false,nil), got drift=%v missing=%v err=%v", drift, missing, err)
	}
}
