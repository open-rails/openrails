package openrails

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// newTestRemote serves h and returns a client with a static key and a default merchant.
func newTestRemote(t *testing.T, h http.HandlerFunc, opts ...ClientOption) *Client {
	t.Helper()
	server := httptest.NewServer(h)
	t.Cleanup(server.Close)
	client, err := NewRemote(server.URL+"/", append([]ClientOption{WithAPIKey("test-key"), WithDefaultMerchant("fixture")}, opts...)...)
	require.NoError(t, err)
	return client
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func okResponse(body string) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}
}

func TestNewRemoteValidatesConfigurationWithoutIO(t *testing.T) {
	for _, base := range []string{"", "not a url", "ftp://example.com", "http://", "https://user:pass@example.com", "https://example.com?x=y", "https://example.com?", "https://example.com#f"} {
		client, err := NewRemote(base, WithAPIKey("key"))
		require.ErrorContains(t, err, "base URL", base)
		require.Nil(t, client)
	}
	for name, opts := range map[string][]ClientOption{
		"no credential":            nil,
		"blank key":                {WithAPIKey("  ")},
		"nil credential provider":  {WithCredentialProvider(nil)},
		"nil token provider":       {WithTokenProvider(nil)},
		"zero merchant ID":         {WithAPIKey("k"), WithMerchantID(MerchantID{})},
		"empty default merchant":   {WithAPIKey("k"), WithDefaultMerchant("")},
		"invalid default merchant": {WithAPIKey("k"), WithDefaultMerchant("a/b")},
	} {
		client, err := NewRemote("https://openrails.test", opts...)
		require.Error(t, err, name)
		require.Nil(t, client, name)
	}
	_, err := NewRemote("https://openrails.test", WithAPIKey("k"), WithDefaultMerchant(""))
	require.ErrorIs(t, err, ErrInvalid)

	var seen atomic.Value
	client := newTestRemote(t, func(w http.ResponseWriter, r *http.Request) {
		seen.Store(r.Method + " " + r.URL.Path + " " + r.Header.Get("Authorization") + " " + r.Header.Get("Accept"))
		_, _ = w.Write([]byte(`{}`))
	}, WithAPIKey(" sk-test "))
	require.NoError(t, client.Verify(t.Context()))
	require.Equal(t, "GET /v2/merchant/settings Bearer sk-test application/json", seen.Load(), "trailing base slash trimmed, key trimmed, slug routes to v2")
}

func TestClientDecodesTheErrorEnvelope(t *testing.T) {
	type response struct {
		status  int
		headers map[string]string
		body    string
	}
	cases := []struct {
		name    string
		resp    response
		want    ErrorDetails
		retry   string
		classes []error
	}{{
		name: "canonical envelope keeps exact metadata and proxy headers",
		resp: response{http.StatusConflict, map[string]string{"X-Request-ID": "header-request", "Retry-After": "12"},
			`{"error":{"type":"invalid_request_error","code":"idempotency_key_reused","message":"These terms differ","param":"amount","metadata":{"original_amount":9223372036854775807,"nested":{"minimum":-9223372036854775808}}}}`},
		want: ErrorDetails{Type: "invalid_request_error", Code: "idempotency_key_reused", Message: "These terms differ", RequestID: "header-request", Param: new("amount"),
			Metadata: map[string]any{"original_amount": json.Number("9223372036854775807"), "nested": map[string]any{"minimum": json.Number("-9223372036854775808")}}},
		retry:   "12",
		classes: []error{ErrConflict, ErrIdempotencyKeyReused},
	}, {
		name:    "body request id wins over the header",
		resp:    response{http.StatusNotFound, map[string]string{"X-Request-ID": "header"}, `{"error":{"type":"invalid_request_error","code":"resource_missing","message":"gone","request_id":"body"}}`},
		want:    ErrorDetails{Type: "invalid_request_error", Code: "resource_missing", Message: "gone", RequestID: "body"},
		classes: []error{ErrNotFound},
	}, {
		name:    "foreign proxy page is a one-line excerpt without a code",
		resp:    response{http.StatusBadGateway, nil, "<html>\n\t<body>Bad Gateway\r\n  detail </body>\n</html>"},
		want:    ErrorDetails{Message: "<html> <body>Bad Gateway detail </body> </html>"},
		classes: []error{ErrInternal, ErrUnreachable},
	}, {
		name:    "retired top-level shape is opaque",
		resp:    response{http.StatusBadRequest, nil, `{"code":"insufficient_credits","message":"m"}`},
		want:    ErrorDetails{Message: `{"code":"insufficient_credits","message":"m"}`},
		classes: []error{ErrInvalid},
	}, {
		name:    "envelope with trailing data is opaque",
		resp:    response{http.StatusPaymentRequired, nil, `{"error":{"code":"insufficient_credits"}} {}`},
		want:    ErrorDetails{Message: `{"error":{"code":"insufficient_credits"}} {}`},
		classes: []error{ErrPaymentRefused},
	}, {
		name:    "empty body",
		resp:    response{http.StatusServiceUnavailable, map[string]string{"Retry-After": "3"}, ""},
		retry:   "3",
		classes: []error{ErrInternal, ErrUnreachable},
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := newTestRemote(t, func(w http.ResponseWriter, _ *http.Request) {
				for k, v := range tc.resp.headers {
					w.Header().Set(k, v)
				}
				w.WriteHeader(tc.resp.status)
				_, _ = w.Write([]byte(tc.resp.body))
			})
			_, err := client.GetMerchantSettings(t.Context())
			var status *StatusError
			require.ErrorAs(t, err, &status)
			require.Equal(t, tc.resp.status, status.Status)
			require.Equal(t, tc.want, status.ErrorDetails)
			require.Equal(t, tc.retry, status.RetryAfter)
			for _, class := range tc.classes {
				require.ErrorIs(t, err, class)
			}
			require.NotContains(t, err.Error(), "\n")
			if tc.want.Code == "" {
				require.NotErrorIs(t, err, ErrInsufficientCredits, "a code is never inferred from an opaque body")
			}
		})
	}
}

func TestErrorExcerptIsBounded(t *testing.T) {
	got := excerptErrorBody([]byte(strings.Repeat("é", 400) + "\x00\xff tail"))
	require.LessOrEqual(t, len(got), maxErrorMessageBytes+len("…"))
	require.True(t, strings.HasSuffix(got, "…"))
	require.True(t, strings.HasPrefix(got, "éé"), "truncation keeps whole runes")
	require.Equal(t, "a b", excerptErrorBody([]byte("\x00a   b\xff\n")))
}

func TestPaymentFailureCrossesTheClient(t *testing.T) {
	client := newTestRemote(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusPaymentRequired)
		_, _ = w.Write([]byte(`{"error":{"type":"card_error","code":"card_declined","message":"x","metadata":{"failure":{"reason":"expired_card","message":"Card expired","field":"exp"}}}}`))
	})
	_, err := client.GetMerchantSettings(t.Context())
	require.ErrorIs(t, err, ErrCardDeclined)
	failure, ok := PaymentFailureFrom(err)
	require.True(t, ok)
	require.Equal(t, PaymentFailure{Reason: "expired_card", Message: "Card expired", Field: "exp"}, *failure)
	_, ok = PaymentFailureFrom(&StatusError{Status: 402, ErrorDetails: ErrorDetails{Code: CodePaymentMethodStale, Metadata: map[string]any{"failure": map[string]any{"reason": "r"}}}})
	require.False(t, ok, "only card_declined carries a customer-facing failure")
}

// No server verdict is never reported as a rejected operation: it is
// ErrUnreachable, the write may have committed.
func TestClientTransportFailuresAreUnreachable(t *testing.T) {
	var minted atomic.Int64
	counting := WithCredentialProvider(func(context.Context, CredentialTarget) (string, error) { minted.Add(1); return "k", nil })
	bodies := map[string]string{
		"oversized":      `"` + strings.Repeat("a", 1<<20) + `"`,
		"two values":     `{} {}`,
		"malformed JSON": `{"display_name":`,
		"wrong type":     `[]`,
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			client := newTestRemote(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) })
			_, err := client.GetMerchantSettings(t.Context())
			require.ErrorIs(t, err, ErrUnreachable)
			var status *StatusError
			require.False(t, errors.As(err, &status))
		})
	}

	client := newTestRemote(t, func(http.ResponseWriter, *http.Request) { t.Error("request sent on a finished context") }, counting)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err := client.Verify(ctx)
	require.ErrorIs(t, err, context.Canceled)
	require.ErrorIs(t, err, ErrUnreachable)
	var status *StatusError
	require.False(t, errors.As(err, &status))
	expired, cancelExpired := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancelExpired()
	require.ErrorIs(t, client.Verify(expired), context.DeadlineExceeded)
	require.Zero(t, minted.Load(), "a finished context mints no credential")

	server := httptest.NewServer(http.NotFoundHandler())
	server.Close()
	closed, err := NewRemote(server.URL, WithAPIKey("k"), WithDefaultMerchant("fixture"))
	require.NoError(t, err)
	require.ErrorIs(t, closed.Verify(t.Context()), ErrUnreachable)
}

func TestClientDeadlineOwnership(t *testing.T) {
	callerDeadline := time.Now().Add(time.Minute)
	cases := []struct {
		name     string
		timeout  time.Duration
		deadline bool
		cancel   bool
		check    func(t *testing.T, observed time.Time, has bool)
		wantErr  error
	}{
		{name: "default adds no deadline", check: func(t *testing.T, _ time.Time, has bool) { require.False(t, has) }},
		{name: "caller deadline unchanged", deadline: true, check: func(t *testing.T, got time.Time, has bool) {
			require.True(t, has)
			require.True(t, got.Equal(callerDeadline))
		}},
		{name: "longer timeout never extends the caller", deadline: true, timeout: time.Hour, check: func(t *testing.T, got time.Time, has bool) {
			require.True(t, has)
			require.True(t, got.Equal(callerDeadline))
		}},
		{name: "explicit timeout shortens and expires", deadline: true, timeout: 20 * time.Millisecond, wantErr: context.DeadlineExceeded},
		{name: "caller cancellation", cancel: true, wantErr: context.Canceled},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if tc.deadline {
				var cancelDeadline context.CancelFunc
				ctx, cancelDeadline = context.WithDeadline(ctx, callerDeadline)
				defer cancelDeadline()
			}
			called := false
			transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
				called = true
				if tc.wantErr != nil {
					if tc.cancel {
						cancel()
					}
					<-r.Context().Done()
					return nil, r.Context().Err()
				}
				observed, has := r.Context().Deadline()
				tc.check(t, observed, has)
				return okResponse("{}"), nil
			})
			client, err := NewRemote("https://openrails.test", WithAPIKey("k"), WithDefaultMerchant("fixture"),
				WithHTTPClient(&http.Client{Transport: transport}), WithTimeout(tc.timeout))
			require.NoError(t, err)
			err = client.Verify(ctx)
			require.True(t, called)
			if tc.wantErr == nil {
				require.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, tc.wantErr)
			require.ErrorIs(t, err, ErrUnreachable)
			if !tc.cancel {
				require.NoError(t, ctx.Err(), "the child timeout must not cancel the caller")
			}
		})
	}
}
