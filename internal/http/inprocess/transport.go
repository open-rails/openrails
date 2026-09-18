package inprocess

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/open-rails/openrails/internal/requestauth"

	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/http/middleware"
	"github.com/open-rails/openrails/pkg/api"
	"github.com/open-rails/openrails/pkg/merchant"
)

// hostPermissions is the embedded host's authority over its own merchant: the
// full merchant owner grant, identical to what a merchant-owner API key
// resolves to on the standalone wire path.
func hostPermissions() []string {
	return []string{string(controlplane.MerchantType.OwnerGrant())}
}

// NewTransport uses the same host authority and context isolation for embedded
// clients and database-only operator commands. configuredMerchant is read on
// each call so a runtime may be bound after constructing its client.
func NewTransport(handler http.Handler, configuredMerchant func() merchant.ID) http.RoundTripper {
	return &inprocessTransport{handler: handler, configuredMerchant: configuredMerchant}
}

// inprocessTransport dispatches SDK requests directly into the in-process
// neutral handler — no socket, no serialization loss, one JSON round-trip. It
// attaches the host principal as a CONTEXT VALUE (requestauth.WithHostPrincipal);
// headers are never the trust carrier, so nothing a network peer sends can
// impersonate the host.
type inprocessTransport struct {
	handler            http.Handler
	configuredMerchant func() merchant.ID
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
	// The client carries its construction-time binding in ctx (#445): an
	// unbound in-process client, and a runtime bound to another merchant after
	// that client was built, are both refused before any handler runs (#772).
	// Synthesized as a response (not a RoundTrip error): an error here would
	// surface via remote.go's doRaw as ErrUnreachable, which is wrong for a
	// well-formed request the engine deliberately refuses.
	mid, _ := merchant.FromContext(ctx)
	refuse := func(message string) (*http.Response, error) {
		if stream && req.Body != nil {
			_ = req.Body.Close()
		}
		return conflictResponse(req, message), nil
	}
	if mid.IsZero() {
		return refuse("openrails: in-process client is not bound to a merchant")
	}
	// Live read: EnsureMerchant/provisioning may bind the merchant after New.
	if bound := t.configuredMerchant(); !bound.IsZero() && mid != bound {
		return refuse(merchantMismatchMsg(bound, mid))
	}
	// Only the caller's cancellation and deadline reach the engine; every host
	// context value is dropped (engineContext).
	ctx = engineContext(ctx)
	ctx = requestauth.WithHostPrincipal(ctx, &requestauth.HostPrincipal{
		MerchantID:  mid,
		Permissions: hostPermissions(),
	})
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

// conflictResponse synthesizes a 409 response in the pkg/api Stripe error
// envelope shape ({"error":{"type","code","message"}}) for a merchant binding
// conflict (#772). remote.go's do/statusErrorFromBody parses this envelope
// like any real non-2xx wire response, so the call surfaces as a StatusError
// (ErrConflict) identically to every other in-process rejection.
func conflictResponse(req *http.Request, message string) *http.Response {
	body, _ := json.Marshal(api.ConflictError(message).ToResponse())
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

func merchantMismatchMsg(bound, pinned merchant.ID) string {
	return fmt.Sprintf("openrails: client is bound to merchant %s but the runtime is bound to merchant %s", pinned, bound)
}

// engineContext derives the context the engine serves an in-process call under.
// It keeps only the host's cancellation and deadline: every host context value
// — the session user the host is serving, request auth caches, a pinned
// merchant connection, rate-limit subjects — is dropped, so the engine
// attributes the call to the Client's bound merchant exactly as it attributes a
// standalone API-key request, never to the host's own caller.
func engineContext(host context.Context) context.Context {
	return detachedValues{Context: host}
}

type detachedValues struct{ context.Context }

func (detachedValues) Value(any) any { return nil }
