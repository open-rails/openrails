//go:build integration

package intents

import (
	"context"
	"encoding/json"
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

type fakeNMICardUpdateGateway struct {
	vaultID   string
	billingID string
	target    nmiCard

	card       atomic.Value
	updateMode atomic.Value
	getCalls   atomic.Int64
	patchCalls atomic.Int64
}

func newFakeNMICardUpdateGateway(t *testing.T, vaultID, billingID string, oldCard, target nmiCard) (*fakeNMICardUpdateGateway, *nmi.NMIClient) {
	t.Helper()
	gateway := &fakeNMICardUpdateGateway{vaultID: vaultID, billingID: billingID, target: target}
	gateway.card.Store(oldCard)
	gateway.updateMode.Store("ok")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/customers":
			gateway.getCalls.Add(1)
			card := gateway.card.Load().(nmiCard)
			fmt.Fprintf(w, `{"customers":[{"object":"customer","id":"%s","billing":[{"id":"%s","priority":1,"payment_details":{"card_number":"************%s","card_exp":"%s","card_type":"%s"}}]}],"next_cursor":null,"has_more":false}`,
				gateway.vaultID, gateway.billingID, card.LastFour, strings.ReplaceAll(card.ExpiryDate, "/", ""), card.CardType)
		case r.Method == http.MethodPatch && r.URL.Path == "/customers/"+gateway.vaultID:
			gateway.patchCalls.Add(1)
			var body struct {
				Billing []struct {
					ID             string `json:"id"`
					PaymentDetails struct {
						PaymentToken string `json:"payment_token"`
					} `json:"payment_details"`
				} `json:"billing"`
			}
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			require.Len(t, body.Billing, 1)
			require.Equal(t, gateway.billingID, body.Billing[0].ID)
			require.Equal(t, "collect-token-new", body.Billing[0].PaymentDetails.PaymentToken)
			switch gateway.updateMode.Load().(string) {
			case "ambiguous_landed":
				gateway.card.Store(gateway.target)
				w.WriteHeader(http.StatusBadGateway)
			case "timeout_landed":
				gateway.card.Store(gateway.target)
				<-r.Context().Done()
			case "ambiguous_lost":
				w.WriteHeader(http.StatusBadGateway)
			case "rejected":
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprint(w, `{"type":"invalidRequest","error_code":"E_TOKEN","message":"payment token rejected"}`)
			default:
				gateway.card.Store(gateway.target)
				fmt.Fprintf(w, `{"object":"customer","id":"%s"}`, gateway.vaultID)
			}
		default:
			t.Fatalf("unexpected fake NMI request %s %s", r.Method, r.URL.String())
		}
	}))
	t.Cleanup(server.Close)

	client, err := nmi.NewClient("mobius", &config.NMIProviderSettings{SecurityKey: "test_security_key"}, true)
	require.NoError(t, err)
	client.V5BaseURL = server.URL
	client.QueryURL = server.URL
	client.DirectPostURL = server.URL
	return gateway, client
}

type paymentMethodUpdateFixture struct {
	db      *db.DB
	runner  *Runner
	handler *NMIPaymentMethodUpdateHandler
	through *PaymentMethodUpdateThrough
	gateway *fakeNMICardUpdateGateway
	pm      *models.PaymentMethod
	request *paymentmethods.UpdatePaymentMethodRequest
	ctx     context.Context
	pspID   uuid.UUID
}

func newPaymentMethodUpdateFixture(t *testing.T) *paymentMethodUpdateFixture {
	t.Helper()
	dbi := dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID())
	pool := dbi.Pool()
	dbtest.EnsureTestMerchant(context.Background(), t, pool)
	ctx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	pspID := dbtest.EnsureTestPSP(ctx, t, pool, dbtest.TestMerchantID.UUID(), "mobius")
	customerID := dbtest.EnsureCustomerIDPgx(ctx, t, pool, uuid.NewString())
	vaultID := "vault-update-" + uuid.NewString()[:8]
	billingID := "bill-update-" + uuid.NewString()[:8]
	oldCard := nmiCard{LastFour: "1111", CardType: "Visa", ExpiryDate: "01/29"}
	target := nmiCard{LastFour: "4242", CardType: "Mastercard", ExpiryDate: "12/30"}
	pm := &models.PaymentMethod{
		ID:                   uuid.New(),
		CustomerID:           customerID,
		Rail:                 models.RailNMI,
		PspID:                pspID,
		RailCustomerRef:      vaultID,
		RailMethodRef:        billingID,
		RebillDriver:         models.RebillDriverProvider,
		InitialTransactionID: "txn-update-" + uuid.NewString()[:8],
		LastFour:             stringPtr(oldCard.LastFour),
		CardType:             stringPtr(oldCard.CardType),
		ExpiryDate:           stringPtr(oldCard.ExpiryDate),
		CreatedAt:            time.Now().UTC(),
		UpdatedAt:            time.Now().UTC(),
	}
	require.NoError(t, paymentmethods.NewPaymentMethodRepo(dbi).Create(ctx, pm))

	token := "collect-token-new"
	lastFour, cardType, expiry := target.LastFour, target.CardType, target.ExpiryDate
	request := &paymentmethods.UpdatePaymentMethodRequest{
		PaymentToken: &token,
		LastFour:     &lastFour,
		CardType:     &cardType,
		ExpiryDate:   &expiry,
	}
	gateway, client := newFakeNMICardUpdateGateway(t, vaultID, billingID, oldCard, target)
	store := NewStore(dbi)
	handler := NewNMIPaymentMethodUpdateHandler(dbi, fakeVaultClientResolver{client: client}, store, nil)
	runner := &Runner{Store: store, Registry: NewRegistry(handler), Config: fullModeConfig()}
	through := &PaymentMethodUpdateThrough{Runner: runner, DB: dbi}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.rail_intents WHERE intent_type = $1 AND idempotency_key = $2",
			TypeNMIPaymentMethodUpdate, NMIPaymentMethodUpdateIdempotencyKey(pm.ID, token))
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.payment_methods WHERE id = $1", pm.ID)
	})
	return &paymentMethodUpdateFixture{
		db: dbi, runner: runner, handler: handler, through: through, gateway: gateway,
		pm: pm, request: request, ctx: ctx, pspID: pspID,
	}
}

func (fx *paymentMethodUpdateFixture) execute(t *testing.T) paymentmethods.PaymentMethodUpdateOutcome {
	t.Helper()
	out, err := fx.through.ExecutePaymentMethodUpdate(fx.ctx, fx.pm, fx.request)
	require.NoError(t, err)
	return out
}

func (fx *paymentMethodUpdateFixture) intent(t *testing.T) (status string, payload []byte, evidence []byte) {
	t.Helper()
	err := fx.db.Pool().QueryRow(fx.ctx,
		`SELECT status, payload, result_evidence FROM openrails.rail_intents WHERE intent_type = $1 AND idempotency_key = $2`,
		TypeNMIPaymentMethodUpdate, NMIPaymentMethodUpdateIdempotencyKey(fx.pm.ID, *fx.request.PaymentToken),
	).Scan(&status, &payload, &evidence)
	require.NoError(t, err)
	return status, payload, evidence
}

func (fx *paymentMethodUpdateFixture) localCard(t *testing.T) nmiCard {
	t.Helper()
	pm, err := paymentmethods.NewPaymentMethodRepo(fx.db).GetByID(fx.ctx, fx.pm.ID)
	require.NoError(t, err)
	return nmiCard{LastFour: *pm.LastFour, CardType: *pm.CardType, ExpiryDate: *pm.ExpiryDate}
}

func (fx *paymentMethodUpdateFixture) advanceClock(d time.Duration) {
	fx.runner.Clock = clockwork.NewFakeClockAt(time.Now().UTC().Add(d))
}

func TestNMIPaymentMethodUpdateIntent_HappyPathAndReplay(t *testing.T) {
	fx := newPaymentMethodUpdateFixture(t)

	out := fx.execute(t)
	require.True(t, out.Done, "reason=%s", out.Reason)
	require.Equal(t, fx.gateway.target, fx.localCard(t))
	require.EqualValues(t, 1, fx.gateway.patchCalls.Load())
	status, payload, _ := fx.intent(t)
	require.Equal(t, StatusSucceeded, status)
	require.Empty(t, payload, "single-use token must be pruned after success")

	reads, writes := fx.gateway.getCalls.Load(), fx.gateway.patchCalls.Load()
	replay := fx.execute(t)
	require.True(t, replay.Done)
	require.Equal(t, reads, fx.gateway.getCalls.Load())
	require.Equal(t, writes, fx.gateway.patchCalls.Load())
}

func TestNMIPaymentMethodUpdateIntent_CrashBeforeExecute(t *testing.T) {
	fx := newPaymentMethodUpdateFixture(t)
	payload := fx.throughPayload()
	_, err := NewStore(fx.db).Enqueue(fx.ctx, EnqueueParams{
		MerchantID: dbtest.TestMerchantID.UUID(), Provider: "nmi", IntentType: TypeNMIPaymentMethodUpdate,
		PspID: fx.pspID, Payload: payload,
		IdempotencyKey: NMIPaymentMethodUpdateIdempotencyKey(fx.pm.ID, payload.PaymentToken),
		NextAttemptAt:  time.Now().UTC().Add(-time.Minute), Origin: OriginUser,
	})
	require.NoError(t, err)
	require.Zero(t, fx.gateway.patchCalls.Load())

	_, err = fx.runner.RunExecuteOnce(fx.ctx)
	require.NoError(t, err)
	status, _, _ := fx.intent(t)
	require.Equal(t, StatusSucceeded, status)
	require.Equal(t, fx.gateway.target, fx.localCard(t))
	require.EqualValues(t, 1, fx.gateway.patchCalls.Load())
}

func TestNMIPaymentMethodUpdateIntent_CrashAfterSubmissionBoundaryDoesNotSendToken(t *testing.T) {
	fx := newPaymentMethodUpdateFixture(t)
	payload := fx.throughPayload()
	store := NewStore(fx.db)
	row, err := store.Enqueue(fx.ctx, EnqueueParams{
		MerchantID: dbtest.TestMerchantID.UUID(), Provider: "nmi", IntentType: TypeNMIPaymentMethodUpdate,
		PspID: fx.pspID, Payload: payload,
		IdempotencyKey: NMIPaymentMethodUpdateIdempotencyKey(fx.pm.ID, payload.PaymentToken),
		NextAttemptAt:  time.Now().UTC().Add(-time.Minute), Origin: OriginUser,
	})
	require.NoError(t, err)
	claimed, ok, err := store.ClaimByID(fx.ctx, row.ID, time.Now().UTC(), time.Now().UTC().Add(-time.Minute))
	require.NoError(t, err)
	require.True(t, ok)
	require.NoError(t, store.RecordProgress(fx.ctx, claimed.ID, map[string]any{
		"submission_started": true,
		"old_card":           nmiCard{LastFour: "1111", CardType: "Visa", ExpiryDate: "01/29"},
	}))

	_, err = fx.runner.RunExecuteOnce(fx.ctx)
	require.NoError(t, err)
	status, intentPayload, evidence := fx.intent(t)
	require.Equal(t, StatusFailedTerminal, status)
	require.Empty(t, intentPayload)
	require.True(t, intentEvidenceBool(evidence, "retokenize"))
	require.Zero(t, fx.gateway.patchCalls.Load(), "a token may not be sent after a crash at the submission boundary")
	require.Equal(t, nmiCard{LastFour: "1111", CardType: "Visa", ExpiryDate: "01/29"}, fx.localCard(t))
}

func TestNMIPaymentMethodUpdateIntent_AmbiguousLandedVerifierFinalizes(t *testing.T) {
	fx := newPaymentMethodUpdateFixture(t)
	fx.gateway.updateMode.Store("ambiguous_landed")

	out := fx.execute(t)
	require.False(t, out.Done)
	require.False(t, out.Terminal)
	status, payload, _ := fx.intent(t)
	require.Equal(t, StatusUnknownNeedsVerify, status)
	require.NotEmpty(t, payload)
	require.Equal(t, nmiCard{LastFour: "1111", CardType: "Visa", ExpiryDate: "01/29"}, fx.localCard(t))

	fx.advanceClock(2 * time.Minute)
	_, err := fx.runner.RunVerifyOnce(fx.ctx)
	require.NoError(t, err)
	status, payload, _ = fx.intent(t)
	require.Equal(t, StatusSucceeded, status)
	require.Empty(t, payload)
	require.Equal(t, fx.gateway.target, fx.localCard(t))
	require.EqualValues(t, 1, fx.gateway.patchCalls.Load(), "single-use token is never resubmitted")
}

func TestNMIPaymentMethodUpdateIntent_TimeoutAfterSendRecoversFromProviderTruth(t *testing.T) {
	fx := newPaymentMethodUpdateFixture(t)
	fx.gateway.updateMode.Store("timeout_landed")
	requestCtx, cancel := context.WithTimeout(fx.ctx, 100*time.Millisecond)
	defer cancel()

	_, err := fx.through.ExecutePaymentMethodUpdate(requestCtx, fx.pm, fx.request)
	require.Error(t, err)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Equal(t, nmiCard{LastFour: "1111", CardType: "Visa", ExpiryDate: "01/29"}, fx.localCard(t))

	fx.advanceClock(3 * time.Minute)
	_, err = fx.runner.RunExecuteOnce(fx.ctx)
	require.NoError(t, err)
	status, payload, _ := fx.intent(t)
	require.Equal(t, StatusSucceeded, status)
	require.Empty(t, payload)
	require.Equal(t, fx.gateway.target, fx.localCard(t))
	require.EqualValues(t, 1, fx.gateway.patchCalls.Load(), "timed-out single-use token is never resubmitted")
}

func TestNMIPaymentMethodUpdateIntent_AmbiguousLostRequiresRetokenization(t *testing.T) {
	fx := newPaymentMethodUpdateFixture(t)
	fx.gateway.updateMode.Store("ambiguous_lost")

	out := fx.execute(t)
	require.False(t, out.Done)
	require.False(t, out.Terminal)
	fx.advanceClock(2 * time.Minute)
	_, err := fx.runner.RunVerifyOnce(fx.ctx)
	require.NoError(t, err)
	status, payload, evidence := fx.intent(t)
	require.Equal(t, StatusFailedTerminal, status)
	require.Empty(t, payload, "terminal intent must not retain the Collect.js token")
	require.True(t, intentEvidenceBool(evidence, "retokenize"))
	require.EqualValues(t, 1, fx.gateway.patchCalls.Load(), "single-use token is never resubmitted")
	require.Equal(t, nmiCard{LastFour: "1111", CardType: "Visa", ExpiryDate: "01/29"}, fx.localCard(t))

	retry := fx.execute(t)
	require.True(t, retry.Terminal)
	require.True(t, retry.Retokenize)
}

func TestNMIPaymentMethodUpdateIntent_CleanRejectionIsTerminal(t *testing.T) {
	fx := newPaymentMethodUpdateFixture(t)
	fx.gateway.updateMode.Store("rejected")

	out := fx.execute(t)
	require.True(t, out.Terminal)
	require.False(t, out.Retokenize)
	status, payload, _ := fx.intent(t)
	require.Equal(t, StatusFailedTerminal, status)
	require.Empty(t, payload)
	require.EqualValues(t, 1, fx.gateway.patchCalls.Load())
}

func TestNMIPaymentMethodUpdateIntent_LocalFinalizeFailureVerifierRepairs(t *testing.T) {
	fx := newPaymentMethodUpdateFixture(t)
	var fail atomic.Bool
	fail.Store(true)
	fx.handler.finalizeWrite = func(ctx context.Context, pm *models.PaymentMethod) error {
		if fail.Swap(false) {
			return fmt.Errorf("injected local write failure")
		}
		return paymentmethods.NewPaymentMethodRepo(fx.db).Update(ctx, pm)
	}

	out := fx.execute(t)
	require.False(t, out.Done)
	status, _, _ := fx.intent(t)
	require.Equal(t, StatusUnknownNeedsVerify, status)
	require.Equal(t, nmiCard{LastFour: "1111", CardType: "Visa", ExpiryDate: "01/29"}, fx.localCard(t))

	fx.advanceClock(2 * time.Minute)
	_, err := fx.runner.RunVerifyOnce(fx.ctx)
	require.NoError(t, err)
	status, _, _ = fx.intent(t)
	require.Equal(t, StatusSucceeded, status)
	require.Equal(t, fx.gateway.target, fx.localCard(t))
	require.EqualValues(t, 1, fx.gateway.patchCalls.Load())
}

func (fx *paymentMethodUpdateFixture) throughPayload() NMIPaymentMethodUpdatePayload {
	return NMIPaymentMethodUpdatePayload{
		UserID:          fx.pm.CustomerID.String(),
		PaymentMethodID: fx.pm.ID,
		RailCustomerRef: fx.pm.RailCustomerRef,
		RailMethodRef:   fx.pm.RailMethodRef,
		PaymentToken:    *fx.request.PaymentToken,
		TargetCard: nmiCard{
			LastFour: *fx.request.LastFour, CardType: *fx.request.CardType, ExpiryDate: *fx.request.ExpiryDate,
		},
	}
}
