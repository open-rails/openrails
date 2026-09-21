//go:build integration

package riverjobs

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/config"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/reconcile/converge"
	"github.com/stretchr/testify/require"
)

// TestSubscriptionLifecycleSimulation (tracker issue #773 — this task was
// briefed as "#771", but #771 was already claimed by an unrelated
// subscription-repricing design when this work started; filed as a new
// tracker entry instead of clobbering it) drives month-scale subscriptions
// through the REAL lifecycle machinery — the converge engine (DERIVE/LIFE)
// that turns a lapsed `active` row into `past_due` or `unknown`, and the
// DunningWorker/manual-rebill path that turns a `past_due` row back into
// `active` (or terminally cancels it) — entirely under a clockwork.FakeClock,
// ticking one simulated day at a time. No River scheduler/client runs; each
// tick calls the workers' entry points directly.
//
// Doctrine asserted throughout (derived from the code, not invented):
//   - collection.Window / MaxFailures / RetryOffsets
//     (internal/modules/subscriptions/dunning.go): a >=28-day ("monthly")
//     billing cycle retries at +2d/+5d/+9d/+13d after the first failure (5
//     failures total) and stays recoverable for a 14-day window.
//   - reconcile.Decide's first-party law (internal/reconcile/decider.go
//     decideFromFirstParty): an active row past its period end enters
//     past_due ONLY with ownership evidence (a completed payment that opened
//     the current period, or a fresher rail watermark) — otherwise it waits
//     out PeriodGrace (48h) and then PARKS as `unknown`, never guessing at a
//     charge that was never OpenRails' to make.
//   - gen.SubscriptionProjectsStandingAccess: auto_renew && status IN
//     (pending, active, past_due, unknown) projects access — entitlements
//     stay live through past_due AND unknown, revoked only by a proven
//     terminal event (FailMembership's terminal cancel).
func TestSubscriptionLifecycleSimulation(t *testing.T) {
	ctx := context.Background()
	// The simulation drives module services and worker INTERNALS directly
	// (processSubscription, Converge, CreditExpiryWorker) — it stands in for the
	// layer that opens the merchant connection in production, so it must supply
	// what that layer supplies. Proving a worker pins its own merchant is a
	// different test's job (or#862); on an unpinned handle every seed write and
	// every lifecycle read here hits the FORCEd RLS and the simulation asserts
	// on nothing.
	dbi := dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID())
	pool := dbtest.SharedMerchantPool(t, dbtest.TestMerchantID.UUID())
	dbtest.EnsureTestMerchant(ctx, t, pool)
	mctx := dbtest.WithTestMerchant(ctx)
	// #836/#839: terminal collection outcomes are gated on the destructive kill
	// switch, which ships OFF — a fresh deployment cancels nothing until an
	// operator arms it. This simulation asserts what a live, REVIEWED deployment
	// does, so it puts itself in that state.
	require.NoError(t, dbi.RunInMerchantConn(mctx, func(sctx context.Context) error {
		dbtest.ArmDestructiveActions(sctx, t, dbtest.TestMerchantID.UUID())
		return nil
	}))
	t.Cleanup(func() {
		_ = dbi.RunInMerchantConn(mctx, func(sctx context.Context) error {
			dbtest.DisarmDestructiveActions(sctx, t, dbi.Qx(sctx))
			return nil
		})
	})

	t.Run("happy_renewals", func(t *testing.T) {
		testHappyRenewals(t, mctx, dbi)
	})
	t.Run("dunning_recovery", func(t *testing.T) {
		testDunningRecovery(t, mctx, dbi)
	})
	t.Run("exhausted_dunning_and_no_evidence_parking", func(t *testing.T) {
		t.Run("exhausted_dunning", func(t *testing.T) {
			testExhaustedDunning(t, mctx, dbi)
		})
		t.Run("no_evidence_parking", func(t *testing.T) {
			testNoEvidenceParking(t, mctx, dbi)
		})
	})
}

// --- shared simulation harness -------------------------------------------

const (
	// simCycleHours is the "monthly" tier (>=28d): retry offsets +2/+5/+9/+13d,
	// dunning window 14d, max 5 failures (collection.RetryOffsets/Window/MaxFailures).
	simCycleHours = 30 * 24
)

// simSub is one seeded subscription plus the identifiers a scenario asserts against.
type simSub struct {
	pspID               uuid.UUID
	providerID, vaultID string
	subID               uuid.UUID
	customerID          uuid.UUID
	entName             string
}

// seedSimSubscription creates an entitlement-bearing product, a monthly price,
// an NMI payment method,
// and a subscription starting `active` for [periodStart, periodStart+cycle).
// When withInitialPayment is true it also records a completed payment at
// periodStart — the checkout charge that "opens" the period and is the #664
// ownership evidence the LIFE pass requires before it will ever move an
// active row into past_due (decideFromFirstParty /
// ListLapsedSubscriptionsWithEvidence's payment_opened_period leg). Omitting
// it is the no-evidence-parking case: the row waits out PeriodGrace and then
// parks as `unknown` instead of ever entering dunning.
func seedSimSubscription(t *testing.T, ctx context.Context, dbi *db.DB, periodStart time.Time, withInitialPayment bool) simSub {
	t.Helper()
	pool := dbtest.SharedMerchantPool(t, dbtest.TestMerchantID.UUID())
	q := dbtest.Queries(pool)
	entName := "sim_pro_access_" + uuid.New().String()[:8]
	productID := uuid.New()
	priceID := uuid.New()
	paymentMethodID := uuid.New()
	subID := uuid.New()
	userID := uuid.New().String()
	now := periodStart

	entitlementsSpec := map[string]*int{entName: nil}
	entitlementsSpecJSON, err := json.Marshal(entitlementsSpec)
	require.NoError(t, err)

	description := "Simulation product"
	_, err = q.CreateProduct(ctx, gen.CreateProductParams{
		ID: productID, Key: "sim_product_" + uuid.New().String(), DisplayName: "Sim Product",
		MerchantID: dbtest.TestMerchantID.UUID(), Description: &description,
		EntitlementsSpec: entitlementsSpecJSON,
		Archived:         false, CreatedAt: now, UpdatedAt: now,
	})
	require.NoError(t, err)

	cycleHours32 := int32(simCycleHours)
	_, err = q.CreatePrice(ctx, gen.CreatePriceParams{
		ID: priceID, ProductID: productID, Amount: 9990000, Currency: "USD", MerchantID: dbtest.TestMerchantID.UUID(),
		Archived: false, AccessDurationHours: &cycleHours32, AutoRenew: true,
		CreatedAt: now, UpdatedAt: now,
	})
	require.NoError(t, err)

	tenantSubjectID := dbtest.EnsureCustomerIDPgx(ctx, t, pool, userID)
	pspID := dbtest.EnsureTestPSP(ctx, t, pool, dbtest.TestMerchantID.UUID(), "nmi")
	billingID := "bill_" + uuid.New().String()
	vaultID, providerID := "vault_"+uuid.NewString(), "sub_sim_"+uuid.NewString()
	_, err = q.CreatePaymentMethod(ctx, gen.CreatePaymentMethodParams{
		ID: paymentMethodID, MerchantID: dbtest.TestMerchantID.UUID(), CustomerID: tenantSubjectID, Rail: "nmi",
		PspID:           pspID,
		RailCustomerRef: vaultID, RailMethodRef: billingID,

		InitialTransactionID: "txn_initial_" + uuid.New().String(),
		CreatedAt:            now, UpdatedAt: now,
	})
	require.NoError(t, err)
	dbtest.SeedNMIStoredCredentialRefs(ctx, t, pool, paymentMethodID)

	periodEnd := periodStart.Add(simCycleHours * time.Hour)
	_, err = q.CreateSubscription(ctx, gen.CreateSubscriptionParams{
		CollectionPolicy: string(models.CollectionPolicyProviderDunning),
		ID:               subID, MerchantID: dbtest.TestMerchantID.UUID(), CustomerID: tenantSubjectID, ProductID: productID, PriceID: &priceID,
		Status: string(models.StatusActive), Rail: "nmi",
		PspID:                 pspID,
		RailSubscriptionID:    providerID,
		PaymentMethodID:       &paymentMethodID,
		CurrentPeriodStartsAt: &periodStart, CurrentPeriodEndsAt: &periodEnd,
		StartedAt: periodStart, CreatedAt: now, UpdatedAt: now, EntitlementsSpecSnapshot: entitlementsSpecJSON,
	})
	require.NoError(t, err)

	if withInitialPayment {
		paymentSvc := payments.NewPaymentService(dbi)
		require.NoError(t, paymentSvc.Create(ctx, &models.Payment{
			ID:             uuid.New(),
			CustomerID:     tenantSubjectID,
			PriceID:        priceID,
			SubscriptionID: &subID,
			Rail:           models.RailNMI,
			PspID:          &pspID,
			// or#827: a completed positive charge must DECLARE where the money
			// moved; the signup payment is a real rail settlement.
			MoneyMovement: models.MoneyMovementRail,
			TransactionID: "txn_signup_" + uuid.New().String(),
			Amount:        9990000,
			ListAmount:    9990000,
			Currency:      "USD",
			Status:        payments.PaymentStatusCompletedValue,
			PurchasedAt:   periodStart,
			CreatedAt:     periodStart,
		}))
	}

	t.Cleanup(func() {
		bg := context.Background()
		_, _ = pool.Exec(bg, "DELETE FROM billing.grants WHERE source_id LIKE '%' || $1 || '%'", subID.String())
		_, _ = pool.Exec(bg, "DELETE FROM billing.entitlements WHERE source_id = $1", subID)
		_, _ = pool.Exec(bg, "DELETE FROM billing.payments WHERE subscription_id = $1", subID)
		_, _ = pool.Exec(bg, "DELETE FROM billing.reconciliation_findings WHERE subject_key = $1", "subscription:"+subID.String())
		_, _ = pool.Exec(bg, "DELETE FROM billing.subscriptions WHERE id = $1", subID)
		_, _ = pool.Exec(bg, "DELETE FROM billing.payment_methods WHERE id = $1", paymentMethodID)
		_, _ = pool.Exec(bg, "DELETE FROM billing.prices WHERE id = $1", priceID)
		_, _ = pool.Exec(bg, "DELETE FROM billing.products WHERE id = $1", productID)
	})

	return simSub{subID: subID, customerID: tenantSubjectID, entName: entName, pspID: pspID, providerID: providerID, vaultID: vaultID}
}

// nmiStub is an httptest server speaking NMI's classic Direct Post wire
// format (the same shape internal/river/jobs_dunning_integration_test.go
// stubs). Responses are scripted per-scenario and consumed FIFO; a request
// arriving with the queue empty is an UNSCRIPTED charge attempt — recorded as
// a violation instead of a panic (this handler runs on the server's own
// goroutine, where testing.T.Fatal is unsafe).
type nmiStub struct {
	mu         sync.Mutex
	responses  []string
	requests   int
	violations []string
	server     *httptest.Server
	sub        simSub
	nextDate   string
	payments   map[string]string
}

func newNMIStub(t *testing.T) *nmiStub {
	t.Helper()
	s := &nmiStub{payments: map[string]string{}}
	s.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		s.mu.Lock()
		defer s.mu.Unlock()
		if r.Method == http.MethodGet && r.URL.Path == "/subscriptions/"+s.sub.providerID {
			_ = json.NewEncoder(w).Encode(map[string]any{"id": s.sub.providerID, "customer_vault_id": s.sub.vaultID, "amount": "9.99", "delayed_condition": "active", "paused_subscription": "0", "next_billing_date": s.nextDate, "plan": map[string]any{"id": "plan-sim", "plan_amount": "9.99", "plan_payments": "0", "day_frequency": "30"}})
			return
		}
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/payments/") {
			id := strings.TrimPrefix(r.URL.Path, "/payments/")
			if _, ok := s.payments[id]; !ok {
				w.WriteHeader(404)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "amount": "9.99", "currency": "USD", "customer_vault_id": s.sub.vaultID, "response": "1", "actions": []map[string]any{{"type": "sale", "amount": "9.99", "success": true, "response": "1"}}})
			return
		}
		if r.Form.Get("report_type") == "transaction" {
			fmt.Fprint(w, "<nm_response>")
			for id, order := range s.payments {
				if order == r.Form.Get("order_id") {
					fmt.Fprintf(w, `<transaction><transaction_id>%s</transaction_id><order_id>%s</order_id><action><action_type>sale</action_type><success>1</success></action></transaction>`, id, order)
				}
			}
			fmt.Fprint(w, "</nm_response>")
			return
		}
		if r.Form.Get("type") != "sale" || r.Form.Get("recurring") != "rebill_subscription" {
			s.violations = append(s.violations, "unsupported request: "+r.URL.Path)
			w.WriteHeader(400)
			return
		}
		s.requests++
		var resp string
		if len(s.responses) == 0 {
			s.violations = append(s.violations, fmt.Sprintf("unscripted NMI charge #%d", s.requests))
			resp = "response=3&responsetext=unscripted request"
		} else {
			resp = s.responses[0]
			s.responses = s.responses[1:]
		}
		parsed, _ := url.ParseQuery(resp)
		if parsed.Get("response") == "1" {
			s.payments[parsed.Get("transactionid")] = r.Form.Get("orderid")
		}
		fmt.Fprint(w, resp)

	}))
	t.Cleanup(s.server.Close)
	return s
}

// The simulation explicitly supplies each gateway period; it does not claim
// that Classic rebill_subscription advances the provider schedule this way.
func (s *nmiStub) prepareRenewal(periodEnd time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextDate = periodEnd.Add(simCycleHours * time.Hour).UTC().Format("2006-01-02")
}

// enqueue schedules the next scripted response(s), FIFO.
func (s *nmiStub) enqueue(responses ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.responses = append(s.responses, responses...)
}

func (s *nmiStub) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests
}

// assertDrained fails the test if the script has unconsumed responses or any
// unscripted request occurred — the count-based assertions are only
// meaningful if every wire exchange was accounted for.
func (s *nmiStub) assertDrained(t *testing.T) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	require.Empty(t, s.responses, "scripted NMI responses left unconsumed")
	require.Empty(t, s.violations, "unscripted NMI requests occurred: %v", s.violations)
}

const (
	nmiApprovedFmt  = "response=1&transactionid=%s"
	nmiSoftDeclined = "response=2&response_code=202&responsetext=Insufficient Funds"
	nmiHardDeclined = "response=2&response_code=201&responsetext=Do Not Honor"
)

func approvedResponse() string { return fmt.Sprintf(nmiApprovedFmt, "txn_"+uuid.New().String()) }

// simRig bundles the fake clock and every service the workers need, all
// wired to the SAME clock so period math, dunning-schedule math, and
// credit-expiry math advance in lockstep with the tick loop.
type simRig struct {
	dbi            *db.DB
	clock          *clockwork.FakeClock
	priceSvc       *catalog.PriceService
	productSvc     *catalog.ProductService
	entitlementSvc *entitlements.EntitlementService
	notifSvc       *subscriptions.NotificationService
	paymentSvc     *payments.PaymentService
	moneySvc       *money.MoneyService
	dunning        *DunningWorker
	creditExpiry   CreditExpiryWorker
	engine         *converge.ConvergeEngine
	// tickStep is the simulated-day granularity. Default 24h. The LIFE pass's
	// first-party law only fires strictly AFTER the period end (decideFromFirstParty:
	// `!sub.PeriodEnd.Before(now)` stays a no-op transition while PeriodEnd==now), so
	// a tick lands the FIRST failure at periodEnd+tickStep, not at periodEnd itself —
	// a real deployment sweeps every few minutes, so that detection lag is
	// near-zero; this simulation's daily granularity manufactures up to a full
	// tickStep of lag instead. That lag stacks with the schedule's own last
	// retry offset (13d for the monthly tier): at 24h ticks the 5th attempt
	// lands at periodEnd+24h+13d, exactly ON the 14d dunning window boundary —
	// a knife-edge tie against ClaimRailIntentByID's strict `expires_at > now`
	// claim gate that a real deployment's sub-minute detection lag would never
	// produce. Scenarios that walk the retry schedule out to its last offset
	// use a finer step (see testExhaustedDunning) so the manufactured lag
	// can't collide with a real boundary; this is a simulation-fidelity fix,
	// not a production code change.
	tickStep time.Duration
}

func newSimRig(t *testing.T, dbi *db.DB, start time.Time, stub *nmiStub) *simRig {
	return newSimRigWithStep(t, dbi, start, stub, 24*time.Hour)
}

func newSimRigWithStep(t *testing.T, dbi *db.DB, start time.Time, stub *nmiStub, tickStep time.Duration) *simRig {
	t.Helper()
	clock := clockwork.NewFakeClockAt(start)

	client, err := nmi.NewAccountClient(dbtest.TestMerchantID.UUID(), stub.sub.pspID, "nmi", &config.NMIProviderSettings{
		SecurityKey: "test_security_key", WebhookSecret: "test_secret",
	}, true)
	require.NoError(t, err)
	client.DirectPostURL = stub.server.URL
	client.QueryURL = stub.server.URL
	client.V5BaseURL = stub.server.URL

	priceSvc := catalog.NewPriceService(dbi)
	productSvc := catalog.NewProductService(dbi)
	entitlementSvc := entitlements.NewEntitlementService(dbi, clock)
	notifSvc := subscriptions.NewNotificationService(dbi, nil)
	paymentSvc := payments.NewPaymentService(dbi, clock)

	worker := &DunningWorker{
		DB: dbi, Clock: clock,
		NMIResolver: fakeDunningNMIResolver{client: client},
		// or#865: the worker's self-assembled intent Runner parks every intent
		// when no mode is stated — the simulation renews and dunns for real, so
		// it says "full".
		Config: fullModeConfig(),
	}

	engine := converge.NewConvergeEngine(dbi)
	engine.Now = func() time.Time { return clock.Now().UTC() }

	return &simRig{
		dbi: dbi, clock: clock, priceSvc: priceSvc, productSvc: productSvc,
		entitlementSvc: entitlementSvc, notifSvc: notifSvc, paymentSvc: paymentSvc,
		moneySvc: money.NewMoneyService(dbi, clock), dunning: worker,
		creditExpiry: CreditExpiryWorker{DB: dbi, Clock: clock}, engine: engine,
		tickStep: tickStep,
	}
}

func (r *simRig) lifecycle() *subscriptions.SubscriptionLifecycleService {
	lifecycle := subscriptions.NewSubscriptionLifecycleService(r.dbi, r.productSvc, r.priceSvc, r.entitlementSvc, r.notifSvc, r.paymentSvc, r.clock)
	return lifecycle
}

// convergeToFixpoint drives the DERIVE/LIFE/CON engine to a stable state for
// one scope: a single Converge() pass only applies findings visible BEFORE
// its own repairs land (e.g. "active past period end" -> past_due happens in
// the same pass that "past_due with no retry scheduled" was computed against
// the PRE-repair row), so a multi-hop transition (period_overdue ->
// dunning_overdue, both ClassAuto/#511) needs a second pass in the same tick
// to fully propagate. Looping to zero findings collapses that into one
// simulated day — a faithful compression of a real deployment's
// every-few-minutes sweep cadence.
func convergeToFixpoint(t *testing.T, ctx context.Context, engine *converge.ConvergeEngine, scope converge.Scope) {
	t.Helper()
	for i := 0; i < 6; i++ {
		res, err := engine.Converge(ctx, scope)
		require.NoError(t, err)
		if res.Findings == 0 {
			return
		}
	}
}

// tick advances the fake clock by 24h, converges the scope to a fixpoint,
// then — mirroring DunningWorker.Work's query — runs a dunning attempt if the
// subscription is now past_due and due, and runs the credit-expiry sweep
// (cheap no-op most days). Returns the reloaded row for assertions.
func (r *simRig) tick(t *testing.T, ctx context.Context, scope converge.Scope, subID uuid.UUID) *models.Subscription {
	t.Helper()
	r.clock.Advance(r.tickStep)
	convergeToFixpoint(t, ctx, r.engine, scope)

	sub, err := subscriptions.NewSubscriptionRepo(r.dbi).GetByID(ctx, subID)
	require.NoError(t, err)

	now := r.clock.Now().UTC()
	if sub.Status == models.StatusPastDue && sub.NextRetryAt != nil && !sub.NextRetryAt.UTC().After(now) {
		_, processErr := r.dunning.processSubscription(ctx, sub, r.lifecycle(), r.priceSvc, false)
		require.NoError(t, processErr)
		sub, err = subscriptions.NewSubscriptionRepo(r.dbi).GetByID(ctx, subID)
		require.NoError(t, err)
	}

	require.NoError(t, r.creditExpiry.Work(ctx, nil))
	return sub
}

// waitFor ticks up to maxTicks times, asserting entitlement standing access
// on EVERY tick (the #664/#691 doctrine under test: access must never drop
// while OpenRails is merely retrying), until cond reports the awaited
// outcome. Fails the test if the budget is exhausted first.
func (r *simRig) waitFor(t *testing.T, ctx context.Context, scope converge.Scope, sub simSub, maxTicks int, wantEntitled bool, cond func(*models.Subscription) bool) *models.Subscription {
	t.Helper()
	for i := 0; i < maxTicks; i++ {
		s := r.tick(t, ctx, scope, sub.subID)
		entitled, err := r.entitlementSvc.IsCustomerEntitled(ctx, sub.customerID, sub.entName, r.clock.Now().UTC())
		require.NoError(t, err)
		require.Equal(t, wantEntitled, entitled, "entitlement standing-access state at tick %d (day %d)", i+1, i+1)
		if cond(s) {
			return s
		}
	}
	t.Fatalf("condition not met within %d ticks", maxTicks)
	return nil
}

// creditGrantCount counts the grants-ledger rows for one subscription's
// per-renewal credit deposits. GrantSubscriptionCredits (internal/modules/
// money/subscription_credits.go) keys each deposit's grants.source_id to a
// composite natural key "openrails:sub_credit_grant:<cadence>:<sub_id>:
// <label>:<period_end>" (#491: natural-key string, not the bare
// subscription id) — a LIKE match on the subscription id substring is the
// correct "exactly once per renewal" probe, and is idempotency-key-exact
// (depositTx's own idempotency check is keyed on this same source_id).
func creditGrantCount(t *testing.T, ctx context.Context, dbi *db.DB, subID uuid.UUID) int {
	t.Helper()
	var n int
	require.NoError(t, dbtest.SharedMerchantPool(t, dbtest.TestMerchantID.UUID()).QueryRow(ctx,
		"SELECT count(*) FROM billing.grants WHERE source_id LIKE '%' || $1 || '%' AND kind = 'credit' AND event = 'grant'",
		subID.String()).Scan(&n))
	return n
}

func paymentCount(t *testing.T, ctx context.Context, dbi *db.DB, subID uuid.UUID, status string) int {
	t.Helper()
	var n int
	require.NoError(t, dbtest.SharedMerchantPool(t, dbtest.TestMerchantID.UUID()).QueryRow(ctx,
		"SELECT count(*) FROM billing.payments WHERE subscription_id = $1 AND status = $2",
		subID, status).Scan(&n))
	return n
}

// --- scenario 1: happy renewals -------------------------------------------

// testHappyRenewals verifies one charge and payment per renewal, standing access,
// and no bundled balance deposits.
func testHappyRenewals(t *testing.T, ctx context.Context, dbi *db.DB) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	stub := newNMIStub(t)
	sim := seedSimSubscription(t, ctx, dbi, start, true)
	stub.sub = sim
	stub.prepareRenewal(start.Add(simCycleHours * time.Hour))
	rig := newSimRig(t, dbi, start, stub)
	scope := converge.Scope{Merchant: dbtest.TestMerchantID, Customer: &sim.customerID}

	// t=0: DERIVE materializes the standing entitlement window before any
	// period has lapsed (ListUngrantedSubscriptions fires on any grantable
	// subscription, not just lapsed ones).
	convergeToFixpoint(t, ctx, rig.engine, scope)
	active, err := rig.entitlementSvc.IsCustomerEntitled(ctx, sim.customerID, sim.entName, rig.clock.Now().UTC())
	require.NoError(t, err)
	require.True(t, active, "entitlement should be granted at signup")

	periodEnd := start.Add(simCycleHours * time.Hour)
	for cycle := 1; cycle <= 3; cycle++ {
		stub.prepareRenewal(periodEnd)
		stub.enqueue(approvedResponse())
		wantEnd := periodEnd.Add(simCycleHours * time.Hour)

		sub := rig.waitFor(t, ctx, scope, sim, 40, true, func(s *models.Subscription) bool {
			return s.Status == models.StatusActive && s.CurrentPeriodEndsAt != nil && !s.CurrentPeriodEndsAt.Before(wantEnd)
		})
		require.Equal(t, string(models.StatusActive), string(sub.Status), "cycle %d renewed to active", cycle)

		require.Equal(t, cycle, stub.count(), "cycle %d: exactly one dunning attempt", cycle)
		require.Equal(t, cycle+1, paymentCount(t, ctx, dbi, sim.subID, payments.PaymentStatusCompletedValue), "cycle %d: signup + one completed renewal payment per cycle", cycle)
		require.Equal(t, 0, creditGrantCount(t, ctx, dbi, sim.subID), "cycle %d: no bundled balance grant", cycle)

		periodEnd = wantEnd
	}
	stub.assertDrained(t)

	// Renewals do not materialize the deferred bundled balance feature.
	bal, err := rig.moneySvc.GetBalanceForCustomer(ctx, identity.CustomerID(sim.customerID), "USD")
	require.NoError(t, err)
	require.Equal(t, int64(0), bal.Balance, "subscription renewals do not create bundled balances")
}

// --- scenario 2: dunning recovery -----------------------------------------

// testDunningRecovery: the first renewal attempt is declined (soft), the
// scheduled retry succeeds. Asserts the subscription NEVER loses its
// entitlement while past_due (standing access, #691), records exactly one
// failed payment + one completed renewal payment, grants credit exactly once
// (on the successful attempt, not the declined one), and resumes the normal
// renewal cadence afterward.
func testDunningRecovery(t *testing.T, ctx context.Context, dbi *db.DB) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	stub := newNMIStub(t)
	sim := seedSimSubscription(t, ctx, dbi, start, true)
	stub.sub = sim
	stub.prepareRenewal(start.Add(simCycleHours * time.Hour))
	rig := newSimRig(t, dbi, start, stub)
	scope := converge.Scope{Merchant: dbtest.TestMerchantID, Customer: &sim.customerID}
	convergeToFixpoint(t, ctx, rig.engine, scope)

	firstPeriodEnd := start.Add(simCycleHours * time.Hour)
	stub.enqueue(nmiSoftDeclined, approvedResponse())

	sub := rig.waitFor(t, ctx, scope, sim, 45, true, func(s *models.Subscription) bool {
		return s.Status == models.StatusActive && s.CurrentPeriodEndsAt != nil && s.CurrentPeriodEndsAt.After(firstPeriodEnd)
	})
	require.Equal(t, string(models.StatusActive), string(sub.Status), "recovered to active")
	require.Equal(t, 2, stub.count(), "one declined attempt + one successful retry")
	require.Equal(t, 1, paymentCount(t, ctx, dbi, sim.subID, payments.PaymentStatusFailedValue), "the declined attempt is durably recorded")
	require.Equal(t, 2, paymentCount(t, ctx, dbi, sim.subID, payments.PaymentStatusCompletedValue), "signup + the successful renewal (not the decline)")
	require.Equal(t, 0, creditGrantCount(t, ctx, dbi, sim.subID), "renewal success does not grant a bundled balance")

	// Prove the cadence is fully restored: the next cycle renews normally too.
	secondPeriodEnd := *sub.CurrentPeriodEndsAt
	stub.prepareRenewal(secondPeriodEnd)
	stub.enqueue(approvedResponse())
	sub = rig.waitFor(t, ctx, scope, sim, 40, true, func(s *models.Subscription) bool {
		return s.Status == models.StatusActive && s.CurrentPeriodEndsAt != nil && s.CurrentPeriodEndsAt.After(secondPeriodEnd)
	})
	require.Equal(t, 3, stub.count())
	require.Equal(t, 0, creditGrantCount(t, ctx, dbi, sim.subID))
	stub.assertDrained(t)
}

// --- scenario 3a: exhausted dunning ----------------------------------------

// testExhaustedDunning: every scheduled retry is declined (soft — never a
// hard/non-retryable decline) until the monthly schedule's 5-failure budget
// (collection.MaxFailures) is exhausted. Only then — certainty, not
// a guess — does the subscription terminally cancel and lose its
// entitlement. No credit is ever granted for the un-renewed cycle.
func testExhaustedDunning(t *testing.T, ctx context.Context, dbi *db.DB) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	stub := newNMIStub(t)
	sim := seedSimSubscription(t, ctx, dbi, start, true)
	stub.sub = sim
	stub.prepareRenewal(start.Add(simCycleHours * time.Hour))
	// A 6h step (not the default 24h): this scenario walks the retry schedule
	// all the way to its last offset (13d), where the manufactured detection
	// lag at 24h ticks would land the 5th attempt exactly ON the 14d dunning
	// window boundary (see simRig.tickStep doc). 6h keeps the whole schedule
	// exactly tick-aligned (every offset is a multiple of 24h) while leaving
	// 18h of margin before the window closes.
	rig := newSimRigWithStep(t, dbi, start, stub, 6*time.Hour)
	scope := converge.Scope{Merchant: dbtest.TestMerchantID, Customer: &sim.customerID}
	convergeToFixpoint(t, ctx, rig.engine, scope)

	// 5 soft declines: collection.MaxFailures(720h) == 5 (offsets
	// +2/+5/+9/+13d plus the initial failure). Never approved — exhaustion is
	// the point.
	stub.enqueue(nmiSoftDeclined, nmiSoftDeclined, nmiSoftDeclined, nmiSoftDeclined, nmiSoftDeclined)

	// Entitlement is standing-access-active through every retry (past_due is
	// non-terminal); it only drops on the tick that actually cancels, so this
	// loop checks entitlement OUTSIDE the strict per-tick assertion and
	// verifies the exact before/after instead.
	var sub *models.Subscription
	for i := 0; i < 200; i++ {
		sub = rig.tick(t, ctx, scope, sim.subID)
		if sub.Status == models.StatusCancelled {
			break
		}
		entitled, err := rig.entitlementSvc.IsCustomerEntitled(ctx, sim.customerID, sim.entName, rig.clock.Now().UTC())
		require.NoError(t, err)
		require.True(t, entitled, "standing access must survive every non-terminal dunning retry (tick %d)", i+1)
	}
	require.Equal(t, string(models.StatusCancelled), string(sub.Status), "5th failure exhausts the schedule and terminates")
	require.NotNil(t, sub.CancelType)
	require.Equal(t, string(models.CancelTypeExpired), string(*sub.CancelType))

	entitled, err := rig.entitlementSvc.IsCustomerEntitled(ctx, sim.customerID, sim.entName, rig.clock.Now().UTC())
	require.NoError(t, err)
	require.False(t, entitled, "entitlement revoked on the certainty-gated terminal cancel")

	require.Equal(t, 5, stub.count(), "exactly the 5-failure budget, never more")
	require.Equal(t, 5, paymentCount(t, ctx, dbi, sim.subID, payments.PaymentStatusFailedValue))
	require.Equal(t, 1, paymentCount(t, ctx, dbi, sim.subID, payments.PaymentStatusCompletedValue), "only the original signup payment; the cycle never renewed")
	require.Equal(t, 0, creditGrantCount(t, ctx, dbi, sim.subID), "no credit for a cycle that was never paid")
	stub.assertDrained(t)

	// Stays cancelled: no further attempts, no entitlement resurrection.
	for i := 0; i < 5; i++ {
		sub = rig.tick(t, ctx, scope, sim.subID)
		require.Equal(t, string(models.StatusCancelled), string(sub.Status))
	}
	require.Equal(t, 5, stub.count(), "a cancelled subscription is never dunned again")
	entitled, err = rig.entitlementSvc.IsCustomerEntitled(ctx, sim.customerID, sim.entName, rig.clock.Now().UTC())
	require.NoError(t, err)
	require.False(t, entitled)
}

// --- scenario 3b: no-evidence parking --------------------------------------

// testNoEvidenceParking: a subscription lapses with NO first-party ownership
// evidence (no completed payment ever recorded — simulating a row whose
// billing history OpenRails cannot vouch for). The #664 LIFE law refuses to
// guess: it never enters dunning and never charges; after PeriodGrace (48h)
// it parks as `unknown`, where it durably stays (no plane can produce
// certainty without a provider snapshot). Entitlement/standing access is
// untouched throughout — `unknown` is a non-terminal status.
func testNoEvidenceParking(t *testing.T, ctx context.Context, dbi *db.DB) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	stub := newNMIStub(t) // never scripted: zero requests expected, ever.
	sim := seedSimSubscription(t, ctx, dbi, start, false /* no ownership evidence */)
	stub.sub = sim
	rig := newSimRig(t, dbi, start, stub)
	scope := converge.Scope{Merchant: dbtest.TestMerchantID, Customer: &sim.customerID}
	convergeToFixpoint(t, ctx, rig.engine, scope)

	// Parks at periodEnd+3d: the LIFE pass's evidence-less law waits out
	// PeriodGrace (48h, strict >) before parking (decideFromFirstParty's
	// "no_ownership_evidence" branch) — periodEnd+30d (this sub's cycle) is
	// day30; day31/day32 stay "within_grace_slack", day33 (72h > 48h) parks.
	sub := rig.waitFor(t, ctx, scope, sim, 40, true, func(s *models.Subscription) bool {
		return s.Status == models.StatusUnknown
	})
	require.Equal(t, string(models.StatusUnknown), string(sub.Status), "no-evidence lapse parks, it does not dun or cancel")
	require.Nil(t, sub.CancelledAt)
	require.Zero(t, stub.count(), "never charged: OpenRails had no evidence this rebill was ever its own to attempt")
	require.Zero(t, paymentCount(t, ctx, dbi, sim.subID, payments.PaymentStatusFailedValue))
	require.Zero(t, paymentCount(t, ctx, dbi, sim.subID, payments.PaymentStatusCompletedValue))
	require.Zero(t, creditGrantCount(t, ctx, dbi, sim.subID))

	// Stays unknown: no sweep plane can resolve it without a provider snapshot.
	for i := 0; i < 5; i++ {
		sub = rig.tick(t, ctx, scope, sim.subID)
		require.Equal(t, string(models.StatusUnknown), string(sub.Status))
		entitled, err := rig.entitlementSvc.IsCustomerEntitled(ctx, sim.customerID, sim.entName, rig.clock.Now().UTC())
		require.NoError(t, err)
		require.True(t, entitled, "unknown is non-terminal: standing access is untouched")
	}
	require.Zero(t, stub.count())
}
