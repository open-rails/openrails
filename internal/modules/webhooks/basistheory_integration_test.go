//go:build integration

package webhooks

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/integrations/basistheory"
	"github.com/open-rails/openrails/internal/modules/idempotency"
	"github.com/open-rails/openrails/internal/railresolve"
	"github.com/open-rails/openrails/pkg/merchant"
)

// #795 B6 folds: token.deleted PARKS the instrument (cancellation-last-resort:
// no terminal cancel, no delete), network-token.updated touches NT status only
// (never PAN-side expiry), account-updater UPD_* rotates rail_method_ref, and
// duplicate deliveries are idempotent.

type btWebhookFixture struct {
	dbi        *db.DB
	dispatcher *WebhookDispatcher
	ctx        context.Context
	tokenID    string
	ntID       string
	methodID   uuid.UUID
	customerID uuid.UUID
	btAPI      *httptest.Server
}

func newBTWebhookFixture(t *testing.T) *btWebhookFixture {
	t.Helper()
	dsn := dbtest.MerchantPinnedDSN(t, dbtest.TestMerchantID.UUID())
	dbi := dbtest.OpenAppDB(t, dsn)
	pool := dbi.Pool()
	dbtest.EnsureTestMerchant(context.Background(), t, pool)
	ctx := merchant.WithID(context.Background(), dbtest.TestMerchantID)

	fx := &btWebhookFixture{
		dbi:      dbi,
		ctx:      ctx,
		tokenID:  uuid.NewString(),
		ntID:     uuid.NewString(),
		methodID: uuid.New(),
	}
	userID := uuid.NewString()
	fx.customerID = dbtest.EnsureCustomerIDPgx(ctx, t, pool, userID)

	// The BT API leg used by token.updated / AU job fetches (none of these
	// tests hit it; declared so client builds succeed).
	fx.btAPI = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(fx.btAPI.Close)

	now := time.Now().UTC().Truncate(time.Second)
	_, err := gen.New(pool).CreatePaymentMethod(ctx, gen.CreatePaymentMethodParams{
		ID:                   fx.methodID,
		MerchantID:           dbtest.TestMerchantID.UUID(),
		CustomerID:           fx.customerID,
		Rail:                 string(models.RailVaultedCard),
		RailMethodRef:        fx.tokenID,
		InitialTransactionID: "",
		RebillDriver:         "openrails",
		VaultProvider:        "basis_theory",
		VaultFingerprint:     "fp_" + uuid.NewString()[:10],
		NetworkTokenID:       fx.ntID,
		NetworkTokenStatus:   "active",
		NetworkTokenPar:      "par_x",
		ChargeVia:            "pan_proxy",
		LastFour:             ptr("1111"),
		CardType:             ptr("visa"),
		ExpiryDate:           ptr("12/31"),
		CreatedAt:            now,
		UpdatedAt:            now,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.payment_methods WHERE id = $1", fx.methodID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.webhook_events WHERE rail = 'vaulted_card'")
	})

	fx.dispatcher = &WebhookDispatcher{
		DB:                   dbi,
		DeduplicationService: NewDeduplicationService(idempotency.NewIdempotencyService(nil), dbi),
		RailConfigs: railresolve.FixedSet{
			"bt": {
				Rail:      models.RailVaultedCard,
				AccountID: "tnt_test",
				VaultedCard: &config.VaultedCardRailConfig{
					APIKey:     "key_private_test",
					APIBaseURL: fx.btAPI.URL,
				},
			},
		},
	}
	return fx
}

func ptr(s string) *string { return &s }

func (fx *btWebhookFixture) deliver(t *testing.T, eventID, eventType string, data any) error {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"id":        eventID,
		"tenant_id": "tnt_test",
		"type":      eventType,
		"data":      data,
	})
	require.NoError(t, err)
	verified := true
	return fx.dispatcher.Process(fx.ctx, &WebhookMessage{
		Rail:           string(models.RailVaultedCard),
		EventID:        eventID,
		EventType:      eventType,
		Payload:        payload,
		SignatureValid: &verified,
		ReceivedAt:     time.Now(),
	})
}

func (fx *btWebhookFixture) methodRow(t *testing.T) gen.OpenrailsPaymentMethod {
	t.Helper()
	row, err := fx.dbi.Gen(fx.ctx).GetPaymentMethodByID(fx.ctx, fx.methodID)
	require.NoError(t, err)
	return row
}

func TestBasisTheoryWebhook_TokenDeletedParksInstrument(t *testing.T) {
	fx := newBTWebhookFixture(t)
	eventID := "evt_" + uuid.NewString()

	require.NoError(t, fx.deliver(t, eventID, basistheory.EventTokenDeleted,
		map[string]any{"token": map[string]any{"id": fx.tokenID, "type": "card"}}))

	row := fx.methodRow(t)
	require.Equal(t, "bt_token_deleted", row.ParkReason)
	require.NotNil(t, row.ParkedAt)
	// Cancellation-last-resort: the instrument row still EXISTS (never deleted).
	require.Equal(t, fx.tokenID, row.RailMethodRef)

	// Duplicate delivery is idempotent (dedup short-circuits; park unchanged).
	firstParkedAt := *row.ParkedAt
	require.NoError(t, fx.deliver(t, eventID, basistheory.EventTokenDeleted,
		map[string]any{"token": map[string]any{"id": fx.tokenID, "type": "card"}}))
	row = fx.methodRow(t)
	require.Equal(t, firstParkedAt, *row.ParkedAt)

	// A DIFFERENT event id for the same token is also park-idempotent
	// (write-once park reason).
	require.NoError(t, fx.deliver(t, "evt_"+uuid.NewString(), basistheory.EventTokenExpired,
		map[string]any{"token": map[string]any{"id": fx.tokenID, "type": "card"}}))
	row = fx.methodRow(t)
	require.Equal(t, "bt_token_deleted", row.ParkReason, "first park wins")
}

func TestBasisTheoryWebhook_NetworkTokenUpdateNeverTouchesPANExpiry(t *testing.T) {
	fx := newBTWebhookFixture(t)

	require.NoError(t, fx.deliver(t, "evt_"+uuid.NewString(), basistheory.EventNetworkTokenUpdated,
		map[string]any{"network_token": map[string]any{"id": fx.ntID, "status": "suspended", "token_id": fx.tokenID}}))

	row := fx.methodRow(t)
	require.Equal(t, "suspended", row.NetworkTokenStatus)
	require.NotNil(t, row.ExpiryDate)
	require.Equal(t, "12/31", *row.ExpiryDate, "NT lifecycle must never touch PAN-side expiry")
	require.Equal(t, "pan_proxy", row.ChargeVia)

	// network-token.deleted: status folds to deleted, id kept, charge_via stays.
	require.NoError(t, fx.deliver(t, "evt_"+uuid.NewString(), basistheory.EventNetworkTokenDeleted,
		map[string]any{"network_token": map[string]any{"id": fx.ntID}}))
	row = fx.methodRow(t)
	require.Equal(t, "deleted", row.NetworkTokenStatus)
	require.Equal(t, fx.ntID, row.NetworkTokenID)
}

func TestBasisTheoryWebhook_AccountUpdaterFold(t *testing.T) {
	fx := newBTWebhookFixture(t)
	svc := &basisTheoryWebhookService{d: fx.dispatcher}

	t.Run("UPD_PAN rotates rail_method_ref and refreshes metadata", func(t *testing.T) {
		newToken := uuid.NewString()
		require.NoError(t, svc.FoldAccountUpdaterRows(fx.ctx, []basistheory.AccountUpdaterResultRow{{
			Token:              fx.tokenID,
			NewToken:           newToken,
			NewExpirationMonth: "7",
			NewExpirationYear:  "2033",
			ResultCode:         basistheory.AUUpdatedPAN,
			NewFingerprint:     "fp_rotated",
			NewBrand:           "mastercard",
			NewLast4:           "4444",
		}}))
		row := fx.methodRow(t)
		require.Equal(t, newToken, row.RailMethodRef, "rail_method_ref rotates to new_token")
		require.Equal(t, "fp_rotated", row.VaultFingerprint)
		require.Equal(t, "4444", *row.LastFour)
		require.Equal(t, "mastercard", *row.CardType)
		require.Equal(t, "07/33", *row.ExpiryDate)
		fx.tokenID = newToken
	})

	t.Run("NO_UPDATE is a no-op", func(t *testing.T) {
		before := fx.methodRow(t)
		require.NoError(t, svc.FoldAccountUpdaterRows(fx.ctx, []basistheory.AccountUpdaterResultRow{{
			Token: fx.tokenID, ResultCode: basistheory.AUNoUpdate,
		}}))
		after := fx.methodRow(t)
		require.Equal(t, before.RailMethodRef, after.RailMethodRef)
		require.Equal(t, before.VaultFingerprint, after.VaultFingerprint)
	})

	t.Run("WRN_CLOSED_ACCOUNT parks", func(t *testing.T) {
		require.NoError(t, svc.FoldAccountUpdaterRows(fx.ctx, []basistheory.AccountUpdaterResultRow{{
			Token: fx.tokenID, ResultCode: basistheory.AUClosedAccount,
		}}))
		row := fx.methodRow(t)
		require.Equal(t, "bt_au_closed_account", row.ParkReason)
	})
}

func TestBasisTheoryWebhook_UnverifiedIsRejectedNonRetryable(t *testing.T) {
	fx := newBTWebhookFixture(t)
	payload, _ := json.Marshal(map[string]any{"id": "evt_x", "type": basistheory.EventTokenDeleted})
	err := fx.dispatcher.Process(fx.ctx, &WebhookMessage{
		Rail:      string(models.RailVaultedCard),
		EventID:   "evt_x",
		EventType: basistheory.EventTokenDeleted,
		Payload:   payload,
		// SignatureValid deliberately unset.
		ReceivedAt: time.Now(),
	})
	require.Error(t, err)
	require.True(t, IsWebhookErrorNonRetryable(err))
}

// TestBasisTheoryWebhook_ParseAUResultCSV pins the CSV v1.2 header-name-based
// parsing the AU fold consumes.
func TestBasisTheoryWebhook_ParseAUResultCSV(t *testing.T) {
	csv := "token,expiration_year,expiration_month,new_token,new_expiration_year,new_expiration_month,result_code,new_fingerprint,new_brand,new_last4\n" +
		fmt.Sprintf("%s,2031,12,%s,2033,07,UPD_PAN,fp_new,visa,9876\n", "tok_a", "tok_b") +
		"tok_c,2030,01,,,,WRN_CLOSED_ACCOUNT,,,\n"
	rows, err := basistheory.ParseAccountUpdaterResults(strings.NewReader(csv))
	require.NoError(t, err)
	require.Len(t, rows, 2)
	require.Equal(t, "tok_b", rows[0].NewToken)
	require.Equal(t, basistheory.AUUpdatedPAN, rows[0].ResultCode)
	require.Equal(t, basistheory.AUClosedAccount, rows[1].ResultCode)
}
