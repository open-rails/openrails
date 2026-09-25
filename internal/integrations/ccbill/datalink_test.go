package ccbill

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/providerposture"
)

type dataLinkCall struct {
	Path string
	Form url.Values
}

// dataLinkFake is CCBill DataLink: main.cgi reports and the Subscription
// Management System, answering a scripted status and body.
type dataLinkFake struct {
	*httptest.Server
	mu     sync.Mutex
	calls  []dataLinkCall
	status int
	body   string
}

func newDataLinkFake(t *testing.T, status int, body string) (*dataLinkFake, *DataLinkClient) {
	f := &dataLinkFake{status: status, body: body}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f.mu.Lock()
		f.calls = append(f.calls, dataLinkCall{Path: r.URL.Path, Form: r.PostForm})
		status, body := f.status, f.body
		f.mu.Unlock()
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(f.Close)
	c := NewDataLinkClient(&config.CCBillConfig{ClientAccNum: "900100", ClientSubAcc: "0000", DataLinkUsername: "dluser", DataLinkPassword: "dlpass"})
	c.BaseURL, c.HTTPClient = f.URL, f.Client()
	return f, c
}

func (f *dataLinkFake) answer(status int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status, f.body = status, body
}

func (f *dataLinkFake) Calls() []dataLinkCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]dataLinkCall(nil), f.calls...)
}

func TestTransactionExportRequestAndRows(t *testing.T) {
	f, c := newDataLinkFake(t, 200, strings.Join([]string{
		`"REBILL","900100","0","0125217202000000017","2026-06-01 04:05:06","918273645","23.99"`,
		`"CANCELLATION","900100","0","0999000000000000001","2026-06-02"`,
		`"REFUND","900100","0","0999000000000000002","2026-06-03","5.00"`,
		`"MYSTERY","900100","0","0999000000000000003"`,
	}, "\n"))
	c.DevMode = true
	// 2026-06-11 00:00 UTC is 2026-06-10 17:00 on CCBill's MST clock.
	rows, err := c.FetchTransactionExport(t.Context(), time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 6, 11, 0, 0, 0, 0, time.UTC),
		[]DataLinkTxnType{DataLinkTxnRebill, DataLinkTxnCancellation, DataLinkTxnRefund})
	require.NoError(t, err)
	call := f.Calls()[0]
	require.Equal(t, "/data/main.cgi", call.Path)
	require.Equal(t, url.Values{
		"transactionTypes": {"REBILL,CANCELLATION,REFUND"}, "startTime": {"20260531170000"}, "endTime": {"20260610170000"},
		"clientAccnum": {"900100"}, "clientSubacc": {"0000"}, "username": {"dluser"}, "password": {"dlpass"}, "testMode": {"1"},
	}, call.Form)

	require.Len(t, rows, 4, "unrequested types are preserved, never dropped")
	for i, want := range [][4]string{
		{"0125217202000000017", "2026-06-01 04:05:06", "918273645", "23.99"},
		{"0999000000000000001", "2026-06-02", "", ""},
		{"0999000000000000002", "2026-06-03", "", "5.00"},
		{"0999000000000000003", "", "", ""},
	} {
		r := rows[i]
		require.Equal(t, want, [4]string{r.SubscriptionID(), r.Timestamp(), r.TransactionID(), r.Amount()}, i)
	}
	require.Equal(t, DataLinkTxnType("MYSTERY"), rows[3].TransactionType)

	f.answer(200, "")
	c.DevMode, c.ClientSubAcc = false, ""
	rows, err = c.FetchTransactionExport(t.Context(), time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC), AllDataLinkTxnTypes)
	require.NoError(t, err)
	require.Empty(t, rows, "an empty window is zero rows")
	form := f.Calls()[1].Form
	require.Equal(t, "REBILL,CANCELLATION,EXPIRE,REFUND,CHARGEBACK", form.Get("transactionTypes"))
	require.NotContains(t, form, "testMode", "live accounts never request synthetic data")
	require.NotContains(t, form, "clientSubacc")

	start, end := time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC), time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	for name, args := range map[string][2]time.Time{"reversed": {start, end}, "zero start": {{}, start}, "zero end": {end, {}}} {
		_, err := c.FetchTransactionExport(t.Context(), args[0], args[1], []DataLinkTxnType{DataLinkTxnRebill})
		require.Error(t, err, name)
	}
	_, err = c.FetchTransactionExport(t.Context(), end, start, nil)
	require.Error(t, err)
	require.Len(t, f.Calls(), 2)
}

func TestDataLinkRejectsErrorsWithoutRetryingAuth(t *testing.T) {
	window := func(c *DataLinkClient, ctx context.Context) error {
		_, err := c.FetchTransactionExport(ctx, time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC), []DataLinkTxnType{DataLinkTxnRebill})
		return err
	}
	for _, tc := range []struct {
		status int
		body   string
		want   string
	}{
		{200, "Error: authentication failed", "error payload"},
		{200, "Invalid date range", "error payload"},
		{200, "access denied for account", "error payload"},
		{401, "", "authentication failed"},
		{403, "", "access forbidden"},
	} {
		f, c := newDataLinkFake(t, tc.status, tc.body)
		require.ErrorContains(t, window(c, t.Context()), tc.want)
		require.Len(t, f.Calls(), 1, "definite rejections are not retried")
	}

	f, c := newDataLinkFake(t, 500, "")
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, window(c, ctx), context.DeadlineExceeded, "transient retries honor the caller's context")
	require.Len(t, f.Calls(), 1)
}

func TestActiveMembersRosterRejectsPartialBatches(t *testing.T) {
	const row = `"ACTIVEMEMBERS","900100","x","0125217202000000017","2026-05-03","user","u@example.com","1","2026-06-03","2026-06-03"`
	f, c := newDataLinkFake(t, 200, row)
	c.DevMode = true
	records, err := c.FetchActiveMembers(t.Context())
	require.NoError(t, err)
	require.Equal(t, []CCBillRecord{{TransactionType: "ACTIVEMEMBERS", ClientAccNum: "900100", Field2: "x", SubscriptionID: "0125217202000000017",
		Date: "2026-05-03", Username: "user", Email: "u@example.com", Status: "1", RebillDate: "2026-06-03", ExpiryDate: "2026-06-03"}}, records)
	require.Equal(t, "ACTIVEMEMBERS", f.Calls()[0].Form.Get("transactionTypes"))
	require.Equal(t, "1", f.Calls()[0].Form.Get("testMode"))

	f.answer(200, `"REBILL","1"`)
	_, err = c.FetchActiveMembers(t.Context())
	require.ErrorContains(t, err, "expected ACTIVEMEMBERS")

	for name, csv := range map[string]string{
		"short row":             row + "\n" + `"ACTIVEMEMBERS","900100"`,
		"empty subscription id": strings.Replace(row, "0125217202000000017", " ", 1),
		"malformed csv":         `"ACTIVEMEMBERS,"broken`,
	} {
		records, err := c.ProcessCSVData(t.Context(), csv)
		require.Error(t, err, name)
		require.Nil(t, records, "one bad row fails the whole batch: %s", name)
	}
}

func TestDataLinkStatusAndPaidThrough(t *testing.T) {
	for status, want := range map[string]bool{"1": true, "Y": true, "yes": true, " ACTIVE ": true, "a": true, "0": false, "cancelled": false, "": false, "inactive": false} {
		require.Equal(t, want, IsDataLinkActiveStatus(status), status)
	}
	now := time.Date(2026, 5, 25, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		record CCBillRecord
		want   time.Time
		ok     bool
	}{
		{CCBillRecord{ExpiryDate: "2026-06-03"}, time.Date(2026, 6, 3, 23, 59, 59, 0, time.UTC), true},
		{CCBillRecord{ExpiryDate: "06/03/2026"}, time.Date(2026, 6, 3, 23, 59, 59, 0, time.UTC), true},
		{CCBillRecord{ExpiryDate: "2026-05-01", RebillDate: "2026-06-10T08:00:00Z"}, time.Time{}, false},
		{CCBillRecord{ExpiryDate: "garbage", RebillDate: "2026-05-25"}, time.Time{}, false},
		{CCBillRecord{RebillDate: "2026-06-10"}, time.Time{}, false},
		{CCBillRecord{ExpiryDate: "2026-05-01"}, time.Time{}, false},
		{CCBillRecord{}, time.Time{}, false},
	} {
		got, ok := DataLinkPaidThrough(tc.record, now)
		require.Equal(t, tc.ok, ok, "%+v", tc.record)
		require.Equal(t, tc.want, got, "%+v", tc.record)
	}
}

func TestProbeCredentialsIsABoundedRead(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
		ok     bool
	}{{200, "", true}, {401, "", false}, {200, "Error: authentication failed", false}} {
		f, c := newDataLinkFake(t, tc.status, tc.body)
		c.ReadOnly = true
		require.Equal(t, tc.ok, c.ProbeCredentials(t.Context()) == nil, tc.body)
		form := f.Calls()[0].Form
		require.Equal(t, "REBILL", form.Get("transactionTypes"))
		start, err := time.Parse("20060102150405", form.Get("startTime"))
		require.NoError(t, err)
		end, err := time.Parse("20060102150405", form.Get("endTime"))
		require.NoError(t, err)
		require.LessOrEqual(t, end.Sub(start), time.Second)
	}
	f, c := newDataLinkFake(t, 200, "")
	c.Password = ""
	require.ErrorContains(t, c.ProbeCredentials(t.Context()), "password")
	require.Empty(t, f.Calls())
}

func TestSubscriptionManagementWireAndStatusDocuments(t *testing.T) {
	f, c := newDataLinkFake(t, 200, `<?xml version='1.0' standalone='yes'?>
<results>
  <nextBillingDate>20260801</nextBillingDate>
  <recurringSubscription>1</recurringSubscription>
  <signupDate>20160724003047</signupDate>
  <subscriptionStatus>2</subscriptionStatus>
  <timesRebilled>121</timesRebilled>
  <someFutureField>kept</someFutureField>
</results>`)
	c.ReadOnly = true
	res, err := c.ViewSubscriptionStatus(t.Context(), " 0123456789 ")
	require.NoError(t, err, "reads stay available under readonly")
	call := f.Calls()[0]
	require.Equal(t, subscriptionManagementPath, call.Path)
	require.Equal(t, "action=viewSubscriptionStatus&clientAccnum=900100&clientSubacc=0000&password=dlpass&returnXML=1&subscriptionId=0123456789&username=dluser", call.Form.Encode())
	require.Equal(t, "2", res.RawStatus)
	require.Equal(t, "kept", res.Fields["someFutureField"])
	exp, ok := res.ExpiresAt()
	require.True(t, ok)
	require.Equal(t, time.Date(2026, 8, 1, 23, 59, 59, 0, time.UTC), exp)

	// A dead subscription still reports recurringSubscription=1; only the status decides.
	f.answer(200, `<results><cancelDate>20130131123315</cancelDate><expirationDate>20130131123315</expirationDate><recurringSubscription>1</recurringSubscription><subscriptionStatus>0</subscriptionStatus></results>`)
	res, err = c.ViewSubscriptionStatus(t.Context(), "113027706000000428")
	require.NoError(t, err)
	rebilling, err := res.Rebilling()
	require.NoError(t, err)
	require.False(t, rebilling)
	exp, ok = res.ExpiresAt()
	require.True(t, ok)
	require.Equal(t, time.Date(2013, 1, 31, 12, 33, 15, 0, time.UTC), exp)

	for body, want := range map[string]string{
		`<results><subscriptionId>1</subscriptionId></results>`: "missing subscriptionStatus",
		`-3`:                    `bare code "-3"`,
		`<results>-3</results>`: `results="-3"`,
		"two\nlines":            "unrecognized response shape",
		`<results><a>`:          "decode xml",
		`<results></results>`:   "no element values",
	} {
		f.answer(200, body)
		_, err := c.ViewSubscriptionStatus(t.Context(), "1")
		require.ErrorContains(t, err, want, body)
	}
	_, err = c.ViewSubscriptionStatus(t.Context(), " ")
	require.Error(t, err)
}

func TestSubscriptionStatusVocabularyAndExpiry(t *testing.T) {
	for status, want := range map[string][2]bool{"2": {true, true}, "1": {false, true}, "0": {false, false}} {
		res := SubscriptionStatusResult{RawStatus: status}
		rebilling, err := res.Rebilling()
		require.NoError(t, err)
		active, err := res.Active()
		require.NoError(t, err)
		require.Equal(t, want, [2]bool{rebilling, active}, status)
	}
	_, err := SubscriptionStatusResult{RawStatus: "7"}.Rebilling()
	require.Error(t, err, "an unrecognized status is never a guessed default")
	_, err = SubscriptionStatusResult{RawStatus: ""}.Active()
	require.Error(t, err)

	for _, tc := range []struct {
		fields map[string]string
		want   time.Time
		ok     bool
	}{
		{map[string]string{"nextBillingDate": "20260801", "expirationDate": "20130131123315"}, time.Date(2026, 8, 1, 23, 59, 59, 0, time.UTC), true},
		{map[string]string{"nextBillingDate": "soon", "expirationDate": "20130131123315"}, time.Date(2013, 1, 31, 12, 33, 15, 0, time.UTC), true},
		{map[string]string{"expirationDate": "not-a-date"}, time.Time{}, false},
		{map[string]string{}, time.Time{}, false},
	} {
		got, ok := SubscriptionStatusResult{Fields: tc.fields}.ExpiresAt()
		require.Equal(t, tc.ok, ok, tc.fields)
		require.Equal(t, tc.want, got, tc.fields)
	}
}

func TestCancelSubscriptionOutcomeClasses(t *testing.T) {
	for _, tc := range []struct {
		status        int
		body          string
		results       string
		want          error
		indeterminate bool
	}{
		{200, `<results>1</results>`, "1", nil, false},
		{200, `1`, "1", nil, false},
		{200, `"1"`, "1", nil, false},
		{200, `<results>0</results>`, "0", ErrCancelRejected, false},
		{200, `-1`, "-1", ErrCancelRejected, false},
		{200, `-7`, "-7", ErrDataLinkIndeterminate, false},
		{200, `<results>-7</results>`, "-7", ErrDataLinkIndeterminate, false},
		{403, ``, "", ErrDataLinkAuth, false},
		{200, `invalid username or password`, "", ErrDataLinkAuth, false},
		{200, "weird\nmultiline", "", nil, true},
		{502, ``, "", nil, true},
		{200, ``, "", nil, true},
	} {
		f, c := newDataLinkFake(t, tc.status, tc.body)
		res, err := c.CancelSubscription(t.Context(), "0123456789")
		require.Equal(t, tc.results, res.Results, tc.body)
		switch {
		case tc.indeterminate:
			require.Error(t, err, tc.body)
			require.NotErrorIs(t, err, ErrCancelRejected, "an uninterpretable answer may have executed")
			require.NotErrorIs(t, err, ErrDataLinkAuth)
		case tc.want != nil:
			require.ErrorIs(t, err, tc.want, tc.body)
		default:
			require.NoError(t, err, tc.body)
		}
		require.Equal(t, "cancelSubscription", f.Calls()[0].Form.Get("action"))
		require.Len(t, f.Calls(), 1, "the mutation is never blindly retried")
	}
}

func TestCancelSubscriptionIsGatedBeforeTheWire(t *testing.T) {
	f, c := newDataLinkFake(t, 200, `<results>1</results>`)
	c.ReadOnly = true
	_, err := c.CancelSubscription(t.Context(), "sub-1")
	require.ErrorIs(t, err, ErrProviderReadOnly)

	c.ReadOnly, c.DevMode = false, true
	_, err = c.CancelSubscription(t.Context(), "sub-1")
	require.ErrorIs(t, err, providerposture.ErrDisarmed, "CCBill has no authoritative test signal")
	require.Empty(t, f.Calls())

	c.LoopbackFixture = true
	_, err = c.CancelSubscription(t.Context(), "sub-1")
	require.NoError(t, err)
	c.BaseURL = "https://datalink.ccbill.com"
	_, err = c.CancelSubscription(t.Context(), "sub-1")
	require.ErrorIs(t, err, providerposture.ErrDisarmed, "the fixture marker only reaches loopback")
	_, err = c.CancelSubscription(t.Context(), " ")
	require.Error(t, err)
	require.Len(t, f.Calls(), 1)
}
