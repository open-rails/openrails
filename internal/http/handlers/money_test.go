package handlers

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/delinquency"
	"github.com/open-rails/openrails/internal/reconcile/recommend"
	billingservice "github.com/open-rails/openrails/internal/service"
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

	original := refundRequest{Amount: 500, Reason: "requested_by_customer", RevokeAccess: true}
	meta := adminRefundMetadata(" key-123 ", original, "completed", "re_123")
	require.Equal(t, "key-123", meta["admin_refund_idempotency_key"])
	require.Equal(t, "re_123", meta["provider_refund_id"])
	existing := &models.Payment{Amount: -500, Metadata: meta}

	require.True(t, adminRefundMatchesRequest(existing, refundRequest{Amount: 500, Reason: " requested_by_customer ", RevokeAccess: true}))
	for _, changed := range []refundRequest{
		{Amount: 400, Reason: original.Reason, RevokeAccess: true},
		{Amount: 500, Reason: "duplicate", RevokeAccess: true},
		{Amount: 500, Reason: original.Reason},
		{Amount: 500, Reason: original.Reason, RevokeAccess: true, Full: true},
	} {
		require.False(t, adminRefundMatchesRequest(existing, changed), "%+v", changed)
	}
	require.False(t, adminRefundMatchesRequest(nil, original))

	full := refundRequest{Full: true}
	require.True(t, adminRefundMatchesRequest(&models.Payment{Amount: -999, Metadata: adminRefundMetadata("k", full, "completed", "")}, full),
		"a full refund matches whatever amount it resolved to")
}

func TestPaymentStatusAndRefundTotals(t *testing.T) {
	charge := func(status string) *models.Payment {
		return &models.Payment{ID: uuid.New(), Rail: models.RailNMI, Amount: 1000, Currency: "USD", Status: status, CreatedAt: time.Unix(100, 0)}
	}
	for status, want := range map[string]struct {
		status   string
		captured bool
	}{"completed": {"succeeded", true}, "": {"succeeded", true}, "pending": {"pending", false}, "failed": {"failed", false}} {
		got := PaymentToAPI(charge(status), nil)
		require.Equal(t, "charge", got.Object)
		require.Equal(t, want.status, got.Status, status)
		require.Equal(t, want.captured, got.Captured, status)
	}

	original := uuid.New()
	for _, status := range []string{"", "pending", "failed"} {
		refund := &models.Payment{ID: uuid.New(), RefundedPaymentID: &original, Amount: -500, Currency: "USD", Status: status}
		got := PaymentToAPI(refund, nil)
		require.Equal(t, "refund", got.Object)
		require.False(t, got.Captured)
		require.Equal(t, map[string]string{"": "succeeded", "pending": "pending", "failed": "failed"}[status], got.Status)
	}

	refund := func(amount int64, status string) *models.Payment {
		return &models.Payment{ID: uuid.New(), RefundedPaymentID: &original, Amount: amount, Currency: "USD", Status: status}
	}
	for _, tc := range []struct {
		name     string
		refunds  []*models.Payment
		total    int64
		status   string
		refunded bool
	}{
		{"none", nil, 0, "succeeded", false},
		{"only completed refunds count", []*models.Payment{refund(-300, "completed"), refund(-400, "pending"), refund(-500, "failed")}, 300, "partially_refunded", false},
		{"multiple full", []*models.Payment{refund(-300, "completed"), refund(-700, "completed")}, 1000, "refunded", true},
		{"legacy positive amount", []*models.Payment{refund(300, "completed")}, 300, "partially_refunded", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := charge("completed")
			detail := PaymentToAPI(p, tc.refunds)
			require.Equal(t, []any{tc.total, tc.status, tc.refunded}, []any{detail.AmountRefunded, detail.Status, detail.Refunded})
			if tc.refunds != nil {
				require.Len(t, detail.Refunds.Data, len(tc.refunds))
			}
			history := PaymentToUserAPI(p, tc.total)
			require.Equal(t, []any{detail.AmountRefunded, detail.Status, detail.Refunded, detail.Captured},
				[]any{history.AmountRefunded, history.Status, history.Refunded, history.Captured}, "history and detail agree")
		})
	}
	failed := PaymentToAPI(charge("failed"), []*models.Payment{refund(-1000, "completed")})
	require.Equal(t, "failed", failed.Status, "a failed charge never reads as refunded")
}

// The delinquency roster spells instants at full RFC3339 precision, money as
// a decimal string (exact past 2^53) and the customer as a plain UUID.
func TestServiceDelinquencyRowsWire(t *testing.T) {
	customer := uuid.New()
	entered := time.Date(2026, 9, 16, 12, 0, 0, 123456789, time.FixedZone("x", 3600))
	since := entered.Add(-time.Hour)
	raw, err := json.Marshal(serviceDelinquencyRows([]billingservice.DelinquencySnapshot{{
		CustomerID: customer, Currency: "USD", State: delinquency.StateDelinquent, OverdueSince: &since,
		OverdueAmount: 9007199254740993, OverdueInvoices: 2, EnteredAt: entered, EvaluatedAt: entered,
	}}))
	require.NoError(t, err)
	var wire []map[string]any
	require.NoError(t, json.Unmarshal(raw, &wire))
	require.Equal(t, customer.String(), wire[0]["customer_id"])
	require.Equal(t, "9007199254740993", wire[0]["overdue_amount"])
	require.Equal(t, "2026-09-16T11:00:00.123456789Z", wire[0]["entered_at"])
	require.Equal(t, "2026-09-16T10:00:00.123456789Z", wire[0]["overdue_since"])
}
