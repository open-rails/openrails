package solana

import (
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/gagliardetto/solana-go/rpc"
	"github.com/gagliardetto/solana-go/rpc/jsonrpc"

	"github.com/open-rails/openrails/internal/shared/redact"
)

// #SEC-17: a Solana RPC credential is a MERCHANT secret and providers take it
// in the query string (Helius `?api-key=`). The pinned solana-go client formats
// the full request URL into every error it returns, and those errors are logged
// and were echoed to clients — so an endpoint URL we HOLD must never contain the
// key. RPCEndpoint.URL is credential-free; the stripped parameters are
// re-attached here, on a CLONE of the outbound request, so the request object
// solana-go formats into its errors keeps the credential-free URL.

// secretQueryTransport re-attaches credential query parameters at dial time.
type secretQueryTransport struct {
	base   http.RoundTripper
	secret url.Values
}

func (t *secretQueryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	if len(t.secret) == 0 {
		return base.RoundTrip(req)
	}
	clone := req.Clone(req.Context())
	u := *req.URL
	q := u.Query()
	for name, vals := range t.secret {
		for i, v := range vals {
			if i == 0 {
				q.Set(name, v)
			} else {
				q.Add(name, v)
			}
		}
	}
	u.RawQuery = q.Encode()
	clone.URL = &u
	return base.RoundTrip(clone)
}

// rpcHTTPTimeout mirrors solana-go's own generous client timeout; per-call
// deadlines come from the caller's context.
const rpcHTTPTimeout = 5 * time.Minute

func newRPCHTTPTransport() *http.Transport {
	return &http.Transport{
		IdleConnTimeout:     rpcHTTPTimeout,
		MaxConnsPerHost:     9,
		MaxIdleConnsPerHost: 9,
		Proxy:               http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   rpcHTTPTimeout,
			KeepAlive: 180 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:   true,
		TLSHandshakeTimeout: 10 * time.Second,
	}
}

// newEndpointClient builds the solana-go client for one endpoint: it sees only
// the credential-free URL; credentials ride the transport.
func newEndpointClient(ep RPCEndpoint) *rpc.Client {
	httpClient := &http.Client{
		Timeout:   rpcHTTPTimeout,
		Transport: &secretQueryTransport{base: newRPCHTTPTransport(), secret: ep.secret},
	}
	return rpc.NewWithCustomRPCClient(jsonrpc.NewClientWithOpts(ep.URL, &jsonrpc.RPCClientOpts{HTTPClient: httpClient}))
}

// newSecretEndpoint builds an endpoint whose credential is carried out-of-band.
func newSecretEndpoint(name, rawURL string, priority int, secret url.Values) RPCEndpoint {
	safeURL, stripped := redact.StripSecretQuery(rawURL)
	if len(stripped) > 0 {
		if secret == nil {
			secret = url.Values{}
		}
		for k, v := range stripped {
			secret[k] = v
		}
	}
	return RPCEndpoint{Name: name, URL: safeURL, Priority: priority, secret: secret}
}
