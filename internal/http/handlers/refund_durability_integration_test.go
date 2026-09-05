//go:build integration

package handlers

import (
	"bytes"
	"context"
	"fmt"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/pkg/merchant"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

func TestAdminCCBillRefundRejectedBeforeReservationAndAccessChanges(t *testing.T) {
	for _, amount := range []int64{5_000_000, 10_000_000} {
		t.Run(fmt.Sprint(amount), func(t *testing.T) {
			fx := newFindingsFixture(t)
			payID := fx.seedCompletedPayment("ccbill-"+uuid.NewString(), nil)
			fx.exec(`UPDATE openrails.payments SET rail='ccbill',psp_id=$2 WHERE id=$1`, payID, fx.pspFor("ccbill"))
			grant, _, err := fx.rt.ProductAccessService.GrantProductAccess(fx.ctx, productaccess.GrantParams{UserID: fx.customer.String(), ProductID: fx.product, SourceType: models.ProductAccessSourcePurchase, SourceID: payID.String(), PaymentID: &payID})
			require.NoError(t, err)
			req := httptest.NewRequest(http.MethodPost, "/refund", strings.NewReader(fmt.Sprintf(`{"amount":%d,"revoke_access":true}`, amount))).WithContext(fx.ctx)
			req.SetPathValue("id", payID.String())
			req.Header.Set("Idempotency-Key", "ccbill-refusal")
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			AdminRefundPayment(httprequest.NewHTTP(rec, req, fx.rt))
			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			require.Contains(t, rec.Body.String(), "automatic CCBill refunds are unavailable")
			var count int
			require.NoError(t, fx.dbi.Pool().QueryRow(fx.ctx, `SELECT count(*) FROM openrails.rail_intents WHERE merchant_id=$1`, fx.merchant).Scan(&count))
			require.Zero(t, count)
			require.NoError(t, fx.dbi.Pool().QueryRow(fx.ctx, `SELECT count(*) FROM openrails.payments WHERE refunded_payment_id=$1`, payID).Scan(&count))
			require.Zero(t, count)
			got, err := fx.rt.ProductAccessService.GetGrant(fx.ctx, grant.ID)
			require.NoError(t, err)
			require.Equal(t, models.ProductAccessStatusActive, got.Status)
		})
	}
}

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

// Force the intent insert to fail after the reservation insert. Both writes
// must roll back, so a corrected request can still use the whole balance.
func TestRefundEnqueueFailureRollsBackReservation(t *testing.T) {
	fx := newFindingsFixture(t)
	payID := fx.seedCompletedPayment("enqueue-"+uuid.NewString(), nil)
	super := dbtest.SharedSuperuserPGXPool(t)
	constraint := "test_refund_enqueue_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	_, err := super.Exec(fx.ctx, fmt.Sprintf(`ALTER TABLE openrails.rail_intents ADD CONSTRAINT %s CHECK (merchant_id <> '%s')`, constraint, fx.merchant))
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = super.Exec(context.Background(), "ALTER TABLE openrails.rail_intents DROP CONSTRAINT IF EXISTS "+constraint)
	})
	hr := httprequest.NewHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/refund", nil).WithContext(fx.ctx), fx.rt)
	_, _, err = executeAdminRefund(fx.ctx, hr, payID, refundRequest{Amount: 10000000}, "key")
	require.Error(t, err)
	total, err := fx.rt.PaymentService.GetRefundTotalByPaymentID(fx.ctx, payID)
	require.NoError(t, err)
	require.Zero(t, total)
	require.Zero(t, fx.fake.refundCalls.Load())
	_, err = super.Exec(fx.ctx, "ALTER TABLE openrails.rail_intents DROP CONSTRAINT "+constraint)
	require.NoError(t, err)
	_, status, err := executeAdminRefund(fx.ctx, hr, payID, refundRequest{Amount: 10000000}, "key")
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, status)
}

func TestRefundFinalizationRecoversReceiptAndRevokesAtomically(t *testing.T) {
	fx := newFindingsFixture(t)
	payID := fx.seedCompletedPayment("revoke-"+uuid.NewString(), nil)
	grant, _, err := fx.rt.ProductAccessService.GrantProductAccess(fx.ctx, productaccess.GrantParams{UserID: fx.customer.String(), ProductID: fx.product, SourceType: models.ProductAccessSourcePurchase, SourceID: payID.String(), PaymentID: &payID})
	require.NoError(t, err)
	now := fx.rt.Clock.Now()
	end := now.Add(time.Hour)
	entitlement := &models.Entitlement{ID: uuid.New(), CustomerID: fx.customer, Entitlement: "refund-access", StartAt: now.Add(-time.Hour), EndAt: &end, SourceType: models.EntitlementSourceOneOff, SourceID: &payID, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, fx.rt.EntitlementService.Insert(fx.ctx, entitlement))
	super := dbtest.SharedSuperuserPGXPool(t)
	constraint := "test_refund_revoke_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	_, err = super.Exec(fx.ctx, fmt.Sprintf(`ALTER TABLE openrails.grants ADD CONSTRAINT %s CHECK (merchant_id <> '%s' OR event <> 'revoke')`, constraint, fx.merchant))
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = super.Exec(context.Background(), "ALTER TABLE openrails.grants DROP CONSTRAINT IF EXISTS "+constraint)
	})
	hr := httprequest.NewHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/refund", nil).WithContext(fx.ctx), fx.rt)
	req := refundRequest{Amount: 10000000, RevokeAccess: true}
	refund, status, err := executeAdminRefund(fx.ctx, hr, payID, req, "revoke-key")
	require.NoError(t, err)
	require.Equal(t, http.StatusAccepted, status)
	require.Equal(t, "pending", refund.Status)
	require.Equal(t, "txn_refund_1", refund.Metadata["provider_refund_id"], "exact success survives local rollback")
	var revokedAt *time.Time
	require.NoError(t, fx.dbi.Pool().QueryRow(fx.ctx, `SELECT revoked_at FROM openrails.entitlements WHERE id=$1`, entitlement.ID).Scan(&revokedAt))
	require.Nil(t, revokedAt, "first access update rolled back when second failed")
	got, err := fx.rt.ProductAccessService.GetGrant(fx.ctx, grant.ID)
	require.NoError(t, err)
	require.Equal(t, models.ProductAccessStatusActive, got.Status)
	_, _, err = executeAdminRefund(fx.ctx, hr, payID, refundRequest{Amount: req.Amount}, "revoke-key")
	require.Error(t, err, "same key cannot change revocation policy")
	_, err = super.Exec(fx.ctx, "ALTER TABLE openrails.grants DROP CONSTRAINT "+constraint)
	require.NoError(t, err)
	fx.exec(`UPDATE openrails.rail_intents SET next_attempt_at=now() WHERE payment_id=$1`, payID)
	_, err = fx.rt.IntentRunner().RunVerifyOnce(fx.ctx)
	require.NoError(t, err)
	refund, err = fx.rt.PaymentService.GetByID(fx.ctx, refund.ID)
	require.NoError(t, err)
	require.Equal(t, "completed", refund.Status)
	got, err = fx.rt.ProductAccessService.GetGrant(fx.ctx, grant.ID)
	require.NoError(t, err)
	require.Equal(t, models.ProductAccessStatusRevoked, got.Status)
	require.NoError(t, fx.dbi.Pool().QueryRow(fx.ctx, `SELECT revoked_at FROM openrails.entitlements WHERE id=$1`, entitlement.ID).Scan(&revokedAt))
	require.NotNil(t, revokedAt)
	_, status, err = executeAdminRefund(fx.ctx, hr, payID, req, "revoke-key")
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, status)
	require.EqualValues(t, 1, fx.fake.refundCalls.Load(), "known success never sends money twice")
}

func TestConcurrentRefundsCannotOverReserve(t *testing.T) {
	fx := newFindingsFixture(t)
	payID := fx.seedCompletedPayment("concurrent-"+uuid.NewString(), nil)
	type result struct {
		status int
		err    error
	}
	results := make(chan result, 2)
	start := make(chan struct{})
	for _, key := range []string{"first", "second"} {
		go func(key string) {
			ctx := merchant.WithID(context.Background(), merchant.ID(fx.merchant))
			ctx, release, err := fx.dbi.WithMerchantConn(ctx)
			if err != nil {
				results <- result{err: err}
				return
			}
			defer release()
			hr := httprequest.NewHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/refund", nil).WithContext(ctx), fx.rt)
			<-start
			_, status, err := executeAdminRefund(ctx, hr, payID, refundRequest{Amount: 10000000}, key)
			results <- result{status, err}
		}(key)
	}
	close(start)
	succeeded, refused := 0, 0
	for range 2 {
		got := <-results
		if got.err == nil {
			require.Equal(t, http.StatusCreated, got.status)
			succeeded++
		} else {
			status, _ := adminRefundErrorResponse(got.err)
			require.Equal(t, http.StatusBadRequest, status)
			refused++
		}
	}
	require.Equal(t, 1, succeeded)
	require.Equal(t, 1, refused)
	require.EqualValues(t, 1, fx.fake.refundCalls.Load())
	total, err := fx.rt.PaymentService.GetRefundTotalByPaymentID(fx.ctx, payID)
	require.NoError(t, err)
	require.EqualValues(t, 10000000, total)
}
