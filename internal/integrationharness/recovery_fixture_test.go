//go:build integration

package integrationharness

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/pkg/merchant"
)

// SubscriptionFixture is a past-due, OpenRails-rebilled NMI subscription: the
// state a customer retry-now acts on.
type SubscriptionFixture struct {
	Merchant     merchant.ID
	Customer     uuid.UUID
	Method       uuid.UUID
	Subscription uuid.UUID
	Product      uuid.UUID
	Price        uuid.UUID
	PSP          uuid.UUID
	Vault        string
	PeriodEnd    time.Time
	Amount       int64
}

// SubscriptionOption shapes SeedPastDueSubscription.
type SubscriptionOption func(*subscriptionSeed)

type subscriptionSeed struct {
	rail         models.Rail
	rebillDriver string
	customer     uuid.UUID
}

// SubscriptionForCustomer seeds the subscription for an existing customer.
func SubscriptionForCustomer(customer uuid.UUID) SubscriptionOption {
	return func(s *subscriptionSeed) { s.customer = customer }
}

// SubscriptionOnRail seeds the subscription and its method on another rail.
func SubscriptionOnRail(rail models.Rail) SubscriptionOption {
	return func(s *subscriptionSeed) { s.rail = rail }
}

// SubscriptionProviderBilled marks the NMI method provider-rebilled (the
// rail's own recurring engine charges it; OpenRails never manual-rebills it).
func SubscriptionProviderBilled() SubscriptionOption {
	return func(s *subscriptionSeed) { s.rebillDriver = models.RebillDriverProvider }
}

// SeedPastDueSubscription arms a loopback NMI account, vaults an instrument
// with billing refs for a fresh customer and creates a monthly subscription
// whose last renewal failed: past_due, one recorded failure, next scheduled
// retry two days out (the dunning worker will not touch it during a test).
func (h *Harness) SeedPastDueSubscription(rt *app.Runtime, mid merchant.ID, opts ...SubscriptionOption) SubscriptionFixture {
	h.t.Helper()
	seed := subscriptionSeed{rail: models.RailNMI, rebillDriver: models.RebillDriverOpenRails}
	for _, opt := range opts {
		opt(&seed)
	}
	var psp uuid.UUID
	if seed.rail == models.RailNMI {
		psp = h.ArmLoopbackNMI(rt, mid)
	} else {
		psp = h.fixturePSP(mid, seed.rail)
	}
	pool := h.sharedPool()
	q := gen.New(pool)
	now := time.Now().UTC().Truncate(time.Second)
	customer, method, subscription, product, price := seed.customer, uuid.New(), uuid.New(), uuid.New(), uuid.New()
	vault := "vault-" + method.String()[:8]
	cycleHours := int32(720)
	description := "Recovery"
	if customer == uuid.Nil {
		customer = uuid.New()
		_, err := pool.Exec(h.ctx, `INSERT INTO openrails.customers(merchant_id,id) VALUES($1,$2)`, mid.UUID(), customer)
		require.NoError(h.t, err)
	}
	_, err := q.CreateProduct(h.ctx, gen.CreateProductParams{
		ID: product, Key: "recovery-" + product.String()[:8], DisplayName: "Recovery Pro", MerchantID: mid.UUID(),
		Description: &description, CreatedAt: now, UpdatedAt: now,
	})
	require.NoError(h.t, err)
	_, err = q.CreatePrice(h.ctx, gen.CreatePriceParams{
		ID: price, ProductID: product, Key: "recovery-monthly-" + price.String()[:8], Amount: 12_000_000, Currency: "USD", MerchantID: mid.UUID(),
		AccessDurationHours: &cycleHours, AutoRenew: true, CreatedAt: now, UpdatedAt: now,
	})
	require.NoError(h.t, err)
	_, err = q.CreatePaymentMethod(h.ctx, gen.CreatePaymentMethodParams{
		ID: method, MerchantID: mid.UUID(), CustomerID: customer, Rail: string(seed.rail), PspID: psp,
		RailCustomerRef: vault, RailMethodRef: "billing-" + method.String()[:8], RebillDriver: seed.rebillDriver,
		InitialTransactionID: "init-" + method.String(), CreatedAt: now, UpdatedAt: now,
	})
	require.NoError(h.t, err)
	dbtest.SeedNMIStoredCredentialRefs(h.ctx, h.t, pool, method)

	periodEnd := now.Add(-2 * time.Hour)
	periodStart := periodEnd.Add(-time.Duration(cycleHours) * time.Hour)
	nextRetry := now.Add(48 * time.Hour)
	attempts := int32(1)
	_, err = q.CreateSubscription(h.ctx, gen.CreateSubscriptionParams{
		ID: subscription, MerchantID: mid.UUID(), CustomerID: customer, ProductID: product, PriceID: &price,
		Status: string(models.StatusPastDue), Rail: string(seed.rail), PspID: psp,
		RailSubscriptionID: "rail-sub-" + subscription.String()[:8], PaymentMethodID: &method,
		CurrentPeriodStartsAt: &periodStart, CurrentPeriodEndsAt: &periodEnd, StartedAt: periodStart,
		LastRetryAt: &periodEnd, NextRetryAt: &nextRetry, RetryAttempts: &attempts, CreatedAt: now, UpdatedAt: now,
	})
	require.NoError(h.t, err)
	// End what this fixture opened so it holds no obligation on a shared
	// provider account (the payment-provider drain tests count them). A
	// refused rail's rows reference nothing else and are removed with it.
	t := h.t
	t.Cleanup(func() {
		ctx := context.Background()
		if seed.rail != models.RailNMI {
			for _, row := range []struct {
				table string
				id    uuid.UUID
			}{{"subscriptions", subscription}, {"payment_methods", method}, {"prices", price}, {"products", product}} {
				_, err := pool.Exec(ctx, `DELETE FROM openrails.`+row.table+` WHERE id = $1`, row.id)
				assertNoCleanupError(t, err, "delete "+row.table)
			}
			return
		}
		_, err := pool.Exec(ctx, `UPDATE openrails.rail_intents SET status = 'superseded', updated_at = now() WHERE subscription_id = $1 AND status IN ('pending', 'in_flight', 'failed_retryable', 'unknown_needs_verify')`, subscription)
		assertNoCleanupError(t, err, "supersede live rebills")
		_, err = pool.Exec(ctx, `UPDATE openrails.subscriptions SET status = 'cancelled', cancelled_at = now(), cancel_type = 'merchant', ended_at = now(), next_retry_at = NULL, updated_at = now() WHERE id = $1 AND status <> 'cancelled'`, subscription)
		assertNoCleanupError(t, err, "end subscription")
	})
	return SubscriptionFixture{Merchant: mid, Customer: customer, Method: method, Subscription: subscription, Product: product, Price: price, PSP: psp, Vault: vault, PeriodEnd: periodEnd, Amount: 12_000_000}
}

// SeedStripeMethod vaults a Stripe instrument for the customer: a saved
// method the customer surface must refuse.
func (h *Harness) SeedStripeMethod(mid merchant.ID, customer uuid.UUID) uuid.UUID {
	h.t.Helper()
	pool := h.sharedPool()
	psp := h.fixturePSP(mid, models.RailStripe)
	method := uuid.New()
	t := h.t
	t.Cleanup(func() {
		_, err := pool.Exec(context.Background(), `DELETE FROM openrails.payment_methods WHERE id = $1`, method)
		assertNoCleanupError(t, err, "delete stripe method")
	})
	now := time.Now().UTC()
	_, err := gen.New(pool).CreatePaymentMethod(h.ctx, gen.CreatePaymentMethodParams{
		ID: method, MerchantID: mid.UUID(), CustomerID: customer, Rail: string(models.RailStripe), PspID: psp,
		RailCustomerRef: "cus_" + method.String()[:8], RailMethodRef: "pm_" + method.String()[:8],
		InitialTransactionID: "pi_" + method.String()[:8], CreatedAt: now, UpdatedAt: now,
	})
	require.NoError(h.t, err)
	return method
}

// HoldRowLock locks one row FOR UPDATE until release is called, so
// concurrent requests queue behind it; LockWaiters observes them queued.
func (h *Harness) HoldRowLock(table string, id uuid.UUID) (release func()) {
	h.t.Helper()
	tx, err := h.sharedPool().Begin(h.ctx)
	require.NoError(h.t, err)
	_, err = tx.Exec(h.ctx, `SELECT id FROM openrails.`+table+` WHERE id = $1 FOR UPDATE`, id)
	require.NoError(h.t, err)
	return func() { require.NoError(h.t, tx.Rollback(h.ctx)) }
}

// LockWaiters counts sessions of the test database blocked on a lock.
func (h *Harness) LockWaiters() int {
	var n int
	if err := h.sharedPool().QueryRow(h.ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&n); err != nil {
		return 0
	}
	return n
}

// fixturePSP is the merchant's provider account on a rail the customer
// surface refuses. One the fixture had to create is removed at cleanup (after
// the rows that name it), so no test leaves a second account on a shared
// merchant behind.
func (h *Harness) fixturePSP(mid merchant.ID, rail models.Rail) uuid.UUID {
	h.t.Helper()
	pool := h.sharedPool()
	var existing uuid.UUID
	err := pool.QueryRow(h.ctx, `SELECT id FROM openrails.psps WHERE merchant_id = $1 AND rail = $2 AND archived = false ORDER BY created_at DESC, id DESC LIMIT 1`, mid.UUID(), string(rail)).Scan(&existing)
	if err == nil {
		return existing
	}
	psp := dbtest.EnsureTestPSP(h.ctx, h.t, pool, mid.UUID(), string(rail))
	t := h.t
	t.Cleanup(func() {
		_, err := pool.Exec(context.Background(), `DELETE FROM openrails.psps WHERE id = $1`, psp)
		assertNoCleanupError(t, err, "delete fixture psp")
	})
	return psp
}

func assertNoCleanupError(t *testing.T, err error, what string) {
	if err != nil {
		t.Errorf("fixture cleanup %s: %v", what, err)
	}
}

// SubscriptionState is the recovery-relevant slice of a subscription row.
type SubscriptionState struct {
	Status        string
	RetryAttempts *int32
	NextRetryAt   *time.Time
	PeriodEnd     *time.Time
}

func (h *Harness) SubscriptionState(id uuid.UUID) SubscriptionState {
	h.t.Helper()
	var s SubscriptionState
	require.NoError(h.t, h.sharedPool().QueryRow(h.ctx, `SELECT status::text, retry_attempts, next_retry_at, current_period_ends_at FROM openrails.subscriptions WHERE id = $1`, id).
		Scan(&s.Status, &s.RetryAttempts, &s.NextRetryAt, &s.PeriodEnd))
	return s
}

// LatestRebillOperation returns the newest manual_rebill operation for the
// subscription.
func (h *Harness) LatestRebillOperation(subscription uuid.UUID) CollectionOperation {
	h.t.Helper()
	var op CollectionOperation
	require.NoError(h.t, h.sharedPool().QueryRow(h.ctx, `SELECT id, status FROM openrails.rail_intents WHERE intent_type=$1 AND subscription_id=$2 ORDER BY created_at DESC LIMIT 1`,
		intents.TypeManualRebill, subscription).Scan(&op.ID, &op.Status))
	return op
}

// RebillOperations counts the subscription's manual_rebill operations.
func (h *Harness) RebillOperations(subscription uuid.UUID) int {
	h.t.Helper()
	var n int
	require.NoError(h.t, h.sharedPool().QueryRow(h.ctx, `SELECT count(*) FROM openrails.rail_intents WHERE intent_type=$1 AND subscription_id=$2`, intents.TypeManualRebill, subscription).Scan(&n))
	return n
}

// SubscriptionPayments lists the subscription's payment rows' statuses,
// newest first.
func (h *Harness) SubscriptionPayments(subscription uuid.UUID) []string {
	h.t.Helper()
	rows, err := h.sharedPool().Query(h.ctx, `SELECT status::text FROM openrails.payments WHERE subscription_id=$1 AND deleted_at IS NULL ORDER BY purchased_at DESC, id DESC`, subscription)
	require.NoError(h.t, err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		require.NoError(h.t, rows.Scan(&s))
		out = append(out, s)
	}
	return out
}
