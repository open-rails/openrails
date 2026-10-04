package handlers

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/reconcile/recommend"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// or#863: a finding-approve refund amount is exact micros; a float is refused,
// never truncated, on every route the amount can arrive by.
func TestFindingAmountMicrosIsExact(t *testing.T) {
	const beyondFloat64 int64 = 9_007_199_254_740_993 // 2^53 + 1
	require.NotEqual(t, beyondFloat64, int64(float64(beyondFloat64)))

	for _, tc := range []struct {
		raw  any
		want int64
	}{
		{json.Number("9007199254740993"), beyondFloat64},
		{" 19990000 ", 19_990_000},
	} {
		got, err := paramAmountMicros(tc.raw)
		require.NoError(t, err)
		require.Equal(t, tc.want, got)
	}
	for _, bad := range []any{float64(60_000_000), float32(1), json.Number("60000000.5"), json.Number("0"), json.Number("-1"), "abc", true, nil, map[string]any{}} {
		_, err := paramAmountMicros(bad)
		require.Error(t, err, "%#v", bad)
	}

	overrides, err := recommend.DecodeParams([]byte(`{"amount": 9007199254740993}`))
	require.NoError(t, err)
	evidence, ok := recommend.FromEvidence(map[string]any{"recommendation": map[string]any{
		"action": recommend.ActionCancelAndRefund,
		"params": map[string]any{"amount": json.RawMessage(`9007199254740993`)},
	}})
	require.True(t, ok)
	for _, raw := range []any{overrides["amount"], evidence.Params["amount"]} {
		got, err := paramAmountMicros(raw)
		require.NoError(t, err)
		require.Equal(t, beyondFloat64, got)
	}
	empty, err := recommend.DecodeParams(nil)
	require.NoError(t, err)
	require.Nil(t, empty)
}

// #671: a refund converts to the rail's minor unit exactly or not at all.
func TestRefundAmountCentsIsExact(t *testing.T) {
	for _, tc := range []struct {
		currency string
		native   int64
		want     moneyutil.Cents
	}{
		{"USD", 60_000_000, 6000},
		{"USD", 10_000, 1},
		{"USD", 19_990_000, 1999},
		{"JPY", 10_000, 1},
	} {
		got, err := refundAmountCents(tc.currency, tc.native)
		require.NoError(t, err)
		require.Equal(t, tc.want, got, "%s %d", tc.currency, tc.native)
	}
	for _, tc := range []struct {
		currency string
		native   int64
	}{{"USD", 5_000}, {"USD", 60_000_001}, {"", 10_000}, {"XXX", 10_000}} {
		_, err := refundAmountCents(tc.currency, tc.native)
		require.Error(t, err, "%s %d", tc.currency, tc.native)
	}
}

// An admin refund replayed under its key matches only the exact same request,
// and its reservation id is scoped to (payment, trimmed key).
func TestAdminRefundIdempotencyIdentity(t *testing.T) {
	payment := uuid.New()
	id := adminRefundReservationTransactionID(payment, "refund-key")
	require.Equal(t, id, adminRefundReservationTransactionID(payment, " refund-key "))
	require.NotEqual(t, id, adminRefundReservationTransactionID(uuid.New(), "refund-key"))
	require.NotEqual(t, id, adminRefundReservationTransactionID(payment, "other-key"))

	original := RefundRequest{Amount: 500, Reason: "requested_by_customer", RevokeAccess: true}
	meta := adminRefundMetadata(" key-123 ", original, "completed", "re_123")
	require.Equal(t, "key-123", meta["admin_refund_idempotency_key"])
	require.Equal(t, "re_123", meta["provider_refund_id"])
	existing := &models.Payment{Amount: -500, Metadata: meta}

	require.True(t, adminRefundMatchesRequest(existing, RefundRequest{Amount: 500, Reason: " requested_by_customer ", RevokeAccess: true}))
	for _, changed := range []RefundRequest{
		{Amount: 400, Reason: original.Reason, RevokeAccess: true},
		{Amount: 500, Reason: "duplicate", RevokeAccess: true},
		{Amount: 500, Reason: original.Reason},
		{Amount: 500, Reason: original.Reason, RevokeAccess: true, Full: true},
	} {
		require.False(t, adminRefundMatchesRequest(existing, changed), "%+v", changed)
	}
	require.False(t, adminRefundMatchesRequest(nil, original))

	full := RefundRequest{Full: true}
	require.True(t, adminRefundMatchesRequest(&models.Payment{Amount: -999, Metadata: adminRefundMetadata("k", full, "completed", "")}, full),
		"a full refund matches whatever amount it resolved to")
}

func TestPaymentStatusAndRefundTotals(t *testing.T) {
	charge := func(status string) *models.Payment {
		return &models.Payment{ID: uuid.New(), Rail: models.RailNMI, Amount: 1000, Currency: "USD", Status: status, CreatedAt: time.Unix(100, 0)}
	}
	for status, want := range map[string]billing.PaymentStatus{"completed": billing.PaymentSucceeded, "pending": billing.PaymentPending, "failed": billing.PaymentFailed} {
		got := PaymentToAPI(charge(status), nil)
		require.Equal(t, billing.PaymentCharge, got.Kind)
		require.Equal(t, want, got.Status, status)
		require.Nil(t, got.Refunds, "a list item carries no refunds")
	}

	original := uuid.New()
	for status, want := range map[string]billing.PaymentStatus{"completed": billing.PaymentSucceeded, "pending": billing.PaymentPending, "failed": billing.PaymentFailed} {
		got := PaymentToAPI(&models.Payment{ID: uuid.New(), RefundedPaymentID: &original, Amount: -500, Currency: "USD", Status: status}, nil)
		require.Equal(t, billing.PaymentRefund, got.Kind)
		require.Equal(t, want, got.Status)
	}
	chargeback := "chargeback"
	require.Equal(t, billing.PaymentChargeback, PaymentToAPI(&models.Payment{ID: uuid.New(), Amount: -500, Status: "completed", ReversalKind: &chargeback}, nil).Kind)

	refund := func(amount int64, status string) *models.Payment {
		return &models.Payment{ID: uuid.New(), RefundedPaymentID: &original, Amount: amount, Currency: "USD", Status: status}
	}
	for _, tc := range []struct {
		name    string
		refunds []*models.Payment
		total   int64
		status  billing.PaymentStatus
	}{
		{"none", []*models.Payment{}, 0, billing.PaymentSucceeded},
		{"only completed refunds count", []*models.Payment{refund(-300, "completed"), refund(-400, "pending"), refund(-500, "failed")}, 300, billing.PaymentPartiallyRefunded},
		{"multiple full", []*models.Payment{refund(-300, "completed"), refund(-700, "completed")}, 1000, billing.PaymentRefunded},
		{"legacy positive amount", []*models.Payment{refund(300, "completed")}, 300, billing.PaymentPartiallyRefunded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := charge("completed")
			detail := PaymentToAPI(p, tc.refunds)
			require.Equal(t, []any{tc.total, tc.status}, []any{detail.AmountRefunded, detail.Status})
			require.Len(t, detail.Refunds, len(tc.refunds))
			history := paymentView(p, tc.total)
			require.Equal(t, []any{detail.AmountRefunded, detail.Status}, []any{history.AmountRefunded, history.Status}, "history and detail agree")
		})
	}
	failed := PaymentToAPI(charge("failed"), []*models.Payment{refund(-1000, "completed")})
	require.Equal(t, billing.PaymentFailed, failed.Status, "a failed charge never reads as refunded")
	require.NotNil(t, failed.Failure)

	manual := PaymentToAPI(&models.Payment{ID: uuid.New(), Rail: models.Rail(models.ChannelManual), Amount: 1000, Status: "completed"}, nil)
	require.Equal(t, billing.ChannelManual, manual.Channel)
	require.Nil(t, manual.Rail, "an off-rail payment names no rail")
}
