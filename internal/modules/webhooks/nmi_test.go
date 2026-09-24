package webhooks

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/open-rails/openrails/pkg/merchant"
)

// GAP-1: NMI ids may arrive as bare JSON numbers; they must never pass through float64.
func TestStringishKeepsExactDigits(t *testing.T) {
	t.Parallel()
	for raw, want := range map[string]string{
		`"sub-1"`:            "sub-1",
		`9007199254740993`:   "9007199254740993",
		`12.0`:               "12",
		`12.50`:              "12.50",
		`1e3`:                "1e3",
		`true`:               "true",
		`null`:               "",
		`" padded "`:         " padded ",
		`123456789012345678`: "123456789012345678",
	} {
		var s Stringish
		require.NoError(t, json.Unmarshal([]byte(raw), &s), raw)
		require.Equal(t, want, s.String(), raw)
	}
	var i Intish
	require.NoError(t, json.Unmarshal([]byte(`" 7 "`), &i))
	require.Equal(t, 7, i.Int())
	require.Error(t, json.Unmarshal([]byte(`"7.5"`), &i))
}

func TestNMITransactionSubscriptionReference(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		body *NMITransactionEventBody
		want string
	}{
		{"subscription wins", &NMITransactionEventBody{Subscription: &NMISubscriptionRef{SubscriptionID: " sub-1 "}, OrderID: "order"}, "sub-1"},
		{"detail subscription", &NMITransactionEventBody{TransactionDetail: &NMITransactionDetail{Subscription: &NMISubscriptionRef{SubscriptionID: "sub-d"}, OrderID: "order"}}, "sub-d"},
		{"order id", &NMITransactionEventBody{OrderID: " order-1 ", PONumber: "po"}, "order-1"},
		{"detail order id", &NMITransactionEventBody{TransactionDetail: &NMITransactionDetail{OrderID: " detail-order "}}, "detail-order"},
		{"po number", &NMITransactionEventBody{PONumber: " po-1 "}, "po-1"},
		{"detail po number", &NMITransactionEventBody{TransactionDetail: &NMITransactionDetail{PONumber: " detail-po "}}, "detail-po"},
		{"blank subscription falls through", &NMITransactionEventBody{Subscription: &NMISubscriptionRef{SubscriptionID: "  "}, OrderID: "order-2"}, "order-2"},
		{"customer id is never a reference", &NMITransactionEventBody{CustomerID: "cust"}, ""},
		{"nil", nil, ""},
	} {
		require.Equal(t, tc.want, transactionSubscriptionID(tc.body), tc.name)
	}
}

// MONEY-6 at the NMI edge: decimal strings round half away from zero, first parseable candidate wins.
func TestNMITransactionAmountCents(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		body *NMITransactionEventBody
		want moneyutil.Cents
		err  bool
	}{
		{"top-level", &NMITransactionEventBody{Amount: "19.99", TransactionDetail: &NMITransactionDetail{Amount: "1.00"}}, 1999, false},
		{"malformed falls back to detail", &NMITransactionEventBody{Amount: "not-a-number", TransactionDetail: &NMITransactionDetail{Amount: "12.34"}}, 1234, false},
		{"blank falls back, half rounds up", &NMITransactionEventBody{TransactionDetail: &NMITransactionDetail{Amount: "7.255"}}, 726, false},
		{"action amount", &NMITransactionEventBody{Action: &NMIAction{Amount: "1.005"}}, 101, false},
		{"detail action amount", &NMITransactionEventBody{TransactionDetail: &NMITransactionDetail{Action: &NMIAction{Amount: "10.014"}}}, 1001, false},
		{"negative rounds away from zero", &NMITransactionEventBody{Amount: "-1.005"}, -101, false},
		{"only malformed", &NMITransactionEventBody{Amount: "abc"}, 0, true},
		{"none", &NMITransactionEventBody{}, 0, true},
		{"nil", nil, 0, true},
	} {
		got, err := transactionAmountCents(tc.body)
		if tc.err {
			require.Error(t, err, tc.name)
			continue
		}
		require.NoError(t, err, tc.name)
		require.Equal(t, tc.want, got, tc.name)
	}
}

func TestNMIAmountMatchesExpected(t *testing.T) {
	t.Parallel()
	const price = moneyutil.Micros(19_990_000) // 1999 cents, tolerance 39
	for billed, ok := range map[moneyutil.Cents]bool{1999: true, 1960: true, 2038: true, 1959: false, 2039: false, 0: false} {
		require.Equal(t, ok, nmiAmountMatchesExpected("USD", billed, price), billed)
	}
	require.True(t, nmiAmountMatchesExpected("USD", 5, 0), "no expected amount: nothing to compare")
	require.False(t, nmiAmountMatchesExpected("USD", 1999, 19_995_000), "sub-cent expectation never matches")
}

func TestNMIChargebackFieldParsing(t *testing.T) {
	t.Parallel()
	require.Equal(t, "1111", normalizeNMIChargebackLast4("411111******1111"))
	require.Equal(t, "1111", normalizeNMIChargebackLast4(" 1111 "))
	require.Empty(t, normalizeNMIChargebackLast4("****"))
	require.Empty(t, normalizeNMIChargebackLast4("x123"))

	amount, err := parseNMIChargebackAmountCents("11.11")
	require.NoError(t, err)
	require.EqualValues(t, 1111, amount)
	for _, bad := range []string{"", "0.00", "-1.00", "abc"} {
		_, err := parseNMIChargebackAmountCents(bad)
		require.Error(t, err, bad)
	}

	for raw, want := range map[string]time.Time{
		"3/29/2020":            time.Date(2020, 3, 29, 0, 0, 0, 0, time.UTC),
		"2020-03-29":           time.Date(2020, 3, 29, 0, 0, 0, 0, time.UTC),
		"20200329":             time.Date(2020, 3, 29, 0, 0, 0, 0, time.UTC),
		"2020-03-29T10:00:00Z": time.Date(2020, 3, 29, 10, 0, 0, 0, time.UTC),
	} {
		ts, ok := parseNMIChargebackDate(raw)
		require.True(t, ok, raw)
		require.True(t, want.Equal(ts), raw)
	}
	for _, bad := range []string{"", "not-a-date"} {
		_, ok := parseNMIChargebackDate(bad)
		require.False(t, ok, bad)
	}

	for _, tc := range []struct{ reason, code, wantCode, wantReason string }{
		{"101: Introductory chargeback", "", "101", "Introductory chargeback"},
		{"Introductory chargeback", "204", "204", "Introductory chargeback"},
		{"101: kept", "204", "204", "101: kept"},
		{"A1: not numeric", "", "", "A1: not numeric"},
		{" plain ", "", "", "plain"},
	} {
		code, reason := splitNMIChargebackReason(tc.reason, tc.code)
		require.Equal(t, tc.wantCode, code, tc.reason)
		require.Equal(t, tc.wantReason, reason, tc.reason)
	}
}

func TestNMIDelayedStart(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 6, 17, 0, 0, 0, 0, time.UTC)
	meta := json.RawMessage(`{"delayed_start":"` + start.Format(time.RFC3339) + `"}`)
	require.Equal(t, start, *nmiDelayedStartFromSubscriptionMetadata(meta))
	for _, bad := range []string{``, `{`, `{}`, `{"delayed_start":"not-a-time"}`, `{"delayed_start":17}`} {
		require.Nil(t, nmiDelayedStartFromSubscriptionMetadata(json.RawMessage(bad)), bad)
	}
	require.Equal(t, start, *nmiFutureDelayedStart(meta, start.Add(-time.Hour)))
	require.Nil(t, nmiFutureDelayedStart(meta, start), "a start at now is not in the future")
	require.Nil(t, nmiFutureDelayedStart(meta, start.Add(time.Hour)))
}

type recordingConvergeEnqueuer struct{ requests []ConvergeRequest }

func (e *recordingConvergeEnqueuer) EnqueueSubscriptionConverge(_ context.Context, req ConvergeRequest) error {
	e.requests = append(e.requests, req)
	return nil
}

// #684: NMI subscription events only mark the subscription dirty for fetch-and-converge.
func TestNMIWebhookMarksSubscriptionDirty(t *testing.T) {
	t.Parallel()
	merchantID, pspID := uuid.New(), uuid.New()
	ctx := db.WithPSPID(merchant.WithID(context.Background(), merchant.ID(merchantID)), pspID)
	svc := func(eventType, body string, enq SubscriptionConvergeEnqueuer) *NMIWebhookService {
		return &NMIWebhookService{Rail: "nmi", ConvergeEnqueuer: enq, Data: NMIWebhookEvent{EventID: uuid.NewString(), EventType: eventType, EventBody: json.RawMessage(body)}}
	}

	for _, tc := range []struct{ eventType, body, ref string }{
		{EventTypeNMIAddSubscription, `{"subscription_id":9007199254740993}`, "9007199254740993"},
		{EventTypeNMIUpdateSubscription, `{"subscription_id":" sub_r "}`, "sub_r"},
		{EventTypeNMIDeleteSubscription, `{"subscription_id":"sub_d"}`, "sub_d"},
		{EventTypeNMITransactionSuccess, `{"order_id":" sub_t "}`, "sub_t"},
		{EventTypeNMITransactionFailure, `{"transaction":{"subscription":{"subscription_id":"sub_f"}}}`, "sub_f"},
	} {
		enq := &recordingConvergeEnqueuer{}
		require.NoError(t, svc(tc.eventType, tc.body, enq).HandleNMIWebhook(ctx), tc.eventType)
		require.Equal(t, []ConvergeRequest{{MerchantID: merchantID, PSPID: pspID, Rail: "nmi", SubscriptionReference: tc.ref, EventType: tc.eventType}}, enq.requests, tc.eventType)
	}

	for name, s := range map[string]*NMIWebhookService{
		"recurring without id":          svc(EventTypeNMIUpdateSubscription, `{}`, &recordingConvergeEnqueuer{}),
		"transaction without reference": svc(EventTypeNMITransactionSuccess, `{"customerid":"c"}`, &recordingConvergeEnqueuer{}),
		"unsupported event":             svc("unsupported", `{}`, &recordingConvergeEnqueuer{}),
	} {
		err := s.HandleNMIWebhook(ctx)
		require.Error(t, err, name)
		require.True(t, IsWebhookErrorNonRetryable(err), name)
	}

	// Missing infrastructure is retryable: the provider redelivers and nothing is lost.
	for name, tc := range map[string]struct {
		ctx context.Context
		enq SubscriptionConvergeEnqueuer
	}{
		"no enqueuer": {ctx, nil},
		"no merchant": {db.WithPSPID(context.Background(), pspID), &recordingConvergeEnqueuer{}},
		"no psp":      {merchant.WithID(context.Background(), merchant.ID(merchantID)), &recordingConvergeEnqueuer{}},
	} {
		err := svc(EventTypeNMIUpdateSubscription, `{"subscription_id":"sub"}`, tc.enq).HandleNMIWebhook(tc.ctx)
		require.Error(t, err, name)
		require.False(t, IsWebhookErrorNonRetryable(err), name)
	}
}

func TestNMIChargebackBatchRequiresRail(t *testing.T) {
	t.Parallel()
	svc := &NMIWebhookService{Data: NMIWebhookEvent{EventID: uuid.NewString(), EventType: EventTypeNMIChargebackComplete, EventBody: json.RawMessage(`{"chargebacks":[],"batch":{"count":0}}`)}}
	require.ErrorContains(t, svc.HandleNMIWebhook(context.Background()), "nmi webhook rail is required")
}
