//go:build integration

package checkout

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/integrations/stripeapi"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/modules/webhooks"
	"github.com/open-rails/openrails/internal/railresolve"
)

// fakeStripeTier models the Stripe Billing surface a tier change touches with
// Stripe's own semantics: the subscription object is returned in full by the
// update that mutated it (latest_invoice expanded on request), a schedule
// created from a subscription attaches to it and copies its current phase,
// and idempotency keys replay the stored answer of a completed request (2xx
// or 4xx; a 5xx stores nothing). A declined payment follows
// payment_behavior: error_if_incomplete answers 402 and changes nothing; the
// default (allow_incomplete) applies the update, leaves the invoice open and
// the subscription past_due, and answers 200.
type fakeStripeTier struct {
	mu        sync.Mutex
	subs      map[string]*fakeStripeSub
	schedules map[string]map[string]any
	responses map[string]fakeStripeStored
	// mode scripts the next mutating request: "" (apply and answer),
	// "lostAfterLanding" (apply, answer 502 once), "lostBeforeLanding"
	// (answer 502 once, nothing applied), "decline" (402 stored under the key),
	// "inUse" (409 idempotency_key_in_use once).
	mode     string
	requests []fakeStripeRequest
	created  int
	// hold blocks mutating requests until released (concurrency scripts).
	hold chan struct{}
	// arrived is closed when a held request reached the fake.
	arrived chan struct{}
	// readHold blocks the next read of readHoldPath (one shot) until
	// released; readArrived is closed when it reached the fake.
	readHold, readArrived chan struct{}
	readHoldPath          string
	// executionLag is how much later than now Stripe executes a write (its
	// billing_cycle_anchor=now is its own clock, not the enqueue time).
	executionLag time.Duration
}

type fakeStripeSub struct {
	id, itemID, priceID string
	metadata            map[string]string
	scheduleID          string
	periodStart, period int64
	// status is "" (active) or "past_due"; invoiceOpen marks the latest
	// invoice unpaid.
	status      string
	invoiceOpen bool
}

type fakeStripeStored struct {
	status int
	body   []byte
}

type fakeStripeRequest struct {
	method, path, key string
	form              url.Values
}

func newFakeStripeTier(t *testing.T) *fakeStripeTier {
	t.Helper()
	f := &fakeStripeTier{subs: map[string]*fakeStripeSub{}, schedules: map[string]map[string]any{}, responses: map[string]fakeStripeStored{}}
	srv := httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(srv.Close)
	stripeapi.SetBaseTransport(stripeapi.HostRewriteTransport(srv.URL))
	t.Cleanup(func() { stripeapi.SetBaseTransport(nil) })
	return f
}

func (f *fakeStripeTier) declare(id, itemID, priceID string, periodStart, periodEnd int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.subs[id] = &fakeStripeSub{id: id, itemID: itemID, priceID: priceID, metadata: map[string]string{}, periodStart: periodStart, period: periodEnd}
}

func (f *fakeStripeTier) setMode(mode string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mode = mode
}

func (f *fakeStripeTier) posts(pathPrefix string) []fakeStripeRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []fakeStripeRequest
	for _, r := range f.requests {
		if r.method == http.MethodPost && strings.HasPrefix(r.path, pathPrefix) {
			out = append(out, r)
		}
	}
	return out
}

func (f *fakeStripeTier) sub(id string) fakeStripeSub {
	f.mu.Lock()
	defer f.mu.Unlock()
	return *f.subs[id]
}

func (f *fakeStripeTier) subJSON(s *fakeStripeSub, expandInvoice bool) map[string]any {
	var schedule any
	if s.scheduleID != "" {
		schedule = s.scheduleID
	}
	status := s.status
	if status == "" {
		status = "active"
	}
	var invoice any = "in_" + s.id
	if expandInvoice {
		invoiceStatus, paid, amountPaid, amountDue := "paid", true, int64(1000), int64(0)
		if s.invoiceOpen {
			invoiceStatus, paid, amountPaid, amountDue = "open", false, 0, 1000
		}
		invoice = map[string]any{"id": "in_" + s.id, "object": "invoice", "status": invoiceStatus, "paid": paid, "amount_paid": amountPaid, "amount_due": amountDue,
			"currency": "usd", "created": s.periodStart, "billing_reason": "subscription_update"}
	}
	return map[string]any{"id": s.id, "object": "subscription", "status": status, "metadata": s.metadata, "schedule": schedule, "latest_invoice": invoice,
		"items": map[string]any{"data": []any{map[string]any{"id": s.itemID, "price": map[string]any{"id": s.priceID}, "current_period_start": s.periodStart, "current_period_end": s.period}}}}
}

func writeStripeJSON(w http.ResponseWriter, status int, v any) []byte {
	body, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
	return body
}

// holdNextRead pauses the next GET of path until the returned release is
// called; the arrived channel closes when the request reaches the fake.
func (f *fakeStripeTier) holdNextRead(path string) (arrived <-chan struct{}, release func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.readHold, f.readArrived, f.readHoldPath = make(chan struct{}), make(chan struct{}), path
	hold := f.readHold
	return f.readArrived, func() { close(hold) }
}

func (f *fakeStripeTier) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		f.mu.Lock()
		hold, arrived := f.readHold, f.readArrived
		if hold != nil && r.URL.Path == f.readHoldPath {
			f.readHold, f.readArrived, f.readHoldPath = nil, nil, ""
		} else {
			hold = nil
		}
		f.mu.Unlock()
		if hold != nil {
			close(arrived)
			<-hold
		}
	}
	if r.Method == http.MethodPost {
		f.mu.Lock()
		hold, arrived := f.hold, f.arrived
		f.mu.Unlock()
		if hold != nil {
			if arrived != nil {
				close(arrived)
			}
			<-hold
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	_ = r.ParseForm()
	key := r.Header.Get(stripeapi.IdempotencyKeyHeader)
	f.requests = append(f.requests, fakeStripeRequest{method: r.Method, path: r.URL.Path, key: key, form: r.PostForm})
	if r.Method == http.MethodGet {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v1/subscriptions/"):
			if s, ok := f.subs[strings.TrimPrefix(r.URL.Path, "/v1/subscriptions/")]; ok {
				writeStripeJSON(w, http.StatusOK, f.subJSON(s, r.URL.Query().Get("expand[]") == "latest_invoice"))
				return
			}
		case strings.HasPrefix(r.URL.Path, "/v1/subscription_schedules/"):
			if sch, ok := f.schedules[strings.TrimPrefix(r.URL.Path, "/v1/subscription_schedules/")]; ok {
				writeStripeJSON(w, http.StatusOK, sch)
				return
			}
		}
		writeStripeJSON(w, http.StatusNotFound, map[string]any{"error": map[string]any{"type": "invalid_request_error", "message": "No such object", "code": "resource_missing"}})
		return
	}
	if stored, ok := f.responses[key]; ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(stored.status)
		_, _ = w.Write(stored.body)
		return
	}
	mode := f.mode
	f.mode = ""
	switch mode {
	case "lostBeforeLanding":
		writeStripeJSON(w, http.StatusBadGateway, map[string]any{"error": map[string]any{"message": "upstream failure"}})
		return
	case "inUse":
		writeStripeJSON(w, http.StatusConflict, map[string]any{"error": map[string]any{"type": "idempotency_error", "code": "idempotency_key_in_use", "message": "There is currently another in-progress request using this Idempotency Key"}})
		return
	case "decline":
		// Only an explicit error_if_incomplete turns a declined payment into
		// a refused update; the default applies it with an open invoice.
		if r.PostForm.Get("payment_behavior") == "error_if_incomplete" || !strings.HasPrefix(r.URL.Path, "/v1/subscriptions/") {
			body := writeStripeJSON(w, http.StatusPaymentRequired, map[string]any{"error": map[string]any{"type": "card_error", "code": "card_declined", "decline_code": "insufficient_funds", "message": "Your card has insufficient funds."}})
			f.responses[key] = fakeStripeStored{status: http.StatusPaymentRequired, body: body}
			return
		}
	}
	var answer map[string]any
	switch {
	case strings.HasPrefix(r.URL.Path, "/v1/subscriptions/"):
		s, ok := f.subs[strings.TrimPrefix(r.URL.Path, "/v1/subscriptions/")]
		if !ok {
			writeStripeJSON(w, http.StatusNotFound, map[string]any{"error": map[string]any{"message": "No such subscription", "code": "resource_missing"}})
			return
		}
		if s.scheduleID != "" {
			writeStripeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": "This subscription is managed by a subscription schedule"}})
			return
		}
		s.priceID = r.PostForm.Get("items[0][price]")
		for k, v := range r.PostForm {
			if strings.HasPrefix(k, "metadata[") {
				s.metadata[strings.TrimSuffix(strings.TrimPrefix(k, "metadata["), "]")] = v[0]
			}
		}
		if r.PostForm.Get("billing_cycle_anchor") == "now" {
			s.periodStart = time.Now().Add(f.executionLag).Unix()
			s.period = s.periodStart + 30*24*3600
		}
		if mode == "decline" {
			s.status, s.invoiceOpen = "past_due", true
		}
		answer = f.subJSON(s, false)
	case r.URL.Path == "/v1/subscription_schedules":
		s, ok := f.subs[r.PostForm.Get("from_subscription")]
		if !ok || s.scheduleID != "" {
			writeStripeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": "The subscription already has a schedule or does not exist"}})
			return
		}
		f.created++
		id := fmt.Sprintf("sub_sched_%d", f.created)
		s.scheduleID = id
		f.schedules[id] = map[string]any{"id": id, "object": "subscription_schedule", "subscription": s.id, "status": "active", "end_behavior": "release", "metadata": map[string]string{},
			"phases": []any{map[string]any{"start_date": s.periodStart, "end_date": s.period, "items": []any{map[string]any{"price": s.priceID, "quantity": 1}}}}}
		answer = f.schedules[id]
	case strings.HasPrefix(r.URL.Path, "/v1/subscription_schedules/"):
		sch, ok := f.schedules[strings.TrimPrefix(r.URL.Path, "/v1/subscription_schedules/")]
		if !ok {
			writeStripeJSON(w, http.StatusNotFound, map[string]any{"error": map[string]any{"message": "No such schedule", "code": "resource_missing"}})
			return
		}
		phase := func(i int) map[string]any {
			p := "phases[" + strconv.Itoa(i) + "]"
			start, _ := strconv.ParseInt(r.PostForm.Get(p+"[start_date]"), 10, 64)
			end, _ := strconv.ParseInt(r.PostForm.Get(p+"[end_date]"), 10, 64)
			return map[string]any{"start_date": start, "end_date": end, "items": []any{map[string]any{"price": r.PostForm.Get(p + "[items][0][price]"), "quantity": 1}}}
		}
		sch["phases"] = []any{phase(0), phase(1)}
		sch["end_behavior"] = r.PostForm.Get("end_behavior")
		meta := map[string]string{}
		for k, v := range r.PostForm {
			if strings.HasPrefix(k, "metadata[") {
				meta[strings.TrimSuffix(strings.TrimPrefix(k, "metadata["), "]")] = v[0]
			}
		}
		sch["metadata"] = meta
		answer = sch
	default:
		writeStripeJSON(w, http.StatusNotImplemented, map[string]any{"error": map[string]any{"message": "unexpected " + r.Method + " " + r.URL.Path}})
		return
	}
	body, _ := json.Marshal(answer)
	// Stripe stores the completed answer under the key even when the caller
	// never receives it.
	f.responses[key] = fakeStripeStored{status: http.StatusOK, body: body}
	if mode == "lostAfterLanding" {
		writeStripeJSON(w, http.StatusBadGateway, map[string]any{"error": map[string]any{"message": "upstream failure after commit"}})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

type stripeTierFixture struct {
	t        *testing.T
	db       *db.DB
	svc      *CheckoutService
	stripe   *fakeStripeTier
	ctx      context.Context
	clock    *clockwork.FakeClock
	user     *UserIdentity
	sub      *models.Subscription
	basic    *models.Price
	pro      *models.Price
	basicRef string
	proRef   string
}

func newStripeTierFixture(t *testing.T) *stripeTierFixture {
	t.Helper()
	dbi := dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID())
	pool := dbi.Pool()
	dbtest.EnsureTestMerchant(context.Background(), t, pool)
	ctx := checkoutFixtureCtx(t, pool, "stripe")
	pspID := dbtest.EnsureTestPSP(ctx, t, pool, dbtest.TestMerchantID.UUID(), "stripe")
	now := time.Now().UTC().Truncate(time.Second)
	clock := clockwork.NewFakeClockAt(now)
	sfx := uuid.NewString()[:8]
	userID := uuid.New().String()
	customerID := dbtest.EnsureCustomerIDPgx(ctx, t, pool, userID)
	group := "tier-" + sfx
	hours := 720
	seed := func(key, name string, rank int, amount int64, ref string) (*models.Product, *models.Price) {
		product := &models.Product{ID: uuid.New(), Key: key + "-" + sfx, DisplayName: name, TierGroup: &group, TierRank: rank, CreatedAt: now, UpdatedAt: now}
		price := &models.Price{ID: uuid.New(), ProductID: product.ID, Amount: amount, Currency: "USD", AutoRenew: true, AccessDurationHours: &hours, CreatedAt: now, UpdatedAt: now}
		insertProductAndPrice(ctx, t, pool, product, price)
		_, err := pool.Exec(ctx, `UPDATE billing.products SET tier_group=$2, tier_rank=$3 WHERE id=$1`, product.ID, group, rank)
		require.NoError(t, err)
		_, err = pool.Exec(ctx, `INSERT INTO billing.price_psp_bindings(merchant_id, price_id, psp_id, price_ref) VALUES ($1,$2,$3,$4)`, dbtest.TestMerchantID.UUID(), price.ID, pspID, ref)
		require.NoError(t, err)
		return product, price
	}
	basicRef, proRef := "price_basic_"+sfx, "price_pro_"+sfx
	_, basic := seed("basic", "Basic", 1, 10_000_000, basicRef)
	_, pro := seed("pro", "Pro", 2, 30_000_000, proRef)

	stripe := newFakeStripeTier(t)
	railSub := "sub_" + sfx
	periodStart, periodEnd := now.Add(-5*24*time.Hour), now.Add(25*24*time.Hour)
	stripe.declare(railSub, "si_"+sfx, basicRef, periodStart.Unix(), periodEnd.Unix())

	pm := &models.PaymentMethod{ID: uuid.New(), CustomerID: customerID, Rail: models.RailStripe, PspID: pspID, RailCustomerRef: "cus_" + sfx, RailMethodRef: "pm_" + sfx, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, paymentmethods.NewPaymentMethodRepo(dbi).Create(ctx, pm))
	subID := uuid.New()
	_, err := pool.Exec(ctx, `INSERT INTO billing.subscriptions
	        (id, price_id, product_id, status, rail, psp_id, rail_subscription_id,
	         current_period_starts_at, current_period_ends_at, started_at,
	         payment_method_id, customer_id, merchant_id)
	      VALUES ($1, $2, $3, 'active', 'stripe', $4, $5, $6, $7, $6, $8, $9, $10)`,
		subID, basic.ID, basic.ProductID, pspID, railSub, periodStart, periodEnd, pm.ID, customerID, dbtest.TestMerchantID.UUID())
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM billing.rail_intents WHERE subscription_id = $1", subID)
		_, _ = pool.Exec(ctx, "DELETE FROM billing.subscriptions WHERE customer_id = $1", customerID)
		_, _ = pool.Exec(ctx, "DELETE FROM billing.payment_methods WHERE id = $1", pm.ID)
		_, _ = pool.Exec(ctx, "DELETE FROM billing.price_psp_bindings WHERE price_id = ANY($1)", []uuid.UUID{basic.ID, pro.ID})
		_, _ = pool.Exec(ctx, "DELETE FROM billing.prices WHERE id = ANY($1)", []uuid.UUID{basic.ID, pro.ID})
		_, _ = pool.Exec(ctx, "DELETE FROM billing.products WHERE id = ANY($1)", []uuid.UUID{basic.ProductID, pro.ProductID})
	})

	priceSvc := catalog.NewPriceService(dbi)
	productSvc := catalog.NewProductService(dbi)
	paymentSvc := payments.NewPaymentService(dbi, clock)
	entSvc := entitlements.NewEntitlementService(dbi, clock)
	subSvc := subscriptions.NewSubscriptionService(dbi, priceSvc, productSvc, nil, clock)
	svc := NewCheckoutService(subSvc, productSvc, priceSvc, paymentSvc, entSvc, paymentmethods.NewPaymentMethodService(dbi), nil, nil, nil, nil, nil, clock)
	svc.Config = &config.Config{ProviderWriteMode: config.ProviderWriteModeFull}
	svc.Rails = railresolve.FixedSet{"stripe": {Rail: models.RailStripe, Stripe: &config.StripeRailConfig{SecretKey: "sk_test_" + sfx}}}
	fx := &stripeTierFixture{t: t, db: dbi, svc: svc, stripe: stripe, ctx: ctx, clock: clock, user: &UserIdentity{ID: userID}, basic: basic, pro: pro, basicRef: basicRef, proRef: proRef}
	fx.restart()
	fx.sub, err = subSvc.GetByID(ctx, subID)
	require.NoError(t, err)
	return fx
}

// seedCustomer gives another customer of the same merchant an active basic
// subscription that the fake Stripe also knows.
func (fx *stripeTierFixture) seedCustomer(t *testing.T) (*models.Subscription, *UserIdentity) {
	t.Helper()
	pool := fx.db.Pool()
	userID := uuid.NewString()
	customerID := dbtest.EnsureCustomerIDPgx(fx.ctx, t, pool, userID)
	sfx := uuid.NewString()[:8]
	railSub := "sub_" + sfx
	start, end := *fx.sub.CurrentPeriodStartsAt, *fx.sub.CurrentPeriodEndsAt
	fx.stripe.declare(railSub, "si_"+sfx, fx.basicRef, start.Unix(), end.Unix())
	id := uuid.New()
	_, err := pool.Exec(fx.ctx, `INSERT INTO billing.subscriptions
	        (id, price_id, product_id, status, rail, psp_id, rail_subscription_id,
	         current_period_starts_at, current_period_ends_at, started_at, customer_id, merchant_id)
	      VALUES ($1, $2, $3, 'active', 'stripe', $4, $5, $6, $7, $6, $8, $9)`,
		id, fx.basic.ID, fx.basic.ProductID, fx.sub.PspID, railSub, start, end, customerID, dbtest.TestMerchantID.UUID())
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = pool.Exec(fx.ctx, "DELETE FROM billing.rail_intents WHERE subscription_id = $1", id)
		_, _ = pool.Exec(fx.ctx, "DELETE FROM billing.subscriptions WHERE id = $1", id)
	})
	sub, err := fx.svc.SubscriptionService.GetByID(fx.ctx, id)
	require.NoError(t, err)
	return sub, &UserIdentity{ID: userID}
}

// restart rebuilds the runner and handler: every recovery decision must come
// from the database.
func (fx *stripeTierFixture) restart() *intents.Runner {
	runner := &intents.Runner{Store: intents.NewStore(fx.db), Registry: intents.NewRegistry(NewStripeTierChangeIntentHandler(fx.svc)), Config: fullModeConfig(), Clock: fx.clock}
	fx.svc.Intents = runner
	return runner
}

func (fx *stripeTierFixture) change(key string, price *models.Price) (*TierChangeResponse, error) {
	fx.t.Helper()
	return fx.svc.TierChange(fx.ctx, &TierChangeRequest{PriceID: openrails.PriceID(price.ID).String(), SubscriptionID: fx.sub.ID, IdempotencyKey: key}, fx.user)
}

func (fx *stripeTierFixture) operation(key string) gen.OpenrailsRailIntent {
	fx.t.Helper()
	in, err := intents.NewStore(fx.db).GetByIdempotencyKey(fx.ctx, tierChangeIdempotencyKey(key))
	require.NoError(fx.t, err)
	return in
}

func (fx *stripeTierFixture) verifyOnce(t *testing.T, key string) gen.OpenrailsRailIntent {
	t.Helper()
	in := fx.operation(key)
	_, err := fx.db.Qx(fx.ctx).Exec(fx.ctx, `UPDATE billing.rail_intents SET next_attempt_at='epoch', claimed_until=NULL WHERE id=$1`, in.ID)
	require.NoError(t, err)
	_, err = fx.restart().RunVerifyOnce(fx.ctx)
	require.NoError(t, err)
	return fx.operation(key)
}

func (fx *stripeTierFixture) local(t *testing.T) *models.Subscription {
	t.Helper()
	sub, err := fx.svc.SubscriptionService.GetByID(fx.ctx, fx.sub.ID)
	require.NoError(t, err)
	return sub
}

func (fx *stripeTierFixture) operations(t *testing.T) int {
	t.Helper()
	var count int
	require.NoError(t, fx.db.Qx(fx.ctx).QueryRow(fx.ctx, `SELECT count(*) FROM billing.rail_intents WHERE subscription_id=$1`, fx.sub.ID).Scan(&count))
	return count
}

// The same upgrade contract must survive an immediate receipt, a lost response,
// and a webhook that applies the provider state before the restarted verifier.
func TestStripeTierChangeUpgradeReceiptAndRecovery(t *testing.T) {
	for _, mode := range []string{"immediate", "lost_response", "webhook_first"} {
		t.Run(mode, func(t *testing.T) {
			fx := newStripeTierFixture(t)
			if mode != "immediate" {
				fx.stripe.setMode("lostAfterLanding")
			}
			if mode == "webhook_first" {
				fx.stripe.mu.Lock()
				fx.stripe.executionLag = 90 * time.Second
				fx.stripe.mu.Unlock()
			}
			key := "upgrade-" + uuid.NewString()[:8]
			first, err := fx.change(key, fx.pro)
			require.NoError(t, err)
			require.NotEmpty(t, first.OperationID)
			op := fx.operation(key)
			require.Equal(t, op.ID.String(), first.OperationID)
			if mode != "immediate" {
				require.Equal(t, "processing", first.Status)
				require.Equal(t, intents.StatusUnknownNeedsVerify, op.Status)
				require.Equal(t, fx.basic.ID, fx.local(t).PriceID, "nothing commits without a receipt")
				again, err := fx.change(key, fx.pro)
				require.NoError(t, err)
				require.Equal(t, first, again, "the same key answers the live operation")
				_, err = fx.change("new-"+uuid.NewString()[:8], fx.pro)
				var inFlight *TierChangeInFlightError
				require.ErrorAs(t, err, &inFlight)
				require.Equal(t, op.ID, inFlight.OperationID)
				require.ErrorIs(t, err, ErrTierChangePending)
				require.Len(t, fx.stripe.posts("/v1/subscriptions/"), 1, "unknown outcomes are never resent")
				require.Equal(t, 1, fx.operations(t))
				if mode == "webhook_first" {
					entSvc := entitlements.NewEntitlementService(fx.db, fx.clock)
					paymentSvc := payments.NewPaymentService(fx.db, fx.clock)
					notifications := subscriptions.NewNotificationService(fx.db, nil)
					converger := &webhooks.StripeConvergeService{
						DB: fx.db, Clock: fx.clock, Prober: &subscriptions.HTTPStripeLivenessProber{SecretKey: "sk_test_converge"},
						PriceService: fx.svc.PriceService, ProductService: fx.svc.ProductService, SubscriptionService: fx.svc.SubscriptionService,
						SubscriptionLifecycleService: subscriptions.NewSubscriptionLifecycleService(fx.db, fx.svc.ProductService, fx.svc.PriceService, entSvc, notifications, paymentSvc, fx.clock),
						PaymentService:               paymentSvc, NotificationService: notifications,
					}
					_, err = converger.Converge(fx.ctx, fx.sub.RailSubscriptionID)
					require.NoError(t, err)
					require.Equal(t, fx.pro.ID, fx.local(t).PriceID, "the webhook applied the target price first")
				}
				require.Equal(t, intents.StatusSucceeded, fx.verifyOnce(t, key).Status, "restart recovers from the exact provider receipt")
			} else {
				require.Equal(t, "succeeded", first.Status)
				require.Equal(t, intents.StatusSucceeded, op.Status)
			}
			done, err := fx.change(key, fx.pro)
			require.NoError(t, err)
			require.Equal(t, "succeeded", done.Status)
			if mode == "immediate" {
				require.Equal(t, first, done)
			}
			require.Equal(t, "upgrade", done.Action)
			require.Equal(t, first.OperationID, done.OperationID)
			require.Equal(t, first.AmountDueNow, done.AmountDueNow)
			require.Equal(t, "in_"+fx.sub.RailSubscriptionID, done.Payment.TransactionID)
			posts := fx.stripe.posts("/v1/subscriptions/")
			require.Len(t, posts, 1)
			require.Equal(t, op.ID.String()+":update", posts[0].key)
			for field, want := range map[string]string{
				"items[0][id]":    "si_" + strings.TrimPrefix(fx.sub.RailSubscriptionID, "sub_"),
				"items[0][price]": fx.proRef, "metadata[internal_price_id]": fx.pro.ID.String(),
				"metadata[openrails_tier_change]": op.ID.String(), "proration_behavior": "always_invoice",
				"billing_cycle_anchor": "now", "payment_behavior": "error_if_incomplete",
			} {
				require.Equal(t, want, posts[0].form.Get(field), field)
			}
			local, landed := fx.local(t), fx.stripe.sub(fx.sub.RailSubscriptionID)
			require.Equal(t, fx.pro.ID, local.PriceID)
			require.Equal(t, fx.proRef, landed.priceID)
			require.Equal(t, landed.periodStart, local.CurrentPeriodStartsAt.Unix())
			require.Equal(t, landed.period, local.CurrentPeriodEndsAt.Unix())
			require.Equal(t, landed.period, done.NextChargeDate.Unix())
			var payload StripeTierChangePayload
			require.NoError(t, json.Unmarshal(op.Payload, &payload))
			require.EqualValues(t, 30_000_000-ceilCredit(10_000_000, 25*24, 720), payload.AmountDueNow)
			require.Equal(t, payload.AmountDueNow, done.AmountDueNow)
			if mode == "webhook_first" {
				require.NotEqual(t, landed.period, payload.PeriodEnd.Unix(), "Stripe executes on its own clock")
			}

			// Catalog edits cannot change the stored receipt or trigger another write.
			_, err = fx.db.Pool().Exec(fx.ctx, `UPDATE billing.prices SET amount = 99_000_000 WHERE id = $1`, fx.pro.ID)
			require.NoError(t, err)
			again, err := fx.change(key, fx.pro)
			require.NoError(t, err)
			require.Equal(t, done, again)
			require.Equal(t, intents.StatusSucceeded, fx.verifyOnce(t, key).Status)
			require.Len(t, fx.stripe.posts("/v1/subscriptions/"), 1)
			require.Equal(t, 1, fx.operations(t))
			_, err = fx.change(key, fx.basic)
			var tierErr *TierChangeError
			require.ErrorAs(t, err, &tierErr)
			require.Equal(t, http.StatusConflict, tierErr.HTTPStatus)
			require.Equal(t, openrails.CodeTierChangeIdempotencyConflict, tierErr.Code)
			_, err = fx.svc.TierChange(fx.ctx, &TierChangeRequest{PriceID: openrails.PriceID(fx.pro.ID).String(), SubscriptionID: fx.sub.ID, IdempotencyKey: key}, &UserIdentity{ID: uuid.NewString()})
			require.ErrorAs(t, err, &tierErr)
			require.Equal(t, openrails.CodeTierChangeIdempotencyConflict, tierErr.Code)
			require.NotContains(t, tierErr.Message, done.OperationID)
			_, err = fx.change("other-"+uuid.NewString()[:8], fx.pro)
			require.ErrorIs(t, err, ErrTierChangeSameProduct)
			require.Len(t, fx.stripe.posts("/v1/subscriptions/"), 1)
		})
	}
}

// ceilCredit mirrors CalculateModelBUpgradeCharge's customer-favored credit
// for whole-hour periods.
func ceilCredit(oldFull int64, hoursRemaining, cycleHours int) int64 {
	credit := oldFull * int64(hoursRemaining) / int64(cycleHours)
	if credit%10_000 != 0 {
		credit += 10_000 - credit%10_000
	}
	return credit
}

func TestStripeTierChangeUnconfirmedRequestReplaysFrozenWrite(t *testing.T) {
	for _, mode := range []string{"lostBeforeLanding", "inUse"} {
		t.Run(mode, func(t *testing.T) {
			fx := newStripeTierFixture(t)
			fx.stripe.setMode(mode)
			key := "unsent-" + uuid.NewString()[:8]
			first, err := fx.change(key, fx.pro)
			require.NoError(t, err)
			require.Equal(t, "processing", first.Status)
			require.Equal(t, intents.StatusUnknownNeedsVerify, fx.operation(key).Status)
			// Neither old provider state nor key-in-use proves non-execution.
			require.Equal(t, intents.StatusFailedRetryable, fx.verifyOnce(t, key).Status)
			require.Equal(t, fx.basic.ID, fx.local(t).PriceID)
			done, err := fx.change(key, fx.pro)
			require.NoError(t, err)
			require.Equal(t, "succeeded", done.Status)
			posts := fx.stripe.posts("/v1/subscriptions/")
			require.Len(t, posts, 2)
			require.Equal(t, posts[0].key, posts[1].key, "same provider idempotency key")
			require.Equal(t, posts[0].form, posts[1].form, "identical frozen request")
			require.Equal(t, fx.pro.ID, fx.local(t).PriceID)
			require.Equal(t, 1, fx.operations(t))
		})
	}
}

func TestStripeTierChangeWindowElapsedNeedsOperator(t *testing.T) {
	fx := newStripeTierFixture(t)
	fx.stripe.setMode("lostBeforeLanding")
	key := "window-" + uuid.NewString()[:8]
	_, err := fx.change(key, fx.pro)
	require.NoError(t, err)
	fx.clock.Advance(stripeTierChangeReplayWindow + time.Minute)
	stuck := fx.verifyOnce(t, key)
	require.Equal(t, intents.StatusUnknownNeedsVerify, stuck.Status)
	require.Contains(t, *stuck.LastFailureReason, "idempotency window elapsed")
	resp, err := fx.change(key, fx.pro)
	require.NoError(t, err)
	require.Equal(t, "processing", resp.Status, "past the window the executor never resends")
	require.Len(t, fx.stripe.posts("/v1/subscriptions/"), 1)

	runner := fx.restart()
	resolution := intents.Resolution{Actor: "ops@example.test", Reason: "stripe dashboard ticket 42"}
	// A receipt is the exact subscription read back; the provider does not
	// show the change, so it is refused.
	resolution.ProviderReference = fx.sub.RailSubscriptionID
	_, err = runner.Resolve(fx.ctx, stuck.ID, resolution)
	require.ErrorIs(t, err, intents.ErrResolutionRejected)
	require.ErrorContains(t, err, "does not carry this operation's key")
	resolution.ProviderReference = "sub_other"
	_, err = runner.Resolve(fx.ctx, stuck.ID, resolution)
	require.ErrorIs(t, err, intents.ErrResolutionRejected)
	require.Equal(t, intents.StatusUnknownNeedsVerify, fx.operation(key).Status)

	// Provider-confirmed non-execution closes it; the client learns it must
	// start over; a new key is admitted and pushes Stripe once.
	resolution.ProviderReference, resolution.NotExecuted = "", true
	closed, err := runner.Resolve(fx.ctx, stuck.ID, resolution)
	require.NoError(t, err)
	require.Equal(t, intents.StatusFailedTerminal, closed.Status)
	_, err = fx.change(key, fx.pro)
	var tierErr *TierChangeError
	require.ErrorAs(t, err, &tierErr)
	require.Equal(t, http.StatusConflict, tierErr.HTTPStatus)
	require.Equal(t, openrails.CodeTierChangeRefused, tierErr.Code)
	require.Equal(t, fx.basic.ID, fx.local(t).PriceID)
	fresh, err := fx.change("fresh-"+uuid.NewString()[:8], fx.pro)
	require.NoError(t, err)
	require.Equal(t, "succeeded", fresh.Status)
	require.Len(t, fx.stripe.posts("/v1/subscriptions/"), 2)

	// Non-execution is refused while the provider shows the change.
	fx2 := newStripeTierFixture(t)
	fx2.stripe.setMode("lostAfterLanding")
	key2 := "landed-" + uuid.NewString()[:8]
	_, err = fx2.change(key2, fx2.pro)
	require.NoError(t, err)
	landed := fx2.operation(key2)
	runner2 := fx2.restart()
	_, err = runner2.Resolve(fx2.ctx, landed.ID, intents.Resolution{NotExecuted: true, Actor: "ops", Reason: "portal shows nothing"})
	require.ErrorIs(t, err, intents.ErrResolutionRejected)
	require.ErrorContains(t, err, "provider shows the price change")
	resolved, err := runner2.Resolve(fx2.ctx, landed.ID, intents.Resolution{ProviderReference: fx2.sub.RailSubscriptionID, Actor: "ops", Reason: "portal shows the change"})
	require.NoError(t, err)
	require.Equal(t, intents.StatusSucceeded, resolved.Status)
	require.Equal(t, fx2.pro.ID, fx2.local(t).PriceID)
	require.Len(t, fx2.stripe.posts("/v1/subscriptions/"), 1)
}

func TestStripeTierChangeRefusesDivergentProviderState(t *testing.T) {
	fx := newStripeTierFixture(t)
	fx.stripe.declare(fx.sub.RailSubscriptionID, "si_x", "price_elsewhere", 1, 2)
	_, err := fx.change("diverged-"+uuid.NewString()[:8], fx.pro)
	var tierErr *TierChangeError
	require.ErrorAs(t, err, &tierErr)
	require.Equal(t, http.StatusConflict, tierErr.HTTPStatus)
	require.Empty(t, fx.stripe.posts("/v1/subscriptions/"))
	require.Equal(t, 0, fx.operations(t), "no operation is frozen from divergent facts")
}

func TestStripeTierChangeConcurrentSameKeyRunsOnce(t *testing.T) {
	fx := newStripeTierFixture(t)
	key := "race-" + uuid.NewString()[:8]
	hold, arrived := make(chan struct{}), make(chan struct{})
	fx.stripe.mu.Lock()
	fx.stripe.hold, fx.stripe.arrived = hold, arrived
	fx.stripe.mu.Unlock()
	type answer struct {
		resp *TierChangeResponse
		err  error
	}
	first := make(chan answer, 1)
	go func() {
		resp, err := fx.change(key, fx.pro)
		first <- answer{resp, err}
	}()
	select {
	case <-arrived:
	case <-time.After(20 * time.Second):
		t.Fatal("the first request never reached Stripe")
	}
	second, err := fx.change(key, fx.pro)
	require.NoError(t, err)
	require.Equal(t, "processing", second.Status, "the second caller sees the in-flight operation")
	close(hold)
	got := <-first
	require.NoError(t, got.err)
	require.Equal(t, "succeeded", got.resp.Status)
	require.Equal(t, second.OperationID, got.resp.OperationID)
	require.Len(t, fx.stripe.posts("/v1/subscriptions/"), 1)
	replay, err := fx.change(key, fx.pro)
	require.NoError(t, err)
	require.Equal(t, got.resp, replay)
	require.Equal(t, 1, fx.operations(t))
}

func TestStripeTierChangeDowngradeSchedulesOnce(t *testing.T) {
	fx := newStripeTierFixture(t)
	// Start on Pro so Basic is a downgrade.
	_, err := fx.db.Pool().Exec(fx.ctx, `UPDATE billing.subscriptions SET price_id=$2, product_id=$3 WHERE id=$1`, fx.sub.ID, fx.pro.ID, fx.pro.ProductID)
	require.NoError(t, err)
	fx.stripe.declare(fx.sub.RailSubscriptionID, "si_pro", fx.proRef, fx.sub.CurrentPeriodStartsAt.Unix(), fx.sub.CurrentPeriodEndsAt.Unix())
	key := "down-" + uuid.NewString()[:8]
	first, err := fx.change(key, fx.basic)
	require.NoError(t, err)
	require.Equal(t, "succeeded", first.Status)
	require.Equal(t, "downgrade", first.Action)
	require.Zero(t, first.AmountDueNow)
	require.EqualValues(t, 10_000_000, first.NextChargeAmount)
	require.True(t, first.DelayedStart.Equal(*fx.sub.CurrentPeriodEndsAt))
	op := fx.operation(key)
	creates, phases := fx.stripe.posts("/v1/subscription_schedules"), fx.stripe.posts("/v1/subscription_schedules/")
	require.Len(t, creates, 2, "create then phases")
	require.Len(t, phases, 1)
	require.Equal(t, op.ID.String()+":schedule", creates[0].key)
	require.Equal(t, fx.sub.RailSubscriptionID, creates[0].form.Get("from_subscription"))
	require.Equal(t, op.ID.String()+":phases", phases[0].key)
	require.Equal(t, "release", phases[0].form.Get("end_behavior"))
	require.Equal(t, "none", phases[0].form.Get("proration_behavior"))
	require.Equal(t, fx.proRef, phases[0].form.Get("phases[0][items][0][price]"))
	require.Equal(t, strconv.FormatInt(fx.sub.CurrentPeriodEndsAt.Unix(), 10), phases[0].form.Get("phases[0][end_date]"))
	require.Equal(t, fx.basicRef, phases[0].form.Get("phases[1][items][0][price]"))
	require.Equal(t, "1", phases[0].form.Get("phases[1][duration][interval_count]"))
	require.Equal(t, "month", phases[0].form.Get("phases[1][duration][interval]"))
	require.Equal(t, op.ID.String(), phases[0].form.Get("metadata[openrails_tier_change]"))
	local := fx.local(t)
	require.NotNil(t, local.ScheduledPriceID)
	require.Equal(t, fx.basic.ID, *local.ScheduledPriceID)
	require.Equal(t, fx.pro.ID, local.PriceID)

	again, err := fx.change(key, fx.basic)
	require.NoError(t, err)
	require.Equal(t, first, again)
	require.Len(t, fx.stripe.posts("/v1/subscription_schedules"), 2)
	// A new key is blocked by the scheduled change, not by a second schedule.
	blocked, err := fx.change("again-"+uuid.NewString()[:8], fx.basic)
	require.NoError(t, err)
	require.Equal(t, "blocked", blocked.Status)
	require.Len(t, fx.stripe.posts("/v1/subscription_schedules"), 2)
}

func TestStripeTierChangeDowngradeLostCreateReplaysKey(t *testing.T) {
	fx := newStripeTierFixture(t)
	_, err := fx.db.Pool().Exec(fx.ctx, `UPDATE billing.subscriptions SET price_id=$2, product_id=$3 WHERE id=$1`, fx.sub.ID, fx.pro.ID, fx.pro.ProductID)
	require.NoError(t, err)
	fx.stripe.declare(fx.sub.RailSubscriptionID, "si_pro", fx.proRef, fx.sub.CurrentPeriodStartsAt.Unix(), fx.sub.CurrentPeriodEndsAt.Unix())
	fx.stripe.setMode("lostAfterLanding")
	key := "downlost-" + uuid.NewString()[:8]
	first, err := fx.change(key, fx.basic)
	require.NoError(t, err)
	require.Equal(t, "processing", first.Status)
	require.Equal(t, intents.StatusUnknownNeedsVerify, fx.operation(key).Status)
	require.Nil(t, fx.local(t).ScheduledPriceID)
	require.Equal(t, fx.sub.RailSubscriptionID, fx.stripe.sub(fx.sub.RailSubscriptionID).id)
	require.NotEmpty(t, fx.stripe.sub(fx.sub.RailSubscriptionID).scheduleID, "the create landed at Stripe")

	// An attached schedule is not read as this operation's; the executor
	// replays the create under its key and Stripe hands back the same object.
	require.Equal(t, intents.StatusFailedRetryable, fx.verifyOnce(t, key).Status)
	done, err := fx.change(key, fx.basic)
	require.NoError(t, err)
	require.Equal(t, "succeeded", done.Status)
	creates := fx.stripe.posts("/v1/subscription_schedules")
	require.Len(t, creates, 3, "create, replayed create, phases")
	require.Equal(t, creates[0].key, creates[1].key)
	require.Equal(t, 1, fx.stripe.created, "one schedule exists at Stripe")
	require.Equal(t, fx.basic.ID, *fx.local(t).ScheduledPriceID)
	require.Equal(t, 1, fx.operations(t))
}

func TestStripeTierChangeOperatorReleasesUnsentOperation(t *testing.T) {
	fx := newStripeTierFixture(t)
	fx.svc.Config = &config.Config{ProviderWriteMode: config.ProviderWriteModeReadOnly}
	runner := fx.restart()
	runner.Config = fx.svc.Config
	key := "parked-" + uuid.NewString()[:8]
	resp, err := fx.change(key, fx.pro)
	require.NoError(t, err)
	require.Equal(t, "processing", resp.Status)
	parked := fx.operation(key)
	require.Equal(t, intents.StatusPending, parked.Status)
	require.Empty(t, fx.stripe.posts("/v1/subscriptions/"))
	released, err := runner.Resolve(fx.ctx, parked.ID, intents.Resolution{NotExecuted: true, Actor: "ops", Reason: "provider never armed"})
	require.NoError(t, err)
	require.Equal(t, intents.StatusFailedTerminal, released.Status)
	_, err = fx.change(key, fx.pro)
	var tierErr *TierChangeError
	require.ErrorAs(t, err, &tierErr)
	require.Equal(t, http.StatusConflict, tierErr.HTTPStatus)
	require.Equal(t, openrails.CodeTierChangeRefused, tierErr.Code)
	require.Empty(t, fx.stripe.posts("/v1/subscriptions/"))
}

// Two requests under one merchant-scoped key both miss the replay lookup; B
// pauses in its Stripe preflight while A, a different customer's change,
// inserts and completes under the same key. B's enqueue meets A's row and is
// refused as a key conflict: it never executes or renders A's operation.
func TestStripeTierChangeKeyReuseDuringPreflightIsRefused(t *testing.T) {
	fx := newStripeTierFixture(t)
	otherSub, otherUser := fx.seedCustomer(t)
	key := "reuse-" + uuid.NewString()[:8]
	arrived, release := fx.stripe.holdNextRead("/v1/subscriptions/" + fx.sub.RailSubscriptionID)
	type answer struct {
		resp *TierChangeResponse
		err  error
	}
	b := make(chan answer, 1)
	go func() {
		resp, err := fx.change(key, fx.pro)
		b <- answer{resp, err}
	}()
	select {
	case <-arrived:
	case <-time.After(20 * time.Second):
		t.Fatal("B never reached its provider preflight")
	}
	require.Equal(t, 0, fx.operations(t), "B passed the replay lookup before any operation existed")
	a, err := fx.svc.TierChange(fx.ctx, &TierChangeRequest{PriceID: openrails.PriceID(fx.pro.ID).String(), SubscriptionID: otherSub.ID, IdempotencyKey: key}, otherUser)
	require.NoError(t, err)
	require.Equal(t, "succeeded", a.Status)
	release()
	got := <-b
	require.Nil(t, got.resp, "B never receives A's result")
	var refused *TierChangeError
	require.ErrorAs(t, got.err, &refused)
	require.Equal(t, http.StatusConflict, refused.HTTPStatus)
	require.Equal(t, openrails.CodeTierChangeIdempotencyConflict, refused.Code)
	require.NotContains(t, refused.Message, a.OperationID)
	require.Equal(t, fx.basic.ID, fx.local(t).PriceID)
	require.Equal(t, fx.basicRef, fx.stripe.sub(fx.sub.RailSubscriptionID).priceID, "B's subscription was never pushed")
	require.Len(t, fx.stripe.posts("/v1/subscriptions/"+otherSub.RailSubscriptionID), 1, "A ran once")
	require.Equal(t, 0, fx.operations(t))
	// The key stays A's: B's retry is refused the same way, A's replays.
	_, err = fx.change(key, fx.pro)
	require.ErrorAs(t, err, &refused)
	require.Equal(t, openrails.CodeTierChangeIdempotencyConflict, refused.Code)
	again, err := fx.svc.TierChange(fx.ctx, &TierChangeRequest{PriceID: openrails.PriceID(fx.pro.ID).String(), SubscriptionID: otherSub.ID, IdempotencyKey: key}, otherUser)
	require.NoError(t, err)
	require.Equal(t, a, again)
}

// Stripe's default payment_behavior (allow_incomplete) applies a price change
// whose invoice cannot be paid and leaves the subscription past_due; the
// upgrade therefore sends error_if_incomplete, under which the same declined
// payment is a 402 and nothing changes.
func TestStripeTierChangePaymentBehaviorRefusesUnpaidUpgrade(t *testing.T) {
	fx := newStripeTierFixture(t)
	railSub := fx.sub.RailSubscriptionID
	itemID := fx.stripe.sub(railSub).itemID
	stripe := &subscriptions.StripeService{Config: fx.svc.Config, Rails: fx.svc.Rails}
	fx.stripe.setMode("decline")
	applied, err := stripe.ChangeSubscriptionPrice(fx.ctx, subscriptions.StripePriceChangeParams{SubscriptionID: railSub, ItemID: itemID, StripePriceID: fx.proRef, InternalPriceID: fx.pro.ID.String(), Key: "default-" + uuid.NewString()[:8], ProrationBehavior: "always_invoice"})
	require.NoError(t, err, "the default applies the change despite the declined payment")
	require.Equal(t, "past_due", applied.Status)
	require.Equal(t, fx.proRef, applied.PriceID)
	fx.stripe.declare(railSub, itemID, fx.basicRef, fx.sub.CurrentPeriodStartsAt.Unix(), fx.sub.CurrentPeriodEndsAt.Unix())

	fx.stripe.setMode("decline")
	key := "unpaid-" + uuid.NewString()[:8]
	_, err = fx.change(key, fx.pro)
	var refused *TierChangeError
	require.ErrorAs(t, err, &refused)
	require.Equal(t, http.StatusPaymentRequired, refused.HTTPStatus)
	require.Equal(t, "insufficient_funds", refused.Code)
	require.Contains(t, refused.Message, "insufficient funds")
	posts := fx.stripe.posts("/v1/subscriptions/")
	require.Equal(t, "error_if_incomplete", posts[len(posts)-1].form.Get("payment_behavior"))
	require.Equal(t, fx.basicRef, fx.stripe.sub(railSub).priceID, "the refused update changed nothing at Stripe")
	require.Empty(t, fx.stripe.sub(railSub).status)
	require.Equal(t, fx.basic.ID, fx.local(t).PriceID)
	require.Equal(t, intents.StatusFailedTerminal, fx.operation(key).Status)
	require.Len(t, posts, 2, "default behavior probe then refused tier change")
	_, err = fx.change(key, fx.pro)
	require.ErrorAs(t, err, &refused)
	require.Equal(t, http.StatusPaymentRequired, refused.HTTPStatus)
	require.Len(t, fx.stripe.posts("/v1/subscriptions/"), 2, "a refused operation is never resent")
	fresh, err := fx.change("retry-"+uuid.NewString()[:8], fx.pro)
	require.NoError(t, err)
	require.Equal(t, "succeeded", fresh.Status)
	require.Len(t, fx.stripe.posts("/v1/subscriptions/"), 3, "a fresh key is a new attempt")
}

// A tier change needs a client Idempotency-Key: without one it is refused
// before any admission, provider read or operation.
func TestStripeTierChangeRequiresIdempotencyKey(t *testing.T) {
	fx := newStripeTierFixture(t)
	for _, key := range []string{"", "   "} {
		_, err := fx.change(key, fx.pro)
		var refused *TierChangeError
		require.ErrorAs(t, err, &refused)
		require.Equal(t, http.StatusBadRequest, refused.HTTPStatus)
		require.Equal(t, openrails.CodeTierChangeIdempotencyKeyRequired, refused.Code)
	}
	// Even a request its admission would refuse answers the missing key first.
	_, err := fx.change("", fx.basic)
	var refused *TierChangeError
	require.ErrorAs(t, err, &refused)
	require.Equal(t, openrails.CodeTierChangeIdempotencyKeyRequired, refused.Code)
	fx.stripe.mu.Lock()
	requests := len(fx.stripe.requests)
	fx.stripe.mu.Unlock()
	require.Zero(t, requests, "nothing reached Stripe")
	require.Equal(t, 0, fx.operations(t))
	require.Equal(t, fx.basic.ID, fx.local(t).PriceID)
}
