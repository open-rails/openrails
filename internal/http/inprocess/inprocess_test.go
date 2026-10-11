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

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/api"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/requestauth"
)

type hostKey struct{}

// hostContext carries everything a host request might: private values, a
// host principal of another merchant, and a merchant pin.
func hostContext(t *testing.T, pin billing.MerchantID) context.Context {
	ctx := context.WithValue(t.Context(), hostKey{}, "host-private")
	ctx = requestauth.WithHostPrincipal(ctx, &requestauth.HostPrincipal{MerchantID: billing.MerchantID(uuid.New())})
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
	host, cancel := context.WithDeadline(hostContext(t, billing.MerchantID(uuid.New())), deadline)
	defer cancel()
	ctx := engineContext(host)
	require.Nil(t, ctx.Value(hostKey{}))
	_, ok := merchant.FromContext(ctx)
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

// fakeDirectory is a directory of rows by id; err fails every read.
type fakeDirectory struct {
	rows map[billing.MerchantID]*merchants.Merchant
	err  error
}

func (d fakeDirectory) Get(_ context.Context, id billing.MerchantID) (*merchants.Merchant, error) {
	if d.err != nil {
		return nil, d.err
	}
	if m, ok := d.rows[id]; ok {
		return m, nil
	}
	return nil, merchants.ErrMerchantNotFound
}

func live(ids ...billing.MerchantID) fakeDirectory {
	d := fakeDirectory{rows: map[billing.MerchantID]*merchants.Merchant{}}
	for _, id := range ids {
		d.rows[id] = &merchants.Merchant{ID: id, Slug: "shop", Status: merchants.StatusActive}
	}
	return d
}

// The host principal is the handle's merchant, named by the private context
// value alone: no header, ambient pin or host principal of the caller's
// selects one, and no host value reaches the handler.
func TestTransportBindsTheHandlesMerchant(t *testing.T) {
	bound, ambient := billing.MerchantID(uuid.New()), billing.MerchantID(uuid.New())
	type observed struct {
		host     *requestauth.HostPrincipal
		merchant billing.MerchantID
	}
	var got observed
	transport := NewTransport(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Nil(t, r.Context().Value(hostKey{}))
		got = observed{}
		got.host, _ = requestauth.HostPrincipalFromContext(r.Context())
		got.merchant, _ = merchant.FromContext(r.Context())
		_, _ = w.Write([]byte("ok"))
	}), live(bound, ambient))

	for _, auth := range []string{"", "Bearer merchant-key"} {
		req, err := http.NewRequestWithContext(WithMerchant(hostContext(t, ambient), bound), http.MethodGet, "http://openrails.invalid/v1/admin/payments", nil)
		require.NoError(t, err)
		req.Header.Set("Authorization", auth)
		req.Header.Set("OpenRails-Merchant", "id:"+ambient.String())
		res, err := transport.RoundTrip(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, res.StatusCode)
		require.NoError(t, res.Body.Close())
		require.Equal(t, observed{host: &requestauth.HostPrincipal{MerchantID: bound, MerchantSlug: "shop"}, merchant: bound}, got)
	}
}

// A call that names no merchant, or one that is unknown or deleted, is
// refused with 403 merchant_unresolved before any handler runs; a directory
// that cannot be read is a transport failure, never a refusal.
func TestTransportRefusesAMerchantThatIsNotLive(t *testing.T) {
	known, deleted := billing.MerchantID(uuid.New()), billing.MerchantID(uuid.New())
	directory := live(known)
	directory.rows[deleted] = &merchants.Merchant{ID: deleted, Slug: "gone", Status: merchants.StatusDeleted}
	called := 0
	handler := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called++ })
	transport := NewTransport(handler, directory)
	for name, ctx := range map[string]context.Context{
		"no handle merchant": hostContext(t, known),
		"zero":               WithMerchant(t.Context(), billing.MerchantID{}),
		"unknown":            WithMerchant(t.Context(), billing.MerchantID(uuid.New())),
		"deleted":            WithMerchant(t.Context(), deleted),
	} {
		res, body := roundTrip(t, transport, ctx, http.MethodGet, "/v1/admin/payments", "")
		require.Equal(t, http.StatusForbidden, res.StatusCode, name)
		var envelope api.ErrorResponse
		require.NoError(t, json.Unmarshal([]byte(body), &envelope))
		require.Equal(t, billing.CodeMerchantUnresolved, envelope.Error.Code, name)
	}
	require.Zero(t, called)

	down := errors.New("directory down")
	req, err := http.NewRequestWithContext(WithMerchant(t.Context(), known), http.MethodGet, "http://openrails.invalid/v1/admin/payments", nil)
	require.NoError(t, err)
	_, err = NewTransport(handler, fakeDirectory{err: down}).RoundTrip(req)
	require.ErrorIs(t, err, down)
	require.Zero(t, called)
}

// Archive exports stream through a pipe: the book is never buffered, and a
// caller that stops reading or cancels releases the producer.
func TestArchiveResponsesStream(t *testing.T) {
	bound := billing.MerchantID(uuid.New())
	book := bytes.Repeat([]byte("archive-data\n"), 200000)
	done := make(chan error, 1)
	transport := NewTransport(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, err := w.Write(book)
		done <- err
	}), live(bound))
	req, _ := http.NewRequestWithContext(WithMerchant(t.Context(), bound), http.MethodGet, "http://openrails.invalid/v1/admin/billing-archive", nil)
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
	bound := billing.MerchantID(uuid.New())
	ctx, cancel := context.WithCancel(WithMerchant(t.Context(), bound))
	defer cancel()
	reader, writer := io.Pipe()
	defer writer.Close()
	started, finished := make(chan struct{}), make(chan error, 1)
	transport := NewTransport(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		_, err := io.Copy(io.Discard, r.Body)
		finished <- err
		w.WriteHeader(http.StatusBadRequest)
	}), live(bound))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://openrails.invalid/v1/admin/billing-archive", reader)
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
