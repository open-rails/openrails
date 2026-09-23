package railresolve

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/pkg/merchant"
)

// NMIFactory is the only way OpenRails builds an NMI client for a PSP (#1055):
// checkout, payment methods, vault, rebills, refunds, subscriptions, cutover,
// provider pulls, catalog and startup posture all come through here. The client
// is bound to the exact merchant + PSP + account, the PSP's own versioned
// security key and its declared endpoint_deployment, so its posture key is the
// one startup verified. There is no bare-key path.
type NMIFactory struct {
	Config *config.Config
	// Endpoints points clients at a loopback fake gateway (test seam). Unset
	// fields fall back to config.ProviderSandbox's NMI gateway.
	Endpoints NMIEndpoints
	// Transport replaces the wire at the real endpoints (test seam). Unlike
	// Endpoints it exempts nothing: posture is verified through it.
	Transport http.RoundTripper
}

// LoopbackNMIEndpoints routes every NMI API to one loopback fake gateway.
func LoopbackNMIEndpoints(url string) NMIEndpoints {
	return NMIEndpoints{V5BaseURL: url, DirectPostURL: url, QueryURL: url}
}

func (f *NMIFactory) testMode() bool {
	return f != nil && f.Config != nil && f.Config.IsTestMode()
}

func (f *NMIFactory) endpoints() NMIEndpoints {
	if f == nil {
		return NMIEndpoints{}
	}
	endpoints := f.Endpoints
	if f.Config != nil {
		if gateway := f.Config.SandboxNMIGatewayURL(); gateway != "" {
			for _, url := range []*string{&endpoints.V5BaseURL, &endpoints.DirectPostURL, &endpoints.QueryURL} {
				if *url == "" {
					*url = gateway
				}
			}
		}
	}
	return endpoints
}

// Settings reads the PSP's credential set: its versioned security key, the
// optional webhook secret and the declared endpoint deployment.
func (f *NMIFactory) Settings(ctx context.Context, secrets merchants.MerchantSecretReader, mid merchant.ID, scope merchants.PSPScope) (*config.NMIProviderSettings, error) {
	if secrets == nil {
		return nil, errors.New("merchant credential store is not armed")
	}
	read := func(key string) (string, error) {
		ref, err := scope.SecretRef(key)
		if err != nil {
			return "", err
		}
		sec, err := merchants.ReadSecretRef(ctx, secrets, mid, ref)
		if errors.Is(err, merchants.ErrSecretNotFound) {
			return "", nil
		}
		if err != nil {
			return "", fmt.Errorf("merchant %s rail %s: secret %s backend failed: %w", mid.String(), scope.Rail, ref.Name, err)
		}
		return strings.TrimSpace(sec.Value), nil
	}
	securityKey, err := read("security_key")
	if err != nil {
		return nil, err
	}
	if securityKey == "" {
		name, _ := merchants.PSPSecretName(scope.Rail, scope.Environment, scope.AccountID, "security_key")
		return nil, fmt.Errorf("merchant %s rail %s: secret %s missing (#725: a declared account never falls back to boot rails)", mid.String(), scope.Rail, name)
	}
	webhookSecret, _ := read("webhook_signing_secret")
	deployment, err := config.NMIEndpointDeployment(scope.Settings)
	if err != nil {
		return nil, err
	}
	return &config.NMIProviderSettings{SecurityKey: securityKey, WebhookSecret: webhookSecret, EndpointDeployment: deployment}, nil
}

// Client builds the PSP-bound client for scope.
func (f *NMIFactory) Client(ctx context.Context, secrets merchants.MerchantSecretReader, mid merchant.ID, scope merchants.PSPScope) (*nmi.NMIClient, error) {
	settings, err := f.Settings(ctx, secrets, mid, scope)
	if err != nil {
		return nil, err
	}
	return f.ClientFor(mid, scope, settings)
}

// ClientFor binds already-read settings to scope (callers that hold the
// credential set, such as a custodian proxy, still get the same client).
func (f *NMIFactory) ClientFor(mid merchant.ID, scope merchants.PSPScope, settings *config.NMIProviderSettings) (*nmi.NMIClient, error) {
	client, err := nmi.NewAccountClient(mid.UUID(), scope.ID, scope.AccountID, settings, f.testMode())
	if err != nil {
		return nil, fmt.Errorf("build NMI client for PSP %s: %w", scope.ID, err)
	}
	client.ReadOnly = f != nil && f.Config != nil && f.Config.IsProviderReadOnly()
	if endpoints := f.endpoints(); endpoints != (NMIEndpoints{}) {
		// Loopback fake gateways only: the client refuses any mutation whose
		// destination is not a literal loopback IP.
		client.LoopbackFixture = true
		if endpoints.V5BaseURL != "" {
			client.V5BaseURL = endpoints.V5BaseURL
		}
		if endpoints.DirectPostURL != "" {
			client.DirectPostURL = endpoints.DirectPostURL
		}
		if endpoints.QueryURL != "" {
			client.QueryURL = endpoints.QueryURL
		}
	}
	if f != nil && f.Transport != nil {
		client.UseTransport(f.Transport)
	}
	return client, nil
}

// ProxyPosture is the posture identity of scope's credential forwarded by a
// custodian proxy: the same identity, deployment and destination rules.
func (f *NMIFactory) ProxyPosture(mid merchant.ID, scope merchants.PSPScope, settings *config.NMIProviderSettings) (*nmi.NMIClient, error) {
	return nmi.ProxyPostureClient(mid.UUID(), scope.ID, scope.AccountID, settings, f.endpoints().DirectPostURL, f.testMode())
}
