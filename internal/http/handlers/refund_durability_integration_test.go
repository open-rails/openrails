//go:build integration

package handlers

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/productaccess"
	"github.com/stretchr/testify/require"
)

func TestRefundInterruptedBeforeEnqueueMustRemainRetryable(t *testing.T) {
	fx := newFindingsFixture(t)
	payID := fx.seedCompletedPayment("audit-"+uuid.NewString(), nil)
	hr := httprequest.NewHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/refund", nil).WithContext(fx.ctx), fx.rt)
	req := refundRequest{Amount: 10000000}
	var prepared *adminRefundPrepared
	require.NoError(t, fx.dbi.MerchantTx(fx.ctx, func(ctx context.Context, tx pgx.Tx) error {
		txDB := db.NewWithPgxTx(tx)
		var err error
		prepared, err = prepareAdminRefund(ctx, hr, txDB, payments.NewPaymentService(txDB), payID, req, "audit-key")
		return err
	}))
	// Simulate request cancellation immediately after reservation commit.
	cancelled, cancel := context.WithCancel(fx.ctx)
	cancel()
	_, _, err := issuePreparedAdminRefund(cancelled, hr, prepared)
	require.Error(t, err)
	var n int
	require.NoError(t, fx.dbi.Pool().QueryRow(fx.ctx, "SELECT count(*) FROM openrails.rail_intents WHERE payment_id=$1", payID).Scan(&n))
	require.Equal(t, 1, n, "reservation and durable intent commit together")
	total, err := fx.rt.PaymentService.GetRefundTotalByPaymentID(fx.ctx, payID)
	require.NoError(t, err)
	require.EqualValues(t, 10000000, total, "pending operation reserves refundable balance")
	_, _, retryErr := executeAdminRefund(fx.ctx, hr, payID, req, "audit-key")
	t.Logf("replay after cancelled enqueue: %v", retryErr)
	require.NoError(t, retryErr, "a refund that never entered the intent ledger must be resumable")
}

func TestParkedRefundMustNotRevokePurchasedAccess(t *testing.T) {
	fx := newFindingsFixture(t)
	fx.rt.Config.ProviderWriteMode = config.ProviderWriteModeReadOnly
	payID := fx.seedCompletedPayment("audit-"+uuid.NewString(), nil)
	grant, _, err := fx.rt.ProductAccessService.GrantProductAccess(fx.ctx, productaccess.GrantParams{UserID: fx.customer.String(), ProductID: fx.product, SourceType: models.ProductAccessSourcePurchase, SourceID: payID.String(), PaymentID: &payID})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/refund", bytes.NewBufferString(`{"amount":10000000,"revoke_access":true}`)).WithContext(fx.ctx)
	req.SetPathValue("id", payID.String())
	req.Header.Set("Idempotency-Key", "audit-key")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	AdminRefundPayment(httprequest.NewHTTP(rec, req, fx.rt))
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	require.Zero(t, fx.fake.refundCalls.Load(), "no refund reached the provider")
	got, err := fx.rt.ProductAccessService.GetGrant(fx.ctx, grant.ID)
	require.NoError(t, err)
	t.Logf("parked refund HTTP %d, provider calls=%d, product access=%s", rec.Code, fx.fake.refundCalls.Load(), got.Status)
	require.Equal(t, models.ProductAccessStatusActive, got.Status, "customer must retain paid access while refund is unexecuted")
}

func TestTerminalRefundCanRetryWithNewClientKey(t *testing.T) {
	fx := newFindingsFixture(t)
	payID := fx.seedCompletedPayment("audit-"+uuid.NewString(), nil)
	hr := httprequest.NewHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/refund", nil).WithContext(fx.ctx), fx.rt)
	req := refundRequest{Amount: 5000000}
	fx.fake.refundStatus.Store(`{"object":"transaction","id":"txn_refund_1","response":"2","response_text":"DECLINED","response_code":"300"}`)
	_, _, err := executeAdminRefund(fx.ctx, hr, payID, req, "first-key")
	require.Error(t, err)
	require.EqualValues(t, 1, fx.fake.refundCalls.Load())
	fx.fake.refundStatus.Store("") // Provider now permits the refund.
	_, _, err = executeAdminRefund(fx.ctx, hr, payID, req, "new-key")
	t.Logf("fresh key after terminal refusal: %v; provider calls=%d", err, fx.fake.refundCalls.Load())
	require.NoError(t, err, "fresh caller key should permit a new operation after a proved refusal")
	require.EqualValues(t, 2, fx.fake.refundCalls.Load())
}

func TestDistinctEqualPartialRefundsAreNotDuplicates(t *testing.T) {
	fx := newFindingsFixture(t)
	payID := fx.seedCompletedPayment("audit-"+uuid.NewString(), nil)
	hr := httprequest.NewHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/refund", nil).WithContext(fx.ctx), fx.rt)
	req := refundRequest{Amount: 5000000}
	_, status, err := executeAdminRefund(fx.ctx, hr, payID, req, "first-partial")
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, status)
	total, err := fx.rt.PaymentService.GetRefundTotalByPaymentID(fx.ctx, payID)
	require.NoError(t, err)
	require.EqualValues(t, 5000000, total)
	fx.fake.refundStatus.Store(`{"object":"transaction","id":"txn_refund_2","response":"1","response_text":"SUCCESS"}`)
	_, status, err = executeAdminRefund(fx.ctx, hr, payID, req, "second-partial")
	t.Logf("second $5 refund of $10 purchase, fresh key: %v; provider calls=%d", err, fx.fake.refundCalls.Load())
	require.NoError(t, err, "a separately authorized equal-size partial refund within the remaining balance is a distinct operation")
	require.Equal(t, http.StatusCreated, status)
	require.EqualValues(t, 2, fx.fake.refundCalls.Load())
	_, _, err = executeAdminRefund(fx.ctx, hr, payID, req, "third-partial")
	require.Error(t, err)
	require.EqualValues(t, 2, fx.fake.refundCalls.Load(), "remaining balance prevents over-refund")
}
