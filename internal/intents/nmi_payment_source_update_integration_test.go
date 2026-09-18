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

	"github.com/open-rails/openrails"
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
	pspID   uuid.UUID
	ctx     context.Context
}

func newPaymentSourceSwapFixture(t *testing.T) *paymentSourceSwapFixture {
	t.Helper()
	dbi := dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID())
	pool := dbi.Pool()
	dbtest.EnsureTestMerchant(context.Background(), t, pool)
	ctx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	pspID := dbtest.EnsureTestPSP(ctx, t, pool, dbtest.TestMerchantID.UUID(), "mobius")

	userID := uuid.New().String()
	customerID := dbtest.EnsureCustomerIDPgx(ctx, t, pool, userID)
	sfx := uuid.NewString()[:8]

	mkPM := func(railCustomerRef string) *models.PaymentMethod {
		pm := &models.PaymentMethod{
			ID:                   uuid.New(),
			CustomerID:           customerID,
			Rail:                 models.RailNMI,
			PspID:                pspID,
			RailCustomerRef:      railCustomerRef,
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
	         payment_method_id, customer_id, merchant_id, psp_id)
	      VALUES ($1, $2, $3, 'active', 'mobius', $4, $5, $6, $5, $7, $8, $9, $10)`,
		subID, priceID, productID, railSubID,
		now.Add(-time.Hour), now.Add(720*time.Hour), oldPM.ID, customerID, dbtest.TestMerchantID.UUID(), pspID)
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
		// or#865 made a nil ModeView fail CLOSED: without it every intent parks
		// rather than executing, so the fixture must state its mode.
		Config: fullModeConfig(),
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
		pspID:   pspID,
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

// otherPSP registers a second provider account on the merchant and returns
// its id; reattribute moves a method onto it the way a #297 custody remap's
// RemapPaymentMethodCustody does (psp_id only; the vault handle stays).
func (fx *paymentSourceSwapFixture) otherPSP(t *testing.T) uuid.UUID {
	t.Helper()
	id := dbtest.EnsureTestPSP(fx.ctx, t, fx.db.Pool(), dbtest.TestMerchantID.UUID(), "other-mobius-"+uuid.NewString()[:8])
	t.Cleanup(func() {
		_, _ = fx.db.Pool().Exec(fx.ctx, "UPDATE openrails.psps SET archived = true WHERE id = $1", id)
	})
	return id
}

func (fx *paymentSourceSwapFixture) reattribute(t *testing.T, pm *models.PaymentMethod, psp uuid.UUID) {
	t.Helper()
	_, err := fx.db.Pool().Exec(fx.ctx, "UPDATE openrails.payment_methods SET psp_id = $1 WHERE id = $2", psp, pm.ID)
	require.NoError(t, err)
	pm.PspID = psp
}

func (fx *paymentSourceSwapFixture) intentCount(t *testing.T) int {
	t.Helper()
	var n int
	require.NoError(t, fx.db.Pool().QueryRow(fx.ctx,
		"SELECT count(*) FROM openrails.rail_intents WHERE intent_type = $1 AND subscription_id = $2",
		TypeNMIPaymentSourceUpdate, fx.sub.ID).Scan(&n))
	return n
}

// latestIntentEvidenceCode reads result_evidence.code of the most recent swap
// intent for the fixture's subscription.
func (fx *paymentSourceSwapFixture) latestIntentEvidenceCode(t *testing.T) string {
	t.Helper()
	var code *string
	require.NoError(t, fx.db.Pool().QueryRow(fx.ctx,
		"SELECT result_evidence->>'code' FROM openrails.rail_intents WHERE intent_type = $1 AND subscription_id = $2 ORDER BY created_at DESC, id LIMIT 1",
		TypeNMIPaymentSourceUpdate, fx.sub.ID).Scan(&code))
	if code == nil {
		return ""
	}
	return *code
}

// Cross-account updates stop at the durable producer boundary: the target is
// re-read under its row lock, no intent row is created and the fake NMI
// receives no write — a target vault from another PSP can never be sent
// through the archived/source account's client (#657).
func TestNMIPaymentSourceUpdateIntent_CrossPSPRefusesBeforeProviderCall(t *testing.T) {
	fx := newPaymentSourceSwapFixture(t)
	fx.reattribute(t, fx.newPM, fx.otherPSP(t))

	sub, err := subscriptions.NewSubscriptionRepo(fx.db).GetByID(fx.ctx, fx.sub.ID)
	require.NoError(t, err)
	_, err = fx.through.ExecutePaymentSourceUpdate(fx.ctx, sub, fx.newPM, OriginUser, "cross-psp test")
	require.ErrorIs(t, err, subscriptions.ErrPaymentMethodProviderAccountMismatch)
	require.Zero(t, fx.gateway.updateCalls.Load(), "cross-PSP guard must run before any provider write")
	require.Zero(t, fx.intentCount(t), "a refused request leaves no durable intent behind")
}

func TestNMIPaymentSourceUpdateIntent_CustodianHeldTargetRefusesBeforeProviderCall(t *testing.T) {
	fx := newPaymentSourceSwapFixture(t)
	custodian := uuid.New()
	_, err := fx.db.Pool().Exec(fx.ctx, `INSERT INTO openrails.custodians(id,merchant_id,key,kind,account_id) VALUES($1,$2,$3,'basis_theory',$3)`, custodian, dbtest.TestMerchantID.UUID(), "source-update-"+custodian.String())
	require.NoError(t, err)
	_, err = fx.db.Pool().Exec(fx.ctx, `UPDATE openrails.payment_methods SET custodian='basis_theory',custodian_id=$2,rail_method_ref='custodian-token' WHERE id=$1`, fx.newPM.ID, custodian)
	require.NoError(t, err)
	// The PSP remains unchanged, and the old vault reference remains for
	// forensic correlation. It must never be used as a live NMI address.
	_, err = fx.through.ExecutePaymentSourceUpdate(fx.ctx, fx.sub, fx.newPM, OriginUser, "custodian-held target")
	require.Error(t, err)
	require.Zero(t, fx.gateway.updateCalls.Load())
	require.Zero(t, fx.gateway.getCalls.Load())
	require.Zero(t, fx.intentCount(t))
}

// The producer's check is made against the CURRENT row, not the caller's
// stale copy: a method re-attributed between the HTTP pre-check and the
// durable seam is refused with zero writes.
func TestNMIPaymentSourceUpdateIntent_ProducerRereadsTargetUnderLock(t *testing.T) {
	fx := newPaymentSourceSwapFixture(t)
	stale := *fx.newPM // what an HTTP handler read a moment ago: same PSP as the subscription
	fx.reattribute(t, fx.newPM, fx.otherPSP(t))

	sub, err := subscriptions.NewSubscriptionRepo(fx.db).GetByID(fx.ctx, fx.sub.ID)
	require.NoError(t, err)
	_, err = fx.through.ExecutePaymentSourceUpdate(fx.ctx, sub, &stale, OriginUser, "stale caller copy")
	require.ErrorIs(t, err, subscriptions.ErrPaymentMethodProviderAccountMismatch)
	require.Zero(t, fx.gateway.updateCalls.Load())
	require.Zero(t, fx.intentCount(t))
}

// A zero identifier is refused with the coded invalid-parameter envelope
// before the row lock: no provider traffic, no durable intent, never a
// "not found" lookup.
func TestNMIPaymentSourceUpdateIntent_ZeroIdentifiersRefusedBeforeAnyLookup(t *testing.T) {
	fx := newPaymentSourceSwapFixture(t)
	sub, err := subscriptions.NewSubscriptionRepo(fx.db).GetByID(fx.ctx, fx.sub.ID)
	require.NoError(t, err)

	tests := []struct {
		name   string
		param  string
		mutate func(*models.Subscription, *models.PaymentMethod)
	}{
		{"zero subscription id", "subscription_id", func(s *models.Subscription, _ *models.PaymentMethod) { s.ID = uuid.Nil }},
		{"zero subscription psp", "subscription.psp_id", func(s *models.Subscription, _ *models.PaymentMethod) { s.PspID = uuid.Nil }},
		{"zero payment method id", "payment_method_id", func(_ *models.Subscription, p *models.PaymentMethod) { p.ID = uuid.Nil }},
		{"zero payment method psp", "payment_method.psp_id", func(_ *models.Subscription, p *models.PaymentMethod) { p.PspID = uuid.Nil }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, pm := *sub, *fx.newPM
			tt.mutate(&s, &pm)
			_, err := fx.through.ExecutePaymentSourceUpdate(fx.ctx, &s, &pm, OriginUser, "zero id test")
			require.ErrorIs(t, err, openrails.ErrInvalid)
			var se *openrails.StatusError
			require.ErrorAs(t, err, &se)
			require.Equal(t, "invalid_param", se.Code)
			require.NotNil(t, se.Param)
			require.Equal(t, tt.param, *se.Param)
			require.NotErrorIs(t, err, paymentmethods.ErrPaymentMethodNotFound, "a zero id is invalid, never a missing row")
			require.Zero(t, fx.gateway.updateCalls.Load())
			require.Zero(t, fx.intentCount(t), "a refused request leaves no durable intent")
		})
	}
	// A zero merchant scope is refused too (the ambient scope resolves first,
	// so this one surfaces as the missing-merchant refusal).
	_, err = fx.through.ExecutePaymentSourceUpdate(merchant.WithID(context.Background(), merchant.ID{}), sub, fx.newPM, OriginUser, "zero merchant")
	require.Error(t, err)
	require.Zero(t, fx.gateway.updateCalls.Load())
	require.Zero(t, fx.intentCount(t))
}

// A frozen payload carrying a zero identifier fails closed in the executor
// exactly like a missing one: terminal, zero provider writes.
func TestNMIPaymentSourceUpdateIntent_ZeroIdentifierInFrozenPayloadIsTerminal(t *testing.T) {
	zero := uuid.Nil
	tests := []struct {
		name   string
		break_ func(*NMIPaymentSourceUpdatePayload)
	}{
		{"zero target psp", func(p *NMIPaymentSourceUpdatePayload) { p.NewPspID = uuid.Nil }},
		{"zero target method", func(p *NMIPaymentSourceUpdatePayload) { p.NewPaymentMethodID = uuid.Nil }},
		{"zero old method", func(p *NMIPaymentSourceUpdatePayload) { p.OldPaymentMethodID = &zero }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fx := newPaymentSourceSwapFixture(t)
			oldID, subID := fx.oldPM.ID, fx.sub.ID
			payload := NMIPaymentSourceUpdatePayload{
				UserID: fx.sub.CustomerID.String(), RailSubscriptionID: fx.sub.RailSubscriptionID,
				NewPaymentMethodID: fx.newPM.ID, NewRailCustomerRef: fx.newPM.RailCustomerRef, NewPspID: fx.pspID,
				OldPaymentMethodID: &oldID, OldRailCustomerRef: fx.oldPM.RailCustomerRef,
			}
			tt.break_(&payload)
			_, err := NewStore(fx.db).Enqueue(fx.ctx, EnqueueParams{
				MerchantID: dbtest.TestMerchantID.UUID(), Provider: "mobius", PspID: fx.pspID,
				IntentType: TypeNMIPaymentSourceUpdate, SubscriptionID: &subID, Payload: payload,
				IdempotencyKey: NMIPaymentSourceUpdateIdempotencyKey(subID, fx.newPM.RailCustomerRef, 0),
				NextAttemptAt:  time.Now().UTC().Add(-time.Minute),
				Origin:         OriginUser, OriginReason: "zero payload id test",
			})
			require.NoError(t, err)

			_, err = fx.runner.RunExecuteOnce(fx.ctx)
			require.NoError(t, err)
			status, _ := fx.latestIntent(t)
			require.Equal(t, StatusFailedTerminal, status)
			require.Zero(t, fx.gateway.updateCalls.Load(), "a malformed payload never reaches the provider")
			require.Equal(t, fx.oldPM.ID, fx.localPaymentMethodID(t))
		})
	}
}

// R3's reproduction on PR #488, now the regression: a same-PSP swap goes
// ambiguous, the verifier finds it not executed (failed_retryable), the target
// method is re-attributed to another PSP underneath the unresolved intent, and
// the scheduled executor re-runs it. The executor must refuse under its row
// lock: failed_terminal with psp_mismatch evidence, the ONE lost attempt is the
// only provider write ever, the subscription stays on the old method, and no
// later pass resurrects it.
func TestNMIPaymentSourceUpdateIntent_TargetReattributedAfterEnqueueFailsTerminal(t *testing.T) {
	fx := newPaymentSourceSwapFixture(t)
	fx.gateway.updateMode.Store("ambiguous_lost")

	out := fx.swapTo(t, fx.newPM)
	require.False(t, out.Done)
	require.False(t, out.Terminal)
	require.EqualValues(t, 1, fx.gateway.updateCalls.Load())

	fx.advanceClock(2 * time.Minute)
	_, err := fx.runner.RunVerifyOnce(fx.ctx)
	require.NoError(t, err)
	status, _ := fx.latestIntent(t)
	require.Equal(t, StatusFailedRetryable, status)

	fx.reattribute(t, fx.newPM, fx.otherPSP(t))
	fx.gateway.updateMode.Store("ok")

	fx.advanceClock(15 * time.Minute)
	_, err = fx.runner.RunExecuteOnce(fx.ctx)
	require.NoError(t, err)
	status, n := fx.latestIntent(t)
	require.Equal(t, StatusFailedTerminal, status)
	require.Equal(t, 1, n)
	require.Equal(t, EvidenceCodePSPMismatch, fx.latestIntentEvidenceCode(t))
	require.EqualValues(t, 1, fx.gateway.updateCalls.Load(), "no provider write after the re-attribution")
	require.Equal(t, fx.oldPM.RailCustomerRef, fx.gateway.vault.Load().(string))
	require.Equal(t, fx.oldPM.ID, fx.localPaymentMethodID(t), "never finalized onto a method another PSP owns")

	// Terminal is terminal: neither the executor nor the verifier touches it again.
	fx.advanceClock(2 * time.Hour)
	_, err = fx.runner.RunExecuteOnce(fx.ctx)
	require.NoError(t, err)
	_, err = fx.runner.RunVerifyOnce(fx.ctx)
	require.NoError(t, err)
	status, _ = fx.latestIntent(t)
	require.Equal(t, StatusFailedTerminal, status)
	require.EqualValues(t, 1, fx.gateway.updateCalls.Load())
}

// A pending intent (crash before inline execution) whose target was
// re-attributed before the scheduled executor got to it: same refusal, and
// the frozen payload PSP is what makes the drift visible.
func TestNMIPaymentSourceUpdateIntent_PendingTargetReattributedFailsTerminal(t *testing.T) {
	fx := newPaymentSourceSwapFixture(t)
	oldID, subID := fx.oldPM.ID, fx.sub.ID
	_, err := NewStore(fx.db).Enqueue(fx.ctx, EnqueueParams{
		MerchantID: dbtest.TestMerchantID.UUID(), Provider: "mobius", PspID: fx.pspID,
		IntentType: TypeNMIPaymentSourceUpdate, SubscriptionID: &subID,
		Payload: NMIPaymentSourceUpdatePayload{
			UserID: fx.sub.CustomerID.String(), RailSubscriptionID: fx.sub.RailSubscriptionID,
			NewPaymentMethodID: fx.newPM.ID, NewRailCustomerRef: fx.newPM.RailCustomerRef, NewPspID: fx.pspID,
			OldPaymentMethodID: &oldID, OldRailCustomerRef: fx.oldPM.RailCustomerRef,
		},
		IdempotencyKey: NMIPaymentSourceUpdateIdempotencyKey(subID, fx.newPM.RailCustomerRef, 0),
		NextAttemptAt:  time.Now().UTC().Add(-time.Minute),
		Origin:         OriginUser, OriginReason: "pending re-attribution test",
	})
	require.NoError(t, err)
	fx.reattribute(t, fx.newPM, fx.otherPSP(t))

	_, err = fx.runner.RunExecuteOnce(fx.ctx)
	require.NoError(t, err)
	status, _ := fx.latestIntent(t)
	require.Equal(t, StatusFailedTerminal, status)
	require.Equal(t, EvidenceCodePSPMismatch, fx.latestIntentEvidenceCode(t))
	require.Zero(t, fx.gateway.updateCalls.Load())
	require.Equal(t, fx.oldPM.ID, fx.localPaymentMethodID(t))
}

// Lock ordering against a re-attribution in flight: a transaction holds the
// target method FOR UPDATE (what the #297 remap holds while it flips) and only
// commits its psp_id change AFTER the executor has claimed the intent and is
// waiting on its FOR SHARE pin. The executor must observe the committed
// re-attribution — never the pre-flip row — and refuse with zero writes.
func TestNMIPaymentSourceUpdateIntent_ExecutorPinWaitsForReattributionInFlight(t *testing.T) {
	fx := newPaymentSourceSwapFixture(t)
	other := fx.otherPSP(t)
	oldID, subID := fx.oldPM.ID, fx.sub.ID
	_, err := NewStore(fx.db).Enqueue(fx.ctx, EnqueueParams{
		MerchantID: dbtest.TestMerchantID.UUID(), Provider: "mobius", PspID: fx.pspID,
		IntentType: TypeNMIPaymentSourceUpdate, SubscriptionID: &subID,
		Payload: NMIPaymentSourceUpdatePayload{
			UserID: fx.sub.CustomerID.String(), RailSubscriptionID: fx.sub.RailSubscriptionID,
			NewPaymentMethodID: fx.newPM.ID, NewRailCustomerRef: fx.newPM.RailCustomerRef, NewPspID: fx.pspID,
			OldPaymentMethodID: &oldID, OldRailCustomerRef: fx.oldPM.RailCustomerRef,
		},
		IdempotencyKey: NMIPaymentSourceUpdateIdempotencyKey(subID, fx.newPM.RailCustomerRef, 0),
		NextAttemptAt:  time.Now().UTC().Add(-time.Minute),
		Origin:         OriginUser, OriginReason: "lock-ordering test",
	})
	require.NoError(t, err)

	flip, err := fx.db.Pool().Begin(fx.ctx)
	require.NoError(t, err)
	defer func() { _ = flip.Rollback(context.Background()) }()
	_, err = flip.Exec(fx.ctx, "SELECT 1 FROM openrails.payment_methods WHERE id = $1 FOR UPDATE", fx.newPM.ID)
	require.NoError(t, err)

	done := make(chan error, 1)
	go func() {
		_, err := fx.runner.RunExecuteOnce(fx.ctx)
		done <- err
	}()

	// The executor has claimed the intent and is blocked on the row lock.
	require.Eventually(t, func() bool {
		var status string
		if err := fx.db.Pool().QueryRow(fx.ctx,
			"SELECT status FROM openrails.rail_intents WHERE intent_type = $1 AND subscription_id = $2",
			TypeNMIPaymentSourceUpdate, subID).Scan(&status); err != nil {
			return false
		}
		return status == StatusInFlight
	}, 10*time.Second, 20*time.Millisecond)
	require.Eventually(t, func() bool {
		var waiting bool
		require.NoError(t, fx.db.Pool().QueryRow(fx.ctx,
			"SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock' AND query ILIKE '%FOR SHARE%')").Scan(&waiting))
		return waiting
	}, 10*time.Second, 20*time.Millisecond, "executor must be waiting on the FOR SHARE pin")
	select {
	case err := <-done:
		t.Fatalf("executor finished while the row was locked: %v", err)
	default:
	}

	_, err = flip.Exec(fx.ctx, "UPDATE openrails.payment_methods SET psp_id = $1 WHERE id = $2", other, fx.newPM.ID)
	require.NoError(t, err)
	require.NoError(t, flip.Commit(fx.ctx))

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(30 * time.Second):
		t.Fatal("executor did not finish after the flip committed")
	}
	status, _ := fx.latestIntent(t)
	require.Equal(t, StatusFailedTerminal, status)
	require.Equal(t, EvidenceCodePSPMismatch, fx.latestIntentEvidenceCode(t))
	require.Zero(t, fx.gateway.updateCalls.Load(), "the pin saw the committed re-attribution; nothing reached the provider")
	require.Equal(t, fx.oldPM.ID, fx.localPaymentMethodID(t))
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
		PspID:          fx.pspID,
		IntentType:     TypeNMIPaymentSourceUpdate,
		SubscriptionID: &subID,
		Payload: NMIPaymentSourceUpdatePayload{
			UserID:             fx.sub.CustomerID.String(),
			RailSubscriptionID: fx.sub.RailSubscriptionID,
			NewPaymentMethodID: fx.newPM.ID,
			NewRailCustomerRef: fx.newPM.RailCustomerRef,
			NewPspID:           fx.pspID,
			OldPaymentMethodID: &oldID,
			OldRailCustomerRef: fx.oldPM.RailCustomerRef,
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
