package webhooks

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/stretchr/testify/require"
)

func TestTransactionSubscriptionID_Fallbacks(t *testing.T) {
	tests := []struct {
		name string
		body *NMITransactionEventBody
		want string
	}{
		{
			name: "subscription reference",
			body: &NMITransactionEventBody{
				Subscription: &NMISubscriptionRef{SubscriptionID: " sub-123 "},
			},
			want: "sub-123",
		},
		/*{
			name: "transaction detail subscription",
			body: &NMITransactionEventBody{
				TransactionDetail: &NMITransactionDetail{
					Subscription: &NMISubscriptionRef{SubscriptionID: " detail-456 "},
				},
			},
			want: "detail-456",
		},*/
		{
			name: "order id fallback",
			body: &NMITransactionEventBody{OrderID: " order-789 "},
			want: "order-789",
		},
		{
			name: "transaction detail order id fallback",
			body: &NMITransactionEventBody{
				TransactionDetail: &NMITransactionDetail{OrderID: " detail-order "},
			},
			want: "detail-order",
		},
		{
			name: "po number fallback",
			body: &NMITransactionEventBody{PONumber: " po-001 "},
			want: "po-001",
		},
		{
			name: "transaction detail po number fallback",
			body: &NMITransactionEventBody{
				TransactionDetail: &NMITransactionDetail{PONumber: " detail-po "},
			},
			want: "detail-po",
		},
		/*{
			name: "customer id fallback",
			body: &NMITransactionEventBody{CustomerID: " cust-002 "},
			want: "cust-002",
		},
		{
			name: "transaction detail customer id fallback",
			body: &NMITransactionEventBody{
				TransactionDetail: &NMITransactionDetail{CustomerID: " detail-cust "},
			},
			want: "detail-cust",
		},*/
		{
			name: "empty payload",
			body: &NMITransactionEventBody{},
			want: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, transactionSubscriptionID(tc.body))
		})
	}
}

func TestTransactionActionSource(t *testing.T) {
	require.Equal(t, "recurring", transactionActionSource(&NMITransactionEventBody{
		Action: &NMIAction{Source: "Recurring"},
	}))

	require.Equal(t, "retry", transactionActionSource(&NMITransactionEventBody{
		TransactionDetail: &NMITransactionDetail{
			Action: &NMIAction{Source: "Retry"},
		},
	}))

	require.Equal(t, "", transactionActionSource(&NMITransactionEventBody{}))
}

func TestIsRecurringSource(t *testing.T) {
	require.True(t, isRecurringSource("recurring"))
	require.True(t, isRecurringSource("RETRY"))
	require.False(t, isRecurringSource("api"))
	require.False(t, isRecurringSource(""))
}

func TestNormalizeNMIChargebackLast4(t *testing.T) {
	require.Equal(t, "1111", normalizeNMIChargebackLast4("411111******1111"))
	require.Equal(t, "1111", normalizeNMIChargebackLast4(" 1111 "))
	require.Equal(t, "", normalizeNMIChargebackLast4("****"))
}

func TestSplitNMIChargebackReason(t *testing.T) {
	code, reason := splitNMIChargebackReason("101: Introductory chargeback", "")
	require.Equal(t, "101", code)
	require.Equal(t, "Introductory chargeback", reason)

	code, reason = splitNMIChargebackReason("Introductory chargeback", "204")
	require.Equal(t, "204", code)
	require.Equal(t, "Introductory chargeback", reason)
}

func TestHandleChargebackComplete_RequiresProcessor(t *testing.T) {
	body, err := json.Marshal(NMIChargebackBatchEventBody{
		Chargebacks: []NMIChargebackEntry{},
		Batch: &NMIChargebackBatch{
			Count: 0,
		},
	})
	require.NoError(t, err)

	svc := &NMIWebhookService{
		Data: NMIWebhookEvent{
			EventID:   uuid.New().String(),
			EventType: string(EventTypeNMIChargebackComplete),
			EventBody: body,
		},
	}

	err = svc.handleChargebackComplete(context.Background())
	require.Error(t, err)
	require.ErrorContains(t, err, "nmi webhook processor is required")
}

func TestParseNMIChargebackDate(t *testing.T) {
	ts, ok := parseNMIChargebackDate("3/29/2020")
	require.True(t, ok)
	require.Equal(t, time.Date(2020, time.March, 29, 0, 0, 0, 0, time.UTC), ts)

	_, ok = parseNMIChargebackDate("not-a-date")
	require.False(t, ok)
}

func TestParseNMIChargebackAmountCents(t *testing.T) {
	amount, err := parseNMIChargebackAmountCents("11.11")
	require.NoError(t, err)
	require.EqualValues(t, 1111, amount)

	_, err = parseNMIChargebackAmountCents("")
	require.Error(t, err)
}

func TestParseDecimalToCents(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    int64
		wantErr bool
	}{
		{name: "whole dollars", raw: "10", want: 1000},
		{name: "two decimals", raw: "10.01", want: 1001},
		{name: "three decimals rounds up", raw: "10.015", want: 1002},
		{name: "three decimals rounds down", raw: "10.014", want: 1001},
		{name: "exact half up", raw: "1.005", want: 101},
		{name: "negative rounds away from zero", raw: "-1.005", want: -101},
		{name: "invalid", raw: "abc", wantErr: true},
		{name: "empty", raw: "", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := moneyutil.ParseDecimalToCents(tc.raw)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestTransactionAmountCents_UsesFallbackFields(t *testing.T) {
	body := &NMITransactionEventBody{
		Amount: "",
		TransactionDetail: &NMITransactionDetail{
			Amount: "7.255",
		},
	}

	amount, err := transactionAmountCents(body)
	require.NoError(t, err)
	require.EqualValues(t, 726, amount)
}
