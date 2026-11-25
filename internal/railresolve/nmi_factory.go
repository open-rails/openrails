package railresolve

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/writeposture"
)

// NMIFactory is the only way OpenRails builds an NMI client for a PSP. The
// client is bound to the exact merchant, PSP and account, the PSP's versioned
// security key and its declared environment, so its posture key is the one
// startup verified. There is no bare-key path.
type NMIFactory struct {
	// Gateway is the loopback NMI gateway (Deps.ProviderEndpoints), or empty
	// for NMI's own endpoints.
	Gateway string
	// Postures is each merchant's write posture: a readonly merchant's
	// client writes nothing, and so does every client without Postures.
	Postures writeposture.Reader
	// Transport replaces the wire at the real endpoints (test seam). Unlike
	// the sandbox gateway it exempts nothing: posture is verified through it.
	Transport http.RoundTripper
	Log       *slog.Logger
}

func (f *NMIFactory) gateway() string {
	if f == nil {
		return ""
	}
	return f.Gateway
}

// Settings reads the PSP's credential set: its versioned security key, the
// optional webhook secret and the declared environment.
func (f *NMIFactory) Settings(ctx context.Context, secrets merchants.MerchantSecretReader, mid billing.MerchantID, scope merchants.PSPScope) (*config.NMIProviderSettings, error) {
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
	if securityKey == "" && !scope.Declared {
		return nil, fmt.Errorf("merchant %s: %w", mid.String(), scope.ErrUndeclared())
	}
	if securityKey == "" {
		name, _ := merchants.PSPSecretName(scope.Rail, scope.Environment, scope.AccountID, "security_key")
		return nil, fmt.Errorf("merchant %s rail %s: secret %s missing (a declared account never falls back to boot rails)", mid.String(), scope.Rail, name)
	}
	webhookSecret, _ := read("webhook_signing_secret")
	if _, err := config.CardEntry(scope.Rail, scope.Settings, scope.CustodianID != nil); err != nil {
		return nil, err
	}
	return &config.NMIProviderSettings{SecurityKey: securityKey, WebhookSecret: webhookSecret, Environment: scope.Environment}, nil
}

// Client builds the PSP-bound client for scope.
func (f *NMIFactory) Client(ctx context.Context, secrets merchants.MerchantSecretReader, mid billing.MerchantID, scope merchants.PSPScope) (*nmi.NMIClient, error) {
	settings, err := f.Settings(ctx, secrets, mid, scope)
	if err != nil {
		return nil, err
	}
	return f.ClientFor(ctx, mid, scope, settings)
}

// ClientFor binds already-read settings to scope (callers that hold the
// credential set, such as a custodian proxy, still get the same client). The
// client is readonly when the merchant's write posture is.
func (f *NMIFactory) ClientFor(ctx context.Context, mid billing.MerchantID, scope merchants.PSPScope, settings *config.NMIProviderSettings) (*nmi.NMIClient, error) {
	client, err := nmi.NewAccountClient(f.Log, mid.UUID(), scope.ID, scope.AccountID, settings)
	if err != nil {
		return nil, fmt.Errorf("build NMI client for PSP %s: %w", scope.ID, err)
	}
	client.ReadOnly = f == nil || f.Postures == nil || f.Postures.Posture(ctx, mid.UUID()).ReadOnly()
	f.Wire(client)
	return client, nil
}

// Wire points a client at the factory's loopback gateway and transport, as
// every client the factory builds; a credential probe built elsewhere uses it.
func (f *NMIFactory) Wire(client *nmi.NMIClient) {
	if f == nil || client == nil {
		return
	}
	if gateway := f.gateway(); gateway != "" {
		// Loopback fake gateways only: the client refuses any mutation whose
		// destination is not a literal loopback IP.
		client.LoopbackFixture = true
		client.V5BaseURL, client.DirectPostURL, client.QueryURL = gateway, gateway, gateway
	}
	if f.Transport != nil {
		client.UseTransport(f.Transport)
	}
}

// ProxyPosture is the posture identity of scope's credential forwarded by a
// custodian proxy: the same identity, environment and destination rules.
func (f *NMIFactory) ProxyPosture(mid billing.MerchantID, scope merchants.PSPScope, settings *config.NMIProviderSettings) (*nmi.NMIClient, error) {
	return nmi.ProxyPostureClient(f.Log, mid.UUID(), scope.ID, scope.AccountID, settings, f.gateway())
}
