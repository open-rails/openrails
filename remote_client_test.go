package openrails

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/merchant"
)

// testMerchant is the merchant the fixtures' credential is filed under.
var testMerchant = billing.MerchantID(uuid.MustParse("7e57c0de-0000-4000-8000-000000000001"))

// newTestRemote serves h and returns a handle on it with a static key.
func newTestRemote(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(h)
	t.Cleanup(server.Close)
	return remoteHandle(t, server.URL+"/", RemoteOptions{Credentials: map[billing.MerchantID]RemoteCredential{testMerchant: {APIKey: "test-key"}}})
}

// remoteHandle is testMerchant's handle of NewRemote(baseURL, opts).
func remoteHandle(t *testing.T, baseURL string, opts RemoteOptions) *Client {
	t.Helper()
	remote, err := NewRemote(baseURL, opts)
	require.NoError(t, err)
	t.Cleanup(func() { _ = remote.Close(context.Background()) })
	handle, err := remote.Merchant(testMerchant)
	require.NoError(t, err)
	return handle
}

// tokenCredential files a Token credential under testMerchant.
func tokenCredential(token func(context.Context) (string, error)) RemoteOptions {
	return RemoteOptions{Credentials: map[billing.MerchantID]RemoteCredential{testMerchant: {Token: token}}}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func okResponse(body string) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}
}

func TestNewRemoteValidatesConfigurationWithoutIO(t *testing.T) {
	key := RemoteCredential{APIKey: "key"}
	token := func(context.Context) (string, error) { return "t", nil }
	signer, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	for _, base := range []string{"", "not a url", "ftp://example.com", "http://", "https://user:pass@example.com", "https://example.com?x=y", "https://example.com?", "https://example.com#f"} {
		remote, err := NewRemote(base, RemoteOptions{Credentials: map[billing.MerchantID]RemoteCredential{testMerchant: key}})
		require.ErrorContains(t, err, "base URL", base)
		require.Nil(t, remote)
		handle, err := NewRemoteMerchant(base, key)
		require.ErrorContains(t, err, "base URL", base)
		require.Nil(t, handle)
	}
	assertion := func(edit func(*AssertionKey)) *AssertionKey {
		k := &AssertionKey{Issuer: "https://shop.example", KeyID: "k1", Signer: signer, TokenURL: "https://billing.example/oauth2/token"}
		edit(k)
		return k
	}
	for name, credential := range map[string]RemoteCredential{
		"no credential":         {},
		"blank key":             {APIKey: "  "},
		"key and token":         {APIKey: "k", Token: token},
		"assertion alone":       {Assertion: assertion(func(*AssertionKey) {})},
		"assertion no issuer":   {APIKey: "k", Assertion: assertion(func(k *AssertionKey) { k.Issuer = " " })},
		"assertion no key id":   {APIKey: "k", Assertion: assertion(func(k *AssertionKey) { k.KeyID = "" })},
		"assertion no signer":   {APIKey: "k", Assertion: assertion(func(k *AssertionKey) { k.Signer = nil })},
		"assertion no endpoint": {APIKey: "k", Assertion: assertion(func(k *AssertionKey) { k.TokenURL = "" })},
		"assertion bad endpoint": {APIKey: "k", Assertion: assertion(func(k *AssertionKey) {
			k.TokenURL = "billing.example/oauth2/token"
		})},
		"assertion key family": {APIKey: "k", Assertion: assertion(func(k *AssertionKey) { k.Signer = unsupportedSigner{} })},
	} {
		remote, err := NewRemote("https://openrails.test", RemoteOptions{Credentials: map[billing.MerchantID]RemoteCredential{testMerchant: credential}})
		require.Error(t, err, name)
		require.Nil(t, remote, name)
		handle, err := NewRemoteMerchant("https://openrails.test", credential)
		require.Error(t, err, name)
		require.Nil(t, handle, name)
	}
	for name, credentials := range map[string]map[billing.MerchantID]RemoteCredential{
		"no merchant":   nil,
		"zero merchant": {billing.MerchantID{}: key},
	} {
		remote, err := NewRemote("https://openrails.test", RemoteOptions{Credentials: credentials})
		require.Error(t, err, name)
		require.Nil(t, remote, name)
	}

	var seen atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.Store(r.Method + " " + r.URL.Path + " " + r.Header.Get("Authorization") + " " + r.Header.Get("Accept"))
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()
	client := remoteHandle(t, server.URL+"/", RemoteOptions{Credentials: map[billing.MerchantID]RemoteCredential{testMerchant: {APIKey: " sk-test "}}})
	require.NoError(t, readConfiguration(client, t.Context()))
	require.Equal(t, "GET /v1/admin/configuration Bearer sk-test application/json", seen.Load(), "trailing base slash trimmed, key trimmed")
}

// unsupportedSigner holds a key of no JWS family.
type unsupportedSigner struct{}

func (unsupportedSigner) Public() crypto.PublicKey { return struct{}{} }
func (unsupportedSigner) Sign(io.Reader, []byte, crypto.SignerOpts) ([]byte, error) {
	return nil, errors.New("unsupported")
}

// One Remote holds a credential per merchant: each handle sends its own and
// nothing that names a merchant, whatever the caller's context carries, and
// a merchant without a credential has no handle.
func TestRemoteHandlesSendTheirOwnCredential(t *testing.T) {
	a, b := billing.MerchantID(uuid.New()), billing.MerchantID(uuid.New())
	type observed struct{ auth, selector string }
	seen := make(chan observed, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- observed{r.Header.Get("Authorization"), strings.Join(r.Header.Values("OpenRails"+"-Merchant"), ",")}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()
	remote, err := NewRemote(server.URL, RemoteOptions{Credentials: map[billing.MerchantID]RemoteCredential{
		a: {APIKey: "key-a"},
		b: {Token: func(context.Context) (string, error) { return " token-b ", nil }},
	}})
	require.NoError(t, err)
	for id, want := range map[billing.MerchantID]string{a: "Bearer key-a", b: "Bearer token-b"} {
		handle, err := remote.Merchant(id)
		require.NoError(t, err)
		require.Equal(t, id, handle.MerchantID())
		again, err := remote.Merchant(id)
		require.NoError(t, err)
		require.Same(t, handle, again)
		// An ambient merchant, as OpenRails' own middleware pins, selects nothing.
		require.NoError(t, readConfiguration(handle, merchant.WithID(t.Context(), a)))
		require.Equal(t, observed{auth: want}, <-seen)
	}
	for _, id := range []billing.MerchantID{billing.MerchantID(uuid.New()), {}} {
		handle, err := remote.Merchant(id)
		require.ErrorIs(t, err, billing.ErrMerchantUnresolved)
		require.Nil(t, handle)
	}

	single, err := NewRemoteMerchant(server.URL, RemoteCredential{APIKey: "key-a"})
	require.NoError(t, err)
	require.True(t, single.MerchantID().IsZero())
	require.NoError(t, readConfiguration(single, t.Context()))
	require.Equal(t, observed{auth: "Bearer key-a"}, <-seen)

	require.NoError(t, remote.Close(t.Context()))
}

// Ready is the server's own readiness: one request, with no credential.
func TestRemoteReady(t *testing.T) {
	var status atomic.Int64
	status.Store(http.StatusOK)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/health/ready", r.URL.Path)
		require.Empty(t, r.Header.Get("Authorization"))
		w.WriteHeader(int(status.Load()))
	}))
	remote, err := NewRemote(server.URL, RemoteOptions{Credentials: map[billing.MerchantID]RemoteCredential{testMerchant: {APIKey: "k"}}})
	require.NoError(t, err)
	require.NoError(t, remote.Ready(t.Context()))
	status.Store(http.StatusServiceUnavailable)
	require.ErrorIs(t, remote.Ready(t.Context()), billing.ErrUnreachable)
	server.Close()
	require.ErrorIs(t, remote.Ready(t.Context()), billing.ErrUnreachable)
}

// The server names the merchant it acted on (OpenRails-Bound-Merchant); a
// handle whose credential was filed under another merchant's ID discards the
// response, success or refusal, and says which two merchants disagree. A
// server that sends no header is not checked.
func TestHandleRefusesAResponseBoundToAnotherMerchant(t *testing.T) {
	filed, other := testMerchant, billing.MerchantID(uuid.New())
	var bound atomic.Value // []string: the header's values
	var status atomic.Int64
	status.Store(http.StatusOK)
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/v1/admin/configuration" {
			http.NotFound(w, r)
			return
		}
		for _, value := range bound.Load().([]string) {
			w.Header().Add(merchant.BoundHeader, value)
		}
		if code := int(status.Load()); code != http.StatusOK {
			w.WriteHeader(code)
			_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","code":"resource_missing","message":"gone"}}`))
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()
	handle := remoteHandle(t, server.URL, RemoteOptions{Credentials: map[billing.MerchantID]RemoteCredential{filed: {APIKey: "k"}}})

	bound.Store([]string(nil))
	require.NoError(t, readConfiguration(handle, t.Context()), "absent: a server that does not send the header")
	bound.Store([]string{filed.String()})
	require.NoError(t, readConfiguration(handle, t.Context()), "matching")

	bound.Store([]string{other.String()})
	for _, code := range []int{http.StatusOK, http.StatusNotFound} {
		status.Store(int64(code))
		err := readConfiguration(handle, t.Context())
		var mismatch *BoundMerchantError
		require.ErrorAs(t, err, &mismatch, "differing, status %d", code)
		require.Equal(t, BoundMerchantError{Merchant: filed, Bound: other}, *mismatch)
		require.ErrorContains(t, err, filed.String())
		require.ErrorContains(t, err, other.String())
		var refusal *billing.StatusError
		require.False(t, errors.As(err, &refusal), "the other merchant's answer is discarded")
	}
	require.Equal(t, filed, handle.MerchantID(), "a filed handle keeps its merchant")
	status.Store(http.StatusOK)
	for name, values := range map[string][]string{"not an id": {"shop"}, "zero": {billing.MerchantID{}.String()}, "two": {filed.String(), filed.String()}} {
		bound.Store(values)
		err := readConfiguration(handle, t.Context())
		require.ErrorIs(t, err, billing.ErrUnreachable, name)
		require.ErrorContains(t, err, merchant.BoundHeader, name)
	}
	require.EqualValues(t, 7, calls.Load(), "every call reached the server: the check follows the answer")

	// With no filed ID the handle adopts the first merchant it is told, and
	// refuses another afterwards.
	single, err := NewRemoteMerchant(server.URL, RemoteCredential{APIKey: "k"})
	require.NoError(t, err)
	bound.Store([]string(nil))
	require.NoError(t, readConfiguration(single, t.Context()))
	require.True(t, single.MerchantID().IsZero(), "nothing named a merchant yet")
	bound.Store([]string{other.String()})
	require.NoError(t, readConfiguration(single, t.Context()))
	require.Equal(t, other, single.MerchantID())
	bound.Store([]string(nil))
	require.NoError(t, readConfiguration(single, t.Context()))
	bound.Store([]string{filed.String()})
	var mismatch *BoundMerchantError
	require.ErrorAs(t, readConfiguration(single, t.Context()), &mismatch)
	require.Equal(t, BoundMerchantError{Merchant: other, Bound: filed}, *mismatch)
	require.Equal(t, other, single.MerchantID(), "the adopted merchant stays")
}

// A credential that cannot be minted fails the call: nothing is sent, and no
// other credential stands in.
func TestCredentialFailureSendsNothing(t *testing.T) {
	failure := errors.New("no credential for this merchant")
	for name, token := range map[string]func(context.Context) (string, error){
		"error": func(context.Context) (string, error) { return "", failure },
		"blank": func(context.Context) (string, error) { return " ", nil },
	} {
		server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("request sent without a minted credential") }))
		err := readConfiguration(remoteHandle(t, server.URL, tokenCredential(token)), t.Context())
		server.Close()
		require.Error(t, err, name)
		if name == "error" {
			require.ErrorIs(t, err, failure)
		}
	}
}

// Extra headers of a call cannot carry a second credential, and the caller's
// map is never mutated.
func TestExtraHeadersCannotCarryACredential(t *testing.T) {
	client := newTestRemote(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, []string{"Bearer test-key"}, r.Header.Values("Authorization"))
		require.Equal(t, "kept", r.Header.Get("X-Trace"))
		require.Equal(t, []string{testMerchant.String()}, r.Header.Values(merchant.ExpectedHeader))
		_, _ = w.Write([]byte(`{}`))
	})
	extra := http.Header{"authorization": {"Bearer broader-key"}, "Authorization": {"Bearer broader-key"}, "X-Trace": {"kept"}, merchant.ExpectedHeader: {billing.MerchantID(uuid.New()).String()}}
	original := extra.Clone()
	require.NoError(t, client.doWithHeaders(t.Context(), http.MethodGet, "/v1/admin/configuration", nil, nil, extra))
	require.True(t, reflect.DeepEqual(extra, original), "request mutated the caller's header map")
}

// readConfiguration is one authenticated merchant read.
func readConfiguration(c *Client, ctx context.Context, options ...RequestOption) error {
	_, err := c.GetMerchantConfiguration(ctx, options...)
	return err
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
		want    billing.ErrorDetails
		retry   string
		classes []error
	}{{
		name: "canonical envelope keeps exact metadata and proxy headers",
		resp: response{http.StatusConflict, map[string]string{"X-Request-ID": "header-request", "Retry-After": "12"},
			`{"error":{"type":"invalid_request_error","code":"idempotency_key_reused","message":"These terms differ","param":"amount","metadata":{"original_amount":9223372036854775807,"nested":{"minimum":-9223372036854775808}}}}`},
		want: billing.ErrorDetails{Type: "invalid_request_error", Code: "idempotency_key_reused", Message: "These terms differ", RequestID: "header-request", Param: new("amount"),
			Metadata: map[string]any{"original_amount": json.Number("9223372036854775807"), "nested": map[string]any{"minimum": json.Number("-9223372036854775808")}}},
		retry:   "12",
		classes: []error{billing.ErrConflict, billing.ErrIdempotencyKeyReused},
	}, {
		name:    "body request id wins over the header",
		resp:    response{http.StatusNotFound, map[string]string{"X-Request-ID": "header"}, `{"error":{"type":"invalid_request_error","code":"resource_missing","message":"gone","request_id":"body"}}`},
		want:    billing.ErrorDetails{Type: "invalid_request_error", Code: "resource_missing", Message: "gone", RequestID: "body"},
		classes: []error{billing.ErrNotFound},
	}, {
		name:    "foreign proxy page is a one-line excerpt without a code",
		resp:    response{http.StatusBadGateway, nil, "<html>\n\t<body>Bad Gateway\r\n  detail </body>\n</html>"},
		want:    billing.ErrorDetails{Message: "<html> <body>Bad Gateway detail </body> </html>"},
		classes: []error{billing.ErrInternal, billing.ErrUnreachable},
	}, {
		name:    "retired top-level shape is opaque",
		resp:    response{http.StatusBadRequest, nil, `{"code":"insufficient_credits","message":"m"}`},
		want:    billing.ErrorDetails{Message: `{"code":"insufficient_credits","message":"m"}`},
		classes: []error{billing.ErrInvalid},
	}, {
		name:    "envelope with trailing data is opaque",
		resp:    response{http.StatusPaymentRequired, nil, `{"error":{"code":"insufficient_credits"}} {}`},
		want:    billing.ErrorDetails{Message: `{"error":{"code":"insufficient_credits"}} {}`},
		classes: []error{billing.ErrPaymentRefused},
	}, {
		name:    "empty body",
		resp:    response{http.StatusServiceUnavailable, map[string]string{"Retry-After": "3"}, ""},
		retry:   "3",
		classes: []error{billing.ErrInternal, billing.ErrUnreachable},
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
			_, err := client.GetMerchantConfiguration(t.Context())
			var status *billing.StatusError
			require.ErrorAs(t, err, &status)
			require.Equal(t, tc.resp.status, status.Status)
			require.Equal(t, tc.want, status.ErrorDetails)
			require.Equal(t, tc.retry, status.RetryAfter)
			for _, class := range tc.classes {
				require.ErrorIs(t, err, class)
			}
			require.NotContains(t, err.Error(), "\n")
			if tc.want.Code == "" {
				require.NotErrorIs(t, err, billing.ErrInsufficientCredits, "a code is never inferred from an opaque body")
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
	_, err := client.GetMerchantConfiguration(t.Context())
	require.ErrorIs(t, err, billing.ErrCardDeclined)
	failure, ok := billing.PaymentFailureFrom(err)
	require.True(t, ok)
	require.Equal(t, billing.PaymentFailure{Reason: "expired_card", Message: "Card expired", Field: "exp"}, *failure)
	_, ok = billing.PaymentFailureFrom(&billing.StatusError{Status: 402, ErrorDetails: billing.ErrorDetails{Code: billing.CodePaymentMethodStale, Metadata: map[string]any{"failure": map[string]any{"reason": "r"}}}})
	require.False(t, ok, "only card_declined carries a customer-facing failure")
}

// No server verdict is never reported as a rejected operation: it is
// ErrUnreachable, the write may have committed.
func TestClientTransportFailuresAreUnreachable(t *testing.T) {
	var minted atomic.Int64
	counting := tokenCredential(func(context.Context) (string, error) { minted.Add(1); return "k", nil })
	bodies := map[string]string{
		"oversized":      `"` + strings.Repeat("a", 1<<20) + `"`,
		"two values":     `{} {}`,
		"malformed JSON": `{"display_name":`,
		"wrong type":     `[]`,
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			client := newTestRemote(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) })
			_, err := client.GetMerchantConfiguration(t.Context())
			require.ErrorIs(t, err, billing.ErrUnreachable)
			var status *billing.StatusError
			require.False(t, errors.As(err, &status))
		})
	}

	unsent := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("request sent on a finished context") }))
	defer unsent.Close()
	client := remoteHandle(t, unsent.URL, counting)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err := readConfiguration(client, ctx)
	require.ErrorIs(t, err, context.Canceled)
	require.ErrorIs(t, err, billing.ErrUnreachable)
	var status *billing.StatusError
	require.False(t, errors.As(err, &status))
	expired, cancelExpired := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancelExpired()
	require.ErrorIs(t, readConfiguration(client, expired), context.DeadlineExceeded)
	require.Zero(t, minted.Load(), "a finished context mints no credential")

	server := httptest.NewServer(http.NotFoundHandler())
	server.Close()
	closed, err := NewRemoteMerchant(server.URL, RemoteCredential{APIKey: "k"})
	require.NoError(t, err)
	require.ErrorIs(t, readConfiguration(closed, t.Context()), billing.ErrUnreachable)
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
			client := remoteHandle(t, "https://openrails.test", RemoteOptions{
				Credentials: map[billing.MerchantID]RemoteCredential{testMerchant: {APIKey: "k"}},
				HTTPClient:  &http.Client{Transport: transport}, Timeout: tc.timeout,
			})
			err := readConfiguration(client, ctx)
			require.True(t, called)
			if tc.wantErr == nil {
				require.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, tc.wantErr)
			require.ErrorIs(t, err, billing.ErrUnreachable)
			if !tc.cancel {
				require.NoError(t, ctx.Err(), "the child timeout must not cancel the caller")
			}
		})
	}
}
