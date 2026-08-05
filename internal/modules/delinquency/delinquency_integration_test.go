//go:build integration

package delinquency_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jonboulle/clockwork"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/delinquency"
	"github.com/open-rails/openrails/internal/modules/merchantconfig"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/merchant"
)

func TestMain(m *testing.M) { dbtest.RunMain(m) }

// or#878. What is being pinned here is a BOUNDARY as much as a state machine:
// OpenRails may know a payer is delinquent, refuse new spend, and say so — and
// it may not cancel a subscription, revoke an entitlement, or touch a stored
// card because a bill went unpaid. Every case below asserts both halves.

// declineCharger declines every charge with one verbatim rail code, the way a
// real gateway does. NMI 200 is a generic decline: or#870 bucket 1, so
// collection keeps its schedule and the invoice stays owed — exactly the state
// the delinquency clock is supposed to run on.
type declineCharger struct{ calls int }

func (c *declineCharger) ChargeSavedMethod(_ context.Context, _ money.ChargeRequest) (money.ChargeResult, error) {
	c.calls++
	code, msg := "200", "declined"
	return money.ChargeResult{Rail: string(models.RailNMI), Declined: true, FailureCode: &code, FailureMessage: &msg}, nil
}

type env struct {
	dbi      *db.DB
	pool     *pgxpool.Pool
	money    *money.MoneyService
	svc      *delinquency.Service
	clock    *clockwork.FakeClock
	payer    identity.CustomerID
	currency string
	ctx      context.Context
	merchant merchant.ID
}

// newEnv seeds one arrears payer with a saved card, under a fake clock both the
// money service and the evaluator share.
func newEnv(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
	dbi := dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID())
	pool := dbi.Pool()
	dbtest.EnsureTestMerchant(ctx, t, pool)
	ctx = dbtest.WithTestMerchant(ctx)

	clock := clockwork.NewFakeClockAt(time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC))
	payer := identity.CustomerIDFromString(uuid.NewString())
	dbtest.EnsureCustomerIDPgx(ctx, t, pool, payer.UUID().String())

	e := &env{
		dbi: dbi, pool: pool,
		money:    money.NewMoneyService(dbi, clock),
		svc:      delinquency.NewService(dbi, clock),
		clock:    clock,
		payer:    payer,
		currency: money.DefaultCurrency,
		ctx:      ctx,
		merchant: dbtest.TestMerchantID,
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.host_lifecycle_events WHERE subject_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.customer_delinquency WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.notification_queue WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoice_items WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoices WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.money_settings WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.merchant_configurations WHERE merchant_id = $1", dbtest.TestMerchantID.UUID())
	})
	return e
}

// setPolicy writes the merchant's declared delinquency policy.
func (e *env) setPolicy(t *testing.T, ctx context.Context, graceDays int, floor int64) {
	t.Helper()
	store := merchantconfig.NewStore(e.dbi)
	cfg, _, err := store.Get(ctx)
	require.NoError(t, err)
	cfg.ArrearsGraceDays = &graceDays
	cfg.ArrearsDelinquencyFloor = &floor
	require.NoError(t, store.Upsert(ctx, cfg))
}

// accrueAndBill runs the real arrears path: accrue owed usage, then finalize the
// statement so it becomes an OPEN receivable with a due date.
func (e *env) accrueAndBill(t *testing.T, amount int64) uuid.UUID {
	t.Helper()
	pm := e.seedCard(t)
	_, err := e.money.UpsertAccountSettings(e.ctx, e.payer, e.currency, money.AccountSettingsInput{
		BillingMode: strptr(money.BillingModeArrears), AutoTopupPaymentMethod: &pm,
	})
	require.NoError(t, err)
	accruedAt := e.clock.Now().UTC()
	_, err = e.money.AccrueOwed(e.ctx, e.payer, e.currency, "usage", "or878-"+uuid.NewString(), amount)
	require.NoError(t, err)

	// The statement period is half-open, so it has to CLOSE after the accrual's
	// invoice_at or the item falls outside it and the invoice finalizes paid.
	e.clock.Advance(time.Minute)
	now := e.clock.Now().UTC()
	inv, err := e.money.FinalizeInvoice(e.ctx, e.payer, e.currency, accruedAt.Add(-24*time.Hour), now)
	require.NoError(t, err)
	require.NotNil(t, inv)
	require.Equal(t, "open", inv.Status)
	require.Equal(t, amount, inv.AmountDue)
	require.NotNil(t, inv.DueAt, "an arrears receivable must carry a due date; the delinquency clock runs on it")
	return inv.ID
}

func (e *env) seedCard(t *testing.T) uuid.UUID {
	t.Helper()
	pm := uuid.New()
	_, err := gen.New(e.pool).CreatePaymentMethod(e.ctx, gen.CreatePaymentMethodParams{
		ID:                   pm,
		MerchantID:           e.merchant.UUID(),
		CustomerID:           e.payer.UUID(),
		Rail:                 string(models.RailNMI),
		PspID:                dbtest.EnsureTestPSP(e.ctx, t, e.pool, e.merchant.UUID(), string(models.RailNMI)),
		InitialTransactionID: "init_" + pm.String(),
		RailCustomerRef:      "vault_" + pm.String(),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = e.pool.Exec(e.ctx, "DELETE FROM openrails.payment_methods WHERE id = $1", pm) })
	return pm
}

// seedActiveSubscription is the CONTROL in every case: a live subscription on
// the same payer. Delinquency must never touch it.
func (e *env) seedActiveSubscription(t *testing.T) uuid.UUID {
	t.Helper()
	q := e.dbi.Gen(e.ctx)
	now := e.clock.Now().UTC()
	periodEnd := now.AddDate(0, 1, 0)
	productID, priceID, subID := uuid.New(), uuid.New(), uuid.New()
	cycleHours := int32(30 * 24)

	_, err := q.CreateProduct(e.ctx, gen.CreateProductParams{
		ID: productID, MerchantID: e.merchant.UUID(),
		Key: "or878_control_" + uuid.NewString(), DisplayName: "Delinquency Control",
		CreatedAt: now, UpdatedAt: now,
	})
	require.NoError(t, err)
	_, err = q.CreatePrice(e.ctx, gen.CreatePriceParams{
		ID: priceID, MerchantID: e.merchant.UUID(), ProductID: productID,
		Amount: 100, Currency: "USD", AccessDurationHours: &cycleHours, AutoRenew: true,
		CreatedAt: now, UpdatedAt: now,
	})
	require.NoError(t, err)
	_, err = q.CreateSubscription(e.ctx, gen.CreateSubscriptionParams{
		ID: subID, MerchantID: e.merchant.UUID(), CustomerID: e.payer.UUID(),
		ProductID: productID, PriceID: &priceID,
		Status: string(models.StatusActive), Rail: string(models.RailNMI),
		PspID:                 dbtest.EnsureTestPSP(e.ctx, t, e.pool, e.merchant.UUID(), string(models.RailNMI)),
		RailSubscriptionID:    "sub_or878_" + subID.String(),
		CurrentPeriodStartsAt: &now, CurrentPeriodEndsAt: &periodEnd, StartedAt: now,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = e.pool.Exec(e.ctx, "DELETE FROM openrails.subscriptions WHERE id = $1", subID)
		_, _ = e.pool.Exec(e.ctx, "DELETE FROM openrails.prices WHERE id = $1", priceID)
		_, _ = e.pool.Exec(e.ctx, "DELETE FROM openrails.products WHERE id = $1", productID)
	})
	return subID
}

func (e *env) state(t *testing.T) delinquency.Snapshot {
	t.Helper()
	rows, err := e.svc.ListForCustomer(e.ctx, e.payer)
	require.NoError(t, err)
	require.Len(t, rows, 1, "one (payer, currency) delinquency row")
	return rows[0]
}

func (e *env) events(t *testing.T) []gen.ListPendingHostLifecycleEventsRow {
	t.Helper()
	rows, err := e.dbi.Gen(e.ctx).ListPendingHostLifecycleEvents(e.ctx, gen.ListPendingHostLifecycleEventsParams{
		MerchantID: e.merchant.UUID(), RowLimit: 50,
	})
	require.NoError(t, err)
	out := make([]gen.ListPendingHostLifecycleEventsRow, 0, len(rows))
	for _, r := range rows {
		if r.SubjectID == e.payer.UUID() {
			out = append(out, r)
		}
	}
	return out
}

func strptr(s string) *string { return &s }

// TestDelinquencyLifecycle is the whole arc through the production entry points:
// accrue → invoice → a bucket-1 decline → grace → past grace → delinquent →
// paid → current. One event per transition, no event for a re-evaluation, and a
// live subscription untouched throughout.
func TestDelinquencyLifecycle(t *testing.T) {
	e := newEnv(t)
	e.setPolicy(t, e.ctx, 7, 1_000_000)
	subID := e.seedActiveSubscription(t)
	invoiceID := e.accrueAndBill(t, 5_000_000)

	// A real decline on the real collection path. or#870 bucket 1: the schedule
	// continues and the debt stands — which is precisely the situation the
	// delinquency clock exists to measure, and the DECLINE alone must not
	// produce a delinquency state.
	charger := &declineCharger{}
	_, err := e.money.ChargeOutstanding(e.ctx, charger, 0)
	require.NoError(t, err)
	require.Equal(t, 1, charger.calls, "the collection path must have attempted the charge")

	rows, err := e.svc.ListForCustomer(e.ctx, e.payer)
	require.NoError(t, err)
	require.Empty(t, rows, "a decline is the CARD axis; it may not create delinquency state on its own")

	// --- inside grace -------------------------------------------------------
	e.clock.Advance(time.Hour)
	res, err := e.svc.Evaluate(e.ctx, e.clock.Now().UTC())
	require.NoError(t, err)
	require.Len(t, res.Transitions, 1)
	require.Equal(t, delinquency.StateGrace, res.Transitions[0].To)

	snap := e.state(t)
	require.Equal(t, delinquency.StateGrace, snap.State)
	require.Equal(t, int64(5_000_000), snap.OverdueAmount)
	require.Equal(t, 1, snap.OverdueInvoices)
	require.NotNil(t, snap.OverdueSince)

	delinquent, err := e.svc.IsDelinquent(e.ctx, e.payer, e.currency)
	require.NoError(t, err)
	require.False(t, delinquent, "grace must not refuse anyone's spend — that is what grace IS")

	// --- past grace ---------------------------------------------------------
	e.clock.Advance(8 * 24 * time.Hour)
	res, err = e.svc.Evaluate(e.ctx, e.clock.Now().UTC())
	require.NoError(t, err)
	require.Len(t, res.Transitions, 1)
	require.Equal(t, delinquency.StateGrace, res.Transitions[0].From)
	require.Equal(t, delinquency.StateDelinquent, res.Transitions[0].To)
	require.Equal(t, delinquency.StateDelinquent, e.state(t).State)

	delinquent, err = e.svc.IsDelinquent(e.ctx, e.payer, e.currency)
	require.NoError(t, err)
	require.True(t, delinquent, "past grace, over the floor: new spend is refused")

	// Re-evaluating an unchanged state is a no-op — no second instruction to
	// the host, no second notification.
	before := len(e.events(t))
	res, err = e.svc.Evaluate(e.ctx, e.clock.Now().UTC())
	require.NoError(t, err)
	require.Empty(t, res.Transitions, "a level, re-read, is not an edge")
	require.Len(t, e.events(t), before, "no duplicate signal for an unchanged state")

	// --- the boundary: what delinquency may NOT do --------------------------
	requireSubscriptionActive(t, e, subID)
	var cards int
	require.NoError(t, e.pool.QueryRow(e.ctx,
		`SELECT count(*) FROM openrails.payment_methods WHERE customer_id = $1`, e.payer.UUID()).Scan(&cards))
	require.Equal(t, 1, cards, "delinquency must never delete a stored payment method")
	var invoiceStatus string
	require.NoError(t, e.pool.QueryRow(e.ctx,
		`SELECT status FROM openrails.invoices WHERE id = $1`, invoiceID).Scan(&invoiceStatus))
	require.NotEqual(t, "uncollectible", invoiceStatus,
		"delinquency is about SERVICE; writing the debt off is the collection schedule's decision, not the clock's")

	// --- paid ---------------------------------------------------------------
	settleInvoice(t, e, invoiceID)
	res, err = e.svc.Evaluate(e.ctx, e.clock.Now().UTC())
	require.NoError(t, err)
	require.Len(t, res.Transitions, 1)
	require.Equal(t, delinquency.StateDelinquent, res.Transitions[0].From)
	require.Equal(t, delinquency.StateCurrent, res.Transitions[0].To)

	snap = e.state(t)
	require.Equal(t, delinquency.StateCurrent, snap.State)
	require.Nil(t, snap.OverdueSince)
	require.Zero(t, snap.OverdueAmount)

	delinquent, err = e.svc.IsDelinquent(e.ctx, e.payer, e.currency)
	require.NoError(t, err)
	require.False(t, delinquent, "a payer who has settled must be admitted again")

	// --- the signal ---------------------------------------------------------
	events := e.events(t)
	require.Len(t, events, 3, "exactly one event per transition: grace, entered, cleared")
	require.Equal(t, "delinquency.grace", events[0].EventType)
	require.Equal(t, "delinquency.entered", events[1].EventType)
	require.Equal(t, "delinquency.cleared", events[2].EventType)
	for _, ev := range events {
		require.Equal(t, "customer", ev.SubjectType)
		require.Equal(t, e.payer.UUID(), ev.SubjectID)
		require.Equal(t, e.currency, ev.Currency)
	}
	var payload map[string]any
	require.NoError(t, json.Unmarshal(events[1].Data, &payload))
	require.Equal(t, "grace", payload["from_state"])
	require.Equal(t, "delinquent", payload["to_state"])
	require.EqualValues(t, 5_000_000, payload["overdue_amount"])
	require.EqualValues(t, 7, payload["grace_days"])

	// --- the payer was told -------------------------------------------------
	requireNotification(t, e, models.NotificationAccountDelinquent)
	requireNotification(t, e, models.NotificationAccountDelinquencyClosed)
}

// A payer whose debt never reaches the merchant's floor sits in grace forever.
// This is the guard against a rounding remnant on a year-old invoice quietly
// turning into a shutoff instruction.
func TestDebtBelowTheFloorNeverEscalates(t *testing.T) {
	e := newEnv(t)
	e.setPolicy(t, e.ctx, 0, 10_000_000)
	e.accrueAndBill(t, 500_000)

	e.clock.Advance(400 * 24 * time.Hour)
	_, err := e.svc.Evaluate(e.ctx, e.clock.Now().UTC())
	require.NoError(t, err)

	require.Equal(t, delinquency.StateGrace, e.state(t).State)
	delinquent, err := e.svc.IsDelinquent(e.ctx, e.payer, e.currency)
	require.NoError(t, err)
	require.False(t, delinquent)
}

// The grace window is the MERCHANT's business policy, so two merchants reading
// the same debt must be able to reach opposite answers.
func TestPolicyIsPerMerchant(t *testing.T) {
	strict := newEnv(t)
	strict.setPolicy(t, strict.ctx, 0, 0)
	strict.accrueAndBill(t, 5_000_000)
	strict.clock.Advance(time.Hour)
	_, err := strict.svc.Evaluate(strict.ctx, strict.clock.Now().UTC())
	require.NoError(t, err)
	require.Equal(t, delinquency.StateDelinquent, strict.state(t).State,
		"grace 0 means delinquent as soon as it is overdue")

	lenient := newEnv(t)
	lenient.setPolicy(t, lenient.ctx, 90, 0)
	lenient.accrueAndBill(t, 5_000_000)
	lenient.clock.Advance(30 * 24 * time.Hour)
	_, err = lenient.svc.Evaluate(lenient.ctx, lenient.clock.Now().UTC())
	require.NoError(t, err)
	require.Equal(t, delinquency.StateGrace, lenient.state(t).State,
		"the same 30-day-old debt is merely in grace under a 90-day window")
}

// Two merchants, one shared database, RLS forced: neither may read, evaluate or
// ack the other's delinquency. The feed carries shutoff instructions, so a leak
// here is a leak of who is failing to pay whom.
func TestDelinquencyIsMerchantIsolated(t *testing.T) {
	e := newEnv(t)
	e.setPolicy(t, e.ctx, 0, 0)
	e.accrueAndBill(t, 5_000_000)
	e.clock.Advance(time.Hour)
	_, err := e.svc.Evaluate(e.ctx, e.clock.Now().UTC())
	require.NoError(t, err)
	require.Equal(t, delinquency.StateDelinquent, e.state(t).State)

	other := merchant.ID(uuid.New())
	_, err = e.pool.Exec(e.ctx, `
		INSERT INTO openrails.merchants (id, slug, status) VALUES ($1, $2, 'active')`,
		other.UUID(), "or878-other-"+uuid.NewString()[:8])
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = e.pool.Exec(context.Background(), "DELETE FROM openrails.merchants WHERE id = $1", other.UUID())
	})

	otherDB := dbtest.OpenMerchantDB(t, other.UUID())
	otherCtx := merchant.WithID(context.Background(), other)
	otherSvc := delinquency.NewService(otherDB, e.clock)

	rows, err := otherSvc.List(otherCtx, "", 100)
	require.NoError(t, err)
	require.Empty(t, rows, "another merchant's delinquency roster must be invisible")

	rows, err = otherSvc.ListForCustomer(otherCtx, e.payer)
	require.NoError(t, err)
	require.Empty(t, rows)

	delinquent, err := otherSvc.IsDelinquent(otherCtx, e.payer, e.currency)
	require.NoError(t, err)
	require.False(t, delinquent, "one merchant's unpaid bill must never gate another merchant's admission")

	events, err := otherDB.Gen(otherCtx).ListPendingHostLifecycleEvents(otherCtx, gen.ListPendingHostLifecycleEventsParams{
		MerchantID: other.UUID(), RowLimit: 50,
	})
	require.NoError(t, err)
	require.Empty(t, events)

	// And the evaluator, run under the other merchant, must find no work.
	res, err := otherSvc.Evaluate(otherCtx, e.clock.Now().UTC())
	require.NoError(t, err)
	require.Zero(t, res.Evaluated)
	require.Equal(t, delinquency.StateDelinquent, e.state(t).State, "the original state is untouched")
}

// The feed is the deliverable, so its ack discipline is pinned directly: acked
// events leave the pending feed, and only DELIVERED rows are ever prunable. (The
// merchant-scoped host seam over the same table is covered in
// internal/controlplane.)
func TestHostLifecycleFeedAckDiscipline(t *testing.T) {
	e := newEnv(t)
	e.setPolicy(t, e.ctx, 0, 0)
	e.accrueAndBill(t, 5_000_000)
	e.clock.Advance(time.Hour)
	_, err := e.svc.Evaluate(e.ctx, e.clock.Now().UTC())
	require.NoError(t, err)

	pending := e.events(t)
	require.Len(t, pending, 1)
	target := pending[0]
	require.Equal(t, "delinquency.entered", target.EventType)

	q := e.dbi.Gen(e.ctx)
	now := e.clock.Now().UTC()

	// Pruning must not touch an UNACKED event: an unread shutoff instruction is
	// undone work, not garbage.
	n, err := q.DeleteDeliveredHostLifecycleEventsBefore(e.ctx, gen.DeleteDeliveredHostLifecycleEventsBeforeParams{
		MerchantID: e.merchant.UUID(), Cutoff: now.Add(365 * 24 * time.Hour), RowLimit: 100,
	})
	require.NoError(t, err)
	require.Zero(t, n)
	require.Len(t, e.events(t), 1)

	acked, err := q.AcknowledgeHostLifecycleEvent(e.ctx, gen.AcknowledgeHostLifecycleEventParams{
		MerchantID: e.merchant.UUID(), ID: target.ID, Now: now,
	})
	require.NoError(t, err)
	require.EqualValues(t, 1, acked)
	require.Empty(t, e.events(t), "an acked event leaves the pending feed")

	n, err = q.DeleteDeliveredHostLifecycleEventsBefore(e.ctx, gen.DeleteDeliveredHostLifecycleEventsBeforeParams{
		MerchantID: e.merchant.UUID(), Cutoff: now.Add(time.Hour), RowLimit: 100,
	})
	require.NoError(t, err)
	require.EqualValues(t, 1, n)
}

// The admission gate never refuses on the projection alone. A stored
// `delinquent` whose debt has since been settled must admit — our evaluation lag
// is our problem, not the customer's outage.
func TestAdmissionGateFailsOpenOnAStaleProjection(t *testing.T) {
	e := newEnv(t)
	e.setPolicy(t, e.ctx, 0, 0)
	invoiceID := e.accrueAndBill(t, 5_000_000)
	e.clock.Advance(time.Hour)
	_, err := e.svc.Evaluate(e.ctx, e.clock.Now().UTC())
	require.NoError(t, err)

	delinquent, err := e.svc.IsDelinquent(e.ctx, e.payer, e.currency)
	require.NoError(t, err)
	require.True(t, delinquent)

	// Settle the debt WITHOUT re-running the evaluator: the stored state still
	// says delinquent.
	settleInvoice(t, e, invoiceID)
	require.Equal(t, delinquency.StateDelinquent, e.state(t).State, "the projection is deliberately left stale")

	delinquent, err = e.svc.IsDelinquent(e.ctx, e.payer, e.currency)
	require.NoError(t, err)
	require.False(t, delinquent,
		"the gate must re-read the invoices before refusing; a paid customer is never held out by our lag")
}

func requireSubscriptionActive(t *testing.T, e *env, subID uuid.UUID) {
	t.Helper()
	var status string
	var cancelledAt *time.Time
	require.NoError(t, e.pool.QueryRow(e.ctx,
		`SELECT status, cancelled_at FROM openrails.subscriptions WHERE id = $1`, subID).Scan(&status, &cancelledAt))
	require.Equal(t, string(models.StatusActive), status,
		"delinquency must NEVER cancel a subscription: the operator owns the shutoff, and entitlements are never lost to our judgement")
	require.Nil(t, cancelledAt)
}

func requireNotification(t *testing.T, e *env, eventType models.NotificationEventType) {
	t.Helper()
	var n int
	require.NoError(t, e.pool.QueryRow(e.ctx,
		`SELECT count(*) FROM openrails.notification_queue WHERE customer_id = $1 AND event_type = $2`,
		e.payer.UUID(), string(eventType)).Scan(&n))
	require.Equal(t, 1, n, "the payer is told exactly once on each rung it can act on: %s", eventType)
}

// settleInvoice pays the receivable off the way a successful collection does —
// amount_due to zero, status paid — so the derivation sees a settled debt.
func settleInvoice(t *testing.T, e *env, invoiceID uuid.UUID) {
	t.Helper()
	_, err := e.pool.Exec(e.ctx, `
		UPDATE openrails.invoices
		   SET amount_paid = total_amount, amount_due = 0, status = 'paid', paid_at = $2, updated_at = $2
		 WHERE id = $1`, invoiceID, e.clock.Now().UTC())
	require.NoError(t, err)
}
