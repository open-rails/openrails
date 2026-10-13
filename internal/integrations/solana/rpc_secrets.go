package solana

import (
	"crypto/sha256"
	"encoding/hex"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gagliardetto/solana-go/rpc"
	"github.com/gagliardetto/solana-go/rpc/jsonrpc"
)

// An RPC endpoint URL is a secret: providers put the key in its path
// (QuickNode, Alchemy) or query (Helius ?api-key=). solana-go formats the
// request URL into its errors, which get logged, so its client sees only the
// endpoint's origin and the full URL is restored here, on a clone of the
// outbound request.

// secretURLTransport sends each request to the endpoint's full URL.
type secretURLTransport struct {
	base   http.RoundTripper
	target *url.URL
}

func (t *secretURLTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	if t.target == nil {
		return base.RoundTrip(req)
	}
	clone := req.Clone(req.Context())
	target := *t.target
	clone.URL = &target
	clone.Host = target.Host
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
// the endpoint's origin; the full URL rides the transport.
func newEndpointClient(ep RPCEndpoint) *rpc.Client {
	httpClient := &http.Client{
		Timeout:   rpcHTTPTimeout,
		Transport: &secretURLTransport{base: newRPCHTTPTransport(), target: ep.target},
	}
	return rpc.NewWithCustomRPCClient(jsonrpc.NewClientWithOpts(ep.URL, &jsonrpc.RPCClientOpts{HTTPClient: httpClient}))
}

// newSecretEndpoint is an endpoint whose path and query are held back from
// everything but the transport.
func newSecretEndpoint(name, rawURL string, priority int) RPCEndpoint {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || u.Host == "" {
		return RPCEndpoint{Name: name, Priority: priority}
	}
	origin := u.Scheme + "://" + u.Host
	if (u.Path == "" || u.Path == "/") && u.RawQuery == "" && u.User == nil {
		return RPCEndpoint{Name: name, URL: origin, Priority: priority}
	}
	return RPCEndpoint{Name: name, URL: origin, Priority: priority, target: u}
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

// PrimaryCredentialFingerprint fingerprints the primary endpoint's full URL;
// empty when it carries no credential.
func (c *RPCFallbackClient) PrimaryCredentialFingerprint() string {
	if len(c.endpoints) == 0 || c.endpoints[0].target == nil {
		return ""
	}
	return CredentialFingerprint(c.endpoints[0].target.String())
}
