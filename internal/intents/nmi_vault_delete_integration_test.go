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
	"github.com/open-rails/openrails/pkg/merchant"
)

// fakeNMIVaultGateway scripts the two surfaces the nmi_vault_delete intent
// touches: the id-filtered v5 customer roster (the verify read) and the v5
// customer / billing-entry DELETE.
type fakeNMIVaultGateway struct {
	vaultID   string
	billingID string

	present          atomic.Bool  // customer exists at NMI
	deleteMode       atomic.Value // "ok" | "ambiguous500"
	listCalls        atomic.Int64
	vaultDeleteCalls atomic.Int64
	entryDeleteCalls atomic.Int64
}

func newFakeNMIVaultGateway(t *testing.T, vaultID, billingID string) (*fakeNMIVaultGateway, *nmi.NMIClient) {
	t.Helper()
	f := &fakeNMIVaultGateway{vaultID: vaultID, billingID: billingID}
	f.present.Store(true)
	f.deleteMode.Store("ok")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/customers"):
			f.listCalls.Add(1)
			if f.present.Load() && r.URL.Query().Get("id") == f.vaultID {
				fmt.Fprintf(w, `{"customers":[{"object":"customer","id":"%s","billing":[{"id":"%s","priority":1}]}],"next_cursor":null,"has_more":false}`,
					f.vaultID, f.billingID)
				return
			}
			fmt.Fprint(w, `{"customers":[],"next_cursor":null,"has_more":false}`)
		case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/billing/"):
			f.entryDeleteCalls.Add(1)
			if f.deleteMode.Load().(string) == "ambiguous500" {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			f.present.Store(false)
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodDelete:
			f.vaultDeleteCalls.Add(1)
			if f.deleteMode.Load().(string) == "ambiguous500" {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			f.present.Store(false)
			w.WriteHeader(http.StatusNoContent)
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

type fakeVaultClientResolver struct{ client *nmi.NMIClient }

func (f fakeVaultClientResolver) ResolveClientForPaymentMethod(context.Context, *models.PaymentMethod) (*nmi.NMIClient, error) {
	return f.client, nil
}

type vaultDeleteFixture struct {
	db      *db.DB
	runner  *Runner
	gateway *fakeNMIVaultGateway
	pm      *models.PaymentMethod
	ctx     context.Context
}

func newVaultDeleteFixture(t *testing.T) *vaultDeleteFixture {
	t.Helper()
	dsn := dbtest.MerchantPinnedDSN(t, dbtest.TestMerchantID.UUID())
	dbi := dbtest.OpenAppDB(t, dsn)
	pool := dbi.Pool()
	dbtest.EnsureTestMerchant(context.Background(), t, pool)
	ctx := merchant.WithID(context.Background(), dbtest.TestMerchantID)

	userID := uuid.New().String()
	customerID := dbtest.EnsureCustomerIDPgx(ctx, t, pool, userID)
	vaultID := "vault-del-" + uuid.NewString()[:8]
	billingID := "bill-del-" + uuid.NewString()[:8]

	pm := &models.PaymentMethod{
		ID:                   uuid.New(),
		CustomerID:           customerID,
		Rail:                 models.RailNMI,
		RailCustomerRef:      vaultID,
		RailMethodRef:        billingID,
		RebillDriver:         models.RebillDriverProvider,
		InitialTransactionID: "txn-" + uuid.NewString()[:8],
		CreatedAt:            time.Now().UTC(),
		UpdatedAt:            time.Now().UTC(),
	}
	require.NoError(t, paymentmethods.NewPaymentMethodRepo(dbi).Create(ctx, pm))
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.rail_intents WHERE intent_type = $1 AND idempotency_key = $2", TypeNMIVaultDelete, NMIVaultDeleteIdempotencyKey(pm.ID))
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.payment_methods WHERE id = $1", pm.ID)
	})

	gateway, client := newFakeNMIVaultGateway(t, vaultID, billingID)
	runner := &Runner{
		Store:    NewStore(dbi),
		Registry: NewRegistry(NewNMIVaultDeleteHandler(dbi, fakeVaultClientResolver{client: client})),
	}
	return &vaultDeleteFixture{db: dbi, runner: runner, gateway: gateway, pm: pm, ctx: ctx}
}

func (fx *vaultDeleteFixture) localRowExists(t *testing.T) bool {
	t.Helper()
	_, err := paymentmethods.NewPaymentMethodRepo(fx.db).GetByID(fx.ctx, fx.pm.ID)
	if err == nil {
		return true
	}
	require.ErrorIs(t, err, paymentmethods.ErrPaymentMethodNotFound)
	return false
}

func (fx *vaultDeleteFixture) intentStatus(t *testing.T) string {
	t.Helper()
	rows, err := fx.db.Pool().Query(fx.ctx,
		"SELECT status FROM openrails.rail_intents WHERE intent_type = $1 AND idempotency_key = $2",
		TypeNMIVaultDelete, NMIVaultDeleteIdempotencyKey(fx.pm.ID))
	require.NoError(t, err)
	defer rows.Close()
	var status string
	require.True(t, rows.Next(), "intent row must exist")
	require.NoError(t, rows.Scan(&status))
	return status
}

func (fx *vaultDeleteFixture) advanceClock(d time.Duration) {
	fx.runner.Clock = clockwork.NewFakeClockAt(time.Now().UTC().Add(d))
}

func (fx *vaultDeleteFixture) executeThrough(t *testing.T) paymentmethods.VaultDeleteOutcome {
	t.Helper()
	out, err := (&VaultDeleteThrough{Runner: fx.runner}).ExecuteVaultDelete(fx.ctx, fx.pm)
	require.NoError(t, err)
	return out
}

// Happy path: one durable intent, one remote DELETE, local row gone; a
// replayed delete answers from the durable tombstone with ZERO provider calls.
func TestNMIVaultDeleteIntent_WriteThroughHappyPathAndReplay(t *testing.T) {
	fx := newVaultDeleteFixture(t)

	out := fx.executeThrough(t)
	require.True(t, out.Done, "inline execution must confirm the delete (reason=%s)", out.Reason)
	require.EqualValues(t, 1, fx.gateway.vaultDeleteCalls.Load())
	require.False(t, fx.localRowExists(t), "local row removed by finalize")
	require.Equal(t, StatusSucceeded, fx.intentStatus(t))

	deletesBefore := fx.gateway.vaultDeleteCalls.Load()
	listsBefore := fx.gateway.listCalls.Load()
	replay := fx.executeThrough(t)
	require.True(t, replay.Done)
	require.Equal(t, deletesBefore, fx.gateway.vaultDeleteCalls.Load(), "replay never re-deletes")
	require.Equal(t, listsBefore, fx.gateway.listCalls.Load(), "replay never re-reads")
}

// Crash-before-execute: the intent was durably enqueued but the process died
// before inline execution. The scheduled executor drains it — exactly one
// external delete.
func TestNMIVaultDeleteIntent_CrashBeforeExecute(t *testing.T) {
	fx := newVaultDeleteFixture(t)

	_, err := NewStore(fx.db).Enqueue(fx.ctx, EnqueueParams{
		MerchantID: dbtest.TestMerchantID.UUID(),
		Provider:   "mobius",
		IntentType: TypeNMIVaultDelete,
		Payload: NMIVaultDeletePayload{
			UserID:          fx.pm.CustomerID.String(),
			PaymentMethodID: fx.pm.ID,
			RailCustomerRef: fx.pm.RailCustomerRef,
			RailMethodRef:   fx.pm.RailMethodRef,
		},
		IdempotencyKey: NMIVaultDeleteIdempotencyKey(fx.pm.ID),
		NextAttemptAt:  time.Now().UTC().Add(-time.Minute),
		Origin:         OriginUser,
		OriginReason:   "crash-before-execute test",
	})
	require.NoError(t, err)
	require.EqualValues(t, 0, fx.gateway.vaultDeleteCalls.Load(), "nothing executed yet")

	_, err = fx.runner.RunExecuteOnce(fx.ctx)
	require.NoError(t, err)
	require.EqualValues(t, 1, fx.gateway.vaultDeleteCalls.Load())
	require.False(t, fx.localRowExists(t))
	require.Equal(t, StatusSucceeded, fx.intentStatus(t))
}

// Ambiguous timeout where the delete actually LANDED: never treated as a
// failure — the verifier's read sees the vault absent and finalizes off the
// original intent. Exactly one external delete attempt, local row removed.
func TestNMIVaultDeleteIntent_AmbiguousDeleteLanded_VerifierResolves(t *testing.T) {
	fx := newVaultDeleteFixture(t)
	fx.gateway.deleteMode.Store("ambiguous500")

	out := fx.executeThrough(t)
	require.False(t, out.Done)
	require.False(t, out.Terminal, "ambiguity is never a terminal failure")
	require.Equal(t, StatusUnknownNeedsVerify, fx.intentStatus(t))
	require.EqualValues(t, 1, fx.gateway.vaultDeleteCalls.Load())
	require.True(t, fx.localRowExists(t), "local row survives until the remote delete is confirmed")

	// The 502 hid a delete that actually landed.
	fx.gateway.present.Store(false)

	fx.advanceClock(2 * time.Minute)
	_, err := fx.runner.RunVerifyOnce(fx.ctx)
	require.NoError(t, err)
	require.Equal(t, StatusSucceeded, fx.intentStatus(t))
	require.False(t, fx.localRowExists(t))
	require.EqualValues(t, 1, fx.gateway.vaultDeleteCalls.Load(), "verification never re-deletes")
}

// Ambiguous timeout where the delete did NOT land: verify answers "still
// present ⇒ not executed", the executor re-sends, and the delete lands.
// Effectively-once: the vault ends deleted exactly once, never lost.
func TestNMIVaultDeleteIntent_AmbiguousNotExecuted_ExecutorRetries(t *testing.T) {
	fx := newVaultDeleteFixture(t)
	fx.gateway.deleteMode.Store("ambiguous500")

	out := fx.executeThrough(t)
	require.False(t, out.Done)
	require.False(t, out.Terminal)
	require.Equal(t, StatusUnknownNeedsVerify, fx.intentStatus(t))

	// Verify: vault still present ⇒ verified not executed ⇒ retryable.
	fx.advanceClock(2 * time.Minute)
	_, err := fx.runner.RunVerifyOnce(fx.ctx)
	require.NoError(t, err)
	require.Equal(t, StatusFailedRetryable, fx.intentStatus(t))
	require.True(t, fx.localRowExists(t))

	// Provider recovers; the scheduled executor finishes the SAME intent.
	fx.gateway.deleteMode.Store("ok")
	fx.advanceClock(15 * time.Minute)
	_, err = fx.runner.RunExecuteOnce(fx.ctx)
	require.NoError(t, err)
	require.Equal(t, StatusSucceeded, fx.intentStatus(t))
	require.False(t, fx.localRowExists(t))
	require.EqualValues(t, 2, fx.gateway.vaultDeleteCalls.Load(), "one lost attempt + one effective delete")
}

// Shared vault (#682) through the intent path: the handler recomputes
// sharedness at execution time and deletes ONLY this row's billing entry —
// the sibling card's vault survives.
func TestNMIVaultDeleteIntent_SharedVaultScopesToBillingEntry(t *testing.T) {
	fx := newVaultDeleteFixture(t)

	sibling := &models.PaymentMethod{
		ID:                   uuid.New(),
		CustomerID:           fx.pm.CustomerID,
		Rail:                 models.RailNMI,
		RailCustomerRef:      fx.pm.RailCustomerRef, // shares the vault
		RailMethodRef:        "bill-sib-" + uuid.NewString()[:8],
		RebillDriver:         models.RebillDriverProvider,
		InitialTransactionID: "txn-" + uuid.NewString()[:8],
		CreatedAt:            time.Now().UTC(),
		UpdatedAt:            time.Now().UTC(),
	}
	require.NoError(t, paymentmethods.NewPaymentMethodRepo(fx.db).Create(fx.ctx, sibling))
	t.Cleanup(func() {
		_, _ = fx.db.Pool().Exec(fx.ctx, "DELETE FROM openrails.payment_methods WHERE id = $1", sibling.ID)
	})

	out := fx.executeThrough(t)
	require.True(t, out.Done, "reason=%s", out.Reason)
	require.EqualValues(t, 1, fx.gateway.entryDeleteCalls.Load(), "exactly one billing-entry delete")
	require.EqualValues(t, 0, fx.gateway.vaultDeleteCalls.Load(), "the shared vault itself must NEVER be deleted")
	require.False(t, fx.localRowExists(t))
	_, err := paymentmethods.NewPaymentMethodRepo(fx.db).GetByID(fx.ctx, sibling.ID)
	require.NoError(t, err, "sibling row must survive")
}

// Relevance guard: a payment method that went back into use by an active
// subscription between enqueue and execute supersedes the delete — billing
// state in use is never destroyed.
func TestNMIVaultDeleteIntent_SupersededWhenBackInUse(t *testing.T) {
	fx := newVaultDeleteFixture(t)
	pool := fx.db.Pool()

	// Durable enqueue (simulating a crash before inline execution), then the
	// method goes back into use before the executor drains it.
	_, err := NewStore(fx.db).Enqueue(fx.ctx, EnqueueParams{
		MerchantID: dbtest.TestMerchantID.UUID(),
		Provider:   "mobius",
		IntentType: TypeNMIVaultDelete,
		Payload: NMIVaultDeletePayload{
			UserID:          fx.pm.CustomerID.String(),
			PaymentMethodID: fx.pm.ID,
			RailCustomerRef: fx.pm.RailCustomerRef,
			RailMethodRef:   fx.pm.RailMethodRef,
		},
		IdempotencyKey: NMIVaultDeleteIdempotencyKey(fx.pm.ID),
		NextAttemptAt:  time.Now().UTC().Add(-time.Minute),
		Origin:         OriginUser,
		OriginReason:   "superseded test",
	})
	require.NoError(t, err)

	now := time.Now().UTC()
	sfx := uuid.NewString()[:8]
	productID, priceID, subID := uuid.New(), uuid.New(), uuid.New()
	exec := func(sql string, args ...any) {
		t.Helper()
		_, err := pool.Exec(fx.ctx, sql, args...)
		require.NoError(t, err)
	}
	exec(`INSERT INTO openrails.products (id, key, display_name, merchant_id) VALUES ($1, $2, $2, $3)`,
		productID, "vdel-prod-"+sfx, dbtest.TestMerchantID.UUID())
	exec(`INSERT INTO openrails.prices (id, product_id, amount, currency, access_duration_hours, auto_renew, merchant_id)
	      VALUES ($1, $2, 999, 'USD', 720, true, $3)`, priceID, productID, dbtest.TestMerchantID.UUID())
	exec(`INSERT INTO openrails.subscriptions
	        (id, price_id, product_id, status, rail, rail_subscription_id,
	         current_period_starts_at, current_period_ends_at, started_at,
	         payment_method_id, customer_id, merchant_id)
	      VALUES ($1, $2, $3, 'active', 'mobius', $4, $5, $6, $5, $7, $8, $9)`,
		subID, priceID, productID, "psid-vdel-"+sfx,
		now.Add(-time.Hour), now.Add(720*time.Hour), fx.pm.ID, fx.pm.CustomerID, dbtest.TestMerchantID.UUID())
	t.Cleanup(func() {
		_, _ = pool.Exec(fx.ctx, "DELETE FROM openrails.subscriptions WHERE id = $1", subID)
		_, _ = pool.Exec(fx.ctx, "DELETE FROM openrails.prices WHERE id = $1", priceID)
		_, _ = pool.Exec(fx.ctx, "DELETE FROM openrails.products WHERE id = $1", productID)
	})

	_, err = fx.runner.RunExecuteOnce(fx.ctx)
	require.NoError(t, err)
	require.Equal(t, StatusSuperseded, fx.intentStatus(t))
	require.EqualValues(t, 0, fx.gateway.vaultDeleteCalls.Load(), "in-use billing state is never destroyed")
	require.EqualValues(t, 0, fx.gateway.entryDeleteCalls.Load())
	require.True(t, fx.localRowExists(t))
}
