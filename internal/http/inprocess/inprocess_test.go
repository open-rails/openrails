package inprocess

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/merchanttarget"
	"github.com/open-rails/openrails/internal/requestauth"
	"github.com/open-rails/openrails/pkg/api"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/open-rails/openrails/pkg/merchant"
)

type hostKey struct{}

// hostContext carries everything a host request might: private values, a
// session user, a host principal of another merchant, and a merchant pin.
func hostContext(t *testing.T, pin merchant.ID) context.Context {
	ctx := context.WithValue(t.Context(), hostKey{}, "host-private")
	ctx = requestauth.WithHostPrincipal(ctx, &requestauth.HostPrincipal{MerchantID: merchant.ID(uuid.New()), Permissions: []string{"platform:*"}})
	ctx = billingauth.SetUserContext(ctx, billingauth.UserContext{UserID: uuid.NewString()})
	if !pin.IsZero() {
		ctx = merchant.WithID(ctx, pin)
	}
	return ctx
}

func roundTrip(t *testing.T, rt http.RoundTripper, ctx context.Context, method, path, authorization string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, method, "http://openrails.invalid"+path, nil)
	require.NoError(t, err)
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	res, err := rt.RoundTrip(req)
	require.NoError(t, err)
	body, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	require.NoError(t, res.Body.Close())
	return res, string(body)
}

func TestEngineContextKeepsOnlyCancellation(t *testing.T) {
	deadline := time.Now().Add(time.Hour)
	host, cancel := context.WithDeadline(hostContext(t, merchant.ID(uuid.New())), deadline)
	defer cancel()
	ctx := engineContext(host)
	require.Nil(t, ctx.Value(hostKey{}))
	_, ok := billingauth.FromContext(ctx)
	require.False(t, ok)
	_, ok = merchant.FromContext(ctx)
	require.False(t, ok)
	_, ok = requestauth.HostPrincipalFromContext(ctx)
	require.False(t, ok)
	got, ok := ctx.Deadline()
	require.True(t, ok)
	require.Equal(t, deadline, got)
	cancel()
	<-ctx.Done()
	require.ErrorIs(t, ctx.Err(), context.Canceled)
}

// Only the per-client private capability mints the host principal; any other
// credential reaches the handler unauthenticated, and no host identity leaks.
func TestTransportAuthority(t *testing.T) {
	bound := merchant.ID(uuid.New())
	type observed struct {
		host     *requestauth.HostPrincipal
		merchant merchant.ID
		auth     string
	}
	var got observed
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Nil(t, r.Context().Value(hostKey{}))
		_, user := billingauth.FromContext(r.Context())
		require.False(t, user, "the host's session user never reaches the engine")
		got = observed{auth: r.Header.Get("Authorization")}
		got.host, _ = requestauth.HostPrincipalFromContext(r.Context())
		got.merchant, _ = merchant.FromContext(r.Context())
		_, _ = w.Write([]byte("ok"))
	})
	transport, capability := NewTransport(handler, func() merchant.ID { return bound })
	other, _ := NewTransport(handler, func() merchant.ID { return bound })
	require.NotEmpty(t, capability)

	for _, auth := range []string{"Bearer " + capability, "", "Bearer in-process-host", "Bearer merchant-key", capability} {
		res, body := roundTrip(t, transport, hostContext(t, bound), http.MethodGet, "/v1/merchant/payments", auth)
		require.Equal(t, http.StatusOK, res.StatusCode)
		require.Equal(t, "ok", body)
		require.Equal(t, auth, got.auth, "explicit credentials pass through for normal verification")
		require.Equal(t, bound, got.merchant)
		if auth == "Bearer "+capability {
			require.NotNil(t, got.host)
			require.Equal(t, bound, got.host.MerchantID)
			require.Equal(t, []string{"merchant:*"}, got.host.Permissions)
		} else {
			require.Nil(t, got.host)
		}
	}
	_, _ = roundTrip(t, other, hostContext(t, bound), http.MethodGet, "/v1/merchant/payments", "Bearer "+capability)
	require.Nil(t, got.host, "a capability is scoped to the client that minted it")
}

// An unbound client, a conflicting pin, or a runtime rebound after the client
// was built are refused with a 409 envelope before any handler runs.
func TestTransportRefusesMerchantMismatch(t *testing.T) {
	original := merchant.ID(uuid.New())
	bound := original
	called := 0
	transport, capability := NewTransport(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called++ }), func() merchant.ID { return bound })
	for _, tc := range []struct {
		name   string
		pin    merchant.ID
		rebind merchant.ID
	}{
		{"unbound client", merchant.ID{}, original},
		{"conflicting pin", merchant.ID(uuid.New()), original},
		{"runtime rebound", original, merchant.ID(uuid.New())},
	} {
		bound = tc.rebind
		res, body := roundTrip(t, transport, hostContext(t, tc.pin), http.MethodGet, "/v1/merchant/payments", "Bearer "+capability)
		require.Equal(t, http.StatusConflict, res.StatusCode, tc.name)
		var envelope api.ErrorResponse
		require.NoError(t, json.Unmarshal([]byte(body), &envelope))
		require.NotEmpty(t, envelope.Error.Code, tc.name)
	}
	require.Zero(t, called)
	bound = merchant.ID{}
	res, _ := roundTrip(t, transport, hostContext(t, original), http.MethodGet, "/v1/merchant/payments", "")
	require.Equal(t, http.StatusOK, res.StatusCode, "a not-yet-bound runtime accepts the client's own binding")
}

func TestTransportExplicitSelector(t *testing.T) {
	target := billingauth.Target{MerchantID: merchant.ID(uuid.New()), MerchantSlug: "store"}
	bound := merchant.ID{}
	var resolveErr error
	var host *requestauth.HostPrincipal
	var selected billingauth.Target
	var sawAmbient bool
	transport, capability := NewTransportWithResolver(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		host, _ = requestauth.HostPrincipalFromContext(r.Context())
		selected, _ = merchanttarget.FromContext(r.Context())
	}), func() merchant.ID { return bound }, func(ctx context.Context, r *http.Request) (billingauth.Target, error) {
		sawAmbient = ctx.Value(hostKey{}) != nil || r.Context().Value(hostKey{}) != nil
		return target, resolveErr
	})

	res, _ := roundTrip(t, transport, hostContext(t, merchant.ID(uuid.New())), http.MethodGet, "/v2/merchant/payments", "Bearer "+capability)
	require.Equal(t, http.StatusOK, res.StatusCode, "the resolved target replaces the client's construction pin")
	require.False(t, sawAmbient, "resolution never sees host context values")
	require.Equal(t, target, selected)
	require.Equal(t, target.MerchantID, host.MerchantID)
	require.Equal(t, "store", host.MerchantSlug)

	for _, tc := range []struct {
		err    error
		bound  merchant.ID
		status int
		code   string
	}{
		{billingauth.GateError{Status: http.StatusForbidden, Message: "not yours"}, merchant.ID{}, http.StatusForbidden, "merchant_selection_invalid"},
		{errors.New("directory down"), merchant.ID{}, http.StatusConflict, ""},
		{nil, merchant.ID(uuid.New()), http.StatusConflict, ""},
	} {
		resolveErr, bound, selected = tc.err, tc.bound, billingauth.Target{}
		res, body := roundTrip(t, transport, t.Context(), http.MethodGet, "/v2/merchant/payments", "Bearer "+capability)
		require.Equal(t, tc.status, res.StatusCode)
		require.Contains(t, body, tc.code)
		require.Zero(t, selected, "refused selections never reach the handler")
	}
}

// Archive exports stream through a pipe: the book is never buffered, and a
// caller that stops reading or cancels releases the producer.
func TestArchiveResponsesStream(t *testing.T) {
	bound := merchant.ID(uuid.New())
	book := bytes.Repeat([]byte("archive-data\n"), 200000)
	done := make(chan error, 1)
	transport, _ := NewTransport(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, err := w.Write(book)
		done <- err
	}), func() merchant.ID { return bound })
	req, _ := http.NewRequestWithContext(merchant.WithID(t.Context(), bound), http.MethodGet, "http://openrails.invalid/v1/merchant/billing-archive", nil)
	res, err := transport.RoundTrip(req)
	require.NoError(t, err)
	select {
	case <-done:
		t.Fatal("the archive was buffered before the caller read it")
	default:
	}
	got, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	require.True(t, bytes.Equal(book, got))
	require.NoError(t, <-done)
	require.NoError(t, res.Body.Close())

	stream := func(ctx context.Context, h http.HandlerFunc) (*http.Response, error) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://openrails.invalid", nil)
		return streamInprocessResponse(h, req)
	}
	t.Run("close stops the producer and cancels its context", func(t *testing.T) {
		wrote, ctxErr := make(chan error, 1), make(chan error, 1)
		res, err := stream(t.Context(), func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, err := w.Write([]byte("archive"))
			wrote <- err
			<-r.Context().Done()
			ctxErr <- r.Context().Err()
		})
		require.NoError(t, err)
		require.NoError(t, res.Body.Close())
		require.ErrorIs(t, <-wrote, io.ErrClosedPipe)
		require.ErrorIs(t, <-ctxErr, context.Canceled)
	})
	t.Run("cancel after headers interrupts both sides", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		wrote := make(chan error, 1)
		res, err := stream(ctx, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, err := w.Write([]byte("blocked until cancellation"))
			wrote <- err
		})
		require.NoError(t, err)
		defer res.Body.Close()
		cancel()
		require.Error(t, <-wrote)
		_, err = io.ReadAll(res.Body)
		require.ErrorIs(t, err, context.Canceled)
	})
	t.Run("cancel before headers", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		res, err := stream(ctx, func(http.ResponseWriter, *http.Request) { cancel() })
		if res != nil {
			_ = res.Body.Close()
		}
		if err != nil {
			require.ErrorIs(t, err, context.Canceled)
		}
	})
	t.Run("panic is a 500 with a truncated body", func(t *testing.T) {
		res, err := stream(t.Context(), func(http.ResponseWriter, *http.Request) { panic("fixture") })
		require.NoError(t, err)
		defer res.Body.Close()
		require.Equal(t, http.StatusInternalServerError, res.StatusCode)
		_, err = io.ReadAll(res.Body)
		require.Error(t, err)
	})
}

// A canceled upload must unblock a handler still waiting for request bytes.
func TestUploadCancellationClosesRequestBody(t *testing.T) {
	bound := merchant.ID(uuid.New())
	ctx, cancel := context.WithCancel(merchant.WithID(t.Context(), bound))
	defer cancel()
	reader, writer := io.Pipe()
	defer writer.Close()
	started, finished := make(chan struct{}), make(chan error, 1)
	transport, _ := NewTransport(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		_, err := io.Copy(io.Discard, r.Body)
		finished <- err
		w.WriteHeader(http.StatusBadRequest)
	}), func() merchant.ID { return bound })
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://openrails.invalid/v1/merchant/billing-archive", reader)
	require.NoError(t, err)
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		if res, _ := transport.RoundTrip(req); res != nil {
			_ = res.Body.Close()
		}
	}()
	select {
	case <-started:
	case <-returned:
		t.Fatal("upload returned before reaching its handler")
	}
	cancel()
	require.Error(t, <-finished)
	<-returned
}
