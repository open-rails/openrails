package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/open-rails/openrails/internal/requestauth"

	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/http/middleware"
	"github.com/open-rails/openrails/internal/http/router"
	httproutes "github.com/open-rails/openrails/internal/http/routes"
	"github.com/open-rails/openrails/pkg/api"
	"github.com/open-rails/openrails/pkg/merchant"
)

// inprocessBaseURL is the synthetic base the unified embedded client is built
// with. `.invalid` (RFC 2606) never resolves, so if the in-process transport
// were ever bypassed the request fails instead of leaking onto the network.
const inprocessBaseURL = "http://openrails.invalid"

// newServiceHandler builds the in-process merchant API surface (#685): the SAME
// neutral RegisterServiceRoutes mux the standalone server mounts at
// /v1/merchant (#670), including the real permission gate and the
// MerchantDBConnMW RLS pin. The gate is built with NO resolvers, so the ONLY
// credential it accepts is the context-attached host principal — which only the
// in-process transport can set; a request reaching this handler without it
// (i.e. anything network-shaped) is rejected 401 by the real middleware.
func newServiceHandler(rt *app.Runtime) http.Handler {
	mux := http.NewServeMux()
	opts := httproutes.Options{Gate: httproutes.NewGate(httproutes.GateOptions{})}
	httproutes.RegisterServiceRoutes(router.NewMux(mux, "/v1/merchant", rt), rt, opts)
	httproutes.RegisterMerchantActionRoutes(router.NewMux(mux, "/v1/merchant", rt), rt, opts)
	httproutes.RegisterCatalogRoutes(router.NewMux(mux, "/v1/merchant/catalog", rt), rt, opts)
	// #737: DeclaredBilling import, same gate (host principal holds merchant:*).
	httproutes.RegisterImportRoutes(router.NewMux(mux, "/v1/import", rt), rt, opts)
	// The same request body cap the HTTP mounts apply, so an oversized request
	// is refused with the same 413 envelope in every deployment.
	return middleware.BodyLimitHTTP(middleware.DefaultMaxBodyBytes)(mux)
}

// hostPermissions is the embedded host's authority over its own merchant: the
// full merchant owner grant, identical to what a merchant-owner API key
// resolves to on the standalone wire path.
func hostPermissions() []string {
	return []string{string(controlplane.MerchantType.OwnerGrant())}
}

// inprocessTransport dispatches SDK requests directly into the in-process
// neutral handler — no socket, no serialization loss, one JSON round-trip. It
// attaches the host principal as a CONTEXT VALUE (requestauth.WithHostPrincipal);
// headers are never the trust carrier, so nothing a network peer sends can
// impersonate the host.
type inprocessTransport struct {
	handler http.Handler
	rt      *app.Runtime
}

func (t *inprocessTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx := req.Context()
	// The client carries its construction-time binding in ctx. A runtime bound
	// after that client was built refuses a different merchant (#772).
	mid, _ := merchant.FromContext(ctx)
	if mid.IsZero() {
		return conflictResponse(req, "openrails: in-process client is not bound to a merchant"), nil
	}
	if bound := t.rt.ConfiguredMerchant(); !bound.IsZero() && mid != bound {
		return conflictResponse(req, merchantMismatchMsg(bound, mid)), nil
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
	w := &bufferedResponse{header: make(http.Header)}
	t.handler.ServeHTTP(w, req.Clone(ctx))
	return w.response(req), nil
}

// conflictResponse synthesizes a 409 response in the pkg/api Stripe error
// envelope shape ({"error":{"type","code","message"}}) for a merchant
// binding conflict (#772). remote.go's do/statusErrorFromBody parses this envelope
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
