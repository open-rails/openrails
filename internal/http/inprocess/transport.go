package inprocess

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/open-rails/openrails/internal/merchanttarget"
	"github.com/open-rails/openrails/internal/requestauth"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/api"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/http/middleware"
	"github.com/open-rails/openrails/internal/merchant"
)

// NewTransport uses the same host authority and context isolation for embedded
// clients and database-only operator commands. configuredMerchant is read on
// each call so a runtime may be bound after constructing its client.
func NewTransport(handler http.Handler, configuredMerchant func() billing.MerchantID) (http.RoundTripper, string) {
	return newTransport(handler, configuredMerchant, "")
}

// NewTransportWithResolver supports explicit per-operation merchant selectors.
// Resolution does not grant authority; only the private capability creates a
// host principal, and all other credentials retain normal verification.
func NewTransportWithResolver(handler http.Handler, configuredMerchant func() billing.MerchantID, resolve func(context.Context, *http.Request) (billingauth.Target, error)) (http.RoundTripper, string) {
	transport, capability := newTransport(handler, configuredMerchant, "")
	transport.(*inprocessTransport).resolveTarget = resolve
	return transport, capability
}

func newTransport(handler http.Handler, configuredMerchant func() billing.MerchantID, subject string) (http.RoundTripper, string) {
	// Only the constructor's private default token provider receives this
	// per-client capability. A forwarded caller credential cannot name a mode.
	capability := rand.Text()
	return &inprocessTransport{handler: handler, configuredMerchant: configuredMerchant, hostCredential: capability, subject: subject}, capability
}

// inprocessTransport dispatches Client requests straight into the in-process
// handler: no socket, one JSON round trip. Only the internal default host
// credential gets the host principal (a context value); explicit customer
// credentials verify normally. Network mounts never use it.
type inprocessTransport struct {
	resolveTarget      func(context.Context, *http.Request) (billingauth.Target, error)
	handler            http.Handler
	configuredMerchant func() billing.MerchantID
	hostCredential     string
	subject            string
}

func (t *inprocessTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	stream := req.Method == http.MethodGet && middleware.IsMerchantBillingArchive(req)
	if req.Body != nil && !stream {
		// A canceled upload must also unblock a reader waiting for more bytes.
		stop := context.AfterFunc(req.Context(), func() { _ = req.Body.Close() })
		defer stop()
		defer req.Body.Close()
	}
	ctx := req.Context()
	// An unbound in-process client, and a runtime bound to another merchant
	// after the client was built, are refused before any handler runs, as a
	// 409 response: a RoundTrip error would surface as ErrUnreachable, wrong
	// for a well-formed request the engine refuses.
	mid, _ := merchant.FromContext(ctx)
	refuse := func(message string) (*http.Response, error) {
		if stream && req.Body != nil {
			_ = req.Body.Close()
		}
		return conflictResponse(req, message), nil
	}
	// Drop all ambient values before target resolution; retain only the trusted
	// resolver's fresh result, including the requested forwarded name.
	ctx = engineContext(ctx)
	var slug string
	if t.resolveTarget != nil {
		selectionRequest := req.Clone(ctx)
		target, err := t.resolveTarget(ctx, selectionRequest)
		if err != nil {
			var gate billingauth.GateError
			if errors.As(err, &gate) {
				return selectionErrorResponse(req, gate), nil
			}
			return refuse(err.Error())
		}
		mid, slug = target.MerchantID, target.MerchantSlug
		ctx = merchanttarget.WithResolved(selectionRequest.Context(), target)
	}
	if mid.IsZero() {
		return refuse("openrails: in-process client is not bound to a merchant")
	}
	// Live read: the runtime may be bound after its client was built.
	if bound := t.configuredMerchant(); !bound.IsZero() && mid != bound {
		return refuse(merchantMismatchMsg(bound, mid))
	}
	// Only the caller's cancellation and deadline reach the engine; every host
	// context value is dropped (engineContext).
	if req.Header.Get("Authorization") == "Bearer "+t.hostCredential {
		ctx = requestauth.WithHostPrincipal(ctx, &requestauth.HostPrincipal{MerchantID: mid, MerchantSlug: slug, Subject: t.subject})
	}

	// The in-process analogue of middleware.ResolveMerchantHTTP: pin the
	// bound merchant before any merchant-owned DB access.
	ctx = merchant.WithID(ctx, mid)
	if stream {
		return streamInprocessResponse(t.handler, req.Clone(ctx))
	}
	w := &bufferedResponse{header: make(http.Header)}
	t.handler.ServeHTTP(w, req.Clone(ctx))
	return w.response(req), nil
}

// conflictResponse synthesizes a 409 in the OpenRails error envelope for a
// merchant binding conflict. The remote client parses it like any non-2xx
// response, so the call surfaces as a StatusError (ErrConflict).
func conflictResponse(req *http.Request, message string) *http.Response {
	body, _ := json.Marshal(api.Coded(billing.CodeMerchantBindingMismatch, message).ToResponse())
	header := make(http.Header)
	header.Set("Content-Type", "application/json")
	return &http.Response{
		Status:        fmt.Sprintf("%d %s", http.StatusConflict, http.StatusText(http.StatusConflict)),
		StatusCode:    http.StatusConflict,
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

func merchantMismatchMsg(bound, pinned billing.MerchantID) string {
	return fmt.Sprintf("openrails: client is bound to merchant %s but the runtime is bound to merchant %s", pinned, bound)
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

func selectionErrorResponse(req *http.Request, failure billingauth.GateError) *http.Response {
	body, _ := json.Marshal(billingauth.RefusalError(failure).ToResponse())
	header := make(http.Header)
	header.Set("Content-Type", "application/json")
	return &http.Response{StatusCode: failure.Status, Status: http.StatusText(failure.Status), Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1, Header: header, Body: io.NopCloser(bytes.NewReader(body)), ContentLength: int64(len(body)), Request: req}
}
