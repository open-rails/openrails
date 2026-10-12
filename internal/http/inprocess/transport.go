package inprocess

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/http/middleware"
	"github.com/open-rails/openrails/internal/requestauth"
)

type merchantKey struct{}
type invokerKey struct{}

// WithMerchant names the merchant a handle's call acts on. Only code holding
// the engine can set it, and no network request carries it: it is the
// in-process root of trust.
func WithMerchant(ctx context.Context, id billing.MerchantID) context.Context {
	return context.WithValue(ctx, merchantKey{}, id)
}

// WithInvoker names the person a handle's call acts for. Like the merchant,
// only in-process code can set it; without it the call is the host's own.
func WithInvoker(ctx context.Context, invoker billingauth.Invoker) context.Context {
	return context.WithValue(ctx, invokerKey{}, invoker)
}

// NewTransport dispatches a handle's calls into handler as the host itself,
// bound to the merchant WithMerchant names and acting for the person
// WithInvoker names, if any. The route gate reads that merchant live on every
// call: one that is not live is 403 merchant_unresolved.
func NewTransport(handler http.Handler) http.RoundTripper {
	return &inprocessTransport{handler: handler}
}

// inprocessTransport dispatches Client requests straight into the in-process
// handler: no socket, one JSON round trip. The host principal is a context
// value; network mounts never use it.
type inprocessTransport struct {
	handler http.Handler
}

func (t *inprocessTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	stream := req.Method == http.MethodGet && middleware.IsMerchantBillingArchive(req)
	if req.Body != nil && !stream {
		// A canceled upload must also unblock a reader waiting for more bytes.
		stop := context.AfterFunc(req.Context(), func() { _ = req.Body.Close() })
		defer stop()
		defer req.Body.Close()
	}
	principal := &requestauth.HostPrincipal{}
	principal.MerchantID, _ = req.Context().Value(merchantKey{}).(billing.MerchantID)
	if invoker, ok := req.Context().Value(invokerKey{}).(billingauth.Invoker); ok {
		principal.Invoker = &invoker
	}
	// Only the caller's cancellation and deadline reach the engine; every other
	// host context value is dropped (engineContext).
	ctx := requestauth.WithHostPrincipal(engineContext(req.Context()), principal)
	if stream {
		return streamInprocessResponse(t.handler, req.Clone(ctx))
	}
	w := &bufferedResponse{header: make(http.Header)}
	t.handler.ServeHTTP(w, req.Clone(ctx))
	return w.response(req), nil
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
