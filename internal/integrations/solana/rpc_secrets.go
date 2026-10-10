package solana

import (
	"crypto/sha256"
	"encoding/hex"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/gagliardetto/solana-go/rpc"
	"github.com/gagliardetto/solana-go/rpc/jsonrpc"

	"github.com/open-rails/openrails/internal/shared/redact"
)

// A Solana RPC credential is a merchant secret, and providers take it in the
// query string (Helius `?api-key=`). solana-go formats the full request URL
// into its errors, which get logged, so a held endpoint URL never contains the
// key: RPCEndpoint.URL is credential-free and the parameters are re-attached
// here, on a clone of the outbound request.

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

// CredentialFingerprint is a short, non-reversible digest of a credential, so
// arming can be asserted without the secret in a URL, log line or error.
func CredentialFingerprint(v string) string {
	if v == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(v))
	return hex.EncodeToString(sum[:])[:12]
}

// PrimaryCredentialFingerprint fingerprints the credential armed on the
// primary endpoint; empty when the endpoint carries none.
func (c *RPCFallbackClient) PrimaryCredentialFingerprint() string {
	if len(c.endpoints) == 0 {
		return ""
	}
	vals := make([]string, 0, len(c.endpoints[0].secret))
	for _, v := range c.endpoints[0].secret {
		vals = append(vals, v...)
	}
	sort.Strings(vals)
	return CredentialFingerprint(strings.Join(vals, "\x00"))
}
