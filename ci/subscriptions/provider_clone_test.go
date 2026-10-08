//go:build e2e && integration

package subscriptions_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/merchantarchive"
	"github.com/open-rails/openrails/internal/merchants"
)

// Separate databases matter here: schemas in one database can still share
// advisory locks. The only shared financial authority in these tests is the
// simulated provider, not a PostgreSQL row, lease or advisory lock.
func providerCopyDatabase(t *testing.T, adminDSN string) string {
	t.Helper()
	admin, err := pgx.Connect(t.Context(), adminDSN)
	require.NoError(t, err)
	name := "provider_copy_" + uuid.NewString()[:8]
	_, err = admin.Exec(t.Context(), "CREATE DATABASE "+pgx.Identifier{name}.Sanitize())
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, err := admin.Exec(ctx, "DROP DATABASE "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
		require.NoError(t, err)
		require.NoError(t, admin.Close(ctx))
	})
	u, err := url.Parse(adminDSN)
	require.NoError(t, err)
	u.Path = "/" + name
	return u.String()
}

func copiedStripeBook(t *testing.T) (*engineCase, *engineCase, *clockwork.FakeClock) {
	t.Helper()
	adminDSN := dsn(t)
	t.Setenv("OPENRAILS_E2E_DSN", providerCopyDatabase(t, adminDSN))
	a := newWorld(t)
	one := enroll(t, a, "stripe", embedded)
	a.settle()
	merchantID := a.client[embedded].MerchantID()
	for {
		events, err := a.client[embedded].ListHostEvents(t.Context(), billing.HostEventListParams{})
		require.NoError(t, err)
		if len(events.Items) == 0 {
			break
		}
		for _, event := range events.Items {
			_, err := a.client[embedded].AcknowledgeHostEvent(t.Context(), event.ID)
			require.NoError(t, err)
		}
	}
	a.stop()
	sourceDB, err := db.NewWithPGXPool(a.pool, a.schema)
	require.NoError(t, err)
	var archive bytes.Buffer
	err = merchantarchive.Export(t.Context(), sourceDB, merchantID, &archive)
	require.NoError(t, err, "copy source must export normally: %v", errors.Unwrap(err))

	t.Setenv("OPENRAILS_E2E_DSN", providerCopyDatabase(t, adminDSN))
	b := prepareWorld(t, 12)
	b.slug, b.auth, b.stripe, b.nmi = a.slug, a.auth, a.stripe, a.nmi
	b.clock = clockwork.NewFakeClockAt(a.clock.Now())
	targetDB, err := db.NewWithPGXPool(b.pool, b.schema)
	require.NoError(t, err)
	directory, err := merchants.NewDirectoryService(targetDB.DataPool())
	require.NoError(t, err)
	_, created, err := directory.RegisterForRestore(t.Context(), merchantID, b.slug)
	require.NoError(t, err)
	require.True(t, created)
	_, err = merchantarchive.Restore(t.Context(), targetDB, merchantID, bytes.NewReader(archive.Bytes()))
	require.NoError(t, err)
	var firstDB, secondDB string
	require.NoError(t, a.pool.QueryRow(t.Context(), "SELECT current_database()").Scan(&firstDB))
	require.NoError(t, b.pool.QueryRow(t.Context(), "SELECT current_database()").Scan(&secondDB))
	require.NotEqual(t, firstDB, secondDB)
	a.start()
	b.start()
	providerClock := clockwork.NewFakeClockAt(time.Now())
	a.stripe.setClock(providerClock.Now)
	two, customer := *one, *one.c
	two.w, customer.w = b, b
	two.c = &customer
	return one, &two, providerClock
}

func stripeIntentCreate(r *http.Request) bool {
	return r.Method == http.MethodPost && r.URL.Path == "/v1/payment_intents"
}

// A copied book admits the same renewal in two independent databases. The
// provider must see the same operation identity and execute it only once.
func TestCopiedStripeBooksRenewConcurrently(t *testing.T) {
	a, b, _ := copiedStripeBook(t)
	paidThrough := a.periodEnd()
	provider := a.w.stripe
	before := len(provider.submitted("/v1/payment_intents"))
	g := provider.hold(newGate(stripeIntentCreate, false))
	var release sync.Once
	unblock := func() { release.Do(func() { close(g.release) }) }
	t.Cleanup(func() { unblock(); provider.unhold() })
	a.toPeriodEnd()
	b.toPeriodEnd()
	var jobs sync.WaitGroup
	jobs.Go(a.w.runRenewals)
	select {
	case <-g.arrived:
	case <-time.After(15 * time.Second):
		t.Fatal("first renewal did not reach the provider")
	}
	jobs.Go(b.w.runRenewals)
	require.Eventually(t, func() bool { return len(provider.submitted("/v1/payment_intents")) >= before+2 }, 15*time.Second, 10*time.Millisecond,
		"both independent copies must submit while the first operation is in flight")
	unblock()
	jobs.Wait()
	provider.unhold()
	for _, e := range []*engineCase{a, b} {
		e.w.until(func() bool { return e.periodEnd().After(paidThrough) }, "the shared charge is adopted by this copy")
		require.Len(t, completed(e.w.payments(embedded, e.c.id)), 2)
	}
	require.Len(t, a.providerLedger(), 2, "one initial payment and one shared renewal")
	require.Equal(t, a.periodEnd(), b.periodEnd())
	var first, second string
	require.NoError(t, a.w.pool.QueryRow(t.Context(), a.w.q(`SELECT id::text FROM billing.provider_intents WHERE intent_type='subscription_collection' AND subscription_id=$1`), a.sub.UUID()).Scan(&first))
	require.NoError(t, b.w.pool.QueryRow(t.Context(), b.w.q(`SELECT id::text FROM billing.provider_intents WHERE intent_type='subscription_collection' AND subscription_id=$1`), b.sub.UUID()).Scan(&second))
	require.Equal(t, first, second, "obligation identity is independent of which database admitted it")
}

// The stale copy has never submitted its next renewal. A provider record for
// that obligation is still authoritative after the short-lived key expires.
func TestCopiedStripeBookReadsAgedObligationBeforeFirstSubmission(t *testing.T) {
	a, b, providerClock := copiedStripeBook(t)
	paidThrough := a.periodEnd()
	a.toPeriodEnd()
	a.w.runRenewals()
	require.True(t, a.periodEnd().After(paidThrough))
	before := len(a.w.stripe.submitted("/v1/payment_intents"))
	providerClock.Advance(25 * time.Hour)
	var operations int
	require.NoError(t, b.w.pool.QueryRow(t.Context(), b.w.q(`SELECT count(*) FROM billing.provider_intents WHERE intent_type='subscription_collection'`)).Scan(&operations))
	require.Zero(t, operations, "the stale copy has no local submission fence or accepted renewal")
	b.toPeriodEnd()
	b.w.runRenewals()
	b.w.until(func() bool { return b.periodEnd().After(paidThrough) }, "the stale copy adopts the persistent provider record")
	require.Equal(t, before, len(a.w.stripe.submitted("/v1/payment_intents")), "readback succeeds without any new POST after key expiry")
	require.Len(t, a.providerLedger(), 2)
	require.Equal(t, a.periodEnd(), b.periodEnd())
	require.Len(t, completed(b.w.payments(embedded, b.c.id)), 2)
}

func TestCopiedStripeBookRefusesConflictingRenewalTerms(t *testing.T) {
	a, b, _ := copiedStripeBook(t)
	paidThrough := a.periodEnd()
	a.toPeriodEnd()
	a.w.runRenewals()
	require.True(t, a.periodEnd().After(paidThrough))
	before := len(a.w.stripe.submitted("/v1/payment_intents"))
	// A restored database can independently author a different next price.
	// Use the normal immutable-price/versioning and scheduled-reprice APIs.
	client := b.w.client[embedded]
	old, err := client.GetPrice(t.Context(), pid(b.price), billing.GetPriceParams{})
	require.NoError(t, err)
	product, err := client.GetProduct(t.Context(), old.ProductID)
	require.NoError(t, err)
	_, err = client.CreatePrice(t.Context(), billing.CreatePriceParams{
		ProductID: old.ProductID, Key: old.Key, UnitAmount: old.UnitAmount + 10000,
		Currency: old.Currency, AutoRenew: true, AccessDurationHours: old.AccessDurationHours,
	})
	require.NoError(t, err)
	batch, err := client.CreateRepriceBatch(t.Context(), billing.CreateRepriceBatchParams{
		ProductKey: product.Key, PriceKey: old.Key, EffectiveAt: paidThrough, AcknowledgeShortNotice: true,
	})
	require.NoError(t, err)
	require.Len(t, batch.Scheduled, 1)
	b.toPeriodEnd()
	b.w.runRenewals()
	require.Equal(t, paidThrough, b.periodEnd(), "conflicting terms are not applied locally")
	require.Equal(t, before, len(a.w.stripe.submitted("/v1/payment_intents")), "reconcile the conflict; never try a different charge key")
	require.Len(t, a.providerLedger(), 2)
	require.Len(t, completed(b.w.payments(embedded, b.c.id)), 1)
	b.w.waive("recorded", "the divergent copy refuses the other database's paid renewal; the test asserts its unchanged period/payment count and no provider submission")
}

// A failed first attempt may legitimately be retried with a later attempt
// ordinal. Once that retry pays, an older copy must not charge attempt zero.
func TestCopiedStripeBookRefusesStaleAttemptAfterSuccessfulRetry(t *testing.T) {
	a, b, providerClock := copiedStripeBook(t)
	paidThrough := a.periodEnd()
	a.setDecline(visa.Last4, "insufficient_funds", "")
	a.toPeriodEnd()
	a.w.runRenewals()
	sub := a.w.subscription(embedded, a.sub)
	require.Equal(t, billing.SubscriptionPastDue, sub.Status)
	require.NotNil(t, sub.NextRetryAt)
	require.Equal(t, paidThrough, a.periodEnd())
	require.Len(t, a.providerLedger(), 1)
	firstAttempt := a.w.stripe.submitted("/v1/payment_intents")
	require.Len(t, firstAttempt, 2, "initial payment and declined renewal")
	a.setDecline(visa.Last4, "", "")
	advance := sub.NextRetryAt.Sub(a.w.clock.Now()) + time.Second
	a.w.advance(advance)
	providerClock.Advance(advance)
	a.w.runRenewals()
	require.True(t, a.periodEnd().After(paidThrough))
	require.Len(t, a.providerLedger(), 2)
	attempts := a.w.stripe.submitted("/v1/payment_intents")
	require.Len(t, attempts, 3, "the next eligible attempt is a distinct provider request")
	require.NotEqual(t, attempts[1].IdempotencyKey, attempts[2].IdempotencyKey)
	b.toPeriodEnd()
	b.w.runRenewals()
	require.Equal(t, paidThrough, b.periodEnd(), "the older attempt needs reconciliation with the later paid attempt")
	require.Len(t, a.w.stripe.submitted("/v1/payment_intents"), len(attempts))
	require.Len(t, a.providerLedger(), 2)
	b.w.waive("recorded", "the stale attempt is held for reconciliation after the other database's later attempt paid; provider and local period counts are asserted")
}

// The provider acts but the response is lost. After its key can expire, a
// temporarily empty read must not permit another charge. Once visible, the
// persistent operation record resolves both independently copied books.
func TestCopiedStripeBookRecoversAgedLostReplyWithoutResend(t *testing.T) {
	a, b, providerClock := copiedStripeBook(t)
	paidThrough := a.periodEnd()
	provider := a.w.stripe
	provider.delayIntentVisibility(48 * time.Hour)
	g := provider.hold(newGate(stripeIntentCreate, true))
	g.served = true
	var release sync.Once
	unblock := func() { release.Do(func() { close(g.release) }) }
	t.Cleanup(func() { unblock(); provider.unhold() })
	a.toPeriodEnd()
	_, err := a.w.jobs.Insert(t.Context(), dunningPass{}, &river.InsertOpts{Queue: openrails.QueueBilling})
	require.NoError(t, err)
	select {
	case <-g.arrived:
	case <-time.After(15 * time.Second):
		t.Fatal("renewal did not reach the provider")
	}
	require.Len(t, a.providerLedger(), 2, "the provider committed before the response was lost")
	a.w.stop()
	unblock()
	provider.unhold()
	before := len(provider.submitted("/v1/payment_intents"))
	providerClock.Advance(25 * time.Hour)
	a.w.advance(25 * time.Hour)
	a.w.start()
	a.w.wake()
	a.w.runRenewals()
	for range 3 {
		a.w.advance(time.Hour)
		a.w.wake()
	}
	require.Equal(t, paidThrough, a.periodEnd())
	require.Equal(t, before, len(provider.submitted("/v1/payment_intents")), "an aged uncertain submission cannot be re-sent on an empty read")
	require.Len(t, a.providerLedger(), 2)
	providerClock.Advance(24 * time.Hour)
	a.w.until(func() bool { return a.periodEnd().After(paidThrough) }, "the original provider record becomes visible")
	b.toPeriodEnd()
	b.w.runRenewals()
	b.w.until(func() bool { return b.periodEnd().After(paidThrough) }, "the stale copy also reads the persistent obligation")
	require.Equal(t, before, len(provider.submitted("/v1/payment_intents")))
	require.Len(t, a.providerLedger(), 2)
	require.Equal(t, a.periodEnd(), b.periodEnd())
}
