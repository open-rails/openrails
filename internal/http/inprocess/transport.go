package inprocess

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/api"
	"github.com/open-rails/openrails/internal/http/middleware"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/requestauth"
)

// Directory reads a merchant's directory row.
type Directory interface {
	Get(context.Context, billing.MerchantID) (*merchants.Merchant, error)
}

// Live is id's row while it is a live merchant: billing.ErrMerchantUnresolved
// for an unknown, deleted or archived one.
func Live(ctx context.Context, directory Directory, id billing.MerchantID) (*merchants.Merchant, error) {
	if id.IsZero() || directory == nil {
		return nil, billing.ErrMerchantUnresolved
	}
	m, err := directory.Get(ctx, id)
	switch {
	case errors.Is(err, merchants.ErrMerchantNotFound):
		return nil, billing.ErrMerchantUnresolved
	case err != nil:
		return nil, err
	case m == nil || m.Status != merchants.StatusActive:
		return nil, billing.ErrMerchantUnresolved
	}
	return m, nil
}

type merchantKey struct{}

// WithMerchant names the merchant a handle's call acts on. Only code holding
// the engine can set it, and no network request carries it: it is the
// in-process root of trust.
func WithMerchant(ctx context.Context, id billing.MerchantID) context.Context {
	return context.WithValue(ctx, merchantKey{}, id)
}

// NewTransport dispatches a handle's calls into handler as the host itself,
// bound to the merchant WithMerchant names, read live from directory on every
// call.
func NewTransport(handler http.Handler, directory Directory) http.RoundTripper {
	return &inprocessTransport{handler: handler, directory: directory}
}

// inprocessTransport dispatches Client requests straight into the in-process
// handler: no socket, one JSON round trip. The host principal is a context
// value; network mounts never use it.
type inprocessTransport struct {
	handler   http.Handler
	directory Directory
}

func (t *inprocessTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	stream := req.Method == http.MethodGet && middleware.IsMerchantBillingArchive(req)
	if req.Body != nil && !stream {
		// A canceled upload must also unblock a reader waiting for more bytes.
		stop := context.AfterFunc(req.Context(), func() { _ = req.Body.Close() })
		defer stop()
		defer req.Body.Close()
	}
	mid, _ := req.Context().Value(merchantKey{}).(billing.MerchantID)
	// Only the caller's cancellation and deadline reach the engine; every host
	// context value is dropped (engineContext).
	ctx := engineContext(req.Context())
	m, err := Live(ctx, t.directory, mid)
	if err != nil {
		if stream && req.Body != nil {
			_ = req.Body.Close()
		}
		// A refusal is a response: a RoundTrip error would surface as
		// ErrUnreachable, wrong for a well-formed request the engine refuses.
		if errors.Is(err, billing.ErrMerchantUnresolved) {
			return unresolvedResponse(req, mid), nil
		}
		return nil, fmt.Errorf("openrails: read merchant %s: %w", mid, err)
	}
	ctx = requestauth.WithHostPrincipal(ctx, &requestauth.HostPrincipal{MerchantID: m.ID, MerchantSlug: m.Slug})
	// Pin the merchant before any merchant-owned DB access.
	ctx = merchant.WithID(ctx, m.ID)
	if stream {
		return streamInprocessResponse(t.handler, req.Clone(ctx))
	}
	w := &bufferedResponse{header: make(http.Header)}
	t.handler.ServeHTTP(w, req.Clone(ctx))
	return w.response(req), nil
}

// unresolvedResponse is the 403 a handle whose merchant is not live gets, in
// the OpenRails error envelope.
func unresolvedResponse(req *http.Request, mid billing.MerchantID) *http.Response {
	refusal := api.Coded(billing.CodeMerchantUnresolved, "merchant "+mid.String()+" is not a live merchant of this engine")
	body, _ := json.Marshal(refusal.ToResponse())
	header := make(http.Header)
	header.Set("Content-Type", "application/json")
	return &http.Response{
		Status:        fmt.Sprintf("%d %s", refusal.HTTPStatus, http.StatusText(refusal.HTTPStatus)),
		StatusCode:    refusal.HTTPStatus,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        header,
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       req,
	}
}

// bufferedResponse is a minimal in-memory http.ResponseWriter for the
// in-process round trip (no httptest import on the production path).
type bufferedResponse struct {
	header      http.Header
	buf         bytes.Buffer
	status      int
	wroteHeader bool
}

func (w *bufferedResponse) Header() http.Header { return w.header }

func (w *bufferedResponse) WriteHeader(code int) {
	if !w.wroteHeader {
		w.status = code
		w.wroteHeader = true
	}
}

func (w *bufferedResponse) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.buf.Write(p)
}

func (w *bufferedResponse) response(req *http.Request) *http.Response {
	if !w.wroteHeader {
		w.status = http.StatusOK
	}
	body := w.buf.Bytes()
	return &http.Response{
		Status:        fmt.Sprintf("%d %s", w.status, http.StatusText(w.status)),
		StatusCode:    w.status,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        w.header,
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       req,
	}
}

// engineContext keeps only the host's cancellation and deadline: every host
// context value (session user, auth caches, a pinned connection, rate-limit
// subjects) is dropped, so the engine attributes the call to the Client's
// bound merchant, never to the host's own caller.
func engineContext(host context.Context) context.Context {
	return detachedValues{Context: host}
}

type detachedValues struct{ context.Context }

func (detachedValues) Value(any) any { return nil }
