//go:build integration

package intents

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/pkg/merchant"
)

// fakeNMISwapGateway scripts the two surfaces the nmi_payment_source_update
// intent touches: the v5 subscription GET (the verify/read-first leg, answering
// the recurring record's CURRENT vault) and the classic direct-post
// recurring=update_subscription write.
type fakeNMISwapGateway struct {
	railSubID string

	vault       atomic.Value // vault id NMI currently bills
	updateMode  atomic.Value // "ok" | "ambiguous_landed" | "ambiguous_lost" | "rejected"
	getCalls    atomic.Int64
	updateCalls atomic.Int64
}

func newFakeNMISwapGateway(t *testing.T, railSubID, initialVault string) (*fakeNMISwapGateway, *nmi.NMIClient) {
	t.Helper()
	f := &fakeNMISwapGateway{railSubID: railSubID}
	f.vault.Store(initialVault)
	f.updateMode.Store("ok")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/subscriptions/"):
			f.getCalls.Add(1)
			fmt.Fprintf(w, `{"object":"subscription","id":"%s","customer_vault_id":"%s","delayed_condition":"active"}`,
				f.railSubID, f.vault.Load().(string))
		case r.Method == http.MethodPost:
			_ = r.ParseForm()
			if r.PostFormValue("recurring") != "update_subscription" {
				fmt.Fprint(w, "response=1")
				return
			}
			f.updateCalls.Add(1)
			switch f.updateMode.Load().(string) {
			case "ambiguous_landed":
				// The update reached NMI, but the response is lost.
				f.vault.Store(r.PostFormValue("customer_vault_id"))
				w.WriteHeader(http.StatusBadGateway)
			case "ambiguous_lost":
				w.WriteHeader(http.StatusBadGateway)
			case "rejected":
				// Parsed clean refusal: NMI understood and said no.
				fmt.Fprint(w, "response=3&responsetext=Invalid Customer Vault Id")
			default:
				f.vault.Store(r.PostFormValue("customer_vault_id"))
				fmt.Fprint(w, "response=1&responsetext=SUCCESS")
			}
		default:
			fmt.Fprint(w, `{}`)
		}
	}))
	t.Cleanup(srv.Close)

	client, err := nmi.NewClient("mobius", &config.NMIProviderSettings{
		SecurityKey: "test_security_key", WebhookSecret: "test_secret",
	}, true)
	require.NoError(t, err)
	client.V5BaseURL = srv.URL
	client.QueryURL = srv.URL
	client.DirectPostURL = srv.URL
	return f, client
}

type paymentSourceSwapFixture struct {
	db      *db.DB
	runner  *Runner
	gateway *fakeNMISwapGateway
	through *PaymentSourceUpdateThrough
	sub     *models.Subscription
	oldPM   *models.PaymentMethod
	newPM   *models.PaymentMethod
	ctx     context.Context
}

func newPaymentSourceSwapFixture(t *testing.T) *paymentSourceSwapFixture {
	t.Helper()
	dsn := dbtest.MerchantPinnedDSN(t, dbtest.TestMerchantID.UUID())
	dbi := dbtest.OpenAppDB(t, dsn)
	pool := dbi.Pool()
	dbtest.EnsureTestMerchant(context.Background(), t, pool)
	ctx := merchant.WithID(context.Background(), dbtest.TestMerchantID)

	userID := uuid.New().String()
	customerID := dbtest.EnsureCustomerIDPgx(ctx, t, pool, userID)
	sfx := uuid.NewString()[:8]

	mkPM := func(vaultID string) *models.PaymentMethod {
		pm := &models.PaymentMethod{
			ID:                   uuid.New(),
			CustomerID:           customerID,
			Rail:                 models.RailNMI,
			RailCustomerRef:      vaultID,
			RailMethodRef:        "bill-" + uuid.NewString()[:8],
			RebillDriver:         models.RebillDriverProvider,
			InitialTransactionID: "txn-" + uuid.NewString()[:8],
			CreatedAt:            time.Now().UTC(),
			UpdatedAt:            time.Now().UTC(),
		}
		require.NoError(t, paymentmethods.NewPaymentMethodRepo(dbi).Create(ctx, pm))
		return pm
	}
	oldPM := mkPM("vault-old-" + sfx)
	newPM := mkPM("vault-new-" + sfx)

	now := time.Now().UTC()
	productID, priceID, subID := uuid.New(), uuid.New(), uuid.New()
	railSubID := "psid-swap-" + sfx
	exec := func(sql string, args ...any) {
		t.Helper()
		_, err := pool.Exec(ctx, sql, args...)
		require.NoError(t, err)
	}
	exec(`INSERT INTO openrails.products (id, key, display_name, merchant_id) VALUES ($1, $2, $2, $3)`,
		productID, "swap-prod-"+sfx, dbtest.TestMerchantID.UUID())
	exec(`INSERT INTO openrails.prices (id, product_id, amount, currency, access_duration_hours, auto_renew, merchant_id)
	      VALUES ($1, $2, 999, 'USD', 720, true, $3)`, priceID, productID, dbtest.TestMerchantID.UUID())
	exec(`INSERT INTO openrails.subscriptions
	        (id, price_id, product_id, status, rail, rail_subscription_id,
	         current_period_starts_at, current_period_ends_at, started_at,
	         payment_method_id, customer_id, merchant_id)
	      VALUES ($1, $2, $3, 'active', 'mobius', $4, $5, $6, $5, $7, $8, $9)`,
		subID, priceID, productID, railSubID,
		now.Add(-time.Hour), now.Add(720*time.Hour), oldPM.ID, customerID, dbtest.TestMerchantID.UUID())
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.rail_intents WHERE intent_type = $1 AND subscription_id = $2", TypeNMIPaymentSourceUpdate, subID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.subscriptions WHERE id = $1", subID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.prices WHERE id = $1", priceID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.products WHERE id = $1", productID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.payment_methods WHERE id = ANY($1)", []uuid.UUID{oldPM.ID, newPM.ID})
	})

	gateway, client := newFakeNMISwapGateway(t, railSubID, oldPM.RailCustomerRef)
	runner := &Runner{
		Store:    NewStore(dbi),
		Registry: NewRegistry(NewNMIPaymentSourceUpdateHandler(dbi, fakeNMIResolver{client: client}, nil)),
	}
	sub, err := subscriptions.NewSubscriptionRepo(dbi).GetByID(ctx, subID)
	require.NoError(t, err)

	return &paymentSourceSwapFixture{
		db:      dbi,
		runner:  runner,
		gateway: gateway,
		through: &PaymentSourceUpdateThrough{Runner: runner, DB: dbi},
		sub:     sub,
		oldPM:   oldPM,
		newPM:   newPM,
		ctx:     ctx,
	}
}

// localPaymentMethodID reads the subscription row's CURRENT payment-method
// link (set only by the intent's finalize, after provider confirmation).
func (fx *paymentSourceSwapFixture) localPaymentMethodID(t *testing.T) uuid.UUID {
	t.Helper()
	sub, err := subscriptions.NewSubscriptionRepo(fx.db).GetByID(fx.ctx, fx.sub.ID)
	require.NoError(t, err)
	require.NotNil(t, sub.PaymentMethodID)
	return *sub.PaymentMethodID
}

// latestIntent returns (status, count) of swap intents for the fixture's
// subscription, status being the most recent row's.
func (fx *paymentSourceSwapFixture) latestIntent(t *testing.T) (string, int) {
	t.Helper()
	rows, err := fx.db.Pool().Query(fx.ctx,
		"SELECT status FROM openrails.rail_intents WHERE intent_type = $1 AND subscription_id = $2 ORDER BY created_at DESC, id",
		TypeNMIPaymentSourceUpdate, fx.sub.ID)
	require.NoError(t, err)
	defer rows.Close()
	var statuses []string
	for rows.Next() {
		var s string
		require.NoError(t, rows.Scan(&s))
		statuses = append(statuses, s)
	}
	require.NotEmpty(t, statuses, "intent row must exist")
	return statuses[0], len(statuses)
}

func (fx *paymentSourceSwapFixture) advanceClock(d time.Duration) {
	fx.runner.Clock = clockwork.NewFakeClockAt(time.Now().UTC().Add(d))
}

// swapTo re-reads the subscription (the producer derives the old side from the
// CURRENT row, like both real call sites do) and executes the write-through.
func (fx *paymentSourceSwapFixture) swapTo(t *testing.T, pm *models.PaymentMethod) PaymentSourceUpdateOutcome {
	t.Helper()
	sub, err := subscriptions.NewSubscriptionRepo(fx.db).GetByID(fx.ctx, fx.sub.ID)
	require.NoError(t, err)
	out, err := fx.through.ExecutePaymentSourceUpdate(fx.ctx, sub, pm, OriginUser, "integration-test payment-method swap")
	require.NoError(t, err)
	return out
}

// Happy path: durable intent, one provider write, provider bills the new vault
// AND the local row points at the new method only after that confirmation; a
// replayed request never re-sends the update (the read-first execute verifies
// the provider already bills the new vault — zero write calls).
func TestNMIPaymentSourceUpdateIntent_WriteThroughHappyPathAndReplay(t *testing.T) {
	fx := newPaymentSourceSwapFixture(t)

	out := fx.swapTo(t, fx.newPM)
	require.True(t, out.Done, "inline execution must confirm the swap (reason=%s)", out.Reason)
	require.EqualValues(t, 1, fx.gateway.updateCalls.Load())
	require.Equal(t, fx.newPM.RailCustomerRef, fx.gateway.vault.Load().(string), "provider bills the new vault")
	require.Equal(t, fx.newPM.ID, fx.localPaymentMethodID(t), "local row finalized onto the new method")
	status, _ := fx.latestIntent(t)
	require.Equal(t, StatusSucceeded, status)

	writesBefore := fx.gateway.updateCalls.Load()
	replay := fx.swapTo(t, fx.newPM)
	require.True(t, replay.Done)
	require.Equal(t, writesBefore, fx.gateway.updateCalls.Load(), "replay never re-sends the update")
	require.Equal(t, fx.newPM.ID, fx.localPaymentMethodID(t))
}

// Ambiguous timeout where the update actually LANDED at NMI: the caller gets
// processing (never success, never decline), the local row keeps the OLD link
// until the provider is confirmed, a retried request maps onto the SAME intent
// with zero provider calls, and the verifier's read (current vault == new)
// completes the intent and converges the local row. Exactly one write ever.
func TestNMIPaymentSourceUpdateIntent_AmbiguousLanded_VerifierResolves(t *testing.T) {
	fx := newPaymentSourceSwapFixture(t)
	fx.gateway.updateMode.Store("ambiguous_landed")

	out := fx.swapTo(t, fx.newPM)
	require.False(t, out.Done, "ambiguity is never success")
	require.False(t, out.Terminal, "ambiguity is never a decline")
	require.EqualValues(t, 1, fx.gateway.updateCalls.Load())
	require.Equal(t, fx.oldPM.ID, fx.localPaymentMethodID(t), "local row untouched until the provider is confirmed")
	status, n := fx.latestIntent(t)
	require.Equal(t, StatusUnknownNeedsVerify, status)
	require.Equal(t, 1, n)

	// Retry while unresolved: same idempotency key, same intent, zero calls.
	writes, reads := fx.gateway.updateCalls.Load(), fx.gateway.getCalls.Load()
	retry := fx.swapTo(t, fx.newPM)
	require.False(t, retry.Done)
	require.False(t, retry.Terminal)
	_, n = fx.latestIntent(t)
	require.Equal(t, 1, n, "retry maps onto the same durable intent")
	require.Equal(t, writes, fx.gateway.updateCalls.Load())
	require.Equal(t, reads, fx.gateway.getCalls.Load())

	fx.advanceClock(2 * time.Minute)
	_, err := fx.runner.RunVerifyOnce(fx.ctx)
	require.NoError(t, err)
	status, _ = fx.latestIntent(t)
	require.Equal(t, StatusSucceeded, status)
	require.Equal(t, fx.newPM.ID, fx.localPaymentMethodID(t), "verifier finalized the local row off the provider read")
	require.EqualValues(t, 1, fx.gateway.updateCalls.Load(), "verification never re-sends")
}

// Ambiguous timeout where the update did NOT land: verify reads the OLD vault
// ("verified not executed"), the scheduled executor re-sends, and local+remote
// end consistent on the new instrument. Effectively-once, never a silent split.
func TestNMIPaymentSourceUpdateIntent_AmbiguousLost_ExecutorRetries(t *testing.T) {
	fx := newPaymentSourceSwapFixture(t)
	fx.gateway.updateMode.Store("ambiguous_lost")

	out := fx.swapTo(t, fx.newPM)
	require.False(t, out.Done)
	require.False(t, out.Terminal)
	status, _ := fx.latestIntent(t)
	require.Equal(t, StatusUnknownNeedsVerify, status)

	// Verify: provider still bills the old vault ⇒ verified not executed.
	fx.advanceClock(2 * time.Minute)
	_, err := fx.runner.RunVerifyOnce(fx.ctx)
	require.NoError(t, err)
	status, _ = fx.latestIntent(t)
	require.Equal(t, StatusFailedRetryable, status)
	require.Equal(t, fx.oldPM.ID, fx.localPaymentMethodID(t), "local row stays on the old method while the provider does")

	// Provider recovers; the scheduled executor finishes the SAME intent.
	fx.gateway.updateMode.Store("ok")
	fx.advanceClock(15 * time.Minute)
	_, err = fx.runner.RunExecuteOnce(fx.ctx)
	require.NoError(t, err)
	status, n := fx.latestIntent(t)
	require.Equal(t, StatusSucceeded, status)
	require.Equal(t, 1, n)
	require.EqualValues(t, 2, fx.gateway.updateCalls.Load(), "one lost attempt + one effective update")
	require.Equal(t, fx.newPM.RailCustomerRef, fx.gateway.vault.Load().(string))
	require.Equal(t, fx.newPM.ID, fx.localPaymentMethodID(t))
}

// Crash-before-execute: the intent was durably enqueued but the process died
// before inline execution. The scheduled executor drains it — the swap is
// never lost, exactly one provider write, local and remote consistent.
func TestNMIPaymentSourceUpdateIntent_CrashBeforeExecute(t *testing.T) {
	fx := newPaymentSourceSwapFixture(t)

	oldID := fx.oldPM.ID
	subID := fx.sub.ID
	_, err := NewStore(fx.db).Enqueue(fx.ctx, EnqueueParams{
		MerchantID:     dbtest.TestMerchantID.UUID(),
		Provider:       "mobius",
		IntentType:     TypeNMIPaymentSourceUpdate,
		SubscriptionID: &subID,
		Payload: NMIPaymentSourceUpdatePayload{
			UserID:             fx.sub.CustomerID.String(),
			RailSubscriptionID: fx.sub.RailSubscriptionID,
			NewPaymentMethodID: fx.newPM.ID,
			NewVaultID:         fx.newPM.RailCustomerRef,
			OldPaymentMethodID: &oldID,
			OldVaultID:         fx.oldPM.RailCustomerRef,
		},
		IdempotencyKey: NMIPaymentSourceUpdateIdempotencyKey(subID, fx.newPM.RailCustomerRef, 0),
		NextAttemptAt:  time.Now().UTC().Add(-time.Minute),
		Origin:         OriginUser,
		OriginReason:   "crash-before-execute test",
	})
	require.NoError(t, err)
	require.EqualValues(t, 0, fx.gateway.updateCalls.Load(), "nothing executed yet")

	_, err = fx.runner.RunExecuteOnce(fx.ctx)
	require.NoError(t, err)
	status, _ := fx.latestIntent(t)
	require.Equal(t, StatusSucceeded, status)
	require.EqualValues(t, 1, fx.gateway.updateCalls.Load())
	require.Equal(t, fx.newPM.RailCustomerRef, fx.gateway.vault.Load().(string))
	require.Equal(t, fx.newPM.ID, fx.localPaymentMethodID(t))
}

// Swap-back cycle (A→B, B→A, A→B): the attempt-count idempotency key means a
// repeat of a PREVIOUSLY completed swap is a fresh intent that genuinely
// executes — never falsely answered "done" from an old succeeded tombstone.
func TestNMIPaymentSourceUpdateIntent_RepeatSwapCycleReExecutes(t *testing.T) {
	fx := newPaymentSourceSwapFixture(t)

	require.True(t, fx.swapTo(t, fx.newPM).Done)
	require.True(t, fx.swapTo(t, fx.oldPM).Done)
	require.Equal(t, fx.oldPM.RailCustomerRef, fx.gateway.vault.Load().(string))

	out := fx.swapTo(t, fx.newPM) // repeat of swap #1: same sub, same target vault
	require.True(t, out.Done)
	require.EqualValues(t, 3, fx.gateway.updateCalls.Load(), "the repeated swap must actually execute")
	require.Equal(t, fx.newPM.RailCustomerRef, fx.gateway.vault.Load().(string))
	require.Equal(t, fx.newPM.ID, fx.localPaymentMethodID(t))
	_, n := fx.latestIntent(t)
	require.Equal(t, 3, n, "three distinct durable intents, one per logical swap")
}

// A parsed clean rejection is TERMINAL: the user gets an immediate failure and
// the sweeper never re-pushes a definite refusal (Paul 2026-07-02 — only
// ambiguity earns system-driven repair). Local row untouched.
func TestNMIPaymentSourceUpdateIntent_CleanRejectionIsTerminal(t *testing.T) {
	fx := newPaymentSourceSwapFixture(t)
	fx.gateway.updateMode.Store("rejected")

	out := fx.swapTo(t, fx.newPM)
	require.False(t, out.Done)
	require.True(t, out.Terminal, "clean refusal must surface as an immediate terminal failure")
	status, _ := fx.latestIntent(t)
	require.Equal(t, StatusFailedTerminal, status)
	require.Equal(t, fx.oldPM.ID, fx.localPaymentMethodID(t), "local row must stay on the old method")

	// The sweeper must NOT resurrect a definite refusal.
	fx.advanceClock(2 * time.Minute)
	_, err := fx.runner.RunExecuteOnce(fx.ctx)
	require.NoError(t, err)
	before := fx.gateway.updateCalls.Load()
	fx.advanceClock(10 * time.Minute)
	_, err = fx.runner.RunExecuteOnce(fx.ctx)
	require.NoError(t, err)
	require.Equal(t, before, fx.gateway.updateCalls.Load(), "no background re-push after terminal rejection")
}
