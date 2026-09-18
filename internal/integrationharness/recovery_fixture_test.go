//go:build integration

package integrationharness

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/app"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/intents"
	riverjobs "github.com/open-rails/openrails/internal/river"
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
	// RailSubscriptionID is the NMI recurring subscription a rebill names;
	// register its plan on the gateway (FakeNMIGateway.RegisterPlan).
	RailSubscriptionID string
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
	return SubscriptionFixture{Merchant: mid, Customer: customer, Method: method, Subscription: subscription, Product: product, Price: price, PSP: psp, Vault: vault, PeriodEnd: periodEnd, Amount: 12_000_000, RailSubscriptionID: "rail-sub-" + subscription.String()[:8]}
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

// ReattributeToAnotherPSP moves the subscription's instrument to another NMI
// account of the merchant, as the #297 custody remap does (it re-attributes
// instruments and leaves subscriptions): the #657 same-PSP invariant no longer
// holds. Cleanup restores the instrument and removes the account.
func (h *Harness) ReattributeToAnotherPSP(f SubscriptionFixture) uuid.UUID {
	h.t.Helper()
	pool := h.sharedPool()
	other := uuid.New()
	_, err := pool.Exec(h.ctx, `INSERT INTO openrails.psps (id, merchant_id, rail, environment, account_id, key, archived) VALUES ($1, $2, 'nmi', 'test', $3, 'nmi-remapped', true)`, other, f.Merchant.UUID(), "remapped-"+other.String()[:12])
	require.NoError(h.t, err)
	t := h.t
	t.Cleanup(func() {
		_, err := pool.Exec(context.Background(), `DELETE FROM openrails.psps WHERE id = $1`, other)
		assertNoCleanupError(t, err, "delete remapped psp")
	})
	_, err = pool.Exec(h.ctx, `UPDATE openrails.payment_methods SET psp_id = $2 WHERE id = $1`, f.Method, other)
	require.NoError(h.t, err)
	t.Cleanup(func() {
		_, err := pool.Exec(context.Background(), `UPDATE openrails.payment_methods SET psp_id = $2 WHERE id = $1`, f.Method, f.PSP)
		assertNoCleanupError(t, err, "restore payment method psp")
	})
	return other
}

// AgePastDunningWindow moves the missed renewal older than the monthly
// dunning window (#839).
func (h *Harness) AgePastDunningWindow(f SubscriptionFixture) {
	h.t.Helper()
	_, err := h.sharedPool().Exec(h.ctx, `UPDATE openrails.subscriptions SET current_period_starts_at = now() - interval '50 days', current_period_ends_at = now() - interval '20 days' WHERE id = $1`, f.Subscription)
	require.NoError(h.t, err)
}

// ScheduleDunningIn puts the subscription's scheduled retry at now+in on the
// database clock (in <= 0 makes it due). The schedule is only a schedule: a
// live attempt claim is a separate column.
func (h *Harness) ScheduleDunningIn(f SubscriptionFixture, in time.Duration) {
	h.t.Helper()
	_, err := h.sharedPool().Exec(h.ctx, `UPDATE openrails.subscriptions SET last_retry_at = now() - interval '2 hours', next_retry_at = now() + make_interval(secs => $2) WHERE id = $1`, f.Subscription, in.Seconds())
	require.NoError(h.t, err)
}

// DunningClaim is the subscription's live attempt claim, if any.
func (h *Harness) DunningClaim(id uuid.UUID) (holder string, live bool) {
	h.t.Helper()
	var who *string
	var until *time.Time
	var now time.Time
	require.NoError(h.t, h.sharedPool().QueryRow(h.ctx, `SELECT dunning_claim_holder, dunning_claimed_until, now() FROM openrails.subscriptions WHERE id = $1`, id).Scan(&who, &until, &now))
	if who == nil || until == nil {
		return "", false
	}
	return *who, until.After(now)
}

// FireDunning inserts the dunning job the four-hourly schedule inserts and
// returns its id, so a test can wait for that exact pass to finish.
func (h *Harness) FireDunning(pool *pgxpool.Pool) int64 {
	h.t.Helper()
	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{})
	require.NoError(h.t, err)
	res, err := client.Insert(h.ctx, riverjobs.DunningArgs{}, &river.InsertOpts{Queue: riverjobs.QueueBilling})
	require.NoError(h.t, err)
	return res.Job.ID
}

// WaitForRiverJob blocks until the job reaches a terminal state and returns it.
func (h *Harness) WaitForRiverJob(id int64) string {
	h.t.Helper()
	var state string
	require.Eventually(h.t, func() bool {
		if err := h.sharedPool().QueryRow(h.ctx, `SELECT state::text FROM public.river_job WHERE id = $1`, id).Scan(&state); err != nil {
			return false
		}
		return state == "completed" || state == "discarded" || state == "cancelled"
	}, 90*time.Second, 250*time.Millisecond, "dunning pass %d finished (last state %s)", id, state)
	return state
}

// OperationEvidence is the operation's retained result evidence.
func (h *Harness) OperationEvidence(id uuid.UUID) string {
	h.t.Helper()
	var evidence *string
	require.NoError(h.t, h.sharedPool().QueryRow(h.ctx, `SELECT result_evidence::text FROM openrails.rail_intents WHERE id = $1`, id).Scan(&evidence))
	if evidence == nil {
		return ""
	}
	return *evidence
}

// OperationExpiry is the operation's relevance window end.
func (h *Harness) OperationExpiry(id uuid.UUID) *time.Time {
	h.t.Helper()
	var expires *time.Time
	require.NoError(h.t, h.sharedPool().QueryRow(h.ctx, `SELECT expires_at FROM openrails.rail_intents WHERE id = $1`, id).Scan(&expires))
	return expires
}

// SeedAnotherInvoice issues a second past-due invoice for the fixture's
// payer, for the period after its first invoice.
func (h *Harness) SeedAnotherInvoice(rt *app.Runtime, f CollectionFixture, amount int64) uuid.UUID {
	h.t.Helper()
	var from time.Time
	require.NoError(h.t, h.sharedPool().QueryRow(h.ctx, `SELECT period_to FROM openrails.invoices WHERE id = $1`, f.Invoice).Scan(&from))
	payer := identity.CustomerID(f.Customer)
	var invoice uuid.UUID
	require.NoError(h.t, rt.DB.RunInMerchantConn(merchant.WithID(h.ctx, f.Merchant), func(c context.Context) error {
		if _, err := rt.MoneyService.AccrueOwed(c, payer, f.Currency, "workflow", uuid.NewString(), amount); err != nil {
			return err
		}
		issued, err := rt.MoneyService.FinalizeInvoice(c, payer, f.Currency, from, time.Now())
		if err != nil {
			return err
		}
		invoice = issued.ID
		_, err = rt.MoneyService.MarkInvoicesPastDue(c, time.Now().Add(31*24*time.Hour))
		return err
	}))
	return invoice
}

// FailPeriod puts a renewed subscription into its NEXT period's first dunning
// failure: past due at that period's end, one recorded attempt, a schedule
// days out. It returns the new period end.
func (h *Harness) FailPeriod(f SubscriptionFixture) time.Time {
	h.t.Helper()
	var periodEnd time.Time
	require.NoError(h.t, h.sharedPool().QueryRow(h.ctx, `
		UPDATE openrails.subscriptions
		SET status = 'past_due',
		    current_period_starts_at = now() - interval '30 days',
		    current_period_ends_at = now() - interval '1 hour',
		    retry_attempts = 1,
		    last_retry_at = now() - interval '1 hour',
		    next_retry_at = now() + interval '2 days',
		    dunning_claim_holder = NULL,
		    dunning_claimed_until = NULL,
		    updated_at = now()
		WHERE id = $1
		RETURNING current_period_ends_at`, f.Subscription).Scan(&periodEnd))
	return periodEnd
}

// salesForOrder is every sale the gateway recorded for one order reference.
func salesForOrder(g *FakeNMIGateway, orderID string) []NMISale {
	var out []NMISale
	for _, sale := range g.Sales() {
		if sale.OrderID == orderID {
			out = append(out, sale)
		}
	}
	return out
}

// FlipCustody moves the instrument to a custodian the way the #297 remap
// does: the provider account and the old vault reference stay, custody,
// rail_method_ref and charge_via move. A rebill cannot be sent on it.
func (h *Harness) FlipCustody(f SubscriptionFixture) {
	h.t.Helper()
	pool := h.sharedPool()
	custodian := dbtest.EnsureTestCustodian(h.ctx, h.t, pool, f.Merchant.UUID())
	_, err := pool.Exec(h.ctx, `
		UPDATE openrails.payment_methods
		   SET custodian = 'basis_theory', custodian_id = $2, rail_method_ref = $3, charge_via = 'pan_proxy'
		 WHERE id = $1`, f.Method, custodian, "bt-token-"+uuid.NewString()[:8])
	require.NoError(h.t, err)
	t := h.t
	t.Cleanup(func() {
		_, err := pool.Exec(context.Background(), `UPDATE openrails.payment_methods SET custodian = 'psp', custodian_id = NULL WHERE id = $1`, f.Method)
		assertNoCleanupError(t, err, "restore custody")
	})
}
