package embed

import (
	"bytes"
	"fmt"
	"io"
	"net/http"

	authcore "github.com/open-rails/authkit/embedded"

	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/http/router"
	httproutes "github.com/open-rails/openrails/internal/http/routes"
	"github.com/open-rails/openrails/pkg/billingauth"
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
	// #737: DeclaredBilling import, same gate (host principal holds merchant:*).
	httproutes.RegisterImportRoutes(router.NewMux(mux, "/v1/import", rt), rt, opts)
	return mux
}

// hostPermissions is the embedded host's authority over its own merchant: the
// full merchant owner grant, identical to what a merchant-owner API key
// resolves to on the standalone wire path.
func hostPermissions() []string {
	return []string{authcore.OwnerGrant(controlplane.MerchantType)}
}

// inprocessTransport dispatches SDK requests directly into the in-process
// neutral handler — no socket, no serialization loss, one JSON round-trip. It
// attaches the host principal as a CONTEXT VALUE (billingauth.WithHostPrincipal);
// headers are never the trust carrier, so nothing a network peer sends can
// impersonate the host.
type inprocessTransport struct {
	handler http.Handler
	rt      *app.Runtime
}

func (t *inprocessTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx := req.Context()
	// Live read: EnsureMerchant/provisioning may bind the merchant after New.
	mid := t.rt.ConfiguredMerchant
	if mid.IsZero() {
		if v, ok := merchant.FromContext(ctx); ok {
			mid = v // host pinned it per call via openrails.WithMerchant
		}
	}
	ctx = billingauth.WithHostPrincipal(ctx, &billingauth.HostPrincipal{
		MerchantID:  mid,
		Permissions: hostPermissions(),
	})
	if !mid.IsZero() {
		// The in-process analogue of middleware.ResolveMerchantHTTP: pin the
		// bound merchant before any merchant-owned DB access.
		ctx = merchant.WithID(ctx, mid)
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
