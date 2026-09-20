package railresolve

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
	"github.com/open-rails/openrails/pkg/merchant"
)

// NMIClientResolver arms the store-scoped NMI client for one merchant write
// (rebills, deletes, refunds, collection reads). ok=false with nil err = the
// merchant declares no NMI account; err = declared but not armable (fail
// closed, never a boot-plane fallback).
type NMIClientResolver interface {
	ResolveNMIClient(ctx context.Context, merchantID uuid.UUID, stampedAccountID *uuid.UUID) (*nmi.NMIClient, bool, error)
}

// NMIEndpoints overrides NMI base URLs on store-armed clients (a test seam
// for fake gateways). Zero value = real endpoints.
type NMIEndpoints struct {
	V5BaseURL     string
	DirectPostURL string
	QueryURL      string
}

// NMIArmer is the store-armed NMIClientResolver (#725/#788): per merchant,
// at decision time, from the armed rail state. Nothing is cached, so a
// rotated credential takes effect on the next call. MerchantsFn is late-bound.
type NMIArmer struct {
	Config      *config.Config
	DB          *db.DB
	MerchantsFn func() *merchants.Service
	Endpoints   NMIEndpoints
}

var _ NMIClientResolver = (*NMIArmer)(nil)

func (a *NMIArmer) merchants() *merchants.Service {
	if a == nil || a.MerchantsFn == nil {
		return nil
	}
	return a.MerchantsFn()
}

func (a *NMIArmer) testMode() bool { return a != nil && a.Config != nil && a.Config.IsTestMode() }

// Environment is the deployment's PSP environment (#681).
func (a *NMIArmer) Environment() string { return config.ExpectedProviderEnvironment(a.testMode()) }

// ResolveNMIClient arms the client for the stamped provenance account when
// present (archived stays chargeable for existing obligations), else the
// merchant's NMI pull scope.
func (a *NMIArmer) ResolveNMIClient(ctx context.Context, merchantID uuid.UUID, stampedAccountID *uuid.UUID) (*nmi.NMIClient, bool, error) {
	svc := a.merchants()
	if svc == nil || a.DB == nil {
		return nil, false, nil
	}
	mid := merchant.ID(merchantID)
	scope, ok, err := a.ResolveScope(ctx, mid, string(models.RailNMI), stampedAccountID)
	if err != nil || !ok {
		return nil, false, err
	}
	client, err := a.NMIClient(ctx, mid, scope)
	if err != nil {
		return nil, false, err
	}
	return client, true, nil
}

// ResolveScope picks the account a write settles through: the stamped
// provenance account when present, else the pull scope (active for new work,
// else newest archived for drain).
func (a *NMIArmer) ResolveScope(ctx context.Context, mid merchant.ID, rail string, stamped *uuid.UUID) (merchants.PSPScope, bool, error) {
	svc := a.merchants()
	if svc == nil || a.DB == nil {
		return merchants.PSPScope{}, false, nil
	}
	if stamped != nil {
		row, err := a.DB.Gen(ctx).GetPSP(ctx, *stamped)
		if err != nil {
			return merchants.PSPScope{}, false, fmt.Errorf("load stamped PSP: %w", err)
		}
		if row.MerchantID != mid.UUID() {
			return merchants.PSPScope{}, false, errors.New("stamped provider account belongs to another merchant")
		}
		if !rails.SameRail(models.Rail(row.Rail), models.Rail(rail)) {
			return merchants.PSPScope{}, false, fmt.Errorf("stamped PSP %s is on rail %s, not %s", row.ID, row.Rail, rail)
		}
		return merchants.PSPScope{ID: row.ID, Rail: row.Rail, Environment: row.Environment, AccountID: row.AccountID, CustodianID: row.CustodianID}, true, nil
	}
	return svc.PullPSPScope(ctx, mid, rail, a.Environment())
}

// NMIClient builds the store-armed client for scope. A declared account with
// a missing security key fails closed.
func (a *NMIArmer) NMIClient(ctx context.Context, mid merchant.ID, scope merchants.PSPScope) (*nmi.NMIClient, error) {
	securityKey, err := a.RequireSecret(ctx, mid, scope, "security_key")
	if err != nil {
		return nil, err
	}
	webhookSecret, _, _ := a.Secret(ctx, mid, scope, "webhook_signing_secret")
	client, err := nmi.NewAccountClient(mid.UUID(), scope.ID, scope.AccountID, &config.NMIProviderSettings{SecurityKey: securityKey, WebhookSecret: webhookSecret}, a.testMode())
	if err != nil {
		return nil, fmt.Errorf("build store-armed NMI client: %w", err)
	}
	client.ReadOnly = a.Config != nil && a.Config.IsProviderReadOnly()
	if a.Endpoints.V5BaseURL != "" {
		client.V5BaseURL = a.Endpoints.V5BaseURL
	}
	if a.Endpoints.DirectPostURL != "" {
		client.DirectPostURL = a.Endpoints.DirectPostURL
	}
	if a.Endpoints.QueryURL != "" {
		client.QueryURL = a.Endpoints.QueryURL
	}
	return client, nil
}

// Secret loads one scoped secret honouring the PSP row's rotation floor
// (or#812). found=false with nil err = genuinely absent.
func (a *NMIArmer) Secret(ctx context.Context, mid merchant.ID, scope merchants.PSPScope, key string) (string, bool, error) {
	svc := a.merchants()
	if svc == nil || svc.Secrets() == nil {
		return "", false, nil
	}
	ref, err := scope.SecretRef(key)
	if err != nil {
		return "", false, err
	}
	sec, err := merchants.ReadSecretRef(ctx, svc.Secrets(), mid, ref)
	if errors.Is(err, merchants.ErrSecretNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	value := strings.TrimSpace(sec.Value)
	if value == "" {
		return "", false, nil
	}
	return value, true, nil
}

// RequireSecret is Secret plus the fail-closed contract: a declared account
// with a missing/unreadable secret errors, never a boot fallback.
func (a *NMIArmer) RequireSecret(ctx context.Context, mid merchant.ID, scope merchants.PSPScope, key string) (string, error) {
	value, found, err := a.Secret(ctx, mid, scope, key)
	name, _ := merchants.PSPSecretName(scope.Rail, scope.Environment, scope.AccountID, key)
	if err != nil {
		return "", fmt.Errorf("merchant %s rail %s: secret %s backend failed: %w", mid.String(), scope.Rail, name, err)
	}
	if !found {
		return "", fmt.Errorf("merchant %s rail %s: secret %s missing (#725: a declared account never falls back to boot rails)", mid.String(), scope.Rail, name)
	}
	return value, nil
}
