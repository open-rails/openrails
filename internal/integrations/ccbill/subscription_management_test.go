package ccbill

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// #696 wire-pinning tests. The SMS wire shape is PROVISIONAL (Phase 0 live
// probe pending); these pins make any post-probe adjustment a deliberate,
// visible diff instead of a silent drift.

func newSMSTestClient(baseURL string) *DataLinkClient {
	return &DataLinkClient{
		BaseURL:      baseURL,
		ClientAccNum: "900100",
		ClientSubAcc: "0000",
		Username:     "dluser",
		Password:     "dlpass",
		HTTPClient:   &http.Client{Timeout: 5 * time.Second},
	}
}

// Known inputs => exact encoded request (url.Values.Encode sorts keys).
func TestSubscriptionManagementForm_WirePinning(t *testing.T) {
	c := newSMSTestClient("https://datalink.ccbill.com")
	form := c.subscriptionManagementForm(actionCancelSubscription, "0123456789")
	assert.Equal(t,
		"action=cancelSubscription&clientAccnum=900100&clientSubacc=0000&password=dlpass&returnXML=1&subscriptionId=0123456789&username=dluser",
		form.Encode())

	view := c.subscriptionManagementForm(actionViewSubscriptionStatus, "0123456789")
	assert.Equal(t,
		"action=viewSubscriptionStatus&clientAccnum=900100&clientSubacc=0000&password=dlpass&returnXML=1&subscriptionId=0123456789&username=dluser",
		view.Encode())

	// No subaccount configured -> clientSubacc omitted entirely.
	c.ClientSubAcc = ""
	assert.Equal(t,
		"action=cancelSubscription&clientAccnum=900100&password=dlpass&returnXML=1&subscriptionId=0123456789&username=dluser",
		c.subscriptionManagementForm(actionCancelSubscription, "0123456789").Encode())
}

// smsCapture records the last SMS request and serves a scripted body.
type smsCapture struct {
	hits   atomic.Int64
	path   atomic.Value // string
	body   atomic.Value // string (last request form body)
	status atomic.Int64 // 0 = 200
	answer atomic.Value // string response body
}

func newSMSServer(t *testing.T) (*smsCapture, *DataLinkClient) {
	t.Helper()
	cap := &smsCapture{}
	cap.answer.Store("")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.hits.Add(1)
		cap.path.Store(r.URL.Path)
		require.NoError(t, r.ParseForm())
		cap.body.Store(r.PostForm.Encode())
		if st := cap.status.Load(); st != 0 {
			w.WriteHeader(int(st))
		}
		_, _ = w.Write([]byte(cap.answer.Load().(string)))
	}))
	t.Cleanup(srv.Close)
	return cap, newSMSTestClient(srv.URL)
}

// TestViewSubscriptionStatus_ProductionGolden pins the EXACT documents captured
// from the live production DataLink account (#696 Phase 0, 2026-07-03) — the
// verified wire shape, so any silent drift breaks here.
func TestViewSubscriptionStatus_ProductionGolden(t *testing.T) {
	t.Run("active recurring (status 2)", func(t *testing.T) {
		cap, c := newSMSServer(t)
		cap.answer.Store(`<?xml version='1.0' standalone='yes'?>
<results>
  <chargebacksIssued>0</chargebacksIssued>
  <nextBillingDate>20260801</nextBillingDate>
  <recurringSubscription>1</recurringSubscription>
  <refundsIssued>0</refundsIssued>
  <signupDate>20160724003047</signupDate>
  <subscriptionStatus>2</subscriptionStatus>
  <timesRebilled>121</timesRebilled>
  <voidsIssued>0</voidsIssued>
</results>`)
		res, err := c.ViewSubscriptionStatus(context.Background(), "116206701000000779")
		require.NoError(t, err)
		assert.Equal(t, "2", res.RawStatus)
		rebilling, err := res.Rebilling()
		require.NoError(t, err)
		assert.True(t, rebilling)
		active, err := res.Active()
		require.NoError(t, err)
		assert.True(t, active)
		exp, ok := res.ExpiresAt()
		require.True(t, ok, "nextBillingDate parses")
		assert.Equal(t, time.Date(2026, 8, 1, 23, 59, 59, 0, time.UTC), exp)
	})

	t.Run("dead / expired (status 0)", func(t *testing.T) {
		cap, c := newSMSServer(t)
		cap.answer.Store(`<?xml version='1.0' standalone='yes'?>
<results>
  <cancelDate>20130131123315</cancelDate>
  <expirationDate>20130131123315</expirationDate>
  <recurringSubscription>1</recurringSubscription>
  <refundsIssued>0</refundsIssued>
  <returnsIssued>1</returnsIssued>
  <signupDate>20130127160132</signupDate>
  <subscriptionStatus>0</subscriptionStatus>
  <timesRebilled>0</timesRebilled>
</results>`)
		res, err := c.ViewSubscriptionStatus(context.Background(), "113027706000000428")
		require.NoError(t, err)
		assert.Equal(t, "0", res.RawStatus)
		rebilling, err := res.Rebilling()
		require.NoError(t, err)
		assert.False(t, rebilling, "dead sub never rebills despite recurringSubscription=1")
		active, err := res.Active()
		require.NoError(t, err)
		assert.False(t, active)
		exp, ok := res.ExpiresAt()
		require.True(t, ok, "14-digit expirationDate parses")
		assert.Equal(t, time.Date(2013, 1, 31, 12, 33, 15, 0, time.UTC), exp)
	})
}

func TestViewSubscriptionStatus_ParsesCapturedStyleXML(t *testing.T) {
	cap, c := newSMSServer(t)
	cap.answer.Store(`<?xml version="1.0"?>
<results>
  <subscriptionId>0123456789</subscriptionId>
  <clientAccnum>900100</clientAccnum>
  <subscriptionStatus>2</subscriptionStatus>
  <recurringSubscription>1</recurringSubscription>
  <expirationDate>20260815</expirationDate>
  <someFutureField>ignored-but-preserved</someFutureField>
</results>`)

	res, err := c.ViewSubscriptionStatus(context.Background(), "0123456789")
	require.NoError(t, err)
	assert.Equal(t, "/utils/subscriptionManagement.cgi", cap.path.Load())
	assert.Equal(t,
		"action=viewSubscriptionStatus&clientAccnum=900100&clientSubacc=0000&password=dlpass&returnXML=1&subscriptionId=0123456789&username=dluser",
		cap.body.Load())

	assert.Equal(t, "2", res.RawStatus)
	assert.Equal(t, "ignored-but-preserved", res.Fields["someFutureField"], "unknown fields preserved verbatim")
	rebilling, err := res.Rebilling()
	require.NoError(t, err)
	assert.True(t, rebilling)
	active, err := res.Active()
	require.NoError(t, err)
	assert.True(t, active)
	exp, ok := res.ExpiresAt()
	require.True(t, ok)
	assert.Equal(t, time.Date(2026, 8, 15, 23, 59, 59, 0, time.UTC), exp)
}

func TestViewSubscriptionStatus_StatusVocabulary(t *testing.T) {
	cases := []struct {
		status        string
		rebilling     bool
		active        bool
		vocabularyErr bool
	}{
		{"2", true, true, false},
		{"1", false, true, false},
		{"0", false, false, false},
		{"7", false, false, true}, // unrecognized: error, never a guessed default (#651)
	}
	for _, tc := range cases {
		res := SubscriptionStatusResult{RawStatus: tc.status}
		rebilling, err := res.Rebilling()
		active, aerr := res.Active()
		if tc.vocabularyErr {
			assert.Error(t, err, tc.status)
			assert.Error(t, aerr, tc.status)
			continue
		}
		require.NoError(t, err, tc.status)
		require.NoError(t, aerr, tc.status)
		assert.Equal(t, tc.rebilling, rebilling, tc.status)
		assert.Equal(t, tc.active, active, tc.status)
	}
}

func TestViewSubscriptionStatus_MissingStatusIsError(t *testing.T) {
	cap, c := newSMSServer(t)
	cap.answer.Store(`<results><subscriptionId>0123456789</subscriptionId></results>`)
	_, err := c.ViewSubscriptionStatus(context.Background(), "0123456789")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing subscriptionStatus")
}

func TestViewSubscriptionStatus_BareCodeAndResultsCodeAreErrors(t *testing.T) {
	cap, c := newSMSServer(t)
	cap.answer.Store(`-3`)
	_, err := c.ViewSubscriptionStatus(context.Background(), "0123456789")
	require.Error(t, err)
	assert.Contains(t, err.Error(), `bare code "-3"`)

	cap.answer.Store(`<results>-3</results>`)
	_, err = c.ViewSubscriptionStatus(context.Background(), "0123456789")
	require.Error(t, err)
	assert.Contains(t, err.Error(), `results="-3"`)
}

func TestViewSubscriptionStatus_AuthClasses(t *testing.T) {
	cap, c := newSMSServer(t)
	cap.status.Store(http.StatusUnauthorized)
	_, err := c.ViewSubscriptionStatus(context.Background(), "0123456789")
	require.ErrorIs(t, err, ErrDataLinkAuth)

	cap.status.Store(0)
	cap.answer.Store(`Error: authentication failed`)
	_, err = c.ViewSubscriptionStatus(context.Background(), "0123456789")
	require.ErrorIs(t, err, ErrDataLinkAuth)
}

func TestCancelSubscription_SuccessShapes(t *testing.T) {
	cap, c := newSMSServer(t)
	cap.answer.Store(`<results>1</results>`)
	res, err := c.CancelSubscription(context.Background(), "0123456789")
	require.NoError(t, err)
	assert.Equal(t, "1", res.Results)
	assert.Equal(t,
		"action=cancelSubscription&clientAccnum=900100&clientSubacc=0000&password=dlpass&returnXML=1&subscriptionId=0123456789&username=dluser",
		cap.body.Load())

	// Legacy bare-token answer (returnXML ignored).
	cap.answer.Store("1")
	res, err = c.CancelSubscription(context.Background(), "0123456789")
	require.NoError(t, err)
	assert.Equal(t, "1", res.Results)

	// Quoted CSV-style token.
	cap.answer.Store(`"1"`)
	_, err = c.CancelSubscription(context.Background(), "0123456789")
	require.NoError(t, err)
}

func TestCancelSubscription_RejectAndAuthClasses(t *testing.T) {
	cap, c := newSMSServer(t)

	cap.answer.Store(`<results>0</results>`)
	res, err := c.CancelSubscription(context.Background(), "0123456789")
	require.ErrorIs(t, err, ErrCancelRejected)
	assert.Equal(t, "0", res.Results, "verbatim reject code carried")

	cap.answer.Store(`-1`)
	_, err = c.CancelSubscription(context.Background(), "0123456789")
	require.ErrorIs(t, err, ErrCancelRejected)

	cap.status.Store(http.StatusForbidden)
	_, err = c.CancelSubscription(context.Background(), "0123456789")
	require.ErrorIs(t, err, ErrDataLinkAuth)
	cap.status.Store(0)

	// Unparseable multi-line garbage: NOT a definite reject — generic error
	// (the caller must verify, never assume).
	cap.answer.Store("weird\nmultiline\nanswer")
	_, err = c.CancelSubscription(context.Background(), "0123456789")
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrCancelRejected)
	assert.NotErrorIs(t, err, ErrDataLinkAuth)
}

// mode=readonly blocks the mutation at the transport: zero HTTP traffic.
func TestCancelSubscription_ReadOnlyBlocksAtTransport(t *testing.T) {
	cap, c := newSMSServer(t)
	c.ReadOnly = true
	_, err := c.CancelSubscription(context.Background(), "0123456789")
	require.ErrorIs(t, err, ErrProviderReadOnly)
	assert.Zero(t, cap.hits.Load(), "readonly cancel must never touch the wire")

	// Reads stay available under readonly.
	cap.answer.Store(`<results><subscriptionStatus>2</subscriptionStatus></results>`)
	_, err = c.ViewSubscriptionStatus(context.Background(), "0123456789")
	require.NoError(t, err)
	assert.EqualValues(t, 1, cap.hits.Load())
}

// #696 refund leg. WIRE PROVISIONAL — these pins lock the modeled request/
// response shape so any post-first-real-refund correction is a visible diff.

// Known inputs => exact encoded refund request (url.Values.Encode sorts keys).
// The amount is DECIMAL major units (9.99), NOT integer cents — see refundForm.
func TestRefundTransactionForm_WirePinning(t *testing.T) {
	c := newSMSTestClient("https://datalink.ccbill.com")

	// Partial refund: transactionId + decimal amount both present.
	partial := c.refundForm(actionVoidOrRefundTransaction, "0123456789", "TX999", moneyutil.Cents(999))
	assert.Equal(t,
		"action=voidOrRefundTransaction&amount=9.99&clientAccnum=900100&clientSubacc=0000&password=dlpass&returnXML=1&subscriptionId=0123456789&transactionId=TX999&username=dluser",
		partial.Encode())

	// Zero amount => whole-transaction refund, amount key omitted.
	whole := c.refundForm(actionVoidOrRefundTransaction, "0123456789", "TX999", 0)
	assert.Equal(t,
		"action=voidOrRefundTransaction&clientAccnum=900100&clientSubacc=0000&password=dlpass&returnXML=1&subscriptionId=0123456789&transactionId=TX999&username=dluser",
		whole.Encode())

	// 6000 cents => "60.00".
	assert.Equal(t, "60.00",
		c.refundForm(actionVoidOrRefundTransaction, "s", "t", moneyutil.Cents(6000)).Get("amount"))
}

func TestRefundTransaction_SuccessAndClassification(t *testing.T) {
	t.Run("success (results=1) hits the refund endpoint with the refund form", func(t *testing.T) {
		cap, c := newSMSServer(t)
		cap.answer.Store(`<results>1</results>`)
		res, err := c.RefundTransaction(context.Background(), "0123456789", "TX999", moneyutil.Cents(999))
		require.NoError(t, err)
		assert.Equal(t, "1", res.Results)
		assert.Equal(t, actionVoidOrRefundTransaction, res.Action)
		assert.Equal(t, "/utils/subscriptionManagement.cgi", cap.path.Load())
		assert.Equal(t,
			"action=voidOrRefundTransaction&amount=9.99&clientAccnum=900100&clientSubacc=0000&password=dlpass&returnXML=1&subscriptionId=0123456789&transactionId=TX999&username=dluser",
			cap.body.Load())
	})

	t.Run("auth class (-7) is ErrDataLinkAuth (did not execute)", func(t *testing.T) {
		cap, c := newSMSServer(t)
		cap.answer.Store(`<results>-7</results>`)
		_, err := c.RefundTransaction(context.Background(), "s", "t", 0)
		require.ErrorIs(t, err, ErrDataLinkAuth)
		assert.NotErrorIs(t, err, ErrRefundRejected)
	})

	t.Run("definite reject (results=0) is ErrRefundRejected (verify, never decline)", func(t *testing.T) {
		cap, c := newSMSServer(t)
		cap.answer.Store(`<results>0</results>`)
		res, err := c.RefundTransaction(context.Background(), "s", "t", 0)
		require.ErrorIs(t, err, ErrRefundRejected)
		assert.Equal(t, "0", res.Results)
	})

	t.Run("unparseable multi-line answer is ambiguous, not a definite reject", func(t *testing.T) {
		cap, c := newSMSServer(t)
		cap.answer.Store("weird\nmultiline\nanswer")
		_, err := c.RefundTransaction(context.Background(), "s", "t", 0)
		require.Error(t, err)
		assert.NotErrorIs(t, err, ErrRefundRejected)
		assert.NotErrorIs(t, err, ErrDataLinkAuth)
	})
}

// mode=readonly blocks the refund at the transport: zero HTTP traffic.
func TestRefundTransaction_ReadOnlyBlocksAtTransport(t *testing.T) {
	cap, c := newSMSServer(t)
	c.ReadOnly = true
	_, err := c.RefundTransaction(context.Background(), "s", "t", moneyutil.Cents(999))
	require.ErrorIs(t, err, ErrProviderReadOnly)
	assert.Zero(t, cap.hits.Load(), "readonly refund must never touch the wire")
}

func TestRefundsAndVoidsIssued(t *testing.T) {
	// Active-sub golden: both counters present and zero -> known, total 0.
	total, known := SubscriptionStatusResult{Fields: map[string]string{
		"refundsIssued": "0", "voidsIssued": "0",
	}}.RefundsAndVoidsIssued()
	assert.True(t, known)
	assert.EqualValues(t, 0, total)

	// A refund recorded: refunds=1, voids=2 -> total 3.
	total, known = SubscriptionStatusResult{Fields: map[string]string{
		"refundsIssued": "1", "voidsIssued": "2",
	}}.RefundsAndVoidsIssued()
	assert.True(t, known)
	assert.EqualValues(t, 3, total)

	// returnsIssued (chargebacks) is NOT counted as a merchant refund.
	total, known = SubscriptionStatusResult{Fields: map[string]string{
		"refundsIssued": "0", "returnsIssued": "5",
	}}.RefundsAndVoidsIssued()
	assert.True(t, known)
	assert.EqualValues(t, 0, total)

	// refundsIssued absent (some dead-sub responses) -> not known.
	_, known = SubscriptionStatusResult{Fields: map[string]string{
		"returnsIssued": "1",
	}}.RefundsAndVoidsIssued()
	assert.False(t, known)
}

func TestSubscriptionStatusResult_ExpiresAtFormats(t *testing.T) {
	// VERIFIED production formats (#696 Phase 0): 8-digit YYYYMMDD
	// (nextBillingDate) and 14-digit YYYYMMDDHHMMSS (expirationDate). A
	// date-only value pins to end-of-day UTC (webhook convention).
	cases := []struct {
		field string
		raw   string
		want  time.Time
	}{
		{"expirationDate", "20130131123315", time.Date(2013, 1, 31, 12, 33, 15, 0, time.UTC)},
		{"nextBillingDate", "20260801", time.Date(2026, 8, 1, 23, 59, 59, 0, time.UTC)},
	}
	for _, tc := range cases {
		res := SubscriptionStatusResult{Fields: map[string]string{tc.field: tc.raw}}
		got, ok := res.ExpiresAt()
		require.True(t, ok, tc.raw)
		assert.Equal(t, tc.want, got, tc.raw)
	}
	// nextBillingDate wins over expirationDate when both present (active sub).
	both := SubscriptionStatusResult{Fields: map[string]string{
		"nextBillingDate": "20260801", "expirationDate": "20130131123315",
	}}
	got, ok := both.ExpiresAt()
	require.True(t, ok)
	assert.Equal(t, time.Date(2026, 8, 1, 23, 59, 59, 0, time.UTC), got)
	// Absent / unparseable -> no fabricated date.
	_, ok = SubscriptionStatusResult{Fields: map[string]string{}}.ExpiresAt()
	assert.False(t, ok)
	_, ok = SubscriptionStatusResult{Fields: map[string]string{"expirationDate": "not-a-date"}}.ExpiresAt()
	assert.False(t, ok)
}
