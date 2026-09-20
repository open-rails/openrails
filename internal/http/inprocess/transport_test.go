package inprocess

import (
	"context"
	"github.com/open-rails/openrails/internal/requestauth"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/open-rails/openrails/pkg/merchant"
)

func TestEngineContextKeepsOnlyCancellation(t *testing.T) {
	type hostKey struct{}
	deadline := time.Now().Add(time.Hour)
	host, cancel := context.WithDeadline(context.WithValue(context.Background(), hostKey{}, "host"), deadline)
	defer cancel()
	host = billingauth.SetUserContext(merchant.WithID(host, merchant.ID(uuid.New())), billingauth.UserContext{UserID: uuid.NewString()})

	ctx := engineContext(host)
	require.Nil(t, ctx.Value(hostKey{}))
	_, ok := billingauth.FromContext(ctx)
	require.False(t, ok, "host session user must not reach the engine")
	_, ok = merchant.FromContext(ctx)
	require.False(t, ok, "host merchant pin must not reach the engine")

	got, ok := ctx.Deadline()
	require.True(t, ok)
	require.Equal(t, deadline, got)

	bound := merchant.ID(uuid.New())
	engine := merchant.WithID(ctx, bound)
	pinned, ok := merchant.FromContext(engine)
	require.True(t, ok)
	require.Equal(t, bound, pinned)
	require.Nil(t, engine.Value(hostKey{}))

	derived, derivedCancel := context.WithCancel(engine)
	defer derivedCancel()
	require.NoError(t, derived.Err())
	cancel()
	<-derived.Done()
	require.ErrorIs(t, derived.Err(), context.Canceled)
	require.ErrorIs(t, ctx.Err(), context.Canceled)
}

func TestExplicitCredentialsNeverInheritHostAuthority(t *testing.T) {
	mid := merchant.ID(uuid.New())
	ctx := merchant.WithID(requestauth.WithHostPrincipal(t.Context(), &requestauth.HostPrincipal{MerchantID: mid, Permissions: hostPermissions()}), mid)
	ctx = billingauth.SetUserContext(ctx, billingauth.UserContext{UserID: uuid.NewString()})
	for _, token := range []string{"", "customer-credential", "invalid-credential", "merchant-key", "Bearer in-process-host"} {
		transport, _ := NewTransport(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, host := requestauth.HostPrincipalFromContext(r.Context())
			require.False(t, host)
			_, user := billingauth.FromContext(r.Context())
			require.False(t, user)
			require.Equal(t, token, r.Header.Get("Authorization"))
			w.WriteHeader(http.StatusUnauthorized) // the real verifier owns authentication
		}), func() merchant.ID { return mid })
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://openrails.invalid/v1/me/status", nil)
		require.NoError(t, err)
		req.Header.Set("Authorization", token)
		res, err := transport.RoundTrip(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusUnauthorized, res.StatusCode)
		require.NoError(t, res.Body.Close())
	}
}

func TestTransportMerchantAuthorityAndIsolation(t *testing.T) {
	type privateKey struct{}
	bound := merchant.ID(uuid.New())
	original := bound
	called := 0
	handler := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		called++
		require.Nil(t, req.Context().Value(privateKey{}))
		_, ok := billingauth.FromContext(req.Context())
		require.False(t, ok)
		principal, ok := requestauth.HostPrincipalFromContext(req.Context())
		require.True(t, ok)
		require.Equal(t, bound, principal.MerchantID)
		require.Equal(t, hostPermissions(), principal.Permissions)
		mid, ok := merchant.FromContext(req.Context())
		require.True(t, ok)
		require.Equal(t, bound, mid)
		_, _ = w.Write([]byte("ok"))
	})
	transport, hostCapability := NewTransport(handler, func() merchant.ID { return bound })
	host := context.WithValue(t.Context(), privateKey{}, "host-private")
	host = requestauth.WithHostPrincipal(host, &requestauth.HostPrincipal{MerchantID: merchant.ID(uuid.New()), Permissions: []string{"platform:*"}})
	host = billingauth.SetUserContext(host, billingauth.UserContext{UserID: uuid.NewString()})
	// The shared Client attaches its immutable construction binding.
	host = merchant.WithID(host, original)
	for _, path := range []string{"/v1/merchant/ordinary", "/v1/merchant/billing-archive"} {
		req, _ := http.NewRequestWithContext(host, http.MethodGet, "http://openrails.invalid"+path, nil)
		req.Header.Set("Authorization", "Bearer "+hostCapability)
		resp, err := transport.RoundTrip(req)
		require.NoError(t, err)
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.Equal(t, "ok", string(body))
		require.NoError(t, resp.Body.Close())
	}
	bound = merchant.ID(uuid.New()) // a changed runtime binding must refuse the original Client
	req, _ := http.NewRequestWithContext(host, http.MethodGet, "http://openrails.invalid/ordinary", nil)
	resp, err := transport.RoundTrip(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusConflict, resp.StatusCode)
	require.Equal(t, 2, called)
	conflict, _ := http.NewRequestWithContext(merchant.WithID(host, original), http.MethodGet, "http://openrails.invalid/ordinary", nil)
	resp, err = transport.RoundTrip(conflict)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusConflict, resp.StatusCode)
	require.Equal(t, 2, called, "conflicting pin must not reach handler")
	unbound, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://openrails.invalid/ordinary", nil)
	resp, err = transport.RoundTrip(unbound)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusConflict, resp.StatusCode)
	require.Equal(t, 2, called, "unbound calls must not reach handler")
}

func TestTransportImportCancellationClosesRequestBody(t *testing.T) {
	bound := merchant.ID(uuid.New())
	ctx, cancel := context.WithCancel(merchant.WithID(t.Context(), bound))
	defer cancel()
	reader, writer := io.Pipe()
	defer writer.Close()
	started := make(chan struct{})
	finished := make(chan error, 1)
	transport, _ := NewTransport(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		_, err := io.Copy(io.Discard, r.Body)
		finished <- err
		w.WriteHeader(http.StatusBadRequest)
	}), func() merchant.ID { return bound })
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://openrails.invalid/v1/merchant/billing-archive", reader)
	require.NoError(t, err)
	roundTripDone := make(chan struct{})
	go func() {
		defer close(roundTripDone)
		resp, _ := transport.RoundTrip(req)
		if resp != nil {
			_ = resp.Body.Close()
		}
	}()
	select {
	case <-started:
	case <-roundTripDone:
		t.Fatal("import returned before reaching its handler")
	}
	cancel()
	require.Error(t, <-finished)
	<-roundTripDone
}
